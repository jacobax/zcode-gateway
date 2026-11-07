package main

import "testing"

// 只透传白名单里的 beta：上游对不认识的 beta 会 400，整串透传风险太大。
func TestFilterAnthropicBeta(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"mid-conversation-system", "mid-conversation-system"},
		{"mid-conversation-system,unknown-beta", "mid-conversation-system"},
		{" unknown-beta , mid-conversation-system ", "mid-conversation-system"},
		{"totally-made-up", ""},
	}
	for _, c := range cases {
		if got := filterAnthropicBeta(c.in); got != c.want {
			t.Fatalf("filterAnthropicBeta(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
