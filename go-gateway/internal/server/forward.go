package server

// forward.go 抽出「选号 → 轮转 → 转发上游 chat/completions」的核心流程，
// 供三种协议入口共用：
//
//   - POST /v1/chat/completions  原生 OpenAI Chat Completions（本文件直通）
//   - POST /v1/responses         OpenAI Responses API（Codex / ChatGPT 系）
//   - POST /v1/messages          Anthropic Messages API（Claude Code / Claude Desktop）
//
// 设计：后两者在进入本流程之前把请求体转换成 OpenAI Chat 形态，
// 拿到上游的 chat 响应（非流式 map 或原始 SSE 流）后，各自再转回目标协议。
// 这样账号池、粘性会话、熔断冷却、积分冷却、统计日志全部只有一份实现，
// 协议适配层只负责「形状转换」，不碰任何调度状态。

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// modelLockedError 请求的模型不在「限制使用的模型」白名单内。
//
// 单独成型（而不是拼一个字符串）是为了让调用方能识别它并回以 400 +
// 明确的错误码，而不是当成「账号不可用」的 503 —— 后者会误导用户去查账号。
//
// 名字保留历史叫法（Locked）：错误码 model_not_allowed 与各协议的判定链
// 都建在它上面，改名的收益抵不过动这条链的风险。
type modelLockedError struct {
	requested string   // 客户端请求的模型（可能为空，表示请求体未带 model）
	allowed   []string // 当前放行的模型白名单（非空；空名单根本不会走到这里）
}

func (e *modelLockedError) Error() string {
	got := e.requested
	if got == "" {
		got = "(未指定)"
	}
	list := allowedModelsText(e.allowed)
	// 文案必须**列出全部**允许的模型：用户配了 3 个模型时只报「不允许 x」
	// 完全没法排查 —— 他不知道该改成哪一个，只能挨个试。
	return "当前已限制可使用的模型，只允许调用 " + list + "；收到的是 " + got +
		"。请在客户端把模型改为 " + list + " 中的任意一个，或在界面的「放行模型」里调整限制。"
}

// chatResult 一次成功的上游调用结果。
//
// 二者互斥：
//   - Stream != nil  → 流式：调用方负责 Close，并按目标协议解析/转换 SSE。
//   - Response != nil → 非流式：上游 SSE 已被 Aggregate 成 OpenAI chat.completion。
type chatResult struct {
	UID      string
	Model    string
	Stream   io.ReadCloser
	Response map[string]any
}

// forwardChat 执行「选号 → token 刷新 → 转发 → 失败换号」的完整轮转。
//
// 参数：
//   - body：已转换成 OpenAI Chat 形态的请求体（原始字节，发往上游前由
//     upstream.Client 再做一次 PrepareBody：强制 stream、归一化 role/tool_choice）。
//   - stream：调用方是否要求流式。上游恒为流式，非流式时本函数读完后 Aggregate。
//   - sessKey：会话粘性键；空串表示不做粘性绑定。
//
// 返回：
//   - result：成功时非 nil。
//   - status/lastErr：失败时给出应回给客户端的 HTTP 状态与最后一处错误，
//     调用方据此生成对应协议的错误体。
//
// 失败语义与原有 chatCompletions 完全一致：传输层错误只换号不喂熔断，
// 业务错误按 Classify 结果施加冷却/禁用/熔断。
func (h *Handler) forwardChat(body []byte, stream bool, sessKey string) (*chatResult, int, error) {
	tried := map[string]bool{}
	var lastErr error
	var lastUID string
	lastStatus := http.StatusServiceUnavailable
	// lastKind/lastBody 记录最后一次上游失败的分类与原始响应体，
	// 用于在全部账号失败时给客户端一句**可读**的原因（见函数末尾的 FriendlyMessage）。
	var lastKind upstream.ErrKind
	var lastBody string
	// lastTransportErr 非 nil 表示最后一次失败是传输层（无上游响应体）。
	var lastTransportErr error

	var stickyUID string
	if sessKey != "" && h.cfg.Session != nil {
		if uid, ok := h.cfg.Session.Resolve(sessKey); ok {
			stickyUID = uid
		}
	}

	// 在途租约：成功选中即占名额，函数出口统一释放（成功即转移给调用方持有）。
	var heldUID string
	var handedOff bool
	defer func() {
		if heldUID != "" && !handedOff {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID && h.cfg.Session != nil {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}

	// 请求的目标模型：用于「模型级限流」的选号过滤与冷却记账。
	// 取不到时为空串，各环节自动退化为原有行为（不做模型过滤）。
	//
	// 同时解析可选的区域前缀 `cn:` / `global:`：前缀只是给网关的**选号指令**，
	// 上游不认识它，故必须把请求体里的 model 改写成裸名（见下方 rewriteModel）。
	// 注意返回顺序是 (realm, bare) —— 写反会把区域当成模型名，
	// 表现为「单一模型锁定」报「收到的是 (未指定)」。
	//
	// realm 不再参与选号：上游已把区域约束升级为 route/preferRegion
	//（含「按区域的模型能力真值」），比字符串 realm 更完整且能区分
	// 「偏好」与「强制」。这里只取 bare 用于改写请求体。
	_, model := resolveModel(modelOf(body))

	// 前缀只是给网关的**选号指令**，上游不认识它 —— 必须把请求体里的
	// model 改写成裸名，否则上游返回 400 code=11102 model [cn:xxx] not found
	//（实测确认：前缀成功约束了选号，却让请求本身失败）。
	// 无前缀时 model 与原值相同，rewriteModel 会原样返回，不做多余序列化。
	// 系统提示词替换必须排在 rewriteModel **之前**：两者都会重新序列化请求体，
	// 先做提示词替换可以少一次整体编码，也避免「模型已改写成裸名、提示词却没换」
	// 这种半改状态的中间结果出现在日志/排查视野里。
	//
	// 放在 forwardChat 而不是各协议入口：三种协议（chat / messages / responses）
	// 的请求体都在这里汇合成 OpenAI Chat 形态，在此改写只需一处，
	// 也不会漏掉任何一条出站路径。
	//
	// mode=passthrough（缺省）时**完全不调用** Rewrite：既有行为必须逐字不变
	//（不重新序列化、不动 messages），因此这里是显式分支而非「传空串让它空转」。
	if h.cfg.PromptMode == prompt.ModeCustom && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	}

	body = rewriteModel(body, model)

	// 区域路由：仅对「图像能力两区不同」的模型 + 带图片的请求生效，
	// 其余情况返回 RegionAny，行为与引入本特性之前完全一致。
	// 详见 imageRouteFor 的注释。
	route := imageRouteFor(model, requestHasImage(body))
	preferRegion := route.Region

	// 「限制使用的模型」白名单：非空时只放行名单内的模型。
	//
	// 为什么在选号之前就拒绝（而不是换个模型重试）：这份名单是**策略**的一部分
	//（轮转模式下尤其如此 —— 语义是「把这个账号的指定模型额度烧干净再换号」）。
	// 若允许其他模型通过，客户端换个模型就能绕过轮转与额度控制，也让「当前烧的
	// 是哪个模型」变得不可预期 —— 因此明确拒绝并说明原因，比静默改写模型
	//（用户以为在用 A、实际用了 B）更安全。
	//
	// 比较用的是剥前缀后的**裸模型名**（上面 resolveModel 的结果）：用户配的是
	// `deepseek-v4.1-flash`，客户端可能带 `cn:` 前缀请求，两者应视为同一个模型。
	// 大小写不敏感 —— 客户端写法并不统一。名单为空 = 不限制（默认，向后兼容）。
	//
	// 名单取自 h.allowed（NewHandler 里归一化并缓存），不是 h.cfg.AllowedModel ——
	// 后者是单值的历史字段，已在归一化时合并进 h.allowed。
	if !modelAllowed(h.allowed, model) {
		// 返回非 nil 的 result：调用方会在错误分支里读 result.UID 记日志，
		// 返回 nil 会 panic。UID 留空即可（本次没有选中任何账号）。
		return &chatResult{Model: model}, http.StatusBadRequest,
			&modelLockedError{requested: model, allowed: h.allowed}
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDForModelRegion(stickyUID, model, preferRegion)
			if acct == nil {
				if h.cfg.Session != nil {
					h.cfg.Session.Unbind(sessKey)
				}
				stickyUID = ""
			}
		}
		if acct == nil {
			acct = h.pickAccount(model, tried, route)
		}
		if acct == nil {
			// 区域受限且选不出号：**不降级**，明确告诉用户缺哪个区域的账号。
			// 静默跨区会让图片被后端换成占位符，模型回「我看不见图片」——
			// 用户完全无从判断是网络、模型还是网关的问题。
			if route.Required {
				return &chatResult{Model: model}, http.StatusServiceUnavailable,
					&forwardFailure{
						Kind:    FailureImageRegionUnavailable,
						Status:  http.StatusServiceUnavailable,
						Message: imageRegionUnavailableMessage(route.Region),
					}
			}
			lastStatus = http.StatusServiceUnavailable
			break
		}
		tried[acct.UID] = true
		lastUID = acct.UID

		if !h.cfg.Pool.Acquire(acct.UID) {
			if stickyUID != "" && acct.UID == stickyUID && h.cfg.Session != nil {
				h.cfg.Session.Unbind(sessKey)
				stickyUID = ""
			}
			continue
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				// 末尾的可读文案读的是 lastKind/lastBody/lastTransportErr，三者必须
				// 与 lastErr 同步更新 —— 否则会沿用**上一个账号**留下的分类，例如把
				// 「刷新失败」报成「额度已耗尽」，把用户引向错误的排查方向。
				if errors.As(err, &ue) {
					lastKind, lastTransportErr = ue.Kind, nil
				} else {
					// 非 upstream.Error 的失败基本都是传输层（超时 / 连接被拒）
					lastKind, lastTransportErr = upstream.ErrNone, err
				}
				lastBody = ""
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				log.Printf("chat refresh uid=%s: save auth failed: %v", acct.UID, err)
			}
		}

		rc, status, respBody, terr := h.cfg.Upstream.ChatStream(acct, body)
		if terr != nil {
			lastStatus = http.StatusServiceUnavailable
			lastErr = terr
			// 传输层失败（超时/连接被拒/DNS）没有上游业务体，Classify 不适用；
			// 单独标记，使末尾的错误文案也能给出可读原因而不是原始 Go 报错
			// （实测：原始文案会把 tcp 四元组与 wsarecv 细节直接抛给客户端）。
			lastKind = upstream.ErrNone
			lastBody = ""
			lastTransportErr = terr
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			lastStatus = status
			kind := upstream.Classify(status, string(respBody))
			lastKind = kind
			lastBody = string(respBody)
			lastTransportErr = nil
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}

			// 上下文超长是**请求侧**错误：换号无用（同一请求体发给任何账号都同样失败），
			// 继续轮转只会把整个请求体对着每个账号重传一遍（实测 1.12M token × 3），
			// 最后还被伪装成「账号全部不可用」，把排查方向引向账号故障。
			//
			// 立即以真实状态返回，交由客户端精简上下文后重试。
			// 不罚账号也不换号：账号状态完全不动（applyErrorPolicy 对请求侧错误
			// 本就只换号不罚，这里连换号都省掉）。
			//
			// 这是**唯一**改变对外状态码的路径；其余失败仍沿用原有的
			// 503 no_healthy_account 契约（账号池耗尽的语义）。
			if kind == upstream.ErrContextTooLong {
				uid := acct.UID
				releaseHeld()
				// 带上 UID：调用方在失败路径也要读 result.UID 记日志，
				// 返回 nil 会让它空指针崩溃（本测试即抓到此点）。
				return &chatResult{UID: uid}, status, &forwardFailure{
					Kind:   FailureContextTooLong,
					Status: status,
					// 保留上游原文：下游客户端靠文案识别上下文溢出并触发自动压缩，
					// 只回我们自己的措辞会让它认不出这是溢出。
					Message: upstream.ContextTooLongMessage(string(respBody)),
				}
			}

			h.applyErrorPolicy(acct.UID, model, kind, string(respBody))
			fail(acct.UID)
			continue
		}

		h.cfg.Pool.NoteSuccess(acct.UID)
		// 粘性跟随最终成功号。
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}

		uid := acct.UID
		handedOff = true // 租约移交调用方，由其读完/关闭后释放

		if stream {
			return &chatResult{UID: uid, Model: modelOf(body), Stream: rc}, status, nil
		}

		resp, err := upstream.Aggregate(rc)
		rc.Close()
		h.cfg.Pool.Release(uid)
		handedOff = false
		heldUID = ""
		if err != nil {
			return nil, http.StatusBadGateway, err
		}
		return &chatResult{UID: uid, Model: modelOf(body), Response: resp}, http.StatusOK, nil
	}

	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		// 客户端可读性：优先用提炼后的原因（如「账号额度已耗尽…」），
		// 拿不到再用原始文案 —— 原始文案含整段上游 JSON 或 tcp 底层细节，又长又难懂。
		if friendly := upstream.FriendlyMessage(lastKind, lastStatus, lastBody); friendly != "" {
			msg = friendly
		} else if lastTransportErr != nil {
			msg = "无法连接上游（网络超时 / 连接被拒）：请检查本机网络或代理设置后重试"
		} else {
			msg += ": " + lastErr.Error()
		}
	}
	// 失败时也带上最后尝试过的账号，请求日志据此仍能显示 uid（与原实现一致）。
	return &chatResult{UID: lastUID}, lastStatus, errors.New(msg)
}

// FailureKind 失败类别：决定回给客户端的错误码。
//
// 存在的意义是让**请求侧**错误说实话：旧实现无论什么原因都回
// no_healthy_account + "all accounts unavailable (cooling/disabled)"，
// 把「这次请求太大」伪装成「账号全挂了」，排查时被直接带偏（2026-09-15 现场）。
type FailureKind int

const (
	// FailureUpstream 其余上游失败：沿用既有契约（503 no_healthy_account）。
	// 账号池耗尽的语义由它承载，客户端据此稍后重试。
	FailureUpstream FailureKind = iota
	// FailureContextTooLong 请求上下文超出模型窗口：请求侧错误，换号无用。
	FailureContextTooLong
	// FailureImageRegionUnavailable 带图片的请求需要一个特定区域的账号，
	// 而账号池里该区域此刻没有可用账号。
	//
	// 单独成型是为了让错误文案说清「缺什么、该怎么办」—— 与账号池耗尽的
	// 503 不同，这不是「稍后重试就好」，而是「你得加一个那个区域的账号」。
	FailureImageRegionUnavailable
)

// imageRegionUnavailableMessage 生成「带图片请求缺少该区域账号」的说明。
//
// 必须同时给出**原因**与**出路**：这类失败用户第一次遇到时完全无法自行判断
// （网关返回 503，客户端只显示「服务不可用」），而原因（上游两区同名模型的
// 图像能力不同）与出路（补一个那个区域的账号）都只有网关知道。
func imageRegionUnavailableMessage(region auth.Region) string {
	name := "国服"
	if region == auth.RegionIntl {
		name = "国际版"
	}
	return "该模型带图片的请求需要" + name + "账号（实测只有" + name +
		"后端能读取图片，另一个区域会把图片替换成占位符后交给模型，" +
		"表现为模型回复「无法查看图片」），但账号池里此刻没有可用的" + name +
		"账号。请添加/启用一个" + name + "账号，或去掉图片后重试。"
}

// forwardFailure 一次需要特殊上报的转发失败。
//
// 只有需要偏离「503 no_healthy_account」默认契约的失败才用它；
// 其余失败仍是普通 error，行为与旧实现完全一致。
type forwardFailure struct {
	Kind    FailureKind
	Status  int    // 回给客户端的 HTTP 状态
	Message string // 面向客户端的错误消息
}

func (e *forwardFailure) Error() string { return e.Message }

// pickAccount 按 imageRoute 选号。
//
// 与旧的 PickForModelRegion（偏好语义）的区别在于 Required：
//
//	Required=false → 偏好：先在该区域挑，挑不到放开到全池（旧行为）。
//	                 「区域不符但能用」好过因为该区域没号而失败。
//	Required=true  → 强制：只在该区域挑，挑不到返回 nil，由调用方报错。
//	                 此时跨区降级**不是**「能用就行」—— 后端会静默丢弃图片，
//	                 用户拿到的是「模型说它看不见图片」，无从排查。
func (h *Handler) pickAccount(model string, tried map[string]bool, route imageRoute) *auth.Auth {
	if route.Region == auth.RegionAny {
		return h.cfg.Pool.PickForModelRegion(model, tried, auth.RegionAny)
	}
	if route.Required {
		return h.cfg.Pool.PickForModelRegionStrict(model, tried, route.Region)
	}
	return h.cfg.Pool.PickForModelRegion(model, tried, route.Region)
}

// failureOf 取出 *forwardFailure（若有），供各协议入口按类别选错误码。
func failureOf(err error) *forwardFailure {
	var f *forwardFailure
	if errors.As(err, &f) {
		return f
	}
	return nil
}

// errText 供各协议入口取失败文案。
func errText(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

// release 供调用方在流式转发结束后归还租约。
func (h *Handler) release(uid string) {
	if uid != "" {
		h.cfg.Pool.Release(uid)
	}
}

// modelOf 从 OpenAI Chat 请求体里取 model 字段（仅用于日志与回填响应）。
func modelOf(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Model
}

// rewriteModel 把请求体里的 model 字段改成 bare。
//
// 为什么必须改写：`cn:` / `global:` 前缀只是**给网关的选号指令**，
// 上游并不认识它。若把带前缀的原串原样转发，上游会返回
//
//	400 code=11102 model [cn:glm-5.2] service info not found
//
// 即「前缀成功约束了选号，却让请求本身失败」——
// 实测确认过这个现象（前缀请求全部 11102，无前缀的同名请求正常）。
//
// bare 为空或与当前值相同时返回原 body（不重新序列化，避免无谓的格式变化）。
// 解析失败时同样原样返回：宁可让上游报错，也不要把请求体改坏。
func rewriteModel(body []byte, bare string) []byte {
	if bare == "" {
		return body
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return body
	}
	cur, ok := doc["model"]
	if !ok {
		return body // 没有 model 字段：无从改写
	}
	var curStr string
	if err := json.Unmarshal(cur, &curStr); err != nil || curStr == bare {
		return body // 不是字符串或无需改动
	}
	encoded, err := json.Marshal(bare)
	if err != nil {
		return body
	}
	doc["model"] = encoded
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

// requestHasImage 报告 OpenAI Chat 请求体里是否携带图片分片。
//
// 三种协议入口最终都会把图片归一成 `{"type":"image_url", ...}` 分片
// （responses.go 的 input_image、messages.go 的 Anthropic image 块），
// 因此只需在这里认这一种形状。
//
// 判据用「分片里存在 image_url 键」而不是「type == image_url」：
// 上游/客户端对 image 分片的 type 写法不止一种（实测有 image_url、
// input_image），按 type 精确匹配会漏判，而漏判的后果是请求被路由到
// 读不到图片的后端 —— 正是本函数要避免的。
func requestHasImage(body []byte) bool {
	var probe struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	for _, m := range probe.Messages {
		if len(m.Content) == 0 {
			continue
		}
		// content 可能是字符串（纯文本）或分片数组；只有数组才可能含图片。
		var parts []map[string]any
		if err := json.Unmarshal(m.Content, &parts); err != nil {
			continue
		}
		for _, p := range parts {
			if _, ok := p["image_url"]; ok {
				return true
			}
			if t, _ := p["type"].(string); t == "image_url" || t == "input_image" || t == "image" {
				return true
			}
		}
	}
	return false
}

// imageRoute 带图片的请求该如何选号。
type imageRoute struct {
	// Region 应使用的区域；RegionAny = 不做区域约束。
	Region auth.Region
	// Required 为 true 时**只**在该区域选号，选不出就报错，绝不跨区降级。
	//
	// 与「偏好」的区别是本特性的核心：偏好会在该区域没号时静默回退到读不到图的
	// 后端，用户拿到一句「抱歉，我无法查看图片」却毫不知情；Required 则明确
	// 告诉用户「需要哪个区域的账号」，可排查、可行动。
	Required bool
}

// imageRouteFor 报告「带图片的该模型请求」应使用的区域。
//
// 背景（实测 2026-09-16，逐账号 × 逐模型发图验证，见 measured.go 的实测表）：
//
//	glm-5.3 / glm-5.2 是**两区共有**的模型名，但两区是**不同的后端模型**。
//	国服后端能读图；国际版后端把图片替换成固定占位符（prompt_tokens 增量
//	恒为 +33，与图片体积无关），模型只能回「无法查看图片」。
//
// 池里国服与国际版账号混用，选号又只看到期日与冷却、不看区域，
// 于是同一个 glm-5.3 会随机命中两个后端 —— 用户看到「时好时坏」。
//
// 三种情形：
//
//	不带图片                  → 不约束（RegionAny）。两个区域的文本能力都正常，
//	                            没必要为纯文本放弃一半账号的额度。
//	带图片 + 该模型**只在一区**可读 → Required：迁移到那个区域，选不出就报错。
//	带图片 + 两区都能读        → 不约束（RegionAny）。例如 hy3 / kimi-k2.6
//	                            实测两区都能读，对它们偏好只会白白损失一半额度。
//
// 与 measured.go 的分工：那里给的是「某模型在某区域能不能读图」的**事实**，
// 这里把它翻译成**路由决策**。两者共用同一份实测表，不会各自漂移。
func imageRouteFor(model string, hasImage bool) imageRoute {
	if !hasImage {
		return imageRoute{Region: auth.RegionAny}
	}
	cn := measuredImageCapability(model, auth.RegionCN) == measSupported
	intl := measuredImageCapability(model, auth.RegionIntl) == measSupported
	switch {
	case cn && intl:
		// 两区都能读：不约束。对它们做偏好只会放弃一半账号的额度而无任何收益。
		return imageRoute{Region: auth.RegionAny}
	case cn && !intl:
		return imageRoute{Region: auth.RegionCN, Required: true}
	case intl && !cn:
		return imageRoute{Region: auth.RegionIntl, Required: true}
	default:
		// 两区都读不到，或该模型没有实测结论：不约束，交给运行时按原策略处理。
		// 这里**不**直接报错 —— 「没实测过」不等于「不行」，把没验过的模型
		// 一律拒掉会误伤本可用的图片能力。
		return imageRoute{Region: auth.RegionAny}
	}
}

// buildSessKey 为没有原生会话字段的协议（如 Anthropic Messages）合成粘性键。
//
// Anthropic 请求体没有 conversation_id，但有 system + 首条 user 消息；
// 用它们的短哈希做键即可让同一会话稳定命中同一账号。
func buildSessKey(seed string) string {
	if seed == "" {
		return ""
	}
	return session.SessionKeyFromSeed(seed)
}
