package scheduler

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// ---------------------------------------------------------------------------
// 活跃地图闭环回归
//
// 判据链与幂等口径是本任务的核心，因此测试重点不在「请求发出去了没有」，
// 而在**边界上有没有多做写操作**：
//   - 无该日格时绝不能补签（无判据 ≠ 漏签）
//   - 无卡时绝不能补签（会白挨一次 403）
//   - 本月已领时绝不能重复兑换
//   - 无次数时绝不能抽奖（消耗性操作）
//
// 响应字面量抄自 2026-09-16 真实账号实测。
// ---------------------------------------------------------------------------

// growthStub 假上游：按区域/路径分派，并记录收到的写请求。
type growthStub struct {
	mu sync.Mutex

	streakDays    int
	cardBalance   int
	tier7dStatus  string
	tier14dStatus string
	tier28dStatus string
	cells         []map[string]any
	chances       int
	giftCredit    int64
	compCredit    int64

	streakFail  int // 非 0 时 streak 返回该状态码
	heatmapFail int
	chancesFail int

	// 写接口的拒绝开关（模拟上游业务错误）。
	makeupReject int // 0=成功，否则返回该状态码
	redeemReject int // 0=成功
	drawReject   int // 0=成功
	giftReject   int // 0=成功
	compReject   int // 0=成功

	makeupBodies []string
	redeemBodies []string
	drawBodies   []string
	giftCalls    int
	compCalls    int
	reportCalls  int
	streakCalls  int
	heatmapCalls int
	chancesCalls int
}

func (g *growthStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/v2/report":
			// 活跃上报本身不在本测试关注范围，回 200 让流程继续走到 streak 自检。
			g.reportCalls++
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))

		case "/activity/growth/streak":
			g.streakCalls++
			if g.streakFail != 0 {
				w.WriteHeader(g.streakFail)
				_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
				return
			}
			body, _ := json.Marshal(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{
					"streak":       map[string]any{"days": g.streakDays},
					"makeup_cards": map[string]any{"balance": g.cardBalance, "max": 4},
					"redemption_status": map[string]any{
						"tier_7d_status":  g.tier7dStatus,
						"tier_14d_status": g.tier14dStatus,
						"tier_28d_status": g.tier28dStatus,
						"remaining_days":  3,
						"tiers": []map[string]any{
							{"tier": "7d", "days": 7, "credit": 0, "chances": 1},
							{"tier": "14d", "days": 14, "credit": 50, "chances": 1},
							{"tier": "28d", "days": 28, "credit": 150, "chances": 1},
						},
					},
				},
			})
			_, _ = w.Write(body)

		case "/activity/growth/heatmap":
			g.heatmapCalls++
			if g.heatmapFail != 0 {
				w.WriteHeader(g.heatmapFail)
				_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
				return
			}
			body, _ := json.Marshal(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"cells": g.cells},
			})
			_, _ = w.Write(body)

		case "/activity/growth/makeup-cards/use":
			g.makeupBodies = append(g.makeupBodies, readBody(r))
			if g.makeupReject != 0 {
				w.WriteHeader(g.makeupReject)
				_, _ = w.Write([]byte(`{"code":403,"msg":"no makeup card balance"}`))
				return
			}
			// 补签成功：连登 +1，卡 -1（真实语义：保住连续性）
			g.cardBalance--
			g.streakDays++
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))

		case "/activity/growth/redeem":
			g.redeemBodies = append(g.redeemBodies, readBody(r))
			if g.redeemReject != 0 {
				w.WriteHeader(g.redeemReject)
				_, _ = w.Write([]byte(`{"code":403,"msg":"连续登录天数不足，请继续打卡或使用补签卡"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit_granted":50,"energy_granted":3,"cards_granted":1,"chances_granted":1}}`))

		case "/activity/growth/lottery/chances":
			g.chancesCalls++
			if g.chancesFail != 0 {
				w.WriteHeader(g.chancesFail)
				_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
				return
			}
			body, _ := json.Marshal(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"balance": g.chances},
			})
			_, _ = w.Write(body)

		case "/activity/growth/lottery/draw":
			g.drawBodies = append(g.drawBodies, readBody(r))
			if g.drawReject != 0 {
				w.WriteHeader(g.drawReject)
				_, _ = w.Write([]byte(`{"code":400,"msg":"insufficient lottery chance balance"}`))
				return
			}
			if g.chances > 0 {
				g.chances--
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"prize_code":"c10","prize_name":"10 积分","prize_type":"credit","credit_amount":10}}`))

		case "/billing/meter/claim-gift":
			g.giftCalls++
			if g.giftReject != 0 {
				w.WriteHeader(g.giftReject)
				_, _ = w.Write([]byte(`{"code":10001,"msg":"每人限领一次，您已领取过无法重复领取"}`))
				return
			}
			body, _ := json.Marshal(map[string]any{
				"code": 0, "msg": "OK", "data": map[string]any{"credit": g.giftCredit},
			})
			_, _ = w.Write(body)

		case "/billing/meter/claim-compensation":
			g.compCalls++
			if g.compReject != 0 {
				w.WriteHeader(g.compReject)
				_, _ = w.Write([]byte(`{"code":10001,"msg":"补偿领取活动未开启或已过期"}`))
				return
			}
			body, _ := json.Marshal(map[string]any{
				"code": 0, "msg": "OK", "data": map[string]any{"credit": g.compCredit},
			})
			_, _ = w.Write(body)

		default:
			http.Error(w, "unexpected: "+r.URL.Path, 404)
		}
	}
}

func readBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	raw, _ := io.ReadAll(r.Body)
	return string(raw)
}

// yesterdayCST 昨日（CST）——补签判据盯的就是这一天。
func yesterdayCST() string { return upstream.GrowthYesterday(time.Now()) }

// cellFor 造一个指定日期的热力格。
func cellFor(date string, score int) map[string]any {
	return map[string]any{"date": date, "score": score, "has_new_buddy": false}
}

// newGrowthScheduler 构造一个只跑活跃地图的调度器，并把账号间隔置零以加速测试。
func newGrowthScheduler(t *testing.T, g *growthStub, accounts ...*auth.Auth) *Scheduler {
	t.Helper()
	srv := httptest.NewServer(g.handler())
	t.Cleanup(srv.Close)

	p := pool.New("")
	for _, a := range accounts {
		p.Add(a)
	}
	s := New(Config{
		Pool:              p,
		Upstream:          &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		CheckinDisabled:   true,
		KeepaliveDisabled: true,
		ActivityDisabled:  true,
		NightOwlDisabled:  true,
		SchoolDisabled:    true,
		TrialDisabled:     true,
	})
	return s
}

func growthAuth() *auth.Auth { return &auth.Auth{UID: "u1", AccessToken: "tok"} }

// resetGrowthDelay 把账号间隔置零，避免测试白等。
func resetGrowthDelay(t *testing.T) {
	t.Helper()
	old := growthAccountDelay
	growthAccountDelay = 0
	t.Cleanup(func() { growthAccountDelay = old })
}

// ---------------------------------------------------------------------------
// 补签判据链
// ---------------------------------------------------------------------------

// TestMakeupWhenMissedAndHasCard 昨日漏签 + 有卡 → 补签，且 target_date 是昨日。
func TestMakeupWhenMissedAndHasCard(t *testing.T) {
	resetGrowthDelay(t)
	y := yesterdayCST()
	g := &growthStub{
		streakDays:  2,
		cardBalance: 1,
		cells: []map[string]any{
			cellFor(y, 0), // 昨日漏签
		},
	}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.makeupBodies) != 1 {
		t.Fatalf("应补签 1 次，实际 %d 次（bodies=%v）", len(g.makeupBodies), g.makeupBodies)
	}
	if !strings.Contains(g.makeupBodies[0], `"target_date":"`+y+`"`) {
		t.Errorf("target_date 应为昨日 %s，实际 body=%s", y, g.makeupBodies[0])
	}
}

// TestMakeupSkippedWhenYesterdayScored 昨日有分（未漏签）→ 绝不补签。
//
// 这是最要紧的一条：乱补签会**消耗真实的补签卡**，且补的是一个并不缺的日子。
func TestMakeupSkippedWhenYesterdayScored(t *testing.T) {
	resetGrowthDelay(t)
	y := yesterdayCST()
	g := &growthStub{
		streakDays:  2,
		cardBalance: 3, // 有卡也不能补
		cells:       []map[string]any{cellFor(y, 7)},
	}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.makeupBodies) != 0 {
		t.Errorf("昨日有分时不得补签，实际发了 %d 次: %v", len(g.makeupBodies), g.makeupBodies)
	}
}

// TestMakeupSkippedWhenNoCellForYesterday 无该日格 → 视为「无判据」，不动。
//
// 活跃地图是定长 365 格窗口，窗口外的日子天然缺席。
// 把缺席当 score==0 会去补一个根本没有判据的日子。
func TestMakeupSkippedWhenNoCellForYesterday(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{
		streakDays:  2,
		cardBalance: 3,
		// 刻意不放昨日那一格
		cells: []map[string]any{
			cellFor("2026-01-02", 0),
			cellFor("2026-01-03", 0),
		},
	}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.makeupBodies) != 0 {
		t.Errorf("无该日格时不得补签（无判据 ≠ 漏签），实际 %d 次: %v",
			len(g.makeupBodies), g.makeupBodies)
	}
	if g.heatmapCalls == 0 {
		t.Error("应至少查过一次 heatmap 作为判据")
	}
}

// TestMakeupSkippedWhenNoCard 漏签但无卡 → 不动写接口。
func TestMakeupSkippedWhenNoCard(t *testing.T) {
	resetGrowthDelay(t)
	y := yesterdayCST()
	g := &growthStub{
		streakDays:  2,
		cardBalance: 0, // 无卡
		cells:       []map[string]any{cellFor(y, 0)},
	}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.makeupBodies) != 0 {
		t.Errorf("无卡时不得发补签请求，实际 %d 次: %v", len(g.makeupBodies), g.makeupBodies)
	}
}

// TestMakeupBusinessErrorSilent 补签被业务拒绝时不 panic、不重试、不影响后续步骤。
func TestMakeupBusinessErrorSilent(t *testing.T) {
	resetGrowthDelay(t)
	y := yesterdayCST()
	g := &growthStub{
		streakDays:   2,
		cardBalance:  1,
		cells:        []map[string]any{cellFor(y, 0)},
		makeupReject: 403,
	}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow() // 不得 panic

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.makeupBodies) != 1 {
		t.Errorf("应尝试补签恰好 1 次（不因业务错误重试），实际 %d 次", len(g.makeupBodies))
	}
}

// TestMakeupSkippedOnHeatmapFailure 判据查询失败 → 静默不动写接口（无写风险）。
func TestMakeupSkippedOnHeatmapFailure(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{
		streakDays:  2,
		cardBalance: 1,
		heatmapFail: 500,
	}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.makeupBodies) != 0 {
		t.Errorf("判据拿不到时不得补签，实际 %d 次", len(g.makeupBodies))
	}
}

// TestMakeupSkippedOnFirstOfMonth 月初（CST 1 号）昨日必跨月 → 提前跳过、不发写请求。
//
// 实测跨月补签被上游拒（400 only current month makeup allowed）。
// 这条判据在真机上只在每月 1 号生效，因此必须靠单测锁定。
func TestMakeupSkippedOnFirstOfMonth(t *testing.T) {
	// 直接验证判据函数本身（真机时刻不可控，这是唯一可测的方式）。
	firstOfMonthCST := time.Date(2026, 9, 30, 16, 0, 0, 0, time.UTC) // CST 10/1 00:00
	if !upstream.GrowthMakeupOutOfMonth(firstOfMonthCST) {
		t.Error("CST 每月 1 号应判定为「昨日已跨月」，从而跳过一次必然被拒的补签")
	}
	midMonthCST := time.Date(2026, 10, 2, 4, 0, 0, 0, time.UTC) // CST 10/2 12:00
	if upstream.GrowthMakeupOutOfMonth(midMonthCST) {
		t.Error("月中不应判定为跨月")
	}
}

// ---------------------------------------------------------------------------
// 档位兑换
// ---------------------------------------------------------------------------

// TestRedeemPicksHighestEligibleTier days=20 且 7d 已领、14d 未领 → 领 14d（而不是 7d）。
func TestRedeemPicksHighestEligibleTier(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{
		streakDays:    20,
		tier7dStatus:  "claimed",
		tier14dStatus: "locked",
		tier28dStatus: "locked",
	}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.redeemBodies) != 1 {
		t.Fatalf("应兑换 1 次，实际 %d 次: %v", len(g.redeemBodies), g.redeemBodies)
	}
	if !strings.Contains(g.redeemBodies[0], `"tier":"14d"`) {
		t.Errorf("days=20 且 7d 已领时应挑 14d，实际 body=%s", g.redeemBodies[0])
	}
}

// TestRedeemHighestTierWhenAllReached days=30 全档未领 → 只领最高的 28d（一天一档）。
func TestRedeemHighestTierWhenAllReached(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{
		streakDays:    30,
		tier7dStatus:  "locked",
		tier14dStatus: "locked",
		tier28dStatus: "locked",
	}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.redeemBodies) != 1 {
		t.Fatalf("每日最多领一档，实际 %d 次: %v", len(g.redeemBodies), g.redeemBodies)
	}
	if !strings.Contains(g.redeemBodies[0], `"tier":"28d"`) {
		t.Errorf("应挑最高可领档 28d，实际 body=%s", g.redeemBodies[0])
	}
}

// TestRedeemSkippedWhenNoneEligible days=3 → 任何档都没到门槛，不动写接口。
func TestRedeemSkippedWhenNoneEligible(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{streakDays: 3, tier7dStatus: "locked"}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.redeemBodies) != 0 {
		t.Errorf("未达 7d 门槛不得发兑换请求，实际 %d 次: %v", len(g.redeemBodies), g.redeemBodies)
	}
}

// TestRedeemSkippedWhenAllClaimed 全档本月已领 → 不动写接口（幂等，不刷请求）。
func TestRedeemSkippedWhenAllClaimed(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{
		streakDays:    30,
		tier7dStatus:  "claimed",
		tier14dStatus: "claimed",
		tier28dStatus: "claimed",
	}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.redeemBodies) != 0 {
		t.Errorf("全档本月已领时不得重复兑换，实际 %d 次: %v", len(g.redeemBodies), g.redeemBodies)
	}
}

// TestGrowthEligibleTier 表驱动：挑档逻辑（含档位数组乱序）。
func TestGrowthEligibleTier(t *testing.T) {
	mk := func(statuses map[string]string, tiers []upstream.GrowthTierSpec) *upstream.GrowthRedemptionStatus {
		rs := &upstream.GrowthRedemptionStatus{Tiers: tiers}
		rs.Tier7dStatus = statuses["7d"]
		rs.Tier14dStatus = statuses["14d"]
		rs.Tier28dStatus = statuses["28d"]
		return rs
	}
	std := []upstream.GrowthTierSpec{
		{Tier: "7d", Days: 7}, {Tier: "14d", Days: 14}, {Tier: "28d", Days: 28},
	}
	cases := []struct {
		name   string
		days   int
		status map[string]string
		tiers  []upstream.GrowthTierSpec
		want   string
	}{
		{"days=0 无可领", 0, map[string]string{}, std, ""},
		{"days=6 未达 7d", 6, map[string]string{}, std, ""},
		{"days=7 领 7d", 7, map[string]string{}, std, "7d"},
		{"days=13 仍只够 7d", 13, map[string]string{}, std, "7d"},
		{"days=14 领 14d", 14, map[string]string{}, std, "14d"},
		{"days=28 领 28d", 28, map[string]string{}, std, "28d"},
		{"days=100 仍领最高的 28d", 100, map[string]string{}, std, "28d"},
		{"7d 已领 → 落到 14d", 20, map[string]string{"7d": "claimed"}, std, "14d"},
		{"7d/14d 已领 → 28d", 30, map[string]string{"7d": "claimed", "14d": "claimed"}, std, "28d"},
		{"全已领 → 空", 30, map[string]string{"7d": "claimed", "14d": "claimed", "28d": "claimed"}, std, ""},
		// 关键：不依赖 tiers 数组顺序（上游若改序，按下标取会静默挑错档）。
		{
			name: "档位数组逆序也要挑最高", days: 30, status: map[string]string{},
			tiers: []upstream.GrowthTierSpec{
				{Tier: "28d", Days: 28}, {Tier: "7d", Days: 7}, {Tier: "14d", Days: 14},
			},
			want: "28d",
		},
		{
			name: "档位数组乱序 + 14d 已领", days: 30,
			status: map[string]string{"14d": "claimed"},
			tiers: []upstream.GrowthTierSpec{
				{Tier: "14d", Days: 14}, {Tier: "28d", Days: 28}, {Tier: "7d", Days: 7},
			},
			want: "28d",
		},
		// 无 tiers 数据（上游未下发）→ 不得臆造档位。
		{"无 tiers 数据", 30, map[string]string{}, nil, ""},
		// available 状态视为可领（不是 claimed）。
		{"available 视为可领", 20, map[string]string{"7d": "available"}, std, "14d"},
	}
	for _, tc := range cases {
		got := growthEligibleTier(tc.days, mk(tc.status, tc.tiers))
		if got != tc.want {
			t.Errorf("%s: growthEligibleTier(days=%d)=%q want %q", tc.name, tc.days, got, tc.want)
		}
	}
}

// TestGrowthEligibleTierNilStatus nil 安全性。
func TestGrowthEligibleTierNilStatus(t *testing.T) {
	if got := growthEligibleTier(30, nil); got != "" {
		t.Errorf("nil status 应返回空档位，实际 %q", got)
	}
}

// TestRedeemUsesFreshTokenEachRound 两轮（隔日）兑换的幂等键必须不同。
//
// 复用幂等键会让上游把第二次领取当成第一次的重放而丢弃 —— 表现为「成功但没到账」。
func TestRedeemUsesFreshTokenEachRound(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{streakDays: 30, tier7dStatus: "locked", tier14dStatus: "locked", tier28dStatus: "locked"}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()        // 第一轮
	s.clearGrowthClaimed("u1") // 模拟次日（只看 token 是否新生成）
	s.RunGrowthMapNow()        // 第二轮

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.redeemBodies) != 2 {
		t.Fatalf("应兑换 2 次，实际 %d 次", len(g.redeemBodies))
	}
	if g.redeemBodies[0] == g.redeemBodies[1] {
		t.Errorf("两轮兑换的请求体完全相同（幂等键被复用）: %s", g.redeemBodies[0])
	}
}

// TestRedeemBusinessErrorSilent 兑换被业务拒绝（天数不足）时不 panic。
func TestRedeemBusinessErrorSilent(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{streakDays: 30, redeemReject: 403}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow() // 不得 panic
}

// ---------------------------------------------------------------------------
// 抽奖
// ---------------------------------------------------------------------------

// TestLotteryDrawsOncePerChance 有 2 次 → 抽 2 次，每次新 token。
func TestLotteryDrawsOncePerChance(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{chances: 2}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.drawBodies) != 2 {
		t.Fatalf("chances=2 应抽 2 次，实际 %d 次: %v", len(g.drawBodies), g.drawBodies)
	}
	if g.drawBodies[0] == g.drawBodies[1] {
		t.Errorf("两次抽奖的 client_token 不得相同: %s", g.drawBodies[0])
	}
}

// TestLotterySkippedWhenNoChances 无次数 → 绝不抽奖（消耗性操作）。
func TestLotterySkippedWhenNoChances(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{chances: 0}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.drawBodies) != 0 {
		t.Errorf("无次数时不得抽奖，实际 %d 次: %v", len(g.drawBodies), g.drawBodies)
	}
	if g.chancesCalls == 0 {
		t.Error("应先查询次数再决定是否抽奖")
	}
}

// TestLotteryCappedPerRound 次数异常偏大时不得在一轮里烧光（上限保护）。
func TestLotteryCappedPerRound(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{chances: 99}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.drawBodies) > maxGrowthLotteryDraws {
		t.Errorf("单轮抽奖次数应封顶 %d，实际 %d 次", maxGrowthLotteryDraws, len(g.drawBodies))
	}
}

// TestLotteryBusinessErrorStopsDrawing 抽到「无次数」业务错误 → 立即停手，不继续轰炸。
func TestLotteryBusinessErrorStopsDrawing(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{chances: 3, drawReject: 400}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.drawBodies) != 1 {
		t.Errorf("命中「无次数」业务错误后应立即停手，实际抽了 %d 次", len(g.drawBodies))
	}
}

// TestLotterySkippedOnChancesFailure 查次数失败 → 不抽奖（不做无依据的消耗）。
func TestLotterySkippedOnChancesFailure(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{chances: 5, chancesFail: 500}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.drawBodies) != 0 {
		t.Errorf("次数未知时不得抽奖，实际 %d 次", len(g.drawBodies))
	}
}

// ---------------------------------------------------------------------------
// 礼包 / 补偿 + 幂等
// ---------------------------------------------------------------------------

// TestBenefitsClaimedAndBusinessErrorSilent 礼包/补偿各调一次；已领过的业务错误不影响后续步骤。
func TestBenefitsClaimedAndBusinessErrorSilent(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{
		giftReject: 400, // 已领过
		compReject: 400, // 活动未开启
		streakDays: 2,
	}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow() // 不得 panic

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.giftCalls != 1 || g.compCalls != 1 {
		t.Errorf("礼包/补偿应各调一次，实际 gift=%d comp=%d", g.giftCalls, g.compCalls)
	}
}

// TestGrowthMapDailyIdempotent 同一天内第二轮不再打上游（按天幂等）。
func TestGrowthMapDailyIdempotent(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{streakDays: 30, chances: 1}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()
	g.mu.Lock()
	firstGift, firstRedeem := g.giftCalls, len(g.redeemBodies)
	g.mu.Unlock()

	s.growthMapOne(growthAuth()) // 模拟同日排程再次触发

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.giftCalls != firstGift {
		t.Errorf("同日第二轮不得重复领礼包，gift %d → %d", firstGift, g.giftCalls)
	}
	if len(g.redeemBodies) != firstRedeem {
		t.Errorf("同日第二轮不得重复兑换，redeem %d → %d", firstRedeem, len(g.redeemBodies))
	}
}

// TestRunGrowthMapNowBypassesDailyGate 手动触发豁免当日闸（用户显式点击就该执行）。
func TestRunGrowthMapNowBypassesDailyGate(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{giftCredit: 300}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()
	s.RunGrowthMapNow() // 手动再触发一次

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.giftCalls != 2 {
		t.Errorf("手动触发应豁免当日闸，礼包应被调 2 次，实际 %d 次", g.giftCalls)
	}
}

// TestGrowthMapSkipsDisabledAccounts 禁用账号不参与。
func TestGrowthMapSkipsDisabledAccounts(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{streakDays: 30}
	s := newGrowthScheduler(t, g,
		&auth.Auth{UID: "u1", AccessToken: "tok"},
		&auth.Auth{UID: "u2", AccessToken: "tok"},
	)
	s.cfg.Pool.Disable("u2", "测试禁用")

	s.RunGrowthMapNow()

	// 只应有一个账号跑（每号一次礼包调用）。
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.giftCalls != 1 {
		t.Errorf("禁用账号不应参与，礼包调用应为 1 次，实际 %d 次", g.giftCalls)
	}
}

// TestGrowthMapSkipsIntlByDefault 默认只跑国服（与活跃上报共用区域开关）。
func TestGrowthMapSkipsIntlByDefault(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{streakDays: 30}
	s := newGrowthScheduler(t, g,
		&auth.Auth{UID: "cn", AccessToken: "tok", Domain: "copilot.tencent.com"},
		&auth.Auth{UID: "intl", AccessToken: "tok", Domain: "www.workbuddy.ai"},
	)

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.giftCalls != 1 {
		t.Errorf("默认仅国服参与，礼包调用应为 1 次，实际 %d 次", g.giftCalls)
	}
}

// TestGrowthMapIncludesIntlWhenScopeAll scope=all 时国际版也参与。
//
// 实测依据：国际版账号的 /activity/growth/streak 与 heatmap 同样返回真实数据
// （21 个号实测 global 7 个全部 200），因此它们确实该参与。
func TestGrowthMapIncludesIntlWhenScopeAll(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{streakDays: 30}
	s := newGrowthScheduler(t, g,
		&auth.Auth{UID: "intl", AccessToken: "tok", Domain: "www.workbuddy.ai"},
	)
	s.cfg.CheckinScope = "all"

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.giftCalls != 1 {
		t.Errorf("scope=all 时国际版应参与，礼包调用应为 1 次，实际 %d 次", g.giftCalls)
	}
}

// TestGrowthMapStreakFailureSkipsRest streak 拿不到 → 不补签/不兑换/不抽奖。
//
// 三条判据全部依赖这份快照，拿不到就无从判断，只能整轮放弃（下轮重试）。
func TestGrowthMapStreakFailureSkipsRest(t *testing.T) {
	resetGrowthDelay(t)
	y := yesterdayCST()
	g := &growthStub{
		streakFail:  500,
		cardBalance: 3,
		cells:       []map[string]any{cellFor(y, 0)},
		chances:     3,
	}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.makeupBodies) != 0 {
		t.Errorf("快照拿不到时不得补签，实际 %d 次", len(g.makeupBodies))
	}
	if len(g.redeemBodies) != 0 {
		t.Errorf("快照拿不到时不得兑换，实际 %d 次", len(g.redeemBodies))
	}
	if len(g.drawBodies) != 0 {
		t.Errorf("快照拿不到时不得抽奖，实际 %d 次", len(g.drawBodies))
	}
	// 礼包/补偿不依赖快照，先于快照执行，因此仍然应该领（先落袋）。
	if g.giftCalls != 1 || g.compCalls != 1 {
		t.Errorf("礼包/补偿不依赖连登快照，仍应领取：gift=%d comp=%d", g.giftCalls, g.compCalls)
	}
}

// TestGrowthMapOrderBenefitsBeforeStreak 顺序：礼包/补偿 → 快照 → 补签 → 兑换 → 抽奖。
//
// 顺序错了会静默出错：若先兑换后补签，补签恢复的天数要等到明天才能用来兑换。
func TestGrowthMapOrderBenefitsBeforeStreak(t *testing.T) {
	resetGrowthDelay(t)
	y := yesterdayCST()
	g := &growthStub{
		streakDays:  6, // 补签后变 7 → 够 7d 档
		cardBalance: 1,
		cells:       []map[string]any{cellFor(y, 0)},
	}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	// 补签成功了（卡 -1、天数 +1）
	if len(g.makeupBodies) != 1 {
		t.Fatalf("应补签 1 次，实际 %d 次", len(g.makeupBodies))
	}
	// 关键断言：补签后重读快照拿到了 days=7，因此**当天**就兑换了 7d 档 ——
	// 若实现用补签前的旧天数（6），这里就不会有兑换请求。
	if len(g.redeemBodies) != 1 {
		t.Fatalf("补签到 7 天后应当天即可兑换 7d（证明用了补签后的天数），实际兑换 %d 次",
			len(g.redeemBodies))
	}
	if !strings.Contains(g.redeemBodies[0], `"tier":"7d"`) {
		t.Errorf("应兑换 7d，实际 body=%s", g.redeemBodies[0])
	}
	// streak 快照至少读了两次：一次初始，一次补签后重读。
	if g.streakCalls < 2 {
		t.Errorf("补签成功后应重读 streak 快照，实际读取 %d 次", g.streakCalls)
	}
}

// TestGrowthMapReusesActivityStreakSnapshot 活跃上报路径下，闭环不得为连登快照再打一次上游。
//
// 依据是需求里的硬约束「复用既有 streak 请求，不要多发一次」：
// data.streak / data.makeup_cards / data.redemption_status 本就在同一个响应体里，
// 分两次读既多打一次上游，也让同一份数据出现两个可能不一致的快照。
func TestGrowthMapReusesActivityStreakSnapshot(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{
		streakDays:   30,
		tier7dStatus: "locked", tier14dStatus: "locked", tier28dStatus: "locked",
	}
	s := newGrowthScheduler(t, g, growthAuth())
	// 只留活跃上报开启，其余关闭，构造「上报 → 闭环」的真实路径。
	s.cfg.ActivityDisabled = false
	s.cfg.ActivityReportCount = 1

	s.RunActivityNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	// 恰好 1 次：上报后的自检。闭环复用了它，没有第二次。
	if g.streakCalls != 1 {
		t.Errorf("活跃上报路径下 streak 应只读 1 次（闭环复用自检那份），实际 %d 次", g.streakCalls)
	}
	// 但闭环确实跑了：days=30 应触发兑换。
	if len(g.redeemBodies) != 1 {
		t.Errorf("闭环应复用快照完成兑换，实际兑换 %d 次", len(g.redeemBodies))
	}
}

// TestGrowthMapReadsOwnSnapshotWhenManual 手动触发路径（无上报）时闭环自己读快照。
func TestGrowthMapReadsOwnSnapshotWhenManual(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{streakDays: 30}
	s := newGrowthScheduler(t, g, growthAuth())

	s.RunGrowthMapNow()

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.streakCalls != 1 {
		t.Errorf("手动触发时应自己读 1 次快照，实际 %d 次", g.streakCalls)
	}
}

// TestGrowthMapEmptyPool 空账号池不 panic。
func TestGrowthMapEmptyPool(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{}
	s := newGrowthScheduler(t, g)
	s.RunGrowthMapNow() // 不得 panic
}

// TestRunTaskByNameGrowthMap 手动触发入口可识别 growthmap。
func TestRunTaskByNameGrowthMap(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{streakDays: 30}
	s := newGrowthScheduler(t, g, growthAuth())

	res, err := s.RunTaskByName(TaskNameGrowthMap)
	if err != nil {
		t.Fatalf("RunTaskByName(%s): %v", TaskNameGrowthMap, err)
	}
	if !res.Ran {
		t.Errorf("应回报已执行: %+v", res)
	}
}

// TestLogGrowthErrWarnsOnSessionDead 登录态失效必须记 WARN，不能被当业务拒绝静默。
//
// 这是**真实实测发现的缺口**：拿失效 token 打 growth 端点时，网关回的是
// openresty 的 401 HTML 页（正文无 12153），早期实现按「4xx = 业务拒绝」一刀切，
// 把这类认证故障静默掉了 —— 表现为「token 已死但任务永远不吭声」。
func TestLogGrowthErrWarnsOnSessionDead(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"401 会话失效 12153", &upstream.Error{Kind: upstream.ErrSessionDead, Status: 401, Msg: "Offline user session not found"}},
		{"401 openresty HTML 页（实测）", &upstream.Error{Kind: upstream.ErrClient, Status: 401, Msg: "<html><head><title>401 Authorization Required</title></head>"}},
		{"404 端点不存在", &upstream.Error{Kind: upstream.ErrNotFound, Status: 404, Msg: "Route Not Found"}},
		{"429 限流", &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, Msg: "too many requests"}},
		{"500 上游故障", &upstream.Error{Kind: upstream.ErrServer, Status: 500, Msg: "boom"}},
	}
	for _, tc := range cases {
		if upstream.IsBusinessRejection(tc.err) {
			t.Errorf("%s: 不该被当成业务拒绝（会被静默掉，故障无人知晓）", tc.name)
		}
		logGrowthErr("growth", "u1", "test", tc.err) // 不得 panic
	}
}

// TestGrowthBenefitsWarnOnSessionDead 礼包/补偿遇到认证失败时不得静默吞掉。
//
// 直接验证 claimGrowthBenefits 的错误分支：它必须走 logGrowthErr，
// 而不是像早期实现那样 `if err == nil` 直接丢弃。
func TestGrowthBenefitsWarnOnSessionDead(t *testing.T) {
	resetGrowthDelay(t)
	html401 := "<html><head><title>401 Authorization Required</title></head></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(html401))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(growthAuth())
	s := New(Config{
		Pool:              p,
		Upstream:          &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL},
		CheckinDisabled:   true,
		KeepaliveDisabled: true,
		ActivityDisabled:  true,
		NightOwlDisabled:  true,
		SchoolDisabled:    true,
		TrialDisabled:     true,
	})

	// 捕获日志：认证失败必须留下一条 WARN。
	var buf strings.Builder
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	s.RunGrowthMapNow()

	if !strings.Contains(buf.String(), "401") {
		t.Errorf("失效 token 的 401 必须留下日志痕迹，实际输出:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "claim-gift") {
		t.Errorf("礼包失败应被记录（含失败环节名），实际输出:\n%s", buf.String())
	}
}

// TestGrowthBenefitsBusinessErrorStaysSilent 业务拒绝仍然必须完全静默（不刷 WARN）。
func TestGrowthBenefitsBusinessErrorStaysSilent(t *testing.T) {
	resetGrowthDelay(t)
	g := &growthStub{giftReject: 400, compReject: 400}
	s := newGrowthScheduler(t, g, growthAuth())

	var buf strings.Builder
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	s.RunGrowthMapNow()

	out := buf.String()
	if strings.Contains(out, "WARN") {
		t.Errorf("礼包/补偿的业务拒绝（已领过）是每天常态，不得刷 WARN，实际输出:\n%s", out)
	}
	if strings.Contains(out, "claim-gift") || strings.Contains(out, "claim-compensation") {
		t.Errorf("业务拒绝不应留下日志，实际输出:\n%s", out)
	}
}

// TestLogGrowthErrSilentForBusinessRejection 业务拒绝静默、传输层失败才记 WARN。
//
// 这是「不刷 WARN」原则的代码级证据：两类错误走的是同一个函数，
// 差别只在 IsBusinessRejection。
func TestLogGrowthErrSilentForBusinessRejection(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"业务拒绝 403", &upstream.Error{Kind: upstream.ErrClient, Status: 403, Msg: "no makeup card balance"}},
		{"业务拒绝 400", &upstream.Error{Kind: upstream.ErrClient, Status: 400, Msg: "insufficient lottery chance balance"}},
		{"业务拒绝 409", &upstream.Error{Kind: upstream.ErrClient, Status: 409, Msg: "duplicate"}},
	}
	for _, tc := range cases {
		if !upstream.IsBusinessRejection(tc.err) {
			t.Errorf("%s: 应被判定为业务拒绝（静默）", tc.name)
		}
		logGrowthErr("growth", "u1", "test", tc.err) // 不得 panic
	}
	// 传输层与 5xx 不是业务拒绝 → 会被记 WARN。
	if upstream.IsBusinessRejection(nil) {
		t.Error("nil 不是业务拒绝")
	}
	if upstream.IsBusinessRejection(&upstream.Error{Kind: upstream.ErrServer, Status: 500, Msg: "boom"}) {
		t.Error("5xx 不是业务拒绝（必须记 WARN，否则故障无人知晓）")
	}
}
