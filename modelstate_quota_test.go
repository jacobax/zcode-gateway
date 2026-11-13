package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

// 额度耗尽类失败（1005/13xx）绝不落到 Disabled：那是单向门，额度恢复后
// 没有流量能唤醒它们（2026-11-10 实测 5 号站额度恢复后仍显示已禁用）。
func TestCappedModelFailureNeverDisables(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := int64(1000)
	for i := 0; i < 12; i++ { // 足够越过 stage1/stage2 各两次
		store.recordModelFailureAt("acc", "GLM-5.3-Flash", "exceed quota limit", now+int64(i), capCooldown)
	}
	state, ok := store.ModelStateFor("acc", "GLM-5.3-Flash")
	if !ok {
		t.Fatal("state should exist")
	}
	if state.Disabled {
		t.Fatalf("capped failure must never disable: %#v", state)
	}
	if state.Stage != 2 || state.CooldownUntil <= now {
		t.Fatalf("should be parked at stage2 with an active cooldown: %#v", state)
	}
	// 冷却期内不可用，冷却过了恢复可用。
	if store.ModelHealthy("acc", "GLM-5.3-Flash", now+5) {
		t.Fatal("should be cooling right after failure")
	}
	if !store.ModelHealthy("acc", "GLM-5.3-Flash", state.CooldownUntil+1) {
		t.Fatal("should recover after stage2 cooldown")
	}
}

// 普通失败（网络/鉴权）仍然走原来的升级路径，直到 Disabled。
func TestUncappedModelFailureStillDisables(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := int64(1000)
	for i := 0; i < 9; i++ {
		store.recordModelFailureAt("acc", "GLM-5.3-Flash", "connection refused", now+int64(i), capDisable)
	}
	state, _ := store.ModelStateFor("acc", "GLM-5.3-Flash")
	if !state.Disabled || state.Stage != 3 {
		t.Fatalf("uncapped failure should disable at stage3: %#v", state)
	}
}

// 额度恢复要按模型清熔断：上游分桶记账，账号合计>0 不代表每个模型都恢复。
// 没有分模型快照时保守不动。
func TestHealModelStates(t *testing.T) {
	server, store := newQuotaTestServer(t)
	acc := store.GetAccount("acc1")
	for _, model := range []string{"GLM-5.3-Flash", "GLM-5.3"} {
		for i := 0; i < 9; i++ {
			store.RecordModelFailure(acc.ID, model, "exceed quota limit")
		}
	}
	if st, _ := store.ModelStateFor(acc.ID, "GLM-5.3-Flash"); !st.Disabled {
		t.Fatal("precondition: Flash should be Disabled")
	}

	// 无分模型快照：不清。
	server.healModelStates(acc, Quota{Granted: 100, Remaining: 70})
	if _, ok := store.ModelStateFor(acc.ID, "GLM-5.3-Flash"); !ok {
		t.Fatal("no per-model snapshot: nothing may be cleared")
	}

	models := func(flash, glm int64) []QuotaModel {
		return []QuotaModel{
			{Name: "GLM-5.3-Flash", Remaining: flash},
			{Name: "GLM-5.3", Remaining: glm},
		}
	}
	// Flash 桶为 0：Flash 熔断保留，GLM-5.3 恢复则清。
	server.healModelStates(acc, Quota{Models: models(0, 3_000_000)})
	if _, ok := store.ModelStateFor(acc.ID, "GLM-5.3-Flash"); !ok {
		t.Fatal("Flash bucket still empty: its state must stay")
	}
	if _, ok := store.ModelStateFor(acc.ID, "GLM-5.3"); ok {
		t.Fatal("GLM-5.3 bucket recovered: its state must be cleared")
	}

	// 两桶都恢复：全部清除。
	server.healModelStates(acc, Quota{Models: models(4_000_000, 3_000_000)})
	if _, ok := store.ModelStateFor(acc.ID, "GLM-5.3-Flash"); ok {
		t.Fatal("Flash recovered: state must be cleared")
	}
	actions := accountEventActions(store)
	if len(actions) == 0 || actions[len(actions)-1] != "模型熔断解除" {
		t.Fatalf("heal event missing: %v", actions)
	}
}

// 计数型 doer：统计 refreshQuota 实际打了几次 balance。
type countingQuotaDoer struct {
	fakeQuotaDoer
	balanceCalls atomic.Int64
}

func (d *countingQuotaDoer) Do(req *fhttp.Request) (*fhttp.Response, error) {
	if req != nil && strings.Contains(req.URL.Path, "/billing/balance") {
		d.balanceCalls.Add(1)
	}
	return d.fakeQuotaDoer.Do(req)
}

// 1005 命中后强制刷新要去重：1005 风暴时每个号 30 秒最多一次真实请求。
func TestForceQuotaRefreshDedup(t *testing.T) {
	server, store := newQuotaTestServer(t)
	doer := &countingQuotaDoer{fakeQuotaDoer: fakeQuotaDoer{
		current: quotaCurrentOnePlan,
		balance: `{"code":0,"data":{"balances":[{"entitlement_id":"e1","show_name":"GLM-5.3",
		 "total_units":100,"used_units":30,"remaining_units":70,"user_plan_id":"u1","plan_id":"start-plan-1"}]}}`,
	}}
	server.quotaClient = doer
	acc := store.GetAccount("acc1")

	server.forceQuotaRefresh(acc)
	waitFor(t, func() bool { return doer.balanceCalls.Load() >= 1 })
	server.forceQuotaRefresh(acc)
	time.Sleep(300 * time.Millisecond)
	if got := doer.balanceCalls.Load(); got != 1 {
		t.Fatalf("second call within 30s must be deduped, balance calls = %d", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within 3s")
}

// 选号本来就跳过额度缓存说「明确耗尽」的号（p.healthy 检查
// Granted>0 && Remaining<=0）；钉住这个行为，别在重构选号时弄丢。
func TestPoolSkipsQuotaExhaustedAccount(t *testing.T) {
	store := newPoolTestStore(t)
	store.UpdateAccountMemory("a1", func(acc *Account) { acc.Quota = Quota{Granted: 1000, Remaining: 0} })
	pool := NewPool(store)
	acc, release, err := pool.AcquireForModel(context.Background(), "", "GLM-5.3-Flash")
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != "a2" {
		t.Fatalf("known-empty account must be skipped, got %s", acc.ID)
	}
	release()
}

// 上游按模型分桶：账号合计>0 但请求的那个模型桶为 0 时，必须跳过该号；
// 其它模型的请求不受影响。
func TestPoolSkipsModelExhaustedBucket(t *testing.T) {
	store := newPoolTestStore(t)
	// a1：Flash 桶空、GLM-5.3 桶有量（复刻 zai-1df9c927 线上现场）。
	store.UpdateAccountMemory("a1", func(acc *Account) {
		acc.Quota = Quota{
			Granted: 8_000_000, Remaining: 3_000_000,
			Models: []QuotaModel{
				{Name: "GLM-5.3", Granted: 3_000_000, Remaining: 3_000_000},
				{Name: "GLM-5.3-Flash", Granted: 5_000_000, Remaining: 0},
			},
		}
	})
	store.UpdateAccountMemory("a2", func(acc *Account) {
		acc.Quota = Quota{Granted: 8_000_000, Remaining: 8_000_000}
	})
	pool := NewPool(store)
	ctx := context.Background()

	acc, release, err := pool.AcquireForModel(ctx, "", "GLM-5.3-Flash")
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != "a2" {
		t.Fatalf("model-exhausted bucket must be skipped, got %s", acc.ID)
	}
	release()

	// a1 的 GLM-5.3 桶还有量：禁用 a2 后 GLM-5.3 请求仍能用 a1。
	store.UpdateAccountMemory("a2", func(acc *Account) { acc.Enabled = false })
	acc, release, err = pool.AcquireForModel(ctx, "", "GLM-5.3")
	if err != nil || acc == nil {
		t.Fatalf("other model's bucket is fine: acc=%#v err=%v", acc, err)
	}
	if acc.ID != "a1" {
		t.Fatalf("expected a1 for GLM-5.3, got %s", acc.ID)
	}
	release()
}

// 快照缺该模型条目 = 未知，不能当耗尽过滤（否则会把池子过滤空）。
func TestQuotaModelExhaustedUnknownModel(t *testing.T) {
	acc := &Account{Quota: Quota{Models: []QuotaModel{
		{Name: "GLM-5.3", Remaining: 3_000_000},
		{Name: "GLM-5.3-Flash", Remaining: 0},
	}}}
	if quotaModelExhausted(acc, "GLM-5.3-Air") {
		t.Fatal("missing entry must be treated as unknown, not exhausted")
	}
	if quotaModelExhausted(acc, "") || quotaModelExhausted(nil, "GLM-5.3") {
		t.Fatal("empty model / nil account must be unknown")
	}
	if !quotaModelExhausted(acc, "glm-5.3-flash") {
		t.Fatal("zero bucket must be exhausted, match case-insensitive")
	}
	if quotaModelExhausted(acc, "glm-5.3") {
		t.Fatal("bucket with quota is not exhausted")
	}
}
