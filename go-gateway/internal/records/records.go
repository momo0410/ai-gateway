// Package records 把网关侧自动任务的执行结果写进宿主的账号记录文件
// （account_records.json），让它们出现在界面「账号记录 → 任务」分类里。
//
// 为什么需要它：活跃上报 / 夜猫子 / 开学季 / trial 这 4 个任务实现在 Go 网关里，
// 它们此前只调 log.Printf 写 stdout，而宿主启动网关子进程时把 stdout/stderr
// 丢进了 null —— 日志永远到不了界面。用户看到的现象是「任务跑了，但一条记录都没有」。
//
// 设计要点（与宿主侧 crates/.../account_records.rs 严格对齐）：
//
//  1. **同一个文件、同一种结构**。不新增第二套格式：界面只读 account_records.json，
//     换个格式等于用户还是看不到。字段名逐字对齐
//     （ts / accountId / accountName / kind / title / result / amount / detail）。
//  2. **写入绝不失败主流程**。记录是旁路观测数据，写不进去只打日志。
//     与宿主侧注释「写入绝不 panic」是同一条原则。
//  3. **原子写（临时文件 + rename）**。宿主用 serde_json 解析**整个文件**，
//     解析失败会退化成「空记录集」，后续写入就把历史整体覆盖掉。
//     因此绝不能出现「半截文件」——这正是原子写的意义。
//  4. **路径只有一个来源**。由宿主写 native config 时把绝对路径透传下来。
//     两边各自推算数据目录（宿主看 AI_GATEWAY_HOME，网关看自己的 cwd）
//     迟早会算出不同的值，而那种错法表现为「静默不记录」，极难排查。
package records

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxRecords 记录上限，与宿主侧 ACCOUNT_RECORD_MAX 保持一致。
//
// 两边数值不同会出现「网关裁一批、宿主再裁一批」，用户看到的条数与保留策略对不上。
const MaxRecords = 20000

// DefaultRetentionDays 兜底保留天数，与宿主侧 RECORD_RETENTION_DEFAULT_DAYS 一致。
// 正常路径下保留天数由宿主透传（见 Config.AccountRecords），这里只防配置缺失。
const DefaultRetentionDays = 60

// 记录类型与结果值（宿主侧 KIND_TASK / 界面配色按这些字面量匹配，改动需两边同步）。
const (
	KindTask = "task"

	ResultSuccess = "success"
	ResultFailed  = "failed"
	ResultAlready = "already"
	ResultInfo    = "info"
)

// Identity 账号在宿主账号库里的身份。
//
// 为什么必须由宿主传下来、而不能用网关手上的 uid 顶替：
//
//   - 界面「账号卡片 → 查看记录」按 accounts.json 的 **id**（uuid）过滤，
//     而网关凭证里只有 uid。网关把 uid 当 accountId 写进去，用户点开某个账号
//     的记录永远查不到这些任务 —— 过滤条件根本对不上，且不会报任何错。
//   - name 同理：宿主取 account_display_name（email → nickname → uid），
//     而网关的凭证里没有 email，只有 nickname/uid。
type Identity struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// record 一条账号记录。字段顺序与宿主侧 AccountRecord::to_json 保持一致，
// 便于两边直接 diff 文件。
//
// 为什么**没有** Source 字段：网关只写 task 记录，而 source 只对积分变化有意义
//（宿主侧 account_records.rs 的 CREDIT_SOURCE_*）。网关不写积分记录，
// 因此这里刻意不声明它 —— 声明一个永不赋值的字段，只会让人误以为网关也会写来源。
//
// 那宿主的 source 会不会被网关弄丢？不会：append 走的是 load()，它用
// []json.RawMessage **按原始字节**透传每条既有记录，未知字段一个比特都不动。
// 这条契约有 TestPreservesHostCreditSourceField 专门锁住 —— 它是两边共用
// 同一个文件时最容易出错的地方（丢了字段只会表现为「界面上来源徽标不见了」）。
type record struct {
	Ts          int64  `json:"ts"`
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Result      string `json:"result"`
	Amount      int64  `json:"amount"`
	Detail      string `json:"detail"`
}

// Recorder 账号记录写入器。零值不可用，请用 New 构造。
//
// 并发安全：4 个任务可能在同一时刻被派发（scheduler.Run 里按到点时刻批量触发），
// 加上积分巡检等 goroutine，写入必须串行化，否则「读-改-写」会互相覆盖。
type Recorder struct {
	mu            sync.Mutex
	path          string
	retentionDays int
	identities    map[string]Identity

	// now 供测试注入时钟（保留天数裁剪与「每日一次」去重都依赖它）。
	now func() time.Time
}

// New 构造写入器。
//
// path 为空表示未启用（例如脱离宿主单独跑 gateway.exe）：
// 此时所有写入都是静默 no-op，不报错也不刷日志 —— 旁路观测数据不该让
// 独立运行模式产生噪音。
func New(path string, retentionDays int, identities map[string]Identity) *Recorder {
	if retentionDays <= 0 {
		retentionDays = DefaultRetentionDays
	}
	if identities == nil {
		identities = map[string]Identity{}
	}
	return &Recorder{
		path:          strings.TrimSpace(path),
		retentionDays: retentionDays,
		identities:    identities,
		now:           time.Now,
	}
}

// Enabled 报告是否配置了记录文件路径。
func (r *Recorder) Enabled() bool { return r != nil && r.path != "" }

// Path 记录文件路径（供启动日志与排查用）。
func (r *Recorder) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

// IdentityIndex 返回 uid → Identity 映射的副本（供需要「宿主 id ↔ 网关 uid」
// 双向翻译的调用方使用，如成长任务入口按 accountId 找账号）。
//
// 为什么返回副本而不是直接暴露 map：调用方多为并发路径，暴露内部 map
// 会让它在别人写入时读到半更新的状态。账号是几十级规模，复制代价可忽略。
func (r *Recorder) IdentityIndex() map[string]Identity {
	if r == nil {
		return nil
	}
	out := make(map[string]Identity, len(r.identities))
	for uid, id := range r.identities {
		out[uid] = id
	}
	return out
}

// Task 追加一条任务记录（每次都写）。
//
// uid 用于解析宿主的 accountId / accountName；成功后调用（有新变化时）。
func (r *Recorder) Task(uid, title, result, detail string) {
	r.append(uid, title, result, detail, false)
}

// TaskDaily 追加一条任务记录，但同一账号 + 同一标题 + 同一结果**每天最多一条**。
//
// 为什么需要：任务是按整点排程的，但用户可能手工触发、机器休眠后迟到唤醒也会补跑，
// 每次都写就会把「今天的记录」刷成一片重复。去重键含 result，
// 因此「今天先失败、后成功」两条都保留（那正是需要看到的转变）。
func (r *Recorder) TaskDaily(uid, title, result, detail string) {
	r.append(uid, title, result, detail, true)
}

// TaskAllDaily 追加一条「与具体账号无关」的汇总任务记录（每天最多一条）。
//
// 用于整体条件判断，例如「夜猫窗口外」「活动整体不在期」——
// 这类结论对每个账号都一样，逐账号各写一条等于把记录刷爆。
func (r *Recorder) TaskAllDaily(title, result, detail string) {
	r.append("", title, result, detail, true)
}

// append 读-改-写一条记录。失败只打日志，绝不影响调用方主流程。
func (r *Recorder) append(uid, title, result, detail string, daily bool) {
	if !r.Enabled() {
		return
	}

	id, name := r.identity(uid)
	rec := record{
		Ts:          r.now().UnixMilli(),
		AccountID:   id,
		AccountName: name,
		Kind:        KindTask,
		Title:       title,
		Result:      result,
		Amount:      0,
		Detail:      oneLine(detail, 300),
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	all, ok := r.load()
	if !ok {
		// 文件存在但读不出来（不是 JSON 数组 / 编码损坏）：与宿主侧 load_raw 同款容错，
		// 视作空集从头开始。硬失败会让用户的记录永久停更，比丢一批脏数据更糟。
		log.Printf("records: %s 不可解析，按空记录集继续写入", r.path)
	}
	if daily && alreadyToday(all, rec) {
		return
	}
	all = append(all, marshalRecord(rec))
	kept := normalize(all, rec.Ts, r.retentionDays)
	if err := atomicWrite(r.path, mustJSONArray(kept)); err != nil {
		// 旁路数据：写失败只记日志（与宿主侧「写入绝不 panic」同一条原则）。
		log.Printf("records: 写入 %s 失败: %v", r.path, err)
	}
}

// identity 把网关的 uid 映射成宿主的 accountId / accountName。
func (r *Recorder) identity(uid string) (string, string) {
	if uid == "" {
		// 汇总记录：与具体账号无关，界面按「全部账号 · 时间」展示。
		return "", "全部账号"
	}
	if id, ok := r.identities[uid]; ok {
		name := strings.TrimSpace(id.Name)
		if name == "" {
			name = uid
		}
		return id.ID, name
	}
	// 映射缺失（宿主还没同步到该账号）：退回 uid —— 至少记录不会丢，
	// 宁可「按 id 查不到」也不要「整条记录消失」。
	return uid, uid
}

// load 读取现有记录，**按原始字节保留**未知字段与既有格式。
//
// 为什么用 []json.RawMessage 而不是 []map[string]any：后者会把整份文件重新编码，
// 任何一处编码差异（大整数变科学计数法、字段顺序变化、宿主新增字段被丢掉）
// 都会落到**每一历史条**上。原始字节透传则保证宿主的记录一个比特都不变。
func (r *Recorder) load() ([]json.RawMessage, bool) {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		// 文件不存在是首次写入的正常情况。
		return nil, true
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, true
	}
	var all []json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, false
	}
	return all, true
}

// normalize 裁剪：先按保留天数去掉过期记录，再按上限截断最旧的。
//
// 与宿主侧 normalize_records 同一套口径（先时间、后条数），
// 保证两个写入方对同一个文件的理解一致。
func normalize(all []json.RawMessage, atMs int64, keepDays int) []json.RawMessage {
	days := int64(keepDays)
	if days <= 0 {
		days = DefaultRetentionDays
	}
	cutoff := atMs - days*24*3600*1000

	kept := make([]json.RawMessage, 0, len(all))
	for _, item := range all {
		if tsOf(item) >= cutoff {
			kept = append(kept, item)
		}
	}
	if len(kept) > MaxRecords {
		kept = kept[len(kept)-MaxRecords:]
	}
	return kept
}

// tsOf 取一条记录的 ts（毫秒）；缺失或类型不对按 0 处理。
func tsOf(item json.RawMessage) int64 {
	var probe struct {
		Ts int64 `json:"ts"`
	}
	if err := json.Unmarshal(item, &probe); err != nil {
		return 0
	}
	return probe.Ts
}

// alreadyToday 该账号今天（CST）是否已有同标题同结果的记录。
//
// 用 CST 而非本地时区：与 nightowl 的时间窗判定同一口径，
// 换台机器/改系统时区不该让「今天」的定义漂移。
func alreadyToday(all []json.RawMessage, want record) bool {
	day := cstDay(want.Ts)
	for _, item := range all {
		var probe record
		if err := json.Unmarshal(item, &probe); err != nil {
			continue
		}
		if probe.AccountID == want.AccountID &&
			probe.Title == want.Title &&
			probe.Result == want.Result &&
			cstDay(probe.Ts) == day {
			return true
		}
	}
	return false
}

// cstDay 把一个毫秒时间戳映射成 CST 日序号（同一天返回同一个值）。
func cstDay(ms int64) int64 {
	return (ms + 8*3600*1000) / (24 * 3600 * 1000)
}

// marshalRecord 序列化单条记录（紧凑形式，不含缩进）。
//
// 为什么不用 json.Marshal：它默认把 < > & 转义成 \u003c 之类，而宿主用的
// serde_json 不转义。上游错误体常带 HTML（网关 401 时返回 openresty 错误页），
// 两个写入方交替改写同一个文件会让同一段文本时而 \u003c 时而 <，
// diff 变噪声、人工核对记录时极难读。
//
// 缩进不在这里做：全部交给 mustJSONArray 统一处理，
// 保证「读回来的旧记录」与「刚写的新记录」走同一条格式化路径（幂等）。
//
// 单条记录只含基础类型，编码不该失败；真失败时返回一个空对象兜底 ——
// 宁可丢一条记录，也不能让整个数组写坏（写坏会让宿主解析失败并丢掉全部历史）。
func marshalRecord(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return json.RawMessage("{}")
	}
	// Encode 会补一个结尾换行；数组拼接时由调用方控制换行，故去掉。
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
}

// mustJSONArray 把记录数组拼成与 serde_json::to_string_pretty 同形状的文本。
//
// 关键：每个元素都先经 json.Indent **重新**格式化，而不是原样拼接读进来的字节。
// 为什么必须重排（实测踩过）：从文件读回的 RawMessage 已带上一轮写入的缩进，
// 原样拼接再整体缩进一次，缩进就会每次写入累计增长（2 → 4 → 6 空格），
// 几轮之后文件形状彻底走样且无法与宿主输出对齐。
//
// 用 json.Indent 而非「解码成 any 再编码」：它是词法层面的重排，
// 不把数字转成 float64（ts 是 13 位毫秒，大数走 float64 会丢精度）。
func mustJSONArray(items []json.RawMessage) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	var buf bytes.Buffer
	buf.WriteString("[\n")
	for _, item := range items {
		var indented bytes.Buffer
		if err := json.Indent(&indented, item, "", "  "); err != nil {
			// 单条损坏不应让整份文件写不出来：跳过它，保留其余记录。
			// （宿主侧 load_raw 对整文件损坏是「退化成空集」，比这激进得多。）
			continue
		}
		buf.WriteString("  ")
		buf.WriteString(strings.ReplaceAll(indented.String(), "\n", "\n  "))
		buf.WriteString(",\n")
	}
	// 去掉最后一条多加的逗号，换成换行收尾
	out := buf.Bytes()
	out = bytes.TrimSuffix(out, []byte(",\n"))
	out = append(out, '\n', ']')
	return out
}

// atomicWrite 原子写文件（临时文件 + rename）。
//
// 为什么必须原子：宿主读这个文件时是先 read_to_string 再整体 JSON 解析，
// 读到半截文件会解析失败并退化成「空记录集」，随后的写入就把历史全部覆盖。
// 非原子写在这里不是「偶尔少一条」，而是「偶尔全丢」。
func atomicWrite(path string, content []byte) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return err
	}
	// Windows 上 rename 可能因目标文件正被宿主短暂打开而失败（杀软/索引器也会）。
	// 短暂重试即可；最终失败也只记日志 —— 旁路数据不值得让主流程失败。
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if err = os.Rename(tmp, path); err == nil {
			return nil
		}
		time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
	}
	_ = os.Remove(tmp)
	return err
}

// oneLine 把多行错误压成单行并限长。
//
// 界面把 detail 拼在记录行末尾（单行、truncate），多行文本在那里会显示成空白；
// 限长则避免上游返回的一大段响应体把记录文件撑大。
func oneLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) > max {
		return string(runes[:max]) + "…"
	}
	return s
}
