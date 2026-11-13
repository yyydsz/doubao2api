// Package pool 维护上游 API key 池：冷却、熔断、会话粘性与大请求预算。
//
// 设计沿用 workbuddy2api 的经验：
//   - 按 key 维度记状态（冷却/失败次数/是否停用），选号跳过不可用项；
//   - 会话粘性把同一条对话固定到同一个 key —— 上游前缀缓存按 key 归属，换 key 即丢缓存；
//   - 大请求按滑动窗口做预算，只有当某个 key 短时间内被压满大请求时才让出一次。
package pool

import (
	"math/rand"
	"sync"
	"time"
)

// Key 上游凭据。
type Key struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// Options 池参数。
type Options struct {
	RateLimitCooldown time.Duration // 429 冷却，默认 60s
	OverloadCooldown  time.Duration // 5xx/529 冷却，默认 15s
	StickyTTL         time.Duration // 会话粘性有效期，默认 10m
	LargeRequestBytes int           // 大请求阈值，0 = 关闭预算
	LargeBudget       int           // 窗口内每 key 允许的大请求条数，0 = 关闭
	LargeWindow       time.Duration // 预算滑动窗口
	Deterministic     bool          // 按池内顺序选号（测试/排障）
}

func (o Options) withDefaults() Options {
	if o.RateLimitCooldown <= 0 {
		o.RateLimitCooldown = 60 * time.Second
	}
	if o.OverloadCooldown <= 0 {
		o.OverloadCooldown = 15 * time.Second
	}
	if o.StickyTTL <= 0 {
		o.StickyTTL = 10 * time.Minute
	}
	if o.LargeWindow <= 0 {
		o.LargeWindow = 2 * time.Minute
	}
	return o
}

type Handle struct {
	name      string
	key       string
	coolUntil time.Time
	lastUsed  time.Time
	fails     int
	disabled  bool
	reason    string
	inFlight  int
	largeUse  []time.Time
}

type sticky struct {
	name string
	at   time.Time
}

// Pool key 池。
type Pool struct {
	mu     sync.Mutex
	opt    Options
	items  []*Handle
	byName map[string]*Handle
	sticky map[string]sticky
	rnd    *rand.Rand
}

// New 创建池。
func New(opts Options) *Pool {
	o := opts.withDefaults()
	return &Pool{
		opt:    o,
		byName: map[string]*Handle{},
		sticky: map[string]sticky{},
		rnd:    rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Add 追加一个 key。
func (p *Pool) Add(k Key) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.byName[k.Name]; ok {
		return
	}
	e := &Handle{name: k.Name, key: k.Key}
	p.items = append(p.items, e)
	p.byName[k.Name] = e
}

// Remove 从池中移除一个 key（面板停用账号）。
func (p *Pool) Remove(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byName, name)
	out := p.items[:0]
	for _, e := range p.items {
		if e.name != name {
			out = append(out, e)
		}
	}
	p.items = out
}

// Len 返回 key 总数。
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.items)
}

// Key 按名字取明文 key。
func (p *Pool) Key(name string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byName[name]; ok {
		return e.key, true
	}
	return "", false
}

// Pick 选一个可用 key，跳过 tried、冷却中、已停用，以及（大请求时）预算已满的 key。
// 返回 nil 表示当前没有可用 key。
func (p *Pool) Pick(tried map[string]bool, large bool) *Handle {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	type cand struct {
		e    *Handle
		idle time.Duration
	}
	var cands []cand
	for _, e := range p.items {
		if e.disabled || (tried != nil && tried[e.name]) {
			continue
		}
		if now.Before(e.coolUntil) {
			continue
		}
		if large && p.budgetFullLocked(e, now) {
			continue
		}
		idle := now.Sub(e.lastUsed)
		cands = append(cands, cand{e: e, idle: idle})
	}
	if len(cands) == 0 {
		return nil
	}
	// 取闲置最久的前若干个，再在其中随机，避免所有并发都撞同一个 key
	if len(cands) > 5 {
		// 简单选择排序取 top5
		for i := 0; i < 5; i++ {
			for j := i + 1; j < len(cands); j++ {
				if cands[j].idle > cands[i].idle {
					cands[i], cands[j] = cands[j], cands[i]
				}
			}
		}
		cands = cands[:5]
	}
	if p.opt.Deterministic {
		cc := cands[0]
		cc.e.lastUsed = now
		return cc.e
	}
	c := cands[p.rnd.Intn(len(cands))]
	c.e.lastUsed = now
	return c.e
}

// Name 返回条目名（供调用方记录，不暴露内部结构）。
func (e *Handle) Name() string { return e.name }

// Key 返回条目明文 key。
func (e *Handle) Key() string { return e.key }

// budgetFullLocked 判断该 key 在窗口内的大请求是否已达预算。
func (p *Pool) budgetFullLocked(e *Handle, now time.Time) bool {
	if p.opt.LargeBudget <= 0 || p.opt.LargeWindow <= 0 {
		return false
	}
	kept := e.largeUse[:0]
	for _, at := range e.largeUse {
		if now.Sub(at) < p.opt.LargeWindow {
			kept = append(kept, at)
		}
	}
	e.largeUse = kept
	return len(kept) >= p.opt.LargeBudget
}

// NoteLarge 记录某 key 刚承接了一条大请求。
func (p *Pool) NoteLarge(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byName[name]; ok {
		e.largeUse = append(e.largeUse, time.Now())
	}
}

// NoteSuccess 记录成功（用于观测）。
func (p *Pool) NoteSuccess(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byName[name]; ok {
		e.fails = 0
	}
}

// NoteRateLimit 记录限流：冷却 + 计失败次数。
func (p *Pool) NoteRateLimit(name string, retryAfter time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byName[name]
	if !ok {
		return
	}
	e.fails++
	d := p.opt.RateLimitCooldown
	if retryAfter > d {
		d = retryAfter
	}
	e.coolUntil = time.Now().Add(d)
}

// NoteOverload 记录 5xx/过载：短冷却。
func (p *Pool) NoteOverload(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byName[name]
	if !ok {
		return
	}
	e.fails++
	e.coolUntil = time.Now().Add(p.opt.OverloadCooldown)
}

// NoteAuthFailure 鉴权失败：直接停用，避免反复用坏 key。
func (p *Pool) NoteAuthFailure(name, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byName[name]; ok {
		e.disabled = true
		e.reason = reason
	}
}

// ---------- 会话粘性 ----------

// ResolveSticky 取某会话已绑定的 key 名。
func (p *Pool) ResolveSticky(conv string) (string, bool) {
	if conv == "" {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sticky[conv]
	if !ok || time.Since(s.at) > p.opt.StickyTTL {
		delete(p.sticky, conv)
		return "", false
	}
	if e, ok := p.byName[s.name]; ok && e.disabled {
		delete(p.sticky, conv)
		return "", false
	}
	return s.name, true
}

// BindSticky 把会话绑定到某个 key。
func (p *Pool) BindSticky(conv, name string) {
	if conv == "" || name == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sticky[conv] = sticky{name: name, at: time.Now()}
}

// UnbindSticky 解绑。
func (p *Pool) UnbindSticky(conv string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sticky, conv)
}

// PickBySticky 按名字取仍可用的条目（冷却中/停用时返回 nil）。
func (p *Pool) PickBySticky(name string, large bool) *Handle {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byName[name]
	if !ok || e.disabled {
		return nil
	}
	now := time.Now()
	if now.Before(e.coolUntil) {
		return nil
	}
	if large && p.budgetFullLocked(e, now) {
		return nil
	}
	e.lastUsed = now
	return e
}

// ---------- 观测 ----------

// Stat 单个 key 的状态快照。
type Stat struct {
	Name       string `json:"name"`
	Cooling    bool   `json:"cooling"`
	CoolLeftMS int64  `json:"cool_left_ms,omitempty"`
	Disabled   bool   `json:"disabled"`
	Reason     string `json:"reason,omitempty"`
	Fails      int    `json:"fails"`
	InFlight   int    `json:"in_flight"`
}

// Stats 返回全部 key 的状态。
func (p *Pool) Stats() []Stat {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	out := make([]Stat, 0, len(p.items))
	for _, e := range p.items {
		s := Stat{Name: e.name, Disabled: e.disabled, Reason: e.reason, Fails: e.fails, InFlight: e.inFlight}
		if now.Before(e.coolUntil) {
			s.Cooling = true
			s.CoolLeftMS = int64(e.coolUntil.Sub(now) / time.Millisecond)
		}
		out = append(out, s)
	}
	return out
}

// Healthy 返回可用（未停用且不在冷却）数量。
func (p *Pool) Healthy() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	n := 0
	for _, e := range p.items {
		if !e.disabled && !now.Before(e.coolUntil) {
			n++
		}
	}
	return n
}
