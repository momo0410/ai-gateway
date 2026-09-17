package scheduler

import (
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
// trial 加油包领取回归
//
// 为什么需要：国际版没有签到/任务中心，trial 是其唯一积分增益动作。
//
// 幂等语义是设计核心：上游用业务码 14051 表达「已领过」，
// 客户端必须判定为**幂等成功**而非错误 —— 否则已领过的账号会被反复重试并刷错误日志。
// 实测（2026-09-15，5 个真实国际版账号）全部返回 14051。
// ---------------------------------------------------------------------------

// trialRecorder 记录 trial 端点请求，可控地返回「已领过」。
type trialRecorder struct {
	mu      sync.Mutex
	calls   int
	already bool // true = 返回 14051（已领过）
	fail    int  // 非 0 时返回该状态码
}

func (r *trialRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/billing/ide/trial") {
			http.Error(w, "unexpected: "+req.URL.Path, 404)
			return
		}
		r.mu.Lock()
		r.calls++
		r.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.fail != 0:
			w.WriteHeader(r.fail)
			_, _ = w.Write([]byte(`{"code":` + itoa(r.fail) + `,"msg":"boom"}`))
		case r.already:
			// 已领过：HTTP 200 + 业务码 14051
			_, _ = w.Write([]byte(`{"code":14051,"msg":"already claimed"}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		}
	}
}

func (r *trialRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func newTrialScheduler(t *testing.T, rec *trialRecorder, accounts ...*auth.Auth) *Scheduler {
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
		NightOwlDisabled:  true,
		SchoolDisabled:    true,
		TrialHours:        []int{9, 21},
	})
}

// TestTrialClaimsForIntlAccount 国际版账号会调 trial 端点。
func TestTrialClaimsForIntlAccount(t *testing.T) {
	rec := &trialRecorder{}
	s := newTrialScheduler(t, rec, &auth.Auth{
		UID: "intl", AccessToken: "tok", Domain: "www.workbuddy.ai",
	})

	s.RunTrialNow()

	if n := rec.count(); n != 1 {
		t.Errorf("国际版账号应调 1 次 trial 端点，实际 %d 次", n)
	}
}

// TestTrialSkipsCNAccounts 国服账号不调该端点（国服无此接口）。
//
// 这是区域过滤**与其它任务相反**的地方：其它任务默认只跑国服，
// 而 trial 只跑国际版。
func TestTrialSkipsCNAccounts(t *testing.T) {
	rec := &trialRecorder{}
	s := newTrialScheduler(t, rec,
		&auth.Auth{UID: "cn", AccessToken: "tok", Domain: "copilot.tencent.com"},
	)

	s.RunTrialNow()

	if n := rec.count(); n != 0 {
		t.Errorf("国服账号不应调 trial 端点（国服无此接口），实际 %d 次", n)
	}
}

// TestTrialAlreadyClaimedIsNotError 已领过（14051）视为幂等成功，不报错。
//
// 这是本功能最关键的一条：若把 14051 当失败，
// 每天重试都会刷错误日志，且可能触发熔断/冷却等副作用。
func TestTrialAlreadyClaimedIsNotError(t *testing.T) {
	rec := &trialRecorder{already: true}
	s := newTrialScheduler(t, rec, &auth.Auth{
		UID: "intl", AccessToken: "tok", Domain: "www.workbuddy.ai",
	})

	// 直接验证客户端语义
	claimed, err := s.cfg.Upstream.ClaimTrial(&auth.Auth{
		UID: "intl", AccessToken: "tok", Domain: "www.workbuddy.ai",
	})
	if err != nil {
		t.Fatalf("已领过（14051）应视为幂等成功，实际报错: %v", err)
	}
	if claimed {
		t.Error("已领过时 claimed 应为 false（表示本次没有新领）")
	}

	// 遍历也不应中断
	s.RunTrialNow()
	if n := rec.count(); n == 0 {
		t.Error("应实际调用过端点")
	}
}

// TestTrialRejectsCNInClient 客户端侧也有防线：国服账号直接报错，不发请求。
func TestTrialRejectsCNInClient(t *testing.T) {
	rec := &trialRecorder{}
	s := newTrialScheduler(t, rec)

	_, err := s.cfg.Upstream.ClaimTrial(&auth.Auth{
		UID: "cn", AccessToken: "tok", Domain: "copilot.tencent.com",
	})
	if err == nil {
		t.Error("国服账号应直接报错（客户端侧防线）")
	}
	if n := rec.count(); n != 0 {
		t.Errorf("应在发请求前就拒绝，实际发出 %d 次请求", n)
	}
}

// TestTrialSkipsDisabledAccounts 禁用账号不参与。
func TestTrialSkipsDisabledAccounts(t *testing.T) {
	rec := &trialRecorder{}
	s := newTrialScheduler(t, rec, &auth.Auth{
		UID: "intl", AccessToken: "tok", Domain: "www.workbuddy.ai",
	})
	s.cfg.Pool.Disable("intl", "测试禁用")

	s.RunTrialNow()

	if n := rec.count(); n != 0 {
		t.Errorf("被禁用的账号不应参与，实际 %d 次调用", n)
	}
}

// TestTrialFailureDoesNotStopOthers 单账号失败不影响其余账号。
func TestTrialFailureDoesNotStopOthers(t *testing.T) {
	rec := &trialRecorder{fail: 500}
	s := newTrialScheduler(t, rec,
		&auth.Auth{UID: "i1", AccessToken: "tok", Domain: "www.workbuddy.ai"},
		&auth.Auth{UID: "i2", AccessToken: "tok", Domain: "www.workbuddy.ai"},
		&auth.Auth{UID: "i3", AccessToken: "tok", Domain: "www.workbuddy.ai"},
	)

	s.RunTrialNow()

	if n := rec.count(); n != 3 {
		t.Errorf("单账号失败不应中断遍历，应尝试 3 次，实际 %d 次", n)
	}
}

// TestTrialDisabledNoSchedule 禁用后不进排程、不触发调用。
func TestTrialDisabledNoSchedule(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no call expected", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "i1", AccessToken: "tok", Domain: "www.workbuddy.ai"})
	s := New(Config{
		Pool:              p,
		Upstream:          &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		CheckinDisabled:   true,
		KeepaliveDisabled: true,
		ActivityDisabled:  true,
		NightOwlDisabled:  true,
		SchoolDisabled:    true,
		TrialDisabled:     true,
		TrialHours:        []int{9, 21},
	})

	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("全部禁用时不应有排程，实际 at=%v kinds=%v", at, kinds)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("不应有上游调用，实际 %d 次", n)
	}
}

// TestNextWakeIncludesTrial 排程应包含 trial 时点。
func TestNextWakeIncludesTrial(t *testing.T) {
	s := New(Config{
		CheckinDisabled:   true,
		KeepaliveDisabled: true,
		ActivityDisabled:  true,
		NightOwlDisabled:  true,
		SchoolDisabled:    true,
		TrialHours:        []int{9},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 8, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskTrial) {
		t.Errorf("kinds=%v 应含 taskTrial", kinds)
	}
}

// TestTrialAlreadyMarkersCoversBothForms 幂等码的两种 Msg 形态都要识别。
//
// doJSON 在不同分支下拼出的 Msg 格式不同：
//   - HTTP 200 + 业务码非 0 → "code=14051 msg=..."
//   - HTTP ≥400 → 原始 JSON body，形如 `{"code":14051,...}`
//
// 只认一种会导致另一种被当成失败。这里通过真实 HTTP 响应验证
// （而非直接调内部函数，因为判定函数在 upstream 包内、不导出）。
func TestTrialAlreadyMarkersCoversBothForms(t *testing.T) {
	// 形态一：HTTP 200 + 业务码 14051（已在 TestTrialAlreadyClaimedIsNotError 覆盖）
	// 形态二：HTTP 400 + 原始 body 里带 14051
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"HTTP200+业务码", 200, `{"code":14051,"msg":"already claimed"}`},
		{"HTTP400+原始body", 400, `{"code":14051,"msg":"already claimed"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if c.status != 200 {
					w.WriteHeader(c.status)
				}
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL}
			claimed, err := up.ClaimTrial(&auth.Auth{
				UID: "intl", AccessToken: "tok", Domain: "www.workbuddy.ai",
			})
			if err != nil {
				t.Errorf("14051 应视为幂等成功，实际报错: %v", err)
			}
			if claimed {
				t.Error("已领过时 claimed 应为 false")
			}
		})
	}

	// 反例：其它错误码不应被当成「已领过」
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
	}))
	defer srv.Close()
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL}
	if _, err := up.ClaimTrial(&auth.Auth{
		UID: "intl", AccessToken: "tok", Domain: "www.workbuddy.ai",
	}); err == nil {
		t.Error("非 14051 的错误应如实返回，不应被误判为「已领过」")
	}
}
