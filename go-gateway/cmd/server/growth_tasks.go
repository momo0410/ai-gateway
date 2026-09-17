// growth_tasks.go 成长任务「一键完成」的组装层。
//
// 职责边界：本文件只做**接线**（把池里的账号解析成 growtask 需要的形状、
// 把 HTTP 层的 accountId 翻译成 uid），编排逻辑全在 internal/growtask。
// 与 handler.go 的 GrowthTaskAPI 回调面配合，让 server 包不必 import growtask。
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/growtask"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/records"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/upstream"
)

// growTaskTimeout 单次 HTTP 请求允许的最长执行时间。
//
// 为什么给到 10 分钟：跑全部待办时每个账号都可能包含**真实对话**
// （默认 chatGap 6s / reportGap 1.05s），17 个任务串行下来分钟级是常态。
// 给短了会在中途被切断，而任务已经执行了一半 —— 半途而废比慢更糟。
const growTaskTimeout = 10 * time.Minute

// growthTaskAdapter 实现 server.GrowthTaskAPI。
type growthTaskAdapter struct {
	runner   *growtask.Runner
	p        *pool.Pool
	recorder *records.Recorder
}

// newGrowthTaskAPI 组装成长任务入口。
//
// recorder 可能为 nil（未配置 account_records.file 时）：growtask 内部对 nil
// recorder 是安全的（记录是旁路观测数据），照传即可。
func newGrowthTaskAPI(p *pool.Pool, up *upstream.Client, recorder *records.Recorder) *server.GrowthTaskAPI {
	adapter := &growthTaskAdapter{
		runner:   growtask.New(growtask.Options{Upstream: up, Records: recorder}),
		p:        p,
		recorder: recorder,
	}
	return &server.GrowthTaskAPI{
		List:   adapter.list,
		RunOne: adapter.runOne,
		RunAll: adapter.runAll,
	}
}

// lookup 把宿主账号 id 解析成池里的 auth。
//
// 为什么需要这层翻译：宿主界面按**账号库 id** 发起请求，而池按 **uid** 索引，
// 两者是不同的字符串（id 是账号库内部的随机标识，同一账号重新采集后会变）。
// 不做翻译会「找不到账号」，而且是静默的 —— 界面只会显示一条错误，
// 排查时完全看不出是 id/uid 弄混了。
//
// 回退顺序：先 id→uid 映射，再当 uid 直查。后者是为独立运行网关
// （没有宿主传 identities）保留的 —— 那种场景下界面拿不到 id，传的就是 uid。
func (a *growthTaskAdapter) lookup(accountID string) (*auth.Auth, error) {
	key := accountID

	// 1) 尝试 id → uid
	if index := a.recorder.IdentityIndex(); index != nil {
		for uid, id := range index {
			if id.ID == accountID {
				key = uid
				break
			}
		}
	}

	// 2) 按 uid 直查（同时覆盖上面翻译成功与「本来就是 uid」两种情况）
	if acc := a.p.AuthByUID(key); acc != nil {
		return acc, nil
	}

	// 3) 仍未命中：给出可操作的提示，而不是干巴巴说找不到。
	//    最常见成因是「账号库里有但这个账号不在网关池中」（禁用 / 需重登 / 未同步）。
	return nil, fmt.Errorf("账号不在网关池中（id=%s）：可能是已禁用、需重新登录，或尚未同步到网关", accountID)
}

// list 列出某账号的成长任务（只读）。
func (a *growthTaskAdapter) list(accountID string) (any, error) {
	acc, err := a.lookup(accountID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), growTaskTimeout)
	defer cancel()
	tasks, err := a.runner.ListTasks(ctx, acc)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"accountId": accountID,
		"tasks":     tasks,
		"total":     len(tasks),
	}, nil
}

// runOne 执行单账号的单任务；code 为空表示「跑该账号的全部待办」。
func (a *growthTaskAdapter) runOne(accountID, code string) (any, error) {
	acc, err := a.lookup(accountID)
	if err != nil {
		return nil, err
	}
	// 账号级互斥：防连点造成重复消耗（任务动作里有真实对话与领奖）。
	// 占用中直接拒绝而不是排队 —— 排队会让第二次点击在几十秒后突然执行，
	// 用户早已离开页面，反而更容易误以为「没反应就多点几次」。
	if !a.runner.Lock(acc.UID) {
		return nil, errors.New("该账号的任务正在执行中，请稍候")
	}
	defer a.runner.Unlock(acc.UID)

	ctx, cancel := context.WithTimeout(context.Background(), growTaskTimeout)
	defer cancel()

	if code == "" {
		items := a.runner.RunAll(ctx, acc)
		return map[string]any{
			"accountId": accountID,
			"items":     items,
			"total":     len(items),
		}, nil
	}
	item := a.runner.RunOne(ctx, acc, code)
	return map[string]any{"accountId": accountID, "item": item}, nil
}

// runAll 对所有账号跑一轮。
//
// 默认不含国际版（growtask 的 IncludeIntl 默认 false）：国际版的成长域是
// 另一套任务集（无奖励字段、无 progress），本包的动作对它无意义（实测确认）。
// 这与活跃上报的 checkin_scope 默认 cn 一致。
func (a *growthTaskAdapter) runAll() (any, error) {
	accounts := a.poolAuths()
	if len(accounts) == 0 {
		return nil, errors.New("账号池为空，请先启动网关或同步账号")
	}
	ctx, cancel := context.WithTimeout(context.Background(), growTaskTimeout)
	defer cancel()
	results := a.runner.RunAllAccounts(ctx, growtask.FleetOptions{
		Accounts:    accounts,
		Concurrency: 2,
	})
	return map[string]any{
		"results": results,
		"summary": growtask.Summarize(results),
	}, nil
}

// poolAuths 取出池里全部未禁用的账号。
//
// 跳过禁用账号：禁用语义是「不进账号池」，而成长任务要在池里跑才有凭据。
func (a *growthTaskAdapter) poolAuths() []*auth.Auth {
	out := make([]*auth.Auth, 0, 16)
	for _, st := range a.p.List() {
		if st.Disabled {
			continue
		}
		if acc := a.p.AuthByUID(st.UID); acc != nil {
			out = append(out, acc)
		}
	}
	return out
}
