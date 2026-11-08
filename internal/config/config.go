// Package config 加载 ChinaCPA 配置（config.json，缺省值内置）。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"chinacpa/internal/notify"
)

// Config 全局配置。
type Config struct {
	// Path 配置文件路径（Load 记录、Save 回写用；"-" 表示不落盘到 JSON 本身）
	Path   string `json:"-"`
	Listen string `json:"listen"`
	// APIKey 网关 /v1/* 的 Bearer 密钥
	APIKey string `json:"api_key"`
	// PanelPassword 管理面板密码
	PanelPassword string `json:"panel_password"`
	// DataDir 数据目录（账号凭证/用量/状态）
	DataDir string `json:"data_dir"`

	Schedule Schedule `json:"schedule"`

	Qwen QwenConfig `json:"qwen"`

	Doubao DoubaoConfig `json:"doubao"`

	// Minimax MiniMax 配置
	Minimax MinimaxConfig `json:"minimax"`

	// ModelAliases 模型别名：客户端写死的模型名 -> ChinaCPA 模型名。
	// 例：{"gpt-5.2-codex": "workbuddy:glm-5.3", "claude-sonnet-4-6": "trae:glm-5.2"}
	ModelAliases map[string]string `json:"model_aliases"`

	Pricing PricingConfig `json:"pricing"`

	// Notify 签到结果推送（Webhook / Telegram / Server 酱）
	Notify notify.Config `json:"notify"`

	// GatewayEnabled 是否挂载 /v1 OpenAI 兼容网关（默认 true）。
	// 只想拿它当「代签器」的人可以关掉，面板与签到不受影响；
	// 用指针是为了区分「未配置」与「显式 false」——老配置文件没有这个字段，
	// 直接 bool 会在缺省零值下把网关误关。
	GatewayEnabled *bool `json:"gateway_enabled,omitempty"`

	// RequestLogDays 请求日志保留天数（本地数据库 data/requests.db，0 = 默认 90 天）
	RequestLogDays int `json:"request_log_days"`

	Server ServerConfig `json:"server"`
}

// Schedule 定时任务配置。
type Schedule struct {
	// CheckinHours 每日自动签到的小时列表（本地时间）
	CheckinHours []int `json:"checkin_hours"`
	// CheckinEnabled 自动签到总开关
	CheckinEnabled bool `json:"checkin_enabled"`
	// TasksEnabled WorkBuddy 任务中心自动接受+领奖
	TasksEnabled bool `json:"tasks_enabled"`
	// QuotaRefreshMinutes 额度自动刷新间隔（分钟）
	QuotaRefreshMinutes int `json:"quota_refresh_minutes"`
	// TokenRefreshHours token 保活刷新的小时列表
	TokenRefreshHours []int `json:"token_refresh_hours"`

	// CheckinConcurrency 签到全局并发上限（同一时刻最多几个账号在打上游）。
	// 多账号同步签到是最好认的机器特征：20 个号在 09:00:00 一起发请求，
	// 顺序错开、并发放到 2 个，看起来才像人。<=0 用默认 2。
	CheckinConcurrency int `json:"checkin_concurrency"`
	// CheckinJitterMinutes 签到时点随机抖动窗口（分钟）。
	// 进入配置小时后的 0~N 分钟内随机取时刻，避免整点齐发。0 = 不抖动。
	CheckinJitterMinutes int `json:"checkin_jitter_minutes"`
	// CheckinDeadline 当日签到的截止时刻（HH:MM），过点不再发起新尝试。
	// 空字符串用默认 23:30。次日 0 点重置重试计数。
	CheckinDeadline string `json:"checkin_deadline"`
	// CheckinRetryMinutes 失败重试的退避序列（分钟）。次数用尽前会一直按此间隔重试，
	// 用尽后当日不再尝试（避免对上游持续轰炸）。<=0 用默认 [5,15,60,180]。
	CheckinRetryMinutes []int `json:"checkin_retry_minutes"`
}

// QwenConfig Qwen 平台配置。
type QwenConfig struct {
	// DailyRequestLimit OAuth 免费额度每日请求数上限（qwen3-coder 2000/天）
	DailyRequestLimit int64 `json:"daily_request_limit"`
}

// DoubaoConfig 豆包平台配置。
type DoubaoConfig struct {
	// ImageDailyLimit 每账号每日图片生成张数上限（本地计数，<=0 用默认 50）
	ImageDailyLimit int64 `json:"image_daily_limit"`
	// VideoDailyLimit 每账号每日视频生成条数上限（本地计数，<=0 用默认 10）
	VideoDailyLimit int64 `json:"video_daily_limit"`
	// CheckinViaBrowser 是否开启豆包每日签到（浏览器通道）。
	// 豆包签到接口带设备签名，必须借页面上下文调用，因此每次签到会临时起一个
	// 无头 Chrome（约 20~40 秒）。默认关闭：只用豆包聊天的人不需要这个开销。
	CheckinViaBrowser bool `json:"checkin_via_browser"`
	// ChromePath 显式指定浏览器可执行文件路径（留空则自动探测 Chrome / Edge）
	ChromePath string `json:"chrome_path,omitempty"`
	// Proxy 豆包出网路由（豆包是境内站点，默认跟随环境变量即可）：
	//   ""/"env"              跟随 HTTP_PROXY / HTTPS_PROXY 环境变量
	//   "system"              用 Windows 系统代理（Internet 选项里那个）
	//   "direct"              强制直连，完全不用代理（最稳）
	//   "http://127.0.0.1:2080"  指定代理
	// 走代理失败时会自动改走直连重试一次，所以一般不用改这里。
	Proxy string `json:"proxy,omitempty"`
	// BrowserNoProxy 签到用的无头浏览器是否强制直连（nil = 默认 true）。
	// 浏览器读系统代理；代理软件退出后注册表开关常残留，会表现为「关掉代理还是连不上」。
	BrowserNoProxy *bool `json:"browser_no_proxy,omitempty"`
}

// MinimaxConfig MiniMax 平台配置。
type MinimaxConfig struct {
	// Region 默认区域（cn / global）。账号自身的 Realm 优先于它；
	// 这里只在导入时决定"国内版还是国际版"实例承接。
	Region string `json:"region"`
	// Timezone 签到结算时区（留空用 Asia/Shanghai）。跨区账号可改成对应时区。
	Timezone string `json:"timezone,omitempty"`
}

// PricingConfig 计价配置。
type PricingConfig struct {
	// File 模型单价表路径（$/1M tokens）
	File string `json:"file"`
	// USDCNY 美元兑人民币汇率
	USDCNY float64 `json:"usd_cny"`
}

// ServerConfig HTTP 服务参数。
type ServerConfig struct {
	ReadTimeoutSeconds  int `json:"read_timeout_seconds"`
	WriteTimeoutSeconds int `json:"write_timeout_seconds"`
}

// Default 返回内置缺省配置。
func Default() *Config {
	return &Config{
		Listen:        ":8313",
		APIKey:        "sk-chinacpa-please-change-me",
		PanelPassword: "123456",
		DataDir:       "./data",
		Schedule: Schedule{
			CheckinHours:         []int{9, 21},
			CheckinEnabled:       true,
			TasksEnabled:         true,
			QuotaRefreshMinutes:  10,
			TokenRefreshHours:    []int{3},
			CheckinConcurrency:   2,
			CheckinJitterMinutes: 15,
			CheckinDeadline:      "23:30",
			CheckinRetryMinutes:  []int{5, 15, 60, 180},
		},
		Qwen:   QwenConfig{DailyRequestLimit: 2000},
		Doubao: DoubaoConfig{ImageDailyLimit: 50, VideoDailyLimit: 10},
		Pricing: PricingConfig{
			File:   "",
			USDCNY: 7.1,
		},
		RequestLogDays: 90,
		Server:         ServerConfig{ReadTimeoutSeconds: 0, WriteTimeoutSeconds: 0},
	}
}

// Load 读取 path 的 JSON 配置并合并到缺省值；文件不存在返回缺省配置。
func Load(path string) (*Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.normalize()
	cfg.Path = path
	return cfg, nil
}

// Save 将当前配置原子写回启动时的配置文件（tmp + rename，防半写）。
// 仅覆盖面板可安全热改的字段集合，其余字段以文件现状为准（避免并发漂移）。
func (c *Config) Save() error {
	if c.Path == "" {
		return fmt.Errorf("config path 未记录（非 Load 加载的配置不可保存）")
	}
	raw, err := os.ReadFile(c.Path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read config: %w", err)
	}
	// 以文件现状为基底合并，只更新面板管理的字段
	disk := Default()
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, disk)
	}
	disk.APIKey = c.APIKey
	out, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.Path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	return os.Rename(tmp, c.Path)
}

// normalize 修正非法取值。
func (c *Config) normalize() {
	if c.Listen == "" {
		c.Listen = ":8313"
	}
	if c.APIKey == "" {
		c.APIKey = "sk-chinacpa-please-change-me"
	}
	if c.PanelPassword == "" {
		c.PanelPassword = "123456"
	}
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if len(c.Schedule.CheckinHours) == 0 {
		c.Schedule.CheckinHours = []int{9, 21}
	}
	if c.Schedule.QuotaRefreshMinutes <= 0 {
		c.Schedule.QuotaRefreshMinutes = 10
	}
	if c.Qwen.DailyRequestLimit <= 0 {
		c.Qwen.DailyRequestLimit = 2000
	}
	if c.Doubao.ImageDailyLimit <= 0 {
		c.Doubao.ImageDailyLimit = 50
	}
	if c.Doubao.VideoDailyLimit <= 0 {
		c.Doubao.VideoDailyLimit = 10
	}
	if strings.TrimSpace(c.Minimax.Region) == "" {
		c.Minimax.Region = "cn"
	}
	if c.Pricing.USDCNY <= 0 {
		c.Pricing.USDCNY = 7.1
	}
	if c.RequestLogDays <= 0 {
		c.RequestLogDays = 90
	}
	if c.Schedule.CheckinConcurrency <= 0 {
		c.Schedule.CheckinConcurrency = 2
	}
	if c.Schedule.CheckinJitterMinutes < 0 {
		c.Schedule.CheckinJitterMinutes = 0
	}
	if _, _, ok := parseClock(c.Schedule.CheckinDeadline); !ok {
		c.Schedule.CheckinDeadline = "23:30"
	}
	if len(c.Schedule.CheckinRetryMinutes) == 0 {
		c.Schedule.CheckinRetryMinutes = []int{5, 15, 60, 180}
	}
}

// parseClock 解析 HH:MM（允许 H:MM）。
func parseClock(s string) (hour, minute int, ok bool) {
	if s == "" {
		return 0, 0, false
	}
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// CheckinDeadlineClock 返回当日签到截止时刻（非法或空返回 23:30）。
func (c *Config) CheckinDeadlineClock() (hour, minute int) {
	if h, m, ok := parseClock(c.Schedule.CheckinDeadline); ok {
		return h, m
	}
	return 23, 30
}

// GatewayOn 是否启用 OpenAI 兼容网关（缺省启用）。
func (c *Config) GatewayOn() bool {
	return c.GatewayEnabled == nil || *c.GatewayEnabled
}

// PricingFileAbs 计价表绝对路径（未配置时落在 DataDir/pricing.json）。
func (c *Config) PricingFileAbs() string {
	f := c.Pricing.File
	if f == "" {
		f = filepath.Join(c.DataDir, "pricing.json")
	}
	if filepath.IsAbs(f) {
		return f
	}
	abs, err := filepath.Abs(f)
	if err != nil {
		return f
	}
	return abs
}

// SaveExample 向 path 写出带注释说明的示例配置（JSON 不支持注释，字段说明见 README）。
func SaveExample(path string) error {
	cfg := Default()
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}
