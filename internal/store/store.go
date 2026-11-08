// Package store 负责 ChinaCPA 落盘数据：账号凭证、额度快照、签到历史、用量日志。
// 全部 JSON / JSONL，原子写（tmp+rename），零三方依赖。
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"chinacpa/internal/model"
)

// Store 数据目录管理器。
type Store struct {
	dir string
	mu  sync.Mutex
	// quotaCache uid -> 最新额度快照（内存镜像 + state.json 落盘）
	quotaCache map[string]*model.QuotaInfo
	// checkinLog provider:uid:date -> 结果（用于面板展示与幂等提示）
	checkinLog map[string]model.CheckinResult
	// counters 进程外每日计数器持久化（键 provider:uid:day:kind -> 值），
	// 供豆包等无上游额度接口的平台跨重启恢复本地日配额
	counters map[string]int64

	// OnQuotaChanged 额度快照变更回调（通知 SSE / 浮动栏实时刷新）
	OnQuotaChanged func(q *model.QuotaInfo)
}

// Open 初始化数据目录并加载既有状态。
func Open(dir string) (*Store, error) {
	s := &Store{
		dir:        dir,
		quotaCache: map[string]*model.QuotaInfo{},
		checkinLog: map[string]model.CheckinResult{},
		counters:   map[string]int64{},
	}
	for _, sub := range []string{"", "accounts", "usage"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("init data dir: %w", err)
		}
	}
	s.loadState()
	return s, nil
}

// Dir 返回数据目录。
func (s *Store) Dir() string { return s.dir }

// ── 账号凭证 ──────────────────────────────────────────────

func (s *Store) accountFile(provider, uid string) string {
	safe := sanitize(provider) + "-" + sanitize(uid)
	return filepath.Join(s.dir, "accounts", safe+".json")
}

// SaveAccount 保存账号凭证（原子写）。
func (s *Store) SaveAccount(a *model.Account) error {
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().Unix()
	}
	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.accountFile(a.Provider, a.UID), raw)
}

// DeleteAccount 删除账号凭证文件。
func (s *Store) DeleteAccount(provider, uid string) error {
	err := os.Remove(s.accountFile(provider, uid))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	s.mu.Lock()
	delete(s.quotaCache, provider+":"+uid)
	delete(s.checkinLog, provider+":"+uid+":"+time.Now().Format("2006-01-02"))
	s.mu.Unlock()
	return nil
}

// LoadAccounts 扫描 accounts 目录加载全部账号。
func (s *Store) LoadAccounts() ([]*model.Account, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "accounts"))
	if err != nil {
		return nil, err
	}
	var out []*model.Account
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, "accounts", e.Name()))
		if err != nil {
			continue
		}
		var a model.Account
		if err := json.Unmarshal(raw, &a); err != nil || a.UID == "" {
			continue
		}
		if a.Provider == "" {
			// 从文件名还原 provider
			base := strings.TrimSuffix(e.Name(), ".json")
			if i := strings.Index(base, "-"); i > 0 {
				a.Provider = base[:i]
			}
		}
		out = append(out, &a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider+out[i].UID < out[j].Provider+out[j].UID })
	return out, nil
}

// ── 状态（额度快照 + 签到历史） ───────────────────────────

type stateFile struct {
	Quotas     map[string]*model.QuotaInfo    `json:"quotas"`
	CheckinLog map[string]model.CheckinResult `json:"checkin_log"`
	Counters   map[string]int64               `json:"counters,omitempty"`
}

func (s *Store) statePath() string { return filepath.Join(s.dir, "state.json") }

func (s *Store) loadState() {
	raw, err := os.ReadFile(s.statePath())
	if err != nil {
		return
	}
	var st stateFile
	if err := json.Unmarshal(raw, &st); err != nil {
		return
	}
	if st.Quotas != nil {
		s.quotaCache = st.Quotas
	}
	if st.CheckinLog != nil {
		s.checkinLog = st.CheckinLog
	}
	if st.Counters != nil {
		s.counters = st.Counters
	}
}

func (s *Store) persistStateLocked() {
	st := stateFile{Quotas: s.quotaCache, CheckinLog: s.checkinLog, Counters: s.counters}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	_ = atomicWrite(s.statePath(), raw)
}

// GetCounter 读取跨重启计数器（provider:uid:day:kind）。缺失或过期返回 0。
func (s *Store) GetCounter(key string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counters[key]
}

// SetCounter 更新跨重启计数器并落盘。
func (s *Store) SetCounter(key string, v int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters[key] = v
	s.persistStateLocked()
}

// CountersPrefix 读取某前缀下的全部计数器（如 "doubao:<uid>:" 当日各类计数）。
func (s *Store) CountersPrefix(prefix string) map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int64{}
	for k, v := range s.counters {
		if strings.HasPrefix(k, prefix) {
			out[k] = v
		}
	}
	return out
}

// SetQuota 更新额度快照（内存 + 落盘）。
func (s *Store) SetQuota(q *model.QuotaInfo) {
	q.FetchedAt = time.Now().Unix()
	s.mu.Lock()
	s.quotaCache[q.Provider+":"+q.UID] = q
	s.persistStateLocked()
	fn := s.OnQuotaChanged
	s.mu.Unlock()
	if fn != nil {
		fn(q)
	}
}

// GetQuota 读取额度快照（无缓存返回 nil）。
func (s *Store) GetQuota(provider, uid string) *model.QuotaInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if q, ok := s.quotaCache[provider+":"+uid]; ok {
		cp := *q
		return &cp
	}
	return nil
}

// AllQuotas 返回全部快照副本。
func (s *Store) AllQuotas() map[string]*model.QuotaInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*model.QuotaInfo, len(s.quotaCache))
	for k, v := range s.quotaCache {
		cp := *v
		out[k] = &cp
	}
	return out
}

// RecordCheckin 记录今日签到结果。
// 记录会归一化 Phase / At（provider 未填时按三态推导），供面板展示
// 「几点领到的」以及区分 claimed（真领到）/ already（上游已签）/ info（无签到体系）。
func (s *Store) RecordCheckin(provider, uid string, r model.CheckinResult) {
	r.NormalizePhase(time.Now().Unix())
	s.mu.Lock()
	defer s.mu.Unlock()
	key := provider + ":" + uid + ":" + time.Now().Format("2006-01-02")
	s.checkinLog[key] = r
	// 只保留最近 checkinRetentionDays 天
	cutoff := time.Now().AddDate(0, 0, -checkinRetentionDays).Format("2006-01-02")
	for k := range s.checkinLog {
		if d := k[strings.LastIndex(k, ":")+1:]; d < cutoff {
			delete(s.checkinLog, k)
		}
	}
	s.persistStateLocked()
}

// TodayCheckin 返回今日签到结果。
func (s *Store) TodayCheckin(provider, uid string) (model.CheckinResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.checkinLog[provider+":"+uid+":"+time.Now().Format("2006-01-02")]
	if ok && r.Phase == "" {
		r.NormalizePhase(0)
	}
	return r, ok
}

// CheckinRecord 一条历史签到记录。
type CheckinRecord struct {
	Provider string              `json:"provider"`
	UID      string              `json:"uid"`
	Date     string              `json:"date"`
	Result   model.CheckinResult `json:"result"`
}

// CheckinHistory 返回最近 days 天的签到记录（按日期倒序，最近 500 条）。
// 面板「签到历史」与通知汇总共用；只读快照，不改动内部状态。
func (s *Store) CheckinHistory(days int) []CheckinRecord {
	if days <= 0 {
		days = 7
	}
	cutoff := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
	s.mu.Lock()
	out := make([]CheckinRecord, 0, len(s.checkinLog))
	for k, v := range s.checkinLog {
		i := strings.LastIndex(k, ":")
		if i <= 0 {
			continue
		}
		date := k[i+1:]
		if date < cutoff {
			continue
		}
		head := k[:i]
		j := strings.Index(head, ":")
		if j <= 0 {
			continue
		}
		r := v
		if r.Phase == "" {
			r.NormalizePhase(0)
		}
		out = append(out, CheckinRecord{Provider: head[:j], UID: head[j+1:], Date: date, Result: r})
	}
	s.mu.Unlock()
	sort.Slice(out, func(a, b int) bool {
		if out[a].Date != out[b].Date {
			return out[a].Date > out[b].Date
		}
		return out[a].Result.At > out[b].Result.At
	})
	if len(out) > 500 {
		out = out[:500]
	}
	return out
}

// checkinRetentionDays 签到记录保留天数（够看一周节奏，又不会把 state.json 撑大）。
const checkinRetentionDays = 30

// ── 工具 ─────────────────────────────────────────────────

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
