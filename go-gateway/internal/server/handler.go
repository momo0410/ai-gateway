// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权
	MaxRotate int    // 单请求最多换号次数，默认 3
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429 冷却，默认 60s
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m
	// Usage Token 用量统计器（可选；nil = 不统计，/usage 返回 enabled=false）。
	Usage *usage.Stats
	// RunTask 手动触发一轮养号任务（活跃上报 / 夜猫子 / 开学季 / trial）。
	//
	// 用回调而非直接持有 *scheduler.Scheduler：server 包不该依赖调度器的
	// 内部结构（Config 字段、排程循环），只借一个「按名字跑一轮」的入口，
	// 依赖方向仍是 main 组装、server 消费。nil = 该能力不可用（如单测）。
	RunTask func(name string) (scheduler.TaskRunResult, error)


	// GrowthTasks 成长任务「一键完成」能力的回调。
	//
	// 与 RunTask 同样的依赖倒置理由：server 包不该依赖 growtask 的内部结构
	// （Runner 的编排、动作注册表），只借三个入口。nil = 该能力不可用（如单测）。
	//
	// 为什么需要它：这 17 个成长任务的实现此前只存在于库里、没有任何对外入口，
	// 因此被链接器的死代码消除剔出了二进制 —— 表现为「代码写了但根本调不到」。
	GrowthTasks *GrowthTaskAPI

	// AllowedModels 「限制使用的模型」白名单：**非空时只放行列表内的模型**，
	// 其余一律拒绝；空（或 nil）= 不限制（默认，向后兼容）。
	//
	// 三个工作模式（自动 / 积分轮转 / 手动）共用同一份白名单 —— 它限制的是
	// 「网关放行哪些模型」，与「用哪些账号」是正交的两件事，所以不该只在
	// 轮转模式下生效。轮转模式尤其依赖它：轮转的语义是「把这个账号的某个
	// 模型额度烧干净再换下一个账号」，模型是策略的一部分，不锁定的话客户端
	// 换个模型就能绕过轮转策略，账号选择与额度消耗都会变得不可预期。
	//
	// 大小写不敏感比较；元素两侧空白被忽略；元素自带 `cn:` / `global:` 前缀
	// 时先剥掉再比较（用户既可能在界面上选到裸名，也可能手写带前缀的名字）。
	// 由 NewHandler 归一化后缓存到 Handler.allowed，请求路径上零分配。
	AllowedModels []string

	// AllowedModel 已废弃的**单值**写法（历史字段，仅为向后兼容保留）。
	//
	// 老配置里可能是 `pool.allowed_model: "deepseek-v4.1-flash"` 这样的字符串，
	// 调用方（main.go）读出来塞在这里。NewHandler 会把它并进 AllowedModels，
	// 因此老配置的行为逐字不变。**新代码一律用 AllowedModels。**
	AllowedModel string

	// PromptMode 系统提示词替换模式："passthrough"（缺省）/ "custom"。
	//
	// 空串按 passthrough 处理（NewHandler 里兜底）：这是**新增能力**，
	// 既有的调用方与老配置都没有这个字段，必须保持「透传客户端原始 system」
	// 的既有行为不变。
	PromptMode string
	// PromptText custom 模式下注入的自有系统提示词文本（来自 config.PromptText）。
	//
	// 在配置解析阶段一次性读盘并缓存，请求路径只做内存改写（不做文件 IO）。
	PromptText string
}

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux
	// allowed 「限制使用的模型」白名单（已归一化：剥前缀、去空白）。
	//
	// 在 NewHandler 里算一次并缓存，而不是每个请求现算：归一化会分配，
	// 而请求路径上（forwardChat 每次调用）做这件事纯属浪费 —— 白名单
	// 只在进程启动时定一次，运行期不会变（改了配置要重启网关）。
	//
	// 空（nil 或零长度）= 不限制，这是默认值也是老配置的行为。
	allowed []string
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	// 缺省 passthrough：未显式配置时严格保持既有行为（透传客户端原始 system）。
	// 直接在 Config 上兜底而不是依赖调用方传对，是为了让「忘记传」也不可能
	// 变成「静默替换别人的 system」——后者的破坏性远大于一个默认值。
	if strings.TrimSpace(cfg.PromptMode) == "" {
		cfg.PromptMode = prompt.ModePassthrough
	}
	// 白名单归一化：多值字段 + 老配置的单值字段**合并**（不是二选一）。
	//
	// 为什么合并而不是「多值非空就忽略单值」：宿主升级过程中可能出现两者
	// 同时被写进配置的中间态（新的写多值、旧的单值还留在文件里）。若此时
	// 忽略单值，用户原来锁定的那个模型会**静默失效** —— 表现为「升级后
	// 限制突然不管用了」，是安全方向的错误，宁可多放行一个也不能漏。
	h := &Handler{
		cfg:     cfg,
		mux:     http.NewServeMux(),
		allowed: normalizeAllowedModels(append(append([]string{}, cfg.AllowedModels...), cfg.AllowedModel)),
	}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responses))
	h.mux.HandleFunc("POST /responses", h.withAuth(h.responses))
	h.mux.HandleFunc("POST /v1/messages", h.withAuth(h.messages))
	h.mux.HandleFunc("POST /messages", h.withAuth(h.messages))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	// /v1/models/regions 是给人看的「按区域的能力真值」对比视图。
	// 必须注册在 /v1/models 之后（Go 1.22 ServeMux 按具体度优先，顺序无关，
	// 但显式排在后面读起来更清楚它是 /v1/models 的补充而非替代）。
	h.mux.HandleFunc("GET /v1/models/regions", h.withAuth(h.modelsRegions))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	// /debug/* 同样走鉴权：它透出账号 UID 与域名，属敏感信息。
	h.mux.HandleFunc("GET /debug/", h.withAuth(h.debugHandler))
	h.mux.HandleFunc("GET /usage", h.withAuth(h.usageReport))
	// 养号任务手动触发：与 /status 同用 withAuth —— 它会向上游发真实请求，
	// 未鉴权暴露等于给人一个刷账号活跃度的开关。
	h.mux.HandleFunc("POST /tasks/run", h.withAuth(h.tasksRun))
	h.mux.HandleFunc("POST /tasks/growth", h.withAuth(h.growthTasks))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// withAuth 校验客户端凭据。
//
// 同时接受两种头部形态，覆盖不同客户端的认证习惯：
//   - Authorization: Bearer <key> —— OpenAI SDK、Claude Code 的 ANTHROPIC_AUTH_TOKEN、
//     Claude Desktop 3P（inferenceGatewayAuthScheme=bearer）
//   - x-api-key: <key>            —— Anthropic SDK、Claude Code 的 ANTHROPIC_API_KEY
func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.authorized(r) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

// authorized 判断请求是否携带了正确的网关密钥；未配置密钥时一律放行。
func (h *Handler) authorized(r *http.Request) bool {
	if h.cfg.APIKey == "" {
		return true
	}
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		return strings.TrimPrefix(authz, "Bearer ") == h.cfg.APIKey
	}
	return r.Header.Get("x-api-key") == h.cfg.APIKey
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy": healthy,
		"total":   total,
		"service": ServiceName,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":        h.cfg.Pool.List(),
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
	})
}

// GrowthTaskAPI 成长任务能力的最小接口面。
//
// 刻意用回调/窄接口而不是 import growtask 的具体类型：与 RunTask 同一条
// 依赖倒置原则 —— server 只负责 HTTP 编解码与错误映射，编排逻辑留在 growtask 包。
// 字段全是函数，main 组装时按需注入；nil 字段对应「该能力不可用」。
type GrowthTaskAPI struct {
	// List 列出某账号的成长任务（只读，无副作用）。参数是宿主账号库的 id。
	List func(accountID string) (any, error)
	// RunOne 对单账号执行单个任务。code 为空表示「跑该账号的全部待办」。
	RunOne func(accountID, code string) (any, error)
	// RunAll 对所有国服账号跑一轮（整轮，耗时可到分钟级）。
	RunAll func() (any, error)
}

// growthTasks 成长任务入口（POST /tasks/growth，body: {"action":"list|run","accountId":"...","taskCode":"..."}）。
//
// 为什么用单一路由 + action 而不是三条路由：这三个动作共享同一个账号解析与
// 忙碌判定，拆开会把「账号没找到 / 账号正忙」的错误映射抄三遍，
// 而它们必须完全一致（否则三个入口对同一状况给出不同提示）。
func (h *Handler) growthTasks(w http.ResponseWriter, r *http.Request) {
	if h.cfg.GrowthTasks == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "growth_tasks_unavailable",
			"growth task runner not configured on this gateway instance")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req struct {
		Action    string `json:"action"`
		AccountID string `json:"accountId"`
		TaskCode  string `json:"taskCode"`
	}
	if err := jsonUnmarshal(string(body), &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}

	api := h.cfg.GrowthTasks
	var (
		res any
		err error
	)
	switch strings.TrimSpace(req.Action) {
	case "list":
		if api.List == nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "growth_tasks_unavailable", "list not configured")
			return
		}
		if strings.TrimSpace(req.AccountID) == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing accountId")
			return
		}
		res, err = api.List(strings.TrimSpace(req.AccountID))
	case "run":
		if api.RunOne == nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "growth_tasks_unavailable", "run not configured")
			return
		}
		if strings.TrimSpace(req.AccountID) == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing accountId")
			return
		}
		res, err = api.RunOne(strings.TrimSpace(req.AccountID), strings.TrimSpace(req.TaskCode))
	case "run-all":
		if api.RunAll == nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "growth_tasks_unavailable", "run-all not configured")
			return
		}
		res, err = api.RunAll()
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			`unknown action (want "list" | "run" | "run-all")`)
		return
	}

	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "growth_task_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// tasksRun 手动触发一轮养号任务（POST /tasks/run，body: {"task":"activity"}）。
//
// 为什么要有这个入口：这 4 个任务此前只有「按点自动跑」，用户既看不见执行结果、
// 也没法在改完配置后立刻验证。手动触发是**可观测性**的一部分。
//
// 返回 200 + ran=false 表示「被前置条件挡下」（如夜猫子不在时段内）——
// 这是正常状态而非错误，宿主界面据此给出人话说明；真正的问题（未知任务名）
// 才返回 400。
func (h *Handler) tasksRun(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("task"))
	if name == "" {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		var parsed struct {
			Task string `json:"task"`
		}
		if err := jsonUnmarshal(string(body), &parsed); err == nil {
			name = strings.TrimSpace(parsed.Task)
		}
	}
	if name == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing task name")
		return
	}
	if h.cfg.RunTask == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "tasks_unavailable",
			"task runner not configured on this gateway instance")
		return
	}
	res, err := h.cfg.RunTask(name)
	if errors.Is(err, scheduler.ErrTaskRunning) {
		// 与宿主 checkin_all 的 already_running 同一语义：不是故障，是防重入。
		writeJSON(w, http.StatusOK, res)
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// usageReport 返回网关累计 Token 用量（GET /usage?days=N，days 省略或 0 = 全部）。
//
// 数据来源是网关自己记录的每次成功请求的上游 usage，与本地客户端日志统计相互独立。
// 未装配统计器时返回 enabled=false，让宿主能区分「网关没开统计」与「统计为空」。
func (h *Handler) usageReport(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Usage == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":     false,
			"generatedAt": time.Now().UnixMilli(),
		})
		return
	}
	days := 0
	if raw := r.URL.Query().Get("days"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			days = n
		}
	}
	snapshot := h.cfg.Usage.Snapshot(days)
	snapshot["enabled"] = true
	writeJSON(w, http.StatusOK, snapshot)
}

// recordUsage 把一次请求采集到的完整用量写入统计；无计量或未装配统计时跳过。
// 只统计成功请求（上游返回了可用 usage 的请求），失败请求不计入。
func (h *Handler) recordUsage(s *chatStat) {
	if h.cfg.Usage == nil || !s.hasCounters {
		return
	}
	h.cfg.Usage.Record(s.uid, s.model, s.counters)
}

// withImageCapability 给静态表条目补上能力字段。
//
// 静态表取自 /v3/config 的 agents[cli].models，实测（2026-09-16，国服 16 个
// cli 模型 + 国际版 21 个模型池）该清单下**全部**模型 supportsImages=true，
// 因此统一标注为支持图片。
//
// 只影响「上游不可达、回退静态表」时的结果：动态拉取成功时用上游真值。
// 不标注的话，回退期间客户端会把所有模型当纯文本 —— 图片能力整个消失，
// 而这恰恰是最难排查的一类问题（网关看起来完全正常）。
func withImageCapability(entries []map[string]any) []map[string]any {
	yes := true
	for _, m := range entries {
		for k, v := range modelCapabilityFields(&yes) {
			if _, exists := m[k]; !exists {
				m[k] = v
			}
		}
	}
	return entries
}

// 静态 CN 模型表（api-reference §5，动态接口失败时的回退）。
//
// 已按 /v3/config 的 data.agents[name=="cli"].models 校正（实测 2026-09-16，
// 共 16 个）。此前的表已明显过期：缺 glm-5.3 / glm-5.3-flash / auto /
// hy4-preview / hy3-x / kimi-k3-1 / kimi-k2.8-preview / deepseek-v4.1-flash，
// 却留着上游已下架的 hy3-preview / hy3-preview-agent / deepseek-v4-flash。
//
// 过期会**真的出错**，不只是展示问题：能力真值表用这张表兜底，
// 若它缺了 glm-5.3，网关就会认为「国服没有 glm-5.3」，于是把带图片的
// glm-5.3 请求判成「只能去国际版」—— 正好与事实相反（实测只有国服能读图）。
// 也就是说，静态表错误会把图片请求推向读不到图的那一侧。
var staticModels = withImageCapability([]map[string]any{
	{"id": "auto", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 256000},
	{"id": "hy4-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
	{"id": "hy3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 192000},
	{"id": "hy3-x", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 192000},
	{"id": "deepseek-v4.1-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
	{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
	{"id": "glm-5.3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
	{"id": "glm-5.3-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 200000},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 200000},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 512000},
	{"id": "kimi-k3-1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
	{"id": "kimi-k2.8-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
	{"id": "kimi-k2.7", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 256000},
	{"id": "kimi-k2.6", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 256000},
})

// staticModelsIntl 国际版静态模型表（动态接口失败时的回退）。
//
// 取自 /v3/config 的 data.agents[name=="cli"].models（实测 2026-09-15），
// 即客户端选模型时真正看到的清单，另加 hy4-preview（见下）。
//
// 历史：此前该表抄自本地缓存 acc-product-config-v3.json，其中
//   - gpt-5.3-codex 属于 CodeBuddy 产品清单，不在 WorkBuddy 的 cli 清单里；
//   - 缺 kimi-k2.8-preview、hy4-preview-f。
//
// 现已按 /v3/config 校正。
//
// 关于 hy4-preview：它不在 cli 清单里，但**实测可用**（HTTP 200 正常出流），
// 且出现在 /v3/config 的 data.models 与 productFeaturesConfig.ModelTrialBanner 中
// （作为 hy4-preview-f 的试用目标模型）。保留它，避免用户手动指定时报「模型不存在」。
//
// 注意与国服的差异（这也是客户端选模型时最易踩的坑）：
//
//	国服   deepseek-v4-flash   / glm-5.2 / kimi-k2.7 / minimax-m3
//	国际版 deepseek-v4.1-flash / glm-5.3 / kimi-k3   / gpt-5.6-* / gemini-3.5-flash
var staticModelsIntl = withImageCapability([]map[string]any{
	{"id": "default-model", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 200000},
	{"id": "fast-model", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 200000},
	{"id": "balanced-model", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 256000},
	{"id": "primary-model", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 272000},
	{"id": "deep-model", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 200000},
	{"id": "hy4-preview-f", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 300000},
	{"id": "hy4-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 200000},
	{"id": "hy3", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 192000},
	{"id": "deepseek-v4.1-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 300000},
	{"id": "gpt-6-astra", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 400000},
	{"id": "gpt-5.6-sol", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 1000000},
	{"id": "gpt-5.6-terra", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 1000000},
	{"id": "gpt-5.6-luna", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 1000000},
	{"id": "gpt-5.5", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 1000000},
	{"id": "gpt-5.4", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 272000},
	{"id": "gemini-3.5-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 1000000},
	{"id": "glm-5.3", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 1000000},
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 1000000},
	{"id": "kimi-k3", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 1000000},
	{"id": "kimi-k2.8-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 300000},
	{"id": "kimi-k2.6", "object": "model", "created": 1753600000, "owned_by": "workbuddy-intl", "context_length": 256000},
})

// staticModelsAll 合并两个区域的模型（按 id 去重，国服优先）。
//
// /v1/models 没有账号上下文，因此返回并集：客户端据此得知全部可用名称。
// 具体某个名称能否用，取决于实际选中的账号属于哪个区域 ——
// 不匹配时上游会返回 code=11102 model service info not found，提示清晰。
var staticModelsAll = func() []map[string]any {
	seen := map[string]bool{}
	out := make([]map[string]any, 0, len(staticModels)+len(staticModelsIntl))
	for _, m := range append(append([]map[string]any{}, staticModels...), staticModelsIntl...) {
		id, _ := m["id"].(string)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, m)
	}
	return out
}()

// dynamicModelsTTL 动态模型清单的缓存时长；modelsFetchFailCooldown 是拉取失败后的负缓存时长。
//
// 缓存本身按区域分桶，在 capability.go 的 regionModelCache 里；
// 这两个常量被那里复用，故留在包内共享。
const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：优先动态（按区域，缓存 1h），失败回退静态表。
//
// 列表是**两区并集**：/v1/models 没有账号上下文，无法知道用户会选哪个账号，
// 因此必须让客户端看到全部可用的名称，同时用能力字段诚实地表达
// 「这个名称只在某一个区域存在」。见 mergedModelList 与 capabilityFieldsFor。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.mergedModelList(),
	})
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := readLimitedBody(r)
	if err != nil {
		writeBodyReadError(w, err, openAIBodyCodes, writeOpenAIError)
		return
	}
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)

	st := newChatStat(time.Now(), body, peek.Stream)
	defer func() {
		st.done()
		h.recordUsage(st)
	}()

	sessKey := ""
	if h.cfg.Session != nil {
		sessKey = session.ExtractKey(body)
	}

	result, status, ferr := h.forwardChat(body, peek.Stream, sessKey)
	if ferr != nil {
		st.status = status
		st.uid = result.UID
		code, msg := openAIFailure(ferr)
		writeOpenAIError(w, status, code, msg)
		return
	}
	st.uid = result.UID

	if result.Stream != nil {
		st.status = http.StatusOK
		stats := newChatStatsReaderSince(result.Stream, st.start)
		_ = upstream.Stream(w, stats)
		st.ttfb = stats.TTFB()
		st.toks, _ = stats.Tokens()
		if counters, ok := stats.Usage(); ok {
			st.setCounters(counters)
		}
		result.Stream.Close()
		h.release(result.UID)
		return
	}

	writeJSON(w, http.StatusOK, result.Response)
	st.status = http.StatusOK
	st.toks = completionTokens(result.Response)
	if u, ok := result.Response["usage"].(map[string]any); ok {
		st.setUsageMap(u)
	}
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify），此处不再按原始 status 二次判断。
// 仅在 chat 轮转循环内调用：调用方已准备好 lastErr 并打算 continue 换号。
//
// model 是本次请求的目标模型，rawBody 是上游原始响应体 —— 仅 ErrModelRate 需要
// 用到（按 uid+model 记账，并从文案里取重置时间）。
//
// 六条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrModelRate → CooldownModel：**模型级**冷却到上游给的重置时间；该账号其他模型不受影响。
//   - ErrSoftRate / ErrNotFound → Cooldown(CoolSoft)：账号级即时软冷却（429/404）。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；模型冷却按各自截止到期；
// 熔断按其指数退避截止到期；成功（NoteSuccess）清 fails/熔断；
// 签到解冻（ReenableIfCredits→reviveCoolingLocked）只清账号级冷却，不动熔断与模型冷却。
func (h *Handler) applyErrorPolicy(uid, model string, kind upstream.ErrKind, rawBody string) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrModelRate:
		// 模型级限流（429 code=6004）：只冷却该账号的这个模型。
		//
		// 上游文案给出确切重置时刻（"将在 2026-09-15 13:25:47 UTC+8 重置"），
		// 优先用它 —— 比固定 soft_rate 更准，既不会过早重试（继续撞限流），
		// 也不会过晚恢复（白等）。解析不出时回退固定软冷却时长。
		now := time.Now()
		until, parsed := upstream.ParseResetTime(rawBody, now)
		if !parsed {
			until = now.Add(h.cfg.SoftCooldown)
		}
		h.cfg.Pool.CooldownModel(uid, model, until, modelRateReason(model, until, parsed), parsed)
	case upstream.ErrSoftRate:
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
	}
}

// modelRateReason 生成模型冷却的展示文案（含模型名与重置时间）。
//
// 前端直接用这条文案，避免两端各自格式化时间导致口径不一致。
func modelRateReason(model string, until time.Time, parsed bool) string {
	name := model
	if name == "" {
		name = "（未知模型）"
	}
	if parsed {
		return fmt.Sprintf("%s 已达频率上限，%s 重置", name, until.In(time.Local).Format("01-02 15:04"))
	}
	return fmt.Sprintf("%s 已达频率上限（未取到重置时间，按软冷却 %s 处理）", name, until.In(time.Local).Format("15:04"))
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// maxRequestBody 请求体上限。
//
// 为什么从 8MB 提到 32MB：长对话（Claude Code / Codex 一轮带上大量文件内容与工具
// 结果）很容易突破 8MB，而**静默截断**会把合法 JSON 切成半截字节透传给上游，
// 上游 json.Decoder 报 `unexpected EOF`，表现为 400 code=11101
// "Unmarshal chat params failed with error: unexpected EOF" —— 客户端只看到
// 「请求参数有误」，完全无法定位到是网关截断（Issue #5 实测）。
const maxRequestBody = 32 << 20

// errBodyTooLarge 请求体超过 maxRequestBody。作为哨兵错误供 handler 回 413，
// 避免把截断后的坏字节继续往下传（那样只能在上游报出难以定位的解析错误）。
var errBodyTooLarge = errors.New("request body too large")

// readLimitedBody 读取请求体，超过 maxRequestBody 时**显式报错**而非静默截断。
//
// 关键差别：LimitReader 读满即返回，调用方无法区分「读完了」与「被截断了」，
// 于是截断体一路流到上游才炸。这里多读 1 字节来判定越界：读回长度 > 上限
// 即说明源还没结束，直接返回 errBodyTooLarge。
func readLimitedBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxRequestBody {
		return nil, errBodyTooLarge
	}
	if len(body) == 0 {
		return nil, errors.New("empty request body")
	}
	return body, nil
}

var nowFunc = time.Now

func jsonUnmarshal(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// openAIFailure 把转发失败翻译成 OpenAI 形状的错误码与消息。
//
// 默认沿用既有契约（no_healthy_account + 503，语义是「账号池暂时不可用，
// 稍后重试」）；只有**请求侧**错误才偏离 —— 那些错误重试多少次都一样，
// 必须让客户端看到真实原因，而不是被误导去等账号恢复。
//
// 两类请求侧错误（两者都是「重试无用、要改请求」）：
//   - 模型不在白名单 → model_not_allowed（见 modelLockedError）
//   - 上下文超长       → context_length_exceeded
func openAIFailure(err error) (code, msg string) {
	if f := failureOf(err); f != nil && f.Kind == FailureContextTooLong {
		return "context_length_exceeded", f.Message
	}
	// 其余交给 errorCodeFor（当前只有 model_not_allowed 与 no_healthy_account），
	// 即 chat/completions 一直以来的行为。
	return errorCodeFor(err), errText(err)
}

// anthropicFailure 同上，Anthropic 词汇表。
//
// 上下文超长在 Anthropic 语义里是 invalid_request_error（其真实文案即
// "prompt is too long: ..."），而非 request_too_large（那是请求**字节数**超限）。
func anthropicFailure(err error) (code, msg string) {
	if f := failureOf(err); f != nil && f.Kind == FailureContextTooLong {
		return "invalid_request_error", f.Message
	}
	if errorCodeFor(err) != "no_healthy_account" {
		return "invalid_request_error", errText(err)
	}
	return "api_error", errText(err)
}

// responsesFailure 同上，Responses API 的上游失败码是 upstream_error。
//
// 模型白名单拒绝沿用 #14 为该协议定的 invalid_request_error，**不**把
// errorCodeFor 的 model_not_allowed 直接透出：model_not_allowed 是本网关给
// chat/completions 形状定的码，不属于 Responses 词汇表（见 responsesBodyCodes
// ——该协议用 invalid_request / payload_too_large）。同理 anthropicFailure 保持
// invalid_request_error。三个入口共享的是「谁来判定失败类别」这条映射链，
// 不是同一个码面值；把码面值也一起统一会让各协议的词汇表互相串味。
func responsesFailure(err error) (code, msg string) {
	if f := failureOf(err); f != nil && f.Kind == FailureContextTooLong {
		return "context_length_exceeded", f.Message
	}
	if errorCodeFor(err) != "no_healthy_account" {
		return "invalid_request_error", errText(err)
	}
	return "upstream_error", errText(err)
}

// writeOpenAIError 写出 OpenAI 形状的错误体。
func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}

// bodyErrorCodes 各协议在「请求体超限」与「读取失败」两种情形下使用的错误码。
//
// 分开配置是因为三家协议的词汇表不同：OpenAI 用 payload_too_large /
// invalid_request，Anthropic 用 request_too_large / invalid_request_error，
// 客户端按自家词汇表分支处理，混用会让错误提示退化成未知错误。
type bodyErrorCodes struct {
	tooLarge   string
	badRequest string
}

// writeBodyReadError 把 readLimitedBody 的失败翻译成目标协议的错误响应。
//
// 超限返回 413 并给出明确原因：客户端据此知道要缩减历史，而不是收到一个
// "请求参数有误"然后无从下手（静默截断透传时的表现，见 Issue #5）。
// 其余读取错误维持 400 原语义。
func writeBodyReadError(w http.ResponseWriter, err error, codes bodyErrorCodes, write func(http.ResponseWriter, int, string, string)) {
	if errors.Is(err, errBodyTooLarge) {
		write(w, http.StatusRequestEntityTooLarge, codes.tooLarge,
			fmt.Sprintf("request body exceeds %d MB limit; reduce the conversation history or attachment size", maxRequestBody>>20))
		return
	}
	write(w, http.StatusBadRequest, codes.badRequest, "read body: "+err.Error())
}

// openAIBodyCodes OpenAI 系（chat/completions）的错误码。
var openAIBodyCodes = bodyErrorCodes{tooLarge: "payload_too_large", badRequest: "invalid_request"}

// anthropicBodyCodes Anthropic Messages 的错误码。
var anthropicBodyCodes = bodyErrorCodes{tooLarge: "request_too_large", badRequest: "invalid_request_error"}

// responsesBodyCodes OpenAI Responses 的错误码。
var responsesBodyCodes = bodyErrorCodes{tooLarge: "payload_too_large", badRequest: "invalid_request"}

// errorCodeFor 把 forwardChat 的错误映射成面向客户端的错误码。
//
// 区分「模型不在白名单」与「账号都不可用」很重要：前者是**调用方
// 需要改的东西**（换模型或调整「放行模型」），后者是**服务端状态**。都报
// no_healthy_account 会把用户引向排查账号，而真正的原因在请求里。
func errorCodeFor(err error) string {
	var locked *modelLockedError
	if errors.As(err, &locked) {
		return "model_not_allowed"
	}
	// 带图片请求缺区域账号：不是「账号池暂时不可用、稍后重试」，而是
	// 「你的池子缺一类账号」。用独立的码让客户端/用户能区分，而不是
	// 混进 no_healthy_account 里被当成一次普通的负载抖动。
	if f := failureOf(err); f != nil && f.Kind == FailureImageRegionUnavailable {
		return "image_region_unavailable"
	}
	return "no_healthy_account"
}
