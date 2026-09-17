//go:build live

// proxy_scope_live_test.go 真实账号 + 本地假代理的**三个开关**实测
// （只在显式指定 live 构建标签时编译与运行）。
//
// 为什么必须走真实上游：本次改动的风险不在代码分支，而在**真实流量的走向** ——
// 「关」到底是不是真直连、开着的那一路到底落在哪个代理上，用 127.0.0.1 的假上游
// 是证不出来的（ProxyFromEnvironment 永不代理 loopback，环境变量那条通道在假上游
// 上恒为 0 条，等于没测）。真实域名 + 真代理计数才能给出结论。
//
// 做法：起**两个**本地假代理（显式 / 环境变量），把网关配成
//
//	proxy = 显式代理；HTTP_PROXY/HTTPS_PROXY = 环境变量代理
//
// 然后按开关组合发真实只读请求（拉积分余额），用两个代理上的**计数**判定。
//
// 运行方式（PowerShell）：
//
//	$env:WB_LIVE_ACCOUNTS = "$env:USERPROFILE\.ai-gateway\accounts.json"   # 只读
//	$env:WB_LIVE_PROXY    = "1"
//	go test -tags live ./internal/upstream/ -run TestLiveProxyScope -v
//
// 安全边界（与 proxy_live_test.go 同一套）：
//   - 账号库**只读**：读入后转成网关凭证形态写入临时目录，绝不回写源文件；
//   - 只发**只读**请求（拉积分余额），不做任何写操作；
//   - 假代理只记录**主机名与端口**（CONNECT 行里本来就只有这些），
//     不记录请求头、不记录 body —— 不会有 token 落盘；
//   - 报告只打印 uid 前 8 位与域名，不含昵称/邮箱。
package upstream

import (
	"os"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestLiveProxyScope 真实账号实测三个开关的四种组合。
//
// 每种组合都用**同一对**假代理各发一次两条通道的请求，按计数判定：
//
//	组合              国服应落在        国际版应落在
//	默认(关/开)       直连(两个都是0)   显式代理
//	只 cn 开          显式代理          直连
//	都关              直连              直连
//	都开              显式代理          显式代理
//
// 只发 billing 只读探针（get-user-resource）：国服与国际版都要走 billingBase，
// 端点形状一致、两边都能打真实上游、且不改任何状态。
func TestLiveProxyScope(t *testing.T) {
	if os.Getenv("WB_LIVE_PROXY") != "1" {
		t.Skip("未设置 WB_LIVE_PROXY=1，跳过真实账号代理开关实测")
	}
	cn, intl := loadLiveAccts(t)
	t.Logf("选中账号：cn uid8=%s domain=%s / intl uid8=%s domain=%s",
		shortUIDLive(cn.UID), cn.Domain, shortUIDLive(intl.UID), intl.Domain)

	// 两个假代理：显式配置的那个 与 模拟 HTTPS_PROXY 的那个。
	px := newFakeProxy(t)
	envPx := newFakeProxy(t)
	t.Logf("显式代理 = %s；环境变量代理 = %s（都是本地假代理，只记录 CONNECT 主机名）",
		px.srv.URL, envPx.srv.URL)

	// 环境变量必须在**本进程**里设置。这里能生效是因为本用例只发真实域名的请求，
	// 不依赖 httpproxy 的缓存时序：缓存的是「配置读到了什么」，
	// 而我们在任何请求发出**之前**就设好了。
	//
	// 注意：Go 的 httpproxy 在首次读取后即缓存，因此 t.Setenv 之后**如果之前
	// 已经发过请求**就不生效了。本用例刻意先设环境变量、再建 client、再发请求。
	t.Setenv("HTTP_PROXY", envPx.srv.URL)
	t.Setenv("HTTPS_PROXY", envPx.srv.URL)
	// NO_PROXY 必须清空：本机可能恰好覆盖了要测的域名，用例会「因为环境碰巧
	// 如此」而变绿。
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	cases := []struct {
		name      string
		cn, intl  bool
		wantCNVia int // 国服应经**显式**代理的条数
		wantIntl  int // 国际版应经**显式**代理的条数
		wantCNEnv int // 国服应经**环境变量**代理的条数
		intlEnv   int // 国际版应经环境变量代理的条数
	}{
		// 默认（宿主 Default()）：国际版开、国服关。
		// 国服必须**两个代理都不碰** = 真直连。
		{"默认(cn=off,intl=on)", false, true, 0, 1, 0, 0},
		// 只开国服：国服走显式代理；国际版关掉 → 两个代理都不碰（真直连）。
		{"只 cn 开", true, false, 1, 0, 0, 0},
		// 都关：两条通道都真直连。
		{"都关", false, false, 0, 0, 0, 0},
		// 都开：国服与国际版都走显式代理（不是环境变量那个）。
		{"都开", true, true, 1, 1, 0, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New()
			if c.ChatBaseCN == "" || c.BillingBaseCN == "" || c.BaseIntl == "" {
				t.Fatal("基址未初始化")
			}
			if err := c.SetProxy(px.srv.URL); err != nil {
				t.Fatalf("SetProxy: %v", err)
			}
			if err := c.SetProxyScope(tc.cn, tc.intl); err != nil {
				t.Fatalf("SetProxyScope: %v", err)
			}

			// 一次探针：返回（经显式代理条数, 经环境变量代理条数）。
			probe := func(a *auth.Auth, label string) (int, int) {
				beforeExp, beforeEnv := px.count(), envPx.count()
				// 401/403 也是有效结论：说明请求真的到了上游并拿到 http 响应；
				// 我们关心的是**流量走向**而不是业务成功。
				_, err := c.UserResourceDetail(a)
				viaExp := px.count() - beforeExp
				viaEnv := envPx.count() - beforeEnv
				t.Logf("%s uid8=%s：经显式代理 %d 条、经环境变量代理 %d 条（err=%v）",
					label, shortUIDLive(a.UID), viaExp, viaEnv, err)
				return viaExp, viaEnv
			}

			cnExp, cnEnv := probe(cn, "国服")
			if cnExp != tc.wantCNVia {
				t.Errorf("国服经显式代理 %d 条，期望 %d 条\n显式代理记录：%v", cnExp, tc.wantCNVia, px.urls())
			}
			if cnEnv != tc.wantCNEnv {
				t.Errorf("国服经环境变量代理 %d 条，期望 %d 条\n环境变量代理记录：%v",
					cnEnv, tc.wantCNEnv, envPx.urls())
			}

			intlExp, intlEnvGot := probe(intl, "国际版")
			if intlExp != tc.wantIntl {
				t.Errorf("国际版经显式代理 %d 条，期望 %d 条\n显式代理记录：%v",
					intlExp, tc.wantIntl, px.urls())
			}
			if intlEnvGot != tc.intlEnv {
				t.Errorf("国际版经环境变量代理 %d 条，期望 %d 条（开了开关就必须走**显式**代理，"+
					"而不是回落环境变量）\n环境变量代理记录：%v",
					intlEnvGot, tc.intlEnv, envPx.urls())
			}
		})
	}
}
