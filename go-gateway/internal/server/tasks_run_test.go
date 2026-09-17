package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/scheduler"
)

// ---------------------------------------------------------------------------
// POST /tasks/run 手动触发养号任务
//
// 为什么要有这个入口：活跃上报 / 夜猫子 / 开学季 / trial 这 4 个任务此前只有
// 「按点自动跑」，用户在界面上既看不到执行结果，也没法在改完配置后立刻验证。
//
// 契约要点：
//   - 200 + ran=false 表示「被前置条件挡下」（如夜猫子不在时段内）——
//     正常状态而非错误，界面据此给人话说明
//   - 未知任务名 → 400（点了没反应必须能看见原因）
//   - 重入 → 200 + skip=already_running（与宿主 checkin_all 同一语义）
// ---------------------------------------------------------------------------

// tasksHandler 构建带 RunTask 回调的 handler。
func tasksHandler(t *testing.T, run func(string) (scheduler.TaskRunResult, error)) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		RunTask:  run,
	})
}

func postTask(t *testing.T, h *Handler, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/tasks/run", strings.NewReader(body)))
	var parsed map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return rec, parsed
}

// TestTasksRunDispatchesNamedTask 正常路径：任务名透传到调度器，结果原样回给宿主。
func TestTasksRunDispatchesNamedTask(t *testing.T) {
	var got string
	h := tasksHandler(t, func(name string) (scheduler.TaskRunResult, error) {
		got = name
		return scheduler.TaskRunResult{Task: name, Ran: true, Message: "已触发一轮"}, nil
	})

	rec, body := postTask(t, h, `{"task":"activity"}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got != "activity" {
		t.Errorf("调度器收到任务名 %q want activity", got)
	}
	if body["ran"] != true {
		t.Errorf("ran=%v want true", body["ran"])
	}
	if body["message"] == "" {
		t.Error("必须带面向用户的中文说明")
	}
}

// TestTasksRunAcceptsQueryParam 任务名也可走 query（便于 curl 手测）。
func TestTasksRunAcceptsQueryParam(t *testing.T) {
	var got string
	h := tasksHandler(t, func(name string) (scheduler.TaskRunResult, error) {
		got = name
		return scheduler.TaskRunResult{Task: name, Ran: true}, nil
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/tasks/run?task=nightowl", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got != "nightowl" {
		t.Errorf("调度器收到任务名 %q want nightowl", got)
	}
}

// TestTasksRunSkipIsSuccess 被前置条件挡下时是 200 + ran=false + skip，
// 不是错误码 —— 界面要能区分「没跑成」与「调用失败」。
func TestTasksRunSkipIsSuccess(t *testing.T) {
	h := tasksHandler(t, func(name string) (scheduler.TaskRunResult, error) {
		return scheduler.TaskRunResult{
			Task: name, Ran: false, Skip: "outside_window",
			Message: "当前不在夜猫子时段",
		}, nil
	})

	rec, body := postTask(t, h, `{"task":"nightowl"}`)
	if rec.Code != 200 {
		t.Fatalf("跳过不是错误，应返回 200，实际 code=%d", rec.Code)
	}
	if body["ran"] != false {
		t.Errorf("ran=%v want false", body["ran"])
	}
	if body["skip"] != "outside_window" {
		t.Errorf("skip=%v want outside_window", body["skip"])
	}
}

// TestTasksRunUnknownTaskIsBadRequest 未知任务名必须 400：界面点了没反应时，
// 唯一的线索就是这个错误。
func TestTasksRunUnknownTaskIsBadRequest(t *testing.T) {
	h := tasksHandler(t, func(name string) (scheduler.TaskRunResult, error) {
		return scheduler.TaskRunResult{}, errors.New(`unknown task "bogus"`)
	})

	rec, body := postTask(t, h, `{"task":"bogus"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || !strings.Contains(errObj["message"].(string), "bogus") {
		t.Errorf("错误体应说明是哪个任务名不认识: %v", body)
	}
}

// TestTasksRunMissingTaskName 缺任务名 → 400（而不是静默跑了个空任务）。
func TestTasksRunMissingTaskName(t *testing.T) {
	h := tasksHandler(t, func(name string) (scheduler.TaskRunResult, error) {
		t.Errorf("缺任务名时不应调用调度器，实际收到 %q", name)
		return scheduler.TaskRunResult{}, nil
	})

	rec, _ := postTask(t, h, `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
}

// TestTasksRunReentryIsNotAnError 重入返回 200 + already_running：
// 与宿主 checkin_all 的 already_running 同一语义（不是故障，是防重入）。
func TestTasksRunReentryIsNotAnError(t *testing.T) {
	h := tasksHandler(t, func(name string) (scheduler.TaskRunResult, error) {
		return scheduler.TaskRunResult{
			Task: name, Skip: "already_running", Message: "该任务正在执行中，请稍后再试",
		}, scheduler.ErrTaskRunning
	})

	rec, body := postTask(t, h, `{"task":"activity"}`)
	if rec.Code != 200 {
		t.Fatalf("重入不是错误，应返回 200，实际 code=%d body=%s", rec.Code, rec.Body)
	}
	if body["skip"] != "already_running" {
		t.Errorf("skip=%v want already_running", body["skip"])
	}
}

// TestTasksRunRequiresAuth 该接口会向上游发真实请求，未鉴权暴露等于给出一个
// 刷账号活跃度的开关，必须与 /status 同用鉴权。
func TestTasksRunRequiresAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		APIKey:   "sk-secret",
		RunTask: func(name string) (scheduler.TaskRunResult, error) {
			t.Error("未鉴权请求不应触达调度器")
			return scheduler.TaskRunResult{}, nil
		},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/tasks/run", strings.NewReader(`{"task":"activity"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
}

// TestTasksRunUnavailableWithoutRunner 未装配调度器时明确报 503，
// 而不是假装成功（宿主会以为任务跑了）。
func TestTasksRunUnavailableWithoutRunner(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})

	rec, _ := postTask(t, h, `{"task":"activity"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503", rec.Code)
	}
}
