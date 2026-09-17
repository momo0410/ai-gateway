//! 常量、路径与通用工具函数（对照 server.py 常量区与工具区）

use chrono::{Local, TimeZone};
use serde_json::{json, Value};
use std::collections::{HashMap, HashSet};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Mutex, OnceLock};
use std::time::{SystemTime, UNIX_EPOCH};

// ---------------------------------------------------------------------------
// 常量
// ---------------------------------------------------------------------------

/// 国服 API 基址。
pub const WORKBUDDY_API_ENDPOINT: &str = "https://www.codebuddy.cn";

/// 国际版基址。
///
/// 与国服不同，国际版的 Web 与 API（chat / billing / 签到 / 旅行 / 刷新）
/// 全部在同一域名下，因此只需一个常量。
pub const WORKBUDDY_API_ENDPOINT_INTL: &str = "https://www.workbuddy.ai";

/// 服务区域。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Region {
    /// 国服（codebuddy.cn / workbuddy.cn）
    Cn,
    /// 国际版（workbuddy.ai / codebuddy.ai）
    Intl,
}

impl Region {
    pub const ALL: [Region; 2] = [Region::Cn, Region::Intl];

    /// 由账号的 domain 字段判断区域；后缀 .ai 视为国际版。
    /// domain 缺失时按国服处理（历史上只存在国服账号）。
    pub fn from_domain(domain: &str) -> Region {
        if domain.trim().to_ascii_lowercase().ends_with(".ai") {
            Region::Intl
        } else {
            Region::Cn
        }
    }

    /// 由账号记录判断区域。
    pub fn of(account: &serde_json::Value) -> Region {
        Region::from_domain(account.get("domain").and_then(|v| v.as_str()).unwrap_or(""))
    }

    /// 由界面传入的区域键解析区域；未知值按国服处理（保持历史默认）。
    pub fn from_key(key: &str) -> Region {
        if key.trim().eq_ignore_ascii_case("intl") {
            Region::Intl
        } else {
            Region::Cn
        }
    }

    /// 界面与命令层使用的区域键。
    pub fn key(self) -> &'static str {
        match self {
            Region::Cn => "cn",
            Region::Intl => "intl",
        }
    }

    /// 该区域的 API 基址。
    pub fn api_endpoint(self) -> &'static str {
        match self {
            Region::Cn => WORKBUDDY_API_ENDPOINT,
            Region::Intl => WORKBUDDY_API_ENDPOINT_INTL,
        }
    }

    /// OAuth 登录使用的平台标识。
    ///
    /// 两个区域签发 state 的是同一套 `/v2/plugin/auth/state`，但平台标识不同：
    /// 国际版登录页把 `workbuddy-ai` 判定为插件平台（`WORKBUDDYAI`），沿用国服的
    /// `workbuddy` 会落到 Web 分支，取不到插件 token 缓存。
    ///
    /// 实测（2026-09，`platform=workbuddy-ai`）：国际版走 Keycloak realm 的
    /// Google / GitHub / X 联合登录（该区域**没有扫码**），授权后
    /// `/v2/plugin/auth/token` 正常返回 accessToken / refreshToken，
    /// `login/account` 也能取到 uid。
    pub fn oauth_platform(self) -> &'static str {
        match self {
            Region::Cn => WORKBUDDY_PLATFORM,
            Region::Intl => WORKBUDDY_PLATFORM_INTL,
        }
    }

    /// 界面展示用的区域名。
    pub fn label(self) -> &'static str {
        match self {
            Region::Cn => "国服",
            Region::Intl => "国际版",
        }
    }

    /// 该区域账号凭证里 `domain` 字段的默认值。
    ///
    /// 客户端写认证文件时一定带 domain，但**本工具自己产出的备份**与历史记录
    /// 可能缺失该字段；缺失时按来源区域补齐，`Region::from_domain` 能原样还原。
    pub fn auth_domain(self) -> &'static str {
        match self {
            Region::Cn => "www.workbuddy.cn",
            Region::Intl => "www.workbuddy.ai",
        }
    }
}

/// 按账号区域返回 API 基址（供各模块拼接端点）。
pub fn api_endpoint_for(account: &serde_json::Value) -> &'static str {
    Region::of(account).api_endpoint()
}

// ---------------------------------------------------------------------------
// 自动签到 / 自动旅行：国服专属
// ---------------------------------------------------------------------------
//
// 历史说明：早期版本曾在配置里提供 `region_scope` 字段（`"cn" | "all"`），
// 允许用户选择「仅国服」还是「国服 + 国际版」。由于上游国际版（workbuddy.ai）
// 的签到接口 (`checkin-activity-status`) 与猫猫旅行接口 (`travel/status`)
// 至今不返回真实数据，对国际版账号执行只会产生无意义的失败日志与请求，
// 该字段已在前端 UI 中移除，后端也不再读取。
//
// 现在自动签到 / 自动旅行**硬绑定**为「仅国服」。任何历史配置文件里残留的
// `region_scope` 字段都将在加载时被丢弃，写出时也不会再写回。

/// 判断账号是否为自动签到 / 自动旅行的覆盖目标：仅国服账号。
pub fn account_supported_by_auto_tasks(account: &Value) -> bool {
    Region::of(account) == Region::Cn
}

pub const WORKBUDDY_API_PREFIX: &str = "/v2/plugin";
pub const WORKBUDDY_PLATFORM: &str = "workbuddy";

/// 国际版平台标识。
///
/// 国际版登录页把 `workbuddy-ai` 判定为插件平台（见其前端平台枚举
/// `WORKBUDDYAI="workbuddy-ai"`），插件 token 缓存接口
/// `/v2/plugin/auth/token` 在两个区域是同一套。
pub const WORKBUDDY_PLATFORM_INTL: &str = "workbuddy-ai";

pub const OAUTH_TIMEOUT_SECONDS: i64 = 600;

pub const CHECKIN_API_PREFIX: &str = "/v2/billing/meter";
pub const CHECKIN_LOG_KEEP_DAYS: i64 = 30;
pub const CHECKIN_LOG_MAX_RECORDS: usize = 500;

// ---------------------------------------------------------------------------
// 记录保留天数（设置页可配置）
//
// 为什么需要可配置：签到日志、积分快照、任务记录都属于「本地观察数据」，
// 保留多久取决于用户的硬盘与排查需求 —— 有人要长期回溯，有人只想留一周。
// 硬编码常量会让「改一下」变成「改代码重编译」，因此抽成设置项。
//
// 与既有常量的关系：CHECKIN_LOG_KEEP_DAYS 等常量仍是**默认值**，
// 用户没配时行为与改动前完全一致（零迁移成本）。
// ---------------------------------------------------------------------------

/// 设置项键名（app_settings.json 里的一项）。
pub const RECORD_RETENTION_KEY: &str = "recordRetentionDays";

/// 默认保留天数。所有者要求 60 天（此前积分快照硬编码 90 天）。
pub const RECORD_RETENTION_DEFAULT_DAYS: i64 = 60;

/// 允许的取值范围。
///
/// 下限 1 天：0 或负数会让所有记录立即被清理，等于静默关闭记录功能，
/// 用户很难意识到自己「什么都没存」；真要关闭应显式提供开关而不是填 0。
/// 上限 3650 天（约 10 年）：防止误填极大值导致清理逻辑溢出或文件无限增长。
pub const RECORD_RETENTION_MIN_DAYS: i64 = 1;
pub const RECORD_RETENTION_MAX_DAYS: i64 = 3650;

/// 设置页提供的预设选项（值, 显示文案）。
///
/// 只列常用档位而非任意输入：保留天数是「粗粒度」选择，
/// 下拉/勾选比手填更不容易填错（所有者明确偏好勾选而非手写）。
pub const RECORD_RETENTION_PRESETS: &[(i64, &str)] = &[
    (7, "7 天"),
    (30, "30 天"),
    (60, "60 天（默认）"),
    (90, "90 天"),
    (180, "180 天"),
    (365, "1 年"),
];

/// 归一化保留天数：非法值回落到默认值，并夹到允许区间。
///
/// 单独抽出来是因为「读取配置」有多个入口（统计、清理、UI 展示），
/// 每处各自校验容易出现口径不一致。
pub fn normalize_retention_days(days: i64) -> i64 {
    if days <= 0 {
        return RECORD_RETENTION_DEFAULT_DAYS;
    }
    days.clamp(RECORD_RETENTION_MIN_DAYS, RECORD_RETENTION_MAX_DAYS)
}

/// 读取「记录保留天数」设置（缺省或非法时返回默认值）。
pub fn record_retention_days() -> i64 {
    let raw = load_app_settings()
        .get(RECORD_RETENTION_KEY)
        .and_then(Value::as_i64)
        .unwrap_or(RECORD_RETENTION_DEFAULT_DAYS);
    normalize_retention_days(raw)
}

/// 写入「记录保留天数」设置，返回归一化后的实际生效值。
///
/// 返回归一化值而非原样回显：让调用方（与前端）立刻看到真实生效的数字，
/// 避免「填了 0 但界面显示 0、实际按 60 天清理」这类不一致。
pub fn set_record_retention_days(days: i64) -> std::io::Result<i64> {
    let normalized = normalize_retention_days(days);
    save_app_settings(&serde_json::json!({ RECORD_RETENTION_KEY: normalized }))?;
    Ok(normalized)
}

/// 派猫猫旅行接口前缀（成长中心，非 /v2/plugin 体系，直接挂在 API 域名下）。
pub const TRAVEL_API_PREFIX: &str = "/activity/growth/buddy/travel";

static CHECKIN_LOG_WRITE_LOCK: Mutex<()> = Mutex::new(());
static TRAVEL_CACHE_WRITE_LOCK: Mutex<()> = Mutex::new(());

/// Serialize travel-cache read-modify-write across depart and claim cycles.
pub fn with_travel_cache_lock<T>(f: impl FnOnce() -> T) -> T {
    let _guard = TRAVEL_CACHE_WRITE_LOCK.lock().unwrap();
    f()
}

pub const ROTATE_LOG_MAX_RECORDS: usize = 200;

/// 官网套餐页桌面 Chrome UA（plans-usage 捕获）。
pub const DEFAULT_HTTP_USER_AGENT: &str = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36";

// ---------------------------------------------------------------------------
// 路径
// ---------------------------------------------------------------------------

pub fn home_dir() -> PathBuf {
    dirs::home_dir().unwrap_or_else(|| PathBuf::from("."))
}

/// 本软件自身的数据目录（`~/.wb-switch`）。
///
/// 可用环境变量 `AI_GATEWAY_HOME` 覆盖到任意目录，用于**开发/测试隔离**：
/// 起一个独立实例、指向空目录，就不会动到正在使用的那份账号库与网关状态。
///
/// 为什么需要这个开关：Windows 上 `dirs::home_dir()` 走 `SHGetKnownFolderPath`，
/// **不读 `USERPROFILE`**（实测：把 USERPROFILE 指到临时目录后，宿主服务仍然读到
/// 真实的 ~/.ai-gateway），因此光靠环境变量没法隔离数据目录。
///
/// ## 为什么目录名是 `.wb-switch` 而不是 `.ai-gateway`（2026-09-16）
///
/// 应用在 1.0.0 更名时把数据目录一并换成了 `~/.ai-gateway`，并为老用户做了
/// 一次性「复制式迁移」。但 2026-09-16 起本仓库与老仓库
/// （workbuddy-switch-gateway，0.8.x 线）**并行维护**，两版会同时装在同一台
/// 机器上使用，于是那个迁移暴露出一个真实的数据缺陷：
///
///   迁移的幂等标记 `.migrated-from-wb-switch` 让迁移**只跑一次**，
///   此后 `.ai-gateway` 就成了迁移那一刻的快照。用户在任一版本里新增的账号
///   都不会出现在另一个版本里 —— 而从用户视角这是**同一个应用的两代**，
///   理当共用同一份账号库。
///
/// 因此数据目录回到 `.wb-switch`（0.8.x 一直在用的那个），两版真正共享同一
/// 份账号库与网关状态。`.ai-gateway` 不再被写入，仅在老用户机器上存在时作为
/// 一次性迁移来源（见 migrate_store.rs）。
///
/// 保留 `AI_GATEWAY_HOME` 变量名不变：它是新版本才引入的，且改名只涉及源码，
/// 改了反而会让既有的隔离脚本失效。
pub fn store_dir() -> PathBuf {
    if let Some(dir) = std::env::var_os("AI_GATEWAY_HOME") {
        let p = PathBuf::from(dir);
        if !p.as_os_str().is_empty() {
            return p;
        }
    }
    home_dir().join(".wb-switch")
}

pub fn accounts_file() -> PathBuf {
    store_dir().join("accounts.json")
}

// ---------------------------------------------------------------------------
// 应用设置（多应用共用的界面偏好与手动路径）
// ---------------------------------------------------------------------------

/// 应用设置文件（`<store_dir>/app_settings.json`）。
///
/// 与 WorkBuddy 的 `auto_*_config.json` 分开：那些是**业务开关**（签到/保活排程），
/// 这里是**界面与路径偏好**（主题、各应用手动 exe 路径、豆包端点等），
/// 生命周期与迁移策略都不同，混在一个文件里会让任一侧的 schema 变更互相牵连。
pub fn app_settings_file() -> PathBuf {
    store_dir().join("app_settings.json")
}

/// 把数据目录隔离到临时目录，供单元测试使用。
///
/// 为什么需要一把全局锁：`AI_GATEWAY_HOME` 是**进程级**环境变量，而 cargo 默认
/// 多线程跑测试。两个测试同时改它，会让其中一个读到另一个的目录 ——
/// 表现为「刚写入的账号查不到」这类看似随机的失败。
/// 所有依赖数据目录的测试都必须先拿这把锁。
#[cfg(test)]
pub mod test_isolation {
    use std::path::PathBuf;
    use std::sync::{Mutex, MutexGuard, OnceLock};

    fn lock() -> &'static Mutex<()> {
        static LOCK: OnceLock<Mutex<()>> = OnceLock::new();
        LOCK.get_or_init(|| Mutex::new(()))
    }

    /// 隔离守卫：持有期间数据目录指向临时目录，释放时清理。
    ///
    /// 用 `MutexGuard` 而非 `Drop` 实现清理：这样即使测试 panic，锁也会释放，
    /// 后续测试不会被永久阻塞（poisoned 锁用 `into_inner` 兜底）。
    pub struct Isolated {
        _guard: MutexGuard<'static, ()>,
        dir: PathBuf,
    }

    impl Isolated {
        pub fn new(tag: &str) -> Self {
            let guard = lock().lock().unwrap_or_else(|e| e.into_inner());
            let dir = std::env::temp_dir().join(format!(
                "ai-gateway-test-{tag}-{}-{}",
                std::process::id(),
                std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .map(|d| d.as_nanos())
                    .unwrap_or(0)
            ));
            let _ = std::fs::remove_dir_all(&dir);
            let _ = std::fs::create_dir_all(&dir);
            std::env::set_var("AI_GATEWAY_HOME", &dir);
            Self { _guard: guard, dir }
        }

        /// 隔离出的数据目录路径。
        pub fn dir(&self) -> &std::path::Path {
            &self.dir
        }
    }

    impl Drop for Isolated {
        fn drop(&mut self) {
            std::env::remove_var("AI_GATEWAY_HOME");
            let _ = std::fs::remove_dir_all(&self.dir);
        }
    }
}


/// 读取应用设置（缺失或损坏返回空对象）。
pub fn load_app_settings() -> Value {    std::fs::read_to_string(app_settings_file())
        .ok()
        .and_then(|text| serde_json::from_str::<Value>(&text).ok())
        .filter(Value::is_object)
        .unwrap_or_else(|| serde_json::json!({}))
}

/// 合并式写入应用设置（只覆盖传入的键，其余保留）。
pub fn save_app_settings(patch: &Value) -> std::io::Result<Value> {
    let mut merged = load_app_settings();
    if let (Some(dst), Some(src)) = (merged.as_object_mut(), patch.as_object()) {
        for (k, v) in src {
            dst.insert(k.clone(), v.clone());
        }
    }
    std::fs::create_dir_all(store_dir())?;
    let content = serde_json::to_string_pretty(&merged).unwrap_or_default();
    atomic_write(&app_settings_file(), &content)?;
    Ok(merged)
}

/// 读单个设置项（字符串）。
pub fn app_setting_str(key: &str) -> Option<String> {
    load_app_settings()
        .get(key)
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(str::to_string)
}

/// 读单个设置项（布尔）。
pub fn app_setting_bool(key: &str, default: bool) -> bool {
    load_app_settings()
        .get(key)
        .and_then(Value::as_bool)
        .unwrap_or(default)
}

/// 写单个设置项。
pub fn set_app_setting(key: &str, value: Value) -> std::io::Result<()> {
    save_app_settings(&serde_json::json!({ key: value })).map(|_| ())
}

pub fn backup_dir() -> PathBuf {
    store_dir().join("backups")
}

pub fn checkin_config_file() -> PathBuf {
    store_dir().join("auto_checkin_config.json")
}

pub fn checkin_logs_file() -> PathBuf {
    store_dir().join("auto_checkin_logs.json")
}

pub fn travel_config_file() -> PathBuf {
    store_dir().join("auto_travel_config.json")
}

pub fn travel_cache_file() -> PathBuf {
    store_dir().join("travel_cache.json")
}

pub fn credit_usage_snapshots_file() -> PathBuf {
    store_dir().join("credit_usage_snapshots.json")
}

pub fn official_usage_cache_file() -> PathBuf {
    store_dir().join("official_usage_cache.json")
}

pub fn auto_rotate_config_file() -> PathBuf {
    store_dir().join("auto_rotate_config.json")
}

pub fn auto_rotate_logs_file() -> PathBuf {
    store_dir().join("auto_rotate_logs.json")
}

/// WorkBuddy 客户端 exe 缓存文件（按区域各一份：国服 / 国际版）。
pub fn workbuddy_exe_cache_file_for(region: Region) -> PathBuf {
    match region {
        Region::Cn => store_dir().join("workbuddy_exe.json"),
        Region::Intl => store_dir().join("workbuddy_ai_exe.json"),
    }
}

fn parse_workbuddy_exe_cache_json(text: &str) -> Option<PathBuf> {
    let v: Value = serde_json::from_str(text).ok()?;
    let exe = v.get("exe")?.as_str()?.trim();
    if exe.is_empty() {
        None
    } else {
        Some(PathBuf::from(exe))
    }
}

/// 读取上次成功解析到的 WorkBuddy 客户端 exe；损坏或空文件视为无缓存。
pub fn load_workbuddy_exe_cache_for(region: Region) -> Option<PathBuf> {
    let f = workbuddy_exe_cache_file_for(region);
    if !f.exists() {
        return None;
    }
    let text = std::fs::read_to_string(&f).ok()?;
    parse_workbuddy_exe_cache_json(&text)
}

/// 记住已存在的客户端 exe，供下次未运行时启动。
pub fn save_workbuddy_exe_cache_for(region: Region, exe: &Path) -> std::io::Result<()> {
    std::fs::create_dir_all(store_dir())?;
    let content =
        serde_json::to_string_pretty(&json!({ "exe": exe.to_string_lossy() })).unwrap_or_default();
    atomic_write(&workbuddy_exe_cache_file_for(region), &content)
}

pub fn clear_workbuddy_exe_cache_for(region: Region) {
    let _ = std::fs::remove_file(workbuddy_exe_cache_file_for(region));
}

pub fn codebuddy_cn_app_cache_file() -> PathBuf {
    store_dir().join("codebuddy_cn_app.json")
}

fn parse_codebuddy_cn_app_cache_json(text: &str) -> Option<PathBuf> {
    parse_workbuddy_exe_cache_json(text)
}

/// 读取上次成功解析到的 CodeBuddy CN 应用路径；损坏或空文件视为无缓存。
pub fn load_codebuddy_cn_app_cache() -> Option<PathBuf> {
    let f = codebuddy_cn_app_cache_file();
    if !f.exists() {
        return None;
    }
    let text = std::fs::read_to_string(&f).ok()?;
    parse_codebuddy_cn_app_cache_json(&text)
}

pub fn save_codebuddy_cn_app_cache(path: &Path) -> std::io::Result<()> {
    std::fs::create_dir_all(store_dir())?;
    let content =
        serde_json::to_string_pretty(&json!({ "exe": path.to_string_lossy() })).unwrap_or_default();
    atomic_write(&codebuddy_cn_app_cache_file(), &content)
}

pub fn clear_codebuddy_cn_app_cache() {
    let _ = std::fs::remove_file(codebuddy_cn_app_cache_file());
}

// ---------------------------------------------------------------------------
// 签到配置 / 日志（对照 server.py load/save_checkin_config / load/save/add_checkin_log）
// ---------------------------------------------------------------------------

/// 默认签到配置。旧时间窗口字段仅为配置文件兼容保留，调度不再读取。
pub fn default_checkin_config() -> Value {
    json!({
        "enabled": true,
        "start_hour": 6,
        "end_hour": 12,
        "keepalive_days": 0,
        "lazy_refresh_hours": 24,
    })
}

fn merge_checkin_config(input: &Value) -> Value {
    let mut merged = default_checkin_config();
    let Some(map) = input.as_object() else {
        return merged;
    };
    if let Some(enabled) = map.get("enabled").and_then(Value::as_bool) {
        merged["enabled"] = json!(enabled);
    }
    for key in [
        "start_hour",
        "end_hour",
        "keepalive_days",
        "lazy_refresh_hours",
    ] {
        if let Some(value) = map.get(key).and_then(Value::as_i64) {
            merged[key] = json!(value);
        }
    }
    merged
}

/// 读取签到配置（缺失/损坏时合并默认值）。
pub fn load_checkin_config() -> Value {
    let f = checkin_config_file();
    if f.exists() {
        if let Ok(text) = std::fs::read_to_string(&f) {
            if let Ok(value) = serde_json::from_str::<Value>(&text) {
                return merge_checkin_config(&value);
            }
        }
    }
    default_checkin_config()
}

/// 保存签到配置（只保留已知字段）。
pub fn save_checkin_config(cfg: &Value) -> std::io::Result<()> {
    let merged = merge_checkin_config(cfg);
    std::fs::create_dir_all(store_dir())?;
    let content = serde_json::to_string_pretty(&merged).unwrap_or_default();
    atomic_write(&checkin_config_file(), &content)
}

/// 读取签到日志。
pub fn load_checkin_logs() -> Vec<Value> {
    let f = checkin_logs_file();
    if f.exists() {
        if let Ok(text) = std::fs::read_to_string(&f) {
            if let Ok(Value::Array(arr)) = serde_json::from_str::<Value>(&text) {
                return arr;
            }
        }
    }
    vec![]
}

fn save_checkin_logs_unlocked(logs: &[Value]) -> std::io::Result<()> {
    let kept = normalize_checkin_logs(logs, now_ms());
    std::fs::create_dir_all(store_dir())?;
    let content = serde_json::to_string_pretty(&kept).unwrap_or_default();
    atomic_write(&checkin_logs_file(), &content)
}

/// 保存签到日志（30 天过滤 + 保留最近 500 条，保持插入顺序）。
pub fn save_checkin_logs(logs: &[Value]) -> std::io::Result<()> {
    let _guard = CHECKIN_LOG_WRITE_LOCK.lock().unwrap();
    save_checkin_logs_unlocked(logs)
}

fn checkin_log_local_date(ts_ms: i64) -> Option<String> {
    Local
        .timestamp_millis_opt(ts_ms)
        .single()
        .map(|date| date.format("%Y-%m-%d").to_string())
}

fn legacy_checkin_identity(entry: &Value) -> Option<String> {
    if let Some(account_id) = entry
        .get("accountId")
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|value| !value.is_empty())
    {
        return Some(format!("account:{account_id}"));
    }

    // Old log rows predate accountId and only carried the display identity in
    // `email`. Keep this fallback namespaced so it can never merge with a
    // stable local account ID that happens to have the same text.
    entry
        .get("email")
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(|email| format!("legacy:{email}"))
}

/// Apply the persisted check-in log contract without changing the source file.
///
/// `success` and `error` entries retain their full multiplicity. Only repeated
/// legacy `already` rows are reduced to the latest timestamp for one account
/// and local calendar date.
fn normalize_checkin_logs(logs: &[Value], at_ms: i64) -> Vec<Value> {
    // 保留天数来自设置项（默认 60 天，设置页可调）；
    // CHECKIN_LOG_KEEP_DAYS 仅作为「用户从未配置过」时的语义参照保留。
    let cutoff = at_ms.saturating_sub(record_retention_days() * 24 * 3600 * 1000);
    let retained: Vec<(usize, i64, &Value)> = logs
        .iter()
        .enumerate()
        .filter_map(|(index, entry)| {
            let ts = norm_ts(entry.get("ts"))?;
            (ts >= cutoff).then_some((index, ts, entry))
        })
        .collect();

    let mut dedupable_already_indices = HashSet::new();
    let mut latest_already = HashMap::<(String, String), (i64, usize)>::new();
    for (index, ts, entry) in &retained {
        if entry.get("result").and_then(Value::as_str) != Some("already") {
            continue;
        }
        let Some(identity) = legacy_checkin_identity(entry) else {
            continue;
        };
        let Some(date) = checkin_log_local_date(*ts) else {
            continue;
        };
        dedupable_already_indices.insert(*index);
        let candidate = (*ts, *index);
        latest_already
            .entry((identity, date))
            .and_modify(|current| {
                if candidate >= *current {
                    *current = candidate;
                }
            })
            .or_insert(candidate);
    }

    let winning_already_indices: HashSet<usize> = latest_already
        .into_values()
        .map(|(_, index)| index)
        .collect();
    let mut normalized: Vec<Value> = retained
        .into_iter()
        .filter(|(index, _, entry)| {
            entry.get("result").and_then(Value::as_str) != Some("already")
                || !dedupable_already_indices.contains(index)
                || winning_already_indices.contains(index)
        })
        .map(|(_, _, entry)| entry.clone())
        .collect();

    if normalized.len() > CHECKIN_LOG_MAX_RECORDS {
        normalized.drain(..normalized.len() - CHECKIN_LOG_MAX_RECORDS);
    }
    normalized
}

fn compact_checkin_logs_at(path: &Path, at_ms: i64) -> std::io::Result<bool> {
    if !path.exists() {
        return Ok(false);
    }
    let text = std::fs::read_to_string(path)?;
    let Ok(Value::Array(logs)) = serde_json::from_str::<Value>(&text) else {
        // Preserve unreadable user data rather than replacing it with an empty
        // file. Normal log loading keeps its existing tolerant behavior.
        return Ok(false);
    };
    let normalized = normalize_checkin_logs(&logs, at_ms);
    if normalized == logs {
        return Ok(false);
    }
    let content = serde_json::to_string_pretty(&normalized).unwrap_or_default();
    atomic_write(path, &content)?;
    Ok(true)
}

/// Compact legacy persisted check-in logs once during host startup.
///
/// Returns `true` only when the file was rewritten. Loading logs remains a
/// read-only operation; both hosts invoke this explicit migration before their
/// first automatic verification cycle.
pub fn compact_checkin_logs() -> std::io::Result<bool> {
    let _guard = CHECKIN_LOG_WRITE_LOCK.lock().unwrap();
    compact_checkin_logs_at(&checkin_logs_file(), now_ms())
}

/// 追加一条签到日志。
pub fn add_checkin_log(entry: &Value) {
    // Account-scoped check-in coordination permits unrelated accounts to run
    // concurrently. Serialize the file read-modify-write so neither entry is lost.
    let _guard = CHECKIN_LOG_WRITE_LOCK.lock().unwrap();
    let mut logs = load_checkin_logs();
    logs.push(entry.clone());
    let _ = save_checkin_logs_unlocked(&logs);
}

// ---------------------------------------------------------------------------
// 派猫猫旅行配置 / 缓存
// ---------------------------------------------------------------------------

/// 默认自动旅行配置。
pub fn default_travel_config() -> Value {
    json!({
        "enabled": true,
    })
}

/// 读取自动旅行配置（缺失/损坏时合并默认值）。
pub fn load_travel_config() -> Value {
    let mut cfg = default_travel_config();
    let f = travel_config_file();
    if f.exists() {
        if let Ok(text) = std::fs::read_to_string(&f) {
            if let Ok(Value::Object(map)) = serde_json::from_str::<Value>(&text) {
                if let Some(enabled) = map.get("enabled").and_then(Value::as_bool) {
                    cfg["enabled"] = json!(enabled);
                }
            }
        }
    }
    cfg
}

/// 保存自动旅行配置（只保留已知字段）。
pub fn save_travel_config(cfg: &Value) -> std::io::Result<()> {
    let mut merged = default_travel_config();
    if let Some(enabled) = cfg.get("enabled").and_then(Value::as_bool) {
        merged["enabled"] = json!(enabled);
    }
    std::fs::create_dir_all(store_dir())?;
    let content = serde_json::to_string_pretty(&merged).unwrap_or_default();
    atomic_write(&travel_config_file(), &content)
}

/// 读取旅行缓存（`{ date, completed, results: { accountId: {...} } }`）。
pub fn load_travel_cache() -> Value {
    let f = travel_cache_file();
    if f.exists() {
        if let Ok(text) = std::fs::read_to_string(&f) {
            if let Ok(Value::Object(map)) = serde_json::from_str::<Value>(&text) {
                return Value::Object(map);
            }
        }
    }
    json!({})
}

/// 保存旅行缓存。
pub fn save_travel_cache(cache: &Value) -> std::io::Result<()> {
    std::fs::create_dir_all(store_dir())?;
    let content = serde_json::to_string_pretty(cache).unwrap_or_default();
    atomic_write(&travel_cache_file(), &content)
}

// ---------------------------------------------------------------------------
// 自动轮换配置 / 日志（CodeBuddy CLI 账号轮换）
// ---------------------------------------------------------------------------

/// 默认自动轮换配置。
pub fn default_auto_rotate_config() -> Value {
    json!({
        "enabled": false,
        "check_interval_minutes": 5,
        "cooldown_minutes": 120,
        "min_gap_hours": 24,
        "min_urgency_hours": 72,
        "active_guard_minutes": 30,
        "min_remaining_credits": 0,
    })
}

/// 读取自动轮换配置（缺失/损坏时合并默认值）。
pub fn load_auto_rotate_config() -> Value {
    let mut cfg = default_auto_rotate_config();
    let f = auto_rotate_config_file();
    if f.exists() {
        if let Ok(text) = std::fs::read_to_string(&f) {
            if let Ok(Value::Object(map)) = serde_json::from_str::<Value>(&text) {
                for (k, v) in map {
                    cfg[k] = v;
                }
            }
        }
    }
    cfg
}

/// 保存自动轮换配置（只保留已知字段）。
pub fn save_auto_rotate_config(cfg: &Value) -> std::io::Result<()> {
    let mut merged = default_auto_rotate_config();
    let allowed: Vec<&str> = vec![
        "enabled",
        "check_interval_minutes",
        "cooldown_minutes",
        "min_gap_hours",
        "min_urgency_hours",
        "active_guard_minutes",
        "min_remaining_credits",
    ];
    for k in allowed {
        if let Some(v) = cfg.get(k) {
            merged[k] = v.clone();
        }
    }
    std::fs::create_dir_all(store_dir())?;
    let content = serde_json::to_string_pretty(&merged).unwrap_or_default();
    atomic_write(&auto_rotate_config_file(), &content)
}

/// 读取自动轮换日志。
pub fn load_rotate_logs() -> Vec<Value> {
    let f = auto_rotate_logs_file();
    if f.exists() {
        if let Ok(text) = std::fs::read_to_string(&f) {
            if let Ok(Value::Array(arr)) = serde_json::from_str::<Value>(&text) {
                return arr;
            }
        }
    }
    vec![]
}

/// 保存自动轮换日志（保留最近 N 条，保持插入顺序）。
pub fn save_rotate_logs(logs: &[Value]) -> std::io::Result<()> {
    let mut kept: Vec<Value> = logs.to_vec();
    if kept.len() > ROTATE_LOG_MAX_RECORDS {
        kept.drain(..kept.len() - ROTATE_LOG_MAX_RECORDS);
    }
    std::fs::create_dir_all(store_dir())?;
    let content = serde_json::to_string_pretty(&kept).unwrap_or_default();
    atomic_write(&auto_rotate_logs_file(), &content)
}

/// 追加一条自动轮换日志。
pub fn add_rotate_log(entry: &Value) {
    let mut logs = load_rotate_logs();
    logs.push(entry.clone());
    let _ = save_rotate_logs(&logs);
}

// ---------------------------------------------------------------------------
// 并发运行标志（替代 Python threading.Lock，Send 安全可跨 await）
// ---------------------------------------------------------------------------

/// RAII 运行标志：进入临界区置 true，Drop 时复位。
pub struct RunFlagGuard<'a> {
    flag: &'a AtomicBool,
}

impl<'a> RunFlagGuard<'a> {
    /// 尝试获取标志；已被占用返回 None。
    pub fn try_acquire(flag: &'a AtomicBool) -> Option<Self> {
        if flag.swap(true, Ordering::SeqCst) {
            None
        } else {
            Some(Self { flag })
        }
    }
}

impl Drop for RunFlagGuard<'_> {
    fn drop(&mut self) {
        self.flag.store(false, Ordering::SeqCst);
    }
}

// ---------------------------------------------------------------------------
// 时间
// ---------------------------------------------------------------------------

pub fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

pub fn now_secs() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

pub fn utc_iso() -> String {
    // 对照 Python utc_iso：%Y-%m-%dT%H-%M-%S + "Z"
    format!("{}Z", chrono::Utc::now().format("%Y-%m-%dT%H-%M-%S"))
}

// ---------------------------------------------------------------------------
// 文件
// ---------------------------------------------------------------------------

/// 原子写文件（临时文件 + rename），对照 Python atomic_write。
pub fn atomic_write(path: &Path, content: &str) -> std::io::Result<()> {
    let file_name = path
        .file_name()
        .map(|n| n.to_string_lossy().to_string())
        .unwrap_or_default();
    let tmp = path.with_file_name(format!("{file_name}.tmp-{}", uuid::Uuid::new_v4().simple()));
    if let Err(e) = std::fs::write(&tmp, content) {
        eprintln!("[atomic] write tmp FAILED: {e}");
        return Err(e);
    }
    if let Err(e) = std::fs::rename(&tmp, path) {
        eprintln!("[atomic] rename FAILED: {e}");
        return Err(e);
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// 时间戳归一化
// ---------------------------------------------------------------------------

/// 把秒/毫秒/字符串时间戳统一为毫秒；无效返回 None。对照 server.py `_norm_ts`。
pub fn norm_ts(v: Option<&Value>) -> Option<i64> {
    let mut ts: i64 = match v {
        Some(Value::String(s)) => s.trim().parse::<f64>().ok()? as i64,
        Some(Value::Number(n)) => n.as_i64().or_else(|| n.as_f64().map(|f| f as i64))?,
        _ => return None,
    };
    if ts < 10_000_000_000 {
        ts *= 1000; // 秒 → 毫秒
    }
    Some(ts)
}

// ---------------------------------------------------------------------------
// HTTP 客户端（对照 Python http_request）
// ---------------------------------------------------------------------------

static HTTP_CLIENT: OnceLock<reqwest::Client> = OnceLock::new();

/// 构造宿主 HTTP 客户端。
///
/// **必须显式挂代理**：reqwest 与 Go 一样，默认只读 `HTTPS_PROXY` 等环境变量，
/// 不看 Windows 注册表里的系统代理。实测（2026-09）：国际版 `www.workbuddy.ai`
/// 在国内直连 12/12 全部 `ECONNRESET`，走代理 12/12 成功 —— 所以「浏览器能打开」
/// 不代表宿主能调通，积分查询与 token 刷新都会失败，界面显示
/// `error sending request for url (https://www.w…)`。
///
/// 代理地址**复用**「设置 → 更新代理」里已填的值（`github_config.json` 的 `proxy`），
/// 用户无需配两遍。未配置时不挂代理（保持原有直连行为）。
fn http_client_builder() -> reqwest::ClientBuilder {
    apply_proxy(
        reqwest::Client::builder()
            .timeout(std::time::Duration::from_secs(30))
            .user_agent(DEFAULT_HTTP_USER_AGENT),
        proxy_url().as_deref(),
    )
}

/// 把代理挂到 builder 上（抽成纯函数以便单测）。
///
/// `proxy` 为 None / 空串 → 原样返回（直连，保持原有行为）。
///
/// 本机回环（网关 /healthz、/status、/v1/models）不能走代理 —— 否则
/// 「探测本地网关是否在跑」会被转发到远端代理而失败，表现为网关明明活着
/// 却显示未就绪。
///
/// **注意**必须用 `Proxy::no_proxy(Some(NoProxy))`（排除指定主机），
/// **不能**用 `ClientBuilder::no_proxy()` —— 后者的语义是「清空所有代理 +
/// 关闭系统代理」，会把上面刚设的代理一起删掉。实测踩过：
/// `builder.proxy(p).no_proxy()` 导致代理完全不生效，国际版积分查询仍报
/// `error sending request`。
fn apply_proxy(builder: reqwest::ClientBuilder, proxy: Option<&str>) -> reqwest::ClientBuilder {
    let Some(raw) = proxy.map(str::trim).filter(|s| !s.is_empty()) else {
        return builder;
    };
    match reqwest::Proxy::all(raw) {
        Ok(p) => {
            let no_proxy = reqwest::NoProxy::from_string("localhost,127.0.0.1,::1");
            builder.proxy(p.no_proxy(no_proxy))
        }
        // 地址非法：不挂代理，回落直连（由调用方报错）
        Err(_) => builder,
    }
}

/// 读取「设置 → 更新代理」里配置的代理地址；未配置返回 None。
///
/// 与网关的 `upstream_proxy()` 同源（都读 `github_config.json` 的 `proxy`），
/// 保证宿主与网关走同一个出口。
fn proxy_url() -> Option<String> {
    let raw = crate::modules::update::load_github_config()
        .get("proxy")
        .and_then(Value::as_str)
        .unwrap_or("")
        .trim()
        .to_string();
    if raw.is_empty() {
        None
    } else {
        Some(raw)
    }
}

fn http_client() -> &'static reqwest::Client {
    HTTP_CLIENT.get_or_init(|| {
        http_client_builder()
            .build()
            .expect("failed to build reqwest client")
    })
}

/// 通用 HTTP 请求，返回解析后的 JSON。
///
/// 行为对齐 Python 版：
/// - 2xx：解析 body 为 JSON；
/// - HTTP 错误：body 可解析则返回其 JSON，否则 `{"code": <status>, "message": <body 前 500 字符>}`；
/// - 网络错误：`{"code": -1, "message": <原因>}`。
pub async fn http_request(
    url: &str,
    method: &str,
    body: Option<Value>,
    headers: Option<&HashMap<String, String>>,
) -> Value {
    http_request_with_proxy(url, method, body, headers, None).await
}

/// 通用 HTTP 请求，可为单次请求显式指定 HTTP/HTTPS 代理。
pub async fn http_request_with_proxy(
    url: &str,
    method: &str,
    body: Option<Value>,
    headers: Option<&HashMap<String, String>>,
    proxy: Option<&str>,
) -> Value {
    let method = reqwest::Method::from_bytes(method.as_bytes()).unwrap_or(reqwest::Method::GET);
    let client = match proxy.map(str::trim).filter(|value| !value.is_empty()) {
        Some(proxy) => match http_client_builder()
            .proxy(match reqwest::Proxy::all(proxy) {
                Ok(proxy) => proxy,
                Err(e) => return json!({"code": -1, "message": format!("代理地址无效: {e}")}),
            })
            .build()
        {
            Ok(client) => client,
            Err(e) => return json!({"code": -1, "message": format!("代理客户端创建失败: {e}")}),
        },
        None => http_client().clone(),
    };
    let mut req = client.request(method, url);
    req = req.header("Content-Type", "application/json");
    if let Some(h) = headers {
        for (k, v) in h {
            req = req.header(k, v);
        }
    }
    if let Some(b) = body {
        req = req.json(&b);
    }
    match req.send().await {
        Ok(resp) => {
            let status = resp.status();
            let text = resp.text().await.unwrap_or_default();
            if status.is_success() {
                serde_json::from_str(&text).unwrap_or(Value::Null)
            } else {
                serde_json::from_str(&text).unwrap_or_else(|_| {
                    json!({
                        "code": status.as_u16(),
                        "message": text.chars().take(500).collect::<String>(),
                    })
                })
            }
        }
        Err(e) => json!({"code": -1, "message": e.to_string()}),
    }
}

/// 通用 HTTP 请求，返回原始响应（状态码 + 响应头 + 响应体），可选是否跟随重定向。
///
/// 供需要读取响应头（如 302 的 `Location`）或自行处理非 JSON 响应的场景使用；
/// 其余场景优先用 [`http_request_with_proxy`]。失败（网络错误 / 代理配置错误）
/// 返回 `(0, HashMap::new(), 错误信息)`，由调用方根据 status 判断。
pub async fn http_request_raw(
    url: &str,
    method: &str,
    body: Option<Value>,
    headers: Option<&HashMap<String, String>>,
    proxy: Option<&str>,
    follow_redirects: bool,
) -> (u16, HashMap<String, String>, String) {
    let method = reqwest::Method::from_bytes(method.as_bytes()).unwrap_or(reqwest::Method::GET);
    let client = match proxy.map(str::trim).filter(|value| !value.is_empty()) {
        Some(proxy) => {
            let mut builder = http_client_builder().proxy(match reqwest::Proxy::all(proxy) {
                Ok(proxy) => proxy,
                Err(e) => return (0, HashMap::new(), format!("代理地址无效: {e}")),
            });
            if !follow_redirects {
                builder = builder.redirect(reqwest::redirect::Policy::none());
            }
            match builder.build() {
                Ok(client) => client,
                Err(e) => return (0, HashMap::new(), format!("代理客户端创建失败: {e}")),
            }
        }
        None => {
            if follow_redirects {
                http_client().clone()
            } else {
                match http_client_builder()
                    .redirect(reqwest::redirect::Policy::none())
                    .build()
                {
                    Ok(client) => client,
                    Err(e) => return (0, HashMap::new(), format!("客户端创建失败: {e}")),
                }
            }
        }
    };
    let mut req = client.request(method, url);
    req = req.header("Content-Type", "application/json");
    if let Some(h) = headers {
        for (k, v) in h {
            req = req.header(k, v);
        }
    }
    if let Some(b) = body {
        req = req.json(&b);
    }
    match req.send().await {
        Ok(resp) => {
            let status = resp.status().as_u16();
            let mut resp_headers = HashMap::new();
            for (k, v) in resp.headers() {
                if let Ok(vs) = v.to_str() {
                    resp_headers.insert(k.as_str().to_string(), vs.to_string());
                }
            }
            let text = resp.text().await.unwrap_or_default();
            (status, resp_headers, text)
        }
        Err(e) => (0, HashMap::new(), e.to_string()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn local_timestamp_ms(year: i32, month: u32, day: u32, hour: u32) -> i64 {
        Local
            .with_ymd_and_hms(year, month, day, hour, 0, 0)
            .single()
            .expect("test timestamp must be unambiguous")
            .timestamp_millis()
    }

    // -----------------------------------------------------------------------
    // 出站代理（宿主侧）
    //
    // 背景：reqwest 有两个名字极像、语义相反、且都不会编译报错的 API ——
    //
    //   Proxy::no_proxy(Option<NoProxy>)   排除指定主机，**保留**代理
    //   ClientBuilder::no_proxy()          清空所有代理 + 关系统代理
    //
    // 最初误写成 `builder.proxy(p).no_proxy()`（本意是排除回环），实际把刚设的
    // 代理删掉了 → 宿主仍直连 → 国际版积分查询报
    // `error sending request for url (https://www.workbuddy.ai/...)`。
    //
    // 单看返回值无法区分这两种写法（都返回 builder），所以必须**发一个真实请求**
    // 看它是否经过代理：这里起一个只会被「作为 HTTP 代理」访问到的本地监听端口，
    // 请求一个不存在的目标域名 —— 若代理生效，监听端会收到 CONNECT 请求。
    // -----------------------------------------------------------------------

    /// 起一个本地假代理，返回 (端口, 收到请求的计数器)。
    /// 假代理对任何 CONNECT 都回 200 然后立即关闭，仅用于证明「请求确实来了」。
    fn spawn_fake_proxy() -> (u16, std::sync::Arc<std::sync::atomic::AtomicUsize>) {
        use std::io::{BufRead, BufReader, Write};
        use std::sync::atomic::{AtomicUsize, Ordering};
        use std::sync::Arc;

        let listener = std::net::TcpListener::bind("127.0.0.1:0").expect("应能绑定本地端口");
        let port = listener.local_addr().expect("应能取到端口").port();
        let hits = Arc::new(AtomicUsize::new(0));
        let hits_bg = hits.clone();

        std::thread::spawn(move || {
            for stream in listener.incoming() {
                let Ok(mut s) = stream else { continue };
                // 读一行就够了：CONNECT host:port HTTP/1.1
                let mut line = String::new();
                {
                    let mut r = BufReader::new(&mut s);
                    let _ = r.read_line(&mut line);
                }
                if line.starts_with("CONNECT") {
                    hits_bg.fetch_add(1, Ordering::SeqCst);
                }
                // 回 200 让客户端认为隧道建立，然后关闭（客户端随后会失败，无妨）
                let _ = s.write_all(b"HTTP/1.1 200 Connection Established\r\n\r\n");
                let _ = s.flush();
            }
        });

        (port, hits)
    }

    /// 核心回归：挂了代理后，请求**必须**经过代理。
    ///
    /// 若误用 ClientBuilder::no_proxy()，代理被清空 → 请求直连 → 假代理
    /// 收不到任何东西 → hits == 0 → 本测试失败。
    #[tokio::test]
    async fn proxy_is_actually_used_after_applying_no_proxy() {
        use std::sync::atomic::Ordering;

        let (port, hits) = spawn_fake_proxy();
        let proxy = format!("http://127.0.0.1:{port}");

        let client = apply_proxy(
            reqwest::Client::builder().timeout(std::time::Duration::from_secs(3)),
            Some(&proxy),
        )
        .build()
        .expect("client 应能构建");

        // 目标域名故意不可达：我们只关心「请求是否先到了假代理」。
        let _ = client
            .get("https://workbuddy.ai.invalid/probe")
            .send()
            .await;

        assert!(
            hits.load(Ordering::SeqCst) > 0,
            "请求没有经过代理 —— 代理被 no_proxy 清空了？\
             （误用 ClientBuilder::no_proxy() 会导致此现象）"
        );
    }

    /// 回环地址被排除：访问 127.0.0.1 时不走代理。
    ///
    /// 否则「探测本地网关是否在跑」会被转发到远端代理而失败，
    /// 表现为网关明明活着却显示未就绪。
    #[tokio::test]
    async fn loopback_bypasses_proxy() {
        use std::sync::atomic::Ordering;

        // 目标是一个真实的本地服务（不是代理），它应被直接访问。
        let listener = std::net::TcpListener::bind("127.0.0.1:0").expect("应能绑定");
        let port = listener.local_addr().expect("取端口").port();
        let direct_hits = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let dh = direct_hits.clone();
        std::thread::spawn(move || {
            use std::io::{BufRead, BufReader, Write};
            for stream in listener.incoming() {
                let Ok(mut s) = stream else { continue };
                let mut line = String::new();
                {
                    let mut r = BufReader::new(&mut s);
                    let _ = r.read_line(&mut line);
                }
                if line.starts_with("GET") {
                    dh.fetch_add(1, Ordering::SeqCst);
                }
                let _ = s.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}");
                let _ = s.flush();
            }
        });

        // 代理指向一个不存在的端口：若回环流量误走代理，请求必然失败。
        let (_proxy_port, proxy_hits) = spawn_fake_proxy();
        let client = apply_proxy(
            reqwest::Client::builder().timeout(std::time::Duration::from_secs(3)),
            Some(&format!("http://127.0.0.1:{_proxy_port}")),
        )
        .build()
        .expect("client 应能构建");

        let resp = client
            .get(format!("http://127.0.0.1:{port}/probe"))
            .send()
            .await;

        assert!(resp.is_ok(), "回环请求应直连成功，实际失败: {resp:?}");
        assert!(
            direct_hits.load(Ordering::SeqCst) > 0,
            "回环请求没有到达本地服务"
        );
        assert_eq!(
            proxy_hits.load(Ordering::SeqCst),
            0,
            "回环请求不应经过代理（NoProxy 未生效）"
        );
    }

    /// 未配置代理时保持原有直连行为（不因新特性改变默认）。
    #[tokio::test]
    async fn no_proxy_configured_means_direct() {
        use std::sync::atomic::Ordering;

        let (port, hits) = spawn_fake_proxy();
        let client = apply_proxy(
            reqwest::Client::builder().timeout(std::time::Duration::from_secs(2)),
            None, // 未配置
        )
        .build()
        .expect("client 应能构建");

        // 请求本机另一个端口（直连），不应碰代理。
        let _ = client.get(format!("http://127.0.0.1:{port}/x")).send().await;
        // 此时假代理收到的是普通 GET（非 CONNECT），计数应为 0。
        assert_eq!(
            hits.load(Ordering::SeqCst),
            0,
            "未配置代理时不应走代理"
        );
    }

    /// 非法代理地址不应让 client 构建失败（回落直连）。
    #[test]
    fn invalid_proxy_falls_back_to_direct() {
        let built = apply_proxy(reqwest::Client::builder(), Some("://bad")).build();
        assert!(built.is_ok(), "非法代理地址应回落直连而不是报错");
    }

    #[test]
    fn region_key_round_trips_and_unknown_falls_back_to_cn() {
        assert_eq!(Region::from_key("intl"), Region::Intl);
        assert_eq!(Region::from_key("INTL"), Region::Intl);
        assert_eq!(Region::from_key(" intl "), Region::Intl);
        assert_eq!(Region::from_key("cn"), Region::Cn);
        // 空值 / 未知值按国服处理，与 from_domain 的历史默认一致
        assert_eq!(Region::from_key(""), Region::Cn);
        assert_eq!(Region::from_key("mars"), Region::Cn);
        for region in Region::ALL {
            assert_eq!(Region::from_key(region.key()), region);
        }
    }

    #[test]
    fn oauth_platform_is_region_specific() {
        // 国际版必须用 workbuddy-ai：用国服的 workbuddy 会在国际版登录页走
        // Web 分支，扫码后拿不到插件 token
        assert_eq!(Region::Cn.oauth_platform(), "workbuddy");
        assert_eq!(Region::Intl.oauth_platform(), "workbuddy-ai");
        assert_ne!(
            Region::Cn.oauth_platform(),
            Region::Intl.oauth_platform()
        );
    }

    #[test]
    fn oauth_endpoint_follows_region() {
        assert_eq!(Region::Cn.api_endpoint(), "https://www.codebuddy.cn");
        assert_eq!(Region::Intl.api_endpoint(), "https://www.workbuddy.ai");
    }

    #[test]
    fn auto_checkin_defaults_enabled_and_preserves_legacy_fields() {
        let cfg = default_checkin_config();
        assert_eq!(cfg.get("enabled").and_then(Value::as_bool), Some(true));
        assert_eq!(cfg.get("start_hour").and_then(Value::as_i64), Some(6));
        assert_eq!(cfg.get("end_hour").and_then(Value::as_i64), Some(12));
    }

    #[test]
    fn auto_checkin_explicit_false_wins_and_invalid_value_uses_default() {
        let disabled = merge_checkin_config(&json!({"enabled": false, "keepalive_days": 7}));
        assert_eq!(
            disabled.get("enabled").and_then(Value::as_bool),
            Some(false)
        );
        assert_eq!(
            disabled.get("keepalive_days").and_then(Value::as_i64),
            Some(7)
        );

        let corrupt = merge_checkin_config(&json!({"enabled": "no", "lazy_refresh_hours": null}));
        assert_eq!(corrupt.get("enabled").and_then(Value::as_bool), Some(true));
        assert_eq!(
            corrupt.get("lazy_refresh_hours").and_then(Value::as_i64),
            Some(24)
        );
    }

    #[test]
    fn checkin_log_normalization_keeps_latest_already_per_identity_and_local_date() {
        let day = local_timestamp_ms(2026, 8, 20, 12);
        let logs = vec![
            json!({"accountId": "a", "email": "same", "result": "already", "ts": day + 1, "marker": "a-old"}),
            json!({"accountId": "b", "email": "same", "result": "already", "ts": day + 2, "marker": "b"}),
            json!({"accountId": "a", "email": "same", "result": "already", "ts": day + 3, "marker": "a-new"}),
            json!({"email": "legacy@example.com", "result": "already", "ts": day + 4, "marker": "legacy-old"}),
            json!({"email": "legacy@example.com", "result": "already", "ts": day + 5, "marker": "legacy-new"}),
            json!({"result": "already", "ts": day + 6, "marker": "no-identity"}),
        ];

        let normalized = normalize_checkin_logs(&logs, day + 10);
        let markers: Vec<&str> = normalized
            .iter()
            .filter_map(|entry| entry.get("marker").and_then(Value::as_str))
            .collect();

        assert_eq!(markers, vec!["b", "a-new", "legacy-new", "no-identity"]);
    }

    #[test]
    fn checkin_log_identity_namespaces_stable_ids_and_legacy_email_fallbacks() {
        let day = local_timestamp_ms(2026, 8, 20, 12);
        let logs = vec![
            json!({"accountId": "same@example.com", "email": "display", "result": "already", "ts": day + 1, "marker": "stable-old"}),
            json!({"email": "same@example.com", "result": "already", "ts": day + 2, "marker": "legacy-old"}),
            json!({"accountId": "same@example.com", "email": "display", "result": "already", "ts": day + 3, "marker": "stable-new"}),
            json!({"email": "same@example.com", "result": "already", "ts": day + 4, "marker": "legacy-new"}),
        ];

        let normalized = normalize_checkin_logs(&logs, day + 10);
        let markers: Vec<&str> = normalized
            .iter()
            .filter_map(|entry| entry.get("marker").and_then(Value::as_str))
            .collect();

        assert_eq!(markers, vec!["stable-new", "legacy-new"]);
    }

    #[test]
    fn checkin_log_normalization_keeps_already_for_separate_dates() {
        let first_day = local_timestamp_ms(2026, 8, 19, 12);
        let second_day = local_timestamp_ms(2026, 8, 20, 12);
        let logs = vec![
            json!({"accountId": "a", "result": "already", "ts": first_day}),
            json!({"accountId": "a", "result": "already", "ts": second_day}),
        ];

        assert_eq!(normalize_checkin_logs(&logs, second_day).len(), 2);
    }

    #[test]
    fn checkin_log_normalization_preserves_success_and_error_multiplicity() {
        let day = local_timestamp_ms(2026, 8, 20, 12);
        let logs = vec![
            json!({"accountId": "a", "result": "success", "ts": day + 1}),
            json!({"accountId": "a", "result": "success", "ts": day + 2}),
            json!({"accountId": "a", "result": "error", "ts": day + 3}),
            json!({"accountId": "a", "result": "error", "ts": day + 4}),
        ];

        assert_eq!(normalize_checkin_logs(&logs, day + 10), logs);
    }

    #[test]
    fn checkin_log_normalization_applies_retention_and_record_cap() {
        let now = local_timestamp_ms(2026, 8, 20, 12);
        let cutoff = now - CHECKIN_LOG_KEEP_DAYS * 24 * 3600 * 1000;
        let mut logs = vec![json!({
            "accountId": "old",
            "result": "success",
            "ts": cutoff - 1,
            "marker": -1,
        })];
        logs.extend((0..505).map(|marker| {
            json!({
                "accountId": "a",
                "result": "success",
                "ts": now,
                "marker": marker,
            })
        }));

        let normalized = normalize_checkin_logs(&logs, now);
        assert_eq!(normalized.len(), CHECKIN_LOG_MAX_RECORDS);
        assert_eq!(normalized[0]["marker"], 5);
        assert_eq!(normalized.last().unwrap()["marker"], 504);
    }

    #[test]
    fn checkin_log_normalization_deduplicates_before_taking_final_500() {
        let now = local_timestamp_ms(2026, 8, 20, 12);
        let mut logs = vec![
            json!({"accountId": "duplicate", "result": "already", "ts": now - 2, "marker": "duplicate-old"}),
            json!({"accountId": "duplicate", "result": "already", "ts": now - 1, "marker": "duplicate-new"}),
        ];
        logs.extend((0..500).map(|marker| {
            json!({
                "accountId": "a",
                "result": "success",
                "ts": now,
                "marker": marker,
            })
        }));

        let normalized = normalize_checkin_logs(&logs, now);
        assert_eq!(normalized.len(), CHECKIN_LOG_MAX_RECORDS);
        assert_eq!(normalized[0]["marker"], 0);
        assert_eq!(normalized.last().unwrap()["marker"], 499);
        assert!(normalized
            .iter()
            .all(|entry| entry["marker"] != "duplicate-old"));
    }

    #[test]
    fn checkin_log_normalization_is_idempotent() {
        let day = local_timestamp_ms(2026, 8, 20, 12);
        let logs = vec![
            json!({"accountId": "a", "result": "already", "ts": day + 1}),
            json!({"accountId": "a", "result": "already", "ts": day + 2}),
            json!({"accountId": "a", "result": "success", "ts": day + 3}),
        ];

        let once = normalize_checkin_logs(&logs, day + 10);
        assert_eq!(normalize_checkin_logs(&once, day + 10), once);
    }

    #[test]
    fn persisted_checkin_log_compaction_writes_only_when_changed() {
        let day = local_timestamp_ms(2026, 8, 20, 12);
        let dir = std::env::temp_dir().join(format!(
            "ai-gateway-checkin-log-compaction-{}",
            uuid::Uuid::new_v4()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("logs.json");
        let logs = json!([
            {"accountId": "a", "result": "already", "ts": day + 1},
            {"accountId": "a", "result": "already", "ts": day + 2}
        ]);
        std::fs::write(&path, serde_json::to_string_pretty(&logs).unwrap()).unwrap();

        assert!(compact_checkin_logs_at(&path, day + 10).unwrap());
        let after_first = std::fs::read_to_string(&path).unwrap();
        assert!(!compact_checkin_logs_at(&path, day + 10).unwrap());
        assert_eq!(std::fs::read_to_string(&path).unwrap(), after_first);

        let missing = dir.join("missing.json");
        assert!(!compact_checkin_logs_at(&missing, day + 10).unwrap());
        assert!(!missing.exists());

        let corrupt = dir.join("corrupt.json");
        std::fs::write(&corrupt, "not-json").unwrap();
        assert!(!compact_checkin_logs_at(&corrupt, day + 10).unwrap());
        assert_eq!(std::fs::read_to_string(&corrupt).unwrap(), "not-json");

        std::fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn parse_workbuddy_exe_cache_json_reads_exe() {
        let path = parse_workbuddy_exe_cache_json(
            r#"{ "exe": "D:\\Users\\Zhou\\AppData\\Local\\Programs\\WorkBuddy\\WorkBuddy.exe" }"#,
        )
        .expect("valid cache");
        assert_eq!(
            path.to_string_lossy(),
            r"D:\Users\Zhou\AppData\Local\Programs\WorkBuddy\WorkBuddy.exe"
        );
    }

    #[test]
    fn parse_workbuddy_exe_cache_json_ignores_corrupt_and_empty() {
        assert!(parse_workbuddy_exe_cache_json("not-json").is_none());
        assert!(parse_workbuddy_exe_cache_json(r#"{ "exe": "  " }"#).is_none());
        assert!(parse_workbuddy_exe_cache_json("{}").is_none());
    }

    #[test]
    fn parse_codebuddy_cn_app_cache_json_reads_exe() {
        let path = parse_codebuddy_cn_app_cache_json(
            r#"{ "exe": "C:\\Users\\Zhou\\AppData\\Local\\Programs\\CodeBuddy CN\\CodeBuddy CN.exe" }"#,
        )
        .expect("valid cache");
        assert_eq!(
            path.to_string_lossy(),
            r"C:\Users\Zhou\AppData\Local\Programs\CodeBuddy CN\CodeBuddy CN.exe"
        );
    }

    #[test]
    fn parse_codebuddy_cn_app_cache_json_ignores_corrupt_and_empty() {
        assert!(parse_codebuddy_cn_app_cache_json("not-json").is_none());
        assert!(parse_codebuddy_cn_app_cache_json(r#"{ "exe": "  " }"#).is_none());
        assert!(parse_codebuddy_cn_app_cache_json("{}").is_none());
    }

    #[test]
    fn codebuddy_cn_app_cache_file_is_not_workbuddy_exe_cache() {
        assert_ne!(
            codebuddy_cn_app_cache_file(),
            workbuddy_exe_cache_file_for(Region::Cn)
        );
        assert!(codebuddy_cn_app_cache_file()
            .file_name()
            .is_some_and(|n| n == "codebuddy_cn_app.json"));
        // 国服与国际版各自独立的 exe 缓存文件，互不覆盖。
        assert_ne!(
            workbuddy_exe_cache_file_for(Region::Cn),
            workbuddy_exe_cache_file_for(Region::Intl)
        );
        assert!(workbuddy_exe_cache_file_for(Region::Intl)
            .file_name()
            .is_some_and(|n| n == "workbuddy_ai_exe.json"));
    }

    #[test]
    fn default_http_user_agent_matches_official_chrome_desktop() {
        assert_eq!(
            DEFAULT_HTTP_USER_AGENT,
            "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
        );
        let _ = http_client_builder();
    }
    // -----------------------------------------------------------------------
    // 记录保留天数（设置页可配置）
    //
    // 默认 60 天；越界值必须被夹到合法区间而不是原样存下 ——
    // 否则「填 0」会让所有记录立刻被清理，用户却以为只是「先不限制」。
    // -----------------------------------------------------------------------

    #[test]
    fn retention_normalize_falls_back_on_non_positive() {
        // 0 与负数都回落到默认值：静默「什么都不存」是最糟的失败模式
        assert_eq!(normalize_retention_days(0), RECORD_RETENTION_DEFAULT_DAYS);
        assert_eq!(normalize_retention_days(-1), RECORD_RETENTION_DEFAULT_DAYS);
        assert_eq!(normalize_retention_days(-9999), RECORD_RETENTION_DEFAULT_DAYS);
    }

    #[test]
    fn retention_normalize_clamps_to_range() {
        assert_eq!(normalize_retention_days(1), RECORD_RETENTION_MIN_DAYS);
        assert_eq!(normalize_retention_days(60), 60);
        assert_eq!(normalize_retention_days(365), 365);
        assert_eq!(
            normalize_retention_days(i64::MAX),
            RECORD_RETENTION_MAX_DAYS
        );
    }

    #[test]
    fn retention_default_is_60_days() {
        assert_eq!(RECORD_RETENTION_DEFAULT_DAYS, 60);
    }

    #[test]
    fn retention_presets_are_within_range_and_unique() {
        let mut seen = std::collections::HashSet::new();
        for (days, label) in RECORD_RETENTION_PRESETS {
            assert!(
                *days >= RECORD_RETENTION_MIN_DAYS && *days <= RECORD_RETENTION_MAX_DAYS,
                "预设 {days} 天越界"
            );
            assert!(seen.insert(*days), "预设 {days} 天重复");
            assert!(!label.is_empty(), "预设 {days} 天缺少文案");
        }
        // 默认值必须能在预设里选到，否则用户改完就回不到默认
        assert!(
            RECORD_RETENTION_PRESETS
                .iter()
                .any(|(d, _)| *d == RECORD_RETENTION_DEFAULT_DAYS),
            "预设里必须包含默认值 {} 天",
            RECORD_RETENTION_DEFAULT_DAYS
        );
    }

    #[test]
    fn retention_reads_default_when_unset() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("retention-unset");
        assert_eq!(record_retention_days(), RECORD_RETENTION_DEFAULT_DAYS);
    }

    #[test]
    fn retention_round_trips_through_settings() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("retention-roundtrip");
        let applied = set_record_retention_days(30).expect("写入应成功");
        assert_eq!(applied, 30);
        assert_eq!(record_retention_days(), 30);
    }

    #[test]
    fn retention_save_returns_normalized_value() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("retention-normalized");
        // 填 0：存下的应是默认值，返回的也必须是真实生效值
        let applied = set_record_retention_days(0).expect("写入应成功");
        assert_eq!(applied, RECORD_RETENTION_DEFAULT_DAYS);
        assert_eq!(record_retention_days(), RECORD_RETENTION_DEFAULT_DAYS);

        // 填极大值：夹到上限
        let applied = set_record_retention_days(999_999).expect("写入应成功");
        assert_eq!(applied, RECORD_RETENTION_MAX_DAYS);
        assert_eq!(record_retention_days(), RECORD_RETENTION_MAX_DAYS);
    }

    #[test]
    fn retention_setting_survives_other_settings_writes() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("retention-merge");
        set_record_retention_days(90).expect("写入应成功");
        // 其它设置项写入不应覆盖保留天数（合并式写入）
        save_app_settings(&json!({ "someOtherKey": true })).expect("写入应成功");
        assert_eq!(record_retention_days(), 90);
    }

    #[test]
    fn checkin_log_retention_follows_setting() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("retention-checkin");
        // 设为 7 天：8 天前的记录应被清掉，6 天前的应保留
        set_record_retention_days(7).expect("写入应成功");
        let now = local_timestamp_ms(2026, 8, 20, 12);
        let day = 24 * 3600 * 1000;
        let logs = vec![
            json!({"accountId": "old", "result": "success", "ts": now - 8 * day}),
            json!({"accountId": "fresh", "result": "success", "ts": now - 6 * day}),
        ];
        let kept = normalize_checkin_logs(&logs, now);
        let ids: Vec<&str> = kept
            .iter()
            .filter_map(|v| v.get("accountId").and_then(Value::as_str))
            .collect();
        assert!(ids.contains(&"fresh"), "6 天前的记录应保留，实际 {ids:?}");
        assert!(!ids.contains(&"old"), "8 天前的记录应被清理，实际 {ids:?}");
    }
}
