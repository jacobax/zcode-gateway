package main

// 活动/领取这一族接口上报的客户端版本。
//
// 上游按客户端版本下发活动：低于门槛（2026-11 实测是 3.17.1）时
// marketing/touch 回 deliveries:[]、领取回 1004「暂不满足领取条件」，
// 达到门槛才有横幅、领取才 200。写死一个版本号，上游每发一版就要改一次代码，
// 所以这里从官网 /releases 页解析最新版本，缓存起来定期刷新。
//
// 取值优先级（高 → 低）：
//  1. 环境变量 ZCODE_CLAIM_APP_VERSION —— 显式指定，完全不联网，用来应急
//  2. /releases 解析到的最新版本 —— 6 小时刷新一次，失败 1 小时后重试
//  3. claimVersionFloor —— 内置兜底，保证从没联网成功过时也能用
//
// 只升不降：解析结果比当前值旧时忽略，避免页面改版/回滚把版本号带回去。
// 注意它和 appVersion 不是一回事：模型请求（/v1/messages 等）报新版本会被
// 上游判 3012，那边必须留在 3.16.3，两个版本号不能合并。

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// claimVersionFloor 是内置的活动客户端版本下限。上游门槛只会越来越高，
	// 这个值的作用是「联网失败时别退回到会被判 1004 的版本」。
	claimVersionFloor = "3.17.1"

	claimVersionURL     = "https://zcode.z.ai/releases"
	claimVersionTTL     = 6 * time.Hour
	claimVersionRetry   = time.Hour
	claimVersionTimeout = 25 * time.Second
	claimVersionMaxBody = 8 << 20
)

var (
	claimVersionOverride = strings.TrimSpace(os.Getenv("ZCODE_CLAIM_APP_VERSION"))
	claimVersionValue    atomic.Value // string
	claimVersionNextAt   atomic.Int64 // unix 秒；到点才允许再拉
	claimVersionLastErr  atomic.Value // string
	claimVersionFetched  atomic.Int64 // unix 秒；最近一次成功解析的时间

	claimVersionMu         sync.Mutex
	claimVersionRefreshing bool
)

// releaseVersionPatterns 从 /releases 页面抠版本号。两条独立证据：
// 更新日志标题 `Release v3.17.2`，以及下载直链 `.../releases/3.17.2/macos-arm64/...`。
// 页面结构改版时只要有一条还在就能继续工作。
var releaseVersionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`Release\s+v(\d+\.\d+\.\d+)`),
	regexp.MustCompile(`/releases/(\d+\.\d+\.\d+)/`),
}

func init() {
	claimVersionValue.Store(claimVersionFloor)
	claimVersionLastErr.Store("")
	if claimVersionOverride != "" {
		claimVersionValue.Store(claimVersionOverride)
		// 显式指定 = 用户接管，永不自动刷新。
		claimVersionNextAt.Store(1 << 62)
	}
}

// claimAppVersion 返回当前要上报给活动/领取接口的客户端版本。
func claimAppVersion() string {
	return claimVersionValue.Load().(string)
}

// claimVersionStatus 给面板/健康检查看：当前版本、来源、最近错误。
func claimVersionStatus() map[string]any {
	out := map[string]any{
		"version": claimAppVersion(),
		"source":  "floor",
	}
	switch {
	case claimVersionOverride != "":
		out["source"] = "env"
	case claimVersionFetched.Load() > 0:
		out["source"] = "releases"
	}
	if at := claimVersionFetched.Load(); at > 0 {
		out["fetchedAt"] = at
	}
	if msg, _ := claimVersionLastErr.Load().(string); msg != "" {
		out["error"] = msg
	}
	return out
}

// ensureClaimVersion 在缓存过期时异步刷新一次，立即返回，不阻塞调用方。
// 活动探测是热路径，不能为了一个版本号卡住。
func ensureClaimVersion() {
	if claimVersionOverride != "" {
		return
	}
	if time.Now().Unix() < claimVersionNextAt.Load() {
		return
	}
	claimVersionMu.Lock()
	if claimVersionRefreshing {
		claimVersionMu.Unlock()
		return
	}
	claimVersionRefreshing = true
	claimVersionMu.Unlock()

	go func() {
		// panic 兜底：拉版本号失败最多是版本旧一点，不该把进程带走。
		defer guardPanic("claimVersion")
		defer func() {
			claimVersionMu.Lock()
			claimVersionRefreshing = false
			claimVersionMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), claimVersionTimeout)
		defer cancel()
		version, err := fetchLatestClaimVersion(ctx)
		if err != nil {
			claimVersionLastErr.Store(err.Error())
			claimVersionNextAt.Store(time.Now().Add(claimVersionRetry).Unix())
			return
		}
		current := claimAppVersion()
		if compareVersions(version, current) > 0 {
			claimVersionValue.Store(version)
			log.Printf("活动客户端版本更新：%s → %s（来自 %s）", current, version, claimVersionURL)
		}
		claimVersionLastErr.Store("")
		claimVersionFetched.Store(time.Now().Unix())
		claimVersionNextAt.Store(time.Now().Add(claimVersionTTL).Unix())
	}()
}

// fetchLatestClaimVersion 拉 /releases 页面，取其中最大的版本号。
func fetchLatestClaimVersion(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claimVersionURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ZCode/"+appVersion)
	req.Header.Set("Accept", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("拉取 %s 失败: %w", claimVersionURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("拉取 %s 返回 HTTP %d", claimVersionURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, claimVersionMaxBody))
	if err != nil {
		return "", fmt.Errorf("读取 %s 失败: %w", claimVersionURL, err)
	}
	best := pickLatestReleaseVersion(string(body))
	if best == "" {
		return "", fmt.Errorf("%s 页面里没找到版本号（页面结构可能变了）", claimVersionURL)
	}
	return best, nil
}

// pickLatestReleaseVersion 从 /releases 页面里挑出最大的版本号。
// 抽出来是为了能脱网测：页面结构改版时单测会先炸，而不是等线上领不到才发现。
func pickLatestReleaseVersion(page string) string {
	best := ""
	for _, pattern := range releaseVersionPatterns {
		for _, match := range pattern.FindAllStringSubmatch(page, -1) {
			if len(match) < 2 {
				continue
			}
			if compareVersions(match[1], best) > 0 {
				best = match[1]
			}
		}
	}
	return best
}

// compareVersions 比较两个 major.minor.patch 版本号，返回 -1 / 0 / 1。
// 解析不出来的段按 0 处理，够用且不会 panic。
func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := range pa {
		switch {
		case pa[i] < pb[i]:
			return -1
		case pa[i] > pb[i]:
			return 1
		}
	}
	return 0
}

func versionParts(v string) [3]int {
	var out [3]int
	for i, part := range strings.SplitN(strings.TrimSpace(v), ".", 3) {
		if i > 2 {
			break
		}
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 0 {
			continue
		}
		out[i] = n
	}
	return out
}
