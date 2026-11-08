// checkin_cdp.go 豆包每日签到（浏览器通道）。
//
// 为什么必须用浏览器：豆包签到活动页是渐进水合的 SPA，签到接口
// （/alice/ug/campaign/landing/checkin）要求完整设备签名与会话，
// 站外直连一律返回「系统错误」。而页面自己发起的请求天然带齐这些信息，
// 所以这里做的是「起一个无头 Chrome → 注入 Cookie 与客户端 UA → 打开活动页
// → 读页面自己拿到的签到状态 → 必要时在页面上下文里补一次动作」。
//
// 判定优先级（从硬到软）：
//  1. 活动页自身接口返回的 daily_checkin.checked_in_today（最权威）
//  2. landing/checkin 的响应体（点击后才有）
//  3. 页面上「已签到 / 明日再来」文案（DOM 改版就会失效，只作兜底）
//
// 全程无窗口、约 20~40 秒；同一时刻只允许一个实例（cdpMu），
// 多账号由调度层的并发闸门排队，避免 N 个 Chrome 一起抢内存。
package doubao

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"chinacpa/internal/model"
)

const (
	// doubaoActivityURL 签到活动页（页内会自动拉 campaign 状态）
	doubaoActivityURL = "https://www.doubao.com/pc/agent-explore"
	// doubaoCheckinUA 客户端 UA：活动页会校验 UA 含 SamanthaDoubao/
	doubaoCheckinUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36 SamanthaDoubao/2.33.3"
)

// SetCheckinOptions 配置签到浏览器通道（main 启动时注入；不调用则保持关闭）。
func (p *Provider) SetCheckinOptions(viaBrowser bool, chromePath string) {
	p.checkinBrowser = viaBrowser
	p.checkinChromePath = strings.TrimSpace(chromePath)
}

// SetBrowserNoProxy 控制无头浏览器是否强制直连（默认 true）。
//
// 无头 Chrome 默认跟随 Windows 系统代理，而代理软件退出后注册表里的
// ProxyEnable 常常还是 1 —— 浏览器就会一直往一个已经没人监听的端口打，
// 表现为「关掉代理还是连不上豆包」。豆包是境内站点，直连最稳，所以默认断开代理。
func (p *Provider) SetBrowserNoProxy(noProxy bool) { p.browserNoProxy = noProxy }

// checkinViaBrowser 走一次完整的无头浏览器签到流程。
// 任何异常都以「失败结果」返回，不向上抛 error —— 调度层需要的是可读原因。
func (p *Provider) checkinViaBrowser(ctx context.Context, a *model.Account) *model.CheckinResult {
	cookies := parseCookieHeader(a.WebCookie)
	if len(cookies) == 0 || cookies["sessionid"] == "" {
		return &model.CheckinResult{OK: false, Message: "缺少 sessionid Cookie，请重新扫码或粘贴完整 Cookie"}
	}

	// 全机串行：两个 Chrome 同时起会互相抢调试端口与内存
	p.cdpMu.Lock()
	defer p.cdpMu.Unlock()

	chrome, err := p.findChrome()
	if err != nil {
		return &model.CheckinResult{OK: false, Message: err.Error()}
	}
	port, err := freeDebugPort()
	if err != nil {
		return &model.CheckinResult{OK: false, Message: err.Error()}
	}
	dir, err := os.MkdirTemp("", "chinacpa-doubao-*")
	if err != nil {
		return &model.CheckinResult{OK: false, Message: "创建浏览器目录失败: " + err.Error()}
	}
	defer os.RemoveAll(dir)

	args := []string{
		"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-extensions", "--window-size=1280,900",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir=" + dir,
	}
	if p.browserNoProxy {
		// 豆包是境内站点：绕开系统代理，避免代理软件关闭后注册表残留把浏览器卡死
		args = append(args, "--no-proxy-server")
	}
	args = append(args, "about:blank")
	cmd := exec.Command(chrome, args...)
	if err := cmd.Start(); err != nil {
		return &model.CheckinResult{OK: false, Message: "启动无头浏览器失败: " + err.Error()}
	}
	defer func() {
		closeBrowserViaCDP(port)
		if cmd.Process != nil {
			waitProcessExit(cmd.Process, 4*time.Second)
		}
	}()

	wsURL, err := waitDebugTarget(port, 20*time.Second)
	if err != nil {
		return &model.CheckinResult{OK: false, Message: err.Error()}
	}
	ws, err := wsDial(wsURL, 5*time.Second)
	if err != nil {
		return &model.CheckinResult{OK: false, Message: "CDP 连接失败: " + err.Error()}
	}
	defer ws.Close()

	// 1) 伪装成客户端：UA 必须含 SamanthaDoubao/
	_, _ = cdpCall(ws, 1, "Emulation.setUserAgentOverride", map[string]any{"userAgent": doubaoCheckinUA}, 5*time.Second)
	if _, err := cdpCall(ws, 2, "Network.enable", map[string]any{}, 5*time.Second); err != nil {
		return &model.CheckinResult{OK: false, Message: "Network.enable 失败: " + err.Error()}
	}
	// 2) 注入 Cookie（domain 统一 .doubao.com）
	n := 0
	for name, value := range cookies {
		if strings.TrimSpace(value) == "" {
			continue
		}
		params := map[string]any{
			"name": name, "value": value,
			"domain": ".doubao.com", "path": "/",
			"secure": true, "httpOnly": true,
		}
		if _, err := cdpCall(ws, 10+n, "Network.setCookie", params, 5*time.Second); err == nil {
			n++
		}
	}
	if n == 0 {
		return &model.CheckinResult{OK: false, Message: "Cookie 注入失败"}
	}
	_, _ = cdpCall(ws, 100, "Page.enable", map[string]any{}, 5*time.Second)
	// 3) 文档创建前挂请求钩子：读页面自己拿到的签到状态，比看 DOM 稳
	_, _ = cdpCall(ws, 101, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": netLogJS}, 5*time.Second)
	if _, err := cdpCall(ws, 102, "Page.navigate", map[string]any{"url": doubaoActivityURL}, 15*time.Second); err != nil {
		return &model.CheckinResult{OK: false, Message: "打开活动页失败: " + err.Error()}
	}

	// 4) 等页面就绪（渐进水合：卡片后到，按钮先到会点空）
	state, err := p.waitPageReady(ctx, ws)
	if err != nil {
		return &model.CheckinResult{OK: false, Message: err.Error()}
	}
	if state.Login {
		return &model.CheckinResult{OK: false, Message: "登录态已失效（活动页要求登录），请重新扫码或更新 Cookie"}
	}

	// 5) 先读页面接口（权威）：已签就直接返回，不再点按钮
	if phase, detail, ok := pageStatusFetch(ws); ok {
		switch phase {
		case "already", "claimed":
			claimed := claimTaskRewards(ws)
			msg := "今日已签到（浏览器通道确认）"
			if detail != "" {
				msg = detail
			}
			return &model.CheckinResult{OK: true, Already: true, Message: wrapTasks(msg, claimed)}
		}
	}
	if phase, detail, ok := parseNet(netRows(ws, 300)); ok {
		switch phase {
		case "already", "claimed":
			claimed := claimTaskRewards(ws)
			msg := "今日已签到（接口确认）"
			if detail != "" {
				msg = detail
			}
			return &model.CheckinResult{OK: true, Already: true, Message: wrapTasks(msg, claimed)}
		}
	}
	if state.cardCheckedIn() {
		claimed := claimTaskRewards(ws)
		return &model.CheckinResult{OK: true, Already: true, Message: wrapTasks("今日已签到（页面显示已签）", claimed)}
	}

	// 6) 未签 → 定位并真实点击签到按钮
	if state.Btn == "" {
		return &model.CheckinResult{OK: false, Message: "活动页未找到签到按钮（页面可能改版）：" + clip(state.Sample, 120)}
	}
	clicked := ""
	for i := 0; i < 2 && clicked == ""; i++ {
		if i > 0 {
			time.Sleep(2 * time.Second)
			if st, err := scanPage(ws, 600); err == nil {
				if st.cardCheckedIn() || st.Success || st.Already {
					claimed := claimTaskRewards(ws)
					return &model.CheckinResult{OK: true, Already: true, Message: wrapTasks("今日已签到（重扫确认）", claimed)}
				}
				state = st
			}
		}
		text, x, y, lerr := locateSigninButton(ws)
		if lerr != nil || text == "" {
			continue
		}
		if cerr := dispatchClick(ws, x, y); cerr != nil {
			continue
		}
		clicked = text
	}
	if clicked == "" {
		return &model.CheckinResult{OK: false, Message: "点击签到按钮失败（页面结构可能已改版）"}
	}

	// 7) 等结果：接口响应 > 页面文案
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return &model.CheckinResult{OK: false, Message: "签到超时（浏览器通道耗时过长）"}
		}
		time.Sleep(1500 * time.Millisecond)
		if phase, detail, ok := parseNet(netRows(ws, 500)); ok && phase != "claimable" {
			claimed := claimTaskRewards(ws)
			msg := "签到成功（接口确认）"
			if detail != "" {
				msg = detail
			}
			if phase == "already" {
				return &model.CheckinResult{OK: true, Already: true, Message: wrapTasks(msg, claimed)}
			}
			return &model.CheckinResult{OK: true, Message: wrapTasks(msg, claimed)}
		}
		if st, err := scanPage(ws, 520); err == nil {
			if st.Success {
				claimed := claimTaskRewards(ws)
				return &model.CheckinResult{OK: true, Message: wrapTasks("签到成功", claimed)}
			}
			if st.Already || st.cardCheckedIn() {
				claimed := claimTaskRewards(ws)
				return &model.CheckinResult{OK: true, Already: true, Message: wrapTasks("今日已签到（点击后确认）", claimed)}
			}
		}
	}
	return &model.CheckinResult{OK: false, Message: "已点击「" + clicked + "」但未确认结果，请到豆包 App 核对"}
}

// waitPageReady 等活动页水合到可判定状态。
func (p *Provider) waitPageReady(ctx context.Context, ws *wsConn) (scanState, error) {
	var last scanState
	started := time.Now()
	deadline := started.Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return last, fmt.Errorf("签到超时（等待活动页加载）")
		}
		time.Sleep(2 * time.Second)
		st, err := scanPage(ws, 300)
		if err != nil {
			continue
		}
		last = st
		// 只认「卡片出现 / 明确已签 / 需要登录」为就绪；孤立按钮最多多等 12 秒，
		// 防止对未挂载完的壳点空。
		if st.Ready && (st.Already || st.Success || st.Login || st.Card != "" ||
			(st.Btn != "" && time.Since(started) > 12*time.Second)) {
			return last, nil
		}
	}
	if !last.Ready {
		return last, fmt.Errorf("活动页 60 秒内未加载完成")
	}
	return last, nil
}

// ── 浏览器进程管理 ───────────────────────────────────────

// findChrome 找系统浏览器（优先 Chrome，回落 Edge）。
func (p *Provider) findChrome() (string, error) {
	if p.checkinChromePath != "" {
		if _, err := os.Stat(p.checkinChromePath); err == nil {
			return p.checkinChromePath, nil
		}
		return "", fmt.Errorf("配置的浏览器路径不存在: %s", p.checkinChromePath)
	}
	cands := []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("未找到 Chrome / Edge：豆包签到需要本机浏览器，或在 doubao.chrome_path 指定路径")
}

// freeDebugPort 找 9333-9433 之间的可用调试端口。
func freeDebugPort() (int, error) {
	for p := 9333; p <= 9433; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			ln.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("9333-9433 无可用调试端口")
}

type debugTarget struct {
	URL   string `json:"url"`
	WSURL string `json:"webSocketDebuggerUrl"`
}

// waitDebugTarget 等调试端口就绪并取第一个可驱动的页面。
func waitDebugTarget(port int, timeout time.Duration) (string, error) {
	cl := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(700 * time.Millisecond)
		resp, err := cl.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
		if err != nil {
			continue
		}
		var targets []debugTarget
		_ = json.NewDecoder(resp.Body).Decode(&targets)
		resp.Body.Close()
		for _, t := range targets {
			if t.WSURL != "" && (strings.HasPrefix(t.URL, "about:") || t.URL == "") {
				return t.WSURL, nil
			}
		}
		if len(targets) > 0 && targets[0].WSURL != "" {
			return targets[0].WSURL, nil
		}
	}
	return "", fmt.Errorf("浏览器调试端口未就绪")
}

// closeBrowserViaCDP 优雅关闭浏览器：Chrome 是多进程架构，直接 Kill 主进程会留下
// 孤儿 renderer 占住 user-data-dir。
func closeBrowserViaCDP(port int) {
	if port <= 0 {
		return
	}
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var v struct {
		WSURL string `json:"webSocketDebuggerUrl"`
	}
	if json.NewDecoder(resp.Body).Decode(&v) != nil || v.WSURL == "" {
		return
	}
	ws, err := wsDial(v.WSURL, 3*time.Second)
	if err != nil {
		return
	}
	defer ws.Close()
	_ = ws.writeText([]byte(`{"id":1,"method":"Browser.close"}`))
}

func waitProcessExit(proc *os.Process, timeout time.Duration) {
	if proc == nil {
		return
	}
	done := make(chan struct{})
	go func() { _, _ = proc.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		_ = proc.Kill()
	}
}

// ── CDP 基础调用 ─────────────────────────────────────────

// cdpCall 发一条 CDP 命令并等响应（按 id 匹配，其余事件忽略）。
// 页面弹原生 confirm 时自动点「确定」——否则 Runtime.evaluate 会被挂住。
func cdpCall(ws *wsConn, id int, method string, params map[string]any, timeout time.Duration) (json.RawMessage, error) {
	req := map[string]any{"id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	buf, _ := json.Marshal(req)
	if err := ws.writeText(buf); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msg, err := ws.readMessage(timeout)
		if err != nil {
			return nil, err
		}
		var resp struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(msg, &resp) != nil {
			continue
		}
		if resp.Method == "Page.javascriptDialogOpening" {
			buf, _ := json.Marshal(map[string]any{
				"id": 999999, "method": "Page.handleJavaScriptDialog",
				"params": map[string]any{"accept": true},
			})
			_ = ws.writeText(buf)
			continue
		}
		if resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, resp.Error.Message)
		}
		// Runtime.evaluate 的业务返回值在 result.result.value（内嵌 JSON 字符串）
		if method == "Runtime.evaluate" {
			var inner struct {
				Result struct {
					Value string `json:"value"`
				} `json:"result"`
			}
			if json.Unmarshal(resp.Result, &inner) == nil && inner.Result.Value != "" {
				return json.RawMessage(inner.Result.Value), nil
			}
			return json.RawMessage(`{}`), nil
		}
		return resp.Result, nil
	}
	return nil, fmt.Errorf("%s: CDP 响应超时", method)
}

// evalJS 在页面执行表达式并等 Promise。
func evalJS(ws *wsConn, id int, expr string, timeout time.Duration) (json.RawMessage, error) {
	return cdpCall(ws, id, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "awaitPromise": true,
	}, timeout)
}

// netRow 页面钩子记录的一条请求响应。
type netRow struct {
	URL    string `json:"url"`
	Status int    `json:"status"`
	Body   string `json:"body"`
}

func netRows(ws *wsConn, id int) []netRow {
	raw, err := evalJS(ws, id, `JSON.stringify(window.__ccpaNet || [])`, 6*time.Second)
	if err != nil {
		return nil
	}
	var out []netRow
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// ── 页面脚本 ─────────────────────────────────────────────

// netLogJS 文档创建前注入：记录页面自身发起的请求与响应。
// 活动接口跑在 iframe 里，因此同时向 parent/top 镜像一份。
const netLogJS = `(() => {
  if (window.__ccpaNet) return;
  window.__ccpaNet = [];
  const rec = (url, status, body) => {
    const row = { url: String(url).slice(0, 200), status, body: String(body || '').slice(0, 200000) };
    try { window.__ccpaNet.push(row); } catch (e) {}
    try { if (parent && parent !== window) { (parent.__ccpaNet = parent.__ccpaNet || []).push(row); } } catch (e) {}
    try { if (top && top !== window && top !== parent) { (top.__ccpaNet = top.__ccpaNet || []).push(row); } } catch (e) {}
  };
  try {
    const of = window.fetch;
    if (of) window.fetch = async function (...args) {
      const res = await of.apply(this, args);
      try {
        const url = String((args[0] && args[0].url) || args[0] || '');
        const c = res.clone();
        c.text().then(t => rec(url, res.status, t)).catch(() => {});
      } catch (e) {}
      return res;
    };
  } catch (e) {}
  try {
    const oo = XMLHttpRequest.prototype.open, os = XMLHttpRequest.prototype.send;
    XMLHttpRequest.prototype.open = function (m, u) { this.__u = u; return oo.apply(this, arguments); };
    XMLHttpRequest.prototype.send = function () {
      this.addEventListener('load', () => {
        try {
          let b = '';
          try { b = this.responseText; } catch (e) { try { b = JSON.stringify(this.response); } catch (e2) {} }
          rec(this.__u, this.status, b);
        } catch (e) {}
      });
      return os.apply(this, arguments);
    };
  } catch (e) {}
})();`

// pageStatusJS 页面上下文内查签到状态（只读，带页面自己的会话签名）。
const pageStatusJS = `(async () => {
  try {
    const r = await fetch('/alice/ug/campaign/landing/get', {
      method: 'POST', credentials: 'include',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ scene: '', biz_params: {} })
    });
    return await r.text();
  } catch (e) { return 'FETCH_ERR:' + (e && e.message); }
})()`

// scanJS 页面状态扫描。
// 已签判据只认「已签到 / 明日再来」：任务列表里的「已领取」不能当签到完成信号，
// 否则随便一个已完成的任务都会把未签误报成已签。
const scanJS = `(() => {
  const texts = ['立即签到','每日签到','签到领额度','今日签到','立即领取','立即签到领额度'];
  const alreadyTexts = ['已签到','明日再来'];
  const successTexts = ['签到成功','领取成功','额度已到账'];
  const body = (document.body && document.body.innerText) || '';
  const has = arr => arr.some(x => body.includes(x));
  const els = [...document.querySelectorAll('button,[role=button],a,div,span')];
  const vis = e => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
  const pick = root => {
    const pool = root ? [...root.querySelectorAll('button,[role=button],a,div,span')] : els;
    for (const tx of texts) {
      for (const e of pool) {
        const t = (e.innerText || '').trim();
        if (t === tx && !e.disabled && vis(e)) return tx;
      }
    }
    return null;
  };
  let cardEl = null;
  for (const e of els) {
    const t = (e.innerText || '').trim();
    if (t && t.includes('每日签到') && t.length < 200) { cardEl = e; break; }
  }
  const btn = (cardEl && pick(cardEl)) || pick(null);
  const card = cardEl ? (cardEl.innerText || '').trim() : '';
  return JSON.stringify({
    ready: document.readyState === 'complete' || document.readyState === 'interactive',
    btn, already: has(alreadyTexts), success: has(successTexts),
    card, login: body.includes('扫码登录') || body.includes('手机号登录'),
    sample: body.replace(/\s+/g, ' ').slice(0, 400)
  });
})()`

// locateJS 定位签到按钮中心坐标。
// 返回坐标而不是直接 e.click()：React 合成 click 可能触发同步导航销毁执行上下文，
// 走 CDP Input 域的真实鼠标事件（与 playwright 同机制）最稳。
const locateJS = `(async () => {
  const texts = ['立即签到','每日签到','签到领额度','今日签到','立即领取','立即签到领额度'];
  const els = [...document.querySelectorAll('button,[role=button],a,div,span')];
  const vis = e => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
  const pick = root => {
    const pool = root ? [...root.querySelectorAll('button,[role=button],a,div,span')] : els;
    for (const tx of texts) {
      for (const e of pool) {
        const t = (e.innerText || '').trim();
        if (t === tx && !e.disabled && vis(e)) return { e, tx };
      }
    }
    return null;
  };
  let card = null;
  for (const e of els) {
    const t = (e.innerText || '').trim();
    if (t && t.includes('每日签到') && t.length < 200) { card = e; break; }
  }
  let hit = card ? pick(card) : null;
  if (!hit) hit = pick(null);
  if (!hit) return null;
  const e = hit.e;
  e.scrollIntoView({ block: 'center', inline: 'center' });
  await new Promise(r => setTimeout(r, 500));
  const r = e.getBoundingClientRect();
  return JSON.stringify({ text: hit.tx, x: Math.round(r.x + r.width / 2), y: Math.round(r.y + r.height / 2) });
})()`

// claimTasksJS 把「已完成待领」的任务奖励点一遍（只点领取，不碰去完成）。
const claimTasksJS = `(async () => {
  const sleep = ms => new Promise(r => setTimeout(r, ms));
  await sleep(2000);
  const vis = e => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
  const texts = ['领取','立即领取','去领取','一键领取'];
  let n = 0;
  for (let round = 0; round < 2; round++) {
    const btns = [...document.querySelectorAll('button,[role=button],div,span')].filter(e => {
      const t = (e.innerText || '').trim();
      return texts.includes(t) && !e.disabled && vis(e);
    });
    if (!btns.length) break;
    for (const b of btns.slice(0, 8)) {
      try { b.click(); n++; await sleep(900); } catch (e) {}
    }
    await sleep(1000);
  }
  return JSON.stringify({ n });
})()`

// ── 页面状态解析 ─────────────────────────────────────────

type scanState struct {
	Ready   bool   `json:"ready"`
	Btn     string `json:"btn"`
	Already bool   `json:"already"`
	Success bool   `json:"success"`
	Card    string `json:"card"`
	Login   bool   `json:"login"`
	Sample  string `json:"sample"`
}

// cardCheckedIn 签到卡片区域显示「已签到 / 明日再来」。
func (s scanState) cardCheckedIn() bool {
	return s.Card != "" && (strings.Contains(s.Card, "已签到") || strings.Contains(s.Card, "明日再来"))
}

func scanPage(ws *wsConn, id int) (scanState, error) {
	raw, err := evalJS(ws, id, scanJS, 8*time.Second)
	if err != nil {
		return scanState{}, err
	}
	var out scanState
	if err := json.Unmarshal(raw, &out); err != nil {
		return scanState{}, err
	}
	return out, nil
}

// pageStatusFetch 页面内查签到状态，解析 checked_in_today。
func pageStatusFetch(ws *wsConn) (phase, detail string, ok bool) {
	raw, err := evalJS(ws, 700, pageStatusJS, 15*time.Second)
	if err != nil {
		return "", "", false
	}
	body := string(raw)
	lb := strings.ToLower(body)
	if strings.HasPrefix(lb, "fetch_err") {
		return "", "", false
	}
	if i := strings.Index(lb, `"checked_in_today"`); i >= 0 {
		seg := lb[i:min(len(lb), i+40)]
		if strings.Contains(seg, "true") {
			return "already", "今日已签到（活动接口确认）", true
		}
		if strings.Contains(seg, "false") {
			return "claimable", "今日未签到", true
		}
	}
	return "", "", false
}

// checkinFlagRe 从活动详情响应里提取 daily_checkin 状态。
var checkinFlagRe = regexp.MustCompile(
	`"daily_checkin":\{"checked_in_today":(true|false),"continuous_days":(\d+),"last_checkin_date":"([^"]+)"`)

// parseNet 从钩子记录里判定签到状态。
func parseNet(rows []netRow) (phase, detail string, ok bool) {
	for _, r := range rows {
		lu := strings.ToLower(r.URL)
		if strings.Contains(lu, "landing/checkin") {
			lb := strings.ToLower(r.Body)
			if strings.Contains(lb, `"checked_in":true`) || strings.Contains(lb, `"checked_in": true`) {
				return "claimed", "签到成功（接口确认）", true
			}
			if strings.Contains(lb, "already") || strings.Contains(r.Body, "已领取") || strings.Contains(r.Body, "已签到") {
				return "already", "今日已领取（接口确认）", true
			}
		}
		if m := checkinFlagRe.FindStringSubmatch(r.Body); m != nil {
			if m[1] == "true" {
				return "already", "今日已签到（连续 " + m[2] + " 天，最近 " + m[3] + "）", true
			}
			return "claimable", "今日未签到（上次 " + m[3] + "）", true
		}
	}
	return "", "", false
}

// locateSigninButton 找到签到按钮中心坐标。
func locateSigninButton(ws *wsConn) (text string, x, y float64, err error) {
	raw, err := evalJS(ws, 400, locateJS, 12*time.Second)
	if err != nil {
		return "", 0, 0, err
	}
	var loc *struct {
		Text string  `json:"text"`
		X    float64 `json:"x"`
		Y    float64 `json:"y"`
	}
	if err := json.Unmarshal(raw, &loc); err != nil || loc == nil {
		return "", 0, 0, nil
	}
	return loc.Text, loc.X, loc.Y, nil
}

// dispatchClick 用 CDP Input 域模拟一次真实鼠标点击（移入 → 按下 → 抬起的完整序列，
// 页面的 React 事件依赖这个序列）。
func dispatchClick(ws *wsConn, x, y float64) error {
	move := map[string]any{"type": "mouseMoved", "x": x, "y": y, "button": "none"}
	if _, err := cdpCall(ws, 401, "Input.dispatchMouseEvent", move, 5*time.Second); err != nil {
		return err
	}
	time.Sleep(120 * time.Millisecond)
	press := map[string]any{"type": "mousePressed", "x": x, "y": y, "button": "left", "buttons": 1, "clickCount": 1}
	if _, err := cdpCall(ws, 402, "Input.dispatchMouseEvent", press, 5*time.Second); err != nil {
		return err
	}
	time.Sleep(90 * time.Millisecond)
	release := map[string]any{"type": "mouseReleased", "x": x, "y": y, "button": "left", "buttons": 0, "clickCount": 1}
	if _, err := cdpCall(ws, 403, "Input.dispatchMouseEvent", release, 5*time.Second); err != nil {
		return err
	}
	return nil
}

// claimTaskRewards 领取已完成任务奖励（失败不影响签到结论）。
func claimTaskRewards(ws *wsConn) int {
	if ws == nil {
		return 0
	}
	raw, err := evalJS(ws, 777, claimTasksJS, 40*time.Second)
	if err != nil {
		return 0
	}
	var r struct {
		N int `json:"n"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return 0
	}
	return r.N
}

// wrapTasks 在结论文案后追加任务奖励数量。
func wrapTasks(base string, n int) string {
	if n > 0 {
		return fmt.Sprintf("%s（另领 %d 个任务奖励）", base, n)
	}
	return base
}

// ── 小工具 ───────────────────────────────────────────────

// parseCookieHeader 把 Cookie 头解析成 map。
func parseCookieHeader(cookie string) map[string]string {
	out := map[string]string{}
	for _, item := range strings.Split(cookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(kv) == 2 && kv[0] != "" && kv[1] != "" {
			out[kv[0]] = kv[1]
		}
	}
	return out
}

// clip 截断用于错误提示的页面文本。
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
