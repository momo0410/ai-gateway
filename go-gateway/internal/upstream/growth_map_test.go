package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// ---------------------------------------------------------------------------
// 活跃地图 upstream 层回归
//
// 本文件的响应字面量全部抄自 2026-09-16 的真实账号实测（21 个号，国服 14 / 国际版 7），
// 不是凭空构造的：字段名、状态码、中文文案都与上游一致，
// 这样一旦上游改字段，测试会因为解析出零值而失败，而不是静默返回空数据。
// ---------------------------------------------------------------------------

// streakRealBody 实测 GET /activity/growth/streak 的完整响应（脱敏：requestId 换成固定值）。
const streakRealBody = `{"code":0,"msg":"OK","requestId":"r1","data":{` +
	`"streak":{"days":2,"month_total_days":8,"month_consumed_days":0,` +
	`"next_tier":"7d","next_tier_remaining":4,"makeup_dates":[]},` +
	`"makeup_cards":{"balance":0,"max":4},` +
	`"redemption_status":{"tier_7d_count":0,"tier_14d_count":0,"tier_28d_count":0,` +
	`"tier_7d_status":"locked","tier_14d_status":"locked","tier_28d_status":"locked",` +
	`"remaining_days":3,"tiers":[` +
	`{"tier":"7d","days":7,"credit":0,"energy":2,"cards":1,"chances":1},` +
	`{"tier":"14d","days":14,"credit":50,"energy":3,"cards":1,"chances":1},` +
	`{"tier":"28d","days":28,"credit":150,"energy":5,"cards":1,"chances":1}]},` +
	`"timezone":"Asia/Shanghai","launch_date":"2026-06-17"}}`

// growthTestClient 构造一个只服务 growth/billing 域的测试客户端。
func growthTestClient(fn rtFunc) *Client { return testClient(fn) }

// TestGrowthStreakStateParsesRealResponse 用实测响应验证三个切片都被解析出来。
//
// 这个测试的价值在于**切片层级**：makeup_cards 与 redemption_status 是 data 下的
// 同级字段（不是嵌在 streak 里）。层级写错时字段会静默为 0，
// 表现为「补签永远说没卡」——不报错、不崩，极难排查。
func TestGrowthStreakStateParsesRealResponse(t *testing.T) {
	c := growthTestClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return nil, errors.New("want GET")
		}
		if err := travelPath(r, "/activity/growth/streak"); err != nil {
			return nil, err
		}
		return jsonResp(200, streakRealBody), nil
	})

	st, err := c.GrowthStreakState(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("streak state: %v", err)
	}
	if st.Days() != 2 {
		t.Errorf("days=%d want 2", st.Days())
	}
	if st.Streak.MonthTotalDays != 8 {
		t.Errorf("month_total_days=%d want 8", st.Streak.MonthTotalDays)
	}
	if st.MakeupCards.Balance != 0 || st.MakeupCards.Max != 4 {
		t.Errorf("makeup_cards=%+v want {0 4}", st.MakeupCards)
	}
	if st.Redemption.Tier7dStatus != "locked" || st.Redemption.Tier14dStatus != "locked" {
		t.Errorf("redemption statuses=%+v", st.Redemption)
	}
	if len(st.Redemption.Tiers) != 3 {
		t.Fatalf("tiers=%d want 3", len(st.Redemption.Tiers))
	}
	// 档位门槛与赠品：7d/14d/28d，credit 分别为 0/50/150。
	wantDays := []int{7, 14, 28}
	wantCredit := []int{0, 50, 150}
	wantChances := []int{1, 1, 1}
	for i, sp := range st.Redemption.Tiers {
		if sp.Days != wantDays[i] || sp.Credit != wantCredit[i] || sp.Chances != wantChances[i] {
			t.Errorf("tiers[%d]=%+v want days=%d credit=%d chances=%d",
				i, sp, wantDays[i], wantCredit[i], wantChances[i])
		}
		if sp.Cards != 1 {
			t.Errorf("tiers[%d].cards=%d want 1（每档都送补签卡）", i, sp.Cards)
		}
	}
}

// TestGrowthStreakStateToleratesMissingFields 缺 redemption_status 时不得报错。
//
// 上游若在某区域/某版本不返回兑换段，解析失败会让整个闭环连补签都做不了；
// 零值语义（各档未 claimed）是安全的降级：挑档仍由天数闸兜底。
func TestGrowthStreakStateToleratesMissingFields(t *testing.T) {
	c := growthTestClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"streak":{"days":9}}}`), nil
	})
	st, err := c.GrowthStreakState(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("streak state: %v", err)
	}
	if st.Days() != 9 {
		t.Errorf("days=%d want 9", st.Days())
	}
	if len(st.Redemption.Tiers) != 0 {
		t.Errorf("tiers 应为空，实际 %+v", st.Redemption.Tiers)
	}
}

// TestGrowthHeatmapParsesCells 热力格解析（含实测的 has_new_buddy 额外字段）。
func TestGrowthHeatmapParsesCells(t *testing.T) {
	c := growthTestClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return nil, errors.New("want GET")
		}
		if err := travelPath(r, "/activity/growth/heatmap"); err != nil {
			return nil, err
		}
		return jsonResp(200, `{"code":0,"data":{"cells":[`+
			`{"date":"2026-09-14","score":0,"has_new_buddy":false},`+
			`{"date":"2026-09-15","score":2,"has_new_buddy":false},`+
			`{"date":"2026-09-16","score":6,"has_new_buddy":true}],`+
			`"today":{"date":"2026-09-16","score":6,"is_active":true},`+
			`"range":{"start_date":"2025-09-17","end_date":"2026-09-16","days":365}}}`), nil
	})
	cells, err := c.GrowthHeatmap(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("heatmap: %v", err)
	}
	if len(cells) != 3 {
		t.Fatalf("cells=%d want 3", len(cells))
	}
	if cells[1].Date != "2026-09-15" || cells[1].Score != 2 {
		t.Errorf("cells[1]=%+v", cells[1])
	}
}

// TestHeatmapDayScore 表驱动：命中 / 无该日格 / 带时间后缀。
func TestHeatmapDayScore(t *testing.T) {
	cells := []HeatmapCell{
		{Date: "2026-09-14", Score: 0},
		{Date: "2026-09-15", Score: 2},
		{Date: "2026-09-16T00:00:00+08:00", Score: 6},
	}
	cases := []struct {
		name      string
		date      string
		wantScore int
		wantOK    bool
	}{
		{"命中 score=0（漏签）", "2026-09-14", 0, true},
		{"命中 score>0（已活跃）", "2026-09-15", 2, true},
		{"带时间后缀也应命中", "2026-09-16", 6, true},
		// 关键分支：无该日格必须是 ok=false，不能与 score==0 混为一谈 ——
		// 混了就会对活跃地图窗口外的日子发起必然失败的补签。
		{"无该日格", "2026-09-13", 0, false},
		{"完全不在窗口内", "2000-01-01", 0, false},
		{"空 cells", "2026-09-14", 0, false},
	}
	for _, tc := range cases {
		cs := cells
		if tc.name == "空 cells" {
			cs = nil
		}
		score, ok := HeatmapDayScore(cs, tc.date)
		if ok != tc.wantOK || (ok && score != tc.wantScore) {
			t.Errorf("%s: HeatmapDayScore(%q)=(%d,%v) want (%d,%v)",
				tc.name, tc.date, score, ok, tc.wantScore, tc.wantOK)
		}
	}
}

// TestGrowthYesterday 昨日口径固定 CST（+8），与宿主机时区无关。
func TestGrowthYesterday(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		{
			// UTC 16:30 = CST 次日 00:30：跨过 CST 零点后，「昨日」必须是 CST 的那一天。
			// 若实现用本地时区，在 UTC 机器上会算出前一天，补签就补错了日子。
			name: "UTC 跨 CST 零点",
			now:  time.Date(2026, 9, 16, 16, 30, 0, 0, time.UTC),
			want: "2026-09-16",
		},
		{
			name: "CST 当日",
			now:  time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC), // CST 18:00
			want: "2026-09-15",
		},
		{
			// UTC 15:59 = CST 23:59，仍是同一天 → 昨日是 9/15。
			name: "CST 零点前一刻",
			now:  time.Date(2026, 9, 16, 15, 59, 0, 0, time.UTC),
			want: "2026-09-15",
		},
		{
			name: "跨月边界（10/1 CST 的昨日是 9/30）",
			now:  time.Date(2026, 9, 30, 16, 0, 0, 0, time.UTC), // CST 10/1 00:00
			want: "2026-09-30",
		},
	}
	for _, tc := range cases {
		if got := GrowthYesterday(tc.now); got != tc.want {
			t.Errorf("%s: GrowthYesterday(%v)=%q want %q", tc.name, tc.now, got, tc.want)
		}
	}
}

// TestGrowthMakeupOutOfMonth 月初判据：CST 每月 1 号的「昨日」已跨月，补签必被拒。
//
// 实测上游对跨月补签回 400 only current month makeup allowed，
// 因此调用方应在月初直接跳过，省掉这次注定失败的写请求。
func TestGrowthMakeupOutOfMonth(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"CST 10/1 00:00 应判跨月", time.Date(2026, 9, 30, 16, 0, 0, 0, time.UTC), true},
		{"CST 10/1 12:00 应判跨月", time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC), true},
		{"CST 10/2 应可补", time.Date(2026, 10, 2, 4, 0, 0, 0, time.UTC), false},
		// UTC 15:59 = CST 9/30 23:59 → 还是 30 号，昨日在月内。
		{"CST 9/30（月内最后一天）", time.Date(2026, 9, 30, 15, 59, 0, 0, time.UTC), false},
	}
	for _, tc := range cases {
		if got := GrowthMakeupOutOfMonth(tc.now); got != tc.want {
			t.Errorf("%s: GrowthMakeupOutOfMonth(%v)=%v want %v", tc.name, tc.now, got, tc.want)
		}
	}
}

// TestUseMakeupCardSendsTargetDate 补签请求体必须带正确的 target_date。
func TestUseMakeupCardSendsTargetDate(t *testing.T) {
	var got []byte
	c := growthTestClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			return nil, errors.New("want POST")
		}
		if err := travelPath(r, "/activity/growth/makeup-cards/use"); err != nil {
			return nil, err
		}
		got, _ = io.ReadAll(r.Body)
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	if err := c.UseMakeupCard(&auth.Auth{AccessToken: "at", UID: "u1"}, "2026-09-15"); err != nil {
		t.Fatalf("use makeup card: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatalf("body parse: %v (%s)", err, got)
	}
	if body["target_date"] != "2026-09-15" {
		t.Errorf("target_date=%v want 2026-09-15 (body=%s)", body["target_date"], got)
	}
}

// TestMakeupBusinessRejections 补签的三种业务拒绝必须是可识别的（静默依据）。
// 文案与状态码取自 2026-09-16 实测。
func TestMakeupBusinessRejections(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantNoCard bool
		wantBroken bool
		wantMonth  bool
	}{
		{
			name:   "实测：无补签卡（403）",
			status: 403, body: `{"code":403,"msg":"no makeup card balance","requestId":"x"}`,
			wantNoCard: true,
		},
		{
			name:   "实测：该日无漏签（400）",
			status: 400, body: `{"code":400,"msg":"date is not broken, no makeup needed","requestId":"x"}`,
			wantBroken: true,
		},
		{
			name:   "实测：跨月（400）",
			status: 400, body: `{"code":400,"msg":"only current month makeup allowed","requestId":"x"}`,
			wantMonth: true,
		},
	}
	for _, tc := range cases {
		c := growthTestClient(func(r *http.Request) (*http.Response, error) {
			return jsonResp(tc.status, tc.body), nil
		})
		err := c.UseMakeupCard(&auth.Auth{AccessToken: "at", UID: "u1"}, "2026-09-15")
		if err == nil {
			t.Fatalf("%s: want error", tc.name)
		}
		if got := IsMakeupNoCard(err); got != tc.wantNoCard {
			t.Errorf("%s: IsMakeupNoCard=%v want %v (%v)", tc.name, got, tc.wantNoCard, err)
		}
		if got := IsMakeupNotBroken(err); got != tc.wantBroken {
			t.Errorf("%s: IsMakeupNotBroken=%v want %v", tc.name, got, tc.wantBroken)
		}
		if got := IsMakeupOutOfMonth(err); got != tc.wantMonth {
			t.Errorf("%s: IsMakeupOutOfMonth=%v want %v", tc.name, got, tc.wantMonth)
		}
		// 三种都是 HTTP 4xx 的业务拒绝 → 必须被 IsBusinessRejection 认定为「常态」，
		// 否则调度侧会为每天的常态刷 WARN。
		if !IsBusinessRejection(err) {
			t.Errorf("%s: 业务拒绝应被 IsBusinessRejection 认定", tc.name)
		}
	}
}

// TestIsBusinessRejectionTable 表驱动钉死「什么该静默、什么必须报」的分界线。
//
// 这份表来自一次**真实实测踩到的坑**：拿失效 token 打 growth 端点时，
// 网关返回的是 openresty 的 401 HTML 错误页（正文里没有 12153），
// Classify 因此把它判成 ErrClient。若按「4xx = 业务拒绝」一刀切，
// 这个错误会被静默掉 —— 表现为「token 已死但任务永远不吭声」。
func TestIsBusinessRejectionTable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// —— 业务答复：必须静默 ——
		{"礼包已领过 400", &Error{Kind: ErrClient, Status: 400, Msg: `code=10001 msg=每人限领一次，您已领取过无法重复领取`}, true},
		{"无补签卡 403", &Error{Kind: ErrClient, Status: 403, Msg: "no makeup card balance"}, true},
		{"天数不足 403", &Error{Kind: ErrClient, Status: 403, Msg: "连续登录天数不足，请继续打卡或使用补签卡"}, true},
		{"无抽奖次数 400", &Error{Kind: ErrClient, Status: 400, Msg: "insufficient lottery chance balance"}, true},
		{"未知档位 400", &Error{Kind: ErrClient, Status: 400, Msg: "unknown tier"}, true},

		// —— 真正的故障：必须记 ——
		{"nil", nil, false},
		{"传输层失败", errors.New("connection reset by peer"), false},
		{"401 登录态失效（12153）", &Error{Kind: ErrSessionDead, Status: 401, Msg: "Offline user session not found"}, false},
		// 关键用例：openresty 401 HTML 页被 Classify 判成 ErrClient，但状态码是 401。
		// 这正是实测遇到的那一种，绝不能被当成业务答复静默掉。
		{"401 网关 HTML 错误页（实测坑）", &Error{Kind: ErrClient, Status: 401, Msg: "<html><head><title>401 Authorization Required</title></head>"}, false},
		{"404 端点不存在", &Error{Kind: ErrNotFound, Status: 404, Msg: "Route Not Found"}, false},
		{"429 被限流", &Error{Kind: ErrSoftRate, Status: 429, Msg: "too many requests"}, false},
		{"500 上游故障", &Error{Kind: ErrServer, Status: 500, Msg: "boom"}, false},
		{"503 上游故障", &Error{Kind: ErrServer, Status: 503, Msg: "unavailable"}, false},
		{"402 额度耗尽", &Error{Kind: ErrHardCredit, Status: 402, Msg: "payment required"}, false},
	}
	for _, tc := range cases {
		if got := IsBusinessRejection(tc.err); got != tc.want {
			t.Errorf("%s: IsBusinessRejection=%v want %v (%v)", tc.name, got, tc.want, tc.err)
		}
	}
}

// TestIsBusinessRejectionViaRealClassification 用真实响应体走一遍 Classify，
// 确认「分类 → 是否静默」这条链在端到端上是自洽的。
//
// 单独写一个是因为前面的表是手工构造 *Error；这里走的是
// Classify(status, body) 这条真实路径（doJSON 内部就是这么调用的）。
func TestIsBusinessRejectionViaRealClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{
			name:   "实测：礼包已领过",
			status: 400,
			body:   `{"code":10001,"msg":"每人限领一次，您已领取过无法重复领取"}`,
			want:   true,
		},
		{
			name:   "实测：无补签卡",
			status: 403,
			body:   `{"code":403,"msg":"no makeup card balance"}`,
			want:   true,
		},
		{
			// 实测原文（openresty 401 页，无 12153）—— 必须判为「要记」。
			name:   "实测：失效 token 的 openresty 401 页",
			status: 401,
			body:   "<html>\n<head><title>401 Authorization Required</title></head>\n<body>\n<center><h1>401 Authorization Required</h1></center>\n<hr><center>openresty</center>\n</body>\n</html>",
			want:   false,
		},
		{
			name:   "实测：会话失效 12153",
			status: 401,
			body:   `{"code":12153,"msg":"Offline user session not found"}`,
			want:   false,
		},
	}
	for _, tc := range cases {
		kind := Classify(tc.status, tc.body)
		err := &Error{Kind: kind, Status: tc.status, Msg: tc.body}
		if got := IsBusinessRejection(err); got != tc.want {
			t.Errorf("%s: kind=%v IsBusinessRejection=%v want %v", tc.name, kind, got, tc.want)
		}
	}
}

// TestIsBusinessRejectionViaClient401 端到端：失效 token 打真实形状的 401 响应，
// 必须被判成「值得记」而不是静默。
func TestIsBusinessRejectionViaClient401(t *testing.T) {
	html401 := "<html><head><title>401 Authorization Required</title></head>" +
		"<body><center><h1>401 Authorization Required</h1></center></body></html>"
	c := growthTestClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(401, html401), nil
	})
	_, err := c.GrowthStreakState(&auth.Auth{AccessToken: "bad", UID: "u1"})
	if err == nil {
		t.Fatal("want 401 error")
	}
	if IsBusinessRejection(err) {
		t.Errorf("失效 token 的 401 被当成业务拒绝会被静默吞掉，任务将永远不报警: %v", err)
	}
}

// TestMakeupTransportErrorIsNotBusinessRejection 传输层失败绝不能算业务拒绝。
//
// 这是「静默」与「必须报错」的分界线：把网络故障也静默掉，
// 补签就会长期静默失败而无人察觉。
func TestMakeupTransportErrorIsNotBusinessRejection(t *testing.T) {
	c := growthTestClient(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset by peer")
	})
	err := c.UseMakeupCard(&auth.Auth{AccessToken: "at", UID: "u1"}, "2026-09-15")
	if err == nil {
		t.Fatal("want transport error")
	}
	if IsBusinessRejection(err) {
		t.Errorf("传输层错误不应被当作业务拒绝（会被静默吞掉）: %v", err)
	}
	if IsMakeupNoCard(err) || IsMakeupNotBroken(err) || IsMakeupOutOfMonth(err) {
		t.Errorf("传输层错误不应命中任何补签业务判据: %v", err)
	}
}

// TestMakeup5xxIsNotBusinessRejection 5xx 属于「真出问题了」，同样不能静默。
func TestMakeup5xxIsNotBusinessRejection(t *testing.T) {
	c := growthTestClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(503, `{"code":503,"msg":"service unavailable"}`), nil
	})
	err := c.UseMakeupCard(&auth.Auth{AccessToken: "at", UID: "u1"}, "2026-09-15")
	if err == nil {
		t.Fatal("want error")
	}
	if IsBusinessRejection(err) {
		t.Errorf("5xx 不应算业务拒绝: %v", err)
	}
}

// TestGrowthRedeemSendsTierAndFreshToken 兑换请求体：档位 + 非空且每次不同的幂等键。
func TestGrowthRedeemSendsTierAndFreshToken(t *testing.T) {
	var bodies [][]byte
	c := growthTestClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			return nil, errors.New("want POST")
		}
		if err := travelPath(r, "/activity/growth/redeem"); err != nil {
			return nil, err
		}
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, raw)
		return jsonResp(200, `{"code":0,"data":{"credit_granted":0,"energy_granted":2,"cards_granted":1,"chances_granted":1}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	for i := 0; i < 2; i++ {
		res, err := c.GrowthRedeem(a, "7d", "")
		if err != nil {
			t.Fatalf("redeem %d: %v", i, err)
		}
		if res.EnergyGranted != 2 || res.CardsGranted != 1 || res.ChancesGranted != 1 {
			t.Errorf("redeem %d result=%+v", i, res)
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("bodies=%d want 2", len(bodies))
	}
	tokens := make([]string, 0, 2)
	for i, raw := range bodies {
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("body %d parse: %v", i, err)
		}
		if body["tier"] != "7d" {
			t.Errorf("body %d tier=%v want 7d", i, body["tier"])
		}
		tok, _ := body["client_token"].(string)
		if tok == "" {
			t.Errorf("body %d 缺 client_token（幂等键不得为空）: %s", i, raw)
		}
		tokens = append(tokens, tok)
	}
	// 复用幂等键可能被上游按 key 去重吞掉本次领取 —— 必须每次新。
	if tokens[0] == tokens[1] {
		t.Errorf("两次 redeem 的 client_token 相同（%q）—— 会被上游幂等去重吞掉", tokens[0])
	}
}

// TestGrowthRedeemBusinessRejections 兑换的实测业务拒绝。
func TestGrowthRedeemBusinessRejections(t *testing.T) {
	cases := []struct {
		name           string
		status         int
		body           string
		wantNotEnough  bool
		wantAlreadyGot bool
	}{
		{
			name:   "实测：天数不足（403）",
			status: 403, body: `{"code":403,"msg":"连续登录天数不足，请继续打卡或使用补签卡","requestId":"x"}`,
			wantNotEnough: true,
		},
		{
			name:   "实测：未知档位（400）",
			status: 400, body: `{"code":400,"msg":"unknown tier","requestId":"x"}`,
		},
		{
			name:   "幂等态：409 已领取",
			status: 409, body: `{"code":409,"msg":"duplicate: already claimed this tier this month"}`,
			wantAlreadyGot: true,
		},
		{
			name:   "幂等态：408 已领取",
			status: 408, body: `{"code":408,"msg":"already claimed"}`,
			wantAlreadyGot: true,
		},
	}
	for _, tc := range cases {
		c := growthTestClient(func(r *http.Request) (*http.Response, error) {
			return jsonResp(tc.status, tc.body), nil
		})
		_, err := c.GrowthRedeem(&auth.Auth{AccessToken: "at", UID: "u1"}, "7d", "tok-1")
		if err == nil {
			t.Fatalf("%s: want error", tc.name)
		}
		if got := IsRedeemNotEnoughDays(err); got != tc.wantNotEnough {
			t.Errorf("%s: IsRedeemNotEnoughDays=%v want %v", tc.name, got, tc.wantNotEnough)
		}
		if got := IsRedeemAlreadyClaimed(err); got != tc.wantAlreadyGot {
			t.Errorf("%s: IsRedeemAlreadyClaimed=%v want %v", tc.name, got, tc.wantAlreadyGot)
		}
	}
}

// TestGrowthLotteryChances 抽奖次数余额解析（实测 data.balance）。
func TestGrowthLotteryChances(t *testing.T) {
	c := growthTestClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return nil, errors.New("want GET")
		}
		if err := travelPath(r, "/activity/growth/lottery/chances"); err != nil {
			return nil, err
		}
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"balance":0}}`), nil
	})
	n, err := c.GrowthLotteryChances(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("chances: %v", err)
	}
	if n != 0 {
		t.Errorf("balance=%d want 0", n)
	}
}

// TestGrowthLotteryDrawFreshTokenAndResult 抽奖：每次新 token + 奖品字段解析。
func TestGrowthLotteryDrawFreshTokenAndResult(t *testing.T) {
	var bodies [][]byte
	c := growthTestClient(func(r *http.Request) (*http.Response, error) {
		if err := travelPath(r, "/activity/growth/lottery/draw"); err != nil {
			return nil, err
		}
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, raw)
		return jsonResp(200, `{"code":0,"data":{"prize_code":"credit_10","prize_name":"10 积分","prize_type":"credit","credit_amount":10}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	for i := 0; i < 2; i++ {
		res, err := c.GrowthLotteryDraw(a, "")
		if err != nil {
			t.Fatalf("draw %d: %v", i, err)
		}
		if res.PrizeName != "10 积分" || res.CreditAmount != 10 || res.PrizeType != "credit" {
			t.Errorf("draw %d result=%+v", i, res)
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("bodies=%d want 2", len(bodies))
	}
	var t0, t1 string
	_ = json.Unmarshal(bodies[0], &struct{}{})
	var b0, b1 map[string]any
	_ = json.Unmarshal(bodies[0], &b0)
	_ = json.Unmarshal(bodies[1], &b1)
	t0, _ = b0["client_token"].(string)
	t1, _ = b1["client_token"].(string)
	if t0 == "" || t1 == "" {
		t.Fatalf("client_token 不得为空: %q %q", t0, t1)
	}
	// 抽奖对幂等键最敏感：复用会让上游把第二次抽奖当成第一次的重放。
	if t0 == t1 {
		t.Errorf("两次 draw 的 client_token 相同（%q）", t0)
	}
}

// TestGrowthLotteryDrawRejections 实测无次数（400）与抽奖未开启（400）。
func TestGrowthLotteryDrawRejections(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		wantNoChance bool
		wantDisabled bool
	}{
		{
			name:         "实测：无抽奖次数",
			body:         `{"code":400,"msg":"insufficient lottery chance balance","requestId":"x"}`,
			wantNoChance: true,
		},
		{
			name:         "抽奖未开启",
			body:         `{"code":400,"msg":"lottery disabled"}`,
			wantDisabled: true,
		},
	}
	for _, tc := range cases {
		c := growthTestClient(func(r *http.Request) (*http.Response, error) {
			return jsonResp(400, tc.body), nil
		})
		_, err := c.GrowthLotteryDraw(&auth.Auth{AccessToken: "at", UID: "u1"}, "t")
		if err == nil {
			t.Fatalf("%s: want error", tc.name)
		}
		if got := IsLotteryNoChance(err); got != tc.wantNoChance {
			t.Errorf("%s: IsLotteryNoChance=%v want %v", tc.name, got, tc.wantNoChance)
		}
		if got := IsLotteryDisabled(err); got != tc.wantDisabled {
			t.Errorf("%s: IsLotteryDisabled=%v want %v", tc.name, got, tc.wantDisabled)
		}
	}
}

// TestClaimGiftAndCompensationBusinessErrors 礼包/补偿的实测业务错误（均为 400 code=10001）。
//
// 这两个是「每号一次 / 有则领」的幂等写：对已有账号而言业务错误是每天都会发生的常态，
// 必须能被识别为业务拒绝（静默），否则日志会被刷爆。
func TestClaimGiftAndCompensationBusinessErrors(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
	}{
		{
			name: "实测：礼包已领过",
			path: "/billing/meter/claim-gift",
			body: `{"code":10001,"msg":"每人限领一次，您已领取过无法重复领取","requestId":"x"}`,
		},
		{
			name: "实测：国服补偿活动未开启",
			path: "/billing/meter/claim-compensation",
			body: `{"code":10001,"msg":"补偿领取活动未开启或已过期","requestId":"x"}`,
		},
		{
			name: "实测：国际版赠送活动未开启",
			path: "/billing/meter/claim-gift",
			body: `{"code":10001,"msg":"赠送活动未开启或已过期","requestId":"x"}`,
		},
	}
	for _, tc := range cases {
		c := growthTestClient(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != tc.path {
				return nil, errors.New("wrong path: " + r.URL.Path)
			}
			return jsonResp(400, tc.body), nil
		})
		var err error
		if strings.HasSuffix(tc.path, "claim-gift") {
			_, err = c.ClaimGift(&auth.Auth{AccessToken: "at", UID: "u1"})
		} else {
			_, err = c.ClaimCompensation(&auth.Auth{AccessToken: "at", UID: "u1"})
		}
		if err == nil {
			t.Fatalf("%s: want business error", tc.name)
		}
		if !IsBusinessRejection(err) {
			t.Errorf("%s: 应被识别为业务拒绝（否则会刷 WARN）: %v", tc.name, err)
		}
	}
}

// TestClaimGiftSuccessReturnsCredit 成功路径回到账积分。
func TestClaimGiftSuccessReturnsCredit(t *testing.T) {
	c := growthTestClient(func(r *http.Request) (*http.Response, error) {
		if err := travelPath(r, "/billing/meter/claim-gift"); err != nil {
			return nil, err
		}
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"credit":300}}`), nil
	})
	credit, err := c.ClaimGift(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("claim gift: %v", err)
	}
	if credit != 300 {
		t.Errorf("credit=%d want 300", credit)
	}
}

// TestClaimBillingCreditToleratesMissingData 回执缺字段不视为失败（按 0 记）。
func TestClaimBillingCreditToleratesMissingData(t *testing.T) {
	c := growthTestClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":null}`), nil
	})
	credit, err := c.ClaimCompensation(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("claim compensation: %v", err)
	}
	if credit != 0 {
		t.Errorf("credit=%d want 0", credit)
	}
}

// TestGrowthClientTokenUniqueAndPrefixed 幂等键：前缀保留 + 大量生成不碰撞。
//
// 碰撞会让两次不同的领取被判成同一次（少领一份），因此必须实测其唯一性，
// 而不是「相信 crypto/rand」。
func TestGrowthClientTokenUniqueAndPrefixed(t *testing.T) {
	seen := make(map[string]bool, 2000)
	for i := 0; i < 2000; i++ {
		tok := growthClientToken("draw")
		if !strings.HasPrefix(tok, "draw-") {
			t.Fatalf("token=%q 应以 draw- 开头", tok)
		}
		if seen[tok] {
			t.Fatalf("token 碰撞: %q", tok)
		}
		seen[tok] = true
	}
}

// TestGrowthRedemptionStatusClaimed 各档 claimed 判定（含未知档位）。
func TestGrowthRedemptionStatusClaimed(t *testing.T) {
	rs := &GrowthRedemptionStatus{
		Tier7dStatus:  "claimed",
		Tier14dStatus: "locked",
		Tier28dStatus: "available",
	}
	cases := map[string]bool{"7d": true, "14d": false, "28d": false, "99d": false}
	for tier, want := range cases {
		if got := rs.Claimed(tier); got != want {
			t.Errorf("Claimed(%q)=%v want %v", tier, got, want)
		}
	}
}

// TestGrowthEndpointsUseChatBase 五个 growth 端点必须走 chatBase（不是 billingBase）。
//
// 两个域在国服是不同域名：发错域会 404 —— 而 404 在调度侧表现为「静默无事发生」，
// 不会有人发现。故用两个不同 base 把这条约束钉死。
func TestGrowthEndpointsUseChatBase(t *testing.T) {
	paths := []string{
		"/activity/growth/streak",
		"/activity/growth/heatmap",
		"/activity/growth/makeup-cards/use",
		"/activity/growth/redeem",
		"/activity/growth/lottery/chances",
		"/activity/growth/lottery/draw",
	}
	for _, p := range paths {
		var gotHost string
		c := &Client{
			HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
				gotHost = r.URL.Host
				if r.URL.Path != p {
					return nil, errors.New("wrong path: " + r.URL.Path)
				}
				return jsonResp(200, `{"code":0,"data":{}}`), nil
			})},
			ChatBaseCN:    "https://chat.example",
			BillingBaseCN: "https://billing.example",
		}
		a := &auth.Auth{AccessToken: "at", UID: "u1"}
		switch p {
		case "/activity/growth/streak":
			_, _ = c.GrowthStreakState(a)
		case "/activity/growth/heatmap":
			_, _ = c.GrowthHeatmap(a)
		case "/activity/growth/makeup-cards/use":
			_ = c.UseMakeupCard(a, "2026-09-15")
		case "/activity/growth/redeem":
			_, _ = c.GrowthRedeem(a, "7d", "t")
		case "/activity/growth/lottery/chances":
			_, _ = c.GrowthLotteryChances(a)
		case "/activity/growth/lottery/draw":
			_, _ = c.GrowthLotteryDraw(a, "t")
		}
		if gotHost != "chat.example" {
			t.Errorf("%s 应走 chatBase(chat.example)，实际 host=%q", p, gotHost)
		}
	}
}

// TestClaimBenefitsUseBillingBase 礼包/补偿必须走 billingBase。
//
// 实测依据：拿国际版 token 打国服 billing 域会被网关直接 401
// （openresty 授权错误页），因此基址必须随账号区域走。
func TestClaimBenefitsUseBillingBase(t *testing.T) {
	for _, p := range []string{"/billing/meter/claim-gift", "/billing/meter/claim-compensation"} {
		var gotHost string
		c := &Client{
			HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
				gotHost = r.URL.Host
				return jsonResp(200, `{"code":0,"data":{}}`), nil
			})},
			ChatBaseCN:    "https://chat.example",
			BillingBaseCN: "https://billing.example",
		}
		a := &auth.Auth{AccessToken: "at", UID: "u1"}
		if strings.HasSuffix(p, "claim-gift") {
			_, _ = c.ClaimGift(a)
		} else {
			_, _ = c.ClaimCompensation(a)
		}
		if gotHost != "billing.example" {
			t.Errorf("%s 应走 billingBase(billing.example)，实际 host=%q", p, gotHost)
		}
	}
}

// TestClaimBenefitsRouteByRealm 国际版账号走国际版 billing 基址（实测必需）。
func TestClaimBenefitsRouteByRealm(t *testing.T) {
	var gotHost string
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			gotHost = r.URL.Host
			return jsonResp(200, `{"code":0,"data":{}}`), nil
		})},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		BaseIntl:      "https://intl.example",
	}
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.workbuddy.ai"}
	_, _ = c.ClaimGift(a)
	if gotHost != "intl.example" {
		t.Errorf("国际版账号应走 BaseIntl，实际 host=%q", gotHost)
	}
}

// TestGrowthRewardBodiesMatchRealShape 端到端核对请求体形状（防字段名手误）。
func TestGrowthRewardBodiesMatchRealShape(t *testing.T) {
	var sawRedeem, sawDraw bool
	c := growthTestClient(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/activity/growth/redeem":
			sawRedeem = true
			if !bytes.Contains(raw, []byte(`"tier":"14d"`)) {
				t.Errorf("redeem body=%s 应含 tier=14d", raw)
			}
			if !bytes.Contains(raw, []byte(`"client_token"`)) {
				t.Errorf("redeem body=%s 应含 client_token", raw)
			}
		case "/activity/growth/lottery/draw":
			sawDraw = true
			if !bytes.Contains(raw, []byte(`"client_token"`)) {
				t.Errorf("draw body=%s 应含 client_token", raw)
			}
		}
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, _ = c.GrowthRedeem(a, "14d", "")
	_, _ = c.GrowthLotteryDraw(a, "")
	if !sawRedeem || !sawDraw {
		t.Errorf("sawRedeem=%v sawDraw=%v，两个端点都应被调用到", sawRedeem, sawDraw)
	}
}
