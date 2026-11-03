package main

import "testing"

// 版本号只升不降：解析结果比当前值旧时忽略。compareVersions 是这条规则的实现。
func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"3.17.2", "3.17.1", 1},
		{"3.17.1", "3.17.1", 0},
		{"3.16.3", "3.17.1", -1},
		{"3.18.0", "3.17.9", 1},
		{"4.0.0", "3.99.99", 1},
		{"", "3.17.1", -1},
		{"3.17", "3.17.0", 0},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// /releases 页面的两条版本号来源：更新日志标题和下载直链。
// 只要还有一条能匹配上，动态版本号就不会退化成内置下限。
func TestPickLatestReleaseVersion(t *testing.T) {
	page := `<h2 class="text-3xl">Release v3.17.2</h2>
<span class="font-mono text-sm">3.17.2</span>
<a href="https://cdn-zcode.z.ai/zcode/electron/releases/3.17.2/macos-arm64/ZCode-3.17.2-mac-arm64.dmg">dmg</a>
<h2 class="text-3xl">Release v3.16.3</h2>
<a href="https://cdn-zcode.z.ai/zcode/electron/releases/3.16.3/win-x64/ZCode-3.16.3-win-x64.exe">exe</a>`
	if got := pickLatestReleaseVersion(page); got != "3.17.2" {
		t.Fatalf("pickLatestReleaseVersion = %q, want 3.17.2", got)
	}

	// 只有下载直链、没有 "Release v" 标题时也要能解析出来。
	onlyLinks := `<a href="https://cdn-zcode.z.ai/zcode/electron/releases/3.18.0/macos-arm64/ZCode.dmg">dmg</a>
<a href="https://cdn-zcode.z.ai/zcode/electron/releases/3.17.2/macos-x64/ZCode.dmg">dmg</a>`
	if got := pickLatestReleaseVersion(onlyLinks); got != "3.18.0" {
		t.Fatalf("only links: got %q, want 3.18.0", got)
	}

	// 页面改版到两条都匹配不上时返回空串，调用方据此保留旧值。
	if got := pickLatestReleaseVersion("<html>nothing here</html>"); got != "" {
		t.Fatalf("empty page: got %q, want empty", got)
	}
}

// 内置下限必须能被比较函数识别，且是合法版本号。
func TestClaimVersionFloor(t *testing.T) {
	if compareVersions(claimVersionFloor, "0.0.0") <= 0 {
		t.Fatalf("claimVersionFloor = %q, want a positive version", claimVersionFloor)
	}
	if got := versionParts(claimVersionFloor); got[0] < 3 {
		t.Fatalf("claimVersionFloor major = %d, want >= 3", got[0])
	}
}
