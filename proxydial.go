package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"golang.org/x/net/proxy"
)

// tlsInsecureDialer 服务带 ?tls_insecure=1 的 https 代理条目。
//
// 为什么存在：tls-client v1.17.1 的库存 connectDialer 对 https 代理腿用
// tls.Dialer + tls.Config{ServerName: 代理主机名} 严格校验证书，且**不读**
// insecureSkipVerify（该选项只进 newRoundTripper，只管上游 TLS）——自签且无
// IP SAN 的裸 IP https 代理（如家用宽带 DDNS + 自签）必然报
// "doesn't contain any IP SANs"。原来唯一的出路是本地 relay 垫一层
// （contrib/relay，OPERATIONS「出口 IP 与风控」）；connectDialer 未导出改不了，
// 但 v1.17.1 的 WithProxyDialerFactory（client_options.go:174）允许整只替换，
// 参考 opencode2api internal/httpx.ProxyTunnelDial（2026-11 已上线验证）。
//
// 语义（对照 connect.go newConnectDialer 的 https 分支）：代理腿 TLS 跳过校验，
// 其余保持——http/1.1 CONNECT、Proxy-Authorization 取 URL 凭据、透传
// connectHeaders。不设 NextProtos：协商结果为空走 http/1.1（库存拨号器会
// 谈 h2 并复用连接，单代理场景收益可忽略，不引入 http2.Transport 依赖）。
// 通往上游的 TLS 不经过这里：roundtripper 对返回的隧道连接原样做 uTLS
// Chrome_131 握手（roundtripper.go:687），指纹与直连完全一致。
type tlsInsecureDialer struct {
	proxy   *url.URL
	dialer  net.Dialer
	headers fhttp.Header
}

func (d *tlsInsecureDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *tlsInsecureDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host := d.proxy.Host
	if d.proxy.Port() == "" {
		host = net.JoinHostPort(d.proxy.Hostname(), "443")
	}
	conn, err := d.dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("proxy dial: %w", err)
	}
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         d.proxy.Hostname(),
		InsecureSkipVerify: true, // 该拨号器只服务 tls_insecure=1 的条目，见工厂里的守卫
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("proxy tls: %w", err)
	}
	req := "CONNECT " + address + " HTTP/1.1\r\nHost: " + address + "\r\n"
	// URL 凭据优先（normalizeProxyURL 保留 userinfo）；connectHeaders 若已带
	// 认证头则让位，避免重复头发给代理。
	if d.proxy.User != nil && d.headers.Get("Proxy-Authorization") == "" {
		pass, _ := d.proxy.User.Password()
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString(
			[]byte(d.proxy.User.Username()+":"+pass)) + "\r\n"
	}
	for k, vs := range d.headers {
		for _, v := range vs {
			req += k + ": " + v + "\r\n"
		}
	}
	if _, err := tlsConn.Write([]byte(req + "\r\n")); err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("proxy connect write: %w", err)
	}
	resp, err := fhttp.ReadResponse(bufio.NewReader(tlsConn), &fhttp.Request{Method: "CONNECT"})
	if err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("proxy connect read: %w", err)
	}
	if resp.StatusCode != 200 {
		tlsConn.Close()
		return nil, fmt.Errorf("proxy refused CONNECT: HTTP %d", resp.StatusCode)
	}
	return tlsConn, nil
}

// customProxyDialerFactory 是拨号器工厂的本体，只服务
// 「https + tls_insecure=1」的代理 URL——newTLSClient 只在这种 URL 上挂载它；
// 出现其它 URL 说明挂载逻辑出了错，宁可报错也不静默跳过证书校验。
//
// 注意：tls-client v1.17.1 禁止 WithProxyUrl 与工厂并存（client.go:160），
// 所以挂载必须走 flaggedProxyDialerFactory 闭包带 URL，不能设 WithProxyUrl。
func customProxyDialerFactory(proxyUrlStr string, timeout time.Duration,
	localAddr *net.TCPAddr, connectHeaders fhttp.Header, logger tls_client.Logger) (proxy.ContextDialer, error) {
	u, err := url.Parse(proxyUrlStr)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid proxy URL %q", proxyUrlStr)
	}
	if u.Scheme != "https" || u.Query().Get("tls_insecure") != "1" {
		return nil, fmt.Errorf("proxy dialer factory only serves https proxies with tls_insecure=1, got %q", proxyUrlStr)
	}
	d := net.Dialer{Timeout: timeout}
	if localAddr != nil {
		d.LocalAddr = localAddr
	}
	return &tlsInsecureDialer{proxy: u, dialer: d, headers: connectHeaders}, nil
}

// flaggedProxyDialerFactory 把指定代理 URL 捕获进工厂闭包。tls-client 把
// config.proxyUrl 原样传给工厂，但校验禁止两者并存，URL 只能从这里带入。
func flaggedProxyDialerFactory(proxyURL string) tls_client.ProxyDialerFactory {
	return func(_ string, timeout time.Duration, localAddr *net.TCPAddr,
		connectHeaders fhttp.Header, logger tls_client.Logger) (proxy.ContextDialer, error) {
		return customProxyDialerFactory(proxyURL, timeout, localAddr, connectHeaders, logger)
	}
}
