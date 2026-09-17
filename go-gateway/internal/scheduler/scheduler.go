// Package scheduler 定时任务：每日签到（09/21点，末尾顺带派猫/领奖）+ token keepalive（22点）。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/records"
	"workbuddy2api/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即两类任务都启用，
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	KeepaliveHours []int // 默认 [22]
	// ActivityHours 活跃上报时点，默认 [10]。
	//
	// 为什么要单独一个任务：签到只恢复余额，**连登天数**要靠对话活跃上报点亮。
	// 一条 chat_request_send 同时点亮连登 + 解锁 first_buddy（领养前置），
	// 因此它是「能领养」的前提。
	ActivityHours []int
	// TrialHours 国际版 trial 加油包领取时点，默认 [9, 21]（与签到同步）。
	//
	// 国际版没有签到/任务中心，trial 是其唯一的积分增益动作；
	// 上游用 14051 表达「已领过」，客户端视为幂等成功，故可每天重试。
	TrialHours []int
	// SchoolHours 开学季活动任务时点，默认 [12]。
	//
	// 该活动是**限时**的：服务端下发 in_period，下线后自动跳过，
	// 代码无需人工清理。只领取已达标的奖励，不伪造完成动作。
	SchoolHours []int
	// NightOwlHours 夜猫子任务时点，默认 [1]。
	//
	// growth 有个时段敏感任务只在夜猫窗口（23:00–08:00 CST）内计入，
	// 故单独排一个落在窗口内的时点（01 点避开 22 点的 token 保活）。
	NightOwlHours []int

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点，搭签到便车的猫猫旅行也随之停摆。
	CheckinDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// NightOwlDisabled 显式关闭夜猫子排程（schedule.nightowl_enabled=false）。
	NightOwlDisabled bool
	// SchoolDisabled 显式关闭开学季活动排程（schedule.school_enabled=false）。
	SchoolDisabled bool
	// TrialDisabled 显式关闭 trial 领取排程（schedule.trial_enabled=false）。
	TrialDisabled bool

	// ActivityReportCount 每个账号每日上报条数，默认 3（与官方客户端行为接近）。
	// 多条共用同一 conversationId，requestId 各自独立。
	ActivityReportCount int

	// CheckinScope 签到 + 猫猫旅行 + 活跃上报覆盖的账号区域："cn"（缺省，仅国服）/ "all"。
	//
	// 国际版（workbuddy.ai）的 billing 与 growth 接口暂无真实数据，默认跳过；
	// token 保活不受此限制（两个区域都需要刷新）。
	CheckinScope string

	// Records 账号记录写入器（nil = 不记录）。
	//
	// 为什么要它：这 4 个任务的日志此前只走 log.Printf 写 stdout，而宿主启动
	// 网关子进程时把 stdout/stderr 丢进了 Stdio::null —— 任务照跑，但界面上
	// 一条执行痕迹都没有（用户报的正是这个现象）。改为写宿主已经在读的
	// account_records.json，记录就能与签到并列出现在「账号记录 → 任务」里。
	Records *records.Recorder
}

// checkinScopeAllows 该账号是否参与签到与猫猫旅行。
func (s *Scheduler) checkinScopeAllows(a *auth.Auth) bool {
	if strings.EqualFold(strings.TrimSpace(s.cfg.CheckinScope), "all") {
		return true
	}
	return !upstream.IsIntl(a)
}

// 手动触发用的任务名。用字符串而非 taskKind 暴露给宿主：
// taskKind 是内部排程的实现细节（顺序会随排程重构变化），
// 而宿主（GUI/webui）需要的是稳定的对外标识。
const (
	TaskNameActivity = "activity"
	TaskNameNightOwl = "nightowl"
	TaskNameSchool   = "school"
	TaskNameTrial    = "trial"
	// TaskNameGrowthMap 活跃地图闭环（补签/兑换/抽奖/礼包）。
	//
	// 它没有独立排程时点（并入活跃上报，见 growthmap.go 顶部说明），
	// 但仍注册为可手动触发的任务名：日排程每个账号一天只跑一轮，
	// 用户想立刻确认「我的补签卡/抽奖次数有没有被处理」时需要一个入口。
	TaskNameGrowthMap = "growthmap"
)

// ErrTaskRunning 该任务已有一轮手动触发在执行中。
//
// 为什么需要去重：一轮活跃上报按账号数 × 条数 × 间隔串行跑，大账号池下可长达数分钟。
// 用户连点「立即执行」若不拦，会叠加出成倍的重复上报 —— 那正是活跃上报刻意
// 控制条数要规避的风控画像。
var ErrTaskRunning = errors.New("task already running")

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	//
	// growthClaimed 同一把锁下的「活跃地图当日已跑」标记（uid → 自然日 CST）。
	// 两类任务都是「每日一轮」的养号动作，用同一把锁即可：它们只在写各自 map 时短暂持有，
	// 不跨网络请求，不会把排程拖慢。
	mu             sync.Mutex
	adoptTried     map[string]string
	growthClaimed  map[string]string

	// taskMu/taskBusy 手动触发的在跑标记。与 mu 分开：排程循环会自动跑同一批任务，
	// 共用一把锁会让「手动触发」与「到点执行」互相阻塞，把定时任务拖慢。
	taskMu   sync.Mutex
	taskBusy map[string]bool
}

// TaskRunResult 手动触发一轮任务的结果。
//
// 为什么要回报「跑了没有」而不只是成功/失败：这些任务都有前置条件
// （夜猫子限时段、开学季限活动期、活跃上报与开学季只跑国服），
// 不满足时**跳过是正确行为**而非错误。宿主据此在界面上说明原因，
// 否则用户点了「立即执行」看不到任何变化，会以为功能坏了。
type TaskRunResult struct {
	Task string `json:"task"`
	// Ran 是否真的执行了一轮（false = 被前置条件挡下）。
	Ran bool `json:"ran"`
	// Skip 跳过原因码；Ran=true 时为空。
	//
	// 用稳定的英文码而非直接给中文：宿主可据此分支（如高亮提示），
	// 文案留给 Message，改文案不会破坏调用方的判断。
	Skip string `json:"skip,omitempty"`
	// Message 面向用户的中文说明，可直接显示在界面上。
	Message string `json:"message"`
}

// claimTask 尝试认领某个任务；已被认领时返回 false。
//
// 手动触发与到点排程共用一组标记：它们在跑同一批上游请求，若互不感知，
// 用户恰好在 10:00 点「立即执行」就会与排程那轮叠成双倍上报 ——
// 正是活跃上报刻意控制条数要规避的风控画像。
func (s *Scheduler) claimTask(name string) bool {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if s.taskBusy[name] {
		return false
	}
	s.taskBusy[name] = true
	return true
}

// releaseTask 归还认领。
func (s *Scheduler) releaseTask(name string) {
	s.taskMu.Lock()
	delete(s.taskBusy, name)
	s.taskMu.Unlock()
}

// RunTaskByName 手动触发一轮指定任务（供宿主「立即执行」按钮）。
//
// 只做「立刻跑一轮」，不检查 enabled 开关：开关管的是**后台是否自动排程**，
// 用户主动点击就该执行 —— 与宿主侧 checkin_all / travel_run 的既有语义一致。
//
// 未知任务名返回错误而非静默成功：宿主拼错名字时必须能看见，否则按钮点了没反应。
func (s *Scheduler) RunTaskByName(name string) (TaskRunResult, error) {
	switch name {
	case TaskNameActivity, TaskNameNightOwl, TaskNameSchool, TaskNameTrial, TaskNameGrowthMap:
	default:
		return TaskRunResult{}, fmt.Errorf("unknown task %q", name)
	}

	if !s.claimTask(name) {
		return TaskRunResult{
				Task: name, Skip: "already_running",
				Message: "该任务正在执行中，请稍后再试",
			},
			ErrTaskRunning
	}
	defer s.releaseTask(name)

	// 夜猫子只在夜猫窗口内计入：窗口外触发会被 runNightOwl 直接跳过，
	// 与其让用户白等一轮然后什么都没发生，不如提前回报原因。
	if name == TaskNameNightOwl && !withinNightWindow() {
		return TaskRunResult{
			Task:    name,
			Skip:    "outside_window",
			Message: "当前不在夜猫子时段（23:00–08:00 北京时间），上游不计入本次上报",
		}, nil
	}

	switch name {
	case TaskNameActivity:
		s.RunActivityNow()
	case TaskNameNightOwl:
		s.RunNightOwlNow()
	case TaskNameSchool:
		s.RunSchoolNow()
	case TaskNameTrial:
		s.RunTrialNow()
	case TaskNameGrowthMap:
		s.RunGrowthMapNow()
	}
	return TaskRunResult{Task: name, Ran: true, Message: "已触发一轮"}, nil
}

// runCareTask 到点执行一个养号任务，并与手动触发互斥。
//
// 与手动触发的唯一区别：排程这轮被占用时**记一行日志就走**，不排队。
// 排队会让「迟到唤醒 + 用户刚点过立即执行」叠成两轮；而下一个整点很快就会再来，
// 漏掉一轮的代价远小于双倍上报。
func (s *Scheduler) runCareTask(name string, run func()) {
	if !s.claimTask(name) {
		log.Printf("%s: skipped (a manual run is already in progress)", name)
		return
	}
	defer s.releaseTask(name)
	run()
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.NightOwlHours) == 0 {
		cfg.NightOwlHours = []int{1}
	}
	if len(cfg.SchoolHours) == 0 {
		cfg.SchoolHours = []int{12}
	}
	if len(cfg.TrialHours) == 0 {
		cfg.TrialHours = []int{9, 21}
	}
	if cfg.ActivityReportCount <= 0 {
		cfg.ActivityReportCount = defaultActivityReportCount
	}
	return &Scheduler{
		cfg:           cfg,
		adoptTried:    make(map[string]string),
		growthClaimed: make(map[string]string),
		taskBusy:      make(map[string]bool),
	}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskKeepalive
	taskActivity
	taskNightOwl
	taskSchool
	taskTrial
)

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !s.cfg.CheckinDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.CheckinHours), taskCheckin})
	}
	if !s.cfg.KeepaliveDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.KeepaliveHours), taskKeepalive})
	}
	if !s.cfg.ActivityDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.ActivityHours), taskActivity})
	}
	if !s.cfg.NightOwlDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.NightOwlHours), taskNightOwl})
	}
	if !s.cfg.SchoolDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.SchoolHours), taskSchool})
	}
	if !s.cfg.TrialDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.TrialHours), taskTrial})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 所有任务全部禁用：不空转，只等退出信号。
			<-ctx.Done()
			return
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			for _, k := range kinds {
				switch k {
				case taskCheckin:
					s.RunCheckinNow()
				case taskKeepalive:
					s.RunKeepaliveNow()
				case taskActivity:
					s.runCareTask(TaskNameActivity, func() { s.runActivity(ctx) })
				case taskNightOwl:
					s.runCareTask(TaskNameNightOwl, func() { s.runNightOwl(ctx) })
				case taskSchool:
					s.runCareTask(TaskNameSchool, func() { s.runSchool(ctx) })
				case taskTrial:
					s.runCareTask(TaskNameTrial, func() { s.runTrial(ctx) })
				}
			}
		}
	}
}

// DefaultCreditRefreshInterval 积分到期巡检的默认周期。
//
// 为什么需要独立于签到的高频巡检：到期日决定选号优先级，而它会随消费变化
// （快过期的额度烧完后，该账号的最近到期日跳到下一档，应立刻让出流量）。
// 签到每天只跑两次，间隔太久会让分层选号长期依据过时数据。
// 15 分钟 × 账号数 的请求量相对上游可忽略，且巡检本身不签到、不改账号状态。
const DefaultCreditRefreshInterval = 15 * time.Minute

// creditRefreshGap 巡检账号之间的间隔，避免瞬间并发打满上游。
const creditRefreshGap = 300 * time.Millisecond

// RunCreditRefreshLoop 周期性刷新所有账号的积分余额与到期日，阻塞直到 ctx 取消。
//
// interval <= 0 时用 DefaultCreditRefreshInterval。
func (s *Scheduler) RunCreditRefreshLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultCreditRefreshInterval
	}
	// 启动先跑一轮：否则重启后要等一个周期才拿到到期日，
	// 这段时间内分层选号只能依赖 state.json 里持久化的旧值。
	s.refreshCreditsWithGap(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refreshCreditsWithGap(ctx)
		}
	}
}

// refreshCreditsWithGap 跑一轮积分巡检，账号之间留出间隔；ctx 取消时提前退出。
func (s *Scheduler) refreshCreditsWithGap(ctx context.Context) {
	for i, st := range s.cfg.Pool.List() {
		if ctx.Err() != nil {
			return
		}
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(creditRefreshGap):
			}
		}
		info, err := s.cfg.Upstream.UserResourceDetail(a)
		if err != nil {
			log.Printf("credit-refresh %s: %v", st.UID, err)
			continue
		}
		s.cfg.Pool.SetCreditsAndExpiry(st.UID, info.Remain, info.SoonestExpireAt)
		if info.Remain > 0 {
			// 余额恢复的账号顺带复活，避免硬冷却的号空等到下一个签到时点。
			s.cfg.Pool.ReenableIfCredits(st.UID, info.Remain)
		}
	}
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻，末尾顺带跑一趟猫猫旅行。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
// 区域范围（cfg.CheckinScope，默认仅国服）之外的账号跳过签到与旅行。
//
// 旅行搭签到便车而非独立排程：每日上限按「派出」计 1 次/天且在派出时锁定奖励，
// 晚领不丢分，故分钟粒度巡检无增益，与签到时点（09/21 点）合并执行即可。
// 注意顺序：先签到解冻，旅行才能覆盖到本轮刚恢复的账号。
func (s *Scheduler) RunCheckinNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		if !s.checkinScopeAllows(a) {
			continue
		}
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			log.Printf("checkin %s: %v", st.UID, err)
			// 已签到等业务错误也继续走余额查询
		}
		info, err := s.cfg.Upstream.UserResourceDetail(a)
		if err != nil {
			log.Printf("user-resource %s: %v", st.UID, err)
			continue
		}
		// 一次请求同时取回余额与「最近到期」：到期日驱动账号池的分层选号，
		// 顺带回写凭证文件，让宿主（workbuddy-switch）也能看到最新到期信息。
		s.cfg.Pool.SetCreditsAndExpiry(st.UID, info.Remain, info.SoonestExpireAt)
		s.cfg.Pool.ReenableIfCredits(st.UID, info.Remain)
	}
	// 签到收尾（09/21 点）：顺带推进一趟旅行状态机（领养 / 派出 / 领奖）。
	s.RunTravelNow()
}

// RunCreditRefreshNow 立即刷新所有账号的积分余额与到期日（不签到、不解冻）。
//
// 与签到的分工：签到是「每天两次」的重操作（含旅行），而到期日会随消费实时变化，
// 需要更高频地刷新才能让分层选号跟上（某账号把快过期额度烧完后，
// 它的最近到期日会跳到下一档，此时就应让出流量给更紧迫的账号）。
func (s *Scheduler) RunCreditRefreshNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		info, err := s.cfg.Upstream.UserResourceDetail(a)
		if err != nil {
			log.Printf("credit-refresh %s: %v", st.UID, err)
			continue
		}
		s.cfg.Pool.SetCreditsAndExpiry(st.UID, info.Remain, info.SoonestExpireAt)
		// 余额耗尽时不在此解冻（那是签到的职责）；但余额恢复的账号顺带复活，
		// 避免硬冷却的号要等到下一个签到时点才回到池中。
		if info.Remain > 0 {
			s.cfg.Pool.ReenableIfCredits(st.UID, info.Remain)
		}
	}
}

// RunKeepaliveNow 立即对所有账号刷新 token。
//
// session 死亡走**连续计数**语义（见 pool.NoteSessionDead）：一次刷新失败不再
// 立即杀号 —— 12153 会被网络抖动/上游闪断临时触发，一次即禁用会误杀健康账号。
// 连续 pool.SessionDeadThreshold() 次才禁用；刷新成功则清零计数，
// 因此被误判的账号有复活路径。
func (s *Scheduler) RunKeepaliveNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("keepalive %s: session dead x%d, disabled (re-login required)",
						uid8(st.UID), pool.SessionDeadThreshold())
				} else {
					// 未达阈值：保留账号，下轮再判（误判防护）
					log.Printf("keepalive %s: session dead %d/%d (not disabling yet): %v",
						uid8(st.UID), s.cfg.Pool.SessionDeadFails(st.UID),
						pool.SessionDeadThreshold(), err)
				}
			} else {
				log.Printf("keepalive %s: %v", uid8(st.UID), err)
			}
			continue
		}
		// 刷新成功 = 账号确实未死：清零连续 12153 计数（复活路径）。
		s.cfg.Pool.ClearSessionDead(st.UID)
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", uid8(st.UID), err)
		}
	}
}
