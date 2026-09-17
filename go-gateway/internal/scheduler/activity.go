// activity.go 活跃上报任务：发送 chat_request_send 事件，点亮 growth 连登天数
// 并解锁 first_buddy 任务（领养前置）。
//
// 为什么需要它：签到只恢复**余额**，连登天数与领养资格靠对话活跃度点亮。
// 没有本任务时，纯挂机账号的连登不会增长、也无法领养猫猫。
//
// 风控口径：每号每天上报若干条即可（默认 3 条），不做多时点高频上报。
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

const (
	// defaultActivityReportCount 每号每日上报条数。
	//
	// 取 3 而非 1：单条上报偶发被服务端丢弃（见下），多条提高点亮成功率；
	// 也不宜过多，避免被风控当成异常流量。
	defaultActivityReportCount = 3

	// activityReportGap 同账号两条上报之间的间隔，避免秒发触发风控。
	activityReportGap = 800 * time.Millisecond

	// activityAccountDelay 账号之间的间隔，摊平请求。
	activityAccountDelay = 400 * time.Millisecond
)

// RunActivityNow 立即对所有账号做一轮活跃上报（供 CLI 一次性触发、测试）。
// 内部走 runActivity，取背景 ctx（不可取消）。
func (s *Scheduler) RunActivityNow() {
	s.runActivity(context.Background())
}

// runActivity 活跃上报遍历，随 ctx 取消立即退出。
//
// 单账号失败只记日志、不影响其余账号：上报是尽力而为的日常任务，
// 一个号失败不该拖垮整轮。
func (s *Scheduler) runActivity(ctx context.Context) {
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		// 区域过滤：国际版的 growth 接口暂无真实数据，默认只跑国服。
		// （与签到共用 checkinScopeAllows，语义一致：schedule.checkin_scope=all 时全跑。）
		if !s.checkinScopeAllows(a) {
			continue
		}
		if !first {
			if !sleepCtx(ctx, activityAccountDelay) {
				return // 优雅停机：剩余账号下轮再报
			}
		}
		first = false

		// 多条共用同一 conversationId（同会话），requestId 各自独立（每条一个）。
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		count := s.cfg.ActivityReportCount
		ok := 0
		var lastErr error
		for i := 1; i <= count; i++ {
			rid := fmt.Sprintf("%s-r%d", cid, i)
			if err := s.cfg.Upstream.ReportChatActivity(a, cid, rid); err != nil {
				log.Printf("activity %s: report %d/%d failed: %v", uid8(a.UID), i, count, err)
				lastErr = err
				break // 本号上报失败：不再续发，streak 自检无意义
			}
			ok++
			if i < count {
				if !sleepCtx(ctx, activityReportGap) {
					return // 取消时立即放弃本号剩余条数
				}
			}
		}
		if ok < count {
			// 失败必须留痕：这条记录是用户排查「连登为什么没涨」的唯一线索
			//（宿主的网关子进程丢弃了 stdout，日志到不了界面）。
			// 用 Daily 是因为排程按整点触发、机器休眠后还会补跑，
			// 不去重会把同一天的记录刷成一片重复行。
			s.cfg.Records.TaskDaily(a.UID, "活跃上报", records.ResultFailed,
				fmt.Sprintf("第 %d/%d 条上报失败: %v", ok+1, count, lastErr))
			continue // 未发满：streak 自检与领养重试均无意义
		}
		log.Printf("activity %s: reported %d/%d ok", uid8(a.UID), ok, count)
		// 成功上报确实点亮了连登，属于「有新变化」，每次都要留痕。
		s.cfg.Records.Task(a.UID, "活跃上报", records.ResultSuccess,
			fmt.Sprintf("已上报 %d/%d 条（点亮连登天数）", ok, count))
		// 回读 streak 自检，并**把这份快照交给活跃地图闭环复用**：
		// 连登天数、补签卡余额、各档兑换状态本来就在同一个响应体里
		//（data.streak / data.makeup_cards / data.redemption_status），
		// 闭环再单独请求一次等于对同一端点打两次上游。
		streak, _ := s.readStreakState(a)
		s.travelAdoptForce(a) // 对话量刚补满 → 立即重试领养（豁免当日防抖）
		// 活跃地图闭环（礼包/补偿/补签/兑换/抽奖）紧跟在上报之后：
		// 它的每一步都以「刚被点亮的连登天数」为前提，分开排程会顺序倒挂。
		s.growthMapRound(a, streak, false)
	}
}

// checkActivityStreak 上报成功后回读连登天数，用于发现**静默失败**。
//
// 背景：上报返回 200 不等于真的计分 —— 事件缺 userId 时服务端 200 但直接丢弃，
// 连登天数不动。回读是唯一能发现这种情况的手段。
//
// 判定口径：
//   - 回读失败 → 告警，但不重试（上报本身已成功，且按天幂等）
//   - days == 0 → 告警「上报成功但连登仍为 0」
//   - 正常 → 打一行日志便于对账
//
// 实测标定（2026-09-15，19 个真实账号）：上报前多个账号 days=0，
// 上报 1 条后同一批账号全部变为 days=1 —— 说明本接口确实点亮连登，
// 且 days=0 更多是「今天还没上报」而非静默丢弃。
// 保留 days=0 告警是因为**已上报仍为 0** 才是异常，而此处正好在上报之后调用。
//
// 返回 true 表示「上报 OK 但 streak 可疑」，供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	_, suspect := s.readStreakState(a)
	return suspect
}

// readStreakState 读一次连登快照，同时完成「自检」与「供活跃地图复用」。
//
// 为什么合并成一个调用：streak 响应体里同时装着连登天数（自检要看）、
// 补签卡余额与各档兑换状态（活跃地图要看）。分成两次读就是对同一端点打两次上游 ——
// 既有被风控多算一次请求的代价，也让同一份数据出现两个可能不一致的快照。
//
// 返回值 suspect 的语义与 checkActivityStreak 一致（true = 上报成功但天数可疑）。
// 读取失败时 state 为 nil、suspect 为 true：调用方据此跳过依赖天数的步骤。
func (s *Scheduler) readStreakState(a *auth.Auth) (*upstream.GrowthStreakState, bool) {
	st, err := s.cfg.Upstream.GrowthStreakState(a)
	if err != nil {
		log.Printf("WARN: activity %s: streak check failed (report ok): %v", uid8(a.UID), err)
		return nil, true
	}
	if st.Days() == 0 {
		log.Printf("WARN: activity %s: report ok but streak.days=0 (silent drop?)", uid8(a.UID))
		return st, true
	}
	log.Printf("activity %s: streak days=%d", uid8(a.UID), st.Days())
	return st, false
}

// sleepCtx 睡满 d；ctx 取消时立即返回 false（优雅停机不等满间隔）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// uid8 日志用短 uid（前 8 字符）；不足则原样返回。
//
// 刻意不打印完整 uid：日志会被贴到 issue/PR，完整 uid 属于账号标识。
func uid8(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
