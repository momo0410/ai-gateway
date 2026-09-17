package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// ---------------------------------------------------------------------------
// 连续 12153 计数回归
//
// 缺陷背景：旧行为「一次 ErrSessionDead 即永久禁用」。但 12153 会被
// 网络抖动 / 上游闪断 / refresh 竞态**临时性**触发，一次失败就杀号会误杀
// 健康账号 —— 实测发现一批 disabled 账号其实 refresh 完全正常。
//
// 现改为连续 sessionDeadThreshold 次才禁用，且任何证明账号未死的时刻清零。
// ---------------------------------------------------------------------------

func newSessionDeadPool(t *testing.T, uid string) *Pool {
	t.Helper()
	p := New("")
	p.Add(&auth.Auth{UID: uid, AccessToken: "tok", RefreshToken: "rt"})
	return p
}

// TestNoteSessionDeadNotDisabledBeforeThreshold 未达阈值不禁用，只累计。
func TestNoteSessionDeadNotDisabledBeforeThreshold(t *testing.T) {
	p := newSessionDeadPool(t, "u1")
	n := SessionDeadThreshold()
	if n < 2 {
		t.Fatalf("阈值应 >=2 才有「连续」语义，实际 %d", n)
	}

	for i := 1; i < n; i++ {
		if disabled := p.NoteSessionDead("u1"); disabled {
			t.Fatalf("第 %d 次（阈值 %d）不应返回已禁用", i, n)
		}
		st, _ := p.Status("u1")
		if st.Disabled {
			t.Fatalf("第 %d 次不应禁用账号（偶发失败不该杀号）", i)
		}
		if got := p.SessionDeadFails("u1"); got != i {
			t.Errorf("第 %d 次后计数应为 %d，实际 %d", i, i, got)
		}
	}
}

// TestNoteSessionDeadDisablesAtThreshold 达阈值即禁用并清零计数。
func TestNoteSessionDeadDisablesAtThreshold(t *testing.T) {
	p := newSessionDeadPool(t, "u1")
	n := SessionDeadThreshold()

	for i := 1; i < n; i++ {
		p.NoteSessionDead("u1")
	}
	if disabled := p.NoteSessionDead("u1"); !disabled {
		t.Fatalf("连续 %d 次后应返回已禁用", n)
	}

	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Error("达阈值后账号应被禁用")
	}
	if got := p.SessionDeadFails("u1"); got != 0 {
		t.Errorf("禁用后计数应清零，实际 %d", got)
	}
}

// TestClearSessionDead 清零计数且不禁用账号。
func TestClearSessionDead(t *testing.T) {
	p := newSessionDeadPool(t, "u1")
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	if p.SessionDeadFails("u1") == 0 {
		t.Fatal("前置条件：应有计数")
	}

	p.ClearSessionDead("u1")
	if got := p.SessionDeadFails("u1"); got != 0 {
		t.Errorf("清零后应为 0，实际 %d", got)
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Error("清零不应禁用账号")
	}
}

// TestNoteSuccessClearsSessionDead chat 成功即清零（误判防护复活路径）。
func TestNoteSuccessClearsSessionDead(t *testing.T) {
	p := newSessionDeadPool(t, "u1")
	p.NoteSessionDead("u1")
	if p.SessionDeadFails("u1") == 0 {
		t.Fatal("前置条件：应有计数")
	}
	p.NoteSuccess("u1")
	if got := p.SessionDeadFails("u1"); got != 0 {
		t.Errorf("成功后应清零，实际 %d", got)
	}
}

// TestNoteSessionDeadUnknownUID 未知 uid 为空操作，不 panic。
func TestNoteSessionDeadUnknownUID(t *testing.T) {
	p := newSessionDeadPool(t, "u1")
	if disabled := p.NoteSessionDead("nope"); disabled {
		t.Error("未知 uid 应返回 false")
	}
	if got := p.SessionDeadFails("nope"); got != 0 {
		t.Errorf("未知 uid 计数应为 0，实际 %d", got)
	}
	p.ClearSessionDead("nope") // 不应 panic
}

// TestSessionDeadCounterSurvivesPartialSuccess 计数是**连续**语义：
// 中途一次成功即清零，重新计数。
func TestSessionDeadCounterSurvivesPartialSuccess(t *testing.T) {
	p := newSessionDeadPool(t, "u1")
	n := SessionDeadThreshold()

	// 累计 n-1 次
	for i := 1; i < n; i++ {
		p.NoteSessionDead("u1")
	}
	// 一次成功 → 清零
	p.NoteSuccess("u1")
	if got := p.SessionDeadFails("u1"); got != 0 {
		t.Fatalf("成功后应清零，实际 %d", got)
	}
	// 再累计 n-1 次仍不该禁用（说明确实按连续计，不是累计总数）
	for i := 1; i < n; i++ {
		if p.NoteSessionDead("u1") {
			t.Fatalf("清零后重新累计第 %d 次不应禁用（应为连续语义）", i)
		}
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Error("清零后重新累计未达阈值，不应禁用")
	}
}
