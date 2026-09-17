package growtask

import (
	"context"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestListTasksIsReadOnly 查询**绝不能有副作用**。
//
// 这条守的是一个很容易犯的错：把「列任务」实现成「顺手报名」。
// 那样用户每刷新一次「任务中心」页面都会产生写操作，既违反直觉也难排查
//（"我没点任何按钮，怎么账号状态变了"）。
func TestListTasksIsReadOnly(t *testing.T) {
	f := newFakeUpstream().
		add("chat_5", 0, 5, upstream.TaskAcceptNotAccepted).
		add("RichMeow_Chat", 0, 1, upstream.TaskAcceptAccepted)
	r := testRunner(f)

	views, err := r.ListTasks(context.Background(), testAuth())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("views=%d want 2", len(views))
	}
	// 关键断言：查询不得产生任何写操作。
	if len(f.acceptCalls) != 0 {
		t.Errorf("查询不该报名, got %v", f.acceptCalls)
	}
	if len(f.claimCalls) != 0 {
		t.Errorf("查询不该领奖, got %v", f.claimCalls)
	}
	if f.reports["/v2/report"] != 0 {
		t.Errorf("查询不该上报行为事件, got %d 次", f.reports["/v2/report"])
	}
}

// TestListTasksStatusMapping 展示状态到上游状态的映射。
func TestListTasksStatusMapping(t *testing.T) {
	f := newFakeUpstream().
		add("chat_5", 3, 5, upstream.TaskAcceptInProgress).   // 进行中
		add("RichMeow_Chat", 0, 1, upstream.TaskAcceptNotAccepted). // 未报名
		add("playbook_prompt", 1, 1, upstream.TaskAcceptAccepted).  // 已达标待领
		add("first_buddy", 1, 1, upstream.TaskAcceptClaimed).       // 已领奖
		add("Expert_Philanthropy", 0, 1, upstream.TaskAcceptAccepted) // 不可自动
	r := testRunner(f)

	views, err := r.ListTasks(context.Background(), testAuth())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byCode := map[string]TaskView{}
	for _, v := range views {
		byCode[v.Code] = v
	}

	if got := byCode["chat_5"]; got.Status != ViewInProgress || got.Progress != "3/5" {
		t.Errorf("chat_5: status=%s progress=%s want in_progress 3/5", got.Status, got.Progress)
	}
	if got := byCode["RichMeow_Chat"]; got.Status != ViewNotAccepted || !got.Automatable {
		t.Errorf("RichMeow_Chat: status=%s automatable=%v", got.Status, got.Automatable)
	}
	// 已达标 → claimable，且 claimable 的任务状态必须排在最前。
	if got := byCode["playbook_prompt"]; got.Status != ViewClaimable || !got.Claimable {
		t.Errorf("playbook_prompt: status=%s claimable=%v", got.Status, got.Claimable)
	}
	if got := byCode["first_buddy"]; got.Status != ViewClaimed || !got.Claimed {
		t.Errorf("first_buddy: status=%s claimed=%v", got.Status, got.Claimed)
	}
	// 不可自动的任务必须带人工指引，且不得被标为可自动。
	ph := byCode["Expert_Philanthropy"]
	if ph.Automatable {
		t.Error("Expert_Philanthropy 不该被标为可自动")
	}
	if ph.Status != ViewUnsupported || ph.Hint == "" {
		t.Errorf("不可自动任务应带指引: status=%s hint=%q", ph.Status, ph.Hint)
	}

	// 排序：待办在前、已领奖垫底。playbook_prompt(可领) 必须排第一，
	// first_buddy(已领奖) 必须排最后。
	if views[0].Code != "playbook_prompt" {
		t.Errorf("首个应是可领奖任务, got %s", views[0].Code)
	}
	if views[len(views)-1].Code != "first_buddy" {
		t.Errorf("末个应是已领奖任务, got %s", views[len(views)-1].Code)
	}
}

// TestListTasksMarksNeedsChat 消耗对话的任务必须被标记出来，
// 界面才能在做之前如实提示资源消耗。
func TestListTasksMarksNeedsChat(t *testing.T) {
	f := newFakeUpstream().
		add("expert_5", 0, 5, upstream.TaskAcceptAccepted).
		add("RichMeow_Chat", 0, 1, upstream.TaskAcceptAccepted)
	r := testRunner(f)

	views, err := r.ListTasks(context.Background(), testAuth())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, v := range views {
		switch v.Code {
		case "expert_5":
			if !v.NeedsChat {
				t.Error("expert_5 含真实对话，NeedsChat 应为 true")
			}
		case "RichMeow_Chat":
			if v.NeedsChat {
				t.Error("RichMeow_Chat 是纯事件上报，NeedsChat 应为 false")
			}
		}
	}
}

// TestPendingTasksExcludesClaimedAndUnsupported PendingTasks 是「一键完成」
// 的作用范围，必须排除已领奖与不可自动的任务。
func TestPendingTasksExcludesClaimedAndUnsupported(t *testing.T) {
	f := newFakeUpstream().
		add("chat_5", 0, 5, upstream.TaskAcceptAccepted).
		add("first_buddy", 1, 1, upstream.TaskAcceptClaimed).
		add("Expert_Philanthropy", 0, 1, upstream.TaskAcceptAccepted)
	r := testRunner(f)

	pending, err := r.PendingTasks(context.Background(), testAuth())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	for _, v := range pending {
		if v.Code == "first_buddy" {
			t.Error("已领奖任务不该出现在待办里")
		}
		if v.Code == "Expert_Philanthropy" {
			t.Error("不可自动任务不该出现在待办里")
		}
	}
	if len(pending) != 1 || pending[0].Code != "chat_5" {
		t.Errorf("pending=%+v", pending)
	}
}

// ---------------------------------------------------------------------------
// 多账号批量
// ---------------------------------------------------------------------------

// fleetTestAuth 造 n 个账号。
func fleetTestAuth(n int) []*auth.Auth {
	out := make([]*auth.Auth, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &auth.Auth{
			AccessToken: "at", UID: "uid-" + string(rune('a'+i)),
			Domain: "copilot.tencent.com",
		})
	}
	return out
}

// TestRunAllAccountsSkipsIntlByDefault 国际版账号默认跳过：
// 它的成长域是另一套任务集（无奖励），跑本包动作没有意义。
func TestRunAllAccountsSkipsIntlByDefault(t *testing.T) {
	f := newFakeUpstream().add("chat_5", 5, 5, upstream.TaskAcceptClaimed)
	r := testRunner(f)

	accounts := []*auth.Auth{
		{AccessToken: "at", UID: "cn-1", Domain: "copilot.tencent.com"},
		{AccessToken: "at", UID: "intl-1", Domain: "www.workbuddy.ai"},
	}
	results := r.RunAllAccounts(context.Background(), FleetOptions{Accounts: accounts})

	if len(results) != 2 {
		t.Fatalf("results=%d want 2", len(results))
	}
	if results[0].Skipped {
		t.Errorf("国服账号不该被跳过: %+v", results[0])
	}
	if !results[1].Skipped {
		t.Errorf("国际版账号应被跳过: %+v", results[1])
	}
	if results[1].Realm != auth.RealmGlobal {
		t.Errorf("realm=%s want global", results[1].Realm)
	}
	// 结果顺序必须与传入顺序一致（宿主按序渲染）。
	if results[0].UID != "cn-1" || results[1].UID != "intl-1" {
		t.Errorf("结果顺序与传入不一致: %s, %s", results[0].UID, results[1].UID)
	}
}

// TestRunAllAccountsIncludeIntl 显式开启时才处理国际版。
func TestRunAllAccountsIncludeIntl(t *testing.T) {
	f := newFakeUpstream().add("chat_5", 5, 5, upstream.TaskAcceptClaimed)
	r := testRunner(f)

	accounts := []*auth.Auth{{AccessToken: "at", UID: "intl-1", Domain: "www.workbuddy.ai"}}
	results := r.RunAllAccounts(context.Background(), FleetOptions{
		Accounts: accounts, IncludeIntl: true,
	})
	if results[0].Skipped {
		t.Errorf("开启 IncludeIntl 后不该跳过: %+v", results[0])
	}
}

// TestRunAllAccountsSkipsNoToken 无令牌的账号跳过并说明原因（需重新登录）。
func TestRunAllAccountsSkipsNoToken(t *testing.T) {
	f := newFakeUpstream()
	r := testRunner(f)

	results := r.RunAllAccounts(context.Background(), FleetOptions{
		Accounts: []*auth.Auth{{UID: "no-token", Domain: "copilot.tencent.com"}},
	})
	if !results[0].Skipped {
		t.Fatal("无令牌账号应跳过")
	}
	if results[0].SkipReason == "" {
		t.Error("跳过必须说明原因")
	}
}

// TestRunAllAccountsSkipsBusyAccount 账号正在被单账号入口执行时，
// 批量入口应跳过它而不是并发重跑（那会浪费真实对话额度）。
func TestRunAllAccountsSkipsBusyAccount(t *testing.T) {
	f := newFakeUpstream().add("chat_5", 5, 5, upstream.TaskAcceptClaimed)
	r := testRunner(f)

	a := &auth.Auth{AccessToken: "at", UID: "busy-1", Domain: "copilot.tencent.com"}
	if !r.Lock(a.UID) {
		t.Fatal("预置加锁失败")
	}
	defer r.Unlock(a.UID)

	results := r.RunAllAccounts(context.Background(), FleetOptions{Accounts: []*auth.Auth{a}})
	if !results[0].Skipped {
		t.Fatalf("忙碌账号应跳过: %+v", results[0])
	}
	if results[0].SkipReason != ErrAccountBusy.Error() {
		t.Errorf("skip_reason=%q want %q", results[0].SkipReason, ErrAccountBusy.Error())
	}
}

// TestRunAllAccountsConcurrency 并发执行时每个账号都被完整处理，
// 且结果顺序稳定。
func TestRunAllAccountsConcurrency(t *testing.T) {
	f := newFakeUpstream().add("chat_5", 5, 5, upstream.TaskAcceptClaimed)
	r := testRunner(f)

	accounts := fleetTestAuth(6)
	var mu sync.Mutex
	started := 0
	results := r.RunAllAccounts(context.Background(), FleetOptions{
		Accounts:    accounts,
		Concurrency: 3,
		OnAccount: func(*auth.Auth) {
			mu.Lock()
			started++
			mu.Unlock()
		},
	})

	if len(results) != 6 {
		t.Fatalf("results=%d want 6", len(results))
	}
	if started != 6 {
		t.Errorf("OnAccount 调用 %d 次 want 6", started)
	}
	// 顺序稳定：第 i 个结果必须对应第 i 个账号。
	for i, res := range results {
		if res.UID != accounts[i].UID {
			t.Errorf("结果[%d].UID=%s want %s", i, res.UID, accounts[i].UID)
		}
		if res.Skipped {
			t.Errorf("账号 %s 不该被跳过: %+v", res.UID, res)
		}
	}
}

// TestRunAllAccountsConcurrencyCapped 并发度必须被钳到上限：
// 每个账号的动作都可能含真实对话，并发过高会在上游形成异常流量画像。
func TestRunAllAccountsConcurrencyCapped(t *testing.T) {
	f := newFakeUpstream().add("chat_5", 5, 5, upstream.TaskAcceptClaimed)
	r := testRunner(f)

	// 传一个远超上限的值，只验证不崩且全部完成（上限本身是常量约束）。
	results := r.RunAllAccounts(context.Background(), FleetOptions{
		Accounts: fleetTestAuth(4), Concurrency: 99,
	})
	if len(results) != 4 {
		t.Fatalf("results=%d want 4", len(results))
	}
	for _, res := range results {
		if res.Skipped {
			t.Errorf("账号 %s 不该被跳过", res.UID)
		}
	}
}

// TestRunAllAccountsAggregatesClaims 领奖汇总必须准确：
// 界面顶部的「本轮 +N 积分」直接用它，算错会让用户以为奖励没到账。
func TestRunAllAccountsAggregatesClaims(t *testing.T) {
	f := newFakeUpstream().
		add("chat_5", 5, 5, upstream.TaskAcceptAccepted).
		add("RichMeow_Chat", 1, 1, upstream.TaskAcceptAccepted)
	r := testRunner(f)

	a := &auth.Auth{AccessToken: "at", UID: "agg-1", Domain: "copilot.tencent.com"}
	results := r.RunAllAccounts(context.Background(), FleetOptions{Accounts: []*auth.Auth{a}})

	res := results[0]
	if res.Claimed != 2 {
		t.Errorf("claimed=%d want 2", res.Claimed)
	}
	// 假上游对每次领奖都回 +100/+5。
	if res.Credit != 200 || res.Energy != 10 {
		t.Errorf("credit=%d energy=%d want 200/10", res.Credit, res.Energy)
	}

	sum := Summarize(results)
	if sum.Accounts != 1 || sum.Claimed != 2 || sum.Credit != 200 || sum.Energy != 10 {
		t.Errorf("summary=%+v", sum)
	}
}

// TestSummarizeCountsSkippedAndFailed 汇总要正确区分跳过与失败：
// 跳过是正常（区域不符/无令牌），失败才是需要用户关注的。
func TestSummarizeCountsSkippedAndFailed(t *testing.T) {
	results := []AccountResult{
		{UID: "a", Claimed: 1, Credit: 100, Energy: 5},
		{UID: "b", Skipped: true, SkipReason: "国际版"},
		{UID: "c", Error: "列表拉取失败"},
	}
	sum := Summarize(results)
	if sum.Accounts != 2 {
		t.Errorf("accounts=%d want 2（跳过的不计入）", sum.Accounts)
	}
	if sum.Skipped != 1 {
		t.Errorf("skipped=%d want 1", sum.Skipped)
	}
	if sum.Failed != 1 {
		t.Errorf("failed=%d want 1", sum.Failed)
	}
	if sum.Credit != 100 || sum.Energy != 5 {
		t.Errorf("credit=%d energy=%d", sum.Credit, sum.Energy)
	}
}

// TestRunAllAccountsReportsListFailure 整轮失败（列表拉不到）必须报到
// AccountResult.Error，界面才能提示"该账号本轮未执行成功"。
func TestRunAllAccountsReportsListFailure(t *testing.T) {
	f := newFakeUpstream()
	f.listFail = true
	r := testRunner(f)

	a := &auth.Auth{AccessToken: "at", UID: "fail-1", Domain: "copilot.tencent.com"}
	results := r.RunAllAccounts(context.Background(), FleetOptions{Accounts: []*auth.Auth{a}})

	if results[0].Error == "" {
		t.Error("整轮失败应记入 Error")
	}
	if results[0].Skipped {
		t.Error("整轮失败不是跳过（跳过是前置条件不符）")
	}
	if got := Summarize(results).Failed; got != 1 {
		t.Errorf("failed=%d want 1", got)
	}
}

// TestRunAllAccountsOnResultCallback 回调应被调用（界面用它实时更新进度）。
func TestRunAllAccountsOnResultCallback(t *testing.T) {
	f := newFakeUpstream().add("chat_5", 5, 5, upstream.TaskAcceptClaimed)
	r := testRunner(f)

	var mu sync.Mutex
	seen := 0
	r.RunAllAccounts(context.Background(), FleetOptions{
		Accounts: fleetTestAuth(3),
		OnResult: func(AccountResult) {
			mu.Lock()
			seen++
			mu.Unlock()
		},
	})
	if seen != 3 {
		t.Errorf("OnResult 调用 %d 次 want 3", seen)
	}
}

// TestRunAllAccountsEmptyInput 空输入不应崩，也不该 panic。
func TestRunAllAccountsEmptyInput(t *testing.T) {
	r := testRunner(newFakeUpstream())
	results := r.RunAllAccounts(context.Background(), FleetOptions{})
	if len(results) != 0 {
		t.Errorf("results=%d want 0", len(results))
	}
	if sum := Summarize(results); sum.Accounts != 0 {
		t.Errorf("summary=%+v", sum)
	}
}
