// growth_reward.go growth 域「连登档位兑换 + 抽奖」与 billing 域「新手礼包 / 活动补偿」。
//
// 端点与语义全部经 2026-09-16 真实账号实测确认（21 个号：国服 14 / 国际版 7）：
//
//	POST /activity/growth/redeem   {"tier":"7d","client_token":"<uuid>"}
//	     days 不足 → 403 {"code":403,"msg":"连续登录天数不足，请继续打卡或使用补签卡"}
//	     未知档位 → 400 {"code":400,"msg":"unknown tier"}
//	POST /activity/growth/lottery/draw  {"client_token":"<uuid>"}
//	     无次数   → 400 {"code":400,"msg":"insufficient lottery chance balance"}
//	GET  /activity/growth/lottery/chances → data.balance
//	POST /billing/meter/claim-gift         → 400 code=10001「每人限领一次，您已领取过无法重复领取」
//	POST /billing/meter/claim-compensation → 400 code=10001「补偿领取活动未开启或已过期」
//
// 兑换档位是**里程碑**语义（7d/14d/28d，同月每档各可领一次），不是按天领。
// 全部走 growthJSON（chatBase + BillingHeaders，与 travel.go 同域同信封）；
// billing 两个领取走 billingJSON（billingBase）。
package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"workbuddy2api/internal/auth"
)

// 连登兑换 / 抽奖 / 礼包路径（实测）。
const (
	redeemPath            = "/activity/growth/redeem"
	lotteryChancesPath    = "/activity/growth/lottery/chances"
	lotteryDrawPath       = "/activity/growth/lottery/draw"
	claimGiftPath         = "/billing/meter/claim-gift"
	claimCompensationPath = "/billing/meter/claim-compensation"
)

// GrowthRedeemResult 兑换回执（成功态 data）。
type GrowthRedeemResult struct {
	CardsGranted   int `json:"cards_granted"`
	CardsOverflow  int `json:"cards_overflow"`
	CreditGranted  int `json:"credit_granted"`
	EnergyGranted  int `json:"energy_granted"`
	ChancesGranted int `json:"chances_granted"`
}

// GrowthRedeem 兑换连登奖励档位（tier 取 7d/14d/28d）。
//
// clientToken 是幂等键，**每次调用都必须新**：复用旧键可能被上游按幂等键去重吞掉本次领取
// （表现为「返回成功但没到账」）。空串时自动生成，调用方无需关心。
//
// 业务拒绝（天数不足 403 / 本月已领 408/409 / 未知档位 400）都是常态，
// 调用方按 IsRedeemNotEnoughDays / IsRedeemAlreadyClaimed 静默跳过。
func (c *Client) GrowthRedeem(a *auth.Auth, tier, clientToken string) (*GrowthRedeemResult, error) {
	if clientToken == "" {
		clientToken = growthClientToken("redeem-" + tier)
	}
	data, err := c.growthJSON(a, http.MethodPost, redeemPath,
		map[string]any{"tier": tier, "client_token": clientToken})
	if err != nil {
		return nil, err
	}
	var res GrowthRedeemResult
	if len(data) > 0 {
		// 回执字段缺失不视为失败：调用方按 0 记日志即可。
		_ = json.Unmarshal(data, &res)
	}
	return &res, nil
}

// GrowthLotteryChances 查询抽奖次数余额（GET lottery/chances → data.balance）。
// 0 = 无次数（连登奖励未送或已抽完），是正常态、无副作用。
func (c *Client) GrowthLotteryChances(a *auth.Auth) (int, error) {
	data, err := c.growthJSON(a, http.MethodGet, lotteryChancesPath, nil)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Balance int `json:"balance"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, err
	}
	return resp.Balance, nil
}

// GrowthLotteryDrawResult 单次抽奖结果（成功态 data）。
// prize_type credit=积分 / physical=实物（实物需人工填收货地址，本项目不代填）。
type GrowthLotteryDrawResult struct {
	PrizeCode    string `json:"prize_code"`
	PrizeName    string `json:"prize_name"`
	PrizeType    string `json:"prize_type"`
	CreditAmount int    `json:"credit_amount"`
}

// GrowthLotteryDraw 抽一次奖。
//
// client_token 每次必须新（与 redeem 同理，且抽奖直接决定中奖归属，
// 复用幂等键可能让上游把第二次抽奖当成第一次的重放而丢弃）。
func (c *Client) GrowthLotteryDraw(a *auth.Auth, clientToken string) (*GrowthLotteryDrawResult, error) {
	if clientToken == "" {
		clientToken = growthClientToken("draw")
	}
	data, err := c.growthJSON(a, http.MethodPost, lotteryDrawPath,
		map[string]any{"client_token": clientToken})
	if err != nil {
		return nil, err
	}
	var res GrowthLotteryDrawResult
	if len(data) > 0 {
		_ = json.Unmarshal(data, &res) // 奖品字段缺失不视为失败
	}
	return &res, nil
}

// growthClientToken 生成每次唯一的幂等键 `<prefix>-<32hex>`。
//
// 用 crypto/rand 而非时间戳/计数器：这两个接口的幂等键直接决定「本次领取有没有生效」，
// 可预测或可碰撞的键会让上游把两次不同的领取判成同一次。
// 形态不严格要求 uuid —— 上游只把它当作每次独立的键，不校验格式。
func growthClientToken(prefix string) string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand 实际不可失败；退回纳秒时间戳保证「每次不同」这一唯一要求。
		return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return prefix + "-" + hex.EncodeToString(buf[:])
}

// ClaimGift 领取新手礼包（每号一次）。已领 → 400 code=10001，属常态。
func (c *Client) ClaimGift(a *auth.Auth) (int64, error) {
	return c.claimBillingCredit(a, claimGiftPath)
}

// ClaimCompensation 领取活动补偿（有则领，无则业务错误）。属常态。
func (c *Client) ClaimCompensation(a *auth.Auth) (int64, error) {
	return c.claimBillingCredit(a, claimCompensationPath)
}

// claimBillingCredit billing 域领取类公共实现：POST path → data.credit。
// 回执字段缺失不视为失败（调用方按 0 记日志）。
func (c *Client) claimBillingCredit(a *auth.Auth, path string) (int64, error) {
	data, err := c.billingJSON(a, http.MethodPost, path, map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

// IsRedeemAlreadyClaimed 报告是否「本月已领取该档位」（HTTP 408/409，或文案含已领取）。
//
// 为什么同时收 408 与 409：参考实现记录上游对「重复领取」用过两种状态码，
// 实测本次只观察到 403/400，故这里把两者的文案特征一起登记，
// 上游换码时不至于把幂等态当成错误刷 WARN。
func IsRedeemAlreadyClaimed(err error) bool {
	if isErrMarker(err, http.StatusConflict, "duplicate", "already claimed", "已领取", "重复领取") {
		return true
	}
	return isErrMarker(err, http.StatusRequestTimeout, "duplicate", "already claimed", "已领取", "重复领取")
}

// IsRedeemNotEnoughDays 报告是否「连续登录天数不足」（HTTP 403）。
// 属正常态：连登天数还没到该档门槛（实测 days=2 兑 7d → 403）。
func IsRedeemNotEnoughDays(err error) bool {
	return isErrMarker(err, http.StatusForbidden, "连续登录天数不足", "not enough", "insufficient")
}

// IsLotteryNoChance 报告是否「无抽奖次数」（HTTP 400）。
// 属正常态：chances=0 时抽奖不消耗任何东西（实测 400 insufficient lottery chance balance）。
func IsLotteryNoChance(err error) bool {
	return isErrMarker(err, http.StatusBadRequest, "insufficient lottery chance balance")
}

// IsLotteryDisabled 报告是否「抽奖未开启」（HTTP 400）。正常态。
func IsLotteryDisabled(err error) bool {
	return isErrMarker(err, http.StatusBadRequest, "lottery disabled")
}
