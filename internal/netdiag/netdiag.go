// Package netdiag 提供「到某个站点到底通不通」的分通道诊断：直连、环境变量代理、
// Windows 系统代理各跑一遍 DNS → TCP → TLS → HTTP，把每一跳的结果和 TLS 证书
// 签发者列出来。
//
// 存在的理由：程序报「连不上」时，真正的原因至少有四种，靠猜永远猜不准——
//  1. 本机出网被拦（安全软件 / 防火墙 / DNS）
//  2. 系统代理已关闭但注册表 ProxyEnable 还是 1，浏览器与 Chrome 仍往死端口打
//  3. 代理在跑，但规则把国内域名也甩给了境外节点（豆包会 404 / 拒连 / 风控）
//  4. 上游改版（路径 404），网络其实是好的
//
// 分通道跑一遍就能一眼区分上面四种，而不是让用户在「换 Cookie」「关代理」之间瞎试。
package netdiag

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Step 单跳检测结果。
type Step struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Route 一条通路的检测结果（直连 / 某代理）。
type Route struct {
	Label  string `json:"label"`
	Proxy  string `json:"proxy,omitempty"`
	OK     bool   `json:"ok"`
	Status int    `json:"status,omitempty"`
	Steps  []Step `json:"steps"`
}

// proxyNone 表示直连（显式关闭代理）。
const proxyNone = "none"

// Request 描述一次要被重复投递的探测请求（各通路用完全相同的报文，才可比）。
type Request struct {
	Host        string
	Method      string // 空 = GET
	Path        string
	ContentType string
	Body        []byte
	Headers     map[string]string
}

// EnvProxy 读取进程环境变量里的代理（Go 的 http.ProxyFromEnvironment 只认这个）。
func EnvProxy() string {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// normalizeProxy 把 "127.0.0.1:2080" 之类的裸地址补成 URL 形态。
func normalizeProxy(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// "http=127.0.0.1:7890;https=127.0.0.1:7890" 这种按协议分列的写法，取 https 优先
	if strings.Contains(raw, "=") {
		var httpPart, httpsPart string
		for _, seg := range strings.Split(raw, ";") {
			kv := strings.SplitN(strings.TrimSpace(seg), "=", 2)
			if len(kv) != 2 {
				continue
			}
			switch strings.ToLower(kv[0]) {
			case "https":
				httpsPart = kv[1]
			case "http":
				httpPart = kv[1]
			}
		}
		if httpsPart != "" {
			raw = httpsPart
		} else if httpPart != "" {
			raw = httpPart
		}
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return raw
}

// candidates 汇总要试的通路（同一条代理只留一份）。
func candidates() []struct{ label, proxy string } {
	out := []struct{ label, proxy string }{{"直连（不使用代理）", proxyNone}}
	seen := map[string]bool{proxyNone: true}
	add := func(label, raw string) {
		p := normalizeProxy(raw)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, struct{ label, proxy string }{label, p})
	}
	add("环境变量代理", EnvProxy())
	if enabled, server := SystemProxy(); enabled && server != "" {
		add("Windows 系统代理", server)
	}
	return out
}

// Check 对同一请求跑全部通路，返回可读结果。ctx 只控制整体上限。
func Check(ctx context.Context, req Request) []Route {
	if !strings.HasPrefix(req.Path, "/") {
		req.Path = "/" + req.Path
	}
	var routes []Route
	for _, c := range candidates() {
		routes = append(routes, probe(ctx, req, c.label, c.proxy))
	}
	return routes
}

// classifyErr 把底层网络错误翻译成一句能指导用户的话。
func classifyErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	var certErr x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &certErr), errors.As(err, &hostErr), errors.As(err, &invalid),
		strings.Contains(msg, "x509"), strings.Contains(msg, "certificate"):
		return "TLS 证书不被信任（可能存在中间人代理/自签证书）"
	case strings.Contains(msg, "proxyconnect"), strings.Contains(msg, "proxy"):
		return "代理握手失败（端口没在监听，或代理进程已退出）"
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "积极拒绝"),
		strings.Contains(msg, "No connection could be made"):
		return "连接被拒绝（端口未监听 / 安全软件拦截）"
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "找不到主机"),
		strings.Contains(msg, "DNS"):
		return "DNS 解析失败（域名被污染或本机 DNS 不可用）"
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "Timeout"),
		strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"), strings.Contains(msg, "超时"):
		return "连接超时（丢包 / 代理不通 / 被网络设备拦截）"
	case strings.Contains(msg, "reset by peer"), strings.Contains(msg, "EOF"):
		return "连接被对端重置（上游风控或中间设备阻断）"
	}
	return msg
}

// probe 跑单条通路。proxy 为 proxyNone 表示直连。
func probe(ctx context.Context, req Request, label, proxy string) Route {
	host := req.Host
	r := Route{Label: label, Proxy: proxy}
	if proxy == proxyNone {
		r.Proxy = ""
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// 1) 域名解析：直连要本机 DNS；走代理时由代理远端解析
	if proxy == proxyNone {
		t0 := time.Now()
		ips, err := net.DefaultResolver.LookupHost(cctx, host)
		if err != nil {
			r.Steps = append(r.Steps, Step{"DNS 解析", false, classifyErr(err)})
		} else {
			r.Steps = append(r.Steps, Step{"DNS 解析", true,
				fmt.Sprintf("%s → %s（%dms）", host, strings.Join(ips, ", "), time.Since(t0).Milliseconds())})
		}
	} else {
		u, _ := url.Parse(proxy)
		r.Steps = append(r.Steps, Step{"远端解析", true, "由代理 " + u.Host + " 解析域名"})
	}

	// 2) TCP：直连探 443，走代理先探代理端口是否活着（"关了代理但注册表还开着"就死在这一步）
	dialAddr := net.JoinHostPort(host, "443")
	if proxy != proxyNone {
		u, err := url.Parse(proxy)
		if err != nil {
			r.Steps = append(r.Steps, Step{"代理地址", false, "无法解析: " + proxy})
			return r
		}
		dialAddr = u.Host
		if u.Port() == "" {
			dialAddr = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	d := net.Dialer{Timeout: 8 * time.Second}
	t0 := time.Now()
	conn, err := d.DialContext(cctx, "tcp", dialAddr)
	if err != nil {
		r.Steps = append(r.Steps, Step{"TCP 连接", false, dialAddr + " → " + classifyErr(err)})
		return r
	}
	_ = conn.Close()
	r.Steps = append(r.Steps, Step{"TCP 连接", true, fmt.Sprintf("%s 可连（%dms）", dialAddr, time.Since(t0).Milliseconds())})

	// 3) TLS + 4) HTTP：一次请求同时拿到证书签发者和状态码
	var issuer string
	tr := &http.Transport{
		ForceAttemptHTTP2: false,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// 正常校验照做；额外把证书签发者记下来，用来看是否被中间人接管
			VerifyPeerCertificate: func(_ [][]byte, chains [][]*x509.Certificate) error {
				if len(chains) > 0 && len(chains[0]) > 0 {
					issuer = chains[0][0].Issuer.CommonName
				}
				return nil
			},
		},
	}
	if proxy == proxyNone {
		tr.Proxy = nil
	} else {
		pu, _ := url.Parse(proxy)
		tr.Proxy = http.ProxyURL(pu)
	}
	defer tr.CloseIdleConnections()

	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}

	client := &http.Client{
		Timeout:   12 * time.Second,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // 302 本身也是有效信息
		},
	}
	req2, err := http.NewRequestWithContext(cctx, method, "https://"+host+req.Path, bytes.NewReader(req.Body))
	if err != nil {
		r.Steps = append(r.Steps, Step{"构造请求", false, err.Error()})
		return r
	}
	req2.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36")
	if req.ContentType != "" {
		req2.Header.Set("Content-Type", req.ContentType)
	}
	for k, v := range req.Headers {
		req2.Header.Set(k, v)
	}
	t0 = time.Now()
	resp, err := client.Do(req2)
	if err != nil {
		r.Steps = append(r.Steps, Step{"HTTPS 请求", false, classifyErr(err)})
		return r
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	elapsed := time.Since(t0).Milliseconds()

	if issuer != "" {
		r.Steps = append(r.Steps, Step{"TLS 证书", true, "签发者: " + issuer})
	}
	body := strings.Join(strings.Fields(string(raw)), " ")
	if len([]rune(body)) > 100 {
		body = string([]rune(body)[:100]) + "…"
	}
	r.Status = resp.StatusCode
	okHTTP := resp.StatusCode < 500
	r.Steps = append(r.Steps, Step{"HTTPS 请求", okHTTP,
		fmt.Sprintf("HTTP %d（%dms）%s", resp.StatusCode, elapsed, body)})
	// 只有前四跳都过且 HTTP 状态不是 5xx 才认为这条通路可用
	r.OK = okHTTP
	for _, s := range r.Steps {
		if !s.OK {
			r.OK = false
			break
		}
	}
	return r
}

// Report 一次完整诊断：分通路明细 + 结论 + 额外提示（由调用方补充业务判断）。
type Report struct {
	Host    string   `json:"host"`
	Path    string   `json:"path"`
	Method  string   `json:"method"`
	Routes  []Route  `json:"routes"`
	Summary []string `json:"summary"`
	Extra   []string `json:"extra,omitempty"`
}

// Run 跑诊断并组装 Report；extra 为调用方追加的业务提示（可为 nil）。
func Run(ctx context.Context, req Request, extra []string) Report {
	routes := Check(ctx, req)
	return Report{
		Host:    req.Host,
		Path:    req.Path,
		Method:  req.Method,
		Routes:  routes,
		Summary: Summary(req.Host, routes),
		Extra:   extra,
	}
}

// Summary 把分通道结果压成几句结论 + 建议，面板直接展示。
func Summary(host string, routes []Route) []string {
	if len(routes) == 0 {
		return []string{"没有可用的检测通路"}
	}
	var directOK, proxyOK bool
	var proxyBroken []string
	// 证书签发者按通路收集：跨通路不一致才说明某条链路被接管（中间人代理）
	issuerOf := map[string]string{}
	var directIssuer string
	for _, r := range routes {
		for _, s := range r.Steps {
			if s.Name == "TLS 证书" && s.OK && issuerOf[r.Label] == "" {
				issuerOf[r.Label] = strings.TrimPrefix(s.Detail, "签发者: ")
			}
		}
		if r.Proxy == "" {
			directOK = r.OK
			directIssuer = issuerOf[r.Label]
			continue
		}
		if r.OK {
			proxyOK = true
		} else {
			proxyBroken = append(proxyBroken, r.Label)
		}
	}
	var mitm []string
	for label, is := range issuerOf {
		if directIssuer != "" && is != directIssuer && label != "" {
			mitm = append(mitm, label+" 的证书签发者与直连不同（"+is+" ≠ "+directIssuer+"）")
		}
	}

	var out []string
	switch {
	case directOK && len(proxyBroken) == 0:
		out = append(out, "✓ 直连与代理都能到达 "+host+"，网络不是问题。")
		out = append(out, "  → 那问题不在网络：看下面的 HTTP 状态码。200 说明链路通、只是凭证无效；404 说明上游路径改版。")
	case directOK && len(proxyBroken) > 0:
		out = append(out, "△ 直连正常，但 "+strings.Join(proxyBroken, "、")+" 到不了 "+host+"。")
		out = append(out, "  → 浏览器/无头 Chrome 会走系统代理，所以网页侧会「连不上」；程序侧应让豆包走直连。")
		out = append(out, "  → 若是代理软件已关闭：注册表 ProxyEnable 仍是 1，去「Internet 选项 → 连接 → 局域网设置」取消勾选，或重新打开代理软件后再关闭一次。")
	case !directOK && proxyOK:
		out = append(out, "△ 直连到不了 "+host+"，但代理能到。")
		out = append(out, "  → 说明本机出网被拦（安全软件/防火墙/DNS），或者该域名必须走代理。给程序配上代理即可。")
	default:
		out = append(out, "✗ 所有通路都到不了 "+host+"。")
		out = append(out, "  → 先排除本机因素：浏览器能否打开该站点？同一网络下手机能否打开？")
		out = append(out, "  → 若浏览器能开而程序不能：多半是安全软件拦了本程序的出站连接，给它加白名单。")
	}
	if len(mitm) > 0 {
		out = append(out, "! 检测到不同通路看到的 TLS 证书不一致，存在中间人代理的迹象：")
		for _, m := range mitm {
			out = append(out, "  · "+m)
		}
	}
	return out
}
