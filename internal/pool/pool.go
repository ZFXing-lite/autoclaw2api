// Package pool 账号池：内存索引 + 冷却/禁用状态机 + state.json 持久化。
// 挑选策略：healthy 账号中取 points Top5 加权随机（全 0 退化为均匀随机）。
package pool

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"autoclaw2api/internal/auth"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolHard CoolKind = iota // 余额/配额不足 → 长冷却
	CoolSoft                 // 429 → 短冷却
	CoolErr                  // 连续错误 → 中冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	case CoolErr:
		return "error_threshold"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID             string    `json:"uid"`
	Nickname        string    `json:"nickname,omitempty"`
	Points          int64     `json:"points"`
	Cooling         bool      `json:"cooling"`
	CoolKind        string    `json:"cool_kind,omitempty"`
	CoolRemaining   int64     `json:"cool_remaining_sec,omitempty"`
	Until           time.Time `json:"until,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	Disabled        bool      `json:"disabled"`
	SuccessCount    int64     `json:"success_count,omitempty"`
	ErrCount        int       `json:"err_count,omitempty"`
	LastSuccessTime time.Time `json:"last_success,omitempty"`
	LastErrTime     time.Time `json:"last_err,omitempty"`
}

type entry struct {
	a            *auth.Auth
	points       int64
	successCount int64
	errCount     int
	lastErr      time.Time
	lastSuccess  time.Time
	coolKind     CoolKind
	until        time.Time
	disabled     bool
	reason       string
	lastUsed     time.Time // 最近被选中时刻（防并发撞号）
}

func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	return true
}

// stateAccount 单个账号的持久化状态。
type stateAccount struct {
	Points       int64     `json:"points"`
	Disabled     bool      `json:"disabled"`
	Reason       string    `json:"reason,omitempty"`
	Until        time.Time `json:"until,omitempty"`
	CoolKind     CoolKind  `json:"cool_kind"`
	SuccessCount int64     `json:"success_count,omitempty"`
	ErrCount     int       `json:"err_count,omitempty"`
	LastSuccess  time.Time `json:"last_success,omitempty"`
	LastErr      time.Time `json:"last_err,omitempty"`
}

type stateFile struct {
	Accounts map[string]stateAccount `json:"accounts"`
}

var flushInterval = 5 * time.Second

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	dirty   atomic.Bool

	randInt64N func(n int64) int64 // 仅供测试注入确定性随机源
}

// New 构建池；stateFp 非空时加载旧状态，并启动后台周期性落盘 goroutine。
func New(stateFp string) *Pool {
	p := &Pool{byUID: map[string]*entry{}, stateFp: stateFp}
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	return p
}

// SetRandomSource 仅供测试注入确定性随机源；生产代码不应调用。
func (p *Pool) SetRandomSource(fn func(n int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.randInt64N = fn
}

func (p *Pool) startFlusher() {
	go func() {
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for range t.C {
			p.mu.Lock()
			if p.dirty.Swap(false) {
				p.saveLocked()
			}
			p.mu.Unlock()
		}
	}()
}

// Flush 同步把内存状态落盘（幂等：无变更不写盘）。供进程退出前调用。
func (p *Pool) Flush() {
	p.mu.Lock()
	if p.dirty.Swap(false) {
		p.saveLocked()
	}
	p.mu.Unlock()
}

// Add 加入账号；已存在则保留原状态、更新凭证（upsert 单账号）。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertLocked(a)
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]bool, len(auths))
	for _, a := range auths {
		seen[a.UserID] = true
		p.upsertLocked(a)
	}
	changed := false
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
			changed = true
		}
	}
	if changed {
		p.saveLocked()
	}
}

func (p *Pool) upsertLocked(a *auth.Auth) {
	if e, ok := p.byUID[a.UserID]; ok {
		e.a = a // 保留 points/cooling 状态
		return
	}
	p.byUID[a.UserID] = &entry{a: a}
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。
func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].points != cands[j].points {
			return cands[i].points > cands[j].points
		}
		return cands[i].a.UserID < cands[j].a.UserID
	})
	if len(cands) > 5 {
		cands = cands[:5]
	}

	eligible := make([]*entry, 0, len(cands))
	for _, e := range cands {
		if now.Sub(e.lastUsed) >= minPickGap {
			eligible = append(eligible, e)
		}
	}
	var e *entry
	if len(eligible) == 0 {
		e = cands[0]
		for _, c := range cands[1:] {
			if c.lastUsed.Before(e.lastUsed) {
				e = c
			}
		}
	} else {
		e = p.pickWeighted(eligible)
	}
	e.lastUsed = time.Now()
	return e.a
}

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中。
var minPickGap = 100 * time.Millisecond

func (p *Pool) pickWeighted(cands []*entry) *entry {
	var total int64
	for _, e := range cands {
		total += e.points
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		return cands[int(rnd(int64(len(cands))))]
	}
	r := rnd(total)
	var acc int64
	for _, e := range cands {
		acc += e.points
		if r < acc {
			return e
		}
	}
	return cands[len(cands)-1]
}

// SetPoints 更新账号积分余额。
func (p *Pool) SetPoints(uid string, points int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.points = points
		p.dirty.Store(true)
	}
}

// Cooldown 冷却账号至 now+d。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.coolKind = kind
		e.reason = reason
		e.errCount = 0
		p.dirty.Store(true)
	}
}

// Disable 永久禁用（token 失效），需人工重登后恢复。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
		p.dirty.Store(true)
	}
}

// ReenableAfterManual 运维接口：人工重登后手动解禁。
func (p *Pool) ReenableAfterManual(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = false
		e.until = time.Time{}
		e.coolKind = 0
		e.reason = ""
		e.errCount = 0
		p.dirty.Store(true)
	}
}

// ReenableIfPoints 积分刷新后解冻：仅当 points >= 0 且账号处于冷却（非禁用）时恢复。
func (p *Pool) ReenableIfPoints(uid string, points int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.points = points
		if !e.disabled {
			e.until = time.Time{}
			e.coolKind = 0
			e.reason = ""
			e.errCount = 0
		}
		p.dirty.Store(true)
	}
}

// NoteError 记录一次非余额/非 429 错误；达到 threshold 自动冷却 d 时长。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		e.lastErr = time.Now()
		if e.errCount >= threshold {
			e.until = time.Now().Add(d)
			e.coolKind = CoolErr
			e.reason = "consecutive errors"
			e.errCount = 0
		}
		p.dirty.Store(true)
	}
}

// NoteSuccess 成功请求累加成功计数、刷新 lastSuccess，并重置连续错误。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.successCount++
		e.lastSuccess = time.Now()
		e.errCount = 0
		p.dirty.Store(true)
	}
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// Counts 返回总数与 healthy 数。
func (p *Pool) Counts() (total, healthy int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		total++
		if e.healthy(now) {
			healthy++
		}
	}
	return total, healthy
}

// CountsDetailed 返回 total/healthy/cooling/disabled 四类计数。
func (p *Pool) CountsDetailed() (total, healthy, cooling, disabled int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		total++
		switch {
		case e.disabled:
			disabled++
		case !e.until.IsZero() && now.Before(e.until):
			cooling++
		default:
			healthy++
		}
	}
	return total, healthy, cooling, disabled
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	st := Status{
		UID:             uid,
		Nickname:        e.a.UserName,
		Points:          e.points,
		Cooling:         !e.until.IsZero() && now.Before(e.until),
		Reason:          e.reason,
		Disabled:        e.disabled,
		SuccessCount:    e.successCount,
		ErrCount:        e.errCount,
		LastSuccessTime: e.lastSuccess,
		LastErrTime:     e.lastErr,
		Until:           e.until,
	}
	if st.Cooling {
		st.CoolRemaining = int64(time.Until(e.until).Seconds() + 0.999)
		if st.CoolRemaining < 0 {
			st.CoolRemaining = 0
		}
		st.CoolKind = e.coolKind.String()
	}
	return st
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	for uid, s := range sf.Accounts {
		p.byUID[uid] = &entry{
			a:            &auth.Auth{UserID: uid}, // placeholder，Add 时会换成完整凭证
			points:       s.Points,
			disabled:     s.Disabled,
			reason:       s.Reason,
			until:        s.Until,
			coolKind:     s.CoolKind,
			successCount: s.SuccessCount,
			errCount:     s.ErrCount,
			lastErr:      s.LastErr,
			lastSuccess:  s.LastSuccess,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: map[string]stateAccount{}}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = stateAccount{
			Points:       e.points,
			Disabled:     e.disabled,
			Reason:       e.reason,
			Until:        e.until,
			CoolKind:     e.coolKind,
			SuccessCount: e.successCount,
			ErrCount:     e.errCount,
			LastSuccess:  e.lastSuccess,
			LastErr:      e.lastErr,
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.stateFp)
}
