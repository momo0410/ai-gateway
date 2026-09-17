package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 现场数据（2026-09-15 实测场景，14 个账号）。
//
// 账号标识已脱敏为合成 id：本仓库是公开仓库，真实 uid 不应入库。
// credits / expire_at 保留原值 —— 它们才是测试语义所在（档位与排队判定）。
//
// 现场观测：4 个更早档位账号对主力模型 deepseek-v4.1-flash 模型冷却，
// 流量落在 10-11 档的 8 个账号，10-14 档的 2 个排队 —— 正是所有者问的
// 「14 个账号，4 冷却 + 8 均衡，还有两个哪去了」。
var queuedFieldAccounts = []struct {
	uid     string
	credits int64
	expire  int64
}{
	{"acct-a1", 4721, 1790827287}, // 10-01
	{"acct-a2", 7266, 1791248123}, // 10-06
	{"acct-a3", 2231, 1791631663}, // 10-10
	{"acct-a4", 2389, 1791631558}, // 10-10
	{"acct-b1", 2141, 1791695172}, // 10-11
	{"acct-b2", 5524, 1791693914}, // 10-11
	{"acct-b3", 2085, 1791696294}, // 10-11
	{"acct-b4", 2144, 1791688504}, // 10-11
	{"acct-b5", 2158, 1791695539}, // 10-11
	{"acct-b6", 5878, 1791688505}, // 10-11
	{"acct-b7", 2101, 1791695043}, // 10-11
	{"acct-b8", 2097, 1791694658}, // 10-11
	{"acct-c1", 2200, 1791951767}, // 10-14
	{"acct-c2", 1643, 1791952524}, // 10-14
}

func newQueuedFieldPool() *Pool {
	p := New("")
	for _, a := range queuedFieldAccounts {
		p.Add(&auth.Auth{UID: a.uid, SoonestExpireAt: a.expire})
		p.SetCredits(a.uid, a.credits)
	}
	return p
}

// TestQueuedFlagMatchesLiveScenario 是本次改动的核心回归测试。
//
// 它锁定的正是实现里踩过的坑：档位判定若**不排除模型冷却账号**，就会算出
// 「最早档位 = 10-01」，于是把真正在服务的 8 个账号全标成「排队」，
// 还把冷却中的账号标成「会路由」—— 与事实完全相反。
func TestQueuedFlagMatchesLiveScenario(t *testing.T) {
	p := newQueuedFieldPool()
	// 现场：前 4 个对主力模型冷却
	for _, uid := range []string{"acct-a1", "acct-a2", "acct-a3", "acct-a4"} {
		p.CooldownModel(uid, "deepseek-v4.1-flash", time.Now().Add(4*time.Hour), "限流", true)
	}

	now := time.Now()
	p.mu.RLock()
	day := p.routedTierDayLocked(now)
	p.mu.RUnlock()

	// 生效档位必须是 10-11（而不是被冷却账号拉低的 10-01）
	if day != "2026-10-11" {
		t.Fatalf("生效档位 = %q，期望 2026-10-11（被模型冷却的账号不应参与档位判定）", day)
	}

	queued := map[string]bool{}
	for _, st := range p.List() {
		queued[st.UID] = st.Queued
	}

	// 10-11 档 8 个：正在服务，不得标成排队
	for _, uid := range []string{
		"acct-b1", "acct-b2", "acct-b3", "acct-b4",
		"acct-b5", "acct-b6", "acct-b7", "acct-b8",
	} {
		if queued[uid] {
			t.Errorf("%s 属于当前生效档位（10-11），不应标为排队", uid)
		}
	}

	// 10-14 档 2 个：正是所有者问的「少掉的两个」，必须标为排队
	for _, uid := range []string{"acct-c1", "acct-c2"} {
		if !queued[uid] {
			t.Errorf("%s 档位 10-14 晚于生效档位，应标为排队（这是当初困惑的那两个账号）", uid)
		}
	}

	// 被模型冷却的更早档位账号：不是「排队」（它们档位更早），而是被模型冷却挡住
	for _, uid := range []string{"acct-a1", "acct-a2", "acct-a3", "acct-a4"} {
		if queued[uid] {
			t.Errorf("%s 档位早于生效档位，其不可用原因是模型冷却而非排队", uid)
		}
	}
}

// TestQueuedFlagFalseWhenAllInSameTier 全部同档时不应有人被标排队。
func TestQueuedFlagFalseWhenAllInSameTier(t *testing.T) {
	p := New("")
	sameDay := time.Now().Add(72 * time.Hour).Unix()
	for _, uid := range []string{"a", "b", "c"} {
		p.Add(&auth.Auth{UID: uid, SoonestExpireAt: sameDay})
		p.SetCredits(uid, 1000)
	}
	for _, st := range p.List() {
		if st.Queued {
			t.Errorf("%s 全部同档，不应有人排队", st.UID)
		}
	}
}

// TestQueuedFlagIgnoresUnknownExpiry 到期日未知的账号不参与档位判定，也不被标排队。
//
// 未知到期日的账号在 pick 里排最后（只有其它账号都不可用才轮到），
// 但它们没有档位键可比，标"排队"会给出无法解释的信息，故不标。
func TestQueuedFlagIgnoresUnknownExpiry(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "known", SoonestExpireAt: time.Now().Add(48 * time.Hour).Unix()})
	p.Add(&auth.Auth{UID: "unknown"}) // 无 SoonestExpireAt
	p.SetCredits("known", 1000)
	p.SetCredits("unknown", 1000)

	now := time.Now()
	p.mu.RLock()
	day := p.routedTierDayLocked(now)
	p.mu.RUnlock()
	if day == "" {
		t.Fatal("有已知到期日的账号时，生效档位不应为空")
	}
	for _, st := range p.List() {
		if st.UID == "unknown" && st.Queued {
			t.Error("到期日未知的账号不应被标为排队（无档位可比）")
		}
	}
}

// TestQueuedFlagClearedWhenEarlierTierUnavailable 更早档位全部不可用时，
// 生效档位下沉，原先排队的账号应自动不再排队。
func TestQueuedFlagClearedWhenEarlierTierUnavailable(t *testing.T) {
	p := newQueuedFieldPool()

	// 先让 10-11 及更早的账号全部账号级冷却，只剩 10-14 的 2 个可用
	for _, a := range queuedFieldAccounts {
		if a.expire < 1791951767 { // 早于 10-14
			p.Cooldown(a.uid, CoolHard, time.Hour, "测试占用")
		}
	}

	now := time.Now()
	p.mu.RLock()
	day := p.routedTierDayLocked(now)
	p.mu.RUnlock()
	if day != "2026-10-14" {
		t.Fatalf("生效档位 = %q，期望下沉到 2026-10-14", day)
	}
	for _, st := range p.List() {
		if st.UID == "acct-c1" || st.UID == "acct-c2" {
			if st.Queued {
				t.Errorf("%s 现在是最早可用档位，不应再排队", st.UID)
			}
		}
	}
}
