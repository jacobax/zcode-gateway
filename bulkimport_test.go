package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testJWT 造一个形状合法、payload 能解出 user_id 的 JWT。
func testJWT(userID string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":"` + userID + `","sub":"` + userID + `"}`))
	return "eyJhbGciOiJIUzI1NiJ9." + payload + ".sig"
}

func TestParseBulkImportAccountFile(t *testing.T) {
	file := `{"version":1,"providers":{
		"zai":[{"name":"oauth-login","mode":"jwt","secret":"` + testJWT("u-1") + `"},
		       {"name":"oauth-login","mode":"jwt","secret":"` + testJWT("u-2") + `"},
		       {"name":"broken","mode":"jwt","secret":"not-a-jwt"}],
		"bigmodel":[]}}`
	creds, err := parseBulkImport(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 2 {
		t.Fatalf("only the two well-formed JWTs should survive: %#v", creds)
	}
	if creds[0].Provider != "zai" || creds[0].JWT != testJWT("u-1") {
		t.Fatalf("first credential: %#v", creds[0])
	}
	if creds[1].JWT != testJWT("u-2") {
		t.Fatalf("second credential: %#v", creds[1])
	}
}

func TestParseBulkImportJWTLines(t *testing.T) {
	content := "# 注释行\n\n" + testJWT("u-1") + "\nBearer " + testJWT("u-2") + "\n   \nnot-a-jwt\n"
	creds, err := parseBulkImport(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 2 {
		t.Fatalf("expected 2 JWTs, got %#v", creds)
	}
	if creds[0].Provider != "zai" || creds[0].JWT != testJWT("u-1") {
		t.Fatalf("first credential: %#v", creds[0])
	}
	// 带 Bearer 前缀的行也要能认。
	if creds[1].JWT != testJWT("u-2") {
		t.Fatalf("Bearer prefix should be stripped: %#v", creds[1])
	}
}

func TestParseBulkImportErrors(t *testing.T) {
	cases := []struct{ name, content string }{
		{"空内容", "   \n\t "},
		{"坏 JSON", "{not json"},
		{"没有 providers", `{"version":1}`},
		{"没有可用 JWT", `{"providers":{"zai":[{"secret":"nope"}]}}`},
		{"没有 JWT 行", "hello\nworld"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseBulkImport(tc.content); err == nil {
				t.Fatalf("expected an error for %q", tc.content)
			}
		})
	}
}

func TestHandleBulkImport(t *testing.T) {
	store := newTestStore(t, "bulk-import")
	server := NewServer(store, mustUpstream(t), "http://test")
	// 两个好 JWT + 一个形状对但 payload 解不出来的。
	content := testJWT("user-0001") + "\n" + testJWT("user-0002") + "\naaa.bbb.ccc"
	post := func() (int, map[string]any) {
		body, _ := json.Marshal(map[string]string{"content": content})
		rec := httptest.NewRecorder()
		server.handleBulkImport(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/import-bulk", strings.NewReader(string(body))))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, out := post()
	if code != http.StatusOK || out["imported"] != float64(2) || out["skipped"] != float64(0) || out["invalid"] != float64(1) {
		t.Fatalf("first import: %d %#v", code, out)
	}
	// 同一个 JWT 再导一次：全部跳过，不会重复建号。
	code, out = post()
	if code != http.StatusOK || out["imported"] != float64(0) || out["skipped"] != float64(2) || out["invalid"] != float64(1) {
		t.Fatalf("second import: %d %#v", code, out)
	}

	accounts := store.Accounts()
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accounts))
	}
	for _, acc := range accounts {
		if !acc.Enabled || acc.Mode != "start-plan" || acc.ZcodeJWT == "" {
			t.Fatalf("imported account should be an enabled start-plan account: %#v", acc)
		}
		if !strings.HasPrefix(acc.Label, "zai-") || len(acc.Label) != len("zai-")+8 {
			t.Fatalf("label should be provider + 8-char user id: %q", acc.Label)
		}
		if acc.User.ID == "" {
			t.Fatalf("user id should come from the JWT: %#v", acc.User)
		}
	}
}
