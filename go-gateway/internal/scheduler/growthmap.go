// growthmap.go 活跃地图任务：连登管家闭环（礼包/补偿 → 补签保连登 → 档位兑换 → 抽奖）。
//
// 为什么需要它：活跃上报只负责**点亮**连登天数，但天数点亮之后还有三步收益没人做：
//  1. 断签会把连登打回原点（7/14/28 档全部重攒），而补签卡躺在账户里没人用；
//  2. 达到 7/14/28 天的里程碑奖励是**兑换制**，不会自动到账；
//  3. 兑换送的抽奖次数也不会自动抽。
//
// 三者都是「不做就白白过期」的收益，因此合并成一个每日一轮的闭环。
//
// 为什么并入活跃上报而不是独立排程：这四步全部以「连登天数」为前提，
// 而天数由活跃上报点亮。分成两个时点会出现「先兑换后点亮」的顺序倒挂
// （当天点亮的天数要等到明天才能用来兑换）。因此挂在活跃上报成功之后执行，
// 复用已有排程与区域过滤，不新增时点、不新增开关。
//
// 幂等/静默口径（与 travel / school / trial 一致）：
//   - 按天幂等：每号每天最多一轮（growthClaimedToday 闸；CST 自然日重置，
//     进程重启清零 —— 重启后当日重复写由上游业务错误兜底）。
//   - 业务拒绝是常态：礼包早已领过、补偿活动未开启、无补签卡、天数不足、无抽奖次数。
//     这些一律静默，不刷 WARN —— 老账号每天都命中，属噪音。
//   - 只有真正的传输层失败（无 HTTP 状态）才值得记错误。
package scheduler

import (
	"log"
	"strconv"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/records"
	"workbuddy2api/internal/upstream"
)

// growthAccountDelay 账号之间的间隔，摊平请求（一轮每号最多 5 个上游请求）。
//
// 比活跃上报的账号间隔大：本任务紧跟在活跃上报（每号 3 条 + streak 回读）之后，
// 若再以同样密度连打，同一账号会在短时间内产生一串请求，正是刻意要避开的画像。
var growthAccountDelay = 500 * time.Millisecond

// growthTaskTitle 账号记录里的任务标题（界面「账号记录 → 任务」按此展示）。
const growthTaskTitle = "活跃地图"

// maxGrowthLotteryDraws 每轮每号最多抽几次。
//
// 上限存在的意义：chances 由兑换累积而来，理论上有界（每月三档各送 1），
// 但一旦上游行为变化或本地计数出错，无上限的循环会把次数在一轮里烧光。
// 取 3 = 单月三档全部兑换能拿到的次数，正常路径永远碰不到这个上限。
const maxGrowthLotteryDraws = 3

// RunGrowthMapNow 立即对池内所有可用账号执行一轮活跃地图闭环（供手动触发、测试）。
//
// 豁免「当日已跑」闸：这是用户显式点击的动作，与其它 Run*Now 一致
// （它们同样不检查 enabled 开关 —— 开关管的是后台自动排程）。
// 重复触发是安全的：上游对兑换/补签的重复请求回业务错误，
// 而抽奖本就以「有次数」为闸，不会被重复点击刷出额外消耗。
func (s *Scheduler) RunGrowthMapNow() {
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		// 区域过滤与活跃上报共用：本闭环的地基是 growth 连登，
		// 只有被点亮过连登的账号才有兑换/补签可言。
		if !s.checkinScopeAllows(a) {
			continue
		}
		if !first {
			time.Sleep(growthAccountDelay)
		}
		first = false
		s.growthMapRound(a, nil, true)
	}
}

// growthMapOne 日排程入口：每个账号每日至多一轮（读自己的连登快照）。
func (s *Scheduler) growthMapOne(a *auth.Auth) { s.growthMapRound(a, nil, false) }

// growthMapRound 单账号一轮闭环。
//
// state 为调用方**已经读到的**连登快照（活跃上报那条路径会顺带复用它的自检请求）；
// 传 nil 表示由本函数自己去读。
// force=true 时忽略「当日已跑」闸（手动触发路径）。
//
// 顺序是刻意的：先领礼包/补偿（与连登无关，先落袋），
// 再补签（会改变 streak 天数），最后用**补签后**的天数兑换与抽奖 ——
// 反过来会让补签恢复的天数白等一天。
func (s *Scheduler) growthMapRound(a *auth.Auth, state *upstream.GrowthStreakState, force bool) {
	if !force && s.growthClaimedToday(a.UID) {
		return
	}
	// 无论后续是否成功都标记当日已处理：领取类各状态当日不再重试，
	// 避免同日多趟对上游重复写（次日 CST 自然日重置，且上游本身幂等兜底）。
	s.markGrowthClaimed(a.UID)

	s.claimGrowthBenefits(a)

	if state == nil {
		fresh, err := s.cfg.Upstream.GrowthStreakState(a)
		if err != nil {
			logGrowthErr("growth", a.UID, "streak-state", err)
			return
		}
		state = fresh
	}
	// 补签读的是热力格（判据）与卡余额（同一快照），补签成功后天数会变，
	// 因此必须重读一次再挑档，否则会拿补签前的旧天数判断达标。
	if s.makeUpYesterday(a, state) {
		if fresh, err := s.cfg.Upstream.GrowthStreakState(a); err == nil {
			state = fresh
		}
	}
	s.redeemGrowthTier(a, state)
	s.drawGrowthLottery(a)
}

// claimGrowthBenefits 新手礼包 + 活动补偿。
//
// 两者都是「有则领」的幂等写：礼包每号一次，补偿活动有则领、无则业务错误。
// 只在**真的到账**时写记录与日志 —— 已领过是每天都会发生的常态，不值得留痕。
//
// 失败分两类处理（这是本任务「静默」原则的准确边界）：
//   - 业务拒绝（已领过 / 活动未开启）：常态，完全静默，连日志都不打。
//   - 其他（401 登录态失效 / 404 端点没了 / 429 限流 / 5xx / 传输层）：
//     真出问题了，必须记 —— 否则「token 死了」这类故障会被永久静默。
func (s *Scheduler) claimGrowthBenefits(a *auth.Auth) {
	if credit, err := s.cfg.Upstream.ClaimGift(a); err != nil {
		logGrowthErr("growth", a.UID, "claim-gift", err)
	} else if credit > 0 {
		log.Printf("growth %s: gift ok (+%d credit)", uid8(a.UID), credit)
		s.cfg.Records.Task(a.UID, growthTaskTitle, records.ResultSuccess,
			"领取新手礼包 +"+strconv.FormatInt(credit, 10)+" 积分")
	}
	if credit, err := s.cfg.Upstream.ClaimCompensation(a); err != nil {
		logGrowthErr("growth", a.UID, "claim-compensation", err)
	} else if credit > 0 {
		log.Printf("growth %s: compensation ok (+%d credit)", uid8(a.UID), credit)
		s.cfg.Records.Task(a.UID, growthTaskTitle, records.ResultSuccess,
			"领取活动补偿 +"+strconv.FormatInt(credit, 10)+" 积分")
	}
}

// makeUpYesterday 昨日漏签且有补签卡时补签，返回是否真的补上了。
//
// 判据链（顺序不可调换，每一步都在缩小写操作的范围）：
//
//	heatmap 昨日格存在且 score==0（确实漏签）
//	  → 「无该日格」直接放弃（无判据，不是漏签）
//	  → 「昨日不在本月」直接放弃（上游只允许补当月，请求必然被拒）
//	  → streak.makeup_cards.balance > 0（有卡可扣）
//	  → POST makeup-cards/use {target_date: 昨日}
//
// 为什么值得补：连登一断，7/14/28 三档全部重攒，而一张卡就能保住 —— 代价远小于收益。
// 查询失败一律静默返回 false：补签是纯增益动作，判据拿不到时宁可不做（每日都会重试，无写风险）。
func (s *Scheduler) makeUpYesterday(a *auth.Auth, state *upstream.GrowthStreakState) bool {
	// 月初判据先于网络请求：1 号的「昨日」必然在上个月，补签必被拒。
	// 放在最前面可以省掉每日一次的 heatmap 请求。
	if upstream.GrowthMakeupOutOfMonth(time.Now()) {
		return false
	}
	cells, err := s.cfg.Upstream.GrowthHeatmap(a)
	if err != nil {
		return false // 只读判据失败：静默（无写风险，明日再判）
	}
	yesterday := upstream.GrowthYesterday(time.Now())
	score, ok := upstream.HeatmapDayScore(cells, yesterday)
	if !ok {
		return false // 无该日格 = 无判据，不动（活跃地图是定长窗口，窗口外日子天然缺席）
	}
	if score != 0 {
		return false // 昨日有分：未漏签，无需补
	}
	if state.MakeupCards.Balance <= 0 {
		return false // 有漏签但无卡：静默（卡由兑换发放，下轮再看）
	}
	if err := s.cfg.Upstream.UseMakeupCard(a, yesterday); err != nil {
		if upstream.IsBusinessRejection(err) {
			// 无卡/无漏签/跨月：上游已给出明确答复，属常态，静默。
			return false
		}
		logGrowthErr("growth", a.UID, "makeup "+yesterday, err)
		return false
	}
	log.Printf("growth %s: makeup ok %s (streak kept)", uid8(a.UID), yesterday)
	s.cfg.Records.Task(a.UID, growthTaskTitle, records.ResultSuccess,
		"补签 "+yesterday+"（消耗 1 张补签卡，保住连登）")
	return true
}

// redeemGrowthTier 从高到低挑「已达标且本月未领」的最高档位兑换一次。
//
// 一次只领一档：档位是里程碑，同日把 7/14/28 一次领完会让当天产生多次写请求；
// 而档位不会过期（同月内有效），每天领一档在月内完全来得及。
func (s *Scheduler) redeemGrowthTier(a *auth.Auth, state *upstream.GrowthStreakState) {
	tier := growthEligibleTier(state.Days(), &state.Redemption)
	if tier == "" {
		return // 未达标或本月已领完：正常态，不打日志（很多天都到不了 7d）
	}
	res, err := s.cfg.Upstream.GrowthRedeem(a, tier, "")
	switch {
	case err == nil:
		log.Printf("growth %s: redeem tier=%s ok (+%d credit, +%d energy, +%d card, +%d chance)",
			uid8(a.UID), tier, res.CreditGranted, res.EnergyGranted, res.CardsGranted, res.ChancesGranted)
		s.cfg.Records.Task(a.UID, growthTaskTitle, records.ResultSuccess,
			"兑换连登 "+tier+" 奖励（+"+strconv.Itoa(res.CreditGranted)+" 积分）")
	case upstream.IsRedeemAlreadyClaimed(err), upstream.IsRedeemNotEnoughDays(err):
		// 本月已领 / 天数不足：上游的幂等答复，属常态，静默。
		return
	default:
		logGrowthErr("growth", a.UID, "redeem "+tier, err)
	}
}

// growthEligibleTier 按当前连登天数挑「尚未领取且已达标」的最高档位；"" = 无可领档。
//
// 达标判据用 tiers[].days 与 days 直接比较，**不用 remaining_days**：
// 实测该字段与 tiers 不自洽（同一账号 days=2 时报 remaining_days=3，
// 而 7d 档的 days=7，7-2=5），拿它当门槛会挑错档甚至跳过可领档位。
//
// 上游 tiers 数组实测按 days 升序，但这里按 Days 取最大值而不是取最后一个下标 ——
// 顺序变了也不会静默挑错。
func growthEligibleTier(days int, rs *upstream.GrowthRedemptionStatus) string {
	if rs == nil {
		return ""
	}
	best := ""
	bestDays := -1
	for _, sp := range rs.Tiers {
		if sp.Tier == "" || days < sp.Days {
			continue // 未达标
		}
		if rs.Claimed(sp.Tier) {
			continue // 本月已领
		}
		if sp.Days > bestDays {
			best, bestDays = sp.Tier, sp.Days
		}
	}
	return best
}

// drawGrowthLottery 抽奖：只在有次数时抽，每次用新的 client_token。
//
// 抽奖是**消耗性**的（一次一扣），因此必须先查到确切的余额再抽，
// 不能靠「抽到报错为止」试探 —— 那样会多打一次必然失败的写请求。
func (s *Scheduler) drawGrowthLottery(a *auth.Auth) {
	chances, err := s.cfg.Upstream.GrowthLotteryChances(a)
	if err != nil {
		logGrowthErr("growth", a.UID, "lottery-chances", err)
		return
	}
	if chances <= 0 {
		return // 无次数：绝大多数账号的常态，静默
	}
	if chances > maxGrowthLotteryDraws {
		chances = maxGrowthLotteryDraws
	}
	for i := 0; i < chances; i++ {
		res, err := s.cfg.Upstream.GrowthLotteryDraw(a, "") // 空 = 每次自动生成新 client_token
		switch {
		case err == nil:
			log.Printf("growth %s: lottery prize=%s type=%s (+%d credit)",
				uid8(a.UID), res.PrizeName, res.PrizeType, res.CreditAmount)
			detail := "抽奖获得 " + res.PrizeName
			if res.PrizeType == "" {
				detail = "抽奖获得奖品"
			}
			s.cfg.Records.Task(a.UID, growthTaskTitle, records.ResultSuccess, detail)
		case upstream.IsLotteryNoChance(err), upstream.IsLotteryDisabled(err):
			// 无次数/未开启：上游答复，属常态，静默停下（剩余次数不可能再成功）。
			return
		default:
			logGrowthErr("growth", a.UID, "lottery-draw", err)
			return
		}
	}
}

// growthClaimedToday 该账号当日是否已跑过一轮活跃地图闭环。
func (s *Scheduler) growthClaimedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.growthClaimed[uid] == travelDay(time.Now())
}

// markGrowthClaimed 记录该账号当日已跑过一轮。
func (s *Scheduler) markGrowthClaimed(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.growthClaimed[uid] = travelDay(time.Now())
}

// clearGrowthClaimed 清除该账号的当日标记（供测试模拟「次日」）。
func (s *Scheduler) clearGrowthClaimed(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.growthClaimed, uid)
}

// logGrowthErr 记录一次**真正值得关注**的失败（传输层 / 5xx）。
//
// 存在的意义是把「上游业务拒绝」与「真的坏了」在代码层面分开：
// 前者是每天都会发生的常态（已领过、无卡、天数不足），后者才需要人去查。
// 混在一起打日志会让真正的故障淹没在噪音里。
func logGrowthErr(prefix, uid, what string, err error) {
	if upstream.IsBusinessRejection(err) {
		// 兜底：调用方本应各自静默，这里防的是将来新增分支时漏判。
		return
	}
	log.Printf("WARN: %s %s: %s: %v", prefix, uid8(uid), what, err)
}
