package main

import (
	"testing"
)

// withCfg 在测试期间替换全局配置，结束后恢复，避免用例互相污染。
func withCfg(t *testing.T, c Config) {
	t.Helper()
	old := cfg
	t.Cleanup(func() { cfg = old })
	cfg = c
}

func cfgWith(targets, never []string) Config {
	c := defaultConfig()
	c.TargetSSIDs = targets
	c.NeverProxySSIDs = never
	return c
}

func TestMatchOne(t *testing.T) {
	withCfg(t, cfgWith([]string{"MyPhone-5G"}, nil))
	cases := []struct {
		name, ssid, target string
		want               bool
	}{
		{"exact", "MyPhone-5G", "MyPhone-5G", true},
		{"normalized", "  myphone 5g ", "MyPhone-5G", true},
		{"short prefix", "MyPhone", "MyPhone-5G", true},
		{"prefix reversed", "MyPhone-5G", "MyPhone", true},
		{"too short", "Home", "HomePhone", false},
		{"length gap", "MyPhone", "MyPhone-Router-Plus", false},
		{"unrelated", "Campus-WiFi", "MyPhone", false},
		{"empty ssid", "", "MyPhone", false},
		{"empty target", "MyPhone", "", false},
	}
	for _, tc := range cases {
		if got := matchOne(tc.ssid, tc.target); got != tc.want {
			t.Errorf("%s: matchOne(%q, %q) = %v, want %v", tc.name, tc.ssid, tc.target, got, tc.want)
		}
	}
}

func TestMatchOneStrictMode(t *testing.T) {
	c := cfgWith([]string{"MyPhone-5G"}, nil)
	c.FuzzyMatch = false
	withCfg(t, c)
	if matchOne("MyPhone", "MyPhone-5G") {
		t.Fatal("fuzzy_match=false 时不应接受前缀匹配")
	}
	if !matchOne("myphone-5g", "MyPhone-5G") {
		t.Fatal("fuzzy_match=false 时仍应接受归一化后的完全相等")
	}
}

func TestMatchOneBoundary(t *testing.T) {
	withCfg(t, cfgWith([]string{"abcdef"}, nil))
	if !matchOne("abcdef", "abcdefg") {
		t.Fatal("6 字节前缀、长度差 1 应命中")
	}
	if matchOne("abcde", "abcdefg") {
		t.Fatal("短的一方不足 6 字节不应命中")
	}
}

func TestMatchesMultipleTargets(t *testing.T) {
	withCfg(t, cfgWith([]string{"MyPhone-5G", "Home-WiFi"}, nil))
	if !matches("Home-Wi") {
		t.Fatal("应命中第二个目标网络的前缀")
	}
	if matches("Campus-WiFi") {
		t.Fatal("非目标网络不应命中")
	}
}

func TestIsNeverProxy(t *testing.T) {
	withCfg(t, cfgWith(nil, []string{"Campus-WiFi", "Home"}))
	cases := []struct {
		ssid string
		want bool
	}{
		{"Campus-WiFi", true},
		{"campus wifi 5g", true},
		{"Campus-WiFi-2024-Guest", true}, // 禁止名单允许更长的前缀变体
		{"Home", true},
		{"HomePhone", false}, // 短名单只认精确，避免误伤
		{"Office", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isNeverProxy(tc.ssid); got != tc.want {
			t.Errorf("isNeverProxy(%q) = %v, want %v", tc.ssid, got, tc.want)
		}
	}
}

func TestIsNeverProxyEmptyList(t *testing.T) {
	withCfg(t, cfgWith([]string{"MyPhone"}, nil))
	if isNeverProxy("Campus-WiFi") {
		t.Fatal("空禁止名单不应命中任何网络")
	}
}

func TestEffectiveWant(t *testing.T) {
	withCfg(t, cfgWith([]string{"MyPhone-5G"}, []string{"MyPhone-5G"}))
	if effectiveWant("MyPhone-5G") {
		t.Fatal("同时命中目标和禁止名单时必须判 OFF（禁止优先）")
	}

	withCfg(t, cfgWith([]string{"MyPhone-5G"}, []string{"Office-WiFi"}))
	if !effectiveWant("MyPhone") {
		t.Fatal("只命中目标时应判 ON")
	}
	if effectiveWant("Campus-WiFi") {
		t.Fatal("非目标网络应判 OFF")
	}
}

func TestCleanSSIDList(t *testing.T) {
	got := cleanSSIDList([]string{" A ", "a", "A", "", "  ", "B"})
	want := []string{"A", "B"}
	if len(got) != len(want) {
		t.Fatalf("cleanSSIDList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cleanSSIDList = %v, want %v", got, want)
		}
	}
}

func TestParseConfigDefaults(t *testing.T) {
	c, err := parseConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.FuzzyMatch || !c.AutoConnect || !c.SyncVergeConfig || !c.ShowTray {
		t.Fatalf("默认布尔值丢失：%+v", c)
	}
	if c.ReconcileSeconds != 60 || c.AutoConnectSecs != 15 || c.LogMaxBytes != 512*1024 {
		t.Fatalf("默认数值错误：%+v", c)
	}
	if len(c.TargetSSIDs) != 0 {
		t.Fatalf("默认不应有目标网络：%v", c.TargetSSIDs)
	}
}

func TestParseConfigLegacyTarget(t *testing.T) {
	c, err := parseConfig([]byte(`{"target_ssid":"MyPhone","never_proxy_ssids":["Campus"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TargetSSIDs) != 1 || c.TargetSSIDs[0] != "MyPhone" {
		t.Fatalf("旧字段未并入 target_ssids：%v", c.TargetSSIDs)
	}
	if c.TargetSSID != "" {
		t.Fatal("写出前应清空旧字段，避免重复")
	}
	if len(c.NeverProxySSIDs) != 1 || c.NeverProxySSIDs[0] != "Campus" {
		t.Fatalf("禁止名单解析错误：%v", c.NeverProxySSIDs)
	}
}

func TestParseConfigDedupeAndClamp(t *testing.T) {
	raw := []byte(`{
		"target_ssid": "MyPhone",
		"target_ssids": ["myphone", "Home"],
		"reconcile_seconds": 5,
		"auto_connect_seconds": 1,
		"auto_connect_min_signal": 300,
		"log_max_bytes": 0
	}`)
	c, err := parseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TargetSSIDs) != 2 || c.TargetSSIDs[0] != "myphone" || c.TargetSSIDs[1] != "Home" {
		t.Fatalf("去重结果错误：%v", c.TargetSSIDs)
	}
	if c.ReconcileSeconds != 60 || c.AutoConnectSecs != 15 || c.AutoConnectMinSig != 0 || c.LogMaxBytes != 512*1024 {
		t.Fatalf("越界值未回落到默认：%+v", c)
	}
}

func TestParseConfigInvalid(t *testing.T) {
	if _, err := parseConfig([]byte(`{"target_ssids":`)); err == nil {
		t.Fatal("非法 JSON 应返回错误")
	}
}

func TestTargetDisplay(t *testing.T) {
	withCfg(t, cfgWith(nil, nil))
	if got := targetDisplay(); got != "未配置" {
		t.Fatalf("空目标显示为 %q", got)
	}
	withCfg(t, cfgWith([]string{"A", "B"}, nil))
	if got := targetDisplay(); got != "A, B" {
		t.Fatalf("多目标显示为 %q", got)
	}
}

func TestSplitTokens(t *testing.T) {
	got := splitTokens("1,2 3，4、5")
	want := []string{"1", "2", "3", "4", "5"}
	if len(got) != len(want) {
		t.Fatalf("splitTokens = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitTokens = %v, want %v", got, want)
		}
	}
}

func TestFilterList(t *testing.T) {
	got := filterList([]string{"A", "B", "C"}, []string{"C", "A"})
	if len(got) != 2 || got[0] != "A" || got[1] != "C" {
		t.Fatalf("filterList = %v", got)
	}
}
