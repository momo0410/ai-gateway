// trial.go 国际版 trial 加油包领取任务。
//
// 为什么需要它：国际版**没有签到、没有任务中心**（那些都是国服独有的活动），
// trial 加油包是国际版唯一天然的积分增益动作。
// 对从未被发放额度的国际版账号（实测存在 TotalCount=0 / Packages=[] 的账号）尤其重要。
//
// 为什么可以每天重试：上游用业务码 14051 表达「已领取过」，
// 客户端把它判定为**幂等成功**而非错误（见 upstream.ClaimTrial）。
// 因此无需本地记账、无需「只跑一次」的特殊逻辑 —— 每天试一次，
// 已领过的账号静默通过，新账号自动领到。
package scheduler

import (
	"context"
	"log"
	"time"

	"workbuddy2api/internal/records"
	"workbuddy2api/internal/upstream"
)

// trialGap 账号之间的间隔，摊平请求。
const trialGap = 400 * time.Millisecond

// RunTrialNow 立即执行一轮 trial 领取（供 CLI 一次性触发、测试）。
func (s *Scheduler) RunTrialNow() {
	s.runTrial(context.Background())
}

// runTrial 遍历**国际版**账号领取 trial 加油包。
//
// 区域过滤与其它任务相反：这里只要国际版（国服无此端点）。
// 不复用 checkinScopeAllows —— 那个是「默认只跑国服」，语义正好相反。
func (s *Scheduler) runTrial(ctx context.Context) {
	first := true
	newly, already := 0, 0

	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		if !upstream.IsIntl(a) {
			continue // 国服无此端点
		}
		if !first {
			if !sleepCtx(ctx, trialGap) {
				return
			}
		}
		first = false

		claimed, err := s.cfg.Upstream.ClaimTrial(a)
		switch {
		case err != nil:
			log.Printf("trial %s: %v", uid8(a.UID), err)
			// 失败按天去重：trial 每天最多跑两次，同一失败不必重复出现。
			s.cfg.Records.TaskDaily(a.UID, "trial 加油包", records.ResultFailed, err.Error())
		case claimed:
			newly++
			log.Printf("trial %s: claimed new trial pack", uid8(a.UID))
			// 只有**真的领到新包**才写：这是国际版唯一的积分增益动作，
			// 属于用户最想知道的「有新变化」。
			s.cfg.Records.Task(a.UID, "trial 加油包", records.ResultSuccess, "领取到新的 trial 加油包")
		default:
			already++
			// 「已领过」是幂等成功而非失败，但它同样是**没有变化**，
			// 每天写一条只会刷屏 —— 按天去重后保留一条可查即可。
			s.cfg.Records.TaskDaily(a.UID, "trial 加油包", records.ResultAlready, "本周期已领取过")
		}
	}

	// 只在有新领取时打汇总，避免每天刷一行「全部已领过」的噪音日志。
	if newly > 0 {
		log.Printf("trial: claimed %d new pack(s), %d already claimed", newly, already)
	}
}
