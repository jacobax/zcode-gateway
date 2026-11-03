package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// label 转义必须精确：含引号/反斜杠的账号名如果原样写出去，Prometheus
// 会丢弃整个 scrape。
func TestEscapeMetricLabel(t *testing.T) {
	cases := map[string]string{
		`plain`:          `plain`,
		`with"quote`:     `with\"quote`,
		`with\backslash`: `with\\backslash`,
		"with\nnewline":  `with\nnewline`,
		`中文账号`:           `中文账号`,
	}
	for in, want := range cases {
		if got := escapeMetricLabel(in); got != want {
			t.Errorf("escapeMetricLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// 状态映射要稳定：面板中文状态 → 英文枚举，未知状态落 healthy 而不是空串。
func TestMetricState(t *testing.T) {
	cases := []struct {
		status  string
		enabled bool
		want    string
	}{
		{"可用", true, "healthy"},
		{"冷却中", true, "cooling"},
		{"额度耗尽", true, "exhausted"},
		{"待生效", true, "pending"},
		{"可用", false, "disabled"},
		{"已禁用", false, "disabled"},
		{"", true, "healthy"},
	}
	for _, c := range cases {
		if got := metricState(c.status, c.enabled); got != c.want {
			t.Errorf("metricState(%q,%v) = %q, want %q", c.status, c.enabled, got, c.want)
		}
	}
}

// 输出必须是合法 exposition：每个指标都有 HELP/TYPE，且同一状态两次抓取逐字节相等。
func TestMetricsOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(`{
		"version":1,
		"settings":{"claimWindow":"19-21"},
		"accounts":[
			{"id":"a1","label":"alpha","enabled":true,"mode":"start-plan","priority":1,
			 "quota":{"granted":100,"remaining":60,"source":"api"},
			 "usage":{"requests":7,"input_tokens":10,"output_tokens":5}},
			{"id":"a2","label":"beta\"quoted","enabled":false,"mode":"start-plan","priority":2,
			 "quota":{"granted":0,"remaining":0,"source":"api-empty"},
			 "usage":{"requests":0}}
		]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	upstream, err := NewUpstream()
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, upstream, "http://127.0.0.1:0")

	render := func() string {
		var b strings.Builder
		server.writeMetrics(&b)
		return b.String()
	}
	first, second := render(), render()
	if first != second {
		t.Fatalf("output not deterministic:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	for _, want := range []string{
		"# TYPE zcg_build_info gauge",
		"# TYPE zcg_pool_accounts gauge",
		`zcg_pool_accounts{state="healthy"} 1`,
		`zcg_pool_accounts{state="disabled"} 1`,
		`zcg_account_quota_remaining{account="alpha"} 60`,
		`zcg_account_requests_total{account="alpha"} 7`,
		// 引号必须被转义，否则整个 scrape 会被 Prometheus 丢掉。
		`zcg_account_quota_granted{account="beta\"quoted"} 0`,
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("missing %q in output:\n%s", want, first)
		}
	}
	// 每个 HELP 后面都要有对应的 TYPE。
	for _, line := range strings.Split(first, "\n") {
		if strings.HasPrefix(line, "# HELP ") {
			name := strings.Fields(line)[2]
			if !strings.Contains(first, "# TYPE "+name+" ") {
				t.Fatalf("metric %q has HELP but no TYPE", name)
			}
		}
	}
}

// 未授权时不能吐指标。
func TestMetricsRequiresToken(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.UpdateSettings(func(st *Settings) { st.ProxyToken = "secret-token" })
	upstream, err := NewUpstream()
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, upstream, "http://127.0.0.1:0")

	rec := httptest.NewRecorder()
	server.handleMetrics(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 401 {
		t.Fatalf("want 401 without token, got %d", rec.Code)
	}

	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec = httptest.NewRecorder()
	server.handleMetrics(rec, req)
	if rec.Code != 200 {
		t.Fatalf("want 200 with token, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "zcg_build_info") {
		t.Fatalf("body missing metrics: %s", rec.Body.String())
	}
}
