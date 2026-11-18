package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
)

// selfSignedNoIPSAN 生成只带 DNSNames ["proxy.invalid"] 的自签证书——
// 故意不给 IP SAN，让「裸 IP 代理腿 + 严格校验」必然失败
// （x509: cannot validate certificate for ... because it doesn't contain any IP SANs）。
func selfSignedNoIPSAN(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "proxy.invalid"},
		DNSNames:     []string{"proxy.invalid"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	return cert
}

type authRecorder struct {
	mu   sync.Mutex
	last string
}

func (r *authRecorder) set(v string) { r.mu.Lock(); r.last = v; r.mu.Unlock() }
func (r *authRecorder) get() string  { r.mu.Lock(); defer r.mu.Unlock(); return r.last }

// fakeSelfSignedProxy 起一个自签 https CONNECT 代理：记录每条 CONNECT 的
// Proxy-Authorization，回 200 后裸转发到 target。
func fakeSelfSignedProxy(t *testing.T, targetURL string) (addr string, rec *authRecorder) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	rec = &authRecorder{}
	cert := selfSignedNoIPSAN(t)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveProxyConn(tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}}), targetURL, rec)
		}
	}()
	return ln.Addr().String(), rec
}

// serveProxyConn 处理一条代理连接：读 CONNECT → 记认证头 → 回 200 → 双向转发。
// 代理腿的错误只丢连接不上报——断言全部落在客户端行为上。
func serveProxyConn(conn net.Conn, targetURL string, rec *authRecorder) {
	defer conn.Close()
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil || req.Method != "CONNECT" {
		return
	}
	rec.set(req.Header.Get("Proxy-Authorization"))
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}
	u, err := url.Parse(targetURL)
	if err != nil {
		return
	}
	upstream, err := net.Dial("tcp", u.Host)
	if err != nil {
		return
	}
	defer upstream.Close()
	go io.Copy(conn, upstream)
	io.Copy(upstream, conn)
}

// 三态验证（对照 opencode2api 的验收做法）：
//  1. 阴性：不带 tls_insecure=1，库存 connectDialer 严格校验 → 必须以证书错误失败；
//  2. 正向：?tls_insecure=1 → 自定义拨号器跳过代理腿校验，隧道打通拿到目标响应；
//  3. 凭据：URL userinfo 翻成 Proxy-Authorization Basic 发给代理。
func TestProxyTLSInsecure(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("through-tunnel"))
	}))
	defer target.Close()

	addr, rec := fakeSelfSignedProxy(t, target.URL)

	t.Run("默认严格校验必须失败", func(t *testing.T) {
		c, err := newTLSClient("https://" + addr)
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		req, _ := fhttp.NewRequest("GET", target.URL, nil)
		_, err = c.Do(req)
		if err == nil {
			t.Fatal("裸 IP 自签代理 + 严格校验应该失败，却拿到了响应")
		}
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "x509") && !strings.Contains(msg, "certificate") {
			t.Fatalf("失败原因应是证书校验，得到: %v", err)
		}
	})

	t.Run("tls_insecure=1 打通隧道", func(t *testing.T) {
		c, err := newTLSClient("https://" + addr + "?tls_insecure=1")
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		req, _ := fhttp.NewRequest("GET", target.URL, nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("flag 开着隧道应该打通: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !strings.Contains(string(body), "through-tunnel") {
			t.Fatalf("目标响应不对: status=%d body=%q", resp.StatusCode, body)
		}
	})

	t.Run("URL 凭据变成 Proxy-Authorization", func(t *testing.T) {
		c, err := newTLSClient("https://user:secret@" + addr + "?tls_insecure=1")
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		req, _ := fhttp.NewRequest("GET", target.URL, nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("带凭据隧道应该打通: %v", err)
		}
		resp.Body.Close()
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:secret"))
		if got := rec.get(); got != want {
			t.Fatalf("Proxy-Authorization = %q, want %q", got, want)
		}
	})

	// 工厂守卫：不带 flag 的 URL 绝不能被它接管（防挂载逻辑回归）。
	t.Run("工厂拒绝非 flag 条目", func(t *testing.T) {
		_, err := customProxyDialerFactory("https://"+addr, time.Second, nil, nil, tls_client.NewNoopLogger())
		if err == nil || !strings.Contains(err.Error(), "tls_insecure") {
			t.Fatalf("非 flag 条目应被工厂拒绝，得到: %v", err)
		}
	})
}
