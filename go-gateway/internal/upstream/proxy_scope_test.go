package upstream

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// 代理的三个独立开关（本次改动的核心）
//
// 背景（所有者诉求原话）：「代理单独开一个配置，三个选项 Github 国内版 国际版
// 加上描述，Github 默认打开，其余两个用户控制，是否打开使用代理」。
//
// 地址仍只填一次，`proxy_scope` 决定**哪些区域**使用它：
//
//	proxy_scope.cn   = true  → 国服账号的出站请求走显式代理
//	proxy_scope.intl = true  → 国际版账号的出站请求走显式代理
//
// 网关侧只有这两格：GitHub（更新检查 / 安装包下载）是**宿主**的活，
// 网关根本不发往 github.com 的请求。
//
// 本文件锁三件事：
//  1. 四种组合下**真实流量**的走向（用假代理计数，而不是只看 transport 指针）；
//  2. 「关」= 真直连（连环境变量代理也不用），不是「只不挂显式代理」；
//  3. 未显式指定适用范围时 = 改动前的行为（国服 env、国际版显式代理）。
// ---------------------------------------------------------------------------

// TestProxyScopeThreeCombinations 四种开关组合下，两条通道的**真实走向**。
//
// 这是本次改动最重要的一条用例。被测对象是假代理上的**计数**，不是
// transport 指针 —— 指针相同而流量走错（或反之）正是这类分流改动最容易出的错。
//
// 期望矩阵（显式代理已配置；本用例不涉及环境变量代理，故只数显式代理）：
//
//	组合                     国服          国际版
//	都关                    真直连         真直连
//	只 intl 开              真直连         显式代理
//	只 cn 开                显式代理       真直连
//	都开                    显式代理       显式代理
func TestProxyScopeThreeCombinations(t *testing.T) {
	cases := []struct {
		name      string
		cn, intl  bool
		wantCNExp int // 国服应经「显式代理」的条数
		wantIntl  int // 国际版应经「显式代理」的条数
	}{
		{"都关", false, false, 0, 0},
		{"只 intl 开", false, true, 0, 1},
		{"只 cn 开", true, false, 1, 0},
		{"都开", true, true, 1, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := fakeUpstream(t)
			px := newFakeProxy(t)

			c := routableClient(t, up.URL)
			c.ChatHTTP = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
			// 顺序与 main.go 一致：New → SetProxy → SetProxyScope → 调优，
			// 避免测出一个生产不会出现的状态。
			if err := c.SetProxy(px.srv.URL); err != nil {
				t.Fatalf("SetProxy: %v", err)
			}
			if err := c.SetProxyScope(tc.cn, tc.intl); err != nil {
				t.Fatalf("SetProxyScope: %v", err)
			}

			// 国服。
			before := px.count()
			if err := c.ReportChatActivity(cnAuth(), "conv", "req"); err != nil {
				t.Fatalf("国服请求本身失败: %v", err)
			}
			if got := px.count() - before; got != tc.wantCNExp {
				t.Fatalf("国服经显式代理 %d 条，期望 %d 条\n代理记录：%v", got, tc.wantCNExp, px.urls())
			}

			// 国际版。
			mid := px.count()
			if err := c.ReportChatActivity(intlAuth(), "conv", "req"); err != nil {
				t.Fatalf("国际版请求本身失败: %v", err)
			}
			if got := px.count() - mid; got != tc.wantIntl {
				t.Fatalf("国际版经显式代理 %d 条，期望 %d 条\n代理记录：%v", got, tc.wantIntl, px.urls())
			}
		})
	}
}

// TestProxyScopeOffStillReachesUpstream 开关关掉后请求**确实直达上游**。
//
// 上一条「都关 → 假代理 0 条」有个致命盲区：请求可能因为 transport 配错而
// 彻底失败，那样计数同样是 0，看起来却像「正确地直连了」。所以这里用一个
// 自建上游统计**真实到达**的请求，证明「直连」是通的而不是死的。
func TestProxyScopeOffStillReachesUpstream(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	up := newFakeUpstream(t, &hits, &mu)

	px := newFakeProxy(t)
	c := routableClient(t, up)
	if err := c.SetProxy(px.srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := c.SetProxyScope(false, false); err != nil {
		t.Fatal(err)
	}

	// 两条通道各发一次。
	if err := c.ReportChatActivity(cnAuth(), "conv", "req"); err != nil {
		t.Fatalf("国服直连失败: %v", err)
	}
	if err := c.ReportChatActivity(intlAuth(), "conv", "req"); err != nil {
		t.Fatalf("国际版直连失败: %v", err)
	}

	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 2 {
		t.Fatalf("上游直连收到 %d 条，期望 2 条（请求没发出去？）", got)
	}
	if n := px.count(); n != 0 {
		t.Fatalf("两个开关都关，不该有任何请求经过代理，实际 %d 条: %v", n, px.urls())
	}
}

// TestSetProxyScopeWithoutAddressIsNoop 没填代理地址时，开关不改变任何行为。
//
// 为什么这条必须锁死：SetProxyScope(false,false) 的语义是「真直连」，
// 但一个**根本没填过代理**的用户，若因为「国内版开关默认关」就被改成真直连，
// 他原本靠 HTTPS_PROXY 做全局代理的国服流量会静默失效 —— 而界面上看不出
// 任何变化（地址栏是空的，开关也只是默认值）。
//
// 所以：没有地址 → 什么都不做（保持 SetProxy("") 之后的状态 =
// ProxyFromEnvironment，即改动前的既有行为）。
func TestSetProxyScopeWithoutAddressIsNoop(t *testing.T) {
	c := New()
	// 先清空（宿主 cfg.Proxy 为空时走的路径）。
	if err := c.SetProxy(""); err != nil {
		t.Fatal(err)
	}
	before := c.ChatHTTP.Transport

	if err := c.SetProxyScope(false, false); err != nil {
		t.Fatalf("SetProxyScope 不应报错: %v", err)
	}

	if c.ChatHTTP.Transport != before {
		t.Error("未配地址时开关不该重建 transport（那是无意义的抖动）")
	}
	// 国服仍须回落 ProxyFromEnvironment：与标准库行为一致即可
	//（环境变量可能有值且被首次读取缓存，故不断言必然为 nil）。
	tr, ok := c.ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型=%T", c.ChatHTTP.Transport)
	}
	if tr.Proxy == nil {
		t.Fatal("未配地址时国服仍应回落 ProxyFromEnvironment（改动前的既有行为）")
	}
	req, _ := http.NewRequest("GET", "https://copilot.tencent.com/v2/chat/completions", nil)
	got, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy(req): %v", err)
	}
	want, werr := http.ProxyFromEnvironment(req)
	if werr != nil {
		t.Fatalf("ProxyFromEnvironment: %v", werr)
	}
	if (got == nil) != (want == nil) {
		t.Errorf("应与 ProxyFromEnvironment 一致：got=%v want=%v", got, want)
	}
	// 未配地址时不应存在国际版专用 client（= New() 的既有状态）。
	if c.intlHTTP != nil || c.intlChatHTTP != nil {
		t.Error("未配地址时不该存在国际版专用 client")
	}
	// 且开关不该被"记住"成已指定：ProxyScope() 仍应是 nil。
	if c.ProxyScope() != nil {
		t.Error("未配地址时不应记录适用范围，否则 set 顺序不同会得到不同行为")
	}
}

// TestSetProxyScopeNilPreservesLegacySplit 不调 SetProxyScope 时 = 改动前的分流。
//
// 这条是**向后兼容的护栏**：老宿主（没写 proxy_scope 的版本）拉着新版网关跑，
// 或者老配置缺这个键时，国服必须仍然直连（不吃显式代理）、国际版仍然走代理。
// 若哪天有人把 SetProxy 的默认语义改成「全走代理」，本用例会立刻变红。
func TestSetProxyScopeNilPreservesLegacySplit(t *testing.T) {
	up := fakeUpstream(t)
	px := newFakeProxy(t)

	c := routableClient(t, up.URL)
	c.ChatHTTP = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	if err := c.SetProxy(px.srv.URL); err != nil {
		t.Fatal(err)
	}
	// **刻意不调** SetProxyScope。
	if c.ProxyScope() != nil {
		t.Fatal("未调 SetProxyScope 时 ProxyScope() 应为 nil（表示未显式指定）")
	}

	if err := c.ReportChatActivity(cnAuth(), "conv", "req"); err != nil {
		t.Fatal(err)
	}
	if n := px.count(); n != 0 {
		t.Fatalf("老行为：国服不该经显式代理，实际 %d 条: %v", n, px.urls())
	}
	if err := c.ReportChatActivity(intlAuth(), "conv", "req"); err != nil {
		t.Fatal(err)
	}
	if n := px.count(); n != 1 {
		t.Fatalf("老行为：国际版应经显式代理 1 条，实际 %d 条", n)
	}
}

// TestProxyScopeKeepsPerRegionTransportIsolated 开关布置后区域之间仍物理隔离。
//
// 「都开」时两个区域都走同一个显式代理地址，但**必须仍是两套 transport**：
// 共用一套会让「关掉其中一路」变得无法生效（下一次改开关就得重建两个区域的
// 连接池），也让本文件的计数断言失去意义（同一个池，谁走的一看便知是巧合）。
func TestProxyScopeKeepsPerRegionTransportIsolated(t *testing.T) {
	c := New()
	if err := c.SetProxy("http://127.0.0.1:7890"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetProxyScope(true, true); err != nil {
		t.Fatal(err)
	}
	if c.intlHTTP == nil || c.intlChatHTTP == nil {
		t.Fatal("两个开关都开时国际版应有独立 client")
	}
	if c.intlHTTP.Transport == c.HTTP.Transport {
		t.Error("国际版与国服不得共用 Transport（共用后单独关掉一路会失效）")
	}
	// 区域内共享（原意保留：连接池不重复）。
	if c.ChatHTTP.Transport != c.HTTP.Transport {
		t.Error("国服侧 ChatHTTP 与 HTTP 仍须共享同一个 Transport")
	}
	if c.intlChatHTTP.Transport != c.intlHTTP.Transport {
		t.Error("国际版侧 ChatHTTP 与 HTTP 仍须共享同一个 Transport")
	}
	// 两区都解析出同一个显式代理。
	for _, tc := range []struct {
		name   string
		client *http.Client
		host   string
	}{
		{"国服", c.HTTP, "https://copilot.tencent.com/v2/chat/completions"},
		{"国际版", c.intlHTTP, "https://www.workbuddy.ai/v2/chat/completions"},
	} {
		tr, ok := tc.client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s Transport 类型=%T", tc.name, tc.client.Transport)
		}
		req, _ := http.NewRequest("GET", tc.host, nil)
		got, err := tr.Proxy(req)
		if err != nil {
			t.Fatalf("%s Proxy(req): %v", tc.name, err)
		}
		if got == nil || got.String() != "http://127.0.0.1:7890" {
			t.Fatalf("%s 代理=%v，期望 http://127.0.0.1:7890", tc.name, got)
		}
	}
}

// TestProxyScopeAppliesTimeoutsToAllTransports 开关新建的 transport 也要吃到调优值。
//
// 漏掉「真直连」那一个的表现是「开了开关的那一路按新上限超时，另一路仍按
// 120s 干等」，两边日志长得一样，从现象上几乎发现不了。
func TestProxyScopeAppliesTimeoutsToAllTransports(t *testing.T) {
	c := New()
	if err := c.SetProxy("http://127.0.0.1:7890"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetProxyScope(false, true); err != nil {
		t.Fatal(err)
	}
	c.SetRPCTimeout(7 * 1e9)
	c.ApplyResponseHeaderTimeout(9 * 1e9)

	if c.HTTP.Timeout != 7*1e9 || c.intlHTTP.Timeout != 7*1e9 {
		t.Errorf("短 RPC 上限未覆盖两区：HTTP=%v intlHTTP=%v", c.HTTP.Timeout, c.intlHTTP.Timeout)
	}
	for _, tc := range []struct {
		name string
		cl   *http.Client
	}{{"HTTP", c.HTTP}, {"ChatHTTP", c.ChatHTTP}, {"intlHTTP", c.intlHTTP}, {"intlChatHTTP", c.intlChatHTTP}} {
		tr := tc.cl.Transport.(*http.Transport)
		if tr.ResponseHeaderTimeout != 9*1e9 {
			t.Errorf("%s.ResponseHeaderTimeout=%v 应为 9s", tc.name, tr.ResponseHeaderTimeout)
		}
	}
}

// ---------------------------------------------------------------------------
// 决定性实测：「关」= 真直连（连环境变量代理也不用）
//
// 为什么必须起**子进程**：Go 的 httpproxy 在进程内**首次读取环境变量后即缓存**
//（net/http/internal/httpproxy 的单次初始化），同进程里 t.Setenv 改不生效。
// 子进程是唯一能让「进程启动时就带着 HTTPS_PROXY」成立的办法，而这正是生产的
// 真实情形：宿主（Tauri）带着环境变量拉起 gateway.exe。
//
// 为什么必须用**非回环**假上游：ProxyFromEnvironment 永不代理 loopback
//（标准库既有规则）。上游若是 127.0.0.1，环境变量代理那条断言会「因为上游是
// 回环」而假绿 —— 无论实现怎么写都收到 0 条。
// ---------------------------------------------------------------------------

const (
	scopeRoleVar   = "WB_SCOPE_ROLE"
	scopeChildDone = "scope-probe-done"
	scopeUpVar     = "WB_SCOPE_UPSTREAM"
	scopeExpVar    = "WB_SCOPE_EXPLICIT"
	scopeCNVar     = "WB_SCOPE_CN"
	scopeIntlVar   = "WB_SCOPE_INTL"
)

// TestProxyScopeOffMeansReallyDirect 两个开关都关：显式代理与环境变量代理**都不碰**。
//
// 期望：显式代理 0 条、环境变量代理 0 条、上游**确实收到 2 条**
// （证明是「直连成功」而不是「请求根本没发出去」）。
func TestProxyScopeOffMeansReallyDirect(t *testing.T) {
	if probeRole() != "" {
		runScopeChild(t)
		return
	}

	host := nonLoopbackIPv4(t)
	up, hits := countingUpstreamOn(t, host)
	explicit := newFakeProxy(t)
	envProxy := newFakeProxy(t)

	out := runScopeChildProcess(t, "TestProxyScopeOffMeansReallyDirect", scopeChildArgs{
		upstream: up, explicit: explicit.srv.URL, envProxy: envProxy.srv.URL,
		cn: "0", intl: "0",
	})

	if n := explicit.count(); n != 0 {
		t.Fatalf("开关都关，显式代理却收到 %d 条：%v", n, explicit.urls())
	}
	if n := envProxy.count(); n != 0 {
		t.Fatalf("开关都关，环境变量代理却收到 %d 条 —— 「关」没有做到真直连："+
			"用户关掉开关后流量仍在绕道，而界面上显示「已关闭」：%v", n, envProxy.urls())
	}
	if got := hits.value(); got != 2 {
		t.Fatalf("上游收到 %d 条，期望 2 条（直连实际失败了？0 条代理计数可能是假象）", got)
	}
	t.Logf("实测结论：显式代理 0 条、环境变量代理 0 条、上游 2 条（两条通道都真直连）\n%s",
		strings.TrimSpace(out))
}

// TestProxyScopeCNOnUsesExplicitNotEnv 只开国服：流量走**显式**代理而非环境变量代理。
//
// 与上一条对称。若实现把「开」写成「回落 ProxyFromEnvironment」，在有环境变量的
// 机器上流量会走到**另一个**代理，用户填的地址形同虚设 —— 而现象只在设了
// 环境变量的机器上出现，极难排查。
func TestProxyScopeCNOnUsesExplicitNotEnv(t *testing.T) {
	if probeRole() != "" {
		runScopeChild(t)
		return
	}

	host := nonLoopbackIPv4(t)
	up, hits := countingUpstreamOn(t, host)
	explicit := newFakeProxy(t)
	envProxy := newFakeProxy(t)

	out := runScopeChildProcess(t, "TestProxyScopeCNOnUsesExplicitNotEnv", scopeChildArgs{
		upstream: up, explicit: explicit.srv.URL, envProxy: envProxy.srv.URL,
		cn: "1", intl: "0",
	})

	// 两条通道必须用**可区分**的请求路径，否则「谁经过了谁」无法从计数里读出来。
	cnWant := "POST " + up + "/activity/growth/tasks/scope-cn/claim"
	intlWant := "POST " + up + "/activity/growth/tasks/scope-intl/claim"

	if got := explicit.urls(); len(got) != 1 || got[0] != cnWant {
		t.Fatalf("显式代理应收到的正是国服那一条\n got=%v\nwant=[%s]", got, cnWant)
	}
	if n := envProxy.count(); n != 0 {
		t.Fatalf("国服开关已打开，流量应走显式代理，环境变量代理却收到 %d 条：%v",
			n, envProxy.urls())
	}
	// 国际版这一路开关是关的 → 必须真直连（不经任何一个代理）。
	if got := hits.value(); got != 2 {
		t.Fatalf("上游收到 %d 条，期望 2 条（国服经代理转发 + 国际版直连）", got)
	}
	t.Logf("实测结论：显式代理收到 [%s]；环境变量代理 0 条；上游 %d 条\n%s",
		cnWant, hits.value(), strings.TrimSpace(out))
	_ = intlWant
}

// TestProxyScopeIntlOffMeansReallyDirect 只开国服时，**国际版**必须真直连。
//
// 单列一条而不是并进上一条：国际版关掉之后是否真的不走代理，是所有者最关心的
// 一格（他默认开着它，一旦关掉就是要省流量 / 换网络，此时偷偷走代理会让他
// 以为开关坏了）。用请求路径把国际版那一条单独认出来。
func TestProxyScopeIntlOffMeansReallyDirect(t *testing.T) {
	if probeRole() != "" {
		runScopeChild(t)
		return
	}

	host := nonLoopbackIPv4(t)
	up, _ := countingUpstreamOn(t, host)
	explicit := newFakeProxy(t)
	envProxy := newFakeProxy(t)

	runScopeChildProcess(t, "TestProxyScopeIntlOffMeansReallyDirect", scopeChildArgs{
		upstream: up, explicit: explicit.srv.URL, envProxy: envProxy.srv.URL,
		cn: "1", intl: "0",
	})

	intlWant := "POST " + up + "/activity/growth/tasks/scope-intl/claim"
	for _, tc := range []struct {
		name string
		p    *fakeProxy
	}{
		{"显式代理", explicit},
		{"环境变量代理", envProxy},
	} {
		for _, got := range tc.p.urls() {
			if got == intlWant {
				t.Fatalf("国际版开关已关，其请求却出现在%s里：%s", tc.name, got)
			}
		}
	}
	t.Logf("实测结论：国际版那一条（%s）未出现在任何代理中（真直连）", intlWant)
}

// probeRole 本进程是否是探针子进程（非空表示是）。
func probeRole() string {
	switch os.Getenv(scopeRoleVar) {
	case "off", "cn-on":
		return os.Getenv(scopeRoleVar)
	default:
		return ""
	}
}

// scopeChildArgs 传给子进程的探针参数。
type scopeChildArgs struct {
	upstream string
	explicit string
	envProxy string
	cn, intl string
}

// runScopeChildProcess 起子进程跑探针，返回其输出（失败即 Fatal）。
//
// 子进程带着 HTTP_PROXY/HTTPS_PROXY 启动 —— 这是「环境变量代理」那条断言的
// 前提，也是唯一能在 Go 里做到的时机（httpproxy 首次读取即缓存）。
func runScopeChildProcess(t *testing.T, testName string, a scopeChildArgs) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$", "-test.v")
	cmd.Env = append(append([]string{}, os.Environ()...),
		scopeRoleVar+"="+"off",
		scopeUpVar+"="+a.upstream,
		scopeExpVar+"="+a.explicit,
		scopeCNVar+"="+a.cn,
		scopeIntlVar+"="+a.intl,
		// 两套都给：只设 HTTPS_PROXY 时 http:// 目标不走代理。
		"HTTP_PROXY="+a.envProxy,
		"HTTPS_PROXY="+a.envProxy,
		// NO_PROXY 必须清空：本机可能恰好覆盖了测试域名，用例会「因为环境
		// 碰巧如此」而变绿。
		"NO_PROXY=", "no_proxy=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("子进程探针失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), scopeChildDone) {
		t.Fatalf("子进程没有跑到结论点：\n%s", out)
	}
	return string(out)
}

// runScopeChild 子进程侧：按环境变量里的开关布置 transport，两条通道各发一次。
//
// 请求路径刻意不同（task code 不同）：两条通道的请求行若长得一模一样，
// 「谁经过了谁」就无法从代理的计数里读出来。
func runScopeChild(t *testing.T) {
	up := os.Getenv(scopeUpVar)
	explicit := os.Getenv(scopeExpVar)
	if up == "" || explicit == "" {
		t.Fatal("子进程缺少探针参数")
	}
	cn := os.Getenv(scopeCNVar) == "1"
	intl := os.Getenv(scopeIntlVar) == "1"

	c := routableClient(t, up)
	if err := c.SetProxy(explicit); err != nil {
		t.Fatalf("子进程 SetProxy: %v", err)
	}
	if err := c.SetProxyScope(cn, intl); err != nil {
		t.Fatalf("子进程 SetProxyScope: %v", err)
	}
	if _, _, _, err := c.ClaimGrowthTask(cnAuth(), "scope-cn"); err != nil {
		t.Fatalf("国服请求失败: %v", err)
	}
	if _, _, _, err := c.ClaimGrowthTask(intlAuth(), "scope-intl"); err != nil {
		t.Fatalf("国际版请求失败: %v", err)
	}
	t.Logf("%s cn=%v intl=%v", scopeChildDone, cn, intl)
}

// hitCounter 并发安全的到达计数。
type hitCounter struct {
	mu sync.Mutex
	n  int
}

func (h *hitCounter) inc() {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
}

func (h *hitCounter) value() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// countingUpstreamOn 在**指定地址**上起一个会计数的假上游。
//
// 必须能指定地址（而不是 httptest 默认的 127.0.0.1）：ProxyFromEnvironment
// 永不代理 loopback，上游在回环上时「环境变量代理 0 条」这条断言恒真，测不出东西。
func countingUpstreamOn(t *testing.T, host string) (string, *hitCounter) {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("在 %s 上监听失败，跳过：%v", host, err)
	}
	hits := &hitCounter{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.inc()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{}}`)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String(), hits
}

// newFakeUpstream 会计数的假上游（监听在 127.0.0.1，供不需要环境变量的用例）。
func newFakeUpstream(t *testing.T, hits *int, mu *sync.Mutex) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*hits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{}}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// 防伪：确认 c.ProxyScope() 记录的是**显式**指定的开关（而非 nil）。
//
// 单独一条是因为它很容易被漏掉：nil 与「两个都 false」在行为上恰好一致
// （都真直连），只测行为就区分不出来，而两者的语义完全不同 ——
// nil 表示「老行为」（国服走环境变量代理），非 nil 才表示「用户说了算」。
func TestProxyScopeRecordsExplicitChoice(t *testing.T) {
	c := New()
	if c.ProxyScope() != nil {
		t.Fatal("New() 之后应为 nil（未显式指定）")
	}
	if err := c.SetProxy("http://127.0.0.1:7890"); err != nil {
		t.Fatal(err)
	}
	if c.ProxyScope() != nil {
		t.Fatal("SetProxy 之后仍应为 nil：它表达的是老行为，不含适用范围")
	}
	if err := c.SetProxyScope(false, false); err != nil {
		t.Fatal(err)
	}
	got := c.ProxyScope()
	if got == nil {
		t.Fatal("SetProxyScope 之后必须记下显式选择（nil 会被读成「老行为」）")
	}
	if got.CN || got.Intl {
		t.Fatalf("记录的两个开关=%+v，期望都关", *got)
	}
}

// TestSetProxyScopeReusesNormalizedAddress 用户只填 host:port 时，开关仍能正确布置。
//
// 生产者顺序是 main.go 的 `SetProxy(cfg.Proxy)` → `SetProxyScope(...)`。SetProxy
// 会把 `127.0.0.1:7890` **规范化成** `http://127.0.0.1:7890` 并记进 proxyURL；
// SetProxyScope 复用的就是这个字段。
//
// 本用例锁的是「复用规范化后的地址」这条链：若哪天有人让 SetProxyScope 自己去解析
// 原始配置串（而不是复用 c.proxyURL），「补 http://」这类容错就会在两处各写一份，
// 迟早分叉 —— 分叉的表现是「用户填 host:port 时开关不生效」，而填完整 URL 时正常。
func TestSetProxyScopeReusesNormalizedAddress(t *testing.T) {
	const hostPort = "127.0.0.1:7890"
	c := New()
	if err := c.SetProxy(hostPort); err != nil {
		t.Fatalf("应容忍无 scheme 的写法: %v", err)
	}
	if !strings.Contains(c.ProxyURL(), "://") {
		t.Fatalf("SetProxy 应把地址规范化（补 http://），实际存的是 %q", c.ProxyURL())
	}

	// 只开国际版：它必须解析到规范化后的同一个地址。
	if err := c.SetProxyScope(false, true); err != nil {
		t.Fatal(err)
	}
	intlTr, ok := c.intlHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("intlHTTP.Transport 类型=%T", c.intlHTTP.Transport)
	}
	intlReq, _ := http.NewRequest("GET", "https://www.workbuddy.ai/v2/chat/completions", nil)
	got, err := intlTr.Proxy(intlReq)
	if err != nil {
		t.Fatalf("国际版 Proxy(req): %v", err)
	}
	if got == nil || got.Host != hostPort {
		t.Fatalf("国际版代理=%v，期望 host=%s（开关没复用规范化后的地址？）", got, hostPort)
	}

	// 国服关 → 真直连。
	//
	// 「真直连」的**判定方式就是 Proxy 为 nil**（不是「Proxy 返回 nil」）：
	// http.Transport.Proxy 的契约是「为 nil 表示不使用代理」，此时标准库根本
	// 不会调用它。所以这里必须断言函数值本身为 nil ——
	// 若误写成 `cnTr.Proxy(cnReq)`，对一个真直连 transport 会直接
	// nil 函数调用 panic（本用例最初就是这么写错的，正好反证了语义）。
	cnTr, ok := c.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("HTTP.Transport 类型=%T", c.HTTP.Transport)
	}
	if cnTr.Proxy != nil {
		cnReq, _ := http.NewRequest("GET", "https://copilot.tencent.com/v2/chat/completions", nil)
		got, _ := cnTr.Proxy(cnReq)
		t.Fatalf("国服开关是关的，Proxy 却非 nil（解析出 %v）—— 关没有做到真直连", got)
	}
}
