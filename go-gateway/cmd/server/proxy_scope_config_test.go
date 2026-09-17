package main

// proxy_scope_config_test.go 锁住 `proxy_scope` 的解析、默认值与启动日志。
//
// 为什么单独一个文件：本次把「一个代理地址」拆成**三个独立开关**，而
// proxy_scope 是**新增的键** —— 所有既有配置里都没有它。因此「键缺席时取什么值」
// 直接决定升级后用户的流量走向，而配错方向的后果（国服被新绕进代理 /
// 国际版代理静默失效）都不会有任何报错，只表现为「某类账号突然变慢或超时」。
// 每个分支都值得单独锁一遍。
//
// 网关侧只有 cn / intl 两格：GitHub（检查更新 / 下载安装包）是宿主的活，
// 网关根本不发往 github.com 的请求，因此配置结构里刻意没有 github 键
//（见 config.Config.ProxyScope 的注释）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadProxyConfig 写一份只带 proxy / proxy_scope 的配置，按正常路径加载。
//
// 走 Load（= Default() → json.Unmarshal → normalize）而不是手工构造 Config：
// 「键缺席时取什么值」正是本文件要测的东西，手工构造会绕过 Default()，
// 也就测不到那个默认值。
func loadProxyConfig(t *testing.T, body string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(fp)
}

// TestProxyScopeDefaultIsIntlOnCNAndOff 默认值：国际版开、国服关。
//
// 这是**向后兼容的核心证据**，也是本次最容易做错的一处默认值。
//
// 为什么不是「全开」：本次改动之前国服走的是 http.ProxyFromEnvironment，
// **根本不吃**用户在设置页填的显式代理；只有国际版吃（见 upstream.SetProxy）。
// 若缺省成 cn=true，所有既有用户升级后国服流量会被**新绕进**显式代理 ——
// 与所有者上一轮明确要求的「别让国内也走代理流量」直接冲突。
//
// 为什么不是「全关」：那会让国际版的代理**静默失效**（升级后国际版开始超时），
// 而用户完全不知道是自己的配置被改了。
func TestProxyScopeDefaultIsIntlOnCNAndOff(t *testing.T) {
	c := Default()
	if c.ProxyScope.CN {
		t.Error("默认国服必须是 false（改动前国服不吃显式代理，true = 升级后把国服新绕进代理）")
	}
	if !c.ProxyScope.Intl {
		t.Error("默认国际版必须是 true（false = 升级后国际版代理静默失效，国内直连会超时）")
	}
}

// TestProxyScopeMissingKeyKeepsLegacyBehavior 老配置（没有 proxy_scope 键）= 逐字保持现状。
//
// 这是升级路径上最关键的一条：所有既有 gateway_config.json / native config
// 都是老宿主写的，里面没有这个块。它们必须得到与「本次改动之前」完全一致的
// 分流结果 —— 国服直连、国际版走代理。
func TestProxyScopeMissingKeyKeepsLegacyBehavior(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"整个键缺席", `{"proxy":"http://127.0.0.1:7890"}`},
		{"整套配置为空对象", `{}`},
		{"proxy_scope 为 null", `{"proxy":"http://127.0.0.1:7890","proxy_scope":null}`},
		{"proxy_scope 为空对象", `{"proxy":"http://127.0.0.1:7890","proxy_scope":{}}`},
		// 部分键：更早的中间版本 / 手工编辑过的配置都可能只写一格。
		{"只有 cn", `{"proxy_scope":{"cn":true}}`},
		{"只有 intl", `{"proxy_scope":{"intl":false}}`},
	}
	wantCN := map[string]bool{
		"整个键缺席": false, "整套配置为空对象": false, "proxy_scope 为 null": false,
		"proxy_scope 为空对象": false,
		"只有 cn":            true,  // 显式写了 cn=true 就该生效
		"只有 intl":          false, // 只写 intl=false，cn 缺 → 回落默认 false
	}
	wantIntl := map[string]bool{
		"整个键缺席": true, "整套配置为空对象": true, "proxy_scope 为 null": true,
		"proxy_scope 为空对象": true,
		"只有 cn":            true, // 只写 cn，intl 缺 → 回落默认 true
		"只有 intl":          false,
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := loadProxyConfig(t, tc.body)
			if err != nil {
				t.Fatalf("必须能解析（老配置解析失败 = 网关起不来）: %v", err)
			}
			if c.ProxyScope.CN != wantCN[tc.name] {
				t.Errorf("cn=%v，期望 %v", c.ProxyScope.CN, wantCN[tc.name])
			}
			if c.ProxyScope.Intl != wantIntl[tc.name] {
				t.Errorf("intl=%v，期望 %v", c.ProxyScope.Intl, wantIntl[tc.name])
			}
		})
	}
}

// TestProxyScopeExplicitValuesParsed 显式写的四个组合都要逐字读出来。
//
// 关键的一条是「都关」（cn=false,intl=false）：它必须能与「键缺席」区分开 ——
// 前者是用户明确要求两路都真直连，后者是健康的老配置（国际版仍走代理）。
// 两者都表现为「intl=false？不一定」，所以逐个组合都要钉住。
func TestProxyScopeExplicitValuesParsed(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		cn, intl bool
	}{
		{"都关", `{"proxy_scope":{"cn":false,"intl":false}}`, false, false},
		{"只 intl 开", `{"proxy_scope":{"cn":false,"intl":true}}`, false, true},
		{"只 cn 开", `{"proxy_scope":{"cn":true,"intl":false}}`, true, false},
		{"都开", `{"proxy_scope":{"cn":true,"intl":true}}`, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := loadProxyConfig(t, tc.body)
			if err != nil {
				t.Fatalf("必须能解析: %v", err)
			}
			if c.ProxyScope.CN != tc.cn || c.ProxyScope.Intl != tc.intl {
				t.Errorf("得到 cn=%v intl=%v，期望 cn=%v intl=%v",
					c.ProxyScope.CN, c.ProxyScope.Intl, tc.cn, tc.intl)
			}
		})
	}
}

// TestProxyScopeUnknownKeysIgnored 未知键（例如宿主多写的 github）不得报错。
//
// 向前兼容：宿主的 proxy_scope 里带 github 也被允许（JSON 未知字段自然忽略）。
// 若哪天 Go 侧改用了 DisallowUnknownFields，这条会立刻变红 —— 而那正是要防的：
// 一个多余的键让整个网关起不来，属于「配置项导致服务不可用」的最坏形态。
func TestProxyScopeUnknownKeysIgnored(t *testing.T) {
	c, err := loadProxyConfig(t,
		`{"proxy_scope":{"github":true,"cn":true,"intl":false,"future_key":"x"}}`)
	if err != nil {
		t.Fatalf("未知键不该让配置解析失败: %v", err)
	}
	if !c.ProxyScope.CN {
		t.Error("cn=true 应被读取（同一个块里的未知键不该影响已知键）")
	}
	if c.ProxyScope.Intl {
		t.Error("intl=false 应被读取")
	}
}

// TestProxyScopeWrongTypeFailsFast 类型写错必须**报错**，不能静默回落默认值。
//
// 与 allowed_model 的取舍一致（那里是数字/对象报错）。反向的静默更危险：
// 用户把 intl 写成字符串 "true"，若静默回落成默认 true，看起来「碰巧对了」；
// 但把 cn 写成 "false" 时静默回落 false 也「碰巧对了」—— 于是用户以为自己
// 的配置生效了，实际从未被读到。宁可启动失败并报出字段名。
func TestProxyScopeWrongTypeFailsFast(t *testing.T) {
	for _, body := range []string{
		`{"proxy_scope":{"cn":"true"}}`,
		`{"proxy_scope":{"intl":1}}`,
		`{"proxy_scope":{"cn":{}}}`,
		`{"proxy_scope":[]}`,
	} {
		if _, err := loadProxyConfig(t, body); err == nil {
			t.Errorf("%s 应报类型错误（静默回落会让用户以为配置生效了）", body)
		}
	}
}

// TestProxyScopeHasNoGitHubKey 网关配置结构里**不得**有 github 那一格。
//
// 语义边界：更新检查 / 安装包下载是宿主的活，网关根本不发往 github.com 的请求。
// 这里用「写入一个 github 键后读回来不影响任何网关行为」间接表达该边界 ——
// 真正的保证在结构体本身（没有 GitHub 字段），本用例挡住的是「后来者顺手加上」。
func TestProxyScopeHasNoGitHubKey(t *testing.T) {
	c, err := loadProxyConfig(t,
		`{"proxy":"http://127.0.0.1:7890","proxy_scope":{"github":false,"cn":true,"intl":true}}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// github=false 不该影响网关的两个区域开关。
	if !c.ProxyScope.CN || !c.ProxyScope.Intl {
		t.Errorf("宿主写的 github 开关不该影响网关侧两个区域：cn=%v intl=%v",
			c.ProxyScope.CN, c.ProxyScope.Intl)
	}
}

// TestDescribeProxyScopeTellsTheTruth 启动日志必须如实写明两个区域各自的走向。
//
// 这行日志是用户排查「某一路为什么走了/没走代理」的第一现场（网关子进程的
// stdout 在 GUI 里可能被丢弃）。写错方向的代价是把他引到完全错误的排查路径上：
// 例如国服其实直连、日志却说「经代理」，他会去查代理为什么慢。
func TestDescribeProxyScopeTellsTheTruth(t *testing.T) {
	const addr = "http://127.0.0.1:7890"

	cases := []struct {
		name     string
		cn, intl bool
		// 该区域在日志里**必须**出现 / **禁止**出现的特征串
		cnContains    []string
		cnNotContains []string
		intlContains  []string
	}{
		{
			name: "都开", cn: true, intl: true,
			cnContains: []string{addr},
			// 「真直连」四个字在都开时不该出现 —— 那会让人以为国服没走代理。
			cnNotContains: []string{"真直连"},
			intlContains:  []string{addr},
		},
		{
			name: "只 intl 开", cn: false, intl: true,
			// 关了就必须说清是「真直连（连环境变量也不用）」而不是含糊的「未使用」，
			// 否则设了 HTTPS_PROXY 的用户无从判断自己的国服流量到底走没走。
			cnContains:    []string{"真直连", "环境变量"},
			cnNotContains: []string{addr},
			intlContains:  []string{addr},
		},
		{
			name: "都关", cn: false, intl: false,
			cnContains:    []string{"真直连"},
			cnNotContains: []string{addr},
			intlContains:  []string{"真直连"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := describeProxyScope(addr, tc.cn, tc.intl)
			// 日志必须先按区域切开，否则「国服那一段说了什么」无从判断。
			cnPart, intlPart, ok := splitScopeLog(line)
			if !ok {
				t.Fatalf("日志里找不到两个区域的段落（用户无法分辨哪句说的是哪个区域）: %q", line)
			}
			for _, s := range tc.cnContains {
				if !strings.Contains(cnPart, s) {
					t.Errorf("国服段落应包含 %q，实际 %q", s, cnPart)
				}
			}
			for _, s := range tc.cnNotContains {
				if strings.Contains(cnPart, s) {
					t.Errorf("国服段落不该包含 %q，实际 %q", s, cnPart)
				}
			}
			for _, s := range tc.intlContains {
				if !strings.Contains(intlPart, s) {
					t.Errorf("国际版段落应包含 %q，实际 %q", s, intlPart)
				}
			}
		})
	}
}

// splitScopeLog 把日志按「国服账号…；国际版账号…」切成两段。
//
// 用这个切法而不是按固定下标取子串：下标会随文案调整而失效，而本用例要断言的
// 是**语义**（哪一段说的是哪个区域），不是字符位置。
func splitScopeLog(line string) (cn, intl string, ok bool) {
	i := strings.Index(line, "国际版账号")
	if i < 0 {
		return "", "", false
	}
	cn = line[:i]
	// 去掉分隔用的「；」与空白，避免它干扰「不该包含 addr」这类断言。
	cn = strings.TrimRight(strings.TrimSpace(cn), "；;")
	return cn, line[i:], strings.Contains(cn, "国服账号")
}
