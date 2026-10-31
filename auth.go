package main

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	adminCookieName = "zcode_admin"
	adminSessionTTL = 7 * 24 * 3600
)

func (s *Server) setPasswordState(pass, hash string, generated bool) {
	s.adminMu.Lock()
	s.adminPass, s.adminHash, s.adminGenerated = pass, hash, generated
	s.adminMu.Unlock()
}

// passwordSource reports where the active panel password comes from, so the
// settings page can tell the operator why an edit took effect (or not).
func (s *Server) passwordSource() string {
	s.adminMu.RLock()
	defer s.adminMu.RUnlock()
	switch {
	case s.adminGenerated:
		return "generated"
	case s.adminPass != "":
		return "env"
	case s.adminHash != "":
		return "panel"
	}
	return "none"
}

// initAdminPassword resolves the panel password in this order:
//  1. a password set from the panel (AdminPasswordManual) — wins over the env var;
//  2. ZCODE_ADMIN_PASSWORD — the bootstrap value for a fresh deployment;
//  3. an older stored hash without the manual flag (pre-1.3 data);
//  4. otherwise generate one and print it to the log.
//
// Order matters: a plain stored hash must not shadow the env var, or an install
// that only ever had ZCODE_ADMIN_PASSWORD set would lock itself out.
func (s *Server) initAdminPassword() {
	configured := strings.TrimSpace(os.Getenv("ZCODE_ADMIN_PASSWORD"))
	settings := s.store.Settings()
	hash := settings.AdminPasswordHash
	switch {
	case settings.AdminPasswordManual && hash != "":
		s.setPasswordState("", hash, false)
		if configured != "" {
			log.Printf("面板密码以面板设置为准，环境变量 ZCODE_ADMIN_PASSWORD 已忽略")
		}
	case configured != "":
		s.setPasswordState(configured, "", false)
	case hash != "":
		s.setPasswordState("", hash, false)
	default:
		token := make([]byte, 12)
		if _, err := rand.Read(token); err != nil {
			token = []byte("zcode-admin")
		}
		password := base64.RawURLEncoding.EncodeToString(token)
		generated, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			s.setPasswordState(password, "", true)
			return
		}
		s.store.UpdateSettings(func(st *Settings) {
			st.AdminPasswordHash = string(generated)
		})
		s.setPasswordState(password, string(generated), true)
	}
}

func (s *Server) passwordMatches(candidate string) bool {
	s.adminMu.RLock()
	pass, hash := s.adminPass, s.adminHash
	s.adminMu.RUnlock()
	if pass != "" {
		return hmac.Equal([]byte(candidate), []byte(pass))
	}
	if hash != "" {
		return bcrypt.CompareHashAndPassword([]byte(hash), []byte(candidate)) == nil
	}
	return false
}

// handleChangePassword replaces the panel password. Once set here it wins over
// ZCODE_ADMIN_PASSWORD, and every other panel session is dropped.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	var body struct {
		Current string `json:"current"`
		Next    string `json:"next"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, 400, "invalid json")
		return
	}
	if !s.passwordMatches(strings.TrimSpace(body.Current)) {
		writeError(w, 401, "当前密码错误")
		return
	}
	next := strings.TrimSpace(body.Next)
	if len(next) < 8 {
		writeError(w, 400, "新密码至少 8 位")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.store.UpdateSettings(func(st *Settings) {
		st.AdminPasswordHash = string(hash)
		st.AdminPasswordManual = true
	})
	s.setPasswordState("", string(hash), false)
	if cookie, err := r.Cookie(adminCookieName); err == nil && s.store.db != nil {
		s.store.db.DeleteAdminSessionsExcept(cookie.Value)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "source": s.passwordSource()})
}

func (s *Server) newAdminSession(w http.ResponseWriter) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	now := time.Now().Unix()
	if s.store.db != nil {
		if err := s.store.db.CreateAdminSession(token, now, now+adminSessionTTL); err != nil {
			return "", err
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   adminSessionTTL,
	})
	return token, nil
}

func (s *Server) adminAuthed(r *http.Request) bool {
	cookie, err := r.Cookie(adminCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	if s.store.db == nil {
		return false
	}
	return s.store.db.AdminSessionValid(cookie.Value, time.Now().Unix())
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, 400, "invalid json")
		return
	}
	if !s.passwordMatches(strings.TrimSpace(body.Password)) {
		writeError(w, 401, "密码错误")
		return
	}
	if _, err := s.newAdminSession(w); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(adminCookieName); err == nil && s.store.db != nil {
		s.store.db.DeleteAdminSession(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: adminCookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"authenticated": s.adminAuthed(r)})
}

// requireAdmin guards every panel API and the SPA shell.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.adminAuthed(r) {
			writeError(w, 401, "not authenticated")
			return
		}
		next(w, r)
	}
}
