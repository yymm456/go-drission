package network

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yymm456/go-drission/chromium/errs"
)

// 这些测试刻意不依赖真实 Chrome：Listener 的记录存取是纯内存逻辑，
// 直接构造 Listener 并调用其事件处理函数即可覆盖，保证在 CI（无浏览器）里也能跑。

func newTestListener() *Listener {
	return &Listener{
		pattern:    "/api/",
		records:    map[string]*Record{},
		order:      []string{},
		maxRecords: defaultMaxRecords,
		sem:        make(chan struct{}, 4),
	}
}

// TestRecordsReturnsDeepCopy 验证 Records 返回的是拷贝：
// 调用方改动返回值不能影响 Listener 内部状态（否则不同调用方会互相干扰）。
func TestRecordsReturnsDeepCopy(t *testing.T) {
	l := newTestListener()

	l.mu.Lock()
	rec := &Record{
		RequestID:      "1",
		URL:            "http://x/api/a",
		Method:         "POST",
		RequestHeaders: map[string]string{"A": "1"},
		Status:         200,
	}
	l.records["1"] = rec
	l.order = append(l.order, "1")
	l.mu.Unlock()

	got := l.Records()
	if len(got) != 1 {
		t.Fatalf("期望 1 条记录，实际 %d", len(got))
	}
	if got[0] == rec {
		t.Fatal("Records 返回了内部指针，调用方改动会污染监听器状态")
	}

	// 改动返回值的字段与 map，内部记录必须保持不变
	got[0].Status = 500
	got[0].ResponseBody = "tampered"
	got[0].RequestHeaders["A"] = "tampered"

	l.mu.Lock()
	inner := l.records["1"]
	l.mu.Unlock()

	if inner.Status != 200 {
		t.Errorf("内部 Status 被外部改动污染: %d", inner.Status)
	}
	if inner.ResponseBody != "" {
		t.Errorf("内部 ResponseBody 被外部改动污染: %q", inner.ResponseBody)
	}
	if inner.RequestHeaders["A"] != "1" {
		t.Errorf("内部 RequestHeaders 被外部改动污染: %v", inner.RequestHeaders)
	}
}

// TestRecordsConcurrentWithBackgroundWrites 是数据竞争的回归测试。
//
// 修复前：Records 直接返回内部 *Record 指针，而后台 goroutine（GetResponseBody 回填）
// 仍在写 Status / ResponseBody，调用方读取即竞争——`go test -race` 必然报警。
// 修复后：Records 返回深拷贝，读写互不干扰。
// 本测试必须配合 -race 运行才有意义（CI 已开启）。
func TestRecordsConcurrentWithBackgroundWrites(t *testing.T) {
	l := newTestListener()

	// 模拟事件回调：持续写入记录（走与生产代码相同的锁路径）
	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			l.mu.Lock()
			id := string(rune('a' + i%20))
			rec, ok := l.records[id]
			if !ok {
				rec = &Record{RequestID: id, URL: "http://x/api/" + id, Method: "POST"}
				l.records[id] = rec
				l.order = append(l.order, id)
			}
			rec.Status = int64(200 + i%5)
			rec.ResponseBody = "body-"
			rec.RequestBody = "req-"
			rec.RequestHeaders = map[string]string{"X": "y"}
			rec.ResponseHeaders = map[string]string{"Y": "z"}
			l.mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	})

	// 模拟调用方：并发反复读取并访问所有字段
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			deadline := time.Now().Add(300 * time.Millisecond)
			for time.Now().Before(deadline) {
				for _, rec := range l.Records() {
					_ = rec.Status
					_ = rec.URL
					_ = rec.Method
					_ = rec.RequestBody
					_ = rec.ResponseBody
					for k, v := range rec.RequestHeaders {
						_, _ = k, v
					}
					for k, v := range rec.ResponseHeaders {
						_, _ = k, v
					}
				}
			}
		})
	}

	readers.Wait()
	close(stop)
	writer.Wait()
}

// TestListenerRecordsCap 验证记录上限生效：超出后按 FIFO 淘汰最旧的，内存不随请求数增长。
//
// 修复前：records / order 只增不减，长时间监听会持续吃内存（CODE_REVIEW 2.11）。
func TestListenerRecordsCap(t *testing.T) {
	l := newTestListener().MaxRecords(3)

	// 依次塞入 5 条，只有最后 3 条应当保留
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		l.mu.Lock()
		rec := l.recordLocked(id)
		rec.URL = "http://x/api/" + id
		l.mu.Unlock()
	}

	got := l.Records()
	if len(got) != 3 {
		t.Fatalf("上限 3，期望保留 3 条，实际 %d", len(got))
	}
	// 顺序必须仍是到达顺序，且淘汰的是最旧的
	for i, want := range []string{"3", "4", "5"} {
		if got[i].RequestID != want {
			t.Errorf("第 %d 条期望 %s，实际 %s", i, want, got[i].RequestID)
		}
	}

	l.mu.Lock()
	leaked := len(l.records)
	l.mu.Unlock()
	if leaked != 3 {
		t.Errorf("records 内部表未同步裁剪，仍有 %d 项", leaked)
	}

	// 调小上限应立即裁剪已有记录
	l.MaxRecords(1)
	if n := l.Len(); n != 1 {
		t.Errorf("调小上限后期望 1 条，实际 %d", n)
	}
	if got := l.Records(); got[0].RequestID != "5" {
		t.Errorf("调小上限后应保留最新的 5，实际 %s", got[0].RequestID)
	}

	// Clear 清空全部记录
	l.Clear()
	if n := l.Len(); n != 0 {
		t.Errorf("Clear 后期望 0 条，实际 %d", n)
	}

	// 默认上限必须是有界的（0 表示不限制，只允许调用方显式指定）
	if defaultMaxRecords <= 0 {
		t.Error("默认记录上限必须为正数，否则默认行为就是无限制增长")
	}
}

// TestListenerRecordsCapUnlimited 验证 MaxRecords(0) 显式关闭上限。
func TestListenerRecordsCapUnlimited(t *testing.T) {
	l := newTestListener().MaxRecords(0)
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		l.mu.Lock()
		l.recordLocked(id)
		l.mu.Unlock()
	}
	if n := l.Len(); n != 5 {
		t.Errorf("上限为 0 时不应淘汰，期望 5 条，实际 %d", n)
	}
}

// TestListenerRejectsBareContext 验证传裸 context 时返回可读错误而不是 panic。
// 底层 chromedp.ListenTarget 遇到无路由信息的 ctx 会直接 panic，必须在库内拦住。
func TestListenerRejectsBareContext(t *testing.T) {
	l := newTestListener()
	if err := l.Start(context.Background()); !errors.Is(err, errs.ErrInvalidContext) {
		t.Fatalf("期望 errs.ErrInvalidContext，实际 %v", err)
	}
}

// TestMatchURL 覆盖监听的 URL 过滤语义。
func TestMatchURL(t *testing.T) {
	cases := []struct {
		pattern  string
		url      string
		expected bool
	}{
		{"", "http://anything", true},             // 空 pattern 命中全部
		{"/api/", "http://x/api/user?id=1", true}, // 子串命中（带 query）
		{"/api/", "http://x/other", false},        // 不命中
		{"cm.bilibili.com", "https://cm.bilibili.com/x", true},
	}
	for _, c := range cases {
		l := &Listener{pattern: c.pattern}
		if got := l.matchURL(c.url); got != c.expected {
			t.Errorf("pattern=%q url=%q: 期望 %v，实际 %v", c.pattern, c.url, c.expected, got)
		}
	}
}

// TestIsBodyMethod 覆盖请求体兜底逻辑的判定条件。
func TestIsBodyMethod(t *testing.T) {
	for _, m := range []string{"POST", "PUT", "PATCH"} {
		if !isBodyMethod(m) {
			t.Errorf("%s 应该判定为可能有请求体", m)
		}
	}
	for _, m := range []string{"GET", "HEAD", "DELETE", "OPTIONS"} {
		if isBodyMethod(m) {
			t.Errorf("%s 不应判定为有请求体", m)
		}
	}
}
