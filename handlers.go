package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
	}
	server.initAdminPassword()
	return server
}

func (s *Server) summary(acc *Account) *AccountSummary {
	if acc == nil {
		return nil
	}
	status := "可用"
	switch {
	case !acc.Enabled:
		status = "已禁用"
	case acc.Quota.Granted > 0 && acc.Quota.Remaining <= 0:
		status = "额度耗尽"
	case acc.ExhaustedUntil > time.Now().Unix():
		status = "冷却中"
	}
	summary := &AccountSummary{
		ID: acc.ID, Label: acc.Label, Provider: acc.Provider, Mode: acc.Mode,
		Priority: acc.Priority, Enabled: acc.Enabled, Status: status,
		Concurrency: normalizeConcurrency(acc.Concurrency), ConcurrencyEffective: s.store.EffectiveConcurrency(acc),
		User: acc.User, Quota: acc.Quota, LastError: acc.LastError,
		ExhaustedUntil: acc.ExhaustedUntil, ClaimBlockedUntil: acc.ClaimBlockedUntil,
		LastUsedAt:     acc.LastUsedAt, CreatedAt: acc.CreatedAt, Usage: acc.Usage,
		HasProject: acc.Project != nil,
	}
	if profile := s.store.ProfileOf(acc); profile.DeviceMid != "" {
		summary.Platform = profile.Platform + "-" + profile.Arch
		summary.OSVersion = profile.OSVersion
		summary.Screen = profile.Screen
		summary.DeviceMid = profile.DeviceMid
	}
	return summary
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
		out["account"] = s.summary(acc)
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
	writeJSON(w, 200, map[string]any{"status": "ready", "account": s.summary(acc)})
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
		writeJSON(w, 200, map[string]any{"ok": true, "account": s.summary(s.store.GetAccount(id))})
	case r.Method == http.MethodPost && action == "fingerprint":
		profile := s.store.RotateProfile(id)
		if profile == nil {
			writeError(w, 404, "account not found")
			return
		}
		s.pool.signal()
		writeJSON(w, 200, map[string]any{"ok": true, "profile": profile})
	case r.Method == http.MethodPost && action == "clear-cooldown":
		if !s.store.UpdateAccount(id, func(a *Account) {
			a.ExhaustedUntil = 0
			a.LastError = ""
			a.LastErrorAt = 0
			a.RateLimitStreak = 0
		}) {
			writeError(w, 404, "account not found")
			return
		}
		s.store.ResetAccountModelStates(id)
		s.pool.signal()
		writeJSON(w, 200, map[string]any{"ok": true, "account": s.summary(s.store.GetAccount(id))})
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
			"settings":       settings,
			"passwordSource": s.passwordSource(),
			"proxyTokenMask": maskToken(real),
			"proxyTokenSet":  real != "",
			"server": ServerInfo{
				Version: s.version, BaseURL: s.baseURL, Fingerprint: settings.Fingerprint,
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
			UAProfile          *string `json:"uaProfile"`
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
			if body.UAProfile != nil {
				switch *body.UAProfile {
				case "zcode", "claude-code", "chrome":
					st.UAProfile = *body.UAProfile
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
	models := []map[string]any{
		{"id": "GLM-5.3-Flash", "object": "model", "created": s.start.Unix(), "owned_by": "zcode"},
		{"id": "GLM-5.3", "object": "model", "created": s.start.Unix(), "owned_by": "zcode"},
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": models})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	inFlight, forced := s.pool.Stats()
	writeJSON(w, 200, map[string]any{
		"ok": true, "version": s.version, "uptime": time.Since(s.start).Seconds(),
		"accounts": len(s.store.Summaries()),
		"pool":     map[string]any{"inFlight": inFlight, "forcedReleases": forced},
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
	tried := map[string]bool{}
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
			resp, reqBytes, err := s.Forward(ctx, acc, opts.body, clientHeaders)
			if err != nil {
				lastErr = err
				if r.Context().Err() == nil {
					s.store.NoteFailure(acc.ID, err.Error())
					s.store.RecordModelFailure(acc.ID, opts.model, err.Error())
					s.dropBindingsIfAccountDead(acc, false)
				}
				return
			}
			if resp.StatusCode == 200 {
				usage, timing, respBytes, respPreview := opts.render(ctx, w, resp, start)
				s.recordLog(acc, opts.model, opts.stream, 200, true, time.Since(start), usage, timing, resp.Header.Get("x-request-id"), sessionKey, r, opts.clientReqBody, respPreview, len(reqBytes), respBytes, "")
				s.pool.TouchSession(sessionKey)
				s.store.ResetRiskStrikes(acc.ID)
				done = true
				return
			}
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			resp.Body.Close()
			lastStatus = resp.StatusCode
			verdict = classifyUpstream(resp.StatusCode, resp.Header.Get("Retry-After"), data)
			if verdict.Retry {
				s.applyVerdict(acc, verdict, opts.model)
				return
			}
			s.recordLog(acc, opts.model, opts.stream, resp.StatusCode, false, time.Since(start), Usage{}, streamTiming{}, resp.Header.Get("x-request-id"), sessionKey, r, opts.clientReqBody, truncate(string(data), 8192), len(reqBytes), len(data), verdict.Reason)
			opts.writeUpstream(w, resp.StatusCode, data)
			done = true
		}()
		if done || r.Context().Err() != nil {
			return
		}
		if verdict.Retry && verdict.Wait > 0 && verdict.Wait <= waitBudget {
			// 账号没坏，等一会儿打同一个号；不占换号预算。
			waitBudget -= verdict.Wait
			select {
			case <-time.After(verdict.Wait):
			case <-r.Context().Done():
				return
			}
			attempt--
			continue
		}
		if verdict.Retry {
			tried[acc.ID] = true
			continue
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
	opts.writeUpstream(w, lastStatus, lastBody)
}

// applyVerdict 把分类结论落到账号状态上：能修的修，修不了才冷却或停用。
func (s *Server) applyVerdict(acc *Account, v upstreamVerdict, model string) {
	switch {
	case v.Disable:
		s.store.DisableAccount(acc.ID, v.Reason)
	case v.Risk:
		strikes := s.store.BumpRiskStrikes(acc.ID)
		s.store.MarkExhausted(acc.ID, time.Now().Add(riskCooldownFor(strikes)), v.Reason)
	case v.Cooldown > 0:
		s.store.MarkExhausted(acc.ID, time.Now().Add(v.Cooldown), v.Reason)
	case v.ModelScoped && model != "":
		s.store.RecordModelFailure(acc.ID, model, v.Reason)
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
	// Close the upstream body when the caller goes away so a disconnected
	// downstream cannot pin the account slot.
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = resp.Body.Close()
		case <-stop:
		}
	}()
	defer close(stop)
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
	reader := bufio.NewReaderSize(resp.Body, 32*1024)
	tracker := &sseUsageTracker{}
	total := 0
	var preview strings.Builder
	var timing streamTiming
	var firstTokenAt, lastTokenAt time.Time
	for {
		if ctx.Err() != nil {
			break
		}
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			total += len(line)
			if preview.Len() < 8192 {
				preview.WriteString(line)
			}
			if tracker.feed(line) {
				now := time.Now()
				if firstTokenAt.IsZero() {
					firstTokenAt = now
				}
				lastTokenAt = now
			}
			if _, werr := io.WriteString(w, line); werr != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	if !firstTokenAt.IsZero() {
		timing.firstTokenMs = firstTokenAt.Sub(start).Milliseconds()
		timing.lastTokenMs = lastTokenAt.Sub(start).Milliseconds()
	}
	return tracker.finish(), timing, total, truncate(preview.String(), 8192)
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
			a.RateLimitStreak = 0
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

func (s *Server) authorized(r *http.Request) bool {
	token := s.store.Settings().ProxyToken
	if token == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	apiKey := r.Header.Get("x-api-key")
	return auth == "Bearer "+token || apiKey == token
}

const rateLimitFreezeAfter = 3

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

// handleProxies 把面板里的代理池操作转发给本地中继（contrib/relay 的管理口）。
// 中继只监听回环，网关是它唯一入口，所以鉴权沿用面板登录态。
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
