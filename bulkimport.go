package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
)

// 批量导入：面板「添加账号」里的两种粘贴格式。
//
//  1. zcode-accounts.json（别的工具或旧版本导出的账号文件）：
//     {"version":1,"providers":{"zai":[{"name":"oauth-login","mode":"jwt","secret":"<JWT>"}]}}
//  2. 每行一个 JWT（空行和 # 开头的注释跳过）。
//
// 两种最后都归成「provider + JWT」列表，账号信息从 JWT 的 payload 里解。
// 这类号只有 ZcodeJWT，没有 project key，所以固定按 start-plan 模式走。

type bulkCredential struct {
	Provider string
	JWT      string
}

type bulkAccountFile struct {
	Version   int                           `json:"version"`
	Providers map[string][]bulkAccountEntry `json:"providers"`
}

type bulkAccountEntry struct {
	Name   string `json:"name"`
	Mode   string `json:"mode"`
	Secret string `json:"secret"`
}

// parseBulkImport 把粘贴内容解析成凭据列表。
func parseBulkImport(content string) ([]bulkCredential, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, fmt.Errorf("导入内容为空")
	}
	if strings.HasPrefix(content, "{") {
		return parseBulkAccountFile(content)
	}
	creds := parseJWTLines(content)
	if len(creds) == 0 {
		return nil, fmt.Errorf("没有找到合法的 JWT（每行一个，三段式 eyJ...）")
	}
	return creds, nil
}

func parseBulkAccountFile(content string) ([]bulkCredential, error) {
	var file bulkAccountFile
	if err := json.Unmarshal([]byte(content), &file); err != nil {
		return nil, fmt.Errorf("账号文件不是合法 JSON: %w", err)
	}
	if len(file.Providers) == 0 {
		return nil, fmt.Errorf("账号文件里没有 providers")
	}
	// provider 顺序固定，导入结果可复现。
	providers := make([]string, 0, len(file.Providers))
	for name := range file.Providers {
		providers = append(providers, name)
	}
	sort.Strings(providers)
	var out []bulkCredential
	for _, provider := range providers {
		for _, entry := range file.Providers[provider] {
			jwt := strings.TrimSpace(entry.Secret)
			if !looksLikeJWT(jwt) {
				continue
			}
			out = append(out, bulkCredential{Provider: provider, JWT: jwt})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("账号文件里没有可用的 JWT")
	}
	return out, nil
}

func parseJWTLines(content string) []bulkCredential {
	var out []bulkCredential
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Bearer "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !looksLikeJWT(line) {
			continue
		}
		out = append(out, bulkCredential{Provider: "zai", JWT: line})
	}
	return out
}

// looksLikeJWT 只做形状检查（三段、前两段非空）；user_id 由
// jwtUserIDFromToken 解析，解不出来才算无效。
func looksLikeJWT(token string) bool {
	parts := strings.Split(token, ".")
	return len(parts) == 3 && parts[0] != "" && parts[1] != ""
}

// handleBulkImport 处理面板「添加账号 → 批量导入」。
func (s *Server) handleBulkImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, 400, "invalid json")
		return
	}
	creds, err := parseBulkImport(body.Content)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	imported, skipped, invalid := 0, 0, 0
	for _, cred := range creds {
		userID := jwtUserIDFromToken(cred.JWT)
		if userID == "" {
			invalid++
			continue
		}
		if existing := s.store.FindByUser(cred.Provider, userID); existing != nil {
			skipped++
			continue
		}
		acc := &Account{
			Label:    bulkAccountLabel(cred.Provider, userID),
			Provider: cred.Provider,
			Mode:     "start-plan",
			User:     UserInfo{ID: userID},
			Enabled:  true,
			ZcodeJWT: cred.JWT,
			Models:   map[string]*UsageTotals{},
		}
		if err := s.store.AddAccount(acc); err != nil {
			invalid++
			continue
		}
		imported++
	}
	log.Printf("批量导入账号：新增 %d，已存在 %d，无效 %d", imported, skipped, invalid)
	writeJSON(w, 200, map[string]any{
		"imported": imported, "skipped": skipped, "invalid": invalid,
		"items": s.store.Summaries(),
	})
}

// bulkAccountLabel 给导入的号起个能认出来的名字：JWT 里没有邮箱，用
// provider + user_id 前 8 位，和桌面端导入的命名习惯一致。
func bulkAccountLabel(provider, userID string) string {
	suffix := userID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	return provider + "-" + suffix
}
