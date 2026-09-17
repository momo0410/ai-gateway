// desktop.go 桌面 / Web 客户端指纹的行为上报（POST /v2/report）。
//
// 背景：成长任务的进度由**服务端行为事件**点亮，而不同任务认不同的客户端指纹。
// 同一份判据事件，用错指纹上报会被服务端接受（HTTP 200）但**不计分** ——
// 这是本功能最容易踩的坑：上报返回 200 会被误当成成功，直到回读进度才发现
// 一直停在 0/N。故三类指纹在此显式区分，各自独立成一条通道：
//
//	CLI 域   billingBase (www.codebuddy.cn)  chat_request_send —— 见 report.go
//	桌面指纹 chatBase    (copilot.tencent.com) UA=WorkBuddy/5.5.6 + extName=workbuddy-desktop
//	Web 指纹 webBase     (www.workbuddy.cn)   x-client-platform: web
//
// 端点是同一个 /v2/report，区别只在**基址 + 请求头 + 事件体形状**。
package upstream

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

const (
	// desktopReportPath desktop 与 web 指纹共用的上报路径（基址不同）。
	desktopReportPath = "/v2/report"

	// appearanceSetPath 外观主题设置（桌面域）。纯 API set 只留痕不计分，
	// 必须再补一条 appearance_skin_apply 事件才点亮 Hp_Appearance（实测口径）。
	appearanceSetPath = "/v2/user-asset/appearance/set"

	// desktopUA 实测桌面客户端 UA（5.5.6 内嵌 CLI 2.137.1）。
	desktopUA = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"

	// webUA 浏览器 UA。成长中心的领奖与 web 域上报都用它 —— 用桌面 UA 调
	// 网页接口会形成「CLI 调网页」的矛盾指纹。
	webUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
)

// DesktopEvent 桌面指纹事件：业务字段任意，公共指纹由上报函数注入。
type DesktopEvent map[string]any

// deriveID 由 uid 稳定派生一个 36 位 hex 设备标识（machineId / sessionId 复用）。
//
// 用派生而非随机：同一账号每次生成相同的设备标识，模拟「固定的一台机器」。
// 每次上报都换 machineId 等于每次都是一台新设备，这在风控视角下比固定设备更可疑。
func deriveID(a *auth.Auth, salt string) string {
	sum := sha256.Sum256([]byte(salt + ":" + a.UID))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

// desktopFingerprint 公共桌面指纹字段。
//
// 这些字段本身不参与业务判据，但缺失会让事件看起来不像真实客户端产生的；
// 业务键里若出现同名键，调用方给出的值优先（可用于对齐真实设备）。
func desktopFingerprint(a *auth.Auth) map[string]any {
	now := time.Now().UnixMilli()
	return map[string]any{
		"timezone":     "Asia/Shanghai",
		"reportDelay":  2000,
		"userId":       a.UID,
		"username":     a.Nickname,
		"userNickname": a.Nickname,
		"product":      "SaaS",
		"releaseDate":  int64(1789036585355),
		"commit":       "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName":      "WorkBuddy",
		"ideType":      "WorkBuddy",
		"ideVersion":   "5.5.6",
		"machineId":    deriveID(a, "machine"),
		"sessionId":    deriveID(a, "session"),
		"extName":      "workbuddy-desktop",
		"extVersion":   "5.5.6",
		"os":           "win32",
		"arch":         "x64",
		"osVersion":    "10.0.26220",
		"cpuCores":     20,
		"memorySize":   24,
		"timestamp":    now,
		"presentAt":    now,
	}
}

// ReportDesktopEvents 以桌面客户端指纹上报一批事件。
//
// 空事件列表直接返回 nil（不报错）：调用方按动作表派发时可能因状态跳过全部事件，
// 此时「什么都没发」是正确行为而非错误。
func (c *Client) ReportDesktopEvents(a *auth.Auth, events ...DesktopEvent) error {
	if len(events) == 0 {
		return nil
	}
	fp := desktopFingerprint(a)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := make(map[string]any, len(fp)+len(ev))
		for k, v := range fp {
			m[k] = v
		}
		for k, v := range ev { // 业务字段覆盖同名指纹字段
			m[k] = v
		}
		arr = append(arr, m)
	}
	raw, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	base := c.chatBase(a)
	req, err := http.NewRequest(http.MethodPost, base+desktopReportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	h := req.Header
	h.Set("Authorization", "Bearer "+a.AccessToken)
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Content-Type", "application/json;charset=UTF-8")
	h.Set("User-Agent", desktopUA)
	h.Set("X-Domain", base)
	h.Set("X-Product", "SaaS")
	// X-Request-ID 拼上纳秒尾巴：同账号的多次上报不能共用同一个请求 id。
	h.Set("X-Request-ID", deriveID(a, "req")+itoa64(time.Now().UnixNano()%1e6))
	if a.UID != "" {
		h.Set("X-User-Id", a.UID)
	}
	_, err = c.doJSON(a, req)
	return err
}

// SetAppearanceTheme 应用外观主题（桌面域）。
//
// 单独留痕用：Hp_Appearance 的判据是随后那条 appearance_skin_apply 事件，
// 本调用让服务端侧存在「该账号确实设置过这个主题」的记录，使事件更自洽。
func (c *Client) SetAppearanceTheme(a *auth.Auth, resourceKey string) error {
	raw, err := json.Marshal(map[string]string{"kind": "theme", "resource_key": resourceKey})
	if err != nil {
		return err
	}
	base := c.chatBase(a)
	req, err := http.NewRequest(http.MethodPost, base+appearanceSetPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	h := req.Header
	h.Set("Authorization", "Bearer "+a.AccessToken)
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Content-Type", "application/json;charset=UTF-8")
	h.Set("User-Agent", desktopUA)
	h.Set("X-Product", "SaaS")
	if a.UID != "" {
		h.Set("X-User-Id", a.UID)
	}
	_, err = c.doJSON(a, req)
	return err
}

// ReportWebEvent 以 Web 浏览器指纹上报单条事件。
//
// 与桌面指纹的区别不只是基址：web 域事件是浏览器形状
//（os/arch/userAgent/machineId），没有 ideName/extName 那一族桌面字段。
// 混用会得到「桌面字段出现在网页事件里」的矛盾指纹。
func (c *Client) ReportWebEvent(a *auth.Auth, eventCode, pageURL, elementID, elementName string) error {
	ev := map[string]any{
		"eventCode": eventCode, "timestamp": time.Now().UnixMilli(), "reportDelay": 0,
		"pageURL": pageURL, "elementId": elementID, "elementName": elementName,
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": webUA,
		"machineId": deriveID(a, "webmachine"), "userId": a.UID,
		"userNickname": a.Nickname, "enterpriseId": a.EnterpriseID,
	}
	raw, err := json.Marshal([]map[string]any{ev})
	if err != nil {
		return err
	}
	base := c.webBase(a)
	req, err := http.NewRequest(http.MethodPost, base+desktopReportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	h := req.Header
	h.Set("Authorization", "Bearer "+a.AccessToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("x-client-platform", "web")
	h.Set("Origin", base)
	h.Set("Referer", pageURL)
	h.Set("User-Agent", webUA)
	if a.UID != "" {
		h.Set("X-User-Id", a.UID)
	}
	_, err = c.doJSON(a, req)
	return err
}

// ---------------------------------------------------------------------------
// 判据事件链构造
//
// 设计：全部是**纯函数**（除时间戳外无副作用、不触网），把「事件形状」与
// 「何时上报、如何重试、如何回读」彻底分开。好处有三个：
//   - 形状可单测：断言每个事件码与关键字段，不必起假上游；
//   - 编排层可复用：多条任务链共享 chatSequence 这个基座（模板/灵感/画布
//     都建立在一次成功的对话之上）；
//   - 改动集中：上游若调整判据字段，只改这里，编排层不动。
// ---------------------------------------------------------------------------

// chatSequence 一次「桌面端成功对话」的完整事件链。
//
// 这是多数桌面任务的共同基座：服务端把对话成功回执（isSuccessful=true）
// 视为「客户端真的完成了一次对话」的证据，其余任务事件 JOIN 到它上面
// （靠 conversationId/requestId 关联）。缺少成功回执的链会被判为无效对话。
func chatSequence(conversationID, requestID, messageID, modelID, modelName string) []DesktopEvent {
	mk := func(code string, extra map[string]any) DesktopEvent {
		ev := DesktopEvent{"eventCode": code}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []DesktopEvent{
		mk("agent_task_created", map[string]any{
			"source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
			"requestModelId": modelID, "requestModelName": modelName,
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{},
			"has_mention": false, "mention_types": []any{},
			"has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{},
			"conversationId": conversationID, "messageId": messageID,
			"buddyId": "", "buddyName": "",
		}),
		mk("chat_message_send", map[string]any{
			"messageId": messageID + "-assistant", "historyCount": 0,
			"isContextTruncated": false, "currentStepCount": 1,
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_send", map[string]any{
			"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
			"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
			"maxSteps": 500, "temperature": 0, "maxRetries": 0,
			"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
			"codebaseId": "", "mentionContextCount": 0, "command": "",
			"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_response", map[string]any{
			"messageId": messageID + "-assistant", "responseModelId": modelID,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"firstTokenAt": time.Now().UnixMilli(), "traceId": requestID,
			"conversationId": conversationID,
			"rootRequestId":  requestID, "parentConversationId": conversationID,
			"agentName": "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_status", map[string]any{
			"messageId": messageID + "-assistant", "messageErrorCode": "0",
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_response", map[string]any{
			"mode": "craft", "toolCallCount": 0,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"rootRequestId": requestID, "parentConversationId": conversationID,
		}),
	}
}

// ChatSequence 导出的一次成功对话事件链（供 growtask 编排层拼接任务专属事件）。
func ChatSequence(conversationID, requestID, messageID, modelID, modelName string) []DesktopEvent {
	return chatSequence(conversationID, requestID, messageID, modelID, modelName)
}

// BuddyAppSequence 「进入 Buddy 应用」五连事件。
//
// buddyID 用企鹅教师助手：它同时是 Buddy_App_QQ 的判据应用，
// 因此同一组事件可覆盖「进入任一应用」(Buddy_App) 与「进入企鹅教师助手」
// (Buddy_App_QQ) 两个任务 —— 少一次完整事件链，也少一份重复的设备行为。
func BuddyAppSequence(buddyID, buddyName string) []DesktopEvent {
	mk := func(code string, extra map[string]any) DesktopEvent {
		ev := DesktopEvent{
			"eventCode": code, "mode": "LOCAL",
			"buddyId": buddyID, "buddyName": buddyName,
		}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []DesktopEvent{
		mk("buddyapp_discover_click", nil),
		mk("buddyapp_show", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2}),
		mk("buddyapp_enter_click", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2, "isFirstPage": "1"}),
		mk("buddyapp_auth_confirm_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
		mk("buddyapp_bindaccount_skip_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
	}
}

// AutomationCreateEvent 「定时任务创建成功」事件。
func AutomationCreateEvent(name string) DesktopEvent {
	return DesktopEvent{
		"eventCode": "automated_task_create_suc", "name": name,
		"source": "manually", "modelId": "fast-model", "modelIsThinking": true,
		"connectorCount": 0, "skills": "", "skillCount": 0,
		"scheduleType": "once", "mode": "LOCAL",
	}
}

// TemplateUseSequence 「使用模板创建任务」事件组，JOIN 一条完整对话链。
func TemplateUseSequence(conversationID, requestID, templateID, templateName string) []DesktopEvent {
	events := chatSequence(conversationID, requestID, "msg-"+templateID, "fast-model", "fast-model")
	return append(events,
		DesktopEvent{
			"eventCode": "agent_task_created_with_template", "mode": "working",
			"isCustomModel": false, "id": templateID, "name": templateName, "requestId": requestID,
		},
		DesktopEvent{"eventCode": "template_used", "template_id": templateID, "task_mode": "working"},
	)
}

// PlaybookPromptSequence 「灵感案例做同款」事件组（JOIN 对话链）。
//
// 判据是 playbook_prompt_send（在 Dialog 里真的发送了 Prompt），
// 而非卡片曝光或点击 —— 只发曝光事件不会计分。
func PlaybookPromptSequence(conversationID, requestID, caseID, caseName string) []DesktopEvent {
	events := chatSequence(conversationID, requestID, "msg-pb", "fast-model", "fast-model")
	payload := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
	}
	withPayload := func(code string, extra map[string]any) DesktopEvent {
		m := map[string]any{"eventCode": code}
		for k, v := range extra {
			m[k] = v
		}
		for k, v := range payload {
			m[k] = v
		}
		return DesktopEvent(m)
	}
	return append(events,
		DesktopEvent{
			"eventCode": "web_element_click", "pageName": "playbook_detail",
			"elementId": "playbook_ctaClick", "elementName": caseName, "source": "discover",
		},
		withPayload("playbook_cta_click", map[string]any{"source": "discover", "position": 0}),
		withPayload("playbook_prompt_send", map[string]any{"conversationId": conversationID, "requestId": requestID}),
	)
}

// DesignCanvasSequence 「设计创意画布」事件组（JOIN 对话链）。
func DesignCanvasSequence(conversationID, requestID string) []DesktopEvent {
	events := chatSequence(conversationID, requestID, "msg-canvas", "fast-model", "fast-model")
	return append(events,
		DesktopEvent{
			"eventCode": "wbx_design_canvas_task_create", "conversationId": conversationID,
			"requestId": requestID, "source": "summon_keyword", "cost": 12000, "isSuccessful": true,
		},
		DesktopEvent{
			"eventCode": "wbx_design_canvas_open", "conversationId": conversationID,
			"requestId": requestID, "id": "ardot-file-" + tail(requestID, 8),
			"source": "summon_keyword", "type": "page", "cost": 13000, "isSuccessful": true,
		},
	)
}

// SkillSequence 「真实对话 + 技能加载」事件组（skill_1 判据）。
//
// 判据是 skill_info 事件（桌面指纹）JOIN 真实会话：客户端在模型发起工具调用
// （即加载技能）时上报。因此 chat_message_response 的 finishReason 必须置为
// tool_calls —— 用 "stop" 表示模型只是正常回答，与「加载了技能」矛盾。
func SkillSequence(conversationID, requestID, skillName, skillID, skillVersion string) []DesktopEvent {
	messageID := "msg-" + tail(requestID, 8)
	events := chatSequence(conversationID, requestID, messageID, "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" {
			ev["finishReason"] = "tool_calls"
		}
	}
	return append(events, DesktopEvent{
		"eventCode":      "skill_info",
		"id":             skillName,
		"skillId":        skillID,
		"skillVersion":   skillVersion,
		"toolStatus":     "success",
		"fileCount":      56,
		"source":         "workbuddy-desktop",
		"conversationId": conversationID, "requestId": requestID, "messageId": messageID,
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"traceId": requestID,
	})
}

// AppearanceApplyEvent 皮肤生效事件（Hp_Appearance 判据）。
func AppearanceApplyEvent(resourceKey string) DesktopEvent {
	return DesktopEvent{
		"eventCode": "appearance_skin_apply", "action": "apply", "source": "settings_close",
		"id": resourceKey, "vipLevel": 0, "series": "", "type": "unknown",
	}
}

// tail 取字符串末尾 n 个字符；不足则整体返回（避免切片越界）。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ChatWithModel 发一条真实桌面指纹对话，返回服务端分配的 requestId。
//
// 为什么需要真实对话而不是全部靠事件伪造：专家类与 skill_1 的判据事件要求
// requestId 是**服务端真实产生**的 id，自造 UUID 不计分。服务端在 SSE 流的
// 首个 data 帧里回这个 id，故必须真的发一次请求并读流。
//
// 读干流是有意的：不读完会在连接池里留下半开的 SSE 连接，
// 上游侧则看到一个「发起后立刻断开」的异常会话。
func (c *Client) ChatWithModel(a *auth.Auth, modelID, prompt string, expertID string) (conversationID, requestID string, err error) {
	conversationID = "wb2api-conv-" + itoa64(time.Now().UnixNano())
	body := map[string]any{
		"model": modelID,
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant. 当前处于中文环境，使用简体中文回答。"},
			map[string]any{"role": "user", "content": prompt},
		},
		"agent":          "cli",
		"temperature":    1,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	base := c.chatBase(a)
	req, err := http.NewRequest(http.MethodPost, base+"/v2/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	h := req.Header
	h.Set("Authorization", "Bearer "+a.AccessToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("User-Agent", desktopUA)
	h.Set("X-Domain", base)
	h.Set("X-Product", "SaaS")
	h.Set("X-User-Id", a.UID)
	h.Set("X-Conversation-ID", conversationID)
	h.Set("X-Request-ID", itoa64(time.Now().UnixNano()))
	h.Set("X-Agent-Intent", "craft")
	h.Set("X-Agent-Type", "main")
	h.Set("X-IDE-Name", "WorkBuddy")
	h.Set("X-IDE-Type", "WorkBuddy")
	h.Set("X-IDE-Version", "5.5.6")
	h.Set("x-codebuddy-request", "1")
	if expertID != "" {
		h.Set("X-Expert-Id", expertID)
	}
	resp, err := c.chatClientFor(a).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("chat http %d", resp.StatusCode)
	}
	rid, err := readSSERequestID(resp.Body)
	if err != nil {
		return "", "", err
	}
	return conversationID, rid, nil
}

// sseIDRe 服务端 requestId 的形状：cmb- 前缀 + 32 hex，或裸 32 hex。
//
// 用正则而非「取第一个 id 字段」：SSE 帧里 id 字段不止一个（chatcmpl-xxx 之类
// 是客户端可自造的会话 id），只有这个形状才是服务端分配的**真实** requestId，
// 而专家类任务的判据恰恰要求它是真实的（自造 UUID 不计分）。
var sseIDRe = regexp.MustCompile(`^(cmb-)?[0-9a-f]{32}$`)

// readSSERequestID 从 SSE 流中解析服务端分配的 requestId，并读干剩余流。
//
// 读干是有意的：提前 return 会让连接停在半开状态，上游侧看到的是一次
// 「发起后立刻断开」的异常会话；同时残留连接会占住连接池。
// 因此这里读完（有上限，避免异常流无限增长）再返回。
//
// 未找到合法 id 时返回错误 —— 调用方必须知道「拿不到真实 id」，
// 否则会用空 id 去发判据事件，得到一次静默不计分的上报。
func readSSERequestID(body io.Reader) (string, error) {
	// 1MB 上限：正常对话流远小于此；超过说明上游返回异常内容，
	// 继续读只是浪费带宽且可能永远读不完。
	const maxBuf = 1 << 20
	lr := io.LimitReader(body, maxBuf)
	br := bufio.NewReaderSize(lr, 8192)

	var found string
	for {
		line, err := br.ReadString('\n')
		if found == "" {
			if id := extractSSEID(line); id != "" {
				found = id
			}
		}
		if err != nil {
			// 读干剩余（上面的循环已把数据读到 EOF/LimitReader 边界）。
			_, _ = io.Copy(io.Discard, br)
			break
		}
	}
	if found == "" {
		return "", fmt.Errorf("SSE 中未找到服务端 requestId")
	}
	return found, nil
}

// extractSSEID 从一行 SSE 文本里提取符合形状的 id 值；无则返回空串。
func extractSSEID(line string) string {
	idx := strings.Index(line, `"id":"`)
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(`"id":"`):]
	end := strings.IndexByte(rest, '"')
	if end <= 0 {
		return ""
	}
	id := rest[:end]
	if sseIDRe.MatchString(id) {
		return id
	}
	return ""
}

// ReportChatActivityModel 与 ReportChatActivity 同通道（billing 域 /v2/report），
// 但把模型字段对齐到指定模型。
//
// 为什么需要它：部分任务的判据是「用过某个**特定模型**对话」
//（如 Model_chat_GLM5.2 要求 glm-5.2），而 report.go 的上报把模型写死成
// deepseek-v4-flash。事件形状完全一致、只有模型字段不同，故复用同一事件结构，
// 避免维护两份「客户端事件形状」定义而逐渐漂移。
func (c *Client) ReportChatActivityModel(a *auth.Auth, conversationID, requestID, modelID, modelName string) error {
	if requestID == "" {
		requestID = conversationID
	}
	now := time.Now().UnixMilli()
	ev := chatRequestEvent{
		EventCode:            "chat_request_send",
		Timestamp:            now,
		Mode:                 "craft",
		ConversationID:       conversationID,
		RequestID:            requestID,
		InputLength:          12,
		RequestModelID:       modelID,
		RequestModelName:     modelName,
		MentionContexts:      []any{},
		KnowledgeID:          []any{},
		KnowledgeName:        []any{},
		PresentAt:            now,
		RootRequestID:        conversationID,
		ParentConversationID: conversationID,
		AgentName:            "default",
		AgentType:            "conversation",
		UserID:               a.UID,
	}
	raw, err := json.Marshal([]chatRequestEvent{ev})
	if err != nil {
		return err
	}
	_, err = c.billingJSON(a, http.MethodPost, reportPath, json.RawMessage(raw))
	return err
}

// nightWindowStartHour / nightWindowEndHour 夜猫时段（CST）：[23:00, 08:00)。
//
// 与 scheduler 的窗口口径必须一致（那边是排程触发时机，这边是动作可行性判断）。
// 两边都按 CST 固定偏移而非本地时区：窗口来自上游活动规则，
// 换机器/改系统时区不应改变判定结果。
const (
	nightWindowStartHour = 23
	nightWindowEndHour   = 8
)

// InNightWindow 当前是否处于夜猫时段（23:00–08:00 CST）。
//
// 供 black_cat 类时段敏感任务判断：窗口外做行为不计分，
// 与其发一次无效上报再等一个永远不会达标的轮询，不如直接跳过并说明原因。
func InNightWindow(now time.Time) bool {
	h := now.UTC().Add(8 * time.Hour).Hour()
	return h >= nightWindowStartHour || h < nightWindowEndHour
}

// MarketExpert 专家市场的单个专家（市场列表响应的子集）。
type MarketExpert struct {
	ExpertID      string `json:"expert_id"`
	ExpertType    string `json:"expert_type"`
	DisplayNameZH string `json:"display_name_zh"`
	ProfessionZH  string `json:"profession_zh"`
	Version       string `json:"version"`
	Categories    []any  `json:"categories"`
}

// MarketExpertList 拉取专家市场的真实专家列表。
//
// 为什么必须"真实"：专家类任务的判据校验专家 id 在平台上真实存在，
// 自造 id 的上报会被静默丢弃（HTTP 200 但不计分）。因此 id 只能从本接口取。
func (c *Client) MarketExpertList(a *auth.Auth, expertType string) ([]MarketExpert, error) {
	body := map[string]any{"page": 1, "page_size": 20, "sort_by": "reco_rank", "sort_order": "desc"}
	if expertType != "" {
		body["expert_type"] = expertType
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	base := c.chatBase(a)
	req, err := http.NewRequest(http.MethodPost, base+"/portal/operation-platform/market/expert/list", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	h := req.Header
	h.Set("Authorization", "Bearer "+a.AccessToken)
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", desktopUA)
	h.Set("X-Domain", base)
	h.Set("X-Product", "SaaS")
	if a.UID != "" {
		h.Set("X-User-Id", a.UID)
	}
	data, err := c.doJSON(a, req)
	if err != nil {
		return nil, err
	}
	var out struct {
		Experts []MarketExpert `json:"experts"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("expert list parse: %w", err)
	}
	return out.Experts, nil
}

// expertCategory 专家的分类标记（事件载荷里的 type 字段），默认 expert-all。
func expertCategory(e MarketExpert) string {
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok && s != "" {
			return s
		}
	}
	return "expert-all"
}

// expertVersion 专家版本，缺省补 1.0.0（上游部分条目 version 为空）。
func expertVersion(e MarketExpert) string {
	if e.Version != "" {
		return e.Version
	}
	return "1.0.0"
}

// ExpertSummonSequence 「召唤平台专家」事件组，需配合 ChatWithModel 与
// ExpertActualUseEvent 构成一次完整的「召唤 + 使用」。
func ExpertSummonSequence(e MarketExpert) []DesktopEvent {
	return []DesktopEvent{
		{
			"eventCode": "web_element_click", "source": e.ExpertID,
			"type": expertCategory(e), "version": expertVersion(e),
			"elementId": "expert_summon_click", "elementName": "立即召唤",
			"pageURL": "/C:/Program%20Files/WorkBuddy/resources/app.asar/renderer/index.html",
		},
		{
			"eventCode": "expert_summon_click", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all", "position": 0,
			"expertType": e.ExpertType, "version": expertVersion(e), "mode": "LOCAL",
		},
		{
			"eventCode": "expert_summoned", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all",
		},
	}
}

// ExpertActualUseEvent 「专家真实使用」事件。requestID 必须是服务端真实分配的 id
//（见 ChatWithModel），自造 id 不计数。mode 区分 craft 与 LOCAL 两种口径，
// 由调用方按目标任务传（Expert_lighthouse 的判据要求 LOCAL）。
func ExpertActualUseEvent(e MarketExpert, conversationID, requestID, mode string) DesktopEvent {
	cost := int64(9000)
	typeField := any(expertCategory(e))
	if mode == "LOCAL" {
		// LOCAL 口径的真实样本里 type 为空、cost=0，对齐它避免形状不一致。
		typeField = ""
		cost = 0
	}
	return DesktopEvent{
		"eventCode": "expert_actual_use",
		"id":        e.ExpertID, "name": e.DisplayNameZH, "expertTitle": e.ProfessionZH,
		"type": typeField, "expertType": e.ExpertType, "source": "builtin",
		"version": expertVersion(e), "cost": cost, "characterCount": 14, "mode": mode,
		"conversationId": conversationID, "requestId": requestID,
		"messageId":      "msg-" + tail(requestID, 8),
		"requestModelId": "fast-model", "requestModelName": "fast-model",
	}
}
