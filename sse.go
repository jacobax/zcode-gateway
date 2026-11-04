package main

import (
	"bufio"
	"context"
	"io"
	"log"
	"strings"
	"sync/atomic"
	"time"
)

// closeBodyOnDone 在 ctx 结束（客户端断开）时关掉上游 body，让阻塞中的读
// 立刻返回。否则一条没人读的连接会一直占着账号槽位。
func closeBodyOnDone(ctx context.Context, body io.ReadCloser) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = body.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// pumpSSE 把上游 SSE 逐行读出来交给 sink，三条流式路径（Anthropic 直通、
// OpenAI 兼容、Responses 兼容）共用同一套超时与计时。
//
// 超时有两道：首字（默认 180s）和流空闲（默认 120s）。整轮的 requestTimeout
// （默认 600s）管不住「上游接了请求但一直不出字」和「出到一半卡住」这两种
// 情况——客户端会干等十分钟。超时直接关掉上游 body，让阻塞中的 ReadString
// 返回错误退出。
//
// sink 负责把这一行转成客户端要的形状并写出；返回的 produced 表示这行是否
// 产出了内容 token（用于首字计时和空闲计时重置）。
func pumpSSE(ctx context.Context, body io.ReadCloser, start time.Time, label string, sink func(line string) (bool, error)) (streamTiming, int, string) {
	firstTimeout := envSecondsAllowZero("ZCODE_FIRST_TOKEN_TIMEOUT_SEC", 180)
	idleTimeout := envSecondsAllowZero("ZCODE_STREAM_IDLE_TIMEOUT_SEC", 120)
	var firstAt atomic.Int64 // UnixNano，0 = 还没出字
	var timedOut atomic.Bool
	closeForTimeout := func() {
		timedOut.Store(true)
		_ = body.Close()
	}
	if firstTimeout > 0 {
		timer := time.AfterFunc(firstTimeout, func() {
			if firstAt.Load() == 0 {
				closeForTimeout()
			}
		})
		defer timer.Stop()
	}
	var idleTimer *time.Timer
	if idleTimeout > 0 {
		idleTimer = time.AfterFunc(idleTimeout, closeForTimeout)
		defer idleTimer.Stop()
	}

	reader := bufio.NewReaderSize(body, 32*1024)
	var timing streamTiming
	var preview strings.Builder
	var firstTokenAt, lastTokenAt time.Time
	total := 0
	for {
		if ctx.Err() != nil {
			break
		}
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			total += len(line)
			if preview.Len() < 8192 {
				preview.WriteString(line)
			}
			produced, werr := sink(line)
			if produced {
				now := time.Now()
				if firstTokenAt.IsZero() {
					firstTokenAt = now
					firstAt.Store(now.UnixNano())
				}
				lastTokenAt = now
				if idleTimer != nil {
					idleTimer.Reset(idleTimeout)
				}
			}
			if werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	if timedOut.Load() {
		log.Printf("stream timeout (%s): first=%s idle=%s bytes=%d firstToken=%v",
			label, firstTimeout, idleTimeout, total, !firstTokenAt.IsZero())
	}
	if !firstTokenAt.IsZero() {
		timing.firstTokenMs = firstTokenAt.Sub(start).Milliseconds()
		timing.lastTokenMs = lastTokenAt.Sub(start).Milliseconds()
	}
	return timing, total, truncate(preview.String(), 8192)
}
