// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrHardCredit                 // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                   // 429 软限流 → 短冷却
	ErrSessionDead                // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound                   // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                     // 5xx 上游故障
	ErrClient                     // 其他 4xx / 业务错误
	// ErrModelRate 模型级限流：该账号的**这个模型**额度用尽（429 code=6004），
	// 与整个账号被封的 ErrSoftRate 不同 —— 上游明确提示"您也可以切换其他模型继续使用"，
	// 即该账号的其他模型仍然可用。冷却时长取上游给出的重置时间（解析失败回退软冷却）。
	ErrModelRate
	// ErrContextTooLong 请求的上下文超出模型窗口（HTTP 400 code=11115）。
	//
	// 这是**请求侧**错误，与账号无关：同一个请求体发给任何账号都会同样失败。
	// 因此必须与 ErrClient 区分开 —— 否则会落入「换号重试」路径，把整个请求体
	// 对着每个账号重传一遍（2026-09-15 实测：1.12M token 的请求被重传 3 次），
	// 最后还被包装成 503 no_healthy_account，把排查方向引向「账号故障」。
	ErrContextTooLong
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrModelRate:
		return "model_rate"
	case ErrContextTooLong:
		return "context_too_long"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers 余额不足关键词（小写比较 + 中文原文比较双通道）。
// hardMarkers 「余额/额度耗尽」的文案特征（命中即 ErrHardCredit → 长冷却）。
//
// 为什么要收单复数两种写法：上游国际版（workbuddy.ai）实际返回的是
// "Credits exhausted. Please visit the link below to purchase add-on packs
// and get more credits: …"（**复数** Credits），而早期只登记了单数
// "credit exhausted"，于是 strings.Contains 恒不命中 → 被判成 ErrSoftRate
// （软冷却 60 秒）→ 60 秒后重试同一个已耗尽账号，形成无限重试。
// 实测该响应体 15 个关键词全部未命中，故补齐复数形态。
var hardMarkers = []string{
	"insufficient credit", "insufficient credits",
	"no credit", "no credits",
	"credit exhausted", "credits exhausted",
	"credit exhaustion", "credits exhaustion",
	"out of credit", "out of credits",
	"quota exceeded", "quota exhaust",
	"payment required",
	"credit not enough", "credits not enough",
	"not enough credit", "not enough credits",
	"credit used up", "credits used up",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// modelRateMarkers 模型级限流的判定依据（429 + 其中之一）。
//
// 主力信号是上游业务码 6004；文案关键词作为兜底 —— 上游改码不改文案时仍能识别，
// 但**必须**同时是 429，避免把其他场景的"频率限制"字样误判成模型限流。
var modelRateMarkers = []string{"超出频率限制", "切换其他模型"}

// modelRateCode 上游「模型级限流」的业务码。
const modelRateCode = 6004

// contextTooLongCode 上游「上下文超长」的业务码。
//
// 实测响应（2026-09-15 现场，prompt 1121509 > 上限 1048576）：
//
//	400 {"code":11115,"msg":"prompt is too long: 1119655 tokens > 1048576 maximum",
//	     "extError":{"code":"context_length_exceeded","type":"invalid_request_error"},
//	     "displayMsg":{"en":"The request exceeds the model context limit...",
//	                   "zh":"对话内容超出模型长度上限，请精简对话或减少附件后重试。"}}
const contextTooLongCode = 11115

// contextTooLongMarkers 上下文超长的判定文案（中英双通道兜底）。
//
// 业务码是主信号；文案兜底用于上游改码不改文案的场景。措辞取自上游真实响应，
// 刻意含中英两版 displayMsg —— 上游按 Accept-Language 切换语言，只认一种会漏判。
//
// 注意 msg 有多种写法：实测同一业务码下遇到过 "prompt is too long"（国际版）
// 与 "input length too long"（国服 glm-5.3），两种都要登记。
var contextTooLongMarkers = []string{
	"context_length_exceeded",
	"prompt is too long",
	"input length too long",
	"exceeds the model context limit",
	"对话内容超出模型长度上限",
	"超出模型长度上限",
}

// IsContextTooLong 报告上游响应是否为「请求上下文超出模型窗口」。
//
// 三路判定，任一命中即成立：业务码 11115、extError.code=context_length_exceeded、
// 或真实文案关键词（见 contextTooLongCode 注释里的实测响应）。
//
// 不按 status 门控：上游以 400 为主，但判定依据是业务语义而非状态码，
// 上游若改用 413 也能识别。
func IsContextTooLong(body string) bool {
	var env apiEnvelope
	if json.Unmarshal([]byte(body), &env) == nil && env.Code == contextTooLongCode {
		return true
	}
	var ext struct {
		ExtError struct {
			Code string `json:"code"`
		} `json:"extError"`
	}
	if json.Unmarshal([]byte(body), &ext) == nil && strings.EqualFold(ext.ExtError.Code, "context_length_exceeded") {
		return true
	}
	lower := strings.ToLower(body)
	for _, m := range contextTooLongMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// resetTimeRe 从报错文案里提取重置时刻。
//
// 实测文案（2026-09-15 现场）：
//
//	您的使用量已超出频率限制，将在 2026-09-15 13:25:47 UTC+8 重置，您也可以切换其他模型继续使用。
//
// 捕获组 1 = 时间字面量（日期 + 时间），捕获组 2 = 时区后缀（如 "UTC+8" / "UTC+08:00"）。
// 时区后缀可选，仅为容错：**实测到的上游文案一律带 "UTC+8"**，无后缀分支尚未在真实
// 响应中观察到。保留它是为了上游改格式时不至于整个解析失败。
var resetTimeRe = regexp.MustCompile(`(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})(?:\s*(UTC[+-]\d{1,2}(?::\d{2})?|Z))?`)

// upstreamZone 上游（CodeBuddy 国服）的业务时区，用于解释**无时区后缀**的时间字面量。
//
// 为什么不用 time.Local：上游是国服服务，其自然日/重置时刻都按 CST（UTC+8）计；
// 用容器本地时区解释会让同一份响应在开发机（+08:00）与 UTC 容器上得出相差 8 小时的
// 结果，UTC 下极端情况会把尚未到期的重置点误判成"已过期"而退化成固定软冷却。
// 与 scheduler 的 cstZone 同一口径（中国无夏令时，固定 +8，不依赖 tzdata）。
//
// 注意：这是**防御性**选择 —— 真实的 6004 文案都带 "UTC+8"，故该分支正常不会走到；
// 带后缀时一律以文案里的偏移为准，不受本变量影响。
var upstreamZone = time.FixedZone("CST", 8*60*60)

// ParseResetTime 从上游报错文案里解析「重置时刻」；解析不出返回零值与 false。
//
// 时区处理：
//   - "UTC+8" / "UTC+08:00" → 固定偏移（**实测文案用的就是这种**）
//   - "Z"                   → UTC
//   - 无时区后缀 / 后缀非法  → 按上游业务时区（CST）解释，而非容器本地时区（见 upstreamZone）
//
// 只返回**未来**的时刻：解析出过去的时间说明文案里的重置点已过（如重放旧日志），
// 此时返回 false 交给调用方回退固定冷却，避免写入一个立即失效的冷却。
func ParseResetTime(body string, now time.Time) (time.Time, bool) {
	m := resetTimeRe.FindStringSubmatch(body)
	if m == nil {
		return time.Time{}, false
	}
	literal := strings.Replace(m[1], "T", " ", 1)
	layout := "2006-01-02 15:04:05"

	var loc *time.Location
	switch tz := strings.TrimSpace(m[2]); {
	case tz == "":
		loc = upstreamZone
	case tz == "Z":
		loc = time.UTC
	default:
		loc = parseUTCOffset(tz)
		if loc == nil {
			loc = upstreamZone
		}
	}
	ts, err := time.ParseInLocation(layout, literal, loc)
	if err != nil {
		return time.Time{}, false
	}
	if !ts.After(now) {
		return time.Time{}, false
	}
	return ts, true
}

// parseUTCOffset 解析 "UTC+8" / "UTC-05:30" 形式的固定偏移时区；非法返回 nil。
func parseUTCOffset(s string) *time.Location {
	rest := strings.TrimPrefix(s, "UTC")
	if rest == "" || (rest[0] != '+' && rest[0] != '-') {
		return nil
	}
	sign := 1
	if rest[0] == '-' {
		sign = -1
	}
	rest = rest[1:]
	hours, minutes := 0, 0
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		h, err1 := strconv.Atoi(rest[:i])
		mm, err2 := strconv.Atoi(rest[i+1:])
		if err1 != nil || err2 != nil {
			return nil
		}
		hours, minutes = h, mm
	} else {
		h, err := strconv.Atoi(rest)
		if err != nil {
			return nil
		}
		hours = h
	}
	if hours > 23 || minutes > 59 {
		return nil
	}
	offset := sign * (hours*3600 + minutes*60)
	return time.FixedZone(s, offset)
}

// IsModelRateLimited 报告 429 响应体是否为「模型级限流」（该账号该模型额度用尽）。
//
// 注意与 ErrSoftRate 的区别：ErrSoftRate 是账号级限流（整个账号被限速），
// 而模型级限流只影响当前请求的那个模型，该账号换模型仍可用 —— 上游文案
// "您也可以切换其他模型继续使用" 明确指出了这一点。
func IsModelRateLimited(body string) bool {
	var env apiEnvelope
	if json.Unmarshal([]byte(body), &env) == nil && env.Code == modelRateCode {
		return true
	}
	// 文案兜底：上游改码不改文案时仍能识别。
	for _, m := range modelRateMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

// creditExhaustedCode 上游「额度耗尽」的业务码。
//
// 与 modelRateCode(6004) 的关键区别在于**嵌套层级**：6004 在顶层 `code`，
// 而 14018 藏在 `error.data.code`：
//
//	{"error":{"data":{"code":14018,"msg":"Credits exhausted. …"}}}
//
// 因此不能用 apiEnvelope（它只解顶层 code）。实测该响应的 HTTP 状态是 **429**，
// 而 429 分支若不识别它就会落进 ErrSoftRate → 只冷却 60 秒 → 无限重试。
const creditExhaustedCode = 14018

// isCreditExhaustedCode 报告响应体是否为「额度耗尽」业务码（含嵌套层级）。
func isCreditExhaustedCode(body string) bool {
	var env struct {
		Error struct {
			Data struct {
				Code int `json:"code"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return false
	}
	return env.Error.Data.Code == creditExhaustedCode
}

// FriendlyMessage 把上游的原始错误体提炼成一句可读的原因，供客户端展示。
//
// 背景：此前直接把整段上游 JSON 拼进 OpenAI 错误体的 message，客户端看到的是
// 「all accounts unavailable (cooling/disabled): upstream soft_rate (http 429):
// {"error":{"data":{"code":14018,"msg":"Credits exhausted. …"}}}」—— 又长又难懂。
//
// 返回空串表示没有更优的表述，调用方应回退到原始文案。
func FriendlyMessage(kind ErrKind, status int, body string) string {
	// 业务码优先，但**只有响应体里真的带 14018 时才把该码写进文案**：
	// ErrHardCredit 也可能来自 HTTP 402 或关键词命中，此时硬写「上游 14018」
	// 会让用户拿着一个与响应不符的码去排查。
	if isCreditExhaustedCode(body) {
		return "账号额度已耗尽（上游 " + strconv.Itoa(creditExhaustedCode) + "）：请为该账号充值，或等待签到 / 免费额度恢复后重试"
	}
	switch {
	case kind == ErrHardCredit:
		return "账号额度已耗尽：请为该账号充值，或等待签到 / 免费额度恢复后重试"
	case kind == ErrModelRate:
		return "该账号在此模型上已达频率上限，已按上游给出的重置时间冷却；同一账号的其他模型仍可用"
	case kind == ErrSoftRate:
		return "账号被上游限流（HTTP 429），已短暂冷却，稍后会自动重试"
	case kind == ErrSessionDead:
		return "账号登录态已失效，需在「账号管理」页重新登录"
	case kind == ErrNotFound:
		return "上游返回 404（接口或模型不存在），已短暂冷却并切换账号"
	case kind == ErrServer && status > 0:
		return "上游服务异常（HTTP " + strconv.Itoa(status) + "），已切换到其他账号"
	}
	return ""
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	// 额度耗尽的业务码优先判定：它的 HTTP 状态是 429，若不先拦，
	// 会落进下面的 429 分支被判成 ErrSoftRate（仅 60 秒冷却）→ 无限重试。
	if isCreditExhaustedCode(body) {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	if status == http.StatusTooManyRequests {
		// 模型级限流优先于账号级软限流：两者的冷却粒度与时长都不同
		// （模型级按 uid+model 冷却到上游给的重置时间）。
		if IsModelRateLimited(body) {
			return ErrModelRate
		}
		return ErrSoftRate
	}
	// 上下文超长必须早于通用 4xx 判定：它是请求侧错误，换号无用，
	// 需要独立 kind 让调用方「立即失败」而不是轮转重传整个请求体。
	// 放在 429 之后是有意的：429 一律按限流归类，保持既有语义不变。
	if IsContextTooLong(body) {
		return ErrContextTooLong
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardMarkers 捕获。
	return ErrNone
}

// ContextTooLongMessage 把上下文超长的上游响应体提炼成一条**保留原文**的客户端消息。
//
// 为什么必须保留上游原文：下游客户端（如 DeepSeek Harness）靠文案模式识别上下文溢出
// （`prompt is too long` / `context_length_exceeded` / `exceeds the model context limit`），
// 据此触发自动压缩并重试。若只回我们自己的措辞，客户端就认不出这是溢出，
// 只会把它当成普通失败 —— 那正是本次死锁难以自愈的原因之一。
//
// 因此输出形如：`<上游 msg>（<中文 displayMsg>）`，两种语言的特征串都在，
// 中文提示同时给人类看。上游字段缺失时逐级回退，最终回退到原始 body。
func ContextTooLongMessage(body string) string {
	var env struct {
		Msg       string `json:"msg"`
		ExtError  struct {
			Message string `json:"message"`
		} `json:"extError"`
		DisplayMsg struct {
			Zh string `json:"zh"`
			En string `json:"en"`
		} `json:"displayMsg"`
	}
	_ = json.Unmarshal([]byte(body), &env)

	primary := strings.TrimSpace(env.Msg)
	if primary == "" {
		primary = strings.TrimSpace(env.ExtError.Message)
	}
	hint := strings.TrimSpace(env.DisplayMsg.Zh)
	if hint == "" {
		hint = strings.TrimSpace(env.DisplayMsg.En)
	}

	switch {
	case primary == "" && hint == "":
		return truncate(strings.TrimSpace(body), 400)
	case primary == "":
		return hint
	case hint == "" || strings.Contains(primary, hint):
		return primary
	default:
		return primary + "（" + hint + "）"
	}
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	// 与 HTTP 共享同一个 *http.Transport 实例，连接池不重复。
	ChatHTTP *http.Client

	// intlHTTP / intlChatHTTP 国际版账号专用 client（代理分流用），语义与
	// HTTP/ChatHTTP 一一对应，只是多挂一个显式代理 transport。
	//
	// 为什么必须成对存在而不是「按需临时构造」：构造 client 要连带构造
	// *http.Transport，而 transport 自带连接池 —— 每次请求新建一个等于池化失效
	// （每个请求一次 TLS 握手），代理侧还会堆起大量短连接。
	//
	// 为空 = 未配显式代理（或尚未调 SetProxy）→ httpFor/chatClientFor 回落
	// HTTP/ChatHTTP，即**改动前的行为**：每个请求都只挂 ProxyFromEnvironment。
	// 即测试里直接字面量构造 &Client{HTTP: ...} 时本字段为 nil，国服与国际版
	// 行为完全一致 —— 老测试的假上游因此不受本次分流影响。
	intlHTTP     *http.Client
	intlChatHTTP *http.Client

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时；<=0 表示未设置（回落 HTTP.Timeout）。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// effortsMu/efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	effortsMu sync.RWMutex
	efforts   map[string][]string

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	SanitizeFingerprints bool

	ChatBaseCN    string
	BillingBaseCN string
	// WebBaseCN / WebBaseIntl 官网域（成长中心）基址。
	//
	// 为什么需要第三个基址：成长任务的**领奖**接口只在官网域提供
	//（www.workbuddy.cn/activity/growth/tasks/<code>/claim），
	// chat 域与 billing 域上都没有该路径（实测 CLI 域同形路径恒 400）。
	// 任务列表与报名仍在 chat 域，因此三个基址并存、不可互相替代。
	WebBaseCN   string
	WebBaseIntl string
	// BaseIntl 国际版基址。与国服不同，国际版所有端点（chat / billing /
	// 签到 / 旅行 / token 刷新）都在同一域名下，因此只需一个 base。
	BaseIntl string

	// proxyURL 当前生效的显式代理（空串 = 未设置，回落环境变量）。
	// 由 SetProxy 维护；国际版（workbuddy.ai）在国内直连不稳定，通常需要它。
	//
	// 作用范围见 scope：未显式指定时**仅国际版账号**（见 httpFor/chatClientFor）；
	// 国服（*.workbuddy.cn / *.codebuddy.cn）在国内直连稳定，把它的流量绕进代理
	// 既无收益，又平白多一跳、多一个故障面 —— 代理挂掉时国服账号会跟着一起
	// 不可用（实测诉求来自所有者）。
	proxyURL string

	// scope 显式的适用范围（宿主配置 proxy_scope 的投影）；nil = 未显式指定。
	//
	// nil 与「两个都 false」**语义不同**，不能混为一谈：
	//   - nil        → 老行为：国服走 ProxyFromEnvironment、国际版走显式代理；
	//   - 非 nil     → 开关说了算（关 = 真直连，连环境变量代理也不用）。
	// 用指针而不是值类型，正是为了保住这个三态；若用值类型，零值
	// {false,false} 会让「没读过开关」与「两个都关」无法区分。
	scope *ProxyScope
}

// IsIntl 判断账号是否属于国际版（供签到/旅行的区域范围过滤复用）。
//
// 依据 auth 文件里的 domain 字段：
//   *.workbuddy.cn / *.codebuddy.cn -> 国服
//   *.workbuddy.ai / *.codebuddy.ai -> 国际版
//
// domain 缺失时按国服处理：历史上只存在国服账号，保持向后兼容。
//
// 判据已下沉到 auth.Auth.IsIntl（账号池做区域路由时也要用同一口径，
// 两处各写一份迟早会漂移）。这里保留函数是为了不打断既有调用点。
func IsIntl(a *auth.Auth) bool { return a.IsIntl() }

// isIntl 包内简写，保持既有调用点不变。
func isIntl(a *auth.Auth) bool { return IsIntl(a) }

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	// HTTP 与 ChatHTTP **共享同一个 Transport**：连接池不重复，
	// 两者只差总时长（ChatHTTP.Timeout=0，首字节由 ResponseHeaderTimeout 约束）。
	//
	// 国际版那一对（intlHTTP/intlChatHTTP）此处**有意留空**：还没配代理，
	// httpFor/chatClientFor 会回落这两个，国服与国际版行为完全一致 ——
	// 与本次代理分流改动之前逐字相同。SetProxy 才会把国际版分出去。
	tr := newTransport(nil)
	return &Client{
		HTTP:                 &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:             &http.Client{Timeout: 0, Transport: tr},
		SanitizeFingerprints: true,
		ChatBaseCN:           "https://copilot.tencent.com",
		BillingBaseCN:        "https://www.codebuddy.cn",
		BaseIntl:             "https://www.workbuddy.ai",
		WebBaseCN:            "https://www.workbuddy.cn",
		WebBaseIntl:          "https://www.workbuddy.ai",
	}
}

// newTransport 构造共用的 http.Transport。
//
// 关键：显式设置 Proxy 而不是依赖 http.ProxyFromEnvironment 的默认行为 ——
// 后者只读 HTTPS_PROXY 等**环境变量**，而用户在软件「设置 → 更新代理」里填的
// 代理是写在配置文件里的，环境变量通常是空的。于是国内直连
// workbuddy.ai 会超时（实测 wsarecv timeout），而浏览器因为读系统代理却正常。
//
// proxyURL 为 nil 时退回 ProxyFromEnvironment（仍尊重环境变量，行为与之前一致）。
//
// nil 分支是**未显式开关时的国服 transport**（见 applyProxy）：有意保留
// ProxyFromEnvironment 而不是设成恒 nil（真正不走任何代理）。理由是它是本次改动
// 之前所有出站请求的既有行为 —— 有用户靠 HTTPS_PROXY 做全局代理，擅自忽略等于
// 替用户改网络配置，且现象隐蔽（只有国服请求突然超时）。
//
// 「真直连」用 newDirectTransport（Proxy 恒 nil），与 nil 分支**不是**同一件事，
// 见其注释。
func newTransport(proxyURL *url.URL) *http.Transport {
	tr := newTransportSkeleton()
	if proxyURL != nil {
		tr.Proxy = http.ProxyURL(proxyURL)
	} else {
		tr.Proxy = http.ProxyFromEnvironment
	}
	return tr
}

// newDirectTransport 构造「真直连」transport：Proxy 恒为 nil。
//
// 这是用户**显式关闭**某个区域的代理开关后的语义：连环境变量里的
// HTTP_PROXY / HTTPS_PROXY 也不使用。
//
// 为什么关掉就要连环境变量一起忽略，而不是「只不挂显式代理、留着
// ProxyFromEnvironment」：开关的名字与文案都是「使用代理」，用户关掉它就是要
// **不走代理**。若这时还偷偷吃 HTTPS_PROXY，用户会在界面上看到「已关闭」、
// 实际流量仍绕道 —— 与「关了开关却仍在走代理」是同一种欺骗，而且更难查
//（现象只在设了环境变量的机器上出现）。
//
// 代价与边界：这是本次唯一会让国服出站行为变化的路径，且**只发生在用户
// 主动关掉国内版开关时**（配置里没有这个键的老用户仍走 ProxyFromEnvironment，
// 见 applyProxy 的 scope==nil 分支）—— 不会因为升级而静默改变任何人的网络配置。
func newDirectTransport() *http.Transport {
	tr := newTransportSkeleton()
	// 刻意不设 tr.Proxy：nil 表示「任何请求都不经代理」。
	return tr
}

// newTransportSkeleton 三个 transport 共用的连接池调优参数。
//
// 抽出来是为了让 env / 显式代理 / 真直连三条路径**只差 Proxy 一个字段**：
// 各写一份迟早会漂移（例如只给显式代理那套设了 ResponseHeaderTimeout），
// 而漂移的表现是「某一路的聊天首字节超时按 120s 干等」，从日志上看不出来。
func newTransportSkeleton() *http.Transport {
	return &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		// 聊天 SSE 首字节前硬上限（对短 RPC 无实际影响：其总时长 120s 更先到期）。
		ResponseHeaderTimeout: 120 * time.Second,
	}
}

// ProxyScope 显式的代理适用范围（三个独立开关里的**网关侧两个**）。
//
// 地址仍只填一次（宿主透传），本结构只决定「哪些区域使用它」。
//
// 为什么需要它：同一个代理对两个区域的收益完全相反 —— 国际版
//（workbuddy.ai）国内直连实测 wsarecv 超时，必须走代理；国服
//（copilot.tencent.com / codebuddy.cn）直连即通，绕进代理只会多一跳延迟、
// 多一个故障面（代理一挂，本来好好的国服账号跟着不可用）。
//
// 零值（false/false）**不是**默认语义：默认由宿主写入，见 cmd/server/config.go
// 的 Default()（cn=false / intl=true）。Go 侧刻意不在本类型上给默认值，
// 避免「默认值」散落在两处而漂移。
type ProxyScope struct {
	// CN 国服账号的出站请求是否使用该显式代理。
	CN bool
	// Intl 国际版账号的出站请求是否使用该显式代理。
	Intl bool
}

// SetProxy 设置出站代理（空串 = 不使用显式代理，回落环境变量）。
//
// 作用范围**仅国际版账号**：本函数一次性备好两套 transport/client，
//   - 国际版（*.ai） → intlHTTP/intlChatHTTP，挂显式代理；
//   - 国服           → HTTP/ChatHTTP，挂 ProxyFromEnvironment（= 既有默认行为）。
//
// 为什么按区域分流而不是给所有账号都挂代理：国际版 workbuddy.ai 在国内直连
// 不稳定（实测 wsarecv 超时）才需要代理；国服 copilot.tencent.com /
// codebuddy.cn 直连即通，把它的流量绕进代理只会（a）多一跳延迟、
// （b）多一个故障面 —— 用户代理一挂，本来能正常用的国服账号一起不可用。
//
// 为什么不用「一个 transport + 按请求挑代理」的写法（比如在 Transport.Proxy
// 里按 req.URL.Host 判断）：那样国服的**连接会与代理的连接共用同一个池**，
// 一旦某次判定出错（或将来新增域名漏判），错的那一侧不会报错、只会静默绕道，
// 从日志上看不出来。两套 transport 是物理隔离，判错的代价只是走错路而不会互相污染。
//
// 每套各自 HTTP/ChatHTTP 共享一个 Transport（连接池不重复），两者只差总时长。
// 既有连接不会被打断，由旧 Transport 自行回收；新请求立即走新代理。
// 启动时调用一次即可。
//
// **语义边界（重要）**：本函数表达的是「未显式选择适用范围」的老行为 ——
// 国服回落 ProxyFromEnvironment。要按开关精确控制，在它之后调 SetProxyScope。
// 这样拆分而不是直接改本函数的签名，是为了让既有调用点（含大量单测）语义不变：
// 只有真的读到 proxy_scope 配置时，行为才由开关决定。
func (c *Client) SetProxy(raw string) error {
	return c.applyProxy(raw, nil)
}

// SetProxyScope 按**显式**的两个开关重新布置 transport（地址沿用已设的代理）。
//
// 语义（这是所有者确认过的口径，改之前请先读明白）：
//
//	开关开 → 该区域走**显式代理**（设置页里填的那个地址）
//	开关关 → 该区域**真直连**：连环境变量里的 HTTP_PROXY / HTTPS_PROXY 也不用
//
// 「关 = 真直连」而不是「关 = 只不挂显式代理」，见 newDirectTransport 的注释。
//
// 必须在 SetProxy 之后调用（本函数不自带地址：地址由 SetProxy 解析并规范化，
// 两处各解析一遍迟早会在「host:port 自动补 http://」这类容错上分叉）。
// 未配代理（地址为空）时本函数是 no-op —— 没有地址可挂，开关也就无意义。
func (c *Client) SetProxyScope(cn, intl bool) error {
	if strings.TrimSpace(c.proxyURL) == "" {
		// 无地址：什么都不做（保持 SetProxy("") 之后的既有状态）。
		// 不能在这里建「真直连」transport 来"落实"两个 false —— 那会让一个
		// 根本没填代理的用户，仅因为开关默认关就把国服从环境变量代理改成直连。
		return nil
	}
	return c.applyProxy(c.proxyURL, &ProxyScope{CN: cn, Intl: intl})
}

// applyProxy SetProxy / SetProxyScope 的共同实现。
//
// scope == nil 表示「未显式指定适用范围」= 老行为：国服走 ProxyFromEnvironment、
// 国际版走显式代理。scope != nil 时按两个开关精确布置（关 = 真直连）。
func (c *Client) applyProxy(raw string, scope *ProxyScope) error {
	raw = strings.TrimSpace(raw)
	var proxyURL *url.URL
	if raw != "" {
		// 容忍用户只填 host:port（如 127.0.0.1:7890）：补 http:// 前缀。
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("代理地址无效: %w", err)
		}
		// 注意 url.Parse 对 "http://:8080" 不报错（Host 为 ":8080"、Hostname() 为空），
		// 必须用 Hostname() 判空，否则会把一个连不上主机的地址当成合法配置。
		if u.Hostname() == "" {
			return fmt.Errorf("代理地址缺少主机名: %s", raw)
		}
		proxyURL = u
	}

	// 每个区域需要**哪一类** transport：
	//
	//	trSpecEnv      ProxyFromEnvironment —— 未显式开关时的既有行为
	//	trSpecExplicit 用户填的显式代理
	//	trSpecDirect   真直连（Proxy=nil，连环境变量代理也不用）
	//
	// 先定「类别」再建实例，而不是先建几个实例再挑：类别是语义，实例是资源。
	// 混在一起写会让「两个区域都开」这种组合不小心落到同一个实例上（见下方
	// 关于区域隔离的注释）。未配显式代理时 trSpecExplicit 无处可建，此时
	// 选它的区域回落 trSpecEnv（= 改动前「没填地址就只跟环境变量走」的行为）。
	cnSpec, intlSpec := trSpecEnv, trSpecEnv
	if scope == nil {
		// 老行为：国服 env、国际版显式代理（有地址时）。
		if proxyURL != nil {
			intlSpec = trSpecExplicit
		}
	} else {
		pickSpec := func(enabled bool) transportSpec {
			if enabled {
				if proxyURL != nil {
					return trSpecExplicit
				}
				return trSpecEnv
			}
			return trSpecDirect
		}
		cnSpec, intlSpec = pickSpec(scope.CN), pickSpec(scope.Intl)
	}

	// 未配地址时两个区域的行为必然相同（都只跟环境变量走），此时**共用**一个
	// transport 并把国际版那一对归空 —— 这是 New()/SetProxy("") 之后的既有状态，
	// 有既有断言依赖它（未配代理时 intlHTTP 必须为 nil）。
	//
	// 反之，只要配了地址就**每个区域各建一个实例**（哪怕两格的类别相同，例如都开
	// → 都是显式代理）。理由：区域隔离是上一轮分流的硬要求 —— 共用连接池会让
	// 「关掉其中一路」之后旧连接仍可能被另一路复用，而那种污染在日志里看不出来。
	// 代价只是一个额外的空连接池（不建连接就不占资源）。
	shareTransports := proxyURL == nil && cnSpec == intlSpec

	// 本次新建的 transport 全部记下来，稍后统一套用继承来的调优值。
	var built []*http.Transport
	newOf := func(spec transportSpec) *http.Transport {
		var tr *http.Transport
		if spec == trSpecExplicit {
			tr = newTransport(proxyURL)
		} else if spec == trSpecDirect {
			tr = newDirectTransport()
		} else {
			tr = newTransport(nil)
		}
		built = append(built, tr)
		return tr
	}
	cnTr := newOf(cnSpec)
	intlTr := cnTr
	if !shareTransports {
		intlTr = newOf(intlSpec)
	}

	// 保留调用方已设的调优值（SetProxy 常在 New 之后、调优之前调用，
	// 但测试/其它调用顺序不确定，故这里从旧 Transport 继承可继承的字段）。
	//
	// ChatHTTP/HTTP 都可能为 nil：测试里字面量构造 &Client{HTTP: ...} 很常见，
	// 直接取 .Transport 会空指针 panic（现象是「配了代理网关直接崩」）。
	headerTimeout := 120 * time.Second
	if c.ChatHTTP != nil {
		if old, ok := c.ChatHTTP.Transport.(*http.Transport); ok && old != nil {
			headerTimeout = old.ResponseHeaderTimeout
		}
	}
	// 短 RPC 总时长同样继承（main.go 是按 client 设的 up.HTTP.Timeout，
	// 若 SetProxy 在调优之后被调用，不继承会把上限悄悄退回 120s）。
	rpcTimeout := 120 * time.Second
	if c.HTTP != nil && c.HTTP.Timeout > 0 {
		rpcTimeout = c.HTTP.Timeout
	}
	// 本次新建的 transport 全部套用继承来的值：漏掉某一个会让那一路悄悄退回
	// 默认（表现为「开了开关的那一路按新上限超时，另一路仍干等 120s」，
	// 两边日志长得一样，从现象上几乎发现不了）。
	for _, tr := range built {
		tr.ResponseHeaderTimeout = headerTimeout
	}

	c.HTTP = &http.Client{Timeout: rpcTimeout, Transport: cnTr}
	c.ChatHTTP = &http.Client{Timeout: 0, Transport: cnTr}
	// 国际版与国服用**同一个** transport 时把 intl 那一对归空，让
	// httpFor/chatClientFor 回落 HTTP/ChatHTTP：两套 client 指向同一个 transport
	// 除了多一层间接没有区别，而归空能保住既有断言「未配代理时 intlHTTP 为 nil」。
	if intlTr == cnTr {
		c.intlHTTP, c.intlChatHTTP = nil, nil
	} else {
		c.intlHTTP = &http.Client{Timeout: rpcTimeout, Transport: intlTr}
		c.intlChatHTTP = &http.Client{Timeout: 0, Transport: intlTr}
	}
	c.proxyURL = raw
	c.scope = scope
	return nil
}

// transportSpec 一个区域需要的 transport **类别**（不是实例）。
type transportSpec int

const (
	// trSpecEnv 回落 ProxyFromEnvironment（尊重 HTTPS_PROXY 等环境变量）。
	trSpecEnv transportSpec = iota
	// trSpecExplicit 走用户在设置页里填的那个显式代理。
	trSpecExplicit
	// trSpecDirect 真直连：Proxy 为 nil，连环境变量代理也不用。
	trSpecDirect
)

// ProxyURL 返回当前生效的显式代理（空串 = 未设置）。
func (c *Client) ProxyURL() string {
	return c.proxyURL
}

// ProxyScope 返回当前生效的显式适用范围；nil 表示未显式指定（= 老行为）。
//
// 供 main.go 打印**如实的**启动日志：说成「所有出站请求经 X」会让用户以为
// 国服流量也在绕道，从而误判国服变慢的原因。
func (c *Client) ProxyScope() *ProxyScope {
	return c.scope
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

// httpFor 返回该账号出站**短 RPC** 应使用的 client（按区域选 transport）。
//
// 国际版 → 挂显式代理的 intlHTTP；国服 → HTTP（ProxyFromEnvironment）。
// 未配显式代理时 intlHTTP 为 nil，两者都回落 HTTP —— 行为与改动前一致。
func (c *Client) httpFor(a *auth.Auth) *http.Client {
	if isIntl(a) && c.intlHTTP != nil {
		return c.intlHTTP
	}
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// chatClientFor 返回该账号**聊天 SSE** 应使用的 client；语义与 httpFor 相同，
// 只差总时长（ChatHTTP 无总时长，靠 ResponseHeaderTimeout 兜底）。
func (c *Client) chatClientFor(a *auth.Auth) *http.Client {
	if isIntl(a) && c.intlChatHTTP != nil {
		return c.intlChatHTTP
	}
	return c.chatHTTP()
}

// ApplyResponseHeaderTimeout 把聊天 SSE 首字节上限应用到**所有** transport。
//
// 为什么要有这个方法而不是让调用方自己类型断言：SetProxy 之后 transport 有
// 两个（国服直连 + 国际版代理），调用方只拿到 c.ChatHTTP.Transport 时，
// 国际版那一个会被漏掉 —— 表现为「国服按新上限超时，国际版仍按 120s 干等」，
// 而日志里两边长得一样，极难发现。把遍历收在这里，新增 transport 时只需改一处。
//
// 参数 <=0 表示未设置（保持 null 语义，不做任何改动）。
func (c *Client) ApplyResponseHeaderTimeout(d time.Duration) {
	if d <= 0 {
		return
	}
	for _, cl := range []*http.Client{c.HTTP, c.ChatHTTP, c.intlHTTP, c.intlChatHTTP} {
		if cl == nil {
			continue
		}
		if tr, ok := cl.Transport.(*http.Transport); ok && tr != nil {
			tr.ResponseHeaderTimeout = d
		}
	}
}

// SetRPCTimeout 把短 RPC（refresh/checkin/balance/FetchModels）总时长上限
// 应用到**所有**短 RPC client（国服直连 + 国际版代理）。
//
// 与 ApplyResponseHeaderTimeout 同理：只设 c.HTTP 会让国际版短 RPC 悄悄退回
// 硬编码 120s，与配置不符。ChatHTTP 不动 —— 它的语义就是无总时长
// （Timeout=0），改它等于把长对话截断。
func (c *Client) SetRPCTimeout(d time.Duration) {
	if d <= 0 {
		return
	}
	if c.HTTP != nil {
		c.HTTP.Timeout = d
	}
	if c.intlHTTP != nil {
		c.intlHTTP.Timeout = d
	}
}

// chatBase 返回该账号的 chat 基址（按区域路由）。
func (c *Client) chatBase(a *auth.Auth) string {
	if isIntl(a) {
		if c.BaseIntl != "" {
			return c.BaseIntl
		}
		return "https://www.workbuddy.ai"
	}
	return c.ChatBaseCN
}

// prepareBody 组装出站请求体（脱敏开关由 Client.SanitizeFingerprints 控制）。
//
// intl 为该账号是否国际版（workbuddy.ai）：国际版要求 messages 首条必须是
// system（实测首条 user → HTTP 400 code=11128），需要在此补一条。
func (c *Client) prepareBody(body []byte, intl bool) []byte {
	return PrepareBodyForRegion(body, c.SanitizeFingerprints, c.effortsSnapshot(), intl)
}

// effortsSnapshot 返回 effort 能力缓存副本；nil 表示未知（透传不降级）。
func (c *Client) effortsSnapshot() map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	if len(c.efforts) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(c.efforts))
	for k, v := range c.efforts {
		cp[k] = v
	}
	return cp
}

// billingBase 返回该账号的 billing 基址（按区域路由）。
//
// 国服 billing 与 chat 分属不同域名；国际版两者同域。
func (c *Client) billingBase(a *auth.Auth) string {
	if isIntl(a) {
		if c.BaseIntl != "" {
			return c.BaseIntl
		}
		return "https://www.workbuddy.ai"
	}
	return c.BillingBaseCN
}

// webBase 返回该账号的官网（成长中心）基址。
//
// 国服与国际版都是各自站点的主域：国服 www.workbuddy.cn、国际版 www.workbuddy.ai。
// 与 billingBase 的区别在于 billing 在国服走 codebuddy.cn（计费后台），
// 而成长中心是面向用户的 Web 站点 —— 两者不可互相替代。
func (c *Client) webBase(a *auth.Auth) string {
	if isIntl(a) {
		if c.WebBaseIntl != "" {
			return c.WebBaseIntl
		}
		return "https://www.workbuddy.ai"
	}
	if c.WebBaseCN != "" {
		return c.WebBaseCN
	}
	return "https://www.workbuddy.cn"
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
//
// a 用来决定走哪套 transport（国际版经代理、国服直连，见 httpFor）。
// 由 req 的 Host 反推区域是**不行的**：BaseIntl / WebBaseIntl / ChatBaseCN 都是
// 可注入字段（测试全部指向 httptest 的 127.0.0.1），从 Host 看不出区域；
// 而区域的真值只有 Auth.Domain 一个来源（auth.Auth.IsIntl）。
// a 允许为 nil（少数内部调用不需要账号），此时按国服处理 = 直连。
func (c *Client) doJSON(a *auth.Auth, req *http.Request) (json.RawMessage, error) {
	resp, err := c.httpFor(a).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。全程持 a 锁，防止并发 SaveAtomic 读半更新 token。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	url := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	RefreshHeaders(req, a)
	data, err := c.doJSON(a, req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify(status, string(body))）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	url := c.chatBase(a) + "/v2/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(c.prepareBody(body, isIntl(a))))
	if err != nil {
		return nil, 0, nil, err
	}
	ChatHeaders(req, a)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	resp, err := c.chatClientFor(a).Do(req)
	if err != nil {
		cancel()
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
			a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）；
	// IdleTimeout<=0 时 monitorBody 原样返回底流、无人调 cancel——可接受：
	// ctx 无 deadline 无 goroutine，连接由 resp.Body.Close 正常清理。
	return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens）。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64    // = maxInputTokens
	MaxTokens     int64    // = maxOutputTokens
	Efforts       []string // reasoning.supportedEfforts（空=未知/固定档）
	// SupportsImages 是否接受图片输入。
	//
	// nil **不等于** false：上游 /v3/config 只给对话模型写 supportsImages，
	// 补全/图片生成等条目整条缺失该字段。缺失时是「未声明」，不是「不支持」——
	// 谎报成纯文本会让客户端把本可用的图片能力关掉，所以这里保留三态。
	SupportsImages *bool
}

// modelsConfigUA 拉模型配置用的 User-Agent。
//
// **必须**用 WorkBuddy 前缀（实测 2026-09-15）：
//
//	WorkBuddy/... → 200，返回 WorkBuddy 产品的模型清单
//	CLI/...       → 200，但返回的是 **CodeBuddy** 产品的清单（另一套模型）
//	其他任意 UA    → 400
//
// 两个清单差异很大且各自都"看起来合理"，很容易误判成上游数据错误：
//
//	国际版 WorkBuddy 20 个 / CodeBuddy 17 个
//	  仅 WorkBuddy 有：gpt-6-astra、deepseek-v4.1-flash、hy4-preview-f、kimi-k2.8-preview
//	  仅 CodeBuddy 有：gpt-5.3-codex、minimax-m3
//
// 其中 gpt-6-astra / deepseek-v4.1-flash / kimi-k2.8-preview 实测在国际版**均可正常调用**，
// 说明 WorkBuddy 前缀拿到的才是本产品真实可用集。
//
// 另：UA 只影响本接口的返回内容，不影响 /v2/chat/completions（实测两者 chat 结果一致），
// 因此这里单独覆盖，不动全局 clientUA。
const modelsConfigUA = "WorkBuddy/5.5.2 WorkBuddy/5.5.2 CLI/2.137.1"

// modelsConfigPath 模型配置接口路径。
//
// 为什么不用 /console/enterprises/personal/models：
// 该接口在国际版恒返回 500（openresty 错误页，实测 5/5 账号，
// 与认证方式/请求头无关），导致国际版永远只能靠硬编码静态表。
// /v3/config 返回同一份模型数据且两个区域都可用（实测国际版 21、国服 52）。
const modelsConfigPath = "/v3/config"

// effectiveSupportsImages 合并「模型是否支持图片」与「账号级多模态是否被禁用」。
//
// 三态语义（返回值可能是 nil = 未声明）：
//
//	supportsImages 缺失 + 未禁用 → nil（未声明，客户端按自己的默认处理）
//	supportsImages=true        → true
//	supportsImages=false       → false
//	disabledMultimodal=true    → false（无条件，账号级开关优先）
//
// disabledMultimodal 优先是刻意的：上游用它表达「该账号不能发图片」，
// 与模型自身能力无关，此时宣称支持会让客户端发出必然失败的请求。
func effectiveSupportsImages(supportsImages *bool, disabledMultimodal bool) *bool {
	if disabledMultimodal {
		f := false
		return &f
	}
	return supportsImages
}

// FetchModels 调上游模型配置接口，返回该账号所在区域的可用模型。
//
// 数据来源是 data.agents[name=="cli"].models（**不是** data.models）：
//
//	data.models        = 产品全部模型池，含图片/视频生成、lite 辅助模型等
//	agents[cli].models = CLI agent 真正可用的子集（正是客户端选模型时看到的）
//
// 实测（国际版）：models 21 个，其中 cli.models 20 个；被排除的是
// gemini-3.0-pro-image（图片生成）、hunyuan-video-art（视频生成）、default-model-lite（内部 lite）等。
// 若直接返回 data.models，客户端会看到一批根本不能用于对话的模型。
//
// 元数据（contextWindow/maxOutputTokens/efforts）从 data.models 按 id 关联补齐。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	url := c.chatBase(a) + modelsConfigPath
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", modelsConfigUA)
	resp, err := c.httpFor(a).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID              string `json:"id"`
				Name            string `json:"name"`
				MaxInputTokens  int64  `json:"maxInputTokens"`
				MaxOutputTokens int64  `json:"maxOutputTokens"`
				Disabled        bool   `json:"disabled"`
				// 指针：区分「显式 false」与「字段缺失」（见 ModelInfo.SupportsImages）。
				SupportsImages *bool `json:"supportsImages"`
				// disabledMultimodal：账号级多模态开关。实测当前恒为 false/缺失，
				// 但一旦为 true，即便 supportsImages=true 也不能收图片。
				DisabledMultimodal bool `json:"disabledMultimodal"`
				Reasoning          struct {
					Effort           string   `json:"effort"`
					SupportedEfforts []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}

	// 先按 id 建索引，便于给 cli 清单补元数据。
	// disabled 单独记一份：agents[cli].models 是「这个 agent 允许用哪些模型」的白名单，
	// 而 disabled 是模型级的停用开关，被停用的模型可以仍留在白名单里。
	// 两者都不看会把已停用的模型下发给客户端（选中即报错）。
	meta := make(map[string]ModelInfo, len(env.Data.Models))
	disabled := make(map[string]bool, len(env.Data.Models))
	for _, m := range env.Data.Models {
		if m.ID == "" {
			continue
		}
		if _, dup := meta[m.ID]; dup {
			continue
		}
		meta[m.ID] = ModelInfo{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.MaxInputTokens,
			MaxTokens:     m.MaxOutputTokens,
			Efforts:       m.Reasoning.SupportedEfforts,
			// 账号级多模态开关为 true 时强制降级为 false：上游语义是
			// 「即便模型本身支持，该账号也不许用图片」，此时不能宣称支持。
			SupportsImages: effectiveSupportsImages(m.SupportsImages, m.DisabledMultimodal),
		}
		if m.Disabled {
			disabled[m.ID] = true
		}
	}

	// 取 cli agent 的可用清单（这是客户端真正能选的模型）。
	var cliModels []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliModels = ag.Models
			break
		}
	}

	out := make([]ModelInfo, 0, len(cliModels))
	seen := make(map[string]bool, len(cliModels))
	for _, id := range cliModels {
		if id == "" || seen[id] {
			continue
		}
		if mi, ok := meta[id]; ok {
			// 上游显式标了 disabled 的不下发（与旧实现一致）。
			// 池里查不到该 id 时无从判断，按「宁可多」返回。
			if disabled[id] {
				continue
			}
			seen[id] = true
			out = append(out, mi)
			continue
		}
		// cli 清单里有、models 池里没有：仍要返回（它确实可用），只是元数据未知。
		seen[id] = true
		out = append(out, ModelInfo{ID: id})
	}

	// 兜底：上游没给 cli agent 时退回全量池（宁可多不可少，保持旧行为）。
	if len(out) == 0 {
		for _, m := range env.Data.Models {
			if m.Disabled || m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			out = append(out, meta[m.ID])
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	// 刷新 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入缓存）。
	cache := make(map[string][]string, len(out))
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
	}
	c.effortsMu.Lock()
	c.efforts = cache
	c.effortsMu.Unlock()
	return out, nil
}

// resourcePackage billing get-user-resource 返回的单个套餐。
type resourcePackage struct {
	PackageName         string `json:"PackageName"`
	CapacitySize        int64  `json:"CapacitySize"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`

	// 到期字段（实测国服与国际版均返回）：
	//
	//	DeductionEndTime 抵扣截止（epoch 毫秒）—— 额度真正失效的时刻，优先采用
	//	ExpiredTime / CycleEndTime 兼容回退（实测可能是 "2006-01-02 15:04:05" 字符串）
	//
	// 声明为 any 是因为同一字段在不同区域/套餐上出现过数字与字符串两种形态。
	DeductionEndTime any `json:"DeductionEndTime"`
	ExpiredTime      any `json:"ExpiredTime"`
	CycleEndTime     any `json:"CycleEndTime"`
}

// remain 该套餐可花费积分（与原聚合口径逐字一致：Cycle* 优先，负值钳 0）。
func (p resourcePackage) remain() int64 {
	var r int64
	switch {
	case p.CycleCapacitySize > 0:
		r = p.CycleCapacityRemain
	case p.CycleCapacityRemain > 0 || p.CycleCapacityUsed > 0:
		r = p.CycleCapacityRemain
	default:
		r = p.CapacityRemain
	}
	if r < 0 {
		r = 0
	}
	return r
}

// expiryUnix 该套餐的到期时刻（Unix 秒）；0 = 未知。
func (p resourcePackage) expiryUnix() int64 {
	for _, v := range []any{p.DeductionEndTime, p.ExpiredTime, p.CycleEndTime} {
		if at := parseExpiryUnix(v); at > 0 {
			return at
		}
	}
	return 0
}

// normalizeEpoch 把秒/毫秒 epoch 统一成秒（上游混用两种精度）。
func normalizeEpoch(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if n > 1_000_000_000_000 {
		return n / 1000
	}
	return n
}

// parseExpiryUnix 解析上游到期字段，兼容 epoch 秒/毫秒与常见日期字符串。
func parseExpiryUnix(v any) int64 {
	switch t := v.(type) {
	case float64:
		return normalizeEpoch(int64(t))
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return 0
		}
		return normalizeEpoch(n)
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return normalizeEpoch(n)
		}
		for _, layout := range []string{
			"2006-01-02 15:04:05",
			"2006-01-02T15:04:05Z07:00",
			"2006-01-02 15:04:05.999",
		} {
			if ts, err := time.ParseInLocation(layout, s, time.Local); err == nil {
				return ts.Unix()
			}
		}
		// 仅日期：按当日 23:59:59 计（额度一般用到当天结束）。
		if ts, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
			return ts.Add(24*time.Hour - time.Second).Unix()
		}
	}
	return 0
}

// CreditInfo 账号积分余额与到期信息（billing get-user-resource 的归一化结果）。
type CreditInfo struct {
	// Remain 所有套餐可花费积分之和（负值钳 0）。
	Remain int64
	// SoonestExpireAt 仍有剩余积分的套餐中最早的到期时刻（Unix 秒）；0 = 未知。
	//
	// 供账号池做「按到期紧迫度分层」选号：先烧快过期的额度，避免积分作废。
	SoonestExpireAt int64
}

// UserResource 查询账号当前可花费积分余额（所有套餐 CycleCapacity 聚合，负值钳 0）。
func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) {
	info, err := c.UserResourceDetail(a)
	return info.Remain, err
}

// UserResourceDetail 查询积分余额与「最近到期」时刻（一次请求同时取回，不额外打上游）。
func (c *Client) UserResourceDetail(a *auth.Auth) (CreditInfo, error) {
	url := c.billingBase(a) + "/v2/billing/meter/get-user-resource"
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return CreditInfo{}, err
	}
	BillingHeaders(req, a)
	data, err := c.doJSON(a, req)
	if err != nil {
		return CreditInfo{}, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []resourcePackage `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return CreditInfo{}, fmt.Errorf("resource parse: %w", err)
	}
	var info CreditInfo
	for _, acct := range resp.Response.Data.Accounts {
		r := acct.remain()
		info.Remain += r
		// 只有「还有剩余」的套餐才代表真实到期压力；已用尽的套餐到期日再早也无意义。
		if r <= 0 {
			continue
		}
		if at := acct.expiryUnix(); at > 0 && (info.SoonestExpireAt == 0 || at < info.SoonestExpireAt) {
			info.SoonestExpireAt = at
		}
	}
	return info, nil
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	url := c.billingBase(a) + "/v2/billing/meter/daily-checkin"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	BillingHeaders(req, a)
	_, err = c.doJSON(a, req)
	return err
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
