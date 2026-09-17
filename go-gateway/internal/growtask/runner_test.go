package growtask

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// ---------------------------------------------------------------------------
// 假上游
//
// 用假上游而非 mock 接口：本包的编排逻辑是否正确的**唯一判据**是
// 「它按什么顺序、发了多少次、什么样的 HTTP 请求」，因此断言必须落在
// 真实 HTTP 层（路径、方法、请求头、请求体），而不是内部方法调用次数。
// 用真 *upstream.Client + 假 Transport，被测代码路径与生产完全一致。
// ---------------------------------------------------------------------------

// fakeUpstream 一个可编程的假上游。
type fakeUpstream struct {
	mu sync.Mutex

	// tasks 当前任务状态；按 task_code 索引。
	tasks map[string]*fakeTask
	// order 任务在列表里的顺序（决定返回顺序）。
	order []string

	// acceptCalls 收到的报名请求（每次调用一组 task_codes）。
	acceptCalls [][]string
	// claimCalls 收到的领奖请求（任务码序列）。
	claimCalls []string
	// reports 收到的上报（路径 → 次数）。
	reports map[string]int
	// reportBodies 记录 /v2/report 的请求体，供断言事件载荷。
	reportBodies [][]map[string]any
	// claimResponses 覆盖领奖响应（默认成功 +100/+5）。
	//
	// 带上 HTTP 状态码是必需的：上游用 **HTTP 400** 表达「进度未达标」，
	// 而 IsGrowthTaskNotCompleted 正是按 4xx 判定。若假上游一律回 200，
	// 就测不出「未达标」与「真故障」的区分 —— 而那正是调用方决定
	// 「该重试还是该放弃」的依据。
	claimResponses map[string]fakeResp
	// acceptFail 让报名请求失败。
	acceptFail bool
	// listFail 让列表请求失败。
	listFail bool

	// acceptMaterializesProgress 报名后是否让 progress 出现（默认 true）。
	//
	// 这不是可有可无的开关，而是复刻真实行为：未报名时上游把 progress
	// 下发为 null，只有报名后才出现 {current,target}。如果本包的编排漏了
	// 「先报名」，回读就会永远拿不到进度。
	acceptMaterializesProgress bool

	// scoreDelayListCalls 行为事件引起的进度变化延后到第 N 次列表查询才可见。
	//
	// 为什么需要它：真实上游的计分是**异步**的（对话完成后立即回读仍是 0/1，
	// 数秒后才变 1/1）。不模拟这个延迟，就测不出「轮询等异步计分」这条
	// 关键路径 —— 而没有它，奖励会漏发且用户看不出来。
	// 0 = 立即生效（默认）。
	scoreDelayListCalls int
	// pendingProgress 待落定的进度：task_code → 目标 current。
	pendingProgress map[string]int64

	// failBillingReport 让 billing 域的 /v2/report 失败（chat 域不受影响）。
	//
	// 两个域的上报路径都是 /v2/report，只能靠 host 区分 —— 这正好用来
	// 独立地打掉「活跃上报」类动作（走 billing）而保留「桌面行为链」（走 chat）。
	failBillingReport bool

	// listCalls 列表被请求的次数（用于断言轮询行为）。
	listCalls int
}

type fakeTask struct {
	code         string
	title        string
	acceptStatus string
	current      int64
	target       int64
	hasProgress  bool
	claimed      bool
	// credit / energy 奖励值。默认 100/5，可用 addWithReward 覆盖。
	//
	// 需要可覆盖是因为真实上游**存在无奖励任务**（black_cat 实测 0 积分 0 能量），
	// 而"无奖励 + 进度不推进"正是最需要被诚实报告的组合（见 runAction 的守卫）。
	credit int64
	energy int64
}

// fakeResp 一条可编程的上游响应（状态码 + 响应体）。
type fakeResp struct {
	status int
	body   string
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{
		tasks:                      map[string]*fakeTask{},
		reports:                    map[string]int{},
		claimResponses:             map[string]fakeResp{},
		acceptMaterializesProgress: true,
		pendingProgress:            map[string]int64{},
	}
}

// add 登记一个任务（默认奖励 100 积分 / 5 能量，与多数成长任务一致）。
func (f *fakeUpstream) add(code string, current, target int64, status string) *fakeUpstream {
	return f.addTask(code, current, target, status, 100, 5)
}

// addNoReward 登记一个**无奖励**任务（复刻 black_cat 的真实形态）。
func (f *fakeUpstream) addNoReward(code string, current, target int64, status string) *fakeUpstream {
	return f.addTask(code, current, target, status, 0, 0)
}

func (f *fakeUpstream) addTask(code string, current, target int64, status string, credit, energy int64) *fakeUpstream {
	f.tasks[code] = &fakeTask{
		code: code, title: "任务-" + code, acceptStatus: status,
		current: current, target: target, credit: credit, energy: energy,
		// 与真实上游一致：未报名时不下发 progress。
		hasProgress: status != upstream.TaskAcceptNotAccepted,
	}
	f.order = append(f.order, code)
	return f
}

// setProgress 直接改进度（模拟服务端异步计分）。
func (f *fakeUpstream) setProgress(code string, current int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.tasks[code]; t != nil {
		t.current = current
		t.hasProgress = true
	}
}

func (f *fakeUpstream) client() *upstream.Client {
	return &upstream.Client{
		HTTP:          &http.Client{Transport: rtFunc(f.roundTrip)},
		ChatHTTP:      &http.Client{Transport: rtFunc(f.roundTrip)},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		WebBaseCN:     "https://web.example",
		WebBaseIntl:   "https://web-intl.example",
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (fn rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func (f *fakeUpstream) roundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/v2/activity/growth/tasks":
		f.listCalls++
		if f.listFail {
			return jsonResp(500, `{"code":500,"msg":"boom"}`), nil
		}
		// 异步计分落定：到点时把待落定的进度写进去。
		if f.scoreDelayListCalls > 0 && f.listCalls >= f.scoreDelayListCalls {
			for code, current := range f.pendingProgress {
				if t := f.tasks[code]; t != nil {
					t.current = current
					t.hasProgress = true
				}
			}
			f.pendingProgress = map[string]int64{}
		}
		return jsonResp(200, f.tasksJSON()), nil

	case r.Method == http.MethodPost && path == "/v2/activity/growth/tasks/accept":
		if f.acceptFail {
			return jsonResp(500, `{"code":500,"msg":"accept failed"}`), nil
		}
		var body struct {
			TaskCodes []string `json:"task_codes"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		f.acceptCalls = append(f.acceptCalls, body.TaskCodes)
		type res struct {
			Code   string `json:"task_code"`
			Status string `json:"status"`
		}
		results := make([]res, 0, len(body.TaskCodes))
		for _, code := range body.TaskCodes {
			t := f.tasks[code]
			if t == nil {
				continue
			}
			if t.acceptStatus == upstream.TaskAcceptAccepted || t.acceptStatus == upstream.TaskAcceptClaimed {
				results = append(results, res{code, "already_accepted"})
				continue
			}
			t.acceptStatus = upstream.TaskAcceptAccepted
			// 复刻真实行为：报名后才下发 progress。
			if f.acceptMaterializesProgress && t.target > 0 {
				t.hasProgress = true
			}
			results = append(results, res{code, upstream.TaskAcceptAccepted})
		}
		out, _ := json.Marshal(map[string]any{
			"code": 0, "msg": "OK", "data": map[string]any{"results": results},
		})
		return jsonResp(200, string(out)), nil

	case r.Method == http.MethodPost && strings.HasPrefix(path, "/activity/growth/tasks/"):
		code := strings.TrimSuffix(strings.TrimPrefix(path, "/activity/growth/tasks/"), "/claim")
		f.claimCalls = append(f.claimCalls, code)
		if custom, ok := f.claimResponses[code]; ok {
			return jsonResp(custom.status, custom.body), nil
		}
		t := f.tasks[code]
		if t == nil {
			return jsonResp(400, `{"code":400,"msg":"task not found"}`), nil
		}
		if t.claimed {
			return jsonResp(200, `{"code":0,"msg":"OK","data":{"already_claimed":true,"credit":0,"energy":0}}`), nil
		}
		// 与真实上游一致：未达标时拒绝领奖。
		if !(t.hasProgress && t.target > 0 && t.current >= t.target) {
			return jsonResp(400, `{"code":400,"msg":"task not completed"}`), nil
		}
		t.claimed = true
		t.acceptStatus = upstream.TaskAcceptClaimed
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5}}`), nil

	case r.Method == http.MethodPost && path == "/v2/report":
		// 两个域共用该路径，靠 host 区分哪个通道。
		isBilling := r.URL.Host == "billing.example"
		if isBilling && f.failBillingReport {
			return jsonResp(500, `{"code":500,"msg":"billing report down"}`), nil
		}
		raw, _ := io.ReadAll(r.Body)
		var arr []map[string]any
		if err := json.Unmarshal(raw, &arr); err == nil {
			f.reportBodies = append(f.reportBodies, arr)
			// 让 chat_request_send / 对话链的上报真的推进进度，
			// 以便端到端验证「上报 → 计分 → 领奖」闭环。
			for _, ev := range arr {
				code, _ := ev["eventCode"].(string)
				f.applyEvent(code)
			}
		}
		f.reports[path]++
		return jsonResp(200, `{"code":0,"msg":"OK","data":{}}`), nil

	case r.Method == http.MethodPost && path == "/v2/user-asset/appearance/set":
		f.reports[path]++
		return jsonResp(200, `{"code":0,"msg":"OK","data":{}}`), nil

	case r.Method == http.MethodGet && path == "/activity/growth/streak":
		return jsonResp(200, `{"code":0,"data":{"streak":{"days":1}}}`), nil

	case r.Method == http.MethodPost && path == "/activity/growth/buddy/agreement",
		r.Method == http.MethodPost && path == "/activity/growth/buddy/first":
		f.reports[path]++
		return jsonResp(200, `{"code":0,"msg":"OK","data":{}}`), nil

	case r.Method == http.MethodPost && path == "/portal/operation-platform/market/expert/list":
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"experts":[
			{"expert_id":"ex_1","expert_type":"agent","display_name_zh":"专家一","profession_zh":"职业","version":"1.0.0"},
			{"expert_id":"ex_2","expert_type":"agent","display_name_zh":"专家二","profession_zh":"职业","version":"1.0.0"}
		]}}`), nil

	case r.Method == http.MethodPost && path == "/v2/chat/completions":
		// 服务端形状的 requestId（专家/skill 判据要求真实 id）。
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"id\":\"cmb-0123456789abcdef0123456789abcdef\",\"choices\":[{\"delta\":{\"content\":\"2\"}}]}\n\n" +
					"data: [DONE]\n\n")),
		}, nil
	}
	return jsonResp(404, `{"code":404,"msg":"no route: `+path+`"}`), nil
}

// applyEvent 让行为事件真的推进进度（复刻服务端计分语义）。
//
// scoreDelayListCalls > 0 时进度不立即生效，而是变成"待落定"，
// 由后续的列表查询在到点时提交 —— 这就是真实上游的异步计分行为。
func (f *fakeUpstream) applyEvent(code string) {
	set := func(taskCode string, current int64) {
		if f.scoreDelayListCalls > 0 {
			f.pendingProgress[taskCode] = current
			return
		}
		if t := f.tasks[taskCode]; t != nil {
			t.current = current
			t.hasProgress = true
		}
	}
	// chat_request_send 计入 chat_5（累计型）。
	if code == "chat_request_send" {
		if t := f.tasks["chat_5"]; t != nil {
			next := t.current + 1
			if next > t.target {
				next = t.target
			}
			set("chat_5", next)
		}
	}
	// 对话成功回执点亮 RichMeow_Chat。
	if code == "chat_message_response" {
		if t := f.tasks["RichMeow_Chat"]; t != nil {
			set("RichMeow_Chat", t.target)
		}
	}
}

// tasksJSON 生成与真实上游同形状的列表响应。
func (f *fakeUpstream) tasksJSON() string {
	type wire struct {
		TaskCode     string `json:"task_code"`
		Title        string `json:"title"`
		AcceptStatus string `json:"accept_status"`
		RewardCredit int64  `json:"reward_credit"`
		RewardEnergy int64  `json:"reward_energy"`
		Progress     any    `json:"progress"`
	}
	items := make([]wire, 0, len(f.order))
	for _, code := range f.order {
		t := f.tasks[code]
		var prog any
		if t.hasProgress {
			prog = map[string]int64{"current": t.current, "target": t.target}
		}
		items = append(items, wire{
			TaskCode: code, Title: t.title, AcceptStatus: t.acceptStatus,
			RewardCredit: t.credit, RewardEnergy: t.energy, Progress: prog,
		})
	}
	out, _ := json.Marshal(map[string]any{
		"code": 0, "msg": "OK", "data": map[string]any{"tasks": items},
	})
	return string(out)
}

// testRunner 构造一个用假上游、且把等待时间压到最小的 Runner。
//
// 压缩间隔是**必需**的：默认轮询预算是 4×3 秒、上报间隔 1 秒，
// 一轮完整测试要跑十几秒。缩短的是时序参数而非逻辑，覆盖的执行路径完全相同。
func testRunner(f *fakeUpstream) *Runner {
	return New(Options{
		Upstream:     f.client(),
		PollAttempts: 3,
		PollGap:      time.Millisecond,
		ReportGap:    time.Millisecond,
		ChatGap:      time.Millisecond,
	})
}

func testAuth() *auth.Auth {
	return &auth.Auth{AccessToken: "at", UID: "u1"}
}

// ---------------------------------------------------------------------------
// 测试
// ---------------------------------------------------------------------------

// TestRunAllAcceptsBeforeActing 未报名的任务必须先报名：
// 未报名时上游把 progress 下发为 null，不先报名则回读永远拿不到进度，
// 「上报 200 ≠ 计分」的自检就失去了依据。
func TestRunAllAcceptsBeforeActing(t *testing.T) {
	f := newFakeUpstream().
		add("chat_5", 0, 5, upstream.TaskAcceptNotAccepted).
		add("RichMeow_Chat", 0, 1, upstream.TaskAcceptNotAccepted)
	r := testRunner(f)

	r.RunAll(context.Background(), testAuth())

	if len(f.acceptCalls) == 0 {
		t.Fatal("应先批量报名再执行动作")
	}
	got := f.acceptCalls[0]
	want := map[string]bool{"chat_5": true, "RichMeow_Chat": true}
	if len(got) != 2 {
		t.Fatalf("报名任务数=%d want 2 (%v)", len(got), got)
	}
	for _, code := range got {
		if !want[code] {
			t.Errorf("不该报名 %s", code)
		}
	}
	// 报名必须发生在任何行为上报之前。
	if f.reports["/v2/report"] == 0 {
		t.Error("报名后应执行行为上报")
	}
}

// TestRunAllSkipsAcceptForAcceptedTasks 已报名/已领奖的任务不重复报名。
func TestRunAllSkipsAcceptForAcceptedTasks(t *testing.T) {
	f := newFakeUpstream().add("chat_5", 0, 5, upstream.TaskAcceptAccepted)
	r := testRunner(f)

	r.RunAll(context.Background(), testAuth())

	if len(f.acceptCalls) != 0 {
		t.Errorf("无需报名时不该调用报名接口, got %v", f.acceptCalls)
	}
}

// TestRunAllClaimHappyPath 端到端闭环：报名 → 上报 → 轮询到达标 → 自动领奖。
func TestRunAllClaimHappyPath(t *testing.T) {
	f := newFakeUpstream().add("chat_5", 0, 2, upstream.TaskAcceptNotAccepted)
	r := testRunner(f)

	items := r.RunAll(context.Background(), testAuth())

	var item *ItemResult
	for i := range items {
		if items[i].Code == "chat_5" {
			item = &items[i]
		}
	}
	if item == nil {
		t.Fatal("结果里缺少 chat_5")
	}
	if item.Status != StatusDone {
		t.Errorf("status=%s message=%s", item.Status, item.Message)
	}
	if !item.Claimed {
		t.Errorf("应自动领奖, item=%+v", item)
	}
	if item.Credit != 100 || item.Energy != 5 {
		t.Errorf("领奖数=%d/%d want 100/5", item.Credit, item.Energy)
	}
	if item.ProgressAfter != "2/2" {
		t.Errorf("progress_after=%q want 2/2", item.ProgressAfter)
	}
	if len(f.claimCalls) != 1 || f.claimCalls[0] != "chat_5" {
		t.Errorf("领奖调用=%v", f.claimCalls)
	}
}

// TestRunAllPollsUntilScoreLands 异步计分：进度延后一拍才可见时，
// 轮询必须等到它落定并领奖。
//
// 为什么这条最关键：真实上游计分是异步的（对话完成后立即回读仍是 0/1，
// 数秒后才变 1/1）。一次性回读会把「还在计分」误判成「未达标」，
// 奖励就此漏掉，而用户完全看不出来 —— 这是本功能最隐蔽的失效方式。
func TestRunAllPollsUntilScoreLands(t *testing.T) {
	f := newFakeUpstream().add("RichMeow_Chat", 0, 1, upstream.TaskAcceptAccepted)
	// 计分延后到第 3 次列表查询才可见（首次读取 before 算第 1 次，
	// 动作完成后的第一次回读是第 2 次，轮询的第 2 拍才拿到达标）。
	f.scoreDelayListCalls = 3
	r := testRunner(f)

	item := r.RunOne(context.Background(), testAuth(), "RichMeow_Chat")

	if item.Status != StatusDone {
		t.Fatalf("status=%s msg=%s", item.Status, item.Message)
	}
	if !item.Claimed {
		t.Errorf("轮询等到计分落定后应自动领奖, item=%+v", item)
	}
	if item.ProgressAfter != "1/1" {
		t.Errorf("progress_after=%q want 1/1", item.ProgressAfter)
	}
	if len(f.claimCalls) != 1 {
		t.Errorf("应恰好领奖一次, got %v", f.claimCalls)
	}
	// 必须真的轮询过：至少 3 次列表查询（before + 回读 + 再轮询）。
	if f.listCalls < 3 {
		t.Errorf("列表查询次数=%d，未体现轮询等待", f.listCalls)
	}
}

// TestRunAllNoClaimWhenScoreNeverLands 计分始终不落定时**不能**领奖，
// 且不得谎报进度。
//
// 这条守的是诚实性：宁可如实报告「仍未达标」，也不能为了让结果好看
// 而假装达标（那会打出一个必然被上游拒绝的领奖请求，并误导用户）。
func TestRunAllNoClaimWhenScoreNeverLands(t *testing.T) {
	f := newFakeUpstream().add("RichMeow_Chat", 0, 1, upstream.TaskAcceptAccepted)
	// 计分落在第 99 次查询 —— 远超轮询预算（3 次）。
	f.scoreDelayListCalls = 99
	r := testRunner(f)

	item := r.RunOne(context.Background(), testAuth(), "RichMeow_Chat")

	if item.Claimed {
		t.Error("进度未达标时不该领奖")
	}
	if len(f.claimCalls) != 0 {
		t.Errorf("进度未达标时不该调用领奖接口, got %v", f.claimCalls)
	}
	if item.ProgressAfter == "1/1" {
		t.Error("进度未达标时不得谎报为达标")
	}
	// 动作本身确实执行了，所以状态仍是 done（区别于「执行失败」）。
	if item.Status != StatusDone {
		t.Errorf("status=%s want done（动作已执行，只是尚未计分）", item.Status)
	}
}

// TestRunAllContinuesAfterItemFailure 单项失败不影响后续项（尽力而为）。
//
// 构造：让 billing 域上报失败（chat_5 的动作会失败），
// 而 chat 域的桌面行为链正常 —— 断言后面的项照样被执行。
func TestRunAllContinuesAfterItemFailure(t *testing.T) {
	f := newFakeUpstream().
		add("chat_5", 0, 5, upstream.TaskAcceptAccepted).
		add("RichMeow_Chat", 0, 1, upstream.TaskAcceptAccepted)
	f.failBillingReport = true
	r := testRunner(f)

	items := r.RunAll(context.Background(), testAuth())

	byCode := map[string]ItemResult{}
	for _, it := range items {
		byCode[it.Code] = it
	}
	// chat_5 的动作失败（billing 上报挂了）→ 该项应为 error，
	// 或至少如实说明上报失败。
	chat5, ok := byCode["chat_5"]
	if !ok {
		t.Fatal("结果里缺少 chat_5")
	}
	failedOrExplained := chat5.Status == StatusError ||
		strings.Contains(chat5.Message, "失败") ||
		strings.Contains(chat5.Message, "中断")
	if !failedOrExplained {
		t.Errorf("billing 上报失败应被如实反映, got status=%s msg=%q", chat5.Status, chat5.Message)
	}
	// 关键断言：后续项仍然被执行（未被前一项的失败带崩）。
	rm, ok := byCode["RichMeow_Chat"]
	if !ok {
		t.Fatal("结果里缺少 RichMeow_Chat")
	}
	if rm.Status != StatusDone || !rm.Claimed {
		t.Errorf("前一项失败后，后续项仍应正常执行并领奖, got %+v", rm)
	}
}

// TestRunAllSkipsClaimedTask 已领奖的任务既不动手也不领奖（幂等，不浪费配额）。
func TestRunAllSkipsClaimedTask(t *testing.T) {
	f := newFakeUpstream().add("chat_5", 5, 5, upstream.TaskAcceptClaimed)
	r := testRunner(f)

	items := r.RunAll(context.Background(), testAuth())

	for _, it := range items {
		if it.Code == "chat_5" {
			if it.Status != StatusSkipped {
				t.Errorf("已领奖任务应跳过, got %+v", it)
			}
			if !strings.Contains(it.Message, "已领取") {
				t.Errorf("跳过原因应说明已领取: %q", it.Message)
			}
		}
	}
	if len(f.claimCalls) != 0 {
		t.Errorf("已领奖任务不该再调领奖接口: %v", f.claimCalls)
	}
	// 全部跳过时不写账号记录（无变化，写了只会刷屏）。
	if f.reports["/v2/report"] != 0 {
		t.Error("已领奖任务不该有任何行为上报")
	}
}

// TestRunAllClaimsAlreadyCompletedTask 进度已达标但没领过奖：
// 直接补领奖，不重复跑动作（省上游配额）。
func TestRunAllClaimsAlreadyCompletedTask(t *testing.T) {
	f := newFakeUpstream().add("chat_5", 5, 5, upstream.TaskAcceptAccepted)
	r := testRunner(f)

	items := r.RunAll(context.Background(), testAuth())

	for _, it := range items {
		if it.Code != "chat_5" {
			continue
		}
		if !it.Claimed {
			t.Errorf("达标未领奖的任务应直接补领, got %+v", it)
		}
		if !strings.Contains(it.Message, "直接领奖") {
			t.Errorf("message=%q", it.Message)
		}
	}
	// 已达标就不该再发行为上报（那是浪费）。
	if f.reports["/v2/report"] != 0 {
		t.Errorf("已达标任务不该再有行为上报, got %d 次", f.reports["/v2/report"])
	}
}

// TestRunAllListFailureReportsError 列表拉不到时整体报错，
// 不编造「全部跳过」—— 那会让用户以为任务都已处理过。
func TestRunAllListFailureReportsError(t *testing.T) {
	f := newFakeUpstream()
	f.listFail = true
	r := testRunner(f)

	items := r.RunAll(context.Background(), testAuth())

	if len(items) != 1 || items[0].Status != StatusError {
		t.Fatalf("items=%+v", items)
	}
	if !strings.Contains(items[0].Message, "拉取任务列表失败") {
		t.Errorf("message=%q", items[0].Message)
	}
}

// TestRunAllContinuesAfterAcceptFailure 报名失败记录在案，其余动作照常执行。
func TestRunAllContinuesAfterAcceptFailure(t *testing.T) {
	f := newFakeUpstream().add("chat_5", 0, 5, upstream.TaskAcceptNotAccepted)
	f.acceptFail = true
	r := testRunner(f)

	items := r.RunAll(context.Background(), testAuth())

	var acceptItem, chatItem *ItemResult
	for i := range items {
		switch items[i].Code {
		case "(批量报名)":
			acceptItem = &items[i]
		case "chat_5":
			chatItem = &items[i]
		}
	}
	if acceptItem == nil || acceptItem.Status != StatusError {
		t.Errorf("报名失败应如实汇报, got %+v", acceptItem)
	}
	if acceptItem != nil && !strings.Contains(acceptItem.Message, "不阻塞") {
		t.Errorf("应说明报名失败不阻塞后续: %q", acceptItem.Message)
	}
	if chatItem == nil {
		t.Fatal("报名失败后仍应尝试执行动作")
	}
}

// TestRunAllIgnoresUnknownTasks 该账号没有的任务报「没有此任务」而非失败。
func TestRunAllIgnoresUnknownTasks(t *testing.T) {
	f := newFakeUpstream().add("chat_5", 0, 5, upstream.TaskAcceptAccepted)
	r := testRunner(f)

	items := r.RunAll(context.Background(), testAuth())

	found := false
	for _, it := range items {
		if it.Code == "expert_5" {
			found = true
			if it.Status != StatusSkipped || !strings.Contains(it.Message, "没有此任务") {
				t.Errorf("不存在的任务应跳过: %+v", it)
			}
		}
	}
	if !found {
		t.Error("结果应包含动作表里的每个任务（含该账号没有的）")
	}
}

// TestRunOneUnsupportedTask 不可自动的任务返回明确指引，而不是假装执行。
func TestRunOneUnsupportedTask(t *testing.T) {
	f := newFakeUpstream()
	r := testRunner(f)

	item := r.RunOne(context.Background(), testAuth(), "Expert_Philanthropy")
	if item.Status != StatusUnsupported {
		t.Errorf("status=%s want unsupported", item.Status)
	}
	if !strings.Contains(item.Message, "无法自动完成") {
		t.Errorf("message=%q", item.Message)
	}
}

// TestRunOneAcceptsFirst 单任务入口也必须先报名（与整轮同一口径）。
func TestRunOneAcceptsFirst(t *testing.T) {
	f := newFakeUpstream().add("RichMeow_Chat", 0, 1, upstream.TaskAcceptNotAccepted)
	r := testRunner(f)

	item := r.RunOne(context.Background(), testAuth(), "RichMeow_Chat")
	if item.Status != StatusDone {
		t.Fatalf("status=%s msg=%s", item.Status, item.Message)
	}
	if len(f.acceptCalls) != 1 || f.acceptCalls[0][0] != "RichMeow_Chat" {
		t.Errorf("应只为该任务报名, got %v", f.acceptCalls)
	}
	if !item.Claimed {
		t.Errorf("应自动领奖, got %+v", item)
	}
}

// TestRunOneMissingTask 该账号没有此任务 → 跳过而非失败。
func TestRunOneMissingTask(t *testing.T) {
	f := newFakeUpstream().add("chat_5", 0, 5, upstream.TaskAcceptAccepted)
	r := testRunner(f)

	item := r.RunOne(context.Background(), testAuth(), "expert_5")
	if item.Status != StatusSkipped {
		t.Errorf("status=%s want skipped", item.Status)
	}
}

// TestAccountLock 账号级互斥：同账号并发跑会互相干扰进度回读，
// 必须能被挡住。
func TestAccountLock(t *testing.T) {
	r := testRunner(newFakeUpstream())
	if !r.Lock("u1") {
		t.Fatal("首次认领应成功")
	}
	if r.Lock("u1") {
		t.Error("同账号重复认领应失败")
	}
	// 不同账号互不影响。
	if !r.Lock("u2") {
		t.Error("不同账号应可同时认领")
	}
	r.Unlock("u1")
	if !r.Lock("u1") {
		t.Error("归还后应可重新认领")
	}
}

// TestSupportedCodesMatchActionTable 动作表与需求里的任务清单一致：
// 17 个可自动任务 + 1 个不可自动。
func TestSupportedCodesMatchActionTable(t *testing.T) {
	codes := SupportedCodes()
	if len(codes) != len(actions) {
		t.Fatalf("codes=%d actions=%d", len(codes), len(actions))
	}
	// 需求点名的任务必须都在表里。
	want := []string{
		"first_buddy", "create_canvas", "chat_5", "Model_chat_GLM5.2", "RichMeow_Chat",
		"Buddy_App", "Buddy_App_QQ", "automation_1", "Library_read", "template_5",
		"playbook_prompt", "expert_5", "Expert_team_use_3", "Hp_Appearance",
		"Expert_lighthouse", "skill_1", "black_cat",
	}
	have := map[string]bool{}
	for _, c := range codes {
		have[c] = true
	}
	for _, c := range want {
		if !have[c] {
			t.Errorf("动作表缺少任务 %s", c)
		}
	}
	if len(have) != len(want) {
		t.Errorf("动作表有多余任务: have=%d want=%d", len(have), len(want))
	}
	// 不可自动的任务**不能**出现在动作表里。
	if actionFor("Expert_Philanthropy") != nil {
		t.Error("Expert_Philanthropy 需真实捐款，不应出现在动作表里")
	}
}

// TestBlackCatMessageIsHonest black_cat 的文案必须诚实。
//
// 实测背景：该任务目标为 3、奖励为 0，而同一夜内用三种判据口径各做一次
// 真实对话后进度都停在 1/3 不动（疑为按夜计数、需跨 3 夜）。
// 因此文案**不能**说"已完成" —— 那会让用户以为任务做完了。
// 要么说明为什么没做（不在窗口），要么如实报告只是"上报/对话"了。
func TestBlackCatMessageIsHonest(t *testing.T) {
	f := newFakeUpstream().addNoReward("black_cat", 0, 3, upstream.TaskAcceptAccepted)
	r := testRunner(f)

	item := r.RunOne(context.Background(), testAuth(), "black_cat")
	if item.Status != StatusDone {
		t.Fatalf("status=%s msg=%s", item.Status, item.Message)
	}
	if strings.Contains(item.Message, "已完成任务") || strings.Contains(item.Message, "任务完成") {
		t.Errorf("文案不该声称任务已完成: %q", item.Message)
	}
	// 非窗口时说明原因；窗口内则如实描述"上报/对话"这一动作本身。
	ok := strings.Contains(item.Message, "不在夜猫时段") ||
		strings.Contains(item.Message, "夜猫时段") ||
		strings.Contains(item.Message, "已达标")
	if !ok {
		t.Errorf("文案未说明时段或动作: %q", item.Message)
	}
}

// TestNoRewardStalledTaskIsReportedHonestly 无奖励且进度未推进时，
// 必须如实说明「进度未推进」，不能把"上报成功"写成"已完成"。
//
// 这条守的是本功能的核心诚实性：上报 200 ≠ 计分。
// 如果动作发出去但进度没动，用户必须能看出来 —— 否则他会以为奖励该到账了。
func TestNoRewardStalledTaskIsReportedHonestly(t *testing.T) {
	// 造一个**无奖励**、目标 3 的任务（复刻 black_cat 的真实形态），
	// 且让计分始终不落定，以便观察"进度未推进"时的报告是否诚实。
	f := newFakeUpstream().addNoReward("black_cat", 1, 3, upstream.TaskAcceptAccepted)
	f.scoreDelayListCalls = 999
	r := testRunner(f)

	item := r.RunOne(context.Background(), testAuth(), "black_cat")

	// 进度确实没动。
	if item.ProgressAfter != "1/3" {
		t.Fatalf("progress_after=%q（假上游不该推进它）", item.ProgressAfter)
	}
	// 必须明确点出"进度未推进"。
	if !strings.Contains(item.Message, "进度未推进") {
		t.Errorf("进度未推进时必须如实说明, got %q", item.Message)
	}
	if item.Claimed {
		t.Error("进度未达标时不该领奖")
	}
}

// TestRunAllContextCancelStops 宿主提前结束本轮时应停止后续项。
func TestRunAllContextCancelStops(t *testing.T) {
	f := newFakeUpstream().
		add("chat_5", 0, 5, upstream.TaskAcceptAccepted).
		add("RichMeow_Chat", 0, 1, upstream.TaskAcceptAccepted)
	r := testRunner(f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	items := r.RunAll(ctx, testAuth())

	cancelled := 0
	for _, it := range items {
		if it.Status == StatusSkipped && strings.Contains(it.Message, "取消") {
			cancelled++
		}
	}
	if cancelled == 0 {
		t.Errorf("取消后应有条目说明被取消, items=%+v", items)
	}
}

// TestClaimFailureKeepsDoneStatus 达标但领奖失败：主流程仍算 done，
// 用 ClaimError 单独表达 —— 标成失败会让用户以为白做了。
func TestClaimFailureKeepsDoneStatus(t *testing.T) {
	f := newFakeUpstream().add("chat_5", 5, 5, upstream.TaskAcceptAccepted)
	// 领奖返回无法解析的响应体 → doJSON 报解析错误（真故障，而非"未达标"）。
	f.claimResponses["chat_5"] = fakeResp{200, `not-json`}
	r := testRunner(f)

	items := r.RunAll(context.Background(), testAuth())

	var found bool
	for _, it := range items {
		if it.Code != "chat_5" {
			continue
		}
		found = true
		if it.Status != StatusDone {
			t.Errorf("领奖失败不该改变主流程状态, got %s", it.Status)
		}
		if it.ClaimError == "" {
			t.Error("领奖失败必须留下 ClaimError")
		}
		if it.Claimed {
			t.Error("领奖失败时不该标记为已领")
		}
	}
	if !found {
		t.Fatal("结果里缺少 chat_5")
	}
}

// TestClaimNotCompletedMessage 上游答「未达标」时的文案要指出
// 「计分可能还在落定」，而不是笼统的失败 —— 这决定用户该重试还是放弃。
func TestClaimNotCompletedMessage(t *testing.T) {
	f := newFakeUpstream().add("chat_5", 5, 5, upstream.TaskAcceptAccepted)
	// 进度回读达标但领奖被拒（计分尚未落定的真实形态）：HTTP 400 + 该文案。
	f.claimResponses["chat_5"] = fakeResp{400, `{"code":400,"msg":"task not completed"}`}
	r := testRunner(f)

	items := r.RunAll(context.Background(), testAuth())

	for _, it := range items {
		if it.Code != "chat_5" {
			continue
		}
		if it.Status != StatusDone {
			t.Errorf("status=%s want done", it.Status)
		}
		if it.ClaimError == "" {
			t.Error("应记录 ClaimError")
		}
		if !strings.Contains(it.Message, "计分") {
			t.Errorf("文案应指出计分可能还在落定: %q", it.Message)
		}
	}
}

// TestRecordedDailyNotRepeated 连点多轮时账号记录按天去重，不刷屏。
//
// Records 为 nil 时 record 必须是安全空操作（宿主的网关子进程无记录器时
// 不能让整个任务崩掉）—— 这条主要守这个不变量。
func TestRecordedDailyNotRepeated(t *testing.T) {
	f := newFakeUpstream().add("chat_5", 0, 5, upstream.TaskAcceptAccepted)
	r := testRunner(f)
	if r.rec != nil {
		t.Fatal("测试应使用 nil Recorder")
	}
	// 连跑两轮都不应 panic。
	for i := 0; i < 2; i++ {
		if items := r.RunAll(context.Background(), testAuth()); len(items) == 0 {
			t.Fatal("应有结果")
		}
	}
}

// TestReportPayloadIsEventArray 上报体必须是事件数组（上游按数组解析）。
func TestReportPayloadIsEventArray(t *testing.T) {
	f := newFakeUpstream().add("RichMeow_Chat", 0, 1, upstream.TaskAcceptAccepted)
	r := testRunner(f)

	r.RunOne(context.Background(), testAuth(), "RichMeow_Chat")

	if len(f.reportBodies) == 0 {
		t.Fatal("应收到上报")
	}
	events := f.reportBodies[0]
	if len(events) == 0 {
		t.Fatal("上报事件数不应为 0")
	}
	if events[0]["eventCode"] != "agent_task_created" {
		t.Errorf("首个事件应为 agent_task_created, got %v", events[0]["eventCode"])
	}
	// 成功回执必须在事件链里（判据）。
	sawSuccess := false
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" && ev["isSuccessful"] == true {
			sawSuccess = true
		}
	}
	if !sawSuccess {
		t.Error("对话链缺少成功回执")
	}
	// 桌面指纹必须被注入。
	if events[0]["extName"] != "workbuddy-desktop" {
		t.Errorf("缺少桌面指纹 extName, got %v", events[0]["extName"])
	}
}
