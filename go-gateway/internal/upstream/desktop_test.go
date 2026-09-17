package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// desktopTestClient 桌面域测试客户端（chat 域需为 copilot 形状以便断言头）。
func desktopTestClient(fn rtFunc) *Client {
	c := testClient(fn)
	c.WebBaseCN = "https://web.example"
	return c
}

// decodeEvents 解出上报的事件数组（上报体是 [{...}] 而非对象）。
func decodeEvents(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("上报体应为事件数组: %v (%s)", err, raw)
	}
	return arr
}

// eventCodes 取事件码序列。
func eventCodes(events []map[string]any) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		if s, ok := e["eventCode"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestReportDesktopEventsFingerprint 断言桌面指纹的关键字段真的发出去了。
//
// 这些字段本身不参与业务判据，但**用错指纹会导致静默不计分**：
// 服务端照收（HTTP 200）却不点亮进度。因此指纹是必须断言的正确性属性，
// 而不是可以省略的实现细节。
func TestReportDesktopEventsFingerprint(t *testing.T) {
	var raw []byte
	var hdr http.Header
	c := desktopTestClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v2/report" {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.URL.Host != "chat.example" {
			return nil, errors.New("桌面上报应走 chat 域, 实际 " + r.URL.Host)
		}
		hdr = r.Header.Clone()
		raw, _ = io.ReadAll(r.Body)
		return jsonResp(200, `{"code":0,"msg":"OK","data":{}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Nickname: "昵称"}
	err := c.ReportDesktopEvents(a, DesktopEvent{"eventCode": "appearance_skin_apply", "action": "apply"})
	if err != nil {
		t.Fatalf("report: %v", err)
	}

	// 请求头：UA 与 X-Product 是桌面身份的一部分。
	if got := hdr.Get("User-Agent"); got != desktopUA {
		t.Errorf("UA=%q want %q", got, desktopUA)
	}
	if hdr.Get("X-Product") != "SaaS" {
		t.Errorf("X-Product=%q", hdr.Get("X-Product"))
	}
	if hdr.Get("Authorization") != "Bearer at" {
		t.Errorf("Authorization 缺失")
	}
	if hdr.Get("X-User-Id") != "u1" {
		t.Errorf("X-User-Id 缺失")
	}

	events := decodeEvents(t, raw)
	if len(events) != 1 {
		t.Fatalf("事件数=%d want 1", len(events))
	}
	ev := events[0]
	for k, want := range map[string]any{
		"extName":    "workbuddy-desktop",
		"ideName":    "WorkBuddy",
		"ideType":    "WorkBuddy",
		"ideVersion": "5.5.6",
		"product":    "SaaS",
		"os":         "win32",
		"userId":     "u1",
		"eventCode":  "appearance_skin_apply",
	} {
		if ev[k] != want {
			t.Errorf("事件字段 %s=%v want %v", k, ev[k], want)
		}
	}
	// machineId / sessionId 由 uid 稳定派生：同一账号必须每次相同，
	// 否则每次上报都像一台新设备（比固定设备更可疑）。
	if ev["machineId"] == "" || ev["machineId"] == nil {
		t.Error("machineId 缺失")
	}
	if ev["machineId"] != deriveID(a, "machine") {
		t.Errorf("machineId 应为稳定派生值, got %v", ev["machineId"])
	}
	if deriveID(a, "machine") == deriveID(a, "session") {
		t.Error("machineId 与 sessionId 不应同值（用了不同 salt）")
	}
	// 不同账号的 machineId 必须不同。
	other := deriveID(&auth.Auth{UID: "u2"}, "machine")
	if other == deriveID(a, "machine") {
		t.Error("不同 uid 派生出相同 machineId")
	}
}

// TestReportDesktopEventsBusinessFieldsOverrideFingerprint 业务字段应能覆盖
// 同名指纹字段（用于对齐真实设备），否则调用方无法纠偏。
func TestReportDesktopEventsBusinessFieldsOverrideFingerprint(t *testing.T) {
	var raw []byte
	c := desktopTestClient(func(r *http.Request) (*http.Response, error) {
		raw, _ = io.ReadAll(r.Body)
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	err := c.ReportDesktopEvents(&auth.Auth{AccessToken: "at", UID: "u1"},
		DesktopEvent{"eventCode": "x", "machineId": "real-device-id"})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	ev := decodeEvents(t, raw)[0]
	if ev["machineId"] != "real-device-id" {
		t.Errorf("业务字段未覆盖指纹, machineId=%v", ev["machineId"])
	}
}

// TestReportDesktopEventsNoEventsIsNoop 空事件列表不应发请求。
func TestReportDesktopEventsNoEventsIsNoop(t *testing.T) {
	called := false
	c := desktopTestClient(func(r *http.Request) (*http.Response, error) {
		called = true
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	if err := c.ReportDesktopEvents(&auth.Auth{AccessToken: "at", UID: "u1"}); err != nil {
		t.Fatalf("err=%v", err)
	}
	if called {
		t.Error("空事件列表不应发起请求")
	}
}

// TestChatSequenceShape 断言对话事件链的事件码**顺序**与关键字段。
//
// 顺序有意义：agent_task_created 在前、成功回执在后，
// 倒序或缺失会让服务端看到一次"没有开始的对话"。
func TestChatSequenceShape(t *testing.T) {
	events := ChatSequence("conv-1", "cmb-0123456789abcdef0123456789abcdef", "msg-1", "fast-model", "fast-model")
	got := eventCodes(toMaps(events))
	want := []string{
		"agent_task_created", "chat_message_send", "chat_request_send",
		"chat_message_response", "chat_message_status", "chat_request_response",
	}
	if len(got) != len(want) {
		t.Fatalf("事件数=%d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("事件[%d]=%s want %s", i, got[i], want[i])
		}
	}

	m := toMaps(events)
	// 成功回执是「真的完成了一次对话」的证据，缺它整条链无效。
	resp := m[3]
	if resp["isSuccessful"] != true {
		t.Error("chat_message_response.isSuccessful 必须为 true")
	}
	if resp["conversationId"] != "conv-1" {
		t.Errorf("conversationId=%v", resp["conversationId"])
	}
	// 所有事件都必须带 conversationId/requestId 关联，否则 JOIN 不上。
	for i, ev := range m {
		switch ev["eventCode"] {
		case "agent_task_created", "chat_message_response":
			if ev["conversationId"] != "conv-1" {
				t.Errorf("%s 缺 conversationId", ev["eventCode"])
			}
		}
		if ev["eventCode"] == "chat_request_send" {
			if ev["parentConversationId"] != "conv-1" || ev["rootRequestId"] != "cmb-0123456789abcdef0123456789abcdef" {
				t.Errorf("chat_request_send 关联字段错误: %v", ev)
			}
		}
		_ = i
	}
}

// TestSkillSequenceUsesToolCalls finishReason 必须是 tool_calls：
// skill_info 的语义是「模型发起了工具调用（加载技能）」，
// 用 "stop" 表示模型只是正常回答，与判据自相矛盾。
func TestSkillSequenceUsesToolCalls(t *testing.T) {
	req := "cmb-0123456789abcdef0123456789abcdef"
	events := toMaps(SkillSequence("conv-1", req, "技能名", "skill_123", "1.0.0"))
	var skill map[string]any
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" {
			if ev["finishReason"] != "tool_calls" {
				t.Errorf("finishReason=%v want tool_calls", ev["finishReason"])
			}
		}
		if ev["eventCode"] == "skill_info" {
			skill = ev
		}
	}
	if skill == nil {
		t.Fatal("缺少 skill_info 事件")
	}
	// skill_info 必须 JOIN 真实会话与真实 requestId，否则不计数。
	for k, want := range map[string]any{
		"id": "技能名", "skillId": "skill_123", "skillVersion": "1.0.0",
		"toolStatus": "success", "source": "workbuddy-desktop",
		"conversationId": "conv-1", "requestId": req,
	} {
		if skill[k] != want {
			t.Errorf("skill_info.%s=%v want %v", k, skill[k], want)
		}
	}
	if skill["messageId"] != "msg-"+req[len(req)-8:] {
		t.Errorf("messageId=%v 应与 message_response 一致", skill["messageId"])
	}
}

// TestTemplateUseSequenceJoinsChat 模板事件必须 JOIN 到一条完整对话链上：
// 只发 template_used 而没有对话链不会被计入。
func TestTemplateUseSequenceJoinsChat(t *testing.T) {
	events := toMaps(TemplateUseSequence("conv-t", "req-t", "3", "竞品分析"))
	codes := eventCodes(events)
	// 前 6 个是对话链，后 2 个是模板专属事件。
	if len(events) != 8 {
		t.Fatalf("事件数=%d want 8 (%v)", len(events), codes)
	}
	if codes[6] != "agent_task_created_with_template" || codes[7] != "template_used" {
		t.Errorf("尾部事件=%v", codes[6:])
	}
	if events[7]["template_id"] != "3" || events[7]["task_mode"] != "working" {
		t.Errorf("template_used 载荷=%v", events[7])
	}
	if events[6]["requestId"] != "req-t" {
		t.Errorf("模板事件缺 requestId 关联: %v", events[6])
	}
}

// TestPlaybookPromptSequenceSendsPrompt 判据是 playbook_prompt_send
// （真的发送了 Prompt），而不是曝光/点击。
func TestPlaybookPromptSequenceSendsPrompt(t *testing.T) {
	events := toMaps(PlaybookPromptSequence("conv-p", "req-p", "case-1", "案例名"))
	var send map[string]any
	for _, ev := range events {
		if ev["eventCode"] == "playbook_prompt_send" {
			send = ev
		}
	}
	if send == nil {
		t.Fatal("缺少 playbook_prompt_send")
	}
	if send["conversationId"] != "conv-p" || send["requestId"] != "req-p" {
		t.Errorf("prompt_send 未 JOIN 对话: %v", send)
	}
	if send["id"] != "case-1" || send["name"] != "案例名" {
		t.Errorf("案例载荷错误: %v", send)
	}
}

// TestDesignCanvasSequence 画布事件带 requestId 关联与成功标记。
func TestDesignCanvasSequence(t *testing.T) {
	events := toMaps(DesignCanvasSequence("conv-c", "cmb-0123456789abcdef0123456789abcdef"))
	var create, open map[string]any
	for _, ev := range events {
		switch ev["eventCode"] {
		case "wbx_design_canvas_task_create":
			create = ev
		case "wbx_design_canvas_open":
			open = ev
		}
	}
	if create == nil || open == nil {
		t.Fatal("缺少画布事件")
	}
	for _, ev := range []map[string]any{create, open} {
		if ev["isSuccessful"] != true {
			t.Errorf("%s.isSuccessful 应为 true", ev["eventCode"])
		}
		if ev["requestId"] != "cmb-0123456789abcdef0123456789abcdef" {
			t.Errorf("%s.requestId=%v", ev["eventCode"], ev["requestId"])
		}
	}
	// open 的 id 由 requestId 尾部派生：requestId 短于 8 位时不能 panic。
	short := toMaps(DesignCanvasSequence("c", "abc"))
	for _, ev := range short {
		if ev["eventCode"] == "wbx_design_canvas_open" && ev["id"] != "ardot-file-abc" {
			t.Errorf("短 requestId 的 id=%v", ev["id"])
		}
	}
}

// TestBuddyAppSequence 五连事件齐备且都带 buddyId。
//
// buddyID 用字面量而非引用 growtask 的常量：upstream 是被依赖方，
// 反向引用会形成循环依赖；这里断言的是「载荷原样带上调用方给的 id」。
func TestBuddyAppSequence(t *testing.T) {
	const buddyID, buddyName = "cb_y5Dy46tPQGGWtueMxXbe", "企鹅教师助手"
	events := toMaps(BuddyAppSequence(buddyID, buddyName))
	want := []string{
		"buddyapp_discover_click", "buddyapp_show", "buddyapp_enter_click",
		"buddyapp_auth_confirm_click", "buddyapp_bindaccount_skip_click",
	}
	got := eventCodes(events)
	if len(got) != len(want) {
		t.Fatalf("事件数=%d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("事件[%d]=%s want %s", i, got[i], want[i])
		}
		if events[i]["buddyId"] != buddyID {
			t.Errorf("%s 缺 buddyId", got[i])
		}
	}
}

// TestAutomationCreateEvent 事件码与来源标记。
func TestAutomationCreateEvent(t *testing.T) {
	ev := AutomationCreateEvent("wb2api 自动化")
	if ev["eventCode"] != "automated_task_create_suc" {
		t.Errorf("eventCode=%v", ev["eventCode"])
	}
	if ev["source"] != "manually" {
		t.Errorf("source=%v want manually（对齐真实客户端手动创建）", ev["source"])
	}
	if ev["name"] != "wb2api 自动化" {
		t.Errorf("name=%v", ev["name"])
	}
}

// TestAppearanceApplyEvent 皮肤生效事件的判据字段。
func TestAppearanceApplyEvent(t *testing.T) {
	ev := AppearanceApplyEvent("theme-tkmw7j")
	if ev["eventCode"] != "appearance_skin_apply" {
		t.Errorf("eventCode=%v", ev["eventCode"])
	}
	if ev["action"] != "apply" || ev["source"] != "settings_close" {
		t.Errorf("action/source=%v/%v", ev["action"], ev["source"])
	}
	if ev["id"] != "theme-tkmw7j" {
		t.Errorf("id=%v", ev["id"])
	}
}

// TestExpertActualUseEventModes LOCAL 与 craft 两种口径的载荷差异：
// Expert_lighthouse 的判据要求 LOCAL，且真实样本里 LOCAL 的 type 为空、cost=0。
func TestExpertActualUseEventModes(t *testing.T) {
	e := MarketExpert{
		ExpertID: "ex_1", ExpertType: "agent", DisplayNameZH: "专家",
		ProfessionZH: "职业", Version: "2.0.0", Categories: []any{"expert-finance"},
	}
	craft := ExpertActualUseEvent(e, "conv", "req", "craft")
	if craft["mode"] != "craft" {
		t.Errorf("mode=%v", craft["mode"])
	}
	if craft["type"] != "expert-finance" {
		t.Errorf("craft 的 type 应取分类, got %v", craft["type"])
	}
	if craft["cost"] != int64(9000) {
		t.Errorf("craft.cost=%v", craft["cost"])
	}
	local := ExpertActualUseEvent(e, "conv", "req", "LOCAL")
	if local["mode"] != "LOCAL" {
		t.Errorf("mode=%v", local["mode"])
	}
	if local["type"] != "" {
		t.Errorf("LOCAL 的 type 应为空, got %v", local["type"])
	}
	if local["cost"] != int64(0) {
		t.Errorf("LOCAL 的 cost 应为 0, got %v", local["cost"])
	}
	// requestId 必须是真实的服务端 id —— 载荷里必须原样带上。
	if local["requestId"] != "req" {
		t.Errorf("requestId=%v", local["requestId"])
	}
	if local["version"] != "2.0.0" {
		t.Errorf("version=%v", local["version"])
	}
}

// TestExpertSummonSequence 召唤链三事件齐备。
func TestExpertSummonSequence(t *testing.T) {
	e := MarketExpert{ExpertID: "ex_2", ExpertType: "team", DisplayNameZH: "专家团", ProfessionZH: "职业"}
	events := toMaps(ExpertSummonSequence(e))
	got := eventCodes(events)
	want := []string{"web_element_click", "expert_summon_click", "expert_summoned"}
	if len(got) != len(want) {
		t.Fatalf("事件数=%d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("事件[%d]=%s want %s", i, got[i], want[i])
		}
	}
	if events[1]["id"] != "ex_2" || events[1]["expertType"] != "team" {
		t.Errorf("summon_click 载荷=%v", events[1])
	}
	// version 缺省补 1.0.0（上游部分条目 version 为空，空串会让事件形状不一致）。
	if events[1]["version"] != "1.0.0" {
		t.Errorf("缺省 version=%v want 1.0.0", events[1]["version"])
	}
}

// TestReportWebEventUsesWebFingerprint Web 域上报必须是浏览器形状，
// 且带 x-client-platform: web。
func TestReportWebEventUsesWebFingerprint(t *testing.T) {
	var raw []byte
	var hdr http.Header
	c := desktopTestClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "web.example" {
			return nil, errors.New("web 上报应走 Web 域, 实际 " + r.URL.Host)
		}
		if r.URL.Path != "/v2/report" {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		hdr = r.Header.Clone()
		raw, _ = io.ReadAll(r.Body)
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	err := c.ReportWebEvent(&auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "e1"},
		"web_element_click", "https://www.workbuddy.cn/space/d/xxx", "library_doc_intro_click", "资料库介绍")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if hdr.Get("x-client-platform") != "web" {
		t.Errorf("x-client-platform=%q", hdr.Get("x-client-platform"))
	}
	if !strings.Contains(hdr.Get("User-Agent"), "Mozilla/5.0") {
		t.Errorf("web 上报应使用浏览器 UA, got %q", hdr.Get("User-Agent"))
	}
	ev := decodeEvents(t, raw)[0]
	for k, want := range map[string]any{
		"eventCode": "web_element_click", "elementId": "library_doc_intro_click",
		"elementName": "资料库介绍", "userId": "u1", "enterpriseId": "e1",
	} {
		if ev[k] != want {
			t.Errorf("web 事件 %s=%v want %v", k, ev[k], want)
		}
	}
	// 桌面字段不该出现在网页事件里（矛盾指纹）。
	for _, k := range []string{"extName", "ideName", "ideVersion"} {
		if _, ok := ev[k]; ok {
			t.Errorf("web 事件不应含桌面字段 %s", k)
		}
	}
	// machineId 用独立 salt：与桌面指纹的设备标识区分开。
	if ev["machineId"] == nil || ev["machineId"] == "" {
		t.Error("web 事件缺 machineId")
	}
}

// TestReadSSERequestID 从 SSE 流抓服务端 requestId，并读干剩余流。
func TestReadSSERequestID(t *testing.T) {
	const want = "cmb-0123456789abcdef0123456789abcdef"
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"\"}}],\"id\":\"chatcmpl-abc\"}\n\n" +
		"data: {\"id\":\"" + want + "\",\"choices\":[{\"delta\":{\"content\":\"1\"}}]}\n\n" +
		"data: [DONE]\n\n"
	got, err := readSSERequestID(strings.NewReader(body))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != want {
		t.Errorf("id=%q want %q", got, want)
	}
}

// TestReadSSERequestIDRejectsFakeShape 非服务端形状的 id（如 chatcmpl-xxx）
// 不能当成 requestId —— 专家类判据要求真实 id，用错了会静默不计分。
func TestReadSSERequestIDRejectsFakeShape(t *testing.T) {
	body := "data: {\"id\":\"chatcmpl-abc\",\"choices\":[]}\n\ndata: [DONE]\n\n"
	_, err := readSSERequestID(strings.NewReader(body))
	if err == nil {
		t.Fatal("非服务端形状的 id 应判定为未找到")
	}
}

// TestReadSSERequestIDBare32Hex 裸 32 hex 也是合法形状。
func TestReadSSERequestIDBare32Hex(t *testing.T) {
	const want = "0123456789abcdef0123456789abcdef"
	got, err := readSSERequestID(strings.NewReader("data: {\"id\":\"" + want + "\"}\n\n"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != want {
		t.Errorf("id=%q want %q", got, want)
	}
}

// TestInNightWindow 夜猫时段（23:00–08:00 CST）边界判定。
//
// 用 UTC 构造输入：CST = UTC+8 固定偏移，因此 UTC 15:00 = CST 23:00。
func TestInNightWindow(t *testing.T) {
	cases := []struct {
		name string
		utc  int // UTC 小时
		want bool
	}{
		{"CST 23:00 窗口开始", 15, true},
		{"CST 00:00 窗口内", 16, true},
		{"CST 07:00 窗口内", 23, true},
		{"CST 08:00 窗口结束（不含）", 0, false},
		{"CST 12:00 窗口外", 4, false},
		{"CST 22:00 窗口外", 14, false},
	}
	base := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	for _, c := range cases {
		at := base.Add(time.Duration(c.utc) * time.Hour)
		if got := InNightWindow(at); got != c.want {
			t.Errorf("%s: InNightWindow=%v want %v", c.name, got, c.want)
		}
	}
}

// toMaps 把事件切片转成可断言的 map 切片。
func toMaps(events []DesktopEvent) []map[string]any {
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, map[string]any(e))
	}
	return out
}
