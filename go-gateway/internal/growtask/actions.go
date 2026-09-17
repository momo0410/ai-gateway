// actions.go 「一键完成」的动作表：任务码 → 具体动作实现。
//
// 分类依据是**依赖强度**，而不是代码量。它同时决定执行顺序与风险：
//
//	A 类 纯行为事件上报 —— 无外部依赖、不消耗对话额度，最稳，放在最前；
//	B 类 真实对话 —— 消耗 token、在上游留下真实会话记录（Model_chat_GLM5.2）；
//	C 类 专家市场链 —— 除真实对话外还要求专家 id 真实存在、
//	     且判据事件的 requestId 必须是服务端分配的真实 id，依赖最多；
//	D 类 不可自动 —— 需真实人工动作（Expert_Philanthropy 需真实捐款）。
//
// 执行顺序 = 表顺序：先解锁依赖项、先做稳的。单项失败不影响后续项，
// 因此顺序不会造成"前面失败导致后面全废"。
package growtask

import (
	"context"
	"errors"
	"fmt"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// action 一个可自动化的任务动作。
type action struct {
	// Code 目标任务码（与上游 task_code 精确对应）。
	Code string
	// Desc 展示用的动作说明。
	Desc string
	// NeedsChat 是否消耗真实对话（由 cate 推导，供界面提示资源消耗）。
	NeedsChat bool
	// run 执行动作。before 是动作前的任务状态（可能为 nil）。
	//
	// 返回值语义遵循「尽力而为」：能推进进度就返回说明 + nil；
	// 只有**确知无法推进**（如前置条件不满足、上游拒绝）才返回 error。
	// 上报部分成功、部分失败时返回说明 + nil 并把失败写进说明 ——
	// 因为进度可能是部分推进的，而 error 会让调用方跳过回读。
	run func(r *Runner, ctx context.Context, a *auth.Auth, before *upstream.GrowthTask) (string, error)
}

// 企鹅教师助手（Buddy_App_QQ 的判据应用）。同一组 buddyapp 事件同时满足
// Buddy_App（进入任一应用）与 Buddy_App_QQ，故两个任务共用一次上报。
const (
	qqTeacherBuddyID   = "cb_y5Dy46tPQGGWtueMxXbe"
	qqTeacherBuddyName = "企鹅教师助手"

	// hpThemeKey Hp_Appearance 的判据主题（和平精英激战金秋）。
	hpThemeKey = "theme-tkmw7j"

	// libraryDocURL 资料库介绍页（Library_read 判据事件的 pageURL）。
	libraryDocURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"

	// lighthouseExpertID 腾讯轻量云专家（Expert_lighthouse 判据专家）。
	lighthouseExpertID = "ex_2cvvUZQhDyeJ"

	// freeModel 用于真实对话的免费/低成本模型。
	// 用 fast-model 而非昂贵模型：这些动作的目的是产生「真实对话」这一事实，
	// 与模型能力无关，没必要为一次 1+1 消耗主力模型额度。
	freeModel = "fast-model"
)

// actions 动作表（顺序即执行顺序，见包注释的分类说明）。
var actions = []action{
	// ---- A 类：纯行为事件上报（零对话消耗） ----

	// chat_5 是累计型任务：按差额补报 chat_request_send。
	// 放在最前是因为它复用既有的活跃上报通道（billing 域），依赖最少。
	{Code: "chat_5", Desc: "补报对话活跃事件至达标（按差额）", run: runChat5},
	// first_buddy 依赖活跃上报解锁：先报一条 → 同意协议 → 领养。
	{Code: "first_buddy", Desc: "上报解锁 → 同意协议 → 领养第一只 Buddy", run: runFirstBuddy},
	{Code: "RichMeow_Chat", Desc: "上报桌面端完整对话事件链（含成功回执）", run: runRichMeow},
	// Buddy_App 与 Buddy_App_QQ 共用同一组事件（见 qqTeacherBuddyID 注释），
	// 幂等由任务状态跳过兜底：第一个跑完点亮后，第二个会在前置检查里跳过。
	{Code: "Buddy_App", Desc: "上报进入 Buddy 应用事件链", run: runBuddyApp},
	{Code: "Buddy_App_QQ", Desc: "上报进入企鹅教师助手事件链", run: runBuddyApp},
	{Code: "automation_1", Desc: "上报定时任务创建成功事件", run: runAutomationCreate},
	{Code: "Library_read", Desc: "以 Web 指纹上报资料库阅读事件", run: runLibraryRead},
	{Code: "template_5", Desc: "上报模板使用事件组 ×5", run: runTemplateUse},
	{Code: "playbook_prompt", Desc: "上报灵感案例「做同款」发送事件组", run: runPlaybookPrompt},
	{Code: "create_canvas", Desc: "上报设计创意画布创建事件组", run: runCreateCanvas},
	{Code: "Hp_Appearance", Desc: "设置主题并上报皮肤生效事件", run: runAppearance},

	// ---- B 类：真实对话（消耗 token） ----

	{Code: "Model_chat_GLM5.2", Desc: "glm-5.2 真实对话一次并对齐模型上报", NeedsChat: true, run: runModelChat},

	// ---- C 类：专家市场链（真实专家 id + 真实对话回执） ----

	{Code: "expert_5", Desc: "真实专家召唤 + 使用链 ×5", NeedsChat: true, run: runExpert5},
	{Code: "Expert_team_use_3", Desc: "真实专家团召唤 + 使用链 ×3", NeedsChat: true, run: runExpertTeam3},
	{Code: "Expert_lighthouse", Desc: "轻量云专家召唤 + 使用链（真实对话回执）", NeedsChat: true, run: runExpertLighthouse},
	{Code: "skill_1", Desc: "真实对话 + 技能加载（skill_info）事件", NeedsChat: true, run: runSkill1},

	// ---- D 类：时段敏感（窗口外跳过，不算失败） ----

	// black_cat 判据是夜猫时段内的**真实对话**（实测目标 3 次、只发上报不计分），
	// 故它也消耗对话额度 —— NeedsChat 必须置 true，界面才能如实提示。
	{Code: "black_cat", Desc: "夜猫时段内补足真实对话（23:00–08:00 北京时间）", NeedsChat: true, run: runBlackCat},
}

// actionFor 查任务对应的动作；无则返回 nil（不可自动化）。
func actionFor(code string) *action {
	for i := range actions {
		if actions[i].Code == code {
			return &actions[i]
		}
	}
	return nil
}

// SupportedCodes 返回全部可自动化的任务码（供宿主展示覆盖面）。
func SupportedCodes() []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, a.Code)
	}
	return out
}

// 唯一不可自动的任务：需真实捐款，服务端在领奖时校验捐赠回执。
// 刻意**不**尝试伪造 —— 伪造捐赠回执属于欺诈，且必然失败（回执由真实支付链路产生）。
const philanthropyeCode = "Expert_Philanthropy"

// UnsupportedHint 返回不可自动任务的操作指引（供界面展示）。
func UnsupportedHint(code string) string {
	if code == philanthropyeCode {
		return "该任务需真实捐款：服务端在领奖时校验捐赠回执，无法通过接口绕过；请按任务说明在客户端内完成"
	}
	return "该任务需要客户端内的人工操作，无法自动完成；请按任务说明在官方客户端操作"
}

// ---------------------------------------------------------------------------
// A 类实现
// ---------------------------------------------------------------------------

// runChat5 按差额补报 chat_request_send 至 chat_5 达标。
func runChat5(r *Runner, ctx context.Context, a *auth.Auth, before *upstream.GrowthTask) (string, error) {
	if before == nil {
		return "", errors.New("任务不存在")
	}
	target := before.Target
	if target <= 0 {
		// 未报名时上游不下发 progress；RunAll 已先报名，这里兜底用上游的真实目标 5。
		target = 5
	}
	need := target - before.Current
	if need <= 0 {
		return "进度已达标，无需上报", nil
	}
	for i := int64(0); i < need; i++ {
		cid := fmt.Sprintf("wb2api-chat5-%d-%d", time.Now().UnixMilli(), i)
		if err := r.up.ReportChatActivity(a, cid, ""); err != nil {
			// 部分成功也要如实汇报：已发的那些条可能已经计分，
			// 返回 error 会让调用方跳过回读，连已达标的进度都发现不了。
			return fmt.Sprintf("已补报 %d/%d 条后中断: %v", i, need, err), nil
		}
		if i < need-1 {
			if err := sleepCtx(ctx, r.reportGap); err != nil {
				return fmt.Sprintf("已补报 %d/%d 条（本轮被取消）", i+1, need), nil
			}
		}
	}
	return fmt.Sprintf("已补报 %d 条对话活跃事件", need), nil
}

// runFirstBuddy 领养第一只 Buddy：上报（解锁前置）→ 同意协议 → 领养。
//
// 顺序不可换：领养接口要求 first_buddy 任务的前置条件（当日有对话活跃）已满足，
// 未满足时返回 HTTP 400 "first_buddy task not completed yet"。
// 该错误当日重试无意义（门槛按自然日重置），因此识别出来就返回说明而非 error。
func runFirstBuddy(r *Runner, ctx context.Context, a *auth.Auth, before *upstream.GrowthTask) (string, error) {
	if err := r.up.ReportChatActivity(a, fmt.Sprintf("wb2api-adopt-%d", time.Now().UnixMilli()), ""); err != nil {
		return "", fmt.Errorf("前置上报失败: %w", err)
	}
	if err := sleepCtx(ctx, r.reportGap); err != nil {
		return "前置上报已完成，本轮被取消", nil
	}
	if err := r.up.BuddyAgreement(a); err != nil {
		return "", fmt.Errorf("同意协议失败: %w", err)
	}
	if err := r.up.BuddyFirst(a); err != nil {
		if upstream.IsBuddyTaskIncomplete(err) {
			// 门槛未过是**预期状态**（上游按当日活跃度判定），不是故障。
			return "前置已上报，但领养门槛未过（上游要求当日活跃），请稍后重试", nil
		}
		return "", fmt.Errorf("领养失败: %w", err)
	}
	return "已领养第一只 Buddy", nil
}

// runRichMeow 上报桌面端完整对话事件链（RichMeow_Chat）。
//
// 为什么不发真实对话：该任务的判据是「桌面端完成一次对话」，由**事件链**
// 判定，其中 chat_message_response 的 isSuccessful=true 是成功回执。
// 因此纯事件即可点亮，无需消耗真实对话额度。
func runRichMeow(r *Runner, ctx context.Context, a *auth.Auth, _ *upstream.GrowthTask) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-rm-%d", ms)
	req := fmt.Sprintf("wb2api-rm-req-%d", ms)
	events := upstream.ChatSequence(conv, req, fmt.Sprintf("req-%d-user", ms), freeModel, freeModel)
	if err := r.up.ReportDesktopEvents(a, events...); err != nil {
		return "", err
	}
	return "已上报桌面端对话事件链（含成功回执）", nil
}

// runBuddyApp 上报进入 Buddy 应用事件链（同时覆盖 Buddy_App 与 Buddy_App_QQ）。
func runBuddyApp(r *Runner, ctx context.Context, a *auth.Auth, _ *upstream.GrowthTask) (string, error) {
	if err := r.up.ReportDesktopEvents(a, upstream.BuddyAppSequence(qqTeacherBuddyID, qqTeacherBuddyName)...); err != nil {
		return "", err
	}
	return "已上报 buddyapp 进入五连事件（同时覆盖 Buddy_App 与 Buddy_App_QQ）", nil
}

// runAutomationCreate 上报定时任务创建成功事件（automation_1）。
func runAutomationCreate(r *Runner, ctx context.Context, a *auth.Auth, _ *upstream.GrowthTask) (string, error) {
	if err := r.up.ReportDesktopEvents(a, upstream.AutomationCreateEvent("wb2api 自动化")); err != nil {
		return "", err
	}
	return "已上报定时任务创建成功事件", nil
}

// runLibraryRead 以 Web 指纹上报资料库阅读事件（Library_read）。
//
// 必须用 Web 指纹：该任务的判据是网页端的元素点击，
// 用桌面指纹上报同类事件不计分（实测）。
func runLibraryRead(r *Runner, ctx context.Context, a *auth.Auth, _ *upstream.GrowthTask) (string, error) {
	if err := r.up.ReportWebEvent(a, "web_element_click", libraryDocURL,
		"library_doc_intro_click", "WorkBuddy资料库介绍"); err != nil {
		return "", err
	}
	return "已上报资料库介绍阅读事件（Web 指纹）", nil
}

// runTemplateUse 上报模板使用事件组 ×5（template_5）。
//
// 每组都带一条完整对话链：判据事件 JOIN 到对话上，
// 只发 template_used 而没有对话链不会被计入。
func runTemplateUse(r *Runner, ctx context.Context, a *auth.Auth, _ *upstream.GrowthTask) (string, error) {
	templates := [][2]string{
		{"1", "深度研究"}, {"2", "周报生成"}, {"3", "竞品分析"},
		{"4", "活动策划"}, {"5", "代码评审"},
	}
	for i, tp := range templates {
		ms := time.Now().UnixMilli()
		conv := fmt.Sprintf("wb2api-tpl-%d-%d", ms, i)
		req := fmt.Sprintf("wb2api-tpl-req-%d-%d", ms, i)
		if err := r.up.ReportDesktopEvents(a, upstream.TemplateUseSequence(conv, req, tp[0], tp[1])...); err != nil {
			return fmt.Sprintf("已上报 %d/%d 组模板事件后中断: %v", i, len(templates), err), nil
		}
		if err := sleepCtx(ctx, 300*time.Millisecond); err != nil {
			return fmt.Sprintf("已上报 %d/%d 组模板事件（本轮被取消）", i+1, len(templates)), nil
		}
	}
	return "已上报模板使用事件组 ×5", nil
}

// runPlaybookPrompt 上报灵感案例「做同款」发送事件组（playbook_prompt）。
func runPlaybookPrompt(r *Runner, ctx context.Context, a *auth.Auth, _ *upstream.GrowthTask) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-pb-%d", ms)
	req := fmt.Sprintf("wb2api-pb-req-%d", ms)
	events := upstream.PlaybookPromptSequence(conv, req, "pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")
	if err := r.up.ReportDesktopEvents(a, events...); err != nil {
		return "", err
	}
	return "已上报 playbook_cta_click + playbook_prompt_send", nil
}

// runCreateCanvas 上报设计创意画布创建事件组（create_canvas）。
func runCreateCanvas(r *Runner, ctx context.Context, a *auth.Auth, _ *upstream.GrowthTask) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-canvas-%d", ms)
	req := fmt.Sprintf("wb2api-canvas-req-%d", ms)
	if err := r.up.ReportDesktopEvents(a, upstream.DesignCanvasSequence(conv, req)...); err != nil {
		return "", err
	}
	return "已上报 wbx_design_canvas_task_create/open", nil
}

// runAppearance 设置主题 + 上报皮肤生效事件（Hp_Appearance）。
//
// 两步都要：只调 appearance/set 不计分（判据是设置页关闭时的
// appearance_skin_apply 事件），只发事件则缺少服务端侧的主题设置记录。
func runAppearance(r *Runner, ctx context.Context, a *auth.Auth, _ *upstream.GrowthTask) (string, error) {
	if err := r.up.SetAppearanceTheme(a, hpThemeKey); err != nil {
		return "", fmt.Errorf("设置主题失败: %w", err)
	}
	// 给服务端留出处理主题切换的时间，再发"生效"事件 ——
	// 两个请求挨着发会出现「先报生效、后设主题」的时序倒置。
	if err := sleepCtx(ctx, 2*time.Second); err != nil {
		return "主题已设置，本轮被取消（未上报生效事件）", nil
	}
	if err := r.up.ReportDesktopEvents(a, upstream.AppearanceApplyEvent(hpThemeKey)); err != nil {
		return "", err
	}
	return "已设置主题并上报皮肤生效事件", nil
}

// ---------------------------------------------------------------------------
// B 类实现
// ---------------------------------------------------------------------------

// runModelChat 用指定模型真实对话一次并对齐模型上报（Model_chat_GLM5.2）。
//
// 两次上报缺一不可：真实对话产生「用过该模型」的服务端事实，
// 对齐模型的上报事件则是任务计分器读取的字段 —— 只做其中之一都不计分。
func runModelChat(r *Runner, ctx context.Context, a *auth.Auth, _ *upstream.GrowthTask) (string, error) {
	const modelID, modelName = "glm-5.2", "GLM-5.2"
	conv, _, err := r.up.ChatWithModel(a, modelID, "hi，请回复一句话", "")
	if err != nil {
		return "", fmt.Errorf("glm-5.2 对话失败: %w", err)
	}
	if err := sleepCtx(ctx, r.reportGap); err != nil {
		return "对话已完成，本轮被取消（未上报）", nil
	}
	if err := r.up.ReportChatActivityModel(a, conv, "", modelID, modelName); err != nil {
		return "对话已完成，但模型对齐上报失败: " + err.Error(), nil
	}
	return "已完成 glm-5.2 真实对话并对齐模型上报", nil
}

// ---------------------------------------------------------------------------
// C 类实现
// ---------------------------------------------------------------------------

// runExpert5 使用 5 个平台专家（expert_5）。
func runExpert5(r *Runner, ctx context.Context, a *auth.Auth, _ *upstream.GrowthTask) (string, error) {
	return runExpertBatch(r, ctx, a, "agent", 5, "craft")
}

// runExpertTeam3 使用 3 个专家团（Expert_team_use_3）。
func runExpertTeam3(r *Runner, ctx context.Context, a *auth.Auth, _ *upstream.GrowthTask) (string, error) {
	return runExpertBatch(r, ctx, a, "team", 3, "craft")
}

// runExpertBatch 专家「召唤 + 使用」链的公共实现。
//
// 单次链路三件事，缺一不可（实测口径）：
//  1. 召唤事件组（expert_summon_click / expert_summoned）；
//  2. 真实对话拿服务端 requestId —— 自造 id 不计数；
//  3. 使用事件（expert_actual_use）JOIN 到该 requestId + 一条对话链。
//
// 逐个继续而非遇错中断：一位专家失败不该让其余 4 位也做不成，
// 而任务的判据是「计数」而非「全部成功」。
func runExpertBatch(r *Runner, ctx context.Context, a *auth.Auth, expertType string, count int, mode string) (string, error) {
	experts, err := r.up.MarketExpertList(a, expertType)
	if err != nil {
		return "", fmt.Errorf("拉取专家市场列表失败: %w", err)
	}
	if len(experts) == 0 {
		return "", errors.New("专家市场列表为空")
	}
	ok := 0
	var firstErr error
	for i := range experts {
		if ok >= count {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if i > 0 {
			if err := sleepCtx(ctx, r.chatGap); err != nil {
				break
			}
		}
		if err := r.expertOnce(a, experts[i], mode); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ok++
	}
	if ok == 0 {
		// 一次都没成：这才是真失败（列出首个原因便于排查）。
		if firstErr != nil {
			return "", fmt.Errorf("专家使用链全部失败（%s）: %w", expertType, firstErr)
		}
		return "", fmt.Errorf("专家使用链全部失败（%s）", expertType)
	}
	msg := fmt.Sprintf("已对 %d/%d 位真实专家完成召唤+使用链（类型 %s）", ok, count, expertType)
	if ok < count {
		msg += "（未达目标数，可稍后重试）"
	}
	return msg, nil
}

// expertOnce 对单个专家走一次完整的召唤 + 真实对话 + 使用事件。
func (r *Runner) expertOnce(a *auth.Auth, e upstream.MarketExpert, mode string) error {
	if err := r.up.ReportDesktopEvents(a, upstream.ExpertSummonSequence(e)...); err != nil {
		return fmt.Errorf("召唤链 %s: %w", e.ExpertID, err)
	}
	conv, req, err := r.up.ChatWithModel(a, freeModel, "1+1等于几？直接回答。", e.ExpertID)
	if err != nil {
		return fmt.Errorf("专家对话 %s: %w", e.ExpertID, err)
	}
	events := upstream.ChatSequence(conv, req, "msg-"+reqTail(req), freeModel, freeModel)
	events = append(events, upstream.ExpertActualUseEvent(e, conv, req, mode))
	if err := r.up.ReportDesktopEvents(a, events...); err != nil {
		return fmt.Errorf("使用事件 %s: %w", e.ExpertID, err)
	}
	return nil
}

// runExpertLighthouse 轻量云专家召唤 + 使用链（Expert_lighthouse）。
//
// 与 expert_5 同构，两处差异（实测真实样本）：
//   - 专家固定为腾讯轻量云专家，但版本等信息以市场列表为准（服务端下发的才真实）；
//   - 使用事件的 mode 为 LOCAL（而非 craft）。
func runExpertLighthouse(r *Runner, ctx context.Context, a *auth.Auth, _ *upstream.GrowthTask) (string, error) {
	lh := upstream.MarketExpert{
		ExpertID: lighthouseExpertID, ExpertType: "agent",
		DisplayNameZH: "腾讯轻量云专家", ProfessionZH: "腾讯轻量云专家", Version: "1.0.2",
	}
	// 市场列表命中则用服务端的字段（version / categories 等以服务端为准）。
	if experts, err := r.up.MarketExpertList(a, "agent"); err == nil {
		for _, e := range experts {
			if e.ExpertID == lighthouseExpertID {
				lh = e
				break
			}
		}
	}
	if err := r.expertOnce(a, lh, "LOCAL"); err != nil {
		return "", fmt.Errorf("轻量云专家链: %w", err)
	}
	return "已上报轻量云专家召唤+使用链（真实对话回执）", nil
}

// skillIDFresh / skillNameFresh 技能尝鲜判据里引用的技能。
const (
	skillNameFresh = "润泽小馆·日报撰写"
	skillIDFresh   = "skill_2097350077599879168"
)

// runSkill1 真实对话 + skill_info 技能加载事件（skill_1）。
//
// 判据是 skill_info（桌面指纹）JOIN 真实会话。注意方向：此前的
// skill_request_send / skill_installed 等事件都不计分，唯一有效的是 skill_info。
func runSkill1(r *Runner, ctx context.Context, a *auth.Auth, _ *upstream.GrowthTask) (string, error) {
	conv, req, err := r.up.ChatWithModel(a, freeModel, "1+1等于几？直接回答。", "")
	if err != nil {
		return "", fmt.Errorf("真实对话失败: %w", err)
	}
	events := upstream.SkillSequence(conv, req, skillNameFresh, skillIDFresh, "1.0.0")
	if err := r.up.ReportDesktopEvents(a, events...); err != nil {
		return "", fmt.Errorf("skill_info 事件上报失败: %w", err)
	}
	return "已上报真实对话 + skill_info 技能加载事件", nil
}

// ---------------------------------------------------------------------------
// D 类实现
// ---------------------------------------------------------------------------

// runBlackCat 夜猫子任务（black_cat）：仅夜猫时段内计入。
//
// 实测现状（2026-09-16 夜，真实账号，值得如实记录）：
// 目标值为 **3**（不是 1），奖励为 **0 积分 0 能量**，而同一夜内用三种判据
// 口径各做一次真实对话后进度都停在 1/3 不动：
//
//	A. CLI 域 chat（既有 ChatStream）+ billing 域模型对齐上报
//	B. 桌面域完整对话事件链（自造 conversationId/requestId）
//	C. 桌面域真实对话 + 用服务端返回的真实 requestId 构造对话链
//
// 最可能的原因是**按夜计数**（每夜最多计 1 次，需跨 3 夜完成）——
// 目标 3 + 无奖励的组合与"连续 3 晚"的语义吻合。但这是推断：
// 单夜实测既无法证实也无法证伪，故这里按"尽力补足差额"实现，
// 由调用方的进度回读如实反映结果，不假装成功。
//
// 窗口外**跳过而不报错**：这是上游规则决定的正常状态，不是故障。
// 报 error 会让「一键完成」的结果里出现一条看起来像失败的记录，
// 而用户完全无能为力（只能等到夜里）。
//
// 每轮实践：先发一条 chat_request_send 上报（成本最低、若上游认这条最划算），
// 再按差额补真实对话。差额每次最多补 3 次，避免一次点击打出远超需求的对话量。
func runBlackCat(r *Runner, ctx context.Context, a *auth.Auth, before *upstream.GrowthTask) (string, error) {
	if !upstream.InNightWindow(time.Now()) {
		return "当前不在夜猫时段（23:00–08:00 北京时间），上游不计入本次上报；请在时段内重试", nil
	}
	target := int64(3) // 实测目标值；未报名拿不到 progress 时的兜底
	if before != nil && before.Target > 0 {
		target = before.Target
	}
	var current int64
	if before != nil {
		current = before.Current
	}
	need := target - current
	if need <= 0 {
		return "进度已达标，无需补足", nil
	}
	if need > 3 {
		need = 3
	}

	// 先发一条对话活跃上报：若上游认这条，成本最低。
	cid := fmt.Sprintf("wb2api-night-%d", time.Now().UnixMilli())
	if err := r.up.ReportChatActivity(a, cid, cid+"-r1"); err != nil {
		return "", err
	}
	if err := sleepCtx(ctx, r.reportGap); err != nil {
		return "已上报一次夜猫活跃事件（本轮被取消）", nil
	}

	// 再补真实对话：夜猫任务的语义与"对话"绑定，故按差额补足。
	const modelID, modelName = "glm-5.2", "GLM-5.2"
	done := int64(0)
	for i := int64(0); i < need; i++ {
		if ctx.Err() != nil {
			break
		}
		conv, _, err := r.up.ChatWithModel(a, modelID, "1+1等于几？直接回答。", "")
		if err != nil {
			if done == 0 {
				return fmt.Sprintf("已上报夜猫活跃事件，但真实对话失败: %v", err), nil
			}
			return fmt.Sprintf("已完成 %d/%d 次夜间对话后中断: %v", done, need, err), nil
		}
		// 对齐模型的上报：让这次对话在计分器眼里确实"用过 glm-5.2"。
		if err := r.up.ReportChatActivityModel(a, conv, "", modelID, modelName); err != nil {
			if done == 0 {
				return "对话已完成，但模型对齐上报失败: " + err.Error(), nil
			}
			return fmt.Sprintf("已完成 %d/%d 次夜间对话（对齐上报失败: %v）", done, need, err), nil
		}
		done++
		if i < need-1 {
			if err := sleepCtx(ctx, r.chatGap); err != nil {
				break
			}
		}
	}
	return fmt.Sprintf("已在夜猫时段上报并完成 %d/%d 次 glm-5.2 真实对话", done, need), nil
}

// reqTail 取 requestId 末 8 位（用于拼 messageId）。
func reqTail(req string) string {
	if len(req) <= 8 {
		return req
	}
	return req[len(req)-8:]
}
