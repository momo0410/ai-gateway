package upstream

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// ---------------------------------------------------------------------------
// 出站代理支持（**仅国际版账号**）
//
// 背景（实机排查）：国际版 workbuddy.ai 在国内直连不稳定（实测 wsarecv 超时），
// 走代理才稳。而 Go 的 http.ProxyFromEnvironment **只读环境变量**、不读 Windows
// 注册表，所以「浏览器能走系统代理」不代表网关也能 —— 必须显式配置。
//
// 演变（2026-09-17，所有者诉求「代理只对国际版生效，别让国内也走代理流量」）：
// 改动前 SetProxy 把同一个 Transport 同时赋给 HTTP 与 ChatHTTP，于是**所有**
// 账号（含国服）的出站请求都走代理。现在按账号区域分流：
//
//	国际版（*.ai） → intlHTTP / intlChatHTTP，挂显式代理
//	国服           → HTTP / ChatHTTP，挂 ProxyFromEnvironment（= 既有默认行为）
//
// 这也是本文件里两条老断言被改写的原因（见 TestSetProxyAppliesToIntlClientsOnly
// 与 TestSetProxyKeepsPerRegionTransportShared 的注释）。
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// 假代理：**真的**记录经过它的请求
//
// 为什么不能只断言「transport 上挂了代理函数」：那只证明配置写对了，不证明
// 请求真的走了它。本次改动的风险恰恰在「国服流量绕道 / 国际版漏走代理」——
// 两者都表现为指针指对了但实际流量走错。所以这里起一个真实的 HTTP 代理监听，
// 由它把请求转发给假上游，用**计数**来证明流量走向。
// ---------------------------------------------------------------------------

// fakeProxy 一个最小可用的 HTTP 正向代理。
//
// 两种形态都支持，因为两种测试需要不同的东西：
//   - 明文 http:// 请求（单测）：代理拿到的是**绝对**形式的 URL；
//   - CONNECT 隧道（真实账号实测，live 标签）：上游全是 https，
//     只有隧道才能把真实流量引过来并计数。
type fakeProxy struct {
	srv *httptest.Server

	mu    sync.Mutex
	lines []string // 每条经过代理的请求（明文为 method+URL，隧道为 CONNECT host:port）
}

func newFakeProxy(t *testing.T) *fakeProxy {
	t.Helper()
	p := &fakeProxy{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			p.record("CONNECT " + r.Host)
			p.tunnel(w, r)
			return
		}
		// r.URL 在正向代理场景下是**绝对**形式（http://host/path），
		// 这正是「经代理」与「直连」最可靠的区分点：直连时 URL 只有 path。
		p.record(r.Method + " " + r.URL.String())

		fwd, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		fwd.Header = r.Header.Clone()
		fwd.Header.Del("Proxy-Connection")
		fwd.ContentLength = r.ContentLength
		resp, err := http.DefaultTransport.RoundTrip(fwd)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeProxy) record(line string) {
	p.mu.Lock()
	p.lines = append(p.lines, line)
	p.mu.Unlock()
}

// tunnel 处理 CONNECT：连到目标主机后双向透传（真实 HTTPS 隧道）。
//
// 必须真的转发，不能只记一笔就拒绝 —— 那样「国际版经代理」的用例会以传输错误
// 收场，分不清是路由错了还是代理不给过。
func (p *fakeProxy) tunnel(w http.ResponseWriter, r *http.Request) {
	dst, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		dst.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		dst.Close()
		return
	}
	defer conn.Close()
	defer dst.Close()
	if _, err := buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buf.Flush(); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(dst, conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, dst); done <- struct{}{} }()
	<-done
	// 让第二条 copy 也能退出：关掉两侧读端会打断它。
	_ = conn.SetDeadline(time.Now())
	_ = dst.SetDeadline(time.Now())
}

// count 经过代理的请求数。
func (p *fakeProxy) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.lines)
}

// urls 经过代理的请求行快照。
func (p *fakeProxy) urls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.lines...)
}

// fakeUpstream 假上游：/v3/config 之外一律回一个万物通用的成功信封。
//
// 一个响应体同时满足 doJSON 系列的解析（code=0 + data）与 FetchModels
// （data.agents[name=cli].models）——避免每个用例各起一个上游，
// 也让「经代理」与「直连」两条路径打到**同一个**上游，计数才有可比性。
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	const body = `{"code":0,"msg":"ok","data":{
		"agents":[{"name":"cli","models":["probe-model"]}],
		"models":[{"id":"probe-model","name":"probe","maxInputTokens":128,"maxOutputTokens":64}]
	}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// routableClient 把三个基址全部指向同一个假上游的 client。
//
// **三个基址指向同一个 host 是刻意的**：这样「国际版走代理、国服直连」只能由
// 账号区域（Auth.Domain）决定，不能由目标域名决定 —— 若哪天有人把分流改成
// 「按 URL host 判断」，本用例会立刻变红。
func routableClient(t *testing.T, up string) *Client {
	t.Helper()
	return &Client{
		HTTP:          &http.Client{},
		SanitizeFingerprints: true,
		ChatBaseCN:    up,
		BillingBaseCN: up,
		BaseIntl:      up,
		WebBaseCN:     up,
		WebBaseIntl:   up,
	}
}

func cnAuth() *auth.Auth {
	return &auth.Auth{UID: "cn-acct", AccessToken: "at", Domain: "www.workbuddy.cn"}
}

func intlAuth() *auth.Auth {
	return &auth.Auth{UID: "intl-acct", AccessToken: "at", Domain: "www.workbuddy.ai"}
}

// ---------------------------------------------------------------------------
// 核心：配了代理时，国服请求**真的**不过代理、国际版请求**真的**过代理
// ---------------------------------------------------------------------------

// TestProxyRoutesIntlThroughProxyAndCNDirect 四个出站通道逐一验证流量走向。
//
// 这是本次改动最重要的一条用例：断言的是**假代理收到的请求计数**，
// 而不是 transport 上的指针。
func TestProxyRoutesIntlThroughProxyAndCNDirect(t *testing.T) {
	up := fakeUpstream(t)
	px := newFakeProxy(t)

	// 假上游与假代理都经 SetProxy 之后才生效：顺序与 main.go 一致
	//（New → SetProxy → 调优），避免测出一个生产不会出现的状态。
	c := routableClient(t, up.URL)
	c.ChatHTTP = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	if err := c.SetProxy(px.srv.URL); err != nil {
		t.Fatalf("SetProxy: %v", err)
	}

	cases := []struct {
		name string
		// call 发一次该通道的出站请求；返回 error 仅作参考（假上游不保证语义成功）
		call func(a *auth.Auth) error
	}{
		{"doJSON/领奖(webBase→ClaimGrowthTask)", func(a *auth.Auth) error {
			_, _, _, err := c.ClaimGrowthTask(a, "chat_5")
			return err
		}},
		{"doJSON/Web领奖链路上报(webBase→ReportWebEvent)", func(a *auth.Auth) error {
			return c.ReportWebEvent(a, "web_element_click", up.URL+"/x", "el", "name")
		}},
		{"doJSON/billing(UserResourceDetail)", func(a *auth.Auth) error {
			_, err := c.UserResourceDetail(a)
			return err
		}},
		{"doJSON/growth(TravelStatus)", func(a *auth.Auth) error {
			_, err := c.TravelStatus(a)
			return err
		}},
		{"FetchModels(chatBase)", func(a *auth.Auth) error {
			_, err := c.FetchModels(a)
			return err
		}},
		{"ChatStream(聊天 SSE)", func(a *auth.Auth) error {
			rc, status, _, err := c.ChatStream(a, []byte(`{"model":"m"}`))
			if rc != nil {
				rc.Close()
			}
			if err == nil && status >= 400 {
				return io.EOF // 只为走到 Do；非 2xx 不影响本次断言
			}
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := px.count()

			// 国际版：必须经代理
			if err := tc.call(intlAuth()); err != nil {
				t.Fatalf("国际版请求本身失败（应能经假代理打到假上游）: %v", err)
			}
			if got := px.count() - before; got != 1 {
				t.Fatalf("国际版：假代理收到 %d 条请求，期望 1 条\n经过代理的请求：%v",
					got, px.urls())
			}

			mid := px.count()
			// 国服：一条都不能经代理
			if err := tc.call(cnAuth()); err != nil {
				t.Fatalf("国服请求本身失败: %v", err)
			}
			if got := px.count() - mid; got != 0 {
				t.Fatalf("国服：假代理收到 %d 条请求，期望 0 条（国服必须直连）\n经过代理的请求：%v",
					got, px.urls())
			}
		})
	}
}

// TestProxyCNRequestStillReachesUpstreamDirectly 国服直连不是「请求根本没发出去」。
//
// 只断言「假代理计数为 0」有个致命盲区：请求可能因为 transport 配错而彻底失败，
// 那样计数同样是 0，看起来却像「正确分流」。所以这里用一个**自建上游**统计
// 真实到达的请求，证明国服的流量确实打到了目标主机。
func TestProxyCNRequestStillReachesUpstreamDirectly(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{}}`)
	}))
	defer up.Close()

	px := newFakeProxy(t)
	c := routableClient(t, up.URL)
	if err := c.SetProxy(px.srv.URL); err != nil {
		t.Fatal(err)
	}

	if err := c.ReportChatActivity(cnAuth(), "conv", "req"); err != nil {
		t.Fatalf("国服请求应能直连到上游: %v", err)
	}
	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 1 {
		t.Fatalf("上游直连收到 %d 条，期望 1 条", got)
	}
	if n := px.count(); n != 0 {
		t.Fatalf("国服请求不该经过代理，假代理却收到 %d 条: %v", n, px.urls())
	}
}

// TestProxyCNRequestReachesUpstreamThroughProxyPath 国际版经代理后确实**转发到了上游**。
//
// 与上一条对称：证明「经代理」不是「请求被代理吞掉」。
func TestProxyCNRequestReachesUpstreamThroughProxyPath(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{}}`)
	}))
	defer up.Close()

	px := newFakeProxy(t)
	c := routableClient(t, up.URL)
	if err := c.SetProxy(px.srv.URL); err != nil {
		t.Fatal(err)
	}

	if err := c.ReportChatActivity(intlAuth(), "conv", "req"); err != nil {
		t.Fatalf("国际版请求应能经代理转发到上游: %v", err)
	}
	if n := px.count(); n != 1 {
		t.Fatalf("国际版应经代理 1 条，实际 %d 条", n)
	}
	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 1 {
		t.Fatalf("经代理转发后上游收到 %d 条，期望 1 条（代理没有把请求送到上游）", got)
	}
}

// TestNoProxyConfiguredNeitherRegionUsesProxy 未配代理时两区都不经代理。
//
// 硬约束：不改变未配代理用户的既有行为 —— 即升级前后都是「全部直连」。
// 这里国际版也断言 0 条。之所以在这个环境里是确定性的：假上游在 127.0.0.1 上，
// 而 ProxyFromEnvironment 永不代理 loopback（httpproxy 的既有规则），
// 因此即使开发机设了 HTTPS_PROXY，本用例也不会因环境而变红。
func TestNoProxyConfiguredNeitherRegionUsesProxy(t *testing.T) {
	up := fakeUpstream(t)
	px := newFakeProxy(t)

	c := routableClient(t, up.URL)
	c.ChatHTTP = &http.Client{}
	// 显式给一个**不含代理**的状态：New() 的默认态。
	// SetProxy("") 是最贴近运行时的路径（宿主 cfg.Proxy 为空时根本不调 SetProxy，
	// 但两条路径都必须等价，这里走一遍以确保清空逻辑不会引入代理）。
	if err := c.SetProxy(""); err != nil {
		t.Fatal(err)
	}

	for _, a := range []*auth.Auth{cnAuth(), intlAuth()} {
		if err := c.ReportChatActivity(a, "conv", "req"); err != nil {
			t.Fatalf("uid=%s 直连失败: %v", a.UID, err)
		}
		_, _, _, err := c.ClaimGrowthTask(a, "chat_5")
		if err != nil {
			t.Fatalf("uid=%s 领奖直连失败: %v", a.UID, err)
		}
	}
	if n := px.count(); n != 0 {
		t.Fatalf("未配代理时不该有任何请求经过代理，实际 %d 条: %v", n, px.urls())
	}
}

// TestUnsetProxyClearsIntlTransport 清空代理后国际版不得继续走旧代理。
//
// 否则表现为「界面里把代理清掉了，国际版却仍在走它」——用户会认为清空无效。
func TestUnsetProxyClearsIntlTransport(t *testing.T) {
	up := fakeUpstream(t)
	px := newFakeProxy(t)

	c := routableClient(t, up.URL)
	if err := c.SetProxy(px.srv.URL); err != nil {
		t.Fatal(err)
	}
	if c.intlHTTP == nil || c.intlChatHTTP == nil {
		t.Fatal("配了代理后国际版应有独立的 client")
	}
	if err := c.SetProxy("", ); err != nil {
		t.Fatal(err)
	}
	if c.intlHTTP != nil || c.intlChatHTTP != nil {
		t.Error("清空代理后国际版 client 必须归空，否则会继续走旧代理")
	}
	// 清空后再发一次国际版请求：不得经过代理。
	if err := c.ReportChatActivity(intlAuth(), "conv", "req"); err != nil {
		t.Fatalf("清空后国际版直连失败: %v", err)
	}
	if n := px.count(); n != 0 {
		t.Fatalf("清空代理后国际版不该经过代理，实际 %d 条: %v", n, px.urls())
	}
}

// TestProxyCountsAreFromProxyNotUpstream 防伪：假代理确实在计数。
//
// 若假代理本身坏掉（永远返回 502 且不计数），上面所有「0 条」的断言都会
// 假绿。这里直接对假代理发一次请求，确认计数机制本身可用。
func TestProxyCountsAreFromProxyNotUpstream(t *testing.T) {
	up := fakeUpstream(t)
	px := newFakeProxy(t)
	if px.count() != 0 {
		t.Fatal("初始计数应为 0")
	}
	// 走一次**显式**代理（相当于国际版那条路），确认代理会记录并成功转发。
	c := routableClient(t, up.URL)
	if err := c.SetProxy(px.srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := c.ReportChatActivity(intlAuth(), "conv", "req"); err != nil {
		t.Fatal(err)
	}
	if px.count() != 1 {
		t.Fatalf("假代理应记到 1 条，实际 %d —— 计数机制失效，其它用例的 0 条断言不可信", px.count())
	}
	// 请求行必须是**绝对**形式（正向代理的特征），进一步证明流量真的走了代理。
	if u := px.urls()[0]; !strings.HasPrefix(u, "POST http://") {
		t.Fatalf("代理收到的请求行不是绝对形式: %q", u)
	}
}

// ---------------------------------------------------------------------------
// transport 布局：区域之间隔离、区域内部共享
// ---------------------------------------------------------------------------

// TestSetProxyAppliesToIntlClientsOnly 代理只挂在国际版 client 上。
//
// **原断言 TestSetProxyAppliesToBothClients 已被本用例取代**，原因：
// 它断言 HTTP 与 ChatHTTP 两个 client 都挂显式代理 —— 那正是本次要消除的行为
// （等于所有账号都走代理）。语义反了就不能「放宽」，只能换被测对象：
// 现在要断言的是「国际版一对挂代理」「国服一对**不**挂显式代理」。
func TestSetProxyAppliesToIntlClientsOnly(t *testing.T) {
	c := New()
	if err := c.SetProxy("http://127.0.0.1:7890"); err != nil {
		t.Fatalf("设置代理失败: %v", err)
	}
	if c.intlHTTP == nil || c.intlChatHTTP == nil {
		t.Fatal("配了代理后必须存在国际版专用 client")
	}

	// 国际版两个 client：必须解析出显式代理。
	for _, tc := range []struct {
		name   string
		client *http.Client
	}{{"intlHTTP", c.intlHTTP}, {"intlChatHTTP", c.intlChatHTTP}} {
		tr, ok := tc.client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s 的 Transport 类型=%T", tc.name, tc.client.Transport)
		}
		if tr.Proxy == nil {
			t.Fatalf("%s 未设置 Proxy 函数", tc.name)
		}
		req, _ := http.NewRequest("GET", "https://www.workbuddy.ai/v2/chat/completions", nil)
		got, err := tr.Proxy(req)
		if err != nil {
			t.Fatalf("%s 解析代理失败: %v", tc.name, err)
		}
		if got == nil || got.String() != "http://127.0.0.1:7890" {
			t.Fatalf("%s 代理=%v，期望 http://127.0.0.1:7890", tc.name, got)
		}
	}

	// 国服两个 client：不得解析出那个显式代理（保持 ProxyFromEnvironment 语义）。
	for _, tc := range []struct {
		name   string
		client *http.Client
	}{{"HTTP", c.HTTP}, {"ChatHTTP", c.ChatHTTP}} {
		tr, ok := tc.client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s 的 Transport 类型=%T", tc.name, tc.client.Transport)
		}
		if tr.Proxy == nil {
			t.Fatalf("%s 仍应有 Proxy 函数（保持 ProxyFromEnvironment，尊重 HTTPS_PROXY）", tc.name)
		}
		req, _ := http.NewRequest("GET", "https://copilot.tencent.com/v2/chat/completions", nil)
		got, err := tr.Proxy(req)
		if err != nil {
			t.Fatalf("%s 解析代理失败: %v", tc.name, err)
		}
		// 与标准库 ProxyFromEnvironment 逐字一致 = 国服仍是「既有默认行为」。
		want, werr := http.ProxyFromEnvironment(req)
		if werr != nil {
			t.Fatalf("ProxyFromEnvironment 报错: %v", werr)
		}
		if (got == nil) != (want == nil) {
			t.Fatalf("%s 与 ProxyFromEnvironment 不一致：got=%v want=%v", tc.name, got, want)
		}
		if got != nil {
			if got.String() == "http://127.0.0.1:7890" {
				t.Fatalf("%s 吃到了显式代理 %s —— 国服流量被绕进代理了", tc.name, got)
			}
			if want != nil && got.String() != want.String() {
				t.Fatalf("%s 与 ProxyFromEnvironment 不一致：got=%v want=%v", tc.name, got, want)
			}
		}
	}
}

// TestSetProxyKeepsPerRegionTransportShared 区域内部共享 transport、区域之间隔离。
//
// **原断言 TestSetProxyKeepsTransportShared 已被本用例取代**，原因：
// 它断言设代理后 ChatHTTP 与 HTTP 仍共享同一个 Transport —— 该断言本身没错，
// 但只覆盖了一半，于是「国际版与国服共用同一个 transport」这个**真正的缺陷**
// 在它的保护下安然存在（共享是它要求的）。现在把语义补全成两条：
//   - 同一区域内 HTTP 与 ChatHTTP 共享（连接池不重复，原意保留）；
//   - 国服与国际版**必须不共享**（各挂各的代理，物理隔离）。
func TestSetProxyKeepsPerRegionTransportShared(t *testing.T) {
	c := New()
	if err := c.SetProxy("http://127.0.0.1:7890"); err != nil {
		t.Fatal(err)
	}
	// 区域内共享（原断言的原意）。
	if c.ChatHTTP.Transport != c.HTTP.Transport {
		t.Error("设代理后 ChatHTTP 与 HTTP 仍须共享同一个 Transport")
	}
	if c.intlChatHTTP.Transport != c.intlHTTP.Transport {
		t.Error("设代理后 intlChatHTTP 与 intlHTTP 仍须共享同一个 Transport")
	}
	// 区域间隔离（新增的硬要求）。
	if c.intlHTTP.Transport == c.HTTP.Transport {
		t.Error("国际版与国服不得共用 Transport —— 共用会让所有账号走同一条代理")
	}
	// 总时长的差异必须保持：ChatHTTP 无总时长，靠 ResponseHeaderTimeout 兜底。
	if c.ChatHTTP.Timeout != 0 {
		t.Errorf("ChatHTTP.Timeout=%v 应为 0", c.ChatHTTP.Timeout)
	}
	if c.HTTP.Timeout == 0 {
		t.Error("HTTP.Timeout 不应为 0（短 RPC 需要总时长上限）")
	}
	// 国际版必须继承同样的时长语义。
	if c.intlChatHTTP.Timeout != 0 {
		t.Errorf("intlChatHTTP.Timeout=%v 应为 0", c.intlChatHTTP.Timeout)
	}
	if c.intlHTTP.Timeout != c.HTTP.Timeout {
		t.Errorf("intlHTTP.Timeout=%v 应与 HTTP.Timeout=%v 一致", c.intlHTTP.Timeout, c.HTTP.Timeout)
	}
}

// TestSetRPCTimeoutAndHeaderTimeoutCoverBothRegions 调优必须覆盖两个区域。
//
// 漏掉国际版那一个的表现是「国服按新上限超时、国际版仍按 120s 干等」，
// 两边日志长得一样，从现象上几乎发现不了。
func TestSetRPCTimeoutAndHeaderTimeoutCoverBothRegions(t *testing.T) {
	c := New()
	if err := c.SetProxy("http://127.0.0.1:7890"); err != nil {
		t.Fatal(err)
	}
	c.SetRPCTimeout(7 * 1e9) // 7s
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

// TestSetProxyTolerateHostPortOnly 用户常只填 host:port，应自动补 http:// 前缀。
func TestSetProxyTolerateHostPortOnly(t *testing.T) {
	c := New()
	if err := c.SetProxy("127.0.0.1:7890"); err != nil {
		t.Fatalf("应容忍无 scheme 的写法: %v", err)
	}
	// 显式代理只作用于国际版，故这里看 intl 那一套。
	tr := c.intlChatHTTP.Transport.(*http.Transport)
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	got, _ := tr.Proxy(req)
	if got == nil || got.Host != "127.0.0.1:7890" {
		t.Fatalf("代理 host=%v，期望 127.0.0.1:7890", got)
	}
}

// TestSetProxyEmptyFallsBackToEnv 空串 = 不用显式代理，回落环境变量（保持旧行为）。
func TestSetProxyEmptyFallsBackToEnv(t *testing.T) {
	c := New()
	// 先设一个显式代理，再清空 —— 这样才有一个**确定性**可断言的对象：
	// 「清空之后不得再指向刚设的那个地址」。
	//
	// 不能用 t.Setenv 清空 HTTPS_PROXY 后断言「必然直连」：httpproxy 的配置在
	// 进程内**首次读取即缓存**（net/http 的 envProxyOnce），t.Setenv 无法生效；
	// 而且开发机常有 HTTP_PROXY/HTTPS_PROXY（实测 127.0.0.1:7897），
	// 断言「直连」会让用例依赖运行环境（CI 绿、本机红）。
	const explicit = "http://127.0.0.1:1"
	if err := c.SetProxy(explicit); err != nil {
		t.Fatal(err)
	}
	if c.ProxyURL() != explicit {
		t.Fatalf("ProxyURL()=%q，应为 %q", c.ProxyURL(), explicit)
	}
	if err := c.SetProxy(""); err != nil {
		t.Fatal(err)
	}
	if c.ProxyURL() != "" {
		t.Errorf("空串时 ProxyURL()=%q，应为空", c.ProxyURL())
	}
	tr := c.ChatHTTP.Transport.(*http.Transport)
	if tr.Proxy == nil {
		t.Fatal("仍应有 Proxy 函数（回落 ProxyFromEnvironment）")
	}
	req, _ := http.NewRequest("GET", "https://www.workbuddy.ai", nil)
	got, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy(req) 报错: %v", err)
	}
	if got != nil && got.String() == explicit {
		t.Fatalf("清空后仍指向刚设的显式代理 %s —— SetProxy(\"\") 没有回落环境变量", explicit)
	}
	// 与标准库行为一致即可（环境变量可能有值，且被首次读取缓存，故不断言必然为 nil）。
	want, werr := http.ProxyFromEnvironment(req)
	if werr != nil {
		t.Fatalf("ProxyFromEnvironment 报错: %v", werr)
	}
	if (got == nil) != (want == nil) {
		t.Errorf("空串应回落 ProxyFromEnvironment：got=%v want=%v", got, want)
	} else if got != nil && got.String() != want.String() {
		t.Errorf("空串应回落 ProxyFromEnvironment：got=%v want=%v", got, want)
	}
}

// TestSetProxyRejectsInvalid 非法地址必须报错（而不是静默直连）。
func TestSetProxyRejectsInvalid(t *testing.T) {
	c := New()
	for _, bad := range []string{"http://", "://nohost", "http://:8080"} {
		if err := c.SetProxy(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

// TestSetProxyAcceptsSocks5 socks5 代理地址应被接受（用户在 Clash 等里可能用它）。
func TestSetProxyAcceptsSocks5(t *testing.T) {
	c := New()
	if err := c.SetProxy("socks5://127.0.0.1:7891"); err != nil {
		t.Fatalf("socks5 地址应可解析: %v", err)
	}
	if c.ProxyURL() != "socks5://127.0.0.1:7891" {
		t.Errorf("ProxyURL()=%q", c.ProxyURL())
	}
	tr := c.intlChatHTTP.Transport.(*http.Transport)
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	got, _ := tr.Proxy(req)
	if got == nil || got.Scheme != "socks5" {
		t.Fatalf("scheme=%v，期望 socks5", got)
	}
	// 国服侧不得出现 socks5。
	cnReq, _ := http.NewRequest("GET", "https://copilot.tencent.com", nil)
	if got, _ := c.ChatHTTP.Transport.(*http.Transport).Proxy(cnReq); got != nil && got.Scheme == "socks5" {
		t.Fatalf("国服吃到了 socks5 代理 %v", got)
	}
}

// TestNewDefaultsToEnvProxy 未调 SetProxy 时，New() 的 Transport 应已挂
// ProxyFromEnvironment（尊重 HTTPS_PROXY，行为与改动前一致）。
//
// 注意：Go 的 httpproxy 配置在**首次读取时缓存**（internal/httpproxy 的
// onceClose 语义），t.Setenv 之后就生效需要重置缓存。这里直接用
// http.ProxyURL 与 ProxyFromEnvironment 的等价性断言，不依赖缓存时序：
// 只要 Transport.Proxy 非 nil 且是 ProxyFromEnvironment 的返回值语义即可。
func TestNewDefaultsToEnvProxy(t *testing.T) {
	c := New()
	tr := c.ChatHTTP.Transport.(*http.Transport)
	if tr.Proxy == nil {
		t.Fatal("New() 应默认挂 ProxyFromEnvironment（而非 nil）")
	}
	// 与标准库的 ProxyFromEnvironment 行为一致性：同一个请求应得到同样结果。
	// （直接函数值比较不可靠，因为 http.Transport 可能包装过。）
	req, _ := http.NewRequest("GET", "https://www.workbuddy.ai", nil)
	got, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy(req) 报错: %v", err)
	}
	want, werr := http.ProxyFromEnvironment(req)
	if werr != nil {
		t.Fatalf("ProxyFromEnvironment 报错: %v", werr)
	}
	if (got == nil) != (want == nil) {
		t.Errorf("与 ProxyFromEnvironment 行为不一致：got=%v want=%v", got, want)
	}
	if got != nil && want != nil && got.String() != want.String() {
		t.Errorf("与 ProxyFromEnvironment 结果不一致：got=%v want=%v", got, want)
	}
	// 未配代理时不得存在国际版专用 client：否则 httpFor 分流到一套没人配过的
	// transport（行为虽等价，但说明 New() 与 SetProxy 的状态机不一致）。
	if c.intlHTTP != nil || c.intlChatHTTP != nil {
		t.Error("未调 SetProxy 时不应存在国际版专用 client")
	}
}

// ---------------------------------------------------------------------------
// 区域判据与 client 选择器的对应关系
// ---------------------------------------------------------------------------

// TestClientSelectorPairsWithRegionBases 区域 → (基址, client) 必须是同一套判据。
//
// 这条用例锁的是**两类判据不能各写一份**的约束：基址由 chatBase/billingBase/
// webBase 决定，transport 由 httpFor/chatClientFor 决定，两边都看 Auth.Domain。
// 若哪天有人只改了其中一边（比如基址按 domain 后缀、client 按别的新字段），
// 就会出现「请求打到国服域名却从代理出站」这种最难查的组合。
func TestClientSelectorPairsWithRegionBases(t *testing.T) {
	up := fakeUpstream(t)
	px := newFakeProxy(t)

	c := routableClient(t, up.URL)
	c.ChatHTTP = &http.Client{}
	if err := c.SetProxy(px.srv.URL); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		a         *auth.Auth
		wantProxy bool
	}{
		{"国服(workbuddy.cn)", &auth.Auth{Domain: "www.workbuddy.cn"}, false},
		{"国服(codebuddy.cn)", &auth.Auth{Domain: "www.codebuddy.cn"}, false},
		{"domain 缺失按国服", &auth.Auth{}, false},
		{"国际版(workbuddy.ai)", &auth.Auth{Domain: "www.workbuddy.ai"}, true},
		{"国际版(codebuddy.ai)", &auth.Auth{Domain: "www.codebuddy.ai"}, true},
		{"国际版大小写不敏感", &auth.Auth{Domain: "WWW.WorkBuddy.AI"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			intl := tc.wantProxy

			// 1) 三个基址的判据
			bases := map[string]string{
				"chatBase":    c.chatBase(tc.a),
				"billingBase": c.billingBase(tc.a),
				"webBase":     c.webBase(tc.a),
			}
			// 本用例里两区基址**同为假上游**，故不能靠 URL 区分；
			// 改为直接断言 isIntl（基址函数用的就是它）与 client 选择一致。
			if isIntl(tc.a) != intl {
				t.Fatalf("isIntl=%v 与期望 %v 不符（基址会被选错）", isIntl(tc.a), intl)
			}
			for name, b := range bases {
				if b == "" {
					t.Fatalf("%s 返回空基址", name)
				}
			}

			// 2) transport 选择必须与 1) 同源
			before := px.count()
			if err := c.ReportChatActivity(tc.a, "conv", "req"); err != nil {
				t.Fatalf("请求失败: %v", err)
			}
			viaProxy := px.count() - before
			if intl && viaProxy != 1 {
				t.Fatalf("国际版应经代理，实际 %d 条", viaProxy)
			}
			if !intl && viaProxy != 0 {
				t.Fatalf("国服应直连，实际经代理 %d 条", viaProxy)
			}
		})
	}
}

// TestDefaultBasesUnchanged 默认基址不得因本次改动而变（回归护栏）。
func TestDefaultBasesUnchanged(t *testing.T) {
	c := New()
	cn := &auth.Auth{Domain: "www.workbuddy.cn"}
	intl := &auth.Auth{Domain: "www.workbuddy.ai"}
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"chatBase(cn)", c.chatBase(cn), "https://copilot.tencent.com"},
		{"billingBase(cn)", c.billingBase(cn), "https://www.codebuddy.cn"},
		{"webBase(cn)", c.webBase(cn), "https://www.workbuddy.cn"},
		{"chatBase(intl)", c.chatBase(intl), "https://www.workbuddy.ai"},
		{"billingBase(intl)", c.billingBase(intl), "https://www.workbuddy.ai"},
		{"webBase(intl)", c.webBase(intl), "https://www.workbuddy.ai"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s=%q want %q", tc.name, tc.got, tc.want)
		}
	}
	// 未配代理时两区共用 HTTP/ChatHTTP（= 改动前的行为）。
	if c.httpFor(cn) != c.HTTP || c.httpFor(intl) != c.HTTP {
		t.Error("未配代理时两区都应走 HTTP")
	}
	if c.chatClientFor(cn) != c.ChatHTTP || c.chatClientFor(intl) != c.ChatHTTP {
		t.Error("未配代理时两区都应走 ChatHTTP")
	}
}

// TestProxyURLRoundTrip 代理地址解析需能被 url 正常解析（socks5 等 scheme 无副作用）。
func TestProxyURLRoundTrip(t *testing.T) {
	c := New()
	if err := c.SetProxy("http://user:pass@127.0.0.1:7890"); err != nil {
		t.Fatal(err)
	}
	tr := c.intlHTTP.Transport.(*http.Transport)
	req, _ := http.NewRequest("GET", "https://www.workbuddy.ai", nil)
	got, err := tr.Proxy(req)
	if err != nil || got == nil {
		t.Fatalf("带认证的代理应可用: got=%v err=%v", got, err)
	}
	if got.User == nil || got.User.Username() != "user" {
		t.Fatalf("代理认证信息丢失: %v", got)
	}
	if _, ok := got.User.Password(); !ok {
		t.Error("代理密码丢失")
	}
	// 确保仍是可用的 *url.URL
	if _, err := url.Parse(got.String()); err != nil {
		t.Fatalf("代理地址不可解析: %v", err)
	}
}

// ---------------------------------------------------------------------------
// HTTPS_PROXY 语义：国服的「直连 transport」到底会不会吃环境变量代理
//
// 这是本次改动里唯一有争议的取舍，所以要有可复现的实测，而不是靠读标准库源码下结论。
// ---------------------------------------------------------------------------

// envProbeChild 子进程标记：非空时本进程只跑探测并退出。
const envProbeChild = "WB_PROXY_ENV_PROBE"

// envProbeExplicit 传给子进程的「用户显式配置的代理」。
const envProbeExplicit = "WB_PROXY_EXPLICIT"

// TestEnvProxyAppliesToCNViaEnvironment 实测：进程启动时带 HTTPS_PROXY、
// 且用户显式配了代理时，国服的直连 transport 究竟还认不认环境变量。
//
// **为什么必须起子进程**：Go 的 httpproxy 在进程内**首次读取环境变量后即缓存**
// （net/http/internal/httpproxy 的单次初始化），同进程里 t.Setenv 改不生效 ——
// 这也是老用例 TestSetProxyEmptyFallsBackToEnv 只能做「与标准库结果一致」这种
// 间接断言的原因。子进程是唯一能让「进程启动时就带着 HTTPS_PROXY」成立的办法，
// 而这正是生产的真实情形：宿主（Tauri）带着环境变量拉起 gateway.exe。
//
// 结论（实测）：**会**。国服 transport 走 ProxyFromEnvironment，因此在用户自己
// 设了 HTTPS_PROXY 的机器上，国服请求仍会经那个代理出去。这是刻意的取舍，
// 不是遗漏 —— 见 newTransport 的注释：ProxyFromEnvironment 是改动前的既有
// 默认行为，有用户靠它做全局代理，网关擅自忽略等于替用户改网络配置。
// 要真正让国服不走任何代理，用户需在 HTTPS_PROXY 之外配 NO_PROXY
// （或把 NO_PROXY 指向国服域名）；下方的断言把这条也一并钉住。
func TestEnvProxyAppliesToCNViaEnvironment(t *testing.T) {
	const envProxy = "http://127.0.0.1:7801"
	const explicit = "http://127.0.0.1:7802"

	if os.Getenv(envProbeChild) == "1" {
		runEnvProbeChild(t, envProxy, explicit)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestEnvProxyAppliesToCNViaEnvironment$", "-test.v")
	cmd.Env = append(append([]string{}, os.Environ()...),
		envProbeChild+"=1",
		envProbeExplicit+"="+explicit,
		"HTTPS_PROXY="+envProxy,
		"HTTP_PROXY="+envProxy,
		// 不清 NO_PROXY 的话，开发机上它可能正好覆盖了我们要测的域名，
		// 用例会「因为环境碰巧如此」而变绿。
		"NO_PROXY=",
		"no_proxy=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("子进程探测失败: %v\n%s", err, out)
	}
	// 子进程的 -test.v 输出里必须能看到结论行，否则说明它没跑到断言就退了。
	if !strings.Contains(string(out), "env-proxy-on-cn=") {
		t.Fatalf("子进程没有给出结论行：\n%s", out)
	}
	t.Logf("子进程实测输出：\n%s", strings.TrimSpace(string(out)))
}

// runEnvProbeChild 在「启动时已带 HTTPS_PROXY」的进程里做实测断言。
//
// 用 t.Fatalf 而不是返回值：子进程的测试失败会自动让 go test 以非 0 退出，
// 父进程 cmd.Run() 因此拿到 error，失败原因原样透传（CombinedOutput）。
func runEnvProbeChild(t *testing.T, envProxy, explicit string) {
	c := New()
	if err := c.SetProxy(explicit); err != nil {
		t.Fatalf("子进程 SetProxy: %v", err)
	}

	// 1) 国服：https 请求打到非 loopback 域名时，走的是环境变量里的代理。
	cnReq, _ := http.NewRequest("GET", "https://copilot.tencent.com/v2/chat/completions", nil)
	cnProxy, err := c.ChatHTTP.Transport.(*http.Transport).Proxy(cnReq)
	if err != nil {
		t.Fatalf("国服 Proxy(req): %v", err)
	}
	if cnProxy == nil {
		t.Fatalf("国服未解析出任何代理，说明 ProxyFromEnvironment 没读到 HTTPS_PROXY=%s", envProxy)
	}
	if cnProxy.String() != envProxy {
		t.Fatalf("国服解析出 %v，期望环境变量里的 %s", cnProxy, envProxy)
	}
	if cnProxy.String() == explicit {
		t.Fatalf("国服吃到了**显式配置**的代理 %s —— 分流失效了", explicit)
	}
	t.Logf("env-proxy-on-cn=%s（国服经环境变量代理，非显式配置的 %s）", cnProxy, explicit)

	// 2) 国际版：显式代理优先，不受 HTTPS_PROXY 影响。
	intlReq, _ := http.NewRequest("GET", "https://www.workbuddy.ai/v2/chat/completions", nil)
	intlProxy, err := c.intlHTTP.Transport.(*http.Transport).Proxy(intlReq)
	if err != nil {
		t.Fatalf("国际版 Proxy(req): %v", err)
	}
	if intlProxy == nil || intlProxy.String() != explicit {
		t.Fatalf("国际版代理=%v，期望显式配置的 %s", intlProxy, explicit)
	}
	t.Logf("explicit-proxy-on-intl=%s", intlProxy)

	// 3) 两个区域的 client 必须不同（否则上面两条断言只是碰巧）。
	cn, intl := cnAuth(), intlAuth()
	if c.httpFor(cn) == c.httpFor(intl) {
		t.Fatal("国服与国际版解析到同一个 http client —— 区域分流未生效")
	}
	if c.chatClientFor(cn) == c.chatClientFor(intl) {
		t.Fatal("国服与国际版解析到同一个 chat client —— 区域分流未生效")
	}
}

// TestEnvProxyRespectsNoProxy 实测 NO_PROXY 能覆盖国服域名（环境变量侧的出口）。
//
// 与上一条同源：只在「进程启动时即带 NO_PROXY」时才有意义，故同样起子进程。
func TestEnvProxyRespectsNoProxy(t *testing.T) {
	const envProxy = "http://127.0.0.1:7801"
	if os.Getenv(envProbeChild) == "2" {
		c := New()
		cnReq, _ := http.NewRequest("GET", "https://copilot.tencent.com/v2/chat/completions", nil)
		got, err := c.ChatHTTP.Transport.(*http.Transport).Proxy(cnReq)
		if err != nil {
			t.Fatalf("国服 Proxy(req): %v", err)
		}
		if got != nil {
			t.Fatalf("NO_PROXY 已覆盖 copilot.tencent.com，国服却仍解析出代理 %v", got)
		}
		intlReq, _ := http.NewRequest("GET", "https://www.workbuddy.ai/v2/chat/completions", nil)
		if got, _ := c.ChatHTTP.Transport.(*http.Transport).Proxy(intlReq); got == nil || got.String() != envProxy {
			t.Fatalf("NO_PROXY 不该影响未列出的域名：workbuddy.ai got=%v want=%s", got, envProxy)
		}
		t.Logf("no-proxy-cn=direct no-proxy-intl=%s", envProxy)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestEnvProxyRespectsNoProxy$", "-test.v")
	cmd.Env = append(append([]string{}, os.Environ()...),
		envProbeChild+"=2",
		"HTTPS_PROXY="+envProxy,
		"HTTP_PROXY="+envProxy,
		"NO_PROXY=copilot.tencent.com,www.codebuddy.cn",
		"no_proxy=copilot.tencent.com,www.codebuddy.cn",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("子进程探测失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "no-proxy-cn=direct") {
		t.Fatalf("子进程没有给出结论行：\n%s", out)
	}
	t.Logf("子进程实测输出：\n%s", strings.TrimSpace(string(out)))
}

// ---------------------------------------------------------------------------
// 决定性实测：HTTPS_PROXY 上的国服流量**真的**出现在代理的计数里
//
// 上一组用例证明的是「Transport.Proxy(req) 解析出了环境变量代理」，属于配置层。
// 本用例把整条链路走完：真起两个假代理、真发请求、用**计数**说话。
//
// 之所以能成立，靠的是刻意绕开标准库的一条规则：ProxyFromEnvironment 永不代理
// loopback（127.0.0.1 恒直连）。因此这里的假上游必须监听在**非 loopback** 的
// 本机地址上，否则 HTTP_PROXY 不生效、用例会「因为上游是 127.0.0.1」而假绿。
// 拿不到非 loopback 地址的环境（无网卡/仅回环）直接跳过，不做假结论。
// ---------------------------------------------------------------------------

const (
	envProbeTraffic = "3" // envProbeChild 取值：跑真实流量探测
	envTrafficUp    = "WB_PROXY_PROBE_UPSTREAM"
	envTrafficExpl  = "WB_PROXY_PROBE_EXPLICIT"
)

// TestEnvProxyReallyCarriesCNTraffic 国服请求在 HTTPS_PROXY 下真的经环境变量代理。
//
// 两个假代理分别承接两条通道，计数互不干扰：
//
//	环境变量 HTTP_PROXY/HTTPS_PROXY → envProxy   （用户自己的全局代理）
//	网关配置里的显式代理            → explicitProxy（「设置 → 更新代理」里填的）
//
// 期望（这是本次取舍的完整表述）：
//
//	国服   → envProxy=1, explicitProxy=0   （只跟环境变量，不跟显式配置）
//	国际版 → envProxy=0, explicitProxy=1   （只跟显式配置）
func TestEnvProxyReallyCarriesCNTraffic(t *testing.T) {
	if os.Getenv(envProbeChild) == envProbeTraffic {
		runTrafficProbeChild(t)
		return
	}

	host := nonLoopbackIPv4(t)
	up := listenFakeUpstreamOn(t, host)
	envProxy := newFakeProxy(t)
	explicitProxy := newFakeProxy(t)

	cmd := exec.Command(os.Args[0], "-test.run=^TestEnvProxyReallyCarriesCNTraffic$", "-test.v")
	cmd.Env = append(append([]string{}, os.Environ()...),
		envProbeChild+"="+envProbeTraffic,
		envTrafficUp+"="+up,
		envTrafficExpl+"="+explicitProxy.srv.URL,
		// 环境变量代理两套都要给：不设 HTTP_PROXY 时 http:// 目标不走代理。
		"HTTP_PROXY="+envProxy.srv.URL,
		"HTTPS_PROXY="+envProxy.srv.URL,
		"NO_PROXY=", "no_proxy=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("子进程流量探测失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "traffic-probe-done") {
		t.Fatalf("子进程没有跑到结论点：\n%s", out)
	}
	t.Logf("子进程输出：\n%s", strings.TrimSpace(string(out)))

	// 断言：国服只出现在环境变量代理上，国际版只出现在显式代理上。
	//
	// 两条通道必须用**可区分**的请求路径（不同 task code），否则「谁经过了谁」
	// 无法从计数里读出来 —— 两个代理收到的请求行长得一模一样。
	cnWant := "POST " + up + "/activity/growth/tasks/cn-probe/claim"
	intlWant := "POST " + up + "/activity/growth/tasks/intl-probe/claim"

	if got := envProxy.urls(); len(got) != 1 || got[0] != cnWant {
		t.Fatalf("环境变量代理（HTTPS_PROXY）应收到的正是国服那一条\n got=%v\nwant=[%s]", got, cnWant)
	}
	if got := explicitProxy.urls(); len(got) != 1 || got[0] != intlWant {
		t.Fatalf("显式代理应收到的正是国际版那一条\n got=%v\nwant=[%s]", got, intlWant)
	}
	t.Logf("实测结论：国服 → 环境变量代理 %v；国际版 → 显式代理 %v", envProxy.urls(), explicitProxy.urls())
}

// runTrafficProbeChild 子进程侧：真发两条请求（国服 + 国际版）后退出。
//
// 只发请求、不断言计数：计数在父进程手里（两个假代理都是父进程起的）。
func runTrafficProbeChild(t *testing.T) {
	up := os.Getenv(envTrafficUp)
	explicit := os.Getenv(envTrafficExpl)
	if up == "" || explicit == "" {
		t.Fatal("子进程缺少探测参数")
	}
	c := routableClient(t, up)
	if err := c.SetProxy(explicit); err != nil {
		t.Fatalf("子进程 SetProxy: %v", err)
	}
	// 国服：只应出现在环境变量代理上。
	if _, _, _, err := c.ClaimGrowthTask(cnAuth(), "cn-probe"); err != nil {
		t.Fatalf("国服请求失败: %v", err)
	}
	// 国际版：只应出现在显式代理上。
	if _, _, _, err := c.ClaimGrowthTask(intlAuth(), "intl-probe"); err != nil {
		t.Fatalf("国际版请求失败: %v", err)
	}
	t.Logf("traffic-probe-done cn=%s intl=%s", cnAuth().Domain, intlAuth().Domain)
}

// nonLoopbackIPv4 取本机一个非回环 IPv4；没有则跳过用例。
func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("读取网卡地址失败，跳过：%v", err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		if ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		return ipnet.IP.String()
	}
	t.Skip("本机没有非回环 IPv4（ProxyFromEnvironment 不代理 loopback，无法做本实测）")
	return ""
}

// listenFakeUpstreamOn 在**指定地址**上起假上游，返回其基址。
//
// 必须是可指定地址而不是 httptest 默认的 127.0.0.1：见本组用例抬头关于
// loopback 的说明。
func listenFakeUpstreamOn(t *testing.T, host string) string {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("在 %s 上监听失败，跳过：%v", host, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{}}`)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}
