package main

// 手动领取：面板上一个「全部领取」和一个账号行上的「领取」。
//
// 为什么不复用 contrib/claimer：那是每小时跑的 sidecar，管的是「额度启停 +
// 定时领取」，跟网关是两个进程。面板按钮要求拿回结果，走 subprocess 反而更绕
// （还得把面板密码、solver 路径、中继地址都传过去）。
//
// 这里只做一件窄事：对指定账号，解一次验证码，提交一次领取。
// 关键约束和 sidecar 一致——解验证码和提交领取必须是同一个出口 IP，
// 阿里云的 captchaVerifyParam 绑 IP，跨网就是 3012。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type claimOutcome struct {
	AccountID string `json:"accountId"`
	Label     string `json:"label"`
	PlanID    string `json:"planId,omitempty"`
	PlanName  string `json:"planName,omitempty"`
	OK        bool   `json:"ok"`
	Skipped   bool   `json:"skipped,omitempty"`
	Code      int    `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
}

type claimJob struct {
	mu       sync.Mutex
	running  bool
	started  int64
	finished int64
	total    int
	done     int
	claimed  int
	results  []claimOutcome
	logs     []string
}

func (j *claimJob) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	logs := append([]string(nil), j.logs...)
	if len(logs) > 300 {
		logs = logs[len(logs)-300:]
	}
	return map[string]any{
		"running":    j.running,
		"startedAt":  j.started,
		"finishedAt": j.finished,
		"total":      j.total,
		"done":       j.done,
		"claimed":    j.claimed,
		"results":    append([]claimOutcome(nil), j.results...),
		"logs":       logs,
	}
}

func (j *claimJob) logf(format string, args ...any) {
	line := time.Now().Format("15:04:05") + " " + fmt.Sprintf(format, args...)
	j.mu.Lock()
	j.logs = append(j.logs, line)
	j.mu.Unlock()
}

// captchaSolverDir 是 solver_pw.js 所在目录。空 = 没配，面板的领取按钮会
// 明确报「未配置验证码求解器」，而不是发一个必然失败的请求。
func captchaSolverDir() string {
	return strings.TrimSpace(envOr("ZCODE_CAPTCHA_SOLVER_DIR", ""))
}

func claimGap() time.Duration {
	sec := 15.0
	if raw := strings.TrimSpace(envOr("ZCODE_CLAIM_GAP_SEC", "")); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil {
			sec = v
		}
	}
	if sec < 0 {
		sec = 0
	}
	return time.Duration(sec * float64(time.Second))
}

func claim3012Retries() int {
	if raw := strings.TrimSpace(envOr("ZCODE_CLAIM_3012_RETRIES", "")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v >= 0 {
			return v
		}
	}
	return 2
}

// claimSolverProxy 返回解验证码时 Chromium 该走的出口。
//
// 必须和 claimDoers() 的第一跳一致：阿里云的 captchaVerifyParam 绑 IP，
// 在 A 网解出来的 token 拿到 B 网去用，上游直接判 3012。
//   direct / direct_fallback → 领取先走直连，solver 也得直连（返回 ""）
//   proxy / proxy_fallback   → 领取先走中继，solver 也走中继
func (s *Server) claimSolverProxy() string {
	if !s.upstream.HasClaimProxy() {
		return ""
	}
	switch s.store.Settings().ClaimEgress {
	case "direct", "direct_fallback":
		return ""
	default:
		return s.upstream.ClaimProxyURL()
	}
}

// rotateClaimEgress 让中继换一个出口。走直连（或没配中继）时什么也不做。
func (s *Server) rotateClaimEgress(ctx context.Context) {
	base := strings.TrimRight(s.claimSolverProxy(), "/")
	if base == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/rotate", nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// solveCaptcha 在 Chromium 里跑官方 SDK 拿 captchaVerifyParam。
// 出口由 claimSolverProxy 决定，和领取保持同一个 IP。
func (s *Server) solveCaptcha(ctx context.Context, scene, region, prefix string) (string, error) {
	dir := captchaSolverDir()
	if dir == "" {
		return "", fmt.Errorf("未配置验证码求解器（设置 ZCODE_CAPTCHA_SOLVER_DIR 指向 contrib/claimer/solver）")
	}
	cmd := exec.CommandContext(ctx, "node", "solver_pw.js", scene, region, prefix)
	cmd.Dir = dir
	env := os.Environ()
	if proxy := s.claimSolverProxy(); proxy != "" {
		env = append(env, "HTTP_PROXY="+proxy, "HTTPS_PROXY="+proxy, "NO_PROXY=")
	}
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("求解超时")
		}
		return "", fmt.Errorf("求解器执行失败: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "VERIFY_PARAM=") {
			continue
		}
		param := strings.TrimSpace(strings.TrimPrefix(line, "VERIFY_PARAM="))
		if len(param) >= 128 {
			return param, nil
		}
	}
	return "", fmt.Errorf("求解器没有输出 captchaVerifyParam")
}

// claimOneAccount 对单个账号走完整流程：查活动 → 选一个可领的 → 换出口 →
// 解验证码 → 领取；3012 换出口重试。wantPlan 为空时自动挑第一个「可领且未领」。
func (s *Server) claimOneAccount(ctx context.Context, acc *Account, wantPlan string) claimOutcome {
	out := claimOutcome{AccountID: acc.ID, Label: acc.Label}
	snap, err := s.detectActivities(ctx, acc)
	if err != nil {
		out.Message = err.Error()
		return out
	}
	var plan *ActivityPlan
	for i := range snap.Plans {
		p := &snap.Plans[i]
		if wantPlan != "" && p.PlanID != wantPlan {
			continue
		}
		if p.Claimed || !p.Claimable {
			continue
		}
		plan = p
		break
	}
	if plan == nil {
		out.Skipped = true
		out.Message = "没有可领的套餐（已领取或当前未开放）"
		return out
	}
	out.PlanID, out.PlanName = plan.PlanID, plan.Name

	captcha := snap.Captcha
	needCaptcha := captcha != nil && captcha.Enabled &&
		captcha.SceneID != "" && captcha.Region != "" && captcha.Prefix != ""
	if needCaptcha && captchaSolverDir() == "" {
		out.Message = "这个活动需要验证码，但网关没配求解器（ZCODE_CAPTCHA_SOLVER_DIR）"
		return out
	}

	retries := claim3012Retries()
	last := out
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			s.claim.logf("%s: 3012 风控，换出口重试 %d/%d", acc.Label, attempt, retries)
			time.Sleep(claimGap())
		}
		// 顺序不能反：先换出口，再解验证码，最后用同一出口领取
		s.rotateClaimEgress(ctx)
		param, region := "", ""
		if needCaptcha {
			p, err := s.solveCaptcha(ctx, captcha.SceneID, captcha.Region, captcha.Prefix)
			if err != nil {
				last = claimOutcome{AccountID: acc.ID, Label: acc.Label,
					PlanID: plan.PlanID, PlanName: plan.Name, Message: err.Error()}
				return last
			}
			param, region = p, captcha.Region
		}
		res, err := s.claimActivity(ctx, acc, plan.PlanID, param, region)
		if err != nil {
			last.Message = err.Error()
			return last
		}
		if res == nil {
			last.Message = "上游没有返回结果"
			return last
		}
		last = claimOutcome{
			AccountID: acc.ID, Label: acc.Label,
			PlanID: plan.PlanID, PlanName: plan.Name,
			OK: res.OK, Code: res.Code, Message: res.Message,
		}
		if res.OK || res.Code != 3012 {
			break
		}
	}
	if last.OK {
		// 领到后立刻刷一次，能到账就顺手启用
		if err := s.refreshQuota(ctx, acc); err == nil {
			if updated := s.store.GetAccount(acc.ID); updated != nil &&
				updated.Quota.Remaining > 0 && !updated.Enabled {
				_ = s.store.UpdateAccount(acc.ID, func(a *Account) { a.Enabled = true })
				s.claim.logf("%s: 额度已到账（%d）→ 已启用", acc.Label, updated.Quota.Remaining)
			}
		}
	}
	return last
}

// startClaimJob 起一个后台领取任务。accountIDs 为空 = 全部账号。
func (s *Server) startClaimJob(accountIDs []string) error {
	job := s.claim
	job.mu.Lock()
	if job.running {
		job.mu.Unlock()
		return fmt.Errorf("已有领取任务在跑，等它结束")
	}
	all := s.store.AccountsForRefresh()
	targets := make([]*Account, 0, len(all))
	if len(accountIDs) == 0 {
		targets = all
	} else {
		want := map[string]bool{}
		for _, id := range accountIDs {
			want[id] = true
		}
		for _, acc := range all {
			if want[acc.ID] {
				targets = append(targets, acc)
			}
		}
		if len(targets) == 0 {
			job.mu.Unlock()
			return fmt.Errorf("没找到要领取的账号")
		}
	}
	job.running = true
	job.started = time.Now().Unix()
	job.finished = 0
	job.total = len(targets)
	job.done = 0
	job.claimed = 0
	job.results = nil
	job.logs = nil
	job.mu.Unlock()

	go func() {
		ctx := context.Background()
		job.logf("开始领取，共 %d 个账号", len(targets))
		gap := claimGap()
		// 先探一个号，问出「当前哪些套餐可领」。之后每个号先在本地额度里对一遍：
		// 已经有这份套餐的直接跳过，不用再打 4~5 次上游——全号 60 个，光跳过
		// 就要跑 18 分钟，纯属浪费。
		activePlans := map[string]bool{}
		for _, acc := range targets {
			snap, err := s.detectActivities(ctx, acc)
			if err != nil {
				continue
			}
			for _, p := range snap.Plans {
				if p.Claimable && !p.Claimed {
					activePlans[p.PlanID] = true
				}
			}
			if len(activePlans) > 0 {
				job.logf("本轮可领：%s", strings.Join(mapKeys(activePlans), ", "))
				break
			}
		}
		attempted := 0
		for _, acc := range targets {
			if len(activePlans) > 0 && accountOwnsAny(acc, activePlans) {
				job.mu.Lock()
				job.done++
				job.results = append(job.results, claimOutcome{
					AccountID: acc.ID, Label: acc.Label,
					Skipped: true, Message: "已经领过这个套餐",
				})
				job.mu.Unlock()
				continue
			}
			if attempted > 0 && gap > 0 {
				time.Sleep(gap)
			}
			attempted++
			out := s.claimOneAccount(ctx, acc, "")
			job.mu.Lock()
			job.done++
			job.results = append(job.results, out)
			if out.OK {
				job.claimed++
			}
			job.mu.Unlock()
			switch {
			case out.Skipped:
				job.logf("%s: 跳过（%s）", acc.Label, out.Message)
			case out.OK:
				job.logf("%s: ✅ 领取成功 %s", acc.Label, out.PlanName)
			default:
				job.logf("%s: ❌ code=%d %s", acc.Label, out.Code, out.Message)
			}
		}
		job.mu.Lock()
		job.running = false
		job.finished = time.Now().Unix()
		claimed, total := job.claimed, job.total
		job.mu.Unlock()
		job.logf("结束：成功 %d / 共 %d", claimed, total)
	}()
	return nil
}

// accountOwnsAny 本地判断这个号是不是已经有其中某份套餐了（含待生效）。
func accountOwnsAny(acc *Account, plans map[string]bool) bool {
	for _, p := range acc.Quota.Plans {
		if plans[p.PlanID] {
			return true
		}
	}
	for _, p := range acc.Quota.Pending {
		if plans[p.PlanID] {
			return true
		}
	}
	return false
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// normalizeClaimWindow 校验并规范化领取时段写法。
//
//	"19,20"  → "19,20"
//	"19-21"  → "19,20,21"
//	"23-2"   → "0,1,2,23"（跨零点）
//	""/"*"   → ""（不限时段）
//
// 非法输入返回 ok=false，调用方保持原值，别让手滑把 claimer 变成「每小时都跑」。
func normalizeClaimWindow(raw string) (string, bool) {
	spec := strings.TrimSpace(raw)
	if spec == "" || spec == "*" {
		return "", true
	}
	hours := map[int]bool{}
	for _, part := range strings.Split(strings.ReplaceAll(spec, " ", ""), ",") {
		if part == "" {
			continue
		}
		if strings.Contains(part, "-") {
			lo, hi, _ := strings.Cut(part, "-")
			a, err1 := strconv.Atoi(lo)
			b, err2 := strconv.Atoi(hi)
			if err1 != nil || err2 != nil || a < 0 || a > 23 || b < 0 || b > 23 {
				return "", false
			}
			if a <= b {
				for h := a; h <= b; h++ {
					hours[h] = true
				}
			} else {
				for h := a; h <= 23; h++ {
					hours[h] = true
				}
				for h := 0; h <= b; h++ {
					hours[h] = true
				}
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || n > 23 {
			return "", false
		}
		hours[n] = true
	}
	if len(hours) == 0 {
		return "", true
	}
	out := make([]int, 0, len(hours))
	for h := range hours {
		out = append(out, h)
	}
	sort.Ints(out)
	parts := make([]string, 0, len(out))
	for _, h := range out {
		parts = append(parts, strconv.Itoa(h))
	}
	return strings.Join(parts, ","), true
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.claim.snapshot())
	case http.MethodPost:
		var body struct {
			AccountID  string   `json:"accountId"`
			AccountIDs []string `json:"accountIds"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
		ids := append([]string(nil), body.AccountIDs...)
		if body.AccountID != "" {
			ids = append(ids, body.AccountID)
		}
		if err := s.startClaimJob(ids); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "job": s.claim.snapshot()})
	default:
		writeError(w, 405, "method not allowed")
	}
}
