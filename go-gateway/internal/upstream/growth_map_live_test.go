//go:build live

// live_test.go 活跃地图真实账号联调（**只在显式指定 live 构建标签时编译与运行**）。
//
// 为什么用构建标签而不是普通的 _test.go：
//   - 它会打真实上游并做真实写操作（补签会消耗补签卡、抽奖会消耗次数），
//     绝不能被 `go test ./...` 顺带跑到；
//   - 同时它又不是一次性的临时脚本 —— 上游改判据、改字段时，
//     需要能一条命令重跑同一套口径，把「怎么验的」固化下来。
//
// 运行方式（PowerShell）：
//
//	$env:WB_LIVE_ACCOUNTS = "D:\...\test-bin\map-probe\accounts.json"
//	$env:WB_LIVE_ACCOUNT  = "<uid 前 8 位>"   # 缺省用第一个国服账号；此处不写真实值
//	go test -tags live ./internal/upstream/ -run TestLiveGrowth -v
//
// 写操作分级（默认最保守，只读）：
//
//	（无额外变量）              只读：streak / heatmap / chances，打印真实形状
//	WB_LIVE_BENEFITS=1          领礼包 + 补偿：幂等写，每号一次，领到就是收益
//	WB_LIVE_MAKEUP=1            补签：**会消耗真实补签卡**，且仅在「昨日确实漏签且有卡」时才发请求
//	WB_LIVE_REDEEM=1            兑换：**会消耗本月档位名额**，仅在「已达标且未领」时才发请求
//	WB_LIVE_DRAW=1              抽奖：**会消耗抽奖次数**，仅在 chances>0 时才发请求
//
// 账号库**只读**：从 WB_LIVE_ACCOUNTS 读入后转成网关的凭证形态写入临时目录，
// 绝不回写源文件（源文件是所有者的生产账号库）。
package upstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// liveMapAccount 所有者账号库里的单条记录（snake_case 字段名，与网关凭证形态不同）。
type liveMapAccount struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	UID          string `json:"uid"`
	Domain       string `json:"domain"`
	ExpiresAt    int64  `json:"expiresAt"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// loadLiveMapAccount 读取真实账号并转成网关凭证形态（走生产同一套 auth.LoadDir 解析）。
func loadLiveMapAccount(t *testing.T, uid8 string) *auth.Auth {
	t.Helper()
	path := os.Getenv("WB_LIVE_ACCOUNTS")
	if path == "" {
		t.Skip("未设置 WB_LIVE_ACCOUNTS，跳过真实账号联调")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取账号库失败: %v", err)
	}
	var all []liveMapAccount
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatalf("解析账号库失败: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("账号库为空")
	}

	// 选号：指定 uid8 前缀优先，否则取第一个国服账号。
	// 默认不碰国际版 —— 国际版账号可能无 uid，且其成长域是否计入尚无定论。
	var pick *liveMapAccount
	for i := range all {
		a := &all[i]
		if uid8 != "" {
			if strings.HasPrefix(a.UID, uid8) {
				pick = a
				break
			}
			continue
		}
		if !strings.HasSuffix(strings.ToLower(a.Domain), ".ai") {
			pick = a
			break
		}
	}
	if pick == nil {
		t.Fatalf("账号库里找不到匹配 uid8=%q 的账号", uid8)
	}

	dir := t.TempDir()
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  pick.AccessToken,
			"refreshToken": pick.RefreshToken,
			"expiresAt":    pick.ExpiresAt,
			"domain":       pick.Domain,
		},
		"account": map[string]any{
			"uid":          pick.UID,
			"enterpriseId": pick.EnterpriseID,
			"nickname":     pick.Nickname,
		},
	}
	enc, _ := json.MarshalIndent(doc, "", "  ")
	file := filepath.Join(dir, "workbuddy-live.json")
	if err := os.WriteFile(file, enc, 0o600); err != nil {
		t.Fatalf("写入临时凭证失败: %v", err)
	}
	auths, err := auth.LoadDir(dir)
	if err != nil || len(auths) != 1 {
		t.Fatalf("加载临时凭证失败: %v (n=%d)", err, len(auths))
	}
	return auths[0]
}

// mapUID8 脱敏：只保留 uid 前 8 位。报告与日志里不出现完整 uid。
func mapUID8(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	if uid == "" {
		return "(nouid)"
	}
	return uid
}

// TestLiveGrowthReadOnly 只读探测：核对三个 GET 端点的真实形状与解析结果。
//
// 这一步的价值是**校验解析器对真实响应的适配**：形状认错了，
// 后面所有写操作的判据都会错，而单测里的假上游是我自己写的，认不出这一点。
func TestLiveGrowthReadOnly(t *testing.T) {
	a := loadLiveMapAccount(t, os.Getenv("WB_LIVE_ACCOUNT"))
	c := New()
	t.Logf("账号 uid8=%s realm=%s", mapUID8(a.UID), a.Realm())

	// --- streak：一次读三个切片 ---
	st, err := c.GrowthStreakState(a)
	if err != nil {
		t.Fatalf("GrowthStreakState: %v", err)
	}
	t.Logf("streak: days=%d month_total=%d 补签卡=%d/%d",
		st.Days(), st.Streak.MonthTotalDays, st.MakeupCards.Balance, st.MakeupCards.Max)
	t.Logf("redemption: 7d=%q 14d=%q 28d=%q remaining_days=%d",
		st.Redemption.Tier7dStatus, st.Redemption.Tier14dStatus, st.Redemption.Tier28dStatus,
		st.Redemption.RemainingDays)
	for _, sp := range st.Redemption.Tiers {
		t.Logf("  tier %s: days=%d credit=%d energy=%d cards=%d chances=%d",
			sp.Tier, sp.Days, sp.Credit, sp.Energy, sp.Cards, sp.Chances)
	}

	// 解析自检：三个切片必须至少有一个非零，否则说明字段层级认错了。
	// （全零在真实账号上不可能：launch_date 是 2026-06，任何账号都跑过若干天。）
	if st.Streak.Days == 0 && st.MakeupCards.Max == 0 && len(st.Redemption.Tiers) == 0 {
		t.Errorf("三个切片全为零 —— 字段层级很可能认错了（response 被解析成空结构）")
	}
	if st.MakeupCards.Max == 0 {
		t.Errorf("makeup_cards.max=0 可疑：实测恒为 4，说明 makeup_cards 段没解析到")
	}
	if len(st.Redemption.Tiers) != 3 {
		t.Errorf("tiers=%d 应为 3（7d/14d/28d）", len(st.Redemption.Tiers))
	}

	// --- heatmap：核对昨日格的判据 ---
	cells, err := c.GrowthHeatmap(a)
	if err != nil {
		t.Fatalf("GrowthHeatmap: %v", err)
	}
	yesterday := GrowthYesterday(time.Now())
	score, ok := HeatmapDayScore(cells, yesterday)
	t.Logf("heatmap: cells=%d 昨日(%s) score=%d hasCell=%v", len(cells), yesterday, score, ok)
	if len(cells) == 0 {
		t.Errorf("cells 为空 —— 真实账号实测应有 365 格")
	}
	if !ok {
		t.Logf("注意：昨日不在活跃地图窗口内（无判据），补签路径不会触发")
	}

	// --- lottery chances ---
	chances, err := c.GrowthLotteryChances(a)
	if err != nil {
		t.Fatalf("GrowthLotteryChances: %v", err)
	}
	t.Logf("lottery: chances=%d", chances)

	// 总结本条实测给出的判据结论（报告直接引用这里）。
	t.Logf("判据结论: 昨日漏签=%v 有补签卡=%v 可补签=%v",
		ok && score == 0, st.MakeupCards.Balance > 0, ok && score == 0 && st.MakeupCards.Balance > 0)
}

// TestLiveGrowthBenefits 领礼包 + 补偿（幂等写，每号一次）。
//
// 为什么这个可以放心跑：两者都是「有则领」的一次性收益。已领过时上游回业务错误，
// 不消耗任何资源；未领过则直接到账。无论哪种结果都是幂等的。
func TestLiveGrowthBenefits(t *testing.T) {
	if os.Getenv("WB_LIVE_BENEFITS") != "1" {
		t.Skip("未设置 WB_LIVE_BENEFITS=1，跳过礼包/补偿领取")
	}
	a := loadLiveMapAccount(t, os.Getenv("WB_LIVE_ACCOUNT"))
	c := New()
	t.Logf("账号 uid8=%s realm=%s", mapUID8(a.UID), a.Realm())

	if credit, err := c.ClaimGift(a); err != nil {
		if !IsBusinessRejection(err) {
			t.Errorf("礼包：传输层/服务端失败（值得关注）: %v", err)
		}
		t.Logf("礼包：业务拒绝（已领过，属常态，静默）status/err=%v", err)
	} else {
		t.Logf("礼包：领取成功 +%d credit", credit)
	}

	if credit, err := c.ClaimCompensation(a); err != nil {
		if !IsBusinessRejection(err) {
			t.Errorf("补偿：传输层/服务端失败（值得关注）: %v", err)
		}
		t.Logf("补偿：业务拒绝（活动未开启/已过期，属常态，静默）err=%v", err)
	} else {
		t.Logf("补偿：领取成功 +%d credit", credit)
	}
}

// TestLiveGrowthMakeup 补签（**消耗真实补签卡**）。
//
// 判据链与本项目 scheduler.makeUpYesterday 完全一致，且必须先确认
// 「昨日确实漏签」——绝不为了测试而乱补签。
func TestLiveGrowthMakeup(t *testing.T) {
	if os.Getenv("WB_LIVE_MAKEUP") != "1" {
		t.Skip("未设置 WB_LIVE_MAKEUP=1，跳过补签（会消耗真实补签卡）")
	}
	a := loadLiveMapAccount(t, os.Getenv("WB_LIVE_ACCOUNT"))
	c := New()

	// 第一步：只读确认真的是漏签，否则不发写请求。
	cells, err := c.GrowthHeatmap(a)
	if err != nil {
		t.Fatalf("heatmap: %v", err)
	}
	yesterday := GrowthYesterday(time.Now())
	score, ok := HeatmapDayScore(cells, yesterday)
	if !ok {
		t.Skipf("昨日 %s 无热力格 —— 无判据，按设计不动写接口", yesterday)
	}
	if score != 0 {
		t.Skipf("昨日 %s score=%d（未漏签）—— 按设计不补签", yesterday, score)
	}
	st, err := c.GrowthStreakState(a)
	if err != nil {
		t.Fatalf("streak: %v", err)
	}
	if st.MakeupCards.Balance <= 0 {
		t.Skipf("昨日漏签但补签卡余额=%d —— 按设计不补签", st.MakeupCards.Balance)
	}
	if GrowthMakeupOutOfMonth(time.Now()) {
		t.Skip("昨日已跨月，上游只允许补当月 —— 按设计不补签")
	}

	t.Logf("前置条件成立：昨日 %s 漏签 + 补签卡 %d 张，执行补签", yesterday, st.MakeupCards.Balance)
	if err := c.UseMakeupCard(a, yesterday); err != nil {
		t.Fatalf("补签失败: %v（业务拒绝=%v）", err, IsBusinessRejection(err))
	}
	t.Logf("补签成功：%s", yesterday)

	// 回读验证：连登天数应因补签而保持/增长。
	after, err := c.GrowthStreakState(a)
	if err == nil {
		t.Logf("补签后：days=%d 补签卡=%d", after.Days(), after.MakeupCards.Balance)
	}
}

// TestLiveGrowthRedeem 档位兑换（**消耗本月档位名额**）。
//
// 只在「已达标且未领」时才发请求 —— 与 scheduler.redeemGrowthTier 判据一致。
func TestLiveGrowthRedeem(t *testing.T) {
	if os.Getenv("WB_LIVE_REDEEM") != "1" {
		t.Skip("未设置 WB_LIVE_REDEEM=1，跳过兑换（会消耗本月档位名额）")
	}
	a := loadLiveMapAccount(t, os.Getenv("WB_LIVE_ACCOUNT"))
	c := New()

	st, err := c.GrowthStreakState(a)
	if err != nil {
		t.Fatalf("streak: %v", err)
	}
	// 用生产同一套挑档逻辑（包内可见）判定，避免联调脚本自己另写一套口径。
	tier := liveEligibleTier(st.Days(), &st.Redemption)
	if tier == "" {
		t.Skipf("无已达标未领档位（days=%d 7d=%q 14d=%q 28d=%q）—— 按设计不兑换",
			st.Days(), st.Redemption.Tier7dStatus, st.Redemption.Tier14dStatus, st.Redemption.Tier28dStatus)
	}
	t.Logf("前置条件成立：days=%d，兑换档位 %s", st.Days(), tier)

	res, err := c.GrowthRedeem(a, tier, "")
	if err != nil {
		if IsRedeemAlreadyClaimed(err) || IsRedeemNotEnoughDays(err) {
			t.Logf("兑换被业务拒绝（幂等/未达标，属常态）: %v", err)
			return
		}
		t.Fatalf("兑换失败: %v", err)
	}
	t.Logf("兑换成功 %s: +%d credit +%d energy +%d cards +%d chances",
		tier, res.CreditGranted, res.EnergyGranted, res.CardsGranted, res.ChancesGranted)
}

// TestLiveGrowthDraw 抽奖（**消耗抽奖次数**）。
//
// 只在 chances>0 时抽一次 —— 不靠「抽到报错为止」试探。
func TestLiveGrowthDraw(t *testing.T) {
	if os.Getenv("WB_LIVE_DRAW") != "1" {
		t.Skip("未设置 WB_LIVE_DRAW=1，跳过抽奖（会消耗抽奖次数）")
	}
	a := loadLiveMapAccount(t, os.Getenv("WB_LIVE_ACCOUNT"))
	c := New()

	chances, err := c.GrowthLotteryChances(a)
	if err != nil {
		t.Fatalf("chances: %v", err)
	}
	if chances <= 0 {
		t.Skipf("抽奖次数=%d —— 按设计不抽奖", chances)
	}
	t.Logf("前置条件成立：chances=%d，抽 1 次", chances)

	res, err := c.GrowthLotteryDraw(a, "")
	if err != nil {
		if IsLotteryNoChance(err) || IsLotteryDisabled(err) {
			t.Logf("抽奖被业务拒绝（无次数/未开启，属常态）: %v", err)
			return
		}
		t.Fatalf("抽奖失败: %v", err)
	}
	t.Logf("抽奖成功: prize=%s type=%s credit=%d", res.PrizeName, res.PrizeType, res.CreditAmount)
}

// TestLiveGrowthWritePlumbing 验证**写请求的形状被真实上游接受**（零消耗）。
//
// 为什么需要它：单测只能证明「请求体是我以为的样子」，证明不了上游认不认。
// 而真正消耗资源的写操作（补签/兑换/抽奖）不能在真实账号上随便跑。
//
// 办法是构造**必然被业务拒绝**的写请求：上游会先去到业务校验那一层，
// 于是我们能拿到业务错误而不是 404/401/参数错误：
//
//	redeem 未达标档位 → 403 连续登录天数不足  ⇒ 证明 tier + client_token 被接受
//	draw   无次数时   → 400 insufficient    ⇒ 证明 client_token 被接受
//	makeup 未漏签日   → 400 not broken      ⇒ 证明 target_date 被接受
//
// 这三条都不消耗任何东西（被拒绝的请求不扣卡、不占档位、不减次数），
// 却能钉死「路径 + 请求头 + 请求体」这一层的正确性 —— 这正是最容易出错、
// 也最难从单测里发现的一层（发错域名/漏请求头都会在这里现形）。
func TestLiveGrowthWritePlumbing(t *testing.T) {
	if os.Getenv("WB_LIVE_PLUMBING") != "1" {
		t.Skip("未设置 WB_LIVE_PLUMBING=1，跳过写请求形状验证（零消耗，但会发真实写请求）")
	}
	a := loadLiveMapAccount(t, os.Getenv("WB_LIVE_ACCOUNT"))
	c := New()
	t.Logf("账号 uid8=%s realm=%s", mapUID8(a.UID), a.Realm())

	st, err := c.GrowthStreakState(a)
	if err != nil {
		t.Fatalf("streak: %v", err)
	}

	// --- redeem：挑一个肯定未达标的档位（28d 门槛最高，days<28 时必被拒）---
	if st.Days() >= 28 {
		t.Logf("redeem：days=%d 已达标，跳过（避免真的领掉本月名额）", st.Days())
	} else {
		_, err := c.GrowthRedeem(a, "28d", "")
		if err == nil {
			t.Errorf("redeem：days=%d 本不该成功，却返回成功 —— 档位门槛判据可能已变", st.Days())
		} else if !IsBusinessRejection(err) {
			t.Errorf("redeem：写请求未被上游接受（非业务错误，说明路径/请求头/请求体有误）: %v", err)
		} else if IsRedeemNotEnoughDays(err) {
			t.Logf("redeem：形状被接受，上游按「天数不足」拒绝（零消耗）: %v", err)
		} else if IsRedeemAlreadyClaimed(err) {
			t.Logf("redeem：形状被接受，上游按「本月已领」拒绝（幂等，零消耗）: %v", err)
		} else {
			t.Logf("redeem：形状被接受，上游返回其他业务错误（零消耗）: %v", err)
		}
	}

	// --- draw：仅在 chances==0 时测（此时必然被拒且不消耗）---
	chances, err := c.GrowthLotteryChances(a)
	if err != nil {
		t.Fatalf("chances: %v", err)
	}
	if chances > 0 {
		t.Logf("draw：chances=%d > 0，跳过（避免真的消耗抽奖次数）", chances)
	} else {
		_, err := c.GrowthLotteryDraw(a, "")
		if err == nil {
			t.Errorf("draw：chances=0 本不该成功，却返回成功 —— 次数判据可能已变")
		} else if !IsBusinessRejection(err) {
			t.Errorf("draw：写请求未被上游接受（非业务错误）: %v", err)
		} else if IsLotteryNoChance(err) || IsLotteryDisabled(err) {
			t.Logf("draw：形状被接受，上游按「无次数/未开启」拒绝（零消耗）: %v", err)
		} else {
			t.Logf("draw：形状被接受，上游返回其他业务错误（零消耗）: %v", err)
		}
	}

	// --- makeup：挑一个**确定没有漏签**的日子（今日尚未结算，必然未被标记为漏签）---
	//
	// 用今日而不是昨日：昨日可能真的漏签（那就变成一次真实补签了）。
	// 今日的 score 即使为 0 也不算漏签（当天还没结束），上游会回
	// 「date is not broken」或「only current month makeup allowed」这类零消耗拒绝。
	//
	// 双重保险：**卡余额为 0 时才探** —— 即使上游对「今日」的判定与预期不符，
	// 没有卡也扣不走任何东西。这让本测试在任何账号上都是安全的。
	today := GrowthDay(time.Now())
	switch {
	case GrowthMakeupOutOfMonth(time.Now()):
		t.Logf("makeup：当前处于月初（昨日跨月），跳过今日探测以免误判")
	case st.MakeupCards.Balance > 0:
		t.Logf("makeup：该账号有 %d 张补签卡，跳过今日探测（避免万一真被扣卡）",
			st.MakeupCards.Balance)
	default:
		err := c.UseMakeupCard(a, today)
		if err == nil {
			t.Errorf("makeup：对「今日」补签本不该成功，却返回成功 —— 判据可能已变")
		} else if !IsBusinessRejection(err) {
			t.Errorf("makeup：写请求未被上游接受（非业务错误）: %v", err)
		} else {
			t.Logf("makeup：形状被接受，上游按业务规则拒绝（零消耗）: %v", err)
		}
	}
}

// liveEligibleTier 与 scheduler.growthEligibleTier 同一判据（跨包无法直接引用，此处照抄语义）。
//
// 照抄是为了让联调脚本的前置条件判定与生产一致；
// 真正的判据测试在 scheduler 包里（TestGrowthEligibleTier 表驱动）。
func liveEligibleTier(days int, rs *GrowthRedemptionStatus) string {
	if rs == nil {
		return ""
	}
	best, bestDays := "", -1
	for _, sp := range rs.Tiers {
		if sp.Tier == "" || days < sp.Days || rs.Claimed(sp.Tier) {
			continue
		}
		if sp.Days > bestDays {
			best, bestDays = sp.Tier, sp.Days
		}
	}
	return best
}
