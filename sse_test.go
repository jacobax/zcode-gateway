package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

// stallBody 模拟「上游接了请求、发完响应头就再也不出字节」。
type stallBody struct {
	ctx  context.Context
	once sync.Once
	ch   chan struct{}
}

func newStallBody(ctx context.Context) *stallBody {
	return &stallBody{ctx: ctx, ch: make(chan struct{})}
}

func (b *stallBody) Read([]byte) (int, error) {
	select {
	case <-b.ch:
		return 0, io.EOF
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
}

func (b *stallBody) Close() error {
	b.once.Do(func() { close(b.ch) })
	return nil
}

// runStalledStream 跑一个渲染器，返回它阻塞了多久。
func runStalledStream(t *testing.T, fn func(ctx context.Context, w http.ResponseWriter, resp *fhttp.Response, start time.Time)) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp := &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: newStallBody(ctx)}
	resp.Header.Set("content-type", "text/event-stream")
	w := httptest.NewRecorder()
	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(ctx, w, resp, start)
	}()
	select {
	case <-done:
		return time.Since(start)
	case <-time.After(10 * time.Second):
		t.Fatal("stream renderer never returned")
		return 0
	}
}

// 三个流式端点必须共用同一套首字超时：上游卡死时谁都不该干等到整轮
// requestTimeout（默认 600s）。这里把预算压到 1s，三者都应该 1s 左右返回。
func TestStreamPathsHonourFirstTokenTimeout(t *testing.T) {
	t.Setenv("ZCODE_FIRST_TOKEN_TIMEOUT_SEC", "1")
	t.Setenv("ZCODE_STREAM_IDLE_TIMEOUT_SEC", "1")
	server := &Server{}
	cases := []struct {
		name string
		fn   func(ctx context.Context, w http.ResponseWriter, resp *fhttp.Response, start time.Time)
	}{
		{"anthropic", func(ctx context.Context, w http.ResponseWriter, resp *fhttp.Response, start time.Time) {
			server.writeUpstream(ctx, w, resp, true, start)
		}},
		{"openai", func(ctx context.Context, w http.ResponseWriter, resp *fhttp.Response, start time.Time) {
			server.writeUpstreamOpenAI(ctx, w, resp, true, "GLM-5.3", start)
		}},
		{"responses", func(ctx context.Context, w http.ResponseWriter, resp *fhttp.Response, start time.Time) {
			server.writeUpstreamResponses(ctx, w, resp, true, "GLM-5.3", start)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			elapsed := runStalledStream(t, tc.fn)
			if elapsed > 3*time.Second {
				t.Fatalf("stalled upstream should be cut off after ~1s, took %v", elapsed)
			}
		})
	}
}

func TestUpstreamBase(t *testing.T) {
	t.Setenv("ZCODE_UPSTREAM_BASE", "")
	if got := upstreamBase(); got != "https://zcode.z.ai" {
		t.Fatalf("default base: %q", got)
	}
	t.Setenv("ZCODE_UPSTREAM_BASE", "https://mirror.example/")
	if got := upstreamBase(); got != "https://mirror.example" {
		t.Fatalf("override should drop the trailing slash: %q", got)
	}
}
