// report.go growth 域「对话活跃上报」：POST {billingBase}/v2/report。
//
// 一条上报同时点亮 growth 连登天数、解锁 first_buddy 任务（领养前置）。
// 事件形状照抄客户端 chat_request_send（全字段，勿用最小子集 —— 上游可能随时加严）。
//
// 关键约束：事件必须带 userId（=账号 uid）。缺它时服务端返回 200 但**静默丢弃**，
// 连登天数不动 —— 表现为「上报成功但没点亮」，极难排查。
// 因此调用方上报后要回读 streak 自检（见 scheduler.checkActivityStreak）。
//
// 实测标定（2026-09-15，19 个真实账号）：
//   - 国服：上报 13/13 成功；上报前部分账号 streak.days=0，上报 1 条后全部变 1
//     → 接口确实点亮连登，且**两种区域都接受**（国际版上报同样返回 code=0）
//   - 国际版：/activity/growth/streak 恒 500（该域在国际版不可用）
//     → 故活跃上报默认只跑国服（schedule.checkin_scope=cn），
//       与签到共用同一区域开关；scope=all 时才带上国际版
package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

// reportPath 活跃上报通道。
const reportPath = "/v2/report"

// chatRequestEvent 客户端 chat_request_send 事件的完整形状。
//
// 字段与官方客户端一致；conversationId 由调用方生成，无需真实会话
// （服务端不校验会话一致性）。
type chatRequestEvent struct {
	EventCode             string `json:"eventCode"`
	Timestamp             int64  `json:"timestamp"`
	ReportDelay           int    `json:"reportDelay"`
	Mode                  string `json:"mode"`
	ConversationID        string `json:"conversationId"`
	RequestID             string `json:"requestId"`
	InputLength           int    `json:"inputLength"`
	RequestModelID        string `json:"requestModelId"`
	RequestModelName      string `json:"requestModelName"`
	IsPlan                bool   `json:"isPlan"`
	IsAutoExecuteTerminal bool   `json:"isAutoExecuteTerminal"`
	IsAutoModify          bool   `json:"isAutoModify"`
	CodebaseEnable        bool   `json:"codebaseEnable"`
	MaxToken              int    `json:"maxToken"`
	MaxSteps              int    `json:"maxSteps"`
	Temperature           int    `json:"temperature"`
	MaxRetries            int    `json:"maxRetries"`
	MentionContexts       []any  `json:"mentionContexts"`
	KnowledgeID           []any  `json:"knowledgeId"`
	KnowledgeName         []any  `json:"knowledgeName"`
	CodebaseID            string `json:"codebaseId"`
	MentionContextCount   int    `json:"mentionContextCount"`
	Command               string `json:"command"`
	ExpertID              string `json:"expertId"`
	RecommendID           string `json:"recommendId"`
	SkillID               string `json:"skillId"`
	SkillCount            int    `json:"skillCount"`
	TotalCount            int    `json:"totalCount"`
	FileURI               string `json:"fileUri"`
	PresentAt             int64  `json:"presentAt"`
	TraceID               string `json:"traceId"`
	RootRequestID         string `json:"rootRequestId"`
	ParentConversationID  string `json:"parentConversationId"`
	AgentName             string `json:"agentName"`
	AgentType             string `json:"agentType"`
	UserID                string `json:"userId"`
}

// ReportChatActivity 发送一条对话活跃上报（chat_request_send）。
//
// conversationID 由调用方生成（形如 wb2api-<毫秒>）；同一会话的多条上报共用它，
// 但 requestID 必须各不相同（服务端按事件去重）。requestID 为空时回落到 conversationID。
//
// 错误语义与 doJSON 一致：HTTP 非 2xx 或业务 code != 0 → *Error。
func (c *Client) ReportChatActivity(a *auth.Auth, conversationID, requestID string) error {
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
		RequestModelID:       "deepseek-v4-flash",
		RequestModelName:     "DeepSeek V4 Flash",
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
	// 走 billingBase（国服 = codebuddy.cn，国际版 = workbuddy.ai），
	// 与 travel 的 growthJSON（chatBase）分属两个域，不可混用。
	_, err = c.billingJSON(a, http.MethodPost, reportPath, json.RawMessage(raw))
	return err
}

// billingJSON 发 billing 域请求并解信封。
//
// 与 growthJSON 的区别仅在基址：billing 域走 billingBase（国服与 chat 不同域），
// growth 域走 chatBase。report 等 billing 端点共用本函数。
func (c *Client) billingJSON(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.billingBase(a)+path, rdr)
	if err != nil {
		return nil, err
	}
	BillingHeaders(req, a)
	return c.doJSON(a, req)
}
