package upstream

// trial.go 国际版专属「一次性 trial 加油包」领取：
// POST {billingBase}/billing/ide/trial。
//
// 为什么需要它：国际版**没有签到、没有任务中心**（那些都是国服独有的活动），
// trial 加油包是国际版唯一天然的积分增益动作。对从未被发放额度的国际版账号
// （实测有账号 TotalCount=0 / Packages=[]）尤其重要。
//
// 幂等：已领过时上游返回业务码 14051，视为**正常**而非错误 ——
// 因此可以放心地每天重试，不需要本地记账。

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
)

// trialPath trial 加油包端点。
const trialPath = "/billing/ide/trial"

// trialAlreadyMarkers 幂等码 14051「已领取过」的两种指纹：
//   - "code=14051"：doJSON 对 HTTP 200 + 业务码非 0 时拼出的 Msg 格式
//   - `"code":14051`：HTTP ≥400 时 doJSON 把原始 JSON body 塞进 Msg
//
// 两种都要认，否则已领过的账号会被当成失败并反复重试。
var trialAlreadyMarkers = []string{"code=14051", `"code":14051`}

// ClaimTrial 领取一次性 trial 加油包。
//
// 仅国际版账号适用（国服无此端点），非国际版直接返回错误 ——
// 调用方应据此跳过，不要对国服账号调用。
//
// 返回 claimed：true = 本次成功新领；false = 已领过（幂等，不算失败）。
func (c *Client) ClaimTrial(a *auth.Auth) (claimed bool, err error) {
	if a == nil || !IsIntl(a) {
		return false, fmt.Errorf("claim trial: only intl accounts")
	}
	_, err = c.billingJSON(a, http.MethodPost, trialPath, nil)
	if err != nil {
		var ue *Error
		if errors.As(err, &ue) && trialAlreadyErr(ue.Msg) {
			return false, nil // 已领过：幂等成功
		}
		return false, err
	}
	return true, nil
}

// trialAlreadyErr 判定错误 Msg 是否携带幂等码 14051（已领取过）。
func trialAlreadyErr(msg string) bool {
	for _, m := range trialAlreadyMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}
