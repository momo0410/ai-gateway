package main

// allowed_model_config_test.go 锁住 `pool.allowed_model` 的**双形状**解析。
//
// 为什么单独一个文件：这个键的历史形状是**字符串**（老配置、老宿主），本轮升级
// 成**数组**（多选）。两种形状都必须能读 —— 老配置解析失败 = 网关直接起不来，
// 是最严重的一类向后兼容事故，因此每个分支都要单独锁一遍，值得独立成篇。

import (
	"os"
	"path/filepath"
	"testing"
)

// loadPoolConfig 写一份只带 pool 块的配置，并按正常路径加载
//（走 Default() → json.Unmarshal → normalize，与生产完全同一条路径）。
func loadPoolConfig(t *testing.T, poolJSON string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"pool":`+poolJSON+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(fp)
}

// TestAllowedModelLegacyStringStillWorks 老配置的单值字符串必须继续工作。
//
// 这是**向后兼容的核心证据**：所有既有的 gateway_config.json 里这个键都是
// 字符串（实测所有者本机的配置就是 `"allowed_model": "deepseek-v4.1-flash"`）。
// 读不出来就是「升级后网关起不来」。
func TestAllowedModelLegacyStringStillWorks(t *testing.T) {
	c, err := loadPoolConfig(t, `{"rotation":true,"allowed_model":"deepseek-v4.1-flash"}`)
	if err != nil {
		t.Fatalf("老配置（单值字符串）必须能解析，实际报错: %v", err)
	}
	if len(c.Pool.AllowedModels) != 1 || c.Pool.AllowedModels[0] != "deepseek-v4.1-flash" {
		t.Errorf("单值字符串应读成单元素名单，实际 %#v", c.Pool.AllowedModels)
	}
}

// TestAllowedModelArrayParsed 新形状：字符串数组（界面多选的落盘形状）。
func TestAllowedModelArrayParsed(t *testing.T) {
	c, err := loadPoolConfig(t, `{"allowed_model":["deepseek-v4.1-flash","glm-5.3","kimi-k3-1"]}`)
	if err != nil {
		t.Fatalf("数组形状必须能解析，实际报错: %v", err)
	}
	want := []string{"deepseek-v4.1-flash", "glm-5.3", "kimi-k3-1"}
	if len(c.Pool.AllowedModels) != len(want) {
		t.Fatalf("名单长度应为 %d，实际 %#v", len(want), c.Pool.AllowedModels)
	}
	for i := range want {
		if c.Pool.AllowedModels[i] != want[i] {
			t.Errorf("名单[%d] 应为 %q，实际 %q", i, want[i], c.Pool.AllowedModels[i])
		}
	}
}

// TestAllowedModelEmptyMeansUnrestricted 空值（缺席 / null / 空串 / 空数组）一律 = 不限制。
//
// 硬要求：老配置升级后行为不能变。「键缺席」是最常见的一种 —— 老配置里根本
// 没有这个键，被读成「一个模型都不放行」会让网关对所有请求返回 400，
// 而用户完全无从判断是自己配错了还是新版本坏了。
//
// 注意 `["", "  "]`（数组里全是空白）**不在**本用例里：解析层的契约是
// 「只判断形状、逐字保留元素」（见下面的 Verbatim 用例），把空白元素清成空
// 名单是 server 包 normalizeAllowedModels 的职责 —— 那一层有对应用例
//（model_lock_test.go 的 TestWhitelistEmptyMeansUnrestricted/全是空白元素）。
// 两层各测各的契约，比在这一层替对方断言更不容易失真。
func TestAllowedModelEmptyMeansUnrestricted(t *testing.T) {
	cases := []struct {
		name     string
		poolJSON string
	}{
		{"键缺席", `{"rotation":false}`},
		{"null", `{"allowed_model":null}`},
		{"空串", `{"allowed_model":""}`},
		{"纯空白串", `{"allowed_model":"   "}`},
		{"空数组", `{"allowed_model":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := loadPoolConfig(t, c.poolJSON)
			if err != nil {
				t.Fatalf("必须能解析，实际报错: %v", err)
			}
			if len(cfg.Pool.AllowedModels) != 0 {
				t.Errorf("空值应解析为空名单（= 不限制），实际 %#v", cfg.Pool.AllowedModels)
			}
		})
	}
}

// TestAllowedModelIgnoresRotation 白名单与 rotation 开关**解耦**。
//
// 本轮的核心变更之一：限制模型不再只在积分轮转模式下生效。
// 若解析层把非轮转的名单丢掉，界面上「自动模式下限定了 3 个模型」会在
// 保存后静默消失 —— 用户看到自己勾的选择没了，却不知道为什么。
func TestAllowedModelIgnoresRotation(t *testing.T) {
	for _, rotation := range []string{"false", "true"} {
		t.Run("rotation="+rotation, func(t *testing.T) {
			c, err := loadPoolConfig(t,
				`{"rotation":`+rotation+`,"allowed_model":["glm-5.3","kimi-k3-1"]}`)
			if err != nil {
				t.Fatalf("必须能解析，实际报错: %v", err)
			}
			if len(c.Pool.AllowedModels) != 2 {
				t.Errorf("名单不应随 rotation 变化，rotation=%s 时实际 %#v",
					rotation, c.Pool.AllowedModels)
			}
		})
	}
}

// TestAllowedModelWrongTypeFailsFast 数字 / 对象等非法形状必须报错，不静默当成不限制。
//
// 为什么不能静默：用户把 `allowed_model` 写成 123 时，若被当成「不限制」，
// 他会以为限制生效了、实际网关放行一切 —— 安全方向的静默失败，比启动失败更糟。
func TestAllowedModelWrongTypeFailsFast(t *testing.T) {
	for _, poolJSON := range []string{`{"allowed_model":123}`, `{"allowed_model":{"a":1}}`} {
		if _, err := loadPoolConfig(t, poolJSON); err == nil {
			t.Errorf("%s 应报类型错误（不能静默当成不限制）", poolJSON)
		}
	}
}

// TestAllowedModelsUnmarshalKeepsElementsVerbatim 解析层不做过多的归一化。
//
// 归一化（剥前缀 / 去空白 / 丢空项）统一在 server 包的 normalizeAllowedModels
// 里做。两边各写一套必然分叉 —— 这里锁住「解析层只负责形状」这个分工，
// 免得后来者顺手在这里再加一份 trim。
func TestAllowedModelsUnmarshalKeepsElementsVerbatim(t *testing.T) {
	c, err := loadPoolConfig(t, `{"allowed_model":[" glm-5.3 ","cn:kimi-k3-1"]}`)
	if err != nil {
		t.Fatalf("必须能解析，实际报错: %v", err)
	}
	if len(c.Pool.AllowedModels) != 2 {
		t.Fatalf("应保留 2 项，实际 %#v", c.Pool.AllowedModels)
	}
	if c.Pool.AllowedModels[0] != " glm-5.3 " {
		t.Errorf("解析层不该做 trim（归一化统一在 server 包），实际 %q", c.Pool.AllowedModels[0])
	}
	if c.Pool.AllowedModels[1] != "cn:kimi-k3-1" {
		t.Errorf("解析层不该剥前缀（归一化统一在 server 包），实际 %q", c.Pool.AllowedModels[1])
	}
}

// TestDefaultHasNoAllowedModels 默认配置不带任何限制。
//
// 「默认是全部」这条需求在配置层的落点：Default() 里绝不能有非空的
// allowed_model，否则全新安装的用户一启动就被限制。
func TestDefaultHasNoAllowedModels(t *testing.T) {
	if got := Default().Pool.AllowedModels; len(got) != 0 {
		t.Errorf("默认配置不应限制任何模型，实际 %#v", got)
	}
}
