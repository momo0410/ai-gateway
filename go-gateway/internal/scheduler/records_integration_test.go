package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/records"
	"workbuddy2api/internal/upstream"
)

// ---------------------------------------------------------------------------
// 4 个养号任务的「执行记录」回归
//
// 背景：这 4 个任务实现在 Go 网关里，此前只 log.Printf 写 stdout，
// 而宿主启动网关子进程时把 stdout/stderr 丢进了 Stdio::null ——
// 用户报的现象是「任务跑了，但界面上一条记录都没有」。
//
// 修法：把结果写进宿主已经在读的 account_records.json。
// 本文件验证的是**调用点**（哪个时刻该写、写什么结果），
// 记录文件本身的形状/并发/裁剪由 internal/records 的用例覆盖。
// ---------------------------------------------------------------------------

// newRecordedScheduler 构造一个带记录写入器的调度器，返回调度器与记录文件路径。
//
// 账号身份映射刻意给成「uid ≠ 宿主 id」，因为那正是最容易写错的地方：
// 界面按账号库的 id 过滤记录，写 uid 进去用户就永远查不到（且不报错）。
func newRecordedScheduler(t *testing.T, cfg Config) (*Scheduler, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "account_records.json")
	cfg.Records = records.New(path, 60, map[string]records.Identity{
		"u1":   {ID: "host-id-1", Name: "用户甲"},
		"u2":   {ID: "host-id-2", Name: "用户乙"},
		"cn":   {ID: "host-id-cn", Name: "国服号"},
		"intl": {ID: "host-id-intl", Name: "国际号"},
		"i1":   {ID: "host-id-i1", Name: "国际1"},
		"i2":   {ID: "host-id-i2", Name: "国际2"},
	})
	// 关掉其它任务，避免干扰
	cfg.CheckinDisabled = true
	cfg.KeepaliveDisabled = true
	cfg.ActivityDisabled = true
	cfg.NightOwlDisabled = true
	cfg.SchoolDisabled = true
	cfg.TrialDisabled = true
	return New(cfg), path
}

// recordsOf 读回记录文件；文件不存在时返回空切片（= 什么都没写）。
func recordsOf(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("读取记录文件失败: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("记录文件不是合法 JSON 数组: %v\n内容: %s", err, raw)
	}
	return out
}

// findByTitle 挑出指定标题的记录。
func findByTitle(all []map[string]any, title string) []map[string]any {
	var out []map[string]any
	for _, rec := range all {
		if rec["title"] == title {
			out = append(out, rec)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 活跃上报
// ---------------------------------------------------------------------------

// TestActivityRecordsSuccess 上报成功要留一条 success 记录。
//
// 这是本功能最核心的一条：用户界面上「任务」分类里必须能看到它，
// 否则「连登天数涨了但不知道是谁干的」这个疑问无法回答。
func TestActivityRecordsSuccess(t *testing.T) {
	rec := &activityRecorder{streak: 3}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok"})
	s, path := newRecordedScheduler(t, Config{
		Pool:                p,
		Upstream:            &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		ActivityReportCount: 2,
		ActivityHours:       []int{10},
	})

	s.RunActivityNow()

	got := findByTitle(recordsOf(t, path), "活跃上报")
	if len(got) != 1 {
		t.Fatalf("上报成功应写 1 条记录，实际 %d 条", len(got))
	}
	r := got[0]
	if r["result"] != "success" {
		t.Errorf("result 应为 success，实际 %v", r["result"])
	}
	if r["kind"] != "task" {
		t.Errorf("kind 应为 task（界面按它分到「任务」分类），实际 %v", r["kind"])
	}
	// accountId 必须是宿主的 id，不是网关的 uid
	if r["accountId"] != "host-id-1" {
		t.Errorf("accountId 应为宿主 id host-id-1，实际 %v（界面按 id 过滤，写 uid 会查不到）", r["accountId"])
	}
	if r["accountName"] != "用户甲" {
		t.Errorf("accountName 应为用户甲，实际 %v", r["accountName"])
	}
	if detail, _ := r["detail"].(string); !strings.Contains(detail, "2/2") {
		t.Errorf("detail 应说明上报条数，实际 %q", detail)
	}
}

// TestActivityRecordsFailure 上报失败要留带错误原因的记录。
//
// 失败留痕是排查的唯一线索：宿主丢掉了网关的 stdout，
// 不留记录的话「上报失败了」这件事在界面上完全不可见。
func TestActivityRecordsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":500,"msg":"upstream boom"}`, 500)
	}))
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok"})
	s, path := newRecordedScheduler(t, Config{
		Pool:                p,
		Upstream:            &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		ActivityReportCount: 1,
		ActivityHours:       []int{10},
	})

	s.RunActivityNow()

	got := findByTitle(recordsOf(t, path), "活跃上报")
	if len(got) != 1 {
		t.Fatalf("上报失败应写 1 条记录，实际 %d 条", len(got))
	}
	if got[0]["result"] != "failed" {
		t.Errorf("result 应为 failed，实际 %v", got[0]["result"])
	}
	detail, _ := got[0]["detail"].(string)
	if !strings.Contains(detail, "1/1") {
		t.Errorf("detail 应指出是第几条失败，实际 %q", detail)
	}
	if detail == "" {
		t.Error("失败记录必须带错误信息，否则用户无法排查")
	}
}

// TestActivitySuccessWritesEveryRound 成功是「有新变化」，每次都要写（不去重）。
//
// 与失败/跳过不同：上报成功确实点亮了连登，一天多次触发就该有多条痕迹。
func TestActivitySuccessWritesEveryRound(t *testing.T) {
	rec := &activityRecorder{streak: 5}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok"})
	s, path := newRecordedScheduler(t, Config{
		Pool:                p,
		Upstream:            &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		ActivityReportCount: 1,
		ActivityHours:       []int{10},
	})

	s.RunActivityNow()
	s.RunActivityNow()

	if got := len(findByTitle(recordsOf(t, path), "活跃上报")); got != 2 {
		t.Errorf("每轮成功都应留痕（共 2 轮），实际 %d 条", got)
	}
}

// TestActivityNoRecordsWhenRecorderDisabled 未配置记录器时不写文件、不 panic。
//
// 独立运行 gateway.exe（无宿主）时必须完全静默。
func TestActivityNoRecordsWhenRecorderDisabled(t *testing.T) {
	rec := &activityRecorder{streak: 1}
	s := newActivityScheduler(t, rec, 1, &auth.Auth{UID: "u1", AccessToken: "tok"})
	// newActivityScheduler 不设 Records（nil）—— 跑通即证明 nil 安全
	s.RunActivityNow()

	if n := len(rec.snapshot()); n != 1 {
		t.Errorf("记录器缺席不应影响上报本身，实际上报 %d 条", n)
	}
}

// ---------------------------------------------------------------------------
// 夜猫子
// ---------------------------------------------------------------------------

// nightSuccessServer 返回一个总是成功的报告端点。
func nightSuccessServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestNightOwlRecordsOutsideWindow 窗口外跳过要留一条汇总记录。
//
// 为什么这条比其他「跳过」更该留痕：排程只在 01 点，用户白天点「立即执行」
// 会被跳过；不写记录的话界面上什么都没发生，用户只会认为功能坏了。
func TestNightOwlRecordsOutsideWindow(t *testing.T) {
	if withinNightWindow() {
		t.Skip("当前正处夜猫窗口内，无法测「窗口外跳过」")
	}
	srv := nightSuccessServer(t)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok"})
	s, path := newRecordedScheduler(t, Config{
		Pool:                p,
		Upstream:            &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		NightOwlHours:       []int{1},
		ActivityDisabled:    true,
		ActivityReportCount: 1,
	})

	s.RunNightOwlNow()

	got := findByTitle(recordsOf(t, path), "夜猫子任务")
	if len(got) != 1 {
		t.Fatalf("窗口外应写 1 条跳过记录，实际 %d 条", len(got))
	}
	if got[0]["result"] != "info" {
		t.Errorf("跳过应记为 info（不是 failed），实际 %v", got[0]["result"])
	}
	if detail, _ := got[0]["detail"].(string); !strings.Contains(detail, "23:00") {
		t.Errorf("detail 应说明时间窗，实际 %q", detail)
	}
	// 与具体账号无关的整轮结论：accountId 为空、名字是可读的「全部账号」
	if got[0]["accountId"] != "" {
		t.Errorf("整轮结论不应挂在某个账号下，实际 accountId=%v", got[0]["accountId"])
	}
	if got[0]["accountName"] != "全部账号" {
		t.Errorf("accountName 应为「全部账号」，实际 %v", got[0]["accountName"])
	}
}

// TestNightOwlOutsideWindowDedupesPerDay 窗口外跳过按天去重，不刷屏。
//
// 调度器每次到点都会调一次；用户也可能反复点「立即执行」。
func TestNightOwlOutsideWindowDedupesPerDay(t *testing.T) {
	if withinNightWindow() {
		t.Skip("当前正处夜猫窗口内")
	}
	srv := nightSuccessServer(t)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok"})
	s, path := newRecordedScheduler(t, Config{
		Pool:                p,
		Upstream:            &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		NightOwlHours:       []int{1},
		ActivityReportCount: 1,
	})

	s.RunNightOwlNow()
	s.RunNightOwlNow()
	s.RunNightOwlNow()

	if got := len(findByTitle(recordsOf(t, path), "夜猫子任务")); got != 1 {
		t.Errorf("同一天的跳过应只写 1 条（避免刷屏），实际 %d 条", got)
	}
}

// TestNightOwlRecordsSuccess 窗口内上报成功要留 success 记录。
func TestNightOwlRecordsSuccess(t *testing.T) {
	if !withinNightWindow() {
		t.Skip("当前不在夜猫窗口内，无法测「窗口内上报」")
	}
	srv := nightSuccessServer(t)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok"})
	s, path := newRecordedScheduler(t, Config{
		Pool:                p,
		Upstream:            &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		NightOwlHours:       []int{1},
		ActivityReportCount: 1,
	})

	s.RunNightOwlNow()

	got := findByTitle(recordsOf(t, path), "夜猫子任务")
	if len(got) != 1 {
		t.Fatalf("窗口内成功应写 1 条记录，实际 %d 条", len(got))
	}
	if got[0]["result"] != "success" {
		t.Errorf("result 应为 success，实际 %v", got[0]["result"])
	}
	if got[0]["accountId"] != "host-id-1" {
		t.Errorf("accountId 应为宿主 id，实际 %v", got[0]["accountId"])
	}
}

// ---------------------------------------------------------------------------
// 开学季活动
// ---------------------------------------------------------------------------

// TestSchoolRecordsClaimSuccess 领到奖励要留 success 记录（一次成功一行）。
//
// 只在**真的领到**时写：每天为「活动在期但没奖励可领」写一条只会刷成噪音。
func TestSchoolRecordsClaimSuccess(t *testing.T) {
	rec := &schoolRecorder{
		inPeriod: true,
		tasks: []map[string]any{
			task("chat_3_times", "finished", 50),
			task("desktop_chat_1_time", "finished", 100),
			task("share_invite", "claimed", 100), // 已领过，不该产生记录
		},
	}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok"})
	s, path := newRecordedScheduler(t, Config{
		Pool:        p,
		Upstream:    &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		SchoolHours: []int{12},
	})

	s.RunSchoolNow()

	got := findByTitle(recordsOf(t, path), "开学季活动")
	if len(got) != 1 {
		t.Fatalf("领到奖励应写 1 条汇总记录（而非每个任务一条），实际 %d 条", len(got))
	}
	if got[0]["result"] != "success" {
		t.Errorf("result 应为 success，实际 %v", got[0]["result"])
	}
	if detail, _ := got[0]["detail"].(string); !strings.Contains(detail, "2") {
		t.Errorf("detail 应说明领取数量，实际 %q", detail)
	}
}

// TestSchoolRecordsNothingClaimedStaysQuiet 活动在期但无奖励可领时不写记录。
//
// 「今天没有可领的」是常态，每天写一条等于把记录页变成噪音墙。
func TestSchoolRecordsNothingClaimedStaysQuiet(t *testing.T) {
	rec := &schoolRecorder{
		inPeriod: true,
		tasks: []map[string]any{
			task("task_student_verify", "pending", 100),
			task("share_invite", "claimed", 100),
		},
	}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok"})
	s, path := newRecordedScheduler(t, Config{
		Pool:        p,
		Upstream:    &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		SchoolHours: []int{12},
	})

	s.RunSchoolNow()

	if got := len(findByTitle(recordsOf(t, path), "开学季活动")); got != 0 {
		t.Errorf("没有奖励可领属于常态，不应写记录，实际 %d 条", got)
	}
}

// TestSchoolRecordsNotInPeriod 活动下线要留一条汇总 info 记录。
//
// 这是「明确且原因重要」的跳过：用户看到「活动不在期」就知道不是功能坏了。
func TestSchoolRecordsNotInPeriod(t *testing.T) {
	rec := &schoolRecorder{inPeriod: false}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok"})
	s, path := newRecordedScheduler(t, Config{
		Pool:        p,
		Upstream:    &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		SchoolHours: []int{12},
	})

	s.RunSchoolNow()

	got := findByTitle(recordsOf(t, path), "开学季活动")
	if len(got) != 1 {
		t.Fatalf("活动整体不在期应写 1 条汇总记录，实际 %d 条", len(got))
	}
	if got[0]["result"] != "info" {
		t.Errorf("下线是正常状态，应记 info 而非 failed，实际 %v", got[0]["result"])
	}
	if got[0]["accountId"] != "" {
		t.Errorf("整轮结论不应挂在某个账号下，实际 %v", got[0]["accountId"])
	}
}

// TestSchoolRecordsFetchFailure 拉清单失败要留 failed 记录。
func TestSchoolRecordsFetchFailure(t *testing.T) {
	rec := &schoolRecorder{inPeriod: true, tasksFail: 500}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok"})
	s, path := newRecordedScheduler(t, Config{
		Pool:        p,
		Upstream:    &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		SchoolHours: []int{12},
	})

	s.RunSchoolNow()

	got := findByTitle(recordsOf(t, path), "开学季活动")
	if len(got) != 1 {
		t.Fatalf("拉清单失败应写 1 条记录，实际 %d 条", len(got))
	}
	if got[0]["result"] != "failed" {
		t.Errorf("result 应为 failed，实际 %v", got[0]["result"])
	}
	if detail, _ := got[0]["detail"].(string); !strings.Contains(detail, "失败") {
		t.Errorf("detail 应说明失败原因，实际 %q", detail)
	}
}

// TestSchoolRecordsClaimFailureNamesTask 领取失败要带上任务 code。
//
// 活动有多个任务，不写清是哪个没领到，用户无法判断问题出在哪。
func TestSchoolRecordsClaimFailureNamesTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/portal/activity/school/tasks") {
			w.Header().Set("Content-Type", "application/json")
			body, _ := json.Marshal(map[string]any{
				"code": 0, "data": map[string]any{
					"in_period": true,
					"tasks":     []map[string]any{task("chat_3_times", "finished", 50)},
				},
			})
			_, _ = w.Write(body)
			return
		}
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"code":500,"msg":"claim boom"}`))
	}))
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok"})
	s, path := newRecordedScheduler(t, Config{
		Pool:        p,
		Upstream:    &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		SchoolHours: []int{12},
	})

	s.RunSchoolNow()

	got := findByTitle(recordsOf(t, path), "开学季活动")
	if len(got) != 1 {
		t.Fatalf("领取失败应写 1 条记录，实际 %d 条", len(got))
	}
	if got[0]["result"] != "failed" {
		t.Errorf("result 应为 failed，实际 %v", got[0]["result"])
	}
	if detail, _ := got[0]["detail"].(string); !strings.Contains(detail, "chat_3_times") {
		t.Errorf("失败信息应带上任务 code（活动有多个任务），实际 %q", detail)
	}
}

// ---------------------------------------------------------------------------
// trial 加油包
// ---------------------------------------------------------------------------

// TestTrialRecordsNewPack 领到新包要留 success 记录。
//
// 国际版没有签到/任务中心，trial 是唯一的积分增益动作 ——
// 领到了却看不到痕迹，用户会以为这个任务没生效。
func TestTrialRecordsNewPack(t *testing.T) {
	rec := &trialRecorder{}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "i1", AccessToken: "tok", Domain: "www.workbuddy.ai"})
	s, path := newRecordedScheduler(t, Config{
		Pool:       p,
		Upstream:   &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		TrialHours: []int{9, 21},
	})

	s.RunTrialNow()

	got := findByTitle(recordsOf(t, path), "trial 加油包")
	if len(got) != 1 {
		t.Fatalf("领到新包应写 1 条记录，实际 %d 条", len(got))
	}
	if got[0]["result"] != "success" {
		t.Errorf("result 应为 success，实际 %v", got[0]["result"])
	}
	if got[0]["accountId"] != "host-id-i1" {
		t.Errorf("accountId 应为宿主 id，实际 %v", got[0]["accountId"])
	}
}

// TestTrialRecordsAlreadyClaimedAsAlready 已领过记为 already（不是 failed）。
//
// 上游用 14051 表达「已领过」，客户端把它判成**幂等成功**。
// 记录里若写成 failed，用户会以为每天都在失败。
func TestTrialRecordsAlreadyClaimedAsAlready(t *testing.T) {
	rec := &trialRecorder{already: true}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "i1", AccessToken: "tok", Domain: "www.workbuddy.ai"})
	s, path := newRecordedScheduler(t, Config{
		Pool:       p,
		Upstream:   &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		TrialHours: []int{9, 21},
	})

	s.RunTrialNow()

	got := findByTitle(recordsOf(t, path), "trial 加油包")
	if len(got) != 1 {
		t.Fatalf("已领过应写 1 条记录，实际 %d 条", len(got))
	}
	if got[0]["result"] != "already" {
		t.Errorf("result 应为 already（幂等成功而非失败），实际 %v", got[0]["result"])
	}
}

// TestTrialAlreadyClaimedDedupesPerDay 已领过按天去重。
//
// trial 默认排在 9/21 两个时点，不去重的话每天至少两条「已领过」。
func TestTrialAlreadyClaimedDedupesPerDay(t *testing.T) {
	rec := &trialRecorder{already: true}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "i1", AccessToken: "tok", Domain: "www.workbuddy.ai"})
	s, path := newRecordedScheduler(t, Config{
		Pool:       p,
		Upstream:   &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		TrialHours: []int{9, 21},
	})

	s.RunTrialNow()
	s.RunTrialNow()

	if got := len(findByTitle(recordsOf(t, path), "trial 加油包")); got != 1 {
		t.Errorf("同一天的「已领过」应只写 1 条，实际 %d 条", got)
	}
}

// TestTrialRecordsFailure 领取失败要留 failed 记录。
func TestTrialRecordsFailure(t *testing.T) {
	rec := &trialRecorder{fail: 500}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "i1", AccessToken: "tok", Domain: "www.workbuddy.ai"})
	s, path := newRecordedScheduler(t, Config{
		Pool:       p,
		Upstream:   &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		TrialHours: []int{9, 21},
	})

	s.RunTrialNow()

	got := findByTitle(recordsOf(t, path), "trial 加油包")
	if len(got) != 1 {
		t.Fatalf("领取失败应写 1 条记录，实际 %d 条", len(got))
	}
	if got[0]["result"] != "failed" {
		t.Errorf("result 应为 failed，实际 %v", got[0]["result"])
	}
	if detail, _ := got[0]["detail"].(string); detail == "" {
		t.Error("失败记录必须带错误信息")
	}
}

// TestTrialSkipsCNNoRecords 国服账号不参与，也不该产生记录。
//
// 若为国服账号写一条「已领过/trial」，用户会以为国际版任务跑到了国服号上。
func TestTrialSkipsCNNoRecords(t *testing.T) {
	rec := &trialRecorder{}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn", AccessToken: "tok", Domain: "copilot.tencent.com"})
	s, path := newRecordedScheduler(t, Config{
		Pool:       p,
		Upstream:   &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		TrialHours: []int{9, 21},
	})

	s.RunTrialNow()

	if got := len(recordsOf(t, path)); got != 0 {
		t.Errorf("国服账号不参与 trial，不应产生记录，实际 %d 条", got)
	}
}

// TestActivitySkipsIntlNoRecords 默认区域范围内国际版不参与活跃上报，也不写记录。
func TestActivitySkipsIntlNoRecords(t *testing.T) {
	rec := &activityRecorder{streak: 1}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "intl", AccessToken: "tok", Domain: "www.workbuddy.ai"})
	s, path := newRecordedScheduler(t, Config{
		Pool:                p,
		Upstream:            &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		ActivityReportCount: 1,
		ActivityHours:       []int{10},
	})

	s.RunActivityNow()

	if got := len(recordsOf(t, path)); got != 0 {
		t.Errorf("国际版默认不参与活跃上报，不应产生记录，实际 %d 条", got)
	}
}

// TestDisabledAccountsNoRecords 被禁用的账号不参与，也不写记录。
func TestDisabledAccountsNoRecords(t *testing.T) {
	rec := &schoolRecorder{inPeriod: true, tasks: []map[string]any{task("chat_3_times", "finished", 50)}}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok"})
	s, path := newRecordedScheduler(t, Config{
		Pool:        p,
		Upstream:    &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		SchoolHours: []int{12},
	})
	s.cfg.Pool.Disable("u1", "测试禁用")

	s.RunSchoolNow()

	if got := len(recordsOf(t, path)); got != 0 {
		t.Errorf("被禁用的账号不参与，不应产生记录，实际 %d 条", got)
	}
}

// ---------------------------------------------------------------------------
// 并发：4 个任务可能同时到点
// ---------------------------------------------------------------------------

// TestConcurrentTasksWriteRecordsSafely 4 个任务同时跑时，记录文件始终可解析且不丢记录。
//
// scheduler.Run 会在同一时刻派发多个任务（配额重叠时 kinds 有多个），
// 加上宿主侧也会写同一个文件 —— 这是本改动最容易出问题的地方：文件写坏后
// 宿主解析失败会退化成空集，接着把历史整体覆盖（用户看到「记录全没了」）。
//
// 用 -race 跑可同时检出数据竞争。
func TestConcurrentTasksWriteRecordsSafely(t *testing.T) {
	rec := &activityRecorder{streak: 3}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok"})
	s, path := newRecordedScheduler(t, Config{
		Pool:                p,
		Upstream:            &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		ActivityReportCount: 1,
		ActivityHours:       []int{10},
		NightOwlHours:       []int{1},
		SchoolHours:         []int{12},
		TrialHours:          []int{9, 21},
	})

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			switch i % 4 {
			case 0:
				s.RunActivityNow()
			case 1:
				s.RunNightOwlNow()
			case 2:
				s.RunSchoolNow()
			case 3:
				s.RunTrialNow()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	// 文件必须仍是合法 JSON 数组，且每条记录字段完整
	all := recordsOf(t, path) // 内部已断言是合法数组
	for _, r := range all {
		if r["kind"] != "task" {
			t.Errorf("kind 被写坏: %v", r)
		}
		if _, ok := r["ts"].(float64); !ok {
			t.Errorf("ts 缺失或类型错误: %v", r)
		}
		if title, _ := r["title"].(string); title == "" {
			t.Errorf("存在残缺记录: %v", r)
		}
	}
}

// TestConcurrentRecordWriteWithHostReader 与「宿主侧读取」并发时文件始终可解析。
//
// 宿主每次打开记录页都会读这个文件；读到半截 JSON 会解析失败并退化成空集，
// 随后的写入就把历史整体覆盖。这里持续读取，确保从未读到坏文件。
func TestConcurrentRecordWriteWithHostReader(t *testing.T) {
	rec := &activityRecorder{streak: 1}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok"})
	s, path := newRecordedScheduler(t, Config{
		Pool:                p,
		Upstream:            &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, BaseIntl: srv.URL},
		ActivityReportCount: 1,
		ActivityHours:       []int{10},
	})

	stop := make(chan struct{})
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	badReads := 0
	var mu sync.Mutex
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				// 文件尚不存在是允许的（首次写入前）
				continue
			}
			var out []map[string]any
			if err := json.Unmarshal(raw, &out); err != nil {
				mu.Lock()
				badReads++
				mu.Unlock()
			}
			time.Sleep(time.Millisecond)
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.RunActivityNow()
		}()
	}
	wg.Wait()
	close(stop)
	readerWG.Wait()

	mu.Lock()
	defer mu.Unlock()
	if badReads > 0 {
		t.Errorf("并发下有 %d 次读到不可解析的记录文件（宿主会因此丢掉全部历史）", badReads)
	}
}
