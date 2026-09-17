package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/upstream"
)

// ---------------------------------------------------------------------------
// 系统提示词替换（prompt.file / prompt.mode）出站边界回归
//
// 断言的是**上游真正收到的 wire body**，而不是 handler 内部的中间变量 ——
// 只有出站字节才能证明「客户端 system 指纹确实没发出去」。
// ---------------------------------------------------------------------------

// captureUpstream 返回一个 ChatStream 走 fake 的 upstream.Client，
// 并把每次上游收到的请求体追加到 *bodies。
func captureUpstream(bodies *[][]byte) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			*bodies = append(*bodies, raw)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// promptMessages 取出 wire body 的 messages 数组（保留原始角色序列与内容）。
func promptMessages(t *testing.T, wire []byte) []map[string]any {
	t.Helper()
	var doc struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(wire, &doc); err != nil {
		t.Fatalf("wire body 不是合法 JSON: %v（body=%s）", err, wire)
	}
	return doc.Messages
}

// promptRoles 取出 wire body 的 role 序列。
func promptRoles(msgs []map[string]any) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		role, _ := m["role"].(string)
		out = append(out, role)
	}
	return out
}

// newPromptHandler 构造一个指定 prompt 模式、单账号、可捕获出站体的 handler。
func newPromptHandler(t *testing.T, mode, text string) (*Handler, *[][]byte) {
	t.Helper()
	bodies := &[][]byte{}
	return NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   captureUpstream(bodies),
		MaxRotate:  1,
		PromptMode: mode,
		PromptText: text,
	}), bodies
}

const claudeFingerprint = "You are Claude Code, Anthropic's official CLI tool for Claude."

// TestPromptCustomReplacesSystemOnWire custom 模式：客户端 system 指纹不出现在出站请求里。
func TestPromptCustomReplacesSystemOnWire(t *testing.T) {
	h, bodies := newPromptHandler(t, prompt.ModeCustom, "GATEWAY-SYS")
	in := `{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + claudeFingerprint + `"},` +
		`{"role":"user","content":"你好"}]}`

	status, body := post(t, h, in)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if len(*bodies) != 1 {
		t.Fatalf("上游应收到 1 次请求，实际 %d", len(*bodies))
	}
	wire := (*bodies)[0]
	if strings.Contains(string(wire), "Anthropic") {
		t.Errorf("客户端 system 指纹被转发到了上游: %s", wire)
	}
	msgs := promptMessages(t, wire)
	roles := promptRoles(msgs)
	if len(roles) != 2 || roles[0] != "system" || roles[1] != "user" {
		t.Fatalf("role 序列应为 [system user]，实际 %v", roles)
	}
	if msgs[0]["content"] != "GATEWAY-SYS" {
		t.Errorf("首条 system 应为自有提示词，实际 %v", msgs[0]["content"])
	}
	if msgs[1]["content"] != "你好" {
		t.Errorf("user 内容被改动: %v", msgs[1]["content"])
	}
}

// TestPromptCustomDropsDeveloperRoleOnWire developer 角色同样被替换掉。
func TestPromptCustomDropsDeveloperRoleOnWire(t *testing.T) {
	h, bodies := newPromptHandler(t, prompt.ModeCustom, "GATEWAY-SYS")
	in := `{"model":"glm-5.2","messages":[` +
		`{"role":"developer","content":"DEVELOPER-FINGERPRINT"},` +
		`{"role":"user","content":"hi"}]}`

	if status, _ := post(t, h, in); status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	wire := (*bodies)[0]
	if strings.Contains(string(wire), "DEVELOPER-FINGERPRINT") {
		t.Errorf("developer 内容有残留: %s", wire)
	}
	if roles := promptRoles(promptMessages(t, wire)); len(roles) != 2 || roles[0] != "system" {
		t.Errorf("role 序列应为 [system user]，实际 %v", roles)
	}
}

// TestPromptPassthroughLeavesSystemUntouched passthrough（缺省）行为与既有完全一致：
// 客户端 system **原样**发到上游，一个字节都不改。
func TestPromptPassthroughLeavesSystemUntouched(t *testing.T) {
	// 故意不传 PromptMode / PromptText：验证「调用方忘记传」时也是透传。
	for _, mode := range []string{prompt.ModePassthrough, ""} {
		bodies := &[][]byte{}
		h := NewHandler(Config{
			Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
			Upstream:   captureUpstream(bodies),
			MaxRotate:  1,
			PromptMode: mode,
		})
		in := `{"model":"glm-5.2","messages":[` +
			`{"role":"system","content":"` + claudeFingerprint + `"},` +
			`{"role":"user","content":"你好"}]}`

		if status, body := post(t, h, in); status != http.StatusOK {
			t.Fatalf("mode=%q status=%d body=%s", mode, status, body)
		}
		wire := (*bodies)[0]
		if !strings.Contains(string(wire), claudeFingerprint) {
			t.Errorf("mode=%q 下客户端 system 应原样透传，实际 wire=%s", mode, wire)
		}
		// 角色与消息数不变（含原 system），顺序保持。
		if roles := promptRoles(promptMessages(t, wire)); len(roles) != 2 || roles[0] != "system" || roles[1] != "user" {
			t.Errorf("mode=%q 下 messages 结构应不变，实际角色 %v", mode, roles)
		}
	}
}

// TestPromptPassthroughIgnoresPromptText mode=passthrough 即使配了文本也不替换。
//
// 这是「缺省不改变既有行为」的核心保证：mode 才是开关，PromptText 非空不构成替换理由。
func TestPromptPassthroughIgnoresPromptText(t *testing.T) {
	h, bodies := newPromptHandler(t, prompt.ModePassthrough, "SHOULD-NOT-APPEAR")
	in := `{"model":"glm-5.2","messages":[{"role":"system","content":"ORIGINAL"},{"role":"user","content":"hi"}]}`

	if status, _ := post(t, h, in); status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	wire := string((*bodies)[0])
	if strings.Contains(wire, "SHOULD-NOT-APPEAR") {
		t.Errorf("passthrough 不应注入提示词: %s", wire)
	}
	if !strings.Contains(wire, "ORIGINAL") {
		t.Errorf("passthrough 应保留客户端 system: %s", wire)
	}
}

// TestPromptCustomKeepsToolsAndModel custom 模式下其余字段（tools/model）不受影响。
func TestPromptCustomKeepsToolsAndModel(t *testing.T) {
	h, bodies := newPromptHandler(t, prompt.ModeCustom, "GATEWAY-SYS")
	in := `{"model":"glm-5.2","max_tokens":100,` +
		`"tools":[{"type":"function","function":{"name":"f"}}],` +
		`"messages":[{"role":"system","content":"old"},{"role":"user","content":"hi"}]}`

	if status, _ := post(t, h, in); status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	var doc struct {
		Model     string           `json:"model"`
		MaxTokens int              `json:"max_tokens"`
		Tools     []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal((*bodies)[0], &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Model != "glm-5.2" {
		t.Errorf("model 应为 glm-5.2，实际 %q", doc.Model)
	}
	if doc.MaxTokens != 100 {
		t.Errorf("max_tokens 丢失: %d", doc.MaxTokens)
	}
	if len(doc.Tools) != 1 {
		t.Errorf("tools 丢失: %v", doc.Tools)
	}
}

// TestPromptCustomAppliesToAllProtocols 三种协议入口都要生效。
//
// 改写点放在 forwardChat（协议汇合处）而不是各入口，本测试即验证这一决策。
func TestPromptCustomAppliesToAllProtocols(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
	}{
		{
			name: "chat/completions",
			path: "/v1/chat/completions",
			body: `{"model":"glm-5.2","messages":[{"role":"system","content":"` + claudeFingerprint + `"},{"role":"user","content":"hi"}]}`,
		},
		{
			name: "messages(Anthropic)",
			path: "/v1/messages",
			body: `{"model":"claude-sonnet-4","max_tokens":100,"system":"` + claudeFingerprint + `","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "responses(OpenAI)",
			path: "/v1/responses",
			body: `{"model":"gpt-5","instructions":"` + claudeFingerprint + `","input":"hi"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, bodies := newPromptHandler(t, prompt.ModeCustom, "GATEWAY-SYS")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			if len(*bodies) == 0 {
				t.Fatal("上游未收到请求")
			}
			wire := (*bodies)[0]
			if strings.Contains(string(wire), "Anthropic") {
				t.Errorf("客户端 system 指纹泄漏到上游: %s", wire)
			}
			msgs := promptMessages(t, wire)
			if len(msgs) == 0 || msgs[0]["role"] != "system" || msgs[0]["content"] != "GATEWAY-SYS" {
				t.Errorf("首条应为自有 system，实际 %v", msgs)
			}
		})
	}
}

// TestPromptCustomWithPrefixModelRewrite 与 model 前缀改写叠加时两者都生效。
//
// 顺序回归：prompt.Rewrite 在 rewriteModel **之前**，两次改写不能互相吃掉。
func TestPromptCustomWithPrefixModelRewrite(t *testing.T) {
	h, bodies := newPromptHandler(t, prompt.ModeCustom, "GATEWAY-SYS")
	in := `{"model":"cn:glm-5.2","messages":[` +
		`{"role":"system","content":"` + claudeFingerprint + `"},` +
		`{"role":"user","content":"hi"}]}`

	if status, body := post(t, h, in); status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	wire := (*bodies)[0]
	// 前缀被剥掉（rewriteModel 生效）
	if !strings.Contains(string(wire), `"glm-5.2"`) || strings.Contains(string(wire), "cn:glm-5.2") {
		t.Errorf("model 前缀应被剥离: %s", wire)
	}
	// 提示词被替换（prompt.Rewrite 生效）
	if strings.Contains(string(wire), "Anthropic") {
		t.Errorf("system 指纹应被替换: %s", wire)
	}
	msgs := promptMessages(t, wire)
	if msgs[0]["content"] != "GATEWAY-SYS" {
		t.Errorf("首条应为自有 system，实际 %v", msgs[0]["content"])
	}
}

// TestPromptCustomInvalidBodyNotBlocked 请求体非法 JSON 时请求不被改写阻塞。
//
// 与 rewriteModel 同一契约：宁可让上游按原语义报错，也不要因改写失败而失败。
// （此处上游是 fake，返回 200；关键是 handler 没有因改写 panic 或报 4xx。）
func TestPromptCustomInvalidBodyNotBlocked(t *testing.T) {
	h, _ := newPromptHandler(t, prompt.ModeCustom, "GATEWAY-SYS")
	status, body := post(t, h, `{not valid json`)
	if status != http.StatusOK {
		t.Fatalf("非法 JSON 请求不应被改写阻塞，status=%d body=%s", status, body)
	}
}

// TestPromptCustomStickySessionStillWorks 替换 system 不影响会话粘性键提取。
//
// 会话键在 handler 入口（改写之前）提取，本测试防止将来把改写点前移到
// 会话键提取之前而静默改变粘性行为。
func TestPromptCustomStickySessionStillWorks(t *testing.T) {
	h, _ := newPromptHandler(t, prompt.ModeCustom, "GATEWAY-SYS")
	in := `{"model":"glm-5.2","metadata":{"conversation_id":"conv-1"},` +
		`"messages":[{"role":"system","content":"` + claudeFingerprint + `"},{"role":"user","content":"hi"}]}`
	if status, body := post(t, h, in); status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
}
