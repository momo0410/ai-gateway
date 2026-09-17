package server

import "strings"

// model_allow.go 实现「限制使用的模型」白名单（多选）。
//
// 语义演进：此前是**单值** `AllowedModel`，且仅在「单一模型 + 积分轮转」模式下
// 生效；现在升级为**多值白名单 + 三个工作模式都有**，默认（空）不限制。
//
// 三条硬约束，任何一条破了都是**破坏性变更**：
//
//  1. 空 = 不限制。老配置升级后行为必须逐字不变 —— 老配置里根本可能没有这个键，
//     把它读成「一个模型都不放行」会让网关对所有请求返回 400，且用户完全无从
//     判断是自己配错了还是新版本坏了。
//  2. 大小写不敏感。客户端写模型名的大小写并不统一（实测同时出现过
//     `DeepSeek-V4.1-Flash` 与 `deepseek-v4.1-flash`），逐字节比较会误拒。
//  3. 比较前剥掉区域前缀 `cn:` / `global:`。前缀只是给网关的**选号指令**，
//     上游不认识它，转发前也会被 rewriteModel 抹掉；因此「配的是裸名、
//     客户端带前缀请求」必须视为同一个模型。实测注释（forward.go）里
//     记着这条：用户配 `deepseek-v4.1-flash`，客户端可能带 `cn:` 前缀请求。

// normalizeAllowedModels 归一化白名单：去空白、剥区域前缀、丢弃空项。
//
// 刻意**不去重**：列表只有几个元素，去重带来的复杂度（且要额外分配）换不来
// 任何可观测收益；重复项在匹配时只是多一次 EqualFold。
//
// 刻意**不做大小写折叠**：折叠要额外分配一份新切片，而比较侧用
// strings.EqualFold 已经是零分配的 —— 归一化只负责「形状」，大小写交给比较。
func normalizeAllowedModels(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		// 与请求侧同样先剥前缀：用户既可能在界面上选到裸名，也可能手写带前缀的名。
		// resolveModel 的返回顺序是 (realm, bare)，写反会把区域当成模型名。
		_, bare := resolveModel(strings.TrimSpace(item))
		if bare = strings.TrimSpace(bare); bare != "" {
			out = append(out, bare)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// modelAllowed 判断请求的**裸模型名**是否在白名单内；白名单为空 = 全部放行。
//
// 调用方必须传剥过前缀的裸名（forward.go 里已是如此）—— 这里再兜一次
// resolveModel，是为了让「有人直接从别处调这个函数」也不会因带前缀而误拒。
func modelAllowed(allowed []string, model string) bool {
	if len(allowed) == 0 {
		return true
	}
	_, bare := resolveModel(model)
	for _, item := range allowed {
		if strings.EqualFold(bare, item) {
			return true
		}
	}
	return false
}

// allowedModelsText 把白名单拼成给用户看的一行文案（顿号分隔）。
//
// 错误信息必须**列出全部**允许的模型：只报「不允许 x」在用户配了 3 个模型时
// 完全没法排查 —— 他不知道该改成哪一个，只能挨个试。
func allowedModelsText(allowed []string) string {
	return strings.Join(allowed, "、")
}
