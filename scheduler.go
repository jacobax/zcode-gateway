package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type accountSlot struct {
	id       string
	mu       sync.Mutex
	limit    int
	inFlight int
	notify   chan struct{}
}

type sessionLock struct {
	mu   sync.Mutex
	refs int
}

type Pool struct {
	store      *Store
	mu         sync.Mutex
	slots      map[string]*accountSlot
	sessions   map[string]*sessionLock
	wake       chan struct{}
	stickyTTL  int64
	stickyWait time.Duration
	maxHold    time.Duration
	inFlight   int
	forced     int64
}

// stickyTTLSeconds is how long an idle session keeps its preferred account.
// The TTL slides forward on every successful request, so it only expires after
// a full TTL without traffic.
func stickyTTLSeconds() int64 {
	const fallback = 86400
	if v := envOr("ZCODE_STICKY_TTL_SEC", ""); v != "" {
		if n, err := time.ParseDuration(v + "s"); err == nil && n > 0 {
			return int64(n.Seconds())
		}
	}
	return fallback
}

// stickyWaitMilliseconds bounds how long a request queues for its preferred
// account before failing over to another idle account for that request only.
func stickyWaitMilliseconds() time.Duration {
	const fallback = 3000
	if raw := strings.TrimSpace(os.Getenv("ZCODE_STICKY_WAIT_MS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			if n > 30000 {
				n = 30000
			}
			return time.Duration(n) * time.Millisecond
		}
	}
	return fallback * time.Millisecond
}

func NewPool(store *Store) *Pool {
	maxHold := 600 * time.Second
	if n, err := time.ParseDuration(envOr("ZCODE_MAX_HOLD_SEC", "600") + "s"); err == nil && n > 0 {
		maxHold = n
	}
	return &Pool{
		store:      store,
		slots:      map[string]*accountSlot{},
		sessions:   map[string]*sessionLock{},
		wake:       make(chan struct{}),
		stickyTTL:  stickyTTLSeconds(),
		stickyWait: stickyWaitMilliseconds(),
		maxHold:    maxHold,
	}
}

func (p *Pool) slotFor(acc *Account) *accountSlot {
	p.mu.Lock()
	defer p.mu.Unlock()
	if slot, ok := p.slots[acc.ID]; ok {
		return slot
	}
	slot := &accountSlot{id: acc.ID, limit: 1, notify: make(chan struct{})}
	p.slots[acc.ID] = slot
	return slot
}

func (p *Pool) signal() {
	p.mu.Lock()
	close(p.wake)
	p.wake = make(chan struct{})
	p.mu.Unlock()
}

func (p *Pool) healthy(acc *Account, now int64) bool {
	if acc == nil || !acc.Enabled || acc.ExhaustedUntil > now {
		return false
	}
	if acc.Quota.Granted > 0 && acc.Quota.Remaining <= 0 {
		return false
	}
	return true
}

func slotLimit(n int) int {
	if n < 1 {
		return 1
	}
	if n > 5 {
		return 5
	}
	return n
}

// holdSlot takes one of the account's concurrency slots, waiting for one to
// free up. The returned release is idempotent and is also called by a watchdog
// after maxHold, so a stalled or abandoned request can never keep the account
// busy forever.
func (p *Pool) holdSlot(ctx context.Context, slot *accountSlot, limit int) (func(), error) {
	slot.mu.Lock()
	slot.limit = slotLimit(limit)
	for slot.inFlight >= slot.limit {
		notify := slot.notify
		slot.mu.Unlock()
		select {
		case <-notify:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		slot.mu.Lock()
	}
	slot.inFlight++
	slot.mu.Unlock()
	return p.trackSlot(slot), nil
}

// trackSlot starts in-flight accounting and the maxHold watchdog for a slot
// whose counter the caller has already incremented.
func (p *Pool) trackSlot(slot *accountSlot) func() {
	var once sync.Once
	var watchdog *time.Timer
	release := func() {
		once.Do(func() {
			watchdog.Stop()
			slot.mu.Lock()
			slot.inFlight--
			close(slot.notify)
			slot.notify = make(chan struct{})
			slot.mu.Unlock()
			p.mu.Lock()
			p.inFlight--
			p.mu.Unlock()
			p.signal()
		})
	}
	watchdog = time.AfterFunc(p.maxHold, func() {
		p.mu.Lock()
		p.forced++
		p.mu.Unlock()
		release()
	})
	p.mu.Lock()
	p.inFlight++
	p.mu.Unlock()
	return release
}

// Acquire returns an available account and a release function. It enforces
// one in-flight request per account and sticky session routing.
func (p *Pool) Acquire(ctx context.Context, sessionKey string) (*Account, func(), error) {
	return p.AcquireExcludingModel(ctx, sessionKey, nil, "")
}

func (p *Pool) AcquireForModel(ctx context.Context, sessionKey, model string) (*Account, func(), error) {
	return p.AcquireExcludingModel(ctx, sessionKey, nil, model)
}

// AcquireExcluding behaves like Acquire but skips accounts already tried by
// the current request. This lets a rate-limited account be retried on a later
// request without cooling it inside one retry loop.
func (p *Pool) AcquireExcluding(ctx context.Context, sessionKey string, exclude map[string]bool) (*Account, func(), error) {
	return p.AcquireExcludingModel(ctx, sessionKey, exclude, "")
}

func (p *Pool) AcquireExcludingModel(ctx context.Context, sessionKey string, exclude map[string]bool, model string) (*Account, func(), error) {
	if sessionKey == "" {
		return p.acquire(ctx, "", exclude, model)
	}
	unlock := p.lockSession(sessionKey)
	acc, release, err := p.acquire(ctx, sessionKey, exclude, model)
	unlock()
	return acc, release, err
}

func (p *Pool) lockSession(key string) func() {
	p.mu.Lock()
	lock := p.sessions[key]
	if lock == nil {
		lock = &sessionLock{}
		p.sessions[key] = lock
	}
	lock.refs++
	p.mu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		p.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(p.sessions, key)
		}
		p.mu.Unlock()
	}
}

func (p *Pool) acquire(ctx context.Context, sessionKey string, exclude map[string]bool, model string) (*Account, func(), error) {
	// preferred is the session's bound account. It survives transient failures:
	// a busy, cooling or excluded account only makes this request fall back, it
	// never rewrites or drops the preference, so the session returns to it.
	preferred := ""
	if sessionKey != "" && p.store.db != nil {
		if accountID, ok := p.store.db.GetBinding(sessionKey, time.Now().Unix()); ok {
			preferred = accountID
		}
	}
	waitDeadline := time.Now().Add(p.stickyWait)
	for {
		now := time.Now().Unix()
		accounts := p.store.AccountsForRefresh()
		healthy := make([]*Account, 0, len(accounts))
		byID := map[string]*Account{}
		for _, acc := range accounts {
			byID[acc.ID] = acc
			if exclude[acc.ID] {
				continue
			}
			if p.healthy(acc, now) && p.store.ModelHealthy(acc.ID, model, now) {
				healthy = append(healthy, acc)
			}
		}
		if len(healthy) == 0 {
			return nil, nil, errNoAvailableAccount
		}
		// Drop slots for removed accounts.
		p.mu.Lock()
		for id := range p.slots {
			if _, ok := byID[id]; !ok {
				delete(p.slots, id)
			}
		}
		p.mu.Unlock()

		if preferred != "" {
			acc, exists := byID[preferred]
			if !exists || !acc.Enabled || (acc.Quota.Granted > 0 && acc.Quota.Remaining <= 0) {
				// Hard failure: the account is gone, disabled or out of quota.
				// The preference can never come back, so start a new one.
				p.store.db.Unbind(sessionKey)
				preferred = ""
			} else if !exclude[acc.ID] && p.healthy(acc, now) && p.store.ModelHealthy(acc.ID, model, now) {
				slot := p.slotFor(acc)
				// Wait a bounded time for the preferred account; past the budget
				// fall through to another idle account for this request only.
				var release func()
				var err error
				if budget := time.Until(waitDeadline); budget > 0 {
					waitCtx, cancel := context.WithTimeout(ctx, budget)
					release, err = p.holdSlot(waitCtx, slot, p.store.EffectiveConcurrency(acc))
					cancel()
				} else {
					release, err = p.tryHoldSlot(slot, p.store.EffectiveConcurrency(acc))
				}
				if err == nil {
					p.store.SetActive(acc.ID)
					return acc, release, nil
				}
				if ctx.Err() != nil {
					return nil, nil, ctx.Err()
				}
				// Busy or cooling: fall back for this request, keep the binding.
			}
		}

		for _, acc := range p.selectionOrder(healthy, now) {
			slot := p.slotFor(acc)
			// Only take a slot that is free right now; queuing is reserved for
			// the session's own preferred account.
			release, err := p.tryHoldSlot(slot, p.store.EffectiveConcurrency(acc))
			if err != nil {
				continue
			}
			if sessionKey != "" && p.store.db != nil && preferred == "" {
				p.store.db.Bind(sessionKey, acc.ID, time.Now().Unix(), p.stickyTTL)
			}
			p.store.SetActive(acc.ID)
			return acc, release, nil
		}

		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		p.mu.Lock()
		wake := p.wake
		p.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

// tryHoldSlot takes a slot only when it is free right now.
func (p *Pool) tryHoldSlot(slot *accountSlot, limit int) (func(), error) {
	slot.mu.Lock()
	slot.limit = slotLimit(limit)
	if slot.inFlight >= slot.limit {
		slot.mu.Unlock()
		return nil, errNoAvailableAccount
	}
	slot.inFlight++
	slot.mu.Unlock()
	return p.trackSlot(slot), nil
}

func (p *Pool) Stats() (inFlight int, forced int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inFlight, p.forced
}

func (p *Pool) InvalidateAccount(accountID string) {
	if p.store.db != nil {
		p.store.db.UnbindAccount(accountID)
	}
}

// TouchSession slides the preference TTL forward after a successful request,
// so an active session never expires no matter how long it runs.
func (p *Pool) TouchSession(sessionKey string) {
	if p.store.db == nil || sessionKey == "" {
		return
	}
	p.store.db.TouchBinding(sessionKey, time.Now().Unix(), p.stickyTTL)
}

// poolCandidate 是加权选号中间态：账号 + 本次请求里的权重。
type poolCandidate struct {
	acc    *Account
	weight float64
}

// candidateWindow 是加权抽签的短名单大小。先按权重取前 N 个再抽，
// 避免纯随机把 7B 额度的号和快耗尽的号同等对待。
func candidateWindow() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("ZCODE_POOL_TOPN"))); err == nil && n > 0 {
		if n > 20 {
			n = 20
		}
		return n
	}
	return 5
}

// accountWeight 越大越该被选中：余额占比是主因子，闲置时长做补偿。
// 账号本身是否可用由调用方先过滤，这里只管「在健康账号里偏向谁」。
func accountWeight(acc *Account, now int64) float64 {
	balance := 0.5
	if acc.Quota.Granted > 0 {
		balance = float64(acc.Quota.Remaining) / float64(acc.Quota.Granted)
		if balance < 0 {
			balance = 0
		}
		if balance > 1 {
			balance = 1
		}
	}
	idleHours := 0.0
	if acc.LastUsedAt > 0 && now > acc.LastUsedAt {
		idleHours = float64(now-acc.LastUsedAt) / 3600
		if idleHours > 5 {
			idleHours = 5
		}
	}
	return 1 + balance*9 + idleHours*0.4
}

// selectionOrder 给出本次请求的候选顺序：高权重短名单先按权重抽签，
// 短名单之外的按权重降序兜底，保证每个健康账号仍然会被试到。
func (p *Pool) selectionOrder(healthy []*Account, now int64) []*Account {
	candidates := make([]poolCandidate, 0, len(healthy))
	for _, acc := range healthy {
		candidates = append(candidates, poolCandidate{acc: acc, weight: accountWeight(acc, now)})
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].weight > candidates[j].weight })

	window := candidateWindow()
	if window > len(candidates) {
		window = len(candidates)
	}
	order := weightedShuffle(candidates[:window])
	if window < len(candidates) {
		order = append(order, weightedShuffle(candidates[window:])...)
	}
	return order
}

// weightedShuffle 按权重不放回抽样，返回「越重越可能靠前」的随机顺序。
func weightedShuffle(list []poolCandidate) []*Account {
	remaining := append([]poolCandidate(nil), list...)
	out := make([]*Account, 0, len(remaining))
	for len(remaining) > 0 {
		total := 0.0
		for _, c := range remaining {
			total += c.weight
		}
		idx := 0
		if total > 0 {
			r := rand.Float64() * total
			for i, c := range remaining {
				r -= c.weight
				if r <= 0 {
					idx = i
					break
				}
			}
		} else {
			idx = rand.IntN(len(remaining))
		}
		out = append(out, remaining[idx].acc)
		remaining = append(remaining[:idx], remaining[idx+1:]...)
	}
	return out
}

var errNoAvailableAccount = &poolError{"no available account"}

type poolError struct{ message string }

func (e *poolError) Error() string { return e.message }

var sessionPattern = regexp.MustCompile(`(?i)session[_-]([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

func extractSessionKey(r *http.Request, body map[string]any) string {
	for _, name := range []string{
		"X-Session-Id", "X-ZCode-Session-Id", "X-OMP-Session-Id",
		"X-Claude-Code-Session-Id", "X-Conversation-Id",
		"X-Codex-Session-Id", "Session-Id", "Conversation-Id",
		"session_id", "conversation_id",
	} {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return sessionKey(value)
		}
	}
	for _, name := range []string{"session_id", "sessionId", "conversation_id", "conversationId"} {
		if value, ok := body[name].(string); ok && strings.TrimSpace(value) != "" {
			return sessionKey(value)
		}
	}
	if metadata, ok := body["metadata"].(map[string]any); ok {
		for _, name := range []string{"session_id", "sessionId", "conversation_id", "conversationId"} {
			if value, ok := metadata[name].(string); ok && strings.TrimSpace(value) != "" {
				return sessionKey(value)
			}
		}
		if value, ok := metadata["user_id"].(string); ok {
			if match := sessionPattern.FindStringSubmatch(value); len(match) > 1 {
				return sessionKey(match[1])
			}
		}
	}
	// 兜底：header 和 body 都没有会话标识时，从内容派生。
	// 通用 OpenAI 客户端（Codex / Cherry Studio 之类）body 里既没有 conversation_id
	// 也没有 metadata，会话键恒空 → 粘性完全不命中 → 同一对话每轮换号 →
	// 上游前缀缓存全 miss。用「system + 首条 user」派生一个稳定键补上。
	if derived := deriveSessionKey(body); derived != "" {
		return derived
	}
	return ""
}

func sessionKey(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 200 {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// upstreamSessionID 给这次请求一个稳定的上游会话标识：同一个会话永远得到
// 同一个 id，上游的前缀缓存才有机会命中。之前每请求随机，缓存基本失效。
// 拿不到会话信息时退回随机——宁可缓存不命中，也不要把不相关的对话混成
// 一个会话（会把不同用户的内容拼到同一段前缀里）。
func upstreamSessionID(key string) string {
	if key == "" {
		return randomUUID()
	}
	return uuidFromHash("zcg-session\x00" + key)
}

// upstreamUserID 是 metadata.user_id 的值：官方客户端发的是 JSON blob
// {"device_id","account_uuid","session_id"}，上游按它做设备/会话维度统计。
// 我们之前只发裸 user id，形状和官方对不上。
func upstreamUserID(acc *Account, sessionID string) string {
	if acc == nil {
		return ""
	}
	device := acc.DeviceMid
	if acc.Profile != nil && acc.Profile.DeviceMid != "" {
		device = acc.Profile.DeviceMid
	}
	blob, err := json.Marshal(map[string]string{
		"device_id":    device,
		"account_uuid": acc.User.ID,
		"session_id":   sessionID,
	})
	if err != nil {
		return acc.User.ID
	}
	return string(blob)
}

// minDerivedSessionChars 是内容派生会话键的最小首条消息长度。
// 太短的（"hi"/"hello"）不派生：不同对话撞同一个键会把请求挤到一个账号上，
// 而短对话本来也没有前缀缓存收益。
const minDerivedSessionChars = 16

// deriveSessionKey 从请求内容派生会话键。
//
// 用「system + 首条 user」而不是全量消息：多轮对话里历史每轮追加，全量哈希
// 每轮都变，粘性会彻底失效；而这两部分在一次对话中恒定。
// 取不到内容时返回空串，绝不编造，让调用方回落到轮换。
func deriveSessionKey(body map[string]any) string {
	if body == nil {
		return ""
	}
	first := strings.TrimSpace(firstUserText(body["messages"]))
	if len([]rune(first)) < minDerivedSessionChars {
		return ""
	}
	system := strings.TrimSpace(textOfContent(body["system"]))
	sum := sha256.Sum256([]byte(system + "\x00" + first))
	return "d-" + hex.EncodeToString(sum[:8])
}

func firstUserText(messages any) string {
	list, ok := messages.([]any)
	if !ok {
		return ""
	}
	for _, item := range list {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "user" {
			continue
		}
		return textOfContent(msg["content"])
	}
	return ""
}

// promptCacheKeyFor 给上游一个稳定的前缀缓存路由键。
//
// 实测依据（KeyPort 的注释）：同一段 8k token 前缀，不带 key 时
// prompt_cache_hit_tokens=0、费用约 0.34；带上后命中 7808 token、费用约 0.02
// —— 差约 17 倍。
//
// 构造里必须掺入账号 uid：跨账号复用同一个 key 会让上游命中别人的前缀缓存，
// 等于把对话内容泄露给另一个账号。
//
// 默认关闭（ZCODE_PROMPT_CACHE_KEY=1 才开）：上游对未知顶层字段的容忍度
// 没有实测过，贸然加字段一旦被拒就是全站 400。
func promptCacheKeyFor(acc *Account, sessionKey string) string {
	if acc == nil || sessionKey == "" {
		return ""
	}
	uid := acc.User.ID
	if uid == "" {
		uid = acc.ID
	}
	sum := sha256.Sum256([]byte(uid + "|" + sessionKey))
	return "zcg-" + hex.EncodeToString(sum[:8])
}

// textOfContent 把 string 或 content block 数组拍平成文本。
func textOfContent(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, block := range v {
			b.WriteString(blockText(block))
		}
		return b.String()
	case map[string]any:
		return blockText(v)
	}
	return ""
}
