//go:build live

// growthmap_live_test.go 活跃地图**完整调度路径**真实账号联调
// （只在显式指定 live 构建标签时编译与运行）。
//
// 与 upstream 包的 TestLiveGrowth* 的分工：
//   - upstream 侧验证「单个端点/解析器对真实响应的适配」
//   - 本文件验证「调度器 decision 链在真实上游下的行为」——
//     也就是生产真正跑的那条路径：growthMapRound → 礼包/补偿 → 快照 → 补签 → 兑换 → 抽奖。
//
// 为什么必须单独有这一层：单测里的假上游是我自己写的，它只能证明「代码按我理解的语义走」，
// 证明不了「我的理解与真实上游一致」。只有把 scheduler 直接接到真实上游上跑一轮，
// 才能确认判据链在真实数据下不会做出多余的写操作。
//
// 运行方式（PowerShell）：
//
//	$env:WB_LIVE_ACCOUNTS = "D:\...\test-bin\map-probe\accounts.json"
//	$env:WB_LIVE_ACCOUNT  = "<uid 前 8 位>"   # 缺省用第一个国服账号；此处不写真实值
//	go test -tags live ./internal/scheduler/ -run TestLiveGrowthMapScheduler -v
//
// 写操作分级：默认**只领礼包/补偿**（幂等、有则领、领到即收益）。
// 补签/兑换/抽奖在本文件里**不会执行** —— 它们分别需要
// WB_LIVE_MAKEUP / WB_LIVE_REDEEM / WB_LIVE_DRAW，且真实数据下前置条件不成立时本就跳过。
package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// liveMapAccount 所有者账号库里的单条记录（snake_case，与网关凭证形态不同）。
type liveMapAccount struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	UID          string `json:"uid"`
	Domain       string `json:"domain"`
	ExpiresAt    int64  `json:"expiresAt"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// liveMapUID8 脱敏：只保留 uid 前 8 位。
func liveMapUID8(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	if uid == "" {
		return "(nouid)"
	}
	return uid
}

// loadLiveMapAuth 把账号库里的一个账号转成网关凭证形态（走生产同一套 auth.LoadDir）。
func loadLiveMapAuth(t *testing.T, uid8 string) *auth.Auth {
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
	var pick *liveMapAccount
	for i := range all {
		a := &all[i]
		if uid8 != "" && strings.HasPrefix(a.UID, uid8) {
			pick = a
			break
		}
		if uid8 == "" && !strings.HasSuffix(strings.ToLower(a.Domain), ".ai") {
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

// TestLiveGrowthMapScheduler 把真实 scheduler 接到**真实上游**跑一轮活跃地图闭环。
//
// 关注点不是「有没有成功」，而是「有没有做出不该做的写操作」：
// 真实账号此时不满足补签/兑换/抽奖的前置条件，因此这三条路径**必须静默跳过**，
// 只允许礼包/补偿（幂等、有则领）发生。
func TestLiveGrowthMapScheduler(t *testing.T) {
	a := loadLiveMapAuth(t, os.Getenv("WB_LIVE_ACCOUNT"))
	t.Logf("账号 uid8=%s realm=%s", liveMapUID8(a.UID), a.Realm())

	// 关掉账号之间与账号内部的限速，让联调快速跑完（生产值见各文件的 delay 变量）。
	oldGrowthDelay := growthAccountDelay
	growthAccountDelay = 0
	t.Cleanup(func() { growthAccountDelay = oldGrowthDelay })

	p := pool.New("")
	p.Add(a)
	s := New(Config{
		Pool:              p,
		Upstream:          upstream.New(), // 生产客户端 = 真实域名
		CheckinDisabled:   true,
		KeepaliveDisabled: true,
		ActivityDisabled:  true,
		NightOwlDisabled:  true,
		SchoolDisabled:    true,
		TrialDisabled:     true,
	})

	// 前置快照：先只读看清真实判据，作为「应当发生什么」的预期。
	st, err := s.cfg.Upstream.GrowthStreakState(a)
	if err != nil {
		t.Fatalf("前置 streak 读取失败: %v", err)
	}
	cells, err := s.cfg.Upstream.GrowthHeatmap(a)
	if err != nil {
		t.Fatalf("前置 heatmap 读取失败: %v", err)
	}
	yesterday := upstream.GrowthYesterday(time.Now())
	score, hasCell := upstream.HeatmapDayScore(cells, yesterday)
	chances, err := s.cfg.Upstream.GrowthLotteryChances(a)
	if err != nil {
		t.Fatalf("前置 chances 读取失败: %v", err)
	}

	wantMakeup := hasCell && score == 0 && st.MakeupCards.Balance > 0 &&
		!upstream.GrowthMakeupOutOfMonth(time.Now())
	wantTier := growthEligibleTier(st.Days(), &st.Redemption)
	t.Logf("前置判据: days=%d 补签卡=%d 昨日(%s)=%v/%v chances=%d",
		st.Days(), st.MakeupCards.Balance, yesterday, score, hasCell, chances)
	t.Logf("预期: 补签=%v 兑换档位=%q 抽奖次数=%d（补签/兑换/抽奖默认不执行，由环境变量控制）",
		wantMakeup, wantTier, chances)

	// 跑一轮真实闭环。RunGrowthMapNow 豁免当日闸，等价于「用户点了一次立即执行」。
	s.RunGrowthMapNow()

	// 回读：确认闭环没有把账号状态搞坏（能读通、天数没倒退）。
	after, err := s.cfg.Upstream.GrowthStreakState(a)
	if err != nil {
		t.Fatalf("闭环后 streak 读取失败（账号状态可能被破坏）: %v", err)
	}
	t.Logf("闭环后: days=%d 补签卡=%d 7d=%q", after.Days(), after.MakeupCards.Balance,
		after.Redemption.Tier7dStatus)

	if after.Days() < st.Days() {
		t.Errorf("连登天数从 %d 倒退到 %d —— 闭环不应让天数倒退", st.Days(), after.Days())
	}
	// 补签路径：真实数据下前置条件不成立时，卡余额必须原样不动。
	if !wantMakeup && after.MakeupCards.Balance != st.MakeupCards.Balance {
		t.Errorf("前置条件不成立却改了补签卡余额：%d → %d（不应发生写操作）",
			st.MakeupCards.Balance, after.MakeupCards.Balance)
	}
	// 兑换路径：无已达标未领档位时，档位状态必须原样不动。
	if wantTier == "" && after.Redemption.Tier7dStatus != st.Redemption.Tier7dStatus {
		t.Errorf("无达标档位却改了兑换状态：%q → %q",
			st.Redemption.Tier7dStatus, after.Redemption.Tier7dStatus)
	}
}

// TestLiveGrowthMapSchedulerManyAccounts 对账号库里的多个**国服**账号跑一轮真实闭环。
//
// 覆盖面价值：单账号只能证明「这条路能走通」，多账号能暴露
// 「某些账号的数据形状与预期不同」（例如 redemption_status 缺失、
// tiers 为空、cells 不含昨日）——这些正是判据链最容易被真实数据打脸的地方。
func TestLiveGrowthMapSchedulerManyAccounts(t *testing.T) {
	if os.Getenv("WB_LIVE_MANY") != "1" {
		t.Skip("未设置 WB_LIVE_MANY=1，跳过多账号实测（会对多个真实账号发请求）")
	}
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

	limit := 3
	if v := os.Getenv("WB_LIVE_MANY_LIMIT"); v != "" {
		if n, err := json.Number(v).Int64(); err == nil && n > 0 {
			limit = int(n)
		}
	}

	up := upstream.New()
	yesterday := upstream.GrowthYesterday(time.Now())
	checked := 0
	for i := range all {
		if checked >= limit {
			break
		}
		acct := &all[i]
		// 只跑国服：默认区域范围内国际版不参与（与 checkin_scope=cn 一致）。
		if strings.HasSuffix(strings.ToLower(acct.Domain), ".ai") {
			continue
		}
		if strings.TrimSpace(acct.UID) == "" {
			continue
		}
		checked++

		dir := t.TempDir()
		doc := map[string]any{
			"auth": map[string]any{
				"accessToken":  acct.AccessToken,
				"refreshToken": acct.RefreshToken,
				"expiresAt":    acct.ExpiresAt,
				"domain":       acct.Domain,
			},
			"account": map[string]any{
				"uid":          acct.UID,
				"enterpriseId": acct.EnterpriseID,
				"nickname":     acct.Nickname,
			},
		}
		enc, _ := json.MarshalIndent(doc, "", "  ")
		file := filepath.Join(dir, "workbuddy-live.json")
		if err := os.WriteFile(file, enc, 0o600); err != nil {
			t.Fatalf("写入临时凭证失败: %v", err)
		}
		auths, err := auth.LoadDir(dir)
		if err != nil || len(auths) != 1 {
			t.Fatalf("加载临时凭证失败: %v", err)
		}
		a := auths[0]

		st, err := up.GrowthStreakState(a)
		if err != nil {
			t.Logf("[%s] streak 读取失败: %v", liveMapUID8(a.UID), err)
			continue
		}
		cells, err := up.GrowthHeatmap(a)
		if err != nil {
			t.Logf("[%s] heatmap 读取失败: %v", liveMapUID8(a.UID), err)
			continue
		}
		score, hasCell := upstream.HeatmapDayScore(cells, yesterday)
		tier := growthEligibleTier(st.Days(), &st.Redemption)
		t.Logf("[%s] days=%d 卡=%d/%d tiers=%d 昨日(%s)=%v/%v 可领档=%q 7d=%q",
			liveMapUID8(a.UID), st.Days(), st.MakeupCards.Balance, st.MakeupCards.Max,
			len(st.Redemption.Tiers), yesterday, score, hasCell, tier, st.Redemption.Tier7dStatus)

		// 判据自洽性断言：真实数据下 tiers 应恒为 3（7d/14d/28d）。
		if len(st.Redemption.Tiers) != 3 {
			t.Errorf("[%s] tiers=%d 应为 3 —— 上游形状可能已变", liveMapUID8(a.UID), len(st.Redemption.Tiers))
		}
		if st.MakeupCards.Max != 4 {
			t.Errorf("[%s] makeup_cards.max=%d 应为 4 —— 上游形状可能已变",
				liveMapUID8(a.UID), st.MakeupCards.Max)
		}
		if !hasCell {
			t.Errorf("[%s] 昨日 %s 不在热力格窗口内 —— 活跃地图窗口假设可能已变",
				liveMapUID8(a.UID), yesterday)
		}
	}
	if checked == 0 {
		t.Fatal("账号库里没有可用的国服账号")
	}
	t.Logf("已核对 %d 个国服账号", checked)
}
