package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestActivePlansOf(t *testing.T) {
	plans := []PlanInfo{
		{PlanID: "p1", Name: "A", Status: "active", Priority: 1},
		{PlanID: "p2", Name: "B", Status: "expired", Priority: 9},
		{PlanID: "p3", Name: "C", Status: "ACTIVE", Priority: 5},
	}
	active, current := activePlansOf(plans)
	if len(active) != 2 || active[0].PlanID != "p1" || active[1].PlanID != "p3" {
		t.Fatalf("active plans: %#v", active)
	}
	if current == nil || current.PlanID != "p3" {
		t.Fatalf("current should be the highest priority active plan, got %#v", current)
	}
	if _, cur := activePlansOf(nil); cur != nil {
		t.Fatalf("empty input should give nil current, got %#v", cur)
	}
}

func TestPlansFromBalances(t *testing.T) {
	balances := []Balance{
		{EntitlementID: "e1", ShowName: "GLM-5.3", PlanID: "p1", UserPlanID: "u1", TotalUnits: 100},
		{EntitlementID: "e2", ShowName: "GLM-5.3-Flash", PlanID: "p1", UserPlanID: "u1", TotalUnits: 50},
		{EntitlementID: "e3", ShowName: "GLM-5.3", PlanID: "p2", UserPlanID: "u2", TotalUnits: 200},
	}
	active, current := plansFromBalances(balances)
	if len(active) != 2 {
		t.Fatalf("expected 2 synthesised plans, got %#v", active)
	}
	// p1 的两条权益合计 150，p2 一条 200 —— current 取合计最大的那份。
	if current == nil || current.PlanID != "p2" {
		t.Fatalf("current should be p2 (largest total), got %#v", current)
	}
	p1 := active[0]
	if p1.Name != "p1" || p1.Status != "active" || p1.UserPlanID != "u1" {
		t.Fatalf("synthesised plan fields: %#v", p1)
	}
	if len(p1.Entitlements) != 2 || p1.Entitlements[0].GrantUnits != 100 || p1.Entitlements[1].GrantUnits != 50 {
		t.Fatalf("entitlements: %#v", p1.Entitlements)
	}
	if p1.Entitlements[0].Period != "one_time" {
		t.Fatalf("synthesised period: %q", p1.Entitlements[0].Period)
	}
}

func TestIndexBalances(t *testing.T) {
	byEntitlement, byPlanName := indexBalances([]Balance{
		{EntitlementID: "e1", ShowName: "GLM-5.3", UserPlanID: "u1", TotalUnits: 10},
		{EntitlementID: "", ShowName: "GLM-5.3-Flash", UserPlanID: "u1", TotalUnits: 20},
		{EntitlementID: "e3", ShowName: "X", UserPlanID: "", TotalUnits: 30},
	})
	if got := byEntitlement["e1"].TotalUnits; got != 10 {
		t.Fatalf("entitlement index: %d", got)
	}
	if _, ok := byEntitlement[""]; ok {
		t.Fatal("empty entitlement id must not be indexed")
	}
	if got := byPlanName["u1|glm-5.3"].TotalUnits; got != 10 {
		t.Fatalf("plan-name index should be lower-cased, got %d", got)
	}
	if got := byPlanName["u1|glm-5.3-flash"].TotalUnits; got != 20 {
		t.Fatalf("plan-name index fallback: %d", got)
	}
	if _, ok := byPlanName["|x"]; ok {
		t.Fatal("empty user_plan_id must not be indexed")
	}
}

func TestPendingEntitlements(t *testing.T) {
	const now = int64(1000)
	plans := []*PlanInfo{{
		PlanID: "p1", Name: "P1",
		Entitlements: []PlanEntitlement{
			{EntitlementID: "future", ShowName: "GLM-5.3", GrantUnits: 100, EffectiveAt: 2000},
			{EntitlementID: "has-bucket", ShowName: "GLM-5.3-Flash", GrantUnits: 50, EffectiveAt: 2000},
			{EntitlementID: "past", ShowName: "GLM-5.3", GrantUnits: 10, EffectiveAt: 500},
			{EntitlementID: "no-time", ShowName: "GLM-5.3", GrantUnits: 10},
		},
	}}
	byEntitlement := map[string]Balance{"has-bucket": {TotalUnits: 50}}

	got := pendingEntitlements(plans, byEntitlement, now)
	if len(got) != 1 {
		t.Fatalf("only the future entitlement without a bucket is pending: %#v", got)
	}
	want := QuotaPending{PlanID: "p1", PlanName: "P1", Model: "GLM-5.3", Amount: 100, EffectiveAt: 2000}
	if got[0] != want {
		t.Fatalf("pending entry: %#v want %#v", got[0], want)
	}
}

func TestQuotaSourceFor(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		balances []Balance
		pending  []QuotaPending
		want     string
	}{
		{"查余额失败时保持 local", errors.New("boom"), []Balance{{TotalUnits: 1}}, []QuotaPending{{Model: "x"}}, "local"},
		{"有余额桶", nil, []Balance{{TotalUnits: 1}}, []QuotaPending{{Model: "x"}}, "api"},
		{"只有待生效", nil, nil, []QuotaPending{{Model: "x"}}, "api-pending"},
		{"上游明确回空", nil, nil, nil, "api-empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quotaSourceFor(tc.err, tc.balances, tc.pending); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestUsageSnapshot(t *testing.T) {
	models := map[string]*UsageTotals{
		"GLM-5.3": {Requests: 2, Usage: Usage{InputTokens: 10}},
		"nil":     nil,
	}
	got := usageSnapshot(models)
	if len(got) != 1 || got["GLM-5.3"].InputTokens != 10 {
		t.Fatalf("snapshot: %#v", got)
	}
	// 快照是拷贝，改原对象不该影响结果。
	models["GLM-5.3"].InputTokens = 99
	if got["GLM-5.3"].InputTokens != 10 {
		t.Fatalf("snapshot aliased the source: %#v", got)
	}
}

func TestBuildQuotaPlans(t *testing.T) {
	const now = int64(1000)
	onePlan := func(ents ...PlanEntitlement) []*PlanInfo {
		return []*PlanInfo{{PlanID: "p1", Name: "P1", Status: "active", UserPlanID: "u1", Entitlements: ents}}
	}

	t.Run("余额桶决定授予与已用", func(t *testing.T) {
		plans := onePlan(PlanEntitlement{EntitlementID: "e1", ShowName: "GLM-5.3", GrantUnits: 999})
		byEntitlement := map[string]Balance{"e1": {TotalUnits: 100, UsedUnits: 30, RemainingUnits: 70}}
		got := buildQuotaPlans(plans, byEntitlement, nil, nil, now, "api")
		if len(got) != 1 || len(got[0].Models) != 1 {
			t.Fatalf("plans: %#v", got)
		}
		model := got[0].Models[0]
		if model.Granted != 100 || model.Used != 30 || model.Remaining != 70 {
			t.Fatalf("model should follow the balance bucket, got %#v", model)
		}
		if model.Source != "api" {
			t.Fatalf("model source: %q", model.Source)
		}
		if got[0].Granted != 100 || got[0].Used != 30 || got[0].Remaining != 70 || got[0].Percent != 70 {
			t.Fatalf("plan totals: %#v", got[0])
		}
	})

	t.Run("UsedUnits 为 0 时按 total-remaining 反推", func(t *testing.T) {
		plans := onePlan(PlanEntitlement{EntitlementID: "e1", ShowName: "GLM-5.3"})
		byEntitlement := map[string]Balance{"e1": {TotalUnits: 100, UsedUnits: 0, RemainingUnits: 60}}
		got := buildQuotaPlans(plans, byEntitlement, nil, nil, now, "api")
		if got[0].Models[0].Used != 40 {
			t.Fatalf("used should be total-remaining, got %#v", got[0].Models[0])
		}
	})

	t.Run("没有余额桶且生效时间在未来时跳过", func(t *testing.T) {
		plans := onePlan(PlanEntitlement{EntitlementID: "e1", ShowName: "GLM-5.3", GrantUnits: 100, EffectiveAt: now + 1})
		got := buildQuotaPlans(plans, map[string]Balance{}, nil, nil, now, "api")
		if len(got[0].Models) != 0 || got[0].Granted != 0 {
			t.Fatalf("future entitlement must not count as capacity: %#v", got[0])
		}
	})

	t.Run("没有余额桶时用本地用量", func(t *testing.T) {
		plans := onePlan(PlanEntitlement{EntitlementID: "e1", ShowName: "GLM-5.3", GrantUnits: 100})
		usage := map[string]UsageTotals{"GLM-5.3": {Requests: 2, Usage: Usage{InputTokens: 10, OutputTokens: 5}}}
		got := buildQuotaPlans(plans, map[string]Balance{}, nil, usage, now, "local")
		model := got[0].Models[0]
		if model.Granted != 100 || model.Used != 15 || model.Remaining != 85 {
			t.Fatalf("local accounting: %#v", model)
		}
	})

	t.Run("按 user_plan_id+模型名兜底", func(t *testing.T) {
		plans := onePlan(PlanEntitlement{EntitlementID: "missing", ShowName: "GLM-5.3", GrantUnits: 10})
		byPlanName := map[string]Balance{"u1|glm-5.3": {TotalUnits: 40, UsedUnits: 10, RemainingUnits: 30}}
		got := buildQuotaPlans(plans, map[string]Balance{}, byPlanName, nil, now, "api")
		if got[0].Models[0].Granted != 40 {
			t.Fatalf("plan-name fallback: %#v", got[0].Models[0])
		}
	})

	t.Run("已用超过授予时余量钳到 0", func(t *testing.T) {
		plans := onePlan(PlanEntitlement{EntitlementID: "e1", ShowName: "GLM-5.3"})
		byEntitlement := map[string]Balance{"e1": {TotalUnits: 100, UsedUnits: 130, RemainingUnits: 0}}
		got := buildQuotaPlans(plans, byEntitlement, nil, nil, now, "api")
		if got[0].Models[0].Remaining != 0 || got[0].Models[0].Percent != 0 {
			t.Fatalf("remaining must clamp at 0: %#v", got[0].Models[0])
		}
	})

	t.Run("多权益合计到套餐", func(t *testing.T) {
		plans := onePlan(
			PlanEntitlement{EntitlementID: "e1", ShowName: "GLM-5.3"},
			PlanEntitlement{EntitlementID: "e2", ShowName: "GLM-5.3-Flash"},
		)
		byEntitlement := map[string]Balance{
			"e1": {TotalUnits: 100, UsedUnits: 20, RemainingUnits: 80},
			"e2": {TotalUnits: 50, UsedUnits: 50, RemainingUnits: 0},
		}
		got := buildQuotaPlans(plans, byEntitlement, nil, nil, now, "api")
		if got[0].Granted != 150 || got[0].Used != 70 || got[0].Remaining != 80 {
			t.Fatalf("plan totals: %#v", got[0])
		}
		if len(got[0].Models) != 2 {
			t.Fatalf("plan models: %#v", got[0].Models)
		}
	})
}

func TestAggregateQuotaModels(t *testing.T) {
	plans := []QuotaPlan{
		{Models: []QuotaModel{
			{Name: "GLM-5.3", Granted: 100, Used: 20, Remaining: 80, Percent: 80},
			{Name: "GLM-5.3-Flash", Granted: 50, Remaining: 50, Percent: 100},
		}},
		{Models: []QuotaModel{
			{Name: "glm-5.3", Granted: 100, Used: 100, Remaining: 0, Percent: 0},
		}},
	}
	got := aggregateQuotaModels(plans)
	if len(got) != 2 {
		t.Fatalf("same model must collapse into one row: %#v", got)
	}
	// 顺序按首次出现，大小写沿用第一次见到的写法。
	if got[0].Name != "GLM-5.3" || got[1].Name != "GLM-5.3-Flash" {
		t.Fatalf("order/case: %#v", got)
	}
	if got[0].Granted != 200 || got[0].Used != 120 || got[0].Remaining != 80 || got[0].Percent != 40 {
		t.Fatalf("aggregated row: %#v", got[0])
	}
	if out := aggregateQuotaModels(nil); len(out) != 0 {
		t.Fatalf("empty input: %#v", out)
	}
}

func TestApplyQuotaState(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tenMinutes := now.Add(10 * time.Minute).Unix()

	t.Run("额度耗尽", func(t *testing.T) {
		acc := &Account{}
		applyQuotaState(acc, Quota{Granted: 100, Remaining: 0}, nil, now)
		if acc.LastError != "额度耗尽" || acc.ExhaustedUntil != tenMinutes {
			t.Fatalf("exhausted: %#v", acc)
		}
		if acc.Quota.Granted != 100 {
			t.Fatalf("quota must be written back: %#v", acc.Quota)
		}
	})

	t.Run("待生效", func(t *testing.T) {
		acc := &Account{}
		applyQuotaState(acc, Quota{Pending: []QuotaPending{{Model: "GLM-5.3", Amount: 1}}}, nil, now)
		if acc.LastError != "套餐待生效" || acc.ExhaustedUntil != tenMinutes {
			t.Fatalf("pending: %#v", acc)
		}
	})

	t.Run("恢复后清掉额度错误", func(t *testing.T) {
		acc := &Account{LastError: "额度耗尽", LastErrorAt: 123, ExhaustedUntil: now.Unix() - 1}
		applyQuotaState(acc, Quota{Granted: 100, Remaining: 50}, nil, now)
		if acc.LastError != "" || acc.LastErrorAt != 0 || acc.ExhaustedUntil != 0 {
			t.Fatalf("recovery: %#v", acc)
		}
	})

	t.Run("冷却没到期且不是额度错误时保留", func(t *testing.T) {
		acc := &Account{LastError: "风控", ExhaustedUntil: now.Unix() + 300}
		applyQuotaState(acc, Quota{Granted: 100, Remaining: 50}, nil, now)
		if acc.LastError != "风控" || acc.ExhaustedUntil != now.Unix()+300 {
			t.Fatalf("transient state must survive a quota refresh: %#v", acc)
		}
	})

	t.Run("套餐过期", func(t *testing.T) {
		acc := &Account{}
		applyQuotaState(acc, Quota{Granted: 100, Remaining: 50}, &PlanInfo{Status: "expired"}, now)
		if acc.LastError != "套餐已过期" || acc.ExhaustedUntil != tenMinutes {
			t.Fatalf("expired plan: %#v", acc)
		}
	})

	t.Run("模式跟随套餐类型", func(t *testing.T) {
		start := &Account{}
		applyQuotaState(start, Quota{Granted: 1, Remaining: 1}, &PlanInfo{Status: "active", PlanID: "start-plan-1"}, now)
		if start.Mode != "start-plan" {
			t.Fatalf("start-plan mode: %q", start.Mode)
		}
		coding := &Account{}
		applyQuotaState(coding, Quota{Granted: 1, Remaining: 1}, &PlanInfo{Status: "active", PlanID: "coding-1"}, now)
		if coding.Mode != "coding-plan" {
			t.Fatalf("coding-plan mode: %q", coding.Mode)
		}
	})
}

func TestQuotaTransitions(t *testing.T) {
	cases := []struct {
		name        string
		prevError   string
		prevPlan    string
		prevGranted int64
		lastError   string
		planName    string
		granted     int64
		pending     []QuotaPending
		want        []string
	}{
		{name: "耗尽", prevPlan: "P", prevGranted: 100, lastError: "额度耗尽", planName: "P", granted: 100, want: []string{"额度耗尽"}},
		{name: "恢复", prevError: "额度耗尽", prevPlan: "P", prevGranted: 100, planName: "P", granted: 100, want: []string{"额度恢复"}},
		{name: "过期", prevPlan: "P", prevGranted: 100, lastError: "套餐已过期", planName: "P", granted: 100, want: []string{"套餐过期"}},
		{
			name: "待生效", prevPlan: "P", lastError: "套餐待生效", planName: "P",
			pending: []QuotaPending{{Model: "GLM-5.3", Amount: 5, EffectiveAt: 1800000000}},
			want:    []string{"额度待生效"},
		},
		{name: "新套餐", planName: "P", granted: 100, want: []string{"新套餐"}},
		{name: "套餐到期", prevPlan: "P", prevGranted: 100, want: []string{"套餐到期"}},
		{name: "套餐变更", prevPlan: "P", prevGranted: 100, planName: "Q", granted: 100, want: []string{"套餐变更"}},
		{name: "额度到账", prevPlan: "P", planName: "P", granted: 100, want: []string{"额度到账"}},
		{name: "额度清零", prevPlan: "P", prevGranted: 100, planName: "P", want: []string{"额度清零"}},
		{name: "无变化", prevPlan: "P", prevGranted: 100, planName: "P", granted: 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updated := &Account{
				LastError: tc.lastError,
				Quota:     Quota{PlanName: tc.planName, Granted: tc.granted},
			}
			quota := Quota{Granted: tc.granted, Pending: tc.pending}
			events := quotaTransitions(tc.prevError, tc.prevPlan, tc.prevGranted, updated, quota)
			var got []string
			for _, event := range events {
				got = append(got, event.action)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("actions = %v want %v", got, tc.want)
			}
			if tc.name == "待生效" && len(events) == 1 && !strings.Contains(events[0].detail, "GLM-5.3 5") {
				t.Fatalf("pending detail should name the model and amount: %q", events[0].detail)
			}
		})
	}
}

func TestPendingHelpers(t *testing.T) {
	if got := pendingWhen(nil); got != "未知" {
		t.Fatalf("pendingWhen(nil) = %q", got)
	}
	items := []QuotaPending{
		{Model: "GLM-5.3", Amount: 7},
		{Model: "GLM-5.3-Flash", Amount: 3, EffectiveAt: 1800000000},
	}
	if got := pendingSummary(items); got != "GLM-5.3 7，GLM-5.3-Flash 3" {
		t.Fatalf("pendingSummary = %q", got)
	}
	want := time.Unix(1800000000, 0).Format("2006-01-02 15:04")
	if got := pendingWhen(items); got != want {
		t.Fatalf("pendingWhen = %q want %q", got, want)
	}
}
