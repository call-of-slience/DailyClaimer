// Package minimax 实现 MiniMax（MiniMax Code 客户端）provider：
// 设备码登录纳管、凭证导入、token 续期、每日签到、积分额度、模型目录、对话转发。
//
// 协议依据（对上游客户端实测，2026-10）：
//
//	POST {account}/oauth2/device/code           设备码申请（必须带 S256 PKCE）
//	POST {account}/oauth2/token                 device_code / refresh_token 换 token
//	GET  {agent}/minimax-cloud/api/v1/signin/status?timezone_id=Asia/Shanghai
//	POST {agent}/minimax-cloud/api/v1/signin/claim?timezone_id=Asia/Shanghai  {}
//	GET  {agent}/minimax-cloud/api/v1/credit/details
//	GET  {agent}/matrix/api/v1/user/profile
//	GET  {agent}/mavis/api/v1/models
//	POST {agent}/mavis/api/v1/llm/v1/messages   Anthropic Messages 协议
//
// 三个与别家不同的要点：
//  1. access token 只有 1 小时，号池里的号没人替它续，必须自己用 refresh_token 刷；
//  2. 签到是**账号级**接口，只需 Bearer token，不需要切号、不需要客户端在线；
//  3. 国内版（agent.minimax.cn / account.minimax.cn）与国际版（.io）的 token
//     签发域与校验域必须一致，用 CN 端点刷国际号必然 invalid_grant。
package minimax

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"chinacpa/internal/model"
	"chinacpa/internal/provider"
)

// 区域标识（与 workbuddy / qoder 的 Realm 语义一致）。
const (
	RegionCN     = "cn"
	RegionGlobal = "global"
)

const (
	agentBaseCN     = "https://agent.minimax.cn/mavis/api/v1"
	agentBaseGlobal = "https://agent.minimax.io/mavis/api/v1"
	accountCN       = "https://account.minimax.cn"
	accountGlobal   = "https://account.minimax.io"

	// cloudSuffix 签到/积分网关前缀（与模型网关同 host，不同 path）。
	cloudSuffix = "/minimax-cloud/api/v1"
	// matrixSuffix 用户资料网关前缀。
	matrixSuffix = "/matrix/api/v1"

	signinStatusPath = "/signin/status"
	signinClaimPath  = "/signin/claim"
	creditPath       = "/credit/details"
	profilePath      = "/user/profile"
	modelsPath       = "/models"
	messagesPath     = "/llm/v1/messages"

	// 签到结算时区（客户端取 Intl.DateTimeFormat().resolvedOptions().timeZone）。
	// 可用 config.json 的 minimax.timezone 覆盖。
	defaultTZ = "Asia/Shanghai"

	// OAuth 固定参数（客户端自身用的同一套 grant）。
	oauthClientID = "mcode-public"
	oauthScope    = "agent.default"
	oauthAudience = "agent-backend"

	// 领取结果枚举。
	claimResultClaimed = 1
	claimResultAlready = 2

	// signinDayClaimed 签到面板里「已领」的天状态码。
	signinDayClaimed = 3
)

// NormalizeRegion 把任意写法规整为 cn / global（默认 cn）。
func NormalizeRegion(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "global", "en", "intl", "international", "oversea", "overseas":
		return RegionGlobal
	default:
		return RegionCN
	}
}

// Provider MiniMax 实现（并发安全：除 sessions 外无可变状态）。
type Provider struct {
	region string
	http   *http.Client

	// 端点覆盖（单测注入本地 httptest；生产为零值走官方域名）。
	agentBase     string
	accountOrigin string

	timezone string

	// 设备码登录会话（面板发起 → 浏览器授权 → 面板轮询）。
	mu       sync.Mutex
	sessions map[string]*deviceSession
}

// Region 返回该实例的区域标识。
func (p *Provider) Region() string { return p.region }

// SetTimezone 覆盖签到结算时区（空值保持默认 Asia/Shanghai）。
func (p *Provider) SetTimezone(tz string) {
	if strings.TrimSpace(tz) != "" {
		p.timezone = strings.TrimSpace(tz)
	}
}

func (p *Provider) agentBaseURL() string {
	if p.agentBase != "" {
		return strings.TrimRight(p.agentBase, "/")
	}
	if p.region == RegionGlobal {
		return agentBaseGlobal
	}
	return agentBaseCN
}

func (p *Provider) accountURL() string {
	if p.accountOrigin != "" {
		return strings.TrimRight(p.accountOrigin, "/")
	}
	if p.region == RegionGlobal {
		return accountGlobal
	}
	return accountCN
}

// cloudBase 签到网关（agent host + /minimax-cloud/api/v1）。
func (p *Provider) cloudBase() string {
	base := p.agentBaseURL()
	if i := strings.Index(base, "/mavis/api/"); i >= 0 {
		return base[:i] + cloudSuffix
	}
	return strings.TrimRight(base, "/") + cloudSuffix
}

// matrixBase 用户资料网关（agent host + /matrix/api/v1）。
func (p *Provider) matrixBase() string {
	base := p.agentBaseURL()
	if i := strings.Index(base, "/mavis/api/"); i >= 0 {
		return base[:i] + matrixSuffix
	}
	return strings.TrimRight(base, "/") + matrixSuffix
}

// ── 上游响应公共头 ─────────────────────────────────────

// baseResp 是 minimax-cloud 与 matrix 响应共有的业务状态头。
type baseResp struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

// checkBase 解析 base_resp：状态码非 0 视为业务失败（HTTP 仍是 200）。
func checkBase(raw []byte) error {
	var r struct {
		BaseResp baseResp `json:"base_resp"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("响应不是合法 JSON: %s", truncate(string(raw), 160))
	}
	if r.BaseResp.StatusCode != 0 {
		return &provider.Error{
			Kind: classifyStatus(r.BaseResp.StatusCode, r.BaseResp.StatusMsg),
			Code: fmt.Sprint(r.BaseResp.StatusCode),
			Msg:  r.BaseResp.StatusMsg,
		}
	}
	return nil
}

// classifyStatus 业务码 → 错误分类（1004 = 鉴权类，8 = 限流类为常见约定，
// 其余按客户端错误处理，不罚号）。
func classifyStatus(code int, msg string) provider.ErrKind {
	low := strings.ToLower(msg)
	switch {
	case code == 1004 || strings.Contains(low, "token") || strings.Contains(low, "auth") ||
		strings.Contains(low, "login") || strings.Contains(low, "permission"):
		return provider.ErrAuth
	case code == 1002 || strings.Contains(low, "rate") || strings.Contains(low, "limit") ||
		strings.Contains(low, "frequency") || strings.Contains(low, "quota"):
		return provider.ErrRate
	default:
		return provider.ErrClient
	}
}

// do 发一次带 Bearer 的请求，返回状态码与原始响应体。
func (p *Provider) do(ctx context.Context, method, fullURL, token string, body []byte, timeout time.Duration) (int, []byte, error) {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, fullURL, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return 0, nil, &provider.Error{Kind: provider.ErrUpstream, Msg: "MiniMax 网络错误: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, nil
}

// cloudDo 签到/积分网关请求（自动补 timezone_id）。
func (p *Provider) cloudDo(ctx context.Context, token, method, path string, body []byte) (int, []byte, error) {
	u := p.cloudBase() + path
	if !strings.Contains(path, "timezone_id=") {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		u += sep + "timezone_id=" + url.QueryEscape(p.timezoneID())
	}
	return p.do(ctx, method, u, token, body, 20*time.Second)
}

func (p *Provider) timezoneID() string {
	if p.timezone != "" {
		return p.timezone
	}
	return defaultTZ
}

// ── 签到 ───────────────────────────────────────────────

// SigninDay 签到面板中的一天。
type SigninDay struct {
	DayNo  int     `json:"day_no"`
	Points float64 `json:"points"`
	Status int     `json:"status"` // 3=已领；1=未领
	Today  bool    `json:"is_today"`
}

// SigninStatus 签到面板快照。
type SigninStatus struct {
	Days    []SigninDay
	Today   *SigninDay
	Claimed bool
}

// querySignin 查询签到面板。
func (p *Provider) querySignin(ctx context.Context, token string) (*SigninStatus, error) {
	status, raw, err := p.cloudDo(ctx, token, http.MethodGet, signinStatusPath, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, &provider.Error{Kind: provider.ErrAuth, Status: status, Msg: "MiniMax 凭证已失效（HTTP " + fmt.Sprint(status) + "）：" + truncate(string(raw), 120) + "，请重新登录"}
	}
	if status != http.StatusOK {
		return nil, &provider.Error{Kind: provider.ErrUpstream, Status: status, Msg: "签到状态 HTTP " + fmt.Sprint(status) + ": " + truncate(string(raw), 160)}
	}
	if err := checkBase(raw); err != nil {
		return nil, err
	}
	var r struct {
		Data struct {
			Days []SigninDay `json:"days"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("签到状态解析失败: %w", err)
	}
	out := &SigninStatus{Days: r.Data.Days}
	for i := range out.Days {
		if out.Days[i].Today {
			d := out.Days[i]
			out.Today = &d
			out.Claimed = d.Status == signinDayClaimed
		}
	}
	return out, nil
}

// claimSignin 领取今日积分（幂等：已领返回 result=2）。
func (p *Provider) claimSignin(ctx context.Context, token string) (result int, points float64, dayNo int, err error) {
	status, raw, rerr := p.cloudDo(ctx, token, http.MethodPost, signinClaimPath, []byte("{}"))
	if rerr != nil {
		return 0, 0, 0, rerr
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return 0, 0, 0, &provider.Error{Kind: provider.ErrAuth, Status: status, Msg: "MiniMax 凭证已失效（HTTP " + fmt.Sprint(status) + "）：" + truncate(string(raw), 120) + "，请重新登录"}
	}
	if status != http.StatusOK {
		return 0, 0, 0, &provider.Error{Kind: provider.ErrUpstream, Status: status, Msg: "签到 HTTP " + fmt.Sprint(status) + ": " + truncate(string(raw), 160)}
	}
	if berr := checkBase(raw); berr != nil {
		return 0, 0, 0, berr
	}
	var r struct {
		Data struct {
			ClaimResult int     `json:"claim_result"`
			DayNo       int     `json:"day_no"`
			Points      float64 `json:"points"`
		} `json:"data"`
	}
	if jerr := json.Unmarshal(raw, &r); jerr != nil {
		return 0, 0, 0, fmt.Errorf("签到响应解析失败: %w", jerr)
	}
	return r.Data.ClaimResult, r.Data.Points, r.Data.DayNo, nil
}

// CreditSummary 积分概览。
type CreditSummary struct {
	Remaining float64
	Granted   float64
	ExpireAt  int64 // Unix 毫秒
}

// queryCredits 查询积分明细（汇总剩余/累计/最近到期）。
func (p *Provider) queryCredits(ctx context.Context, token string) (*CreditSummary, error) {
	status, raw, err := p.cloudDo(ctx, token, http.MethodGet, creditPath, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, &provider.Error{Kind: provider.ErrAuth, Status: status, Msg: "MiniMax 凭证已失效（HTTP " + fmt.Sprint(status) + "）：" + truncate(string(raw), 120)}
	}
	if status != http.StatusOK {
		return nil, &provider.Error{Kind: provider.ErrUpstream, Status: status, Msg: "积分接口 HTTP " + fmt.Sprint(status)}
	}
	if berr := checkBase(raw); berr != nil {
		return nil, berr
	}
	var r struct {
		Details []struct {
			Granted   string `json:"granted_amount"`
			Remaining string `json:"remaining_amount"`
			ExpireAt  int64  `json:"expire_at_ms"`
		} `json:"details"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("积分响应解析失败: %w", err)
	}
	out := &CreditSummary{}
	for _, d := range r.Details {
		out.Remaining += parseFloatOr0(d.Remaining)
		out.Granted += parseFloatOr0(d.Granted)
		if d.ExpireAt > 0 && (out.ExpireAt == 0 || d.ExpireAt < out.ExpireAt) {
			out.ExpireAt = d.ExpireAt
		}
	}
	return out, nil
}

// ── 用户资料 ───────────────────────────────────────────

// Profile 账号公开资料。
type Profile struct {
	UserID    string `json:"user_id"`
	UserName  string `json:"user_name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// fetchProfile 用 token 取资料（既做身份识别，也做昵称补齐）。
func (p *Provider) fetchProfile(ctx context.Context, token string) (*Profile, error) {
	status, raw, err := p.do(ctx, http.MethodGet, p.matrixBase()+profilePath, token, nil, 15*time.Second)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, &provider.Error{Kind: provider.ErrAuth, Status: status, Msg: "MiniMax 凭证已失效（HTTP " + fmt.Sprint(status) + "）：" + truncate(string(raw), 120)}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("资料接口 HTTP %d: %s", status, truncate(string(raw), 120))
	}
	var out Profile
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("资料响应解析失败: %w", err)
	}
	return &out, nil
}

// identity 尽力识别账号身份：先 profile，失败退化为 token 指纹。
// 返回 uid / 昵称 / 邮箱；网络异常不视为导入失败。
func (p *Provider) identity(ctx context.Context, token string) (uid, nickname, email string) {
	if prof, err := p.fetchProfile(ctx, token); err == nil && prof != nil {
		uid, nickname, email = prof.UserID, prof.UserName, prof.Email
	}
	if uid == "" {
		uid = "mm-" + fingerprint(token)
	}
	return uid, nickname, email
}

// ── 本地登录态（MiniMax Code 客户端）────────────────────

// localAuthFile 本机 MiniMax Code 的 auth.json 结构：
// records 的键带随机后缀，只能遍历取值。
type localAuthFile struct {
	Records map[string]struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		TokenType    string `json:"tokenType"`
		ExpiresAtMs  int64  `json:"expiresAtMs"`
		ClientID     string `json:"clientId"`
	} `json:"records"`
}

// localAuthCandidates 返回本机可能的 auth.json 路径（按修改时间新→旧）。
// 目录布局：~/.minimax/auth/prod/{cn,global}/{clientId}/auth.json
func localAuthCandidates() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	root := filepath.Join(home, ".minimax", "auth")
	var out []string
	for _, pat := range []string{
		filepath.Join(root, "prod", "*", "*", "auth.json"),
		filepath.Join(root, "*", "*", "*", "auth.json"),
	} {
		ms, _ := filepath.Glob(pat)
		out = append(out, ms...)
	}
	// 简单按修改时间倒序（新登录的排在前面）
	sortByModTimeDesc(out)
	return out
}

// readLocalCredential 读取本机客户端登录态（多账号时取最新一份）。
func readLocalCredential() (accessToken, refreshToken string, expiresAt int64, path string, err error) {
	var lastErr error
	for _, p := range localAuthCandidates() {
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			lastErr = rerr
			continue
		}
		var f localAuthFile
		if jerr := json.Unmarshal(raw, &f); jerr != nil {
			lastErr = fmt.Errorf("%s: %w", p, jerr)
			continue
		}
		for _, rec := range f.Records {
			if strings.TrimSpace(rec.AccessToken) == "" {
				continue
			}
			return rec.AccessToken, rec.RefreshToken, rec.ExpiresAtMs / 1000, p, nil
		}
		lastErr = fmt.Errorf("%s 的 records 里没有 accessToken", p)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("找不到 MiniMax Code 登录态（~/.minimax/auth 下没有 auth.json）")
	}
	return "", "", 0, "", lastErr
}

// ── 小工具 ─────────────────────────────────────────────

func parseFloatOr0(s string) float64 {
	var f float64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%f", &f); err != nil {
		return 0
	}
	return f
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// fingerprint 取 token 的短指纹（上游不返身份时的去重键）。
func fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:10]
}

// sortByModTimeDesc 按修改时间倒序（最新的登录态排最前）。
func sortByModTimeDesc(paths []string) {
	sort.SliceStable(paths, func(i, j int) bool {
		fi, ei := os.Stat(paths[i])
		fj, ej := os.Stat(paths[j])
		if ei != nil || ej != nil {
			return false
		}
		return fi.ModTime().After(fj.ModTime())
	})
}

// classifyHTTP 把 http 状态码映射为错误分类（对话转发用）。
func classifyHTTP(status int) provider.ErrKind {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return provider.ErrAuth
	case status == http.StatusTooManyRequests:
		return provider.ErrRate
	case status >= 500:
		return provider.ErrUpstream
	default:
		return provider.ErrClient
	}
}

// model.Account 便捷取值（避免每处都判 nil）。
func accountToken(a *model.Account) string {
	if a == nil {
		return ""
	}
	return a.AccessToken
}
