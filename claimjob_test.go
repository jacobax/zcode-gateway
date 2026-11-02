package main

import "testing"

func TestNormalizeClaimWindow(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"19,20", "19,20", true},
		{" 19 , 20 ", "19,20", true},
		{"20,19", "19,20", true},    // 排序去重
		{"19-21", "19,20,21", true}, // 区间展开
		{"23-2", "0,1,2,23", true},  // 跨零点
		{"19,19,20", "19,20", true}, // 去重
		{"", "", true},              // 不限
		{"*", "", true},             // 不限
		{"24", "", false},           // 越界
		{"-1", "", false},           // 负数（会被当成区间解析失败）
		{"abc", "", false},          // 非数字
		{"19-", "", false},          // 缺右端
		{"19,,20", "19,20", true},   // 空段忽略
	}
	for _, c := range cases {
		got, ok := normalizeClaimWindow(c.in)
		if ok != c.ok {
			t.Fatalf("%q: ok=%v, want %v", c.in, ok, c.ok)
		}
		if ok && got != c.want {
			t.Fatalf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}
