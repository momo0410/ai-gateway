// nightowl.go 夜猫子任务：在夜猫时段（23:00–08:00 CST）补一次 black_cat 任务。
//
// 背景：growth 成长任务里有一个时段敏感的 black_cat 任务，只在夜间窗口内计入。
// 白天跑无效，因此单独排一个 01 点的时点（落在窗口内、又避开 22 点的 token 保活）。
//
// 与活跃上报的关系：两者都发 chat_request_send 事件，形状完全相同，
// 只是**触发时机**与**条数**不同 —— 活跃上报每天 10 点发若干条点亮连登；
// 夜猫子只在窗口内补 1 条以点亮 black_cat。
// 因此复用 ReportChatActivity，不重复实现事件构造。
package scheduler

import (
	"context"
	"fmt"
	"log"
	"time"

	"workbuddy2api/internal/records"
)

// nightWindowStartHour / nightWindowEndHour 夜猫时段（CST）：[23:00, 08:00)。
//
// 用 CST（UTC+8）而非本地时区：窗口定义来自上游活动规则，
// 换台机器/改系统时区不应改变判定结果。
const (
	nightWindowStartHour = 23
	nightWindowEndHour   = 8
	cstOffsetSeconds     = 8 * 3600
)

// cstNow 返回 CST 时区的当前时间。
//
// 不依赖 time.LoadLocation("Asia/Shanghai")：Windows 上若无 tzdata 会失败，
// 而固定偏移已足够（中国自 1991 年起不再使用夏令时）。
func cstNow() time.Time {
	return time.Now().UTC().Add(cstOffsetSeconds * time.Second)
}

// withinNightWindow 当前是否处于夜猫时段。
func withinNightWindow() bool {
	return withinNightWindowAt(cstNow())
}

// withinNightWindowAt 判定给定时刻（CST）是否在夜猫时段内。
//
// 抽成带参函数是为了**可测**：直接用 time.Now() 的版本只能在恰好处于边界的
// 时刻验证，测试若自己重抄一遍判定逻辑就成了假测试（实现改错也照样通过）。
func withinNightWindowAt(cst time.Time) bool {
	h := cst.Hour()
	return h >= nightWindowStartHour || h < nightWindowEndHour
}

// RunNightOwlNow 立即执行一轮夜猫子任务（供 CLI 一次性触发、测试）。
func (s *Scheduler) RunNightOwlNow() {
	s.runNightOwl(context.Background())
}

// runNightOwl 夜猫子遍历。
//
// 只在夜猫窗口内生效：排程已把它放在 01 点，但用户可能手工触发、
// 或机器休眠后迟到唤醒 —— 窗口外执行毫无意义（上游不计入），
// 因此这里再判一次，避免做无用请求。
func (s *Scheduler) runNightOwl(ctx context.Context) {
	if !withinNightWindow() {
		log.Printf("nightowl: skipped (outside 23:00-08:00 CST window)")
		// 窗口外跳过是「明确且原因重要」的：用户点了「立即执行」却什么都没发生，
		// 若不留痕就只能猜测（这正是本功能最初的问题 —— 日志进了 Stdio::null）。
		// 用 TaskAllDaily：这是整轮条件、与具体账号无关，逐账号各写一条等于刷屏。
		s.cfg.Records.TaskAllDaily("夜猫子任务", records.ResultInfo,
			"当前不在夜猫时段（23:00–08:00 北京时间），上游不计入本次上报")
		return
	}

	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		// 与活跃上报共用区域开关（国际版 growth 域不可用）。
		if !s.checkinScopeAllows(a) {
			continue
		}
		if !first {
			if !sleepCtx(ctx, activityAccountDelay) {
				return
			}
		}
		first = false

		cid := fmt.Sprintf("wb2api-night-%d", time.Now().UnixMilli())
		if err := s.cfg.Upstream.ReportChatActivity(a, cid, cid+"-r1"); err != nil {
			log.Printf("nightowl %s: report failed: %v", uid8(a.UID), err)
			// 失败按天去重：排程只有 01 点一次，但机器休眠后补跑、
			// 用户手工触发都会让同一失败重复出现。
			s.cfg.Records.TaskDaily(a.UID, "夜猫子任务", records.ResultFailed, err.Error())
			continue
		}
		log.Printf("nightowl %s: black_cat reported ok", uid8(a.UID))
		// 成功即「有新变化」：black_cat 是时段敏感任务，本轮上报就是它的点亮动作。
		s.cfg.Records.Task(a.UID, "夜猫子任务", records.ResultSuccess,
			"已在夜猫时段补报一次（点亮 black_cat 任务）")
	}
}
