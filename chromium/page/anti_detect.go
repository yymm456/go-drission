package page

import (
	"context"

	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/yymm456/go-drission/chromium/cdpkit"
	"github.com/yymm456/go-drission/chromium/config"
)

// antiDetectScript 在文档创建后、页面自身脚本执行前注入（Page.addScriptToEvaluateOnNewDocument），
// 抹掉最容易被反爬脚本识别的几处自动化指纹：
//  1. navigator.webdriver —— 最经典的检测点，自动化浏览器固定为 true；
//  2. window.chrome —— 无头 / 自动化环境常缺失；
//  3. navigator.plugins / languages —— 无头模式下取值明显异于真实用户。
//
// 不做 WebGL / Canvas / 字体指纹伪造：副作用大、易误伤正常页面，需要时用户可自行用 Tab.Eval 注入 stealth 脚本。
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
// 同一标签页可能被多条路径拿到（OpenPage 复用 / Browser.NewTab / BrowserContext.NewTab），
// 重复注入会让同一段脚本在每次导航时执行多遍，故用标记位挡掉重复调用。
//
// 标记位的「判断 + CDP 调用 + 置位」整体由 antiMu 串行化：若只用读写锁分段，两条并发路径
// 可能同时读到 false 并各注入一次。该锁是标签页私有的，持锁期间只做一次很短的 CDP 调用。
func (t *Tab) ensureAntiDetect(ctx context.Context, o *config.Options) error {
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
// 未开启反检测（WithAntiDetect(false)）时直接跳过。注入失败只返回错误由调用方决定是否告警——属增强能力，不应中断主流程。
func injectAntiDetect(ctx context.Context, o *config.Options) error {
	if o == nil || !o.AntiDetect {
		return nil
	}
	if ctx == nil {
		return nil
	}

	runCtx, cancel := cdpkit.WithDefaultTimeout(ctx, cdpkit.DefaultCallTimeout)
	defer cancel()

	return chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
		_, err := cdppage.AddScriptToEvaluateOnNewDocument(antiDetectScript).Do(c)
		return err
	}))
}
