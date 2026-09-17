//go:build live

// blackcat_live_test.go black_cat 判据的诊断探针（仅 live 构建标签）。
//
// 背景：实测发现按"桌面指纹对话 + billing 域模型对齐上报"做完 2 次夜间对话后，
// black_cat 进度仍是 1/3 —— 上报 HTTP 200 但**不计分**。
// 这正是本功能最危险的失败形态（静默丢弃），因此单独写一个诊断测试，
// 把几种候选口径各试一次，用真实进度变化判断哪一种才对。
//
// 只在显式开启时运行：每次尝试都会发真实对话（消耗额度）。
//
//	$env:WB_LIVE_BLACKCAT = "1"
package growtask

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// blackcatProgress 读取 black_cat 当前进度（ok=false 表示任务缺失）。
func blackcatProgress(t *testing.T, c *upstream.Client, a *auth.Auth) (current, target int64, ok bool) {
	t.Helper()
	tasks, err := c.ListGrowthTasks(a)
	if err != nil {
		t.Fatalf("拉取任务列表失败: %v", err)
	}
	for _, task := range tasks {
		if task.Code == "black_cat" {
			return task.Current, task.Target, true
		}
	}
	return 0, 0, false
}

// TestLiveBlackCatProbe 逐口径试 black_cat，观察哪一种真的推进进度。
//
// 候选口径（每次试一种，试完立刻回读）：
//	A. CLI 域 chat（本项目既有 ChatStream）+ billing 域模型对齐上报
//	B. 桌面域 chat（raw）+ billing 域模型对齐上报（= 当前实现）
//	C. 桌面域完整对话事件链（chat_message_response 成功回执）
//
// 判断依据只看**进度是否变化**：HTTP 200 不作数（那正是要识破的假象）。
func TestLiveBlackCatProbe(t *testing.T) {
	if os.Getenv("WB_LIVE_BLACKCAT") != "1" {
		t.Skip("未设置 WB_LIVE_BLACKCAT=1，跳过 black_cat 诊断")
	}
	a := loadLiveAccount(t, os.Getenv("WB_LIVE_ACCOUNT"))
	c := liveClient()

	cur, tgt, ok := blackcatProgress(t, c, a)
	if !ok {
		t.Fatal("该账号没有 black_cat 任务")
	}
	t.Logf("起始进度 %d/%d，夜猫窗口内=%v", cur, tgt, upstream.InNightWindow(time.Now()))

	report := func(label string, before, after int64) {
		delta := after - before
		verdict := "❌ 不计分"
		if delta > 0 {
			verdict = "✅ 计分"
		}
		t.Logf("  %s：%d → %d (Δ%+d) %s", label, before, after, delta, verdict)
	}

	// 口径 A：CLI 域 chat（既有 ChatStream 通道）+ 模型对齐上报。
	// 参考项目用的正是这条（其 blackcat.go 走 ChatStream + ReportChatActivityModel）。
	if cur < tgt {
		before, _, _ := blackcatProgress(t, c, a)
		body, _ := json.Marshal(map[string]any{
			"model":    "glm-5.2",
			"messages": []map[string]any{{"role": "user", "content": "1+1等于几？直接回答。"}},
			"stream":   true,
		})
		rc, status, respBody, err := c.ChatStream(a, body)
		if err != nil || status >= 400 {
			t.Logf("  口径 A 对话失败: status=%d err=%v body=%.120s", status, err, respBody)
		} else {
			// 读干 SSE：不读完会残留半开连接。
			drainSSE(rc)
			rid := "wb2api-bc-a-" + itoaMs()
			if err := c.ReportChatActivityModel(a, rid, "", "glm-5.2", "GLM-5.2"); err != nil {
				t.Logf("  口径 A 上报失败: %v", err)
			}
			waitScoring()
			after, _, _ := blackcatProgress(t, c, a)
			report("口径A CLI chat + billing 对齐上报", before, after)
			cur = after
		}
	}

	// 口径 B：桌面域完整对话事件链（含成功回执）。
	if cur < tgt {
		before, _, _ := blackcatProgress(t, c, a)
		events := upstream.ChatSequence("wb2api-bc-b-conv", "wb2api-bc-b-req", "msg-bc-b", "glm-5.2", "GLM-5.2")
		if err := c.ReportDesktopEvents(a, events...); err != nil {
			t.Logf("  口径 B 上报失败: %v", err)
		}
		waitScoring()
		after, _, _ := blackcatProgress(t, c, a)
		report("口径B 桌面对话事件链", before, after)
		cur = after
	}

	// 口径 C：**真实**桌面域对话 + 用服务端返回的真实 requestId 构造对话链。
	//
	// 与口径 B 的区别是"真的有没有这次对话"：口径 B 的 conv/req 是自造的，
	// 若上游按 requestId 反查真实会话，自造 id 就不计分。
	if cur < tgt {
		before, _, _ := blackcatProgress(t, c, a)
		conv, req, err := c.ChatWithModel(a, "glm-5.2", "1+1等于几？直接回答。", "")
		if err != nil {
			t.Logf("  口径 C 真实对话失败: %v", err)
		} else {
			t.Logf("  口径 C 拿到服务端 requestId 尾部=%s", req[len(req)-8:])
			events := upstream.ChatSequence(conv, req, "msg-"+req[len(req)-8:], "glm-5.2", "GLM-5.2")
			if err := c.ReportDesktopEvents(a, events...); err != nil {
				t.Logf("  口径 C 上报失败: %v", err)
			}
			waitScoring()
			after, _, _ := blackcatProgress(t, c, a)
			report("口径C 真实对话 + 真实 requestId 对话链", before, after)
			cur = after
		}
	}

	t.Logf("诊断结束，black_cat 最终进度 %d/%d", cur, tgt)
}

// waitScoring 等待服务端异步计分落定（实测约 5–8 秒）。
func waitScoring() { time.Sleep(8 * time.Second) }

// itoaMs 毫秒时间戳字符串。
func itoaMs() string {
	return time.Now().Format("150405.000")
}

// drainSSE 读干 SSE 流（上限 1MB，避免异常流无限增长）。
func drainSSE(rc interface{ Read([]byte) (int, error) }) {
	buf := make([]byte, 8192)
	total := 0
	for total < 1<<20 {
		n, err := rc.Read(buf)
		total += n
		if err != nil {
			return
		}
	}
}
