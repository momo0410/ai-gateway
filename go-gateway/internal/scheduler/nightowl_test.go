package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// ---------------------------------------------------------------------------
// 夜猫子任务回归
//
// growth 有个时段敏感任务只在夜猫窗口（23:00–08:00 CST）内计入。
// 白天执行毫无意义（上游不计分），因此：
//   1. 排程放在 01 点（窗口内，且避开 22 点的 token 保活）
//   2. 执行前再判一次窗口 —— 用户可能手工触发，或机器休眠后迟到唤醒
// ---------------------------------------------------------------------------

// nightRecorder 记录夜猫子上报请求。
type nightRecorder struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (r *nightRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/v2/report") {
			http.Error(w, "unexpected: "+req.URL.Path, 404)
			return
		}
		var arr []map[string]any
		if err := json.NewDecoder(req.Body).Decode(&arr); err == nil {
			r.mu.Lock()
			r.bodies = append(r.bodies, arr...)
			r.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}
}

func (r *nightRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func newNightScheduler(t *testing.T, rec *nightRecorder, accounts ...*auth.Auth) *Scheduler {
	t.Helper()
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	for _, a := range accounts {
		p.Add(a)
	}
	return New(Config{
		Pool:              p,
		Upstream:          &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		CheckinDisabled:   true,
		KeepaliveDisabled: true,
		ActivityDisabled:  true,
		NightOwlHours:     []int{1},
	})
}

// TestNightWindowBoundaries 窗口边界：23:00 起、08:00 止（左闭右开）。
//
// 直接测**真实函数** withinNightWindowAt（而不是在测试里重抄一遍判定逻辑）——
// 后者会在实现改错时依然通过，是假测试。
func TestNightWindowBoundaries(t *testing.T) {
	cases := []struct {
		hour int
		want bool
	}{
		{22, false}, // 窗口前
		{23, true},  // 窗口起点（含）
		{0, true},   // 午夜
		{1, true},   // 默认排程点
		{7, true},   // 窗口末尾前一小时
		{8, false},  // 窗口终点（不含）
		{12, false}, // 白天
	}
	for _, c := range cases {
		// 构造一个 CST 时刻：先取当天 00:00 CST，再加 c.hour 小时
		cstMidnight := time.Date(cstNow().Year(), cstNow().Month(), cstNow().Day(), 0, 0, 0, 0, time.UTC)
		at := cstMidnight.Add(time.Duration(c.hour) * time.Hour)
		if got := withinNightWindowAt(at); got != c.want {
			t.Errorf("CST %02d:00 → %v want %v", c.hour, got, c.want)
		}
	}
}

// TestCstNowIsUtcPlus8 CST 时刻应为 UTC+8（不依赖系统时区）。
func TestCstNowIsUtcPlus8(t *testing.T) {
	utc := time.Now().UTC()
	cst := cstNow()
	diff := cst.Sub(utc)
	// 允许 1 秒误差（两次取时间之间）
	if diff < 8*time.Hour-time.Second || diff > 8*time.Hour+time.Second {
		t.Errorf("cstNow 与 UTC 相差 %v，应约 8 小时", diff)
	}
}

// TestNightOwlReportsOutsideWindowSkips 窗口外不发任何请求。
//
// 这是本任务的核心防护：白天执行只会白打上游（不计分）。
func TestNightOwlReportsOutsideWindowSkips(t *testing.T) {
	if withinNightWindow() {
		t.Skip("当前正处夜猫窗口内，无法测「窗口外跳过」")
	}
	rec := &nightRecorder{}
	s := newNightScheduler(t, rec, &auth.Auth{UID: "u1", AccessToken: "tok"})

	s.RunNightOwlNow()

	if n := rec.count(); n != 0 {
		t.Errorf("窗口外不应上报，实际 %d 条", n)
	}
}

// TestNightOwlReportsInsideWindow 窗口内正常上报。
func TestNightOwlReportsInsideWindow(t *testing.T) {
	if !withinNightWindow() {
		t.Skip("当前不在夜猫窗口内，无法测「窗口内上报」")
	}
	rec := &nightRecorder{}
	s := newNightScheduler(t, rec, &auth.Auth{UID: "u1", AccessToken: "tok"})

	s.RunNightOwlNow()

	if n := rec.count(); n != 1 {
		t.Fatalf("窗口内应上报 1 条，实际 %d 条", n)
	}
	ev := rec.bodies[0]
	if ev["userId"] != "u1" {
		t.Errorf("事件必须带 userId，实际 %v", ev["userId"])
	}
	if ev["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode 应为 chat_request_send，实际 %v", ev["eventCode"])
	}
}

// TestRunNightOwlDirectCallRespectsWindow 直接调 runNightOwl（带 ctx）同样判窗口。
func TestRunNightOwlDirectCallRespectsWindow(t *testing.T) {
	rec := &nightRecorder{}
	s := newNightScheduler(t, rec, &auth.Auth{UID: "u1", AccessToken: "tok"})

	s.runNightOwl(context.Background())

	inWindow := withinNightWindow()
	got := rec.count()
	want := 0
	if inWindow {
		want = 1
	}
	if got != want {
		t.Errorf("窗口内=%v 时应上报 %d 条，实际 %d 条", inWindow, want, got)
	}
}

// TestNightOwlSkipsDisabledAccounts 禁用账号不参与。
func TestNightOwlSkipsDisabledAccounts(t *testing.T) {
	if !withinNightWindow() {
		t.Skip("需在夜猫窗口内运行")
	}
	rec := &nightRecorder{}
	s := newNightScheduler(t, rec,
		&auth.Auth{UID: "u1", AccessToken: "tok"},
		&auth.Auth{UID: "u2", AccessToken: "tok"},
	)
	s.cfg.Pool.Disable("u2", "测试禁用")

	s.RunNightOwlNow()

	for _, ev := range rec.bodies {
		if ev["userId"] == "u2" {
			t.Error("被禁用的账号不应参与夜猫子任务")
		}
	}
}

// TestNightOwlSkipsIntlByDefault 默认只跑国服（与活跃上报同一区域开关）。
func TestNightOwlSkipsIntlByDefault(t *testing.T) {
	if !withinNightWindow() {
		t.Skip("需在夜猫窗口内运行")
	}
	rec := &nightRecorder{}
	s := newNightScheduler(t, rec,
		&auth.Auth{UID: "cn", AccessToken: "tok", Domain: "copilot.tencent.com"},
		&auth.Auth{UID: "intl", AccessToken: "tok", Domain: "www.workbuddy.ai"},
	)

	s.RunNightOwlNow()

	seen := map[string]bool{}
	for _, ev := range rec.bodies {
		seen[ev["userId"].(string)] = true
	}
	if !seen["cn"] {
		t.Error("国服账号应参与")
	}
	if seen["intl"] {
		t.Error("国际版默认应跳过（growth 域在国际版不可用）")
	}
}

// TestNightOwlDisabledNoSchedule 禁用后不进排程、不触发调用。
func TestNightOwlDisabledNoSchedule(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no call expected", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok"})
	s := New(Config{
		Pool:              p,
		Upstream:          &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL},
		CheckinDisabled:   true,
		KeepaliveDisabled: true,
		ActivityDisabled:  true,
		NightOwlDisabled:  true,
		SchoolDisabled:  true,
		TrialDisabled:  true,
		NightOwlHours:     []int{1},
	})

	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("全部禁用时不应有排程，实际 at=%v kinds=%v", at, kinds)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("不应有上游调用，实际 %d 次", n)
	}
}

// TestNextWakeIncludesNightOwl 排程应包含夜猫子时点。
func TestNextWakeIncludesNightOwl(t *testing.T) {
	s := New(Config{
		CheckinDisabled:   true,
		KeepaliveDisabled: true,
		ActivityDisabled:  true,
		NightOwlHours:     []int{1},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 0, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 1, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskNightOwl) {
		t.Errorf("kinds=%v 应含 taskNightOwl", kinds)
	}
}
