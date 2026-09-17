package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// ---------------------------------------------------------------------------
// 区域（region）过滤回归
//
// 场景：同一个模型名在两个区域可能是**不同的后端模型**，能力并不一致
//（实测：国服 glm-5.3 能读图，国际版同名模型读不到，会把图片换成占位符）。
// 不指定区域时选号是随机的，于是「同一个 glm-5.3」会时好时坏 ——
// 用户看到的是模型不稳定，实际是命中了两个不同后端。
//
// 因此两个入口语义不同，都要有回归保护：
//   - PickForModelRegion       区域**偏好**（更好但非必须，选不出则回退）
//   - PickForModelRegionStrict 区域**强制**（只在该区域才对，选不出返回 nil）
//
// 注：上游目前对这两个入口**没有测试覆盖**，本文件补上。
// ---------------------------------------------------------------------------

// realmPool 构造一个含国服 + 国际版账号的池。
func realmPool(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	p.Add(&auth.Auth{
		UID: "cn-1", AccessToken: "t", Domain: "copilot.tencent.com",
		SoonestExpireAt: time.Now().Add(24 * time.Hour).Unix(),
	})
	p.Add(&auth.Auth{
		UID: "intl-1", AccessToken: "t", Domain: "www.workbuddy.ai",
		SoonestExpireAt: time.Now().Add(24 * time.Hour).Unix(),
	})
	return p
}

// TestAuthRealmDetection 账号区域判定（依据登录域名后缀）。
//
// Realm() 是 Region() 的字符串表示（前缀协议与前端展示用字符串），
// 两者委托同一 IsIntl() 判定 —— 本用例同时钉住这两条路径，
// 防止二者判定分叉（分叉会让「前缀选号」与「图片路由」得出不同区域）。
func TestAuthRealmDetection(t *testing.T) {
	cases := []struct {
		domain string
		want   string
	}{
		{"www.workbuddy.ai", auth.RealmGlobal},
		{"workbuddy.ai", auth.RealmGlobal},
		{"WWW.WORKBUDDY.AI", auth.RealmGlobal}, // 大小写不敏感
		{"copilot.tencent.com", auth.RealmCN},
		{"www.codebuddy.cn", auth.RealmCN},
		{"", auth.RealmCN},
	}
	for _, c := range cases {
		a := &auth.Auth{Domain: c.domain}
		if got := a.Realm(); got != c.want {
			t.Errorf("Domain=%q Realm()=%q want %q", c.domain, got, c.want)
		}
		wantRegion := auth.RegionCN
		if c.want == auth.RealmGlobal {
			wantRegion = auth.RegionIntl
		}
		if got := a.Region(); got != wantRegion {
			t.Errorf("Domain=%q Region()=%v want %v", c.domain, got, wantRegion)
		}
	}
	// nil 安全
	var nilAuth *auth.Auth
	if nilAuth.Realm() != auth.RealmCN {
		t.Error("nil 账号应视为国服（且不 panic）")
	}
}

// TestPickForModelRegionPrefersIntl 指定国际版偏好时只在国际版里挑。
func TestPickForModelRegionPrefersIntl(t *testing.T) {
	p := realmPool(t)
	// 多试几次：选号含随机性，必须每次都命中正确区域
	for i := 0; i < 30; i++ {
		got := p.PickForModelRegion("gpt-6-astra", nil, auth.RegionIntl)
		if got == nil {
			t.Fatal("应选出国际版账号")
		}
		if got.Realm() != auth.RealmGlobal {
			t.Fatalf("偏好 intl 却选出 %s（domain=%s）", got.UID, got.Domain)
		}
	}
}

// TestPickForModelRegionPrefersCN 指定国服偏好时只在国服里挑。
func TestPickForModelRegionPrefersCN(t *testing.T) {
	p := realmPool(t)
	for i := 0; i < 30; i++ {
		got := p.PickForModelRegion("deepseek-v4-flash", nil, auth.RegionCN)
		if got == nil {
			t.Fatal("应选出国服账号")
		}
		if got.Realm() != auth.RealmCN {
			t.Fatalf("偏好 cn 却选出 %s（domain=%s）", got.UID, got.Domain)
		}
	}
}

// TestPickForModelRegionAnyUnrestricted RegionAny = 不限制（保持既有行为）。
func TestPickForModelRegionAnyUnrestricted(t *testing.T) {
	p := realmPool(t)
	seen := map[string]bool{}
	for i := 0; i < 60; i++ {
		got := p.PickForModelRegion("glm-5.2", nil, auth.RegionAny)
		if got == nil {
			t.Fatal("RegionAny 应能选出账号")
		}
		seen[got.UID] = true
	}
	// 不限制时应两个区域都可能被选中（分层同档 + 加权随机）
	if len(seen) < 2 {
		t.Errorf("RegionAny 应不限制区域，但只选中了 %v", seen)
	}
}

// TestPickForModelRegionStrictNoMatchReturnsNil 强制版：区域无可用账号时返回 nil。
//
// 这是刻意的设计：**不回退**到其它区域。回退不是「降级可用」，
// 而是「静默地做错事」—— 带图请求跨区后图片被换成占位符，
// 模型回「我看不见图片」，用户完全无从判断是网络、模型还是网关的问题。
func TestPickForModelRegionStrictNoMatchReturnsNil(t *testing.T) {
	p := New("")
	// 池里只有国服账号
	p.Add(&auth.Auth{
		UID: "cn-1", AccessToken: "t", Domain: "copilot.tencent.com",
		SoonestExpireAt: time.Now().Add(24 * time.Hour).Unix(),
	})

	if got := p.PickForModelRegionStrict("gpt-6-astra", nil, auth.RegionIntl); got != nil {
		t.Errorf("强制 intl 但池里只有国服账号，应返回 nil，实际 %s", got.UID)
	}
	// 对照：偏好版此时会回退 —— 这是它与强制版的**语义差异**，不是 bug。
	// 两个断言成对存在，防止有人把两者实现成同一个（那会让带图请求静默跨区）。
	if got := p.PickForModelRegion("gpt-6-astra", nil, auth.RegionIntl); got == nil {
		t.Error("偏好版在目标区域无号时应回退到其它区域，而非返回 nil")
	}
	// 对照：不指定区域时能选出来
	if got := p.PickForModelRegion("glm-5.2", nil, auth.RegionAny); got == nil {
		t.Error("不指定区域时应能选出账号")
	}
}

// TestPickForModelRegionRotation 轮转模式下同样尊重区域偏好。
func TestPickForModelRegionRotation(t *testing.T) {
	p := realmPool(t)
	p.SetRotation(true)
	defer p.SetRotation(false)

	for i := 0; i < 10; i++ {
		got := p.PickForModelRegion("gpt-6-astra", nil, auth.RegionIntl)
		if got == nil {
			t.Fatal("轮转模式下应选出国际版账号")
		}
		if got.Realm() != auth.RealmGlobal {
			t.Fatalf("轮转模式偏好 intl 却选出 %s（domain=%s）", got.UID, got.Domain)
		}
	}
}

// TestPickForModelStillWorks 既有入口（无区域参数）行为不变。
func TestPickForModelStillWorks(t *testing.T) {
	p := realmPool(t)
	if got := p.PickForModel("glm-5.2", nil); got == nil {
		t.Error("PickForModel 应保持可用（等价于不限制区域）")
	}
}
