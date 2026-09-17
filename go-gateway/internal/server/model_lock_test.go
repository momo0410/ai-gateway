package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// ---------------------------------------------------------------------------
// 「限制使用的模型」白名单（三个工作模式都有）
//
// 语义：白名单非空时，只放行名单内的模型，其余一律 400 拒绝；空 = 不限制。
// 为什么必须限制：轮转模式下 = 「把这个账号的指定模型额度烧干净再换号」，
// 模型是策略的一部分；不限制的话客户端换个模型就能绕过轮转与额度控制。
//
// ⚠️ 本文件的前半部分（newLockedHandler 及其用例）刻意继续使用**废弃的
// 单值字段** `Config.AllowedModel` —— 那是老配置的形状，它们继续通过就是
// 「老配置升级后行为逐字不变」的证据。多值用例在文件后半部分。
// ---------------------------------------------------------------------------

// newLockedHandler 构造一个配了**单值**白名单的 handler（账号池里有 1 个可用账号）。
//
// 刻意传废弃的 `AllowedModel`（单值字符串）而不是 `AllowedModels`：
// 老配置就是这个形状，这些用例继续通过 = 向后兼容被真的测到了。
//
// 复用同包已有的 testPoolWith（它已关掉加权随机的 flake 源），
// 不另造一套 pool 构造逻辑。
func newLockedHandler(t *testing.T, allowed string) *Handler {
	t.Helper()
	p := testPoolWith(&auth.Auth{
		UID:             "u1",
		AccessToken:     "t1",
		SoonestExpireAt: time.Now().Add(24 * time.Hour).Unix(),
	})
	return NewHandler(Config{
		Pool:         p,
		Upstream:     upstream.New(),
		MaxRotate:    1,
		AllowedModel: allowed,
	})
}

// newWhitelistHandler 构造一个配了**多值**白名单的 handler（同上，1 个可用账号）。
func newWhitelistHandler(t *testing.T, allowed ...string) *Handler {
	t.Helper()
	p := testPoolWith(&auth.Auth{
		UID:             "u1",
		AccessToken:     "t1",
		SoonestExpireAt: time.Now().Add(24 * time.Hour).Unix(),
	})
	return NewHandler(Config{
		Pool:          p,
		Upstream:      upstream.New(),
		MaxRotate:     1,
		AllowedModels: allowed,
	})
}

// post 发一个 chat/completions 请求，返回状态码与响应体。
func post(t *testing.T, h *Handler, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestModelLockRejectsOtherModel 锁定后，其他模型必须被 400 拒绝。
func TestModelLockRejectsOtherModel(t *testing.T) {
	h := newLockedHandler(t, "deepseek-v4.1-flash")
	status, body := post(t, h, `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)

	if status != http.StatusBadRequest {
		t.Fatalf("非锁定模型应返回 400，实际 %d（body=%s）", status, body)
	}
	var parsed struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, body)
	}
	// 错误码必须是 model_not_allowed，不能是 no_healthy_account ——
	// 后者会把用户引向排查账号，而真正的原因在请求里。
	if parsed.Error.Code != "model_not_allowed" {
		t.Errorf("错误码应为 model_not_allowed，实际 %q", parsed.Error.Code)
	}
	// 文案要说清「当前用什么、应该改什么」，否则用户不知道怎么办。
	if !strings.Contains(parsed.Error.Message, "deepseek-v4.1-flash") {
		t.Errorf("错误信息应包含被锁定的模型名，实际: %s", parsed.Error.Message)
	}
	if !strings.Contains(parsed.Error.Message, "glm-5.3") {
		t.Errorf("错误信息应包含客户端请求的模型名，实际: %s", parsed.Error.Message)
	}
}

// TestModelLockAllowsExactModel 锁定模型本身必须放行（其余环节照常，最终因无 mock 上游而失败于网络）。
func TestModelLockAllowsExactModel(t *testing.T) {
	h := newLockedHandler(t, "deepseek-v4.1-flash")
	status, body := post(t, h, `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)

	// 关键断言：**不能**是 400 —— 说明没有被模型锁拒绝。
	// （真实上游不可达，所以最终会是 5xx，那是另一条路径，不属于本测试范围。）
	if status == http.StatusBadRequest {
		t.Fatalf("锁定模型本身不应被拒绝，实际 400: %s", body)
	}
}

// TestModelLockIsCaseInsensitive 模型名大小写不敏感（客户端写法不统一）。
func TestModelLockIsCaseInsensitive(t *testing.T) {
	h := newLockedHandler(t, "DeepSeek-V4.1-Flash")
	status, body := post(t, h, `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
	if status == http.StatusBadRequest {
		t.Fatalf("大小写不同不应被拒绝，实际 400: %s", body)
	}
}

// TestModelLockDisabledByDefault 未配置锁定时不限制任何模型（向后兼容）。
func TestModelLockDisabledByDefault(t *testing.T) {
	h := newLockedHandler(t, "")
	status, body := post(t, h, `{"model":"any-model","messages":[{"role":"user","content":"hi"}]}`)
	if status == http.StatusBadRequest {
		t.Fatalf("未锁定模型时不应有 400 拒绝，实际: %s", body)
	}
}

// TestModelLockRejectsMissingModel 请求体没带 model 时也应被拒绝
// （否则「不指定模型」就成了绕过锁定的后门）。
func TestModelLockRejectsMissingModel(t *testing.T) {
	h := newLockedHandler(t, "deepseek-v4.1-flash")
	status, body := post(t, h, `{"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("缺少 model 时应返回 400（不能成为绕过锁定的后门），实际 %d: %s", status, body)
	}
	if !strings.Contains(body, "model_not_allowed") {
		t.Errorf("应为 model_not_allowed，实际: %s", body)
	}
}

// TestErrorCodeForDistinguishesLock 错误码映射：白名单拒绝与账号不可用要分开。
func TestErrorCodeForDistinguishesLock(t *testing.T) {
	if got := errorCodeFor(&modelLockedError{requested: "a", allowed: []string{"b"}}); got != "model_not_allowed" {
		t.Errorf("白名单拒绝应映射为 model_not_allowed，实际 %s", got)
	}
	if got := errorCodeFor(errString("all accounts unavailable")); got != "no_healthy_account" {
		t.Errorf("其他错误应保持 no_healthy_account，实际 %s", got)
	}
}

// TestModelLockOtherProtocolsKeepTheirVocabulary 另外两个协议入口的模型锁定错误码
// 必须保持各自词汇表，不能把 chat/completions 的 model_not_allowed 透出去。
//
// 为什么专门锁：三个入口共用 errorCodeFor 做判定，很容易在「抽公共映射函数」时
// 顺手把码面值一起统一 —— 但 Responses 用 invalid_request_error / upstream_error，
// Anthropic 用 invalid_request_error / api_error，model_not_allowed 只属于
// chat/completions 形状。协议词汇表串味同样是破坏性变更，只是不易被察觉。
func TestModelLockOtherProtocolsKeepTheirVocabulary(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		body     string
		wantCode string
	}{
		{
			"responses",
			"/v1/responses",
			`{"model":"glm-5.3","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			"invalid_request_error",
		},
		{
			"messages",
			"/v1/messages",
			`{"model":"glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			"invalid_request_error",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newLockedHandler(t, "deepseek-v4.1-flash")
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
			rec := httptest.NewRecorder()
			h.mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("锁定模型应返回 400，实际 %d（body=%s）", rec.Code, rec.Body)
			}
			body := rec.Body.String()
			if !strings.Contains(body, c.wantCode) {
				t.Errorf("应含 %q，实际: %s", c.wantCode, body)
			}
			if strings.Contains(body, "model_not_allowed") {
				t.Errorf("model_not_allowed 只属于 chat/completions 形状，不该出现在 %s: %s", c.name, body)
			}
			if strings.Contains(body, "no_healthy_account") {
				t.Errorf("这是请求侧错误，不该报账号故障: %s", body)
			}
		})
	}
}

// errString 简易 error 实现，避免引入 errors.New 之外的依赖。
type errString string

func (e errString) Error() string { return string(e) }

// ---------------------------------------------------------------------------
// 多值白名单（本轮升级的核心）
// ---------------------------------------------------------------------------

// TestWhitelistAllowsAnyListedModel 名单内的**每一个**模型都必须放行。
//
// 只测一个会漏掉「实现其实是单值」这类缺陷：把 3 个模型写进名单、逐个请求，
// 任何一个被拒都说明多值没真正生效。
func TestWhitelistAllowsAnyListedModel(t *testing.T) {
	list := []string{"deepseek-v4.1-flash", "glm-5.3", "kimi-k3-1"}
	for _, model := range list {
		h := newWhitelistHandler(t, list...)
		status, body := post(t, h, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
		// 关键断言：**不是 400** = 没被白名单拒绝。
		//（真实上游不可达，最终会是 5xx，那属于另一条路径。）
		if status == http.StatusBadRequest {
			t.Errorf("名单内的模型 %s 不应被拒绝，实际 400: %s", model, body)
		}
	}
}

// TestWhitelistRejectsUnlistedModel 名单外的模型必须被 400 拒绝。
func TestWhitelistRejectsUnlistedModel(t *testing.T) {
	h := newWhitelistHandler(t, "deepseek-v4.1-flash", "glm-5.3")
	status, body := post(t, h, `{"model":"kimi-k3-1","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("名单外的模型应返回 400，实际 %d（body=%s）", status, body)
	}
	if !strings.Contains(body, "model_not_allowed") {
		t.Errorf("错误码应为 model_not_allowed，实际: %s", body)
	}
}

// TestWhitelistErrorMessageListsAllAllowed 错误信息必须**列出全部**允许的模型。
//
// 这是本轮明确的需求：用户配 3 个模型时，只报「不允许 x」完全没法排查 ——
// 他不知道该改成哪一个，只能挨个试。因此逐个断言名单里的每一项都出现。
func TestWhitelistErrorMessageListsAllAllowed(t *testing.T) {
	list := []string{"deepseek-v4.1-flash", "glm-5.3", "kimi-k3-1"}
	h := newWhitelistHandler(t, list...)
	_, body := post(t, h, `{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}]}`)

	var parsed struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, body)
	}
	for _, model := range list {
		if !strings.Contains(parsed.Error.Message, model) {
			t.Errorf("错误信息应列出允许的模型 %q，实际: %s", model, parsed.Error.Message)
		}
	}
	// 也要说清「收到的是什么」，否则用户不知道是自己拼错了还是没配。
	if !strings.Contains(parsed.Error.Message, "gpt-5.6-sol") {
		t.Errorf("错误信息应包含客户端请求的模型名，实际: %s", parsed.Error.Message)
	}
}

// TestWhitelistEmptyMeansUnrestricted 空名单 = 不限制（默认，硬要求）。
//
// 覆盖三种「空」：nil、空切片、只含空白的元素。任一被读成「一个都不放行」
// 都会让网关对所有请求返回 400 —— 而老配置里根本没有这个键。
func TestWhitelistEmptyMeansUnrestricted(t *testing.T) {
	cases := []struct {
		name string
		list []string
	}{
		{"nil", nil},
		{"空切片", []string{}},
		{"全是空白元素", []string{"", "   ", "\t"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newWhitelistHandler(t, c.list...)
			status, body := post(t, h, `{"model":"any-model","messages":[{"role":"user","content":"hi"}]}`)
			if status == http.StatusBadRequest {
				t.Fatalf("空名单不应限制任何模型，实际 400: %s", body)
			}
		})
	}
	// 老配置的单值字段为空串：等价于「没有配置」，同样不限制。
	t.Run("单值字段为空串", func(t *testing.T) {
		h := newLockedHandler(t, "")
		status, body := post(t, h, `{"model":"any-model","messages":[{"role":"user","content":"hi"}]}`)
		if status == http.StatusBadRequest {
			t.Fatalf("空名单不应限制任何模型，实际 400: %s", body)
		}
	})
}

// TestWhitelistIsCaseInsensitive 多值名单同样大小写不敏感（客户端写法不统一）。
func TestWhitelistIsCaseInsensitive(t *testing.T) {
	h := newWhitelistHandler(t, "DeepSeek-V4.1-Flash", "GLM-5.3")
	for _, model := range []string{"deepseek-v4.1-flash", "glm-5.3", "GLM-5.3"} {
		status, body := post(t, h, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
		if status == http.StatusBadRequest {
			t.Errorf("大小写不同不应被拒绝（%s），实际 400: %s", model, body)
		}
	}
}

// TestWhitelistStripsRealmPrefix 带 `cn:` / `global:` 前缀的请求视为同一个模型。
//
// 实测依据（forward.go 既有注释）：用户配的是 `deepseek-v4.1-flash`，
// 客户端可能带 `cn:` 前缀请求，两者必须视为同一个模型 —— 前缀只是给网关的
// 选号指令，上游根本不认识它，转发前也会被 rewriteModel 抹掉。
func TestWhitelistStripsRealmPrefix(t *testing.T) {
	h := newWhitelistHandler(t, "deepseek-v4.1-flash")
	cases := []struct {
		model string
		want  bool // true = 应放行
	}{
		{"deepseek-v4.1-flash", true},
		{"cn:deepseek-v4.1-flash", true},
		{"global:deepseek-v4.1-flash", true},
		{"cn:glm-5.3", false}, // 剥完前缀仍不在名单里 → 拒
		// 前缀本身**大小写敏感**（沿用 resolveModel 的既有判定，不另造一套）：
		// `CN:` 不是网关认识的区域前缀，因此它被认为是模型名的一部分，与
		// 裸名 `deepseek-v4.1-flash` 不是同一个模型。
		//
		// 刻意保持这个行为而不是「顺手也做成不敏感」：网关转发前用同一个
		// resolveModel 改写请求体，`CN:xxx` 不会被剥掉，上游必然报
		// model not found —— 放行只会把失败推迟到上游、错误信息更含糊。
		// 在这里明确拒绝反而是更可读的失败。
		{"CN:deepseek-v4.1-flash", false},
	}
	for _, c := range cases {
		status, body := post(t, h, `{"model":"`+c.model+`","messages":[{"role":"user","content":"hi"}]}`)
		got := status != http.StatusBadRequest
		if got != c.want {
			t.Errorf("模型 %q：放行=%v，期望 %v（HTTP %d, body=%s）", c.model, got, c.want, status, body)
		}
	}
}

// TestWhitelistConfiguredWithPrefixAlsoMatches 名单里若写了带前缀的名字，也要能匹配裸名。
//
// 用户可能在界面上手写带前缀的名字；此时剥前缀再比才能与客户端的裸名请求对上。
func TestWhitelistConfiguredWithPrefixAlsoMatches(t *testing.T) {
	h := newWhitelistHandler(t, "cn:deepseek-v4.1-flash")
	status, body := post(t, h, `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
	if status == http.StatusBadRequest {
		t.Fatalf("名单里带前缀时裸名请求也应放行，实际 400: %s", body)
	}
	status, body = post(t, h, `{"model":"cn:deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
	if status == http.StatusBadRequest {
		t.Fatalf("带前缀请求同样应放行，实际 400: %s", body)
	}
}

// TestWhitelistMergesLegacySingleValue 多值与老的单值字段**合并**，不是二选一。
//
// 为什么必须合并：宿主升级过程中可能出现两者同时被写进配置的中间态。
// 若此时忽略单值，用户原来锁定的那个模型会**静默失效** —— 表现为
// 「升级后限制突然不管用了」，是安全方向的错误，宁可多放行也不能漏。
func TestWhitelistMergesLegacySingleValue(t *testing.T) {
	p := testPoolWith(&auth.Auth{
		UID:             "u1",
		AccessToken:     "t1",
		SoonestExpireAt: time.Now().Add(24 * time.Hour).Unix(),
	})
	h := NewHandler(Config{
		Pool:          p,
		Upstream:      upstream.New(),
		MaxRotate:     1,
		AllowedModels: []string{"glm-5.3"},
		AllowedModel:  "deepseek-v4.1-flash", // 老配置残留
	})
	for _, model := range []string{"glm-5.3", "deepseek-v4.1-flash"} {
		status, body := post(t, h, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
		if status == http.StatusBadRequest {
			t.Errorf("合并后的名单应含 %s，实际被拒 400: %s", model, body)
		}
	}
	status, body := post(t, h, `{"model":"kimi-k3-1","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusBadRequest {
		t.Errorf("不在合并名单里的模型仍应被拒，实际 %d: %s", status, body)
	}
}

// TestWhitelistAppliesToAllProtocols 三个协议入口都要受白名单约束。
//
// 只守 chat/completions 是不够的：客户端完全可以走 /v1/messages 绕过限制 ——
// 那会让「限制使用」形同虚设，且从界面上完全看不出来。
func TestWhitelistAppliesToAllProtocols(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
	}{
		{
			"chat/completions",
			"/v1/chat/completions",
			`{"model":"kimi-k3-1","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			"responses",
			"/v1/responses",
			`{"model":"kimi-k3-1","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
		},
		{
			"messages",
			"/v1/messages",
			`{"model":"kimi-k3-1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newWhitelistHandler(t, "deepseek-v4.1-flash")
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
			rec := httptest.NewRecorder()
			h.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("名单外模型在 %s 上应被 400 拒绝，实际 %d（body=%s）",
					c.name, rec.Code, rec.Body)
			}
		})
	}
}

// TestNormalizeAllowedModels 归一化规则本身。
func TestNormalizeAllowedModels(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil → nil", nil, nil},
		{"去空白", []string{"  glm-5.3  "}, []string{"glm-5.3"}},
		{"剥 cn 前缀", []string{"cn:glm-5.3"}, []string{"glm-5.3"}},
		{"剥 global 前缀", []string{"global:gpt-5.6-sol"}, []string{"gpt-5.6-sol"}},
		// 非枚举前缀不剥（与 resolveModel 同一判定：`deepseek:v3` 里的
		// `deepseek` 不是区域，剥了就把模型名改坏了）。
		{"非区域前缀不剥", []string{"deepseek:v3"}, []string{"deepseek:v3"}},
		{"丢弃空项", []string{"glm-5.3", "", "  ", "kimi-k3-1"}, []string{"glm-5.3", "kimi-k3-1"}},
		{"全空 → nil（= 不限制）", []string{"", "  "}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeAllowedModels(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("normalizeAllowedModels(%q)=%q，期望 %q", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("normalizeAllowedModels(%q)=%q，期望 %q", c.in, got, c.want)
				}
			}
		})
	}
}

// TestModelAllowedDirect 直接锁住判定函数的语义（含空名单的短路）。
func TestModelAllowedDirect(t *testing.T) {
	if !modelAllowed(nil, "anything") {
		t.Error("空名单必须放行一切（不限制）")
	}
	if !modelAllowed([]string{}, "anything") {
		t.Error("零长度名单必须放行一切（不限制）")
	}
	if !modelAllowed([]string{"glm-5.3"}, "GLM-5.3") {
		t.Error("大小写不敏感")
	}
	if modelAllowed([]string{"glm-5.3"}, "glm-5.2") {
		t.Error("不同模型必须拒绝")
	}
}
