package network

import (
	"testing"

	cdpnetwork "github.com/chromedp/cdproto/network"
)

// 本文件覆盖 mergeExtraHeaders 的纯逻辑（不需要浏览器）。
//
// 为什么值得单独测：CDP 把 Set-Cookie 藏在 responseReceivedExtraInfo 里，
// 而且重复头是「用 \n 连接成单键」而不是数组 —— 这个约定不测就会在
// 「一次响应下发多个 Set-Cookie」时静默丢数据（只剩一条）。

func TestMergeExtraHeadersSupplementsResponseHeaders(t *testing.T) {
	rec := &Record{ResponseHeaders: map[string]string{"Content-Type": "text/html"}}

	mergeExtraHeaders(rec, cdpnetwork.Headers{
		"content-type": "text/html; charset=utf-8",
		"x-extra":      "from-extrainfo",
	})

	if got := rec.ResponseHeaders["content-type"]; got != "text/html; charset=utf-8" {
		t.Errorf("ExtraInfo 的头应以它为准覆盖初步头，实际 %q", got)
	}
	if got := rec.ResponseHeaders["x-extra"]; got != "from-extrainfo" {
		t.Errorf("初步头里没有的键应被补进来，实际 %q", got)
	}
}

func TestMergeExtraHeadersSplitsMultipleSetCookies(t *testing.T) {
	rec := &Record{}

	// CDP 约定：重复头用 \n 连接成单键
	mergeExtraHeaders(rec, cdpnetwork.Headers{
		"Set-Cookie": "a=1; Path=/\nb=2; Path=/",
	})

	if len(rec.SetCookies) != 2 {
		t.Fatalf("期望拆出 2 条 Set-Cookie，实际 %d：%v", len(rec.SetCookies), rec.SetCookies)
	}
	if rec.SetCookies[0] != "a=1; Path=/" || rec.SetCookies[1] != "b=2; Path=/" {
		t.Errorf("Set-Cookie 拆分结果不符：%v", rec.SetCookies)
	}
}

func TestMergeExtraHeadersSetCookieCaseInsensitive(t *testing.T) {
	for _, key := range []string{"Set-Cookie", "set-cookie", "SET-COOKIE"} {
		rec := &Record{}
		mergeExtraHeaders(rec, cdpnetwork.Headers{key: "sid=abc; Path=/"})
		if len(rec.SetCookies) != 1 {
			t.Errorf("键名 %q 未被识别为 Set-Cookie：%v", key, rec.SetCookies)
		}
	}
}

func TestMergeExtraHeadersWithoutSetCookie(t *testing.T) {
	rec := &Record{}
	mergeExtraHeaders(rec, cdpnetwork.Headers{"X-Foo": "bar"})

	if rec.SetCookies != nil {
		t.Errorf("响应没有 Set-Cookie 时该字段应为 nil，实际 %v", rec.SetCookies)
	}
}

func TestMergeExtraHeadersEmptyIsNoop(t *testing.T) {
	rec := &Record{}
	mergeExtraHeaders(rec, nil)

	if rec.ResponseHeaders != nil {
		t.Errorf("空头不应创建 map，实际 %v", rec.ResponseHeaders)
	}
	if rec.SetCookies != nil {
		t.Errorf("空头不应产生 SetCookies，实际 %v", rec.SetCookies)
	}
}

func TestRecordCloneCopiesSetCookies(t *testing.T) {
	rec := &Record{ResponseBody: "x", SetCookies: []string{"a=1", "b=2"}}
	c := rec.clone()

	if len(c.SetCookies) != 2 {
		t.Fatalf("clone 应复制 SetCookies，实际 %v", c.SetCookies)
	}
	// 必须是独立底层数组：调用方改动不能影响内部记录
	c.SetCookies[0] = "changed"
	if rec.SetCookies[0] != "a=1" {
		t.Errorf("clone 与原件共享了底层数组（深拷贝漏字段）：%v", rec.SetCookies)
	}
}
