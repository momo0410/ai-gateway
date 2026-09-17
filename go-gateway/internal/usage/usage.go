// Package usage 网关侧 Token 用量统计。
//
// 数据来源：每次成功请求结束后，从上游响应（非流式 usage 对象 / 流式 SSE 末帧
// usage）提取 prompt/completion/cache 计量，按「服务器本地日期」聚合。
//
// 四个维度全部按天存储，导出时可按天数范围过滤：
//   - days：全体请求（summary/daily 由它派生）
//   - models：模型 × 日期
//   - accounts：账号 × 日期
//   - accountModels：账号 × 模型 × 日期
//
// accountModels 是唯一**不能**由其它维度推导出来的：models 与 accounts 各自
// 聚合后，交叉关系已经丢失（只知道「甲账号共 3 万」「glm-5.2 共 4 万」，
// 无法还原「甲账号的 glm-5.2 用了多少」）。界面「按账号筛选看用了哪些模型」
// 必须读它，因此单独累计一份，而不是让前端拿两个维度硬凑。
//
// 持久化：与账号池 state.json 同目录的 usage.json；Record 只置脏标志，
// 由后台 flusher 周期落盘，进程退出时 main 调用 Flush 兜底。
// 落盘与导出只含聚合数字、uid 与模型名，不含消息正文或认证信息。
package usage

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// dayLayout 聚合用的日期键格式（本地时区），固定宽度保证字符串可直接比较。
const dayLayout = "2006-01-02"

// flushInterval 后台落盘周期。
var flushInterval = 5 * time.Second

// Counters 一组请求的 Token 计量。Input 已包含 CacheRead（与上游 prompt_tokens
// 含 cached tokens 的口径一致），CacheWrite 为单独新增的缓存写入量。
type Counters struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Records    int64 `json:"records"`
}

// Add 累加另一组计量。
func (c Counters) Add(o Counters) Counters {
	return Counters{
		Input:      c.Input + o.Input,
		Output:     c.Output + o.Output,
		CacheRead:  c.CacheRead + o.CacheRead,
		CacheWrite: c.CacheWrite + o.CacheWrite,
		Records:    c.Records + o.Records,
	}
}

// Empty 报告是否没有任何计量（用于跳过记录与聚合）。
func (c Counters) Empty() bool {
	return c.Input == 0 && c.Output == 0 && c.CacheRead == 0 && c.CacheWrite == 0 && c.Records == 0
}

// Value 把计量导出成与宿主 Token 统计页一致的字段口径：
// total = input + output + cacheWrite（不含 cacheRead，避免与 input 重复计数）。
func (c Counters) Value() map[string]any {
	uncached := c.Input - c.CacheRead
	if uncached < 0 {
		uncached = 0
	}
	var hitRate any
	if c.Input > 0 {
		hitRate = float64(c.CacheRead) / float64(c.Input)
	}
	return map[string]any{
		"total":         c.Input + c.Output + c.CacheWrite,
		"input":         c.Input,
		"output":        c.Output,
		"cacheRead":     c.CacheRead,
		"cacheWrite":    c.CacheWrite,
		"uncachedInput": uncached,
		"records":       c.Records,
		"cacheHitRate":  hitRate,
	}
}

// fileState usage.json 的磁盘结构。
//
// AccountModels 用 omitempty：老版本网关写下的文件没有这个键，读回来是 nil，
// 由 load 兜底成空 map —— 不因为「多了个字段」就让既有历史统计失效。
type fileState struct {
	Version       int                                       `json:"version"`
	SavedAt       time.Time                                 `json:"savedAt"`
	Days          map[string]Counters                       `json:"days"`
	Models        map[string]map[string]Counters            `json:"models,omitempty"`
	Accounts      map[string]map[string]Counters            `json:"accounts,omitempty"`
	AccountModels map[string]map[string]map[string]Counters `json:"accountModels,omitempty"`
}

// Stats 网关 Token 用量聚合器。并发安全；path 为空时纯内存（不落盘）。
type Stats struct {
	mu       sync.Mutex
	dirty    atomic.Bool
	path     string
	days     map[string]Counters
	models   map[string]map[string]Counters
	accounts map[string]map[string]Counters
	// accountModels：uid -> model -> 日期 -> 计量。
	accountModels map[string]map[string]map[string]Counters

	// now 供测试注入时钟；nil 时用 time.Now。
	now func() time.Time
}

// New 构建统计器；path 非空时加载旧数据并启动后台落盘。
func New(path string) *Stats {
	s := &Stats{
		path:          path,
		days:          map[string]Counters{},
		models:        map[string]map[string]Counters{},
		accounts:      map[string]map[string]Counters{},
		accountModels: map[string]map[string]map[string]Counters{},
	}
	if path != "" {
		s.load()
		go s.flusher()
	}
	return s
}

func (s *Stats) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Record 记录一次成功请求的用量；uid/model 为空时归一成 "-"。
func (s *Stats) Record(uid, model string, c Counters) {
	s.RecordAt(uid, model, s.clock(), c)
}

// RecordAt 同 Record，但显式指定时刻（测试用）。
func (s *Stats) RecordAt(uid, model string, at time.Time, c Counters) {
	if c.Input == 0 && c.Output == 0 && c.CacheRead == 0 && c.CacheWrite == 0 {
		// 上游未返回可用 usage：不计数，也不产生空记录。
		return
	}
	day := at.In(time.Local).Format(dayLayout)
	entry := Counters{
		Input:      c.Input,
		Output:     c.Output,
		CacheRead:  c.CacheRead,
		CacheWrite: c.CacheWrite,
		Records:    1,
	}
	if model == "" {
		model = "-"
	}
	if uid == "" {
		uid = "-"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	addTo(s.days, day, entry)
	addTo(nested(s.models, model), day, entry)
	addTo(nested(s.accounts, uid), day, entry)
	addTo(nested2(s.accountModels, uid, model), day, entry)
	s.dirty.Store(true)
}

func nested(m map[string]map[string]Counters, key string) map[string]Counters {
	inner, ok := m[key]
	if !ok {
		inner = map[string]Counters{}
		m[key] = inner
	}
	return inner
}

// nested2 同 nested，但多一层键（账号 -> 模型 -> 日期）。
//
// 单独写一个而不是把 nested 泛型化：Go 的 map 嵌套每加一层类型就变一次，
// 泛型版本为了省这几行会引入类型参数与约束，读起来比多一个 10 行函数更绕。
func nested2(m map[string]map[string]map[string]Counters, key, sub string) map[string]Counters {
	byModel, ok := m[key]
	if !ok {
		byModel = map[string]map[string]Counters{}
		m[key] = byModel
	}
	return nested(byModel, sub)
}

func addTo(m map[string]Counters, key string, c Counters) {
	m[key] = m[key].Add(c)
}

// Snapshot 导出一份可 JSON 序列化的聚合快照。days<=0 表示全部历史。
func (s *Stats) Snapshot(days int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock()
	cutoff := ""
	if days > 0 {
		cutoff = now.AddDate(0, 0, -(days - 1)).Format(dayLayout)
	}

	summary := sumDays(s.days, cutoff)
	models := groupList(s.models, cutoff)
	accounts := groupList(s.accounts, cutoff)
	daily := daySeries(s.days, cutoff)

	dailyByModel := map[string]any{}
	for model, series := range s.models {
		dailyByModel[model] = daySeries(series, cutoff)
	}

	// accountModels：账号 -> 模型分组列表。
	//
	// 只保留**该范围内仍有计量**的账号与模型（groupList 会丢掉全空的），
	// 因此「全部为 0 的账号」不会以空数组形式出现在响应里 —— 界面据此
	// 区分「这个号这段时间没消耗」与「这个号根本不在统计里」。
	accountModels := map[string]any{}
	for uid, byModel := range s.accountModels {
		groups := groupList(byModel, cutoff)
		if len(groups) == 0 {
			continue
		}
		accountModels[uid] = groups
	}

	var rangeDays any
	if days > 0 {
		rangeDays = days
	}
	return map[string]any{
		"generatedAt":   now.UnixMilli(),
		"rangeDays":     rangeDays,
		"summary":       summary.Value(),
		"models":        models,
		"accounts":      accounts,
		"accountModels": accountModels,
		"daily":         daily,
		"dailyByModel":  dailyByModel,
	}
}

func sumDays(m map[string]Counters, cutoff string) Counters {
	var total Counters
	for day, c := range m {
		if cutoff != "" && day < cutoff {
			continue
		}
		total = total.Add(c)
	}
	return total
}

// groupList 把「键 -> 日期 -> 计量」两层结构压平成按 total 降序的列表。
func groupList(m map[string]map[string]Counters, cutoff string) []map[string]any {
	out := make([]map[string]any, 0, len(m))
	for key, series := range m {
		c := sumDays(series, cutoff)
		if c.Empty() {
			continue
		}
		value := c.Value()
		value["key"] = key
		out = append(out, value)
	}
	sortByTotalDesc(out)
	return out
}

// daySeries 把日聚合导出成按日期升序的序列（供前端趋势图使用）。
func daySeries(m map[string]Counters, cutoff string) []map[string]any {
	keys := make([]string, 0, len(m))
	for day := range m {
		if cutoff != "" && day < cutoff {
			continue
		}
		keys = append(keys, day)
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(keys))
	for _, day := range keys {
		value := m[day].Value()
		value["key"] = day
		out = append(out, value)
	}
	return out
}

func sortByTotalDesc(values []map[string]any) {
	sort.SliceStable(values, func(i, j int) bool {
		return intOf(values[i]["total"]) > intOf(values[j]["total"])
	})
}

// Flush 同步把内存状态落盘（幂等：无变更或纯内存模式直接返回）。
func (s *Stats) Flush() {
	if s.path == "" || !s.dirty.Load() {
		return
	}
	s.mu.Lock()
	raw, err := json.MarshalIndent(s.fileStateLocked(), "", "  ")
	s.dirty.Store(false)
	s.mu.Unlock()
	if err != nil {
		log.Printf("usage: 序列化失败: %v", err)
		return
	}

	if dir := filepath.Dir(s.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("usage: 落盘失败: %v", err)
		s.dirty.Store(true)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("usage: 落盘失败: %v", err)
		s.dirty.Store(true)
	}
}

func (s *Stats) flusher() {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for range t.C {
		s.Flush()
	}
}

// fileStateLocked 收集内存状态为磁盘结构。调用方必须已持有 s.mu。
func (s *Stats) fileStateLocked() fileState {
	return fileState{
		Version:       1,
		SavedAt:       s.clock(),
		Days:          s.days,
		Models:        s.models,
		Accounts:      s.accounts,
		AccountModels: s.accountModels,
	}
}

func (s *Stats) load() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var fs fileState
	if err := json.Unmarshal(raw, &fs); err != nil {
		log.Printf("usage: 解析 %s 失败，忽略: %v", s.path, err)
		return
	}
	if fs.Version > 1 {
		log.Printf("usage: %s 版本 %d 高于当前支持，忽略", s.path, fs.Version)
		return
	}
	if fs.Days != nil {
		s.days = fs.Days
	}
	if fs.Models != nil {
		s.models = fs.Models
	}
	if fs.Accounts != nil {
		s.accounts = fs.Accounts
	}
	// 老版本文件没有 accountModels（字段缺失 = nil）：保持 New 里的空 map，
	// 让后续 Record 直接写入而不是往 nil map 里塞（那会 panic）。
	//
	// 这里**不**拿 models/accounts 去反推一份：反推出来的是「每个账号都用了
	// 全部模型」这种假交叉，界面会显示成一堆错误的模型明细，比缺失更糟。
	if fs.AccountModels != nil {
		s.accountModels = fs.AccountModels
	}
}

// ParseOpenAIUsage 从上游 usage 对象提取计量。
//
// 返回 ok=false 表示该对象没有任何可识别的 token 字段（例如上游漏发 usage），
// 调用方应跳过本次统计。字段兼容 OpenAI 标准命名与 CodeBuddy 实际会返回的别名：
//   - input:  prompt_tokens / input_tokens
//   - output: completion_tokens / output_tokens
//   - cacheRead:  prompt_cache_hit_tokens > prompt_tokens_details.cached_tokens
//     > input_tokens_details[].cached_tokens
//   - cacheWrite: prompt_cache_write_tokens / cache_write_input_tokens /
//     cache_creation_input_tokens
func ParseOpenAIUsage(u map[string]any) (Counters, bool) {
	if u == nil {
		return Counters{}, false
	}
	input, hasInput := firstNumber(u, "prompt_tokens", "input_tokens")
	output, hasOutput := firstNumber(u, "completion_tokens", "output_tokens", "output_text_tokens")
	if !hasInput && !hasOutput {
		// 兜底：只认明确出现的 token 字段，避免把无关对象记成 0 记录。
		if !hasAnyKey(u, "prompt_tokens", "input_tokens", "completion_tokens", "output_tokens") {
			return Counters{}, false
		}
	}

	read, _ := firstNumber(u, "prompt_cache_hit_tokens", "cache_read_input_tokens")
	if read == 0 {
		if details, ok := u["prompt_tokens_details"].(map[string]any); ok {
			read, _ = firstNumber(details, "cached_tokens")
		}
	}
	if read == 0 {
		if details, ok := u["input_tokens_details"].([]any); ok {
			for _, item := range details {
				if m, ok := item.(map[string]any); ok {
					if n, ok := firstNumber(m, "cached_tokens"); ok && n > 0 {
						read = n
						break
					}
				}
			}
		}
	}
	write, _ := firstNumber(u, "prompt_cache_write_tokens", "cache_write_input_tokens", "cache_creation_input_tokens")

	return Counters{Input: input, Output: output, CacheRead: read, CacheWrite: write}, true
}

func firstNumber(m map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			return numberValue(v), true
		}
	}
	return 0, false
}

func hasAnyKey(m map[string]any, keys ...string) bool {
	for _, key := range keys {
		if _, ok := m[key]; ok {
			return true
		}
	}
	return false
}

func numberValue(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case float32:
		return int64(n)
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case json.Number:
		if parsed, err := n.Int64(); err == nil {
			return parsed
		}
		if parsed, err := n.Float64(); err == nil {
			return int64(parsed)
		}
	case string:
		trimmed := strings.TrimSpace(n)
		if parsed, err := json.Number(trimmed).Int64(); err == nil {
			return parsed
		}
	}
	return 0
}

func intOf(v any) int64 {
	return numberValue(v)
}
