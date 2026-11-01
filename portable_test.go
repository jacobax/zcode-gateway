package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func newTestStore(t *testing.T, tag string) *Store {
	t.Helper()
	dir, err := os.MkdirTemp("", "zcg-portable-"+tag+"-")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		_ = os.RemoveAll(dir)
	})
	store.accounts = nil
	store.settings.ProxyToken = ""
	return store
}

func TestPortableRoundTrip(t *testing.T) {
	payload := portablePayload{Accounts: []portableAccount{{
		Label: "a", Provider: "zai", Mode: "start-plan",
		User: UserInfo{ID: "u1", Name: "账号一"}, Enabled: true,
		ZcodeJWT: "jwt-secret", AccessToken: "access-secret",
	}}}
	bundle, err := sealPortable(payload, "pass1234")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Format != portableFormat || bundle.Version != portableVersion || bundle.Accounts != 1 {
		t.Fatalf("unexpected bundle header: %#v", bundle)
	}
	if strings.Contains(bundle.Ciphertext, "jwt-secret") {
		t.Fatal("ciphertext must not contain plaintext secrets")
	}
	opened, err := openPortable(bundle, "pass1234")
	if err != nil {
		t.Fatal(err)
	}
	if len(opened.Accounts) != 1 || opened.Accounts[0].ZcodeJWT != "jwt-secret" {
		t.Fatalf("secrets lost in round trip: %#v", opened.Accounts)
	}
	if _, err := openPortable(bundle, "wrong-pass"); err == nil {
		t.Fatal("wrong passphrase must fail")
	}
	if _, err := openPortable(portableBundle{Format: "other"}, "pass1234"); err == nil {
		t.Fatal("foreign bundle must be rejected")
	}
}

func TestPortableExportImportHandlers(t *testing.T) {
	source := newTestStore(t, "src")
	acc := &Account{
		ID: "src-1", Label: "来源账号", Provider: "zai", Mode: "start-plan",
		User: UserInfo{ID: "user-42", Name: "来源账号"}, Enabled: true, CreatedAt: 1,
		ZcodeJWT: "jwt-42", AccessToken: "access-42",
		Models: map[string]*UsageTotals{}, Quota: Quota{Granted: 100, Remaining: 100},
	}
	if err := source.AddAccount(acc); err != nil {
		t.Fatal(err)
	}
	sourceServer := NewServer(source, mustUpstream(t), "http://test")

	body, _ := json.Marshal(map[string]string{"passphrase": "pass1234"})
	rec := httptest.NewRecorder()
	sourceServer.handleExportAccounts(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/export", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rec.Code, rec.Body.String())
	}
	var bundle portableBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Accounts != 1 {
		t.Fatalf("expected 1 account in bundle, got %d", bundle.Accounts)
	}

	// 目标实例：一开始是空的
	target := newTestStore(t, "dst")
	targetServer := NewServer(target, mustUpstream(t), "http://test")

	restoreBody, _ := json.Marshal(map[string]any{"passphrase": "pass1234", "bundle": bundle})
	rec = httptest.NewRecorder()
	targetServer.handleRestoreAccounts(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/restore", bytes.NewReader(restoreBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body.String())
	}
	var result struct {
		Imported int `json:"imported"`
		Skipped  int `json:"skipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Imported != 1 {
		t.Fatalf("expected 1 imported, got %+v", result)
	}
	imported := target.FindByUser("zai", "user-42")
	if imported == nil {
		t.Fatal("imported account not found")
	}
	if imported.ZcodeJWT != "jwt-42" || imported.AccessToken != "access-42" {
		t.Fatalf("secrets not restored: %#v", imported)
	}
	if imported.ID == "src-1" {
		// 账号 ID 是本机生成的，导入时应该重新分配，避免和来源机器撞车
		t.Fatalf("imported account should get a fresh local id, got %q", imported.ID)
	}

	// 再导入一次：应该按 provider + 上游用户 ID 判重跳过
	rec = httptest.NewRecorder()
	targetServer.handleRestoreAccounts(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/restore", bytes.NewReader(restoreBody)))
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Imported != 0 || result.Skipped != 1 {
		t.Fatalf("second import should skip: %+v", result)
	}

	// 错口令必须被拒
	badBody, _ := json.Marshal(map[string]any{"passphrase": "nope-nope", "bundle": bundle})
	rec = httptest.NewRecorder()
	targetServer.handleRestoreAccounts(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/restore", bytes.NewReader(badBody)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong passphrase should be 400, got %d", rec.Code)
	}
}
