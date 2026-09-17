// school.go 开学季活动（/portal/activity/school）接口。
//
// 这是一个**限时活动**：任务集与是否在期由服务端下发，
// 活动下线后 tasks 为空、in_period=false —— 调用方据此自动跳过，
// 不做无用请求也不报错（见 scheduler.runSchool）。
//
// 与 growth 域的区别：活动接口走 portal 前缀、基址同 billing 域
//（国服 = codebuddy.cn），且**响应信封可能不同**，故单独解析。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"

	"workbuddy2api/internal/auth"
)

// schoolTasksPath 活动任务清单（只读）。
const schoolTasksPath = "/portal/activity/school/tasks"

// schoolClaimPathFmt 领取任务奖励；路径含 task_code，**无请求体**。
//
// 注意：旧端点 /v2/activity/growth/tasks/reward/claim 是 404 错端点，勿用。
const schoolClaimPathFmt = "/portal/activity/school/tasks/%s/claim"

// SchoolTask 活动任务条目。
type SchoolTask struct {
	Code         string `json:"task_code"`
	Status       string `json:"status"`       // 如 finished / unfinished / claimed
	Progress     int    `json:"progress"`     // 已完成进度
	Target       int    `json:"target_count"` // 目标值
	RewardCredit int64  `json:"reward_credit"`
	TaskType     string `json:"task_type"` // single / recurring
}

// SchoolTasks 拉取活动任务清单，返回 (是否在活动期内, 任务列表)。
//
// 活动下线时服务端返回 tasks 为空 + in_period=false，属**正常状态**而非错误，
// 调用方据此静默跳过即可。
func (c *Client) SchoolTasks(a *auth.Auth) (inPeriod bool, tasks []SchoolTask, err error) {
	data, err := c.billingJSON(a, http.MethodGet, schoolTasksPath, nil)
	if err != nil {
		return false, nil, err
	}
	var resp struct {
		InPeriod bool         `json:"in_period"`
		Tasks    []SchoolTask `json:"tasks"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, nil, err
	}
	return resp.InPeriod, resp.Tasks, nil
}

// SchoolClaim 领取指定任务的奖励（无请求体）。
//
// 未完成的任务会返回业务错误（非 0 code），调用方按「未达标」处理即可，
// 不需要区分具体错误码 —— 每天重试一次的成本可忽略。
func (c *Client) SchoolClaim(a *auth.Auth, taskCode string) error {
	if taskCode == "" {
		return nil
	}
	_, err := c.billingJSON(a, http.MethodPost,
		fmt.Sprintf(schoolClaimPathFmt, taskCode), nil)
	return err
}
