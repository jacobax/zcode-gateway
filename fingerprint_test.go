package main

import (
	"strings"
	"testing"
)

// officialSystemJSON 是手抄的一整段 JSON 字面量，解析失败会让网关静默给上游
// 发一个空 system prompt（直接吃 3012）。这里把它钉住。
func TestOfficialSystemBlocksParse(t *testing.T) {
	if len(officialSystemBlocks) < 2 {
		t.Fatalf("officialSystemJSON should parse into at least 2 blocks, got %d", len(officialSystemBlocks))
	}
	if got := blockText(officialSystemBlocks[0]); got != SystemHead {
		t.Fatalf("first block should be the system head, got %q", got)
	}
	if !hasFingerprint(officialSystemBlocks) {
		t.Fatal("official blocks should be recognised as a fingerprint")
	}
	if clone := cloneOfficialSystem(); len(clone) != len(officialSystemBlocks) {
		t.Fatalf("clone should keep every block: %d vs %d", len(clone), len(officialSystemBlocks))
	}
}

// 三块形态：只有 [head, prefix] 时补第三块，且不能抄 "# Environment"
// （那里是别人的工作目录/平台，抄过来等于骗模型）。
func TestWithThreeSystemBlocks(t *testing.T) {
	head := map[string]any{"type": "text", "text": SystemHead}
	prefix := map[string]any{"type": "text", "text": SystemPrefix}
	client := map[string]any{"type": "text", "text": "client system"}

	got := withThreeSystemBlocks([]any{head, prefix})
	if len(got) != 3 {
		t.Fatalf("want 3 blocks, got %d", len(got))
	}
	tail := blockText(got[2])
	if tail == "" {
		t.Fatal("third block should not be empty")
	}
	if strings.Contains(tail, "# Environment") || strings.Contains(tail, "Platform:") {
		t.Fatalf("third block must not leak a fake environment: %q", tail[:80])
	}

	// 已经有 3 块（客户端 system 在末块）就原样返回。
	got = withThreeSystemBlocks([]any{head, prefix, client})
	if len(got) != 3 || blockText(got[2]) != "client system" {
		t.Fatalf("existing 3 blocks must be kept: %#v", got)
	}
}

// 形态开关默认关：开了才改请求体，避免默认行为漂移。
func TestSystemShapeSwitch(t *testing.T) {
	t.Setenv("ZCODE_SYSTEM_SHAPE", "")
	if systemShapeThreeBlocks() {
		t.Fatal("shape switch must default off")
	}
	t.Setenv("ZCODE_SYSTEM_SHAPE", "3")
	if !systemShapeThreeBlocks() {
		t.Fatal("ZCODE_SYSTEM_SHAPE=3 should enable the 3-block shape")
	}
}
