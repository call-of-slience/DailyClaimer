// browserlogin.go 豆包「窗口登录」通道——扫码接口挂掉时的兜底。
//
// 背景：passport 的扫码接口（get_qrcode / qrlogin）一旦改版、被风控或本地代理劫持，
// 整条扫码链路就断了，而且断在哪一层对使用者完全不可见。
// 这里换一个只依赖 doubao.com 本身的思路：
//
//  1. 起一个**可见**的 Chrome/Edge 窗口（独立临时用户目录，不碰你日常配置）
//  2. 打开豆包首页，你自己登录（手机号验证码 / 扫码都行，页面怎么变都能用）
//  3. 程序通过 CDP 读浏览器的 Cookie 库，拿到 sessionid 就收工
//
// 与扫码通道的区别：不调豆包任何「登录接口」，只借用浏览器的登录结果，
// 因此上游接口改版不影响；代价是需要用户手动点几下。
package doubao

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// browserLoginPage 打开哪个页面：首页即可（已登录会直接进对话页）。
const browserLoginPage = dbOrigin + "/chat/"

// cdpCookie 是 CDP Network.getAllCookies 返回的单条 Cookie。
type cdpCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
}

// BrowserLogin 打开可见浏览器等用户登录，成功后返回可直接入库的 Cookie 串。
// onState 用于向前端推进度（status: opening / waiting / reading / success）。
func (p *Provider) BrowserLogin(ctx context.Context, onState func(status, msg string)) (string, error) {
	state := func(s, m string) {
		if onState != nil {
			onState(s, m)
		}
	}

	state("opening", "正在启动浏览器…")
	chrome, err := p.findChrome()
	if err != nil {
		return "", err
	}

	// 与签到通道共用一把锁：两个 Chrome 同时抢调试端口会互相打架
	p.cdpMu.Lock()
	defer p.cdpMu.Unlock()

	port, err := freeDebugPort()
	if err != nil {
		return "", fmt.Errorf("没有可用的调试端口: %w", err)
	}
	dir, err := os.MkdirTemp("", "chinacpa-login-*")
	if err != nil {
		return "", fmt.Errorf("创建临时浏览器目录失败: %w", err)
	}
	defer os.RemoveAll(dir)

	args := []string{
		// 注意：这里**不加** --headless，用户要能看到窗口自己登录
		"--no-first-run", "--no-default-browser-check", "--disable-extensions",
		"--window-size=460,760",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir=" + dir,
	}
	if p.browserNoProxy {
		// 豆包是境内站点；系统代理残留会让登录页直接打不开
		args = append(args, "--no-proxy-server")
	}
	args = append(args, browserLoginPage)

	cmd := exec.Command(chrome, args...)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("启动浏览器失败（可检查 doubao.chrome_path）: %w", err)
	}
	closed := make(chan struct{})
	go func() { _ = cmd.Wait(); close(closed) }()
	defer func() {
		closeBrowserViaCDP(port)
		if cmd.Process != nil {
			waitProcessExit(cmd.Process, 4*time.Second)
		}
	}()

	state("waiting", "请在弹出的浏览器窗口里登录豆包（登录完成会自动收尾）")
	wsURL, err := waitDebugTarget(port, 25*time.Second)
	if err != nil {
		return "", fmt.Errorf("浏览器调试端口未就绪: %w", err)
	}
	ws, err := wsDial(wsURL, 5*time.Second)
	if err != nil {
		return "", fmt.Errorf("连接浏览器失败: %w", err)
	}
	defer ws.Close()

	if _, err := cdpCall(ws, 1, "Network.enable", map[string]any{}, 5*time.Second); err != nil {
		return "", fmt.Errorf("初始化浏览器会话失败: %w", err)
	}

	deadline := time.Now().Add(8 * time.Minute)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	closedHits := 0
	for seq := 0; time.Now().Before(deadline); seq++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-closed:
			return "", fmt.Errorf("浏览器窗口已被关闭，登录未完成")
		case <-time.After(1500 * time.Millisecond):
		}

		raw, err := cdpCall(ws, 100+seq, "Network.getAllCookies", map[string]any{}, 5*time.Second)
		if err != nil {
			// 窗口被关掉时 CDP 会直接报错，重试几次仍失败就认输
			if closedHits++; closedHits >= 3 {
				return "", fmt.Errorf("与浏览器的连接已断开（窗口被关闭？）")
			}
			continue
		}
		closedHits = 0

		var res struct {
			Cookies []cdpCookie `json:"cookies"`
		}
		if json.Unmarshal(raw, &res) != nil {
			continue
		}
		header := doubaoCookieHeader(res.Cookies)
		if !strings.Contains(header, "sessionid=") {
			continue
		}
		state("reading", "已检测到登录态，正在校验…")
		return header, nil
	}
	return "", fmt.Errorf("等待登录超时（8 分钟），请重试")
}

// doubaoCookieHeader 只保留 doubao.com 域下的 Cookie，拼成 Cookie 请求头。
// 值里可能有空格/分号，直接原样拼接（豆包自身也不做编码）。
func doubaoCookieHeader(cookies []cdpCookie) string {
	var b strings.Builder
	seen := map[string]bool{}
	for _, c := range cookies {
		if c.Name == "" || c.Value == "" || seen[c.Name] {
			continue
		}
		if !strings.Contains(c.Domain, "doubao.com") {
			continue
		}
		seen[c.Name] = true
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(c.Name)
		b.WriteString("=")
		b.WriteString(c.Value)
	}
	return b.String()
}
