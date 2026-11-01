package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"log"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"
)

// 账号导入导出：把账号连同凭据打包成一个口令加密的文件，方便备份和换机。
//
// 为什么不用 data/accounts.json 直接拷：那份文件里的凭据是用本机 master.key
// 加密的，换台机器就解不开。这里改成用用户口令派生密钥，跟机器无关。

const (
	portableFormat  = "zcode-gateway-accounts"
	portableVersion = 1
	portableKDF     = "scrypt-n32768-r8-p1"
	portableMinPass = 8
)

type portableAccount struct {
	Label           string           `json:"label"`
	Provider        string           `json:"provider"`
	Mode            string           `json:"mode"`
	User            UserInfo         `json:"user"`
	Project         *ProjectLocation `json:"project,omitempty"`
	Priority        int              `json:"priority,omitempty"`
	Enabled         bool             `json:"enabled"`
	CreatedAt       int64            `json:"createdAt,omitempty"`
	ZcodeJWT        string           `json:"zcodeJwt,omitempty"`
	AccessToken     string           `json:"accessToken,omitempty"`
	ProjectToken    string           `json:"projectToken,omitempty"`
	ProjectTokenExp int64            `json:"projectTokenExp,omitempty"`
}

type portablePayload struct {
	Accounts []portableAccount `json:"accounts"`
}

type portableBundle struct {
	Format     string `json:"format"`
	Version    int    `json:"version"`
	CreatedAt  int64  `json:"createdAt"`
	KDF        string `json:"kdf"`
	Salt       string `json:"salt"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
	Accounts   int    `json:"accounts"` // 明文计数，方便导出后一眼看出带了多少个
}

func sealPortable(payload portablePayload, passphrase string) (portableBundle, error) {
	plain, err := json.Marshal(payload)
	if err != nil {
		return portableBundle{}, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return portableBundle{}, err
	}
	key, err := scrypt.Key([]byte(passphrase), salt, 32768, 8, 1, 32)
	if err != nil {
		return portableBundle{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return portableBundle{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return portableBundle{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return portableBundle{}, err
	}
	sealed := gcm.Seal(nil, nonce, plain, nil)
	return portableBundle{
		Format:     portableFormat,
		Version:    portableVersion,
		CreatedAt:  time.Now().Unix(),
		KDF:        portableKDF,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(sealed),
		Accounts:   len(payload.Accounts),
	}, nil
}

func openPortable(bundle portableBundle, passphrase string) (portablePayload, error) {
	var payload portablePayload
	if bundle.Format != portableFormat {
		return payload, errors.New("不是本网关导出的备份文件")
	}
	if bundle.Version != portableVersion {
		return payload, fmt.Errorf("备份版本 %d 不受支持", bundle.Version)
	}
	salt, err := base64.StdEncoding.DecodeString(bundle.Salt)
	if err != nil {
		return payload, errors.New("备份文件损坏：salt")
	}
	nonce, err := base64.StdEncoding.DecodeString(bundle.Nonce)
	if err != nil {
		return payload, errors.New("备份文件损坏：nonce")
	}
	sealed, err := base64.StdEncoding.DecodeString(bundle.Ciphertext)
	if err != nil {
		return payload, errors.New("备份文件损坏：ciphertext")
	}
	key, err := scrypt.Key([]byte(passphrase), salt, 32768, 8, 1, 32)
	if err != nil {
		return payload, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return payload, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return payload, err
	}
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return payload, errors.New("口令不对，或者备份文件被改过")
	}
	if err := json.Unmarshal(plain, &payload); err != nil {
		return payload, errors.New("备份内容无法解析")
	}
	return payload, nil
}

func (s *Server) handleExportAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	var body struct {
		Passphrase string `json:"passphrase"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, 400, "invalid json")
		return
	}
	passphrase := strings.TrimSpace(body.Passphrase)
	if len(passphrase) < portableMinPass {
		writeError(w, 400, fmt.Sprintf("口令至少 %d 位", portableMinPass))
		return
	}
	payload := portablePayload{}
	for _, acc := range s.store.Accounts() {
		payload.Accounts = append(payload.Accounts, portableAccount{
			Label:           acc.Label,
			Provider:        acc.Provider,
			Mode:            acc.Mode,
			User:            acc.User,
			Project:         acc.Project,
			Priority:        acc.Priority,
			Enabled:         acc.Enabled,
			CreatedAt:       acc.CreatedAt,
			ZcodeJWT:        acc.ZcodeJWT,
			AccessToken:     acc.AccessToken,
			ProjectToken:    acc.ProjectToken,
			ProjectTokenExp: acc.ProjectTokenExp,
		})
	}
	if len(payload.Accounts) == 0 {
		writeError(w, 400, "没有可导出的账号")
		return
	}
	bundle, err := sealPortable(payload, passphrase)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	log.Printf("导出账号 %d 个（口令加密）", len(payload.Accounts))
	writeJSON(w, 200, bundle)
}

func (s *Server) handleRestoreAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	var body struct {
		Passphrase string          `json:"passphrase"`
		Bundle     json.RawMessage `json:"bundle"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, 400, "invalid json")
		return
	}
	if len(body.Bundle) == 0 {
		writeError(w, 400, "缺少备份内容")
		return
	}
	var bundle portableBundle
	if err := json.Unmarshal(body.Bundle, &bundle); err != nil {
		writeError(w, 400, "备份文件不是合法 JSON")
		return
	}
	payload, err := openPortable(bundle, strings.TrimSpace(body.Passphrase))
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	imported, skipped, invalid := 0, 0, 0
	for _, item := range payload.Accounts {
		provider := strings.TrimSpace(item.Provider)
		if provider == "" {
			provider = "zai"
		}
		// 以「provider + 上游用户 ID」判重：账号 ID 是本机随机生成的，换机后必然不同。
		if strings.TrimSpace(item.User.ID) == "" {
			invalid++
			continue
		}
		if existing := s.store.FindByUser(provider, item.User.ID); existing != nil {
			skipped++
			continue
		}
		acc := &Account{
			Label:           item.Label,
			Provider:        provider,
			Mode:            item.Mode,
			User:            item.User,
			Project:         item.Project,
			Priority:        item.Priority,
			Enabled:         item.Enabled,
			CreatedAt:       item.CreatedAt,
			ZcodeJWT:        item.ZcodeJWT,
			AccessToken:     item.AccessToken,
			ProjectToken:    item.ProjectToken,
			ProjectTokenExp: item.ProjectTokenExp,
			Models:          map[string]*UsageTotals{},
		}
		if acc.Mode == "" {
			acc.Mode = "start-plan"
		}
		if err := s.store.AddAccount(acc); err != nil {
			invalid++
			continue
		}
		imported++
	}
	log.Printf("导入账号：新增 %d，已存在 %d，无效 %d", imported, skipped, invalid)
	writeJSON(w, 200, map[string]any{
		"imported": imported, "skipped": skipped, "invalid": invalid,
		"items": s.store.Summaries(),
	})
}
