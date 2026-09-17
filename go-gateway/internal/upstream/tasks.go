// tasks.go growth 域「成长任务」接口：列表查询 / 报名 / 领奖。
//
// 端点（全部实测确认，2026-09-16，真实账号）：
//
//	GET  {chatBase}/v2/activity/growth/tasks                 全量任务列表
//	POST {chatBase}/v2/activity/growth/tasks/accept          {"task_codes":[...]}
//	POST {webBase}/activity/growth/tasks/{task_code}/claim   领奖（无请求体）
//
// 领奖**必须走 Web 域**（www.workbuddy.cn）。CLI 域上形如
// /v2/activity/growth/tasks/reward/claim 的路径不存在：实测恒返回 HTTP 400，
// 与"任务未完成"的报错无法区分，极易被误判成「进度没达标」而反复重试上报 ——
// 这是比端点写错本身更难排查的失败形态，故在此显式记录。
//
// 语义要点（实测）：
//
//   - accept 是「报名」，本身不产生进度；但它**不是纯装饰** —— 未报名的任务
//     上游把 progress 整个下发为 null（实测 16/18 个任务如此），只有报名后
//     才会出现 {current,target}。因此「先报名再上报行为」是必要的顺序，
//     否则进度无从回读，"上报 200 ≠ 计分" 的自检也就失去了依据。
//   - accept 可幂等重放：重复报名返回 status=already_accepted 而非报错。
//   - claim 仅进度达标后可领；重复领返回 already_claimed=true（幂等）。
package upstream

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"workbuddy2api/internal/auth"
)

// growth 域任务路径。
const (
	growthTasksListPath   = "/v2/activity/growth/tasks"
	growthTasksAcceptPath = "/v2/activity/growth/tasks/accept"
	// growthTaskClaimPathFmt 领奖路径：任务码在**路径**里，且基址是 Web 域。
	growthTaskClaimPathFmt = "/activity/growth/tasks/%s/claim"
)

// 任务报名状态取值（实测，2026-09-16 真实账号）。
//
// 状态机：not_accepted --报名--> accepted --产生进度--> in_progress --> claimed
//
// 注意 `in_progress` 是 2026-09-16 联调时才观察到的第三个状态：
// 任务报名后进度一旦大于 0，accept_status 就从 accepted 变成 in_progress。
// 早期只登记了 accepted/claimed 两个值，导致 NeedsAccept 用
// `!= accepted` 判定时会把进行中的任务误判成"未报名"并重复报名。
const (
	// TaskAcceptNotAccepted 未报名。此时上游不下发 progress（为 null）。
	TaskAcceptNotAccepted = "not_accepted"
	// TaskAcceptAccepted 已报名，尚无进度。进度开始被服务端跟踪。
	TaskAcceptAccepted = "accepted"
	// TaskAcceptInProgress 已报名且进度已大于 0。
	TaskAcceptInProgress = "in_progress"
	// TaskAcceptClaimed 奖励已领取。
	TaskAcceptClaimed = "claimed"
)

// GrowthTask 成长任务条目。
//
// 字段名与上游 JSON 对齐（上游用 reward_ 前缀）；对外只透出这个子集，
// 避免把 icon_url / 弹窗按钮样式等纯展示字段带进日志与接口。
type GrowthTask struct {
	Code         string `json:"task_code"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"` // 操作指引（含客户端跳转说明）
	TaskDesc     string `json:"task_desc,omitempty"`   // 达成条件简述
	Credit       int64  `json:"reward_credit,omitempty"`
	Energy       int64  `json:"reward_energy,omitempty"`
	RewardBuddy  bool   `json:"reward_buddy,omitempty"`
	HasReward    bool   `json:"has_reward,omitempty"`
	TaskType     string `json:"task_type,omitempty"`
	Tag          string `json:"tag,omitempty"`      // 端标记（PC / 限定 / 限量）
	JumpURL      string `json:"jump_url,omitempty"` // workbuddy:// 跳转协议
	Locked       bool   `json:"locked,omitempty"`
	AcceptStatus string `json:"accept_status,omitempty"`

	// HasProgress 上游是否下发了 progress 对象。
	//
	// 必须与 Current/Target 分开表达：未报名时 progress 为 **null**，
	// 若把它当成 {0,0} 就分不清「未报名、进度未知」与「已报名、进度为零」，
	// 而这两种状态的下一步动作完全不同（前者要报名，后者要上报行为）。
	HasProgress bool  `json:"has_progress"`
	Current     int64 `json:"current"`
	Target      int64 `json:"target"`
}

// Participated 报告该任务是否**已参与**（报名过或已领奖）。
//
// 为什么不能只判 `== accepted`：报名后只要产生了进度，状态就变成
// in_progress（实测）。用 `!= accepted` 判定会把「进行中」误判成「未报名」，
// 于是每轮一键完成都重复报名一遍 —— 虽然上游幂等不会出错，
// 但会对上游发出无意义请求，并让「本次新报名 N 个」的汇报数字虚高。
func (t GrowthTask) Participated() bool {
	switch t.AcceptStatus {
	case TaskAcceptAccepted, TaskAcceptInProgress, TaskAcceptClaimed:
		return true
	default:
		return false
	}
}

// Claimed 奖励是否已领取。
func (t GrowthTask) Claimed() bool { return t.AcceptStatus == TaskAcceptClaimed }

// NeedsAccept 是否需要报名。
//
// 已领取的任务不再报名：上游对已领取任务的报名语义未定义，重放没有收益，
// 且会给「一键完成」的结果里混入无意义的动作记录。
//
// 另：lock 的任务也不报名 —— 上游标记未解锁时报名必然被拒，
// 发出去只会得到一条失败记录。
func (t GrowthTask) NeedsAccept() bool {
	return !t.Participated() && !t.Locked
}

// Claimable 进度达标且尚未领取（本地推算，用于决定是否自动领奖）。
//
// 三重条件缺一不可：未领取、进度已知、目标为正且已达成。
// 要求 Target > 0 是有意的 —— 上游对无进度语义的任务（如国际版任务清单）
// 不下发 progress，此时 Target 恰为 0，若不排除就会被误判成「已达标」并触发领奖。
func (t GrowthTask) Claimable() bool {
	return !t.Claimed() && t.HasProgress && t.Target > 0 && t.Current >= t.Target
}

// ProgressText 进度的可读表示，用于日志与结果汇报的「前后对比」。
func (t GrowthTask) ProgressText() string {
	switch {
	case t.HasProgress:
		return itoa64(t.Current) + "/" + itoa64(t.Target)
	case t.Claimed():
		return "claimed"
	case t.AcceptStatus != "":
		return t.AcceptStatus
	default:
		return "?"
	}
}

// itoa64 小整数转字符串（避免为一个日志文案引入 strconv 依赖语义歧义）。
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// growthTaskWire 上游任务条目的线上形状。
//
// 同时登记新旧两套字段名（task_code/code、accept_status/status）：
// 国际版任务清单用的是旧形状（code + status + level_name），
// 严格按新形状解析会得到一堆空 code 的条目，进而在日志里显示成空白任务名。
// 兼容成本极低而误判成本很高，故两套都收。
type growthTaskWire struct {
	TaskCode     string `json:"task_code"`
	Code         string `json:"code"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	TaskDesc     string `json:"task_desc"`
	RewardCredit int64  `json:"reward_credit"`
	RewardEnergy int64  `json:"reward_energy"`
	RewardBuddy  bool   `json:"reward_buddy"`
	HasReward    bool   `json:"has_reward"`
	TaskType     string `json:"task_type"`
	Tag          string `json:"tag"`
	JumpURL      string `json:"jump_url"`
	Locked       bool   `json:"locked"`
	AcceptStatus string `json:"accept_status"`
	Status       string `json:"status"`
	Progress     *struct {
		Current int64 `json:"current"`
		Target  int64 `json:"target"`
	} `json:"progress"`
}

// ListGrowthTasks 拉取全量成长任务列表。
//
// 区域差异：国际版（workbuddy.ai）的同名端点返回的是另一套 5 项任务
//（first_chat / skill_installed / ...，无奖励字段、无 progress），
// 本函数的解析对两套形状都成立，但调用方应据 realm 决定是否推进
//（国际版没有可领的成长任务奖励，见 growtask.Runner 的区域过滤）。
func (c *Client) ListGrowthTasks(a *auth.Auth) ([]GrowthTask, error) {
	data, err := c.growthJSON(a, http.MethodGet, growthTasksListPath, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tasks []growthTaskWire `json:"tasks"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	out := make([]GrowthTask, 0, len(resp.Tasks))
	for _, t := range resp.Tasks {
		code := t.TaskCode
		if code == "" {
			code = t.Code
		}
		status := t.AcceptStatus
		if status == "" {
			status = t.Status
		}
		task := GrowthTask{
			Code:         code,
			Title:        t.Title,
			Description:  t.Description,
			TaskDesc:     t.TaskDesc,
			Credit:       t.RewardCredit,
			Energy:       t.RewardEnergy,
			RewardBuddy:  t.RewardBuddy,
			HasReward:    t.HasReward,
			TaskType:     t.TaskType,
			Tag:          t.Tag,
			JumpURL:      t.JumpURL,
			Locked:       t.Locked,
			AcceptStatus: status,
		}
		// progress 为 null 时保持 HasProgress=false：这是「未报名」的信号，
		// 不能被折叠成 0/0（见 GrowthTask.HasProgress 的说明）。
		if t.Progress != nil {
			task.HasProgress = true
			task.Current = t.Progress.Current
			task.Target = t.Progress.Target
		}
		if task.Code == "" {
			continue // 无任务码的条目无法驱动任何动作，丢弃而非透出空条目
		}
		out = append(out, task)
	}
	return out, nil
}

// AcceptResult 单个任务的报名结果。
type AcceptResult struct {
	Code   string `json:"task_code"`
	Status string `json:"status"` // accepted / already_accepted
}

// AcceptGrowthTasks 报名一批任务，返回上游对每个任务的处理结果。
//
// 幂等：重复报名返回 status=already_accepted 且 HTTP 200，
// 因此调用方无需先查状态再决定是否报名，直接重放即可。
func (c *Client) AcceptGrowthTasks(a *auth.Auth, codes []string) ([]AcceptResult, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	data, err := c.growthJSON(a, http.MethodPost, growthTasksAcceptPath,
		map[string]any{"task_codes": codes})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Results []AcceptResult `json:"results"`
	}
	// 结果解析失败不算失败：报名请求本身已成功，逐条状态只是锦上添花。
	// 把整个报名判成错误会让调用方重试，而重试并不能改善这里的解析。
	_ = json.Unmarshal(data, &resp)
	return resp.Results, nil
}

// ClaimGrowthTask 领取成长任务奖励，返回本次到账的积分与能量。
//
// 返回值 already 表示上游回答「此前已领过」（幂等重放，不算错误）。
//
// 端点细节（实测，勿按直觉改写）：
//   - 基址是 **Web 域**（www.workbuddy.cn），不是 chat/billing 域；
//   - 任务码在**路径**里，无请求体；
//   - 必须带 x-client-platform: web 与指向成长中心的 Origin/Referer，
//     否则被上游按「非 Web 来源」拒绝。
//
// 注意 ClaimGrowthTask 与 chat 的鉴权头不同：这里直接构造头而不用 ChatHeaders，
// 因为成长中心是网页端调用，其 Origin/Referer/UA 与 CLI 完全不同 ——
// 复用 CLI 头族会让请求看起来是「CLI 在调网页接口」。
func (c *Client) ClaimGrowthTask(a *auth.Auth, taskCode string) (credit, energy int64, already bool, err error) {
	if strings.TrimSpace(taskCode) == "" {
		return 0, 0, false, nil
	}
	base := c.webBase(a)
	req, err := http.NewRequest(http.MethodPost,
		base+"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", nil)
	if err != nil {
		return 0, 0, false, err
	}
	h := req.Header
	h.Set("Authorization", "Bearer "+a.AccessToken)
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Content-Type", "application/json")
	h.Set("Origin", base)
	h.Set("Referer", base+"/profile/growth-center")
	h.Set("x-client-platform", "web")
	h.Set("User-Agent", webUA)
	if a.UID != "" {
		h.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		h.Set("X-Enterprise-Id", a.EnterpriseID)
		h.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		h.Set("X-Domain", a.Domain)
	}

	data, err := c.doJSON(a, req)
	if err != nil {
		return 0, 0, false, err
	}
	var resp struct {
		AlreadyClaimed bool  `json:"already_claimed"`
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, false, err
	}
	if resp.AlreadyClaimed {
		return 0, 0, true, nil
	}
	return resp.Credit, resp.Energy, false, nil
}

// growthTaskNotCompletedMarkers 领奖被拒时的「进度未达标」文案特征。
//
// 上游在进度不足时返回业务错误而非静默失败，文案形态多样
//（task not completed / not finished / 未完成），登记多种以便调用方
// 能把「没达标」与「真的出错」区分开：前者应等待计分落定后重试，
// 后者应放弃并留痕，混为一谈会让日志把正常等待渲染成故障。
var growthTaskNotCompletedMarkers = []string{
	"task not completed",
	"not completed yet",
	"task not finished",
	"not finished",
	"未完成",
	"进度不足",
}

// IsGrowthTaskNotCompleted 报告领奖错误是否为「进度未达标」。
//
// 只认 4xx：5xx 是上游故障，与任务进度无关，归为「未达标」会让调用方
// 白白等待一个永远不会靠等待解决的错误。
func IsGrowthTaskNotCompleted(err error) bool {
	if err == nil {
		return false
	}
	ue, ok := err.(*Error)
	if !ok || ue.Status < 400 || ue.Status >= 500 {
		return false
	}
	lower := strings.ToLower(ue.Msg)
	for _, m := range growthTaskNotCompletedMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}
