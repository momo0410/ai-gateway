package server

import (
	"testing"
)

// ---------------------------------------------------------------------------
// 模型名前缀协议回归：`[realm:]model`
//
// 为什么需要：同一个模型名在两个区域可能是不同的服务 ——
// gpt-6-astra 只在国际版存在，deepseek-v4-flash 只在国服存在。
// 账号池默认按「最早到期」分层选号，可能选中另一区域的账号，
// 上游于是返回 11102 model service info not found。
// 客户端用前缀显式指定区域即可避免这个歧义。
// ---------------------------------------------------------------------------

// TestResolveModelPrefix 前缀解析：只认精确的小写枚举。
func TestResolveModelPrefix(t *testing.T) {
	cases := []struct {
		in       string
		wantReal string
		wantBare string
	}{
		{"cn:glm-5.2", "cn", "glm-5.2"},
		{"global:gpt-5.4", "global", "gpt-5.4"},
		// 无前缀 = 不限制区域（保持既有行为）
		{"glm-5.2", "", "glm-5.2"},
		// 前段不在枚举内：不剥离（模型名本身可能含冒号）
		{"deepseek:v3", "", "deepseek:v3"},
		// 大小写敏感：不做归一
		{"GLOBAL:gpt-5", "", "GLOBAL:gpt-5"},
		{"Cn:glm", "", "Cn:glm"},
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.wantReal || bare != c.wantBare {
			t.Errorf("resolveModel(%q)=(%q,%q) want (%q,%q)",
				c.in, realm, bare, c.wantReal, c.wantBare)
		}
	}
}

// TestResolveModelEdge 边界：空串、仅冒号、空前缀、空裸名。
func TestResolveModelEdge(t *testing.T) {
	cases := []struct {
		in       string
		wantReal string
		wantBare string
	}{
		{"", "", ""},
		{":", "", ":"},           // 空前缀不匹配枚举
		{":model", "", ":model"}, // 同上
		{"cn:", "cn", ""},        // 前缀合法 + 空裸名仍剥离
		{"global:", "global", ""},
		{"cn:a:b", "cn", "a:b"}, // 只取第一个冒号
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.wantReal || bare != c.wantBare {
			t.Errorf("resolveModel(%q)=(%q,%q) want (%q,%q)",
				c.in, realm, bare, c.wantReal, c.wantBare)
		}
	}
}

// TestResolveModelReturnsRealmFirst 锁住返回顺序。
//
// 这是真实踩过的坑：写成 `model, realm := resolveModel(...)` 会静默交换两者，
// 表现为「单一模型锁定」报「收到的是 (未指定)」—— 因为 model 变成了空串。
// 该缺陷不会导致编译错误，只能靠测试或线上现象发现。
func TestResolveModelReturnsRealmFirst(t *testing.T) {
	first, second := resolveModel("cn:glm-5.2")
	if first != "cn" {
		t.Errorf("第一个返回值应是 realm（cn），实际 %q —— 调用方若写成 (model, realm) 会静默出错", first)
	}
	if second != "glm-5.2" {
		t.Errorf("第二个返回值应是裸模型名（glm-5.2），实际 %q", second)
	}
}
