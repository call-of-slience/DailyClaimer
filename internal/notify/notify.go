// Package notify 签到结果推送：通用 Webhook / Telegram / Server 酱 /
// 企业微信群机器人 / 飞书群机器人 / 钉钉群机器人 / Bark。
//
// 设计取向：一个人自用代签，最需要的不是漂亮面板，而是「今天全签完了吗」一条消息。
// 因此这里只做单向、无重试、不落盘的通知——失败不影响签到本身（调用方按尽力而为处理）。
//
// 一个易踩的坑：企业微信/飞书/钉钉的机器人都不吃通用 {"title","text"}，
// 各自有独立的报文结构，而且会在 HTTP 200 的响应体里用 errcode 表示失败。
// 因此每个渠道都要「看响应体」，否则会出现「本地显示发送成功、手机什么也没收到」。
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config 通知配置（对应 config.json 的 notify 段）。
type Config struct {
	// Enabled 总开关。关闭时所有渠道都不发。
	Enabled bool `json:"enabled"`
	// Webhook 通用 Webhook：POST JSON {"title":"...","text":"..."}。
	// 若填的是企业微信 / 飞书 / 钉钉 / Bark 的机器人地址，会自动按对方报文格式发送，
	// 不用再单独配下面的字段。
	Webhook string `json:"webhook,omitempty"`
	// WeCom 企业微信群机器人地址（qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…）
	WeCom string `json:"wecom,omitempty"`
	// Feishu 飞书群机器人地址（open.feishu.cn/open-apis/bot/v2/hook/…）
	Feishu string `json:"feishu,omitempty"`
	// DingTalk 钉钉群机器人地址（oapi.dingtalk.com/robot/send?access_token=…）
	DingTalk string `json:"dingtalk,omitempty"`
	// DingTalkSecret 钉钉「加签」安全设置的密钥（SEC 开头）；用关键词或 IP 白名单可留空
	DingTalkSecret string `json:"dingtalk_secret,omitempty"`
	// Bark iOS 推送：填设备 key，或完整地址（https://api.day.app/你的key）
	Bark string `json:"bark,omitempty"`
	// TelegramToken Telegram Bot token（@BotFather 申请）
	TelegramToken string `json:"telegram_token,omitempty"`
	// TelegramChat Telegram chat_id（私聊为用户 id，群为负数）
	TelegramChat string `json:"telegram_chat,omitempty"`
	// ServerChan Server 酱 SendKey（SCT 开头）
	ServerChan string `json:"serverchan,omitempty"`
	// TimeoutSeconds 单次推送超时（<=0 用默认 10 秒）
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// NotifyOnAllSuccess 全部成功时也推送（默认 true；关掉则只在有失败/说明型时推）
	NotifyOnAllSuccess *bool `json:"notify_on_all_success,omitempty"`
}

// Notifier 推送器（并发安全：内部只用不可变配置 + http.Client）。
type Notifier struct {
	cfg    Config
	client *http.Client
	// channels 已配置的渠道数（构造时算好，避免每次 Send 重复判断）
	channels int
	// serverChanBase Server 酱接口前缀（单测可改指向本地服务）
	serverChanBase string
}

// New 构造推送器；cfg 无任何渠道时返回的对象 Enabled() 为 false。
func New(cfg Config) *Notifier {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	n := &Notifier{cfg: cfg, client: &http.Client{Timeout: timeout}, serverChanBase: "https://sctapi.ftqq.com"}
	n.channels = len(n.senders())
	return n
}

// Enabled 是否可用（开关打开且至少配了一个渠道）。
func (n *Notifier) Enabled() bool {
	return n != nil && n.cfg.Enabled && n.channels > 0
}

// sender 一个已配置的渠道：名字用于报错与面板展示，fn 负责实际发送。
type sender struct {
	name string
	fn   func(context.Context, string, string) error
}

// senders 返回全部已配置的渠道（顺序稳定，便于测试与展示）。
func (n *Notifier) senders() []sender {
	var out []sender
	if v := strings.TrimSpace(n.cfg.Webhook); v != "" {
		out = append(out, sender{"webhook", n.sendWebhook})
	}
	if v := strings.TrimSpace(n.cfg.WeCom); v != "" {
		out = append(out, sender{"wecom", func(ctx context.Context, t, b string) error {
			return n.postWeCom(ctx, v, t, b)
		}})
	}
	if v := strings.TrimSpace(n.cfg.Feishu); v != "" {
		out = append(out, sender{"feishu", func(ctx context.Context, t, b string) error {
			return n.postFeishu(ctx, v, t, b)
		}})
	}
	if v := strings.TrimSpace(n.cfg.DingTalk); v != "" {
		secret := strings.TrimSpace(n.cfg.DingTalkSecret)
		out = append(out, sender{"dingtalk", func(ctx context.Context, t, b string) error {
			return n.postDingTalk(ctx, v, secret, t, b)
		}})
	}
	if v := strings.TrimSpace(n.cfg.Bark); v != "" {
		out = append(out, sender{"bark", func(ctx context.Context, t, b string) error {
			return n.postBark(ctx, v, t, b)
		}})
	}
	if strings.TrimSpace(n.cfg.TelegramToken) != "" && strings.TrimSpace(n.cfg.TelegramChat) != "" {
		out = append(out, sender{"telegram", n.sendTelegram})
	}
	if strings.TrimSpace(n.cfg.ServerChan) != "" {
		out = append(out, sender{"serverchan", n.sendServerChan})
	}
	return out
}

// Channels 已配置的渠道名（面板展示用）。
func (n *Notifier) Channels() []string {
	if n == nil {
		return nil
	}
	var out []string
	for _, s := range n.senders() {
		out = append(out, s.name)
	}
	return out
}

// OnlyOnFailure 仅在存在非成功结果时推送（配置了 notify_on_all_success=false）。
func (n *Notifier) OnlyOnFailure() bool {
	return n != nil && n.cfg.NotifyOnAllSuccess != nil && !*n.cfg.NotifyOnAllSuccess
}

// sendResult 单渠道推送结果。
type sendResult struct {
	channel string
	err     error
}

// Send 并发发往全部已配置渠道，返回聚合错误（部分成功时错误里带渠道名）。
// 调用方应把它当成尽力而为：通知失败绝不回滚签到结果。
func (n *Notifier) Send(ctx context.Context, title, text string) error {
	if !n.Enabled() {
		return nil
	}
	jobs := n.senders()
	if len(jobs) == 0 {
		return nil
	}
	ch := make(chan sendResult, len(jobs))
	for _, j := range jobs {
		go func(j sender) {
			ch <- sendResult{channel: j.name, err: j.fn(ctx, title, text)}
		}(j)
	}
	var errs []error
	for range jobs {
		if r := <-ch; r.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.channel, r.err))
		}
	}
	return errors.Join(errs...)
}

// ── 渠道实现 ─────────────────────────────────────────────

// sendWebhook 通用 Webhook。若 URL 是已知的群机器人地址，自动换成对方的报文格式——
// 大多数人是把机器人地址直接粘进 webhook 的，这里替他们把格式差异吃掉。
func (n *Notifier) sendWebhook(ctx context.Context, title, text string) error {
	endpoint := strings.TrimSpace(n.cfg.Webhook)
	switch detect(endpoint) {
	case "wecom":
		return n.postWeCom(ctx, endpoint, title, text)
	case "feishu":
		return n.postFeishu(ctx, endpoint, title, text)
	case "dingtalk":
		return n.postDingTalk(ctx, endpoint, strings.TrimSpace(n.cfg.DingTalkSecret), title, text)
	case "bark":
		return n.postBark(ctx, endpoint, title, text)
	}
	_, err := n.postJSON(ctx, endpoint, map[string]string{"title": title, "text": text})
	return err
}

// detect 按 host 识别群机器人类型；识别不出返回空串（按通用 webhook 处理）。
func detect(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "qyapi.weixin.qq.com":
		return "wecom"
	case host == "open.feishu.cn", host == "open.larksuite.com", strings.HasSuffix(host, ".feishu.cn"):
		return "feishu"
	case host == "oapi.dingtalk.com":
		return "dingtalk"
	case host == "api.day.app", strings.HasSuffix(host, ".day.app"):
		return "bark"
	}
	return ""
}

// postWeCom 企业微信群机器人：markdown 消息，errcode 非 0 视为失败。
func (n *Notifier) postWeCom(ctx context.Context, endpoint, title, text string) error {
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]string{"content": "**" + title + "**\n" + text},
	}
	raw, err := n.postJSON(ctx, endpoint, payload)
	if err != nil {
		return err
	}
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if json.Unmarshal(raw, &r) == nil && r.ErrCode != 0 {
		return fmt.Errorf("企业微信拒收: %s（errcode=%d）", r.ErrMsg, r.ErrCode)
	}
	return nil
}

// postFeishu 飞书群机器人：text 消息；新版返回 code，旧版返回 StatusCode。
func (n *Notifier) postFeishu(ctx context.Context, endpoint, title, text string) error {
	payload := map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": title + "\n" + text},
	}
	raw, err := n.postJSON(ctx, endpoint, payload)
	if err != nil {
		return err
	}
	var r struct {
		Code          int    `json:"code"`
		Msg           string `json:"msg"`
		StatusCode    int    `json:"StatusCode"`
		StatusMessage string `json:"StatusMessage"`
	}
	if json.Unmarshal(raw, &r) == nil {
		if r.Code != 0 {
			return fmt.Errorf("飞书拒收: %s（code=%d）", r.Msg, r.Code)
		}
		if r.StatusCode != 0 {
			return fmt.Errorf("飞书拒收: %s（StatusCode=%d）", r.StatusMessage, r.StatusCode)
		}
	}
	return nil
}

// postDingTalk 钉钉群机器人：markdown 消息；配了加签密钥时补 timestamp+sign。
func (n *Notifier) postDingTalk(ctx context.Context, endpoint, secret, title, text string) error {
	if secret != "" {
		ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts + "\n" + secret))
		sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))
		sep := "?"
		if strings.Contains(endpoint, "?") {
			sep = "&"
		}
		endpoint += sep + "timestamp=" + ts + "&sign=" + url.QueryEscape(sign)
	}
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]string{"title": title, "text": "### " + title + "\n" + text},
	}
	raw, err := n.postJSON(ctx, endpoint, payload)
	if err != nil {
		return err
	}
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if json.Unmarshal(raw, &r) == nil && r.ErrCode != 0 {
		msg := r.ErrMsg
		if r.ErrCode == 310000 {
			msg += "（若机器人开了加签，请填 dingtalk_secret）"
		}
		return fmt.Errorf("钉钉拒收: %s（errcode=%d）", msg, r.ErrCode)
	}
	return nil
}

// postBark Bark（iOS）：只要 key 时自动拼 api.day.app；返回 code 非 200 视为失败。
func (n *Notifier) postBark(ctx context.Context, cfgValue, title, text string) error {
	target := strings.TrimSpace(cfgValue)
	base := "https://api.day.app"
	key := target
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		u, err := url.Parse(target)
		if err != nil {
			return fmt.Errorf("bark 地址无法解析: %w", err)
		}
		base = u.Scheme + "://" + u.Host
		key = strings.Trim(u.Path, "/")
	}
	if key == "" {
		return errors.New("bark 设备 key 为空")
	}
	payload := map[string]any{"title": title, "body": text, "group": "ChinaCPA"}
	raw, err := n.postJSON(ctx, strings.TrimSuffix(base, "/")+"/"+key, payload)
	if err != nil {
		return err
	}
	var r struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &r) == nil && r.Code != 0 && r.Code != 200 {
		return fmt.Errorf("bark 拒收: %s（code=%d）", r.Message, r.Code)
	}
	return nil
}

func (n *Notifier) sendTelegram(ctx context.Context, title, text string) error {
	endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.cfg.TelegramToken)
	payload := map[string]any{
		"chat_id":                  n.cfg.TelegramChat,
		"text":                     title + "\n" + text,
		"disable_web_page_preview": true,
	}
	_, err := n.postJSON(ctx, endpoint, payload)
	return err
}

func (n *Notifier) sendServerChan(ctx context.Context, title, text string) error {
	base := n.serverChanBase
	if base == "" {
		base = "https://sctapi.ftqq.com"
	}
	endpoint := strings.TrimSuffix(base, "/") + "/" + url.PathEscape(strings.TrimSpace(n.cfg.ServerChan)) + ".send"
	form := url.Values{}
	form.Set("title", title)
	form.Set("desp", text)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return n.do(req)
}

// postJSON POST 一个 JSON 报文，返回响应体（供调用方检查 errcode）。
// 只有 2xx 才算 HTTP 层成功；业务失败由各渠道自己按响应体判断。
func (n *Notifier) postJSON(ctx context.Context, endpoint string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return n.doRaw(req)
}

// do 执行请求：只把 2xx 视为成功，响应体最多读 512 字节用于报错。
func (n *Notifier) do(req *http.Request) error {
	_, err := n.doRaw(req)
	return err
}

// doRaw 同 do，但把响应体交回调用方（最多 1KB，够放 errcode/errmsg）。
func (n *Notifier) doRaw(req *http.Request) ([]byte, error) {
	resp, err := n.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode/100 != 2 {
		return raw, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}
