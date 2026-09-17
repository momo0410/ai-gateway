// Package prompt 提供网关自有系统提示词：内置默认 + 文件覆盖，并负责出站改写。
//
// 背景（为什么需要这个包）：客户端（Claude Code / Codex 等 CLI）会在 system
// 提示里注入固定模板句，上游内容审核按**逐字精确匹配**拦截，合法流量被误杀。
// 做法是网关在出站前用自有系统提示词**整体替换**客户端的 system/developer 消息，
// 从源头消灭 system 来源的指纹误报。
//
// 与 internal/upstream/sanitize.go 的关系：两层叠加、互不替代 ——
// sanitize 负责清洗 user/assistant 消息里的指纹串，本包负责 system/developer
// 消息的整体替换。用户明确配了提示词文件时，sanitize 仍照常工作。
package prompt

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// defaultPrompt 内置默认提示词（custom 模式下未指定 file 时使用）。
//
// 用 go:embed 而不是写成 Go 字符串常量：提示词是**文本资产**，独立成 .md
// 文件才能让非 Go 作者（如使用者本人）直接读改、diff 与审阅，也避免在 Go
// 源码里堆多行字符串字面量。
//
//go:embed defaultprompt.md
var defaultPrompt string

// Degraded 降级提示词：刻意极简中性，供「误报重试」场景使用。
//
// 触发场景：请求被上游内容策略拦截、且判定为 system 来源的**指纹误报**时，
// 换这段最小提示词重试一次，绕开误报而不改变用户指令的合法性语义。
//
// 注意：本仓库的 upstream.Classify 目前**没有**内容拦截这一分类
// （无实测样本可依据），因此该常量只是「能力就绪」，尚未接入任何自动重试
// 分支 —— 接入需要先拿到可复现的拦截样本，否则就是在猜上游行为。
const Degraded = "You are a helpful assistant. Respond in the user's language, follow the user's instructions, and be direct and concise."

// 模式常量：供 config 校验与 handler 判定共用，避免同一个字符串在两侧各写一遍。
const (
	// ModePassthrough 透传客户端原始 system（**缺省**，既有行为不变）。
	ModePassthrough = "passthrough"
	// ModeCustom 用网关自有提示词替换客户端的 system/developer 消息。
	ModeCustom = "custom"
)

// Default 返回内置默认提示词文本。
//
// 单独暴露（而不是让调用方用 Load("")）是为了让「内置默认」可被显式读取：
// 测试与运维都想确认当前内置文本到底是哪一版。
func Default() string { return defaultPrompt }

// Load 按 file 加载系统提示词文本。
//   - file 非空 → 读该文件；不存在 / 读失败 / 内容为空 → 返回 error，调用方 fail fast；
//   - file 空   → 返回内置默认 defaultPrompt。
//
// 为什么不像参考实现那样把 mode 也传进来：mode 决定「用不用」这段文本，属于
// 路由语义，由调用方（config.normalizePrompt / handler）判断；Load 只负责
// 「拿到一段提示词文本」。多传一个恒不被使用的参数，只会让签名谎称它关心 mode。
//
// 为什么读失败要 fail fast（而不是静默回落内置默认）：用户**明确**配了文件，
// 说明他要的就是那份提示词；静默换成别的文本会表现为「配了却像没配」——
// 现象与原因隔了十万八千里，极难排查。启动即报错能立刻暴露路径写错/文件缺失。
//
// 为什么内容为空也算错误：空提示词会让 Rewrite 变成空操作（systemPrompt 为空
// 时它原样返回），最终等价于 passthrough —— 用户以为在用自有提示词，实际
// 客户端的 system 一条没动。这同样是「静默用错」，故与读失败同等对待。
func Load(file string) (string, error) {
	if strings.TrimSpace(file) == "" {
		return defaultPrompt, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("prompt.file %s: %w", file, err)
	}
	text := string(raw)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("prompt.file %s: 文件内容为空（空提示词等价于不改写，"+
			"请写入提示词文本，或把 prompt.mode 改回 passthrough）", file)
	}
	// 按**原文**返回（不 TrimSpace）：提示词里可能有意保留的缩进/换行结构，
	// 只把「是否为空」交给 TrimSpace 判断。
	return text, nil
}

// Rewrite 把请求体的系统提示词整体替换为 systemPrompt：
//   - 删除 messages 中所有 role 为 system / developer 的消息；
//   - 在 messages **头部插入**一条 {"role":"system","content":systemPrompt}；
//   - messages 之外的所有字段逐字不动，保留下来的 user/assistant/tool 消息也逐字不动。
//
// 解析失败 → 原样返回（绝不失败）：Rewrite 位于转发关键路径上，任何解析错误
// 都不该阻塞请求，让上游按其原始语义处理即可 —— 宁可少做一次改写，
// 也不能把请求体改坏或让请求失败。
func Rewrite(body []byte, systemPrompt string) []byte {
	// 空提示词 = 不改写：插入一条空 system 只会污染请求，语义上也无意义。
	if len(body) == 0 || strings.TrimSpace(systemPrompt) == "" {
		return body
	}

	// 顶层用 map[string]json.RawMessage 而不是 map[string]any：
	// 后者会把所有字段解码成 any 再重新编码（数字被转成 float64 可能丢精度、
	// 字段顺序被打乱），而本次改写**只**该动 messages。用 RawMessage 让
	// 其余字段（model / stream / tools / metadata …）原样字节透传，
	// 与同包 rewriteModel 的既有做法一致。
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return body
	}
	// 字面量 null 能「成功」解析进 map 类型的变量、却留下 nil map，
	// 下面 doc["messages"] = ... 会 panic（assignment to entry in nil map）。
	// Rewrite 在转发关键路径上，一次 panic 就是一次请求失败（甚至是进程级
	// 崩溃，取决于 recovery），因此必须显式挡住：非对象 body 一律原样返回。
	if doc == nil {
		return body
	}

	// 新 system 消息交给标准库序列化：提示词里可能有引号、换行、反斜杠，
	// 手工拼 JSON 会拼出非法转义。
	sysMsg, err := json.Marshal(map[string]string{"role": "system", "content": systemPrompt})
	if err != nil {
		return body
	}

	rawMsgs, hasMsgs := doc["messages"]
	var msgs []json.RawMessage
	// messages 缺席、为 null、或不是数组（对象/字符串）时，msgs 保持为 nil，
	// 下面直接产出「单条 system」，其余字段照常保留。
	if hasMsgs {
		_ = json.Unmarshal(rawMsgs, &msgs)
	}

	kept := make([]json.RawMessage, 0, len(msgs)+1)
	kept = append(kept, sysMsg) // 头部插入
	for _, m := range msgs {
		if isSystemLike(m) {
			continue // 旧 system/developer 整体丢弃
		}
		kept = append(kept, m)
	}

	encoded, err := json.Marshal(kept)
	if err != nil {
		return body
	}
	doc["messages"] = encoded
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

// isSystemLike 判断一条消息的 role 是否为 system / developer。
//
// 比较用「去空白 + 大小写不敏感」，与 upstream.normalizeRoles 处理 developer
// 的口径保持一致：客户端拼写变体（"System"、"system "）不该让旧提示词漏删 ——
// 漏删意味着客户端指纹留在请求里，正好绕过本功能的目的。
//
// 非法 JSON 或非对象的消息一律**不**当 system（保守保留）：宁可多留一条，
// 也不要误删用户的实际内容。
func isSystemLike(raw json.RawMessage) bool {
	var probe struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(probe.Role)) {
	case "system", "developer":
		return true
	}
	return false
}
