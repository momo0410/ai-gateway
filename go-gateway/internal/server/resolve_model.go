package server

import "strings"

// resolveModel 解析模型名里的区域前缀协议：`[realm:]model`。
//
// 协议（大小写敏感）：
//
//	"cn:glm-5.2"      → realm=cn,     bare=glm-5.2
//	"global:gpt-5.4"  → realm=global, bare=gpt-5.4
//	"glm-5.2"         → realm="",     bare=glm-5.2（无前缀 = 不限制区域）
//	"deepseek:v3"     → realm="",     bare=deepseek:v3（前段不在枚举内，不剥离）
//	"GLOBAL:gpt-5"    → realm="",     bare=GLOBAL:gpt-5（大小写敏感，不做归一）
//
// 为什么需要它：同一个模型名在两个区域可能是不同的服务 ——
// gpt-6-astra 只在国际版存在，deepseek-v4-flash 只在国服存在。
// 账号池默认按「最早到期」分层选号，可能选中另一区域的账号，
// 上游于是返回 11102 model service info not found。
// 客户端用前缀显式指定区域即可避免这个歧义。
//
// 为什么无前缀时返回空串而非 "cn"：空串表示**不限制**，
// 保持既有行为（老客户端不带前缀，不应因此改变选号范围）。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "", model
	}
	prefix := model[:idx]
	if prefix != realmCN && prefix != realmGlobal {
		return "", model
	}
	return prefix, model[idx+1:]
}

// realm 枚举（与 pool / auth 包保持一致）。
const (
	realmCN     = "cn"
	realmGlobal = "global"
)
