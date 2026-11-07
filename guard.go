package main

import (
	"log"
	"runtime/debug"
)

// guardPanic 兜住后台 goroutine 的 panic。
//
// 为什么必须有：Go 里任何一个 goroutine 的未捕获 panic 都会让整个进程退出，
// 后台任务（刷额度、拉模型清单、领取）出一次越界就把所有人的请求全打断，
// 代价远大于丢掉一轮后台任务。这里只记日志，不重试——重试逻辑在各任务内部。
//
// 用法：在 goroutine 第一行 defer guardPanic("refreshQuota")。
func guardPanic(what string) {
	if r := recover(); r != nil {
		log.Printf("%s panic: %v\n%s", what, r, debug.Stack())
	}
}
