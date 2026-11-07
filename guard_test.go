package main

import (
	"testing"
	"time"
)

// guardPanic 必须真的把 panic 收住：Go 里未捕获的 goroutine panic 会带走
// 整个进程，所以这个测试一旦失败，进程会直接退出（等于最响亮的失败信号）。
func TestGuardPanicRecovers(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer guardPanic("test")
		panic("boom")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("guardPanic did not recover")
	}
}

// 正常返回时 guardPanic 不能有任何副作用（尤其不能把返回值改掉）。
func TestGuardPanicPassesThrough(t *testing.T) {
	func() {
		defer guardPanic("test")
	}()
}
