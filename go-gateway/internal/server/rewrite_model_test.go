package server

import (
	"encoding/json"
	"testing"
)

// ---------------------------------------------------------------------------
// 请求体 model 改写回归
//
// 这是端到端实测抓到的真实缺陷：`cn:` / `global:` 前缀成功约束了选号，
// 但**请求体里的 model 仍是带前缀的原串**，上游不认识前缀，返回
//
//	400 code=11102 model [cn:glm-5.2] service info not found
//
// 现象很迷惑：前缀「生效了」（选号确实按区域走），请求却全部失败。
// ---------------------------------------------------------------------------

// TestRewriteModelStripsPrefix 带前缀的 model 被改写成裸名。
func TestRewriteModelStripsPrefix(t *testing.T) {
	in := []byte(`{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	out := rewriteModel(in, "glm-5.2")

	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("改写后应是合法 JSON: %v", err)
	}
	if doc["model"] != "glm-5.2" {
		t.Errorf("model 应被改写为裸名 glm-5.2，实际 %v", doc["model"])
	}
	// 其余字段必须原样保留
	msgs, ok := doc["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Errorf("messages 应原样保留，实际 %v", doc["messages"])
	}
}

// TestRewriteModelNoChangeKeepsBody 无需改写时返回原 body（不重新序列化）。
//
// 保持字节一致可避免无谓的格式变化（如字段顺序、空白），
// 也省掉一次序列化开销。
func TestRewriteModelNoChangeKeepsBody(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","messages":[]}`)
	out := rewriteModel(in, "glm-5.2")
	if string(out) != string(in) {
		t.Errorf("无需改写时应原样返回\n  实际 %s\n  期望 %s", out, in)
	}
}

// TestRewriteModelEmptyBareKeepsBody bare 为空时不动（无法判断该改成什么）。
func TestRewriteModelEmptyBareKeepsBody(t *testing.T) {
	in := []byte(`{"model":"glm-5.2"}`)
	if out := rewriteModel(in, ""); string(out) != string(in) {
		t.Errorf("bare 为空时应原样返回，实际 %s", out)
	}
}

// TestRewriteModelInvalidJSONKeepsBody 非法 JSON 原样返回。
//
// 宁可让上游报错，也不要把请求体改坏 —— 改写失败不应引入新的失败原因。
func TestRewriteModelInvalidJSONKeepsBody(t *testing.T) {
	in := []byte(`{not json`)
	if out := rewriteModel(in, "glm-5.2"); string(out) != string(in) {
		t.Errorf("非法 JSON 应原样返回，实际 %s", out)
	}
}

// TestRewriteModelMissingModelKeepsBody 没有 model 字段时原样返回。
func TestRewriteModelMissingModelKeepsBody(t *testing.T) {
	in := []byte(`{"messages":[]}`)
	if out := rewriteModel(in, "glm-5.2"); string(out) != string(in) {
		t.Errorf("无 model 字段时应原样返回，实际 %s", out)
	}
}

// TestRewriteModelNonStringModelKeepsBody model 不是字符串时原样返回。
func TestRewriteModelNonStringModelKeepsBody(t *testing.T) {
	in := []byte(`{"model":123}`)
	if out := rewriteModel(in, "glm-5.2"); string(out) != string(in) {
		t.Errorf("model 非字符串时应原样返回，实际 %s", out)
	}
}

// TestRewriteModelPreservesOtherFields 其余字段（含嵌套）完整保留。
func TestRewriteModelPreservesOtherFields(t *testing.T) {
	in := []byte(`{"model":"global:gpt-6-astra","stream":true,"max_tokens":100,` +
		`"messages":[{"role":"system","content":"a"},{"role":"user","content":"b"}],` +
		`"tools":[{"type":"function","function":{"name":"f"}}]}`)
	out := rewriteModel(in, "gpt-6-astra")

	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("应是合法 JSON: %v", err)
	}
	if doc["model"] != "gpt-6-astra" {
		t.Errorf("model 应被改写，实际 %v", doc["model"])
	}
	if doc["stream"] != true {
		t.Error("stream 字段丢失")
	}
	if doc["max_tokens"] != float64(100) {
		t.Error("max_tokens 字段丢失")
	}
	if len(doc["messages"].([]any)) != 2 {
		t.Error("messages 数量不对")
	}
	if len(doc["tools"].([]any)) != 1 {
		t.Error("tools 字段丢失")
	}
}

// TestRewriteModelRoundTripWithResolve 与 resolveModel 配合的完整链路。
func TestRewriteModelRoundTripWithResolve(t *testing.T) {
	in := []byte(`{"model":"cn:glm-5.2"}`)
	realm, bare := resolveModel(modelOf(in))
	if realm != "cn" || bare != "glm-5.2" {
		t.Fatalf("resolveModel 解析错误: realm=%q bare=%q", realm, bare)
	}
	out := rewriteModel(in, bare)
	if got := modelOf(out); got != "glm-5.2" {
		t.Errorf("改写后 modelOf 应为裸名，实际 %q", got)
	}
}
