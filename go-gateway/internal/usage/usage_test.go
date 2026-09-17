package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseOpenAIUsageStandard(t *testing.T) {
	c, ok := ParseOpenAIUsage(map[string]any{
		"prompt_tokens":     float64(100),
		"completion_tokens": float64(20),
		"total_tokens":      float64(120),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": float64(40),
		},
	})
	if !ok {
		t.Fatal("标准 usage 应被识别")
	}
	if c.Input != 100 || c.Output != 20 || c.CacheRead != 40 || c.CacheWrite != 0 {
		t.Fatalf("解析结果不符: %+v", c)
	}
}

func TestParseOpenAIUsageAliases(t *testing.T) {
	c, ok := ParseOpenAIUsage(map[string]any{
		"input_tokens":              float64(50),
		"output_tokens":             float64(7),
		"prompt_cache_hit_tokens":   float64(12),
		"prompt_cache_write_tokens": float64(3),
	})
	if !ok {
		t.Fatal("别名 usage 应被识别")
	}
	if c.Input != 50 || c.Output != 7 || c.CacheRead != 12 || c.CacheWrite != 3 {
		t.Fatalf("别名解析结果不符: %+v", c)
	}

	// 嵌套 input_tokens_details 数组中的 cached_tokens。
	c2, ok := ParseOpenAIUsage(map[string]any{
		"input_tokens":  float64(30),
		"output_tokens": float64(5),
		"input_tokens_details": []any{
			map[string]any{"cached_tokens": float64(9)},
		},
	})
	if !ok || c2.CacheRead != 9 {
		t.Fatalf("嵌套 details 解析不符: %+v ok=%v", c2, ok)
	}
}

func TestParseOpenAIUsageMissing(t *testing.T) {
	if _, ok := ParseOpenAIUsage(nil); ok {
		t.Fatal("nil 不应被识别")
	}
	if _, ok := ParseOpenAIUsage(map[string]any{"foo": "bar"}); ok {
		t.Fatal("无关对象不应被识别")
	}
	// 明确的零值字段仍算有效 usage（上游可能返回全 0）。
	if _, ok := ParseOpenAIUsage(map[string]any{"prompt_tokens": float64(0), "completion_tokens": float64(0)}); !ok {
		t.Fatal("含 token 字段的零值 usage 应被识别")
	}
}

func TestRecordAggregatesAndFilters(t *testing.T) {
	s := New("")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.Local)
	// 注入时钟：让「近 7 天」恰好吃到 9/1 与 9/6 两笔，排除 10/11 的历史记录。
	s.now = func() time.Time { return base.AddDate(0, 0, 5) }

	s.RecordAt("uid-a", "model-x", base, Counters{Input: 10, Output: 2, CacheRead: 4})
	s.RecordAt("uid-a", "model-x", base, Counters{Input: 5, Output: 1})
	s.RecordAt("uid-b", "model-y", base.AddDate(0, 0, 5), Counters{Input: 100, Output: 20, CacheWrite: 7})
	s.RecordAt("uid-b", "model-y", base.AddDate(0, 0, -40), Counters{Input: 1000, Output: 100})

	all := s.Snapshot(0)
	summary := all["summary"].(map[string]any)
	if got := intOf(summary["input"]); got != 1115 {
		t.Fatalf("全部 input 应为 1115，实际 %d", got)
	}
	if got := intOf(summary["records"]); got != 4 {
		t.Fatalf("全部 records 应为 4，实际 %d", got)
	}
	if got := intOf(summary["total"]); got != 1115+123+7 {
		t.Fatalf("total 口径不符: %d", got)
	}

	// 近 7 天（含今天）：只含 9/1 与 9/6 三笔。
	week := s.Snapshot(7)
	weekSummary := week["summary"].(map[string]any)
	if got := intOf(weekSummary["input"]); got != 115 {
		t.Fatalf("近 7 天 input 应为 115，实际 %d", got)
	}
	weekModels := week["models"].([]map[string]any)
	if len(weekModels) != 2 {
		t.Fatalf("近 7 天应有 2 个模型，实际 %d", len(weekModels))
	}
	if weekModels[0]["key"] != "model-y" {
		t.Fatalf("模型应按 total 降序，实际首位 %v", weekModels[0]["key"])
	}

	// 日序列按日期升序，且范围外记录被过滤。
	daily := week["daily"].([]map[string]any)
	if len(daily) != 2 || daily[0]["key"] != "2026-09-01" || daily[1]["key"] != "2026-09-06" {
		t.Fatalf("daily 序列不符: %v", daily)
	}

	accounts := all["accounts"].([]map[string]any)
	if len(accounts) != 2 {
		t.Fatalf("应有 2 个账号，实际 %d", len(accounts))
	}
	if accounts[0]["key"] != "uid-b" {
		t.Fatalf("账号应按 total 降序，实际首位 %v", accounts[0]["key"])
	}
}

func TestSnapshotUsesInjectedClock(t *testing.T) {
	s := New("")
	fixed := time.Date(2026, 9, 10, 8, 0, 0, 0, time.Local)
	s.now = func() time.Time { return fixed }
	s.RecordAt("u", "m", fixed, Counters{Input: 1, Output: 1})
	s.RecordAt("u", "m", fixed.AddDate(0, 0, -10), Counters{Input: 9})

	week := s.Snapshot(7)
	if got := intOf(week["summary"].(map[string]any)["input"]); got != 1 {
		t.Fatalf("时钟注入下近 7 天 input 应为 1，实际 %d", got)
	}
	if week["rangeDays"] != 7 {
		t.Fatalf("rangeDays 应为 7，实际 %v", week["rangeDays"])
	}
	if all := s.Snapshot(0); all["rangeDays"] != nil {
		t.Fatalf("全部范围 rangeDays 应为 null，实际 %v", all["rangeDays"])
	}
}

func TestFlushAndReloadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")

	s := New(path)
	at := time.Date(2026, 9, 2, 15, 4, 5, 0, time.Local)
	s.RecordAt("uid-a", "model-x", at, Counters{Input: 10, Output: 3, CacheRead: 5, CacheWrite: 1})
	s.Flush()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("usage.json 应已落盘: %v", err)
	}
	// 原子写不应残留临时文件。
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("临时文件应已被 rename")
	}

	s2 := New(path)
	all := s2.Snapshot(0)
	summary := all["summary"].(map[string]any)
	if intOf(summary["input"]) != 10 || intOf(summary["output"]) != 3 ||
		intOf(summary["cacheRead"]) != 5 || intOf(summary["cacheWrite"]) != 1 ||
		intOf(summary["records"]) != 1 {
		t.Fatalf("重启后数据不一致: %v", summary)
	}
}

func TestFlushSkipsWhenNoPathOrNoChanges(t *testing.T) {
	// 纯内存模式：Flush 不产生文件。
	s := New("")
	s.RecordAt("u", "m", time.Now(), Counters{Input: 1})
	s.Flush()

	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	s2 := New(path)
	s2.Flush() // 无变更：不应创建文件
	if _, err := os.Stat(path); err == nil {
		t.Fatal("无变更时不应落盘")
	}
}

func TestLoadIgnoresCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(path)
	s.RecordAt("u", "m", time.Now(), Counters{Input: 2})
	if got := intOf(s.Snapshot(0)["summary"].(map[string]any)["input"]); got != 2 {
		t.Fatalf("损坏文件应被忽略并从空状态开始，实际 %d", got)
	}
}

func TestCountersValueCacheHitRate(t *testing.T) {
	empty := Counters{}
	if empty.Value()["cacheHitRate"] != nil {
		t.Fatal("无输入时 cacheHitRate 应为 nil")
	}
	// cacheRead 大于 input（异常数据）时 uncachedInput 不应为负。
	odd := Counters{Input: 5, Output: 1, CacheRead: 8, Records: 1}
	if got := intOf(odd.Value()["uncachedInput"]); got != 0 {
		t.Fatalf("uncachedInput 不应为负，实际 %d", got)
	}

	// JSON 序列化：nil 命中率输出为 null。
	raw, err := json.Marshal(empty.Value())
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["cacheHitRate"] != nil {
		t.Fatalf("cacheHitRate 应序列化为 null，实际 %v", decoded["cacheHitRate"])
	}
}

// accountModelsOf 取快照里某个账号的模型分组（键不存在时返回 nil）。
func accountModelsOf(t *testing.T, snapshot map[string]any, uid string) []map[string]any {
	t.Helper()
	all, ok := snapshot["accountModels"].(map[string]any)
	if !ok {
		t.Fatalf("快照应含 accountModels 映射，实际 %T", snapshot["accountModels"])
	}
	groups, ok := all[uid].([]map[string]any)
	if !ok {
		return nil
	}
	return groups
}

// 交叉聚合的核心：同一个账号用了两个模型，必须能分别看到各自的用量，
// 而不是只看到账号合计（那正是「按账号筛选看模型明细」做不到的原因）。
func TestSnapshotSplitsModelsPerAccount(t *testing.T) {
	s := New("")
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.Local)
	s.now = func() time.Time { return at }

	// 账号甲：glm-5.2 用 100+20，hy3 用 30；账号乙只用 hy3。
	s.RecordAt("uid-a", "glm-5.2", at, Counters{Input: 60, Output: 10})
	s.RecordAt("uid-a", "glm-5.2", at, Counters{Input: 40, Output: 10})
	s.RecordAt("uid-a", "hy3", at, Counters{Input: 30})
	s.RecordAt("uid-b", "hy3", at, Counters{Input: 7})

	snap := s.Snapshot(0)

	a := accountModelsOf(t, snap, "uid-a")
	if len(a) != 2 {
		t.Fatalf("uid-a 应有 2 个模型分组，实际 %d: %v", len(a), a)
	}
	// 组内按 total 降序：glm-5.2(120) 在 hy3(30) 之前。
	if a[0]["key"] != "glm-5.2" || a[1]["key"] != "hy3" {
		t.Fatalf("uid-a 模型应按用量降序，实际 %v %v", a[0]["key"], a[1]["key"])
	}
	if got := intOf(a[0]["total"]); got != 120 {
		t.Fatalf("uid-a 的 glm-5.2 total 应为 120，实际 %d", got)
	}
	if got := intOf(a[0]["records"]); got != 2 {
		t.Fatalf("uid-a 的 glm-5.2 应有 2 次调用，实际 %d", got)
	}
	if got := intOf(a[1]["total"]); got != 30 {
		t.Fatalf("uid-a 的 hy3 total 应为 30，实际 %d", got)
	}

	// uid-b 只有 hy3，且用量是它自己的 7 —— 不能串成账号甲的 30。
	b := accountModelsOf(t, snap, "uid-b")
	if len(b) != 1 || b[0]["key"] != "hy3" {
		t.Fatalf("uid-b 应只有 hy3 一个分组，实际 %v", b)
	}
	if got := intOf(b[0]["total"]); got != 7 {
		t.Fatalf("uid-b 的 hy3 total 应为 7（不能串成别人的用量），实际 %d", got)
	}

	// 全局 models 仍是两个模型合并后的口径，与交叉聚合互不干扰。
	models := snap["models"].([]map[string]any)
	if len(models) != 2 {
		t.Fatalf("全局模型分组应为 2 个，实际 %d", len(models))
	}
}

// 交叉聚合必须跟着天数范围一起过滤，否则切「今日」会看到历史模型明细。
func TestAccountModelsHonoursDaysFilter(t *testing.T) {
	s := New("")
	today := time.Date(2026, 9, 10, 12, 0, 0, 0, time.Local)
	s.now = func() time.Time { return today }

	s.RecordAt("uid-a", "today-model", today, Counters{Input: 5})
	s.RecordAt("uid-a", "old-model", today.AddDate(0, 0, -40), Counters{Input: 900})

	week := s.Snapshot(7)
	a := accountModelsOf(t, week, "uid-a")
	if len(a) != 1 || a[0]["key"] != "today-model" {
		t.Fatalf("近 7 天只应剩 today-model，实际 %v", a)
	}

	// 范围外的账号完全不出现在映射里（而不是出现一个空数组）：
	// 界面据此区分「这段时间没消耗」与「数据缺失」。
	s.RecordAt("uid-old", "old-model", today.AddDate(0, 0, -40), Counters{Input: 3})
	week2 := s.Snapshot(7)
	if groups := accountModelsOf(t, week2, "uid-old"); groups != nil {
		t.Fatalf("范围外账号不应出现，实际 %v", groups)
	}
	if _, ok := week2["accountModels"].(map[string]any)["uid-old"]; ok {
		t.Fatal("范围外账号不应以空数组形式出现在 accountModels 里")
	}
}

// 落盘 -> 重载必须保住交叉聚合（否则网关重启一次，模型明细就全没了）。
func TestAccountModelsSurviveFlushAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	at := time.Date(2026, 9, 2, 15, 4, 5, 0, time.Local)

	s := New(path)
	s.RecordAt("uid-a", "model-x", at, Counters{Input: 10, Output: 3})
	s.RecordAt("uid-a", "model-y", at, Counters{Input: 4})
	s.Flush()

	s2 := New(path)
	a := accountModelsOf(t, s2.Snapshot(0), "uid-a")
	if len(a) != 2 {
		t.Fatalf("重载后应有 2 个模型分组，实际 %v", a)
	}
	if a[0]["key"] != "model-x" || intOf(a[0]["total"]) != 13 {
		t.Fatalf("重载后 model-x 分组不符: %v", a[0])
	}
}

// 老版本（无 accountModels 字段）的 usage.json 必须仍能读，
// 且读完后继续 Record 不能 panic（往 nil map 写会崩）。
func TestLoadToleratesLegacyFileWithoutAccountModels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	legacy := `{"version":1,"savedAt":"2026-09-02T15:04:05+08:00",` +
		`"days":{"2026-09-02":{"input":10,"output":3,"records":1}},` +
		`"models":{"model-x":{"2026-09-02":{"input":10,"output":3,"records":1}}},` +
		`"accounts":{"uid-a":{"2026-09-02":{"input":10,"output":3,"records":1}}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	s := New(path)
	if got := intOf(s.Snapshot(0)["summary"].(map[string]any)["input"]); got != 10 {
		t.Fatalf("老文件的历史统计不应丢失，实际 input=%d", got)
	}
	// 关键：老文件没有交叉数据时，不能拿 models/accounts 反推出假明细。
	if groups := accountModelsOf(t, s.Snapshot(0), "uid-a"); groups != nil {
		t.Fatalf("老文件不应凭空产生模型明细，实际 %v", groups)
	}

	// 继续记录新数据：既不能 panic，也要从这一刻起建立交叉聚合。
	s.RecordAt("uid-a", "model-new", time.Now(), Counters{Input: 2})
	a := accountModelsOf(t, s.Snapshot(0), "uid-a")
	if len(a) != 1 || a[0]["key"] != "model-new" {
		t.Fatalf("新记录应建立交叉聚合，实际 %v", a)
	}
}
