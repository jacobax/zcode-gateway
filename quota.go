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
		// 禁用的号也要刷，但只刷「额度待生效」的：它们被禁用唯一的原因就是
		// 额度还没到 effective_at，不刷新就永远发现不了到账——claimer 靠
		// remaining > 0 决定启用，网关不更新它就只能永远等下去。
		// 真正停用的号（401 / 手动禁用 / 上游确认没套餐）不刷，省掉无谓请求。
		if !acc.Enabled && acc.Quota.Source != "api-pending" {
			continue
		}
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

// quotaHTTP 返回额度接口用的 HTTPDoer。生产走 upstream；测试注入 quotaClient
// 后可以脱网跑完整的 refreshQuota。
func (s *Server) quotaHTTP() HTTPDoer {
	if s.quotaClient != nil {
		return s.quotaClient
	}
	return s.upstream
}

// refreshQuota 拉一次额度并落库：billing/current 给套餐定义，billing/balance
// 给余额桶。两个接口都会时不时抽风，所以分工是「谁有数据用谁」；具体规则
// 全部拆在下面的纯函数里，便于单测（这个函数原来 324 行、零覆盖）。
func (s *Server) refreshQuota(ctx context.Context, acc *Account) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	now := time.Now()
	quota := Quota{Source: "local", UpdatedAt: now.Unix()}

	// billing/current 从 2026-10 起对所有账号返回 plans: []，偶尔还 429 回空
	// body。它失败不该拖死整次刷新——真实套餐在下面的 billing/balance 里。
	// 原来这里直接 return err，一次 429 就让这个号的额度停在旧值上一整轮。
	plan, planErr := fetchCurrentPlan(ctx, s.quotaHTTP(), acc.ZcodeJWT, s.profileHeaders(acc), *s.store.EnsureProfile(acc))
	var activePlans []*PlanInfo
	var currentPlan *PlanInfo
	if planErr != nil {
		quota.Error = planErr.Error()
	} else {
		activePlans, currentPlan = activePlansOf(plan.Data.Plans)
		if currentPlan != nil {
			quota.PlanName = currentPlan.Name
			quota.PlanID = currentPlan.PlanID
			quota.Status = currentPlan.Status
			quota.EndsAt = currentPlan.EndsAt
		}
	}

	usageByModel := usageSnapshot(acc.Models)
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
	if len(activePlans) == 0 && balanceErr == nil {
		if len(balancePlans) > 0 {
			activePlans, currentPlan = activePlansOf(balancePlans)
		} else if len(balances) > 0 {
			activePlans, currentPlan = plansFromBalances(balances)
		}
		if currentPlan != nil {
			quota.PlanName = currentPlan.Name
			quota.PlanID = currentPlan.PlanID
			quota.Status = "active"
		}
	}

	byEntitlement, byPlanName := indexBalances(balances)
	// 已领到、但 effective_at 还在未来的额度：上游此时不建 balance 桶，
	// 所以它不在 granted/remaining 里。单独列出来，别让它把「没额度」和
	// 「还没到账」混成一种状态——前者该禁用，后者只能等。
	quota.Pending = pendingEntitlements(activePlans, byEntitlement, now.Unix())
	quota.Source = quotaSourceFor(balanceErr, balances, quota.Pending)
	quota.Plans = buildQuotaPlans(activePlans, byEntitlement, byPlanName, usageByModel, now.Unix(), quota.Source)
	quota.Models = aggregateQuotaModels(quota.Plans)
	for _, p := range quota.Plans {
		quota.Granted += p.Granted
		quota.Used += p.Used
		quota.Remaining += p.Remaining
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
		applyQuotaState(a, quota, currentPlan, now)
	})
	if updated := s.store.GetAccount(acc.ID); updated != nil {
		for _, event := range quotaTransitions(prevError, prevPlan, prevGranted, updated, quota) {
			s.recordAccountEvent(updated, event.action, event.detail)
		}
	}
	if quota.Granted > 0 && quota.Remaining <= 0 {
		// Hard failure: no session can stay on an account without quota.
		s.pool.InvalidateAccount(acc.ID)
	}
	// 两条路都断了才算刷新失败；只挂一条（current 空/429）时 balance 往往还
	// 能给出真实额度，不该让调用方以为整次刷新白跑。
	if planErr != nil && balanceErr != nil {
		return planErr
	}
	return nil
}

// ---------- 下面全是纯函数：不碰网络、不碰 store，可以表驱动测 ----------

// activePlansOf 过滤出 status=active 的套餐，并挑出优先级最高的那份。
func activePlansOf(plans []PlanInfo) ([]*PlanInfo, *PlanInfo) {
	var active []*PlanInfo
	var current *PlanInfo
	for i := range plans {
		if !strings.EqualFold(plans[i].Status, "active") {
			continue
		}
		active = append(active, &plans[i])
		if current == nil || plans[i].Priority > current.Priority {
			current = &plans[i]
		}
	}
	return active, current
}

// plansFromBalances 在上游没给套餐定义时，从余额桶反推套餐：按
// plan_id+user_plan_id 分组，每组就是一份套餐，授予额度取组内合计。
func plansFromBalances(balances []Balance) ([]*PlanInfo, *PlanInfo) {
	groups := map[string]*PlanInfo{}
	var order []string
	for _, b := range balances {
		key := b.PlanID + "|" + b.UserPlanID
		plan, ok := groups[key]
		if !ok {
			plan = &PlanInfo{
				UserPlanID: b.UserPlanID,
				PlanID:     b.PlanID,
				Name:       b.PlanID,
				Status:     "active",
			}
			groups[key] = plan
			order = append(order, key)
		}
		plan.Entitlements = append(plan.Entitlements, PlanEntitlement{
			EntitlementID: b.EntitlementID,
			ShowName:      b.ShowName,
			GrantUnits:    b.TotalUnits,
			Period:        "one_time",
		})
	}
	active := make([]*PlanInfo, 0, len(order))
	var current *PlanInfo
	bestTotal := int64(-1)
	for _, key := range order {
		plan := groups[key]
		var total int64
		for _, ent := range plan.Entitlements {
			total += ent.GrantUnits
		}
		active = append(active, plan)
		if total > bestTotal {
			current, bestTotal = plan, total
		}
	}
	return active, current
}

// indexBalances 建两个索引：按权益 ID，以及按 user_plan_id+模型名（小写）。
// 前者是主路径；后者兜底上游没回 entitlement_id 的老响应。
func indexBalances(balances []Balance) (byEntitlement, byPlanName map[string]Balance) {
	byEntitlement = map[string]Balance{}
	byPlanName = map[string]Balance{}
	for _, b := range balances {
		if b.EntitlementID != "" {
			byEntitlement[b.EntitlementID] = b
		}
		if b.UserPlanID != "" {
			byPlanName[b.UserPlanID+"|"+strings.ToLower(b.ShowName)] = b
		}
	}
	return byEntitlement, byPlanName
}

// pendingEntitlements 挑出「有套餐、有授予额度，但 balance 桶还没建、
// effective_at 在未来」的权益。这些额度现在用不了，不能算进 granted。
func pendingEntitlements(activePlans []*PlanInfo, byEntitlement map[string]Balance, now int64) []QuotaPending {
	var out []QuotaPending
	for _, plan := range activePlans {
		for _, ent := range plan.Entitlements {
			if ent.EffectiveAt <= now || ent.EffectiveAt == 0 {
				continue
			}
			if _, ok := byEntitlement[ent.EntitlementID]; ok {
				continue
			}
			out = append(out, QuotaPending{
				PlanID:      plan.PlanID,
				PlanName:    plan.Name,
				Model:       ent.ShowName,
				Amount:      ent.GrantUnits,
				EffectiveAt: ent.EffectiveAt,
			})
		}
	}
	return out
}

// quotaSourceFor 决定这次刷新的数据源，自动启停只看它：
//
//	api         上游给了余额桶
//	api-empty   上游明确回了空列表（确实没套餐）
//	api-pending 有套餐但还没到生效时间
//	local       余额没查到，本地账本仍然有效，谁都不能据此禁用账号
func quotaSourceFor(balanceErr error, balances []Balance, pending []QuotaPending) string {
	switch {
	case balanceErr != nil:
		return "local"
	case len(balances) > 0:
		return "api"
	case len(pending) > 0:
		return "api-pending"
	default:
		return "api-empty"
	}
}

// usageSnapshot 拷一份本地用量，用来在没有余额桶时估算已用额度。
func usageSnapshot(models map[string]*UsageTotals) map[string]UsageTotals {
	out := make(map[string]UsageTotals, len(models))
	for name, totals := range models {
		if totals != nil {
			out[name] = *totals
		}
	}
	return out
}

// buildQuotaPlans 把「套餐 × 权益」摊平成面板用的 QuotaPlan 列表。
//
// 每个权益先找 entitlement_id 的余额桶，找不到再按 user_plan_id+模型名兜底；
// 两种都没有且 effective_at 还在未来时跳过——那份额度现在用不了，算进
// granted 会让选号以为它有产能，发出去就是 1005。
func buildQuotaPlans(activePlans []*PlanInfo, byEntitlement, byPlanName map[string]Balance, usageByModel map[string]UsageTotals, now int64, source string) []QuotaPlan {
	var out []QuotaPlan
	for _, plan := range activePlans {
		qp := QuotaPlan{
			UserPlanID: plan.UserPlanID,
			PlanID:     plan.PlanID,
			Name:       plan.Name,
			Status:     plan.Status,
			Priority:   plan.Priority,
			StartsAt:   plan.StartsAt,
			EndsAt:     plan.EndsAt,
			Models:     []QuotaModel{},
		}
		for _, ent := range plan.Entitlements {
			balance, ok := byEntitlement[ent.EntitlementID]
			if !ok {
				balance, ok = byPlanName[plan.UserPlanID+"|"+strings.ToLower(ent.ShowName)]
			}
			if !ok && ent.EffectiveAt > now {
				continue
			}
			used := int64(0)
			if ok {
				used = balance.UsedUnits
				if used == 0 && balance.TotalUnits > 0 {
					used = balance.TotalUnits - balance.RemainingUnits
				}
			} else if totals, hit := usageByModel[ent.ShowName]; hit {
				used = int64(totals.Total())
			}
			granted := ent.GrantUnits
			if ok && balance.TotalUnits > 0 {
				granted = balance.TotalUnits
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
				Name: ent.ShowName, Granted: granted, Used: used, Remaining: remaining, Percent: percent, Source: source,
			}
			qp.Models = append(qp.Models, model)
			qp.Granted += granted
			qp.Used += used
			qp.Remaining += remaining
		}
		if qp.Granted > 0 {
			qp.Percent = float64(qp.Remaining) / float64(qp.Granted) * 100
		}
		out = append(out, qp)
	}
	return out
}

// aggregateQuotaModels 把各套餐下的同名模型（大小写不敏感）合成一行。
// 顺序按首次出现，和面板原来的展示顺序一致。
func aggregateQuotaModels(plans []QuotaPlan) []QuotaModel {
	var order []string
	aggregate := map[string]*QuotaModel{}
	for _, plan := range plans {
		for _, model := range plan.Models {
			key := strings.ToLower(model.Name)
			agg, ok := aggregate[key]
			if !ok {
				clone := model
				aggregate[key] = &clone
				order = append(order, key)
				continue
			}
			agg.Granted += model.Granted
			agg.Used += model.Used
			agg.Remaining += model.Remaining
			if agg.Granted > 0 {
				agg.Percent = float64(agg.Remaining) / float64(agg.Granted) * 100
			}
		}
	}
	var out []QuotaModel
	for _, key := range order {
		out = append(out, *aggregate[key])
	}
	return out
}

// applyQuotaState 把这次刷新的结论落到账号上：套餐模式、耗尽/待生效的
// 冷却窗口、以及「额度恢复后把错误清掉」。只改内存，落盘由调用方负责。
func applyQuotaState(acc *Account, quota Quota, currentPlan *PlanInfo, now time.Time) {
	acc.Quota = quota
	if currentPlan != nil {
		if strings.EqualFold(currentPlan.Status, "active") {
			if strings.Contains(currentPlan.PlanID, "start-plan") {
				acc.Mode = "start-plan"
			} else {
				acc.Mode = "coding-plan"
			}
		} else {
			acc.ExhaustedUntil = now.Add(10 * time.Minute).Unix()
			acc.LastError = "套餐已过期"
		}
	}
	// 已领到但还没生效：现在发请求必吃 1005，先别让选号挑中它。
	if quota.Granted == 0 && len(quota.Pending) > 0 {
		acc.ExhaustedUntil = now.Add(10 * time.Minute).Unix()
		acc.LastError = "套餐待生效"
	} else if quota.Remaining <= 0 && quota.Granted > 0 {
		acc.ExhaustedUntil = now.Add(10 * time.Minute).Unix()
		acc.LastError = "额度耗尽"
	} else {
		if acc.ExhaustedUntil < now.Unix() {
			acc.ExhaustedUntil = 0
		}
		if acc.LastError == "额度耗尽" || acc.LastError == "套餐待生效" {
			acc.LastError = ""
			acc.LastErrorAt = 0
		}
	}
}

// quotaEvent 是一条待写入日志的账号状态变化。
type quotaEvent struct {
	action string
	detail string
}

// quotaTransitions 把「刷新前 vs 刷新后」翻成要记的事件。只报真实变化：
// ExhaustedUntil 在耗尽期间每轮都会往后滚，拿它比较会把日志刷爆，所以
// 这里比的是 LastError 文案和套餐名/授予额度。
func quotaTransitions(prevError, prevPlan string, prevGranted int64, updated *Account, quota Quota) []quotaEvent {
	var events []quotaEvent
	if updated.LastError != prevError {
		switch {
		case updated.LastError == "额度耗尽":
			events = append(events, quotaEvent{"额度耗尽", fmt.Sprintf("剩余 %d / 共 %d", quota.Remaining, quota.Granted)})
		case prevError == "额度耗尽" && updated.LastError == "":
			events = append(events, quotaEvent{"额度恢复", fmt.Sprintf("剩余 %d / 共 %d", quota.Remaining, quota.Granted)})
		case updated.LastError == "套餐已过期":
			events = append(events, quotaEvent{"套餐过期", "上游返回非 active 状态"})
		case updated.LastError == "套餐待生效":
			events = append(events, quotaEvent{"额度待生效", fmt.Sprintf("%s，到账时间 %s",
				pendingSummary(quota.Pending), pendingWhen(quota.Pending))})
		}
	}
	newPlan, newGranted := updated.Quota.PlanName, updated.Quota.Granted
	switch {
	case newPlan != prevPlan && newPlan == "":
		events = append(events, quotaEvent{"套餐到期", fmt.Sprintf("原套餐 %s，授予 %d", prevPlan, prevGranted)})
	case newPlan != prevPlan && prevPlan == "":
		events = append(events, quotaEvent{"新套餐", fmt.Sprintf("%s，授予 %d", newPlan, newGranted)})
	case newPlan != prevPlan:
		events = append(events, quotaEvent{"套餐变更", fmt.Sprintf("%s → %s", prevPlan, newPlan)})
	case prevGranted == 0 && newGranted > 0:
		events = append(events, quotaEvent{"额度到账", fmt.Sprintf("授予 %d", newGranted)})
	case prevGranted > 0 && newGranted == 0:
		events = append(events, quotaEvent{"额度清零", fmt.Sprintf("原授予 %d", prevGranted)})
	}
	return events
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
	base := upstreamBase() + "/api/v1/zcode-plan/billing/balance"
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
	resp, err := doRequest(ctx, s.quotaHTTP(), http.MethodGet, base+query, headers, nil)
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
