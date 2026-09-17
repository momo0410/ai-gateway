package scheduler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// ---------------------------------------------------------------------------
// 手动触发入口（RunTaskByName）回归
//
// 为什么需要它：这 4 个任务此前只有「按点自动跑」，用户在界面上既看不到
// 执行结果，也没法在改完配置后立刻验证。手动触发是可观测性的一部分。
//
// 关键约定：
//   - 只跑一轮，**不检查 enabled 开关**（开关管后台排程，用户主动点击就该执行）
//   - 未知任务名必须报错，否则界面点了没反应却无从判断
//   - 重入必须被拦住：活跃上报按「账号数 × 条数 × 间隔」串行跑，连点会叠加成
//     成倍重复上报 —— 正是该任务刻意控制条数要规避的风控画像
// ---------------------------------------------------------------------------

// blockingRecorder 第一次收到上报就发出信号并阻塞，用来复现「上一轮还没跑完」。
type blockingRecorder struct {
	entered  chan struct{}
	release  chan struct{}
	started  sync.Once
	mu       sync.Mutex
	received int
}

func (r *blockingRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/v2/report"):
			r.mu.Lock()
			r.received++
			r.mu.Unlock()
			r.started.Do(func() { close(r.entered) })
			<-r.release // 卡住第一轮，制造「仍在执行中」
		case strings.HasSuffix(req.URL.Path, "/activity/growth/streak"):
			// 回读连登：给个正常值，避免走告警分支
		default:
			http.Error(w, "unexpected path: "+req.URL.Path, 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"streak":{"days":1}}}`))
	}
}

func newBlockingScheduler(t *testing.T, rec *blockingRecorder, accounts ...*auth.Auth) *Scheduler {
	t.Helper()
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		BaseIntl:      srv.URL,
	}
	p := pool.New("")
	for _, a := range accounts {
		p.Add(a)
	}
	return New(Config{
		Pool:                p,
		Upstream:            up,
		ActivityReportCount: 1,
		CheckinDisabled:     true,
		KeepaliveDisabled:   true,
	})
}

// TestRunTaskByNameUnknownTask 未知任务名必须报错而非静默成功。
func TestRunTaskByNameUnknownTask(t *testing.T) {
	s := New(Config{Pool: newPool(), Upstream: &upstream.Client{}})

	for _, name := range []string{"", "actvity", "checkin", "Activity", "run-all"} {
		res, err := s.RunTaskByName(name)
		if err == nil {
			t.Errorf("%q 应报错（拼错名字必须可见，否则界面点了没反应）", name)
		}
		if res.Ran {
			t.Errorf("%q 不应报告 ran=true", name)
		}
	}
}

// TestRunTaskByNameRunsActivity 已知任务名应真的跑一轮活跃上报。
func TestRunTaskByNameRunsActivity(t *testing.T) {
	rec := &activityRecorder{streak: 3}
	s := newActivityScheduler(t, rec, 1, &auth.Auth{UID: "u1", AccessToken: "tok"})

	res, err := s.RunTaskByName(TaskNameActivity)
	if err != nil {
		t.Fatalf("RunTaskByName(activity): %v", err)
	}
	if !res.Ran {
		t.Errorf("活跃上报无前置条件，应报告 ran=true，实际 %+v", res)
	}
	if res.Task != TaskNameActivity {
		t.Errorf("Task=%q want %q", res.Task, TaskNameActivity)
	}
	if got := len(rec.snapshot()); got != 1 {
		t.Errorf("应真的发出 1 条上报，实际 %d 条", got)
	}
}

// TestRunTaskByNameIgnoresDisabledSwitch 禁用开关只关后台排程，不该拦住手动触发。
//
// 与宿主侧 checkin_all / travel_run 的既有语义一致：用户主动点击就该执行。
func TestRunTaskByNameIgnoresDisabledSwitch(t *testing.T) {
	rec := &activityRecorder{streak: 3}
	s := newActivityScheduler(t, rec, 1, &auth.Auth{UID: "u1", AccessToken: "tok"})
	// 显式关掉活跃上报的自动排程
	s.cfg.ActivityDisabled = true

	res, err := s.RunTaskByName(TaskNameActivity)
	if err != nil {
		t.Fatalf("RunTaskByName: %v", err)
	}
	if !res.Ran {
		t.Errorf("手动触发不应被 enabled 开关拦住，实际 %+v", res)
	}
	if got := len(rec.snapshot()); got != 1 {
		t.Errorf("应仍发出 1 条上报，实际 %d 条", got)
	}
}

// TestRunTaskByNameNightOwlOutsideWindowReportsSkip 夜猫子窗口外触发应回报
// 「跳过 + 原因」，而不是静默什么都不做。
//
// 为什么这条最重要：窗口外 runNightOwl 会直接 return，若入口不区分，
// 用户点「立即执行」后界面毫无变化，只会以为功能坏了。
func TestRunTaskByNameNightOwlOutsideWindowReportsSkip(t *testing.T) {
	// 当前真实时刻若恰在窗口内（凌晨跑 CI），本用例无法验证「跳过」分支 ——
	// 明确跳过而不是假通过。
	if withinNightWindow() {
		t.Skip("当前处于夜猫窗口内，无法验证窗口外跳过分支")
	}

	rec := &nightRecorder{}
	s := newNightScheduler(t, rec, &auth.Auth{UID: "u1", AccessToken: "tok"})

	res, err := s.RunTaskByName(TaskNameNightOwl)
	if err != nil {
		t.Fatalf("RunTaskByName(nightowl): %v", err)
	}
	if res.Ran {
		t.Errorf("窗口外不应真的执行，实际 %+v", res)
	}
	if res.Skip != "outside_window" {
		t.Errorf("Skip=%q want outside_window", res.Skip)
	}
	if res.Message == "" {
		t.Error("必须给出面向用户的跳过原因（界面要直接显示）")
	}
	if got := rec.count(); got != 0 {
		t.Errorf("窗口外不应发出任何上报，实际 %d 条", got)
	}
}

// TestWithinNightWindowBoundariesAt 窗口边界：23:00 起、08:00 止（不含 08:00）。
//
// 与上一条互补：上一条只能验证「当前时刻」那一个点，这里把 24 个小时全覆盖 ——
// 边界改错（如把 >= 写成 >）在真实时钟下几乎不可能被偶然撞到。
func TestWithinNightWindowBoundariesAt(t *testing.T) {
	base := cstNow()
	for h := 0; h < 24; h++ {
		at := timeAtHour(base, h)
		want := h >= nightWindowStartHour || h < nightWindowEndHour
		if got := withinNightWindowAt(at); got != want {
			t.Errorf("%02d:00 判定=%v want %v", h, got, want)
		}
	}
}

// TestRunTaskByNameRejectsReentry 同一任务重入必须被拦住。
//
// 用「跑起来就阻塞」的假上游复现「上一轮还没跑完又点一次」。
func TestRunTaskByNameRejectsReentry(t *testing.T) {
	rec := &blockingRecorder{entered: make(chan struct{}), release: make(chan struct{})}
	s := newBlockingScheduler(t, rec, &auth.Auth{UID: "u1", AccessToken: "tok"})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = s.RunTaskByName(TaskNameActivity)
	}()

	// 等第一轮真正进入上报阶段，再发起第二次
	<-rec.entered
	res, err := s.RunTaskByName(TaskNameActivity)
	if !errors.Is(err, ErrTaskRunning) {
		t.Errorf("重入应返回 ErrTaskRunning，实际 err=%v", err)
	}
	if res.Skip != "already_running" {
		t.Errorf("Skip=%q want already_running", res.Skip)
	}
	if res.Message == "" {
		t.Error("重入也必须给出可显示的中文说明")
	}

	close(rec.release)
	wg.Wait()
}

// TestRunTaskByNameDifferentTasksNotBlocked 不同任务之间不互相阻塞。
//
// 为什么：taskBusy 是防「同一个任务」重入，不该退化成全局串行 ——
// 否则用户在活跃上报跑着时点「立即执行 trial」会被无理由拒绝。
func TestRunTaskByNameDifferentTasksNotBlocked(t *testing.T) {
	rec := &blockingRecorder{entered: make(chan struct{}), release: make(chan struct{})}
	s := newBlockingScheduler(t, rec, &auth.Auth{UID: "u1", AccessToken: "tok"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.RunTaskByName(TaskNameActivity)
	}()
	<-rec.entered

	// 另一个任务：国服账号 + trial 只跑国际版 → 没有账号可跑，会立刻返回
	res, err := s.RunTaskByName(TaskNameTrial)
	if errors.Is(err, ErrTaskRunning) {
		t.Error("不同任务不应被互相阻塞（taskBusy 必须按任务名分键）")
	}
	if res.Skip == "already_running" {
		t.Error("不同任务不应报告 already_running")
	}

	close(rec.release)
	<-done
}

// TestScheduledRunSkipsWhenManualRunning 到点排程与手动触发必须互斥。
//
// 场景：用户恰好在 10:00（activity_hours 的整点）点「立即执行」。
// 两者都在发同一批上报，互不感知就会叠成双倍 —— 正是活跃上报刻意
// 控制条数要规避的风控画像。排程那轮应当**直接跳过**而不是排队。
func TestScheduledRunSkipsWhenManualRunning(t *testing.T) {
	rec := &blockingRecorder{entered: make(chan struct{}), release: make(chan struct{})}
	s := newBlockingScheduler(t, rec, &auth.Auth{UID: "u1", AccessToken: "tok"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.RunTaskByName(TaskNameActivity)
	}()
	<-rec.entered

	// 模拟到点唤醒：这轮应被拦住，且**不能**再发一条上报
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		s.runCareTask(TaskNameActivity, func() { s.runActivity(context.Background()) })
	}()
	<-ran // 被拦时立即返回；若错误地排队则会阻塞在这里 → 测试超时失败

	rec.mu.Lock()
	got := rec.received
	rec.mu.Unlock()
	if got != 1 {
		t.Errorf("排程轮应被跳过，上报数应仍为 1，实际 %d", got)
	}

	close(rec.release)
	<-done

	// 手动那轮结束后，排程应能正常认领（标记被正确归还）
	if !s.claimTask(TaskNameActivity) {
		t.Error("手动轮结束后应能重新认领该任务")
	}
	s.releaseTask(TaskNameActivity)
}

// TestScheduledRunReleasesClaimOnPanicFree 认领必须在任务跑完后归还 ——
// 否则一次失败会让该任务此后永远「正在执行中」。
func TestScheduledRunReleasesClaimOnCompletion(t *testing.T) {
	rec := &activityRecorder{streak: 1}
	s := newActivityScheduler(t, rec, 1, &auth.Auth{UID: "u1", AccessToken: "tok"})

	s.runCareTask(TaskNameActivity, func() { s.runActivity(context.Background()) })
	if !s.claimTask(TaskNameActivity) {
		t.Fatal("跑完应已归还认领")
	}
	s.releaseTask(TaskNameActivity)

	// 手动触发同样要归还：连点两次第二次应能正常跑
	if _, err := s.RunTaskByName(TaskNameActivity); err != nil {
		t.Fatalf("第一次手动触发: %v", err)
	}
	if _, err := s.RunTaskByName(TaskNameActivity); err != nil {
		t.Fatalf("第二次手动触发（上一次已归还，不应报 ErrTaskRunning）: %v", err)
	}
}

// timeAtHour 返回 base 所在 CST 日的 h 点整。
func timeAtHour(base time.Time, h int) time.Time {
	return time.Date(base.Year(), base.Month(), base.Day(), h, 0, 0, 0, base.Location())
}
