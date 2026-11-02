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
	balanceResp, balanceErr := s.tryBalance(ctx, acc, currentPlan)
	var balances []Balance
	var balancePlans []PlanInfo
	if balanceResp != nil {
		balances = balanceResp.Data.Balances
		balancePlans = balanceResp.Data.Plans
	}

	// billing/current 现在对所有账号返回 plans: []（2026-10 上游行为变化）。
	// 真实套餐定义在 billing/balance 的 data.plans 里——那里同时有 name 和
	// entitlements[].effective_at，比按 balance 反推更准。没有 plans 时才退
	// 回从 balances 合成，避免面板额度恒为 0。
	if len(activePlans) == 0 && balanceErr == nil && len(balancePlans) > 0 {
		for i := range balancePlans {
			if !strings.EqualFold(balancePlans[i].Status, "active") {
				continue
			}
			activePlans = append(activePlans, &balancePlans[i])
			if currentPlan == nil || balancePlans[i].Priority > currentPlan.Priority {
				currentPlan = &balancePlans[i]
			}
		}
		if currentPlan != nil {
			quota.PlanName = currentPlan.Name
			quota.PlanID = currentPlan.PlanID
			quota.Status = "active"
		}
	}
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
	// 已领到、但 effective_at 还在未来的额度：上游此时不建 balance 桶，
	// 所以它不在 granted/remaining 里。单独列出来，别让它把「没额度」和
	// 「还没到账」混成一种状态——前者该禁用，后者只能等。
	now := time.Now().Unix()
	for _, activePlan := range activePlans {
		for _, ent := range activePlan.Entitlements {
			if ent.EffectiveAt <= now || ent.EffectiveAt == 0 {
				continue
			}
			if _, ok := byEntitlement[ent.EntitlementID]; ok {
				continue
			}
			quota.Pending = append(quota.Pending, QuotaPending{
				PlanID:      activePlan.PlanID,
				PlanName:    activePlan.Name,
				Model:       ent.ShowName,
				Amount:      ent.GrantUnits,
				EffectiveAt: ent.EffectiveAt,
			})
		}
	}
	switch {
	case balanceErr != nil:
		// 查不到余额：保持 local。这时候不知道有没有额度，谁都不能据此禁用账号。
		quota.Source = "local"
	case len(balances) > 0:
		quota.Source = "api"
	case len(quota.Pending) > 0:
		// 有套餐、有授予额度，但一个 balance 桶都还没建：活动没到生效时间。
		// 这和「上游确认没套餐」是两回事，不能触发自动禁用。
		quota.Source = "api-pending"
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
			// 没有 balance 桶、生效时间还在未来：这份额度现在用不了。
			// 算进 granted 会让选号以为它有产能，发出去就是 1005。
			// 它已经进了 quota.Pending，这里跳过。
			if !ok && ent.EffectiveAt > now {
				continue
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
	// 面板上显示的套餐名要跟着"哪份额度真的能用"走，而不是优先级最高的那份。
	// 例：领了优先级 111 的 GLOBAL BUILD（3 亿、23:00 才生效），号上原有的
	// 0817 还在用（8M 立即可用）——这时候 granted 来自 0817，名字也该是 0817，
	// 否则概览会把 8M 错算进 GLOBAL BUILD 名下。
	if best := displayPlan(quota.Plans); best != nil {
		quota.PlanName = best.Name
		quota.PlanID = best.PlanID
		quota.Status = best.Status
		quota.EndsAt = best.EndsAt
		if currentPlan == nil {
			currentPlan = &PlanInfo{PlanID: best.PlanID, Name: best.Name, Status: best.Status}
		}
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
		// 已领到但还没生效：现在发请求必吃 1005，先别让选号挑中它。
		if quota.Granted == 0 && len(quota.Pending) > 0 {
			a.ExhaustedUntil = time.Now().Add(10 * time.Minute).Unix()
			a.LastError = "套餐待生效"
		} else if quota.Remaining <= 0 && quota.Granted > 0 {
			a.ExhaustedUntil = time.Now().Add(10 * time.Minute).Unix()
			a.LastError = "额度耗尽"
		} else {
			if a.ExhaustedUntil < time.Now().Unix() {
				a.ExhaustedUntil = 0
			}
			if a.LastError == "额度耗尽" || a.LastError == "套餐待生效" {
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
		case updated.LastError == "套餐待生效":
			s.recordAccountEvent(updated, "额度待生效", fmt.Sprintf("%s，到账时间 %s",
				pendingSummary(quota.Pending), pendingWhen(quota.Pending)))
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

func pendingSummary(items []QuotaPending) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, fmt.Sprintf("%s %d", item.Model, item.Amount))
	}
	return strings.Join(parts, "，")
}

// displayPlan 挑面板上该显示哪个套餐名：优先「真的还有可用额度」的那份，
// 都没额度时才退回优先级最高的。避免把 A 套餐的额度记到 B 套餐名下。
func displayPlan(plans []QuotaPlan) *QuotaPlan {
	var withQuota *QuotaPlan
	var byPriority *QuotaPlan
	for i := range plans {
		p := &plans[i]
		if p.Remaining > 0 && (withQuota == nil || p.Remaining > withQuota.Remaining) {
			withQuota = p
		}
		if byPriority == nil || p.Priority > byPriority.Priority {
			byPriority = p
		}
	}
	if withQuota != nil {
		return withQuota
	}
	return byPriority
}

func pendingWhen(items []QuotaPending) string {
	for _, item := range items {
		if item.EffectiveAt > 0 {
			return time.Unix(item.EffectiveAt, 0).Format("2006-01-02 15:04")
		}
	}
	return "未知"
}

func (s *Server) tryBalance(ctx context.Context, acc *Account, plan *PlanInfo) (*PlanResponse, error) {
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
	var parsed PlanResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, err
	}
	if parsed.Code != 0 || resp.StatusCode != 200 {
		return nil, fmt.Errorf("balance code=%d HTTP %d", parsed.Code, resp.StatusCode)
	}
	return &parsed, nil
}
