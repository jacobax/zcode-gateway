package main

import "testing"

// accountOwnsAll 的跳过语义：拥有**全部**可领套餐才整号跳过。
// 改前是 accountOwnsAny（拥有任意一个就跳）——多礼包（magpie 上游插件
// #503：横幅/预览/可领列表可同时挂多个）下，领了 A 的号连 B 也领不到。
func TestAccountOwnsAllClaimable(t *testing.T) {
	acc := &Account{Quota: Quota{
		Plans:   []QuotaPlan{{PlanID: "gift-a"}},
		Pending: []QuotaPending{{PlanID: "gift-c"}},
	}}

	// 改前行为（any）：拥有 A → 连 B 一起跳过，B 永远轮不到。
	// 改后：A、B 都可领时不跳过，B 继续领。
	if accountOwnsAll(acc, map[string]bool{"gift-a": true, "gift-b": true}) {
		t.Fatal("拥有 A（和 pending C）但 B 可领：不应整号跳过")
	}
	// 单礼包场景省流优化不变：拥有 A、可领集合就是 {A} → 跳过。
	if !accountOwnsAll(acc, map[string]bool{"gift-a": true}) {
		t.Fatal("单礼包场景：拥有 A 就应跳过")
	}
	if accountOwnsAll(acc, map[string]bool{"gift-b": true}) {
		t.Fatal("不拥有 B：不应跳过")
	}
	// pending（已领未生效）也算拥有。
	if !accountOwnsAll(acc, map[string]bool{"gift-a": true, "gift-c": true}) {
		t.Fatal("A 已领 + C pending：应视为全部拥有")
	}
}

func TestNormalizeClaimWindow(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"19,20", "19,20", true},
		{" 19 , 20 ", "19,20", true},
		{"20,19", "19,20", true},                     // 排序去重
		{"19-21", "19,20,21", true},                  // 区间展开
		{"23-2", "0,1,2,23", true},                   // 跨零点
		{"19,19,20", "19,20", true},                  // 去重
		{"", "", true},                               // 留空 = 不领取
		{"*", "*", true},                             // 不限时段（留空才是「不领取」）
		{"24", "", false},                            // 越界
		{"-1", "", false},                            // 负数（会被当成区间解析失败）
		{"abc", "", false},                           // 非数字
		{"19-", "", false},                           // 缺右端
		{"19,,20", "19,20", true},                    // 空段忽略
		{"0 19-21 * * 1-5", "0 19-21 * * 1-5", true}, // 5 段 cron 原样保存
		{"*/10 9 * * *", "*/10 9 * * *", true},       // 带步长
		{"0 19-21 * *", "", false},                   // 4 段不是 cron，按旧格式解析失败
		{"bad 19 * * *", "", false},                  // 非法字符
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
