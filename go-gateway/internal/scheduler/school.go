// school.go 开学季活动任务：拉任务清单 → 领取已完成任务的奖励。
//
// 设计边界（重要）：本任务**只领取已经达标的任务**，不伪造完成动作。
//
// 活动任务分两类：
//   - 可被正常使用行为点亮的：chat_3_times（对话 3 次）、
//     desktop_chat_1_time（桌面对话）—— 这些由活跃上报任务自然覆盖
//   - 需要真实动作的：task_student_verify（学生认证）、share_invite（邀请）、
//     expert_use（专家功能）—— 本任务**不伪造**这些动作
//
// 理由：伪造学生认证 / 邀请属于刷量，既违反活动规则也可能导致账号被封。
// 因此这里只做「把已达标的奖励领回来」这件安全的事。
//
// 活动是**限时**的：服务端下发 in_period；下线后 tasks 为空、in_period=false，
// 此时静默跳过（不报错、不重试）—— 活动结束后代码自然失效，无需人工清理。
package scheduler

import (
	"context"
	"fmt"
	"log"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/records"
	"workbuddy2api/internal/upstream"
)

// schoolClaimGap 账号之间的间隔，摊平请求。
const schoolClaimGap = 400 * time.Millisecond

// RunSchoolNow 立即执行一轮开学季活动任务（供 CLI 一次性触发、测试）。
func (s *Scheduler) RunSchoolNow() {
	s.runSchool(context.Background())
}

// runSchool 遍历账号：拉活动任务清单，领取已完成任务的奖励。
//
// 单账号失败只记日志、不影响其余账号 —— 活动任务是尽力而为的日常任务。
func (s *Scheduler) runSchool(ctx context.Context) {
	first := true
	// anyInPeriod 与 anyAnswered 必须分开记，否则两种情况会被混为一谈：
	//   - 「问了上游，回答是不在期」→ 结论成立，值得写一条汇总记录；
	//   - 「一个账号都没问到」（全被禁用/区域不符/拉清单全失败）→ 我们对活动
	//     状态一无所知，此时写「活动不在期」是**编造结论**，会把用户引偏。
	anyInPeriod := false
	anyAnswered := false

	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		// 区域过滤：实测该活动只有国服有（国际版返回 404 Route Not Found），
		// 与签到共用同一开关（schedule.checkin_scope）。
		if !s.checkinScopeAllows(a) {
			continue
		}
		if !first {
			if !sleepCtx(ctx, schoolClaimGap) {
				return
			}
		}
		first = false

		inPeriod, tasks, err := s.cfg.Upstream.SchoolTasks(a)
		if err != nil {
			log.Printf("school %s: fetch tasks failed: %v", uid8(a.UID), err)
			// 失败按天去重：活动任务每天只跑一次，失败留一条即可。
			s.cfg.Records.TaskDaily(a.UID, "开学季活动", records.ResultFailed,
				"拉取活动任务清单失败: "+err.Error())
			continue
		}
		anyAnswered = true
		if !inPeriod {
			// 活动已下线：正常状态，不是错误。后续账号也不用再试。
			log.Printf("school %s: activity not in period, skip", uid8(a.UID))
			continue
		}
		anyInPeriod = true

		claimed, claimErrs := s.claimFinishedTasks(a, tasks)
		if claimed > 0 {
			log.Printf("school %s: claimed %d task(s)", uid8(a.UID), claimed)
			// 只在**真的领到奖励**时写成功记录 —— 那才是「有新变化」。
			// 每天为「活动在期但无奖励可领」写一条只会把记录刷成噪音。
			s.cfg.Records.Task(a.UID, "开学季活动", records.ResultSuccess,
				fmt.Sprintf("领取 %d 个已达标任务奖励", claimed))
		}
		// 领取失败必须留痕（按天去重）：这是用户排查「奖励为什么没到账」的线索。
		for _, e := range claimErrs {
			s.cfg.Records.TaskDaily(a.UID, "开学季活动", records.ResultFailed, e)
		}
	}

	// 汇总只在「确实问过上游、且都回答不在期」时写：
	// 没问到任何账号就断言活动下线，会让用户拿着错误结论去排查。
	if anyAnswered && !anyInPeriod {
		log.Printf("school: activity not in period for any account (probably ended)")
		// 整轮结论与具体账号无关，逐账号写会刷屏，故只写一条汇总。
		s.cfg.Records.TaskAllDaily("开学季活动", records.ResultInfo,
			"活动不在期（已下线或尚未开始），本次未做任何领取")
	}
}

// claimFinishedTasks 领取已达标的任务奖励，返回成功领取的数量与逐条失败原因。
//
// 只处理 status == "finished"（已达标待领取）的任务；
// pending（未达标）与 claimed（已领取）都跳过 —— 前者没资格、后者会报重复领取。
//
// 为什么把错误回传给调用方而不是就地记录：本函数只认「哪些失败了」，
// 而调用方（runSchool）才知道该按什么粒度写账号记录（按天去重 / 逐条）。
func (s *Scheduler) claimFinishedTasks(a *auth.Auth, tasks []upstream.SchoolTask) (int, []string) {
	n := 0
	var errs []string
	for _, t := range tasks {
		if t.Status != schoolStatusFinished {
			continue
		}
		if err := s.cfg.Upstream.SchoolClaim(a, t.Code); err != nil {
			log.Printf("school %s: claim %s failed: %v", uid8(a.UID), t.Code, err)
			// 带上任务 code：活动有多个任务，不写清楚用户不知道是哪个没领到。
			errs = append(errs, fmt.Sprintf("领取 %s 失败: %v", t.Code, err))
			continue
		}
		n++
		log.Printf("school %s: claim %s ok (+%d credits)", uid8(a.UID), t.Code, t.RewardCredit)
	}
	return n, errs
}

// schoolStatusFinished 任务已达标、待领取的状态值（实测）。
const schoolStatusFinished = "finished"
