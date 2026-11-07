package main

// 上游下发的端点映射。
//
// 实测（2026-11-03）GET /api/v1/agent/configs 返回：
//
//	{"code":0,"data":{"proxyEndpoint":{"mapping":[
//	  {"from":"https://open.bigmodel.cn/api/anthropic/v1/messages",
//	   "to":"https://zcode.z.ai/api/v1/ultra/anthropic/v1/messages",
//	   "zcodeAuthorizationHeader":"X-Zcode-Authorization"}]}}}
//
// 语义是「客户端配的 base URL → 实际要打的 URL」。我们原来把 ultra /
// ultra-zai 写死在 upstream.go 里，上游一改映射就全挂——这是抗变更的关键一层。
//
// 三条硬约束（照 TriDefender 的实证）：
//   - 严格 fail-open：抓取/解析失败保留旧快照，绝不因为拿不到映射就拒绝服务
//   - 只接受 https 目标：防止映射被降级成明文
//   - 条数上限：防止异常配置把内存撑爆

import (
	"context"
	"strings"
	"sync"
	"time"
)

const (
	endpointMapTTL        = 5 * time.Minute
	endpointMapRetry      = 30 * time.Second
	endpointMapTimeout    = 3 * time.Second
	endpointMapMaxEntries = 256
)

// 客户端配置里的逻辑端点。start-plan 不走这套（它不在客户端配置里，
// 上游也不会给它下发映射）。
const (
	logicalEndpointBigmodel = "https://open.bigmodel.cn/api/anthropic/v1/messages"
	logicalEndpointZai      = "https://api.z.ai/api/anthropic/v1/messages"
)

var (
	endpointMapMu         sync.RWMutex
	endpointMapCurrent    = map[string]string{}
	endpointMapNextAt     time.Time
	endpointMapFetchedAt  time.Time
	endpointMapLastErr    string
	endpointMapRefreshing bool
)

// resolveEndpoint 查映射表。没有映射时返回空串，调用方用自己算出来的默认端点。
func resolveEndpoint(logical string) string {
	if logical == "" {
		return ""
	}
	endpointMapMu.RLock()
	defer endpointMapMu.RUnlock()
	return endpointMapCurrent[logical]
}

// endpointMapStatus 给 /health 和面板看。
func endpointMapStatus() map[string]any {
	endpointMapMu.RLock()
	defer endpointMapMu.RUnlock()
	out := map[string]any{"entries": len(endpointMapCurrent)}
	if !endpointMapFetchedAt.IsZero() {
		out["fetchedAt"] = endpointMapFetchedAt.Unix()
	}
	if endpointMapLastErr != "" {
		out["error"] = endpointMapLastErr
	}
	return out
}

// ensureEndpointMap 在缓存过期时异步刷新一次，立即返回。
// 转发路径不能为了拿映射卡住请求。
func (s *Server) ensureEndpointMap(ctx context.Context, acc *Account) {
	if acc == nil {
		return
	}
	if time.Now().Before(endpointMapNextAt) {
		return
	}
	endpointMapMu.Lock()
	if endpointMapRefreshing {
		endpointMapMu.Unlock()
		return
	}
	endpointMapRefreshing = true
	endpointMapMu.Unlock()

	go func() {
		// panic 兜底：刷新失败只该留旧快照，不该把进程带走。
		defer guardPanic("endpointMap")
		defer func() {
			endpointMapMu.Lock()
			endpointMapRefreshing = false
			endpointMapMu.Unlock()
		}()
		callCtx, cancel := context.WithTimeout(context.Background(), endpointMapTimeout)
		defer cancel()
		mapping, err := s.fetchEndpointMap(callCtx, acc)
		endpointMapMu.Lock()
		defer endpointMapMu.Unlock()
		if err != nil {
			// fail-open：保留旧快照，30 秒后再试。
			endpointMapLastErr = err.Error()
			endpointMapNextAt = time.Now().Add(endpointMapRetry)
			return
		}
		if len(mapping) > 0 {
			endpointMapCurrent = mapping
			endpointMapFetchedAt = time.Now()
		}
		endpointMapLastErr = ""
		endpointMapNextAt = time.Now().Add(endpointMapTTL)
	}()
}

func (s *Server) fetchEndpointMap(ctx context.Context, acc *Account) (map[string]string, error) {
	parsed, _, err := s.zcodeAPICall(ctx, acc, "GET", "/api/v1/agent/configs", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	return parseEndpointMapping(parsed), nil
}

// parseEndpointMapping 从 agent/configs 的响应里抽出「逻辑端点 → 实际端点」表。
//
// 只接受 https 目标：映射是服务端下发的，理论上可信，但降级到明文的代价太大
// （JWT 裸奔），不值得为了「上游可能这么配」留口子。条数上限同理，防止异常
// 配置把内存撑爆。
func parseEndpointMapping(parsed map[string]any) map[string]string {
	data := activityMap(parsed["data"])
	proxy := activityMap(data["proxyEndpoint"])
	out := map[string]string{}
	for _, item := range activitySlice(proxy, "mapping") {
		entry := activityMap(item)
		from := strings.TrimSpace(activityString(entry, "from"))
		to := strings.TrimSpace(activityString(entry, "to"))
		if from == "" || to == "" {
			continue
		}
		if !strings.HasPrefix(to, "https://") {
			continue
		}
		if len(out) >= endpointMapMaxEntries {
			break
		}
		out[from] = to
	}
	return out
}
