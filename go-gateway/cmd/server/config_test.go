package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/prompt"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.Listen != ":7863" {
		t.Errorf("listen=%s", c.Listen)
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.SoftRateDur.Seconds() != 60 {
		t.Errorf("soft=%v", c.SoftRateDur)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9999" || c.APIKey != "k" {
		t.Errorf("c=%+v", c)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("WB2A_LISTEN", ":7777")
	t.Setenv("WB2A_API_KEY", "envkey")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":7777" || c.APIKey != "envkey" {
		t.Errorf("c=%+v", c)
	}
}

func TestBadDuration(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"not-a-duration"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad duration")
	}
}

func TestHardCreditKeyIgnored(t *testing.T) {
	// 退役的 hard_credit 键作为 JSON 未知字段被自然忽略，不报错。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"hard_credit":"not-a-duration","soft_rate":"30s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("hard_credit must be ignored (not validated): %v", err)
	}
	if c.SoftRateDur.Seconds() != 30 {
		t.Errorf("soft_rate=%v want 30s", c.SoftRateDur)
	}
}

func TestNewPoolConfigDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.MaxInFlight != 3 {
		t.Errorf("max_in_flight=%d want 3", c.Pool.MaxInFlight)
	}
	if c.Pool.BreakerThreshold != 3 {
		t.Errorf("breaker_threshold=%d want 3", c.Pool.BreakerThreshold)
	}
	if c.BreakerCooldownDur.Minutes() != 30 {
		t.Errorf("breaker_cooldown=%v want 30m", c.BreakerCooldownDur)
	}
	if c.BreakerCooldownMaxD.Hours() != 6 {
		t.Errorf("breaker_cooldown_max=%v want 6h", c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.5 || c.Pool.IdleWeightMax != 5.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if !c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want true")
	}
	if c.SessionTTL.Minutes() != 30 || c.SessionGCInterval.Minutes() != 5 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		t.Errorf("upstash default should be empty: %+v", c.Upstash)
	}
}

func TestPoolConfigParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{
		"upstash":{"url":"https://foo.upstash.io","token":"tok"},
		"pool":{
			"max_in_flight":5,
			"breaker_threshold":4,
			"breaker_cooldown":"10m",
			"breaker_cooldown_max":"2h",
			"idle_weight_per_hour":0.7,
			"idle_weight_max":8.0
		},
		"session_sticky":{"enabled":false,"ttl":"1h","gc_interval":"2m"}
	}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstash.URL != "https://foo.upstash.io" || c.Upstash.Token != "tok" {
		t.Errorf("upstash=%+v", c.Upstash)
	}
	if c.Pool.MaxInFlight != 5 || c.Pool.BreakerThreshold != 4 {
		t.Errorf("pool=%+v", c.Pool)
	}
	if c.BreakerCooldownDur.Minutes() != 10 || c.BreakerCooldownMaxD.Hours() != 2 {
		t.Errorf("breaker durations=%v/%v", c.BreakerCooldownDur, c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.7 || c.Pool.IdleWeightMax != 8.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want false from file")
	}
	if c.SessionTTL.Hours() != 1 || c.SessionGCInterval.Minutes() != 2 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
}

func TestBadBreakerCooldown(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"breaker_cooldown":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad breaker_cooldown")
	}
}

func TestUpstreamTimeoutDefaults(t *testing.T) {
	// 默认：header 回落 timeout，idle 回落 300。
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Upstream.TimeoutSeconds != 120 {
		t.Errorf("timeout_seconds=%d want 120", c.Upstream.TimeoutSeconds)
	}
	if c.Upstream.HeaderTimeoutSeconds != 120 {
		t.Errorf("header_timeout_seconds=%d want fallback 120", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamHeaderFallsBackToTimeout(t *testing.T) {
	// 只设 timeout_seconds：header 回落同值，idle 回落 300。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":60}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 60 {
		t.Errorf("header_timeout_seconds=%d want fallback 60", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamExplicitHeaderIdle(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":120,"header_timeout_seconds":30,"idle_timeout_seconds":600}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 30 {
		t.Errorf("header_timeout_seconds=%d want 30", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 600 {
		t.Errorf("idle_timeout_seconds=%d want 600", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamEnvOverride(t *testing.T) {
	t.Setenv("WB2A_HEADER_TIMEOUT_SECONDS", "45")
	t.Setenv("WB2A_IDLE_TIMEOUT_SECONDS", "900")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 45 {
		t.Errorf("header_timeout_seconds=%d want env 45", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 900 {
		t.Errorf("idle_timeout_seconds=%d want env 900", c.Upstream.IdleTimeoutSeconds)
	}
}

// TestRetiredTravelIntervalKeyIgnored 退役的 travel_interval_minutes 键按未知字段忽略，不报错。
func TestRetiredTravelIntervalKeyIgnored(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_interval_minutes":15,"checkin_hours":[9]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("retired key should not fail load: %v", err)
	}
	if len(c.Schedule.CheckinHours) != 1 || c.Schedule.CheckinHours[0] != 9 {
		t.Errorf("checkin_hours=%v want [9]（同段其余键照常生效）", c.Schedule.CheckinHours)
	}
}

// TestScheduleEnabledByDefault 两个任务的 enabled 开关默认均为 true：
// 老 config 不写这两个键，行为必须与从前完全一致（照常 9/21 签到、22 保活）。
func TestScheduleEnabledByDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("enabled defaults want true/true, got %v/%v",
			c.Schedule.CheckinEnabled, c.Schedule.KeepaliveEnabled)
	}
}

// TestScheduleLegacyConfigKeepsRunning 老 config（只写小时数组）加载后仍是启用态。
func TestScheduleLegacyConfigKeepsRunning(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_hours":[9,21],"keepalive_hours":[22]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("legacy config must stay enabled: %+v", c.Schedule)
	}
	if len(c.Schedule.CheckinHours) != 2 {
		t.Errorf("checkin_hours=%v", c.Schedule.CheckinHours)
	}
}

// TestScheduleExplicitDisable 显式 checkin_enabled=false 即可真正关掉签到
// （issue #27 边界：此前无论怎么配小时都关不掉）。
func TestScheduleExplicitDisable(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"keepalive_enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CheckinEnabled || c.Schedule.KeepaliveEnabled {
		t.Errorf("want both disabled: %+v", c.Schedule)
	}
	// 小时数组仍回落默认值（禁用与默认值互不干扰：重新启用无需补配小时）。
	if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 9 || c.Schedule.CheckinHours[1] != 21 {
		t.Errorf("checkin_hours=%v want default [9 21] even when disabled", c.Schedule.CheckinHours)
	}
	if len(c.Schedule.KeepaliveHours) != 1 || c.Schedule.KeepaliveHours[0] != 22 {
		t.Errorf("keepalive_hours=%v want default [22] even when disabled", c.Schedule.KeepaliveHours)
	}
}

// TestCheckinScopeDefaultsToCN 缺省（含老 config）签到范围只做国服：国际版接口无数据。
func TestCheckinScopeDefaultsToCN(t *testing.T) {
	for name, body := range map[string]string{
		"缺省":     `{"schedule":{"checkin_hours":[9,21]}}`,
		"空串":     `{"schedule":{"checkin_scope":""}}`,
		"未知取值":   `{"schedule":{"checkin_scope":"intl"}}`,
		"大小写不敏感": `{"schedule":{"checkin_scope":"ALL"}}`,
	} {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(body), 0o600)
		c, err := Load(fp)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := "cn"
		if name == "大小写不敏感" {
			want = "all"
		}
		if c.Schedule.CheckinScope != want {
			t.Errorf("%s: checkin_scope=%q want %q", name, c.Schedule.CheckinScope, want)
		}
	}
}

// TestCheckinScopeAllows 区域范围过滤：默认跳过国际版账号，all 时放行。
func TestCheckinScopeAllows(t *testing.T) {
	cn := &auth.Auth{UID: "cn", Domain: "www.workbuddy.cn"}
	intl := &auth.Auth{UID: "intl", Domain: "www.workbuddy.ai"}
	legacy := &auth.Auth{UID: "legacy"}

	def := Default()
	if !def.checkinScopeAllows(cn) || !def.checkinScopeAllows(legacy) {
		t.Error("默认范围应覆盖国服与无 domain 的历史账号")
	}
	if def.checkinScopeAllows(intl) {
		t.Error("默认范围必须跳过国际版账号")
	}

	def.Schedule.CheckinScope = "all"
	if !def.checkinScopeAllows(intl) {
		t.Error("checkin_scope=all 时应覆盖国际版账号")
	}
}

// TestScheduleDisableKeepsExplicitHours 禁用不擦除用户配置的小时（便于原样恢复）。
func TestScheduleDisableKeepsExplicitHours(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"checkin_hours":[10,14]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CheckinEnabled {
		t.Error("checkin should be disabled")
	}
	if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 10 || c.Schedule.CheckinHours[1] != 14 {
		t.Errorf("explicit hours must be preserved: %v", c.Schedule.CheckinHours)
	}
}

// TestScheduleEmptyHoursFallsBackToDefault 空数组 / null / 缺省都视同「未配置」→ 回落默认。
func TestScheduleEmptyHoursFallsBackToDefault(t *testing.T) {
	cases := map[string]string{
		"absent":   `{}`,
		"empty":    `{"schedule":{}}`,
		"null":     `{"schedule":{"checkin_hours":null,"keepalive_hours":null}}`,
		"emptyarr": `{"schedule":{"checkin_hours":[],"keepalive_hours":[]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			fp := filepath.Join(dir, "c.json")
			os.WriteFile(fp, []byte(body), 0o600)
			c, err := Load(fp)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 9 || c.Schedule.CheckinHours[1] != 21 {
				t.Errorf("checkin_hours=%v want default [9 21]", c.Schedule.CheckinHours)
			}
			if len(c.Schedule.KeepaliveHours) != 1 || c.Schedule.KeepaliveHours[0] != 22 {
				t.Errorf("keepalive_hours=%v want default [22]", c.Schedule.KeepaliveHours)
			}
			if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
				t.Errorf("empty hours must not imply disabled: %+v", c.Schedule)
			}
		})
	}
}

// TestScheduleInvalidHourRejected 非法小时快速失败：指向正确的禁用开关，避免用户
// 猜测哨兵值（[-1] 之类）被静默当成"改到别的整点"。
func TestScheduleInvalidHourRejected(t *testing.T) {
	cases := []struct{ body, wantSwitch string }{
		{`{"schedule":{"checkin_hours":[25]}}`, "checkin_enabled"},
		{`{"schedule":{"checkin_hours":[-1]}}`, "checkin_enabled"},
		{`{"schedule":{"keepalive_hours":[-1]}}`, "keepalive_enabled"},
		// 界面现在可自由填这 4 个任务的时点，非法值同样必须快速失败
		{`{"schedule":{"activity_hours":[24]}}`, "activity_enabled"},
		{`{"schedule":{"nightowl_hours":[-1]}}`, "nightowl_enabled"},
		{`{"schedule":{"school_hours":[99]}}`, "school_enabled"},
		{`{"schedule":{"trial_hours":[-3]}}`, "trial_enabled"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(tc.body), 0o600)
		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for %s", tc.body)
		}
		if !strings.Contains(err.Error(), tc.wantSwitch) {
			t.Errorf("error for %s should point at schedule.%s: %v", tc.body, tc.wantSwitch, err)
		}
	}
}

// TestScheduleNewTaskDefaults 4 个养号任务的缺省值：时刻与开关都要有默认。
//
// 为什么默认值也要测：宿主（界面）读不到这些字段时会自己填一份默认，
// 两边一旦不一致，用户「不改任何东西直接保存」就会把网关配置改成另一套排程。
func TestScheduleNewTaskDefaults(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		hours []int
		want  []int
	}{
		{"activity_hours", c.Schedule.ActivityHours, []int{10}},
		{"nightowl_hours", c.Schedule.NightOwlHours, []int{1}},
		{"school_hours", c.Schedule.SchoolHours, []int{12}},
		{"trial_hours", c.Schedule.TrialHours, []int{9, 21}},
	} {
		if len(tc.hours) != len(tc.want) {
			t.Errorf("%s=%v want %v", tc.name, tc.hours, tc.want)
			continue
		}
		for i := range tc.want {
			if tc.hours[i] != tc.want[i] {
				t.Errorf("%s=%v want %v", tc.name, tc.hours, tc.want)
				break
			}
		}
	}
	if !c.Schedule.ActivityEnabled || !c.Schedule.NightOwlEnabled ||
		!c.Schedule.SchoolEnabled || !c.Schedule.TrialEnabled {
		t.Errorf("4 个任务的开关缺省都应为 true: %+v", c.Schedule)
	}
	if c.Schedule.ActivityReportCount != 3 {
		t.Errorf("activity_report_count=%d want 3", c.Schedule.ActivityReportCount)
	}
}

// TestScheduleNewTaskEmptyHoursFallsBackToDefault 单独把某个新任务的 hours 配成
// 空数组 / null 时，也应回落默认值。
//
// 回归保护：这几行此前被误写在 `if len(ActivityHours)==0` 的块里，
// 于是只有活跃上报为空时才顺带赋值 —— 单独清空 nightowl_hours 会得到空排程，
// 而 nextFire 对空数组返回零时间，任务被**静默关掉**（与「未配置→回落默认」相反）。
func TestScheduleNewTaskEmptyHoursFallsBackToDefault(t *testing.T) {
	cases := map[string]string{
		"emptyarr": `{"schedule":{"nightowl_hours":[],"school_hours":[],"trial_hours":[]}}`,
		"null":     `{"schedule":{"nightowl_hours":null,"school_hours":null,"trial_hours":null}}`,
		"显式活动时点":   `{"schedule":{"activity_hours":[7],"nightowl_hours":[],"school_hours":null,"trial_hours":[]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			fp := filepath.Join(dir, "c.json")
			os.WriteFile(fp, []byte(body), 0o600)
			c, err := Load(fp)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Schedule.NightOwlHours) != 1 || c.Schedule.NightOwlHours[0] != 1 {
				t.Errorf("nightowl_hours=%v want default [1]", c.Schedule.NightOwlHours)
			}
			if len(c.Schedule.SchoolHours) != 1 || c.Schedule.SchoolHours[0] != 12 {
				t.Errorf("school_hours=%v want default [12]", c.Schedule.SchoolHours)
			}
			if len(c.Schedule.TrialHours) != 2 || c.Schedule.TrialHours[0] != 9 || c.Schedule.TrialHours[1] != 21 {
				t.Errorf("trial_hours=%v want default [9 21]", c.Schedule.TrialHours)
			}
		})
	}
}

// TestScheduleNewTaskExplicitHoursAndDisable 显式时点与显式禁用都要被尊重
// （与签到同样的语义：禁用不擦除用户配的小时，便于原样恢复）。
func TestScheduleNewTaskExplicitHoursAndDisable(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"nightowl_hours":[2,3],"school_hours":[15],`+
		`"trial_hours":[8],"activity_hours":[11],"activity_report_count":5,`+
		`"school_enabled":false,"trial_enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Schedule.NightOwlHours) != 2 || c.Schedule.NightOwlHours[0] != 2 || c.Schedule.NightOwlHours[1] != 3 {
		t.Errorf("nightowl_hours=%v want [2 3]", c.Schedule.NightOwlHours)
	}
	if len(c.Schedule.SchoolHours) != 1 || c.Schedule.SchoolHours[0] != 15 {
		t.Errorf("school_hours=%v want [15]", c.Schedule.SchoolHours)
	}
	if len(c.Schedule.TrialHours) != 1 || c.Schedule.TrialHours[0] != 8 {
		t.Errorf("trial_hours=%v want [8]", c.Schedule.TrialHours)
	}
	if c.Schedule.ActivityReportCount != 5 {
		t.Errorf("activity_report_count=%d want 5", c.Schedule.ActivityReportCount)
	}
	if c.Schedule.SchoolEnabled || c.Schedule.TrialEnabled {
		t.Errorf("显式 false 应生效: %+v", c.Schedule)
	}
	if !c.Schedule.ActivityEnabled || !c.Schedule.NightOwlEnabled {
		t.Errorf("未显式关闭的开关应保持 true: %+v", c.Schedule)
	}
}

func TestBadSessionTTL(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"session_sticky":{"ttl":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad session_sticky.ttl")
	}
}

// ---------------------------------------------------------------------------
// prompt 块（自定义系统提示词覆盖内置默认）
// ---------------------------------------------------------------------------

// TestPromptDefaultPassthrough 缺省 passthrough 且不加载文本。
//
// 老配置里没有 prompt 键，必须保持既有行为（透传客户端原始 system）。
func TestPromptDefaultPassthrough(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("prompt.mode 缺省应为 passthrough，实际 %q", c.Prompt.Mode)
	}
	if c.PromptText != "" {
		t.Errorf("passthrough 不应加载 PromptText，实际 len=%d", len(c.PromptText))
	}
}

// TestPromptLegacyConfigStaysPassthrough 老配置（无 prompt 键）行为不变。
func TestPromptLegacyConfigStaysPassthrough(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","features":{"sanitize_blacklist_fingerprints":true}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" || c.PromptText != "" {
		t.Errorf("老配置应缺省 passthrough 且无文本，实际 mode=%q textLen=%d", c.Prompt.Mode, len(c.PromptText))
	}
}

// TestPromptCustomWithoutFileUsesBuiltin custom + 空 file → 内置默认。
func TestPromptCustomWithoutFileUsesBuiltin(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"custom"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "custom" {
		t.Errorf("mode=%q want custom", c.Prompt.Mode)
	}
	if c.PromptText != prompt.Default() {
		t.Errorf("未配 file 时应使用内置默认提示词（len=%d vs %d）", len(c.PromptText), len(prompt.Default()))
	}
	if strings.TrimSpace(c.PromptText) == "" {
		t.Error("内置默认提示词为空")
	}
}

// TestPromptCustomFileOverrides 文件内容覆盖内置默认。
func TestPromptCustomFileOverrides(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(dir, "my-prompt.md")
	want := "你是一名工程助手，只说必要的。"
	os.WriteFile(pf, []byte(want), 0o600)
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"custom","file":`+strconv.Quote(pf)+`}}`), 0o600)

	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.PromptText != want {
		t.Errorf("PromptText=%q want %q", c.PromptText, want)
	}
}

// TestPromptCustomMissingFileFailsFast 显式配了 file 却读不到 → 启动报错。
func TestPromptCustomMissingFileFailsFast(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.md")
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"custom","file":`+strconv.Quote(missing)+`}}`), 0o600)

	if _, err := Load(fp); err == nil {
		t.Fatal("custom 模式下文件不存在应报错（fail fast）")
	} else if !strings.Contains(err.Error(), "prompt.file") {
		t.Errorf("错误信息应指明 prompt.file，实际: %v", err)
	}
}

// TestPromptPassthroughIgnoresBadFile passthrough 下 file 写错不拦启动。
//
// fail fast 的范围只限 custom：passthrough 根本不读这段文本，
// 为不生效的配置拦住服务启动没有意义。
func TestPromptPassthroughIgnoresBadFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"passthrough","file":"D:\\no\\such\\file.md"}}`), 0o600)

	c, err := Load(fp)
	if err != nil {
		t.Fatalf("passthrough 不应因 file 无效而报错: %v", err)
	}
	if c.PromptText != "" {
		t.Errorf("passthrough 下 PromptText 应保持空，实际 len=%d", len(c.PromptText))
	}
}

// TestPromptModeInvalidFailsFast 非法 mode 必须报错（不静默回落）。
//
// 拼错 "costom" 时若静默按 passthrough 跑，现象是「配了定制提示词却没生效」。
func TestPromptModeInvalidFailsFast(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"costom"}}`), 0o600)

	if _, err := Load(fp); err == nil {
		t.Fatal("非法 prompt.mode 应报错")
	} else if !strings.Contains(err.Error(), "prompt.mode") {
		t.Errorf("错误信息应指明 prompt.mode，实际: %v", err)
	}
}

// TestPromptModeNormalization 大小写 / 首尾空白归一，空串回落 passthrough。
func TestPromptModeNormalization(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"CUSTOM", "custom"},
		{"  custom  ", "custom"},
		{"Passthrough", "passthrough"},
		{"", "passthrough"},
		{"   ", "passthrough"},
	} {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(`{"prompt":{"mode":`+strconv.Quote(tc.in)+`}}`), 0o600)
		c, err := Load(fp)
		if err != nil {
			t.Fatalf("mode=%q: %v", tc.in, err)
		}
		if c.Prompt.Mode != tc.want {
			t.Errorf("mode=%q 归一为 %q，期望 %q", tc.in, c.Prompt.Mode, tc.want)
		}
	}
}

// TestPromptEnvOverride env 覆盖 prompt.mode 与 prompt.file。
func TestPromptEnvOverride(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(dir, "env-prompt.md")
	want := "env 指定的提示词"
	os.WriteFile(pf, []byte(want), 0o600)

	t.Setenv("WB2A_PROMPT_MODE", "custom")
	t.Setenv("WB2A_PROMPT_FILE", pf)

	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "custom" {
		t.Errorf("env 应覆盖 mode，实际 %q", c.Prompt.Mode)
	}
	if c.PromptText != want {
		t.Errorf("env 指定的文件内容应被加载，实际 %q", c.PromptText)
	}
}

// TestPromptEnvFileOverridesJSONFile env 的 file 优先于 JSON 里的 file。
func TestPromptEnvFileOverridesJSONFile(t *testing.T) {
	dir := t.TempDir()
	jsonPrompt := filepath.Join(dir, "json.md")
	envPrompt := filepath.Join(dir, "env.md")
	os.WriteFile(jsonPrompt, []byte("来自 JSON"), 0o600)
	os.WriteFile(envPrompt, []byte("来自 ENV"), 0o600)

	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"custom","file":`+strconv.Quote(jsonPrompt)+`}}`), 0o600)

	t.Setenv("WB2A_PROMPT_FILE", envPrompt)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.PromptText != "来自 ENV" {
		t.Errorf("env 应覆盖 JSON 的 file，实际 %q", c.PromptText)
	}
}

// TestPromptEnvModeInvalidFailsFast env 传入非法 mode 同样报错。
func TestPromptEnvModeInvalidFailsFast(t *testing.T) {
	t.Setenv("WB2A_PROMPT_MODE", "bogus")
	if _, err := Load(""); err == nil {
		t.Fatal("env 传入非法 prompt.mode 应报错")
	}
}
