// tasks.go 任务中心的**查询**能力与「整轮跑全部账号」的执行入口。
//
// 与 runner.go 的分工：
//
//	runner.go —— 单账号的动作编排（RunAll / RunOne）
//	tasks.go  —— ① 只读查询（供界面「任务中心」列表）；
//	             ② 多账号批量执行（供宿主「一键完成全部账号」）
//
// 查询与执行分开是有意的：界面刷新列表**不能**有任何副作用。
// 若把「列任务」实现成「顺手报名」，用户每刷新一次页面就会产生写操作，
// 这既违反直觉也难以排查（"我没点任何按钮，怎么账号状态变了"）。
package growtask

import (
	"context"
	"sort"
	"sync"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// 任务在「任务中心」里的展示状态（稳定的枚举，供宿主分支与上色）。
const (
	// ViewClaimed 奖励已领取。
	ViewClaimed = "claimed"
	// ViewClaimable 进度已达标、待领取。
	ViewClaimable = "claimable"
	// ViewInProgress 已参与且有进度但未达标。
	ViewInProgress = "in_progress"
	// ViewAccepted 已报名但尚无进度。
	ViewAccepted = "accepted"
	// ViewNotAccepted 尚未报名（可自动报名）。
	ViewNotAccepted = "not_accepted"
	// ViewUnsupported 需人工操作的任务。
	ViewUnsupported = "unsupported"
	// ViewLocked 上游标记未解锁。
	ViewLocked = "locked"
)

// TaskView 「任务中心」列表项：查询结果，可安全序列化给界面。
type TaskView struct {
	Code        string `json:"task_code"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"` // 客户端操作指引
	TaskDesc    string `json:"task_desc,omitempty"`   // 达成条件
	Credit      int64  `json:"credit,omitempty"`
	Energy      int64  `json:"energy,omitempty"`

	// Status 展示状态（见 View* 常量）。
	Status string `json:"status"`
	// StatusText 状态的中文说明，界面可直接显示。
	StatusText string `json:"status_text"`

	AcceptStatus string `json:"accept_status,omitempty"`
	// HasProgress 上游是否下发了进度。未报名时为 false。
	HasProgress bool   `json:"has_progress"`
	Current     int64  `json:"current"`
	Target      int64  `json:"target"`
	Progress    string `json:"progress"` // "3/5" 形式，界面直接显示

	// Claimable 是否可领取奖励（本地推算）。
	Claimable bool `json:"claimable"`
	Claimed   bool `json:"claimed"`

	// Automatable 是否可一键完成。
	Automatable bool `json:"automatable"`
	// NeedsChat 完成它是否需要消耗真实对话（界面据此提示资源消耗）。
	NeedsChat bool `json:"needs_chat,omitempty"`
	// ActionDesc 一键完成会做什么。
	ActionDesc string `json:"action_desc,omitempty"`
	// Hint 不可自动任务的人工操作指引。
	Hint string `json:"hint,omitempty"`
}

// ListTasks 查询某账号的成长任务状态（**只读**，无任何写操作）。
//
// 返回顺序：待办优先（可领奖 → 进行中 → 可报名），已领奖垫底。
// 界面据此直接渲染，不必自己再排一遍 —— 排序口径只在这里定义一次。
func (r *Runner) ListTasks(ctx context.Context, a *auth.Auth) ([]TaskView, error) {
	tasks, err := r.up.ListGrowthTasks(a)
	if err != nil {
		return nil, err
	}
	out := make([]TaskView, 0, len(tasks))
	for _, t := range tasks {
		view := TaskView{
			Code: t.Code, Title: t.Title, Description: t.Description, TaskDesc: t.TaskDesc,
			Credit: t.Credit, Energy: t.Energy,
			AcceptStatus: t.AcceptStatus, HasProgress: t.HasProgress,
			Current: t.Current, Target: t.Target, Progress: t.ProgressText(),
			Claimable: t.Claimable(), Claimed: t.Claimed(),
		}
		if act := actionFor(t.Code); act != nil {
			view.Automatable = true
			view.NeedsChat = act.NeedsChat
			view.ActionDesc = act.Desc
		} else {
			view.Hint = UnsupportedHint(t.Code)
		}
		view.Status, view.StatusText = viewStatus(t, view.Automatable)
		out = append(out, view)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return viewRank(out[i].Status) < viewRank(out[j].Status)
	})
	return out, nil
}

// PendingTasks 只返回「还没做完」的可自动任务（界面「一键完成」的作用范围）。
func (r *Runner) PendingTasks(ctx context.Context, a *auth.Auth) ([]TaskView, error) {
	all, err := r.ListTasks(ctx, a)
	if err != nil {
		return nil, err
	}
	out := make([]TaskView, 0, len(all))
	for _, v := range all {
		if v.Claimed || !v.Automatable {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// viewStatus 推导展示状态与中文说明。
func viewStatus(t upstream.GrowthTask, automatable bool) (string, string) {
	switch {
	case t.Claimed():
		return ViewClaimed, "奖励已领取"
	case t.Claimable():
		return ViewClaimable, "已达标，可领取奖励"
	case !automatable:
		return ViewUnsupported, "需在客户端手动完成"
	case t.Locked:
		return ViewLocked, "尚未解锁"
	case !t.Participated():
		return ViewNotAccepted, "未报名（可一键报名并完成）"
	case t.HasProgress:
		return ViewInProgress, "进行中"
	default:
		return ViewAccepted, "已报名"
	}
}

// viewRank 排序权重：待办在前，已完成垫底。
func viewRank(status string) int {
	switch status {
	case ViewClaimable:
		return 0
	case ViewInProgress:
		return 1
	case ViewNotAccepted, ViewAccepted:
		return 2
	case ViewUnsupported, ViewLocked:
		return 3
	default: // claimed
		return 4
	}
}

// ---------------------------------------------------------------------------
// 多账号批量执行
// ---------------------------------------------------------------------------

// AccountResult 单账号一轮执行的结果。
type AccountResult struct {
	// UID 账号标识（宿主用它回填界面；本包不打进日志）。
	UID   string `json:"uid"`
	Realm string `json:"realm"`
	// Skipped 本轮未执行（区域不符 / 无凭据 / 账号忙）。
	Skipped    bool   `json:"skipped,omitempty"`
	SkipReason string `json:"skip_reason,omitempty"`
	// Items 逐任务结果。
	Items []ItemResult `json:"items,omitempty"`
	// Claimed / Credit / Energy 本轮领奖汇总。
	Claimed int   `json:"claimed"`
	Credit  int64 `json:"credit"`
	Energy  int64 `json:"energy"`
	// Error 整轮失败的原因（列表都拉不到时）。
	Error string `json:"error,omitempty"`
}

// FleetOptions 多账号执行的参数。
type FleetOptions struct {
	// Accounts 目标账号。
	Accounts []*auth.Auth
	// Concurrency 账号间并发度（<=0 → 1，上限 3）。
	//
	// 上限刻意压到 3：每个账号的动作里都可能有真实对话，
	// 并发过高会在上游形成异常流量画像，得不偿失。
	// 账号**内**始终串行（见 Runner 的账号级互斥）。
	Concurrency int
	// IncludeIntl 是否也跑国际版账号。
	//
	// 默认 false：国际版的成长域是另一套任务集（first_chat / skill_installed …，
	// 无奖励字段、无 progress），本包的动作对它无意义。实测确认。
	IncludeIntl bool
	// OnAccount 每个账号开始前的回调（可选，供界面显示进度）。
	OnAccount func(a *auth.Auth)
	// OnResult 每个账号结束后的回调（可选）。注意：可能被并发调用。
	OnResult func(AccountResult)
}

// maxFleetConcurrency 账号间并发上限（见 FleetOptions.Concurrency 的说明）。
const maxFleetConcurrency = 3

// RunAllAccounts 对一组账号执行「一键完成」，返回逐账号结果。
//
// 账号之间可并发（默认 1，串行），账号内串行。单账号失败不影响其他账号 ——
// 一个账号登不上不该让整批停摆。
//
// 返回顺序与传入顺序一致（即使内部并发执行），宿主可直接按序渲染。
func (r *Runner) RunAllAccounts(ctx context.Context, opts FleetOptions) []AccountResult {
	accounts := opts.Accounts
	results := make([]AccountResult, len(accounts))
	conc := opts.Concurrency
	if conc <= 0 {
		conc = 1
	}
	if conc > maxFleetConcurrency {
		conc = maxFleetConcurrency
	}

	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i := range accounts {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			a := accounts[idx]
			res := r.runAccount(ctx, a, opts.IncludeIntl, opts.OnAccount)
			results[idx] = res
			if opts.OnResult != nil {
				opts.OnResult(res)
			}
		}(i)
	}
	wg.Wait()
	return results
}

// runAccount 单账号一轮：区域过滤 → 抢锁 → RunAll。
func (r *Runner) runAccount(ctx context.Context, a *auth.Auth, includeIntl bool, onStart func(*auth.Auth)) AccountResult {
	res := AccountResult{UID: a.UID, Realm: a.Realm()}
	if a.AccessToken == "" {
		res.Skipped, res.SkipReason = true, "账号无访问令牌（需重新登录）"
		return res
	}
	if !includeIntl && a.IsIntl() {
		// 国际版的成长任务是另一套（无奖励），跑本包的动作没有意义。
		res.Skipped, res.SkipReason = true, "国际版账号的成长任务体系不同，已跳过"
		return res
	}
	// 账号级互斥：用户可能同时在界面上点了单账号执行。
	if !r.Lock(a.UID) {
		res.Skipped, res.SkipReason = true, ErrAccountBusy.Error()
		return res
	}
	defer r.Unlock(a.UID)

	if onStart != nil {
		onStart(a)
	}
	items := r.RunAll(ctx, a)
	res.Items = items
	for _, it := range items {
		if it.Claimed {
			res.Claimed++
			res.Credit += it.Credit
			res.Energy += it.Energy
		}
	}
	// 整轮失败（列表都拉不到）：结果里只有一条 (任务列表) 错误项。
	if len(items) == 1 && items[0].Code == "(任务列表)" && items[0].Status == StatusError {
		res.Error = items[0].Message
	}
	return res
}

// FleetSummary 多账号结果汇总（供界面顶部横幅）。
type FleetSummary struct {
	// Accounts 参与账号数（已排除跳过的）。
	Accounts int `json:"accounts"`
	// Skipped 跳过数。
	Skipped int `json:"skipped"`
	// Claimed / Credit / Energy 合计领奖。
	Claimed int   `json:"claimed"`
	Credit  int64 `json:"credit"`
	Energy  int64 `json:"energy"`
	// Failed 有整轮错误的账号数。
	Failed int `json:"failed"`
}

// Summarize 汇总多账号结果。
func Summarize(results []AccountResult) FleetSummary {
	var s FleetSummary
	for _, r := range results {
		if r.Skipped {
			s.Skipped++
			continue
		}
		s.Accounts++
		s.Claimed += r.Claimed
		s.Credit += r.Credit
		s.Energy += r.Energy
		if r.Error != "" {
			s.Failed++
		}
	}
	return s
}
