package main

import (
	"context"
	"testing"
	"time"
)

// pacing 的核心契约：一个进程内所有并发发送按「预约时刻」排队，平均间隔落在
// [min,max] 区间里，且不会因为并发而互相阻塞。
func TestPacerSpacesRequests(t *testing.T) {
	p := newPacer(50, 50) // 固定 50ms，便于断言
	if !p.enabled() {
		t.Fatal("newPacer(50,50) 应该启用")
	}
	start := time.Now()
	const n = 5
	for i := 0; i < n; i++ {
		if err := p.wait(context.Background()); err != nil {
			t.Fatalf("wait: %v", err)
		}
	}
	// 第 1 次立即发，之后 4 次各等 50ms
	elapsed := time.Since(start)
	want := time.Duration(n-1) * 50 * time.Millisecond
	if elapsed < want {
		t.Fatalf("5 次预约耗时 %v，应至少 %v", elapsed, want)
	}
	if elapsed > want+300*time.Millisecond {
		t.Fatalf("5 次预约耗时 %v，超出容差（want≈%v）", elapsed, want)
	}
}

// maxMS=0 表示关闭：关闭后不应有任何等待。
func TestPacerDisabledWhenZero(t *testing.T) {
	p := newPacer(0, 0)
	if p.enabled() {
		t.Fatal("maxMS=0 应视为关闭")
	}
	start := time.Now()
	for i := 0; i < 10; i++ {
		if err := p.wait(context.Background()); err != nil {
			t.Fatalf("wait: %v", err)
		}
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("关闭后 10 次调用耗时 %v，应该几乎为零", d)
	}
}

// ctx 取消要能提前退出，不能把请求卡在闸门上。
func TestPacerRespectsContext(t *testing.T) {
	p := newPacer(500, 500)
	if err := p.wait(context.Background()); err != nil { // 第一次立即通过
		t.Fatalf("first wait: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := p.wait(ctx)
	if err == nil {
		t.Fatal("ctx 到期后 wait 应返回错误")
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("ctx 取消后仍等了 %v", d)
	}
}

// 并发调用者不应串行化在锁上：预约是并行的，总耗时应接近 n*gap 而不是更长。
func TestPacerConcurrentReservations(t *testing.T) {
	p := newPacer(20, 20)
	start := time.Now()
	done := make(chan struct{})
	const n = 8
	for i := 0; i < n; i++ {
		go func() {
			_ = p.wait(context.Background())
			done <- struct{}{}
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
	elapsed := time.Since(start)
	want := time.Duration(n-1) * 20 * time.Millisecond
	if elapsed < want {
		t.Fatalf("%d 个并发预约耗时 %v，应至少 %v", n, elapsed, want)
	}
	if elapsed > want+400*time.Millisecond {
		t.Fatalf("%d 个并发预约耗时 %v，明显超出（want≈%v）", n, elapsed, want)
	}
}

// 随机间隔必须真的落在区间内（含端点）。
func TestPacerGapRange(t *testing.T) {
	p := newPacer(100, 200)
	for i := 0; i < 200; i++ {
		g := p.randomGap()
		if g < 100*time.Millisecond || g > 200*time.Millisecond {
			t.Fatalf("gap %v 越界 [100ms,200ms]", g)
		}
	}
}
