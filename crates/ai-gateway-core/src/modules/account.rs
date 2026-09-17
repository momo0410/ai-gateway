//! 账号存储：读取/写入 `~/.wb-switch/accounts.json`，与 Python 版共享数据目录。
//!
//! 对照 server.py `load_accounts` / `save_accounts` / `find_account` /
//! `account_display_name` / `account_meta`。

use serde_json::{json, Value};
use std::collections::HashMap;
use std::path::Path;

use crate::modules::auth_file::{self, CredentialFreshness};
use crate::modules::config::{accounts_file, atomic_write, Region};

fn load_accounts_from_path(path: &Path) -> Vec<Value> {
    if let Ok(text) = std::fs::read_to_string(path) {
        if let Ok(Value::Array(accounts)) = serde_json::from_str::<Value>(&text) {
            return accounts;
        }
    }
    vec![]
}

fn save_accounts_to_path(path: &Path, accounts: &[Value]) -> std::io::Result<()> {
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let content = serde_json::to_string_pretty(accounts).unwrap_or_default();
    atomic_write(path, &content)
}

fn find_account_in(accounts: &[Value], account_id: &str) -> Option<Value> {
    accounts
        .iter()
        .find(|account| {
            account.get("id").and_then(Value::as_str) == Some(account_id)
                || account.get("uid").and_then(Value::as_str) == Some(account_id)
        })
        .cloned()
}

fn delete_account_from_path(path: &Path, account_id: &str) -> Result<(), String> {
    let mut accounts = load_accounts_from_path(path);
    let before = accounts.len();
    accounts.retain(|account| account.get("id").and_then(Value::as_str) != Some(account_id));
    if accounts.len() == before {
        return Err("账号不存在".to_string());
    }
    save_accounts_to_path(path, &accounts).map_err(|error| error.to_string())
}

/// 读取账号库；文件缺失或损坏返回空列表。
///
/// **会顺带按 `id` 收敛重复条目**（见 `dedupe_and_heal`）：历史 bug 让
/// uid/email 都缺失的账号每采集一次就多一条，只修写入路径清不掉已有数据。
pub fn load_accounts() -> Vec<Value> {
    let mut accounts = load_accounts_from_path(&accounts_file());
    if dedupe_by_id(&mut accounts) {
        // 只有真的合并过才落盘 —— 避免每次读取都写文件（那是无谓 IO，
        // 也会让「文件 mtime」失去参考价值）。
        if let Err(error) = save_accounts_to_path(&accounts_file(), &accounts) {
            // 收敛失败不影响本次读取：内存里已经是去重后的结果，
            // 下次读取会再试一次。所以只记日志，不向上抛。
            eprintln!("[账号库] 合并重复条目后写回失败（下次读取会重试）: {error}");
        }
    }
    accounts
}

/// 按 `id` 合并重复条目，返回「是否发生过合并」。
///
/// 合并规则（三条都要，缺一会留下新的坑）：
///
/// 1. **`disabled` 取「任一条为 true 则 true」**：禁用是用户的**显式意图**，
///    不该被一条没有该字段的旧条目无声解除。这正是所有者遇到的
///    「禁用了但显示未禁用、还无法启用」。
/// 2. **其余字段取 `refreshedAt` 较新者**：token 相关字段必须用新的，
///    否则会把刷新结果退回去（本事故里两条恰好差在 token 上）。
/// 3. **`id` / `createdAt` 保留先出现那条**：`id` 相同才有合并；
///    `createdAt` 是「同一次采集」的证据，保留任意一条都一样。
///
/// 只对**有非空 id** 的条目去重：无 id 的条目身份未知（可能是手工导入的
/// 不同账号），按 id 合并对它们无从下手，也不该乱合并。
fn dedupe_by_id(accounts: &mut Vec<Value>) -> bool {
    // 收集：id → 该 id 的全部条目下标
    let mut groups: HashMap<String, Vec<usize>> = HashMap::new();
    for (index, account) in accounts.iter().enumerate() {
        let Some(id) = get_str(account, "id").filter(|id| !id.trim().is_empty()) else {
            continue;
        };
        groups.entry(id).or_default().push(index);
    }

    let mut drop_indexes: Vec<usize> = Vec::new();
    let mut replacements: Vec<(usize, Value)> = Vec::new();

    for indexes in groups.values() {
        if indexes.len() < 2 {
            continue;
        }
        let keep_index = indexes[0];

        // 2) 其余字段取 refreshedAt 较新者：以最新那条为基底
        let newest_index = indexes
            .iter()
            .copied()
            .max_by_key(|&i| {
                accounts[i]
                    .get("refreshedAt")
                    .and_then(Value::as_i64)
                    .unwrap_or(0)
            })
            .unwrap_or(keep_index);
        let mut merged = accounts[newest_index].clone();

        // 3) id / createdAt 保留先出现那条
        if let Some(id_value) = accounts[keep_index].get("id").cloned() {
            merged["id"] = id_value;
        }
        if let Some(created_at) = accounts[keep_index].get("createdAt").cloned() {
            merged["createdAt"] = created_at;
        }

        // 1) disabled：任一条为 true 就 true
        let any_disabled = indexes.iter().any(|&i| {
            accounts[i]
                .get("disabled")
                .and_then(Value::as_bool)
                .unwrap_or(false)
        });
        if any_disabled {
            merged["disabled"] = json!(true);
        }
        // 备注同样不该丢：取第一条非空的（用户手写的标签，比 token 更该保）
        if get_str(&merged, "note").is_none() {
            if let Some(note) = indexes
                .iter()
                .filter_map(|&i| get_str(&accounts[i], "note"))
                .next()
            {
                merged["note"] = json!(note);
            }
        }

        replacements.push((keep_index, merged));
        for &i in indexes.iter().skip(1) {
            drop_indexes.push(i);
        }
    }

    if drop_indexes.is_empty() {
        return false;
    }

    for (index, value) in replacements {
        accounts[index] = value;
    }
    // 从后往前删，避免下标位移
    drop_indexes.sort_unstable_by(|a, b| b.cmp(a));
    for index in drop_indexes {
        accounts.remove(index);
    }
    true
}

/// 写回账号库（原子写），保持原 JSON 数组结构。
pub fn save_accounts(accounts: &[Value]) -> std::io::Result<()> {
    save_accounts_to_path(&accounts_file(), accounts)
}

/// 按 id 或 uid 查找账号。
pub fn find_account(account_id: &str) -> Option<Value> {
    find_account_in(&load_accounts(), account_id)
}

/// 账号展示名（email → nickname → uid → unknown）。
pub fn account_display_name(acc: &Value) -> String {
    get_str(acc, "email")
        .or_else(|| get_str(acc, "nickname"))
        .or_else(|| get_str(acc, "uid"))
        .unwrap_or_else(|| "unknown".to_string())
}

/// 账号备注：用户自己写的标签（如「公司号」「备用」「张三的号」）。
///
/// 为什么需要：授权进来的账号往往只带邮箱/手机号/随机 uid，光看这些认不出
/// 「这是谁的号、干什么用的」。备注由用户定义、只存本地账号库，不参与登录。
pub fn account_note(acc: &Value) -> String {
    get_str(acc, "note").unwrap_or_default()
}

/// 设置账号备注并落盘；返回更新后的账号。
///
/// 传空串 = 清除备注。备注是纯展示信息，不触碰任何凭证字段。
pub fn set_account_note(account_id: &str, note: &str) -> Result<Value, String> {
    let mut accounts = load_accounts();
    let trimmed = note.trim();
    let acc = accounts
        .iter_mut()
        .find(|a| get_str(a, "id").as_deref() == Some(account_id))
        .ok_or_else(|| "账号不存在".to_string())?;
    if trimmed.is_empty() {
        // 清除：删字段而不是写空串，保持账号库干净（也避免导出时残留）。
        if let Some(obj) = acc.as_object_mut() {
            obj.remove("note");
        }
    } else {
        if let Some(obj) = acc.as_object_mut() {
            obj.insert("note".to_string(), json!(trimmed));
        }
    }
    let updated = acc.clone();
    save_accounts(&accounts).map_err(|e| e.to_string())?;
    Ok(updated)
}

/// 账号是否被用户**手动禁用**。
///
/// 语义（与所有者确认）：禁用 = **不进网关账号池**，但其它自动任务照跑。
/// 之所以不去掉签到/领养等任务：那些是「养号」动作，号暂时不接流量
/// 不等于不要额度与连登天数；把两者绑死会让用户失去「先养着，以后再启用」
/// 这个最常用的用法。
///
/// 与「自动禁用」的区别：网关内部还有因冷却/熔断/余额耗尽而临时不可用的账号
///（见 pool 的 CoolKind），那是**运行时自动判定**、会自行恢复；
/// 本字段是**用户意图**、只能由用户显式改回。
pub fn account_disabled(acc: &Value) -> bool {
    acc.get("disabled").and_then(Value::as_bool) == Some(true)
}

/// 设置账号的禁用状态并落盘；返回更新后的账号。
///
/// 传 `false` 时**删除字段**而不是写 `false`：与备注同一做法，
/// 保持账号库干净，也让「字段存在与否」可被用作调试线索。
pub fn set_account_disabled(account_id: &str, disabled: bool) -> Result<Value, String> {
    let mut accounts = load_accounts();
    let acc = accounts
        .iter_mut()
        .find(|a| get_str(a, "id").as_deref() == Some(account_id))
        .ok_or_else(|| "账号不存在".to_string())?;
    if let Some(obj) = acc.as_object_mut() {
        if disabled {
            obj.insert("disabled".to_string(), json!(true));
        } else {
            obj.remove("disabled");
        }
    }
    let updated = acc.clone();
    save_accounts(&accounts).map_err(|e| e.to_string())?;
    Ok(updated)
}

/// 账号的展示元数据（不泄露 token）。对照 server.py `account_meta`。
pub fn account_meta(acc: &Value) -> Value {
    // 区域由 domain 后缀推导（国服 .cn / 国际版 .ai），供界面区分展示。
    let region = crate::modules::config::Region::of(acc);
    json!({
        "region": region.label(),
        "regionKey": match region {
            crate::modules::config::Region::Cn => "cn",
            crate::modules::config::Region::Intl => "intl",
        },
        "id": acc.get("id"),
        "uid": acc.get("uid"),
        "email": acc.get("email"),
        "nickname": acc.get("nickname"),
        "enterpriseName": acc.get("enterpriseName"),
        "expiresAt": acc.get("expiresAt"),
        "refreshExpiresAt": acc.get("refreshExpiresAt"),
        "refreshedAt": acc.get("refreshedAt"),
        "createdAt": acc.get("createdAt"),
        // 需重登判定必须复用 refresh::needs_relogin 这一处口径，不能直接读原始标记：
        // 旧版本把传输层失败（code=-1，网络抖动/代理闪断）也写成 needs_relogin，
        // 直接透传会让界面把健康账号显示成「需重新登录」—— 实测一批账号的
        // refresh token 完全正常，是历史误报的受害者。
        // 该函数的注释明确写了「界面展示都复用它」，此处即其中之一。
        "needsRelogin": crate::modules::refresh::needs_relogin(acc),
        // 原始标记仍单独透出：排查时要能区分「库里记了什么」（事实）
        // 与「判定结论是什么」（结论），二者不一致正是误报的证据。
        "needsReloginRaw": acc.get("needs_relogin").and_then(|v| v.as_bool()) == Some(true),
        "needsReloginReason": acc.get("needs_relogin_reason"),
        // 备注：用户自定义标签，用于认出「这是谁的号」。
        "note": acc.get("note"),
        // 用户手动禁用：不进网关账号池（其它自动任务照跑）。
        "disabled": account_disabled(acc),
        // 原始域名（如 www.workbuddy.ai / copilot.tencent.com）：
        // 区域标签只给「国服/国际版」，排查问题时常需要看确切域名。
        "domain": acc.get("domain"),
        // 手机号（国服账号的真实身份线索，邮箱常为空）。
        "phoneNumber": acc
            .get("profile_raw")
            .and_then(|p| p.get("phoneNumber"))
            .cloned()
            .unwrap_or(Value::Null),
        // 账号类型（personal / enterprise）：影响可用模型与额度口径。
        "accountType": acc
            .get("profile_raw")
            .and_then(|p| p.get("type"))
            .cloned()
            .unwrap_or(Value::Null),
    })
}

/// 取非空字符串字段；空/缺失返回 None。
pub fn get_str(v: &Value, key: &str) -> Option<String> {
    v.get(key)
        .and_then(|v| v.as_str())
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
}

/// 返回可用于 UID 缺失场景的真实邮箱。历史展示占位值不参与身份匹配。
fn identity_email(account: &Value) -> Option<String> {
    let email = get_str(account, "email")?;
    if !email.contains('@')
        || email.eq_ignore_ascii_case("unknown")
        || email == "手动添加"
        || get_str(account, "nickname").as_deref() == Some(email.as_str())
        || get_str(account, "uid").as_deref() == Some(email.as_str())
    {
        return None;
    }
    Some(email.to_ascii_lowercase())
}

/// 两条记录是否属于同一服务区域。
///
/// 国服与国际版的 uid / 邮箱是**相互独立的命名空间**，同一串 uid 或同一个邮箱
/// 在两个区域可以同时存在（实测本机国服与国际版各自登录、uid 互不相干）。
/// 因此身份匹配必须带上区域，否则新采集的国际版账号会直接顶掉同 uid 的国服账号。
/// domain 缺失按国服处理，与 `Region::from_domain` 的历史默认一致。
fn same_region(a: &Value, b: &Value) -> bool {
    crate::modules::config::Region::of(a) == crate::modules::config::Region::of(b)
}

/// 按稳定身份将采集结果合并到账号列表，并返回最终持久化的账号。
///
/// 非空 UID 始终优先；仅当新账号没有 UID 时，才使用真实邮箱兜底。
/// 命中已有身份时保留本地 id，避免调用方持有的账号引用失效。
/// 两个区域各自独立匹配，跨区域永不合并。
///
/// **`id` 也参与匹配（必须放在最前）**：`id` 是账号库主键，界面、备注、
/// 禁用、`processedIds` 比对全部按它走。此前这里只看 `uid` / `email`，
/// 于是 **uid 与 email 都缺失**的账号（国际版 OAuth、企业账号常无邮箱）
/// 永远匹配不上 → 每次采集/登录都 `push` 一条 → 账号库里长出重复条目。
///
/// 实测事故（所有者反馈「禁用了但状态不显示，且无法启用」）：同一个号存了两条，
/// 一条 `disabled: true`、另一条没有该字段。`set_account_disabled` 内部用
/// `.find()` 只改**第一条**，而界面渲染**两条**、用户点的恰好是第二条 ⇒
/// 显示「未禁用」、菜单永远只给「禁用」、而网关导出时第一条被过滤 ⇒ 池里也没有它。
/// 根因就是这里的身份口径与 `upsert_account` 不一致（后者有 id 匹配）。
pub fn upsert_collected_account(accounts: &mut Vec<Value>, mut collected: Value) -> Value {
    let collected_id = get_str(&collected, "id");
    let collected_uid = get_str(&collected, "uid");
    let collected_email = identity_email(&collected);
    let matches_identity = |existing: &Value| {
        // 0) id 精确匹配 —— 最准，且不受 uid/email 缺失影响。
        //    放在区域判断**之前**：id 是全库唯一的，同 id 必然同账号，
        //    不该因区域字段缺失（老数据）而漏配。
        if let Some(id) = collected_id.as_deref() {
            if get_str(existing, "id").as_deref() == Some(id) {
                return true;
            }
        }
        if !same_region(existing, &collected) {
            return false;
        }
        if let Some(uid) = collected_uid.as_deref() {
            return get_str(existing, "uid").as_deref() == Some(uid);
        }
        collected_email
            .as_deref()
            .is_some_and(|email| identity_email(existing).as_deref() == Some(email))
    };

    let matching_indexes: Vec<usize> = accounts
        .iter()
        .enumerate()
        .filter_map(|(index, existing)| matches_identity(existing).then_some(index))
        .collect();

    if let Some(&first_index) = matching_indexes.first() {
        let existing = &accounts[first_index];
        if let Some(existing_id) = existing.get("id").cloned() {
            collected["id"] = existing_id;
        }
        if get_str(&collected, "uid").is_none() {
            if let Some(existing_uid) = existing.get("uid").cloned() {
                collected["uid"] = existing_uid;
            }
        }
        if let Some(created_at) = existing.get("createdAt").cloned() {
            collected["createdAt"] = created_at;
        }

        for index in matching_indexes.into_iter().rev() {
            accounts.remove(index);
        }
        accounts.insert(first_index.min(accounts.len()), collected.clone());
    } else {
        accounts.push(collected.clone());
    }

    collected
}

/// 使用统一身份规则保存采集到的账号。
pub fn save_collected_account(collected: Value) -> std::io::Result<Value> {
    let mut accounts = load_accounts();
    let saved = upsert_collected_account(&mut accounts, collected);
    save_accounts(&accounts)?;
    Ok(saved)
}

/// 按 id 覆盖写入账号库（不存在则追加）。对照 server.py `_upsert_account`。
///
/// **匹配不能只看 `id`**：账号库里可能存在只有 `uid` / `email` 而没有 `id` 的条目
/// （手工导入、旧版本遗留、测试数据都会这样）。只按 `id` 匹配时这些条目永远匹配
/// 不上，于是每次刷新 token 都会**再追加一条** —— 账号列表随刷新次数不断长出
/// 重复项（实测：一个只有 `{email, uid}` 的条目在应用启动后被复制成两条）。
///
/// 因此匹配顺序为：`id` → `uid`（同区域）→ `email`（同区域）。
/// 区域必须参与判断：国服与国际版的 uid / 邮箱是相互独立的命名空间，
/// 同一串 uid 在两个区域可以同时存在，跨区域匹配会互相覆盖。
pub fn upsert_account(updated: &Value) -> std::io::Result<()> {
    let mut accounts = load_accounts();
    let id = updated.get("id").and_then(|v| v.as_str()).unwrap_or("");
    let uid = get_str(updated, "uid");
    let email = identity_email(updated);

    let matches = |a: &Value| -> bool {
        // 1) id 精确匹配（最准，且不受区域字段缺失影响）
        if !id.is_empty() && a.get("id").and_then(|v| v.as_str()) == Some(id) {
            return true;
        }
        // 2) 区域必须一致，否则跨区域会互相覆盖
        if !same_region(a, updated) {
            return false;
        }
        // 3) uid 匹配
        if let Some(uid) = uid.as_deref() {
            if get_str(a, "uid").as_deref() == Some(uid) {
                return true;
            }
        }
        // 4) 真实邮箱兜底（仅当双方都有可用邮箱）
        match (email.as_deref(), identity_email(a)) {
            (Some(left), Some(right)) => left == right,
            _ => false,
        }
    };

    if let Some(existing) = accounts.iter_mut().find(|a| matches(a)) {
        // 保留原有 id（与 `upsert_collected_account` 同款约定）：调用方可能仍持有
        // 基于旧 id 的引用，换掉会让界面上的选中态、备注编辑目标等全部失效。
        // 用 `insert` 而非 `or_insert`：传入值带的 id 与库里不一致时，以库里为准。
        let mut merged = updated.clone();
        if let (Some(obj), Some(prev_id)) = (merged.as_object_mut(), existing.get("id").cloned()) {
            obj.insert("id".to_string(), prev_id);
        }
        *existing = merged;
    } else {
        accounts.push(updated.clone());
    }
    save_accounts(&accounts)
}

/// 构造与官方对齐的请求头。对照 server.py `build_auth_headers`。
pub fn build_auth_headers(account: &Value) -> HashMap<String, String> {
    let mut headers = HashMap::new();
    headers.insert(
        "Authorization".to_string(),
        format!(
            "Bearer {}",
            get_str(account, "access_token").unwrap_or_default()
        ),
    );
    headers.insert("Accept".to_string(), "application/json".to_string());
    headers.insert("Content-Type".to_string(), "application/json".to_string());
    if let Some(uid) = get_str(account, "uid") {
        headers.insert("X-User-Id".to_string(), uid);
    }
    if let Some(eid) =
        get_str(account, "enterpriseId").or_else(|| get_str(account, "enterprise_id"))
    {
        headers.insert("X-Enterprise-Id".to_string(), eid.clone());
        headers.insert("X-Tenant-Id".to_string(), eid);
    }
    if let Some(domain) = get_str(account, "domain") {
        headers.insert("X-Domain".to_string(), domain);
    }
    headers
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::modules::config::now_ms;
    use serde_json::json;

    #[test]
    fn account_meta_strips_tokens() {
        let acc = json!({
            "id": "a1",
            "uid": "u1",
            "email": "x@y.z",
            "nickname": "小明",
            "enterpriseName": "某公司",
            "access_token": "SECRET_ACCESS",
            "refresh_token": "SECRET_REFRESH",
            "expiresAt": 123456,
            "needs_relogin": true,
            "needs_relogin_reason": "刷新失败",
        });
        let meta = account_meta(&acc);
        assert_eq!(meta["id"], "a1");
        assert_eq!(meta["needsRelogin"], true);
        assert_eq!(meta["needsReloginReason"], "刷新失败");
        assert!(meta.get("access_token").is_none(), "不得泄露 token");
        assert!(meta.get("refresh_token").is_none(), "不得泄露 token");
    }

    /// 账号详情所需字段必须透出（供「查看账号详情」弹窗回答「这是谁的号」）。
    ///
    /// 此前 account_meta 只有昵称/uid/邮箱，而实际能辨认账号的线索还包括
    /// 手机号（国服账号邮箱常为空）、原始域名、账号类型 —— 都不在返回值里。
    #[test]
    fn account_meta_exposes_identity_fields() {
        let acc = json!({
            "id": "a1",
            "uid": "u1",
            "nickname": "小明",
            "email": "",
            "domain": "copilot.tencent.com",
            "note": "公司号",
            "profile_raw": {"phoneNumber": "13800138000", "type": "personal"},
            "access_token": "SECRET",
        });
        let meta = account_meta(&acc);
        assert_eq!(meta["note"], "公司号");
        assert_eq!(meta["domain"], "copilot.tencent.com");
        assert_eq!(meta["phoneNumber"], "13800138000", "手机号是国服账号的主要身份线索");
        assert_eq!(meta["accountType"], "personal");
        assert!(meta.get("access_token").is_none(), "新增字段不得带出 token");
    }

    /// 回归：`account_meta` 的 `needsRelogin` 必须复用 `refresh::needs_relogin` 的口径，
    /// 不能直接透传原始标记。
    ///
    /// 背景：旧版本把传输层失败（code=-1，网络抖动/代理闪断）也写成 `needs_relogin`。
    /// 直接透传会让界面把**健康账号**显示成「需重新登录」，用户以为要重新登录，
    /// 实际上 refresh token 完全正常。`refresh::needs_relogin` 的注释明确写了
    /// 「界面展示都复用它」，这条测试就是守住这句话。
    #[test]
    fn account_meta_treats_transport_error_relogin_as_healthy() {
        // 历史误报：原因里含网络错误关键字 → 应判为「不需重登」
        let false_alarm = json!({
            "id": "a1",
            "uid": "u1",
            "needs_relogin": true,
            "needs_relogin_reason": "刷新失败(code=-1): error sending request for url (https://www.workbuddy.ai/...)",
        });
        let meta = account_meta(&false_alarm);
        assert_eq!(
            meta["needsRelogin"], false,
            "传输层失败属历史误报，不得让界面显示「需重新登录」"
        );
        assert_eq!(
            meta["needsReloginRaw"], true,
            "原始标记仍要透出：排查时要能区分「库里记了什么」与「判定结论」"
        );

        // 真实失效：服务端明确拒绝 → 应判为「需重登」
        let real = json!({
            "id": "a2",
            "uid": "u2",
            "needs_relogin": true,
            "needs_relogin_reason": "刷新失败(code=12153): Offline user session not found",
        });
        assert_eq!(
            account_meta(&real)["needsRelogin"], true,
            "服务端明确拒绝才是真的需重登"
        );
    }

    /// 字段缺失时不应 panic，也不应伪造值（界面渲染成「—」）。
    #[test]
    fn account_meta_tolerates_missing_optional_fields() {
        let meta = account_meta(&json!({"id": "a1", "uid": "u1"}));
        assert!(meta["note"].is_null(), "无备注应为 null，而非空串");
        assert!(meta["domain"].is_null());
        assert!(meta["phoneNumber"].is_null(), "无 profile_raw 时不应 panic");
        assert!(meta["accountType"].is_null());
    }

    /// 备注读取：有则取值，无则空串（区别于 account_meta 的 null 语义）。
    #[test]
    fn account_note_reads_and_defaults_empty() {
        assert_eq!(account_note(&json!({"note": "备用"})), "备用");
        assert_eq!(account_note(&json!({"note": "  备用  "})), "备用", "应去掉首尾空白");
        assert_eq!(account_note(&json!({})), "");
        assert_eq!(account_note(&json!({"note": ""})), "");
        assert_eq!(account_note(&json!({"note": "   "})), "", "纯空白视为无备注");
    }

    #[test]
    fn account_display_name_priority() {
        assert_eq!(
            account_display_name(&json!({"email": "a@b.c", "nickname": "n"})),
            "a@b.c"
        );
        assert_eq!(
            account_display_name(&json!({"nickname": "n", "uid": "u"})),
            "n"
        );
        assert_eq!(account_display_name(&json!({"uid": "u"})), "u");
        assert_eq!(account_display_name(&json!({})), "unknown");
    }

    #[test]
    fn get_str_trims_and_filters_empty() {
        assert_eq!(get_str(&json!({"k": "  v  "}), "k"), Some("v".to_string()));
        assert_eq!(get_str(&json!({"k": "  "}), "k"), None);
        assert_eq!(get_str(&json!({"k": 123}), "k"), None);
    }

    fn account(id: &str, uid: Option<&str>, nickname: &str, email: Option<&str>) -> Value {
        json!({
            "id": id,
            "uid": uid,
            "nickname": nickname,
            "email": email,
            "access_token": format!("token-{id}"),
            "createdAt": 1,
        })
    }

    /// 带区域的账号记录；uid 在两个区域**可以相同**（实测国际版与国服
    /// 各有一套独立 uid 空间，但契约上不保证互不相同）。
    fn regional_account(id: &str, uid: &str, domain: &str) -> Value {
        json!({
            "id": id,
            "uid": uid,
            "domain": domain,
            "nickname": id,
            "access_token": format!("token-{id}"),
            "createdAt": 1,
        })
    }

    #[test]
    fn same_uid_in_different_regions_is_retained() {
        let mut accounts = vec![regional_account("cn", "shared-uid", "www.workbuddy.cn")];
        let saved = upsert_collected_account(
            &mut accounts,
            regional_account("intl", "shared-uid", "www.workbuddy.ai"),
        );

        assert_eq!(accounts.len(), 2, "跨区域同 uid 不得互相覆盖");
        assert_eq!(saved["id"], "intl");
        assert_eq!(
            accounts
                .iter()
                .find(|a| a["id"] == "cn")
                .map(|a| a["domain"].clone()),
            Some(json!("www.workbuddy.cn"))
        );
    }

    #[test]
    fn same_uid_same_region_still_refreshes_in_place() {
        let mut accounts = vec![regional_account("stable", "uid-1", "www.workbuddy.ai")];
        let saved = upsert_collected_account(
            &mut accounts,
            regional_account("generated", "uid-1", "www.workbuddy.ai"),
        );

        assert_eq!(accounts.len(), 1, "同区域同 uid 仍应原地刷新");
        assert_eq!(saved["id"], "stable");
    }

    #[test]
    fn region_identity_ignores_domain_case_and_missing_domain_is_cn() {
        let mut accounts = vec![regional_account("upper", "uid-1", "WWW.WorkBuddy.AI")];
        upsert_collected_account(
            &mut accounts,
            regional_account("lower", "uid-1", "www.workbuddy.ai"),
        );
        assert_eq!(accounts.len(), 1, "domain 比较应忽略大小写");

        // domain 缺失按国服处理：老记录（无 domain）与新采到的国服账号应合并
        let mut legacy = vec![account("legacy", Some("uid-2"), "旧", None)];
        upsert_collected_account(
            &mut legacy,
            regional_account("cn-new", "uid-2", "www.workbuddy.cn"),
        );
        assert_eq!(legacy.len(), 1, "缺 domain 的历史记录按国服合并");
    }

    #[test]
    fn email_fallback_also_respects_region() {
        let mut accounts = vec![json!({
            "id": "cn", "uid": null, "domain": "www.workbuddy.cn",
            "email": "shared@example.com", "access_token": "t",
        })];
        upsert_collected_account(
            &mut accounts,
            json!({
                "id": "intl", "uid": null, "domain": "www.workbuddy.ai",
                "email": "shared@example.com", "access_token": "t2",
            }),
        );
        assert_eq!(accounts.len(), 2, "跨区域同邮箱不得合并");
    }

    #[test]
    fn same_nickname_with_different_uids_is_retained() {
        let mut accounts = vec![account("old", Some("uid-1"), "同名", Some("同名"))];
        let saved =
            upsert_collected_account(&mut accounts, account("new", Some("uid-2"), "同名", None));

        assert_eq!(accounts.len(), 2);
        assert_eq!(saved["id"], "new");
    }

    #[test]
    fn same_uid_refresh_preserves_local_id_and_removes_duplicates() {
        let mut accounts = vec![
            account("stable", Some("uid-1"), "旧名称", Some("old@example.com")),
            account("duplicate", Some("uid-1"), "重复记录", None),
        ];
        let saved = upsert_collected_account(
            &mut accounts,
            account("generated", Some("uid-1"), "新名称", None),
        );

        assert_eq!(accounts.len(), 1);
        assert_eq!(saved["id"], "stable");
        assert_eq!(saved["nickname"], "新名称");
        assert_eq!(saved["access_token"], "token-generated");
    }

    #[test]
    fn different_uids_with_same_real_email_are_retained() {
        let mut accounts = vec![account(
            "old",
            Some("uid-1"),
            "账号一",
            Some("shared@example.com"),
        )];
        upsert_collected_account(
            &mut accounts,
            account("new", Some("uid-2"), "账号二", Some("shared@example.com")),
        );

        assert_eq!(accounts.len(), 2);
    }

    #[test]
    fn real_email_is_fallback_only_when_collected_uid_is_missing() {
        let mut accounts = vec![account("stable", None, "旧名称", Some("user@example.com"))];
        let saved = upsert_collected_account(
            &mut accounts,
            account("generated", None, "新名称", Some("USER@example.com")),
        );

        assert_eq!(accounts.len(), 1);
        assert_eq!(saved["id"], "stable");
        assert_eq!(saved["nickname"], "新名称");
    }

    #[test]
    fn legacy_synthetic_email_does_not_merge_accounts() {
        let mut accounts = vec![account("old", None, "同名", Some("同名"))];
        upsert_collected_account(&mut accounts, account("new", None, "同名", Some("同名")));

        assert_eq!(accounts.len(), 2);
    }

    #[test]
    fn persisted_same_name_accounts_can_be_found_and_deleted_independently() {
        let test_dir = std::env::temp_dir().join(format!(
            "ai-gateway-same-name-{}",
            uuid::Uuid::new_v4().simple()
        ));
        let path = test_dir.join("accounts.json");
        let mut accounts = vec![];
        upsert_collected_account(
            &mut accounts,
            account("account-1", Some("uid-1"), "同名用户", None),
        );
        upsert_collected_account(
            &mut accounts,
            account("account-2", Some("uid-2"), "同名用户", None),
        );
        save_accounts_to_path(&path, &accounts).expect("same-name accounts should persist");

        let persisted = load_accounts_from_path(&path);
        assert_eq!(
            find_account_in(&persisted, "account-1").unwrap()["uid"],
            "uid-1"
        );
        assert_eq!(
            find_account_in(&persisted, "account-2").unwrap()["uid"],
            "uid-2"
        );

        delete_account_from_path(&path, "account-1").expect("first account should delete");
        let after_first_delete = load_accounts_from_path(&path);
        assert!(find_account_in(&after_first_delete, "account-1").is_none());
        assert_eq!(
            find_account_in(&after_first_delete, "account-2").unwrap()["uid"],
            "uid-2"
        );

        delete_account_from_path(&path, "account-2").expect("second account should delete");
        assert!(load_accounts_from_path(&path).is_empty());
        std::fs::remove_dir_all(&test_dir).expect("temporary account store should clean up");
    }

    // ---- 本机历史账号发现与导入 ----

    /// **回归测试**：只有 uid 没有 id 的账号不得被反复追加。
    ///
    /// 实测事故：账号库里存在一条 `{email, uid}`（无 `id`）的条目，`upsert_account`
    /// 只按 `id` 匹配，于是每次刷新 token 都追加一条新的 —— 应用启动一次，
    /// 账号列表就多出一个重复项。
    #[test]
    fn upsert_account_对无_id_的账号按_uid_去重() {
        let dir = crate::modules::config::test_isolation::Isolated::new("upsert-no-id");
        let _ = dir;

        // 账号库里先有一条只有 uid + email 的条目（无 id）
        let mut accounts = vec![json!({"uid": "legacy-user", "email": "old@example.com"})];
        save_accounts(&accounts).expect("seed");

        // 刷新流程回写同一个账号（带上了 id）
        upsert_account(&json!({
            "uid": "legacy-user",
            "email": "old@example.com",
            "id": "generated-id",
            "needs_relogin": true,
        }))
        .expect("upsert");

        accounts = load_accounts();
        assert_eq!(
            accounts.len(),
            1,
            "同 uid 的账号不得被重复追加，实际 {} 条",
            accounts.len()
        );
        assert_eq!(accounts[0]["needs_relogin"], true, "刷新结果应被写入");
        assert_eq!(
            accounts[0]["id"], "generated-id",
            "原有条目没有 id，应补上新 id"
        );
    }

    /// 已有 id 的账号按 id 匹配，且**保留原有 id**（调用方可能仍持有旧引用）。
    #[test]
    fn upsert_account_保留原有_id() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("upsert-keep-id");
        save_accounts(&[json!({"id": "keep-me", "uid": "u1", "email": "a@b.c"})]).expect("seed");

        upsert_account(&json!({
            "id": "a-different-id",
            "uid": "u1",
            "email": "a@b.c",
            "nickname": "改过的名字",
        }))
        .expect("upsert");

        let accounts = load_accounts();
        assert_eq!(accounts.len(), 1);
        assert_eq!(accounts[0]["id"], "keep-me", "原有 id 必须保留");
        assert_eq!(accounts[0]["nickname"], "改过的名字");
    }

    /// 跨区域的同 uid 不得互相覆盖（两区域身份命名空间独立）。
    #[test]
    fn upsert_account_跨区域不互相覆盖() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("upsert-region");
        save_accounts(&[json!({
            "uid": "same-uid",
            "domain": "www.workbuddy.cn",
            "nickname": "国服号",
        })])
        .expect("seed");

        upsert_account(&json!({
            "uid": "same-uid",
            "domain": "www.workbuddy.ai",
            "nickname": "国际版号",
        }))
        .expect("upsert");

        let accounts = load_accounts();
        assert_eq!(accounts.len(), 2, "两区域身份独立，应各留一条");
        assert!(accounts.iter().any(|a| a["nickname"] == "国服号"));
        assert!(accounts.iter().any(|a| a["nickname"] == "国际版号"));
    }

    /// 同区域同 uid 出现多份文件时，只留凭证最新的一份。
    #[test]
    fn scan_prefers_freshest_credential_per_identity() {
        use crate::modules::auth_file::{discover_local_accounts_in, CredentialFreshness};

        let auth = temp_scan_dir("scan-auth");
        let backups = temp_scan_dir("scan-backup");
        let now = now_ms();
        let fresh = now + 30 * 24 * 3600 * 1000;
        let stale = now - 3600 * 1000;

        let payload = |access: &str, refresh: &str, at_exp: i64, rt_exp: i64| {
            json!({
                "account": {"uid": "uid-1", "nickname": "同一人"},
                "auth": {"accessToken": access, "refreshToken": refresh,
                         "expiresAt": at_exp, "refreshExpiresAt": rt_exp},
            })
            .to_string()
        };
        std::fs::write(
            auth.join("workbuddy-desktop.2026-09-01T00-00-00Z.1.a.info"),
            payload("AT-OLD", "RT-OLD", stale, stale),
        )
        .unwrap();
        std::fs::write(
            auth.join("workbuddy-desktop.2026-09-10T00-00-00Z.1.b.info"),
            payload("AT-NEW", "RT-NEW", fresh, fresh),
        )
        .unwrap();

        let found = discover_local_accounts_in(&auth, &backups);
        assert_eq!(found.len(), 1, "同区域同 uid 只保留一份");
        assert_eq!(
            get_str(&found[0].account, "access_token").as_deref(),
            Some("AT-NEW"),
            "必须保留凭证最新的那份，否则会导入已轮换失效的 refresh token"
        );
        assert_eq!(found[0].freshness, CredentialFreshness::Refreshable);
        assert_eq!(found[0].duplicate_count, 2);

        std::fs::remove_dir_all(&auth).ok();
        std::fs::remove_dir_all(&backups).ok();
    }

    /// 导入同一候选两次必须幂等：第二次原地刷新，不产生重复账号。
    #[test]
    fn reimporting_same_candidate_is_idempotent() {
        use crate::modules::auth_file::discover_local_accounts_in;

        let auth = temp_scan_dir("import-auth");
        let backups = temp_scan_dir("import-backup");
        let now = now_ms();
        let fresh = now + 30 * 24 * 3600 * 1000;

        std::fs::write(
            auth.join("workbuddy-desktop.2026-09-01T00-00-00Z.1.a.info"),
            json!({
                "account": {"uid": "uid-new", "nickname": "新账号"},
                "auth": {"accessToken": "AT-NEW", "refreshToken": "RT-NEW",
                         "expiresAt": fresh, "refreshExpiresAt": fresh},
            })
            .to_string(),
        )
        .unwrap();

        let found = discover_local_accounts_in(&auth, &backups);
        assert_eq!(found.len(), 1, "扫描应先看到 1 个候选");

        // 用账号库的同一套合并规则验证语义（不触碰真实账号库文件）。
        let mut store: Vec<Value> = vec![];
        let saved = upsert_collected_account(&mut store, found[0].account.clone());
        assert_eq!(store.len(), 1);
        assert_eq!(saved["uid"], "uid-new");

        let again = upsert_collected_account(&mut store, found[0].account.clone());
        assert_eq!(store.len(), 1, "重复导入必须幂等");
        assert_eq!(again["id"], saved["id"], "本地 id 必须保持不变");

        std::fs::remove_dir_all(&auth).ok();
        std::fs::remove_dir_all(&backups).ok();
    }

    // -----------------------------------------------------------------------
    // 重复条目：所有者实测「禁用了但状态不显示，且无法启用」的根因。
    //
    // 现场：同一个号在库里两条（uid/email 都是 null 的国际版 OAuth 账号），
    // 一条 disabled=true、另一条没有该字段。禁用只改到第一条，界面渲染两条，
    // 用户点的那张是第二条 ⇒ 显示未禁用、菜单只给「禁用」、网关导出时第一条被过滤。
    // -----------------------------------------------------------------------

    /// 补上 id 匹配后，uid 与 email **都缺失**的采集结果也能合并，不再每次 push。
    ///
    /// 这是重复条目的**产生**路径：此前 matches_identity 只看 uid/email，
    /// 两者皆 null 时恒为 false。
    #[test]
    fn collected_account_without_uid_or_email_merges_by_id() {
        // 现场形状：国际版 OAuth 账号，uid/email/nickname 全 null，只有 id 与 domain
        let collected = json!({
            "id": "acc-noid",
            "uid": null,
            "email": null,
            "nickname": null,
            "domain": "www.workbuddy.ai",
            "access_token": "AT-1",
            "createdAt": 100,
            "refreshedAt": 200,
        });

        let mut store: Vec<Value> = vec![collected.clone()];
        // 再采集一次（同 id）—— 修复前会变成 2 条
        let _ = upsert_collected_account(&mut store, collected.clone());
        assert_eq!(
            store.len(),
            1,
            "uid/email 都缺失时也必须按 id 合并，否则每采集一次多一条"
        );

        // 第三次（模拟刷新后再采集）仍应只有一条
        let mut again_input = collected.clone();
        again_input["access_token"] = json!("AT-2");
        again_input["refreshedAt"] = json!(300);
        let saved = upsert_collected_account(&mut store, again_input);
        assert_eq!(store.len(), 1, "反复采集必须幂等");
        assert_eq!(saved["access_token"], "AT-2", "token 应更新为最新");
    }

    /// `load_accounts` 会把已有重复条目**收敛成一条**并写回。
    ///
    /// 只修写入路径清不掉历史数据 —— 所有者库里已经有那一对。
    #[test]
    fn load_accounts_heals_duplicate_entries() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("account-dedupe");

        // 现场形状：老的在前（无 disabled、token 旧），新的在后（disabled=true、token 新）
        let old_entry = json!({
            "id": "dup-1", "uid": null, "email": null, "nickname": null,
            "domain": "www.workbuddy.ai",
            "access_token": "AT-OLD", "refresh_token": "RT-OLD",
            "createdAt": 100, "refreshedAt": 200,
        });
        let new_entry = json!({
            "id": "dup-1", "uid": null, "email": null, "nickname": null,
            "domain": "www.workbuddy.ai", "disabled": true,
            "access_token": "AT-NEW", "refresh_token": "RT-NEW",
            "createdAt": 100, "refreshedAt": 300,
        });
        let other = json!({
            "id": "keep-1", "uid": "u-keep", "nickname": "别的号",
            "access_token": "AT-K", "createdAt": 1, "refreshedAt": 1,
        });

        save_accounts(&[old_entry, other, new_entry]).unwrap();
        assert_eq!(
            load_accounts_from_path(&accounts_file()).len(),
            3,
            "写盘后应有 3 条"
        );

        let healed = load_accounts();
        assert_eq!(
            healed.len(),
            2,
            "重复 id 应被收敛成一条，实际 {} 条",
            healed.len()
        );

        let dup = healed
            .iter()
            .find(|a| a["id"] == "dup-1")
            .expect("合并后应保留该账号");
        // 1) disabled 取 true（禁用是显式意图，不该被旧条目无声解除）
        assert_eq!(dup["disabled"], json!(true), "disabled 应保留 true");
        // 2) token 取较新的
        assert_eq!(dup["access_token"], "AT-NEW", "token 应取 refreshedAt 较新者");
        // 3) createdAt 保留（同一次采集的证据）
        assert_eq!(dup["createdAt"], json!(100));

        // 写回要真的落盘（下次读取不再需要合并）
        let on_disk = load_accounts_from_path(&accounts_file());
        assert_eq!(
            on_disk.len(),
            2,
            "收敛结果必须写回文件，否则每次读取都要重算"
        );
    }

    /// 不同 id 的账号**不能**被误合并（哪怕它们 uid/email 都缺失）。
    #[test]
    fn dedupe_does_not_merge_distinct_ids() {
        let mut accounts = vec![
            json!({"id": "a", "uid": null, "email": null, "refreshedAt": 1}),
            json!({"id": "b", "uid": null, "email": null, "refreshedAt": 2}),
            json!({"id": "a", "uid": null, "email": null, "refreshedAt": 3}),
        ];
        let merged = dedupe_by_id(&mut accounts);
        assert!(merged);
        assert_eq!(accounts.len(), 2, "只合并同 id 的，不同 id 必须都留着");
        assert!(accounts.iter().any(|a| a["id"] == "b"));
    }

    /// 无 id 的条目**不参与**按 id 去重（身份未知，可能是不同账号）。
    #[test]
    fn dedupe_ignores_entries_without_id() {
        let mut accounts = vec![
            json!({"uid": "u1", "nickname": "无 id 甲"}),
            json!({"uid": "u1", "nickname": "无 id 乙"}),
        ];
        let merged = dedupe_by_id(&mut accounts);
        assert!(!merged, "无 id 的条目不该被按 id 合并");
        assert_eq!(accounts.len(), 2);
    }

    /// 备注也要保住：用户手写的标签比 token 更该留。
    #[test]
    fn dedupe_keeps_note_from_any_entry() {
        let mut accounts = vec![
            json!({"id": "n1", "uid": null, "refreshedAt": 100, "note": "主力号"}),
            json!({"id": "n1", "uid": null, "refreshedAt": 200}),
        ];
        assert!(dedupe_by_id(&mut accounts));
        assert_eq!(accounts.len(), 1);
        assert_eq!(accounts[0]["note"], "主力号", "旧条目的备注不该被丢掉");
        assert_eq!(accounts[0]["refreshedAt"], json!(200), "其余字段取较新者");
    }

    /// 造一个独立的临时扫描目录。
    fn temp_scan_dir(tag: &str) -> std::path::PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "wb-account-{tag}-{}",
            uuid::Uuid::new_v4().simple()
        ));
        std::fs::create_dir_all(&dir).expect("temp dir");
        dir
    }
}

/// 删除账号（按 id）。
pub fn delete_account(account_id: &str) -> Result<(), String> {
    delete_account_from_path(&accounts_file(), account_id)
}

/// 导入本机当前账号（从认证文件读取）。
pub fn import_local() -> Result<Value, String> {
    let acc = crate::modules::auth_file::import_from_auth_file()
        .ok_or("未读取到本地 WorkBuddy 登录信息")?;
    let saved = save_collected_account(acc).map_err(|e| e.to_string())?;
    Ok(account_meta(&saved))
}

/// 本机扫描的候选账号（脱敏，供界面展示与勾选）。
///
/// 序列化为 camelCase 以对接前端；Rust 侧保持 snake_case。
#[derive(Debug, Clone, serde::Serialize)]
#[serde(rename_all = "camelCase")]
pub struct LocalImportCandidate {
    /// 候选在本次扫描结果中的稳定索引。
    pub index: usize,
    /// 来源文件的绝对路径（同时是跨扫描稳定的选择键）。
    pub path: String,
    /// 账号元数据（不含 token）。
    pub meta: Value,
    /// 来源类型键：current / snapshot / backup。
    pub source: String,
    /// 来源类型展示名。
    pub source_label: String,
    /// 凭证可用性键：refreshable / access_only / expired。
    pub freshness: String,
    /// 凭证可用性展示名。
    pub freshness_label: String,
    /// 同账号在本机共有多少份文件（>1 表示存在更旧的重复快照）。
    pub duplicate_count: usize,
    /// 是否已在账号库中（按 区域+uid 命中）。
    pub already_imported: bool,
    /// 账号库中同区域同 uid、但凭证更旧：导入会用这本新凭证覆盖。
    pub updates_stored: bool,
    /// 文件最后修改时间（毫秒）。
    pub modified_at: i64,
}

/// 凭证可用性的展示名。
fn freshness_label(freshness: CredentialFreshness) -> &'static str {
    match freshness {
        CredentialFreshness::Refreshable => "可保活",
        CredentialFreshness::AccessOnly => "仅 access 有效",
        CredentialFreshness::Expired => "凭证已过期",
    }
}

/// 本机扫描结果（含来源目录与文件数统计，供界面说明扫描范围）。
#[derive(Debug, Clone, serde::Serialize)]
#[serde(rename_all = "camelCase")]
pub struct LocalScanResult {
    /// 去重后的候选账号，按凭证到期时间降序。
    pub candidates: Vec<LocalImportCandidate>,
    /// 识别出的认证文件总数（含被去重掉的旧快照）。
    pub files_scanned: usize,
    /// 认证文件目录。
    pub auth_dir: String,
    /// 本工具备份目录。
    pub backup_dir: String,
    /// 可导入（凭证未完全过期）的候选数。
    pub usable: usize,
}

/// 扫描本机全部历史登录态（当前认证文件 + 客户端快照 + 本工具备份）。
///
/// 这是「从本机导入」的数据来源：`import_local_all` 只看两个固定文件名，
/// 因此每区域最多 1 个账号；本函数额外扫出历史快照里的账号。
/// 同一 (区域, uid) 的多份文件已在 `discover_local_accounts` 内按凭证新旧去重。
///
/// 结果按凭证到期时间降序（越新越靠前），并标注哪些账号已在库中。
pub fn scan_local_accounts() -> LocalScanResult {
    let stored = load_accounts();
    let discovery = auth_file::discover_local();
    let files_scanned = discovery.files_scanned;

    let candidates: Vec<LocalImportCandidate> = discovery
        .candidates
        .into_iter()
        .enumerate()
        .map(|(index, candidate)| {
            let meta = account_meta(&candidate.account);
            // 已在库中的判定与 upsert 的身份规则一致：同区域 + 同 uid。
            let candidate_uid = get_str(&candidate.account, "uid");
            let existing = candidate_uid.as_deref().and_then(|uid| {
                stored.iter().find(|a| {
                    Region::of(a) == candidate.region && get_str(a, "uid").as_deref() == Some(uid)
                })
            });
            let candidate_exp = auth_file::credential_expiry(&candidate.account).unwrap_or(0);
            let stored_exp = existing
                .and_then(|a| auth_file::credential_expiry(a))
                .unwrap_or(0);
            LocalImportCandidate {
                index,
                path: candidate.path.to_string_lossy().into_owned(),
                meta,
                source: candidate.source.key().to_string(),
                source_label: candidate.source.label().to_string(),
                freshness: candidate.freshness.key().to_string(),
                freshness_label: freshness_label(candidate.freshness).to_string(),
                duplicate_count: candidate.duplicate_count,
                already_imported: existing.is_some(),
                // 库里没有 → 是新增；库里有但凭证比本机旧 → 导入会刷新它。
                updates_stored: existing.is_some() && candidate_exp > stored_exp,
                modified_at: candidate.modified_at,
            }
        })
        .collect();

    let usable = candidates
        .iter()
        .filter(|c| c.freshness != CredentialFreshness::Expired.key())
        .count();

    LocalScanResult {
        candidates,
        files_scanned,
        auth_dir: discovery.auth_dir.to_string_lossy().into_owned(),
        backup_dir: discovery.backup_dir.to_string_lossy().into_owned(),
        usable,
    }
}

/// 按来源路径（或扫描索引）把本机候选账号并入账号库。
///
/// 选择键用**文件路径**而非索引：扫描与导入之间隔着一次前端往返，期间
/// 客户端可能刚好写入新的快照而改变排序，路径才是稳定标识。`indexes`
/// 同时接受扫描顺序索引，兼容按序号调用的调用方。
///
/// 缺 uid 的候选按「区域 + 真实邮箱」去重（非空 uid 始终优先，与
/// `upsert_collected_account` 同一套规则）；完全无法定身份的记录照旧入库。
pub fn import_local_selected(
    paths: &[String],
    indexes: &[usize],
) -> Result<LocalImportResult, String> {
    let selected: Vec<auth_file::LocalCandidate> = auth_file::discover_local_accounts()
        .into_iter()
        .enumerate()
        .filter(|(index, candidate)| {
            paths.iter().any(|p| candidate.path.to_string_lossy() == p.as_str())
                || indexes.contains(index)
        })
        .map(|(_, candidate)| candidate)
        .collect();

    if selected.is_empty() {
        return Err("未选择任何本机账号（或所选文件已不存在，请重新扫描）".to_string());
    }

    let mut accounts = load_accounts();
    let mut added = 0usize;
    let mut updated = 0usize;
    let mut outcomes: Vec<Value> = Vec::new();

    for candidate in selected {
        // upsert 命中已有身份时会保留本地 id，因此用「保存结果的 id 是否早已
        // 存在」判断本次是覆盖还是新增，可同时覆盖 uid 命中与邮箱兜底命中。
        let known_ids: Vec<String> = accounts.iter().filter_map(|a| get_str(a, "id")).collect();
        let saved = upsert_collected_account(&mut accounts, candidate.account);
        let saved_id = get_str(&saved, "id").unwrap_or_default();
        let was_update = known_ids.iter().any(|id| id == &saved_id);
        if was_update {
            updated += 1;
        } else {
            added += 1;
        }
        outcomes.push(json!({
            "name": account_display_name(&saved),
            "region": Region::of(&saved).label(),
            "source": candidate.source.key(),
            "file": candidate.file_name,
            "freshness": candidate.freshness.key(),
            "updated": was_update,
        }));
    }

    save_accounts(&accounts).map_err(|e| format!("保存账号库失败：{e}"))?;

    Ok(LocalImportResult {
        imported: added + updated,
        added,
        updated,
        outcomes,
    })
}

/// 本机导入的结果计数。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LocalImportResult {
    /// 本次实际写入账号库的数量（新增 + 覆盖）。
    pub imported: usize,
    /// 其中新增的账号数。
    pub added: usize,
    /// 其中覆盖刷新既有账号的数量。
    pub updated: usize,
    /// 逐个账号的结果明细。
    pub outcomes: Vec<Value>,
}

/// 从本机一键导入**所有可发现的区域**（国服 + 国际版）。
///
/// CodeBuddy / WorkBuddy 客户端把不同区域的登录态写在同目录的**不同文件**里：
///   workbuddy-desktop.info       国服
///   workbuddy-desktop-ai.info    国际版
/// 因此这里逐个探测，把能读到的全部并入账号库。
///
/// 只读**当前**登录态（每区域 1 个）。历史登录过的账号要靠
/// `scan_local_accounts` / `import_local_selected` 才能发现。
///
/// 返回本次实际导入（或更新）的账号元数据列表；全部未发现时给出可操作的错误。
pub fn import_local_all() -> Result<Vec<Value>, String> {
    let mut imported: Vec<Value> = Vec::new();
    let mut notes: Vec<String> = Vec::new();

    for region in Region::ALL {
        match crate::modules::auth_file::import_from_auth_file_for(region) {
            None => notes.push(format!("{}：未发现本机登录", region.label())),
            Some(acc) => {
                // 区域以认证文件为准：旧记录可能缺 domain，用文件名兜底标注。
                let mut acc = acc;
                if get_str(&acc, "domain").is_none() {
                    acc["domain"] = json!(region.auth_domain());
                }
                match save_collected_account(acc) {
                    Ok(saved) => imported.push(account_meta(&saved)),
                    Err(e) => notes.push(format!("{}：保存失败 {e}", region.label())),
                }
            }
        }
    }

    if imported.is_empty() {
        return Err(format!(
            "未发现本机登录信息（已尝试 国服 / 国际版）。{}",
            notes.join("；")
        ));
    }
    Ok(imported)
}

// 手动添加账号（token 方式）已随 UI 入口「手动添加」一并下线；
// `identity_email` 中的 "手动添加" 占位过滤保留，用于兼容历史手动添加的旧账号。
