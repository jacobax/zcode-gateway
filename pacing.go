package main

// 全局发送节奏（pacing）。
//
// 为什么需要：上游边缘（阿里云 WAF）拦的是「一个出口 IP 零间隔猛冲」这种流量
// 形态。2026-11-22 实测——aws3 用 10 个账号、2 路并发跑 10 分钟就打出 2717 次
// WAF 405，3 路起客户端 100% 收 405：网关这边请求是一个接一个零间隔发出去的，
// 正好是边缘规则盯的形态。
//
// 做法照抄 AutoZcoder 的 upstream.pacing（B3hnamR/AutoZcoder，MIT）：
// 一个进程级共享的随机间隔闸门，每个真正发往上游的请求先「预约」一个发送时刻，
// 再各自等到那时。间隔随机而不是固定，是为了避免并发请求自同步成整齐的节拍
// （它注释原话：randomized so parallel requests don't self-synchronize into lockstep）。
//
// 用「预约时刻」而不是「锁内 sleep」：并发调用者不会互相阻塞在锁上，ctx 取消
// 也能立刻返回，不会拖住释放。

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

type pacer struct {
	minGap time.Duration
	maxGap time.Duration

	mu   sync.Mutex
	next time.Time // 下一个可用发送时刻；零值表示还没发过
}

// newPacer 用毫秒构造。maxMS <= 0 表示关闭。
func newPacer(minMS, maxMS int) *pacer {
	if minMS < 0 {
		minMS = 0
	}
	if maxMS < minMS {
		maxMS = minMS
	}
	return &pacer{
		minGap: time.Duration(minMS) * time.Millisecond,
		maxGap: time.Duration(maxMS) * time.Millisecond,
	}
}

func (p *pacer) enabled() bool {
	return p != nil && p.maxGap > 0
}

// wait 预约一个发送时刻并等到那时。ctx 取消则提前返回 ctx.Err()。
func (p *pacer) wait(ctx context.Context) error {
	if !p.enabled() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	now := time.Now()
	at := p.next
	if at.Before(now) {
		at = now
	}
	p.next = at.Add(p.randomGap())
	p.mu.Unlock()

	d := time.Until(at)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *pacer) randomGap() time.Duration {
	if p.maxGap <= p.minGap {
		return p.minGap
	}
	// math/rand 的顶层函数在并发下安全；不需要自己加锁。
	return p.minGap + time.Duration(rand.Int63n(int64(p.maxGap-p.minGap)))
}
