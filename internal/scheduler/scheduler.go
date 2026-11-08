// Package scheduler 定时任务：每日自动签到、WorkBuddy 任务中心自动完成、
// 额度定时刷新、token 保活、签到结果推送。轻量 tick 轮询（每分钟），无需三方 cron 库。
//
// 签到排程的四条硬规则（多账号自用场景的关键）：
//  1. 进入配置小时后在抖动窗口内随机取时刻，不让 N 个账号在整点同一秒齐发；
//  2. 全局并发闸门（默认 2），同一时刻最多 2 个账号在打上游；
//  3. 失败按 5m/15m/1h/3h 退避重试，次数用尽当日不再尝试（不轰炸上游）；
//  4. 当日 23:30（可配）后不再发起新尝试，跨日重置计数并推送一次汇总。
package scheduler

import (
	"context"
	"log"
	"math/rand"
	"sync"
	"time"

	"chinacpa/internal/config"
	"chinacpa/internal/model"
	"chinacpa/internal/notify"
	"chinacpa/internal/pool"
	"chinacpa/internal/provider"
	"chinacpa/internal/store"
)

// Scheduler 定时器。
type Scheduler struct {
	cfg  *config.Config
	pool *pool.Pool
	st   *store.Store
	nt   *notify.Notifier

	// sem 签到全局并发闸门（容量 = schedule.checkin_concurrency）
	sem chan struct{}

	mu sync.Mutex
	// day 当前排程自然日（YYYY-MM-DD）；跨日时清空 plan/attempts/nextAt
	day string
	// lastCheckin 每账号当日已达标（成功/已签）的日期，键为 provider:uid
	lastCheckin map[string]string
	// inflight 正在签到的账号（手动与定时共用，防撞车）
	inflight map[string]struct{}
	// plan 抖动后的执行时刻，键 provider:uid:day:hour
	plan map[string]time.Time
	// plannedWindow 已排过抖动的窗口（day:hour），保证同一小时只随机一次
	plannedWindow string
	// attempts 当日已尝试次数，键 provider:uid
	attempts map[string]int
	// nextAt 当日下次允许重试的时刻，键 provider:uid
	nextAt map[string]time.Time

	lastTokenDay  string
	lastQuotaTick time.Time
	quit          chan struct{}
	stopOnce      sync.Once

	// 签到结果批次（攒够一批再推一条消息，避免 N 条轰炸）
	bmu       sync.Mutex
	batch     []batchItem
	batchAt   time.Time
	batchBusy int
}

// batchItem 一条待推送的签到结果。
type batchItem struct {
	Provider string
	UID      string
	Label    string
	Attempt  int // 0 = 手动触发
	Res      model.CheckinResult
}

// New 构造。
func New(cfg *config.Config, p *pool.Pool, st *store.Store) *Scheduler {
	conc := cfg.Schedule.CheckinConcurrency
	if conc <= 0 {
		conc = 2
	}
	return &Scheduler{
		cfg:         cfg,
		pool:        p,
		st:          st,
		nt:          notify.New(cfg.Notify),
		sem:         make(chan struct{}, conc),
		lastCheckin: map[string]string{},
		inflight:    map[string]struct{}{},
		plan:        map[string]time.Time{},
		attempts:    map[string]int{},
		nextAt:      map[string]time.Time{},
		quit:        make(chan struct{}),
	}
}

// Notifier 返回推送器（面板展示渠道状态用）。
func (s *Scheduler) Notifier() *notify.Notifier { return s.nt }

func checkinKey(a *model.Account) string { return a.Provider + ":" + a.UID }

// Start 启动（阻塞前先跑一次额度刷新）。
func (s *Scheduler) Start() {
	go func() {
		// 启动即刷一轮额度
		s.refreshAllQuotas()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-s.quit:
				s.flushBatch(true)
				return
			case now := <-t.C:
				s.tick(now)
			}
		}
	}()
}

// Stop 停止（幂等）。
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.quit) })
}

func (s *Scheduler) tick(now time.Time) {
	cfg := s.cfg.Schedule
	// 1) 额度刷新
	if now.Sub(s.lastQuotaTick) >= time.Duration(cfg.QuotaRefreshMinutes)*time.Minute {
		s.lastQuotaTick = now
		s.refreshAllQuotas()
	}
	day := now.Format("2006-01-02")
	s.rollDay(day)
	hour := now.Hour()
	// 2) 每日签到：配置小时窗口内启动；未达标的账号按抖动/退避计划重试
	if cfg.CheckinEnabled && containsHour(cfg.CheckinHours, hour) && !s.pastDeadline(now) {
		s.tickCheckins(day, hour, now)
	}
	// 3) token 保活
	if s.lastTokenDay != day && containsHour(cfg.TokenRefreshHours, hour) {
		s.lastTokenDay = day
		go s.runTokenKeepalive()
	}
	// 4) 结果汇总推送：过截止时刻强制推一次，其余按「静默 20 秒且无在途」推送
	s.flushBatch(s.pastDeadline(now))
}

// rollDay 跨日重置当日状态（抖动计划、重试计数、已完成标记）。
func (s *Scheduler) rollDay(day string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.day == day {
		return
	}
	// 跨日先结算昨天的批次，再清状态（flushBatch 内部自己取 bmu，不能持 mu）
	s.day = day
	s.plan = map[string]time.Time{}
	s.plannedWindow = ""
	s.attempts = map[string]int{}
	s.nextAt = map[string]time.Time{}
	for k, d := range s.lastCheckin {
		if d != day {
			delete(s.lastCheckin, k)
		}
	}
}

func containsHour(hours []int, h int) bool {
	for _, x := range hours {
		if x == h {
			return true
		}
	}
	return false
}

// pastDeadline 是否已过当日签到截止时刻。
func (s *Scheduler) pastDeadline(now time.Time) bool {
	h, m := s.cfg.CheckinDeadlineClock()
	deadline := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	return now.After(deadline)
}

// maxAttempts 当日最多尝试次数（首次 + 退避重试次数）。
func (s *Scheduler) maxAttempts() int {
	n := len(s.cfg.Schedule.CheckinRetryMinutes)
	if n <= 0 {
		return 5
	}
	return n + 1
}

// retryBackoff 返回退避序列（分钟），非法值过滤。
func (s *Scheduler) retryBackoff() []int {
	out := make([]int, 0, len(s.cfg.Schedule.CheckinRetryMinutes))
	for _, v := range s.cfg.Schedule.CheckinRetryMinutes {
		if v > 0 {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return []int{5, 15, 60, 180}
	}
	return out
}

// tickCheckins 对「今天还没签到成功」的每个账号，在抖动计划与退避计划允许时发起签到。
func (s *Scheduler) tickCheckins(day string, hour int, now time.Time) {
	window := day + ":" + itoa(int64(hour))
	s.mu.Lock()
	if s.plannedWindow != window {
		s.plannedWindow = window
		s.planWindowLocked(day, hour, now)
	}
	s.mu.Unlock()

	maxAtt := s.maxAttempts()
	for _, a := range s.pool.Accounts() {
		if a.Disabled || a.NeedsRelogin {
			continue
		}
		key := checkinKey(a)
		// 面板手动签到成功也会落到 store，这里先看库再触发，避免重复打上游
		if r, ok := s.st.TodayCheckin(a.Provider, a.UID); ok && r.Settled() {
			s.mu.Lock()
			s.lastCheckin[key] = day
			s.mu.Unlock()
			continue
		}
		s.mu.Lock()
		at, planned := s.plan[key+":"+window]
		done := s.lastCheckin[key] == day
		_, busy := s.inflight[key]
		na, hasNext := s.nextAt[key]
		att := s.attempts[key]
		s.mu.Unlock()
		if done || busy {
			continue
		}
		if planned && now.Before(at) {
			continue
		}
		if hasNext && now.Before(na) {
			continue
		}
		if att >= maxAtt {
			continue
		}
		s.goCheckin(a, day, att)
	}
}

// planWindowLocked 为窗口内的账号分配抖动偏移（调用方持 mu）。
// 抖动的意义：N 个账号各自分散在 0~jitter 分钟内，而不是同一秒齐发。
func (s *Scheduler) planWindowLocked(day string, hour int, now time.Time) {
	jitter := s.cfg.Schedule.CheckinJitterMinutes
	if jitter <= 0 {
		return
	}
	base := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	spread := jitter * 60
	for _, a := range s.pool.Accounts() {
		if a.Disabled || a.NeedsRelogin {
			continue
		}
		key := checkinKey(a) + ":" + day + ":" + itoa(int64(hour))
		if _, ok := s.plan[key]; ok {
			continue
		}
		s.plan[key] = base.Add(time.Duration(rand.Intn(spread+1)) * time.Second)
	}
}

// goCheckin 异步发起一次定时签到（计入在途数，用于批次结算）。
func (s *Scheduler) goCheckin(a *model.Account, day string, attempt int) {
	s.bmu.Lock()
	s.batchBusy++
	s.bmu.Unlock()
	go func() {
		defer func() {
			s.bmu.Lock()
			s.batchBusy--
			s.bmu.Unlock()
		}()
		res := s.checkinOne(a)
		if res == nil {
			return
		}
		s.afterScheduled(a, day, attempt, res)
		s.pushBatch(a, res, attempt+1)
	}()
}

// afterScheduled 记录重试进度：未达标则按退避序列安排下一次。
func (s *Scheduler) afterScheduled(a *model.Account, day string, attempt int, res *model.CheckinResult) {
	if res.OK || res.Already {
		return // 已在 checkinOne 里清理尝试计数
	}
	backoff := s.retryBackoff()
	idx := attempt
	if idx >= len(backoff) {
		idx = len(backoff) - 1
	}
	delay := time.Duration(backoff[idx]) * time.Minute
	key := checkinKey(a)
	s.mu.Lock()
	// day 为空表示还没跑过 tick（例如面板手动触发后直接失败），照常记账；
	// 非空且与传入 day 不一致说明结果来自昨天，丢弃避免污染今天的重试预算。
	if s.day == "" || s.day == day {
		s.attempts[key] = attempt + 1
		s.nextAt[key] = time.Now().Add(delay)
	}
	s.mu.Unlock()
	if attempt+1 >= s.maxAttempts() {
		log.Printf("SCHED: [%s] 签到未完成（第 %d 次，已达当日上限）：%s", a.Provider+"/"+label(a), attempt+1, res.Message)
		return
	}
	log.Printf("SCHED: [%s] 签到未完成（第 %d 次）：%s，%s 后重试", a.Provider+"/"+label(a), attempt+1, res.Message, delay)
}

// CheckinAccount 单账号签到（面板手动触发入口）：与定时签到共用闸门与去重，
// 只是不占用当日重试配额、结果同样进入推送批次。
func (s *Scheduler) CheckinAccount(a *model.Account) *model.CheckinResult {
	res := s.checkinOne(a)
	if res == nil {
		return &model.CheckinResult{OK: false, Message: "签到无结果"}
	}
	s.pushBatch(a, res, 0)
	return res
}

// checkinOne 核心签到：
// 单账号去重 → 全局并发闸门 → token 前置刷新 → provider 签到 → 任务中心 → 落盘。
// 返回 nil 表示被去重跳过（同一账号已在签到中）。
func (s *Scheduler) checkinOne(a *model.Account) *model.CheckinResult {
	key := checkinKey(a)
	s.mu.Lock()
	if _, busy := s.inflight[key]; busy {
		s.mu.Unlock()
		return &model.CheckinResult{OK: false, Info: true, Message: "签到进行中"}
	}
	s.inflight[key] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
	}()

	// 全局并发闸门：多账号不能同时打上游（排队上限 2 分钟，超时算说明型而非失败）
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-time.After(2 * time.Minute):
		return &model.CheckinResult{OK: false, Info: true, Message: "签到排队超时，稍后自动重试"}
	case <-s.quit:
		return nil
	}
	// 拿到闸门后打一行带占用的日志：多账号场景下，这是唯一能直接看出
	// 「并发是否真的被压住」的证据（并发 2/2 即说明其余账号在排队）。
	log.Printf("SCHED: [%s] 开始签到（并发 %d/%d）", a.Provider+"/"+label(a), len(s.sem), cap(s.sem))

	pr, ok := s.pool.Provider(a.Provider)
	if !ok {
		return &model.CheckinResult{OK: false, Message: "未知平台"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// 临期 token 先刷新
	if _, err := s.pool.EnsureFreshToken(ctx, a); err != nil {
		log.Printf("SCHED: [%s] token 刷新失败: %v", a.Provider+"/"+label(a), err)
	}
	var res *model.CheckinResult
	if r, err := pr.Checkin(ctx, a); err != nil {
		if err == provider.ErrNotSupported {
			// 平台压根没有签到体系：说明型结果（非失败），面板以中性标签展示
			res = &model.CheckinResult{OK: false, Info: true, Message: "该平台无每日签到"}
		} else {
			res = &model.CheckinResult{OK: false, Message: err.Error()}
		}
	} else {
		res = r
	}
	if res == nil {
		res = &model.CheckinResult{OK: false, Message: "签到无结果"}
	}
	// WorkBuddy：任务中心自动完成 + 领奖。
	// 独立 300 秒预算：纯事件任务只要十几秒，但专家类任务每条都要一次真实对话，
	// 一轮可能跑到几分钟；沿用签到那 90 秒会把补进度截断在半路（钱没领到，
	// 还显示已执行完）。
	if s.cfg.Schedule.TasksEnabled {
		if wb, ok := pr.(interface {
			RunTasks(ctx context.Context, a *model.Account) (int64, string, error)
		}); ok {
			tctx, tcancel := context.WithTimeout(context.Background(), 300*time.Second)
			earned, summary, terr := wb.RunTasks(tctx, a)
			tcancel()
			if terr == nil && earned > 0 {
				res.Message += "；任务中心 +" + itoa(earned) + " 积分（" + summary + "）"
				res.CreditsEarned += earned
			} else if terr != nil {
				log.Printf("SCHED: [%s] 任务中心失败: %v", a.Provider+"/"+label(a), terr)
			}
		}
	}
	// 归一化阶段 + 观察时刻后落盘（带时刻才能分辨「陈旧的已签」与「刚领到」）
	res.NormalizePhase(time.Now().Unix())
	s.st.RecordCheckin(a.Provider, a.UID, *res)
	// 成功或已签：记当天完成并清掉重试计划
	if res.Settled() {
		s.mu.Lock()
		s.lastCheckin[key] = time.Now().Format("2006-01-02")
		delete(s.attempts, key)
		delete(s.nextAt, key)
		s.mu.Unlock()
	}
	if res.Info {
		log.Printf("SCHED: [%s] 签到: %s（说明型，非失败，稍后自动复查）", a.Provider+"/"+label(a), res.Message)
	} else {
		log.Printf("SCHED: [%s] 签到: %s", a.Provider+"/"+label(a), res.Message)
	}
	// 签到后立刻刷新额度
	go s.refreshQuota(a)
	return res
}

// ── 结果汇总推送 ─────────────────────────────────────────

// pushBatch 记录一条签到结果待推送。
func (s *Scheduler) pushBatch(a *model.Account, res *model.CheckinResult, attempt int) {
	s.bmu.Lock()
	s.batch = append(s.batch, batchItem{
		Provider: a.Provider, UID: a.UID, Label: label(a), Attempt: attempt, Res: *res,
	})
	s.batchAt = time.Now()
	s.bmu.Unlock()
}

// flushBatch 结算并推送一批签到结果。
// force=false 时仅在「没有在途签到且静默 20 秒」才推送，避免一次签到推 N 条。
func (s *Scheduler) flushBatch(force bool) {
	s.bmu.Lock()
	if len(s.batch) == 0 {
		s.bmu.Unlock()
		return
	}
	if !force {
		if s.batchBusy > 0 || time.Since(s.batchAt) < 20*time.Second {
			s.bmu.Unlock()
			return
		}
	}
	items := s.batch
	s.batch = nil
	s.bmu.Unlock()

	title, text, onlyFailureSkip := summarise(items)
	if onlyFailureSkip && s.nt.OnlyOnFailure() {
		return
	}
	if !s.nt.Enabled() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.nt.Send(ctx, title, text); err != nil {
			log.Printf("SCHED: 签到通知推送失败: %v", err)
		}
	}()
}

// summarise 生成推送标题与正文；onlyFailureSkip 表示本批次全为成功（可按配置跳过）。
func summarise(items []batchItem) (title, text string, onlyFailureSkip bool) {
	var claimed, already, info, failed int
	var failedLines []string
	for _, it := range items {
		switch it.Res.Phase {
		case model.CheckinPhaseClaimed:
			claimed++
		case model.CheckinPhaseAlready:
			already++
		case model.CheckinPhaseInfo:
			info++
		default:
			failed++
			failedLines = append(failedLines, "· "+it.Provider+"/"+it.Label+"："+it.Res.Message)
		}
	}
	now := time.Now()
	title = "签到汇总 " + now.Format("01-02 15:04") + " · " + itoa(int64(len(items))) + " 个账号"
	var b []string
	b = append(b, "成功领取 "+itoa(int64(claimed))+" · 今日已签 "+itoa(int64(already))+
		" · 无签到体系 "+itoa(int64(info))+" · 失败 "+itoa(int64(failed)))
	for _, it := range items {
		if it.Res.Phase == model.CheckinPhaseInfo {
			continue
		}
		line := "· " + it.Provider + "/" + it.Label + "："
		if it.Res.CreditsEarned > 0 {
			line += "+" + itoa(it.Res.CreditsEarned) + " "
		}
		line += it.Res.Message
		b = append(b, line)
	}
	if len(failedLines) > 0 {
		b = append(b, "", "失败明细：")
		b = append(b, failedLines...)
	}
	return title, joinLines(b), failed == 0 && info == 0
}

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}

// ── 额度与保活 ───────────────────────────────────────────

// label 账号展示标签。
func label(a *model.Account) string {
	if a.Nickname != "" {
		return a.Nickname
	}
	return a.UID
}

// RefreshAllQuotas 手动触发全账号额度刷新。
func (s *Scheduler) RefreshAllQuotas() {
	s.refreshAllQuotas()
}

// refreshAllQuotas 全账号额度刷新。
func (s *Scheduler) refreshAllQuotas() {
	for _, a := range s.pool.Accounts() {
		if a.Disabled {
			continue
		}
		s.refreshQuota(a)
	}
}

// refreshQuota 单账号额度刷新（失败静默，保留旧快照）。
func (s *Scheduler) refreshQuota(a *model.Account) {
	pr, ok := s.pool.Provider(a.Provider)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.pool.EnsureFreshToken(ctx, a); err != nil {
		return
	}
	q, err := pr.Quota(ctx, a)
	if err != nil {
		log.Printf("SCHED: [%s] 额度刷新失败: %v", a.Provider+"/"+label(a), err)
		return
	}
	s.st.SetQuota(q)
}

// runTokenKeepalive token 保活刷新。
func (s *Scheduler) runTokenKeepalive() {
	for _, a := range s.pool.Accounts() {
		if a.Disabled {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if _, err := s.pool.EnsureFreshToken(ctx, a); err != nil {
			log.Printf("SCHED: [%s] 保活刷新失败: %v", a.Provider+"/"+label(a), err)
		}
		cancel()
	}
}

// ── 状态查询（面板展示） ──────────────────────────────────

// Status 今日签到进度快照。
type Status struct {
	Day      string `json:"day"`
	Total    int    `json:"total"`
	Done     int    `json:"done"`     // 今日已领取或上游确认已签
	Info     int    `json:"info"`     // 无签到体系（不计失败）
	Failed   int    `json:"failed"`   // 今日最后一次尝试失败
	Inflight int    `json:"inflight"` // 正在进行
	Pending  int    `json:"pending"`  // 今日还没结论
	NextAt   int64  `json:"next_at"`  // 最近一次计划重试时刻（0=无）
	Deadline string `json:"deadline"`
}

// Status 汇总今日签到进度（面板「今日签到 x/y」）。
func (s *Scheduler) Status() Status {
	day := time.Now().Format("2006-01-02")
	st := Status{Day: day, Deadline: s.cfg.Schedule.CheckinDeadline}
	s.mu.Lock()
	next := int64(0)
	for _, t := range s.nextAt {
		if u := t.Unix(); next == 0 || u < next {
			next = u
		}
	}
	s.mu.Unlock()
	st.NextAt = next
	h, m := s.cfg.CheckinDeadlineClock()
	st.Deadline = pad2(h) + ":" + pad2(m)
	for _, a := range s.pool.Accounts() {
		if a.Disabled || a.NeedsRelogin {
			continue
		}
		st.Total++
		if r, ok := s.st.TodayCheckin(a.Provider, a.UID); ok {
			switch {
			case r.OK || r.Already:
				st.Done++
			case r.Info:
				st.Info++
			default:
				st.Failed++
			}
			continue
		}
		if _, busy := s.inflight[checkinKey(a)]; busy {
			st.Inflight++
		}
	}
	st.Pending = st.Total - st.Done - st.Info - st.Failed - st.Inflight
	if st.Pending < 0 {
		st.Pending = 0
	}
	return st
}

func pad2(n int) string {
	if n < 10 {
		return "0" + itoa(int64(n))
	}
	return itoa(int64(n))
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
