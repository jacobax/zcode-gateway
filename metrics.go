package main

// Prometheus 指标端点。
//
// 设计约束（照 KeyPort 的做法，值得抄）：
//   - 手写 0.0.4 text exposition，零第三方依赖
//   - scrape 时现算，不新增累加器——不存在「内存一份、DB 一份」的双写一致性问题
//   - scrape 不触发任何上游请求
//   - 确定性输出：所有维度排序，同一状态两次抓取逐字节相等（否则看板 diff 会抖）
//   - label 必须转义：账号 label 是用户起的名字，含引号/反斜杠会写出非法
//     exposition，Prometheus 会丢弃整个 scrape
//   - 不暴露高基数或敏感维度（device_mid、token、client_ip 一律不出）
//
// 鉴权和 /v1 一致走代理 Token。Prometheus 侧配：
//
//	authorization:
//	  credentials: <代理 Token>
//
// 或用 x-api-key。

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// handleMetrics 输出 Prometheus text exposition。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	if !s.authorized(r) {
		writeError(w, 401, "invalid local proxy token")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	var b strings.Builder
	s.writeMetrics(&b)
	_, _ = io.WriteString(w, b.String())
}

// metricStateOrder 固定状态序列，保证输出稳定。
var metricStateOrder = []string{"healthy", "cooling", "exhausted", "pending", "disabled"}

// metricState 把面板状态翻成稳定的英文枚举。用英文是为了让 Grafana 那边的
// 查询和告警规则好写，中文 label 值在 URL 编码里很容易踩坑。
func metricState(status string, enabled bool) string {
	if !enabled {
		return "disabled"
	}
	switch status {
	case "冷却中":
		return "cooling"
	case "额度耗尽":
		return "exhausted"
	case "待生效":
		return "pending"
	default:
		return "healthy"
	}
}

func (s *Server) writeMetrics(b *strings.Builder) {
	summaries := s.store.Summaries()
	inFlight, forced := s.pool.Stats()

	claim := claimVersionStatus()
	claimVersion, _ := claim["version"].(string)
	writeMetricHeader(b, "zcg_build_info", "gauge", "网关构建版本与模拟客户端版本。")
	fmt.Fprintf(b, "zcg_build_info{build=\"%s\",app_version=\"%s\",claim_version=\"%s\"} 1\n",
		escapeMetricLabel(buildVersion), escapeMetricLabel(appVersion), escapeMetricLabel(claimVersion))

	// 账号池按状态分布。state 是「当前为什么不能用」的原因分解，不是互斥分类
	// —— 一个号可以同时是 disabled 和 exhausted，这里只报它在选号眼里的那一面。
	counts := map[string]int{}
	for _, item := range summaries {
		counts[metricState(item.Status, item.Enabled)]++
	}
	writeMetricHeader(b, "zcg_pool_accounts", "gauge", "账号池按状态分布。")
	for _, state := range metricStateOrder {
		fmt.Fprintf(b, "zcg_pool_accounts{state=\"%s\"} %d\n", state, counts[state])
	}

	writeMetricHeader(b, "zcg_pool_in_flight", "gauge", "当前在途的上游请求数。")
	fmt.Fprintf(b, "zcg_pool_in_flight %d\n", inFlight)

	writeMetricHeader(b, "zcg_pool_forced_releases", "counter", "并发槽被看门狗强制释放的累计次数。")
	fmt.Fprintf(b, "zcg_pool_forced_releases %d\n", forced)

	// 按账号的额度与用量。账号数有限（几十个），基数可控。
	sorted := append([]*AccountSummary(nil), summaries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Label != sorted[j].Label {
			return sorted[i].Label < sorted[j].Label
		}
		return sorted[i].ID < sorted[j].ID
	})
	writeMetricHeader(b, "zcg_account_quota_remaining", "gauge", "账号剩余额度（上游口径）。")
	for _, item := range sorted {
		fmt.Fprintf(b, "zcg_account_quota_remaining{account=\"%s\"} %d\n",
			escapeMetricLabel(item.Label), item.Quota.Remaining)
	}
	writeMetricHeader(b, "zcg_account_quota_granted", "gauge", "账号授予额度合计。")
	for _, item := range sorted {
		fmt.Fprintf(b, "zcg_account_quota_granted{account=\"%s\"} %d\n",
			escapeMetricLabel(item.Label), item.Quota.Granted)
	}
	writeMetricHeader(b, "zcg_account_requests_total", "counter", "账号累计处理的请求数。")
	for _, item := range sorted {
		fmt.Fprintf(b, "zcg_account_requests_total{account=\"%s\"} %d\n",
			escapeMetricLabel(item.Label), item.Usage.Requests)
	}

	// 按模型的累计 token。数据取自账号的 Models 台账，重启后仍然单调。
	modelRequests := map[string]int{}
	modelTokens := map[string]*UsageTotals{}
	for _, acc := range s.store.AccountsForRefresh() {
		for name, totals := range acc.Models {
			if totals == nil {
				continue
			}
			modelRequests[name] += totals.Requests
			if modelTokens[name] == nil {
				modelTokens[name] = &UsageTotals{}
			}
			modelTokens[name].Add(totals.Usage)
		}
	}
	models := make([]string, 0, len(modelRequests))
	for name := range modelRequests {
		models = append(models, name)
	}
	sort.Strings(models)

	writeMetricHeader(b, "zcg_model_requests_total", "counter", "按模型的累计请求数。")
	for _, name := range models {
		fmt.Fprintf(b, "zcg_model_requests_total{model=\"%s\"} %d\n",
			escapeMetricLabel(name), modelRequests[name])
	}
	writeMetricHeader(b, "zcg_model_tokens_total", "counter", "按模型与类型的累计 token 数。")
	for _, name := range models {
		totals := modelTokens[name]
		if totals == nil {
			continue
		}
		label := escapeMetricLabel(name)
		for _, kv := range []struct {
			kind  string
			value int
		}{
			{"input", totals.InputTokens},
			{"output", totals.OutputTokens},
			{"cache_read", totals.CacheReadTokens},
			{"cache_write", totals.CacheWriteTokens},
			{"reasoning", totals.ReasoningTokens},
		} {
			fmt.Fprintf(b, "zcg_model_tokens_total{model=\"%s\",type=\"%s\"} %d\n", label, kv.kind, kv.value)
		}
	}
}

func writeMetricHeader(b *strings.Builder, name, kind, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

// escapeMetricLabel 转义 Prometheus label 值里必须处理的三个字符。
// 不能用 Go 的 %q：它会把中文转成 \uXXXX，而 exposition 格式不认这种转义。
func escapeMetricLabel(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
