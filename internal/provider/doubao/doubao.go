// Package doubao 实现豆包（www.doubao.com 网页版）provider：
// 扫码登录（字节 passport 协议，自动回调纳管）、Cookie 凭证导入（兜底）、
// 文本对话（语言模型）、Seedream 图片生成、Seedance 视频生成。
// 协议为「提交 + 轮询」模型：POST /chat/completion（SSE 读到 SSE_ACK 拿
// conversation_id）→ 轮询 POST /im/chain/single 取增量文本 / 生成产物。
// 协议依据：Doora v0.8.2 逆向（复刻与反代协议文档.md）+ 社区 doubao2api 实测。
package doubao

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"chinacpa/internal/model"
	"chinacpa/internal/netdiag"
	"chinacpa/internal/provider"
)

const dbUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// Provider 豆包实现。无浏览器依赖：扫码登录走字节 passport 协议（StartLogin，
// 打开链接 → 豆包 App 扫码 → 自动回调纳管）；生成请求以整串 Cookie + 生成的
// 设备标识直连网页 API（fp=s_v_web_id 随 Cookie 自带，msToken 可为空）。
type Provider struct {
	http *http.Client // 按 config 决定的默认路由（重定向不跟随：登录链路需手动收每跳 Set-Cookie）

	// httpDirect 强制直连客户端。仅当默认路由走代理时用它做兜底：
	// 代理进程被关掉 / 规则把豆包甩去境外时，Go 侧还能自己救回来，
	// 不至于让用户对着「连不上」反复重贴 Cookie。
	httpDirect *http.Client
	// viaProxy 默认路由是否经过代理（决定是否值得兜底重试）。
	viaProxy bool
	proxyRaw string

	// ImageDailyLimit / VideoDailyLimit 每账号每日生成上限（本地计数，config 注入）
	ImageDailyLimit int64
	VideoDailyLimit int64

	// chatMu 同一账号串行提交（上游会话/风控约束；不同账号可并行）。
	chatMu sync.Map // uid -> *sync.Mutex

	// qrSessions 扫码登录会话表（StartLogin 建、PollLogin 驱动、面板扫码页只读）。
	qrMu       sync.Mutex
	qrSessions map[string]*qrSession

	// ── 每日签到（浏览器通道）─────────────────────────────
	// 豆包签到接口带设备签名，纯站外直连会被判「系统错误」；页面上下文内调用
	// 才带得上完整会话签名，因此走一次无头浏览器（同一时刻只跑一个实例）。
	checkinBrowser    bool
	checkinChromePath string
	browserNoProxy    bool // 无头浏览器强制直连（避免系统代理残留把签到拖死）
	cdpMu             sync.Mutex
}

// New 构造；imageLimit/videoLimit 为每日生成上限（<=0 用默认 50/10）。
func New(imageLimit, videoLimit int64) *Provider {
	if imageLimit <= 0 {
		imageLimit = 50
	}
	if videoLimit <= 0 {
		videoLimit = 10
	}
	p := &Provider{
		ImageDailyLimit: imageLimit,
		VideoDailyLimit: videoLimit,
		qrSessions:      map[string]*qrSession{},
		browserNoProxy:  true,
	}
	p.SetProxy("")
	return p
}

// SetProxy 设置出网路由。mode 取值：
//
//	""/"env"  跟随环境变量 HTTP(S)_PROXY（默认，等价于未配置）
//	"system"  用 Windows 系统代理（Internet 选项里那个；代理软件关闭后可能残留）
//	"direct"  强制直连，完全不用代理（豆包是境内站点，这条通常最稳）
//	"http://127.0.0.1:2080"  指定代理地址
func (p *Provider) SetProxy(mode string) {
	p.proxyRaw = strings.TrimSpace(mode)
	p.http = newDBClient(p.proxyRaw, false)
	proxyUsed := true
	switch strings.ToLower(p.proxyRaw) {
	case "direct", "none", "off":
		proxyUsed = false
	case "":
		proxyUsed = netdiag.EnvProxy() != ""
	case "env":
		proxyUsed = netdiag.EnvProxy() != ""
	case "system":
		en, srv := netdiag.SystemProxy()
		proxyUsed = en && srv != ""
	}
	p.viaProxy = proxyUsed
	if proxyUsed {
		p.httpDirect = newDBClient("direct", false)
	} else {
		p.httpDirect = nil
	}
}

// proxyMode 归一化后的路由模式（日志/面板展示）。
func (p *Provider) proxyMode() string { return p.proxyRaw }

// newDBClient 构造豆包专用客户端。重定向一律不跟随：登录链路要手动收每跳
// Set-Cookie，凭证失效时上游也会 302 到登录页。
func newDBClient(mode string, _ bool) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = true
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "direct", "none", "off":
		tr.Proxy = nil
	case "system":
		// Go 不读注册表，这里显式把系统代理读出来自己用
		if en, srv := netdiag.SystemProxy(); en && srv != "" {
			if u, err := url.Parse(withScheme(srv)); err == nil {
				tr.Proxy = http.ProxyURL(u)
			} else {
				tr.Proxy = nil
			}
		} else {
			tr.Proxy = nil
		}
	case "", "env":
		tr.Proxy = http.ProxyFromEnvironment
	default:
		if u, err := url.Parse(withScheme(mode)); err == nil && u.Host != "" {
			tr.Proxy = http.ProxyURL(u)
		} else {
			tr.Proxy = http.ProxyFromEnvironment
		}
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// withScheme 补全裸地址（"127.0.0.1:2080" → "http://127.0.0.1:2080"）。
func withScheme(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Contains(raw, "://") {
		return raw
	}
	return "http://" + raw
}

// do 发请求；走代理失败时自动改用直连重试一次。
// mk 用于重建请求（重试时 body 已被消费，必须重造）。
func (p *Provider) do(mk func() (*http.Request, error)) (*http.Response, error) {
	req, err := mk()
	if err != nil {
		return nil, err
	}
	resp, err := p.http.Do(req)
	if err == nil || p.httpDirect == nil {
		return resp, err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return resp, err
	}
	req2, mkErr := mk()
	if mkErr != nil {
		return resp, err
	}
	resp2, err2 := p.httpDirect.Do(req2)
	if err2 != nil {
		return resp, err
	}
	log.Printf("豆包：经代理（%s）请求失败（%v），已自动改走直连", p.proxyRaw, err)
	return resp2, nil
}

func (p *Provider) ID() string          { return "doubao" }
func (p *Provider) DisplayName() string { return "豆包 Doubao" }

// lockAccount 单账号互斥锁。
func (p *Provider) lockAccount(uid string) *sync.Mutex {
	m, _ := p.chatMu.LoadOrStore(uid, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// ── 设备标识 / Cookie 工具 ───────────────────────────────

// seedOf 取（或生成）账号设备标识（生成形态对齐上游风控 SDK：device_id 16 位首位 7、
// web_id 19 位前缀 76）。
func (p *Provider) seedOf(a *model.Account) deviceIDs {
	if a.DoubaoSeed != "" {
		var ids deviceIDs
		if json.Unmarshal([]byte(a.DoubaoSeed), &ids) == nil && ids.DeviceID != "" {
			return ids
		}
	}
	ids := deviceIDs{DeviceID: genDeviceID(), WebID: genWebID()}
	if raw, err := json.Marshal(ids); err == nil {
		a.DoubaoSeed = string(raw)
	}
	return ids
}

// cookieValue 从账号 Cookie 串取字段（URL 解码尽力而为）。
func cookieValue(cookie, name string) string {
	for _, item := range strings.Split(cookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(kv) == 2 && kv[0] == name {
			return strings.TrimSpace(kv[1])
		}
	}
	return ""
}

// fpOf 设备指纹（s_v_web_id Cookie；风控关键字段，缺失上游会拒）。
func fpOf(a *model.Account) string { return cookieValue(a.WebCookie, "s_v_web_id") }

// regionOf 账号区域（cookie flow_user_country / store-country-code，兜底 CN）。
func regionOf(a *model.Account) string {
	for _, name := range []string{"flow_user_country", "store-country-code"} {
		if v := strings.ToUpper(cookieValue(a.WebCookie, name)); len(v) == 2 {
			return v
		}
	}
	return "CN"
}

// dbHeaders 通用请求头（同源页面 fetch 等价形态）。
func dbHeaders(a *model.Account, contentType string) map[string]string {
	h := map[string]string{
		"Content-Type": contentType,
		"User-Agent":   dbUserAgent,
		"Origin":       dbOrigin,
		"Referer":      dbOrigin + "/chat/",
		"Cookie":       a.WebCookie,
	}
	return h
}

// dbPost POST JSON（SSE/JSON 通用；重定向已被 client 禁止，3xx 即凭证失效）。
func (p *Provider) dbPost(ctx context.Context, a *model.Account, path string, query url.Values, contentType string, body []byte, extra map[string]string) (*http.Response, error) {
	u := dbOrigin + path + "?" + query.Encode()
	mk := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for k, v := range dbHeaders(a, contentType) {
			req.Header.Set(k, v)
		}
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		return req, nil
	}
	return p.do(mk)
}

// classifyStatus HTTP 状态 → 错误分类。
func classifyStatus(code int) provider.ErrKind {
	switch {
	case code == http.StatusTooManyRequests:
		return provider.ErrQuota
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return provider.ErrAuth
	case code >= 500:
		return provider.ErrUpstream
	}
	return provider.ErrClient
}

// authExpiredError 统一的凭证失效提示。
func authExpiredError(code int, body string) *provider.Error {
	return &provider.Error{
		Kind:   provider.ErrAuth,
		Status: code,
		Msg:    "豆包 Cookie 已失效或被风控（登录 www.doubao.com → F12 → Network → 任一请求 → 复制完整 Cookie 重新导入）。上游响应: " + truncateRunes(body, 120),
	}
}

// ── 登录（扫码，qrlogin.go）/ 导入（Cookie 兜底）────────

// StartLogin / PollLogin 实现见 qrlogin.go（字节 passport 扫码协议）。

// ImportCredential 解析并校验凭证。支持：
//  1. 整串 Cookie（推荐：F12 → Network → 任一 www.doubao.com 请求的 Cookie 头）
//  2. JSON {"cookie":"...","device_id":"...","web_id":"..."}（后两项可选，
//     缺省自动生成并落盘）
func (p *Provider) ImportCredential(ctx context.Context, raw string) (*model.Account, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("凭证为空")
	}
	cookieStr, devID, webID := "", "", ""
	if strings.HasPrefix(raw, "{") {
		var m map[string]string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, fmt.Errorf("JSON 解析失败: %v", err)
		}
		cookieStr = pick(m["cookie"], m["Cookie"])
		devID = pick(m["device_id"], m["deviceID"])
		webID = pick(m["web_id"], m["webID"])
	} else {
		cookieStr = raw
	}
	if !strings.Contains(cookieStr, "=") {
		return nil, fmt.Errorf("未识别的凭证格式：请粘贴 www.doubao.com 请求头的完整 Cookie 值")
	}
	if fp := cookieValue(cookieStr, "s_v_web_id"); fp == "" {
		return nil, fmt.Errorf("Cookie 缺少 s_v_web_id（风控必需字段）；请复制完整的 Cookie 头")
	}
	if cookieValue(cookieStr, "sessionid") == "" && cookieValue(cookieStr, "sid_guard") == "" {
		return nil, fmt.Errorf("Cookie 缺少 sessionid（未登录）；请先登录 www.doubao.com 再复制")
	}
	a := &model.Account{
		Provider:  p.ID(),
		Source:    "web",
		WebCookie: cookieStr,
	}
	if devID != "" || webID != "" {
		if devID == "" {
			devID = genDeviceID()
		}
		if webID == "" {
			webID = genWebID()
		}
		if rawIDs, err := json.Marshal(deviceIDs{DeviceID: devID, WebID: webID}); err == nil {
			a.DoubaoSeed = string(rawIDs)
		}
	}
	// 校验：素材库首页需登录态（302/401 = Cookie 失效）
	ctxv, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := p.validate(ctxv, a); err != nil {
		return nil, err
	}
	p.seedOf(a) // 生成并保存设备标识
	if a.UID == "" {
		sum := sha256.Sum256([]byte("doubao:" + pick(cookieValue(cookieStr, "sessionid"), cookieStr)))
		a.UID = fmt.Sprintf("doubao-%x", sum[:4])
	}
	return a, nil
}

// validate 校验登录态并尽力取用户名（POST /samantha/aispace/homepage）。
func (p *Provider) validate(ctx context.Context, a *model.Account) error {
	ids := p.seedOf(a)
	q := commonQuery(ids, fpOf(a), regionOf(a))
	body, _ := json.Marshal(map[string]any{})
	resp, err := p.dbPost(ctx, a, "/samantha/aispace/homepage", q, "application/json", body, nil)
	if err != nil {
		return fmt.Errorf("校验请求失败（网络层，直连与代理都试过）: %v；"+
			"请在面板豆包卡片点「网络诊断」看是哪一跳断的，或把 config.json → doubao.proxy 设为 direct（境内直连通常最稳）", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return authExpiredError(resp.StatusCode, string(raw))
	}
	if resp.StatusCode == http.StatusNotFound {
		// 404 不是凭证问题：要么路径改版，要么本地网络/代理把请求投到了错误节点。
		// 别让用户对着「Cookie 已失效」的提示反复重贴凭证。
		return fmt.Errorf("豆包接口返回 404（路径不存在，多为上游改版或网络/代理劫持）：%s；请确认浏览器能打开 www.doubao.com 后重试，持续 404 需更新程序",
			truncateRunes(string(raw), 120))
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("校验失败 HTTP %d: %s", resp.StatusCode, truncateRunes(string(raw), 160))
	}
	// 尽力取昵称（响应结构上游未公开，递归找 nickname/name 字段）
	var data any
	if json.Unmarshal(raw, &data) == nil {
		if nick := deepFindString(data, "nickname", 6); nick != "" {
			a.Nickname = nick
		}
		if a.UID == "" {
			if uid := deepFindString(data, "user_id", 6); uid != "" {
				a.UID = "doubao-" + uid
			}
		}
	}
	return nil
}

// deepFindString 递归找指定键的字符串值（限深度防爆）。
func deepFindString(v any, key string, depth int) string {
	if depth <= 0 {
		return ""
	}
	switch t := v.(type) {
	case map[string]any:
		if s, ok := t[key].(string); ok && s != "" {
			return s
		}
		for _, vv := range t {
			if s := deepFindString(vv, key, depth-1); s != "" {
				return s
			}
		}
	case []any:
		for _, vv := range t {
			if s := deepFindString(vv, key, depth-1); s != "" {
				return s
			}
		}
	}
	return ""
}

// pick 第一个非空串。
func pick(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ── 刷新 / 额度 / 签到 ───────────────────────────────────

// RefreshToken Cookie 长效，无可刷新令牌（返回不变更）。
func (p *Provider) RefreshToken(_ context.Context, _ *model.Account) (bool, error) {
	return false, nil
}

// Quota 网页通道无公开额度接口：探测登录态 + 本地日计数（图片/视频/对话）。
func (p *Provider) Quota(ctx context.Context, a *model.Account) (*model.QuotaInfo, error) {
	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	healthy := true
	if err := p.validate(qctx, a); err != nil {
		if pe, ok := err.(*provider.Error); ok && pe.Kind == provider.ErrAuth {
			healthy = false
		} else {
			return nil, err
		}
	}
	detail := p.buildQuotaDetail(a.UID)
	c := usageToday(a.UID)
	// 剩余额度取图片/视频两类中较小者（主展示值）；对话不设限
	remaining := int64(-1)
	if r := p.ImageDailyLimit - c.images; p.ImageDailyLimit > 0 && (remaining < 0 || r < remaining) {
		remaining = r
	}
	if r := p.VideoDailyLimit - c.videos; p.VideoDailyLimit > 0 && (remaining < 0 || r < remaining) {
		remaining = r
	}
	if remaining < 0 {
		remaining = 0
	}
	return &model.QuotaInfo{
		Provider: p.ID(), UID: a.UID,
		Used:      float64(c.images + c.videos),
		Limit:     float64(p.ImageDailyLimit + p.VideoDailyLimit),
		Remaining: float64(remaining),
		Unit:      "generations/day", Daily: true,
		Healthy: healthy && remaining > 0,
		Detail:  detail,
	}, nil
}

// Checkin 每日签到。
// 豆包签到接口带设备签名、且活动页是渐进水合的前端应用，纯站外直连拿不到
// 可靠结论；因此走一次无头浏览器（页面上下文内调接口 + 必要时点按钮）。
// 未开启浏览器通道时返回「说明型」结果而非失败——聊天用户不该因为没配
// 签到通道就看到一片红色。
func (p *Provider) Checkin(ctx context.Context, a *model.Account) (*model.CheckinResult, error) {
	if !p.checkinBrowser {
		return &model.CheckinResult{
			OK:      false,
			Info:    true,
			Message: "豆包签到需在 config.json 打开 doubao.checkin_via_browser",
		}, nil
	}
	return p.checkinViaBrowser(ctx, a), nil
}

// ── 模型目录 ─────────────────────────────────────────────

// Models 内置目录：文本（网页默认助手）+ Seedream 图片 + Seedance 视频。
// Kind 标注类别，发布编辑器按此分组图片/视频渠道候选。
func (p *Provider) Models(_ context.Context, _ *model.Account) ([]model.ModelInfo, error) {
	out := []model.ModelInfo{
		{ID: textModelID, Name: "豆包对话（网页助手）", Provider: p.ID(), Kind: "chat",
			ContextWindow: 131072, MaxTokens: 8192},
	}
	for _, m := range imageModels {
		out = append(out, model.ModelInfo{ID: m.Public, Name: m.Upstream, Provider: p.ID(),
			Kind: "image", SupportsImages: true})
	}
	for _, m := range videoModels {
		out = append(out, model.ModelInfo{ID: m.Public, Name: m.Upstream, Provider: p.ID(), Kind: "video"})
	}
	return out, nil
}

// ── 提交与轮询引擎 ───────────────────────────────────────

// submit 提交生成请求：POST /chat/completion（SSE 读到 SSE_ACK 即收流）。
func (p *Provider) submit(ctx context.Context, a *model.Account, payload map[string]any) (*sseAck, error) {
	// ext.fp 注入（buildPayload 留空）
	if ext, ok := payload["ext"].(map[string]any); ok {
		ext["fp"] = fpOf(a)
	}
	ids := p.seedOf(a)
	q := commonQuery(ids, fpOf(a), regionOf(a))
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	extra := map[string]string{
		"agw-js-conv":   "str, str",
		"last-event-id": "undefined",
		"x-flow-trace":  flowTrace(),
		"Accept":        "text/event-stream",
	}
	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := p.dbPost(reqCtx, a, "/chat/completion", q, "application/json", body, extra)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
			(resp.StatusCode >= 300 && resp.StatusCode < 400) {
			return nil, authExpiredError(resp.StatusCode, string(raw))
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, &provider.Error{Kind: provider.ErrQuota, Status: resp.StatusCode,
				Msg: "上游限流: " + truncateRunes(string(raw), 160)}
		}
		return nil, &provider.Error{Kind: classifyStatus(resp.StatusCode), Status: resp.StatusCode,
			Msg: fmt.Sprintf("提交失败 HTTP %d: %s", resp.StatusCode, truncateRunes(string(raw), 200))}
	}
	// SSE 增量读：见到 SSE_ACK / STREAM_ERROR 事件（其后跟随空行）即断流，
	// 生成结果改走 /im/chain/single 轮询（Doora 同款策略）
	var sb strings.Builder
	chunk := make([]byte, 16*1024)
	readDone := func() bool {
		s := sb.String()
		for _, marker := range []string{"SSE_ACK", "STREAM_ERROR"} {
			if i := strings.Index(s, marker); i >= 0 && strings.Contains(s[i:], "\n\n") {
				return true
			}
		}
		return sb.Len() > 2<<20 // 防御性上限
	}
	for {
		n, rerr := resp.Body.Read(chunk)
		if n > 0 {
			sb.Write(chunk[:n])
			if readDone() {
				break
			}
		}
		if rerr != nil {
			break // EOF / 超时 / 连接重置：交给 parseSseAck 判定
		}
	}
	return parseSseAck(sb.String())
}

// pollChain 轮询一次会话链。
func (p *Provider) pollChain(ctx context.Context, a *model.Account, conversationID string) (*chainResponse, error) {
	ids := p.seedOf(a)
	q := commonQuery(ids, fpOf(a), regionOf(a))
	body, err := json.Marshal(chainBody(conversationID))
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := p.dbPost(reqCtx, a, "/im/chain/single", q, "application/json; encoding=utf-8", body, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
			(resp.StatusCode >= 300 && resp.StatusCode < 400) {
			return nil, authExpiredError(resp.StatusCode, string(raw))
		}
		return nil, fmt.Errorf("轮询失败 HTTP %d: %s", resp.StatusCode, truncateRunes(string(raw), 160))
	}
	var cr chainResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, fmt.Errorf("轮询响应解析失败: %v", err)
	}
	return &cr, nil
}

// sendConfirm 发送确认轮消息（上游反问参数时）。
func (p *Provider) sendConfirm(ctx context.Context, a *model.Account, conversationID, text string) error {
	payload := confirmPayload(conversationID, text)
	_, err := p.submit(ctx, a, payload)
	return err
}

// ── ChatStream（语言模型）────────────────────────────────

// ChatStream OpenAI 兼容对话 → 豆包网页助手。
// 上游是「提交 + 轮询」模型，流式为轮询增量模拟（每次轮询到新文本即推 delta）；
// 非流式则同步跑完整个轮询返回完整 JSON。
func (p *Provider) ChatStream(ctx context.Context, a *model.Account, modelName string, body []byte) (*provider.ChatResult, error) {
	_ = resolveModel(modelName) // 文本通道不选上游模型（网页默认助手）
	var req struct {
		Stream   *bool `json:"stream"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		EnableThinking *bool `json:"enable_thinking"`
		Thinking       *struct {
			Enabled *bool `json:"enabled"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	prompt := flattenMessages(req.Messages)
	deepThink := false
	if req.EnableThinking != nil {
		deepThink = *req.EnableThinking
	}
	if req.Thinking != nil && req.Thinking.Enabled != nil {
		deepThink = *req.Thinking.Enabled
	}

	unlock := p.lockAccount(a.UID)
	unlock.Lock()
	defer unlock.Unlock()

	ack, err := p.submit(ctx, a, submitPayload("", prompt, nil, deepThink))
	if err != nil {
		return nil, err
	}
	countChat(a.UID)

	stream := req.Stream != nil && *req.Stream
	if !stream {
		// 非流式：同步轮询到完成，返回完整 OpenAI JSON
		text, cerr := p.awaitText(ctx, a, ack.ConversationID, 240*time.Second)
		if cerr != nil {
			return nil, cerr
		}
		out, _ := json.Marshal(openAICompletion(textModelID, text))
		return &provider.ChatResult{
			Body:        io.NopCloser(bytes.NewReader(out)),
			ContentType: "application/json",
		}, nil
	}

	// 流式：后台轮询 → 增量 OpenAI chunk
	pr, pw := io.Pipe()
	go func() {
		p.streamText(ctx, a, ack.ConversationID, pw)
	}()
	return &provider.ChatResult{Body: pr, ContentType: "text/event-stream"}, nil
}

// streamText 轮询文本并增量写 OpenAI SSE（出错时写错误事件后 [DONE]）。
func (p *Provider) streamText(ctx context.Context, a *model.Account, conversationID string, w io.Writer) {
	chunkID := "chatcmpl-doubao-" + conversationID
	emit := func(delta map[string]any, finish string) {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		chunk := map[string]any{
			"id": chunkID, "object": "chat.completion.chunk", "model": textModelID,
			"choices": []any{choice},
		}
		raw, _ := json.Marshal(chunk)
		_, _ = w.Write(append(append([]byte("data: "), raw...), '\n', '\n'))
	}
	sent := ""
	stable := 0
	deadline := time.Now().Add(240 * time.Second)
	confirmRounds := 0
	fail := func(msg string) {
		errPayload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error"}})
		_, _ = w.Write(append(append([]byte("data: "), errPayload...), '\n', '\n'))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
	for time.Now().Before(deadline) {
		cr, err := p.pollChain(ctx, a, conversationID)
		if err != nil {
			if pe, ok := err.(*provider.Error); ok && pe.Kind == provider.ErrAuth {
				fail(err.Error())
				return
			}
			time.Sleep(3 * time.Second)
			continue
		}
		if readClarifyState(cr) && confirmRounds < 4 {
			confirmRounds++
			_ = p.sendConfirm(ctx, a, conversationID, "确认")
			time.Sleep(2 * time.Second)
			continue
		}
		text := parseConversationText(cr)
		if text != "" {
			if kw, _ := hitFailKeyword(text); kw != "" {
				fail("上游失败：" + text)
				return
			}
			if len(text) > len(sent) {
				delta := text[len(sent):]
				sent = text
				stable = 0
				emit(map[string]any{"content": delta}, "")
			} else if len(text) == len(sent) && sent != "" {
				// 文本连续两轮不变视为完成（豆包文本通常整段刷新）
				stable++
				if stable >= 2 {
					emit(map[string]any{}, "stop")
					_, _ = w.Write([]byte("data: [DONE]\n\n"))
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2500 * time.Millisecond):
		}
	}
	fail("豆包回复超时")
}

// awaitText 阻塞轮询直到助手文本稳定（或超时）。
func (p *Provider) awaitText(ctx context.Context, a *model.Account, conversationID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	sent, stable := "", 0
	confirmRounds := 0
	for time.Now().Before(deadline) {
		cr, err := p.pollChain(ctx, a, conversationID)
		if err != nil {
			if pe, ok := err.(*provider.Error); ok && pe.Kind == provider.ErrAuth {
				return "", err
			}
			time.Sleep(3 * time.Second)
			continue
		}
		if readClarifyState(cr) && confirmRounds < 4 {
			confirmRounds++
			_ = p.sendConfirm(ctx, a, conversationID, "确认")
			time.Sleep(2 * time.Second)
			continue
		}
		text := parseConversationText(cr)
		if text != "" {
			if kw, quota := hitFailKeyword(text); kw != "" {
				kind := provider.ErrUpstream
				if quota {
					kind = provider.ErrQuota
				}
				return "", &provider.Error{Kind: kind, Msg: "上游失败: " + truncateRunes(text, 300)}
			}
			if text == sent {
				stable++
				if stable >= 2 {
					return sent, nil
				}
			} else {
				sent, stable = text, 0
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2500 * time.Millisecond):
		}
	}
	if sent != "" {
		return sent, nil
	}
	return "", &provider.Error{Kind: provider.ErrUpstream, Msg: "豆包回复超时"}
}

// flattenMessages OpenAI messages → 单条用户文本（网页协议无多轮结构，历史平铺）。
func flattenMessages(messages []struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}) string {
	var parts []string
	hasSystem := false
	for _, m := range messages {
		text := flattenContent(m.Content)
		if text == "" {
			continue
		}
		if m.Role == "system" || m.Role == "developer" {
			hasSystem = true
			parts = append(parts, "system:\n\n"+text)
			continue
		}
		role := m.Role
		if role != "assistant" && role != "user" {
			role = "user"
		}
		parts = append(parts, role+": "+text)
	}
	joined := strings.Join(parts, "\n\n")
	if !hasSystem && joined != "" {
		joined = "system:\n\n" + joined
	}
	if joined == "" {
		joined = "你好"
	}
	return joined
}

// flattenContent OpenAI content（字符串或分段数组）→ 纯文本。
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var arr []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &arr) == nil {
		var out []string
		for _, part := range arr {
			if part.Text != "" {
				out = append(out, part.Text)
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}

// openAICompletion 非流式响应体。
func openAICompletion(modelName, text string) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-doubao-" + randomLocalId(),
		"object":  "chat.completion",
		"created": timeNowMs() / 1000,
		"model":   modelName,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
}
