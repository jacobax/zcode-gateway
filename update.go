package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// updateRepo 是发布源；面板的「检查更新」就是问它最新 tag 是多少。
const updateRepo = "jacobax/zcode-gateway"

type updateStatus struct {
	Current     string `json:"current"`
	Latest      string `json:"latest,omitempty"`
	HasUpdate   bool   `json:"hasUpdate"`
	ReleaseURL  string `json:"releaseUrl,omitempty"`
	PublishedAt string `json:"publishedAt,omitempty"`
	AutoUpdate  bool   `json:"autoUpdate"`
	CheckedAt   int64  `json:"checkedAt"`
	Error       string `json:"error,omitempty"`
}

type updateChecker struct {
	mu       sync.Mutex
	cached   updateStatus
	cachedAt time.Time
	ttl      time.Duration
}

func newUpdateChecker() *updateChecker {
	return &updateChecker{ttl: 10 * time.Minute}
}

// check 查最新 release。缓存 TTL 内直接返回，避免每次开面板都打 GitHub。
func (c *updateChecker) check(current string, autoUpdate bool, force bool) updateStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if !force && !c.cachedAt.IsZero() && now.Sub(c.cachedAt) < c.ttl {
		status := c.cached
		status.AutoUpdate = autoUpdate
		return status
	}
	status := updateStatus{Current: current, AutoUpdate: autoUpdate, CheckedAt: now.Unix()}
	latest, url, published, err := fetchLatestRelease()
	if err != nil {
		status.Error = err.Error()
		if !c.cachedAt.IsZero() {
			status.Latest = c.cached.Latest
			status.ReleaseURL = c.cached.ReleaseURL
			status.HasUpdate = c.cached.HasUpdate
		}
		c.cached, c.cachedAt = status, now
		return status
	}
	status.Latest = latest
	status.ReleaseURL = url
	status.PublishedAt = published
	status.HasUpdate = versionNewer(latest, current)
	c.cached, c.cachedAt = status, now
	return status
}

func fetchLatestRelease() (tag, url, published string, err error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/"+updateRepo+"/releases/latest", nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "zcode-gateway")
	if token := envOr("ZCODE_GITHUB_TOKEN", ""); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("查询 GitHub 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("GitHub 返回 HTTP %d", resp.StatusCode)
	}
	var payload struct {
		TagName     string `json:"tag_name"`
		HTMLURL     string `json:"html_url"`
		PublishedAt string `json:"published_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", "", "", err
	}
	if strings.TrimSpace(payload.TagName) == "" {
		return "", "", "", fmt.Errorf("GitHub 没返回 tag_name")
	}
	return payload.TagName, payload.HTMLURL, payload.PublishedAt, nil
}

// versionNewer 只比数字段：v1.4.1 > v1.4.0。"dev" 之类的本地构建一律不算落后。
func versionNewer(latest, current string) bool {
	latestParts, ok1 := parseVersion(latest)
	currentParts, ok2 := parseVersion(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < len(latestParts) || i < len(currentParts); i++ {
		var a, b int
		if i < len(latestParts) {
			a = latestParts[i]
		}
		if i < len(currentParts) {
			b = currentParts[i]
		}
		if a != b {
			return a > b
		}
	}
	return false
}

func parseVersion(value string) ([]int, bool) {
	value = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "v"))
	if value == "" {
		return nil, false
	}
	parts := strings.Split(value, ".")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, false
		}
		// 允许 1.4.0-rc1 这类后缀：只取数字前缀。
		digits := part
		if idx := strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }); idx >= 0 {
			digits = part[:idx]
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	force := r.URL.Query().Get("force") == "1"
	autoUpdate := !s.store.Settings().AutoUpdateDisabled
	writeJSON(w, 200, s.updates.check(buildVersion, autoUpdate, force))
}
