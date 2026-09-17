// Package growtask 成长任务「一键完成」的编排层。
//
// 职责边界（为什么单独成包，而不是塞进 upstream 或 scheduler）：
//
//	upstream  —— 只负责「一次 HTTP 调用长什么样」（端点、头、信封、事件形状）
//	growtask  —— 只负责「按什么顺序、做几次、何时算成功、什么时候领奖」
//	scheduler —— 只负责「什么时候触发」（时点、开关、区域范围）
//
// 三者分开后，「一键完成」既能被用户手动触发（宿主调 RunOne/RunAll），
// 也能被排程复用（scheduler 定时跑），而不必把动作实现复制两份。
//
// 核心不变量：**上报 200 不等于计分**。
// 服务端的行为计分是异步的，且用错客户端指纹时会被静默丢弃（HTTP 200 但进度不动）。
// 因此每个动作完成后都必须回读任务进度，达标才领奖 —— 绝不把「上报成功」
// 当成「任务完成」。这是本包存在的根本理由。
package growtask

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/records"
	"workbuddy2api/internal/upstream"
)

// 任务执行结果状态。
const (
	// StatusDone 动作已执行（是否达标另看 Claimed/ProgressAfter）。
	StatusDone = "done"
	// StatusSkipped 无需执行（已完成 / 该账号无此任务 / 前置条件不满足）。
	StatusSkipped = "skipped"
	// StatusError 动作执行失败。
	StatusError = "error"
	// StatusUnsupported 该任务无法自动化（需真实人工动作）。
	StatusUnsupported = "unsupported"
)

// 默认调优参数。
//
// 轮询参数是实测标定值：上游计分是异步的，对话类任务完成后立即回读仍是 0/1，
// 约 5–8 秒后才变 1/1（参考实现三账号实测）。一次性回读会把「还在计分」
// 误判成「未达标」，从而跳过自动领奖 —— 奖励就此漏掉，且用户看不出来。
// 4 次 × 3 秒 ≈ 12 秒的预算足以覆盖该延迟，同时不至于让一次「一键完成」
// 卡到用户以为程序死了。
const (
	defaultPollAttempts = 4
	defaultPollGap      = 3 * time.Second
	// defaultReportGap 连续行为上报之间的间隔，对齐真实客户端的使用节奏，
	// 避免秒级连发形成异常流量画像。
	defaultReportGap = 1050 * time.Millisecond
	// defaultChatGap 真实对话之间的间隔。对话比事件上报昂贵（消耗 token 且
	// 在上游留下真实会话记录），间隔取更大值。
	defaultChatGap = 6 * time.Second
)

// Options 构造参数。
type Options struct {
	// Upstream 上游客户端（必填）。
	Upstream *upstream.Client
	// Records 账号记录写入器（nil = 不记录）。
	//
	// 为什么要它：宿主的网关子进程把 stdout 丢进 Stdio::null，
	// 任务照跑但界面上一条痕迹都没有。写 account_records.json 才能让
	// 「成长任务」出现在「账号记录 → 任务」里。
	Records *records.Recorder

	// PollAttempts / PollGap 达标回读的有界轮询预算（<=沿用默认）。
	PollAttempts int
	PollGap      time.Duration
	// ReportGap 行为上报间隔（<=沿用默认）。
	ReportGap time.Duration
	// ChatGap 真实对话间隔（<=沿用默认）。
	ChatGap time.Duration
}

// Runner 成长任务执行器。
//
// 并发模型：账号级互斥（同账号的动作串行）。同账号并发跑多个动作不仅浪费
// 上游配额（expert 系含真实对话），还会让进度回读互相干扰 ——
// A 动作刚报的事件可能被误算到 B 动作的进度差异里。
type Runner struct {
	up  *upstream.Client
	rec *records.Recorder

	pollAttempts int
	pollGap      time.Duration
	reportGap    time.Duration
	chatGap      time.Duration

	mu   sync.Mutex
	busy map[string]bool
}

// ErrAccountBusy 该账号已有任务动作在执行中。
//
// 暴露成哨兵错误而非只回一句文案：宿主可据此返回 409（与「执行失败」区分），
// 界面才能提示「请等本轮结束」而不是「任务出错」。
var ErrAccountBusy = errors.New("该账号有任务动作正在执行中")

// New 构造 Runner。
func New(o Options) *Runner {
	r := &Runner{
		up:           o.Upstream,
		rec:          o.Records,
		pollAttempts: o.PollAttempts,
		pollGap:      o.PollGap,
		reportGap:    o.ReportGap,
		chatGap:      o.ChatGap,
		busy:         make(map[string]bool),
	}
	if r.pollAttempts <= 0 {
		r.pollAttempts = defaultPollAttempts
	}
	if r.pollGap <= 0 {
		r.pollGap = defaultPollGap
	}
	if r.reportGap <= 0 {
		r.reportGap = defaultReportGap
	}
	if r.chatGap <= 0 {
		r.chatGap = defaultChatGap
	}
	return r
}

// Lock 认领账号；已在执行中返回 false。
func (r *Runner) Lock(uid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.busy[uid] {
		return false
	}
	r.busy[uid] = true
	return true
}

// Unlock 归还账号认领。
func (r *Runner) Unlock(uid string) {
	r.mu.Lock()
	delete(r.busy, uid)
	r.mu.Unlock()
}

// ItemResult 单个任务的执行结果（可直接序列化给宿主展示）。
type ItemResult struct {
	Code string `json:"task_code"`
	// Title 任务标题（取自上游列表，便于界面直接显示）。
	Title string `json:"title,omitempty"`
	// Desc 本次执行做了什么（动作说明）。
	Desc string `json:"desc,omitempty"`
	// Status StatusDone / StatusSkipped / StatusError / StatusUnsupported。
	Status string `json:"status"`
	// Message 面向用户的中文说明。
	Message string `json:"message"`
	// ProgressBefore / ProgressAfter 动作前后的进度（"当前/目标"）。
	//
	// 这是「上报 200 ≠ 计分」的证据：只有 ProgressAfter 达标才算真的完成，
	// 因此必须把它回报给用户，而不是只报「上报成功」。
	ProgressBefore string `json:"progress_before,omitempty"`
	ProgressAfter  string `json:"progress_after,omitempty"`
	// Claimed 是否本次成功领奖。
	Claimed bool  `json:"claimed,omitempty"`
	Credit  int64 `json:"credit,omitempty"`
	Energy  int64 `json:"energy,omitempty"`
	// ClaimError 达标但领奖失败的原因（用户可手动重试）。
	ClaimError string `json:"claim_error,omitempty"`
	// NeedsChat 该动作是否消耗真实对话（供界面提示资源消耗）。
	NeedsChat bool `json:"needs_chat,omitempty"`
}

// RunAll 对单个账号执行全部可自动任务，返回逐项结果。
//
// 流程（顺序有依赖，不可调换）：
//
//	1. 拉任务列表 —— 拿到每个任务的真实进度与报名状态；
//	2. 批量报名未报名的任务 —— 未报名时上游不下发 progress（实测为 null），
//	   不先报名则后续回读拿不到进度，"上报 200 ≠ 计分" 的自检就失去依据；
//	3. 逐项执行动作 —— 单项失败不影响后续项（尽力而为）；
//	4. 每项执行后回读进度（有界轮询等异步计分落定），达标即自动领奖。
//
// ctx 取消时中途停止（已完成的项保留在结果里）；锁由调用方通过 Lock 持有。
func (r *Runner) RunAll(ctx context.Context, a *auth.Auth) []ItemResult {
	var out []ItemResult

	before, err := r.up.ListGrowthTasks(a)
	if err != nil {
		// 列表都拉不到（网络/鉴权/区域），后续动作没有任何依据，直接整体报错。
		// 不在这里编造「全部跳过」：那会让用户以为任务都已处理过。
		out = append(out, ItemResult{
			Code: "(任务列表)", Status: StatusError,
			Message: "拉取任务列表失败: " + err.Error(),
		})
		r.record(a.UID, "拉取任务列表失败: "+err.Error(), records.ResultFailed)
		return out
	}
	byCode := indexTasks(before)

	// 阶段 1：批量报名。
	if res := r.acceptPending(a, before); res != nil {
		out = append(out, *res)
	}

	// 阶段 2：逐项执行。
	for _, act := range actions {
		if ctx.Err() != nil {
			out = append(out, ItemResult{
				Code: act.Code, Status: StatusSkipped,
				Message: "已取消（宿主提前结束本轮）",
			})
			break
		}
		item := r.runAction(ctx, a, act, byCode[act.Code])
		out = append(out, item)
		if err := sleepCtx(ctx, r.reportGap); err != nil {
			break // 取消：不再继续后续项
		}
	}
	r.summarize(a, out)
	return out
}

// RunOne 对单个任务执行「一键完成」。
//
// 与 RunAll 的区别只在范围：报名、动作、回读、领奖四步完全相同，
// 因此复用同一套实现（重复实现两份是进度回读口径漂移的常见来源）。
func (r *Runner) RunOne(ctx context.Context, a *auth.Auth, code string) ItemResult {
	act := actionFor(code)
	if act == nil {
		return ItemResult{
			Code: code, Status: StatusUnsupported,
			Message: "该任务需要客户端内的人工操作（无对应接口），无法自动完成；请在官方客户端按任务说明操作",
		}
	}
	tasks, err := r.up.ListGrowthTasks(a)
	if err != nil {
		return ItemResult{Code: code, Status: StatusError, Message: "拉取任务列表失败: " + err.Error()}
	}
	t := indexTasks(tasks)[code]
	if t == nil {
		return ItemResult{Code: code, Status: StatusSkipped, Message: "该账号没有此任务"}
	}
	// 未报名则先报名：未报名时上游不下发 progress，动作执行后无法回读验证。
	if t.NeedsAccept() {
		if _, err := r.up.AcceptGrowthTasks(a, []string{code}); err != nil {
			log.Printf("growtask: accept %s uid=%s: %v（继续走行为链路）", code, uid8(a.UID), err)
		} else if refreshed := r.refetch(a, code); refreshed != nil {
			t = refreshed
		}
	}
	item := r.runAction(ctx, a, *act, t)
	r.record(a.UID, item.Message, resultFor(item.Status))
	return item
}

// acceptPending 批量报名尚未报名的任务，返回一条汇总结论（无待报名项时返回 nil）。
func (r *Runner) acceptPending(a *auth.Auth, tasks []upstream.GrowthTask) *ItemResult {
	var codes []string
	for _, t := range tasks {
		if t.NeedsAccept() {
			codes = append(codes, t.Code)
		}
	}
	if len(codes) == 0 {
		return nil
	}
	res := &ItemResult{Code: "(批量报名)", Desc: "报名尚未参加的任务"}
	results, err := r.up.AcceptGrowthTasks(a, codes)
	if err != nil {
		// 报名失败不阻塞后续动作：行为事件才是进度的唯一判据，
		// 而报名只是让服务端开始跟踪进度。若真的因为未报名而不计分，
		// 后续的回读会如实显示 0/N —— 结论仍然诚实。
		res.Status = StatusError
		res.Message = fmt.Sprintf("报名 %d 个任务失败（不阻塞后续执行）: %v", len(codes), err)
		return res
	}
	res.Status = StatusDone
	// 逐条统计 accepted / already_accepted，两者都是正常结果。
	news := 0
	for _, one := range results {
		if one.Status == upstream.TaskAcceptAccepted {
			news++
		}
	}
	res.Message = fmt.Sprintf("已报名 %d 个任务（其中 %d 个为本次新报名）", len(codes), news)
	return res
}

// runAction 执行单项任务的完整闭环：执行 → 回读（轮询）→ 达标领奖。
func (r *Runner) runAction(ctx context.Context, a *auth.Auth, act action, before *upstream.GrowthTask) ItemResult {
	item := ItemResult{Code: act.Code, Desc: act.Desc, NeedsChat: act.NeedsChat}

	// 前置：已领取或已达标的任务直接跳过（幂等，不浪费上游配额）。
	if before == nil {
		item.Status = StatusSkipped
		item.Message = "该账号没有此任务"
		return item
	}
	item.Title = before.Title
	item.ProgressBefore = before.ProgressText()
	if before.Claimed() {
		item.Status = StatusSkipped
		item.Message = "该任务奖励已领取"
		return item
	}
	if before.Claimable() {
		// 已达标但没领过：补领奖即可，不必再跑一遍动作。
		item.Status = StatusDone
		item.Message = "进度已达标，直接领奖"
		item.ProgressAfter = before.ProgressText()
		r.claim(a, act.Code, &item)
		return item
	}

	msg, err := act.run(r, ctx, a, before)
	if err != nil {
		item.Status = StatusError
		item.Message = err.Error()
		return item
	}
	item.Status = StatusDone
	item.Message = msg

	// 回读验证：上报 200 ≠ 计分，用有界轮询等异步计分落定。
	after := r.waitProgress(ctx, a, act.Code)
	if after != nil {
		item.ProgressAfter = after.ProgressText()
		if after.Claimable() {
			r.claim(a, act.Code, &item)
			return item
		}
		// 无奖励 + 进度未推进：必须如实说明，不能把「上报成功」写成「已完成」。
		//
		// 触发场景是 black_cat（0 积分 0 能量）：实测在同一夜内用三种判据口径
		//（CLI 对话+模型对齐上报 / 桌面对话事件链 / 真实 requestId 对话链）
		// 各做一次真实对话，进度都停在 1/3 不动 —— 而它的目标恰好是 3。
		// 最可能的原因是**按夜计数**（每夜最多计 1 次，需跨 3 夜），
		// 这是单夜实测无法证实也无法证伪的，故只陈述观察到的事实 + 标注推断。
		//
		// 之所以不直接判为失败：动作确实执行了（对话真的发了），
		// 判失败会让用户以为程序出错；而沉默不说又会让用户以为任务已完成。
		if !after.Claimable() && !after.Claimed() &&
			before.Credit == 0 && before.Energy == 0 && after.Current <= before.Current {
			item.Message += "；但进度未推进（该任务无奖励，且实测同一夜内多次真实对话不累加，疑为按夜计数、需跨夜完成）"
		}
	}
	return item
}

// claim 领奖并把结果写进 item。
//
// 领奖失败**不**把整项判成 error：动作本身已经成功（进度确实推进了），
// 把它标成失败会让用户以为白做了。真实情况是「达标但领奖没成功」，
// 用 ClaimError 单独表达，用户可在界面上手动重试。
func (r *Runner) claim(a *auth.Auth, code string, item *ItemResult) {
	credit, energy, already, err := r.up.ClaimGrowthTask(a, code)
	if err != nil {
		item.ClaimError = err.Error()
		if upstream.IsGrowthTaskNotCompleted(err) {
			// 上游说未达标：进度回读达标但领奖被拒，通常是计分还没落定。
			item.Message += "；领奖时上游仍报未达标（计分可能还在落定），可稍后重试"
		} else {
			item.Message += "；达标但领奖失败（可稍后重试）"
		}
		return
	}
	if already {
		item.Message += "；奖励此前已领取"
		return
	}
	item.Claimed = true
	item.Credit = credit
	item.Energy = energy
	item.Message += fmt.Sprintf("；已自动领奖 +%d 积分 +%d 能量", credit, energy)
}

// waitProgress 在轮询预算内回读任务进度，直到达标/已领取或预算耗尽。
//
// 达标即返回（不空转满预算）；预算耗尽返回最后一次结果（可能仍未达标）——
// 返回 nil 表示连查询都失败，此时调用方不应把进度写成某个猜测值。
func (r *Runner) waitProgress(ctx context.Context, a *auth.Auth, code string) *upstream.GrowthTask {
	t := r.refetch(a, code)
	if t == nil || t.Claimable() || t.Claimed() {
		return t
	}
	for i := 1; i < r.pollAttempts; i++ {
		if err := sleepCtx(ctx, r.pollGap); err != nil {
			return t // 取消：返回已拿到的最佳信息，不假装达标
		}
		next := r.refetch(a, code)
		if next == nil {
			return t // 轮询期间的查询失败不覆盖已有结果
		}
		t = next
		if t.Claimable() || t.Claimed() {
			return t
		}
	}
	return t
}

// refetch 拉取单条任务的最新状态；查询失败或任务不存在返回 nil。
func (r *Runner) refetch(a *auth.Auth, code string) *upstream.GrowthTask {
	tasks, err := r.up.ListGrowthTasks(a)
	if err != nil {
		log.Printf("growtask: refetch %s uid=%s: %v", code, uid8(a.UID), err)
		return nil
	}
	return indexTasks(tasks)[code]
}

// indexTasks 按任务码建索引。
func indexTasks(tasks []upstream.GrowthTask) map[string]*upstream.GrowthTask {
	m := make(map[string]*upstream.GrowthTask, len(tasks))
	for i := range tasks {
		if tasks[i].Code != "" {
			m[tasks[i].Code] = &tasks[i]
		}
	}
	return m
}

// summarize 把整轮结果写一条账号记录（只在真有动作或失败时写，避免刷屏）。
func (r *Runner) summarize(a *auth.Auth, items []ItemResult) {
	done, skipped, failed, claimed := 0, 0, 0, 0
	var credit, energy int64
	for _, it := range items {
		switch it.Status {
		case StatusDone:
			done++
		case StatusSkipped:
			skipped++
		case StatusError:
			failed++
		}
		if it.Claimed {
			claimed++
			credit += it.Credit
			energy += it.Energy
		}
	}
	detail := fmt.Sprintf("执行 %d 项（跳过 %d、失败 %d），领奖 %d 项 +%d 积分 +%d 能量",
		done, skipped, failed, claimed, credit, energy)
	log.Printf("growtask: uid=%s %s", uid8(a.UID), detail)
	// 一轮什么都没做（全部跳过）不值得写记录：那是「无变化」，天天写只会刷屏。
	if done == 0 && failed == 0 {
		return
	}
	result := records.ResultSuccess
	if failed > 0 {
		result = records.ResultFailed
	}
	r.record(a.UID, detail, result)
}

// record 写一条账号记录（Records 为 nil 时是安全的空操作）。
func (r *Runner) record(uid, detail, result string) {
	if r.rec == nil {
		return
	}
	// 用 Daily 而非 Task：用户连点「一键完成」会产生多轮，
	// 每轮都写会让记录区被同一件事刷满。
	r.rec.TaskDaily(uid, recordsTitle, result, detail)
}

// recordsTitle 账号记录里的任务名（宿主界面按此分组显示）。
const recordsTitle = "成长任务"

// resultFor 把执行状态映射为账号记录的结果类型。
func resultFor(status string) string {
	if status == StatusError {
		return records.ResultFailed
	}
	return records.ResultSuccess
}

// sleepCtx 睡满 d；ctx 取消时立即返回错误。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// uid8 日志用短 uid（前 8 字符）。不打印完整 uid：它属于账号标识，
// 日志会被贴进 issue。
func uid8(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
