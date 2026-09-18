package chromium

import (
	"context"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// antiDetectScript 在文档创建后、页面自身脚本执行前注入（Page.addScriptToEvaluateOnNewDocument），
// 用于抹掉最容易被反爬脚本识别的几处自动化指纹。
//
// 只做「低风险、高收益」的三件事：
//  1. navigator.webdriver —— 最经典的检测点，自动化浏览器固定为 true；
//  2. window.chrome —— 无头 / 自动化环境常缺失，缺了反而可疑；
//  3. navigator.plugins / languages —— 无头模式下的取值明显异于真实用户。
//
// 刻意不做 WebGL / Canvas / 字体指纹伪造：那类改动副作用大、易误伤正常页面，
// 需要时用户可自行用 Tab.Eval 注入更完整的 stealth 脚本。
const antiDetectScript = `(function () {
  try {
    Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
  } catch (e) {}
  try {
    window.chrome = window.chrome || {};
    if (!window.chrome.runtime) { window.chrome.runtime = {}; }
  } catch (e) {}
  try {
    if (Object.getOwnPropertyDescriptor(navigator, 'plugins') && navigator.plugins.length === 0) {
      Object.defineProperty(navigator, 'plugins', { get: () => [1, 2, 3, 4, 5] });
    }
  } catch (e) {}
  try {
    Object.defineProperty(navigator, 'languages', { get: () => ['zh-CN', 'zh', 'en-US', 'en'] });
  } catch (e) {}
})();`

// ensureAntiDetect 保证标签页已注入反检测脚本（幂等）。
//
// 之所以需要幂等：一个标签页可能被多条路径拿到——OpenPage 复用已有标签页、
// Browser.NewTab 新建、BrowserContext.NewTab 新建——重复注入会让同一段脚本
// 在每次导航时执行多遍。这里用标记位挡掉重复调用。
//
// 标记位的「判断 + CDP 调用 + 置位」整体由 antiMu 串行化：若只用读写锁分段，
// 两条并发路径可能同时读到 false 并各注入一次，幂等就名存实亡。这个锁是标签页
// 私有的（不像 Browser.mu 是全局的），持锁期间只做一次很短的 CDP 调用，
// 不会像 CODE_REVIEW 2.3 那样把全局锁压在网络 I/O 上。
func (t *Tab) ensureAntiDetect(ctx context.Context, o *options) error {
	if t == nil {
		return nil
	}
	t.antiMu.Lock()
	defer t.antiMu.Unlock()

	if t.antiDetected {
		return nil
	}
	if err := injectAntiDetect(ctx, o); err != nil {
		return err
	}
	t.antiDetected = true
	return nil
}

// injectAntiDetect 在指定标签页上下文上注册反检测初始化脚本。
//
// 未开启反检测（WithAntiDetect(false)）时直接跳过，不做任何 CDP 调用。
// 注入失败只返回错误由调用方决定是否告警——这属于增强能力，不该拖垮主流程。
func injectAntiDetect(ctx context.Context, o *options) error {
	if o == nil || !o.antiDetect {
		return nil
	}
	if ctx == nil {
		return nil
	}

	runCtx, cancel := withDefaultTimeout(ctx, defaultCDPTimeout)
	defer cancel()

	return chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(antiDetectScript).Do(c)
		return err
	}))
}
