//! Tauri commands：前端调用的薄包装，对应 Python 版 HTTP API。
//!
//! 阶段 1 覆盖：get_status / get_accounts / delete_account / oauth_start /
//! oauth_status / import_local。

use serde::Serialize;
use serde_json::{json, Value};

use tauri::Emitter;
use ai_gateway_core::modules::{
    account, auth_file, checkin, codebuddy_cli, codebuddy_cn_ide, config, credit_usage, credits, export_import, oauth,
    process, refresh, rotate, session, switch, token_stats, travel, update,
};

#[derive(Serialize)]
pub struct AppStatus {
    running: bool,
    auth_file: String,
    current: Option<Value>,
    app_path: String,
    version: String,
}

/// GET /api/status —— WorkBuddy 运行状态 + 当前账号。
#[tauri::command]
pub async fn get_status() -> Result<AppStatus, String> {
    // Windows 的运行状态检测会启动 tasklist 子进程。同步 command 默认在
    // Tauri 主线程执行，标题栏拖拽期间一旦焦点事件触发状态刷新，就会阻塞
    // 原生窗口消息循环。放入 blocking 线程，保持窗口移动与 IPC 查询解耦。
    tauri::async_runtime::spawn_blocking(build_app_status)
        .await
        .map_err(|error| format!("查询应用状态失败: {error}"))
}

fn build_app_status() -> AppStatus {
    let auth = auth_file::read_auth_file();
    let current = auth.as_ref().and_then(|a| {
        let acct = a.get("account").cloned().unwrap_or_else(|| json!({}));
        Some(json!({
            "uid": acct.get("uid"),
            "nickname": acct.get("nickname"),
            "email": acct.get("email"),
        }))
    });
    AppStatus {
        running: process::is_workbuddy_running(),
        auth_file: auth_file::auth_file_path().to_string_lossy().to_string(),
        current,
        app_path: auth_file::workbuddy_app_path()
            .to_string_lossy()
            .to_string(),
        version: update::APP_VERSION.to_string(),
    }
}

/// GET /api/accounts —— 账号列表（account_meta，不含 token）。
#[tauri::command]
pub fn get_accounts() -> Value {
    let metas: Vec<Value> = account::load_accounts()
        .iter()
        .map(account::account_meta)
        .collect();
    json!({ "accounts": metas })
}

/// GET /api/codebuddy-cli/status —— CodeBuddy CLI helper 轮换状态（不含 token）。
///
/// async + spawn_blocking：状态检测可能执行 ps / helper 定位等子进程，
/// 避免在账号页挂载刷新时阻塞主线程造成页面卡顿。
#[tauri::command]
pub async fn get_codebuddy_cli_status() -> Result<Value, String> {
    tauri::async_runtime::spawn_blocking(codebuddy_cli::status)
        .await
        .map_err(|error| format!("查询 CodeBuddy CLI 状态失败: {error}"))
}

/// POST /api/codebuddy-cli/install-helper —— 显式安装/升级 CLI helper。
#[tauri::command]
pub async fn install_codebuddy_cli_helper() -> Result<Value, String> {
    tauri::async_runtime::spawn_blocking(codebuddy_cli::install_helper)
        .await
        .map_err(|e| e.to_string())?
}

/// POST /api/codebuddy-cli/switch —— 只切换 CodeBuddy CLI，不重启 WorkBuddy。
///
/// async + spawn_blocking：切换会用登录 shell 定位 node 并执行 apiKeyHelper
/// 校验账号（子进程无超时），同步 command 会阻塞主线程造成 UI 卡顿。
#[tauri::command(rename_all = "camelCase")]
pub async fn switch_codebuddy_cli_account(account_id: String) -> Result<Value, String> {
    if account_id.trim().is_empty() {
        return Err("缺少 accountId".to_string());
    }
    tauri::async_runtime::spawn_blocking(move || codebuddy_cli::set_active_account(&account_id))
        .await
        .map_err(|e| e.to_string())?
}

/// GET /api/codebuddy-cn-ide/status —— CodeBuddy IDE 安装/运行/当前账号。
///
/// async + spawn_blocking：状态检测会跑 tasklist / PowerShell 等子进程（可能
/// 耗时数秒），账号页每次挂载都会刷新，若在主线程执行会造成页面卡顿。
#[tauri::command]
pub async fn get_codebuddy_cn_ide_status() -> Result<Value, String> {
    tauri::async_runtime::spawn_blocking(codebuddy_cn_ide::status)
        .await
        .map_err(|error| format!("查询 CodeBuddy IDE 状态失败: {error}"))
}

/// POST /api/codebuddy-cn-ide/switch —— 注入凭证并可选重启 CodeBuddy CN IDE。
///
/// async + spawn_blocking：切换会关闭并重启 CodeBuddy CN，可能阻塞数十秒，
/// 与 WorkBuddy 切换同理，若在同步 command（主线程）执行会卡死整个 UI。
#[tauri::command(rename_all = "camelCase")]
pub async fn switch_codebuddy_cn_ide_account(
    account_id: String,
    restart: Option<bool>,
) -> Result<Value, String> {
    if account_id.trim().is_empty() {
        return Err("缺少 accountId".to_string());
    }
    tauri::async_runtime::spawn_blocking(move || {
        codebuddy_cn_ide::switch_account(&account_id, restart.unwrap_or(true))
    })
    .await
    .map_err(|e| e.to_string())?
}

/// POST /api/codebuddy-cn-ide/detect —— 读取本机 CN IDE 当前登录并尝试匹配账号库。
///
/// async + spawn_blocking：会通过 Keychain/secret 读取子进程，避免阻塞主线程。
#[tauri::command]
pub async fn detect_codebuddy_cn_ide_account() -> Result<Value, String> {
    tauri::async_runtime::spawn_blocking(codebuddy_cn_ide::detect_current_account)
        .await
        .map_err(|e| e.to_string())?
}


/// DELETE /api/delete —— 删除账号。
#[tauri::command]
pub fn delete_account(account_id: String) -> Result<Value, String> {
    let mut accounts = account::load_accounts();
    let before = accounts.len();
    accounts.retain(|a| a.get("id").and_then(|v| v.as_str()) != Some(account_id.as_str()));
    if accounts.len() == before {
        return Err("账号不存在".to_string());
    }
    account::save_accounts(&accounts).map_err(|e| e.to_string())?;
    Ok(json!({ "ok": true }))
}

/// POST /api/accounts/note —— 设置账号备注（空串 = 清除）。
///
/// 备注是用户自定义标签（如「公司号」「备用」），用于认出「这是谁的号」；
/// 只存本地账号库，不参与登录、不触碰任何凭证字段。
#[tauri::command]
pub fn set_account_note(account_id: String, note: String) -> Result<Value, String> {
    let acc = account::set_account_note(&account_id, &note)?;
    Ok(json!({ "ok": true, "account": account::account_meta(&acc) }))
}

/// POST /api/accounts/disabled —— 设置账号的禁用状态。
///
/// 语义（与所有者确认）：禁用 = **不进网关账号池**，但签到 / 旅行 / 上报等
/// 养号任务照跑。「号暂时不接流量」不等于「不要额度与连登天数」，
/// 把两者绑死会让用户失去「先养着，以后再启用」这个最常用的用法。
///
/// 禁用只改本地账号库字段；真正生效需要在导出凭证时过滤（见 gateway.rs 的
/// should_export），因此这里顺带触发一次重导出 + 重启，做到「点了就生效」。
#[tauri::command]
pub async fn set_account_disabled(account_id: String, disabled: bool) -> Result<Value, String> {
    let acc = account::set_account_disabled(&account_id, disabled)?;
    // 禁用状态直接影响导出集合：立刻重导出，网关在跑则重启以加载新池。
    // 失败不阻断（账号状态已存好），但要如实回报，否则用户以为没生效。
    let sync = ai_gateway_core::modules::gateway::sync_and_reload(true).await;
    Ok(json!({
        "ok": true,
        "account": account::account_meta(&acc),
        "sync": sync,
    }))
}

/// POST /api/oauth/start —— 发起 OAuth 扫码登录。
///
/// `region` 为 `"cn"`（缺省）或 `"intl"`：决定取 state 的域名与平台标识
/// （国服 `workbuddy` / 国际版 `workbuddy-ai`）。
#[tauri::command]
pub async fn oauth_start(region: Option<String>) -> Result<Value, String> {
    let region = config::Region::from_key(region.as_deref().unwrap_or(""));
    oauth::oauth_start(region).await
}

/// GET /api/oauth/status —— 轮询采集结果。
#[tauri::command]
pub async fn oauth_status(login_id: String) -> Value {
    oauth::oauth_poll(&login_id).await
}

/// POST /api/import-local —— 从本机一键导入账号。
///
/// 会同时探测国服（workbuddy-desktop.info）与国际版
/// （workbuddy-desktop-ai.info）两个认证文件，把能读到的账号全部并入账号库。
/// 返回 `accounts` 数组（含 region 字段）；`account` 保留为首个账号以兼容旧前端。
#[tauri::command]
pub fn import_local() -> Result<Value, String> {
    let list = account::import_local_all()?;
    Ok(json!({
        "ok": true,
        "imported": list.len(),
        "accounts": list,
        "account": list.first().cloned(),
    }))
}

/// GET /api/import-local/scan —— 扫描本机全部历史登录态。
///
/// 除两个固定认证文件（当前登录态）外，还会扫出客户端留存的历史登录快照与
/// 本工具切换前的备份，按「区域 + uid」去重后只保留凭证最新的一份。
/// 返回的候选项不含 token，仅用于界面展示与勾选。
#[tauri::command]
pub fn scan_local_accounts() -> Value {
    let scan = account::scan_local_accounts();
    json!({
        "ok": true,
        "candidates": scan.candidates,
        "total": scan.candidates.len(),
        "filesScanned": scan.files_scanned,
        "usable": scan.usable,
        "authDir": scan.auth_dir,
        "backupDir": scan.backup_dir,
    })
}

/// POST /api/import-local/selected —— 按来源文件路径（或扫描索引）批量导入。
#[tauri::command]
pub fn import_local_selected(paths: Vec<String>, indexes: Vec<usize>) -> Result<Value, String> {
    let result = account::import_local_selected(&paths, &indexes)?;
    Ok(json!({
        "ok": true,
        "imported": result.imported,
        "added": result.added,
        "updated": result.updated,
        "outcomes": result.outcomes,
    }))
}

// ---------------------------------------------------------------------------
// 导出 / 导入账号
// ---------------------------------------------------------------------------

/// POST /api/export-accounts —— 按账号 id 列表导出完整记录（含 token）。
#[tauri::command]
pub fn export_accounts(account_ids: Vec<String>) -> Result<Value, String> {
    export_import::export_accounts(&account_ids)
        .map(|records| json!({ "ok": true, "accounts": records }))
}

/// POST /api/export-accounts-to-path —— 把勾选账号的完整记录写入用户选择的路径（保存对话框产物）。
#[tauri::command]
pub fn export_accounts_to_path(account_ids: Vec<String>, path: String) -> Result<Value, String> {
    export_import::export_accounts_to_path(&account_ids, &path)
        .map(|path| json!({ "ok": true, "path": path }))
}

/// POST /api/import/preview —— 解析导入文件并返回脱敏预览（含文件内索引）。
#[tauri::command]
pub fn preview_import_accounts(file_text: String) -> Result<Value, String> {
    export_import::preview_accounts(&file_text)
}

/// POST /api/import —— 按选中索引把账号导入账号库，返回导入/跳过/覆盖计数。
#[tauri::command]
pub fn import_accounts(file_text: String, indexes: Vec<usize>) -> Result<Value, String> {
    let result = export_import::import_accounts(&file_text, &indexes)?;
    Ok(json!({
        "ok": true,
        "imported": result.imported,
        "skipped": result.skipped,
        "overwritten": result.overwritten,
    }))
}

/// 打开系统设置授权面板。默认「完全磁盘访问」（该 anchor 各版本均有效）；
/// 传 `target="app_management"` 尝试「App 管理」（macOS 15+，部分版本不支持深链）。
///
/// 使用 macOS 13+ 深链接格式（`com.apple.settings.PrivacySecurity.extension?Privacy_*`）。
#[tauri::command]
pub fn open_permission_settings(target: Option<String>) -> Result<(), String> {
    let t = target.unwrap_or_else(|| "all_files".to_string());
    let url = match t.as_str() {
        "app_management" => {
            "x-apple.systempreferences:com.apple.settings.PrivacySecurity.extension?Privacy_AppManagement"
        }
        _ => {
            "x-apple.systempreferences:com.apple.settings.PrivacySecurity.extension?Privacy_AllFiles"
        }
    };
    let _ = std::process::Command::new("open").arg(url).spawn();
    Ok(())
}

/// 权限自检：尝试在认证文件目录写/删探针文件，确认完全磁盘访问等授权是否生效。
#[tauri::command]
pub fn check_auth_permission() -> Value {
    let path = auth_file::auth_file_path();
    let probe = path.with_file_name("workbuddy-desktop.info.probe");
    match std::fs::write(&probe, "probe") {
        Ok(_) => {
            let _ = std::fs::remove_file(&probe);
            json!({ "ok": true, "message": "认证目录可写，权限正常" })
        }
        Err(e) => json!({
            "ok": false,
            "error": e.to_string(),
            "dir": path.parent().map(|p| p.to_string_lossy().to_string()),
            "hint": "请在 系统设置→隐私与安全性 中授权：优先「App 管理」开启 ai-gateway，若没有则去「完全磁盘访问」把 ai-gateway 拖进去；授权后需重启 App 生效",
        }),
    }
}

/// 在 Finder 中显示当前 App（便于拖拽到「完全磁盘访问」授权框）。
#[tauri::command]
pub fn reveal_app_in_finder() -> Result<(), String> {
    let exe = std::env::current_exe().map_err(|e| e.to_string())?;
    let _ = std::process::Command::new("open")
        .arg("-R")
        .arg(&exe)
        .spawn();
    Ok(())
}

/// POST /api/switch —— 切换账号（备份 → 关进程 → 复制会话 → 写认证 → 重启）。
///
/// async + spawn_blocking：切换中关闭/启动 WorkBuddy 会阻塞数十秒，
/// 若在同步 command（主线程）执行会卡死整个 UI（loading 遮罩无法渲染）。
#[tauri::command(rename_all = "camelCase")]
pub async fn switch_account(
    app: tauri::AppHandle,
    account_id: String,
    restart: Option<bool>,
    share_sessions: Option<bool>,
    copy_session_ids: Option<Vec<String>>,
) -> Result<Value, String> {
    if account_id.trim().is_empty() {
        return Err("缺少 accountId".to_string());
    }
    let restart = restart.unwrap_or(true);
    let share_sessions = share_sessions.unwrap_or(false);
    let copy_ids = copy_session_ids.unwrap_or_default();
    let progress: switch::ProgressFn = Box::new(move |message| {
        let _ = app.emit("switch-progress", json!({ "message": message }));
    });
    tauri::async_runtime::spawn_blocking(move || {
        switch::switch_account(
            Some(&progress),
            &account_id,
            restart,
            share_sessions,
            &copy_ids,
        )
    })
    .await
    .map_err(|e| e.to_string())?
}

/// GET /api/sessions —— 当前账号的会话列表。
#[tauri::command]
pub fn list_sessions() -> Value {
    match session::current_user_uid() {
        Some(uid) => json!({
            "sessions": session::list_sessions_for_user(&uid),
            "current": uid,
        }),
        None => json!({"sessions": [], "current": Value::Null}),
    }
}

/// POST /api/sessions/copy —— 把勾选会话复制到指定账号（路径 B）。
#[tauri::command(rename_all = "camelCase")]
pub async fn copy_sessions(
    target_account_id: String,
    session_ids: Vec<String>,
) -> Result<Value, String> {
    if target_account_id.trim().is_empty() {
        return Err("缺少 targetAccountId".to_string());
    }
    if session_ids.is_empty() {
        return Err("缺少 sessionIds".to_string());
    }
    tauri::async_runtime::spawn_blocking(move || {
        let target = account::find_account(&target_account_id).ok_or("目标账号不存在")?;
        Ok(session::copy_sessions_for_switch(&target, &session_ids).unwrap_or_else(|| json!({})))
    })
    .await
    .map_err(|e| e.to_string())?
}

// ---------------------------------------------------------------------------
// 阶段 3：签到 + token 刷新
// ---------------------------------------------------------------------------

/// GET /api/checkin/status —— 查询单账号签到状态。
#[tauri::command]
pub async fn get_checkin_status(account_id: String) -> Result<Value, String> {
    let acc = account::find_account(&account_id).ok_or("账号不存在")?;
    Ok(checkin::get_checkin_status(&acc).await)
}

/// POST /api/credits —— 查询单账号积分资源及到期时间。
#[tauri::command]
pub async fn get_credit_expiry(account_id: String) -> Result<Value, String> {
    let acc = account::find_account(&account_id).ok_or("账号不存在")?;
    Ok(credits::get_credit_expiry(&acc).await)
}

/// GET /api/credits/stats —— 本地快照与官方请求用量统计。
/// `refresh = true` 时才重新请求官方用量；默认读缓存。
#[tauri::command]
pub async fn get_credit_statistics(refresh: Option<bool>) -> Value {
    credit_usage::get_statistics(refresh.unwrap_or(false)).await
}

#[tauri::command]
pub async fn get_token_statistics(days: Option<i64>) -> Result<Value, String> {
    tauri::async_runtime::spawn_blocking(move || token_stats::get_statistics(days))
        .await
        .map_err(|error| format!("扫描 Token 统计失败: {error}"))
}

/// POST /api/checkin —— 单账号立即签到。
#[tauri::command]
pub async fn checkin(account_id: String) -> Result<Value, String> {
    let acc = account::find_account(&account_id).ok_or("账号不存在")?;
    Ok(checkin::checkin_account(&acc).await)
}

/// POST /api/checkin/all —— 全部账号立即签到。
#[tauri::command]
pub async fn checkin_all() -> Value {
    checkin::run_checkin_all().await
}

/// POST /api/travel/run —— 全部账号立即走一趟旅行巡检（一键旅行）。
///
/// 手动触发**不**检查「自动旅行」开关：该开关只管后台是否自动跑，
/// 用户主动点击就该执行（与 `checkin_all` 的既有行为一致）。
#[tauri::command]
pub async fn travel_run() -> Value {
    travel::run_travel_now().await
}

/// POST /api/travel/adopt —— 单账号领养 Buddy（账号卡片的「领养」菜单项）。
#[tauri::command]
pub async fn travel_adopt(account_id: String) -> Result<Value, String> {
    let acc = account::find_account(&account_id).ok_or("账号不存在")?;
    Ok(travel::adopt_for_account(&acc).await)
}

/// GET /api/checkin/config —— 自动签到配置。
#[tauri::command]
pub fn get_auto_checkin_config() -> Value {
    crate::modules::config::load_checkin_config()
}

/// POST /api/checkin/config —— 保存自动签到配置。
#[tauri::command]
pub fn save_auto_checkin_config(config: Value) -> Result<Value, String> {
    crate::modules::config::save_checkin_config(&config).map_err(|e| e.to_string())?;
    Ok(crate::modules::config::load_checkin_config())
}

/// GET /api/checkin/logs —— 签到日志。
#[tauri::command]
pub fn get_checkin_logs() -> Value {
    json!({ "logs": crate::modules::config::load_checkin_logs() })
}

// ---------------------------------------------------------------------------
// 记录保留设置（设置页可调）
//
// 覆盖签到日志、积分快照、任务记录三类本地观察数据。
// 默认 60 天；用户可改，改完立刻对后续清理生效（无需重启）。
// ---------------------------------------------------------------------------

/// GET /api/settings/retention —— 读取保留天数设置。
///
/// 同时返回预设档位，避免前端硬编码选项（两处各写一份容易不一致）。
#[tauri::command]
pub fn get_record_retention() -> Value {
    use crate::modules::config::{
        record_retention_days, RECORD_RETENTION_DEFAULT_DAYS, RECORD_RETENTION_MAX_DAYS,
        RECORD_RETENTION_MIN_DAYS, RECORD_RETENTION_PRESETS,
    };
    json!({
        "days": record_retention_days(),
        "defaultDays": RECORD_RETENTION_DEFAULT_DAYS,
        "minDays": RECORD_RETENTION_MIN_DAYS,
        "maxDays": RECORD_RETENTION_MAX_DAYS,
        "presets": RECORD_RETENTION_PRESETS
            .iter()
            .map(|(d, label)| json!({ "days": d, "label": label }))
            .collect::<Vec<_>>(),
    })
}

/// POST /api/settings/retention —— 保存保留天数。
///
/// 返回归一化后的实际生效值：用户填了越界值时立刻看到真实数字，
/// 而不是「界面显示 0、实际按 60 天清理」。
#[tauri::command]
pub fn save_record_retention(days: i64) -> Result<Value, String> {
    let applied = crate::modules::config::set_record_retention_days(days).map_err(|e| e.to_string())?;
    Ok(json!({ "days": applied }))
}

// ---------------------------------------------------------------------------
// 账号记录（任务 / 积分 / Token 三类事件的统一流水）
// ---------------------------------------------------------------------------

/// GET /api/account-records —— 查询账号记录。
///
/// 参数（全部可选）：
///   - accountId：为空 = 全部账号
///   - from / to：毫秒时间戳区间（含端点）；0 = 不限
///   - kinds：事件类型数组（task / credit / token）；空 = 全部
///   - limit：最多返回多少条；0 = 不限
#[tauri::command]
pub fn get_account_records(
    account_id: Option<String>,
    from: Option<i64>,
    to: Option<i64>,
    kinds: Option<Vec<String>>,
    limit: Option<usize>,
) -> Value {
    crate::modules::account_records::query_records(
        account_id.as_deref().unwrap_or(""),
        from.unwrap_or(0),
        to.unwrap_or(0),
        &kinds.unwrap_or_default(),
        limit.unwrap_or(500),
    )
}

/// POST /api/account-records/backfill —— 把历史签到日志回填为账号记录。
///
/// 前端在首次打开「账号记录」时调用一次；幂等，重复调用不会产生重复记录。
#[tauri::command]
pub fn backfill_account_records() -> Result<Value, String> {
    match crate::modules::account_records::backfill_from_checkin_logs() {
        Ok(added) => Ok(json!({ "added": added })),
        Err(e) => Err(e.to_string()),
    }
}

// ---------------------------------------------------------------------------
// 派猫猫旅行
// ---------------------------------------------------------------------------

/// GET /api/travel/status —— 查询单账号今日旅行状态标签。
#[tauri::command]
pub async fn get_travel_status(account_id: String) -> Result<Value, String> {
    account::find_account(&account_id).ok_or("账号不存在")?;
    travel::reconcile_due_travel(Some(account_id.as_str())).await;
    Ok(travel::travel_display(&account_id))
}

/// GET /api/travel/config —— 自动旅行配置。
#[tauri::command]
pub fn get_auto_travel_config() -> Value {
    crate::modules::config::load_travel_config()
}

/// POST /api/travel/config —— 保存自动旅行配置。开启时立刻跑一轮派发/领取。
#[tauri::command]
pub fn save_auto_travel_config(config: Value) -> Result<Value, String> {
    crate::modules::config::save_travel_config(&config).map_err(|e| e.to_string())?;
    let saved = crate::modules::config::load_travel_config();
    if saved.get("enabled").and_then(Value::as_bool) == Some(true) {
        tauri::async_runtime::spawn(async {
            let _ = travel::run_travel_cycle().await;
            let _ = travel::run_travel_claim_cycle().await;
        });
    }
    Ok(saved)
}

// ---------------------------------------------------------------------------
// 自动轮换（CodeBuddy CLI）
// ---------------------------------------------------------------------------

/// GET /api/rotate/config —— 自动轮换配置。
#[tauri::command]
pub fn get_auto_rotate_config() -> Value {
    crate::modules::config::load_auto_rotate_config()
}

/// POST /api/rotate/config —— 保存自动轮换配置。
#[tauri::command]
pub fn save_auto_rotate_config(config: Value) -> Result<Value, String> {
    crate::modules::config::save_auto_rotate_config(&config).map_err(|e| e.to_string())?;
    Ok(crate::modules::config::load_auto_rotate_config())
}

/// GET /api/rotate/status —— 轮换状态（配置 + 上次检查/切换）。
#[tauri::command]
pub fn rotate_status() -> Value {
    rotate::rotate_status()
}

/// POST /api/rotate/run —— 手动触发一次轮换检查。
#[tauri::command]
pub async fn run_rotate() -> Value {
    rotate::run_rotate_cycle().await
}

/// GET /api/rotate/logs —— 最近轮换日志。
#[tauri::command]
pub fn get_rotate_logs() -> Value {
    json!({ "logs": rotate::rotate_logs() })
}

/// POST /api/refresh-token —— 单账号刷新 token。
#[tauri::command]
pub async fn refresh_account_token(account_id: String) -> Result<Value, String> {
    let acc = account::find_account(&account_id).ok_or("账号不存在")?;
    let fresh = refresh::refresh_account_token(acc).await;
    Ok(account::account_meta(&fresh))
}

// ---------------------------------------------------------------------------
// 阶段 4：自动更新
// ---------------------------------------------------------------------------

/// GET /api/update/config —— 更新源配置（owner/repo/token）。
#[tauri::command]
pub fn get_github_config() -> Value {
    update::load_github_config()
}

/// POST /api/update/config —— 保存更新源配置。
///
/// 前端（src/lib/api.ts::saveGithubConfig）把配置包在 `config` 键里传进来
///（与 webui 的 POST body 同一约定），这里必须剥掉那层壳再交给
/// `update::save_github_config` —— 后者读的是配置本身。不剥的话它会读到一份
/// 没有 owner/repo/proxy/proxy_scope 的壳，把地址写成空串、三个开关回落默认值：
/// 用户点「保存代理」反而把配置清空了，而且返回 Ok 毫无报错。
///
/// 同时容忍**裸配置**（直接传配置对象）：无脑只看 `config` 键会让裸形状被读成
/// 空配置，那是同一个坑的另一面。
#[tauri::command]
pub fn save_github_config(config: Value) -> Result<Value, String> {
    let submitted = match config.get("config") {
        // config 键存在且是个对象 → 用它（真实调用形状）
        Some(inner) if inner.is_object() => inner.clone(),
        // 其余（不存在 / null / 非对象）→ 整个参数就是配置
        _ => config,
    };
    update::save_github_config(&submitted).map_err(|e| e.to_string())?;
    Ok(update::load_github_config())
}

/// GET /api/update/check —— 检查 GitHub Releases 是否有新版本。
/// force=true 时绕过缓存强制刷新（设置页手动检查）。
#[tauri::command]
pub async fn check_update(proxy: Option<String>, force: Option<bool>) -> Value {
    update::update_check(proxy.as_deref(), force.unwrap_or(false)).await
}

/// 启动当前应用的新进程并退出旧进程，用于更新安装完成后的立即重启。
#[tauri::command]
pub fn relaunch_app() -> Result<(), String> {
    let executable = std::env::current_exe().map_err(|e| format!("无法定位应用程序: {e}"))?;
    // 更新重启是普通启动路径；不要把系统自启专用参数带给新进程。
    let args = std::env::args_os().skip(1).filter(|arg| {
        #[cfg(desktop)]
        {
            should_forward_relaunch_arg(arg.as_os_str())
        }
        #[cfg(not(desktop))]
        {
            true
        }
    });
    std::process::Command::new(executable)
        .args(args)
        .spawn()
        .map_err(|e| format!("启动应用失败: {e}"))?;
    std::process::exit(0);
}

// ---------------------------------------------------------------------------
// 开机自启（仅桌面端；webui 不提供同名接口）
// ---------------------------------------------------------------------------

/// GET /api/launch-at-login —— 查询系统当前的开机自启注册状态。
///
/// 以 tauri-plugin-autostart 的 OS 状态为唯一事实来源，不另存本地布尔值。
#[tauri::command]
pub fn get_launch_at_login_enabled(_app: tauri::AppHandle) -> Result<bool, String> {
    #[cfg(desktop)]
    {
        use tauri_plugin_autostart::ManagerExt;
        return _app
            .autolaunch()
            .is_enabled()
            .map_err(|e| format!("查询开机自启状态失败：{e}"));
    }
    #[cfg(not(desktop))]
    {
        Err("当前平台不支持开机自启".to_string())
    }
}

#[cfg(desktop)]
fn should_forward_relaunch_arg(arg: &std::ffi::OsStr) -> bool {
    arg != std::ffi::OsStr::new(crate::tray::SILENT_STARTUP_ARG)
}

#[cfg(all(test, desktop))]
mod relaunch_tests {
    use super::should_forward_relaunch_arg;
    use std::ffi::OsStr;

    #[test]
    fn update_relaunch_drops_only_the_exact_silent_startup_arg() {
        assert!(!should_forward_relaunch_arg(OsStr::new("--hidden")));
        assert!(should_forward_relaunch_arg(OsStr::new("--hidden-x")));
        assert!(should_forward_relaunch_arg(OsStr::new("x--hidden")));
        assert!(should_forward_relaunch_arg(OsStr::new("--debug")));
    }
}

/// POST /api/launch-at-login —— 注册 / 移除系统开机自启，并回读权威状态。
///
/// 回读结果与请求值不一致时按失败处理并返回当前真实状态，避免假装设置成功。
#[tauri::command]
pub fn set_launch_at_login_enabled(_app: tauri::AppHandle, enabled: bool) -> Result<bool, String> {
    #[cfg(desktop)]
    {
        use tauri_plugin_autostart::ManagerExt;
        let autostart = _app.autolaunch();
        let action = if enabled { "开启" } else { "关闭" };
        let result = if enabled {
            autostart.enable()
        } else {
            autostart.disable()
        };
        if let Err(e) = result {
            return Err(format!("{action}开机自启失败：{e}"));
        }
        let authoritative = autostart
            .is_enabled()
            .map_err(|e| format!("开机自启设置后回读状态失败：{e}"))?;
        if authoritative != enabled {
            return Err(format!(
                "{action}开机自启未生效（系统当前状态：{}），请稍后重试",
                if authoritative {
                    "已开启"
                } else {
                    "未开启"
                }
            ));
        }
        Ok(authoritative)
    }
    #[cfg(not(desktop))]
    {
        let _ = enabled;
        Err("当前平台不支持开机自启".to_string())
    }
}

// ---------------------------------------------------------------------------
// 兼容网关（workbuddy2api）—— 桌面 GUI 命令
//
// 与 server 版共用 ai_gateway_core::modules::gateway，因此行为一致：
// 同一套端口检测、账号导出、进程托管逻辑。
// 区别：GUI 直接在进程内调用，不起 HTTP 服务，也不需要浏览器。
// ---------------------------------------------------------------------------

/// 网关运行态 + 账号池详情。
#[tauri::command]
pub async fn get_gateway_status() -> Result<Value, String> {
    Ok(ai_gateway_core::modules::gateway::gateway_status().await)
}

/// 读取网关配置。
#[tauri::command]
pub fn get_gateway_config() -> Result<Value, String> {
    let cfg = ai_gateway_core::modules::gateway::load_gateway_config();
    let exe = ai_gateway_core::modules::gateway::resolve_gateway_exe();
    Ok(json!({
        "config": cfg,
        "exeFound": exe.is_some(),
        "exePath": exe.map(|p| p.to_string_lossy().to_string()),
        "exeSource": ai_gateway_core::modules::gateway::gateway_source(),
        "authDir": ai_gateway_core::modules::gateway::gateway_auth_dir().to_string_lossy(),
    }))
}

/// 保存网关配置（仅覆盖传入字段）。
///
/// 前端以 camelCase 传参（`{ port, apiKey, autoStart }`），故此处声明
/// `rename_all = "camelCase"`；只把出现的字段透传给 core 做浅合并，
/// 未传字段沿用磁盘上的现有值。
///
/// 养号任务排程（activity_hours 等）也走这里：它们最终由 write_native_config
/// 转写进网关的 config.json，与 checkin_enabled 等既有字段同一来源。
#[tauri::command(rename_all = "camelCase")]
#[allow(clippy::too_many_arguments)]
pub fn save_gateway_config(
    port: Option<u16>,
    api_key: Option<String>,
    auto_start: Option<bool>,
    mode: Option<String>,
    pinned_uid: Option<String>,
    manual_uids: Option<Vec<String>>,
    activity_hours: Option<Vec<i64>>,
    nightowl_hours: Option<Vec<i64>>,
    school_hours: Option<Vec<i64>>,
    trial_hours: Option<Vec<i64>>,
    activity_enabled: Option<bool>,
    nightowl_enabled: Option<bool>,
    school_enabled: Option<bool>,
    trial_enabled: Option<bool>,
    activity_report_count: Option<i64>,
    prompt_mode: Option<String>,
    prompt_file: Option<String>,
) -> Result<Value, String> {
    let mut patch = serde_json::Map::new();
    if let Some(p) = port {
        patch.insert("port".to_string(), json!(p));
    }
    if let Some(k) = api_key {
        patch.insert("api_key".to_string(), json!(k));
    }
    if let Some(a) = auto_start {
        patch.insert("auto_start".to_string(), json!(a));
    }
    if let Some(m) = mode {
        patch.insert("mode".to_string(), json!(m));
    }
    if let Some(u) = pinned_uid {
        patch.insert("pinned_uid".to_string(), json!(u));
    }
    // 手动模式勾选的账号列表（多选）
    if let Some(list) = manual_uids {
        let cleaned: Vec<String> = list
            .into_iter()
            .map(|s| s.trim().to_string())
            .filter(|s| !s.is_empty())
            .collect();
        patch.insert("manual_uids".to_string(), json!(cleaned));
    }
    // 养号任务排程：与上面同样「传了才覆盖」，未传则保留磁盘上的现有值。
    for (key, value) in [
        ("activity_hours", activity_hours),
        ("nightowl_hours", nightowl_hours),
        ("school_hours", school_hours),
        ("trial_hours", trial_hours),
    ] {
        if let Some(hours) = value {
            patch.insert(key.to_string(), json!(hours));
        }
    }
    for (key, value) in [
        ("activity_enabled", activity_enabled),
        ("nightowl_enabled", nightowl_enabled),
        ("school_enabled", school_enabled),
        ("trial_enabled", trial_enabled),
    ] {
        if let Some(flag) = value {
            patch.insert(key.to_string(), json!(flag));
        }
    }
    if let Some(n) = activity_report_count {
        patch.insert("activity_report_count".to_string(), json!(n));
    }
    // 自定义系统提示词：同样「传了才覆盖」，未传则保留磁盘上的现有值 ——
    // 旧客户端不传这两个字段，绝不能把它们重置（那会把用户已配好的提示词抹掉）。
    if let Some(m) = prompt_mode {
        patch.insert("prompt_mode".to_string(), json!(m));
    }
    if let Some(f) = prompt_file {
        patch.insert("prompt_file".to_string(), json!(f));
    }
    let v = ai_gateway_core::modules::gateway::save_gateway_config(&Value::Object(patch))?;
    Ok(json!({ "config": v }))
}

/// POST /tasks/run —— 手动触发网关侧一轮养号任务（活跃上报 / 夜猫子 / 开学季 / trial）。
///
/// 手动触发**不**检查「启用」开关：该开关只管后台是否自动排程，用户主动点击就该执行
///（与 `checkin_all` / `travel_run` 的既有语义一致）。
///
/// 返回值里的 `ran=false` + `skip` 是**正常结果**（如夜猫子不在 23:00–08:00 窗口内），
/// 界面应当作说明展示而非报错 —— 否则用户点了「立即执行」看到红色错误会以为坏了。
#[tauri::command]
pub async fn run_gateway_task(task: String) -> Result<Value, String> {
    Ok(ai_gateway_core::modules::gateway::run_task_now(&task).await)
}

/// POST /tasks/growth —— 成长任务「一键完成」（列表 / 单账号执行 / 全账号执行）。
///
/// `action` 取 `list` / `run` / `run-all`：
///   - `list`：列出该账号的成长任务（只读，秒级返回）
///   - `run`：执行该账号的任务；`task_code` 为空 = 跑全部待办
///   - `run-all`：所有账号跑一轮
///
/// **耗时差异很大**：`list` 秒级；`run` 单账号分钟级（可能含真实对话，
/// 会消耗 token 与额度）；`run-all` 更久。界面必须给出「正在执行」的反馈，
/// 否则用户会以为没反应而反复点击 —— 而重复点击会重复消耗。
///
/// 错误一律走返回值的 `error` 字段而不是 Err（与 run_gateway_task 一致）：
/// 让界面能同时拿到失败原因，而不是整条请求变红、拿不到任何上下文。
#[tauri::command]
pub async fn run_growth_task(
    action: String,
    account_id: Option<String>,
    task_code: Option<String>,
) -> Result<Value, String> {
    Ok(ai_gateway_core::modules::gateway::growth_task(
        &action,
        account_id.as_deref().unwrap_or(""),
        task_code.as_deref().unwrap_or(""),
    )
    .await)
}

/// 检测端口是否可用。
#[tauri::command]
pub fn check_gateway_port(port: u16) -> Result<Value, String> {
    if port == 0 {
        return Err("端口号需在 1-65535 之间".to_string());
    }
    Ok(ai_gateway_core::modules::gateway::inspect_port(port))
}

/// 查询占用指定端口的进程（供「结束占用进程」对话框展示）。
///
/// **按需调用**：会 spawn netstat/tasklist/powershell，不要在页面挂载或轮询里调
///（那正是热路径 `check_gateway_port` 刻意不查进程的原因）。
/// 同样用 async + spawn_blocking：进程调用是阻塞的，不该占用主线程或异步运行时线程。
#[tauri::command]
pub async fn get_gateway_port_holder(port: u16) -> Result<Value, String> {
    tauri::async_runtime::spawn_blocking(move || {
        Ok(json!({
            "port": port,
            "holder": ai_gateway_core::modules::gateway::port_holder(port).unwrap_or(Value::Null),
        }))
    })
    .await
    .map_err(|e| format!("执行失败: {e}"))?
}

/// 结束占用指定端口的进程，让本网关可以接管该端口。
///
/// 由前端在用户**明确确认**后调用（提示里会展示占用进程名与 PID）。
/// 后端仍会拒绝几类危险目标（自身进程、本程序启动的网关），
/// 因此前端即便误调用也不会造成「网关被自己杀掉」的状态不一致。
///
/// **必须是 async**：本命令要 spawn 3 个控制台进程查占用者，并轮询等待端口释放
///（最长 4 秒）。同步 Tauri 命令跑在主线程，会把这些开销直接变成界面卡死。
#[tauri::command]
pub async fn kill_gateway_port_holder(port: u16) -> Result<Value, String> {
    // 放到阻塞线程池：内部是 std::process + sleep 轮询，不占异步运行时线程
    tauri::async_runtime::spawn_blocking(move || {
        ai_gateway_core::modules::gateway::kill_port_holder(port)
    })
    .await
    .map_err(|e| format!("执行失败: {e}"))?
}

/// 切换网关工作模式并立即生效（重导出凭证 + 按需重启）。
///
/// 与 `save_gateway_config` 的区别：后者只写配置文件，而网关的账号池是
/// 启动时建立的，因此改完必须手动重启才生效。本命令把「保存 + 重导出 + 重启」
/// 合成一步，让「自动 ↔ 手动」点击即生效。
///
/// `manual_uids` 是手动模式下勾选的账号列表；`pinned_uid` 保留仅为兼容旧前端调用。
#[tauri::command(rename_all = "camelCase")]
pub async fn switch_gateway_mode(
    mode: String,
    manual_uids: Option<Vec<String>>,
    pinned_uid: Option<String>,
) -> Result<Value, String> {
    let mode = ai_gateway_core::modules::gateway::GatewayMode::from_str(&mode);
    // 新字段优先；旧前端只传 pinned_uid 时按「只勾了那一个」处理
    let uids: Vec<String> = match manual_uids {
        Some(list) => list,
        None => pinned_uid.into_iter().collect(),
    };
    let result = ai_gateway_core::modules::gateway::switch_mode(mode, uids).await;
    if result.get("ok").and_then(Value::as_bool) == Some(false) {
        let msg = result
            .get("error")
            .and_then(Value::as_str)
            .unwrap_or("切换模式失败")
            .to_string();
        return Err(msg);
    }
    Ok(result)
}

/// 设置「限制使用的模型」白名单（多选）；空数组 = 清除限制（全部放行）。
///
/// 网关运行时自动重启以生效（模型限制由网关启动时读取，与切换模式同理）。
#[tauri::command]
pub async fn set_allowed_models(models: Vec<String>) -> Result<Value, String> {
    let result = ai_gateway_core::modules::gateway::set_allowed_models(&models).await;
    if result.get("ok").and_then(Value::as_bool) == Some(false) {
        let msg = result
            .get("error")
            .and_then(Value::as_str)
            .unwrap_or("设置模型限制失败")
            .to_string();
        return Err(msg);
    }
    Ok(result)
}

/// 设置单个模型限制；空串 = 清除限制。
///
/// 保留这个**单值**入口是为了向后兼容（旧前端、脚本、已发布版本的自调用）：
/// 它现在等价于 `set_allowed_models` 传一个单元素数组。
#[tauri::command]
pub async fn set_allowed_model(model: String) -> Result<Value, String> {
    let result = ai_gateway_core::modules::gateway::set_allowed_model(&model).await;
    if result.get("ok").and_then(Value::as_bool) == Some(false) {
        let msg = result
            .get("error")
            .and_then(Value::as_str)
            .unwrap_or("设置模型失败")
            .to_string();
        return Err(msg);
    }
    Ok(result)
}

/// 启动网关；传 port 时先保存再启动（前端「选端口 → 启动」一步完成）。
#[tauri::command]
pub async fn start_gateway(port: Option<u16>) -> Result<Value, String> {
    if let Some(p) = port {
        if p == 0 {
            return Err("端口号需在 1-65535 之间".to_string());
        }
        ai_gateway_core::modules::gateway::save_gateway_config(&json!({ "port": p }))?;
    }
    let cfg = ai_gateway_core::modules::gateway::load_gateway_config();
    match ai_gateway_core::modules::gateway::start_gateway(&cfg).await {
        Ok(v) => {
            ai_gateway_core::modules::gateway::update_runtime_state("started", None);
            Ok(v)
        }
        Err(e) => {
            let msg = e.clone();
            ai_gateway_core::modules::gateway::update_runtime_state("failed", Some(e));
            Err(msg)
        }
    }
}

/// 停止网关。
#[tauri::command]
pub fn stop_gateway() -> Result<Value, String> {
    let r = ai_gateway_core::modules::gateway::stop_gateway();
    ai_gateway_core::modules::gateway::update_runtime_state("stopped", None);
    Ok(r)
}

/// 重启网关（应用新配置/新账号）。
#[tauri::command]
pub async fn restart_gateway() -> Result<Value, String> {
    ai_gateway_core::modules::gateway::stop_gateway();
    tokio::time::sleep(std::time::Duration::from_millis(600)).await;
    let cfg = ai_gateway_core::modules::gateway::load_gateway_config();
    ai_gateway_core::modules::gateway::start_gateway(&cfg).await
}

/// 双向同步账号；auto_reload 时按需重启网关。
#[tauri::command(rename_all = "camelCase")]
pub async fn sync_gateway_accounts(auto_reload: Option<bool>) -> Result<Value, String> {
    Ok(ai_gateway_core::modules::gateway::sync_and_reload(auto_reload.unwrap_or(true)).await)
}

// ---------------------------------------------------------------------------
// 一键导入：把本网关接入本机已安装的 AI 客户端
// ---------------------------------------------------------------------------

/// 网关根地址（不带 /v1），供客户端配置使用。
fn gateway_root_base() -> (String, String, u16) {
    let cfg = ai_gateway_core::modules::gateway::load_gateway_config();
    let port = cfg.get("port").and_then(Value::as_u64).unwrap_or(7863) as u16;
    let api_key = cfg
        .get("api_key")
        .and_then(Value::as_str)
        .unwrap_or("")
        .to_string();
    (format!("http://127.0.0.1:{port}"), api_key, port)
}

/// 探测全部目标客户端的安装与配置状态。
#[tauri::command]
pub fn detect_agent_clients() -> Result<Value, String> {
    let (base, api_key, _) = gateway_root_base();
    let targets = ai_gateway_core::modules::agent_import::detect_all(&base, &api_key);
    Ok(json!({
        "base": base,
        "hasApiKey": !api_key.is_empty(),
        "targets": targets.iter().map(|t| json!({
            "id": t.id,
            "label": t.label,
            "installed": t.installed,
            "configured": t.configured,
            "configPath": t.config_path,
            "note": t.note,
            "version": t.version,
        })).collect::<Vec<_>>(),
    }))
}

/// 获取网关模型列表（优先从运行中的网关拉取，失败回退预置列表）。
#[tauri::command]
pub async fn get_gateway_models() -> Result<Value, String> {
    Ok(json!({
        "models": ai_gateway_core::modules::gateway::fetch_models().await,
    }))
}

/// 获取网关累计 Token 用量统计（days 省略 = 全部历史）。
#[tauri::command]
pub async fn get_gateway_usage(days: Option<i64>) -> Result<Value, String> {
    Ok(ai_gateway_core::modules::gateway::fetch_usage(days).await)
}

/// 把网关接入指定客户端（写配置 + 自动备份，支持多模型）。
#[tauri::command(rename_all = "camelCase")]
pub fn import_agent_client(
    target: String,
    model: Option<String>,
    models: Option<Vec<String>>,
) -> Result<Value, String> {
    let (base, api_key, _) = gateway_root_base();
    if api_key.trim().is_empty() {
        return Err("请先在网关设置里填写 API Key：客户端需要凭据才能鉴权".to_string());
    }
    let model_list = match models {
        Some(list) if !list.is_empty() => list,
        _ => match model.filter(|m| !m.trim().is_empty()) {
            Some(m) => vec![m],
            None => vec![default_gateway_model()],
        },
    };

    let outcome = ai_gateway_core::modules::agent_import::import_target(
        &target, &base, &api_key, &model_list,
    )?;
    Ok(json!({
        "ok": true,
        "target": outcome.target,
        "backupDir": outcome.backup_dir,
        "files": outcome.files,
        "models": outcome.models,
    }))
}

/// 批量接入/一键更新多个客户端配置。
#[tauri::command(rename_all = "camelCase")]
pub fn batch_import_agent_clients(
    targets: Option<Vec<String>>,
    models: Option<Vec<String>>,
) -> Result<Value, String> {
    let (base, api_key, _) = gateway_root_base();
    if api_key.trim().is_empty() {
        return Err("请先在网关设置里填写 API Key：客户端需要凭据才能鉴权".to_string());
    }

    let model_list = match models {
        Some(list) if !list.is_empty() => list,
        _ => vec![default_gateway_model()],
    };

    let outcomes = match targets {
        Some(ids) if !ids.is_empty() => {
            ai_gateway_core::modules::agent_import::import_targets(&ids, &base, &api_key, &model_list)?
        }
        _ => {
            ai_gateway_core::modules::agent_import::import_all_installed(&base, &api_key, &model_list)?
        }
    };

    Ok(json!({
        "ok": true,
        "count": outcomes.len(),
        "outcomes": outcomes.iter().map(|o| json!({
            "target": o.target,
            "backupDir": o.backup_dir,
            "files": o.files,
            "models": o.models,
        })).collect::<Vec<_>>(),
        "models": model_list,
    }))
}

/// 回滚某个客户端到导入前的配置。
#[tauri::command(rename_all = "camelCase")]
pub fn restore_agent_client(target: String, backup_id: Option<String>) -> Result<Value, String> {
    let backups = ai_gateway_core::modules::agent_import::list_backups(&target);
    let id = match backup_id {
        Some(id) if !id.trim().is_empty() => id,
        _ => backups
            .first()
            .and_then(|b| b.get("id"))
            .and_then(Value::as_str)
            .ok_or_else(|| format!("没有找到 {target} 的备份记录"))?
            .to_string(),
    };
    let restored = ai_gateway_core::modules::agent_import::restore_backup(&target, &id)?;
    Ok(json!({ "ok": true, "restored": restored, "backupId": id }))
}

/// 列出某个客户端的历史备份。
#[tauri::command]
pub fn list_agent_backups(target: String) -> Result<Value, String> {
    Ok(json!({
        "backups": ai_gateway_core::modules::agent_import::list_backups(&target),
    }))
}

/// 默认模型：优先取网关模型列表的第一项，失败时回退到静态表首项。
fn default_gateway_model() -> String {
    "deepseek-v4-flash".to_string()
}
