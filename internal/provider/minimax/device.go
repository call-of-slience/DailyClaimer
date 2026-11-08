package minimax

// device.go MiniMax 设备码登录（OAuth 2.0 Device Authorization Grant，RFC 8628）+ S256 PKCE。
//
// 为什么用设备码而不是「粘贴 token」：MiniMax 的 access token 只有 1 小时，
// 手工抠出来的 token 一小时后必然失效且无法自愈（没有 refresh_token 时）。
// 设备码走完整授权，能一次性拿到 access + refresh，之后由调度器定期续期。
//
// 实测契约（account.minimax.cn 与 .io 同款）：
//
//	POST {account}/oauth2/device/code
//	  form: client_id=mcode-public & scope=agent.default & audience=agent-backend
//	        & code_challenge=base64url(sha256(verifier)) & code_challenge_method=S256
//	  → { device_code, user_code, verification_uri, verification_uri_complete,
//	      interval, expires_in }   （实测 expires_in=300、interval=3）
//
//	POST {account}/oauth2/token
//	  form: client_id & device_code & grant_type=urn:ietf:params:oauth:grant-type:device_code
//	        & code_verifier
//	  → { access_token, refresh_token, expires_in }
//	  → 400 { error: authorization_pending | slow_down | expired_token | access_denied }
//
// 两个实测踩过的坑：
//  1. 必须带 S256 PKCE，缺了上游直接 400 invalid_request；
//  2. 轮询不能快于 interval，收到 slow_down 要把间隔加 5 秒，否则会一直 slow_down。
//     面板每 2 秒轮询一次，所以这里按 nextPollAt 节流，本地挡住过快请求。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"chinacpa/internal/model"
	"chinacpa/internal/provider"
)

const (
	deviceCodePath  = "/oauth2/device/code"
	deviceTokenPath = "/oauth2/token"
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// 授权窗口兜底（上游 normal 返回 300 秒）。
	deviceDefaultTTL = 300 * time.Second
)

// deviceSession 一次进行中的设备码授权。
type deviceSession struct {
	region     string
	deviceCode string
	userCode   string
	verifyURI  string
	verifier   string // PKCE 明文，换 token 时必须带上
	interval   time.Duration
	expiresAt  time.Time
	nextPollAt time.Time // 节流：未到点直接返回 pending，不打上游
	created    time.Time
}

// pkcePair 生成 RFC 7636 的 verifier / challenge（S256）。
func pkcePair() (verifier, challenge string, err error) {
	raw := make([]byte, 48)
	if _, err = rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("生成 PKCE 随机数失败: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// postForm 发一次 OAuth 表单请求（OAuth 端点都是 x-www-form-urlencoded，不是 JSON）。
func (p *Provider) postForm(ctx context.Context, fullURL string, form url.Values) ([]byte, int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, fullURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, nil
}

// StartLogin 申请设备码：返回展示给用户的验证码与授权链接。
// 不消耗任何账号，真正的授权发生在用户浏览器里。
func (p *Provider) StartLogin(ctx context.Context, _ string) (*model.LoginSession, error) {
	verifier, challenge, err := pkcePair()
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("client_id", oauthClientID)
	form.Set("scope", oauthScope)
	form.Set("audience", oauthAudience)
	form.Set("code_challenge", challenge)
	form.Set("code_challenge_method", "S256")

	raw, status, err := p.postForm(ctx, p.accountURL()+deviceCodePath, form)
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrUpstream, Msg: "申请设备码失败: " + err.Error()}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("申请设备码失败 HTTP %d: %s", status, truncate(string(raw), 200))
	}
	var r struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("设备码响应解析失败: %w", err)
	}
	if r.DeviceCode == "" {
		return nil, fmt.Errorf("设备码响应缺少 device_code: %s", truncate(string(raw), 200))
	}
	interval := time.Duration(r.Interval) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second
	}
	ttl := time.Duration(r.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = deviceDefaultTTL
	}
	verify := r.VerificationURIComplete
	if verify == "" {
		verify = r.VerificationURI
	}

	// 会话 ID 用上游 device_code 的后半段派生，避免额外状态字段。
	id := "mm-" + fingerprint(r.DeviceCode)
	now := time.Now()
	p.mu.Lock()
	p.cleanupSessionsLocked(now)
	p.sessions[id] = &deviceSession{
		region:     p.region,
		deviceCode: r.DeviceCode,
		userCode:   r.UserCode,
		verifyURI:  verify,
		verifier:   verifier,
		interval:   interval,
		expiresAt:  now.Add(ttl),
		nextPollAt: now.Add(interval),
		created:    now,
	}
	p.mu.Unlock()

	return &model.LoginSession{
		Provider: p.ID(),
		ID:       id,
		URL:      verify,
		UserCode: r.UserCode,
	}, nil
}

// PollLogin 轮询授权结果。pending 表示用户还没点完，继续等。
func (p *Provider) PollLogin(ctx context.Context, s *model.LoginSession) (*model.PollResult, error) {
	if s == nil {
		return &model.PollResult{Status: model.LoginFailed, Message: "登录会话不存在"}, nil
	}
	now := time.Now()
	p.mu.Lock()
	sess := p.sessions[s.ID]
	if sess != nil && now.Before(sess.nextPollAt) {
		// 本地节流：未到轮询点，不打上游（避免 slow_down 死循环）。
		waitSec := int(sess.nextPollAt.Sub(now)/time.Second) + 1
		p.mu.Unlock()
		return &model.PollResult{
			Status:  model.LoginPending,
			Message: fmt.Sprintf("等待浏览器完成授权（%d 秒后重试）", waitSec),
		}, nil
	}
	if sess != nil {
		sess.nextPollAt = now.Add(sess.interval)
	}
	p.mu.Unlock()

	if sess == nil {
		return &model.PollResult{Status: model.LoginFailed, Message: "登录会话不存在或已过期，请重新发起"}, nil
	}
	if now.After(sess.expiresAt) {
		p.dropSession(s.ID)
		return &model.PollResult{Status: model.LoginFailed, Message: "授权窗口已过期（约 5 分钟），请重新发起登录"}, nil
	}

	form := url.Values{}
	form.Set("client_id", oauthClientID)
	form.Set("device_code", sess.deviceCode)
	form.Set("grant_type", deviceGrantType)
	form.Set("code_verifier", sess.verifier)

	raw, status, err := p.postForm(ctx, p.accountURL()+deviceTokenPath, form)
	if err != nil {
		// 网络抖动不算失败，继续等。
		return &model.PollResult{Status: model.LoginPending, Message: "网络波动，继续等待授权…"}, nil
	}
	if status == http.StatusOK {
		var r struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int64  `json:"expires_in"`
		}
		if err := json.Unmarshal(raw, &r); err != nil || r.AccessToken == "" {
			return &model.PollResult{Status: model.LoginPending, Message: "等待授权完成"}, nil
		}
		p.dropSession(s.ID)

		expiresAt := int64(0)
		if r.ExpiresIn > 0 {
			expiresAt = time.Now().Unix() + r.ExpiresIn
		}
		a := &model.Account{
			Provider:     p.ID(),
			AccessToken:  r.AccessToken,
			RefreshToken: r.RefreshToken,
			ExpiresAt:    expiresAt,
			Realm:        p.region,
			Source:       "device",
		}
		a.UID, a.Nickname, a.Email = p.identity(ctx, r.AccessToken)
		return &model.PollResult{Status: model.LoginDone, Account: a}, nil
	}

	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &e)
	switch e.Error {
	case "authorization_pending":
		return &model.PollResult{Status: model.LoginPending, Message: "等待浏览器完成授权"}, nil
	case "slow_down":
		// RFC 8628 §3.5：把间隔加 5 秒再继续。
		p.mu.Lock()
		if cur := p.sessions[s.ID]; cur != nil {
			cur.interval += 5 * time.Second
			cur.nextPollAt = time.Now().Add(cur.interval)
		}
		p.mu.Unlock()
		return &model.PollResult{Status: model.LoginPending, Message: "轮询过快，已自动放慢"}, nil
	case "expired_token":
		p.dropSession(s.ID)
		return &model.PollResult{Status: model.LoginFailed, Message: "授权码已过期，请重新发起登录"}, nil
	case "access_denied":
		p.dropSession(s.ID)
		return &model.PollResult{Status: model.LoginFailed, Message: "授权被拒绝"}, nil
	default:
		msg := e.Description
		if msg == "" {
			msg = truncate(string(raw), 200)
		}
		return &model.PollResult{Status: model.LoginFailed, Message: "授权失败：" + msg}, nil
	}
}

func (p *Provider) dropSession(id string) {
	p.mu.Lock()
	delete(p.sessions, id)
	p.mu.Unlock()
}

// cleanupSessionsLocked 回收过期会话（调用方持锁）。
func (p *Provider) cleanupSessionsLocked(now time.Time) {
	for id, s := range p.sessions {
		if now.After(s.expiresAt.Add(time.Minute)) {
			delete(p.sessions, id)
		}
	}
}
