package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// claimPath 断言领奖请求打到 **Web 域** 的正确路径与头族。
//
// 这一条是本功能最容易写错也最难发现的地方：CLI 域上的同形路径恒返回 400，
// 与「进度未达标」无法区分。若哪天有人"顺手"把基址改成 chatBase，
// 这个断言必须立刻失败。
func claimPath(r *http.Request, wantCode string) error {
	if r.URL.Path != "/activity/growth/tasks/"+wantCode+"/claim" {
		return errors.New("wrong path: " + r.URL.Path)
	}
	if r.URL.Host != "web.example" {
		return errors.New("claim 必须走 Web 域，实际 host=" + r.URL.Host)
	}
	if r.Method != http.MethodPost {
		return errors.New("want POST, got " + r.Method)
	}
	if r.Header.Get("x-client-platform") != "web" {
		return errors.New("missing x-client-platform: web")
	}
	if r.Header.Get("Authorization") != "Bearer at" {
		return errors.New("missing Authorization")
	}
	if !strings.Contains(r.Header.Get("Referer"), "growth-center") {
		return errors.New("Referer 应指向成长中心, got " + r.Header.Get("Referer"))
	}
	return nil
}

// webTestClient 带 Web 域的测试客户端。
func webTestClient(fn rtFunc) *Client {
	c := testClient(fn)
	c.WebBaseCN = "https://web.example"
	c.WebBaseIntl = "https://web-intl.example"
	return c
}

// realTasksFixture 真实响应形状（2026-09-16 实测，字段名逐字来自上游）。
//
// 三个条目各有代表性：
//   - not_accepted 且 progress 为 **null**（未报名时上游不下发进度）
//   - accepted 且 progress 为 {0,1}（已报名，进度开始跟踪）
//   - claimed 且 progress 为 {1,1}（已领奖）
const realTasksFixture = `{"code":0,"msg":"OK","requestId":"x","data":{"tasks":[
 {"task_code":"playbook_prompt","title":"探索优秀灵感","description":"指引","task_desc":"条件",
  "task_type":"single","jump_url":"workbuddy://playbook","reward_credit":100,"reward_energy":5,
  "reward_buddy":false,"accept_status":"not_accepted","progress":null,"locked":false,"has_reward":true,"tag":"PC"},
 {"task_code":"create_canvas","title":"体验设计创意模式","task_type":"single",
  "reward_credit":300,"reward_energy":5,"accept_status":"accepted",
  "progress":{"current":0,"target":1},"locked":false,"has_reward":true,"tag":"PC"},
 {"task_code":"first_buddy","title":"领养 Buddy","task_type":"auto",
  "reward_credit":300,"reward_energy":8,"accept_status":"claimed",
  "progress":{"current":1,"target":1},"locked":false,"has_reward":true}
]}}`

func TestListGrowthTasksParsesRealShape(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return nil, errors.New("want GET")
		}
		if r.URL.Path != "/v2/activity/growth/tasks" {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.URL.Host != "chat.example" {
			return nil, errors.New("列表应走 chat 域，实际 " + r.URL.Host)
		}
		if r.Header.Get("Authorization") != "Bearer at" {
			return nil, errors.New("missing Authorization")
		}
		return jsonResp(200, realTasksFixture), nil
	})
	tasks, err := c.ListGrowthTasks(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("count=%d want 3", len(tasks))
	}

	byCode := map[string]GrowthTask{}
	for _, x := range tasks {
		byCode[x.Code] = x
	}

	// 未报名：progress 为 null → HasProgress=false，且不能被折叠成 0/0。
	nb := byCode["playbook_prompt"]
	if nb.HasProgress {
		t.Errorf("not_accepted 任务的 progress 为 null，HasProgress 应为 false: %+v", nb)
	}
	if nb.AcceptStatus != TaskAcceptNotAccepted {
		t.Errorf("accept_status=%q", nb.AcceptStatus)
	}
	if nb.Claimable() {
		t.Error("无进度的任务不该被判为可领奖（Target=0 时必须排除）")
	}
	if !nb.NeedsAccept() {
		t.Error("not_accepted 应需要报名")
	}
	if nb.ProgressText() != TaskAcceptNotAccepted {
		t.Errorf("progress text=%q", nb.ProgressText())
	}

	// 已报名：进度被跟踪，但未达标。
	cc := byCode["create_canvas"]
	if !cc.HasProgress || cc.Current != 0 || cc.Target != 1 {
		t.Errorf("create_canvas 进度解析错误: %+v", cc)
	}
	if cc.Claimable() {
		t.Error("0/1 不该可领奖")
	}
	if cc.NeedsAccept() {
		t.Error("已 accepted 不需要重复报名")
	}
	if cc.ProgressText() != "0/1" {
		t.Errorf("progress text=%q want 0/1", cc.ProgressText())
	}
	// 奖励字段名是 reward_ 前缀，别按直觉写成 credit。
	if cc.Credit != 300 || cc.Energy != 5 {
		t.Errorf("奖励解析错误 credit=%d energy=%d", cc.Credit, cc.Energy)
	}

	// 已领奖：既不报名也不领奖。
	fb := byCode["first_buddy"]
	if !fb.Claimed() {
		t.Error("claimed 任务 Claimed() 应为 true")
	}
	if fb.NeedsAccept() {
		t.Error("已领奖的任务不该再报名")
	}
	if fb.Claimable() {
		t.Error("已领奖的任务不该再被判为可领奖（否则会重复打领奖接口）")
	}
}

// TestGrowthTaskProgressUnknownNotZero 未报名（progress 缺失/null）与
// 已报名但进度为零必须可区分 —— 两者的下一步动作完全不同。
func TestGrowthTaskProgressUnknownNotZero(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"tasks":[
		 {"task_code":"a","accept_status":"not_accepted","progress":null},
		 {"task_code":"b","accept_status":"accepted","progress":{"current":0,"target":5}}
		]}}`), nil
	})
	tasks, err := c.ListGrowthTasks(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if tasks[0].HasProgress {
		t.Error("progress=null 应为未知进度")
	}
	if !tasks[1].HasProgress || tasks[1].Target != 5 {
		t.Errorf("已报名任务应带有已知进度: %+v", tasks[1])
	}
}

// TestListGrowthTasksToleratesLegacyShape 国际版返回的是另一套形状
// （code + status 而非 task_code + accept_status）。严格解析会得到一堆空 code
// 的条目，在界面上显示成空白任务名。
func TestListGrowthTasksToleratesLegacyShape(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"tasks":[
		 {"task_id":1,"code":"first_chat","title":"完成一次对话","status":"available","level_name":"养虾尝试"}
		]}}`), nil
	})
	tasks, err := c.ListGrowthTasks(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Code != "first_chat" || tasks[0].AcceptStatus != "available" {
		t.Fatalf("旧形状解析失败: %+v", tasks)
	}
	if tasks[0].HasProgress {
		t.Error("旧形状无 progress，应为未知进度")
	}
}

// TestListGrowthTasksDropsCodeless 无任务码的条目无法驱动任何动作，应丢弃。
func TestListGrowthTasksDropsCodeless(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"tasks":[{"title":"无码任务"},{"task_code":"ok"}]}}`), nil
	})
	tasks, err := c.ListGrowthTasks(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Code != "ok" {
		t.Fatalf("应丢弃无码条目, got %+v", tasks)
	}
}

// TestGrowthTaskNeedsAcceptStates 报名状态机的完整取值。
//
// 回归：`in_progress` 是 2026-09-16 真实账号联调时才发现的第三个状态 ——
// 任务报名后只要进度大于 0，accept_status 就从 accepted 变成 in_progress。
// 早期实现用 `!= accepted` 判定，会把进行中的任务误判成"未报名"并重复报名
//（上游幂等所以不出错，但会发无意义请求、并让"新报名 N 个"的数字虚高）。
// 因为这个 bug 只在真实响应里才现形，单测必须显式钉住它的取值。
func TestGrowthTaskNeedsAcceptStates(t *testing.T) {
	cases := []struct {
		status       string
		wantAccept   bool
		wantParticip bool
		desc         string
	}{
		{TaskAcceptNotAccepted, true, false, "未报名 → 需报名"},
		{TaskAcceptAccepted, false, true, "已报名 → 不重复报名"},
		{TaskAcceptInProgress, false, true, "进行中 → 不重复报名（实测状态）"},
		{TaskAcceptClaimed, false, true, "已领奖 → 不报名"},
		{"", true, false, "状态缺失 → 按未报名处理"},
	}
	for _, c := range cases {
		task := GrowthTask{AcceptStatus: c.status}
		if got := task.NeedsAccept(); got != c.wantAccept {
			t.Errorf("%s (%q): NeedsAccept=%v want %v", c.desc, c.status, got, c.wantAccept)
		}
		if got := task.Participated(); got != c.wantParticip {
			t.Errorf("%s (%q): Participated=%v want %v", c.desc, c.status, got, c.wantParticip)
		}
	}
	// 未解锁的任务不报名：报名必然被拒，只会产生一条失败记录。
	locked := GrowthTask{AcceptStatus: TaskAcceptNotAccepted, Locked: true}
	if locked.NeedsAccept() {
		t.Error("locked 任务不该报名")
	}
}

// TestListGrowthTasksInProgressStatus 真实响应里的 in_progress 必须被解析出来。
func TestListGrowthTasksInProgressStatus(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"tasks":[
		 {"task_code":"chat_5","accept_status":"in_progress","progress":{"current":3,"target":5}}
		]}}`), nil
	})
	tasks, err := c.ListGrowthTasks(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if tasks[0].AcceptStatus != TaskAcceptInProgress {
		t.Fatalf("accept_status=%q", tasks[0].AcceptStatus)
	}
	if tasks[0].NeedsAccept() {
		t.Error("进行中的任务不该被判定为需要报名")
	}
	if tasks[0].Claimable() {
		t.Error("3/5 不该可领奖")
	}
	if tasks[0].ProgressText() != "3/5" {
		t.Errorf("progress=%q", tasks[0].ProgressText())
	}
}

func TestListGrowthTasksBusinessError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(500, `{"code":500,"msg":"boom"}`), nil
	})
	_, err := c.ListGrowthTasks(&auth.Auth{AccessToken: "at", UID: "u1"})
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrServer {
		t.Fatalf("err=%v want ErrServer", err)
	}
}

func TestAcceptGrowthTasksSendsTaskCodes(t *testing.T) {
	var got []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			return nil, errors.New("want POST")
		}
		if r.URL.Path != "/v2/activity/growth/tasks/accept" {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		got, _ = io.ReadAll(r.Body)
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"results":[
			{"task_code":"chat_5","status":"accepted"},
			{"task_code":"template_5","status":"already_accepted"}]}}`), nil
	})
	res, err := c.AcceptGrowthTasks(&auth.Auth{AccessToken: "at", UID: "u1"}, []string{"chat_5", "template_5"})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	var body struct {
		TaskCodes []string `json:"task_codes"`
	}
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatalf("body: %v (%s)", err, got)
	}
	if len(body.TaskCodes) != 2 || body.TaskCodes[0] != "chat_5" {
		t.Errorf("task_codes=%v want [chat_5 template_5]", body.TaskCodes)
	}
	if len(res) != 2 || res[0].Status != TaskAcceptAccepted || res[1].Status != "already_accepted" {
		t.Errorf("results=%+v", res)
	}
}

// TestAcceptGrowthTasksIdempotent 幂等重放：已报名时上游返回
// status=already_accepted 且 HTTP 200，**不是**错误。
// 这正是一键完成可以无脑重放的前提。
func TestAcceptGrowthTasksIdempotent(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"results":[{"task_code":"chat_5","status":"already_accepted"}]}}`), nil
	})
	res, err := c.AcceptGrowthTasks(&auth.Auth{AccessToken: "at", UID: "u1"}, []string{"chat_5"})
	if err != nil {
		t.Fatalf("重复报名不应报错: %v", err)
	}
	if len(res) != 1 || res[0].Status != "already_accepted" {
		t.Errorf("results=%+v", res)
	}
}

// TestAcceptGrowthTasksEmptyIsNoop 空列表不应产生请求。
func TestAcceptGrowthTasksEmptyIsNoop(t *testing.T) {
	called := false
	c := testClient(func(r *http.Request) (*http.Response, error) {
		called = true
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	res, err := c.AcceptGrowthTasks(&auth.Auth{AccessToken: "at", UID: "u1"}, nil)
	if err != nil || res != nil {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if called {
		t.Error("空任务列表不应发起请求")
	}
}

func TestClaimGrowthTaskOnWebDomain(t *testing.T) {
	c := webTestClient(func(r *http.Request) (*http.Response, error) {
		if err := claimPath(r, "chat_5"); err != nil {
			return nil, err
		}
		if r.Body != nil {
			if b, _ := io.ReadAll(r.Body); len(b) > 0 {
				return nil, errors.New("claim 无请求体，实际收到 " + string(b))
			}
		}
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5}}`), nil
	})
	credit, energy, already, err := c.ClaimGrowthTask(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if already || credit != 100 || energy != 5 {
		t.Errorf("credit=%d energy=%d already=%v", credit, energy, already)
	}
}

// TestClaimGrowthTaskAlreadyClaimed 重复领奖是幂等的：
// 上游回 already_claimed=true，调用方应视为「已领过」而非错误。
func TestClaimGrowthTaskAlreadyClaimed(t *testing.T) {
	c := webTestClient(func(r *http.Request) (*http.Response, error) {
		// 这条断言来自真实响应：first_buddy 已领过时上游正是这样回的。
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"already_claimed":true,"credit":0,"energy":0}}`), nil
	})
	credit, energy, already, err := c.ClaimGrowthTask(&auth.Auth{AccessToken: "at", UID: "u1"}, "first_buddy")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !already || credit != 0 || energy != 0 {
		t.Errorf("already=%v credit=%d energy=%d", already, credit, energy)
	}
}

// TestClaimGrowthTaskIntlUsesIntlWebBase 国际版账号的成长中心在国际域，
// 不能把国服 workbuddy.cn 的请求发给国际账号。
func TestClaimGrowthTaskIntlUsesIntlWebBase(t *testing.T) {
	c := webTestClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "web-intl.example" {
			return nil, errors.New("国际版应走 WebBaseIntl, 实际 " + r.URL.Host)
		}
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"credit":1}}`), nil
	})
	if _, _, _, err := c.ClaimGrowthTask(&auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.workbuddy.ai"}, "chat_5"); err != nil {
		t.Fatalf("claim: %v", err)
	}
}

// TestClaimGrowthTaskEscapesCode 任务码进路径必须转义：
// 不转义时含 "/" 的码会把路径拼成另一条（多一级），打到不存在的端点上。
//
// 断言用 EscapedPath 而非 Path —— 后者是**解码后**的路径，%2F 会还原成 "/"，
// 用它判断转义与否会永远看不到区别。
func TestClaimGrowthTaskEscapesCode(t *testing.T) {
	var escaped string
	c := webTestClient(func(r *http.Request) (*http.Response, error) {
		escaped = r.URL.EscapedPath()
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	if _, _, _, err := c.ClaimGrowthTask(&auth.Auth{AccessToken: "at", UID: "u1"}, "a/b"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !strings.Contains(escaped, "a%2Fb") {
		t.Errorf("任务码未转义: %q", escaped)
	}
	if strings.Contains(escaped, "tasks/a/b/") {
		t.Errorf("任务码被拼成额外路径层级: %q", escaped)
	}
}

func TestClaimGrowthTaskEmptyCodeIsNoop(t *testing.T) {
	called := false
	c := webTestClient(func(r *http.Request) (*http.Response, error) {
		called = true
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	_, _, _, err := c.ClaimGrowthTask(&auth.Auth{AccessToken: "at", UID: "u1"}, "  ")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if called {
		t.Error("空任务码不应发起请求")
	}
}

// TestIsGrowthTaskNotCompleted 领奖被拒时区分「未达标」与「真故障」：
// 前者应等计分落定后重试，后者该放弃并留痕。
func TestIsGrowthTaskNotCompleted(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"普通错误", errors.New("boom"), false},
		{"400 未完成（英文）", &Error{Kind: ErrClient, Status: 400, Msg: "task not completed"}, true},
		{"400 未完成（实测文案）", &Error{Kind: ErrClient, Status: 400, Msg: "first_buddy task not completed yet"}, true},
		{"400 未完成（中文）", &Error{Kind: ErrClient, Status: 400, Msg: "任务未完成"}, true},
		{"500 不算未达标", &Error{Kind: ErrServer, Status: 500, Msg: "task not completed"}, false},
		{"401 会话失效", &Error{Kind: ErrSessionDead, Status: 401, Msg: "Offline user session not found"}, false},
	}
	for _, c := range cases {
		if got := IsGrowthTaskNotCompleted(c.err); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// TestGrowthTaskProgressTextClaimed 无进度字段的已领奖任务显示 claimed。
func TestGrowthTaskProgressTextClaimed(t *testing.T) {
	task := GrowthTask{AcceptStatus: TaskAcceptClaimed}
	if got := task.ProgressText(); got != "claimed" {
		t.Errorf("ProgressText=%q want claimed", got)
	}
}
