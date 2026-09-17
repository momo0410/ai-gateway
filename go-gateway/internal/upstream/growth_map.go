// growth_map.go growth 域「活跃地图」读取与补签：热力格 / 补签卡 / 连登状态快照。
//
// 这是「活跃地图」功能族的地基：一个只读快照 + 一个补签写。兑换与抽奖见 growth_reward.go。
//
// 字段口径全部来自 2026-09-16 的真实账号实测（21 个号，国服 14 / 国际版 7），
// 而非推测。实测 data 信封形状（GET /activity/growth/streak，200）：
//
//	{"streak":{"days":2,"month_total_days":8,"month_consumed_days":0,
//	           "next_tier":"7d","next_tier_remaining":4,"makeup_dates":[]},
//	 "makeup_cards":{"balance":0,"max":4},
//	 "redemption_status":{"tier_7d_count":0,"tier_7d_status":"locked",
//	                      "remaining_days":3,
//	                      "tiers":[{"tier":"7d","days":7,"credit":0,"energy":2,"cards":1,"chances":1}, ...]},
//	 "timezone":"Asia/Shanghai","launch_date":"2026-06-17"}
//
// 关键：makeup_cards 与 redemption_status 是 **data 下的同级切片**（不是嵌在 streak 里），
// 因此一次 GET 就能同时供「补签卡余额」「连登天数」「各档兑换状态」三处判据使用 ——
// 不为这三件事分别打上游。
//
// GET /activity/growth/heatmap 实测：data.cells 为**定长 365 格**的连续自然日
// （start_date..end_date，实测 2025-09-17..2026-09-16），每格 {date,score,has_new_buddy}。
// 因为区间是连续且定长的，区间之外的日子会**整格缺席** —— 这正是「无该日格 = 无判据」
// 这一分支的现实来源，而不是空想出来的边界。
package upstream

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// 活跃地图端点（实测）。
const (
	heatmapPath       = "/activity/growth/heatmap"
	makeupCardUsePath = "/activity/growth/makeup-cards/use"
)

// growthZone 上游自然日口径：CST（Asia/Shanghai）固定 +8。
//
// 不依赖容器 tzdata：中国自 1991 年起不再使用夏令时，固定偏移与真实 CST 等价，
// 而 LoadLocation("Asia/Shanghai") 在缺 tzdata 的 Windows/精简镜像上会直接失败。
// 与 scheduler.cstZone、upstream.upstreamZone 同一口径（三处独立定义是包依赖方向所致）。
var growthZone = time.FixedZone("CST", 8*60*60)

// GrowthCST 把时刻换算到上游自然日口径（CST）。
//
// 导出是为了让调度侧判断「今天是本月第几天」这类日历条件时，
// 不必自己再抄一份时区常量（抄错会静默偏 8 小时，且只在月初/月末显形）。
func GrowthCST(t time.Time) time.Time { return t.In(growthZone) }

// GrowthDay 返回 t 所属上游自然日（CST），格式 2006-01-02。
func GrowthDay(t time.Time) string { return GrowthCST(t).Format("2006-01-02") }

// GrowthYesterday 返回 t 的前一个上游自然日（CST）。
//
// 补签判据固定盯「昨日」：连登断档只可能发生在刚刚结束的那个自然日
// （今日尚未结算，今天的 score 还是 0 也不代表漏签）。
func GrowthYesterday(t time.Time) string { return GrowthCST(t).AddDate(0, 0, -1).Format("2006-01-02") }

// GrowthMakeupOutOfMonth 报告 now 的「昨日」是否已跨出当前自然月。
//
// 为什么需要这条判据：实测补签**只允许补当月**（见 IsMakeupOutOfMonth）。
// 于是每月 1 号（CST）的昨日必然落在上个月，补签请求必然被拒。
// 调用方据此直接跳过，省掉一次注定失败的写请求 —— 写请求比读请求更该省。
func GrowthMakeupOutOfMonth(now time.Time) bool { return GrowthCST(now).Day() == 1 }

// GrowthMakeupCards 补签卡余额（streak 响应的 makeup_cards 段）。
type GrowthMakeupCards struct {
	Balance int `json:"balance"` // 可用补签卡数
	Max     int `json:"max"`     // 持有上限
}

// GrowthTierSpec 连登奖励档位配置（redemption_status.tiers 数组元素）。
//
// 实测档位：7d/14d/28d，分别送 {energy,cards,chances} 与 credit（7d 档 credit 为 0）。
type GrowthTierSpec struct {
	Tier    string `json:"tier"`    // "7d"|"14d"|"28d"
	Days    int    `json:"days"`    // 达标所需连登天数（本档门槛）
	Credit  int    `json:"credit"`  // 送积分
	Energy  int    `json:"energy"`  // 送能量
	Cards   int    `json:"cards"`   // 送补签卡
	Chances int    `json:"chances"` // 送抽奖次数
}

// GrowthRedemptionStatus 连登奖励兑换状态（redemption_status）。
// 实测档位状态取值：locked（未达标）/ claimed（本月已领）；参考实现另记录 available（可领）。
type GrowthRedemptionStatus struct {
	Tier7dStatus  string           `json:"tier_7d_status"`
	Tier14dStatus string           `json:"tier_14d_status"`
	Tier28dStatus string           `json:"tier_28d_status"`
	Tiers         []GrowthTierSpec `json:"tiers"`
	// RemainingDays 上游自报的「还差几天」。
	//
	// 刻意**不用它做达标判据**：实测它与 tiers[].days 不自洽 ——
	// 同一账号 days=2 时 next_tier_remaining=4、remaining_days=3，而 7-2=5；
	// 不同账号（global days=2）又给出 5。既然它连「距 7d 还差几天」都不等于 7-days，
	// 就不能拿它当门槛。达标一律用 tiers[].days 与 streak.days 直接比较（见 scheduler.growthEligibleTier）。
	RemainingDays int `json:"remaining_days"`
}

// Claimed 报告指定档位本月是否已领（status=="claimed"）。
func (r *GrowthRedemptionStatus) Claimed(tier string) bool {
	switch tier {
	case "7d":
		return r.Tier7dStatus == "claimed"
	case "14d":
		return r.Tier14dStatus == "claimed"
	case "28d":
		return r.Tier28dStatus == "claimed"
	}
	return false
}

// GrowthStreakState 连登状态快照：一次 GET /activity/growth/streak 读完全部三个切片。
//
// 为什么把三者合成一个类型：补签要看卡、兑换要看天数与档位状态，而它们本来就在
// 同一个响应体里。拆成三个方法等于对同一个端点打三次上游。
type GrowthStreakState struct {
	Streak struct {
		Days           int `json:"days"`
		MonthTotalDays int `json:"month_total_days"`
	} `json:"streak"`
	MakeupCards GrowthMakeupCards      `json:"makeup_cards"`
	Redemption  GrowthRedemptionStatus `json:"redemption_status"`
}

// Days 连登天数（data.streak.days）。
func (s *GrowthStreakState) Days() int {
	if s == nil {
		return 0
	}
	return s.Streak.Days
}

// GrowthStreakState 读取连登状态快照（天数 + 补签卡余额 + 各档兑换状态）。
func (c *Client) GrowthStreakState(a *auth.Auth) (*GrowthStreakState, error) {
	data, err := c.growthJSON(a, http.MethodGet, streakPath, nil)
	if err != nil {
		return nil, err
	}
	var st GrowthStreakState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// HeatmapCell 活跃地图热力格（一日一格）。
type HeatmapCell struct {
	Date  string `json:"date"`  // 2006-01-02
	Score int    `json:"score"` // 当日活跃计分（0 = 漏签）
}

// GrowthHeatmap 读取活跃地图热力格（GET /activity/growth/heatmap）。
func (c *Client) GrowthHeatmap(a *auth.Auth) ([]HeatmapCell, error) {
	data, err := c.growthJSON(a, http.MethodGet, heatmapPath, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Cells []HeatmapCell `json:"cells"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return resp.Cells, nil
}

// HeatmapDayScore 返回 cells 中 date 当日的 score。
//
// ok=false 表示**没有该日格**，调用方必须据此判「无判据」而不是「漏签」：
// 活跃地图是定长 365 格窗口，窗口外的日子天然缺席，
// 把缺席当成 score==0 会导致对窗口外日期发起必然失败的补签。
func HeatmapDayScore(cells []HeatmapCell, date string) (score int, ok bool) {
	for _, c := range cells {
		// 只比前 10 位：上游给的是纯日期，但万一加上时间后缀也不应失配。
		if len(c.Date) >= 10 && c.Date[:10] == date {
			return c.Score, true
		}
	}
	return 0, false
}

// UseMakeupCard 对指定日期使用补签卡（target_date 格式 2006-01-02，CST 自然日）。
//
// 业务拒绝都是常态，调用方一律静默（实测三种，见各自的判定函数）：
//   - 403 no makeup card balance          → 无卡
//   - 400 date is not broken, no makeup needed → 该日无漏签
//   - 400 only current month makeup allowed    → 跨月（只允许补当月）
func (c *Client) UseMakeupCard(a *auth.Auth, targetDate string) error {
	_, err := c.growthJSON(a, http.MethodPost, makeupCardUsePath,
		map[string]any{"target_date": targetDate})
	return err
}

// IsMakeupNoCard 报告是否「没有补签卡余额」（HTTP 403）。
// 属正常态：卡是连登奖励发的，新号/刚用完的号本来就没有。
func IsMakeupNoCard(err error) bool {
	return isErrMarker(err, http.StatusForbidden, "no makeup card balance")
}

// IsMakeupNotBroken 报告是否「该日并未漏签」（HTTP 400）。
// 属正常态：说明这一天其实已经打过卡，无需补。
func IsMakeupNotBroken(err error) bool {
	return isErrMarker(err, http.StatusBadRequest, "date is not broken", "no makeup needed")
}

// IsMakeupOutOfMonth 报告是否「跨月补签被拒」（HTTP 400）。
// 属正常态：上游只允许补当月，调用方本应在月初就跳过（见 GrowthMakeupOutOfMonth）。
func IsMakeupOutOfMonth(err error) bool {
	return isErrMarker(err, http.StatusBadRequest, "only current month makeup allowed")
}

// IsBusinessRejection 报告该错误是否为「上游就业务规则给出的明确答复」。
//
// 用途是决定**该不该刷 WARN**：礼包已领过、补偿不在期、无补签卡、天数不足、
// 没有抽奖次数 —— 这些对老账号来说是每天的常态，占绝大多数；
// 把它们当错误打出来，会把真正需要关注的失败淹没在噪音里。
//
// 判据用**本项目既有的错误分类**（Classify 的 ErrKind）而不是「4xx 一律算」：
//
//	ErrClient（400/403 等业务错误）  → 业务答复，静默
//	ErrSessionDead（401 + 12153）    → 登录态失效，必须记（需重新登录）
//	ErrNotFound（404）               → 端点不存在，必须记（接口可能已下线/改址）
//	ErrSoftRate / ErrModelRate（429）→ 被限流，必须记（不是业务规则问题）
//	ErrServer / 传输层失败           → 必须记
//
// 为什么不能简单按「4xx = 业务拒绝」判：**实测踩过这个坑** ——
// 拿失效 token 调 growth 端点时，网关会返回一个 openresty 的 **401 HTML 错误页**
// （内容里没有 12153，因此 Classify 判成 ErrClient）。按 4xx 一刀切会把它静默掉，
// 表现为「账号 token 已死但本任务永远不吭声」，正是最该被发现的故障。
// 故这里额外按状态码排除 401：无论 body 长什么样，401 都是认证问题而非业务答复。
func IsBusinessRejection(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if !errors.As(err, &ue) {
		return false // 传输层/解析层失败没有 HTTP 状态，不是上游的业务答复
	}
	if ue.Status == http.StatusUnauthorized {
		return false // 401 一律视为认证问题（见上面的实测说明）
	}
	return ue.Kind == ErrClient
}

// isErrMarker 判定 err 是否 *Error 且 HTTP 状态码相符、文案命中任一关键词（大小写不敏感）。
// 传输层/解析层错误返回 false —— 它们没有 HTTP 状态，不是上游的业务答复。
func isErrMarker(err error, status int, markers ...string) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Status != status {
		return false
	}
	return containsAnyFold(ue.Msg, markers)
}

// containsAnyFold 报告 s 是否包含 markers 中任一子串（ASCII 大小写不敏感）。
//
// 用 ToLower 双边比较而不是 EqualFold：这里做的是**子串**匹配（上游文案里
// 关键词只是其中一段），而 EqualFold 只判整串相等，套不上。
// 中文关键词不受 ToLower 影响，仍然逐字命中。
func containsAnyFold(s string, markers []string) bool {
	lower := strings.ToLower(s)
	for _, m := range markers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}
