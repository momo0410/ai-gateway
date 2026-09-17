package records

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 账号记录写入回归
//
// 本包存在的理由：4 个 Go 任务的日志被宿主丢弃（stdout → null），
// 用户界面上看不到任何执行痕迹。修法是把结果写进宿主已经在读的
// account_records.json，因此**字段名与文件形状必须逐字对齐**，
// 否则界面读不出来（且不会报错，只是「没有记录」）。
// ---------------------------------------------------------------------------

func newRecorder(t *testing.T, retentionDays int, identities map[string]Identity) (*Recorder, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "account_records.json")
	return New(path, retentionDays, identities), path
}

// readAll 读回记录文件并断言它是合法 JSON 数组。
func readAll(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取记录文件失败: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("记录文件不是合法 JSON 数组（界面会读不出来）: %v\n内容: %s", err, raw)
	}
	return out
}

// TestTaskWritesHostCompatibleShape 字段名必须与宿主侧 AccountRecord::to_json 逐字一致。
//
// 这是最关键的一条：宿主 query_records 按 "accountId" / "kind" / "ts" 取值，
// 任一处拼错都不会报错 —— 记录会安静地不出现在界面上，正是本 bug 的原症状。
func TestTaskWritesHostCompatibleShape(t *testing.T) {
	r, path := newRecorder(t, 60, map[string]Identity{
		"uid-1": {ID: "acct-uuid-1", Name: "用户甲"},
	})
	r.Task("uid-1", "活跃上报", ResultSuccess, "已上报 3/3 条")

	all := readAll(t, path)
	if len(all) != 1 {
		t.Fatalf("应写入 1 条记录，实际 %d 条", len(all))
	}
	got := all[0]

	// 逐字对齐宿主侧字段名（含大小写）
	want := map[string]any{
		"accountId":   "acct-uuid-1",
		"accountName": "用户甲",
		"kind":        "task",
		"title":       "活跃上报",
		"result":      "success",
		"amount":      float64(0),
		"detail":      "已上报 3/3 条",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("字段 %q = %v，期望 %v", k, got[k], v)
		}
	}
	// ts 必须是毫秒级（宿主按毫秒与界面日期区间比较）
	ts, ok := got["ts"].(float64)
	if !ok {
		t.Fatalf("ts 缺失或不是数字: %v", got["ts"])
	}
	if ts < 1e12 {
		t.Errorf("ts=%v 不是毫秒时间戳（界面按毫秒过滤，秒级会被判成 1970 年）", ts)
	}
	// 不多写字段：界面按 kind 分组，多余的顶层字段只会让两边结构漂移
	if len(got) != 8 {
		t.Errorf("字段数应为 8（与宿主 AccountRecord 一致），实际 %d: %v", len(got), got)
	}
}

// TestIdentityMappingUsesHostIDs 记录里的 accountId 必须是宿主的 uuid，而不是网关的 uid。
//
// 为什么：界面「账号卡片 → 查看记录」按 accounts.json 的 id 过滤。
// 网关写 uid 的话，用户点开账号永远查不到这些任务，且不会有任何报错。
func TestIdentityMappingUsesHostIDs(t *testing.T) {
	r, path := newRecorder(t, 60, map[string]Identity{
		"gw-uid-9": {ID: "host-id-9", Name: "用户乙"},
	})
	r.Task("gw-uid-9", "夜猫子任务", ResultSuccess, "")

	got := readAll(t, path)[0]
	if got["accountId"] != "host-id-9" {
		t.Errorf("accountId 应为宿主 id host-id-9，实际 %v", got["accountId"])
	}
	if got["accountName"] != "用户乙" {
		t.Errorf("accountName 应为用户乙，实际 %v", got["accountName"])
	}
}

// TestUnknownUIDFallsBackToUID 宿主尚未同步该账号时退回 uid，记录不丢。
//
// 宁可「按 id 查不到」也不要「整条记录消失」：前者用户切到「全部账号」还能看到，
// 后者等于这次执行没有任何痕迹。
func TestUnknownUIDFallsBackToUID(t *testing.T) {
	r, path := newRecorder(t, 60, nil)
	r.Task("orphan-uid", "trial 领取", ResultFailed, "boom")

	got := readAll(t, path)[0]
	if got["accountId"] != "orphan-uid" || got["accountName"] != "orphan-uid" {
		t.Errorf("映射缺失时应退回 uid，实际 %v / %v", got["accountId"], got["accountName"])
	}
}

// TestDisabledRecorderIsNoop 未配置路径时静默不写（独立跑 gateway.exe 的场景）。
func TestDisabledRecorderIsNoop(t *testing.T) {
	r := New("", 60, nil)
	if r.Enabled() {
		t.Error("空路径应为未启用")
	}
	// 不应 panic、不应创建任何文件
	r.Task("u", "标题", ResultSuccess, "")
	r.TaskAllDaily("汇总", ResultInfo, "")

	// nil Recorder 同样安全（调度器可能持有 nil）
	var nilRec *Recorder
	if nilRec.Enabled() {
		t.Error("nil Recorder 应为未启用")
	}
	nilRec.Task("u", "标题", ResultSuccess, "")
	nilRec.TaskAllDaily("汇总", ResultInfo, "")
}

// TestWriteFailureDoesNotPanicOrCorrupt 写入失败（路径不可写）只记日志，不 panic。
//
// 旁路观测数据的铁律：写不进去不能让任务本身失败。
func TestWriteFailureDoesNotPanicOrCorrupt(t *testing.T) {
	// 用一个「父路径是文件」的非法路径，保证 MkdirAll/WriteFile 必然失败
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(filepath.Join(blocker, "sub", "account_records.json"), 60, nil)

	// 不 panic 即通过
	r.Task("u", "活跃上报", ResultSuccess, "")
	r.TaskAllDaily("汇总", ResultInfo, "")
}

// TestPreservesHostCreditSourceField 宿主新增的 source 字段必须原样透传。
//
// 背景：宿主（Rust 侧 account_records.rs）给积分记录加了 `source`
//（grant/consume/expire/adjust），用于回答「这笔积分是怎么变的」。
// 网关**只写 task 记录**，但它每次追加都会把整份文件读出来重写 ——
// 若把不认识的字段丢掉，用户看到的积分记录会集体失去来源，
// 而界面上只会表现为「来源徽标不见了」，极难定位到这里。
//
// 这条断言同时锁住两件事：
//  1. source 的值原样保留（不被改写、不被清空）；
//  2. detail 里的中文判据无损（编码路径不能把中文写坏）。
func TestPreservesHostCreditSourceField(t *testing.T) {
	r, path := newRecorder(t, 60, nil)

	// 模拟宿主新格式：带 source 与中文 detail
	host := `[
  {"ts": 1790000000000, "accountId": "a1", "accountName": "甲", "kind": "credit",
   "title": "积分消耗 · 调用扣减", "result": "info", "amount": -12,
   "detail": "余额 -12，额度容量不变（纯消耗）", "source": "consume"},
  {"ts": 1790000000001, "accountId": "a1", "accountName": "甲", "kind": "credit",
   "title": "积分增长 · 额度发放", "result": "success", "amount": 100,
   "detail": "余额 +100，额度容量 +100（新增积分包到账）", "source": "grant"}
]`
	if err := os.WriteFile(path, []byte(host), 0o600); err != nil {
		t.Fatal(err)
	}

	r.now = func() time.Time { return time.UnixMilli(1790000002000) }
	r.Task("u1", "活跃上报", ResultSuccess, "")

	all := readAll(t, path)
	if len(all) != 3 {
		t.Fatalf("应有 3 条（宿主 2 + 网关 1），实际 %d 条", len(all))
	}
	if all[0]["source"] != "consume" {
		t.Errorf("source 被丢弃或改写: %v", all[0]["source"])
	}
	if all[1]["source"] != "grant" {
		t.Errorf("source 被丢弃或改写: %v", all[1]["source"])
	}
	if all[0]["title"] != "积分消耗 · 调用扣减" {
		t.Errorf("标题被改写: %v", all[0]["title"])
	}
	// 中文判据必须无损（编码路径若出错会变成乱码或替换字符）
	if all[0]["detail"] != "余额 -12，额度容量不变（纯消耗）" {
		t.Errorf("中文 detail 被写坏: %v", all[0]["detail"])
	}
	if all[1]["detail"] != "余额 +100，额度容量 +100（新增积分包到账）" {
		t.Errorf("中文 detail 被写坏: %v", all[1]["detail"])
	}
}

// TestPreservesLegacyCreditWithoutSource 老格式（无 source）的记录不该被补字段。
//
// 宿主对历史记录**不会回头补写** source（记录是只追加的事件流）。
// 网关若「顺手」补一个 source: "" 或 source: null，前端就会把老记录
// 渲染成一个空徽标 —— 所以要断言这个键**依然不存在**。
func TestPreservesLegacyCreditWithoutSource(t *testing.T) {
	r, path := newRecorder(t, 60, nil)

	host := `[
  {"ts": 1790000000000, "accountId": "a1", "accountName": "甲", "kind": "credit",
   "title": "积分增长", "result": "success", "amount": 100, "detail": ""}
]`
	if err := os.WriteFile(path, []byte(host), 0o600); err != nil {
		t.Fatal(err)
	}

	r.now = func() time.Time { return time.UnixMilli(1790000001000) }
	r.Task("u1", "活跃上报", ResultSuccess, "")

	all := readAll(t, path)
	if len(all) != 2 {
		t.Fatalf("应有 2 条记录，实际 %d 条", len(all))
	}
	if _, ok := all[0]["source"]; ok {
		t.Errorf("老记录不该被补出 source 字段: %v", all[0]["source"])
	}
}

// TestPreservesExistingRecordsFromHost 追加时不得破坏宿主已写的记录。
//
// 宿主与网关是两个写入方，交替改写同一个文件。网关若把不认识的字段丢掉，
// 或把记录整体重排，用户会看到「积分记录莫名少了一些字段」。
func TestPreservesExistingRecordsFromHost(t *testing.T) {
	r, path := newRecorder(t, 60, nil)

	// 模拟宿主写入：带网关不认识的自定义字段，且 ts 用大整数
	host := `[
  {"ts": 1790000000000, "accountId": "a1", "accountName": "甲", "kind": "credit",
   "title": "积分消耗", "result": "info", "amount": -12, "detail": "",
   "backfilled": true, "hostOnlyField": {"nested": 1}}
]`
	if err := os.WriteFile(path, []byte(host), 0o600); err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return time.UnixMilli(1790000001000) }
	r.Task("u1", "活跃上报", ResultSuccess, "")

	all := readAll(t, path)
	if len(all) != 2 {
		t.Fatalf("应有 2 条记录（宿主 1 + 网关 1），实际 %d 条", len(all))
	}
	// 宿主那条必须原样保留，含它自己的扩展字段
	kept := all[0]
	if kept["kind"] != "credit" || kept["title"] != "积分消耗" {
		t.Errorf("宿主记录被改动: %v", kept)
	}
	if kept["backfilled"] != true {
		t.Error("宿主的 backfilled 字段被丢弃 —— 会让签到回填重复导入")
	}
	if _, ok := kept["hostOnlyField"]; !ok {
		t.Error("宿主记录的未知字段被丢弃（网关应原样透传）")
	}
	if kept["amount"] != float64(-12) {
		t.Errorf("宿主的负数 amount 被改动: %v", kept["amount"])
	}
}

// TestCorruptFileIsRecoveredNotFatal 文件损坏时按空集继续写入，而不是永久停更。
func TestCorruptFileIsRecoveredNotFatal(t *testing.T) {
	r, path := newRecorder(t, 60, nil)
	if err := os.WriteFile(path, []byte("not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	r.Task("u1", "活跃上报", ResultSuccess, "")

	all := readAll(t, path)
	if len(all) != 1 {
		t.Fatalf("损坏文件应被重建为含 1 条记录，实际 %d 条", len(all))
	}
	if all[0]["title"] != "活跃上报" {
		t.Errorf("重建后的记录不对: %v", all[0])
	}
}

// TestEmptyFileIsTreatedAsEmpty 空文件（宿主写坏/占位）同样按空集处理。
func TestEmptyFileIsTreatedAsEmpty(t *testing.T) {
	r, path := newRecorder(t, 60, nil)
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.Task("u1", "活跃上报", ResultSuccess, "")
	if got := len(readAll(t, path)); got != 1 {
		t.Errorf("空文件应被当作空集，写入后应 1 条，实际 %d", got)
	}
}

// TestNormalizeDropsExpiredRecords 按保留天数裁剪过期记录。
func TestNormalizeDropsExpiredRecords(t *testing.T) {
	r, path := newRecorder(t, 7, nil)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	day := int64(24 * 3600 * 1000)
	old := now.UnixMilli() - 8*day
	fresh := now.UnixMilli() - 6*day
	seed := `[{"ts": ` + itoa(old) + `, "accountId": "a", "kind": "task", "title": "旧"},
	          {"ts": ` + itoa(fresh) + `, "accountId": "a", "kind": "task", "title": "新"}]`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	r.Task("a", "活跃上报", ResultSuccess, "")

	all := readAll(t, path)
	titles := map[string]bool{}
	for _, rec := range all {
		titles[rec["title"].(string)] = true
	}
	if titles["旧"] {
		t.Error("超出保留天数的记录应被裁掉")
	}
	if !titles["新"] || !titles["活跃上报"] {
		t.Errorf("保留期内与新记录都应留下，实际 %v", titles)
	}
}

// TestNormalizeTruncatesToMaxKeepingNewest 超上限时截断最旧的，且保留最新的一批。
func TestNormalizeTruncatesToMaxKeepingNewest(t *testing.T) {
	r, path := newRecorder(t, 3650, nil)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	// 直接测 normalize（构造 2 万条真实记录太重）
	items := make([]json.RawMessage, 0, MaxRecords+50)
	for i := 0; i < MaxRecords+50; i++ {
		items = append(items, marshalRecord(record{
			Ts:        now.UnixMilli() - int64(MaxRecords) + int64(i),
			AccountID: "a", Kind: KindTask, Title: "t", Result: ResultSuccess,
		}))
	}
	kept := normalize(items, now.UnixMilli(), 3650)
	if len(kept) != MaxRecords {
		t.Fatalf("应截断到 %d 条，实际 %d", MaxRecords, len(kept))
	}
	// 保留的应是**最新**的一批：首条的 ts 等于原第 50 条
	if got, want := tsOf(kept[0]), tsOf(items[50]); got != want {
		t.Errorf("应截掉最旧的 50 条：首条 ts=%d want %d", got, want)
	}
	_ = path
}

// TestNormalizeNonPositiveRetentionFallsBack 非法保留天数不应把记录清空。
//
// 清空等于静默关闭功能，比「用错天数」严重得多。
func TestNormalizeNonPositiveRetentionFallsBack(t *testing.T) {
	r, path := newRecorder(t, 0, nil) // 0 → 回落默认 60 天
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	if err := os.WriteFile(path, []byte(`[{"ts": `+itoa(now.UnixMilli()-3600*1000)+`, "title": "一小时前"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	r.Task("a", "活跃上报", ResultSuccess, "")

	if got := len(readAll(t, path)); got != 2 {
		t.Errorf("非法保留天数应回落默认而非清空，实际剩 %d 条", got)
	}
}

// ---------------------------------------------------------------------------
// 去重：避免每次调度都刷屏
// ---------------------------------------------------------------------------

// TestTaskDailyDedupesSameDay 同账号同标题同结果当天只写一条。
//
// 任务按整点排程，但用户会手工触发、机器休眠后也会补跑；
// 不去重的话「今天的记录」会变成一片重复行。
func TestTaskDailyDedupesSameDay(t *testing.T) {
	r, path := newRecorder(t, 60, nil)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	r.TaskDaily("u1", "夜猫子任务", ResultInfo, "不在时间窗")
	r.TaskDaily("u1", "夜猫子任务", ResultInfo, "不在时间窗")
	r.TaskDaily("u1", "夜猫子任务", ResultInfo, "不在时间窗")

	if got := len(readAll(t, path)); got != 1 {
		t.Errorf("同一天同标题同结果应只写 1 条，实际 %d 条", got)
	}
}

// TestTaskDailyKeepsDifferentResults 同一天「先失败后成功」两条都保留。
//
// 那正是用户最需要看到的转变；把失败一并去重会掩盖真实故障。
func TestTaskDailyKeepsDifferentResults(t *testing.T) {
	r, path := newRecorder(t, 60, nil)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	r.TaskDaily("u1", "活跃上报", ResultFailed, "网络超时")
	r.TaskDaily("u1", "活跃上报", ResultSuccess, "")

	all := readAll(t, path)
	if len(all) != 2 {
		t.Fatalf("失败与成功是不同结果，应各留一条，实际 %d 条", len(all))
	}
}

// TestTaskDailyIsPerAccount 去重按账号隔离，不跨账号互相抑制。
func TestTaskDailyIsPerAccount(t *testing.T) {
	r, path := newRecorder(t, 60, nil)
	r.now = func() time.Time { return time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC) }

	r.TaskDaily("u1", "活跃上报", ResultSuccess, "")
	r.TaskDaily("u2", "活跃上报", ResultSuccess, "")

	if got := len(readAll(t, path)); got != 2 {
		t.Errorf("不同账号应各写一条，实际 %d 条", got)
	}
}

// TestTaskDailyResetsNextDay 跨天后重新可写（去重不能变成「只写一次」）。
func TestTaskDailyResetsNextDay(t *testing.T) {
	r, path := newRecorder(t, 60, nil)
	at := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return at }

	r.TaskDaily("u1", "夜猫子任务", ResultInfo, "不在时间窗")
	at = at.Add(24 * time.Hour) // 次日同一时刻
	r.TaskDaily("u1", "夜猫子任务", ResultInfo, "不在时间窗")

	if got := len(readAll(t, path)); got != 2 {
		t.Errorf("跨天后应可再写一条，实际 %d 条", got)
	}
}

// TestTaskAlwaysWrites Task（非 Daily）每次调用都写，供「成功且有新变化」场景。
func TestTaskAlwaysWrites(t *testing.T) {
	r, path := newRecorder(t, 60, nil)
	r.now = func() time.Time { return time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC) }

	r.Task("u1", "开学季活动", ResultSuccess, "领取 chat_3_times")
	r.Task("u1", "开学季活动", ResultSuccess, "领取 desktop_chat_1_time")

	if got := len(readAll(t, path)); got != 2 {
		t.Errorf("Task 应每次都写（每个奖励各一条），实际 %d 条", got)
	}
}

// TestTaskAllDailyUsesPlaceholderAccount 汇总记录不带具体账号。
func TestTaskAllDailyUsesPlaceholderAccount(t *testing.T) {
	r, path := newRecorder(t, 60, nil)
	r.now = func() time.Time { return time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC) }

	r.TaskAllDaily("开学季活动", ResultInfo, "活动已下线")

	got := readAll(t, path)[0]
	if got["accountId"] != "" {
		t.Errorf("汇总记录 accountId 应为空串，实际 %v", got["accountId"])
	}
	if got["accountName"] != "全部账号" {
		t.Errorf("汇总记录应有可读的账号名，实际 %v", got["accountName"])
	}
}

// TestDetailIsSingleLineAndBounded 多行/超长错误压成单行并限长。
//
// 界面把 detail 拼在单行末尾，多行文本在那里会显示成空白；
// 不限长则一段上游响应体就能把记录文件撑大。
func TestDetailIsSingleLineAndBounded(t *testing.T) {
	r, path := newRecorder(t, 60, nil)
	r.Task("u1", "活跃上报", ResultFailed, "line1\nline2\r\n\tline3   with   spaces")

	got := readAll(t, path)[0]["detail"].(string)
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("detail 应为单行，实际 %q", got)
	}
	if !strings.Contains(got, "line1 line2 line3") {
		t.Errorf("detail 内容被破坏: %q", got)
	}

	long := strings.Repeat("中", 1000)
	r.Task("u1", "活跃上报", ResultFailed, long)
	all := readAll(t, path)
	gotLong := all[len(all)-1]["detail"].(string)
	// 300 个字符 + 省略号（按 rune 计数，避免把多字节汉字截成半个）
	if n := len([]rune(gotLong)); n > 302 {
		t.Errorf("detail 应限长，实际 %d 个字符", n)
	}
}

// ---------------------------------------------------------------------------
// 并发：4 个任务可能同时到点，写入必须串行化
// ---------------------------------------------------------------------------

// TestConcurrentWritesKeepAllRecords 并发写入不得丢记录、不得写坏 JSON。
//
// 这是本包最关键的正确性保证：scheduler.Run 会在同一时刻派发多个任务，
// 而写入是「读-改-写」——不串行化就会互相覆盖（丢记录），
// 非原子写则会让宿主读到半截文件并把历史整体覆盖。
//
// 用 -race 跑本用例可同时检出数据竞争。
func TestConcurrentWritesKeepAllRecords(t *testing.T) {
	r, path := newRecorder(t, 3650, nil)

	const writers = 8
	const perWriter = 25

	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start // 尽量同时开跑，最大化交错
			for i := 0; i < perWriter; i++ {
				// 每个 (w,i) 的标题唯一 → 既能验总数，也不会被每日去重吃掉
				r.Task("uid-"+itoa(int64(w)), "任务-"+itoa(int64(w))+"-"+itoa(int64(i)), ResultSuccess, "")
			}
		}(w)
	}
	close(start)
	wg.Wait()

	all := readAll(t, path)
	if len(all) != writers*perWriter {
		t.Fatalf("并发写入丢记录：应有 %d 条，实际 %d 条", writers*perWriter, len(all))
	}

	// 每条都必须完整（读-改-写交错会造出字段缺失/空对象）
	seen := map[string]bool{}
	for _, rec := range all {
		title, _ := rec["title"].(string)
		if title == "" {
			t.Fatalf("存在残缺记录（并发交错损坏）: %v", rec)
		}
		if seen[title] {
			t.Errorf("记录重复: %s", title)
		}
		seen[title] = true
		if rec["kind"] != KindTask {
			t.Errorf("kind 被写坏: %v", rec["kind"])
		}
	}
}

// TestConcurrentDailyDedupeIsExact 并发下「每日一条」也必须精确去重。
//
// 读-改-写不加锁时，两个 goroutine 会同时读到「今天还没有」然后各写一条 ——
// 表现为记录里出现成对的重复行。
func TestConcurrentDailyDedupeIsExact(t *testing.T) {
	r, path := newRecorder(t, 60, nil)
	r.now = func() time.Time { return time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC) }

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r.TaskDaily("u1", "夜猫子任务", ResultInfo, "不在时间窗")
		}()
	}
	close(start)
	wg.Wait()

	if got := len(readAll(t, path)); got != 1 {
		t.Errorf("并发下的每日去重应只留 1 条，实际 %d 条", got)
	}
}

// TestConcurrentWithHostWriterDoesNotCorrupt 与「宿主侧写入」并发时文件始终可解析。
//
// 真实场景里宿主也会写这个文件（签到记录、积分变化）。本用例用一个独立的
// goroutine 模拟宿主：原子替换整个文件。网关侧必须保证任何时刻读到的都是
// 完整 JSON —— 否则宿主解析失败会退化成空集，接着就把历史覆盖掉。
func TestConcurrentWithHostWriterDoesNotCorrupt(t *testing.T) {
	r, path := newRecorder(t, 60, nil)

	stop := make(chan struct{})
	var hostWG sync.WaitGroup
	hostWG.Add(1)
	go func() {
		defer hostWG.Done()
		hostRec := `[{"ts": 1790000000000, "accountId": "host", "kind": "credit", "title": "宿主记录"}]`
		for {
			select {
			case <-stop:
				return
			default:
			}
			// 模拟宿主的 atomic_write：临时文件 + rename
			tmp := path + ".host-tmp"
			if err := os.WriteFile(tmp, []byte(hostRec), 0o600); err == nil {
				_ = os.Rename(tmp, path)
			}
			time.Sleep(time.Millisecond)
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.Task("u"+itoa(int64(i)), "活跃上报", ResultSuccess, "")
		}(i)
	}
	wg.Wait()
	close(stop)
	hostWG.Wait()

	// 文件必须是合法 JSON 数组（不校验内容：宿主覆盖是预期行为）
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件失败: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("文件被写坏（宿主会读不出来并覆盖历史）: %v", err)
	}
}

// TestNoTempFilesLeftBehind 正常写入后不残留临时文件。
//
// 残留的 .tmp-<pid> 会被宿主的目录扫描当成垃圾，也会让排查者误以为有并发冲突。
func TestNoTempFilesLeftBehind(t *testing.T) {
	r, path := newRecorder(t, 60, nil)
	r.Task("u1", "活跃上报", ResultSuccess, "")

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("残留临时文件: %s", e.Name())
		}
	}
}

// TestEnabledAndPath 基本访问器语义。
func TestEnabledAndPath(t *testing.T) {
	r, path := newRecorder(t, 60, nil)
	if !r.Enabled() {
		t.Error("配置了路径应报告启用")
	}
	if r.Path() != path {
		t.Errorf("Path()=%q want %q", r.Path(), path)
	}
	// 空白路径视同未配置（宿主可能写空串）
	if New("   ", 60, nil).Enabled() {
		t.Error("空白路径应视同未配置")
	}
}

// TestCstDayMatchesNightOwlWindow 日界用 CST，与夜猫子时间窗同一口径。
func TestCstDayMatchesNightOwlWindow(t *testing.T) {
	// CST 2026-09-16 00:00 与 23:59 必须落在同一天
	base := time.Date(2026, 9, 15, 16, 0, 0, 0, time.UTC) // = CST 09-16 00:00
	start := base.UnixMilli()
	end := base.Add(23*time.Hour + 59*time.Minute).UnixMilli()
	if cstDay(start) != cstDay(end) {
		t.Errorf("CST 同一天被拆成两天: %d vs %d", cstDay(start), cstDay(end))
	}
	// 跨过 CST 午夜后必须换天
	next := base.Add(24 * time.Hour).UnixMilli()
	if cstDay(start) == cstDay(next) {
		t.Error("跨 CST 午夜后应换天")
	}
}

// TestEncodingMatchesSerdeJsonShape 输出形状必须与宿主 serde_json::to_string_pretty 对齐。
//
// 端到端实测发现的差异：Go 的 json.Marshal 默认把 < > & 转义成 \u003c 之类，
// 而 serde_json 不转义。上游错误体常带 HTML（网关 401 时返回 openresty 错误页），
// 两个写入方交替改写同一个文件会让同一段文本时而 \u003c 时而 <，
// diff 变噪声、人工核对记录时极难读。
func TestEncodingMatchesSerdeJsonShape(t *testing.T) {
	r, path := newRecorder(t, 60, nil)
	// 刻意放入 serde_json 不会转义、而 Go 默认会转义的字符
	r.Task("u1", "trial 加油包", ResultFailed,
		`upstream: <html><h1>401</h1></html> a & b`)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)

	if strings.Contains(text, `\u003c`) || strings.Contains(text, `\u0026`) ||
		strings.Contains(text, `\u003e`) {
		t.Errorf("不应做 HTML 转义（宿主 serde_json 不转义，两边会反复改写同一文件）:\n%s", text)
	}
	if !strings.Contains(text, "<html>") {
		t.Errorf("原始尖括号应原样保留:\n%s", text)
	}
	// 缩进与宿主 pretty 输出一致：数组元素各占一行、缩进两格
	if !strings.HasPrefix(text, "[\n  {") {
		t.Errorf("应为 pretty 数组格式（与 serde_json::to_string_pretty 一致）:\n%s", text)
	}
	// 多行记录的每行都要跟着缩进，否则数组层级错乱
	if strings.Contains(text, "\n\"") {
		t.Errorf("多行元素未整体缩进:\n%s", text)
	}

	// 关键：整体仍是宿主能解析的 JSON，且内容无损
	var back []map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("宿主会读不出来: %v\n%s", err, text)
	}
	if len(back) != 1 {
		t.Fatalf("应有 1 条记录，实际 %d", len(back))
	}
	if !strings.Contains(back[0]["detail"].(string), "<html>") {
		t.Errorf("detail 内容被破坏: %v", back[0]["detail"])
	}
}

// TestRoundTripAfterOwnWrite 自己写的文件再读回来必须无损（含多行 detail）。
//
// 读写用同一套结构，形状一旦不自洽就会在第二次写入时丢字段 ——
// 那正是「记录写着写着就少了」这类难查问题的成因。
func TestRoundTripAfterOwnWrite(t *testing.T) {
	r, path := newRecorder(t, 60, map[string]Identity{"u1": {ID: "id-1", Name: "甲"}})
	r.Task("u1", "活跃上报", ResultSuccess, "第一轮")
	r.Task("u1", "活跃上报", ResultSuccess, "第二轮 <b>加粗</b>")

	all := readAll(t, path)
	if len(all) != 2 {
		t.Fatalf("应有 2 条记录，实际 %d 条", len(all))
	}
	if all[0]["detail"] != "第一轮" {
		t.Errorf("第一条 detail 被后写入破坏: %v", all[0]["detail"])
	}
	if all[1]["detail"] != "第二轮 <b>加粗</b>" {
		t.Errorf("第二条 detail 不对: %v", all[1]["detail"])
	}
	for _, rec := range all {
		if rec["accountId"] != "id-1" || rec["accountName"] != "甲" {
			t.Errorf("身份字段在回读时丢失: %v", rec)
		}
	}
}

// TestRepeatedWritesDoNotCompoundIndentation 反复写入不得让缩进逐轮累加。
//
// 端到端实测踩到的真实 bug：读回的 RawMessage 自带上一轮的缩进，
// 原样拼接再整体缩进一次，缩进就会每次写入增长 2 空格（2 → 4 → 6 …），
// 几轮后文件形状彻底走样。这里连续写 5 次并断言缩进恒定。
func TestRepeatedWritesDoNotCompoundIndentation(t *testing.T) {
	r, path := newRecorder(t, 60, map[string]Identity{"u1": {ID: "id-1", Name: "甲"}})

	for i := 0; i < 5; i++ {
		r.Task("u1", "活跃上报", ResultSuccess, "第 "+itoa(int64(i))+" 轮")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)

	// 每条记录的字段行缩进必须恒定 4 空格（数组 2 + 对象 2），不随轮次增长。
	// 4 是实测 serde_json::to_string_pretty 对「数组内对象字段」的缩进
	//（见 crates 侧同形状输出），两侧保持一致 diff 才可读。
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), `"ts"`) {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent != 4 {
			t.Errorf("ts 行缩进应恒为 4 空格，实际 %d（缩进在逐轮累加）:\n%s", indent, text)
		}
	}
	// 记录条数必须正确（重排不能丢记录）
	if got := len(readAll(t, path)); got != 5 {
		t.Errorf("应有 5 条记录，实际 %d 条", got)
	}
}

// TestIndentPreservesLargeTimestamps 重排不得把 ts 这类大整数转成科学计数法。
//
// 13 位毫秒若经 float64 中转会精度受损（界面按 ms 过滤日期，错了就查不到记录）。
func TestIndentPreservesLargeTimestamps(t *testing.T) {
	r, path := newRecorder(t, 3650, nil)
	// 一个典型的大毫秒值（含非零末位，float64 无法精确表示时会暴露）
	ts := int64(1789540074441)
	r.now = func() time.Time { return time.UnixMilli(ts) }

	r.Task("u1", "活跃上报", ResultSuccess, "")
	// 再写一条，触发一次「读回 → 重排 → 写回」，这才是有风险的路径
	r.Task("u1", "活跃上报", ResultSuccess, "")

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "e+") || strings.Contains(string(raw), "E+") {
		t.Errorf("大整数被写成科学计数法:\n%s", raw)
	}
	for _, rec := range readAll(t, path) {
		if got := int64(rec["ts"].(float64)); got != ts {
			t.Errorf("ts 精度受损: %d want %d", got, ts)
		}
	}
}

// itoa 避免引入 strconv 只为拼测试数据。
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
