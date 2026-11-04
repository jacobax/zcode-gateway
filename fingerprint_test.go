package main

import "testing"

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
