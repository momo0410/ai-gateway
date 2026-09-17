package scheduler

import (
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
// 开学季活动任务回归
//
// 设计边界（本文件重点验证）：只领取**已达标**的奖励，不伪造完成动作。
// 伪造学生认证 / 邀请属于刷量，既违反活动规则也可能导致封号。
//
// 活动限时：服务端下发 in_period，下线后 tasks 为空 → 静默跳过、不报错。
// 实测（2026-09-15，19 个真实账号）：国服 14/14 in_period=true（各 5 个任务），
// 国际版 5/5 返回 404 Route Not Found —— 故与签到共用区域开关。
// ---------------------------------------------------------------------------

// schoolRecorder 记录活动接口请求，并可控地返回任务状态。
type schoolRecorder struct {
	mu        sync.Mutex
	claims    []string // 收到 claim 的 task_code
	inPeriod  bool
	tasks     []map[string]any
	tasksFail int // 非 0 时任务清单接口返回该状态码
}

func (r *schoolRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/portal/activity/school/tasks"):
			if r.tasksFail != 0 {
				w.WriteHeader(r.tasksFail)
				_, _ = w.Write([]byte(`{"error_msg":"404 Route Not Found"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			body, _ := json.Marshal(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"in_period": r.inPeriod, "tasks": r.tasks},
			})
			_, _ = w.Write(body)
		case strings.Contains(req.URL.Path, "/portal/activity/school/tasks/") &&
			strings.HasSuffix(req.URL.Path, "/claim"):
			code := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/portal/activity/school/tasks/"), "/claim")
			r.mu.Lock()
			r.claims = append(r.claims, code)
			r.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		default:
			http.Error(w, "unexpected: "+req.URL.Path, 404)
		}
	}
}

func (r *schoolRecorder) claimed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.claims))
	copy(out, r.claims)
	return out
}

func newSchoolScheduler(t *testing.T, rec *schoolRecorder, accounts ...*auth.Auth) *Scheduler {
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
		SchoolHours:       []int{12},
	})
}

// task 构造一个活动任务条目。
func task(code, status string, reward int64) map[string]any {
	return map[string]any{
		"task_code": code, "status": status,
		"progress": 0, "target_count": 1, "reward_credit": reward,
	}
}

// TestSchoolClaimsOnlyFinished 只领取 finished 状态的任务。
//
// pending（未达标）与 claimed（已领取）都必须跳过 ——
// 前者没资格，后者重复领取会被上游拒绝。
func TestSchoolClaimsOnlyFinished(t *testing.T) {
	rec := &schoolRecorder{
		inPeriod: true,
		tasks: []map[string]any{
			task("task_student_verify", "pending", 100),
			task("chat_3_times", "finished", 50),
			task("share_invite", "claimed", 100),
			task("desktop_chat_1_time", "finished", 100),
		},
	}
	s := newSchoolScheduler(t, rec, &auth.Auth{UID: "u1", AccessToken: "tok"})

	s.RunSchoolNow()

	got := rec.claimed()
	want := map[string]bool{"chat_3_times": true, "desktop_chat_1_time": true}
	if len(got) != len(want) {
		t.Fatalf("应领取 %d 个任务，实际 %d 个: %v", len(want), len(got), got)
	}
	for _, c := range got {
		if !want[c] {
			t.Errorf("不应领取 %q（状态非 finished）", c)
		}
	}
}

// TestSchoolDoesNotFakeActions 不伪造需要真实动作的任务。
//
// 这是本功能的设计红线：task_student_verify（学生认证）、share_invite（邀请）、
// expert_use（专家功能）都需要真实动作，伪造属于刷量。
// 本任务只做「把已达标的奖励领回来」，因此这些任务即使在列也不会被 claim
//（除非它们已经是 finished 状态 —— 那是用户自己完成的，领取是正当的）。
func TestSchoolDoesNotFakeActions(t *testing.T) {
	rec := &schoolRecorder{
		inPeriod: true,
		tasks: []map[string]any{
			task("task_student_verify", "pending", 100),
			task("share_invite", "pending", 100),
			task("expert_use", "pending", 50),
		},
	}
	s := newSchoolScheduler(t, rec, &auth.Auth{UID: "u1", AccessToken: "tok"})

	s.RunSchoolNow()

	if got := rec.claimed(); len(got) != 0 {
		t.Errorf("未达标的任务不应被领取（伪造完成属刷量），实际领取了 %v", got)
	}
}

// TestSchoolSkipsWhenNotInPeriod 活动下线时静默跳过，不发 claim 也不报错。
func TestSchoolSkipsWhenNotInPeriod(t *testing.T) {
	rec := &schoolRecorder{inPeriod: false, tasks: nil}
	s := newSchoolScheduler(t, rec, &auth.Auth{UID: "u1", AccessToken: "tok"})

	s.RunSchoolNow()

	if got := rec.claimed(); len(got) != 0 {
		t.Errorf("活动不在期时不应领取，实际 %v", got)
	}
}

// TestSchoolFetchFailureDoesNotClaim 拉清单失败时不得继续 claim。
//
// 失败时拿不到任务状态，此时若盲目 claim 会打出无意义的请求。
func TestSchoolFetchFailureDoesNotClaim(t *testing.T) {
	rec := &schoolRecorder{inPeriod: true, tasksFail: 404}
	s := newSchoolScheduler(t, rec, &auth.Auth{UID: "u1", AccessToken: "tok"})

	s.RunSchoolNow()

	if got := rec.claimed(); len(got) != 0 {
		t.Errorf("拉清单失败时不应 claim，实际 %v", got)
	}
}

// TestSchoolSkipsIntlByDefault 国际版默认跳过（实测该活动国际版 404）。
func TestSchoolSkipsIntlByDefault(t *testing.T) {
	rec := &schoolRecorder{inPeriod: true, tasks: []map[string]any{task("chat_3_times", "finished", 50)}}
	s := newSchoolScheduler(t, rec,
		&auth.Auth{UID: "intl", AccessToken: "tok", Domain: "www.workbuddy.ai"},
	)

	s.RunSchoolNow()

	if got := rec.claimed(); len(got) != 0 {
		t.Errorf("国际版默认应跳过（该活动国际版 404），实际 %v", got)
	}
}

// TestSchoolIncludesIntlWhenScopeAll scope=all 时国际版也参与（活动若未来上线国际版）。
func TestSchoolIncludesIntlWhenScopeAll(t *testing.T) {
	rec := &schoolRecorder{inPeriod: true, tasks: []map[string]any{task("chat_3_times", "finished", 50)}}
	s := newSchoolScheduler(t, rec,
		&auth.Auth{UID: "intl", AccessToken: "tok", Domain: "www.workbuddy.ai"},
	)
	s.cfg.CheckinScope = "all"

	s.RunSchoolNow()

	if got := rec.claimed(); len(got) != 1 {
		t.Errorf("scope=all 时国际版应参与，实际领取 %v", got)
	}
}

// TestSchoolSkipsDisabledAccounts 禁用账号不参与。
func TestSchoolSkipsDisabledAccounts(t *testing.T) {
	rec := &schoolRecorder{inPeriod: true, tasks: []map[string]any{task("chat_3_times", "finished", 50)}}
	s := newSchoolScheduler(t, rec, &auth.Auth{UID: "u1", AccessToken: "tok"})
	s.cfg.Pool.Disable("u1", "测试禁用")

	s.RunSchoolNow()

	if got := rec.claimed(); len(got) != 0 {
		t.Errorf("被禁用的账号不应参与，实际 %v", got)
	}
}

// TestSchoolClaimFailureContinues 单个任务领取失败不影响其余任务。
func TestSchoolClaimFailureContinues(t *testing.T) {
	var claimCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/portal/activity/school/tasks") {
			w.Header().Set("Content-Type", "application/json")
			body, _ := json.Marshal(map[string]any{
				"code": 0, "data": map[string]any{
					"in_period": true,
					"tasks": []map[string]any{
						task("bad_task", "finished", 10),
						task("good_task", "finished", 20),
					},
				},
			})
			_, _ = w.Write(body)
			return
		}
		// 第一个任务失败，第二个成功
		n := claimCount.Add(1)
		if n == 1 {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
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
		SchoolHours:       []int{12},
	})

	s.RunSchoolNow()

	if n := claimCount.Load(); n != 2 {
		t.Errorf("单任务失败不应中断其余任务，应尝试 2 次，实际 %d 次", n)
	}
}

// TestSchoolDisabledNoSchedule 禁用后不进排程、不触发调用。
func TestSchoolDisabledNoSchedule(t *testing.T) {
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
		SchoolDisabled:    true,
		TrialDisabled:     true,
		SchoolHours:       []int{12},
	})

	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("全部禁用时不应有排程，实际 at=%v kinds=%v", at, kinds)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("不应有上游调用，实际 %d 次", n)
	}
}

// TestNextWakeIncludesSchool 排程应包含开学季时点。
func TestNextWakeIncludesSchool(t *testing.T) {
	s := New(Config{
		CheckinDisabled:   true,
		KeepaliveDisabled: true,
		ActivityDisabled:  true,
		NightOwlDisabled:  true,
		SchoolHours:       []int{12},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 11, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 12, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskSchool) {
		t.Errorf("kinds=%v 应含 taskSchool", kinds)
	}
}
