package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (s *Server) quotaLoop(ctx context.Context) {
	for {
		s.refreshAllQuotas(ctx)
		interval := time.Duration(s.store.Settings().QuotaIntervalSec) * time.Second
		if interval < 10*time.Second {
			interval = 30 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (s *Server) refreshAllQuotas(ctx context.Context) {
	if s.store.db != nil {
		s.store.db.PruneBindings(time.Now().Unix())
	}
	accounts := s.store.AccountsForRefresh()
	sem := make(chan struct{}, 2)
	done := make(chan struct{}, len(accounts))
	spawned := 0
	for _, acc := range accounts {
		if !acc.Enabled {
			continue
		}
		acc := acc
		sem <- struct{}{}
		spawned++
		go func() {
			defer func() { <-sem; done <- struct{}{} }()
			_ = s.refreshQuota(ctx, acc)
		}()
	}
	// 只等真正启动的 goroutine：禁用账号不会发 done，按 len(accounts) 等会永久卡死
	for i := 0; i < spawned; i++ {
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) refreshQuota(ctx context.Context, acc *Account) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	plan, err := fetchCurrentPlan(ctx, s.upstream, acc.ZcodeJWT, s.profileHeaders(acc), *s.store.EnsureProfile(acc))
	if err != nil {
		s.store.UpdateAccount(acc.ID, func(a *Account) {
			a.Quota.Error = err.Error()
			a.Quota.UpdatedAt = time.Now().Unix()
		})
		return err
	}
	quota := Quota{Source: "local", UpdatedAt: time.Now().Unix()}
	var activePlans []*PlanInfo
	var currentPlan *PlanInfo
	for i := range plan.Data.Plans {
		if !strings.EqualFold(plan.Data.Plans[i].Status, "active") {
			continue
		}
		activePlans = append(activePlans, &plan.Data.Plans[i])
		if currentPlan == nil || plan.Data.Plans[i].Priority > currentPlan.Priority {
			currentPlan = &plan.Data.Plans[i]
		}
	}
	if currentPlan != nil {
		quota.PlanName = currentPlan.Name
		quota.PlanID = currentPlan.PlanID
		quota.Status = currentPlan.Status
		quota.EndsAt = currentPlan.EndsAt
	}
	usageByModel := map[string]UsageTotals{}
	for name, totals := range acc.Models {
		if totals != nil {
			usageByModel[name] = *totals
		}
	}
	balances, balanceErr := s.tryBalance(ctx, acc, currentPlan)

	// billing/current 现在对所有账号返回 plans: []（2026-10 上游行为变化），
	// 真实额度只在 billing/balance 里。没有 active plan 时按 balance 合成计划，
	// 复用下面的聚合逻辑，避免面板额度恒为 0。
	if len(activePlans) == 0 && balanceErr == nil && len(balances) > 0 {
		groups := map[string]*PlanInfo{}
		var order []string
		for _, b := range balances {
			key := b.PlanID + "|" + b.UserPlanID
			p, ok := groups[key]
			if !ok {
				p = &PlanInfo{
					UserPlanID: b.UserPlanID,
					PlanID:     b.PlanID,
					Name:       b.PlanID,
					Status:     "active",
				}
				groups[key] = p
				order = append(order, key)
			}
			p.Entitlements = append(p.Entitlements, PlanEntitlement{
				EntitlementID: b.EntitlementID,
				ShowName:      b.ShowName,
				GrantUnits:    b.TotalUnits,
				Period:        "one_time",
			})
		}
		var best *PlanInfo
		bestTotal := int64(-1)
		for _, key := range order {
			p := groups[key]
			var total int64
			for _, e := range p.Entitlements {
				total += e.GrantUnits
			}
			activePlans = append(activePlans, p)
			if total > bestTotal {
				best, bestTotal = p, total
			}
		}
		if best != nil {
			currentPlan = best
			quota.PlanName = best.Name
			quota.PlanID = best.PlanID
			quota.Status = "active"
		}
	}
	byEntitlement := map[string]Balance{}
	byPlanName := map[string]Balance{}
	for _, b := range balances {
		if b.EntitlementID != "" {
			byEntitlement[b.EntitlementID] = b
		}
		if b.UserPlanID != "" {
			byPlanName[b.UserPlanID+"|"+strings.ToLower(b.ShowName)] = b
		}
	}
	switch {
	case balanceErr != nil:
		// 查不到余额：保持 local。这时候不知道有没有额度，谁都不能据此禁用账号。
		quota.Source = "local"
	case len(balances) > 0:
		quota.Source = "api"
	default:
		// 上游明确回了空列表：这个号当前没有任何余额/套餐。
		// 和 local 区分开，自动启停才有可靠依据。
		quota.Source = "api-empty"
	}
	aggregate := map[string]*QuotaModel{}
	modelOrder := make([]string, 0, 4)
	for _, activePlan := range activePlans {
		qp := QuotaPlan{
			UserPlanID: activePlan.UserPlanID,
			PlanID:     activePlan.PlanID,
			Name:       activePlan.Name,
			Status:     activePlan.Status,
			Priority:   activePlan.Priority,
			StartsAt:   activePlan.StartsAt,
			EndsAt:     activePlan.EndsAt,
			Models:     []QuotaModel{},
		}
		for _, ent := range activePlan.Entitlements {
			b, ok := byEntitlement[ent.EntitlementID]
			if !ok {
				b, ok = byPlanName[activePlan.UserPlanID+"|"+strings.ToLower(ent.ShowName)]
			}
			used := int64(0)
			if ok {
				used = b.UsedUnits
				if used == 0 && b.TotalUnits > 0 {
					used = b.TotalUnits - b.RemainingUnits
				}
			} else if totals, ok := usageByModel[ent.ShowName]; ok {
				used = int64(totals.Total())
			}
			granted := ent.GrantUnits
			if ok && b.TotalUnits > 0 {
				granted = b.TotalUnits
			}
			remaining := granted - used
			if remaining < 0 {
				remaining = 0
			}
			percent := 0.0
			if granted > 0 {
				percent = float64(remaining) / float64(granted) * 100
			}
			model := QuotaModel{
				Name: ent.ShowName, Granted: granted, Used: used, Remaining: remaining, Percent: percent, Source: quota.Source,
			}
			qp.Models = append(qp.Models, model)
			qp.Granted += granted
			qp.Used += used
			qp.Remaining += remaining
			key := strings.ToLower(ent.ShowName)
			if agg, exists := aggregate[key]; exists {
				agg.Granted += granted
				agg.Used += used
				agg.Remaining += remaining
				if agg.Granted > 0 {
					agg.Percent = float64(agg.Remaining) / float64(agg.Granted) * 100
				}
			} else {
				copy := model
				aggregate[key] = &copy
				modelOrder = append(modelOrder, key)
			}
		}
		if qp.Granted > 0 {
			qp.Percent = float64(qp.Remaining) / float64(qp.Granted) * 100
		}
		quota.Plans = append(quota.Plans, qp)
		quota.Granted += qp.Granted
		quota.Used += qp.Used
		quota.Remaining += qp.Remaining
	}
	for _, key := range modelOrder {
		quota.Models = append(quota.Models, *aggregate[key])
	}
	if quota.Granted > 0 {
		quota.Percent = float64(quota.Remaining) / float64(quota.Granted) * 100
	}
	if balanceErr != nil && quota.Source == "local" {
		quota.Error = "" // local accounting is still valid; do not show an API error
	}
	prevError := acc.LastError
	prevPlan, prevGranted := acc.Quota.PlanName, acc.Quota.Granted
	s.store.UpdateAccount(acc.ID, func(a *Account) {
		a.Quota = quota
		if currentPlan != nil {
			if strings.EqualFold(currentPlan.Status, "active") {
				if strings.Contains(currentPlan.PlanID, "start-plan") {
					a.Mode = "start-plan"
				} else {
					a.Mode = "coding-plan"
				}
			} else {
				a.ExhaustedUntil = time.Now().Add(10 * time.Minute).Unix()
				a.LastError = "套餐已过期"
			}
		}
		if quota.Remaining <= 0 && quota.Granted > 0 {
			a.ExhaustedUntil = time.Now().Add(10 * time.Minute).Unix()
			a.LastError = "额度耗尽"
		} else {
			if a.ExhaustedUntil < time.Now().Unix() {
				a.ExhaustedUntil = 0
			}
			if a.LastError == "额度耗尽" {
				a.LastError = ""
				a.LastErrorAt = 0
			}
		}
	})
	// 额度耗尽 / 恢复是状态变化，各记一条。只比 LastError：
	// ExhaustedUntil 在耗尽期间每轮都会往后滚，比它会把日志刷爆。
	if updated := s.store.GetAccount(acc.ID); updated != nil && updated.LastError != prevError {
		switch {
		case updated.LastError == "额度耗尽":
			s.recordAccountEvent(updated, "额度耗尽", fmt.Sprintf("剩余 %d / 共 %d", quota.Remaining, quota.Granted))
		case prevError == "额度耗尽" && updated.LastError == "":
			s.recordAccountEvent(updated, "额度恢复", fmt.Sprintf("剩余 %d / 共 %d", quota.Remaining, quota.Granted))
		case updated.LastError == "套餐已过期":
			s.recordAccountEvent(updated, "套餐过期", "上游返回非 active 状态")
		}
	}
	// 套餐变化也记一条：换套餐、套餐到期、额度到账/清零。
	// 只在「套餐名变了」或「授予额度 0↔非0」时写，避免 local/api 源切换刷屏。
	if updated := s.store.GetAccount(acc.ID); updated != nil {
		newPlan, newGranted := updated.Quota.PlanName, updated.Quota.Granted
		switch {
		case newPlan != prevPlan && newPlan == "":
			s.recordAccountEvent(updated, "套餐到期", fmt.Sprintf("原套餐 %s，授予 %d", prevPlan, prevGranted))
		case newPlan != prevPlan && prevPlan == "":
			s.recordAccountEvent(updated, "新套餐", fmt.Sprintf("%s，授予 %d", newPlan, newGranted))
		case newPlan != prevPlan:
			s.recordAccountEvent(updated, "套餐变更", fmt.Sprintf("%s → %s", prevPlan, newPlan))
		case prevGranted == 0 && newGranted > 0:
			s.recordAccountEvent(updated, "额度到账", fmt.Sprintf("授予 %d", newGranted))
		case prevGranted > 0 && newGranted == 0:
			s.recordAccountEvent(updated, "额度清零", fmt.Sprintf("原授予 %d", prevGranted))
		}
	}
	if quota.Granted > 0 && quota.Remaining <= 0 {
		// Hard failure: no session can stay on an account without quota.
		s.pool.InvalidateAccount(acc.ID)
	}
	return nil
}

func (s *Server) tryBalance(ctx context.Context, acc *Account, plan *PlanInfo) ([]Balance, error) {
	base := "https://zcode.z.ai/api/v1/zcode-plan/billing/balance"
	headers := s.profileHeaders(acc)
	headers["Authorization"] = "Bearer " + acc.ZcodeJWT
	headers["Accept"] = "application/json"
	headers["X-Request-Id"] = randomUUID()
	headers["X-ZCode-Session-Type"] = "main"
	headers["X-ZCode-Trace-Id"] = randomUUID()
	headers["X-Query-Id"] = "01" + randomHex(11)
	headers["X-ZCode-Query-Source"] = "main_turn"
	headers["X-Session-Id"] = "session_" + randomHex(16)
	query := "?app_version=" + appVersion
	if plan != nil && plan.UserPlanID != "" {
		query += "&user_plan_id=" + plan.UserPlanID
	}
	resp, err := doRequest(ctx, s.upstream, http.MethodGet, base+query, headers, nil)
	if err != nil {
		return nil, err
	}
	data, err := readJSON(resp)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Balances []Balance `json:"balances"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, err
	}
	if parsed.Code != 0 || resp.StatusCode != 200 {
		return nil, fmt.Errorf("balance code=%d HTTP %d", parsed.Code, resp.StatusCode)
	}
	return parsed.Data.Balances, nil
}
