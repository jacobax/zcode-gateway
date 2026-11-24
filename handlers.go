package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

type Server struct {
	store    *Store
	upstream *Upstream
	oauth    *OAuthManager
	pool     *Pool
	start    time.Time
	baseURL  string
	version  string

	adminPass      string
	adminHash      string
	adminGenerated bool
	adminMu        sync.RWMutex
	login          *loginThrottle
	updates        *updateChecker
	claim          *claimJob
	// quotaClient 是测试注入点：nil 表示走 upstream，生产恒为 nil。
	quotaClient HTTPDoer
	// lastEgressRotate 是上一次成功轮换出口的 unix 秒，用于节流见 rotateEgress。
	lastEgressRotate atomic.Int64
}

func NewServer(store *Store, upstream *Upstream, baseURL string) *Server {
	server := &Server{
		store:    store,
		upstream: upstream,
		oauth:    NewOAuthManager(upstream, store),
		pool:     NewPool(store),
		start:    time.Now(),
		baseURL:  baseURL,
		version:  appVersion,
		login:    newLoginThrottle(),
		updates:  newUpdateChecker(),
		claim:    &claimJob{},
	}
	server.initAdminPassword()
	return server
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	accounts := s.store.Summaries()
	overview := Overview{Recent: s.store.Logs(12, "", "")}
	overview.Accounts.Total = len(accounts)
	for _, acc := range accounts {
		if acc.Enabled {
			overview.Accounts.Enabled++
		}
		if acc.Enabled && acc.Quota.Remaining > 0 {
			overview.Accounts.Available++
		}
		if acc.ID == s.store.Settings().ActiveAccountID && acc.Enabled && (acc.Quota.Remaining > 0 || acc.Status == "可用") {
			overview.ActiveAccount = acc
		}
	}
	if overview.ActiveAccount == nil {
		for _, acc := range accounts {
			if acc.Enabled {
				overview.ActiveAccount = acc
				break
			}
		}
	}
	// 套餐分布：一个号可能同时挂着好几个套餐（老套餐还没过期、新活动已经领到），
	// 所以按 Quota.Plans 逐份统计，不能只认 Quota.PlanName——那是「面板上显示
	// 哪个」，选出来只有一份，其余套餐的额度会凭空消失。
	plans := map[string]*PlanStat{}
	planStat := func(name string) *PlanStat {
		name = strings.TrimSpace(name)
		if name == "" {
			name = "无套餐"
		}
		stat := plans[name]
		if stat == nil {
			stat = &PlanStat{Name: name}
			plans[name] = stat
		}
		return stat
	}
	for _, acc := range accounts {
		if len(acc.Quota.Plans) == 0 {
			// 没有套餐明细（local 额度源、或上游没返回 plans）：退回单份口径
			stat := planStat(acc.Quota.PlanName)
			stat.Count++
			// 启用 = 该套餐已生效（有真实余额桶）。只有待生效权益的套餐
			// granted 为 0，不能算进来——那只是「领到了，还没到账」。
			if acc.Enabled && acc.Quota.Granted > 0 {
				stat.Enabled++
				stat.Granted += acc.Quota.Granted
				stat.Remaining += acc.Quota.Remaining
			}
		} else {
			for _, plan := range acc.Quota.Plans {
				stat := planStat(plan.Name)
				stat.Count++
				// 同上：待生效的套餐 granted 为 0，不算「启用」。
				if acc.Enabled && plan.Granted > 0 {
					stat.Enabled++
					stat.Granted += plan.Granted
					stat.Remaining += plan.Remaining
				}
			}
		}
		if !acc.Enabled {
			continue
		}
		overview.Accounts.Quota.Granted += acc.Quota.Granted
		overview.Accounts.Quota.Remaining += acc.Quota.Remaining
		if acc.Quota.Granted > 0 {
			overview.Accounts.Quota.Accounts++
		}
	}
	for _, stat := range plans {
		overview.Accounts.Plans = append(overview.Accounts.Plans, *stat)
	}
	sort.SliceStable(overview.Accounts.Plans, func(i, j int) bool {
		a, b := overview.Accounts.Plans[i], overview.Accounts.Plans[j]
		if a.Remaining != b.Remaining {
			return a.Remaining > b.Remaining
		}
		return a.Name < b.Name
	})
	// 额度按模型拆开：同一个套餐下每个模型的授予/剩余分别汇总。
	models := map[string]*ModelQuotaStat{}
	for _, acc := range accounts {
		if !acc.Enabled {
			continue
		}
		for _, m := range acc.Quota.Models {
			name := strings.TrimSpace(m.Name)
			if name == "" || m.Granted <= 0 {
				continue
			}
			stat := models[name]
			if stat == nil {
				stat = &ModelQuotaStat{Name: name}
				models[name] = stat
			}
			stat.Accounts++
			stat.Granted += m.Granted
			stat.Remaining += m.Remaining
		}
	}
	for _, stat := range models {
		if stat.Granted > 0 {
			stat.Percent = float64(stat.Remaining) / float64(stat.Granted) * 100
		}
		overview.Accounts.Quota.Models = append(overview.Accounts.Quota.Models, *stat)
	}
	sort.SliceStable(overview.Accounts.Quota.Models, func(i, j int) bool {
		a, b := overview.Accounts.Quota.Models[i], overview.Accounts.Quota.Models[j]
		if a.Remaining != b.Remaining {
			return a.Remaining > b.Remaining
		}
		return a.Name < b.Name
	})
	quotaTotal := &overview.Accounts.Quota
	quotaTotal.Used = quotaTotal.Granted - quotaTotal.Remaining
	if quotaTotal.Used < 0 {
		quotaTotal.Used = 0
	}
	if quotaTotal.Granted > 0 {
		quotaTotal.Percent = float64(quotaTotal.Used) / float64(quotaTotal.Granted) * 100
	}
	req, total, u := s.store.TodayTotals()
	overview.Today.Requests = req
	overview.Today.TotalTokens = total
	overview.Today.InputTokens = u.InputTokens
	overview.Today.OutputTokens = u.OutputTokens
	overview.Today.CacheReadTokens = u.CacheReadTokens
	overview.Today.CacheWriteTokens = u.CacheWriteTokens
	overview.Today.ReasoningTokens = u.ReasoningTokens
	settings := s.store.Settings()
	overview.Server = ServerInfo{
		Version: s.version, BaseURL: s.baseURL, Fingerprint: settings.Fingerprint,
		QuotaSeconds: settings.QuotaIntervalSec, DataDir: s.store.dir,
	}
	writeJSON(w, 200, overview)
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{"items": s.store.Summaries()})
	case http.MethodPost:
		var body struct {
			Provider string `json:"provider"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Provider == "" {
			body.Provider = "zai"
		}
		flow, err := s.oauth.Start(r.Context(), body.Provider)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"flowId": flow.ID, "authorizeUrl": flow.AuthorizeURL})
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	if err := s.store.ImportDesktop(); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	items := s.store.Summaries()
	log.Printf("面板触发桌面端导入，当前共 %d 个账号（来源 %s）", len(items), desktopCredentialsPath())
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	flowID := r.URL.Query().Get("flowId")
	if flowID == "" {
		writeError(w, 400, "flowId required")
		return
	}
	status, acc, err := s.oauth.Status(r.Context(), flowID)
	if err != nil {
		writeJSON(w, 200, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	out := map[string]any{"status": status}
	if acc != nil {
		out["account"] = s.store.SummaryFor(acc.ID)
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.URL) == "" {
		writeError(w, 400, "url required")
		return
	}
	acc, err := s.oauth.HandleCallback(r.Context(), body.URL)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ready", "account": s.store.SummaryFor(acc.ID)})
}

func (s *Server) handleAccountByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, 404, "account not found")
		return
	}
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch {
	case r.Method == http.MethodPatch && action == "":
		var body struct {
			Label       *string `json:"label"`
			Priority    *int    `json:"priority"`
			Enabled     *bool   `json:"enabled"`
			Concurrency *int    `json:"concurrency"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, 400, "invalid json")
			return
		}
		if !s.store.UpdateAccount(id, func(a *Account) {
			if body.Label != nil {
				a.Label = strings.TrimSpace(*body.Label)
			}
			if body.Priority != nil {
				a.Priority = *body.Priority
			}
			if body.Enabled != nil {
				a.Enabled = *body.Enabled
			}
			if body.Concurrency != nil {
				a.Concurrency = normalizeConcurrency(*body.Concurrency)
			}
		}) {
			writeError(w, 404, "account not found")
			return
		}
		if body.Concurrency != nil {
			s.store.SetConcurrency(id, *body.Concurrency)
		}
		if body.Enabled != nil {
			action := "已启用"
			if !*body.Enabled {
				action = "已禁用"
			}
			s.recordAccountEvent(s.store.GetAccount(id), action, "面板手动操作")
		}
		if body.Enabled != nil && !*body.Enabled {
			// Disabled is a hard failure: no session may stay pinned to it.
			s.pool.InvalidateAccount(id)
			s.pool.signal()
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case r.Method == http.MethodDelete && action == "":
		s.store.DeleteAccount(id)
		s.pool.signal()
		writeJSON(w, 200, map[string]any{"ok": true})
	case r.Method == http.MethodPost && action == "refresh":
		acc := s.store.GetAccount(id)
		if acc == nil {
			writeError(w, 404, "account not found")
			return
		}
		if err := s.refreshQuota(r.Context(), acc); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "account": s.store.SummaryFor(id)})
	case r.Method == http.MethodPost && action == "fingerprint":
		profile := s.store.RotateProfile(id)
		if profile == nil {
			writeError(w, 404, "account not found")
			return
		}
		s.pool.signal()
		writeJSON(w, 200, map[string]any{"ok": true, "profile": profile})
	case r.Method == http.MethodPost && action == "clear-cooldown":
		// 一并清 RiskStrikes：只清 ExhaustedUntil 会让账号停在「第 N 次」的
		// 退避档位上，下一次 3012 直接从 30m/1h 起跳（2026-11-22 实测：
		// 清过冷却的号下一次风控就写「第 2 次，冷却 30m0s」）。
		if !s.store.UpdateAccount(id, func(a *Account) {
			a.ExhaustedUntil = 0
			a.LastError = ""
			a.LastErrorAt = 0
			a.RiskStrikes = 0
		}) {
			writeError(w, 404, "account not found")
			return
		}
		s.store.ResetAccountModelStates(id)
		s.pool.signal()
		writeJSON(w, 200, map[string]any{"ok": true, "account": s.store.SummaryFor(id)})
	case r.Method == http.MethodGet && action == "activities":
		acc := s.store.GetAccount(id)
		if acc == nil {
			writeError(w, 404, "account not found")
			return
		}
		snapshot, err := s.detectActivities(r.Context(), acc)
		if err != nil {
			writeError(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, snapshot)
	case r.Method == http.MethodPost && action == "claim":
		acc := s.store.GetAccount(id)
		if acc == nil {
			writeError(w, 404, "account not found")
			return
		}
		var body struct {
			PlanID             string `json:"planId"`
			CaptchaVerifyParam string `json:"captchaVerifyParam"`
			CaptchaRegion      string `json:"captchaRegion"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			writeError(w, 400, "invalid json")
			return
		}
		result, err := s.claimActivity(r.Context(), acc, body.PlanID, body.CaptchaVerifyParam, body.CaptchaRegion)
		if err != nil {
			writeError(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, result)
	default:
		writeError(w, 404, "not found")
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/logs")
	if strings.HasPrefix(path, "/") && len(path) > 1 {
		id := strings.TrimPrefix(path, "/")
		if e := s.store.GetLog(id); e != nil {
			writeJSON(w, 200, e)
			return
		}
		writeError(w, 404, "log not found")
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	status := r.URL.Query().Get("status")
	accountID := r.URL.Query().Get("accountId")
	writeJSON(w, 200, map[string]any{"items": s.store.Logs(limit, status, accountID)})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		settings := s.store.Settings()
		real := settings.ProxyToken
		settings.AdminPasswordHash = ""
		// 明文 token 只在 /api/settings/token 单独回显，页面平时拿掩码。
		settings.ProxyToken = ""
		writeJSON(w, 200, map[string]any{
			"settings":         settings,
			"passwordSource":   s.passwordSource(),
			"proxyTokenMask":   maskToken(real),
			"proxyTokenSet":    real != "",
			"egressAvailable":  s.upstream.HasEgress(),
			"claimSolverReady": captchaSolverDir() != "",
			"egressProxy":      proxyDisplay(s.upstream.EgressProxyURL()),
			"server": ServerInfo{
				Version: s.version, BuildVersion: buildVersion, BaseURL: s.baseURL, Fingerprint: settings.Fingerprint,
				QuotaSeconds: settings.QuotaIntervalSec, DataDir: s.store.dir,
			},
		})
	case http.MethodPatch:
		var body struct {
			ProxyToken         *string `json:"proxyToken"`
			Fingerprint        *bool   `json:"fingerprint"`
			StoreBodies        *bool   `json:"storeBodies"`
			QuotaIntervalSec   *int    `json:"quotaIntervalSec"`
			DefaultConcurrency *int    `json:"defaultConcurrency"`
			PromptMode         *string `json:"promptMode"`
			AutoUpdate         *bool   `json:"autoUpdate"`
			EgressMode         *string `json:"egressMode"`
			EgressRotate       *bool   `json:"egressRotate"`
			ClaimEgress        *string `json:"claimEgress"`
			ClaimWindow        *string `json:"claimWindow"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, 400, "invalid json")
			return
		}
		s.store.UpdateSettings(func(st *Settings) {
			if body.ProxyToken != nil {
				st.ProxyToken = strings.TrimSpace(*body.ProxyToken)
			}
			if body.Fingerprint != nil {
				st.Fingerprint = *body.Fingerprint
				st.FingerprintSet = true
			}
			if body.StoreBodies != nil {
				st.StoreBodies = *body.StoreBodies
			}
			if body.QuotaIntervalSec != nil && *body.QuotaIntervalSec >= 10 {
				st.QuotaIntervalSec = *body.QuotaIntervalSec
			}
			if body.DefaultConcurrency != nil {
				st.DefaultConcurrency = clampConcurrency(*body.DefaultConcurrency)
			}
			if body.PromptMode != nil {
				switch *body.PromptMode {
				case "append", "replace":
					st.PromptMode = *body.PromptMode
				}
			}
			if body.AutoUpdate != nil {
				st.AutoUpdateDisabled = !*body.AutoUpdate
			}
			if body.EgressMode != nil {
				switch *body.EgressMode {
				case "direct", "fallback", "always":
					st.EgressMode = *body.EgressMode
				}
			}
			if body.EgressRotate != nil {
				st.EgressRotate = *body.EgressRotate
			}
			if body.ClaimEgress != nil {
				switch *body.ClaimEgress {
				case "direct", "direct_fallback", "proxy", "proxy_fallback":
					st.ClaimEgress = *body.ClaimEgress
				}
			}
			if body.ClaimWindow != nil {
				// 只接受 "19,20" / "19-21" / "" 这类写法，别让非法串把
				// claimer 静默变成「不限时段」。
				if normalized, ok := normalizeClaimWindow(*body.ClaimWindow); ok {
					st.ClaimWindow = normalized
				}
			}
		})
		settings := s.store.Settings()
		mask := maskToken(settings.ProxyToken)
		settings.AdminPasswordHash = ""
		settings.ProxyToken = ""
		writeJSON(w, 200, map[string]any{"ok": true, "settings": settings, "proxyTokenMask": mask, "proxyTokenSet": mask != ""})
	default:
		writeError(w, 405, "method not allowed")
	}
}

// handleRevealToken 单独回显网关 token 明文：面板平时只显示掩码，
// 要复制给客户端时才走这里，避免截图和日志里到处是明文。
func (s *Server) handleRevealToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, map[string]any{"token": s.store.Settings().ProxyToken})
}

func maskToken(token string) string {
	if token == "" {
		return ""
	}
	if len(token) <= 12 {
		return strings.Repeat("•", len(token))
	}
	return token[:5] + "…" + token[len(token)-4:]
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	// 上游常常先给账号权益上线新模型（Start Plan 就从 GLM-5.3 换成了 GLM-5.4），
	// 而 /client/configs 的 builtinModels 晚一步更新。所以「有哪些模型」以账号
	// 权益为准，catalog 只负责补 context_window / max_output_tokens 这些元数据。
	// 反过来做（以 catalog 为准）会把上游已经不给的模型列给客户端。
	type modelEntry struct {
		id  string
		ctx int
		max int
	}
	list := make(map[string]*modelEntry)
	put := func(id string, ctx, max int) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		key := strings.ToLower(id)
		if e, ok := list[key]; ok {
			if e.ctx == 0 {
				e.ctx = ctx
			}
			if e.max == 0 {
				e.max = max
			}
			return
		}
		list[key] = &modelEntry{id: id, ctx: ctx, max: max}
	}

	for _, acc := range s.store.Summaries() {
		for _, qm := range acc.Quota.Models {
			put(qm.Name, 0, 0)
		}
	}
	catalog := modelCatalog()
	if len(list) == 0 {
		// 一个账号都没刷到额度时退回 catalog，保持旧行为。
		for _, m := range catalog {
			put(m.ID, m.ContextWindow, m.MaxOutput)
		}
	} else {
		for _, m := range catalog {
			if e, ok := list[strings.ToLower(strings.TrimSpace(m.ID))]; ok {
				e.ctx, e.max = m.ContextWindow, m.MaxOutput
			}
		}
		// 权益里有、catalog 里没有的模型（新上线的），用 Start Plan 的现行上限。
		for _, e := range list {
			if e.ctx == 0 {
				e.ctx = 1000000
			}
			if e.max == 0 {
				e.max = 128000
			}
		}
	}

	keys := make([]string, 0, len(list))
	for k := range list {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	models := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		e := list[k]
		entry := map[string]any{
			"id": e.id, "object": "model", "created": s.start.Unix(), "owned_by": "zcode",
		}
		if e.ctx > 0 {
			entry["context_window"] = e.ctx
		}
		if e.max > 0 {
			entry["max_output_tokens"] = e.max
		}
		models = append(models, entry)
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": models})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	inFlight, forced := s.pool.Stats()
	writeJSON(w, 200, map[string]any{
		"ok": true, "version": s.version, "build": buildVersion, "uptime": time.Since(s.start).Seconds(),
		"accounts": len(s.store.Summaries()),
		"pool":     map[string]any{"inFlight": inFlight, "forcedReleases": forced},
		// 活动/领取上报的客户端版本，见 claimversion.go。放这里是为了排查
		// 「明明有活动却看不到横幅」时能一眼确认当前用的是哪个版本、从哪来的。
		"claimVersion": claimVersionStatus(),
		"modelCatalog": catalogStatus(),
		"endpointMap":  endpointMapStatus(),
	})
}

// dropBindingsIfAccountDead clears session bindings only for accounts that can
// never serve again: rejected credentials, deleted, disabled or out of quota.
// Transient failures (429, 5xx, network) keep the preference, so sessions
// return to the account once it recovers.
func (s *Server) dropBindingsIfAccountDead(acc *Account, authFailed bool) {
	if acc == nil {
		return
	}
	current := s.store.GetAccount(acc.ID)
	dead := authFailed || current == nil || !current.Enabled ||
		(current.Quota.Granted > 0 && current.Quota.Remaining <= 0)
	if !dead {
		return
	}
	s.pool.InvalidateAccount(acc.ID)
}

// relayOptions 描述一次「拿号 → 转发 → 回写」的完整过程。
// Anthropic 与 OpenAI 两个入口只在编解码上有差别，重试与降级逻辑共用一份。
type relayOptions struct {
	body          map[string]any // 真正发给上游的 Anthropic 请求体
	model         string
	stream        bool
	sessionBody   map[string]any // 客户端原始请求体，用来提取 session key
	clientReqBody string
	render        func(ctx context.Context, w http.ResponseWriter, resp *fhttp.Response, start time.Time) (Usage, streamTiming, int, string)
	writeError    func(w http.ResponseWriter, status int, message string)
	writeUpstream func(w http.ResponseWriter, status int, body []byte)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	if !s.authorized(r) {
		writeError(w, 401, "invalid local proxy token")
		return
	}
	var body map[string]any
	dec := json.NewDecoder(io.LimitReader(r.Body, 32<<20))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		writeError(w, 400, "request body is not valid JSON")
		return
	}
	if _, ok := body["messages"]; !ok {
		writeError(w, 400, "request body requires messages[]")
		return
	}
	stream, _ := body["stream"].(bool)
	model, _ := body["model"].(string)
	if model != "" && !modelAllowed(model) {
		writeError(w, 400, "model not allowed: "+model)
		return
	}
	if model == "" {
		model = "GLM-5.3-Flash"
	}
	clientReqBody := ""
	if s.store.Settings().StoreBodies {
		if data, err := json.Marshal(body); err == nil {
			clientReqBody = truncate(string(data), 8192)
		}
	}
	s.relay(w, r, relayOptions{
		body:          body,
		model:         model,
		stream:        stream,
		sessionBody:   body,
		clientReqBody: clientReqBody,
		render: func(ctx context.Context, w http.ResponseWriter, resp *fhttp.Response, start time.Time) (Usage, streamTiming, int, string) {
			return s.writeUpstream(ctx, w, resp, stream, start)
		},
		writeError:    writeError,
		writeUpstream: writeUpstreamError,
	})
}

// handleChatCompletions 是 OpenAI 兼容入口：请求翻成 Anthropic 再走上游，
// 响应再翻回 chat.completion。这样 Cursor / OpenAI SDK 这类客户端也能直接用。
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, 405, "method not allowed")
		return
	}
	if !s.authorized(r) {
		writeOpenAIError(w, 401, "invalid local proxy token")
		return
	}
	var body map[string]any
	dec := json.NewDecoder(io.LimitReader(r.Body, 32<<20))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		writeOpenAIError(w, 400, "request body is not valid JSON")
		return
	}
	anthropicBody, err := openAIRequestToAnthropic(body)
	if err != nil {
		writeOpenAIError(w, 400, err.Error())
		return
	}
	stream, _ := body["stream"].(bool)
	model, _ := body["model"].(string)
	if model != "" && !modelAllowed(model) {
		writeOpenAIError(w, 400, "model not allowed: "+model)
		return
	}
	if model == "" {
		model = "GLM-5.3-Flash"
	}
	clientReqBody := ""
	if s.store.Settings().StoreBodies {
		if data, err := json.Marshal(body); err == nil {
			clientReqBody = truncate(string(data), 8192)
		}
	}
	s.relay(w, r, relayOptions{
		body:          anthropicBody,
		model:         model,
		stream:        stream,
		sessionBody:   body,
		clientReqBody: clientReqBody,
		render: func(ctx context.Context, w http.ResponseWriter, resp *fhttp.Response, start time.Time) (Usage, streamTiming, int, string) {
			return s.writeUpstreamOpenAI(ctx, w, resp, stream, model, start)
		},
		writeError:    writeOpenAIError,
		writeUpstream: writeOpenAIUpstreamError,
	})
}

func (s *Server) relay(w http.ResponseWriter, r *http.Request, opts relayOptions) {
	start := time.Now()
	sessionKey := extractSessionKey(r, opts.sessionBody)
	accounts := s.store.AccountsForRefresh()
	maxAttempts := len(accounts) + 1
	requestTimeout := envSeconds("ZCODE_REQUEST_TIMEOUT_SEC", 600)
	// 429 原地重试的共享预算：一个请求最多为等频控花这么久，花完就开始换号。
	waitBudget := envSecondsAllowZero("ZCODE_RETRY_WAIT_SEC", 20)
	clientHeaders := map[string]string{
		"anthropic-version": r.Header.Get("anthropic-version"),
		"anthropic-beta":    r.Header.Get("anthropic-beta"),
	}
	var lastStatus int
	var lastBody []byte
	var lastErr error
	egressMode := s.store.Settings().EgressMode
	rotateEgress := s.store.Settings().EgressRotate
	if !s.upstream.HasEgress() {
		egressMode = "direct"
	}
	tried := map[string]bool{}
	// 一个客户端请求最多让几个号「因风控被换掉」。没有上限时 maxAttempts =
	// len(accounts)+1，单个坏请求能逐个毒化全池（2026-11-22 实证：6 个请求
	// 打挂 42 个号，全池 15 分钟冷却）。ZCODE_RISK_MAX_SWITCHES 可调。
	maxRiskSwitches := envInt("ZCODE_RISK_MAX_SWITCHES", 3)
	riskSwitches := 0
	shapeReject := false
	riskCapped := false
	// 兜底出口自己失败（拨号/TLS/中继抖动）与账号无关。把它记到账号账上会
	// 把一次中继抖动摊成整池冷却——2026-11-22 实证：只跑 2 个并发，1 分钟内
	// 20 个号被写成「第 N 次风控退避」，全部源于 egress 重试失败后拿直连的
	// 405 结论去 applyVerdict。这里单独计数，够数就快速失败。
	// 累计失败达 ZCODE_EGRESS_MAX_FAILURES 才快速失败。默认给到 10 而不是 3：
	// 中继抖一下不该让客户端看到 502——账号是好的，多换几个号更划算。
	maxEgressFailures := envInt("ZCODE_EGRESS_MAX_FAILURES", 10)
	egressFailures := 0
	egressDown := false
	for attempt := 0; attempt < maxAttempts; attempt++ {
		acc, release, err := s.pool.AcquireExcludingModel(r.Context(), sessionKey, tried, opts.model)
		if err != nil {
			if errors.Is(err, errNoAvailableAccount) {
				if lastStatus != 0 {
					break
				}
				opts.writeError(w, 503, "no available account")
				return
			}
			if r.Context().Err() != nil {
				return
			}
			opts.writeError(w, 502, err.Error())
			return
		}
		done := false
		verdict := upstreamVerdict{}
		func() {
			defer release()
			ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
			defer cancel()

			viaEgress := egressMode == "always"
			resp, reqBytes, err := s.forwardVia(ctx, acc, opts.body, clientHeaders, viaEgress, sessionKey)
			if err != nil {
				lastErr = err
				if r.Context().Err() == nil {
					s.store.NoteFailure(acc.ID, err.Error())
					s.store.RecordModelFailure(acc.ID, opts.model, err.Error())
					s.dropBindingsIfAccountDead(acc, false)
				}
				return
			}
			verdict, done, lastStatus, lastBody = s.handleAttempt(ctx, w, r, acc, resp, opts, sessionKey, len(reqBytes), start)
			if done {
				return
			}
			// 兜底：直连被风控或限流时，换住宅出口再打同一个号一次。
			// 只有真失败才付这个延迟，正常请求仍然直连。
			if egressMode == "fallback" && !viaEgress && (verdict.Risk || verdict.RateLimited || verdict.InfraBlock) {
				if rotateEgress {
					s.rotateEgress(ctx)
				}
				resp2, reqBytes2, err2 := s.ForwardViaEgress(ctx, acc, opts.body, clientHeaders, sessionKey)
				if err2 != nil {
					// 出口故障不是账号故障：这里绝不能 applyVerdict。直连那次
					// 的结论（多半是 WAF 405）会被记成该号的风控 strike，
					// 而这条路径又不消耗 riskSwitches，于是一个请求就能顺着
					// 轮转把几十个号逐个写成冷却。
					lastErr = err2
					egressFailures++
					if egressFailures >= maxEgressFailures {
						egressDown = true
					}
					return
				}
				verdict, done, lastStatus, lastBody = s.handleAttempt(ctx, w, r, acc, resp2, opts, sessionKey, len(reqBytes2), start)
				if done {
					return
				}
			}
			// 3012 分两档，处置完全相反：
			//   形态问题（client not supported）= 构建级故障，每个号都会拒。
			//     记账号的账毫无意义，只会把一个几分钟能修的问题摊成整池
			//     15 分钟~24 小时指数冷却（2026-11-22 实证：6 个请求打挂 42 个号）。
			//   账号风控 = 单号问题，换号是对的；但也要设上限，否则一个坏
			//     客户端请求仍能逐个毒化全池。
			if verdict.Retry {
				if verdict.Risk && isClientShapeReject(lastBody) {
					shapeReject = true
					return
				}
				if verdict.Risk {
					riskSwitches++
					if riskSwitches >= maxRiskSwitches {
						riskCapped = true
						s.applyVerdict(acc, verdict, opts.model)
						return
					}
				}
				s.applyVerdict(acc, verdict, opts.model)
			}
		}()
		if shapeReject {
			opts.writeError(w, 400,
				"上游拒绝当前请求形态（client not supported）：疑似 system/提示词形态或客户端版本不匹配。"+
					"已停止换号，避免把整池打进风控冷却；修复形态后重试。")
			return
		}
		if riskCapped {
			opts.writeError(w, 502,
				fmt.Sprintf("上游风控 3012：连续 %d 个账号被拒，已停止轮转（避免打挂全池）。"+
					"请检查：出口 IP 是否被多账号共用、并发数、system 形态。", riskSwitches))
			return
		}
		if egressDown {
			opts.writeError(w, 502,
				fmt.Sprintf("兜底出口连续 %d 次失败，直连被拒后无法重试，故快速失败："+
					"检查 ZCODE_PROXY_URL_EGRESS / 本地中继（%v）。账号未被冷却。",
					egressFailures, lastErr))
			return
		}
		if r.Context().Err() != nil {
			return
		}
		var step relayStep
		step, waitBudget = nextRelayStep(verdict, done, waitBudget)
		switch step {
		case relayDone:
			return
		case relayWait:
			// 账号没坏，等一会儿打同一个号；不占换号预算。
			select {
			case <-time.After(verdict.Wait):
			case <-r.Context().Done():
				return
			}
			attempt--
		case relaySwitch:
			tried[acc.ID] = true
		case relayRetry:
			// 转发本身失败，没拿到响应：直接下一轮。
		}
	}

	if lastStatus == 0 {
		if lastErr != nil {
			opts.writeError(w, 502, lastErr.Error())
		} else {
			opts.writeError(w, 503, "no available account")
		}
		return
	}
	status := lastStatus
	// 上游用 200 承载业务失败（比如所有号都没额度）：原样回 200 会让 OpenAI
	// SDK 把错误体当成空回复，不抛异常。按业务码换成对应状态。
	if status == http.StatusOK {
		if mapped := businessErrorStatus(lastBody); mapped != 0 {
			status = mapped
		}
	}
	opts.writeUpstream(w, status, lastBody)
}

// relayStep 是一次尝试结束后的去向。
type relayStep int

const (
	// relayDone：响应已经写出，或不该再试，结束。
	relayDone relayStep = iota
	// relayWait：账号健康、只是被频控，原地等一会儿再打同一个号。
	relayWait
	// relaySwitch：换个号再来，当前号记进 tried。
	relaySwitch
	// relayRetry：转发本身失败（没拿到响应），直接下一轮，账号不记 tried。
	relayRetry
)

func (s relayStep) String() string {
	switch s {
	case relayDone:
		return "done"
	case relayWait:
		return "wait"
	case relaySwitch:
		return "switch"
	case relayRetry:
		return "retry"
	}
	return "unknown"
}

// nextRelayStep 把「上游分类结论 + 剩余等待预算」翻成下一步动作，并返回新的
// 预算。预算是整个请求共享的：等频控花完就换号，别在一个号上把时间耗光。
func nextRelayStep(verdict upstreamVerdict, done bool, waitBudget time.Duration) (relayStep, time.Duration) {
	switch {
	case done:
		return relayDone, waitBudget
	case verdict.Retry && verdict.Wait > 0 && verdict.Wait <= waitBudget:
		return relayWait, waitBudget - verdict.Wait
	case verdict.Retry:
		return relaySwitch, waitBudget
	default:
		return relayRetry, waitBudget
	}
}

func (s *Server) forwardVia(ctx context.Context, acc *Account, body map[string]any, clientHeaders map[string]string, viaEgress bool, sessionKey string) (*fhttp.Response, []byte, error) {
	if viaEgress {
		return s.ForwardViaEgress(ctx, acc, body, clientHeaders, sessionKey)
	}
	return s.Forward(ctx, acc, body, clientHeaders, sessionKey)
}

// handleAttempt 处理一次上游响应：200 就地渲染并记账（done=true），
// 其余情况只做分类，由调用方决定换号、等待还是走兜底出口。
//
// 注意 200 也可能是失败：上游用 200 承载 {"code":1005} 这类业务码，见 business.go。
func (s *Server) handleAttempt(ctx context.Context, w http.ResponseWriter, r *http.Request, acc *Account, resp *fhttp.Response, opts relayOptions, sessionKey string, reqBytes int, start time.Time) (upstreamVerdict, bool, int, []byte) {
	if resp.StatusCode == 200 {
		if verdict, peeked, ok := peekBusinessError(resp); ok {
			resp.Body.Close()
			s.recordLog(acc, opts.model, opts.stream, 200, false, time.Since(start), Usage{}, streamTiming{}, resp.Header.Get("x-request-id"), sessionKey, r, opts.clientReqBody, truncate(string(peeked), 8192), reqBytes, len(peeked), verdict.Reason)
			// 不当成功：不 TouchSession、不 ResetRiskStrikes，交给调用方换号。
			return verdict, false, 200, peeked
		}
		usage, timing, respBytes, respPreview := opts.render(ctx, w, resp, start)
		s.recordLog(acc, opts.model, opts.stream, 200, true, time.Since(start), usage, timing, resp.Header.Get("x-request-id"), sessionKey, r, opts.clientReqBody, respPreview, reqBytes, respBytes, "")
		s.pool.TouchSession(sessionKey)
		s.store.ResetRiskStrikes(acc.ID)
		return upstreamVerdict{}, true, 200, nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	resp.Body.Close()
	verdict := classifyUpstream(resp.StatusCode, resp.Header, data)
	if verdict.Retry {
		// 中间失败必须留痕。换号后客户端可能最终拿到 200，于是这次尝试在
		// 日志表和 journal 里都不存在——2026-11-22 排查时整整 20 次账号冷却
		// 找不到任何对应的请求记录，只能靠反推判定。
		s.recordLog(acc, opts.model, opts.stream, resp.StatusCode, false, time.Since(start),
			Usage{}, streamTiming{}, resp.Header.Get("x-request-id"), sessionKey, r,
			opts.clientReqBody, truncate(string(data), 4096), reqBytes, len(data),
			"换号重试："+verdict.Reason)
		return verdict, false, resp.StatusCode, data
	}
	s.recordLog(acc, opts.model, opts.stream, resp.StatusCode, false, time.Since(start), Usage{}, streamTiming{}, resp.Header.Get("x-request-id"), sessionKey, r, opts.clientReqBody, truncate(string(data), 8192), reqBytes, len(data), verdict.Reason)
	opts.writeUpstream(w, resp.StatusCode, data)
	return verdict, true, resp.StatusCode, data
}

// rotateEgress 让本地中继换一个出口 IP；失败不影响主流程。
//
// 节流是必须的：中继的 /rotate 会主动断开正在传输的隧道，高频调用会让并发
// 请求互相打断。2026-11-22 实证——上游每次 405 都触发一次轮换，一小时 214 次，
// 单请求耗时被拉到 47~54 秒，并产生成片 502；而中继自己的 TTL 是 3000 秒，
// 网关这边按请求轮换纯属负收益。间隔由 ZCODE_EGRESS_ROTATE_MIN_SEC 控制，
// 设 0 表示不节流（仅排障时用）——所以要 allow-zero 版本，envInt 会把 0 当非法。
func (s *Server) rotateEgress(ctx context.Context) {
	minGap := envSecondsAllowZero("ZCODE_EGRESS_ROTATE_MIN_SEC", 30)
	now := time.Now().Unix()
	if minGap > 0 {
		last := s.lastEgressRotate.Load()
		if last != 0 && now-last < int64(minGap/time.Second) {
			return
		}
	}
	// 先占位再发请求：并发的多个 405 不应同时各发一次轮换。
	s.lastEgressRotate.Store(now)
	base := envOr("ZCODE_RELAY_ADMIN", "http://127.0.0.1:3129")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/rotate", nil)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// applyVerdict 把分类结论落到账号状态上：能修的修，修不了才冷却或停用。
func (s *Server) applyVerdict(acc *Account, v upstreamVerdict, model string) {
	switch {
	case v.Disable:
		s.store.DisableAccount(acc.ID, v.Reason)
		s.recordAccountEvent(acc, "已停用", v.Reason)
	case v.InfraBlock:
		// 网关/WAF 层拒绝：请求没进业务层，账号状态一概不动（连 LastError
		// 都不写，否则面板会把一次 WAF 抖动显示成账号故障）。
		return
	case v.Risk:
		strikes := s.store.BumpRiskStrikes(acc.ID)
		cooldown := riskCooldownFor(strikes)
		s.store.MarkExhausted(acc.ID, time.Now().Add(cooldown), v.Reason)
		s.recordAccountEvent(acc, "风控退避", fmt.Sprintf("第 %d 次，冷却 %s：%s", strikes, cooldown, v.Reason))
	case v.Cooldown > 0:
		s.store.MarkExhausted(acc.ID, time.Now().Add(v.Cooldown), v.Reason)
		s.recordAccountEvent(acc, "冷却", fmt.Sprintf("%s：%s", v.Cooldown, v.Reason))
	case v.ModelScoped && model != "":
		var state ModelState
		if v.RateLimited {
			// 额度耗尽（1005/13xx）：账号没坏，冷却封顶不 Disabled，
			// 并立刻刷一次额度让缓存跟上真实（对齐 zcode2api-plus 的
			// markModelExhausted + fireRefresh）。
			state = s.store.RecordModelFailureCapped(acc.ID, model, v.Reason)
			s.forceQuotaRefresh(acc)
		} else {
			state = s.store.RecordModelFailure(acc.ID, model, v.Reason)
		}
		if state.CooldownUntil > 0 || state.Disabled {
			s.recordAccountEvent(acc, "模型级熔断", fmt.Sprintf("%s stage=%d disabled=%v：%s", model, state.Stage, state.Disabled, v.Reason))
		}
	default:
		// 429 / 验证码这类：账号是健康的，只记原因，不冷却。
		s.store.NoteFailure(acc.ID, v.Reason)
	}
	s.dropBindingsIfAccountDead(acc, v.Disable)
}

type streamTiming struct {
	firstTokenMs int64
	lastTokenMs  int64
}

// timingMetrics turns the first/last token timestamps into the generation
// window and decode speed. Thinking tokens count toward generated output.
func timingMetrics(usage Usage, timing streamTiming) (int64, float64) {
	streamMs := timing.lastTokenMs - timing.firstTokenMs
	if streamMs < 0 {
		streamMs = 0
	}
	// ZCode/Anthropic usage.output_tokens already includes thinking tokens;
	// reasoning_tokens is a separate (often estimated) breakdown, so adding it
	// would double-count the thinking portion of the decode speed.
	tokens := usage.OutputTokens
	if streamMs == 0 || tokens == 0 {
		return streamMs, 0
	}
	return streamMs, float64(tokens) * 1000 / float64(streamMs)
}

func (s *Server) writeUpstream(ctx context.Context, w http.ResponseWriter, resp *fhttp.Response, stream bool, start time.Time) (Usage, streamTiming, int, string) {
	defer resp.Body.Close()
	stop := closeBodyOnDone(ctx, resp.Body)
	defer stop()
	for _, name := range []string{"content-type", "cache-control", "x-request-id", "request-id", "x-zcode-request-id", "retry-after"} {
		if v := resp.Header.Get(name); v != "" {
			w.Header().Set(name, v)
		}
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(resp.StatusCode)
	if !stream {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		_, _ = w.Write(data)
		return parseUsageFromJSON(data), streamTiming{}, len(data), truncate(string(data), 8192)
	}
	flusher, _ := w.(http.Flusher)
	tracker := &sseUsageTracker{}
	timing, total, preview := pumpSSE(ctx, resp.Body, start, "anthropic", func(line string) (bool, error) {
		produced := tracker.feed(line)
		if _, err := io.WriteString(w, line); err != nil {
			return produced, err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return produced, nil
	})
	return tracker.finish(), timing, total, preview
}

func (s *Server) recordLog(acc *Account, model string, stream bool, status int, ok bool, dur time.Duration, usage Usage, timing streamTiming, requestID, sessionKey string, r *http.Request, reqBody, respBody string, reqBytes, respBytes int, errMsg string) {
	streamMs, tokensPerSec := timingMetrics(usage, timing)
	entry := &LogEntry{
		ID: randomID(), Time: time.Now().Unix(), AccountID: acc.ID, AccountLabel: acc.Label,
		Model: model, Mode: acc.Mode, Status: status, OK: ok, DurationMs: dur.Milliseconds(),
		FirstTokenMs: timing.firstTokenMs, LastTokenMs: timing.lastTokenMs,
		StreamMs: streamMs, TokensPerSec: tokensPerSec,
		Stream: stream, Usage: usage, UpstreamStatus: status, RequestID: requestID, SessionID: sessionKey,
		ClientIP: clientIP(r), RequestBytes: reqBytes, ResponseBytes: respBytes,
		Error: errMsg,
	}
	if s.store.Settings().StoreBodies {
		entry.RequestBody = reqBody
		entry.ResponseBody = respBody
	}
	s.store.appendLog(entry)
	if ok {
		s.store.ResetModelState(acc.ID, model)
		s.store.UpdateAccountMemory(acc.ID, func(a *Account) {
			a.AddUsage(model, usage)
			a.LastError = ""
			a.LastErrorAt = 0
			a.ExhaustedUntil = 0
		})
	}
}

// recordClaimLog 把一次领取的结果写进 logs 表，面板「调用日志」里能直接看到。
// 领取不是模型请求，token 相关字段留空，mode 固定 claim、model 记套餐名。
func (s *Server) recordClaimLog(acc *Account, planName string, ok bool, code int, message string, dur time.Duration) {
	status := code
	if ok || status == 0 {
		status = 200
	}
	entry := &LogEntry{
		ID:           randomID(),
		Time:         time.Now().Unix(),
		AccountID:    acc.ID,
		AccountLabel: acc.Label,
		Model:        planName,
		Mode:         "claim",
		Status:       status,
		OK:           ok,
		DurationMs:   dur.Milliseconds(),
		Error:        message,
	}
	s.store.appendLog(entry)
}

// recordAccountEvent 把账号状态变化写进日志表（mode=account），
// 面板「请求日志」里能按「账号事件」筛出来，不用再翻 journalctl。
func (s *Server) recordAccountEvent(acc *Account, action, detail string) {
	if acc == nil {
		return
	}
	s.store.appendLog(&LogEntry{
		ID:           randomID(),
		Time:         time.Now().Unix(),
		AccountID:    acc.ID,
		AccountLabel: acc.Label,
		Model:        action,
		Mode:         "account",
		OK:           true,
		Error:        detail,
	})
}

func (s *Server) authorized(r *http.Request) bool {
	token := s.store.Settings().ProxyToken
	if token == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	apiKey := r.Header.Get("x-api-key")
	return auth == "Bearer "+token || apiKey == token
}

func writeUpstreamError(w http.ResponseWriter, status int, body []byte) {
	var parsed any
	if json.Unmarshal(body, &parsed) != nil {
		parsed = map[string]any{"type": "error", "error": map[string]any{"type": "upstream_error", "message": string(body)}}
	}
	writeJSON(w, status, parsed)
}

func decodeJSON(r *http.Request, target any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(target)
}

// relayRequest 把请求原样转发给本地中继（contrib/relay 的管理口），
// 中继只监听回环，网关是它唯一入口，所以鉴权沿用面板登录态。
func (s *Server) relayRequest(w http.ResponseWriter, method, path string, body []byte) {
	base := envOr("ZCODE_RELAY_ADMIN", "http://127.0.0.1:3129")
	client := &http.Client{Timeout: 120 * time.Second}
	var (
		req *http.Request
		err error
	)
	if body != nil {
		req, err = http.NewRequest(method, base+path, bytes.NewReader(body))
	} else {
		req, err = http.NewRequest(method, base+path, nil)
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		writeError(w, 502, "代理池服务不可用（zcode-relay 没起？）："+err.Error())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleProxies：代理池的列表 / 批量添加 / 删除。
func (s *Server) handleProxies(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.relayRequest(w, http.MethodGet, "/proxies", nil)
	case http.MethodPost:
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.relayRequest(w, http.MethodPost, "/proxies", body)
	case http.MethodDelete:
		addr := r.URL.Query().Get("addr")
		s.relayRequest(w, http.MethodDelete, "/proxies?addr="+url.QueryEscape(addr), nil)
	default:
		writeError(w, 405, "method not allowed")
	}
}

// handleProxySettings：体检间隔与"连续失败几次自动删除"。
func (s *Server) handleProxySettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.relayRequest(w, http.MethodGet, "/settings", nil)
	case http.MethodPost:
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.relayRequest(w, http.MethodPost, "/settings", body)
	default:
		writeError(w, 405, "method not allowed")
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		status = 500
		data = []byte(`{"error":"encode failed"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message}})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// clientIP 取真实来源：cloudflared 会带 Cf-Connecting-Ip，其次是反向代理的
// X-Forwarded-For 第一跳，最后回落 RemoteAddr。
func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("Cf-Connecting-Ip")); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		first, _, _ := strings.Cut(v, ",")
		if first = strings.TrimSpace(first); first != "" {
			return first
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
