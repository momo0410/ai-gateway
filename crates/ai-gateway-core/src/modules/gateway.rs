//! 网关（workbuddy2api）托管与账号桥接。
//!
//! 设计：把编译好的 gateway 可执行文件作为**子进程**托管，并复用本机已有的
//! 账号库（`~/.wb-switch/accounts.json`）为其生成凭证目录，使账号管理页与
//! OpenAI 兼容网关共享同一批账号。
//!
//! 为什么用子进程而不是把网关逻辑用 Rust 重写：
//!   网关包含账号池三因子加权重、熔断指数退避、会话粘性、SSE 逐帧规范化、
//!   指纹脱敏等大量经过测试的逻辑，重写既无必要也会引入行为差异。
//!   子进程方式保留其全部行为，且崩溃可独立重启。
//!
//! 账号同步沿用 ai-gateway 侧的职责划分：
//!   - 本模块只做「App 账号库 -> 网关凭证目录」的生成与更新；
//!   - 网关自身刷新 token 后写回的是它自己的 auths/，下次同步时会按
//!     expiresAt 较新者胜出的规则合并回来（见 `sync_auth_to_accounts`）。

use serde_json::{json, Value};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Mutex, OnceLock};

use crate::modules::account;
use crate::modules::config::{atomic_write, now_ms, store_dir};

// ---------------------------------------------------------------------------
// 路径与配置
// ---------------------------------------------------------------------------

/// 网关配置文件名（放在 ~/.wb-switch/ 下，与账号库同目录）。
const GATEWAY_CONFIG: &str = "gateway_config.json";

/// 上游网关在 /healthz 透出的身份标识（响应头 X-Service + 响应体 service 字段）。
///
/// 用途：宿主（本项目）托管网关子进程时，用它识别「同端口上的另一个服务」
/// 冒充应答造成的假启动成功。
const GATEWAY_SERVICE_NAME: &str = "workbuddy2api";
/// 网关凭证目录名（auths/）。
const GATEWAY_AUTH_DIR: &str = "gateway_auths";
/// 网关状态文件名（账号池冷却/熔断状态持久化）。
const GATEWAY_STATE_DIR: &str = "gateway_data";

/// 网关运行目录（~/.wb-switch/gateway）。
pub fn gateway_dir() -> PathBuf {
    store_dir().join("gateway")
}

/// 网关凭证目录。
pub fn gateway_auth_dir() -> PathBuf {
    gateway_dir().join(GATEWAY_AUTH_DIR)
}

/// 网关配置文件路径。
pub fn gateway_config_file() -> PathBuf {
    gateway_dir().join(GATEWAY_CONFIG)
}

/// 网关状态文件路径。
pub fn gateway_state_file() -> PathBuf {
    gateway_dir().join(GATEWAY_STATE_DIR).join("state.json")
}

/// 可执行文件名（Windows 带 .exe 后缀，macOS/Linux 不带）。
fn gateway_exe_name() -> &'static str {
    if cfg!(windows) {
        "gateway.exe"
    } else {
        "gateway"
    }
}

/// 定位网关可执行文件。
///
/// 优先顺序：
///   1. 环境变量 AI_GATEWAY_ROUTER_BIN（显式指定，便于开发/替换）
///   2. 内嵌版本（单文件分发：解压到 ~/.wb-switch/gateway/bin/）
///   3. 与本程序同目录的 gateway.exe（自定义/调试用）
///   4. ~/.wb-switch/gateway/ 下的 gateway.exe
///
/// 内嵌版本优先于「同目录文件」，保证单文件分发场景下运行的是
/// 与主程序同版本、同源码构建的网关，不会误用同目录里遗留的旧文件。
pub fn resolve_gateway_exe() -> Option<PathBuf> {
    if let Ok(p) = std::env::var("AI_GATEWAY_ROUTER_BIN") {
        let pb = PathBuf::from(p);
        if pb.is_file() {
            return Some(pb);
        }
    }

    if crate::modules::gateway_embed::has_embedded() {
        match crate::modules::gateway_embed::materialize() {
            Ok(path) => {
                crate::modules::gateway_embed::cleanup_old();
                return Some(path);
            }
            Err(e) => {
                // 解压失败不应让网关功能静默消失：退回外部文件路径再试
                eprintln!("[gateway] 释放内嵌网关失败: {e}，尝试使用外部文件");
            }
        }
    }

    if let Ok(self_exe) = std::env::current_exe() {
        if let Some(dir) = self_exe.parent() {
            for cand in [dir.join(gateway_exe_name()), dir.join("gateway").join(gateway_exe_name())] {
                if cand.is_file() {
                    return Some(cand);
                }
            }
        }
    }
    let in_store = gateway_dir().join(gateway_exe_name());
    if in_store.is_file() {
        return Some(in_store);
    }
    None
}

/// 当前网关可执行文件的来源描述（供前端诊断展示）。
pub fn gateway_source() -> &'static str {
    if std::env::var("AI_GATEWAY_ROUTER_BIN").is_ok_and(|p| PathBuf::from(p).is_file()) {
        return "env";
    }
    if crate::modules::gateway_embed::has_embedded() {
        return "embedded";
    }
    "external"
}

/// 默认网关配置（首次启动时落盘，之后由用户在设置页修改）。
pub fn default_gateway_config() -> Value {
    json!({
        "enabled": false,
        "port": 7863,
        "listen": ":7863",
        "api_key": "",
        "auto_start": false,
        // 网关工作模式：
        //   "balance" —— 负载均衡：账号池按三因子加权随机选号（默认）
        //   "pinned"  —— 指定账号：只使用 pinned_uid 对应的那一个账号
        "mode": "balance",
        "pinned_uid": null,
        // 「限制使用的模型」白名单：空数组 = 不限制（**默认是全部**）。
        //
        // 为什么默认必须是空：这是**新增能力**，老配置里没有这个键。
        // 默认成任何非空名单都会让既有用户升级后被静默限制住。
        // 三个工作模式共用同一份名单（见 write_native_config）。
        "allowed_model": [],
        "last_status": null,
        "last_error": null,
        // ---- 4 个自动养号任务的排程（写进网关的 schedule 块，见 write_native_config）----
        //
        // 默认值必须与 Go 侧 cmd/server/config.go 的 Default() 逐字一致：
        // 这里是界面上的初值，那边是「键缺席」时的兜底。两边不一致时，
        // 用户「不改任何东西直接保存」就会把网关排程改成另一套时刻。
        "activity_hours": [10],
        "nightowl_hours": [1],
        "school_hours": [12],
        "trial_hours": [9, 21],
        "activity_enabled": true,
        "nightowl_enabled": true,
        "school_enabled": true,
        "trial_enabled": true,
        "activity_report_count": 3,
        "checkin_enabled": true,
        "keepalive_enabled": true,
        "checkin_scope": "cn",
        // ---- 自定义系统提示词（转写进网关 config.json 的 prompt 块）----
        //
        // 默认必须是 passthrough（透传客户端原始 system）：这是**新增能力**，
        // 老配置里没有这两个键。缺省成 custom 的话，既有用户升级后 system 会被
        // 静默替换 —— 人设、项目约定、工具说明全丢，且从请求上看不出是网关动的手。
        // 保守缺省 + 显式开启，用户改配置时才知道自己换掉了什么。
        "prompt_mode": "passthrough",
        // 自定义提示词文件路径；空 = 用网关内置默认提示词（Go 侧 prompt.Load 回落）。
        // 刻意不在宿主侧 substitute 默认路径：两边各有一份默认值迟早会分叉，
        // 而分叉表现为「界面显示的路径与实际加载的不是同一个」。
        "prompt_file": "",
    })
}

/// 把 `patch` 合并到 `base` 之上（浅合并，只覆盖出现的键）。
fn overlay(base: Value, patch: &Value) -> Value {
    let mut out = base;
    if let Some(map) = patch.as_object() {
        if !out.is_object() {
            out = json!({});
        }
        let target = out.as_object_mut().unwrap();
        for (k, v) in map {
            target.insert(k.clone(), v.clone());
        }
    }
    out
}

/// 补齐缺省字段并规范化。
fn finalize_gateway_config(mut cfg: Value) -> Value {
    // port 与 listen 保持一致：以 port 为权威字段（前端只让用户填端口数字）。
    // 兼容老配置里只有 listen 的情况。
    let port = cfg
        .get("port")
        .and_then(Value::as_u64)
        .map(|p| p as u16)
        .filter(|p| *p > 0)
        .unwrap_or_else(|| {
            let s = cfg.get("listen").and_then(Value::as_str).unwrap_or("");
            if s.trim().is_empty() {
                7863
            } else {
                port_of(s)
            }
        });
    cfg["port"] = json!(port);
    cfg["listen"] = json!(normalize_listen(port));
    // 「限制使用的模型」统一成**数组**形状。
    //
    // 老配置里这个键是单值字符串（实测所有者本机的 gateway_config.json 就是
    // `"allowed_model": "deepseek-v4.1-flash"`）。在这里归一化的收益：
    //   - `/api/gateway/config` 与 `/api/gateway/status.config` 的出口形状恒定，
    //     界面不必为「这次拿到的是字符串还是数组」分两条渲染路径；
    //   - 任何一次保存（哪怕只是切个 auto_start 开关）都会把磁盘上的老形状
    //     顺手升级成数组，配置随时间自然收敛，不需要单独的迁移步骤。
    //
    // 读取侧仍必须吃字符串（Go 侧 `AllowedModels.UnmarshalJSON`、前端
    // `normalizeAllowedModels`）：配置文件也可能被直接编辑，或由老版本宿主写入，
    // 归一化只保证「经过本函数之后」的形状。
    cfg["allowed_model"] = allowed_models_of(&cfg);
    cfg
}

/// 读取磁盘上已有的配置（不补默认值）；文件不存在或损坏时返回 None。
fn read_existing_config() -> Option<Value> {
    let text = std::fs::read_to_string(gateway_config_file()).ok()?;
    serde_json::from_str::<Value>(&text).ok().filter(Value::is_object)
}

/// 合并配置：以「默认值 < 磁盘现有配置 < 传入 patch」的顺序覆盖。
///
/// 关键点：必须以**磁盘现有配置**为基准，而不是只用默认值。
/// 否则像「启动后回写 last_status」这类局部更新会把用户设置的
/// listen / api_key 一起重置为默认值，导致界面与网关实际配置不一致
///（曾经因此让账号池查询带上空 api_key，前端显示异常）。
fn merge_gateway_config(input: &Value) -> Value {
    let base = match read_existing_config() {
        Some(existing) => overlay(default_gateway_config(), &existing),
        None => default_gateway_config(),
    };
    finalize_gateway_config(overlay(base, input))
}

/// 读取网关配置（缺字段自动补默认值）。
pub fn load_gateway_config() -> Value {
    let f = gateway_config_file();
    if f.exists() {
        if let Ok(text) = std::fs::read_to_string(&f) {
            if let Ok(v) = serde_json::from_str::<Value>(&text) {
                return merge_gateway_config(&v);
            }
        }
    }
    default_gateway_config()
}

/// 只更新运行态字段（last_status / last_error），不触碰用户配置。
///
/// 启动/停止流程用它记录结果；避免把监听地址、api_key 等设置写坏。
pub fn update_runtime_state(last_status: &str, last_error: Option<String>) {
    let patch = json!({
        "last_status": last_status,
        "last_error": last_error,
    });
    let _ = save_gateway_config(&patch);
}

/// 保存网关配置；未提供的字段沿用磁盘上的现有值。
pub fn save_gateway_config(cfg: &Value) -> Result<Value, String> {
    let merged = merge_gateway_config(cfg);
    std::fs::create_dir_all(gateway_dir()).map_err(|e| e.to_string())?;
    let text = serde_json::to_string_pretty(&merged).map_err(|e| e.to_string())?;
    atomic_write(&gateway_config_file(), &text).map_err(|e| e.to_string())?;
    Ok(merged)
}

// ---------------------------------------------------------------------------
// 账号桥接：账号库 -> 网关凭证
// ---------------------------------------------------------------------------

/// 把毫秒时间戳转成网关要求的「秒」。
fn to_sec(ms: i64) -> i64 {
    if ms <= 0 {
        0
    } else if ms > 1_000_000_000_000 {
        ms / 1000
    } else {
        ms
    }
}

/// 为单个账号生成网关凭证 JSON（嵌套形，与 internal/auth.Parse 对齐）。
///
/// `credit` 参数是上一次导出/网关回写留下的积分到期元数据，原样透传。
/// 必须保留：它由网关的积分巡检写入，是「按到期紧迫度分层选号」的依据；
/// 若这里丢掉，每次账号同步都会把依据抹掉一次（表现为分层均衡时灵时不灵）。
fn build_auth_doc(acc: &Value, credit: Option<Value>) -> Option<(String, String)> {
    let uid = account::get_str(acc, "uid")?;
    let access = account::get_str(acc, "access_token")?;
    let refresh = account::get_str(acc, "refresh_token").unwrap_or_default();
    let nickname = account::get_str(acc, "nickname").unwrap_or_default();
    let enterprise = account::get_str(acc, "enterpriseId").unwrap_or_default();
    let domain = account::get_str(acc, "domain").unwrap_or_default();
    let expires_ms = acc.get("expiresAt").and_then(Value::as_i64).unwrap_or(0);

    let mut doc = json!({
        "auth": {
            "accessToken": access,
            "refreshToken": refresh,
            "expiresAt": to_sec(expires_ms),
            "domain": domain,
        },
        "account": {
            "uid": uid,
            "enterpriseId": enterprise,
            "nickname": nickname,
        },
    });
    if let Some(c) = credit {
        if c.is_object() {
            doc["credit"] = c;
        }
    }
    let file = format!("workbuddy-{uid}.json");
    Some((file, serde_json::to_string_pretty(&doc).ok()?))
}

/// 读取凭证文件里已有的 credit 块（网关积分巡检写入）；无则返回 None。
fn read_credit_block(path: &std::path::Path) -> Option<Value> {
    let text = std::fs::read_to_string(path).ok()?;
    let doc: Value = serde_json::from_str(&text).ok()?;
    doc.get("credit").filter(|v| v.is_object()).cloned()
}

/// 计算账号库的内容指纹：只要 uid + token + 过期时间有变化就算「脏」。
///
/// 用它做增量判断，避免每分钟无条件重写整个凭证目录
/// （写盘 + 触发网关无谓重启都会有代价）。
pub fn accounts_fingerprint() -> String {
    let accounts = account::load_accounts();
    let mut parts: Vec<String> = accounts
        .iter()
        .filter_map(|a| {
            let uid = account::get_str(a, "uid")?;
            let at = account::get_str(a, "access_token").unwrap_or_default();
            let rt = account::get_str(a, "refresh_token").unwrap_or_default();
            let exp = a.get("expiresAt").and_then(Value::as_i64).unwrap_or(0);
            // token 只取尾部若干字符参与指纹：避免把完整凭证写进日志/内存字符串
            let at_tail: String = at.chars().rev().take(12).collect();
            let rt_len = rt.len();
            // 需重登标记必须参与指纹：它的变化会改变「哪些账号该导出」，
            // 若不计入，标记翻转后 `sync_if_changed` 会认为无事发生而不重推凭证。
            let relogin = if needs_relogin(a) { 1 } else { 0 };
            Some(format!(
                "{uid}|{at_tail}|{rt_len}|{}|{relogin}",
                to_sec(exp)
            ))
        })
        .collect();
    parts.sort();
    let mut hash: u64 = 0xcbf2_9ce4_8422_2325;
    for p in &parts {
        for b in p.as_bytes() {
            hash ^= *b as u64;
            hash = hash.wrapping_mul(0x1000_0000_01b3);
        }
    }
    format!("{}-{:x}", parts.len(), hash)
}

/// 上次成功推送时的账号库指纹。
static LAST_FINGERPRINT: Mutex<Option<String>> = Mutex::new(None);

/// 仅当账号库有变化时才推送到网关（供后台定时任务调用）。
///
/// 返回是否发生了变化。这样新增账号能自动进入网关凭证目录，
/// 用户不必手动点「立即同步」。
pub fn sync_if_changed() -> bool {
    let fp = accounts_fingerprint();
    {
        let guard = LAST_FINGERPRINT.lock().unwrap();
        if guard.as_deref() == Some(fp.as_str()) {
            return false;
        }
    }
    match export_accounts_to_gateway() {
        Ok(_) => {
            *LAST_FINGERPRINT.lock().unwrap() = Some(fp);
            true
        }
        Err(e) => {
            eprintln!("[gateway] 自动同步账号失败: {e}");
            false
        }
    }
}

// ---------------------------------------------------------------------------
// 任务执行期间的互斥：自动同步不得打断在途任务
// ---------------------------------------------------------------------------
//
// 缺陷背景（所有者反馈：「立即执行」弹出
// `error sending request for url (http://127.0.0.1:7864/tasks/run)`）：
//
//   `run_task_now` 是一次**同步**等待的 HTTP 请求，而任务本身按
//   「账号数 × 条数 × 800ms」串行跑（实测：19 个账号的活跃上报 41.7s、
//   开学季 9.3s）。同一时刻 `run_auto_sync_loop` 每 30s 就可能在账号库指纹
//   变化时 `stop_gateway()` + 重启 —— 那会**切断在途连接**，宿主侧 reqwest
//   于是报出上面那条传输层错误（不是 HTTP 状态码错误）。
//
//   更要紧的是它会**自激**：养号任务调上游会触发 token 刷新并写回账号库，
//   指纹（含 expiresAt / access_token 尾部）随之变化，下一个周期就把网关重启掉。
//   即「用户点一次立即执行」与「自动同步」互相触发，撞上就失败。
//
// 修法：任务执行期间置「忙」标志，自动同步**推迟**本轮重启（下一轮再试），
// 并设推迟上限防止无限期不生效。注意标志必须在所有退出路径上复位，
// 因此用 RAII 守卫而不是手工 set/clear（见 `TaskBusyGuard`）。

/// 正在执行的养号任务数（**计数**而非布尔）。
///
/// 为什么是计数：`run_task_now` 可以被并发调用（设置页与账号菜单各有一个入口，
/// 用户可能先后触发两个不同任务）。用布尔时先结束的那个会把标志清掉，
/// 另一个仍在执行的任务就重新暴露在「自动同步重启」的窗口里 ——
/// 正是本标志要消除的那个竞态。
static TASK_BUSY_COUNT: AtomicUsize = AtomicUsize::new(0);

/// 自动同步已连续推迟的轮数。
static SYNC_DEFER_ROUNDS: AtomicUsize = AtomicUsize::new(0);

/// 「已检测到账号变化、但还没通过重启生效」的待办标志。
///
/// **必须单独记这个标志**，这是本修复最容易写错的一处：
/// `sync_if_changed()` 在检测到变化的**当轮就把指纹更新掉**，因此「本轮跳过重启」
/// 之后，下一轮它返回 false —— 如果只靠返回值判断，重启就永远不会发生，
/// 自动同步会静默停摆（账号变更长时间不生效，正是需求明确禁止的）。
/// 用待办标志把「已经检测到变化」与「还没重启」分开记，推迟才真的只是推迟。
static SYNC_RESTART_PENDING: AtomicBool = AtomicBool::new(false);

/// 任务忙时最多推迟的自动同步轮数（一轮 = interval 秒）。
///
/// 取值理由：间隔 30s，10 轮 = 最多推迟 5 分钟。实测最长的活跃上报在 19 个账号下
/// 41.7s（约 2 轮），实时上报按「账号数 × 条数 × 800ms」串行，账号更多的用户
/// 会明显更久。给到 5 分钟既能覆盖正常任务，又不会让「新增账号」这类变更
/// 被无限期压住 —— 超过上限就强制执行，宁可打断一次任务也不能让同步停摆。
const MAX_SYNC_DEFER_ROUNDS: usize = 10;

/// 自动同步某一轮该做什么。
///
/// 抽成纯函数是为了让「忙时推迟 / 超限强制 / 无变更时空转」三条分支可被单测直接覆盖
/// —— 真正的循环会启停网关子进程，不适合在单测里跑。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum SyncAction {
    /// 无需重启（没有待生效的变更，或网关没在跑）。
    Idle,
    /// 有变更但任务正在执行：本轮推迟，下轮再看。
    Defer,
    /// 执行重启（无任务在跑，或推迟已达上限）。
    Restart,
}

/// 决定自动同步本轮的动作。判定顺序即优先级，不可调换：
///   1. 没有待生效的变更 → 空转（绝大多数轮次走这里）
///   2. 网关没在跑 → 无需重启（下次启动自然读到新凭证）
///   3. 任务在跑且未达上限 → 推迟（**不重启**，避免切断在途请求）
///   4. 其余（含推迟超限）→ 重启
fn decide_sync_action(
    pending_restart: bool,
    gateway_running: bool,
    task_running: bool,
    deferred_rounds: usize,
    max_defer_rounds: usize,
) -> SyncAction {
    if !pending_restart {
        return SyncAction::Idle;
    }
    if !gateway_running {
        return SyncAction::Idle;
    }
    if task_running && deferred_rounds < max_defer_rounds {
        return SyncAction::Defer;
    }
    SyncAction::Restart
}

/// 养号任务标识 → 界面中文名。
///
/// 必须与 Go 侧写账号记录用的标题**逐字一致**（`scheduler/activity.go` 的
/// 「活跃上报」等）：进度是从统一事件流里按标题数出来的，两边文案一旦分叉，
/// 进度就恒为 0 且不会有任何报错。
pub fn task_label(task: &str) -> &'static str {
    match task.trim() {
        "activity" => "活跃上报",
        "nightowl" => "夜猫子任务",
        "school" => "开学季活动",
        "trial" => "trial 加油包",
        _ => "养号任务",
    }
}

/// 任务本轮预计遍历的账号数（进度分母）。
///
/// 口径刻意与 Go 侧各任务的过滤条件对齐（见 `scheduler/activity.go`、
/// `trial.go`、`school.go`）：非禁用、非「需重登」、有 access token，
/// 再按区域筛（活跃上报 / 夜猫子 / 开学季默认只跑国服，trial 只跑国际版；
/// `checkin_scope=all` 时前三个放开）。
///
/// 这是**预计值**：网关账号池由导出的凭证文件建立，与账号库存在极小的时差
/// （刚授权/刚禁用的账号可能还没同步过去）。用作进度分母足够，
/// 界面文案也据此写成「已记录 N / M」而不是断言性的「已完成」。
fn task_target_total(task: &str) -> usize {
    let scope_all = load_gateway_config()
        .get("checkin_scope")
        .and_then(Value::as_str)
        .unwrap_or("cn")
        .eq_ignore_ascii_case("all");
    let want_intl = task.trim() == "trial";
    account::load_accounts()
        .iter()
        .filter(|a| !needs_relogin(a))
        .filter(|a| !account::account_disabled(a))
        .filter(|a| account::get_str(a, "access_token").is_some())
        .filter(|a| {
            let intl = crate::modules::config::Region::of(a) == crate::modules::config::Region::Intl;
            if want_intl { intl } else { scope_all || !intl }
        })
        .count()
}

/// 任务开始后，统一事件流里已留下记录的账号 id 列表（进度分子 + 逐卡片标记）。
///
/// 为什么从 `account_records.json` 数而不是在宿主另记一份进度：
/// Go 侧每个任务在**处理完一个账号后**就会写一条账号记录（`records.Recorder`），
/// 那本来就是「跑到哪了」的唯一真实来源。另立一套进度账本等于把同一件事记两遍，
/// 两边的口径迟早会分叉（而且分叉时不会有任何报错）。
///
/// 按 accountId 去重：个别任务对同一账号可能写多条（如「领取失败」逐条写），
/// 直接数记录条数会虚高，得出「已记录 30/19 个账号」这种自相矛盾的进度。
fn task_processed_ids(title: &str, since_ms: i64) -> Vec<String> {
    use crate::modules::account_records;
    let snapshot = account_records::query_records(
        "",
        since_ms,
        0,
        &[account_records::KIND_TASK.to_string()],
        0,
    );
    let mut ids: Vec<String> = snapshot
        .get("records")
        .and_then(Value::as_array)
        .map(|items| {
            items
                .iter()
                .filter(|r| r.get("title").and_then(Value::as_str) == Some(title))
                // 汇总记录（accountId 为空，如「活动不在期」）不代表任何一个账号，
                // 计进去会让进度凭空 +1。
                .filter_map(|r| r.get("accountId").and_then(Value::as_str))
                .filter(|id| !id.is_empty())
                .map(str::to_string)
                .collect()
        })
        .unwrap_or_default();
    ids.sort_unstable();
    ids.dedup();
    ids
}

/// 正在执行的任务的运行态（供界面显示「在跑什么、跑到哪了」）。
#[derive(Debug, Clone)]
struct TaskRuntimeState {
    task: String,
    label: String,
    started_at: i64,
    total: usize,
}

static TASK_RUNTIME: Mutex<Option<TaskRuntimeState>> = Mutex::new(None);

/// 任务忙标志的 RAII 守卫：置位即计数 +1，Drop 即 -1。
///
/// 为什么必须是 RAII 而不是手工配对 set/clear：`run_task_now` 有多条提前返回路径
/// （任务名为空、HTTP 客户端构造失败、网关未启动…），将来还会有更多。
/// 手工复位漏掉任意一条，自动同步就会**永久停摆**（`decide_sync_action` 恒返回 Defer，
/// 超过上限后变成每轮都重启 —— 两种都是故障），而且 panic 时更是必然漏掉。
/// 交给 Drop 则「正常返回、提前 return、panic 展开」三种路径自动覆盖。
struct TaskBusyGuard;

impl TaskBusyGuard {
    fn acquire(task: &str) -> Self {
        let label = task_label(task);
        TASK_BUSY_COUNT.fetch_add(1, Ordering::SeqCst);
        *TASK_RUNTIME.lock().unwrap_or_else(|e| e.into_inner()) = Some(TaskRuntimeState {
            task: task.trim().to_string(),
            label: label.to_string(),
            started_at: now_ms(),
            total: task_target_total(task),
        });
        Self
    }
}

impl Drop for TaskBusyGuard {
    fn drop(&mut self) {
        let _prev = TASK_BUSY_COUNT.fetch_sub(1, Ordering::SeqCst);
        *TASK_RUNTIME.lock().unwrap_or_else(|e| e.into_inner()) = None;
        // **刻意不在这里清零 SYNC_DEFER_ROUNDS**。
        //
        // 推迟额度属于「这次待生效的账号变更」，不属于某个具体任务。
        // 若在任务结束时就清零，连续点几次「立即执行」就能把额度一次次续满，
        // 上限形同虚设 —— 那正是需求要防的「无限期推迟自动同步」。
        //
        // 额度由 `apply_pending_restart` 统一管理：只要待办还在，
        // 要么继续累加（任务在跑），要么在 Restart/Idle 分支一并清零。
        // 任务结束后网关即空闲，下一轮 tick 走 Restart 分支，额度自然归零。
    }
}

/// 是否有养号任务正在执行（自动同步据此推迟重启）。
pub fn task_busy() -> bool {
    TASK_BUSY_COUNT.load(Ordering::SeqCst) > 0
}

/// 当前运行态快照（供 `gateway_status` 透出给界面）。
///
/// 无任务时只回 `{ running: false }`：界面据 `running` 分支，
/// 不给它一堆空字段去猜（那正是「看不到在跑什么」的成因之一）。
///
/// `processedIds` 是**宿主账号库的 id**（不是网关 uid）：
/// 界面按 `AccountMeta.id` 给卡片打标记，用 uid 会一个都对不上，
/// 且不会有任何报错（与 `gateway_account_identities` 是同一口径）。
pub fn task_runtime() -> Value {
    // 先把状态**克隆出来再放锁**：算进度要读并解析整个 account_records.json
    // （上限 2 万条），持锁做文件 I/O 会让同刻想置位/复位的任务干等。
    let state = {
        let guard = TASK_RUNTIME.lock().unwrap_or_else(|e| e.into_inner());
        guard.clone()
    };
    let Some(st) = state else {
        return json!({ "running": false });
    };
    let processed_ids = task_processed_ids(&st.label, st.started_at);
    json!({
        "running": true,
        "task": st.task,
        "label": st.label,
        "startedAt": st.started_at,
        "elapsedMs": (now_ms() - st.started_at).max(0),
        "total": st.total,
        "processed": processed_ids.len(),
        "processedIds": processed_ids,
    })
}

/// 把「有待生效的变更」落成实际重启（受忙标志与推迟上限约束）。
async fn apply_pending_restart() {
    let action = decide_sync_action(
        SYNC_RESTART_PENDING.load(Ordering::SeqCst),
        is_running(),
        task_busy(),
        SYNC_DEFER_ROUNDS.load(Ordering::SeqCst),
        MAX_SYNC_DEFER_ROUNDS,
    );
    match action {
        SyncAction::Idle => {
            // 网关没在跑时待办已无意义（下次启动自然是新凭证），清掉避免
            // 它一直挂着、等网关被用户手动启动后立刻吃一发无谓的重启。
            SYNC_RESTART_PENDING.store(false, Ordering::SeqCst);
            SYNC_DEFER_ROUNDS.store(0, Ordering::SeqCst);
        }
        SyncAction::Defer => {
            let round = SYNC_DEFER_ROUNDS.fetch_add(1, Ordering::SeqCst) + 1;
            // 留日志：这是「账号变更为什么没立刻生效」的唯一线索，
            // 没有它，用户只会看到同步莫名其妙地慢了几分钟。
            eprintln!(
                "[gateway] 养号任务执行中，本轮推迟自动重启（{round}/{MAX_SYNC_DEFER_ROUNDS}），\
                 避免切断在途任务请求"
            );
        }
        SyncAction::Restart => {
            SYNC_DEFER_ROUNDS.store(0, Ordering::SeqCst);
            SYNC_RESTART_PENDING.store(false, Ordering::SeqCst);
            let cfg = load_gateway_config();
            stop_gateway();
            match start_gateway(&cfg).await {
                Ok(_) => eprintln!("[gateway] 检测到账号变化，已自动重启网关以加载新账号"),
                Err(e) => eprintln!("[gateway] 账号变化后重启网关失败: {e}"),
            }
        }
    }
}

/// 后台自动同步：账号库变化 → 推送凭证；网关运行中且账号有变动 → 重启使新账号生效。
///
/// 由 GUI / server 的启动流程调用，永续运行。
///
/// 重启一律经 `apply_pending_restart`：那里统一处理「任务在跑就先别重启」，
/// 本函数不再自己调 stop/start（分散写会让忙标志被绕过）。
pub async fn run_auto_sync_loop(interval_secs: u64) {
    let interval = std::time::Duration::from_secs(interval_secs.max(5));
    // 启动先同步一次，保证首屏即是最新
    if sync_if_changed() {
        SYNC_RESTART_PENDING.store(true, Ordering::SeqCst);
    }
    apply_pending_restart().await;
    loop {
        tokio::time::sleep(interval).await;
        // 注意：不能写成 `if !sync_if_changed() { continue; }` ——
        // 指纹在检测到变化的当轮就被更新了，被推迟的重启需要靠待办标志活到下一轮。
        if sync_if_changed() {
            SYNC_RESTART_PENDING.store(true, Ordering::SeqCst);
        }
        apply_pending_restart().await;
    }
}

/// 从候选 uid 中筛出「应当导出到网关」的集合。
///
/// `only` 为 None 表示不筛选（负载均衡，全部导出）；
/// 为 Some(set) 表示只保留 set 内的（指定账号）。
///
/// 抽成纯函数是为了让「导出」与「清理」共用同一判定 —— 二者一旦分叉，
/// 切换模式时就会出现残留凭证（网关仍把旧账号加载进池，表现为切换无效）。
fn select_export_uids(candidates: &[String], only: &Option<Vec<String>>) -> Vec<String> {
    match only {
        None => candidates.to_vec(),
        Some(allowed) => candidates
            .iter()
            .filter(|u| allowed.iter().any(|a| a == *u))
            .cloned()
            .collect(),
    }
}

/// 网关工作模式。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum GatewayMode {
    /// 自动（负载均衡）：账号池加权随机选号，自动避开冷却/熔断的账号；
    /// 全部（未禁用的）账号参与。
    Balance,
    /// 手动：只使用用户勾选的账号，池子内部仍按到期日分层 + 加权随机选号。
    ///
    /// 与旧的「指定账号（Pinned）」的关系：手动模式是它的推广 ——
    /// 勾一个账号时行为与旧「指定账号」等价，勾多个则在勾选集合内均衡。
    /// 旧配置里的 `pinned_uid` 会被当作「只勾了那一个」读取，无需迁移。
    Manual,
    /// 单一模型 + 积分轮转：只用一个账号烧到不可用，再换按到期日排序的下一个。
    ///
    /// 与手动模式的关键区别：**轮转不看勾选列表**（除非同时处于手动模式）——
    /// 它的「换下一个」依赖备选账号都在池里，只导出一个是转不起来的。
    /// 区别在网关侧的选择策略（pool.rotation），而不在凭证范围。
    Rotation,
}

impl GatewayMode {
    pub fn as_str(&self) -> &'static str {
        match self {
            GatewayMode::Balance => "balance",
            GatewayMode::Manual => "manual",
            GatewayMode::Rotation => "rotation",
        }
    }

    pub fn from_str(s: &str) -> Self {
        match s.trim().to_lowercase().as_str() {
            // 兼容旧值："pinned" 是手动模式的单账号特例，读作手动即可
            "manual" | "pinned" | "pin" | "single" => GatewayMode::Manual,
            // 兼容几种自然叫法：轮转 / 单一模型轮转
            "rotation" | "rotate" | "rolling" => GatewayMode::Rotation,
            _ => GatewayMode::Balance,
        }
    }
}

/// 读取当前网关模式。
pub fn gateway_mode() -> GatewayMode {
    GatewayMode::from_str(&load_gateway_config()
        .get("mode").and_then(Value::as_str).unwrap_or("balance"))
}

/// 读取「指定账号」模式锁定的单个 uid（**旧字段，仅为向后兼容保留**）。
///
/// 新代码请用 `manual_uids()`：手动模式支持勾选多个账号，
/// 单个 uid 只是「只勾了一个」的特例。
pub fn pinned_uid() -> Option<String> {
    let cfg = load_gateway_config();
    // 注意：必须先把配置绑定到变量，否则临时值在语句结束即被释放（E0716）
    let s = cfg.get("pinned_uid")?.as_str()?.trim();
    if s.is_empty() { None } else { Some(s.to_string()) }
}

/// 手动模式下用户勾选的账号 uid 列表。
///
/// 读取顺序（保证旧配置无需迁移即可继续工作）：
///   1. `manual_uids` 数组（新字段）
///   2. `pinned_uid` 单值（旧字段）—— 旧版「指定账号」模式只锁一个账号，
///      读作「只勾了那一个」语义完全一致
///
/// 返回空列表表示「没勾任何账号」，调用方应视为配置不完整并提示用户，
/// 而不是悄悄放行全部账号（那会让「手动」模式静默退化成「自动」）。
pub fn manual_uids() -> Vec<String> {
    let cfg = load_gateway_config();
    if let Some(items) = cfg.get("manual_uids").and_then(Value::as_array) {
        return items
            .iter()
            .filter_map(Value::as_str)
            .map(str::trim)
            .filter(|s| !s.is_empty())
            .map(|s| s.to_string())
            .collect();
    }
    pinned_uid().into_iter().collect()
}

/// 当前实际参与网关的账号 uid 集合。
///
/// 自动（Balance）：全部账号；手动（Manual）：仅勾选的账号。
/// 网关依据 `auths/` 目录里的凭证文件建立账号池，
/// 因此「只导出勾选的账号」即可实现手动模式，同时保留熔断/冷却/粘性等能力。
///
/// 轮转模式（Rotation）刻意**不过滤**：它的「换下一个」依赖备选账号都在池里，
/// 只导出一个是转不起来的。
fn active_uids() -> Option<Vec<String>> {
    match gateway_mode() {
        // None = 不过滤，全部导出
        GatewayMode::Balance | GatewayMode::Rotation => None,
        GatewayMode::Manual => Some(manual_uids()),
    }
}

/// 网关是否应启用「单一模型 + 积分轮转」选号策略（写入 native config 的 pool.rotation）。
pub fn rotation_enabled() -> bool {
    matches!(gateway_mode(), GatewayMode::Rotation)
}

/// 账号是否已被标记为「需重新登录」。
///
/// 复用 `refresh::needs_relogin`：它会排除旧版本因传输层失败留下的误报标记
///（`code=-1` 并不代表凭证失效），避免这些账号被永久排除出网关池。
pub use crate::modules::refresh::needs_relogin;

/// 账号库 -> 网关凭证目录。返回 (账号数, 有变化的 uid 列表)。
pub fn export_accounts_to_gateway() -> Result<(usize, Vec<String>), String> {
    export_accounts_to_dir(&gateway_auth_dir(), &account::load_accounts(), active_uids())
}

/// `export_accounts_to_gateway` 的可注入目录版本（便于单测验证真实文件增删）。
///
/// `only` 语义见 `select_export_uids`：None=不过滤（负载均衡），Some=仅这些 uid。
pub fn export_accounts_to_dir(
    dir: &std::path::Path,
    accounts: &[Value],
    only: Option<Vec<String>>,
) -> Result<(usize, Vec<String>), String> {
    std::fs::create_dir_all(dir).map_err(|e| e.to_string())?;
    let mut changed = Vec::new();
    let mut written = 0usize;

    // 「是否导出某账号」只在这里定义一次，导出与清理共用。
    // 分开写会导致切换模式时判定不一致（曾被此坑到：清理用账号库全集，
    // 从负载均衡切到指定账号后其余凭证残留，网关仍把它们加载进池）。
    //
    // 需重登的账号同样不导出：它的 refresh token 已被服务端拒绝，
    // 留在池里只会让每次请求白跑一轮（实测：网关无视该标记持续使用失效账号）。
    // 重新登录成功后 `needs_relogin` 被清除，下次同步会自动把它放回池中。
    let should_export = |acc: &Value| -> bool {
        if needs_relogin(acc) {
            return false;
        }
        // 用户手动禁用的账号不进池：这是显式意图，优先级高于任何模式选择
        //（即使它出现在手动模式的勾选列表里也不导出 —— 否则「禁用」会被模式覆盖，
        // 用户会看到「明明禁用了却还在接流量」）。
        // 注意：只影响网关池；签到 / 旅行 / 上报等养号任务照跑
        //（与所有者确认的语义：禁用 ≠ 停止养号）。
        if crate::modules::account::account_disabled(acc) {
            return false;
        }
        let uid = account::get_str(acc, "uid").unwrap_or_default();
        !select_export_uids(&[uid], &only).is_empty()
    };

    for acc in accounts {
        if !should_export(acc) {
            continue;
        }
        let Some((file, _)) = build_auth_doc(acc, None) else { continue };
        let path = dir.join(&file);
        // 保留网关写入的 credit 元数据（积分到期分层选号的依据）。
        let credit = read_credit_block(&path);
        let Some((_, text)) = build_auth_doc(acc, credit) else { continue };
        // 内容一致则不写盘，避免无意义的文件时间戳变动
        if let Ok(existing) = std::fs::read_to_string(&path) {
            if existing.trim() == text.trim() {
                written += 1;
                continue;
            }
        }
        atomic_write(&path, &text).map_err(|e| e.to_string())?;
        if let Some(uid) = account::get_str(acc, "uid") {
            changed.push(uid);
        }
        written += 1;
    }

    // 清理不再需要的凭证：账号库中已删除的、因模式切换而不再导出的、
    // 已被标记需重新登录的，以及被用户**手动禁用**的
    //（留着只会让网关持续把它加载进池）。
    //
    // 这里的过滤条件必须与上面 `should_export` **逐条对应** ——
    // 二者一旦分叉就会出现「导出时不写、清理时又保留」的残留凭证，
    // 网关仍会把旧账号加载进池，表现为「禁用/切换模式不生效」。
    // （本函数此前正是漏了 disabled 这一条：导出侧已加过滤，
    //   清理侧只滤了 needs_relogin，导致禁用账号的凭证文件永远留着。）
    let all_uids: Vec<String> = accounts
        .iter()
        .filter(|a| !needs_relogin(a))
        .filter(|a| !crate::modules::account::account_disabled(a))
        .filter_map(|a| account::get_str(a, "uid"))
        .collect();
    let live: Vec<String> = select_export_uids(&all_uids, &only)
        .iter()
        .map(|u| format!("workbuddy-{u}.json"))
        .collect();
    if let Ok(entries) = std::fs::read_dir(dir) {
        for ent in entries.flatten() {
            let name = ent.file_name().to_string_lossy().to_string();
            if name.starts_with("workbuddy") && name.ends_with(".json") && !live.contains(&name) {
                if std::fs::remove_file(ent.path()).is_ok() {
                    changed.push(format!("removed:{name}"));
                }
            }
        }
    }
    Ok((written, changed))
}

/// 网关凭证 -> 账号库：把网关刷新后的 token 回写账号库。
///
/// 规则：仅当网关侧 expiresAt（秒）比账号库侧（毫秒）更新时才回写；
/// 空 refresh_token 不覆盖已有值。返回回写的 uid 列表。
pub fn sync_auth_to_accounts() -> Result<Vec<String>, String> {
    let dir = gateway_auth_dir();
    if !dir.is_dir() {
        return Ok(Vec::new());
    }
    let mut accounts = account::load_accounts();
    let mut updated = Vec::new();

    for ent in std::fs::read_dir(&dir).map_err(|e| e.to_string())?.flatten() {
        let path = ent.path();
        if !path.is_file() {
            continue;
        }
        let Ok(text) = std::fs::read_to_string(&path) else { continue };
        let Ok(doc) = serde_json::from_str::<Value>(&text) else { continue };
        // 兼容嵌套形与扁平形
        let (auth, acct) = match (doc.get("auth"), doc.get("account")) {
            (Some(a), Some(b)) => (a.clone(), b.clone()),
            _ => (doc.clone(), doc.clone()),
        };
        let Some(uid) = account::get_str(&acct, "uid") else { continue };
        let Some(gw_at) = account::get_str(&auth, "accessToken") else { continue };
        let gw_exp_ms = auth.get("expiresAt").and_then(Value::as_i64).map(|s| {
            if s > 0 && s < 1_000_000_000_000 { s * 1000 } else { s }
        }).unwrap_or(0);

        let Some(idx) = accounts.iter().position(|a| account::get_str(a, "uid").as_deref() == Some(uid.as_str()))
        else {
            continue; // 网关侧新账号由「导入」流程显式处理，不静默注入
        };

        let cur_exp_ms = accounts[idx].get("expiresAt").and_then(Value::as_i64).unwrap_or(0);
        let cur_at = account::get_str(&accounts[idx], "access_token").unwrap_or_default();

        // 以秒为单位比较，避免 ms/s 精度差导致反复回写
        if to_sec(gw_exp_ms) <= to_sec(cur_exp_ms) && gw_at == cur_at {
            continue;
        }
        if to_sec(gw_exp_ms) < to_sec(cur_exp_ms) {
            continue; // 账号库更新，保持本地
        }

        accounts[idx]["access_token"] = json!(gw_at);
        if let Some(rt) = account::get_str(&auth, "refreshToken") {
            if !rt.is_empty() {
                accounts[idx]["refresh_token"] = json!(rt);
            }
        }
        if gw_exp_ms > 0 {
            accounts[idx]["expiresAt"] = json!(gw_exp_ms);
        }
        if let Some(d) = account::get_str(&auth, "domain") {
            if !d.is_empty() {
                accounts[idx]["domain"] = json!(d);
            }
        }
        accounts[idx]["refreshedAt"] = json!(now_ms());
        updated.push(uid);
    }

    if !updated.is_empty() {
        account::save_accounts(&accounts).map_err(|e| e.to_string())?;
    }
    Ok(updated)
}
// ---------------------------------------------------------------------------
// 进程托管
// ---------------------------------------------------------------------------

static GATEWAY_PROC: OnceLock<Mutex<Option<Child>>> = OnceLock::new();
static GATEWAY_RUNNING: AtomicBool = AtomicBool::new(false);

fn proc_slot() -> &'static Mutex<Option<Child>> {
    GATEWAY_PROC.get_or_init(|| Mutex::new(None))
}

// ---------------------------------------------------------------------------
// 孤儿进程防护：Windows Job Object
// ---------------------------------------------------------------------------

/// 网关子进程所属的 Job Object 包装。
///
/// 只靠 `stop_gateway()` 无法覆盖应用异常结束的路径（崩溃、任务管理器强杀、
/// 更新重启时的 `std::process::exit`）。这些情况下 Windows 不会回收子进程，
/// gateway.exe 会变成孤儿继续占用端口。Job 设置 `KILL_ON_JOB_CLOSE` 后，
/// 父进程无论以何种方式退出，系统在关闭句柄时都会连带终止 Job 内的子进程。
///
/// 句柄在进程存活期间一直持有；句柄关闭（含进程崩溃导致的系统自动关闭）
/// 即代表杀死网关。
#[cfg(windows)]
struct GatewayJob(windows::Win32::Foundation::HANDLE);

// 句柄是进程级资源，仅通过 Win32 API 使用，跨线程共享安全。
#[cfg(windows)]
unsafe impl Send for GatewayJob {}
#[cfg(windows)]
unsafe impl Sync for GatewayJob {}

#[cfg(windows)]
impl GatewayJob {
    /// 创建带 KILL_ON_JOB_CLOSE 限制的 Job；失败返回 None（不影响网关本身运行）。
    fn create() -> Option<Self> {
        use windows::Win32::System::JobObjects::{
            CreateJobObjectW, JobObjectExtendedLimitInformation, SetInformationJobObject,
            JOBOBJECT_EXTENDED_LIMIT_INFORMATION, JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
        };
        unsafe {
            let handle = CreateJobObjectW(None, windows::core::PCWSTR::null()).ok()?;
            let mut info = JOBOBJECT_EXTENDED_LIMIT_INFORMATION::default();
            info.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
            if SetInformationJobObject(
                handle,
                JobObjectExtendedLimitInformation,
                &info as *const _ as *const core::ffi::c_void,
                std::mem::size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>() as u32,
            )
            .is_err()
            {
                let _ = windows::Win32::Foundation::CloseHandle(handle);
                return None;
            }
            Some(Self(handle))
        }
    }

    /// 把网关子进程收进 Job。
    fn assign(&self, child: &Child) -> bool {
        use std::os::windows::io::AsRawHandle;
        use windows::Win32::Foundation::HANDLE;
        use windows::Win32::System::JobObjects::AssignProcessToJobObject;
        let process = HANDLE(child.as_raw_handle());
        unsafe { AssignProcessToJobObject(self.0, process).is_ok() }
    }
}

#[cfg(windows)]
impl Drop for GatewayJob {
    fn drop(&mut self) {
        // 显式关闭句柄：KILL_ON_JOB_CLOSE 在最后一个句柄关闭时终止 Job 内所有进程。
        // （进程存活期间单例 Job 不会被 Drop，句柄保持打开；进程结束时由系统关闭。）
        unsafe {
            let _ = windows::Win32::Foundation::CloseHandle(self.0);
        }
    }
}

/// 进程级单例 Job（创建一次，失败不重试）。
#[cfg(windows)]
fn gateway_job() -> Option<&'static GatewayJob> {
    static JOB: OnceLock<Option<GatewayJob>> = OnceLock::new();
    JOB.get_or_init(GatewayJob::create).as_ref()
}

/// 把新启动的网关子进程纳入 Job：应用进程结束时由系统连带回收。
///
/// 加入失败不阻断启动（显式 stop_gateway 仍可正常停止），只打日志提示兜底失效。
fn attach_child_to_job(child: &Child) {
    #[cfg(windows)]
    {
        match gateway_job() {
            None => eprintln!("[gateway] Job Object 创建失败：应用异常退出时可能残留网关进程"),
            Some(job) if !job.assign(child) => {
                eprintln!(
                    "[gateway] 网关子进程加入 Job Object 失败：应用异常退出时可能残留网关进程"
                )
            }
            Some(_) => {}
        }
    }
    #[cfg(not(windows))]
    {
        let _ = child;
    }
}

/// 网关是否在运行（本进程视角）。
pub fn is_running() -> bool {
    GATEWAY_RUNNING.load(Ordering::SeqCst)
}

/// 从监听地址解析端口，兼容多种写法：
/// `7863` / `:7863` / `0.0.0.0:7863` / `127.0.0.1:7863` / `[::]:7863`
pub fn port_of(listen: &str) -> u16 {
    let s = listen.trim();
    if s.is_empty() {
        return 7863;
    }
    // 纯数字
    if let Ok(p) = s.parse::<u16>() {
        return p;
    }
    s.rsplit(':')
        .next()
        .map(|p| p.trim().trim_end_matches(']'))
        .and_then(|p| p.parse::<u16>().ok())
        .filter(|p| *p > 0)
        .unwrap_or(7863)
}

/// 把端口规范化成网关可用的监听地址（`7863` → `:7863`）。
pub fn normalize_listen(port: u16) -> String {
    format!(":{port}")
}

/// 端口是否可绑定（对外暴露，供前端做占用检测）。
pub fn is_port_available(port: u16) -> bool {
    port > 0 && port_free(port)
}

/// 从 start 开始（含）向后找一个空闲端口，最多找 span 个。
pub fn find_free_port(start: u16, span: u16) -> Option<u16> {
    let begin = start.max(1024);
    for p in begin..begin.saturating_add(span.max(1)) {
        if port_free(p) {
            return Some(p);
        }
    }
    None
}

/// 端口占用检测结果（供前端即时反馈）。
///
/// **刻意不查占用进程**（`holder` 恒为 null）。
///
/// 原因：本函数跑在**同步 Tauri 命令**里（即主线程），而查占用者要 spawn
/// netstat + tasklist + powershell 三个控制台进程。页面一挂载就会调它，
/// 于是「打开页面」变成「主线程卡住数秒」—— 实测表现为界面未响应，
/// 并因频繁创建控制台进程耗尽 desktop heap，弹出
/// 「应用程序无法正常启动 (0xc0000142)」。
///
/// 查占用者改为**按需**触发：用户点了「结束占用进程」才走
/// `kill_port_holder` 的路径（见 `port_holder`），那时才值得付出进程开销。
pub fn inspect_port(port: u16) -> Value {
    // 注意：若网关自身正跑在该端口上，这里会判定为「被占用」。
    // 调用方（前端）需结合 status.running 判断，避免误报。
    let available = port > 0 && port_free(port);
    json!({
        "port": port,
        "available": available,
        "reserved": port > 0 && port < 1024,
        "inUseByGateway": is_running() && port_of(&load_gateway_config()
            .get("listen").and_then(Value::as_str).unwrap_or("")) == port,
        "suggest": if available { Value::Null } else {
            find_free_port(port.saturating_add(1), 50).map(|p| json!(p)).unwrap_or(Value::Null)
        },
        // 保持字段存在（前端类型要求），但不在热路径上填充。
        // 真正需要时由前端调 kill 接口，那边会查并回显实际占用者。
        "holder": Value::Null,
    })
}

/// 判断进程是否属于本项目（网关 / 宿主 GUI）。
///
/// 只按进程名与路径特征判断，不做签名校验 —— 这是「提示措辞」的依据，
/// 不是安全边界；真正能否杀死由操作系统权限决定。
fn is_our_process(name: &str, path: &str) -> bool {
    let n = name.to_ascii_lowercase();
    let p = path.to_ascii_lowercase();
    n.starts_with("gateway-")
        || n == "gateway.exe"
        || n == "gateway"
        || n == "ai-gateway.exe"
        || n == "ai-gateway"
        || n == "wb-switch-rust.exe"
        || p.contains("ai-gateway")
        || p.contains("wb-switch")
        || p.contains("gateway\\bin\\")
}

/// 查询占用指定端口的进程，返回其 JSON 描述。
///
/// 返回 None 表示「查不到」：可能是端口其实空闲、进程已退出、或权限不足。
/// 调用方应把 None 当作「无法提供占用者信息」，而不是「没有占用」。
///
/// **慎用**：本函数会 spawn netstat + tasklist + powershell 三个控制台进程，
/// 开销是毫秒到秒级。不要在轮询或页面挂载路径上调用（见 `inspect_port` 的说明）。
/// 目前只在「用户确认要结束占用进程」这条按需路径上使用。
#[allow(dead_code)]
pub fn port_holder(port: u16) -> Option<Value> {
    let (pid, name, path) = find_port_holder(port)?;
    Some(json!({
        "pid": pid,
        "name": name,
        "path": path,
        "ours": is_our_process(&name, &path),
    }))
}

/// 主动结束占用指定端口的进程。
///
/// 安全约束（刻意的）：
///   1. 端口必须**确实被占用** —— 空闲端口直接拒绝，避免误杀无关进程；
///   2. 不允许杀死当前进程自己（自杀会让调用方拿不到返回值）；
///   3. 不允许杀死本程序启动的网关 —— 那种情况应走「停止网关」，
///      直接杀掉会让宿主与网关的状态不一致。
///
/// 不限制「只能杀自己人」：用户明确要求「把占用端口的进程杀死」，
/// 第三方进程（如误开的其他服务）也是合法目标，但前端会给出更强的警告。
pub fn kill_port_holder(port: u16) -> Result<Value, String> {
    if port == 0 {
        return Err("端口号无效".to_string());
    }
    if port_free(port) {
        return Err(format!("端口 {port} 当前空闲，无需清理"));
    }
    let Some((pid, name, _path)) = find_port_holder(port) else {
        return Err(format!(
            "端口 {port} 被占用，但无法识别占用进程（可能需要管理员权限）"
        ));
    };

    if pid == std::process::id() {
        return Err("占用该端口的是本程序自身，请改用「停止网关」".to_string());
    }
    let lower = name.to_ascii_lowercase();
    if is_running() && lower.starts_with("gateway-") {
        return Err("占用该端口的是本程序启动的网关，请改用「停止网关」".to_string());
    }

    terminate_process(pid)?;
    // 等待端口真正释放：进程退出与端口释放之间有短暂窗口，
    // 立刻返回成功会让前端紧接着的「启动」失败，体验很差。
    for _ in 0..40 {
        if port_free(port) {
            return Ok(json!({
                "ok": true,
                "pid": pid,
                "name": name,
                "message": format!("已结束进程 {name} (PID {pid})，端口 {port} 已释放"),
            }));
        }
        std::thread::sleep(std::time::Duration::from_millis(100));
    }
    Err(format!(
        "已请求结束进程 {name} (PID {pid})，但端口 {port} 仍被占用（可能未完全退出）"
    ))
}

/// 查找占用端口的进程：返回 (pid, 进程名, 可执行文件路径)。
///
/// Windows 用 `netstat -ano` 找 PID，再用 `tasklist` 换进程名、`powershell` 取路径。
/// 其它平台暂不支持（返回 None），前端会退化为「无法识别占用者」的提示。
///
/// **必须走 `process::run_cmd_timeout` 而不是裸 `Command::output()`**：
///  1. 它带 `CREATE_NO_WINDOW`。裸 spawn 控制台程序会闪出 cmd 黑窗口
///     （项目里多处注释指出这是「GUI 卡顿/跳动的主因」）。
///  2. 它有超时。netstat 在连接数多的机器上可能长时间不返回，
///     裸调用会让调用方（HTTP 请求线程）一直挂着。
///  3. 它把 stdout/stderr 与等待**并发**读取，避免输出超过管道缓冲
///     （约 64KB）时子进程写满阻塞、双方互等的死锁。
///
/// 实现初期用的是裸 `Command::new(..).output()`，三样保障一个都没有；
/// 打包后实测表现为弹「应用程序无法正常启动 (0xc0000142)」——
/// 即子进程创建/DLL 初始化失败，且异常未受控地向上传播。
#[cfg(windows)]
fn find_port_holder(port: u16) -> Option<(u32, String, String)> {
    use crate::modules::process::run_cmd_timeout;

    let pid = find_port_pid(port)?;

    // tasklist 换进程名（CSV 便于解析，避免中文列宽对齐问题）
    let out = run_cmd_timeout(
        "tasklist",
        &["/FI", &format!("PID eq {pid}"), "/FO", "CSV", "/NH"],
        5,
    )?;
    let text = String::from_utf8_lossy(&out.stdout);
    let line = text.lines().next()?.trim();
    // 形如："gateway-xxx.exe","33708","Console","1","8,543 K"
    let name = line
        .split("\",\"")
        .next()
        .map(|s| s.trim_matches('"').to_string())
        .unwrap_or_default();
    if name.is_empty() {
        return None;
    }

    // 路径需要 PowerShell（wmic 在新系统已移除）。取不到不影响主流程
    // （进程名已足够提示用户），因此失败时留空而不是整条失败。
    let path = run_cmd_timeout(
        "powershell",
        &[
            "-NoProfile",
            "-Command",
            &format!("(Get-Process -Id {pid} -ErrorAction SilentlyContinue).Path"),
        ],
        8,
    )
    .map(|o| String::from_utf8_lossy(&o.stdout).trim().to_string())
    .unwrap_or_default();

    Some((pid, name, path))
}

/// 从 `netstat -ano -p TCP` 的输出里解析出监听指定端口的 PID。
///
/// 只认 `LISTENING` 行：否则会把「已建立的连接」（客户端侧临时端口恰好
/// 等于目标端口）误判成占用者，进而让用户去杀一个无关进程。
#[cfg(windows)]
fn find_port_pid(port: u16) -> Option<u32> {
    use crate::modules::process::run_cmd_timeout;

    let out = run_cmd_timeout("netstat", &["-ano", "-p", "TCP"], 8)?;
    let text = String::from_utf8_lossy(&out.stdout);
    let needle_v4 = format!(":{port} ");
    let needle_v6 = format!("]:{port} ");
    for line in text.lines() {
        let t = line.trim();
        if !t.starts_with("TCP") || !t.contains("LISTENING") {
            continue;
        }
        // 形如：TCP    0.0.0.0:7864    0.0.0.0:0    LISTENING    33708
        if !(t.contains(&needle_v4) || t.contains(&needle_v6)) {
            continue;
        }
        if let Some(last) = t.split_whitespace().last() {
            if let Ok(p) = last.parse::<u32>() {
                return Some(p);
            }
        }
    }
    None
}

#[cfg(not(windows))]
fn find_port_holder(_port: u16) -> Option<(u32, String, String)> {
    // 非 Windows 平台暂不实现：前端会提示「无法识别占用进程」，
    // 用户仍可手动处理。留出接口便于后续按平台补 lsof/ss 实现。
    None
}

/// 强制结束进程。
#[cfg(windows)]
fn terminate_process(pid: u32) -> Result<(), String> {
    use crate::modules::process::run_cmd_timeout;
    let out = run_cmd_timeout("taskkill", &["/PID", &pid.to_string(), "/F", "/T"], 10)
        .ok_or_else(|| format!("调用 taskkill 失败或超时（PID {pid}）"))?;
    if out.status.success() {
        return Ok(());
    }
    let msg = String::from_utf8_lossy(&out.stderr);
    let msg = msg.trim();
    Err(if msg.is_empty() {
        format!("结束进程 {pid} 失败（可能需要管理员权限）")
    } else {
        format!("结束进程 {pid} 失败: {msg}")
    })
}

#[cfg(not(windows))]
fn terminate_process(pid: u32) -> Result<(), String> {
    use std::process::Command;
    let out = Command::new("kill")
        .args(["-9", &pid.to_string()])
        .output()
        .map_err(|e| format!("调用 kill 失败: {e}"))?;
    if out.status.success() {
        Ok(())
    } else {
        Err(format!("结束进程 {pid} 失败（可能需要权限）"))
    }
}

/// 出站代理地址：复用「设置 → 更新代理」里已填的值（`github_config.json` 的 `proxy`）。
///
/// 设计取舍：**不新增一个「网关代理」配置项**。用户在设置页填的更新代理，
/// 目的就是访问被墙的服务；国际版 workbuddy.ai 属于同类需求，让用户配两遍
/// 既啰嗦又容易只配一处导致「浏览器能用、网关不能用」的困惑。
///
/// 返回空串表示未配置（网关将直连，并在日志里提示国际版可能超时）。
///
/// 注意：地址本身**与三个开关无关**，照旧原样透传。是否真的使用它由
/// `proxy_scope.cn` / `proxy_scope.intl` 在 Go 侧按区域决定（见 native_config_proxy_scope）——
/// 让网关自己按区域判断，而不是宿主在这里把地址清空，是因为清空之后就再也
/// 分不出「用户没配代理」和「用户配了但关掉了某一路」，日志会误导排查方向。
fn upstream_proxy() -> String {
    crate::modules::update::load_github_config()
        .get("proxy")
        .and_then(Value::as_str)
        .unwrap_or("")
        .trim()
        .to_string()
}

/// 写出网关配置里的 `proxy_scope` 块（**只有 cn / intl 两个键**）。
///
/// 为什么不含 github：GitHub 的更新检查与安装包下载是**宿主的活**，与网关无关 ——
/// 网关根本不发往 github.com 的请求。把 github 也塞进来会让网关配置里出现一个
/// 它永远不会读的键，下一个人排查「为什么关了开关还在走代理」时会先怀疑这里。
/// 宿主自己消费 github 那一格（见 update::configured_proxy 的调用点）。
///
/// 两个值都取 `update::proxy_scope()`（缺键 → 默认值，即国际版开、国服关），
/// 与 github_config.json 是同一口径：老配置没有这个键时，网关收到的仍是
/// 「国际版走代理、国服直连」= 本次改动前的行为。
fn native_config_proxy_scope() -> Value {
    let scope = crate::modules::update::proxy_scope();
    json!({ "cn": scope.cn, "intl": scope.intl })
}

/// 从宿主配置里取某个任务的执行时点（小时列表），非法/缺失一律回落默认。
///
/// 为什么要这层兜底而不是直接 `cfg.get(...).cloned()`：
///   - 键缺失（老配置）→ 必须给出与 Go 侧 `Default()` 完全一致的默认值，
///     否则「用户没配过」与「用户配了空」会得到两种不同排程；
///   - 前端数值输入框可能传来 0 / 负数 / >23 的脏值。Go 侧对此是**启动即报错**
///     （validateScheduleHours），一旦写进 native config，网关会直接起不来 ——
///     在宿主这边先滤掉，坏值退化为「该时点不生效」而不是「整个网关挂掉」；
///   - 空列表同样回落默认：Go 侧把空数组视同「未配置」（见其 normalize()）。
///
/// 去重后排序，避免用户重复填同一小时导致网关在同一时刻跑两轮。
fn schedule_hours(cfg: &Value, key: &str, default: &[i64]) -> Value {
    let raw = cfg.get(key).and_then(Value::as_array);
    let mut hours: Vec<i64> = match raw {
        Some(items) => items
            .iter()
            .filter_map(Value::as_i64)
            .filter(|h| (0..=23).contains(h))
            .collect(),
        None => Vec::new(),
    };
    if hours.is_empty() {
        hours = default.to_vec();
    }
    hours.sort_unstable();
    hours.dedup();
    json!(hours)
}

/// 网关记录任务结果所需的账号身份映射：网关 uid → {id, name}。
///
/// 为什么必须由宿主传下去，而不是让网关用它手上的 uid 顶替：
///
///   - 界面「账号卡片 → 查看记录」按账号库的 **id**（uuid）过滤记录，
///     而网关凭证里只有 uid。网关拿 uid 当 accountId 写，用户点开某个账号
///     永远查不到这些任务 —— 过滤条件对不上，且不会有任何报错。
///   - 展示名同理：宿主用 `account::account_display_name`（email → nickname → uid），
///     而网关的凭证里根本没有 email（授权信息只在账号库的 profile_raw）。
///
/// 只含 id / uid / 展示名，**不含任何 token**：这份配置会落到磁盘
/// （gateway_native_config.json），凭证绝不该出现在那里。
fn gateway_account_identities() -> Value {
    let items: Vec<Value> = account::load_accounts()
        .iter()
        .filter_map(|acc| {
            let uid = account::get_str(acc, "uid")?;
            Some(json!({
                "uid": uid,
                "id": acc.get("id").and_then(Value::as_str).unwrap_or(""),
                "name": account::account_display_name(acc),
            }))
        })
        .collect();
    json!(items)
}

/// 归一化自定义提示词模式。
///
/// 只认 `custom`（大小写与首尾空白不敏感），其余一律回落 `passthrough`。
/// 为什么要这层兜底而不是直接透传用户填的值：Go 侧 `normalizePrompt` 对无法识别的
/// mode 是**启动即报错**（刻意的 fail fast，见其注释）—— 用户在前端把 `custom`
/// 拼错，整个网关会起不来。在宿主先滤掉，坏值退化为「该功能不生效」而不是「网关挂掉」，
/// 与 `schedule_hours` 对脏时点的处理是同一取舍。
///
/// 缺省值是 passthrough：老配置没有这个键，必须保持既有行为（透传原始 system）。
fn prompt_mode_of(cfg: &Value) -> &'static str {
    match cfg
        .get("prompt_mode")
        .and_then(Value::as_str)
        .unwrap_or("")
        .trim()
        .to_ascii_lowercase()
        .as_str()
    {
        "custom" => "custom",
        _ => "passthrough",
    }
}

/// 生成网关需要的 config.json（网关原生格式）。
fn write_native_config(cfg: &Value) -> Result<PathBuf, String> {
    let dir = gateway_dir();
    std::fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
    let auth_dir = gateway_auth_dir();
    let state_file = gateway_state_file();
    if let Some(parent) = state_file.parent() {
        std::fs::create_dir_all(parent).map_err(|e| e.to_string())?;
    }

    // 账号记录文件由宿主独占命名：路径只在这里算一次、随配置透传给网关。
    // 两边各自推算数据目录（宿主看 AI_GATEWAY_HOME，网关看自己的 cwd）
    // 迟早会算出不同的值，而那种错法表现为「静默不记录」，极难排查。
    let records_file = crate::modules::account_records::account_records_file();

    let native = json!({
        "listen": cfg.get("listen").and_then(Value::as_str).unwrap_or(":7863"),
        "api_key": cfg.get("api_key").and_then(Value::as_str).unwrap_or(""),
        "auth_dir": auth_dir.to_string_lossy(),
        "state_file": state_file.to_string_lossy(),
        "cooldown": { "soft_rate": "60s" },
        "schedule": {
            "checkin_hours": [9, 21],
            "keepalive_hours": [22],
            // 缺省 true；false = 关签到（猫猫旅行搭签到便车，因此也随之停摆）
            "checkin_enabled": cfg.get("checkin_enabled").and_then(Value::as_bool).unwrap_or(true),
            // 缺省 true；false = 关 token 保活
            "keepalive_enabled": cfg.get("keepalive_enabled").and_then(Value::as_bool).unwrap_or(true),
            // 签到 + 猫猫旅行的区域范围：cn（缺省，仅国服）/ all。
            // 国际版（workbuddy.ai）的 billing 与 growth 接口暂无真实数据，默认跳过。
            "checkin_scope": cfg.get("checkin_scope").and_then(Value::as_str).unwrap_or("cn"),
            // ---- 4 个自动养号任务 ----
            //
            // 为什么必须写在这里：网关只读它自己的 config.json，键缺席时用
            // Go 侧的硬编码默认值（cmd/server/config.go 的 Default()）。宿主界面
            // 改的却是 gateway_config.json —— 不落到此处，用户在界面上做的任何
            // 调整都不会生效，且从界面上完全看不出来（表现为「改了没用」）。
            //
            // 时点是**数值列表**（支持多个整点，如 trial 默认 [9,21]），
            // 因此走 as_array + as_i64 而不是单个数字。
            "activity_hours": schedule_hours(cfg, "activity_hours", &[10]),
            "nightowl_hours": schedule_hours(cfg, "nightowl_hours", &[1]),
            "school_hours": schedule_hours(cfg, "school_hours", &[12]),
            "trial_hours": schedule_hours(cfg, "trial_hours", &[9, 21]),
            // 开关缺省 true：与 checkin_enabled / keepalive_enabled 同语义 ——
            // 老配置里没有这些键时保持既有行为（任务照跑），只有显式 false 才关。
            "activity_enabled": cfg.get("activity_enabled").and_then(Value::as_bool).unwrap_or(true),
            "nightowl_enabled": cfg.get("nightowl_enabled").and_then(Value::as_bool).unwrap_or(true),
            "school_enabled": cfg.get("school_enabled").and_then(Value::as_bool).unwrap_or(true),
            "trial_enabled": cfg.get("trial_enabled").and_then(Value::as_bool).unwrap_or(true),
            // 每号每日上报条数：取 3 而非 1（单条偶发被服务端静默丢弃，多条提高
            // 点亮成功率），也不宜过多以免被风控当成异常流量。上限 20 兜住误填。
            "activity_report_count": cfg
                .get("activity_report_count")
                .and_then(Value::as_i64)
                .filter(|n| *n > 0)
                .unwrap_or(3)
                .min(20),
        },
        "upstream": {
            "timeout_seconds": 120,
            "header_timeout_seconds": 120,
            "idle_timeout_seconds": 300
        },
        "features": { "sanitize_blacklist_fingerprints": true },
        // ---- 自定义系统提示词（Go 侧 Config.Prompt，json tag 逐字对齐）----
        //
        // 为什么必须由宿主写在这里：本函数在**每次启动网关时全量重写**
        // gateway_native_config.json。不写这个块的话，任何落在该文件里的
        // prompt.mode / prompt.file 都会被下次启动抹掉 —— 用户只能在宿主之外
        //（直接改 config.json 或设 WB2A_PROMPT_* 环境变量）启用这个功能，
        // 从界面上看就是「配了却总被重置」。
        //
        // 与 features.sanitize_blacklist_fingerprints 是**两层叠加、互不替代**：
        // 那个清洗消息里的指纹串，这个把 system/developer 消息整体替换。
        "prompt": {
            // 缺省 passthrough = 透传客户端原始 system（既有行为不变）。
            "mode": prompt_mode_of(cfg),
            // 空串 = 用网关内置默认提示词，由 Go 侧 prompt.Load 回落。
            "file": cfg.get("prompt_file").and_then(Value::as_str).unwrap_or("").trim(),
        },
        "upstash": { "url": "", "token": "" },
        // 出站代理：**复用**「设置 → 更新代理」里已填的地址，用户无需配两遍。
        //
        // 为什么网关需要它：国际版（workbuddy.ai）在国内直连不稳定（实测 wsarecv 超时），
        // 走代理才稳。而 Go 的 http.ProxyFromEnvironment **只读环境变量**、不读 Windows
        // 注册表，所以「浏览器能走系统代理」不代表网关也能。
        //
        // 地址只填一次，**适用范围由 proxy_scope 决定**（三个独立开关，见
        // settings 页「网络代理」）。这里写出的是网关侧要用的两格（cn / intl），
        // github 那一格由宿主自己消费，不进网关配置。
        //
        // 每次启动全量重写本文件，所以这个块必须写 —— 否则用户在界面上关掉
        // 国际版代理后，下次启动网关又会按「缺键 → 默认值（intl=true）」把代理打开，
        // 表现为「关了开关，重启后又自己开了」。
        "proxy": upstream_proxy(),
        "proxy_scope": native_config_proxy_scope(),
        "pool": {
            "max_in_flight": 3,
            "breaker_threshold": 3,
            "breaker_cooldown": "30m",
            "breaker_cooldown_max": "6h",
            "idle_weight_per_hour": 0.5,
            "idle_weight_max": 5.0,
            // 「单一模型 + 积分轮转」：缺省 false = 负载均衡（老配置行为不变）。
            // 由当前**工作模式**推导，而不是读 cfg 里的独立开关 ——
            // 避免「模式是轮转、pool.rotation 却是 false」这类不一致状态。
            "rotation": matches!(
                GatewayMode::from_str(cfg.get("mode").and_then(Value::as_str).unwrap_or("balance")),
                GatewayMode::Rotation
            ),
            // 「限制使用的模型」白名单：非空时网关只放行名单内的模型。
            //
            // **三个工作模式都写**（不再只在轮转模式下写）：限制模型与「用哪些
            // 账号」是正交的两件事。此前只在轮转下写，导致用户在自动/手动模式
            // 下配的限制被静默丢弃 —— 官方 native config 里那一项恒为空串，
            // 表现为「界面上勾了、网关照样放行一切」。
            //
            // 形状是**字符串数组**（空数组 = 不限制）。Go 侧
            // `AllowedModels.UnmarshalJSON` 同时吃字符串与数组，因此把老的
            // 单值字符串读成单元素数组、写成数组，两边都自洽。
            "allowed_model": allowed_models_of(cfg),
        },
        "session_sticky": { "enabled": true, "ttl": "30m", "gc_interval": "5m" },
        // ---- 账号记录回写（养号任务的执行痕迹）----
        //
        // 为什么需要：活跃上报 / 夜猫子 / 开学季 / trial 都实现在 Go 网关里，
        // 它们此前只 log.Printf 写 stdout，而宿主启动子进程时把
        // stdout/stderr 丢进了 Stdio::null（见本文件 start_gateway）——
        // 于是「任务跑了但界面上一条记录都没有」。
        //
        // 让 Go 侧直接写宿主已经在读的 account_records.json（同文件、同结构），
        // 记录就能与签到并列出现在「账号记录 → 任务」里，不引入第二套格式。
        "account_records": {
            "file": records_file.to_string_lossy(),
            // 保留天数取宿主设置，与签到日志/积分快照同一口径：
            // 两边各算一次会出现「界面说保留 60 天，网关按 7 天清」这类不一致。
            "retention_days": crate::modules::config::record_retention_days(),
            // 账号身份映射（uid → id/name）：界面按账号库 id 过滤，
            // 见 gateway_account_identities 的注释。
            "identities": gateway_account_identities(),
        }
    });
    let path = dir.join("gateway_native_config.json");
    let text = serde_json::to_string_pretty(&native).map_err(|e| e.to_string())?;
    atomic_write(&path, &text).map_err(|e| e.to_string())?;
    Ok(path)
}

/// 本地端口是否空闲。
///
/// 启动前必须预检：网关只在启动时绑定端口，若端口已被占用（例如另一个网关
/// 或 Docker 映射的服务），子进程会立刻退出，而后面的健康探测会打到**别人的**
/// 服务上并误报「启动成功」。这里先挡住这种误判。
fn port_free(port: u16) -> bool {
    use std::net::{Ipv4Addr, Ipv6Addr, SocketAddr, TcpListener};

    // 判定「端口可用」需要两个维度都通过，缺一不可：
    //
    //   1) 试绑 IPv4/IPv6 通配地址 —— 能发现「已绑定但尚未 accept」的监听者；
    //   2) 主动连接 127.0.0.1 / [::1]     —— 能发现「绑定在特定地址」的服务。
    //
    // 为什么必须两者结合（实测教训）：
    //   * 只看试绑：本机 Docker Desktop 把端口发布成 wslrelay/com.docker.backend
    //     的转发形式，试绑 0.0.0.0 与 [::] 都**能成功**，于是 7863 被误判为空闲，
    //     网关子进程随后绑定失败，而健康探测又打到 Docker 里的旧网关，
    //     最终报告一个虚假的「启动成功」。
    //   * 只看连接：占用但未监听（如处于 TIME_WAIT 的主动关闭端）会漏判。
    if TcpListener::bind(SocketAddr::from((Ipv4Addr::UNSPECIFIED, port))).is_err() {
        return false;
    }
    match TcpListener::bind(SocketAddr::from((Ipv6Addr::UNSPECIFIED, port))) {
        Ok(l) => drop(l),
        // 系统未启用 IPv6 不算占用
        Err(e) if e.kind() == std::io::ErrorKind::AddrNotAvailable => {}
        Err(_) => return false,
    }
    // 已有服务在应答 → 判定为被占用
    !has_listener(port)
}

/// 主动连接探测：127.0.0.1 或 [::1] 上有服务应答即认为端口被占用。
fn has_listener(port: u16) -> bool {
    use std::net::{Ipv4Addr, Ipv6Addr, SocketAddr, TcpStream};
    use std::time::Duration;

    let probes = [
        SocketAddr::from((Ipv4Addr::LOCALHOST, port)),
        SocketAddr::from((Ipv6Addr::LOCALHOST, port)),
    ];
    for addr in probes {
        if TcpStream::connect_timeout(&addr, Duration::from_millis(300)).is_ok() {
            return true;
        }
    }
    false
}

/// 探测网关健康（HTTP /healthz）。
/// /healthz 在无可用账号时返回 503，这代表进程活着但没有可用账号，
/// 因此只有连接失败才算「未就绪」。
pub async fn probe_health(port: u16, timeout_ms: u64) -> Result<Value, String> {
    let url = format!("http://127.0.0.1:{port}/healthz");
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_millis(timeout_ms))
        .build()
        .map_err(|e| e.to_string())?;
    match client.get(&url).send().await {
        Ok(resp) => {
            let status = resp.status().as_u16();
            // 必须在 text() 之前取出响应头 —— text() 会消费 resp（E0382）
            let hdr_service = resp
                .headers()
                .get("X-Service")
                .and_then(|v| v.to_str().ok())
                .unwrap_or("")
                .to_string();
            let body = resp.text().await.unwrap_or_default();
            let parsed: Value = serde_json::from_str(&body).unwrap_or_else(|_| json!({ "raw": body }));

            // 身份校验：确认应答者确实是本项目的网关，而不是恰好占用同端口的
            // 其他服务（Docker 里的旧容器等）—— 否则会报告「假启动成功」。
            //
            // 优先用上游自 v0.2 起在 /healthz 提供的身份标识：
            //   - X-Service 响应头
            //   - 响应体 service 字段
            // 值均为 "workbuddy2api"。这比用 api_key 校验更可靠：
            // api_key 为空（未鉴权）时后者完全失去作用。
            //
            // 兼容旧版网关（无标识）：回退到「用 api_key 访问 /status 是否通」。
            let mut identity_ok = true;
            let body_service = parsed
                .get("service")
                .and_then(Value::as_str)
                .unwrap_or("")
                .to_string();

            if !hdr_service.is_empty() || !body_service.is_empty() {
                identity_ok = hdr_service == GATEWAY_SERVICE_NAME
                    || body_service == GATEWAY_SERVICE_NAME;
            } else {
                let cfg = load_gateway_config();
                let key = cfg.get("api_key").and_then(Value::as_str).unwrap_or("");
                if !key.is_empty() {
                    let surl = format!("http://127.0.0.1:{port}/status");
                    if let Ok(sresp) = client
                        .get(&surl)
                        .header("Authorization", format!("Bearer {key}"))
                        .send()
                        .await
                    {
                        identity_ok = sresp.status().as_u16() == 200;
                    }
                }
            }

            Ok(json!({
                "reachable": true,
                "http_status": status,
                "healthy": status == 200 && identity_ok,
                "identity_ok": identity_ok,
                "detail": parsed,
            }))
        }
        Err(e) => Err(e.to_string()),
    }
}

/// 启动网关子进程。成功返回可访问基址。
pub async fn start_gateway(cfg: &Value) -> Result<Value, String> {
    if is_running() {
        return Err("网关已在运行".to_string());
    }
    let Some(exe) = resolve_gateway_exe() else {
        return Err(format!(
            "未找到网关可执行文件 {}（可用环境变量 AI_GATEWAY_ROUTER_BIN 指定路径）",
            gateway_exe_name()
        ));
    };

    // 先把账号库导出为网关凭证，保证启动即能加载到账号
    let (count, _) = export_accounts_to_gateway()?;
    if count == 0 {
        // 区分「本来就没有账号」与「账号都在需重登状态」：后者不给出提示的话，
        // 用户只会看到「账号库为空」，完全不知道去重新登录就能恢复。
        let accounts = account::load_accounts();
        let dead = accounts.iter().filter(|a| needs_relogin(a)).count();
        if dead > 0 && dead == accounts.len() {
            return Err(format!(
                "账号库中的 {dead} 个账号都已标记「需重新登录」（refresh token 已失效），\
                 已暂停导出到网关。请先在「账号管理」页重新登录，恢复后会自动重新加入。"
            ));
        }
        if gateway_mode() == GatewayMode::Manual {
            let picked = manual_uids();
            // 勾了账号但全都不可用：明确指出问题，而不是笼统说「账号库为空」
            let dead_picked = picked
                .iter()
                .filter(|u| {
                    accounts.iter().any(|a| {
                        account::get_str(a, "uid").as_deref() == Some(u.as_str())
                            && (needs_relogin(a) || account::account_disabled(a))
                    })
                })
                .count();
            if !picked.is_empty() && dead_picked == picked.len() {
                return Err(
                    "手动模式勾选的账号都不可用（需重新登录或已被禁用），\
                     请在网关页面重新勾选。"
                        .to_string(),
                );
            }
        }
        return Err(match gateway_mode() {
            GatewayMode::Manual => "手动模式尚未勾选任何账号，请在网关页面勾选后启动".to_string(),
            GatewayMode::Balance => "账号库为空，请先添加账号再启动网关".to_string(),
            // 轮转模式同样需要至少一个账号；此时「没有下一个可轮转」是主要问题。
            GatewayMode::Rotation => {
                "「单一模型 + 积分轮转」模式需要账号池中有可用账号，请先添加账号再启动网关".to_string()
            }
        });
    }

    // 统一端口来源：优先显式 port 字段，其次解析 listen
    let port = cfg
        .get("port")
        .and_then(Value::as_u64)
        .map(|p| p as u16)
        .filter(|p| *p > 0)
        .unwrap_or_else(|| {
            port_of(cfg.get("listen").and_then(Value::as_str).unwrap_or(":7863"))
        });
    let mut cfg = cfg.clone();
    cfg["port"] = json!(port);
    cfg["listen"] = json!(normalize_listen(port));

    let native_cfg = write_native_config(&cfg)?;

    let mut cmd = Command::new(&exe);
    cmd.arg("-config")
        .arg(&native_cfg)
        .current_dir(gateway_dir())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    // 非 Windows 平台不需要抑制控制台窗口，保持默认行为。
    #[cfg(target_os = "windows")]
    {
        use std::os::windows::process::CommandExt;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        cmd.creation_flags(CREATE_NO_WINDOW);
    }

    // 端口预检：被占用时直接给出可操作的错误，而不是让子进程启动即退出
    if !port_free(port) {
        return Err(format!(
            "端口 {port} 已被占用（可能已有网关或其他服务在监听）。请在配置中改用其他端口，\
             或先停止占用该端口的程序。"
        ));
    }

    let child = cmd.spawn().map_err(|e| format!("启动网关失败: {e}"))?;
    // 立即纳入 Job：之后父进程意外结束也会被系统连带终止，避免孤儿进程占端口。
    attach_child_to_job(&child);
    *proc_slot().lock().unwrap() = Some(child);
    GATEWAY_RUNNING.store(true, Ordering::SeqCst);

    // 等待端口就绪（最多 20s）
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(20);
    let mut last_err = String::new();
    while std::time::Instant::now() < deadline {
        // 子进程若已退出（配置错误/端口冲突等），立即失败并回报退出码
        {
            let mut slot = proc_slot().lock().unwrap();
            if let Some(child) = slot.as_mut() {
                if let Ok(Some(status)) = child.try_wait() {
                    *slot = None;
                    GATEWAY_RUNNING.store(false, Ordering::SeqCst);
                    return Err(format!(
                        "网关启动后立即退出（退出码 {:?}）。常见原因：端口被占用、\
                         配置无效或账号凭证不可读。",
                        status.code()
                    ));
                }
            }
        }
        match probe_health(port, 900).await {
            Ok(v) if v.get("identity_ok").and_then(Value::as_bool) == Some(false) => {
                last_err = format!(
                    "端口 {port} 上的服务不是本网关（api_key 校验未通过），\
                     可能已有其他网关在监听"
                );
            }
            Ok(v) => {
                return Ok(json!({
                    "started": true,
                    "base": format!("http://127.0.0.1:{port}"),
                    "port": port,
                    "accounts": count,
                    "health": v,
                }));
            }
            Err(e) => last_err = e,
        }
        tokio::time::sleep(std::time::Duration::from_millis(350)).await;
    }

    stop_gateway();
    Err(format!("网关启动超时（端口 {port}）：{last_err}"))
}

/// 停止网关子进程。
pub fn stop_gateway() -> Value {
    let mut slot = proc_slot().lock().unwrap();
    let stopped = match slot.as_mut() {
        Some(child) => {
            let pid = child.id();
            // Windows 需连同子进程树一起结束；走 cmd_builder 加 CREATE_NO_WINDOW，
            // 避免退出/停止时闪出 taskkill 控制台窗口。
            let _ = crate::modules::process::cmd_builder("taskkill")
                .args(["/F", "/T", "/PID", &pid.to_string()])
                .stdout(Stdio::null())
                .stderr(Stdio::null())
                .status();
            let _ = child.wait();
            true
        }
        None => false,
    };
    *slot = None;
    GATEWAY_RUNNING.store(false, Ordering::SeqCst);
    json!({ "stopped": stopped })
}

/// 网关综合状态：配置 + 运行态 + 健康 + 账号池详情。
pub async fn gateway_status() -> Value {
    let cfg = load_gateway_config();
    let listen = cfg.get("listen").and_then(Value::as_str).unwrap_or(":7863");
    let port = port_of(listen);
    let exe = resolve_gateway_exe();
    let running = is_running();

    let mut health = Value::Null;
    let mut pool = Value::Null;
    let mut reachable = false;
    if running {
        if let Ok(h) = probe_health(port, 1200).await {
            reachable = true;
            health = h;
        }
        // 账号池详情（/status 需要 api_key）
        if reachable {
            let key = cfg.get("api_key").and_then(Value::as_str).unwrap_or("");
            let url = format!("http://127.0.0.1:{port}/status");
            if let Ok(client) = reqwest::Client::builder()
                .timeout(std::time::Duration::from_millis(1500))
                .build()
            {
                let mut req = client.get(&url);
                if !key.is_empty() {
                    req = req.header("Authorization", format!("Bearer {key}"));
                }
                if let Ok(resp) = req.send().await {
                    if let Ok(v) = resp.json::<Value>().await {
                        pool = v;
                    }
                }
            }
        }
    }

    let account_count = account::load_accounts().len();
    // 需重登的账号不导出到网关，界面需要能看到「为什么某个账号不在池里」。
    let excluded_accounts: Vec<Value> = account::load_accounts()
        .iter()
        .filter(|a| needs_relogin(a))
        .filter_map(|a| {
            let uid = account::get_str(a, "uid")?;
            Some(json!({
                "uid": uid,
                "nickname": account::get_str(a, "nickname").unwrap_or_default(),
                "reason": account::get_str(a, "needs_relogin_reason"),
            }))
        })
        .collect();
    let exe_path = exe.as_ref().map(|p| p.to_string_lossy().to_string());
    let exe_found = exe.is_some();
    json!({
        "running": running,
        "reachable": reachable,
        "base": format!("http://127.0.0.1:{port}"),
        "openaiBase": format!("http://127.0.0.1:{port}/v1"),
        "port": port,
        "exePath": exe_path,
        "exeFound": exe_found,
        "exeSource": gateway_source(),
        "mode": gateway_mode().as_str(),
        "pinnedUid": pinned_uid(),
        // 被排除出网关池的账号（需重新登录），供界面提示
        "excludedAccounts": excluded_accounts,
        // 供前端下拉选择「指定账号」
        "accounts": account::load_accounts()
            .iter()
            .filter_map(|a| {
                let uid = account::get_str(a, "uid")?;
                Some(json!({
                    "uid": uid,
                    "nickname": account::get_str(a, "nickname").unwrap_or_default(),
                    "expiresAt": a.get("expiresAt").and_then(Value::as_i64).unwrap_or(0),
                    // 与 account_meta 同口径：复用 refresh::needs_relogin，
                    // 不直接读原始标记（旧版本把网络抖动也写成 needs_relogin）。
                    // 前端手动模式的「可选账号」过滤依赖这个字段，
                    // 误报会让用户看到「明明能用的号却选不了」。
                    "needsRelogin": crate::modules::refresh::needs_relogin(a),
                }))
            })
            .collect::<Vec<Value>>(),
        "exeSource": gateway_source(),
        "portAvailable": port_free(port),
        "authDir": gateway_auth_dir().to_string_lossy(),
        "accountsInLibrary": account_count,
        // 正在执行的养号任务（含进度）。所有者明确要求「账号卡片上要能看到
        // 正在执行的任务」—— 此前点了「立即执行」只有一个按钮转圈，
        // 看不到在跑什么、跑到哪、哪些账号在跑。
        //
        // 随 status 一起透出（而不是新开一个接口）：账号卡片所在的列表页
        // 与网关页都在轮询 status，复用同一次请求不会引入第二套轮询。
        "taskRuntime": task_runtime(),
        "config": cfg,
        "health": health,
        "pool": pool,
    })
}

/// 按配置自动启动（供启动流程调用）。
///
/// 只认 `auto_start` 这一个开关 —— 界面上的「随 App 启动」对应的就是它。
///
/// 历史缺陷（本次修复）：原实现要求 `enabled && auto_start` 同时为真，但 `enabled`
/// 是 `default_gateway_config()` 里的遗留字段，**全仓库没有任何地方写入它**，
/// 永远停留在默认值 false，于是条件恒不成立、网关从不自动启动 —— 正是用户反馈的
/// 「勾了随 App 启动却从未生效」。`enabled` 与 `auto_start` 语义本就重叠
/// （都是「要不要自动跑」），保留前者参与判定只会制造这种恒假条件，故移除。
///
/// 另注：本函数此前**从未被调用**（死代码），已接入 `lib.rs` 的 setup 启动流程。
pub async fn maybe_autostart() -> Option<Value> {
    let cfg = load_gateway_config();
    let auto = cfg.get("auto_start").and_then(Value::as_bool).unwrap_or(false);
    if !auto {
        return None;
    }
    // 已在运行（如上次退出未清理干净）时不重复启动，但仍同步一次运行态。
    if is_running() {
        update_runtime_state("started", None);
        return None;
    }
    match start_gateway(&cfg).await {
        Ok(v) => {
            update_runtime_state("started", None);
            Some(v)
        }
        Err(e) => {
            update_runtime_state("failed", Some(e));
            None
        }
    }
}

/// 账号库变化后同步到网关，并让运行中的网关热加载。
///
/// 网关只在启动时扫描 auths/，所以新账号需要重启进程才能生效；
/// 这里按需重启，避免用户手动操作。
pub async fn sync_and_reload(restart_if_changed: bool) -> Value {
    let (count, changed) = match export_accounts_to_gateway() {
        Ok(v) => v,
        Err(e) => return json!({ "ok": false, "error": e }),
    };
    let from_gateway = sync_auth_to_accounts().unwrap_or_default();

    let mut reloaded = false;
    if restart_if_changed && !changed.is_empty() && is_running() {
        let cfg = load_gateway_config();
        stop_gateway();
        if start_gateway(&cfg).await.is_ok() {
            reloaded = true;
        }
    }

    json!({
        "ok": true,
        "accounts": count,
        "changed": changed,
        "updatedFromGateway": from_gateway,
        "reloaded": reloaded,
    })
}

/// 仅同步账号（账号库 -> 网关），不重启。
pub fn sync_only() -> Value {
    match export_accounts_to_gateway() {
        Ok((count, changed)) => json!({ "ok": true, "accounts": count, "changed": changed }),
        Err(e) => json!({ "ok": false, "error": e }),
    }
}

/// 构造模式切换要落盘的配置补丁。
///
/// 抽成纯函数便于测试：`switch_mode` 会真的写用户配置并可能重启网关，
/// 不适合在单测里直接调用。
///
/// 语义：
///   - 切到自动（Balance）时清空 `manual_uids` 与 `pinned_uid`，
///     避免残留勾选值导致下次切回手动时用到意料之外的账号。
///   - `pinned_uid` 始终写 null：它是旧字段，新代码统一用 `manual_uids`。
///     保留写 null 是为了让升级后的配置不残留旧锁定值（否则 `manual_uids()`
///     读不到数组时会回退到 `pinned_uid`，行为变得难以预测）。
fn mode_patch(mode: GatewayMode, uids: &[String]) -> Value {
    let list: Vec<Value> = uids
        .iter()
        .map(|s| s.trim())
        .filter(|s| !s.is_empty())
        .map(|s| json!(s))
        .collect();
    // 仅手动模式保留勾选列表；其它模式清空，避免模式切换后残留
    let manual = if mode == GatewayMode::Manual {
        json!(list)
    } else {
        json!([])
    };
    json!({
        "mode": mode.as_str(),
        "manual_uids": manual,
        "pinned_uid": Value::Null,
    })
}

/// 从任意配置值里读出「限制使用的模型」名单。
///
/// **同时接受两种形状**（这是向后兼容的关键）：
///   - 字符串 `"glm-5.3"`        → 老配置/老宿主的单值写法 → `["glm-5.3"]`
///   - 数组   `["glm-5.3","x"]`  → 新宿主的写法            → 逐项读出
///
/// 为什么必须两者都吃：用户从旧版本升级上来时，gateway_config.json 里这个键
/// 是字符串。只认数组会让名单**静默变成空**（= 不限制），表现为「升级后限制
/// 突然不管用了」——安全方向的静默失败，比启动报错更危险。
///
/// 归一化：逐项 trim、丢弃空项；**去重**（界面可能因重复点击传入重复项，
/// 去重后错误信息里不会出现「a、a、b」这种读起来像 bug 的文案）。
/// 元素**不剥区域前缀**：那是网关（Go 侧）的职责，两边各写一套必然分叉。
fn allowed_models_of(cfg: &Value) -> Value {
    let raw = cfg.get("allowed_model");
    let mut out: Vec<String> = Vec::new();
    let mut push = |item: &str| {
        let trimmed = item.trim();
        if !trimmed.is_empty() && !out.iter().any(|existing| existing == trimmed) {
            out.push(trimmed.to_string());
        }
    };
    match raw {
        Some(Value::Array(items)) => {
            for item in items {
                if let Some(s) = item.as_str() {
                    push(s);
                }
            }
        }
        Some(Value::String(s)) => push(s),
        // null / 键缺席 / 其它形状一律当作「未配置」= 不限制。
        // 其它形状不报错：配置读路径上的宽容比严格更重要 —— 这里报错会让
        // 网关**启动不起来**，而一个畸形的名单最多是限制没生效。
        _ => {}
    }
    json!(out)
}

/// 读取「限制使用的模型」名单；未设置返回空数组（= 不限制）。
pub fn allowed_models() -> Vec<String> {
    allowed_models_of(&load_gateway_config())
        .as_array()
        .map(|items| {
            items
                .iter()
                .filter_map(|v| v.as_str().map(str::to_string))
                .collect()
        })
        .unwrap_or_default()
}

/// 读取老的**单值**写法（仅向后兼容，供旧调用方/旧界面读）。
///
/// 多值名单存在时返回**第一项**：旧界面的下拉是单选，给它一个能对上号的值，
/// 比返回空串（界面显示「不限制」而实际有限制）少一次误导。
pub fn allowed_model() -> String {
    allowed_models().into_iter().next().unwrap_or_default()
}

/// 归一化「限制使用的模型」配置值：空数组/全空项 → `[]`（清除限制），
/// 其余逐项 trim + 去重。
///
/// 抽成纯函数便于测试（真正的保存路径会写用户配置并可能重启网关）。
fn allowed_models_patch(models: &[String]) -> Value {
    json!({ "allowed_model": allowed_models_of(&json!({ "allowed_model": models })) })
}

/// 保存「限制使用的模型」白名单并**立即生效**。
///
/// 空数组 = 清除限制（= 全部放行，默认）；名单非空时网关只放行名单内的模型。
/// **三个工作模式共用**：限制模型与「用哪些账号」是正交的两件事，因此不再
/// 只在轮转模式下生效。
///
/// 为什么需要立即生效：`allowed_model` 由网关**启动时**读取，光落盘不会改变
/// 正在运行的进程（与切换模式同理，用户会看到「选了没反应」），因此在网关
/// 运行时重启它。
pub async fn set_allowed_models(models: &[String]) -> Value {
    let normalized = allowed_models_patch(models);
    let list: Vec<String> = normalized
        .get("allowed_model")
        .and_then(Value::as_array)
        .map(|items| {
            items
                .iter()
                .filter_map(|v| v.as_str().map(str::to_string))
                .collect()
        })
        .unwrap_or_default();

    let cfg = match save_gateway_config(&normalized) {
        Ok(v) => v,
        Err(e) => return json!({ "ok": false, "error": e }),
    };
    // 立即生效：网关只在启动时读配置。
    // 重启方式与 switch_mode 保持一致（先停、等端口释放、再启动）——
    // 端口未释放就启动会因占用而失败。
    let mut reloaded = false;
    if is_running() {
        stop_gateway();
        tokio::time::sleep(std::time::Duration::from_millis(600)).await;
        match start_gateway(&cfg).await {
            Ok(_) => {
                reloaded = true;
                update_runtime_state("started", None);
            }
            Err(e) => {
                update_runtime_state("failed", Some(e.clone()));
                return json!({
                    "ok": false,
                    "error": format!("模型限制已保存，但重启网关失败：{e}"),
                    "config": cfg,
                });
            }
        }
    }
    json!({
        "ok": true,
        "reloaded": reloaded,
        // 回显归一化后的完整名单（前端据此更新勾选态，而不是自己猜）。
        "allowedModels": list,
        // 老的**单值**回显字段：旧界面/旧调用方读它。
        // 多值时取第一项（旧界面是单选，给一个能对上号的值比给空串少一次误导）。
        "allowedModel": list.first().cloned().unwrap_or_default(),
    })
}

/// 保存**单个**模型限制（向后兼容入口，等价于 `set_allowed_models` 传单元素）。
///
/// 空串 = 清除限制。
pub async fn set_allowed_model(model: &str) -> Value {
    let trimmed = model.trim();
    let list: Vec<String> = if trimmed.is_empty() {
        Vec::new()
    } else {
        vec![trimmed.to_string()]
    };
    set_allowed_models(&list).await
}

/// 切换工作模式（自动 / 手动 / 轮转）并**立即生效**。
///
/// 为什么需要这个专用入口：`save_gateway_config` 只写配置文件，
/// 而网关的账号池是**启动时**扫描 `gateway_auths/` 建立的，两者都不会
/// 因改配置而变化。于是用户切到手动模式后，池里仍是全部账号，
/// 必须手动点「重启」才真正生效（这正是「切换模式要重启」的根因）。
///
/// 这里把三件事合成一步：
///  1. 落盘新配置（mode / manual_uids）
///  2. 按新模式重导出凭证（清理不再需要的账号文件）
///  3. 若网关正在运行，重启它以加载新池
///
/// 未运行时只做 1+2：下次启动自然是新池，无需空转重启。
pub async fn switch_mode(mode: GatewayMode, uids: Vec<String>) -> Value {
    // 校验必须在**写配置之前**：否则被拒绝的请求仍会把配置改成非法状态
    //（实测：空勾选被拒后，配置里的 manual_uids 已被清空，
    //  用户原来的勾选被一次无效操作悄悄抹掉）。
    let picked: Vec<String> = uids
        .iter()
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .collect();
    if mode == GatewayMode::Manual && picked.is_empty() {
        return json!({
            "ok": false,
            "error": "手动模式需要先勾选至少一个账号",
        });
    }

    let patch = mode_patch(mode, &picked);
    let cfg = match save_gateway_config(&patch) {
        Ok(v) => v,
        Err(e) => return json!({ "ok": false, "error": e }),
    };

    let (count, changed) = match export_accounts_to_gateway() {
        Ok(v) => v,
        Err(e) => return json!({ "ok": false, "error": e, "config": cfg }),
    };

    let mut reloaded = false;
    if is_running() {
        stop_gateway();
        // 停止后端口需要一点时间释放（TIME_WAIT / 子进程退出），
        // 否则紧接着的 start 会因端口被占用而失败。
        tokio::time::sleep(std::time::Duration::from_millis(600)).await;
        match start_gateway(&cfg).await {
            Ok(_) => {
                reloaded = true;
                update_runtime_state("started", None);
            }
            Err(e) => {
                update_runtime_state("failed", Some(e.clone()));
                return json!({
                    "ok": false,
                    "error": format!("模式已保存，但重启网关失败：{e}"),
                    "accounts": count,
                    "changed": changed,
                    "config": cfg,
                });
            }
        }
    }

    json!({
        "ok": true,
        "mode": mode.as_str(),
        // 勾选的账号列表（前端据此回显勾选状态）
        "manualUids": picked,
        "accounts": count,
        "changed": changed,
        "reloaded": reloaded,
        "config": cfg,
    })
}

/// 从运行中的网关动态拉取上游模型列表。仅从网关实时获取，不使用内置静态模型。
pub async fn fetch_models() -> Vec<Value> {
    let cfg = load_gateway_config();
    let port = cfg.get("port").and_then(Value::as_u64).unwrap_or(7863) as u16;
    let api_key = cfg
        .get("api_key")
        .and_then(Value::as_str)
        .unwrap_or("")
        .to_string();

    let url = format!("http://127.0.0.1:{port}/v1/models");
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_millis(2000))
        .build();

    if let Ok(c) = client {
        let mut req = c.get(&url);
        if !api_key.is_empty() {
            req = req.header("Authorization", format!("Bearer {api_key}"));
        }
        if let Ok(resp) = req.send().await {
            if resp.status().is_success() {
                if let Ok(json) = resp.json::<Value>().await {
                    if let Some(arr) = json.get("data").and_then(Value::as_array) {
                        if !arr.is_empty() {
                            return arr.clone();
                        }
                    }
                }
            }
        }
    }

    // 网关未运行 / 未就绪 → 回退内置静态清单。
    //
    // 为什么必须有回退：模型下拉若为空，用户就无法选择「单一模型」，
    // 而「配置模型」这一步通常发生在**启动网关之前**（先配好再启动）——
    // 若只依赖运行中的网关，这个顺序下功能直接不可用（实测就是空列表）。
    static_models()
}

/// 内置模型清单（网关不可达时的回退）。
///
/// 取自上游已知的常用模型；运行中的网关会返回更权威的动态列表，
/// 此处仅保证「未启动时也能选」。
fn static_models() -> Vec<Value> {
    const IDS: &[&str] = &[
        "deepseek-v4.1-flash",
        "deepseek-v4-flash",
        "deepseek-v4-pro",
        "deepseek-v3-2-volc",
        "glm-5.3",
        "glm-5.3-flash",
        "glm-5.2",
        "glm-4.7",
        "kimi-k3-1",
        "kimi-k2.5",
        "minimax-m3",
        "hunyuan-chat",
        "gpt-5.6-sol",
        "gemini-3.5-flash",
    ];
    IDS.iter()
        .map(|id| json!({ "id": id, "object": "model", "owned_by": "workbuddy" }))
        .collect()
}

/// 从运行中的网关拉取 Token 用量统计（GET /usage）。
///
/// `days` 为统计范围（近 N 天，含今天）；None 或非正值表示全部历史。
/// 网关未运行 / 未就绪 / 返回异常时同样返回结构化结果而不报错，
/// 由调用方依据 `running` / `reachable` / `usage` 给出不同提示：
///   { "running": bool, "reachable": bool, "usage": {...} | null, "error": string | null }
pub async fn fetch_usage(days: Option<i64>) -> Value {
    let cfg = load_gateway_config();
    let port = cfg.get("port").and_then(Value::as_u64).unwrap_or(7863) as u16;
    let api_key = cfg
        .get("api_key")
        .and_then(Value::as_str)
        .unwrap_or("")
        .to_string();

    let mut url = format!("http://127.0.0.1:{port}/usage");
    if let Some(d) = days.filter(|d| *d > 0) {
        url.push_str(&format!("?days={d}"));
    }

    let running = is_running();
    let fail = |error: String| {
        json!({
            "running": running,
            "reachable": false,
            "usage": Value::Null,
            "error": error,
        })
    };

    let client = match reqwest::Client::builder()
        .timeout(std::time::Duration::from_millis(2500))
        .build()
    {
        Ok(c) => c,
        Err(e) => return fail(format!("无法创建 HTTP 客户端: {e}")),
    };

    let mut req = client.get(&url);
    if !api_key.is_empty() {
        req = req.header("Authorization", format!("Bearer {api_key}"));
    }

    match req.send().await {
        Ok(resp) => {
            let status = resp.status().as_u16();
            if !resp.status().is_success() {
                return fail(format!("网关 /usage 返回 HTTP {status}"));
            }
            match resp.json::<Value>().await {
                Ok(v) => json!({
                    "running": running,
                    "reachable": true,
                    "usage": v,
                    "error": Value::Null,
                }),
                Err(e) => fail(format!("解析网关 /usage 响应失败: {e}")),
            }
        }
        Err(e) => fail(e.to_string()),
    }
}

/// 手动触发网关侧的一轮养号任务（活跃上报 / 夜猫子 / 开学季 / trial）。
///
/// 为什么走 HTTP 而不是在宿主里重做一遍：这些任务的实现（上报事件形状、
/// 夜猫时间窗判定、只领已达标奖励的边界）全在 Go 网关里且已有测试覆盖，
/// 宿主只借一个入口，不复制业务逻辑。
///
/// 返回结构固定为
///   { "ok": bool, "ran": bool, "skip": string|null, "message": string, "error": string|null }
/// —— 与 `fetch_usage` 同一约定：网关没起来不是异常，而是由 `ok=false` +
/// 可读的 `error` 表达，调用方据此给出「请先启动网关」这类提示。
///
/// 注意 `ran=false` 且 `skip` 非空是**正常结果**（如夜猫子不在时段内），
/// 界面要把它当说明展示，而不是错误。
///
/// **执行期间会置「网关忙」标志**（见 `TaskBusyGuard`），让后台自动同步推迟重启 ——
/// 否则养号任务写回 token 造成的账号库变化会在下一个 30s 周期把网关重启掉，
/// 直接切断本函数正在等待的这条 HTTP 请求（现象就是界面上的
/// `error sending request for url (...)`）。
pub async fn run_task_now(task: &str) -> Value {
    let task = task.trim();
    if task.is_empty() {
        return json!({
            "ok": false, "ran": false, "skip": Value::Null,
            "message": "", "error": "缺少任务名",
        });
    }

    // 从这里到函数返回全程持有：Drop 即复位，覆盖所有提前 return 与 panic。
    // 放在「任务名校验」之后：空任务名根本没跑，不该占用忙标志。
    let _busy = TaskBusyGuard::acquire(task);

    let cfg = load_gateway_config();
    let port = cfg.get("port").and_then(Value::as_u64).unwrap_or(7863) as u16;
    let api_key = cfg
        .get("api_key")
        .and_then(Value::as_str)
        .unwrap_or("")
        .to_string();

    let url = format!("http://127.0.0.1:{port}/tasks/run");
    let fail = |error: String| {
        json!({
            "ok": false, "ran": false, "skip": Value::Null,
            "message": "", "error": error,
        })
    };

    let client = match reqwest::Client::builder()
        // 超时给得比其它接口宽：活跃上报要按「账号数 × 条数 × 间隔」（条间 800ms）
        // 串行跑完，2500ms 那种探活级超时会让大账号池必然超时。
        // 这里只等入口返回，不等整轮跑完（Go 侧同步执行，故仍需留足余量）。
        .timeout(std::time::Duration::from_secs(120))
        .build()
    {
        Ok(c) => c,
        Err(e) => return fail(format!("无法创建 HTTP 客户端: {e}")),
    };

    let mut req = client.post(&url).json(&json!({ "task": task }));
    if !api_key.is_empty() {
        req = req.header("Authorization", format!("Bearer {api_key}"));
    }

    match req.send().await {
        Ok(resp) => {
            let status = resp.status().as_u16();
            let body: Value = resp.json().await.unwrap_or_else(|_| json!({}));
            if !(200..300).contains(&status) {
                // 网关的错误体是 OpenAI 形状，取出里面的 message 给用户看
                let detail = body
                    .get("error")
                    .and_then(|e| e.get("message"))
                    .and_then(Value::as_str)
                    .filter(|s| !s.trim().is_empty())
                    .map(str::to_string)
                    .unwrap_or_else(|| format!("网关 /tasks/run 返回 HTTP {status}"));
                return fail(detail);
            }
            json!({
                "ok": true,
                "ran": body.get("ran").and_then(Value::as_bool).unwrap_or(false),
                // 空串归一成 null：前端据「有无 skip」分支，空串会让它误以为有原因
                "skip": body.get("skip").and_then(Value::as_str).filter(|s| !s.is_empty()),
                "message": body.get("message").and_then(Value::as_str).unwrap_or(""),
                "error": Value::Null,
            })
        }
        Err(e) => fail(if e.is_connect() {
            "无法连接网关，请先启动网关".to_string()
        } else {
            e.to_string()
        }),
    }
}

/// 成长任务「一键完成」：把请求转给网关的 `/tasks/growth`。
///
/// 与 `run_task_now` 的关键差别是**耗时**：跑一轮全部待办时每个账号都可能
/// 包含真实对话（Go 侧默认 chatGap 6s / reportGap 1.05s），单个账号分钟级是常态，
/// 全账号更久。因此：
///   1. 超时给到 10 分钟（与 Go 侧 `growTaskTimeout` 对齐）——给短了会在任务
///      执行到一半时切断，而那时**已经产生了真实消耗**，半途而废比慢更糟；
///   2. 同样持有 `TaskBusyGuard`：它会推迟自动同步重启，否则「任务跑到一半
///      网关被重启」会重现（那是另一个提交刚修掉的缺陷）。
///
/// `action` 取 `list` / `run` / `run-all`；`run` 需要 `account_id`，
/// 可选 `task_code`（为空 = 跑该账号全部待办）。
pub async fn growth_task(action: &str, account_id: &str, task_code: &str) -> Value {
    let action = action.trim();
    if !matches!(action, "list" | "run" | "run-all") {
        return json!({
            "ok": false,
            "error": "action 需为 list / run / run-all",
        });
    }
    if action != "run-all" && account_id.trim().is_empty() {
        return json!({
            "ok": false,
            "error": "缺少账号 id",
        });
    }

    // 与 run_task_now 同理：全程持有，Drop 即复位。
    // 这同时也让自动同步在成长任务期间推迟重启（见 SYNC_RESTART_PENDING）。
    let _busy = TaskBusyGuard::acquire("growth");

    let cfg = load_gateway_config();
    let port = cfg.get("port").and_then(Value::as_u64).unwrap_or(7863) as u16;
    let api_key = cfg
        .get("api_key")
        .and_then(Value::as_str)
        .unwrap_or("")
        .to_string();

    let url = format!("http://127.0.0.1:{port}/tasks/growth");
    let client = match reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(600))
        .build()
    {
        Ok(c) => c,
        Err(e) => return json!({ "ok": false, "error": format!("无法创建 HTTP 客户端: {e}") }),
    };

    let mut body = json!({ "action": action });
    if !account_id.trim().is_empty() {
        body["accountId"] = json!(account_id.trim());
    }
    if !task_code.trim().is_empty() {
        body["taskCode"] = json!(task_code.trim());
    }

    let mut req = client.post(&url).json(&body);
    if !api_key.is_empty() {
        req = req.header("Authorization", format!("Bearer {api_key}"));
    }

    match req.send().await {
        Ok(resp) => {
            let status = resp.status().as_u16();
            let payload: Value = resp.json().await.unwrap_or_else(|_| json!({}));
            if !(200..300).contains(&status) {
                // 网关的错误体是 OpenAI 形状，取出 message 给用户看；
                // 取不到时退回「HTTP N」而不是编一句像是成功的话。
                let detail = payload
                    .get("error")
                    .and_then(|e| e.get("message"))
                    .and_then(Value::as_str)
                    .filter(|s| !s.trim().is_empty())
                    .map(str::to_string)
                    .unwrap_or_else(|| format!("网关 /tasks/growth 返回 HTTP {status}"));
                return json!({ "ok": false, "error": detail });
            }
            // 直接把网关的结果透出去：成长任务的返回结构（tasks/items/summary）
            // 由 Go 侧定义，宿主不该重编一遍 —— 多一层转换就多一处可能丢字段。
            let mut out = payload;
            if let Some(obj) = out.as_object_mut() {
                obj.insert("ok".to_string(), json!(true));
            }
            out
        }
        Err(e) => json!({
            "ok": false,
            "error": if e.is_connect() {
                "无法连接网关，请先启动网关".to_string()
            } else if e.is_timeout() {
                "任务执行超时（超过 10 分钟）。任务可能仍在网关侧继续，请稍后查看记录。".to_string()
            } else {
                e.to_string()
            },
        }),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // 回归保护：局部更新（如启动后回写 last_status）不得覆盖用户设置。
    // 曾经的缺陷是 merge 以「默认值」为基准，导致 api_key / listen 被重置，
    // 前端账号池查询因此带上空 key 而显示为空。
    #[test]
    fn runtime_state_update_preserves_user_config() {
        let dir = std::env::temp_dir().join(format!("wb-gw-test-{}", std::process::id()));
        let _ = std::fs::create_dir_all(&dir);
        // 直接验证 overlay/merge 的语义（不依赖真实 store 目录）
        let defaults = json!({"listen": ":7863", "api_key": "", "auto_start": false});
        let disk = json!({"listen": ":7899", "api_key": "sk-user", "auto_start": true});
        let with_disk = super::overlay(defaults, &disk);
        let after_runtime = super::overlay(with_disk, &json!({"last_status": "started"}));

        assert_eq!(after_runtime["listen"], ":7899", "监听地址不应被运行态更新覆盖");
        assert_eq!(after_runtime["api_key"], "sk-user", "api_key 不应被运行态更新清空");
        assert_eq!(after_runtime["auto_start"], true, "自动启动设置不应丢失");
        assert_eq!(after_runtime["last_status"], "started");
    }

    #[test]
    fn overlay_only_touches_provided_keys() {
        let base = json!({"a": 1, "b": 2});
        let out = super::overlay(base, &json!({"b": 9}));
        assert_eq!(out["a"], 1);
        assert_eq!(out["b"], 9);
    }

    // 回归保护：切换「负载均衡 ↔ 指定账号」时，导出与清理必须用同一判定。
    // 曾经的缺陷是清理用账号库全集，导致切到指定账号后其余凭证残留，
    // 网关仍把它们加载进池 —— 表现为「模式切换没生效」。
    #[test]
    fn select_export_uids_filters_by_mode() {
        let all = vec!["a".to_string(), "b".to_string(), "c".to_string()];

        // 负载均衡：不过滤
        assert_eq!(select_export_uids(&all, &None), all);

        // 指定账号：只保留选中的
        let only = Some(vec!["b".to_string()]);
        assert_eq!(select_export_uids(&all, &only), vec!["b".to_string()]);

        // 选中的不在账号库里（例如账号已删除）：结果为空 → 清理掉全部残留
        let missing = Some(vec!["zzz".to_string()]);
        assert!(select_export_uids(&all, &missing).is_empty());

        // 空选择：同样应清空（调用方据此报「尚未选择账号」）
        assert!(select_export_uids(&all, &Some(vec![])).is_empty());
    }

    // 账号指纹：内容变化必须导致指纹变化，否则自动同步会漏掉新账号。
    #[test]
    fn fingerprint_reflects_token_and_expiry_changes() {
        let base = vec![
            ("uid-1".to_string(), "AT1".to_string(), 1000i64),
            ("uid-2".to_string(), "AT2".to_string(), 2000i64),
        ];
        let calc = |items: &[(String, String, i64)]| -> String {
            let mut parts: Vec<String> = items
                .iter()
                .map(|(u, at, exp)| format!("{u}|{at}|{exp}"))
                .collect();
            parts.sort();
            let mut hash: u64 = 0xcbf2_9ce4_8422_2325;
            for p in &parts {
                for b in p.as_bytes() {
                    hash ^= *b as u64;
                    hash = hash.wrapping_mul(0x1000_0000_01b3);
                }
            }
            format!("{}-{:x}", parts.len(), hash)
        };

        let f1 = calc(&base);
        assert_eq!(f1, calc(&base), "相同内容必须得到相同指纹");

        // token 变化
        let mut changed = base.clone();
        changed[0].1 = "AT1-NEW".to_string();
        assert_ne!(f1, calc(&changed), "token 变化必须改变指纹");

        // 过期时间变化
        let mut changed2 = base.clone();
        changed2[1].2 = 3000;
        assert_ne!(f1, calc(&changed2), "过期时间变化必须改变指纹");

        // 新增账号
        let mut added = base.clone();
        added.push(("uid-3".to_string(), "AT3".to_string(), 3000));
        assert_ne!(f1, calc(&added), "新增账号必须改变指纹");

        // 删除账号
        let removed = vec![base[0].clone()];
        assert_ne!(f1, calc(&removed), "删除账号必须改变指纹");
    }

    #[test]
    fn finalize_fills_listen_default() {
        let cfg = super::finalize_gateway_config(json!({"listen": "  "}));
        assert_eq!(cfg["listen"], ":7863");
    }

    // 回归保护：被标记「需重新登录」的账号不得导出到网关。
    //
    // 曾经的缺陷是导出只看 uid/模式，完全无视 `needs_relogin` —— 于是 refresh
    // token 已被服务端拒绝的账号仍留在网关池里，网关每次请求都拿它去试一轮，
    // 表现为「网关无视账号需重登，一直用失效账号调用模型」。
    #[test]
    fn needs_relogin_accounts_are_excluded_from_gateway_export() {
        let healthy = json!({"uid": "uid-ok", "access_token": "AT", "domain": "www.workbuddy.ai"});
        let dead = json!({
            "uid": "uid-dead",
            "access_token": "AT",
            "domain": "www.workbuddy.ai",
            "needs_relogin": true,
            "needs_relogin_reason": "刷新失败(code=12153)",
        });

        assert!(!needs_relogin(&healthy));
        assert!(needs_relogin(&dead));

        // 判定口径与导出循环一致：dead 应被 should_export 拒绝。
        let only: Option<Vec<String>> = None;
        let should_export = |acc: &Value| -> bool {
            if needs_relogin(acc) {
                return false;
            }
            let uid = account::get_str(acc, "uid").unwrap_or_default();
            !select_export_uids(&[uid], &only).is_empty()
        };
        assert!(should_export(&healthy), "健康账号必须导出");
        assert!(!should_export(&dead), "需重登账号必须被排除出网关凭证目录");

        // 清理清单同样不含它 —— 否则残留凭证会被网关继续加载进池。
        let accounts = vec![healthy, dead];
        let live: Vec<String> = accounts
            .iter()
            .filter(|a| !needs_relogin(a))
            .filter_map(|a| account::get_str(a, "uid"))
            .collect();
        assert_eq!(live, vec!["uid-ok".to_string()]);
    }

    // 标记翻转必须改变指纹，否则自动同步不会重推凭证（账号会一直留在池里）。
    #[test]
    fn fingerprint_changes_when_relogin_flag_flips() {
        let acc_ok = json!({"uid": "uid-1", "access_token": "AT", "expiresAt": 1000});
        let mut acc_dead = acc_ok.clone();
        acc_dead["needs_relogin"] = json!(true);

        let fingerprint = |a: &Value| -> String {
            let uid = account::get_str(a, "uid").unwrap_or_default();
            let at = account::get_str(a, "access_token").unwrap_or_default();
            let rt = account::get_str(a, "refresh_token").unwrap_or_default();
            let exp = a.get("expiresAt").and_then(Value::as_i64).unwrap_or(0);
            let at_tail: String = at.chars().rev().take(12).collect();
            let rt_len = rt.len();
            let relogin = if needs_relogin(a) { 1 } else { 0 };
            format!("{uid}|{at_tail}|{rt_len}|{}|{relogin}", to_sec(exp))
        };

        assert_ne!(
            fingerprint(&acc_ok),
            fingerprint(&acc_dead),
            "需重登标记必须参与指纹，否则标记翻转后不会触发重新同步"
        );
    }

    /// 端到端：导出到真实目录时，需重登账号的凭证文件必须被写入-剔除。
    ///
    /// 覆盖两种路径：
    ///   1. 标记出现时，已有的凭证文件必须被**删除**（否则网关重启仍会加载它）；
    ///   2. 标记清除后（重新登录成功），凭证必须被**写回**。
    #[test]
    fn export_removes_and_restores_relogin_account_credentials() {
        let dir = std::env::temp_dir().join(format!(
            "wb-gw-export-{}",
            uuid::Uuid::new_v4().simple()
        ));
        std::fs::create_dir_all(&dir).expect("temp dir");

        let healthy = json!({
            "uid": "uid-ok", "nickname": "健康号", "access_token": "AT-OK",
            "refresh_token": "RT-OK", "domain": "www.workbuddy.ai", "expiresAt": 1_900_000_000_000i64,
        });
        let dead = json!({
            "uid": "uid-dead", "nickname": "失效号", "access_token": "AT-DEAD",
            "refresh_token": "RT-DEAD", "domain": "www.workbuddy.ai", "expiresAt": 1_900_000_000_000i64,
            "needs_relogin": true,
            "needs_relogin_reason": "刷新失败(code=12153): Offline user session not found",
        });

        // 1) 健康账号：凭证落盘（count 统计的是处理的账号记录数，不是文件数）
        let (count, _) = export_accounts_to_dir(&dir, &[healthy.clone()], None)
            .expect("export healthy");
        assert_eq!(count, 1);
        assert!(dir.join("workbuddy-uid-ok.json").exists());

        // 先把失效账号以「健康」形态写进去（模拟它此前登录正常、凭证已在池中）
        let dead_before = json!({
            "uid": "uid-dead", "nickname": "失效号", "access_token": "AT-DEAD",
            "refresh_token": "RT-DEAD", "domain": "www.workbuddy.ai", "expiresAt": 1_900_000_000_000i64,
        });
        export_accounts_to_dir(&dir, &[dead_before], None).expect("seed dead before flag");
        assert!(
            dir.join("workbuddy-uid-dead.json").exists(),
            "前置条件：失效账号的凭证文件此刻存在"
        );

        // 2) 账号被打上需重登标记后：它的凭证必须被清理，健康账号保留
        let (_, changed) = export_accounts_to_dir(&dir, &[healthy.clone(), dead.clone()], None)
            .expect("export with dead");
        assert!(
            !dir.join("workbuddy-uid-dead.json").exists(),
            "需重登账号的凭证文件必须被删除，否则网关仍会把它加载进池"
        );
        assert!(
            dir.join("workbuddy-uid-ok.json").exists(),
            "健康账号不受影响"
        );
        assert!(
            changed.iter().any(|c| c == "removed:workbuddy-uid-dead.json"),
            "变更列表应报告删除，便于界面/日志追踪：{changed:?}"
        );

        // 3) 重新登录成功（标记清除）后：凭证必须被写回
        let recovered = json!({
            "uid": "uid-dead", "nickname": "失效号", "access_token": "AT-NEW",
            "refresh_token": "RT-NEW", "domain": "www.workbuddy.ai", "expiresAt": 1_900_000_000_000i64,
        });
        export_accounts_to_dir(&dir, &[healthy, recovered], None).expect("export recovered");
        assert!(
            dir.join("workbuddy-uid-dead.json").exists(),
            "重新登录后凭证必须被写回，账号才能回到池中"
        );
        let written: Value = serde_json::from_str(
            &std::fs::read_to_string(dir.join("workbuddy-uid-dead.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(written["auth"]["accessToken"], "AT-NEW");

        std::fs::remove_dir_all(&dir).ok();
    }

    /// 历史误报标记（code=-1）不得导致账号被移出网关池。
    #[test]
    fn legacy_transport_error_flag_keeps_account_in_pool() {
        let dir = std::env::temp_dir().join(format!(
            "wb-gw-legacy-{}",
            uuid::Uuid::new_v4().simple()
        ));
        std::fs::create_dir_all(&dir).expect("temp dir");

        let legacy = json!({
            "uid": "uid-legacy", "nickname": "被误判的号", "access_token": "AT",
            "refresh_token": "RT", "domain": "www.workbuddy.ai",
            "needs_relogin": true,
            "needs_relogin_reason": "刷新失败(code=-1): error sending request for url (https://www.workbuddy.ai/v2/plugin/auth/token/refresh)",
        });

        let (count, _) =
            export_accounts_to_dir(&dir, &[legacy], None).expect("export legacy");
        assert_eq!(count, 1, "传输层失败留下的误报标记不应把账号排除出池");
        assert!(dir.join("workbuddy-uid-legacy.json").exists());

        std::fs::remove_dir_all(&dir).ok();
    }

    // 模式切换补丁：切到自动模式必须清空勾选列表（否则残留旧值，
    // 下次切回手动时会用到意料之外的账号）。
    #[test]
    fn mode_patch_sets_mode_and_manual_uids() {
        let p = super::mode_patch(GatewayMode::Manual, &["uid-1".to_string()]);
        assert_eq!(p["mode"], "manual");
        assert_eq!(p["manual_uids"][0], "uid-1");

        // 多选：顺序保留、空值被剔除
        let m = super::mode_patch(
            GatewayMode::Manual,
            &["uid-1".to_string(), "  ".to_string(), " uid-2 ".to_string()],
        );
        assert_eq!(m["manual_uids"].as_array().map(Vec::len), Some(2));
        assert_eq!(m["manual_uids"][0], "uid-1");
        assert_eq!(m["manual_uids"][1], "uid-2", "首尾空白应被裁掉");

        // 自动模式：勾选列表清空，不带任何遗留值
        let b = super::mode_patch(GatewayMode::Balance, &[]);
        assert_eq!(b["mode"], "balance");
        assert_eq!(b["manual_uids"].as_array().map(Vec::len), Some(0));
        assert!(b["pinned_uid"].is_null());

        // 即使误传了 uid，自动模式也不应保留（否则模式语义会被绕过）
        let b2 = super::mode_patch(GatewayMode::Balance, &["uid-9".to_string()]);
        assert_eq!(b2["manual_uids"].as_array().map(Vec::len), Some(0));

        // 旧字段 pinned_uid 一律写 null：新版统一用 manual_uids
        for mode in [GatewayMode::Balance, GatewayMode::Manual, GatewayMode::Rotation] {
            let v = super::mode_patch(mode, &["uid-1".to_string()]);
            assert!(v["pinned_uid"].is_null(), "pinned_uid 应恒为 null");
        }
    }

    // 模式字符串解析：未知值一律回落 balance（不报错、不误锁账号）。
    #[test]
    fn gateway_mode_from_str_defaults_to_balance() {
        // 旧值 "pinned" 必须读作手动模式，否则老用户升级后配置失效
        for s in ["pinned", "PINNED", "pin", "single", "manual", "MANUAL"] {
            assert_eq!(
                GatewayMode::from_str(s),
                GatewayMode::Manual,
                "输入 {s:?} 应解析为手动模式"
            );
        }
        assert_eq!(GatewayMode::from_str("balance"), GatewayMode::Balance);
        assert_eq!(GatewayMode::from_str(""), GatewayMode::Balance);
        assert_eq!(GatewayMode::from_str("garbage"), GatewayMode::Balance);
    }

    // 「单一模型 + 积分轮转」模式的解析与序列化。
    #[test]
    fn rotation_mode_round_trips() {
        for s in ["rotation", "ROTATION", "rotate", "rolling"] {
            assert_eq!(GatewayMode::from_str(s), GatewayMode::Rotation, "输入 {s:?}");
        }
        assert_eq!(GatewayMode::Rotation.as_str(), "rotation");
        // 不能被误当成手动（两者语义不同：手动只导出勾选的账号）
        assert_ne!(GatewayMode::from_str("rotation"), GatewayMode::Manual);
    }

    // 手动模式与自动模式的字符串互不混淆。
    #[test]
    fn manual_mode_round_trips() {
        assert_eq!(GatewayMode::Manual.as_str(), "manual");
        assert_eq!(GatewayMode::from_str("manual"), GatewayMode::Manual);
        assert_eq!(GatewayMode::Balance.as_str(), "balance");
        assert_ne!(GatewayMode::Manual, GatewayMode::Balance);
    }

    // 轮转模式的 mode_patch 必须清空勾选列表 —— 否则从手动切过去时会残留值，
    // 而 active_uids() 对轮转是「不过滤」，残留值虽不生效，
    // 但下次切回手动会用到意料之外的账号。
    #[test]
    fn mode_patch_for_rotation_clears_manual_uids() {
        let r = super::mode_patch(GatewayMode::Rotation, &["uid-1".to_string()]);
        assert_eq!(r["mode"], "rotation");
        assert_eq!(r["manual_uids"].as_array().map(Vec::len), Some(0));
        assert!(r["pinned_uid"].is_null(), "轮转模式不应带 pinned_uid");
    }

    // write_native_config 必须把 rotation 从**模式**推导出来（而非独立开关），
    // 避免出现「模式=轮转但 pool.rotation=false」这类自相矛盾的配置。
    #[test]
    fn native_config_derives_rotation_from_mode() {
        // 用 helper 直接构造 native 配置太重（会写盘），这里验证推导规则本身：
        let derive = |mode: &str| {
            matches!(
                GatewayMode::from_str(mode),
                GatewayMode::Rotation
            )
        };
        assert!(derive("rotation"));
        assert!(!derive("balance"));
        assert!(!derive("pinned"));
        assert!(!derive("garbage"));
    }

    // write_native_config 必须把 4 个养号任务的排程写进 native config 的 schedule 块。
    //
    // 回归保护：此前这里只写 checkin/keepalive 两项，4 个任务的字段一个都没有 ——
    // 网关于是用它自己的硬编码默认值，用户在设置页改的任何东西都不落盘、
    // 界面与实际行为不一致，且从界面上完全看不出来。
    #[test]
    fn native_config_writes_care_tasks_schedule() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("gw-care-schedule");
        let cfg = json!({
            "port": 7863,
            "listen": ":7863",
            "activity_hours": [7, 19],
            "nightowl_hours": [2],
            "school_hours": [14, 15],
            "trial_hours": [8],
            "activity_enabled": false,
            "nightowl_enabled": true,
            "school_enabled": false,
            "trial_enabled": true,
            "activity_report_count": 5,
        });

        let path = super::write_native_config(&cfg).expect("write native config");
        let text = std::fs::read_to_string(&path).unwrap();
        let native: Value = serde_json::from_str(&text).unwrap();
        let sched = &native["schedule"];

        // 显式配置的时刻必须逐字落盘
        assert_eq!(sched["activity_hours"], json!([7, 19]), "{text}");
        assert_eq!(sched["nightowl_hours"], json!([2]), "{text}");
        assert_eq!(sched["school_hours"], json!([14, 15]), "{text}");
        assert_eq!(sched["trial_hours"], json!([8]), "{text}");
        // 开关：显式 false 必须落成 false（true 是缺省，测 false 才有区分度）
        assert_eq!(sched["activity_enabled"], json!(false), "{text}");
        assert_eq!(sched["nightowl_enabled"], json!(true), "{text}");
        assert_eq!(sched["school_enabled"], json!(false), "{text}");
        assert_eq!(sched["trial_enabled"], json!(true), "{text}");
        assert_eq!(sched["activity_report_count"], json!(5), "{text}");

        // 既有字段不能被这次改动挤掉
        assert_eq!(sched["checkin_hours"], json!([9, 21]), "{text}");
        assert_eq!(sched["keepalive_hours"], json!([22]), "{text}");
        assert_eq!(sched["checkin_enabled"], json!(true), "{text}");
        assert_eq!(sched["keepalive_enabled"], json!(true), "{text}");
        assert_eq!(sched["checkin_scope"], json!("cn"), "{text}");
    }

    // 键缺席（老配置）时必须落上默认值 —— 且与 Go 侧 Default() 一致。
    //
    // 为什么要断言具体数值而不是「非空即可」：界面读的是宿主配置的默认值，
    // 网关读的是自己 Default() 的默认值；两边一旦漂移，用户「什么都没改直接保存」
    // 就会把排程改成另一套时刻，而界面上显示的还是原来那套。
    #[test]
    fn native_config_fills_care_task_defaults() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("gw-care-defaults");
        // 只给启动必需的字段，养号任务相关键全部缺席（模拟老配置）
        let cfg = json!({ "port": 7863, "listen": ":7863" });

        let path = super::write_native_config(&cfg).expect("write native config");
        let native: Value = serde_json::from_str(&std::fs::read_to_string(&path).unwrap()).unwrap();
        let sched = &native["schedule"];

        assert_eq!(sched["activity_hours"], json!([10]));
        assert_eq!(sched["nightowl_hours"], json!([1]));
        assert_eq!(sched["school_hours"], json!([12]));
        assert_eq!(sched["trial_hours"], json!([9, 21]));
        assert_eq!(sched["activity_enabled"], json!(true));
        assert_eq!(sched["nightowl_enabled"], json!(true));
        assert_eq!(sched["school_enabled"], json!(true));
        assert_eq!(sched["trial_enabled"], json!(true));
        assert_eq!(sched["activity_report_count"], json!(3));
    }

    // write_native_config 必须把账号记录文件路径与账号身份映射交给网关。
    //
    // 回归保护：这 4 个养号任务实现在 Go 网关里，日志此前只写 stdout，
    // 而宿主启动子进程时把它丢进了 Stdio::null —— 用户界面上一条执行记录都没有。
    // 修法是让网关写宿主已经在读的 account_records.json，因此**路径必须由宿主给出**：
    // 两边各自推算数据目录迟早会算出不同的值，而那种错法表现为「静默不记录」。
    #[test]
    fn native_config_passes_account_records_target() {
        let iso = crate::modules::config::test_isolation::Isolated::new("gw-records-target");
        let cfg = json!({ "port": 7863, "listen": ":7863" });

        let path = super::write_native_config(&cfg).expect("write native config");
        let text = std::fs::read_to_string(&path).unwrap();
        let native: Value = serde_json::from_str(&text).unwrap();
        let block = &native["account_records"];

        // 路径必须是**绝对路径**且指向宿主算出的那个文件。
        // 相对路径会让网关按自己的 cwd（gateway/ 目录）解析，写到别处去。
        let file = block["file"].as_str().unwrap_or("");
        assert!(!file.is_empty(), "必须透传记录文件路径: {text}");
        assert!(
            std::path::Path::new(file).is_absolute(),
            "记录文件路径必须是绝对路径（网关 cwd 与宿主不同）: {file}"
        );
        assert_eq!(
            file,
            crate::modules::account_records::account_records_file().to_string_lossy(),
            "路径应取自 account_records_file()，而不是另算一个"
        );
        // 与宿主同一口径的保留天数（避免「界面说 60 天、网关按 7 天清」）
        assert_eq!(
            block["retention_days"].as_i64(),
            Some(crate::modules::config::record_retention_days()),
            "{text}"
        );
        // 隔离目录下账号库为空：映射应存在且为空数组（结构齐全，前端/网关都不必判空）
        assert!(
            block["identities"].is_array(),
            "identities 必须是数组: {text}"
        );

        drop(iso);
    }

    // 账号身份映射必须带 id 与展示名，且**绝不能带 token**。
    //
    // 两个理由：
    //   1. 界面按账号库的 id（uuid）过滤记录，而网关凭证里只有 uid。
    //      网关拿 uid 当 accountId 写，用户点开某个账号永远查不到这些任务。
    //   2. 这份配置会落到磁盘（gateway_native_config.json），凭证不该出现在那里。
    #[test]
    fn gateway_account_identities_carry_id_and_name_without_tokens() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("gw-identities");
        crate::modules::account::save_accounts(&[json!({
            "id": "acct-1",
            "uid": "uid-1",
            "email": "shown@example.com",
            "nickname": "昵称甲",
            "access_token": "SECRET_AT",
            "refresh_token": "SECRET_RT",
        })])
        .expect("seed accounts");

        let list = super::gateway_account_identities();
        let arr = list.as_array().expect("should be array");
        assert_eq!(arr.len(), 1, "{list}");
        assert_eq!(arr[0]["uid"], json!("uid-1"));
        assert_eq!(arr[0]["id"], json!("acct-1"));
        // 展示名与 account_display_name 一致（email 优先）
        assert_eq!(arr[0]["name"], json!("shown@example.com"));

        // 凭证绝不能进配置
        let text = list.to_string();
        assert!(!text.contains("SECRET_AT"), "access_token 泄漏进配置: {text}");
        assert!(!text.contains("SECRET_RT"), "refresh_token 泄漏进配置: {text}");
    }

    // schedule_hours：脏值过滤 + 兜底 + 去重排序。
    //
    // 为什么必须过滤非法小时：Go 侧 validateScheduleHours 对越界值**启动即报错**，
    // 写进 native config 会让网关直接起不来。宿主这边先滤掉，坏值退化为
    // 「该时点不生效」，而不是整个网关挂掉。
    #[test]
    fn schedule_hours_filters_and_falls_back() {
        let calls = |v: Value| super::schedule_hours(&v, "activity_hours", &[10]);

        // 缺键 / null / 空数组 → 回落默认
        assert_eq!(calls(json!({})), json!([10]));
        assert_eq!(calls(json!({"activity_hours": null})), json!([10]));
        assert_eq!(calls(json!({"activity_hours": []})), json!([10]));
        // 全是脏值 → 也回落默认（而不是变成「没有任何时点」= 静默关掉任务）
        assert_eq!(calls(json!({"activity_hours": [24, -1, "x", null]})), json!([10]));

        // 合法值保留；非法值单独剔除而不影响同批合法值
        assert_eq!(calls(json!({"activity_hours": [7]})), json!([7]));
        assert_eq!(calls(json!({"activity_hours": [23, 0]})), json!([0, 23]));
        assert_eq!(
            calls(json!({"activity_hours": [7, 99, 8]})),
            json!([7, 8]),
            "非法值应被剔除，合法值保留"
        );

        // 去重 + 升序：重复时点会让网关在同一时刻跑两轮
        assert_eq!(calls(json!({"activity_hours": [9, 9, 3]})), json!([3, 9]));
        // 浮点小时不接受（界面是整数输入框；2.5 点不是有效排程）
        assert_eq!(calls(json!({"activity_hours": [2.5, 6]})), json!([6]));
    }

    // activity_report_count 的上下界：<=0 回落 3，超大值封顶 20。
    //
    // 为什么封顶：条数直接决定每号每日发往上游的请求数，误填 10000 会被风控
    // 判定成异常流量。上限与「取 3」的理由同源（见 Go 侧常量注释）。
    #[test]
    fn native_config_clamps_activity_report_count() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("gw-report-count");
        let count_for = |v: Value| {
            let path = super::write_native_config(&v).expect("write");
            let native: Value =
                serde_json::from_str(&std::fs::read_to_string(&path).unwrap()).unwrap();
            native["schedule"]["activity_report_count"].clone()
        };

        assert_eq!(count_for(json!({"activity_report_count": 0})), json!(3), "0 应回落默认");
        assert_eq!(count_for(json!({"activity_report_count": -5})), json!(3), "负数应回落默认");
        assert_eq!(count_for(json!({"activity_report_count": 7})), json!(7));
        assert_eq!(count_for(json!({"activity_report_count": 10000})), json!(20), "应封顶 20");
    }

    // default_gateway_config 里的养号任务默认值必须齐全。
    //
    // 它是界面上的初值来源（load_gateway_config 以它为 base 合并磁盘配置）；
    // 缺了字段前端就会显示空白输入框，用户一保存就把网关排程改成空。
    #[test]
    fn default_gateway_config_has_care_task_fields() {
        let cfg = super::default_gateway_config();
        assert_eq!(cfg["activity_hours"], json!([10]));
        assert_eq!(cfg["nightowl_hours"], json!([1]));
        assert_eq!(cfg["school_hours"], json!([12]));
        assert_eq!(cfg["trial_hours"], json!([9, 21]));
        for key in [
            "activity_enabled",
            "nightowl_enabled",
            "school_enabled",
            "trial_enabled",
        ] {
            assert_eq!(cfg[key], json!(true), "{key} 缺省应为 true");
        }
        assert_eq!(cfg["activity_report_count"], json!(3));
    }

    // 凭证导出必须原样带着 credit 块：它是网关「按积分到期分层选号」的依据。
    // 丢了它，每次账号同步都会把依据抹掉一次，表现为分层均衡时灵时不灵。
    #[test]
    fn build_auth_doc_preserves_credit_block() {
        let acc = json!({
            "uid": "u1",
            "access_token": "at",
            "refresh_token": "rt",
            "nickname": "n1",
            "domain": "www.workbuddy.cn",
            "expiresAt": 1793263003967i64,
        });

        // 无 credit：不出现该键（保持文件精简）。
        let (file, text) = super::build_auth_doc(&acc, None).expect("doc");
        assert_eq!(file, "workbuddy-u1.json");
        let doc: Value = serde_json::from_str(&text).unwrap();
        assert!(doc.get("credit").is_none(), "no credit expected: {text}");
        // expiresAt 必须转成秒（网关侧按秒解析）。
        assert_eq!(doc["auth"]["expiresAt"], 1793263003i64);

        // 有 credit：原样透传（网关据此分档）。
        let credit = json!({"soonestExpireAt": 1790639067i64});
        let (_, text2) = super::build_auth_doc(&acc, Some(credit)).expect("doc");
        let doc2: Value = serde_json::from_str(&text2).unwrap();
        assert_eq!(doc2["credit"]["soonestExpireAt"], 1790639067i64);

        // 非对象（脏数据）不应写进凭证。
        let (_, text3) = super::build_auth_doc(&acc, Some(json!("garbage"))).expect("doc");
        let doc3: Value = serde_json::from_str(&text3).unwrap();
        assert!(doc3.get("credit").is_none(), "non-object credit must be ignored");
    }

    // read_credit_block 从已有凭证里取回 credit；文件缺失/损坏/无该键时返回 None。
    #[test]
    fn read_credit_block_handles_missing_and_malformed() {
        let dir = std::env::temp_dir().join(format!("ai-gateway-credit-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();

        let good = dir.join("good.json");
        std::fs::write(&good, r#"{"auth":{},"credit":{"soonestExpireAt":123}}"#).unwrap();
        assert_eq!(
            super::read_credit_block(&good).unwrap()["soonestExpireAt"],
            123
        );

        let nokey = dir.join("nokey.json");
        std::fs::write(&nokey, r#"{"auth":{}}"#).unwrap();
        assert!(super::read_credit_block(&nokey).is_none());

        let bad = dir.join("bad.json");
        std::fs::write(&bad, "not json").unwrap();
        assert!(super::read_credit_block(&bad).is_none());

        assert!(super::read_credit_block(&dir.join("absent.json")).is_none());

        let _ = std::fs::remove_dir_all(&dir);
    }

    // 孤儿进程防护：Job 句柄关闭（父进程结束的等价事件）必须连带终止子进程。
    // 覆盖崩溃/任务管理器强杀等 stop_gateway 来不及执行的情况。
    #[cfg(windows)]
    #[test]
    fn job_object_kills_child_when_handle_closes() {
        use std::time::{Duration, Instant};

        let Some(job) = super::GatewayJob::create() else {
            // 极少数环境不支持 Job Object 时跳过；显式 stop_gateway 仍然有效。
            return;
        };
        let mut child = Command::new("cmd")
            .args(["/C", "ping", "-n", "30", "127.0.0.1"])
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .expect("spawn child");
        assert!(job.assign(&child), "子进程应成功加入 Job");

        drop(job); // 等价于父进程退出时系统自动关闭句柄

        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if child.try_wait().expect("try_wait").is_some() {
                break;
            }
            if Instant::now() >= deadline {
                let _ = child.kill();
                panic!("Job 句柄关闭后子进程仍存活");
            }
            std::thread::sleep(Duration::from_millis(50));
        }
    }

    // 启动路径接线：attach_child_to_job 必须把子进程登记进单例 Job，
    // 否则 Job 兜底形同虚设。
    #[cfg(windows)]
    #[test]
    fn attach_child_registers_with_singleton_job() {
        let mut child = Command::new("cmd")
            .args(["/C", "ping", "-n", "30", "127.0.0.1"])
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .expect("spawn child");
        let Some(job) = super::gateway_job() else {
            // 环境不支持 Job Object 时跳过；显式 stop_gateway 仍然有效。
            let _ = child.kill();
            let _ = child.wait();
            return;
        };
        super::attach_child_to_job(&child);

        use std::os::windows::io::AsRawHandle;
        use windows::Win32::Foundation::{BOOL, HANDLE};
        use windows::Win32::System::JobObjects::IsProcessInJob;
        let mut in_job = BOOL::default();
        unsafe {
            IsProcessInJob(HANDLE(child.as_raw_handle()), job.0, &mut in_job)
                .expect("IsProcessInJob");
        }
        assert!(in_job.as_bool(), "子进程应已加入单例 Job");

        let _ = child.kill();
        let _ = child.wait();
    }

    // -----------------------------------------------------------------------
    // 端口占用者识别与清理
    //
    // 真实链路（起进程占端口 → 识别 → 结束）已由 uitest/verify-port-kill.cjs
    // 端到端验证；这里只覆盖**纯逻辑**与**安全约束**，
    // 避免单元测试去起真实进程（慢且不稳定）。
    // -----------------------------------------------------------------------

    #[test]
    fn our_process_detection_covers_own_binaries() {
        // 本项目的三类进程都应被认出来
        assert!(is_our_process("gateway-35538d-2c4b5c2ff3726ad1.exe", ""));
        assert!(is_our_process("gateway.exe", ""));
        assert!(is_our_process("ai-gateway.exe", ""));
        assert!(is_our_process("wb-switch-rust.exe", ""));
        // 按路径兜底（进程名被改名时仍能认出）
        assert!(is_our_process("whatever.exe", r"C:\Users\x\.ai-gateway\gateway\bin\a.exe"));
        assert!(is_our_process("whatever.exe", r"D:\proj\ai-gateway\target\release\a.exe"));
    }

    #[test]
    fn our_process_detection_rejects_third_party() {
        // 第三方进程不能误判成自己的（否则提示措辞会误导用户）
        assert!(!is_our_process("node.exe", r"C:\Program Files\nodejs\node.exe"));
        assert!(!is_our_process("chrome.exe", r"C:\Program Files\Google\Chrome\chrome.exe"));
        assert!(!is_our_process("nginx.exe", r"C:\nginx\nginx.exe"));
        assert!(!is_our_process("", ""));
    }

    #[test]
    fn kill_rejects_zero_port() {
        // 端口 0 无意义，必须拒绝而不是去查占用者
        let err = kill_port_holder(0).expect_err("端口 0 应被拒绝");
        assert!(err.contains("无效"), "错误文案应说明端口无效，实际: {err}");
    }

    #[test]
    fn kill_rejects_free_port() {
        // 关键安全约束：端口空闲时绝不能去杀任何进程。
        // 用一个极不可能被占用的高位端口，保证「空闲」前提成立。
        let port = 59_873u16;
        if !port_free(port) {
            return; // 环境异常（真被占用），跳过而不是误报
        }
        let err = kill_port_holder(port).expect_err("空闲端口应被拒绝");
        // 必须断言**具体原因**是「空闲」，而不是「找不到占用进程」——
        // 否则把空闲检查那道防线删掉，测试照样通过（本测试第一版就踩了这个坑：
        // 删掉防线后仍走 find_port_holder 返回 None 的分支，同样是 Err）。
        assert!(
            err.contains("空闲"),
            "错误文案应说明端口空闲（而非无法识别进程），实际: {err}"
        );
        assert!(
            !err.contains("无法识别"),
            "不应走到「无法识别占用进程」分支 —— 说明空闲检查被绕过了，实际: {err}"
        );
    }

    #[test]
    fn inspect_port_reports_null_holder_when_free() {
        let port = 59_874u16;
        if !port_free(port) {
            return;
        }
        let v = inspect_port(port);
        assert_eq!(v.get("available").and_then(Value::as_bool), Some(true));
        assert!(
            v.get("holder").map(Value::is_null).unwrap_or(false),
            "空闲端口的 holder 应为 null，实际: {v}"
        );
    }

    #[test]
    fn inspect_port_never_spawns_processes_to_find_holder() {
        // 回归：inspect_port 跑在**同步 Tauri 命令**（主线程）里，
        // 且页面挂载/端口变化时就会被调用。它**不得**去 spawn
        // netstat/tasklist/powershell —— 那会把「打开页面」变成主线程卡顿，
        // 并因频繁创建控制台进程耗尽 desktop heap，
        // 实测表现为「应用程序无法正常启动 (0xc0000142)」。
        //
        // 因此无论端口是否被占用，holder 都必须是 null；
        // 真正需要占用者信息时走按需接口（port_holder / kill_port_holder）。
        let listener = match std::net::TcpListener::bind("127.0.0.1:0") {
            Ok(l) => l,
            Err(_) => return,
        };
        let port = match listener.local_addr() {
            Ok(a) => a.port(),
            Err(_) => return,
        };

        let v = inspect_port(port);
        assert_eq!(
            v.get("available").and_then(Value::as_bool),
            Some(false),
            "正在监听的端口应判定为不可用"
        );
        assert!(
            v.get("holder").map(Value::is_null).unwrap_or(false),
            "inspect_port 不得查占用者（会 spawn 进程阻塞主线程），实际: {v}"
        );
        // 字段本身必须存在：前端类型要求它，缺失会让界面报错
        assert!(
            v.get("holder").is_some(),
            "holder 字段必须存在（可为 null），实际: {v}"
        );

        drop(listener);
    }

    /// 按需查询占用者只在 Windows 上有实现（`netstat` + `tasklist` + `powershell`）。
    ///
    /// 非 Windows 平台的 `find_port_holder` 刻意返回 `None`（见其实现处的注释：
    /// 「前端会提示无法识别占用进程，用户仍可手动处理」）。因此这条断言必须加
    /// 平台门控 —— 否则在 Linux/macOS 的 CI 上必然失败，而那不是缺陷。
    /// 本测试曾漏掉门控，被 CI 抓出（ubuntu-22.04 / macos-14 两个 job 都红）。
    #[cfg(windows)]
    #[test]
    fn port_holder_finds_self_when_listening() {
        // 按需接口本身要能正常工作（这条路径允许 spawn 进程）
        let listener = match std::net::TcpListener::bind("127.0.0.1:0") {
            Ok(l) => l,
            Err(_) => return,
        };
        let port = match listener.local_addr() {
            Ok(a) => a.port(),
            Err(_) => return,
        };

        let holder = port_holder(port);
        assert!(
            holder.as_ref().map(|h| !h.is_null()).unwrap_or(false),
            "按需查询应能识别占用者（本测试进程），实际: {holder:?}"
        );
        if let Some(h) = holder {
            assert_eq!(
                h.get("pid").and_then(Value::as_u64),
                Some(std::process::id() as u64),
                "占用者 PID 应是本进程"
            );
        }

        drop(listener);
    }

    /// 非 Windows 平台上「查不到占用者」是**预期行为**，不是缺陷。
    ///
    /// 单独写这条而不是简单删掉上面的测试：让「这个平台不支持」这件事被显式
    /// 记录在测试里，而不是留白。将来有人补了 lsof/ss 实现，这条会提醒他改。
    #[cfg(not(windows))]
    #[test]
    fn port_holder_returns_none_on_unsupported_platform() {
        let listener = match std::net::TcpListener::bind("127.0.0.1:0") {
            Ok(l) => l,
            Err(_) => return,
        };
        let port = match listener.local_addr() {
            Ok(a) => a.port(),
            Err(_) => return,
        };
        assert!(
            port_holder(port).is_none(),
            "非 Windows 平台尚未实现占用者查询，应返回 None"
        );
        drop(listener);
    }

    /// 「不许杀掉自己」这条守卫。
    ///
    /// Windows：`find_port_holder` 能认出占用者就是本进程 → 命中「自身」分支。
    /// 非 Windows：查询未实现，先撞上「无法识别占用进程」→ 同样拒绝，只是文案不同。
    ///
    /// **两种平台都必须拒绝**，这正是本测试要守的核心行为；但断言文案时必须区分
    /// 平台 —— 曾因只断言「自身」文案，在 Linux/macOS 的 CI 上失败（那不是缺陷）。
    /// 这里把「拒绝」与「文案」拆成两条断言，前者跨平台、后者按平台。
    #[test]
    fn kill_refuses_to_kill_itself() {
        // 自己占住端口，然后尝试「清理」它 —— 必须被拒绝，
        // 否则自杀会让调用方拿不到返回值，前端表现为请求悬挂。
        let listener = match std::net::TcpListener::bind("127.0.0.1:0") {
            Ok(l) => l,
            Err(_) => return,
        };
        let port = match listener.local_addr() {
            Ok(a) => a.port(),
            Err(_) => return,
        };
        let err = kill_port_holder(port).expect_err("不应允许杀死自身进程");

        // 跨平台断言：必须被拒绝，且原因与「清理占用者」有关
        assert!(
            !err.is_empty(),
            "拒绝时必须给出可读原因，而不是空错误"
        );

        #[cfg(windows)]
        assert!(
            err.contains("自身") || err.contains("停止网关"),
            "Windows 上错误文案应说明是自身/应走停止网关，实际: {err}"
        );
        #[cfg(not(windows))]
        assert!(
            err.contains("无法识别"),
            "非 Windows 平台应因「未实现占用者查询」而拒绝，实际: {err}"
        );

        drop(listener);
    }

    // -----------------------------------------------------------------------
    // 账号禁用：不进网关池（需求1）
    //
    // 语义（与所有者确认）：禁用 = 不进账号池；签到 / 旅行 / 上报等养号任务照跑。
    // 因此这里只验证「导出集合」这一个可观测点。
    // -----------------------------------------------------------------------

    #[test]
    fn disabled_account_is_excluded_from_gateway_export() {
        let dir = std::env::temp_dir().join(format!("wb-gw-disabled-{}", uuid::Uuid::new_v4().simple()));
        std::fs::create_dir_all(&dir).expect("temp dir");

        let active = json!({
            "uid": "uid-on", "nickname": "启用号", "access_token": "AT-ON",
            "refresh_token": "RT-ON", "domain": "copilot.tencent.com", "expiresAt": 1_900_000_000_000i64,
        });
        let off = json!({
            "uid": "uid-off", "nickname": "禁用号", "access_token": "AT-OFF",
            "refresh_token": "RT-OFF", "domain": "copilot.tencent.com", "expiresAt": 1_900_000_000_000i64,
            "disabled": true,
        });

        // 先让两个账号都在池里（模拟禁用前的状态）。
        //
        // 注意：seed 时**不能**带 disabled 字段 —— 禁用账号在任何情况下都不导出，
        // 因此必须用「未禁用形态」写入，才能构造出「凭证已在池中」这个前置条件。
        let off_before = json!({
            "uid": "uid-off", "nickname": "禁用号", "access_token": "AT-OFF",
            "refresh_token": "RT-OFF", "domain": "copilot.tencent.com", "expiresAt": 1_900_000_000_000i64,
        });
        export_accounts_to_dir(&dir, &[active.clone(), off_before], None).expect("seed both");
        assert!(dir.join("workbuddy-uid-off.json").exists(), "前置条件：禁用号此刻在池中");

        // 打上禁用标记后：它的凭证必须被清理掉
        export_accounts_to_dir(&dir, &[active.clone(), off.clone()], None).expect("export with disabled");
        assert!(
            !dir.join("workbuddy-uid-off.json").exists(),
            "禁用账号的凭证必须被删除，否则网关仍会把它加载进池"
        );
        assert!(dir.join("workbuddy-uid-on.json").exists(), "启用账号应保留");

        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn disabled_takes_priority_over_manual_selection() {
        // 关键：即使禁用账号出现在手动勾选列表里，也不得导出 ——
        // 否则「禁用」会被模式覆盖，用户会看到「明明禁用了却还在接流量」。
        let dir = std::env::temp_dir().join(format!("wb-gw-prio-{}", uuid::Uuid::new_v4().simple()));
        std::fs::create_dir_all(&dir).expect("temp dir");

        let picked = json!({
            "uid": "uid-picked", "access_token": "AT-P", "domain": "copilot.tencent.com",
            "expiresAt": 1_900_000_000_000i64,
        });
        let picked_but_disabled = json!({
            "uid": "uid-pd", "access_token": "AT-PD", "domain": "copilot.tencent.com",
            "expiresAt": 1_900_000_000_000i64, "disabled": true,
        });

        let only = Some(vec!["uid-picked".to_string(), "uid-pd".to_string()]);
        export_accounts_to_dir(&dir, &[picked, picked_but_disabled], only).expect("export");

        assert!(dir.join("workbuddy-uid-picked.json").exists(), "勾选且启用的应导出");
        assert!(
            !dir.join("workbuddy-uid-pd.json").exists(),
            "勾选但被禁用的不应导出（禁用优先于勾选）"
        );

        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn re_enabling_restores_credential() {
        let dir = std::env::temp_dir().join(format!("wb-gw-reenable-{}", uuid::Uuid::new_v4().simple()));
        std::fs::create_dir_all(&dir).expect("temp dir");

        let base = json!({
            "uid": "uid-r", "access_token": "AT-R", "domain": "copilot.tencent.com",
            "expiresAt": 1_900_000_000_000i64,
        });
        let mut disabled = base.clone();
        disabled["disabled"] = json!(true);

        export_accounts_to_dir(&dir, &[disabled], None).expect("export disabled");
        assert!(!dir.join("workbuddy-uid-r.json").exists());

        // 取消禁用后应恢复进池
        export_accounts_to_dir(&dir, &[base], None).expect("export enabled");
        assert!(
            dir.join("workbuddy-uid-r.json").exists(),
            "取消禁用后凭证应重新写入"
        );

        std::fs::remove_dir_all(&dir).ok();
    }

    // -----------------------------------------------------------------------
    // 手动模式：勾选列表决定导出集合（需求2）
    // -----------------------------------------------------------------------

    #[test]
    fn manual_mode_exports_only_selected_accounts() {
        let dir = std::env::temp_dir().join(format!("wb-gw-manual-{}", uuid::Uuid::new_v4().simple()));
        std::fs::create_dir_all(&dir).expect("temp dir");

        let mk = |uid: &str| {
            json!({
                "uid": uid, "access_token": format!("AT-{uid}"),
                "domain": "copilot.tencent.com", "expiresAt": 1_900_000_000_000i64,
            })
        };
        let all = vec![mk("uid-a"), mk("uid-b"), mk("uid-c")];

        // 自动模式（None = 不过滤）：全部导出
        export_accounts_to_dir(&dir, &all, None).expect("export all");
        for u in ["uid-a", "uid-b", "uid-c"] {
            assert!(dir.join(format!("workbuddy-{u}.json")).exists(), "{u} 应在池中");
        }

        // 手动模式只勾 a 和 c：b 必须被清理
        let only = Some(vec!["uid-a".to_string(), "uid-c".to_string()]);
        export_accounts_to_dir(&dir, &all, only).expect("export manual");
        assert!(dir.join("workbuddy-uid-a.json").exists());
        assert!(dir.join("workbuddy-uid-c.json").exists());
        assert!(
            !dir.join("workbuddy-uid-b.json").exists(),
            "未勾选的账号必须被移出池，否则手动模式形同虚设"
        );

        std::fs::remove_dir_all(&dir).ok();
    }

    // 旧字段兼容：pinned_uid 单值读作「只勾了那一个」。
    #[test]
    fn legacy_pinned_uid_reads_as_single_manual_pick() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("gw-legacy-pinned");
        // 旧配置：只有 mode=pinned + pinned_uid，没有 manual_uids
        crate::modules::gateway::save_gateway_config(&json!({
            "mode": "pinned",
            "pinned_uid": "uid-legacy",
        }))
        .expect("save legacy config");

        // 旧 mode 值应读作 Manual
        assert_eq!(gateway_mode(), GatewayMode::Manual, "旧的 pinned 应读作手动模式");
        // 勾选列表应回退为那个单值
        assert_eq!(manual_uids(), vec!["uid-legacy".to_string()]);
    }

    // 新字段优先于旧字段：两者同时存在时以 manual_uids 为准。
    #[test]
    fn manual_uids_takes_priority_over_legacy_pinned() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("gw-manual-priority");
        crate::modules::gateway::save_gateway_config(&json!({
            "mode": "manual",
            "manual_uids": ["uid-new-1", "uid-new-2"],
            "pinned_uid": "uid-old",
        }))
        .expect("save config");

        assert_eq!(
            manual_uids(),
            vec!["uid-new-1".to_string(), "uid-new-2".to_string()],
            "应优先读新字段，而不是回退到旧的 pinned_uid"
        );
    }

    // 空 manual_uids 数组不应回退到 pinned_uid（显式清空是有意义的意图）。
    #[test]
    fn empty_manual_uids_does_not_fall_back_to_pinned() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("gw-empty-manual");
        crate::modules::gateway::save_gateway_config(&json!({
            "mode": "manual",
            "manual_uids": [],
            "pinned_uid": "uid-stale",
        }))
        .expect("save config");

        assert!(
            manual_uids().is_empty(),
            "空数组是「没勾任何账号」的显式表达，不应回退到旧值"
        );
    }

    // 手动模式空勾选时 active_uids 返回空集合（而不是 None=全部）——
    // 否则「手动但没勾」会静默退化成「自动」，用户意图被无声忽略。
    #[test]
    fn manual_mode_with_no_picks_yields_empty_not_all() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("gw-manual-empty");
        crate::modules::gateway::save_gateway_config(&json!({
            "mode": "manual",
            "manual_uids": [],
        }))
        .expect("save config");

        let uids = active_uids();
        assert!(
            matches!(&uids, Some(list) if list.is_empty()),
            "空勾选应得到空集合（Some(vec![])），而非 None（=不过滤全部）"
        );
    }


    // 回归：校验必须在写配置**之前**。
    //
    // 曾出现的缺陷：空勾选被拒后，配置里的 manual_uids 已被清空 ——
    // 用户一次无效操作就悄悄抹掉了原来的勾选。
    #[test]
    fn rejected_empty_manual_pick_does_not_clobber_config() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("gw-reject-order");

        // 先用合法勾选建立状态
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("runtime");
        let ok = rt.block_on(switch_mode(
            GatewayMode::Manual,
            vec!["uid-keep-1".to_string(), "uid-keep-2".to_string()],
        ));
        assert_eq!(ok.get("ok").and_then(Value::as_bool), Some(true));
        assert_eq!(manual_uids(), vec!["uid-keep-1", "uid-keep-2"]);

        // 空勾选应被拒绝，且**不能**改动已保存的勾选
        let rejected = rt.block_on(switch_mode(GatewayMode::Manual, vec![]));
        assert_eq!(rejected.get("ok").and_then(Value::as_bool), Some(false));
        assert_eq!(
            manual_uids(),
            vec!["uid-keep-1", "uid-keep-2"],
            "被拒绝的请求不得清空原有勾选（校验必须在写配置之前）"
        );
    }

    // -----------------------------------------------------------------------
    // 任务执行期间自动同步不得重启网关（本次修复的竞态）
    //
    // 缺陷现象：点「立即执行」弹出
    //   error sending request for url (http://127.0.0.1:7864/tasks/run)
    // —— reqwest 的**传输层**错误：自动同步在任务执行中途 stop_gateway()
    //    重启，把在途请求切断了。养号任务写回 token 又会改变账号指纹，
    //    于是「任务 → 重启 → 任务失败」自激。
    // -----------------------------------------------------------------------

    /// 串行化「碰进程级全局静态」的测试。
    ///
    /// `TASK_BUSY_COUNT` / `SYNC_DEFER_ROUNDS` / `SYNC_RESTART_PENDING` /
    /// `GATEWAY_RUNNING` 都是 `static`，而 cargo 默认**多线程**跑测试 ——
    /// 不串行化就会看到彼此写到一半的状态（表现为看似随机的失败，
    /// 与 `config::test_isolation` 需要全局锁是同一个原因）。
    fn sync_state_lock() -> std::sync::MutexGuard<'static, ()> {
        static LOCK: OnceLock<Mutex<()>> = OnceLock::new();
        LOCK.get_or_init(|| Mutex::new(()))
            .lock()
            .unwrap_or_else(|e| e.into_inner())
    }

    /// 复位忙标志相关的全局状态，让每个测试从干净状态开始。
    fn reset_sync_globals() {
        TASK_BUSY_COUNT.store(0, Ordering::SeqCst);
        SYNC_DEFER_ROUNDS.store(0, Ordering::SeqCst);
        SYNC_RESTART_PENDING.store(false, Ordering::SeqCst);
        GATEWAY_RUNNING.store(false, Ordering::SeqCst);
        *TASK_RUNTIME.lock().unwrap_or_else(|e| e.into_inner()) = None;
    }

    // 决策表：无条件覆盖三条分支的优先级。
    #[test]
    fn sync_decision_table() {
        let d = |pending, running, busy, rounds| {
            decide_sync_action(pending, running, busy, rounds, MAX_SYNC_DEFER_ROUNDS)
        };

        // 没有待生效变更：空转（绝大多数轮次）
        assert_eq!(d(false, true, false, 0), SyncAction::Idle);
        assert_eq!(d(false, true, true, 0), SyncAction::Idle);
        // 有变更但网关没在跑：无需重启（下次启动自然是新凭证）
        assert_eq!(d(true, false, false, 0), SyncAction::Idle);
        assert_eq!(d(true, false, true, 0), SyncAction::Idle);
        // 有变更、网关在跑、任务在跑：**推迟**而不是重启 —— 这就是本次修复的核心
        assert_eq!(
            d(true, true, true, 0),
            SyncAction::Defer,
            "任务执行期间必须推迟重启，否则会切断在途任务请求"
        );
        // 有变更、网关在跑、任务没跑：正常重启
        assert_eq!(d(true, true, false, 0), SyncAction::Restart);
    }

    // 推迟有上限：不能让自动同步被一个长任务无限期压住
    //（否则账号变更长时间不生效，正是需求明确禁止的）。
    #[test]
    fn defer_limit_forces_restart() {
        let rounds = MAX_SYNC_DEFER_ROUNDS;
        assert_eq!(
            decide_sync_action(true, true, true, rounds - 1, rounds),
            SyncAction::Defer,
            "上限前的最后一轮仍应推迟"
        );
        assert_eq!(
            decide_sync_action(true, true, true, rounds, rounds),
            SyncAction::Restart,
            "达到推迟上限后必须强制执行，宁可打断一次任务也不能让同步停摆"
        );
        // 上限之上同样强制（防御：计数器不该越界，但越界也不能变成永久推迟）
        assert_eq!(
            decide_sync_action(true, true, true, rounds + 5, rounds),
            SyncAction::Restart
        );
    }

    // 核心回归：任务在执行时，自动同步**不得**调 stop_gateway/start_gateway。
    //
    // 断言方式是看 `apply_pending_restart` 的动作：Defer 分支只累加计数，
    // 不碰网关进程。这里刻意把 GATEWAY_RUNNING 置真 —— 否则会走
    // 「网关没在跑 → Idle」，测不到忙时的分支。
    #[test]
    fn auto_sync_defers_while_task_running() {
        let _serial = sync_state_lock();
        reset_sync_globals();

        GATEWAY_RUNNING.store(true, Ordering::SeqCst);
        SYNC_RESTART_PENDING.store(true, Ordering::SeqCst);

        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("runtime");

        // 任务在跑：应推迟，且推迟计数递增
        {
            let _busy = TaskBusyGuard::acquire("school");
            assert!(task_busy());
            rt.block_on(apply_pending_restart());
            assert_eq!(
                SYNC_DEFER_ROUNDS.load(Ordering::SeqCst),
                1,
                "忙时应推迟一轮"
            );
            // 待办标志必须**保留**：否则下一轮 sync_if_changed 返回 false，
            // 重启永远不会发生，自动同步静默停摆。
            assert!(
                SYNC_RESTART_PENDING.load(Ordering::SeqCst),
                "推迟不等于放弃：待生效标志必须留到下一轮"
            );
            rt.block_on(apply_pending_restart());
            assert_eq!(SYNC_DEFER_ROUNDS.load(Ordering::SeqCst), 2);
        }

        // 任务结束后恢复重启能力（此处会在真实实现里 stop/start 网关子进程，
        // 因此只验证「不再推迟」这一判据，不真的执行重启路径）。
        assert!(!task_busy());
        assert_eq!(
            decide_sync_action(true, true, task_busy(), 0, MAX_SYNC_DEFER_ROUNDS),
            SyncAction::Restart,
            "任务结束后下一轮应正常重启"
        );

        reset_sync_globals();
    }

    // 忙碌标志必须在**所有**退出路径上复位 —— 漏掉任何一条都会让自动同步
    // 永久推迟（超过上限后变成每轮都重启），是比原缺陷更糟的故障。
    #[test]
    fn busy_flag_resets_on_early_return_and_panic() {
        let _serial = sync_state_lock();
        reset_sync_globals();

        assert!(!task_busy(), "前置条件：初始应为空闲");

        // 路径 1：提前 return（模拟 run_task_now 里「网关未启动」等分支）
        fn early_return() {
            let _busy = TaskBusyGuard::acquire("activity");
            assert!(task_busy());
            return; // 提前返回，未手工复位
        }
        early_return();
        assert!(!task_busy(), "提前 return 后忙标志必须复位（靠 Drop，不靠手工配对）");
        assert_eq!(SYNC_DEFER_ROUNDS.load(Ordering::SeqCst), 0);

        // 路径 2：panic 展开（run_task_now 里任何 panic 都会走这里）
        let hook = std::panic::take_hook();
        std::panic::set_hook(Box::new(|_| {})); // 静音预期内的 panic，避免污染测试输出
        let caught = std::panic::catch_unwind(|| {
            let _busy = TaskBusyGuard::acquire("nightowl");
            assert!(task_busy());
            panic!("模拟任务执行中 panic");
        });
        std::panic::set_hook(hook);
        assert!(caught.is_err(), "前置条件：闭包应 panic");
        assert!(
            !task_busy(),
            "panic 展开后忙标志必须复位 —— 否则自动同步永久停摆"
        );

        // 推迟额度**不随任务结束清零**：它是「这次待生效的账号变更」的额度，
        // 不属于某个具体任务。若在任务结束就清零，连续点几次「立即执行」
        // 就能把额度一次次续满，上限形同虚设 —— 那正是需求要防的
        // 「无限期推迟自动同步」。额度只在 apply_pending_restart 的
        // Restart / Idle 分支归零。
        SYNC_DEFER_ROUNDS.store(3, Ordering::SeqCst);
        {
            let _busy = TaskBusyGuard::acquire("trial");
        }
        assert_eq!(
            SYNC_DEFER_ROUNDS.load(Ordering::SeqCst),
            3,
            "任务结束不得续满推迟额度，否则连续手动触发可无限期推迟自动同步"
        );

        // 但网关空闲后的下一轮应当真的重启并把额度归零（deferral 是延迟而非停摆）
        assert_eq!(
            decide_sync_action(true, true, task_busy(), 3, MAX_SYNC_DEFER_ROUNDS),
            SyncAction::Restart,
            "任务结束后应立即回到重启分支"
        );
        SYNC_DEFER_ROUNDS.store(0, Ordering::SeqCst);

        reset_sync_globals();
    }

    // 并发任务：计数而非布尔。
    //
    // 用布尔时先结束的那个会把标志清掉，另一个仍在执行的任务就重新暴露在
    // 「自动同步重启」的窗口里 —— 正是本标志要消除的竞态。
    #[test]
    fn concurrent_tasks_keep_busy_until_last_one_finishes() {
        let _serial = sync_state_lock();
        reset_sync_globals();

        let first = TaskBusyGuard::acquire("school");
        let second = TaskBusyGuard::acquire("trial");
        assert!(task_busy());

        drop(first);
        assert!(
            task_busy(),
            "还有一个任务在跑，忙标志不得提前清除（否则它会暴露在重启窗口里）"
        );
        assert_eq!(
            decide_sync_action(true, true, task_busy(), 0, MAX_SYNC_DEFER_ROUNDS),
            SyncAction::Defer
        );

        drop(second);
        assert!(!task_busy());

        reset_sync_globals();
    }

    // 运行态快照：界面「正在执行的任务」的数据来源。
    #[test]
    fn task_runtime_reports_label_and_progress() {
        use crate::modules::config::test_isolation::Isolated;

        let _serial = sync_state_lock();
        reset_sync_globals();
        let _iso = Isolated::new("gw-task-runtime");

        // 空闲时只回 running=false：界面据此分支，不必猜一堆空字段
        let idle = task_runtime();
        assert_eq!(idle.get("running").and_then(Value::as_bool), Some(false));

        // 账号库：2 个国服 + 1 个国际版，其中 1 个国服被禁用
        crate::modules::account::save_accounts(&[
            json!({"id": "a", "uid": "u1", "access_token": "at1", "domain": "www.workbuddy.cn"}),
            json!({"id": "b", "uid": "u2", "access_token": "at2", "domain": "www.workbuddy.cn", "disabled": true}),
            json!({"id": "c", "uid": "u3", "access_token": "at3", "domain": "www.workbuddy.ai"}),
        ])
        .expect("seed accounts");

        let _busy = TaskBusyGuard::acquire("school");
        let live = task_runtime();
        assert_eq!(live.get("running").and_then(Value::as_bool), Some(true));
        assert_eq!(live.get("task").and_then(Value::as_str), Some("school"));
        assert_eq!(
            live.get("label").and_then(Value::as_str),
            Some("开学季活动"),
            "标签必须与 Go 侧写账号记录的标题逐字一致，否则进度恒为 0"
        );
        // 开学季只跑国服：u1 合格，u2 被禁用，u3 是国际版
        assert_eq!(
            live.get("total").and_then(Value::as_u64),
            Some(1),
            "分母应与 Go 侧的区域 + 禁用过滤口径一致"
        );
        assert_eq!(live.get("processed").and_then(Value::as_u64), Some(0));

        drop(_busy);
        assert_eq!(
            task_runtime().get("running").and_then(Value::as_bool),
            Some(false)
        );
        reset_sync_globals();
    }

    // 未知任务名必须仍有一个可读标签（界面显示「养号任务」而不是空白）。
    #[test]
    fn task_label_falls_back_for_unknown_task() {
        assert_eq!(task_label("school"), "开学季活动");
        assert_eq!(task_label("  activity  "), "活跃上报", "应容忍首尾空白");
        assert_eq!(task_label("nope"), "养号任务");
    }

    // -----------------------------------------------------------------------
    // 自定义系统提示词：native config 的 prompt 块
    //
    // 缺陷背景：write_native_config 每次启动网关都**全量重写**
    // gateway_native_config.json，而它此前不写 prompt 块 —— 用户落在该文件里的
    // prompt.mode / prompt.file 会被下次启动静默抹掉，只能在宿主之外启用该功能。
    // -----------------------------------------------------------------------

    // 默认必须是 passthrough：老配置没有这两个键。若缺省成 custom，
    // 既有用户升级后 system 会被静默替换（人设/项目约定/工具说明全丢）。
    #[test]
    fn native_config_prompt_defaults_to_passthrough() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("gw-prompt-default");

        let path = write_native_config(&load_gateway_config()).expect("write native config");
        let text = std::fs::read_to_string(&path).expect("read native config");
        let native: Value = serde_json::from_str(&text).expect("native config is json");

        let prompt = native.get("prompt").expect("prompt 块必须存在，否则启动时会被抹掉");
        assert_eq!(
            prompt.get("mode").and_then(Value::as_str),
            Some("passthrough"),
            "缺省必须是 passthrough：缺省 custom 会在升级后静默替换用户的 system"
        );
        assert_eq!(
            prompt.get("file").and_then(Value::as_str),
            Some(""),
            "未配置时 file 应为空串，由 Go 侧回落到内置默认提示词"
        );
    }

    // 用户显式配置后必须逐字透传。
    #[test]
    fn native_config_writes_custom_prompt() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("gw-prompt-custom");

        let cfg = json!({
            "prompt_mode": "custom",
            "prompt_file": "D:\\prompts\\mine.md",
        });
        let path = write_native_config(&cfg).expect("write native config");
        let native: Value =
            serde_json::from_str(&std::fs::read_to_string(&path).expect("read")).expect("json");

        assert_eq!(
            native.pointer("/prompt/mode").and_then(Value::as_str),
            Some("custom")
        );
        assert_eq!(
            native.pointer("/prompt/file").and_then(Value::as_str),
            Some("D:\\prompts\\mine.md"),
            "路径必须原样透传，宿主不得 substitute 默认路径"
        );
    }

    // mode 归一化：大小写/空白容忍，坏值退化而不是让网关起不来。
    //
    // Go 侧 normalizePrompt 对无法识别的 mode 是**启动即报错**（刻意的 fail fast），
    // 因此宿主必须先把脏值滤掉 —— 否则用户把 custom 拼错会让整个网关起不来。
    #[test]
    fn prompt_mode_normalization_is_defensive() {
        let mode_of = |v: Value| super::prompt_mode_of(&v);

        assert_eq!(mode_of(json!({"prompt_mode": "custom"})), "custom");
        assert_eq!(mode_of(json!({"prompt_mode": "  CUSTOM  "})), "custom", "应容忍大小写与空白");
        // 坏值 → passthrough（而不是把非法值透传给网关）
        assert_eq!(mode_of(json!({"prompt_mode": "costom"})), "passthrough", "拼错应回落而非报错");
        assert_eq!(mode_of(json!({"prompt_mode": ""})), "passthrough");
        assert_eq!(mode_of(json!({"prompt_mode": null})), "passthrough");
        assert_eq!(mode_of(json!({})), "passthrough", "键缺席 = 老配置，必须保持既有行为");
        assert_eq!(mode_of(json!({"prompt_mode": 123})), "passthrough", "类型不对也不应 panic");
    }

    // 宿主配置默认值里必须有这两个键：否则界面写不进去（save_gateway_config
    // 以默认值为基底合并，缺键时用户改的值会被丢弃）。
    #[test]
    fn default_config_exposes_prompt_fields() {
        let cfg = default_gateway_config();
        assert_eq!(
            cfg.get("prompt_mode").and_then(Value::as_str),
            Some("passthrough")
        );
        assert_eq!(cfg.get("prompt_file").and_then(Value::as_str), Some(""));
    }

    // -----------------------------------------------------------------------
    // 「限制使用的模型」白名单（多值 / 三模式 / 默认全部）
    //
    // 本轮把单值 `allowed_model`（仅轮转模式）升级为多值白名单（三个模式都有）。
    // 三条必须守住的线：老的单值字符串继续能读、空 = 不限制、native config
    // 每次启动都写出当前的完整名单。
    // -----------------------------------------------------------------------

    // 老配置的单值字符串必须读成单元素名单。
    //
    // 这是向后兼容的核心：所有既有 gateway_config.json 里这个键都是字符串
    //（实测所有者本机的配置就是 `"allowed_model": "deepseek-v4.1-flash"`）。
    // 只认数组会让名单**静默变成空** = 不限制 —— 表现为「升级后限制突然不管
    // 用了」，比启动报错更危险。
    #[test]
    fn allowed_models_reads_legacy_single_string() {
        let out = super::allowed_models_of(&json!({ "allowed_model": "deepseek-v4.1-flash" }));
        assert_eq!(
            out,
            json!(["deepseek-v4.1-flash"]),
            "老的字符串形状必须读成单元素名单"
        );
    }

    // 新形状：字符串数组。
    #[test]
    fn allowed_models_reads_array() {
        let out = super::allowed_models_of(&json!({ "allowed_model": ["a", "b", "c"] }));
        assert_eq!(out, json!(["a", "b", "c"]));
    }

    // 空值一律 = 不限制（空数组 / null / 空串 / 键缺席）。
    #[test]
    fn allowed_models_empty_means_unrestricted() {
        for cfg in [
            json!({ "allowed_model": [] }),
            json!({ "allowed_model": null }),
            json!({ "allowed_model": "" }),
            json!({ "allowed_model": "   " }),
            json!({}), // 老配置根本没有这个键
        ] {
            assert_eq!(
                super::allowed_models_of(&cfg),
                json!([]),
                "空值必须是不限制（空名单），实际输入 {cfg}"
            );
        }
    }

    // 归一化：逐项 trim、丢弃空项、去重。
    //
    // 去重不是为了省空间 —— 错误信息里出现「a、a、b」会让人以为程序有 bug。
    #[test]
    fn allowed_models_normalizes_items() {
        let out = super::allowed_models_of(&json!({
            "allowed_model": ["  a  ", "", "   ", "a", "b"]
        }));
        assert_eq!(out, json!(["a", "b"]), "应 trim、丢空项、去重");
    }

    // 元素**不**剥区域前缀：那是网关（Go 侧）的职责，两边各写一套必然分叉。
    #[test]
    fn allowed_models_keeps_realm_prefix() {
        let out = super::allowed_models_of(&json!({ "allowed_model": ["cn:a", "global:b"] }));
        assert_eq!(
            out,
            json!(["cn:a", "global:b"]),
            "宿主侧不剥前缀，交给 Go 侧 normalizeAllowedModels 统一处理"
        );
    }

    // write_native_config 必须把多值名单写进 native config 的 pool 块。
    //
    // 缺陷背景：write_native_config 每次启动网关都**全量重写**
    // gateway_native_config.json。它此前只在轮转模式下写这一项，于是自动/手动
    // 模式下用户配的限制被静默丢弃 —— 表现为「界面上勾了、网关照样放行一切」。
    #[test]
    fn native_config_writes_allowed_models_in_all_modes() {
        for mode in ["balance", "manual", "rotation"] {
            let _iso = crate::modules::config::test_isolation::Isolated::new("gw-allowed-models");
            let cfg = json!({
                "mode": mode,
                "allowed_model": ["deepseek-v4.1-flash", "glm-5.3"],
            });
            let path = write_native_config(&cfg).expect("write native config");
            let native: Value =
                serde_json::from_str(&std::fs::read_to_string(&path).expect("read")).expect("json");

            assert_eq!(
                native.pointer("/pool/allowed_model"),
                Some(&json!(["deepseek-v4.1-flash", "glm-5.3"])),
                "mode={mode} 时限制名单必须写进 native config（三个模式都要写）"
            );
        }
    }

    // 未配置时 native config 里必须是**空数组**，不能是空串。
    //
    // 为什么形状要对：Go 侧两种都吃，但数组是「多值」语义的规范形状。
    // 写成空串在老代码里会被当成「有值但为空」的边界去处理，多一层风险。
    #[test]
    fn native_config_writes_empty_array_when_unrestricted() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("gw-allowed-empty");
        let path = write_native_config(&json!({ "mode": "balance" })).expect("write");
        let native: Value =
            serde_json::from_str(&std::fs::read_to_string(&path).expect("read")).expect("json");

        assert_eq!(
            native.pointer("/pool/allowed_model"),
            Some(&json!([])),
            "未配置时必须是空数组（= 不限制）"
        );
    }

    // 老配置的单值字符串也要能写进 native config（读→写的完整往返）。
    #[test]
    fn native_config_writes_legacy_string_as_array() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("gw-allowed-legacy");
        let path =
            write_native_config(&json!({ "allowed_model": "deepseek-v4.1-flash" })).expect("write");
        let native: Value =
            serde_json::from_str(&std::fs::read_to_string(&path).expect("read")).expect("json");

        assert_eq!(
            native.pointer("/pool/allowed_model"),
            Some(&json!(["deepseek-v4.1-flash"])),
            "老的单值字符串应被写成单元素数组，行为不变"
        );
    }

    // -----------------------------------------------------------------------
    // 代理的三个独立开关：native config 的 proxy_scope 块
    //
    // 缺陷背景与 prompt / allowed_model 那两个块同源：write_native_config 每次
    // 启动网关都**全量重写** gateway_native_config.json。不写这个块的话，
    // 用户在界面上关掉国际版代理后，下次启动网关又会按「缺键 → 默认值
    //（intl=true）」把代理打开 —— 表现为「关了开关，重启后又自己开了」。
    // -----------------------------------------------------------------------

    // 三个开关必须真的写进 native config，且**只有 cn / intl 两格**。
    //
    // 为什么刻意断言「没有 github」：更新检查与安装包下载是宿主的活，网关根本
    // 不发往 github.com 的请求。把 github 也写进去会让网关配置里出现一个它永远
    // 不会读的键，下一个人排查「为什么关了开关还在走代理」时会先怀疑这里。
    #[test]
    fn native_config_writes_proxy_scope_without_github() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("gw-proxy-scope");
        // 先把 github_config.json 写成一组**非默认**的开关，确保读的是它而不是
        // 恰好等于默认值的兜底结果（否则测试会因为「默认值碰巧一样」而假绿）。
        crate::modules::update::save_github_config(&json!({
            "proxy": "http://127.0.0.1:7897",
            "proxy_scope": {"github": true, "cn": true, "intl": false},
        }))
        .expect("save github config");

        let path = write_native_config(&json!({})).expect("write native config");
        let native: Value =
            serde_json::from_str(&std::fs::read_to_string(&path).expect("read")).expect("json");

        assert_eq!(
            native.pointer("/proxy_scope/cn"),
            Some(&json!(true)),
            "用户在界面上打开的国服代理必须写进 native config"
        );
        assert_eq!(
            native.pointer("/proxy_scope/intl"),
            Some(&json!(false)),
            "用户在界面上关掉的国际版代理必须写进 native config（否则重启网关又自己开了）"
        );
        // 地址照旧原样透传：开关只决定「用不用」，不改地址本身。
        assert_eq!(
            native.pointer("/proxy").and_then(Value::as_str),
            Some("http://127.0.0.1:7897"),
            "代理地址必须原样写出"
        );
        // github 那一格**不进**网关配置（详见本用例开头）。
        let scope = native.get("proxy_scope").expect("proxy_scope 块必须存在");
        assert!(
            scope.get("github").is_none(),
            "proxy_scope 不该含 github：那是宿主的活，网关不发往 github.com 的请求"
        );
        assert_eq!(
            scope.as_object().map(|o| o.len()),
            Some(2),
            "proxy_scope 应恰好只有 cn / intl 两个键，实际 {scope}"
        );
    }

    // 老配置（github_config.json 里没有 proxy_scope）→ native config 写出默认值。
    //
    // 这是升级路径的落点：写出的必须是「国际版开、国服关」= 本次改动前的行为。
    // 若这里写成全 false，所有既有用户一升级，国际版的代理就静默失效了。
    #[test]
    fn native_config_proxy_scope_defaults_for_legacy_config() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("gw-proxy-legacy");
        // 老配置：只有 owner / repo / proxy 三个键，**没有** proxy_scope。
        crate::modules::update::save_github_config(&json!({
            "proxy": "http://127.0.0.1:7890",
        }))
        .expect("save github config");
        // 复核前提：磁盘上确实没有这个键（save 若顺手补了它，本用例就测不到
        // 「读取侧对缺失的兜底」了）。
        let raw = std::fs::read_to_string(crate::modules::update::github_config_file())
            .expect("read github config");
        let raw_json: Value = serde_json::from_str(&raw).expect("json");
        assert_eq!(
            raw_json.pointer("/proxy_scope/intl"),
            Some(&json!(true)),
            "save 时缺键应已按默认值补齐（否则下面的断言测的是别的路径）"
        );

        let path = write_native_config(&json!({})).expect("write native config");
        let native: Value =
            serde_json::from_str(&std::fs::read_to_string(&path).expect("read")).expect("json");

        assert_eq!(
            native.pointer("/proxy_scope/intl"),
            Some(&json!(true)),
            "老配置升级后国际版必须仍走代理（否则国际版账号会莫名开始超时）"
        );
        assert_eq!(
            native.pointer("/proxy_scope/cn"),
            Some(&json!(false)),
            "老配置升级后国服必须仍直连（升级不得把国服新绕进代理）"
        );
    }

    // 未配代理时 native config 也要有这个块（默认值），且 proxy 是空串。
    //
    // 为什么仍要写：与 prompt 块同理，本函数全量重写该文件。块缺席时 Go 侧的
    // 兜底是 Default()（同样是国际版开、国服关），所以**行为**等价；但显式写出
    // 能让落到磁盘的配置自解释 —— 用户直接打开 gateway_native_config.json 时
    // 能看到「网关侧认为这两个开关是什么」，不必去猜默认值。
    #[test]
    fn native_config_writes_proxy_scope_even_without_proxy() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("gw-proxy-none");
        crate::modules::update::save_github_config(&json!({ "proxy": "" })).expect("save");

        let path = write_native_config(&json!({})).expect("write native config");
        let native: Value =
            serde_json::from_str(&std::fs::read_to_string(&path).expect("read")).expect("json");

        assert_eq!(
            native.pointer("/proxy").and_then(Value::as_str),
            Some(""),
            "未配代理时 proxy 应为空串"
        );
        assert_eq!(
            native.pointer("/proxy_scope/intl"),
            Some(&json!(true)),
            "即使没填地址也要写出开关（自解释，且与 Go 侧默认值一致）"
        );
        assert_eq!(native.pointer("/proxy_scope/cn"), Some(&json!(false)));
    }

    // 默认配置里不能有限制（「默认是全部」这条需求在配置层的落点）。
    #[test]
    fn default_config_has_no_allowed_models() {
        let cfg = default_gateway_config();
        assert_eq!(
            cfg.get("allowed_model"),
            Some(&json!([])),
            "默认必须是空名单（= 全部放行），否则全新安装的用户一启动就被限制"
        );
    }

    // default_config_has_no_allowed_models 与 finalize 的归一化：见下方
    // finalize_normalizes_legacy_string_to_array。这里先补后者。
    //
    // finalize_gateway_config 把老的单值字符串归一化成数组。
    //
    // 为什么要在配置出口归一化：`/api/gateway/config` 与 `/status.config` 的形状
    // 恒定，界面才不必为「这次拿到的是字符串还是数组」分两条渲染路径；
    // 且任何一次保存都会把磁盘上的老形状顺手升级，配置随时间自然收敛。
    // 注意**读取侧仍必须吃字符串**（配置文件也可能被直接编辑、或由老宿主写入）
    // —— 归一化只保证「经过本函数之后」的形状。
    #[test]
    fn finalize_normalizes_legacy_string_to_array() {
        let out = super::finalize_gateway_config(json!({
            "port": 7863,
            "allowed_model": "deepseek-v4.1-flash",
        }));
        assert_eq!(
            out.get("allowed_model"),
            Some(&json!(["deepseek-v4.1-flash"])),
            "老的单值字符串应在配置出口归一化成单元素数组"
        );

        // 已经是数组的原样保留（含顺序），不重排 —— 用户看到的顺序应与勾选顺序一致。
        let arr = super::finalize_gateway_config(json!({
            "port": 7863,
            "allowed_model": ["b", "a"],
        }));
        assert_eq!(arr.get("allowed_model"), Some(&json!(["b", "a"])));

        // 键缺席（老配置的常见情形）→ 空数组，而不是保持缺席。
        // 保持缺席会让界面读到 undefined，多一处判空分支。
        let absent = super::finalize_gateway_config(json!({ "port": 7863 }));
        assert_eq!(absent.get("allowed_model"), Some(&json!([])));
    }

    // set_allowed_models 的落盘补丁：归一化后写入，空 → 空数组。
    //
    // 只测纯函数 `allowed_models_patch`，不测 `set_allowed_models` 本身：
    // 后者会写用户配置并**重启网关**，在单测里跑会动到真实进程。
    #[test]
    fn allowed_models_patch_normalizes_and_clears() {
        let patch = super::allowed_models_patch(&[
            "  glm-5.3  ".to_string(),
            "".to_string(),
            "glm-5.3".to_string(),
        ]);
        assert_eq!(
            patch,
            json!({ "allowed_model": ["glm-5.3"] }),
            "补丁必须归一化（trim + 丢空项 + 去重）"
        );

        let cleared = super::allowed_models_patch(&[]);
        assert_eq!(
            cleared,
            json!({ "allowed_model": [] }),
            "空输入 = 清除限制，写成空数组而不是 null（形状统一，便于前端回显）"
        );
    }
}
