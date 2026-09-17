package main

import (
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// account_records 配置块（宿主透传的账号记录回写目标）
//
// 背景：4 个养号任务实现在 Go 网关里，日志此前只写 stdout，而宿主启动
// 子进程时把它丢进了 Stdio::null —— 用户在界面上看不到任何执行痕迹。
// 修法是让网关写宿主已经在读的 account_records.json，因此**路径必须由宿主
// 给出**：两边各自推算数据目录迟早会算出不同的值，而那种错法表现为
// 「静默不记录」，几乎无法从现象定位。
// ---------------------------------------------------------------------------

// TestAccountRecordsAbsentMeansDisabled 整个块缺席时不应启用回写。
//
// 独立运行 gateway.exe（无宿主）走的就是这条路：必须安静地什么都不做，
// 不报错、不刷日志、不创建文件。
func TestAccountRecordsAbsentMeansDisabled(t *testing.T) {
	for name, body := range map[string]string{
		"键完全缺席":       `{}`,
		"空对象":         `{"account_records":{}}`,
		"file 为 null": `{"account_records":{"file":null}}`,
		"file 为空串":    `{"account_records":{"file":""}}`,
	} {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(fp)
		if err != nil {
			t.Fatalf("%s: 缺省不应导致加载失败: %v", name, err)
		}
		if c.AccountRecords.File != "" {
			t.Errorf("%s: File 应为空（= 不记录），实际 %q", name, c.AccountRecords.File)
		}
		if got := c.RecordIdentities(); len(got) != 0 {
			t.Errorf("%s: 身份映射应为空，实际 %v", name, got)
		}
	}
}

// TestAccountRecordsParsedFromFile 宿主写入的字段必须被逐字读出来。
//
// 字段名对不上是这条链路最主要的失效方式：出错时既不报错也不写记录，
// 只表现为「界面上还是没有记录」—— 与原来的 bug 现象完全一样。
func TestAccountRecordsParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	body := `{
		"account_records": {
			"file": "C:\\Users\\tester\\.ai-gateway\\account_records.json",
			"retention_days": 30,
			"identities": [
				{"uid": "uid-1", "id": "acct-uuid-1", "name": "用户甲"},
				{"uid": "uid-2", "id": "acct-uuid-2", "name": "用户乙"}
			]
		}
	}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	if want := `C:\Users\tester\.ai-gateway\account_records.json`; c.AccountRecords.File != want {
		t.Errorf("file=%q want %q", c.AccountRecords.File, want)
	}
	if c.AccountRecords.RetentionDays != 30 {
		t.Errorf("retention_days=%d want 30", c.AccountRecords.RetentionDays)
	}

	ids := c.RecordIdentities()
	if len(ids) != 2 {
		t.Fatalf("应解析出 2 条身份映射，实际 %d 条: %v", len(ids), ids)
	}
	// uid → 宿主的 id 与展示名：界面按 id 过滤记录，写 uid 会查不到
	if got := ids["uid-1"]; got.ID != "acct-uuid-1" || got.Name != "用户甲" {
		t.Errorf("uid-1 映射错误: %+v", got)
	}
	if got := ids["uid-2"]; got.ID != "acct-uuid-2" || got.Name != "用户乙" {
		t.Errorf("uid-2 映射错误: %+v", got)
	}
}

// TestRecordIdentitiesSkipsEmptyUID 缺 uid 的条目跳过，不产生空键。
//
// 空 uid 作为 map 键会与「无账号的汇总记录」语义撞车
// （records 包用空 uid 表示「全部账号」）。
func TestRecordIdentitiesSkipsEmptyUID(t *testing.T) {
	c := Default()
	c.AccountRecords.Identities = append(c.AccountRecords.Identities,
		struct {
			UID  string `json:"uid"`
			ID   string `json:"id"`
			Name string `json:"name"`
		}{UID: "", ID: "orphan-id", Name: "无 uid"},
		struct {
			UID  string `json:"uid"`
			ID   string `json:"id"`
			Name string `json:"name"`
		}{UID: "uid-ok", ID: "id-ok", Name: "正常"},
	)

	ids := c.RecordIdentities()
	if _, ok := ids[""]; ok {
		t.Error("空 uid 不应产生映射条目（会与「全部账号」的汇总记录撞车）")
	}
	if len(ids) != 1 || ids["uid-ok"].ID != "id-ok" {
		t.Errorf("应只保留有 uid 的条目，实际 %v", ids)
	}
}

// TestAccountRecordsPartialIdentities 只给了 file、没给 identities 时仍可用。
//
// 身份映射缺失会退化成「accountId = uid」（见 records.Recorder.identity），
// 记录不会丢 —— 比整条不写要好。这里只要求解析不失败。
func TestAccountRecordsPartialIdentities(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"account_records":{"file":"/tmp/r.json"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("缺 identities 不应导致加载失败: %v", err)
	}
	if c.AccountRecords.File != "/tmp/r.json" {
		t.Errorf("file 应被读出，实际 %q", c.AccountRecords.File)
	}
	if c.AccountRecords.RetentionDays != 0 {
		t.Errorf("retention_days 未给时应为 0（由 records 包回落默认），实际 %d",
			c.AccountRecords.RetentionDays)
	}
	if len(c.RecordIdentities()) != 0 {
		t.Error("身份映射应为空")
	}
}
