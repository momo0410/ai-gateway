package prompt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Load：内置默认 / 文件覆盖 / fail fast
// ---------------------------------------------------------------------------

// TestLoadEmptyFileUsesBuiltin 空 file → 内置默认（custom 模式不配 file 时的路径）。
func TestLoadEmptyFileUsesBuiltin(t *testing.T) {
	got, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") 不应报错: %v", err)
	}
	if got != defaultPrompt {
		t.Errorf("空 file 应返回内置默认，实际 len=%d 期望 len=%d", len(got), len(defaultPrompt))
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("内置默认提示词为空：Rewrite 会因空提示词变成空操作")
	}
}

// TestLoadFileOverridesBuiltin 非空 file → 文件内容**逐字**覆盖内置默认。
func TestLoadFileOverridesBuiltin(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "my-prompt.md")
	want := "你是一名工程助手。\n\n- 保持简洁。\n"
	if err := os.WriteFile(fp, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(fp)
	if err != nil {
		t.Fatalf("Load(%s) 报错: %v", fp, err)
	}
	if got != want {
		t.Errorf("文件内容应逐字覆盖内置默认\n  实际 %q\n  期望 %q", got, want)
	}
	if got == defaultPrompt {
		t.Error("返回了内置默认，文件覆盖未生效")
	}
}

// TestLoadFileMissingFailsFast 文件不存在必须报错（fail fast），不静默回落默认。
//
// 用户明确配了文件却读不到，若静默用别的提示词，现象是「配了却像没配」，
// 排查成本极高 —— 因此启动阶段就要报错。
func TestLoadFileMissingFailsFast(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "not-exist.md")
	if _, err := Load(fp); err == nil {
		t.Fatal("文件不存在应返回 error（fail fast），实际为 nil")
	} else if !strings.Contains(err.Error(), "prompt.file") {
		t.Errorf("错误信息应指明 prompt.file 字段，便于定位，实际: %v", err)
	}
}

// TestLoadFileIsDirectoryFailsFast 路径指向目录（ReadFile 失败）同样报错。
func TestLoadFileIsDirectoryFailsFast(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err == nil {
		t.Fatal("路径是目录应返回 error（fail fast），实际为 nil")
	}
}

// TestLoadEmptyContentFailsFast 文件存在但内容为空白 → 报错。
//
// 空提示词会让 Rewrite 变成空操作（等价于 passthrough），
// 用户以为在用自有提示词、实际客户端 system 一条没动，属于同一种「静默用错」。
func TestLoadEmptyContentFailsFast(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "blank.md")
	if err := os.WriteFile(fp, []byte("   \n\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(fp); err == nil {
		t.Fatal("空白内容应返回 error（fail fast），实际为 nil")
	}
}

// TestLoadWhitespaceFileTreatedAsEmpty file 只含空白 → 视同未配置，用内置默认。
func TestLoadWhitespaceFileTreatedAsEmpty(t *testing.T) {
	got, err := Load("   ")
	if err != nil {
		t.Fatalf("空白 file 应视同未配置: %v", err)
	}
	if got != defaultPrompt {
		t.Error("空白 file 应返回内置默认")
	}
}

// TestDefaultPromptIsNeutralNoFingerprint 内置默认提示词本身不得引入新指纹。
//
// 本功能的目的就是消灭 system 来源的指纹误报，若默认提示词里带上
// 客户端/厂商身份句，等于把误报从客户端搬到网关自己身上。
func TestDefaultPromptIsNeutralNoFingerprint(t *testing.T) {
	for _, banned := range []string{
		"Claude Code", "Anthropic", "OpenAI", "Codex",
		"x-anthropic-billing-header", "cc_",
	} {
		if strings.Contains(defaultPrompt, banned) {
			t.Errorf("内置默认提示词含指纹串 %q：会引入新的 system 指纹误报", banned)
		}
	}
}

// ---------------------------------------------------------------------------
// Rewrite：出站改写（关键路径）
// ---------------------------------------------------------------------------

// rolesOf 取出改写过后的 role 序列，供断言 messages 结构。
func rolesOf(t *testing.T, body []byte) []string {
	t.Helper()
	var doc struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("改写过后的 body 不是合法 JSON: %v（body=%s）", err, body)
	}
	out := make([]string, 0, len(doc.Messages))
	for _, m := range doc.Messages {
		out = append(out, m.Role)
	}
	return out
}

func assertRoles(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("role 序列长度不符: 实际 %v 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("role[%d]=%q 期望 %q（完整序列 %v）", i, got[i], want[i], got)
		}
	}
}

// TestRewriteDropsSystemAndDeveloperAndPrepends 删除 system/developer 并在头部插入自有 system。
func TestRewriteDropsSystemAndDeveloperAndPrepends(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"OLD-SYSTEM-FINGERPRINT"},` +
		`{"role":"developer","content":"OLD-DEVELOPER-FINGERPRINT"},` +
		`{"role":"user","content":"你好"}]}`)

	out := Rewrite(in, "MY-SYS")

	assertRoles(t, rolesOf(t, out), []string{"system", "user"})
	if strings.Contains(string(out), "OLD-SYSTEM-FINGERPRINT") ||
		strings.Contains(string(out), "OLD-DEVELOPER-FINGERPRINT") {
		t.Errorf("旧 system/developer 内容有残留：%s", out)
	}

	var doc struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Messages[0].Content != "MY-SYS" {
		t.Errorf("头部 system 内容应为自有提示词，实际 %q", doc.Messages[0].Content)
	}
}

// TestRewriteIsCaseInsensitiveOnRole role 大小写/空白变体同样要删干净。
//
// 漏删的后果是客户端指纹仍留在请求里，正好绕过本功能的目的。
func TestRewriteIsCaseInsensitiveOnRole(t *testing.T) {
	in := []byte(`{"messages":[` +
		`{"role":"System","content":"A"},` +
		`{"role":"DEVELOPER","content":"B"},` +
		`{"role":" system ","content":"C"},` +
		`{"role":"user","content":"keep"}]}`)

	out := Rewrite(in, "SYS")
	assertRoles(t, rolesOf(t, out), []string{"system", "user"})
	for _, leak := range []string{`"A"`, `"B"`, `"C"`} {
		if strings.Contains(string(out), leak) {
			t.Errorf("旧 system 变体有残留 %s：%s", leak, out)
		}
	}
}

// TestRewriteKeepsUserAssistantTool 只动 messages 层级，其余消息逐字保留（含顺序）。
func TestRewriteKeepsUserAssistantTool(t *testing.T) {
	in := []byte(`{"messages":[` +
		`{"role":"system","content":"old"},` +
		`{"role":"user","content":"u-content"},` +
		`{"role":"assistant","content":"a-content"},` +
		`{"role":"tool","tool_call_id":"t1","content":"tool-result"}]}`)

	out := Rewrite(in, "SYS")
	assertRoles(t, rolesOf(t, out), []string{"system", "user", "assistant", "tool"})

	var doc struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Messages[1]["content"] != "u-content" {
		t.Errorf("user 消息被改动: %v", doc.Messages[1])
	}
	if doc.Messages[2]["content"] != "a-content" {
		t.Errorf("assistant 消息被改动: %v", doc.Messages[2])
	}
	tool := doc.Messages[3]
	if tool["tool_call_id"] != "t1" || tool["content"] != "tool-result" {
		t.Errorf("tool 消息被改动: %v", tool)
	}
}

// TestRewriteKeepsOtherTopLevelFields messages 之外的字段逐字不动。
//
// 用 RawMessage 透传的意义：数字不被转成 float64、字段顺序不变。
func TestRewriteKeepsOtherTopLevelFields(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","stream":true,"max_tokens":100,` +
		`"temperature":0.7,"metadata":{"conversation_id":"c1"},"tools":[{"type":"function"}],` +
		`"messages":[{"role":"user","content":"hi"}]}`)

	out := Rewrite(in, "SYS")
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	// 逐字比较：这些字段必须与原请求**字节一致**。
	for _, field := range []string{"model", "stream", "max_tokens", "temperature", "metadata", "tools"} {
		var want map[string]json.RawMessage
		if err := json.Unmarshal(in, &want); err != nil {
			t.Fatal(err)
		}
		if string(doc[field]) != string(want[field]) {
			t.Errorf("字段 %s 被改动: 实际 %s 期望 %s", field, doc[field], want[field])
		}
	}
}

// TestRewriteNoMessagesFieldInsertsSystemOnly 无 messages 字段 → 插入单条 system，其余字段保留。
func TestRewriteNoMessagesFieldInsertsSystemOnly(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","stream":true}`)

	out := Rewrite(in, "SYS")
	assertRoles(t, rolesOf(t, out), []string{"system"})

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc["model"]) != `"glm-5.2"` || string(doc["stream"]) != "true" {
		t.Errorf("其余字段应原样保留: %s", out)
	}
}

// TestRewriteBadMessagesTypeInsertsSystemOnly messages 类型不符（非数组）同样只插 system。
func TestRewriteBadMessagesTypeInsertsSystemOnly(t *testing.T) {
	for _, in := range []string{
		`{"messages":"not-an-array","model":"m"}`,
		`{"messages":{"role":"system"},"model":"m"}`,
		`{"messages":null,"model":"m"}`,
	} {
		out := Rewrite([]byte(in), "SYS")
		assertRoles(t, rolesOf(t, out), []string{"system"})
		if !strings.Contains(string(out), `"m"`) {
			t.Errorf("其余字段应保留: in=%s out=%s", in, out)
		}
	}
}

// TestRewriteInvalidJSONReturnedAsIs 非法 JSON 原样返回（绝不失败）。
//
// Rewrite 在转发关键路径上：解析失败该让上游按原语义处理，
// 而不是由网关引入一个新的失败原因。
func TestRewriteInvalidJSONReturnedAsIs(t *testing.T) {
	for _, in := range []string{`{not valid json`, `[]`, `"a string"`, `null`} {
		out := Rewrite([]byte(in), "SYS")
		if string(out) != in {
			t.Errorf("非法/非对象 JSON 应原样返回\n  in  %s\n  out %s", in, out)
		}
	}
}

// TestRewriteEmptyInputsReturnedAsIs 空 body / 空提示词一律不改写。
func TestRewriteEmptyInputsReturnedAsIs(t *testing.T) {
	if out := Rewrite(nil, "SYS"); len(out) != 0 {
		t.Errorf("空 body 应原样返回，实际 %s", out)
	}
	in := []byte(`{"messages":[{"role":"system","content":"old"}]}`)
	if out := Rewrite(in, ""); string(out) != string(in) {
		t.Errorf("空提示词应原样返回（不改写），实际 %s", out)
	}
	if out := Rewrite(in, "   "); string(out) != string(in) {
		t.Errorf("空白提示词应原样返回（不改写），实际 %s", out)
	}
}

// TestRewriteEscapesPromptSafely 提示词含引号/换行/反斜杠时仍产出合法 JSON。
//
// 提示词来自用户文件，内容不可预期；手工拼 JSON 会拼出非法转义。
func TestRewriteEscapesPromptSafely(t *testing.T) {
	tricky := "行1 \"带引号\" 与反斜杠 \\ 以及\n换行\t制表"
	in := []byte(`{"messages":[{"role":"system","content":"old"},{"role":"user","content":"hi"}]}`)

	out := Rewrite(in, tricky)
	var doc struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("改写过后的 body 非法 JSON: %v（body=%s）", err, out)
	}
	if doc.Messages[0].Content != tricky {
		t.Errorf("提示词往返不一致\n  实际 %q\n  期望 %q", doc.Messages[0].Content, tricky)
	}
}

// TestRewriteMultimodalContentUntouched 多模态 user content 内部结构不动。
func TestRewriteMultimodalContentUntouched(t *testing.T) {
	in := []byte(`{"messages":[` +
		`{"role":"system","content":"old"},` +
		`{"role":"user","content":[{"type":"text","text":"看图"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`)

	out := Rewrite(in, "SYS")
	// 用 RawMessage 承接 content：头部注入的 system 是字符串 content，
	// 而 user 是多模态数组，两者类型不同，不能共用一个具体类型。
	var doc struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Messages) != 2 {
		t.Fatalf("messages 长度应为 2，实际 %d", len(doc.Messages))
	}
	var parts []map[string]any
	if err := json.Unmarshal(doc.Messages[1].Content, &parts); err != nil {
		t.Fatalf("多模态 content 不是数组: %v（content=%s）", err, doc.Messages[1].Content)
	}
	if len(parts) != 2 {
		t.Fatalf("多模态 content 被改动: %v", parts)
	}
	if parts[0]["type"] != "text" || parts[0]["text"] != "看图" {
		t.Errorf("text part 被改动: %v", parts[0])
	}
	if parts[1]["type"] != "image_url" {
		t.Errorf("image part 被改动: %v", parts[1])
	}
}

// TestRewriteKeepsNonObjectMessages 非对象消息（畸形项）保守保留，不误删用户内容。
func TestRewriteKeepsNonObjectMessages(t *testing.T) {
	in := []byte(`{"messages":[{"role":"system","content":"old"},"just-a-string",123]}`)
	out := Rewrite(in, "SYS")

	var doc struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Messages) != 3 {
		t.Fatalf("畸形项应被保留（消息数 3），实际 %d: %s", len(doc.Messages), out)
	}
}

// TestRewriteOnlySystemMessagesLeavesSingleSystem 全部是 system 时只剩一条自有 system。
func TestRewriteOnlySystemMessagesLeavesSingleSystem(t *testing.T) {
	in := []byte(`{"messages":[{"role":"system","content":"a"},{"role":"system","content":"b"}]}`)
	out := Rewrite(in, "SYS")
	assertRoles(t, rolesOf(t, out), []string{"system"})
}

// TestDegradedIsNeutral Degraded 提示词必须存在且中性（不引入指纹）。
func TestDegradedIsNeutral(t *testing.T) {
	if strings.TrimSpace(Degraded) == "" {
		t.Fatal("Degraded 为空")
	}
	for _, banned := range []string{"Claude", "Anthropic", "OpenAI", "Codex"} {
		if strings.Contains(Degraded, banned) {
			t.Errorf("Degraded 含指纹串 %q", banned)
		}
	}
}
