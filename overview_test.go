package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 概览页的套餐分布与额度合计：只算启用中的账号，禁用号不计入剩余额度。
func TestOverviewPlanAndQuotaStats(t *testing.T) {
	store := newTestStore(t, "overview")
	add := func(id, plan string, enabled bool, granted, remaining int64) {
		acc := &Account{
			ID: id, Label: id, Provider: "zai", Mode: "start-plan",
			User: UserInfo{ID: "u-" + id}, Enabled: enabled, CreatedAt: 1,
			Models: map[string]*UsageTotals{},
			Quota:  Quota{Granted: granted, Remaining: remaining, PlanName: plan},
		}
		if err := store.AddAccount(acc); err != nil {
			t.Fatal(err)
		}
	}
	add("a1", "plan-weekend", true, 1000, 600)
	add("a2", "plan-weekend", true, 1000, 400)
	add("a3", "plan-old", false, 5000, 5000) // 禁用：不该计入额度合计
	add("a4", "", true, 0, 0)                // 没套餐

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
	if len(accounts.Plans) != 3 {
		t.Fatalf("expected 3 plan buckets (weekend/old/无套餐), got %d: %+v", len(accounts.Plans), accounts.Plans)
	}
	// 按剩余额度降序：plan-old 全禁用 → 0，plan-weekend 1000，无套餐 0
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
