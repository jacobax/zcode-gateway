package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

// 这一组测试跑的是 refreshQuota 的编排层（取数 → 选套餐 → 算额度 → 落状态 →
// 记事件 → 踢会话），上游用 fakeQuotaDoer 顶掉，全程不联网。

type fakeQuotaDoer struct {
	current       string
	balance       string
	currentStatus int
	balanceStatus int
}

func (d fakeQuotaDoer) Do(req *fhttp.Request) (*fhttp.Response, error) {
	switch {
	case strings.Contains(req.URL.Path, "/billing/current"):
		return fakeJSON(d.currentStatus, d.current), nil
	case strings.Contains(req.URL.Path, "/billing/balance"):
		return fakeJSON(d.balanceStatus, d.balance), nil
	default:
		return fakeJSON(404, `{"code":404,"msg":"unexpected path"}`), nil
	}
}

func fakeJSON(status int, body string) *fhttp.Response {
	if status == 0 {
		status = 200
	}
	return &fhttp.Response{StatusCode: status, Header: fhttp.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func newQuotaTestServer(t *testing.T) (*Server, *Store) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	acc := &Account{ID: "acc1", Label: "test", Enabled: true, ZcodeJWT: "jwt", Mode: "start-plan", Models: map[string]*UsageTotals{}}
	if err := store.AddAccount(acc); err != nil {
		t.Fatal(err)
	}
	return NewServer(store, &Upstream{}, "http://test"), store
}

func accountEventActions(store *Store) []string {
	var out []string
	for _, e := range store.Logs(20, "account", "") {
		out = append(out, e.Model)
	}
	return out
}

const quotaCurrentOnePlan = `{"code":0,"data":{"plans":[
 {"plan_id":"start-plan-1","name":"Start","status":"active","priority":1,
  "entitlements":[{"entitlement_id":"e1","show_name":"GLM-5.3","grant_units":100}]}]}}`

func TestRefreshQuotaHappyPath(t *testing.T) {
	server, store := newQuotaTestServer(t)
	server.quotaClient = fakeQuotaDoer{
		current: quotaCurrentOnePlan,
		balance: `{"code":0,"data":{"balances":[{"entitlement_id":"e1","show_name":"GLM-5.3",
		 "total_units":100,"used_units":30,"remaining_units":70,"user_plan_id":"u1","plan_id":"start-plan-1"}]}}`,
	}
	if err := server.refreshQuota(context.Background(), store.GetAccount("acc1")); err != nil {
		t.Fatalf("refreshQuota: %v", err)
	}
	updated := store.GetAccount("acc1")
	if updated.Quota.Source != "api" || updated.Quota.Error != "" {
		t.Fatalf("source/error: %#v", updated.Quota)
	}
	if updated.Quota.Granted != 100 || updated.Quota.Used != 30 || updated.Quota.Remaining != 70 || updated.Quota.Percent != 70 {
		t.Fatalf("totals: %#v", updated.Quota)
	}
	if updated.Quota.PlanName != "Start" || updated.Quota.PlanID != "start-plan-1" {
		t.Fatalf("plan: %#v", updated.Quota)
	}
	if len(updated.Quota.Plans) != 1 || len(updated.Quota.Models) != 1 || len(updated.Quota.Pending) != 0 {
		t.Fatalf("shape: %#v", updated.Quota)
	}
	if updated.Mode != "start-plan" || updated.ExhaustedUntil != 0 || updated.LastError != "" {
		t.Fatalf("state: mode=%q exhausted=%d lastError=%q", updated.Mode, updated.ExhaustedUntil, updated.LastError)
	}
	if actions := accountEventActions(store); len(actions) != 1 || actions[0] != "新套餐" {
		t.Fatalf("events: %v", actions)
	}
}

func TestRefreshQuotaExhausted(t *testing.T) {
	server, store := newQuotaTestServer(t)
	server.quotaClient = fakeQuotaDoer{
		current: quotaCurrentOnePlan,
		balance: `{"code":0,"data":{"balances":[{"entitlement_id":"e1","show_name":"GLM-5.3",
		 "total_units":100,"used_units":100,"remaining_units":0,"user_plan_id":"u1","plan_id":"start-plan-1"}]}}`,
	}
	now := time.Now().Unix()
	store.db.Bind("sess", "acc1", now, 600)

	if err := server.refreshQuota(context.Background(), store.GetAccount("acc1")); err != nil {
		t.Fatalf("refreshQuota: %v", err)
	}
	updated := store.GetAccount("acc1")
	if updated.LastError != "额度耗尽" || updated.ExhaustedUntil <= now {
		t.Fatalf("exhausted state: %#v", updated)
	}
	if _, ok := store.db.GetBinding("sess", now+1); ok {
		t.Fatal("exhausted account must drop session bindings")
	}
	if actions := strings.Join(accountEventActions(store), ","); !strings.Contains(actions, "额度耗尽") {
		t.Fatalf("events: %v", actions)
	}
}

func TestRefreshQuotaPending(t *testing.T) {
	server, store := newQuotaTestServer(t)
	effective := time.Now().Add(time.Hour).Unix()
	server.quotaClient = fakeQuotaDoer{
		current: fmt.Sprintf(`{"code":0,"data":{"plans":[{"plan_id":"p1","name":"P1","status":"active","priority":1,
		 "entitlements":[{"entitlement_id":"e9","show_name":"GLM-5.3","grant_units":500,"effective_at":%d}]}]}}`, effective),
		balance: `{"code":0,"data":{}}`,
	}
	if err := server.refreshQuota(context.Background(), store.GetAccount("acc1")); err != nil {
		t.Fatalf("refreshQuota: %v", err)
	}
	updated := store.GetAccount("acc1")
	if updated.Quota.Source != "api-pending" {
		t.Fatalf("source: %q", updated.Quota.Source)
	}
	if updated.Quota.Granted != 0 || len(updated.Quota.Pending) != 1 {
		t.Fatalf("future entitlement must not count as capacity: %#v", updated.Quota)
	}
	if updated.LastError != "套餐待生效" || updated.ExhaustedUntil == 0 {
		t.Fatalf("pending state: %#v", updated)
	}
	if actions := strings.Join(accountEventActions(store), ","); !strings.Contains(actions, "额度待生效") {
		t.Fatalf("events: %v", actions)
	}
}

func TestRefreshQuotaBothFetchesFail(t *testing.T) {
	server, store := newQuotaTestServer(t)
	server.quotaClient = fakeQuotaDoer{
		currentStatus: 500, current: `{"code":500,"msg":"boom"}`,
		balanceStatus: 500, balance: `{"code":500,"msg":"boom"}`,
	}
	if err := server.refreshQuota(context.Background(), store.GetAccount("acc1")); err == nil {
		t.Fatal("expected an error when both fetches fail")
	}
	updated := store.GetAccount("acc1")
	if updated.Quota.Source != "local" || updated.Quota.Error != "" {
		t.Fatalf("local fallback must not surface the API error: %#v", updated.Quota)
	}
	if updated.LastError != "" || updated.ExhaustedUntil != 0 {
		t.Fatalf("unknown quota must not disable the account: %#v", updated)
	}
}

func TestRefreshQuotaPlanFailsBalanceOK(t *testing.T) {
	server, store := newQuotaTestServer(t)
	server.quotaClient = fakeQuotaDoer{
		currentStatus: 500, current: `{"code":500,"msg":"boom"}`,
		balance: `{"code":0,"data":{
		 "plans":[{"plan_id":"p2","name":"P2","status":"active","priority":2,
		  "entitlements":[{"entitlement_id":"e2","show_name":"GLM-5.3","grant_units":200}]}],
		 "balances":[{"entitlement_id":"e2","show_name":"GLM-5.3","total_units":200,
		  "used_units":50,"remaining_units":150,"user_plan_id":"u2","plan_id":"p2"}]}}`,
	}
	if err := server.refreshQuota(context.Background(), store.GetAccount("acc1")); err != nil {
		t.Fatalf("one failed fetch should not fail the refresh: %v", err)
	}
	updated := store.GetAccount("acc1")
	if updated.Quota.Source != "api" || updated.Quota.PlanID != "p2" || updated.Quota.Remaining != 150 {
		t.Fatalf("balance should drive the quota: %#v", updated.Quota)
	}
	if updated.Quota.Error == "" {
		t.Fatal("billing/current error should still be reported while balance data is used")
	}
}

func TestQuotaHTTPFallsBackToUpstream(t *testing.T) {
	server, _ := newQuotaTestServer(t)
	if got := server.quotaHTTP(); got != HTTPDoer(server.upstream) {
		t.Fatalf("nil quotaClient must fall back to upstream, got %#v", got)
	}
	fake := fakeQuotaDoer{}
	server.quotaClient = fake
	if got := server.quotaHTTP(); got != HTTPDoer(fake) {
		t.Fatalf("injected client must win, got %#v", got)
	}
}
