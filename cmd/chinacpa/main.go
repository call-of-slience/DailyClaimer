// ChinaCPA —— 国产大模型反代网关：WorkBuddy(腾讯 CodeBuddy) / Qwen(通义) / Trae(字节)
// 单二进制：OAuth 凭证纳管 + 自动签到积分 + 额度自动刷新 + OpenAI 兼容网关 + Web 仪表板。
package main

import (
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"chinacpa/internal/config"
	"chinacpa/internal/gateway"
	"chinacpa/internal/model"
	"chinacpa/internal/netdiag"
	"chinacpa/internal/panel"
	"chinacpa/internal/pool"
	"chinacpa/internal/provider"
	"chinacpa/internal/provider/doubao"
	"chinacpa/internal/provider/minimax"
	"chinacpa/internal/provider/qoder"
	"chinacpa/internal/provider/qwen"
	"chinacpa/internal/provider/qwenwork"
	"chinacpa/internal/provider/trae"
	"chinacpa/internal/provider/workbuddy"
	"chinacpa/internal/pubmodel"
	"chinacpa/internal/reqlog"
	"chinacpa/internal/scheduler"
	"chinacpa/internal/store"
	"chinacpa/internal/usage"
)

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if _, statErr := os.Stat(*cfgPath); os.IsNotExist(statErr) {
		if err := config.SaveExample(*cfgPath); err == nil {
			log.Printf("已生成示例配置 %s（请按需修改 api_key / panel_password）", *cfgPath)
		}
	}

	// 单实例守卫：先抢监听端口，再去碰任何本地数据库。
	// 顺序很关键——两个实例共用 data/ 时，第二个会卡在 bbolt/sqlite 的文件锁上，
	// 几十秒后只留一句 "open request db: timeout"，看起来像数据库坏了；而真正的
	// 答案是「已经有一个实例在跑」。端口抢在前面，就能立刻给出这个答案。
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		if isAddrInUse(err) {
			log.Fatalf("端口 %s 已被占用：已有一个 ChinaCPA 在运行。\n"+
				"  → 同一个数据目录只能由一个实例使用，两个实例会互相锁住数据库\n"+
				"  → 处理：关掉旧窗口，或双击 start.bat（它会先结束旧进程再启动）", addrDisplay(cfg.Listen))
		}
		log.Fatalf("监听 %s 失败: %v", cfg.Listen, err)
	}
	defer ln.Close()

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("初始化数据目录失败: %v%s", err, lockHintSuffix(err))
	}

	// providers
	wb := workbuddy.New("cn")
	wbi := workbuddy.New("global")
	qw := qwen.New(cfg.Qwen.DailyRequestLimit)
	tr := trae.New()
	qd := qoder.New("cn")
	qdi := qoder.New("intl")
	qww := qwenwork.New()
	db := doubao.New(cfg.Doubao.ImageDailyLimit, cfg.Doubao.VideoDailyLimit)
	db.SetCheckinOptions(cfg.Doubao.CheckinViaBrowser, cfg.Doubao.ChromePath)
	db.SetProxy(cfg.Doubao.Proxy)
	if cfg.Doubao.BrowserNoProxy != nil {
		db.SetBrowserNoProxy(*cfg.Doubao.BrowserNoProxy)
	}
	log.Printf("豆包出网路由: %s", describeDoubaoProxy(cfg.Doubao.Proxy))
	if cfg.Doubao.CheckinViaBrowser {
		log.Printf("豆包签到已启用（浏览器通道）：每次签到会临时启动一个无头浏览器")
	}
	// MiniMax 国内版 / 国际版各一个实例：token 的签发域与校验域必须一致，
	// 用国内端点刷国际号会直接 invalid_grant，所以不能合并成单实例按号切域。
	mm := minimax.New(minimax.RegionCN)
	mmi := minimax.New(minimax.RegionGlobal)
	if tz := cfg.Minimax.Timezone; tz != "" {
		mm.SetTimezone(tz)
		mmi.SetTimezone(tz)
	}

	// pool
	provs := []provider.Provider{wb, wbi, qw, tr, qd, qdi, qww, db, mm, mmi}
	pl, err := pool.New(st, provs)
	if err != nil {
		log.Fatalf("加载账号失败: %v", err)
	}
	pl.OnAccountChanged = func(a *model.Account) {
		if err := pl.SaveAccount(a); err != nil {
			log.Printf("WARN: 账号凭证落盘失败 %s/%s: %v", a.Provider, a.UID, err)
		}
	}
	pl.SetModelAliases(cfg.ModelAliases)

	// usage
	u, err := usage.New(filepath.Join(cfg.DataDir, "usage"), cfg.PricingFileAbs())
	if err != nil {
		log.Fatalf("初始化用量目录失败: %v", err)
	}

	// 发布模型（对外模型 → 多渠道路由 + 单价）；渠道计数独立落盘，跨重启保留
	pub, err := pubmodel.Load(filepath.Join(cfg.DataDir, "models.json"), filepath.Join(cfg.DataDir, "channel_stats.json"))
	if err != nil {
		log.Fatalf("加载发布模型失败: %v", err)
	}
	pub.StartStatsFlusher(10 * time.Second)
	pl.SetPublished(pub)
	pubPrices := map[string]usage.PubPrice{}
	for k, v := range pub.PricingMap() {
		pubPrices[k] = usage.PubPrice{Input: v.Input, Output: v.Output, Cache: v.Cache, PerCall: v.PerCall}
	}
	// 汇率优先级：发布模型配置 > config.json pricing.usd_cny > usage 内置缺省
	pubUSDCNY := pub.USDCNY()
	if pubUSDCNY <= 0 {
		pubUSDCNY = cfg.Pricing.USDCNY
	}
	u.SetPublishedPricing(pubPrices, pubUSDCNY)
	if n := len(pub.Get().Models); n > 0 {
		log.Printf("发布模型 %d 个（/v1/models 仅暴露已发布模型）", n)
	}

	// Qwen 今日计数回填（重启恢复）
	for _, a := range pl.Accounts() {
		if a.Provider == "qwen" {
			qw.RestoreCounters(a.UID, u.RequestsToday("qwen", a.UID))
		}
	}

	// 豆包每日计数跨重启持久化（store 落盘，键 provider:uid:day:kind）
	doubao.SetCounterPersistence(
		func(uid string, c doubao.CounterSnapshot) {
			day := time.Now().Format("2006-01-02")
			st.SetCounter("doubao:"+uid+":"+day+":chats", c.Chats)
			st.SetCounter("doubao:"+uid+":"+day+":images", c.Images)
			st.SetCounter("doubao:"+uid+":"+day+":videos", c.Videos)
		},
		func(uid string) (doubao.CounterSnapshot, bool) {
			day := time.Now().Format("2006-01-02")
			prefix := "doubao:" + uid + ":" + day + ":"
			vals := st.CountersPrefix(prefix)
			if len(vals) == 0 {
				return doubao.CounterSnapshot{}, false
			}
			return doubao.CounterSnapshot{
				Day:    day,
				Chats:  vals[prefix+"chats"],
				Images: vals[prefix+"images"],
				Videos: vals[prefix+"videos"],
			}, true
		},
	)
	for _, a := range pl.Accounts() {
		if a.Provider == "doubao" {
			doubao.RestoreAccountCounters(a.UID)
		}
	}

	// 请求日志（bbolt 本地数据库，自动迁移历史 JSONL）
	rl, err := reqlog.Open(filepath.Join(cfg.DataDir, "usage"))
	if err != nil {
		log.Fatalf("初始化请求日志失败: %v%s", err, lockHintSuffix(err))
	}
	retentionDays := cfg.RequestLogDays
	if retentionDays <= 0 {
		retentionDays = 90
	}
	go func() {
		cutoff := func() int64 { return time.Now().AddDate(0, 0, -retentionDays).Unix() }
		if n, err := rl.DeleteBefore(cutoff()); err == nil && n > 0 {
			log.Printf("请求日志清理 %d 条（保留 %d 天）", n, retentionDays)
		}
		t := time.NewTicker(6 * time.Hour)
		for range t.C {
			if n, err := rl.DeleteBefore(cutoff()); err == nil && n > 0 {
				log.Printf("请求日志清理 %d 条", n)
			}
		}
	}()

	// gateway + scheduler + panel
	gw := gateway.New(pl, u)
	gw.SetQwenCounter(qw)
	gw.SetReqLog(rl)
	sched := scheduler.New(cfg, pl, st)
	sched.Start()
	pn := panel.New(cfg, pl, st, u, sched, pub, rl)

	// 当用量记录或额度刷新时，通过 SSE 实时广播给桌面浮动栏
	gw.OnUsageRecorded = func() {
		pn.Broadcast("usage")
	}
	st.OnQuotaChanged = func(q *model.QuotaInfo) {
		pn.Broadcast("quota")
	}

	mux := http.NewServeMux()
	pn.Register(mux, gw)

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  time.Duration(cfg.Server.ReadTimeoutSeconds) * time.Second,
		WriteTimeout: time.Duration(cfg.Server.WriteTimeoutSeconds) * time.Second,
	}
	gwState := "启用"
	if !cfg.GatewayOn() {
		gwState = "已关闭（仅面板+签到）"
	}
	log.Printf("ChinaCPA 启动: 面板 %s 网关 %s 账号 %d 个 签到并发 %d 抖动 %d 分",
		panelURL(cfg.Listen), gwState, len(pl.Accounts()), cfg.Schedule.CheckinConcurrency, cfg.Schedule.CheckinJitterMinutes)
	logBuildInfo()
	// 端口在启动最开始就抢到了，这里直接复用，不再 ListenAndServe
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("HTTP 服务退出: %v", err)
	}
}

// panelURL 把 listen 配成浏览器能打开的本机地址。
// ":8313" 和 "127.0.0.1:8313" 都显示成 http://127.0.0.1:8313/，
// 不能再写成 "127.0.0.1"+listen，否则会拼出 127.0.0.1127.0.0.1:8313。
func panelURL(listen string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		port = strings.TrimPrefix(strings.TrimSpace(listen), ":")
		if port == "" {
			port = "8313"
		}
		return "http://127.0.0.1:" + port + "/"
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

// addrDisplay 把 ":8313" 显示成 "8313"（人话里带个冒号很怪）。
func addrDisplay(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return listen[1:]
	}
	return listen
}

// isAddrInUse 判断是不是「端口已被占用」。Windows 与 Unix 的文案不同，两个都认。
func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "only one usage of each socket address") ||
		strings.Contains(msg, "address already in use")
}

// lockHintSuffix 数据库被另一个进程锁住时，补一句人能看懂的话。
// 这类报错原文只有 "timeout"，不说清楚会让人以为是数据文件坏了。
func lockHintSuffix(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "locked") ||
		strings.Contains(msg, "another process") {
		return "\n  → 数据目录正被另一个实例占用：同一个 data 目录只能由一个 ChinaCPA 使用" +
			"\n  → 处理：关掉旧窗口，或双击 start.bat（它会先结束旧进程再启动）"
	}
	return ""
}

// logBuildInfo 打印可执行文件路径与构建时间：排查「改了代码但行为没变」
// 这类问题时，第一件事就是确认当前跑的是不是新编译出来的那个文件。
func logBuildInfo() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if fi, err := os.Stat(exe); err == nil {
		log.Printf("可执行文件: %s（构建于 %s）", exe, fi.ModTime().Format("2006-01-02 15:04"))
	}
}

// describeDoubaoProxy 把配置值翻成一句人话，启动日志里一眼看出走哪条路。
func describeDoubaoProxy(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "env":
		if p := netdiag.EnvProxy(); p != "" {
			return "跟随环境变量 → " + p
		}
		return "跟随环境变量（当前未设置，等同直连）"
	case "direct", "none", "off":
		return "强制直连（不使用代理）"
	case "system":
		if en, srv := netdiag.SystemProxy(); en && srv != "" {
			return "Windows 系统代理 → " + srv
		}
		return "Windows 系统代理（未开启，等同直连）"
	}
	return "指定代理 → " + mode
}
