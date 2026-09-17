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
// 活跃上报回归
//
// 为什么需要这个任务：签到只恢复**余额**，连登天数与领养资格靠对话活跃度点亮。
// 一条 chat_request_send 同时点亮连登 + 解锁 first_buddy（领养前置）。
//
// 最容易踩的坑：事件缺 userId 时服务端返回 200 但**静默丢弃** ——
// 表现为「上报成功但连登没涨」。因此调用方必须回读 streak 自检。
// ---------------------------------------------------------------------------

// activityRecorder 记录收到的活跃上报请求。
type activityRecorder struct {
	mu       sync.Mutex
	bodies   []map[string]any
	streak   int
	streakAt atomic.Int32
}

func (r *activityRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/v2/report"):
			var arr []map[string]any
			if err := json.NewDecoder(req.Body).Decode(&arr); err == nil {
				r.mu.Lock()
				r.bodies = append(r.bodies, arr...)
				r.mu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		case strings.HasSuffix(req.URL.Path, "/activity/growth/streak"):
			r.streakAt.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"streak":{"days":` +
				itoa(r.streak) + `}}}`))
		default:
			http.Error(w, "unexpected path: "+req.URL.Path, 404)
		}
	}
}

func (r *activityRecorder) snapshot() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, len(r.bodies))
	copy(out, r.bodies)
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// newActivityScheduler 起一个指向 fake 上游的调度器。
//
// 注意必须同时设 ChatBaseCN / BillingBaseCN / **BaseIntl**：
// 国际版账号走 BaseIntl，漏设会让它打到真实的 workbuddy.ai（测试变慢且 401）。
func newActivityScheduler(t *testing.T, rec *activityRecorder, count int, accounts ...*auth.Auth) *Scheduler {
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
		ActivityReportCount: count,
		// 关掉其它任务，避免干扰
		CheckinDisabled:   true,
		KeepaliveDisabled: true,
	})
}

// newPool 构造带账号的池（供本文件各用例复用）。
func newPool(accounts ...*auth.Auth) *pool.Pool {
	p := pool.New("")
	for _, a := range accounts {
		p.Add(a)
	}
	return p
}

// TestActivitySendsReportWithUserID 上报事件必须带 userId。
//
// 这是最关键的一条：缺 userId 时服务端 200 但静默丢弃，
// 连登天数不动 —— 且从返回码上完全看不出来。
func TestActivitySendsReportWithUserID(t *testing.T) {
	rec := &activityRecorder{streak: 5}
	s := newActivityScheduler(t, rec, 1, &auth.Auth{UID: "u1", AccessToken: "tok"})

	s.RunActivityNow()

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("应上报 1 条，实际 %d 条", len(got))
	}
	ev := got[0]
	if ev["userId"] != "u1" {
		t.Errorf("事件必须带 userId=u1（缺失会被服务端静默丢弃），实际 %v", ev["userId"])
	}
	if ev["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode 应为 chat_request_send，实际 %v", ev["eventCode"])
	}
	// 全字段形状：上游可能随时加严，不能只发最小子集
	for _, k := range []string{"conversationId", "requestId", "timestamp", "mode",
		"inputLength", "requestModelId", "agentName", "agentType", "rootRequestId"} {
		if _, ok := ev[k]; !ok {
			t.Errorf("事件缺少字段 %q（应照抄客户端全字段形状）", k)
		}
	}
}

// TestActivityReportCountAndSharedConversation 多条上报共用会话、各自独立 requestId。
func TestActivityReportCountAndSharedConversation(t *testing.T) {
	rec := &activityRecorder{streak: 3}
	s := newActivityScheduler(t, rec, 3, &auth.Auth{UID: "u1", AccessToken: "tok"})

	s.RunActivityNow()

	got := rec.snapshot()
	if len(got) != 3 {
		t.Fatalf("应上报 3 条，实际 %d 条", len(got))
	}
	cid := got[0]["conversationId"]
	seenRID := map[string]bool{}
	for i, ev := range got {
		if ev["conversationId"] != cid {
			t.Errorf("第 %d 条应共用 conversationId=%v，实际 %v", i+1, cid, ev["conversationId"])
		}
		rid, _ := ev["requestId"].(string)
		if rid == "" {
			t.Errorf("第 %d 条缺 requestId", i+1)
			continue
		}
		if seenRID[rid] {
			t.Errorf("requestId %q 重复 —— 服务端按事件去重，重复会被丢弃", rid)
		}
		seenRID[rid] = true
	}
}

// TestActivityChecksStreakAfterReport 上报成功后要回读 streak 自检。
func TestActivityChecksStreakAfterReport(t *testing.T) {
	rec := &activityRecorder{streak: 7}
	s := newActivityScheduler(t, rec, 2, &auth.Auth{UID: "u1", AccessToken: "tok"})

	s.RunActivityNow()

	if n := rec.streakAt.Load(); n != 1 {
		t.Errorf("上报成功后应回读 streak 1 次，实际 %d 次", n)
	}
}

// TestActivityStreakZeroWarns 回读为 0 时应判定为可疑（静默丢弃的信号）。
func TestActivityStreakZeroWarns(t *testing.T) {
	rec := &activityRecorder{streak: 0}
	s := newActivityScheduler(t, rec, 1, &auth.Auth{UID: "u1", AccessToken: "tok"})

	s.RunActivityNow()

	if rec.streakAt.Load() != 1 {
		t.Fatal("应回读 streak")
	}
	// 直接验证判定函数：days=0 → 可疑
	suspect := s.checkActivityStreak(&auth.Auth{UID: "u1", AccessToken: "tok"})
	if !suspect {
		t.Error("streak.days=0 应判定为可疑（上报 200 但可能被静默丢弃）")
	}
}

// TestActivitySkipsDisabledAccounts 禁用账号不参与上报。
func TestActivitySkipsDisabledAccounts(t *testing.T) {
	rec := &activityRecorder{streak: 1}
	s := newActivityScheduler(t, rec, 1,
		&auth.Auth{UID: "u1", AccessToken: "tok"},
		&auth.Auth{UID: "u2", AccessToken: "tok"},
	)
	s.cfg.Pool.Disable("u2", "测试禁用")

	s.RunActivityNow()

	for _, ev := range rec.snapshot() {
		if ev["userId"] == "u2" {
			t.Error("被禁用的账号不应参与活跃上报")
		}
	}
}

// TestActivitySkipsIntlByDefault 默认只跑国服（国际版 growth 接口暂无真实数据）。
func TestActivitySkipsIntlByDefault(t *testing.T) {
	rec := &activityRecorder{streak: 1}
	s := newActivityScheduler(t, rec, 1,
		&auth.Auth{UID: "cn", AccessToken: "tok", Domain: "copilot.tencent.com"},
		&auth.Auth{UID: "intl", AccessToken: "tok", Domain: "www.workbuddy.ai"},
	)

	s.RunActivityNow()

	seen := map[string]bool{}
	for _, ev := range rec.snapshot() {
		seen[ev["userId"].(string)] = true
	}
	if !seen["cn"] {
		t.Error("国服账号应参与上报")
	}
	if seen["intl"] {
		t.Error("国际版账号默认应跳过（schedule.checkin_scope=cn）")
	}
}

// TestActivityIncludesIntlWhenScopeAll scope=all 时国际版也要上报。
func TestActivityIncludesIntlWhenScopeAll(t *testing.T) {
	rec := &activityRecorder{streak: 1}
	s := newActivityScheduler(t, rec, 1,
		&auth.Auth{UID: "intl", AccessToken: "tok", Domain: "www.workbuddy.ai"},
	)
	s.cfg.CheckinScope = "all"

	s.RunActivityNow()

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("scope=all 时国际版应上报，实际 %d 条", len(got))
	}
	if got[0]["userId"] != "intl" {
		t.Errorf("userId 应为 intl，实际 %v", got[0]["userId"])
	}
}

// TestActivityDisabledNoCalls 显式禁用后不得发起任何上游请求。
func TestActivityDisabledNoCalls(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no upstream call expected", 404)
	}))
	defer srv.Close()

	s := New(Config{
		Pool:              newPool(&auth.Auth{UID: "u1", AccessToken: "tok"}),
		Upstream:          &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL},
		CheckinDisabled:   true,
		KeepaliveDisabled: true,
		ActivityDisabled:  true,
		NightOwlDisabled:  true,
		SchoolDisabled:  true,
		TrialDisabled:  true,
		ActivityHours:     []int{10},
	})

	// nextWake 不应把 activity 排进去
	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("全部禁用时不应有排程，实际 at=%v kinds=%v", at, kinds)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("禁用后不应有上游调用，实际 %d 次", n)
	}
}

// TestActivityReportsBeforeAdoptForce 上报补满对话量后要立即重试领养。
//
// 场景：此前因「对话门槛未达」被标记当日不再重试；活跃上报刚补满对话量，
// 该判断已失效，应豁免防抖重试一次，而不是干等到明天。
func TestActivityReportsBeforeAdoptForce(t *testing.T) {
	var adoptCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v2/report"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/streak"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":3}}}`))
		case strings.HasSuffix(r.URL.Path, "/buddy/agreement"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/buddy/first"):
			adoptCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			http.Error(w, "unexpected: "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()

	s := New(Config{
		Pool:                newPool(&auth.Auth{UID: "u1", AccessToken: "tok"}),
		Upstream:            &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL},
		CheckinDisabled:     true,
		KeepaliveDisabled:   true,
		ActivityReportCount: 1,
	})

	// 先标记当日已试过领养（模拟此前门槛未达）
	s.markAdoptTried("u1")
	if !s.adoptTriedToday("u1") {
		t.Fatal("前置条件：应已标记当日已试")
	}

	s.RunActivityNow()

	if n := adoptCalls.Load(); n == 0 {
		t.Error("上报补满对话量后应豁免防抖、重试领养（travelAdoptForce）")
	}
}

// TestActivityContextCancelStops 取消 ctx 后立即停止，不等满间隔。
func TestActivityContextCancelStops(t *testing.T) {
	rec := &activityRecorder{streak: 1}
	s := newActivityScheduler(t, rec, 5, // 每号 5 条，条间有间隔
		&auth.Auth{UID: "u1", AccessToken: "tok"},
		&auth.Auth{UID: "u2", AccessToken: "tok"},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.runActivity(ctx)
		close(done)
	}()

	// 第一条上报后取消
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后 runActivity 应及时返回，不应等满所有间隔")
	}

	// 不应把所有账号的所有条数都发完
	if n := len(rec.snapshot()); n >= 10 {
		t.Errorf("取消后不应继续发满，实际已发 %d 条", n)
	}
}

// TestActivitySleepCtx 基本语义：正常睡满返回 true，取消返回 false。
func TestActivitySleepCtx(t *testing.T) {
	if !sleepCtx(context.Background(), 0) {
		t.Error("d<=0 应立即返回 true")
	}
	if !sleepCtx(context.Background(), 10*time.Millisecond) {
		t.Error("正常睡满应返回 true")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Second) {
		t.Error("ctx 已取消应返回 false")
	}
}

// TestUid8 日志用短 uid（不打印完整 uid，避免账号标识进入日志/issue）。
func TestUid8(t *testing.T) {
	cases := map[string]string{
		"abcdefgh-1234-5678": "abcdefgh",
		"short":              "short",
		"":                   "",
		"12345678":           "12345678",
	}
	for in, want := range cases {
		if got := uid8(in); got != want {
			t.Errorf("uid8(%q)=%q want %q", in, got, want)
		}
	}
}
