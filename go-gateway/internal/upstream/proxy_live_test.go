//go:build live

// proxy_live_test.go 真实账号 + 本地假代理的**流量走向**实测
//（只在显式指定 live 构建标签时编译与运行）。
//
// 为什么必须走真实上游：「代理只对国际版生效」这条改动的风险不在代码分支，
// 而在**真实流量的走向** —— 国服流量若被绕进代理，用户侧的现象是「本来好好的
// 国服账号突然连不上」（代理挂掉时），而单测里所有假上游都是 127.0.0.1，
// 走不到真实域名，也就证不了这一点。
//
// 做法：起一个会记录 CONNECT 的本地假代理，把它配成网关的显式代理，
// 然后分别用真实国服/国际版账号各发一次真实请求，用**代理上的计数**判定：
//
//	国服   → 假代理 0 条
//	国际版 → 假代理 ≥1 条
//
// 运行方式（PowerShell）：
//
//	$env:WB_LIVE_ACCOUNTS = "C:\Users\...\.ai-gateway\accounts.json"   # 只读
//	$env:WB_LIVE_PROXY    = "1"
//	go test -tags live ./internal/upstream/ -run TestLiveProxySplit -v
//
// 账号库**只读**：从 WB_LIVE_ACCOUNTS 读入后转成网关凭证形态写入临时目录，
// 绝不回写源文件（源文件是所有者的生产账号库）。
//
// 安全边界：
//   - 只发**只读**请求（拉积分余额 / 拉模型列表），不做任何写操作；
//   - 假代理只记录**主机名与端口**（CONNECT 行里本来就只有这些），
//     不记录请求头、不记录 body —— 不会有 token 落盘；
//   - 报告只打印 uid 前 8 位与域名，不含昵称/邮箱。
package upstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// liveAcct 所有者账号库里的单条记录（snake_case 扁平形）。
type liveAcct struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	UID          string `json:"uid"`
	Domain       string `json:"domain"`
	ExpiresAt    int64  `json:"expiresAt"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// loadLiveAccts 读取账号库（只读）并各挑一个国服与国际版账号。
func loadLiveAccts(t *testing.T) (cn, intl *auth.Auth) {
	t.Helper()
	path := os.Getenv("WB_LIVE_ACCOUNTS")
	if path == "" {
		t.Skip("未设置 WB_LIVE_ACCOUNTS，跳过真实账号实测")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取账号库失败: %v", err)
	}
	var all []liveAcct
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatalf("解析账号库失败: %v", err)
	}

	// 各挑一个：国服取第一个，国际版取第一个。
	var pickCN, pickIntl *liveAcct
	for i := range all {
		p := &all[i]
		if p.AccessToken == "" || p.UID == "" {
			continue
		}
		if strings.HasSuffix(strings.ToLower(strings.TrimSpace(p.Domain)), ".ai") {
			if pickIntl == nil {
				pickIntl = p
			}
		} else if pickCN == nil {
			pickCN = p
		}
		if pickCN != nil && pickIntl != nil {
			break
		}
	}
	if pickCN == nil {
		t.Fatal("账号库里没有可用的国服账号")
	}
	if pickIntl == nil {
		t.Fatal("账号库里没有可用的国际版账号")
	}

	// 转成嵌套形写入临时目录，再由 auth.LoadDir 走生产解析路径。
	//
	// 文件名**必须**是 workbuddy*.json：auth.LoadDir 是按这个 glob 找文件的，
	// 换个前缀会「一个都加载不到」（现象是 uid 找不到，而不是解析报错）。
	dir := t.TempDir()
	write := func(p *liveAcct, n int) {
		doc := map[string]any{
			"auth": map[string]any{
				"accessToken":  p.AccessToken,
				"refreshToken": p.RefreshToken,
				"expiresAt":    p.ExpiresAt,
				"domain":       p.Domain,
			},
			"account": map[string]any{
				"uid":          p.UID,
				"enterpriseId": p.EnterpriseID,
				"nickname":     p.Nickname,
			},
		}
		enc, _ := json.MarshalIndent(doc, "", "  ")
		f := filepath.Join(dir, "workbuddy-probe"+itoa(n)+".json")
		if err := os.WriteFile(f, enc, 0o600); err != nil {
			t.Fatalf("写临时凭证失败: %v", err)
		}
	}
	write(pickCN, 1)
	write(pickIntl, 2)

	auths, err := auth.LoadDir(dir)
	if err != nil || len(auths) != 2 {
		t.Fatalf("加载临时凭证失败: %v (n=%d)", err, len(auths))
	}
	for _, a := range auths {
		switch a.UID {
		case pickCN.UID:
			cn = a
		case pickIntl.UID:
			intl = a
		}
	}
	if cn == nil || intl == nil {
		t.Fatalf("临时目录里没找齐两个账号（cn=%v intl=%v）", cn != nil, intl != nil)
	}
	return cn, intl
}

// itoa 小整数转字符串（避免为一行拼接引入 strconv 之外的东西）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}


// shortUIDLive 脱敏：只保留 uid 前 8 位（报告里不出现完整 uid）。
func shortUIDLive(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// TestLiveProxySplit 真实账号实测：国服 0 条经代理、国际版 ≥1 条经代理。
//
// 用假代理而不是本机既有的 7890：本机代理是所有者正在用的东西，
// 我们既不能依赖它的状态，也不该往他的代理日志里灌测试流量。
func TestLiveProxySplit(t *testing.T) {
	if os.Getenv("WB_LIVE_PROXY") != "1" {
		t.Skip("未设置 WB_LIVE_PROXY=1，跳过真实账号代理分流实测")
	}
	cn, intl := loadLiveAccts(t)
	t.Logf("选中账号：cn uid8=%s domain=%s / intl uid8=%s domain=%s",
		shortUIDLive(cn.UID), cn.Domain, shortUIDLive(intl.UID), intl.Domain)

	px := newFakeProxy(t)
	c := New()
	if err := c.SetProxy(px.srv.URL); err != nil {
		t.Fatalf("SetProxy(%s): %v", px.srv.URL, err)
	}
	t.Logf("网关显式代理 = %s（本地假代理，只记录 CONNECT 主机名）", px.srv.URL)

	// 只读探针：billing 域拉积分余额。
	// 选它的原因：国服与国际版**都要**走 billingBase（国服 codebuddy.cn、
	// 国际版 workbuddy.ai），端点形状一致，两边都能打真实上游、且不改任何状态。
	probe := func(a *auth.Auth, label string) {
		before := px.count()
		info, err := c.UserResourceDetail(a)
		after := px.count()
		viaProxy := after - before

		// 401/403 也是有效结论：说明请求真的到了上游并拿到 http 响应；
		// 我们关心的是**流量走向**而不是业务成功。
		t.Logf("%s uid8=%s domain=%s：经代理 %d 条，remain=%d err=%v",
			label, shortUIDLive(a.UID), a.Domain, viaProxy, info.Remain, err)

		if label == "国服" && viaProxy != 0 {
			t.Fatalf("国服请求经代理 %d 条（必须为 0）\n代理记录：%v", viaProxy, px.urls())
		}
		if label == "国际版" && viaProxy == 0 {
			t.Fatalf("国际版请求未经代理（必须 ≥1）\n代理记录：%v", px.urls())
		}
	}

	probe(cn, "国服")
	probe(intl, "国际版")

	t.Logf("实测结论（代理收到的全部请求）：%v", px.urls())
	t.Logf("国服总经代理条数=0（断言通过）；国际版经代理条数≥1（断言通过）")

	// 确认探针确实打的是真实域名（而不是被测试基址顶替成 127.0.0.1）。
	if !strings.Contains(c.billingBase(cn), "codebuddy.cn") {
		t.Fatalf("国服 billingBase=%q 不是真实域名", c.billingBase(cn))
	}
	if !strings.Contains(c.billingBase(intl), "workbuddy.ai") {
		t.Fatalf("国际版 billingBase=%q 不是真实域名", c.billingBase(intl))
	}

	// 聊天通道的国际版 client 必须是代理那一个（只断言 client 身份，
	// 不发聊天请求 —— 聊天会消耗真实额度）。
	//
	// **必须在清空代理之前做**：SetProxy("") 会把 intlChatHTTP 归空，
	// 之后两个区域按设计都回落 ChatHTTP，再比就永远是同一个（本人踩过）。
	if c.chatClientFor(intl) == nil || c.chatClientFor(cn) == nil {
		t.Fatal("chat client 不应为 nil")
	}
	if c.chatClientFor(intl) == c.chatClientFor(cn) {
		t.Fatal("国际版与国服的聊天 client 是同一个 —— 聊天流量未分流")
	}
	if c.httpFor(intl) == c.httpFor(cn) {
		t.Fatal("国际版与国服的短 RPC client 是同一个 —— 短 RPC 流量未分流")
	}
	t.Logf("短 RPC / 聊天通道的区域分流均已生效（client 身份不同）")

	// 反向验证：把代理清空后，国际版也不能再经过它。
	// 否则「清空代理」在界面上看起来生效、实际仍走旧代理。
	if err := c.SetProxy(""); err != nil {
		t.Fatal(err)
	}
	before := px.count()
	_, _ = c.UserResourceDetail(intl)
	if n := px.count() - before; n != 0 {
		t.Fatalf("清空代理后国际版仍经代理 %d 条 —— 清空无效", n)
	}
	// 清空之后两区必须回到「共用同一个 client」= 未配代理时的既有行为。
	if c.httpFor(intl) != c.httpFor(cn) {
		t.Fatal("清空代理后两区仍走不同 client —— 未配代理时的行为被改变了")
	}
	t.Logf("清空代理后国际版经代理条数=0，且两区回落同一个直连 client（断言通过）")
}
