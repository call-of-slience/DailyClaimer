package minimax

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"chinacpa/internal/model"
	"chinacpa/internal/provider"
)

// refreshAhead 续期提前量：距过期不足 5 分钟才刷（见 RefreshToken 的轮换说明）。
const refreshAhead = 300

// 凭证来源（落盘到 Account.Source，决定能否由网关代为续期）。
const (
	// sourceLocal 从本机 MiniMax Code 客户端读取：RT 与客户端共用，网关不代续。
	sourceLocal = "local"
	// sourceJSON 粘贴 auth.json / 简化 JSON 导入。
	sourceJSON = "json"
	// sourcePaste 只粘贴了 access token（无 RT 则无法续期）。
	sourcePaste = "paste"
)

// New 构造指定区域的 MiniMax provider（cn / global）。
// 国际版账号的 token 只能在 .io 域续期与签到，因此区域必须在实例上固定。
func New(region string) *Provider {
	return &Provider{
		region:   NormalizeRegion(region),
		http:     &http.Client{Timeout: 120 * time.Second},
		sessions: map[string]*deviceSession{},
	}
}

func (p *Provider) ID() string {
	if p.region == RegionGlobal {
		return "minimax-intl"
	}
	return "minimax"
}

func (p *Provider) DisplayName() string {
	if p.region == RegionGlobal {
		return "MiniMax 国际版"
	}
	return "MiniMax"
}

// ── 凭证导入 ───────────────────────────────────────────

// ImportCredential 支持四种输入：
//  1. 空 / local / client / 本机 → 读 ~/.minimax/auth 下 MiniMax Code 的登录态；
//  2. auth.json 全文（含 records）；
//  3. JSON {"access_token":…,"refresh_token":…}；
//  4. 裸 access token（JWT 或 mmt_ 开头）。
//
// 身份优先用 /user/profile 查真实 user_id（与上游一致：token 指纹不能证明身份，
// 但拿不到时退化为指纹，保证导入不因网络问题失败）。
func (p *Provider) ImportCredential(ctx context.Context, raw string) (*model.Account, error) {
	in := strings.TrimSpace(raw)
	var (
		accessToken, refreshToken string
		expiresAt                 int64
		source                    string
	)

	switch {
	case in == "" || isLocalAlias(in):
		at, rt, exp, path, err := readLocalCredential()
		if err != nil {
			return nil, fmt.Errorf("读取本机 MiniMax 登录态失败：%w（也可以粘贴 auth.json 内容）", err)
		}
		accessToken, refreshToken, expiresAt = at, rt, exp
		source = sourceLocal
		if p.region == RegionCN && strings.Contains(strings.ToLower(path), "global") {
			return nil, fmt.Errorf("本机登录态是国际版账号，请用「MiniMax 国际版」导入")
		}
	case strings.HasPrefix(in, "{"):
		at, rt, exp, err := parseCredentialJSON(in)
		if err != nil {
			return nil, err
		}
		accessToken, refreshToken, expiresAt = at, rt, exp
		source = sourceJSON
	default:
		accessToken = in
		source = sourcePaste
	}

	accessToken = strings.TrimSpace(strings.TrimPrefix(accessToken, "Bearer "))
	if accessToken == "" {
		return nil, fmt.Errorf("没有可用的 access token")
	}

	a := &model.Account{
		Provider:     p.ID(),
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		Realm:        p.region,
		Source:       source,
	}
	a.UID, a.Nickname, a.Email = p.identity(ctx, accessToken)
	return a, nil
}

// isLocalAlias 判断「从本机客户端读取」的输入别名。
func isLocalAlias(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "local", "client", "本机", "~", "~/.minimax":
		return true
	}
	return false
}

// parseCredentialJSON 兼容 auth.json 全文与简化 JSON。
func parseCredentialJSON(raw string) (accessToken, refreshToken string, expiresAt int64, err error) {
	// 先按客户端 auth.json 解析（records 结构）。
	var f localAuthFile
	if json.Unmarshal([]byte(raw), &f) == nil && len(f.Records) > 0 {
		for _, rec := range f.Records {
			if strings.TrimSpace(rec.AccessToken) == "" {
				continue
			}
			return rec.AccessToken, rec.RefreshToken, rec.ExpiresAtMs / 1000, nil
		}
		return "", "", 0, fmt.Errorf("auth.json 的 records 里没有 accessToken")
	}

	var simple struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    int64  `json:"expires_at"`
		ExpiresAtMs  int64  `json:"expires_at_ms"`
		ExpiresIn    int64  `json:"expires_in"`
		// 驼峰写法（客户端导出的另一种形态）
		AccessTokenCamel  string `json:"accessToken"`
		RefreshTokenCamel string `json:"refreshToken"`
	}
	if err := json.Unmarshal([]byte(raw), &simple); err != nil {
		return "", "", 0, fmt.Errorf("凭证 JSON 解析失败: %w", err)
	}
	at := firstNonEmpty(simple.AccessToken, simple.AccessTokenCamel)
	if at == "" {
		return "", "", 0, fmt.Errorf("JSON 里没有 access_token")
	}
	rt := firstNonEmpty(simple.RefreshToken, simple.RefreshTokenCamel)
	switch {
	case simple.ExpiresAt > 0:
		expiresAt = simple.ExpiresAt
	case simple.ExpiresAtMs > 0:
		expiresAt = simple.ExpiresAtMs / 1000
	case simple.ExpiresIn > 0:
		expiresAt = time.Now().Unix() + simple.ExpiresIn
	}
	return at, rt, expiresAt, nil
}

// ── token 续期 ─────────────────────────────────────────

// Refreshed 一次续期的结果。
type Refreshed struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
}

// refresh 用 refresh_token 换新 token（表单编码，scope/audience 必填）。
func (p *Provider) refresh(ctx context.Context, refreshToken string) (*Refreshed, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("没有 refresh_token，无法续期")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", oauthClientID)
	// scope / audience 必填：缺了上游直接 400 invalid_request（实测）。
	form.Set("scope", oauthScope)
	form.Set("audience", oauthAudience)

	raw, status, err := p.postForm(ctx, p.accountURL()+deviceTokenPath, form)
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrUpstream, Msg: "续期请求失败: " + err.Error()}
	}
	if status != http.StatusOK {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := firstNonEmpty(e.Description, e.Error, truncate(string(raw), 160))
		kind := provider.ErrUpstream
		if e.Error == "invalid_grant" || status == http.StatusBadRequest || status == http.StatusUnauthorized {
			kind = provider.ErrAuth
			msg = "refresh_token 已失效（" + msg + "），请重新登录该账号"
		}
		return nil, &provider.Error{Kind: kind, Status: status, Code: e.Error, Msg: msg}
	}
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.AccessToken == "" {
		return nil, fmt.Errorf("续期响应异常: %s", truncate(string(raw), 160))
	}
	out := &Refreshed{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken}
	if out.RefreshToken == "" {
		out.RefreshToken = refreshToken // 上游未轮换时保留原值
	}
	if r.ExpiresIn > 0 {
		out.ExpiresAt = time.Now().Unix() + r.ExpiresIn
	}
	return out, nil
}

// RefreshToken 实现 provider 接口：刷新并就地更新账号（调用方负责落盘）。
//
// 这里有一道「不必刷就不刷」的闸门，原因是 MiniMax 的续期会**轮换
// refresh_token**：每刷一次，旧的 RT 立即作废。而同一个账号的 RT 很可能正是
// 用户本机 MiniMax Code 客户端正在用的那一份——网关抢着刷，客户端下次续期就会
// invalid_grant 掉登录。账号池的 EnsureFreshToken 是无条件调用本方法的，
// 所以未临期时直接返回未变更，把续期机会留给真正需要的时候。
func (p *Provider) RefreshToken(ctx context.Context, a *model.Account) (bool, error) {
	if a == nil {
		return false, fmt.Errorf("账号为空")
	}
	if a.ExpiresAt > 0 && time.Now().Unix()+refreshAhead < a.ExpiresAt {
		return false, nil
	}
	// 本机客户端的登录态不能代续：MiniMax 续期会轮换 refresh_token，而这份 RT
	// 很可能是 MiniMax Code 客户端正在用的那一份，谁先刷谁就让对方掉登录。
	// 因此只接受「本来就属于网关」的凭证续期（设备码登录 / 手工粘贴的都是独立会话）。
	if a.Source == sourceLocal {
		return false, &provider.Error{
			Kind: provider.ErrAuth,
			Msg: "本机登录态与 MiniMax Code 客户端共用同一份 refresh_token，网关不代续期（" +
				"续期会顶掉客户端登录）。到期后请用面板的「设备码授权」重新接入 MiniMax。",
		}
	}
	if strings.TrimSpace(a.RefreshToken) == "" {
		return false, &provider.Error{Kind: provider.ErrAuth, Msg: "该账号没有 refresh_token，请重新登录导入"}
	}
	nw, err := p.refresh(ctx, a.RefreshToken)
	if err != nil {
		return false, err
	}
	changed := nw.AccessToken != a.AccessToken || nw.RefreshToken != a.RefreshToken
	a.AccessToken, a.RefreshToken = nw.AccessToken, nw.RefreshToken
	if nw.ExpiresAt > 0 {
		a.ExpiresAt = nw.ExpiresAt
	}
	return changed, nil
}

// ensureFresh 在 token 已过期（或 5 分钟内过期）时先续期。
// 续期失败不阻断调用：让上游 401 走正常错误分类，避免把网络问题当凭证失效。
func (p *Provider) ensureFresh(ctx context.Context, a *model.Account) {
	if a == nil || a.RefreshToken == "" || a.ExpiresAt <= 0 {
		return
	}
	if time.Now().Unix()+refreshAhead < a.ExpiresAt {
		return
	}
	if _, err := p.RefreshToken(ctx, a); err != nil {
		return
	}
}

// ── 签到 ───────────────────────────────────────────────

// Checkin 每日签到。流程：查状态 → 未签则领取 → 回查确认。
//
// 「回查确认」是刻意的：上游 claim 返回 claim_result=1 只表示请求被接受，
// 个别情况下（风控/活动切换）积分并未真正入账。因此领完再查一次面板，
// 只有今日状态确实变成「已领」才算成功；回查本身失败时保留成功结论但不谎报复核。
func (p *Provider) Checkin(ctx context.Context, a *model.Account) (*model.CheckinResult, error) {
	if a == nil || strings.TrimSpace(a.AccessToken) == "" {
		return nil, &provider.Error{Kind: provider.ErrAuth, Msg: "MiniMax 账号没有 access token，请重新登录导入"}
	}
	p.ensureFresh(ctx, a)
	token := a.AccessToken

	// 1) 先看今天签没签：已签就不必再打领取接口。
	st, stErr := p.querySignin(ctx, token)
	if stErr == nil && st != nil && st.Claimed {
		return &model.CheckinResult{
			OK:      true,
			Already: true,
			Message: alreadyMessage(st),
		}, nil
	}

	// 2) 领取。
	result, points, dayNo, err := p.claimSignin(ctx, token)
	if err != nil {
		// 401 时若有 refresh_token，续期后重试一次（access token 只有 1 小时）。
		if provider.AsError(err).Kind == provider.ErrAuth && a.RefreshToken != "" {
			if _, rerr := p.RefreshToken(ctx, a); rerr == nil {
				result, points, dayNo, err = p.claimSignin(ctx, a.AccessToken)
				token = a.AccessToken
			}
		}
		if err != nil {
			return nil, err
		}
	}

	switch result {
	case claimResultAlready:
		return &model.CheckinResult{OK: true, Already: true, Message: "今日已领取（上游判定已签）"}, nil
	case claimResultClaimed:
		// 3) 回查确认。
		verify, verr := p.querySignin(ctx, token)
		if verr == nil && verify != nil && verify.Claimed {
			return &model.CheckinResult{
				OK:            true,
				Message:       claimMessage(points, dayNo),
				CreditsEarned: int64(points),
			}, nil
		}
		if verr != nil {
			// 回查失败：领取确实返回成功，但无法复核，如实说明。
			return &model.CheckinResult{
				OK:            true,
				Message:       claimMessage(points, dayNo) + "（状态未复核：" + truncate(verr.Error(), 40) + "）",
				CreditsEarned: int64(points),
			}, nil
		}
		// 回查成功但状态仍未变：不信「领取成功」，判失败让调度层重试。
		return &model.CheckinResult{
			OK:      false,
			Message: "领取返回成功但签到状态未更新，稍后重试",
		}, nil
	default:
		return &model.CheckinResult{
			OK:      false,
			Message: fmt.Sprintf("未知领取结果 %d，稍后重试", result),
		}, nil
	}
}

func claimMessage(points float64, dayNo int) string {
	if points > 0 && dayNo > 0 {
		return fmt.Sprintf("签到成功 +%.0f 积分（第 %d 天）", points, dayNo)
	}
	if points > 0 {
		return fmt.Sprintf("签到成功 +%.0f 积分", points)
	}
	return "签到成功"
}

func alreadyMessage(st *SigninStatus) string {
	if st != nil && st.Today != nil && st.Today.Points > 0 {
		return fmt.Sprintf("今日已领取 +%.0f 积分", st.Today.Points)
	}
	return "今日已领取"
}

// ── 额度 ───────────────────────────────────────────────

// Quota 查询积分余额（MiniMax 是积分制，无每日请求上限）。
func (p *Provider) Quota(ctx context.Context, a *model.Account) (*model.QuotaInfo, error) {
	if a == nil || a.AccessToken == "" {
		return &model.QuotaInfo{Provider: p.ID(), Healthy: true, Unit: "credits", FetchedAt: time.Now().Unix()}, nil
	}
	p.ensureFresh(ctx, a)
	c, err := p.queryCredits(ctx, a.AccessToken)
	if err != nil {
		return nil, err
	}
	q := &model.QuotaInfo{
		Provider:  p.ID(),
		UID:       a.UID,
		Remaining: c.Remaining,
		Limit:     c.Granted,
		Used:      c.Granted - c.Remaining,
		Unit:      "credits",
		Daily:     false,
		Healthy:   c.Remaining > 0,
		Credits:   int64(c.Remaining),
		FetchedAt: time.Now().Unix(),
		Detail: map[string]any{
			"granted":   c.Granted,
			"remaining": c.Remaining,
		},
	}
	if c.ExpireAt > 0 {
		q.Detail["expire_at"] = time.UnixMilli(c.ExpireAt).Format(time.RFC3339)
	}
	return q, nil
}

// ── 模型 ───────────────────────────────────────────────

// Models 拉模型目录（纯动态，不编造兜底）。
func (p *Provider) Models(ctx context.Context, a *model.Account) ([]model.ModelInfo, error) {
	if a == nil || a.AccessToken == "" {
		return nil, &provider.Error{Kind: provider.ErrAuth, Msg: "MiniMax 账号没有 access token"}
	}
	p.ensureFresh(ctx, a)
	status, raw, err := p.do(ctx, http.MethodGet, p.agentBaseURL()+modelsPath, a.AccessToken, nil, 15*time.Second)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &provider.Error{Kind: classifyHTTP(status), Status: status, Msg: "模型目录 HTTP " + fmt.Sprint(status)}
	}
	out := parseModels(raw)
	if len(out) == 0 {
		return nil, fmt.Errorf("MiniMax 模型目录为空")
	}
	for i := range out {
		out[i].Provider = p.ID()
		out[i].Kind = "chat"
	}
	return out, nil
}

// parseModels 兼容两种形态：{providers:[{config:{models:{id:{…}}}}]} 与 {models:[{id}]}。
func parseModels(raw []byte) []model.ModelInfo {
	var top struct {
		Providers []struct {
			ProviderID string `json:"providerId"`
			Config     struct {
				Models map[string]struct {
					Name      string `json:"name"`
					ToolCall  bool   `json:"tool_call"`
					Reasoning bool   `json:"reasoning"`
					Limit     struct {
						Context int64 `json:"context"`
						Output  int64 `json:"output"`
					} `json:"limit"`
				} `json:"models"`
			} `json:"config"`
		} `json:"providers"`
		Models []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Context  int64  `json:"context"`
			Output   int64  `json:"output"`
			ToolCall bool   `json:"tool_call"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []model.ModelInfo
	add := func(id, name string, ctxWin, maxOut int64, tools bool) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, model.ModelInfo{
			ID:            id,
			Name:          name,
			ContextWindow: ctxWin,
			MaxTokens:     maxOut,
			SupportsTools: tools,
		})
	}
	for _, pv := range top.Providers {
		for id, m := range pv.Config.Models {
			add(id, m.Name, m.Limit.Context, m.Limit.Output, m.ToolCall)
		}
	}
	for _, m := range top.Models {
		add(m.ID, m.Name, m.Context, m.Output, m.ToolCall)
	}
	sortModels(out)
	return out
}

// ── 对话转发 ───────────────────────────────────────────

// ChatStream 把 OpenAI 兼容请求转成 Anthropic Messages 发上游，再把响应转回 OpenAI。
// MiniMax 模型接口原生是 Anthropic 协议（/llm/v1/messages），必须双向转换。
func (p *Provider) ChatStream(ctx context.Context, a *model.Account, modelName string, body []byte) (*provider.ChatResult, error) {
	if a == nil || a.AccessToken == "" {
		return nil, &provider.Error{Kind: provider.ErrAuth, Msg: "MiniMax 账号没有 access token"}
	}
	reqBody, wantStream, err := buildRequest(body, modelName)
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrClient, Msg: err.Error()}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.agentBaseURL()+messagesPath, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrUpstream, Msg: "MiniMax 网络错误: " + err.Error()}
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return &provider.ChatResult{
			Status:   resp.StatusCode,
			RespBody: raw,
			Err:      &provider.Error{Kind: classifyHTTP(resp.StatusCode), Status: resp.StatusCode, Msg: upstreamMessage(raw)},
		}, nil
	}

	created := time.Now().Unix()
	if wantStream {
		pr, pw := io.Pipe()
		go func() {
			werr := writeOpenAIStream(pw, resp.Body, modelName, created)
			resp.Body.Close()
			_ = pw.CloseWithError(werr)
		}()
		return &provider.ChatResult{Body: pr, ContentType: "text/event-stream"}, nil
	}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	out, cerr := convertResponse(raw, modelName, created)
	if cerr != nil {
		return nil, cerr
	}
	return &provider.ChatResult{Body: io.NopCloser(bytes.NewReader(out)), ContentType: "application/json"}, nil
}

// upstreamMessage 从 Anthropic 错误体里取可读信息。
func upstreamMessage(raw []byte) string {
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil {
		if m := firstNonEmpty(e.Error.Message, e.Message); m != "" {
			return "MiniMax: " + m
		}
	}
	return "MiniMax 上游错误: " + truncate(string(raw), 160)
}

// ── 小工具 ─────────────────────────────────────────────

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// modelIDPattern 模型 id 的合法字符（用于日志与前端展示的宽松校验）。
var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:\-/]+$`)

// validModelID 判断模型 id 是否可直接拼进请求体。
func validModelID(s string) bool {
	return s != "" && modelIDPattern.MatchString(s)
}

// sortModels 让模型目录顺序稳定（按 id 升序，纯本地排序）。
func sortModels(list []model.ModelInfo) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].ID < list[j-1].ID; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}
