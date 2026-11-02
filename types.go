package main

import "time"

type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_input_tokens"`
	CacheWriteTokens int `json:"cache_creation_input_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
}

func (u Usage) Total() int {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.ReasoningTokens
}

type UsageTotals struct {
	Requests int `json:"requests"`
	Usage
}

func (t *UsageTotals) Add(u Usage) {
	t.Requests++
	t.InputTokens += u.InputTokens
	t.OutputTokens += u.OutputTokens
	t.CacheReadTokens += u.CacheReadTokens
	t.CacheWriteTokens += u.CacheWriteTokens
	t.ReasoningTokens += u.ReasoningTokens
}

type UserInfo struct {
	ID     string `json:"id"`
	Email  string `json:"email"`
	Name   string `json:"name"`
	Avatar string `json:"avatar"`
}

type ProjectLocation struct {
	OrganizationID string `json:"organizationId"`
	ProjectID      string `json:"projectId"`
	APIKeyID       string `json:"apiKeyId"`
	Origin         string `json:"origin"`
	Family         string `json:"family"`
}

type QuotaModel struct {
	Name      string  `json:"name"`
	Granted   int64   `json:"granted"`
	Used      int64   `json:"used"`
	Remaining int64   `json:"remaining"`
	Percent   float64 `json:"percent"`
	Source    string  `json:"source"`
}

type QuotaPlan struct {
	UserPlanID string       `json:"userPlanId,omitempty"`
	PlanID     string       `json:"planId"`
	Name       string       `json:"name"`
	Status     string       `json:"status"`
	Priority   int          `json:"priority"`
	StartsAt   int64        `json:"startsAt,omitempty"`
	EndsAt     int64        `json:"endsAt,omitempty"`
	Granted    int64        `json:"granted"`
	Used       int64        `json:"used"`
	Remaining  int64        `json:"remaining"`
	Percent    float64      `json:"percent"`
	Models     []QuotaModel `json:"models"`
}

type Quota struct {
	Granted   int64        `json:"granted"`
	Used      int64        `json:"used"`
	Remaining int64        `json:"remaining"`
	Percent   float64      `json:"percent"`
	Source    string       `json:"source"`
	Models    []QuotaModel `json:"models"`
	Plans     []QuotaPlan  `json:"plans,omitempty"`
	PlanName  string       `json:"planName"`
	PlanID    string       `json:"planId"`
	Status    string       `json:"status"`
	EndsAt    int64        `json:"endsAt"`
	UpdatedAt int64        `json:"updatedAt"`
	Error     string       `json:"error,omitempty"`
}

type DeviceProfile struct {
	Platform  string `json:"platform"`
	Arch      string `json:"arch"`
	OSVersion string `json:"osVersion"`
	Language  string `json:"language"`
	Timezone  string `json:"timezone"`
	Screen    string `json:"screen"`
	DeviceMid string `json:"deviceMid"`
}

type Account struct {
	ID                 string                  `json:"id"`
	Label              string                  `json:"label"`
	Provider           string                  `json:"provider"`
	Mode               string                  `json:"mode"`
	User               UserInfo                `json:"user"`
	Project            *ProjectLocation        `json:"project,omitempty"`
	Priority           int                     `json:"priority"`
	Enabled            bool                    `json:"enabled"`
	ExhaustedUntil     int64                   `json:"exhaustedUntil,omitempty"`
	LastError          string                  `json:"lastError,omitempty"`
	LastErrorAt        int64                   `json:"lastErrorAt,omitempty"`
	RateLimitStreak    int                     `json:"rateLimitStreak,omitempty"`
	RiskStrikes        int                     `json:"riskStrikes,omitempty"`
	ClaimBlockedUntil  int64                   `json:"claimBlockedUntil,omitempty"`
	DeviceMid          string                  `json:"-"`
	Profile            *DeviceProfile          `json:"-"`
	Concurrency        int                     `json:"-"`
	LastActivationDate string                  `json:"-"`
	CreatedAt          int64                   `json:"createdAt"`
	LastUsedAt         int64                   `json:"lastUsedAt,omitempty"`
	Usage              UsageTotals             `json:"usage"`
	Models             map[string]*UsageTotals `json:"models,omitempty"`
	Quota              Quota                   `json:"quota"`

	// Secrets are not exposed through JSON. The store persists them
	// encrypted under a separate "secrets" object.
	ZcodeJWT        string `json:"-"`
	AccessToken     string `json:"-"`
	ProjectToken    string `json:"-"`
	ProjectTokenExp int64  `json:"-"`
}

func (a *Account) Available(now time.Time) bool {
	return a.Enabled && a.ExhaustedUntil <= now.Unix()
}

func (a *Account) AddUsage(model string, u Usage) {
	a.Usage.Add(u)
	a.LastUsedAt = time.Now().Unix()
	if a.Models == nil {
		a.Models = map[string]*UsageTotals{}
	}
	m := a.Models[model]
	if m == nil {
		m = &UsageTotals{}
		a.Models[model] = m
	}
	m.Add(u)
}

type LogEntry struct {
	ID             string  `json:"id"`
	Time           int64   `json:"time"`
	AccountID      string  `json:"accountId"`
	AccountLabel   string  `json:"accountLabel"`
	Model          string  `json:"model"`
	Mode           string  `json:"mode"`
	Status         int     `json:"status"`
	OK             bool    `json:"ok"`
	DurationMs     int64   `json:"durationMs"`
	FirstTokenMs   int64   `json:"firstTokenMs"`
	LastTokenMs    int64   `json:"lastTokenMs"`
	StreamMs       int64   `json:"streamMs"`
	TokensPerSec   float64 `json:"tokensPerSec"`
	Stream         bool    `json:"stream"`
	Usage          Usage   `json:"usage"`
	UpstreamStatus int     `json:"upstreamStatus,omitempty"`
	Error          string  `json:"error,omitempty"`
	RequestID      string  `json:"requestId,omitempty"`
	SessionID      string  `json:"sessionId,omitempty"`
	ClientIP       string  `json:"clientIp,omitempty"`
	RequestBytes   int     `json:"requestBytes"`
	ResponseBytes  int     `json:"responseBytes"`
	RequestBody    string  `json:"requestBody,omitempty"`
	ResponseBody   string  `json:"responseBody,omitempty"`
}

type Settings struct {
	ProxyToken         string `json:"proxyToken"`
	Fingerprint        bool   `json:"fingerprint"`
	FingerprintSet     bool   `json:"fingerprintSet,omitempty"`
	PromptMode         string `json:"promptMode"`
	UAProfile          string `json:"uaProfile"`
	StoreBodies        bool   `json:"storeBodies"`
	QuotaIntervalSec   int    `json:"quotaIntervalSec"`
	DefaultConcurrency int    `json:"defaultConcurrency"` // 全局默认单账号并发；账号自己设了就覆盖
	ActiveAccountID    string `json:"activeAccountId"`
	AdminPasswordHash  string `json:"adminPasswordHash,omitempty"`
	// AdminPasswordManual 表示密码是在面板里设的：只有这种情况才压过环境变量。
	// 首次启动自动生成的 hash 不带这个标记，否则老部署会被它顶掉 env 密码。
	AdminPasswordManual bool `json:"adminPasswordManual,omitempty"`
	// AutoUpdateDisabled 关掉后，宿主机的 zcode-gateway-autoupdate.sh 会直接跳过。
	AutoUpdateDisabled bool `json:"autoUpdateDisabled,omitempty"`
	// EgressMode 决定模型流量走不走可选出口：direct（默认）/ fallback（被风控时兜底）/ always。
	EgressMode string `json:"egressMode,omitempty"`
	// EgressRotate 为 true 时，兜底重试前先让中继换一个出口 IP。
	EgressRotate bool `json:"egressRotate,omitempty"`
}

type Overview struct {
	Accounts struct {
		Total     int `json:"total"`
		Enabled   int `json:"enabled"`
		Available int `json:"available"`
		// Plans 按套餐名聚合：每个套餐有多少个号、合计剩余多少额度。
		Plans []PlanStat `json:"plans"`
		// Quota 是启用中账号的额度合计。
		Quota OverviewQuota `json:"quota"`
	} `json:"accounts"`
	Today struct {
		Requests         int `json:"requests"`
		InputTokens      int `json:"inputTokens"`
		OutputTokens     int `json:"outputTokens"`
		CacheReadTokens  int `json:"cacheReadTokens"`
		CacheWriteTokens int `json:"cacheWriteTokens"`
		ReasoningTokens  int `json:"reasoningTokens"`
		TotalTokens      int `json:"totalTokens"`
	} `json:"today"`
	ActiveAccount *AccountSummary `json:"activeAccount,omitempty"`
	Recent        []*LogEntry     `json:"recent"`
	Server        ServerInfo      `json:"server"`
}

// PlanStat 是概览页「套餐分布」的一行：某个套餐有多少个号、还剩多少额度。
type PlanStat struct {
	Name      string `json:"name"`
	Count     int    `json:"count"`
	Enabled   int    `json:"enabled"`
	Granted   int64  `json:"granted"`
	Remaining int64  `json:"remaining"`
}

// OverviewQuota 是启用中账号的额度合计。
type OverviewQuota struct {
	Granted   int64   `json:"granted"`
	Used      int64   `json:"used"`
	Remaining int64   `json:"remaining"`
	Percent   float64 `json:"percent"`
	Accounts  int     `json:"accounts"` // 有额度的账号数
}

type ServerInfo struct {
	Version      string `json:"version"`       // 模拟的 ZCode 客户端版本
	BuildVersion string `json:"buildVersion"`  // 网关自身版本（发布 tag）
	BaseURL      string `json:"baseUrl"`
	Fingerprint  bool   `json:"fingerprint"`
	QuotaSeconds int    `json:"quotaSeconds"`
	DataDir      string `json:"dataDir"`
}

type ModelState struct {
	AccountID     string `json:"accountId,omitempty"`
	Model         string `json:"model"`
	Streak        int    `json:"streak"`
	Stage         int    `json:"stage"`
	CooldownUntil int64  `json:"cooldownUntil,omitempty"`
	Disabled      bool   `json:"disabled"`
	LastError     string `json:"lastError,omitempty"`
	UpdatedAt     int64  `json:"updatedAt,omitempty"`
}

type AccountSummary struct {
	ID                   string       `json:"id"`
	Label                string       `json:"label"`
	Provider             string       `json:"provider"`
	Mode                 string       `json:"mode"`
	Priority             int          `json:"priority"`
	Enabled              bool         `json:"enabled"`
	Status               string       `json:"status"`
	Concurrency          int          `json:"concurrency"`          // 0 = 跟随全局默认
	ConcurrencyEffective int          `json:"concurrencyEffective"` // 实际生效值
	ModelStates          []ModelState `json:"modelStates,omitempty"`
	User                 UserInfo     `json:"user"`
	Quota                Quota        `json:"quota"`
	LastError            string       `json:"lastError,omitempty"`
	ExhaustedUntil       int64        `json:"exhaustedUntil,omitempty"`
	ClaimBlockedUntil    int64        `json:"claimBlockedUntil,omitempty"`
	LastUsedAt           int64        `json:"lastUsedAt,omitempty"`
	CreatedAt            int64        `json:"createdAt"`
	Usage                UsageTotals  `json:"usage"`
	Today                TodayStats   `json:"today"`
	HasProject           bool         `json:"hasProject"`
	Platform             string       `json:"platform,omitempty"`
	OSVersion            string       `json:"osVersion,omitempty"`
	Screen               string       `json:"screen,omitempty"`
	DeviceMid            string       `json:"deviceMid,omitempty"`
}
