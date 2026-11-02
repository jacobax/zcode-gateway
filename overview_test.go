package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 概览页的套餐分布、额度合计与按模型拆分：只算启用中的账号。
func TestOverviewPlanAndQuotaStats(t *testing.T) {
	store := newTestStore(t, "overview")
	add := func(id, plan string, enabled bool, granted, remaining int64, models []QuotaModel) {
		acc := &Account{
			ID: id, Label: id, Provider: "zai", Mode: "start-plan",
			User: UserInfo{ID: "u-" + id}, Enabled: enabled, CreatedAt: 1,
			Models: map[string]*UsageTotals{},
			Quota:  Quota{Granted: granted, Remaining: remaining, PlanName: plan, Models: models},
		}
		if err := store.AddAccount(acc); err != nil {
			t.Fatal(err)
		}
	}
	add("a1", "plan-weekend", true, 1000, 600, []QuotaModel{{Name: "GLM-5.3", Granted: 1000, Remaining: 600}})
	add("a2", "plan-weekend", true, 1000, 400, []QuotaModel{{Name: "GLM-5.3", Granted: 1000, Remaining: 400}})
	// 禁用号：不该计入额度合计，也不该出现在按模型统计里
	add("a3", "plan-old", false, 5000, 5000, []QuotaModel{{Name: "GLM-5.3-Flash", Granted: 5000, Remaining: 5000}})
	add("a4", "", true, 0, 0, nil) // 没套餐

	server := NewServer(store, mustUpstream(t), "http://test")
	rec := httptest.NewRecorder()
	server.handleOverview(rec, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("overview: %d %s", rec.Code, rec.Body.String())
	}

	var overview struct {
		Accounts struct {
			Total     int `json:"total"`
			Enabled   int `json:"enabled"`
			Available int `json:"available"`
			Plans     []struct {
				Name      string `json:"name"`
				Count     int    `json:"count"`
				Enabled   int    `json:"enabled"`
				Granted   int64  `json:"granted"`
				Remaining int64  `json:"remaining"`
			} `json:"plans"`
			Quota struct {
				Granted   int64   `json:"granted"`
				Used      int64   `json:"used"`
				Remaining int64   `json:"remaining"`
				Percent   float64 `json:"percent"`
				Accounts  int     `json:"accounts"`
				Models    []struct {
					Name      string  `json:"name"`
					Accounts  int     `json:"accounts"`
					Granted   int64   `json:"granted"`
					Remaining int64   `json:"remaining"`
					Percent   float64 `json:"percent"`
				} `json:"models"`
			} `json:"quota"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	accounts := overview.Accounts

	if accounts.Total != 4 || accounts.Enabled != 3 || accounts.Available != 2 {
		t.Fatalf("unexpected counts: %+v", accounts)
	}
	if accounts.Quota.Granted != 2000 || accounts.Quota.Remaining != 1000 || accounts.Quota.Used != 1000 {
		t.Fatalf("quota totals should ignore disabled accounts: %+v", accounts.Quota)
	}
	if accounts.Quota.Percent != 50 || accounts.Quota.Accounts != 2 {
		t.Fatalf("unexpected quota percent/accounts: %+v", accounts.Quota)
	}

	// 按模型拆分：只有启用中的 GLM-5.3 两个号
	if len(accounts.Quota.Models) != 1 {
		t.Fatalf("expected exactly one model row, got %+v", accounts.Quota.Models)
	}
	model := accounts.Quota.Models[0]
	if model.Name != "GLM-5.3" || model.Accounts != 2 || model.Granted != 2000 || model.Remaining != 1000 || model.Percent != 50 {
		t.Fatalf("unexpected model quota row: %+v", model)
	}

	// 套餐分布：weekend / old / 无套餐 三桶，按剩余降序
	if len(accounts.Plans) != 3 {
		t.Fatalf("expected 3 plan buckets, got %+v", accounts.Plans)
	}
	if accounts.Plans[0].Name != "plan-weekend" {
		t.Fatalf("plans should be sorted by remaining desc, got %+v", accounts.Plans)
	}
	weekend := accounts.Plans[0]
	if weekend.Count != 2 || weekend.Enabled != 2 || weekend.Granted != 2000 || weekend.Remaining != 1000 {
		t.Fatalf("unexpected weekend plan bucket: %+v", weekend)
	}
	var oldPlan, noPlan bool
	for _, plan := range accounts.Plans {
		switch plan.Name {
		case "plan-old":
			oldPlan = true
			if plan.Count != 1 || plan.Enabled != 0 || plan.Remaining != 0 {
				t.Fatalf("disabled plan should not contribute quota: %+v", plan)
			}
		case "无套餐":
			noPlan = true
		}
	}
	if !oldPlan || !noPlan {
		t.Fatalf("missing buckets: %+v", accounts.Plans)
	}
}

// 面板套餐名要跟着"哪份额度真能用"走：领到一份还没生效的高优先级套餐时，
// 不能把原有套餐的额度记到它名下。
func TestDisplayPlanPrefersPlanWithQuota(t *testing.T) {
	plans := []QuotaPlan{
		{PlanID: "plan-pending", Name: "GLOBAL BUILD", Priority: 111, Granted: 0, Remaining: 0},
		{PlanID: "plan-active", Name: "Start Plan", Priority: 90, Granted: 8000000, Remaining: 8000000},
	}
	if got := displayPlan(plans); got == nil || got.PlanID != "plan-active" {
		t.Fatalf("应选还有额度的那份，got %+v", got)
	}
	// 都有额度：挑剩余多的
	both := []QuotaPlan{
		{PlanID: "a", Priority: 111, Remaining: 100},
		{PlanID: "b", Priority: 90, Remaining: 900},
	}
	if got := displayPlan(both); got == nil || got.PlanID != "b" {
		t.Fatalf("应挑剩余额度最多的，got %+v", got)
	}
	// 都没额度：退回优先级最高的（这样面板还能显示"我领到了什么"）
	none := []QuotaPlan{
		{PlanID: "a", Priority: 90},
		{PlanID: "b", Priority: 111},
	}
	if got := displayPlan(none); got == nil || got.PlanID != "b" {
		t.Fatalf("都没额度时应退回优先级最高的，got %+v", got)
	}
	if got := displayPlan(nil); got != nil {
		t.Fatalf("空列表应返回 nil，got %+v", got)
	}
}
