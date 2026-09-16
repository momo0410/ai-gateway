//! 数据目录迁移：`~/.ai-gateway` → `~/.wb-switch`。
//!
//! ## 为什么方向是反的（2026-09-16 拆分之后）
//!
//! 1.0.0 更名时迁移方向是 `~/.wb-switch` → `~/.ai-gateway`。2026-09-16 起
//! 本仓库（AI Gateway，1.x）与老仓库（workbuddy-switch-gateway，0.8.x）
//! **并行维护**，两版会同时装在同一台机器上，于是**共用同一份账号库**成了
//! 硬需求 —— 见 `config::store_dir` 的注释。
//!
//! 目录统一回 `.wb-switch`（0.8.x 一直在用、且此刻仍在被写入的那个），因此
//! 迁移方向反过来：把 `.ai-gateway` 里**已升级过的那批数据**并回 `.wb-switch`。
//!
//! ## 为什么标记文件名换了（这是关键，别合并成同一个）
//!
//! 老标记 `.migrated-from-wb-switch` 写在**目标目录**（当年的 `.ai-gateway`）里。
//! 已经跑过那次迁移的用户，其 `.ai-gateway` 里就有这个标记。若这次复用同名标记
//! 且仍写在目标目录（现在的 `.wb-switch`），逻辑会完全错乱：`.wb-switch` 里通常
//! 没有该标记（它不是当年那次迁移的目标），于是一次启动后标记被写进 `.wb-switch`
//! ——但用户的真实数据其实在 `.ai-gateway`，判定却已是「迁移完成」。
//!
//! 因此用**新的标记名** `.migrated-from-ai-gateway` 写在新的目标目录
//! （`.wb-switch`）里。它与老标记互相独立，两代迁移各自幂等，且对「已经跑过老
//! 迁移的机器」仍会正确地再执行一次反向合并。
//!
//! ## 为什么是「复制 + 并集合并」而不是「移动」
//!
//! 与初版同样的理由，且在拆分场景下更重要：用户可能仍在用 0.8.x，也可能回退。
//! 移动会掏空一侧，回退即数据丢失。并集合并（按 `(区域, uid)` 去重）保证两侧
//! 的账号汇到一处，且**不删除**来源目录里的任何东西。
//!
//! ## 为什么不能「目标已存在就跳过」（2026-09-16 之后的修正）
//!
//! 初版 `copy_dir_merged` 对**任何**已存在的条目都 `continue`。在有真实历史的
//! 机器上这会造成数据丢失：`.wb-switch` 与 `.ai-gateway` 并存期间，两边都在
//! 各自累加，`.wb-switch` 里留着 1.0.0 时代的**旧快照**，它把 `.ai-gateway`
//! 里更新更全的那份挡住了。实测（所有者机器）丢的是 `usage.json` 里
//! 2026-09-16 一天的 15.2 亿 input / 476 万 output / 14.7 亿 cacheRead /
//! 4964 条 records，以及 `gateway_config.json` 里 `allowed_model`
//! 与 `manual_uids` 两个整键。
//!
//! 更隐蔽的一层：`continue` 发生在 `from.is_dir()` 判断**之前**，所以只要目标
//! 已有 `gateway/` 目录，它下面的 `gateway_data/usage.json`、`gateway_config.json`
//! 等文件一个都搬不过来 —— 「跳过」实际是整个子树级的。
//!
//! 因此改为**按文件类型分别合并**，总原则是**只增不减**：合并结果不得丢掉
//! 任何一侧已有的数据。用户已经在旧数据上损失过一次，不能再冒风险。
//!
//! ## 各类文件为什么那样合并
//!
//! | 文件 | 合并方式 | 理由 |
//! |---|---|---|
//! | `gateway/gateway_data/usage.json` | 逐维逐键逐日期**取 max** | 分叉后两侧各自累加 |
//! | `gateway/gateway_config.json` | **只补目标缺失的键** | 目标侧的值是用户当前在用的，更权威 |
//! | `accounts.json` | 已有 `(区域, uid)` 并集，**保留目标侧** | 见 `merge_accounts_file`，既有行为 |
//! | 其余（通用文件） | 保持「已存在则不覆盖」 | 无法判断合并语义，见 `FileKind::Opaque` |
//!
//! 计数器为什么**取 max** 而不是相加：两个文件同源（都从迁移前那一份分叉），
//! 重叠的部分在两侧记的是同一个数。相加会把重叠算两遍（统计虚增），整体覆盖
//! 则丢掉更新一侧的数据（本次事故）。max 的语义正好是「这一格两边都算上」——
//! 分叉之后各自只增不减，较大的数必然已包含较小的数覆盖的全部请求。
//!
//! ## 幂等性
//!
//! 以目标目录里的标记文件为「已迁移」判据：① 重复启动不会反复拷贝；
//! ② 用户之后在某版里删掉的账号，不会被另一版的旧数据「复活」。
//!
//! 合并本身也做成幂等的（取 max / 只补缺失键都是幂等运算，且内容无变化时不写盘），
//! 这样即使标记写失败导致下次重跑，结果也不变。
//!
//! ## 环境变量覆盖时不迁移
//!
//! 设了 `AI_GATEWAY_HOME` 说明用户在刻意隔离（开发/测试实例），
//! 此时把真实用户数据搬进去反而会污染隔离环境。

use std::path::{Path, PathBuf};

use super::config;

/// 旧数据目录名（更名后、拆分前那一代）。
const LEGACY_DIR_NAME: &str = ".ai-gateway";

/// 迁移结果摘要。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum MigrationOutcome {
    /// 本次完成了迁移，携带拷贝的条目数与并集合并进来的账号数。
    Migrated {
        from: PathBuf,
        to: PathBuf,
        entries: usize,
        accounts_merged: usize,
    },
    /// 无需迁移（旧目录不存在 / 已迁移过 / 目标被环境变量覆盖）。
    Skipped(&'static str),
}

impl MigrationOutcome {
    /// 是否发生了实际迁移。
    pub fn migrated(&self) -> bool {
        matches!(self, MigrationOutcome::Migrated { .. })
    }

    /// 人类可读描述。
    pub fn describe(&self) -> String {
        match self {
            MigrationOutcome::Migrated {
                from,
                to,
                entries,
                accounts_merged,
            } => {
                let extra = if *accounts_merged > 0 {
                    format!("，并集合并 {accounts_merged} 个账号")
                } else {
                    String::new()
                };
                format!(
                    "已从旧数据目录迁移 {entries} 个条目{extra}：{} → {}（旧目录保留，可继续用旧版）",
                    from.display(),
                    to.display()
                )
            }
            MigrationOutcome::Skipped(reason) => format!("跳过数据目录迁移：{reason}"),
        }
    }
}

/// 迁移完成标记文件名（写在目标目录内）。
///
/// **为什么需要显式标记，而不是「目标目录有数据就跳过」**：后者在实测中造成过
/// 真实的数据不可见 —— 一次截图演示模式的运行在空的新目录下建出了一份**不完整**
/// 的账号库（2 个账号），而「目标已有数据」正是跳过条件，于是旧目录里真正的
/// 8 个账号从此再也搬不过来。
///
/// 标记的另一个作用：迁移只在**第一次**启动时发生。之后用户在某一版里删除的
/// 账号，不会被另一版的数据「复活」。
///
/// **注意**：这个文件名与更名前那一代的 `.migrated-from-wb-switch` 刻意不同，
/// 原因见文件头注释 —— 两者若同名会让「已跑过老迁移的机器」被误判为已完成。
const MARKER_FILE: &str = ".migrated-from-ai-gateway";

/// 旧数据目录路径（更名后、拆分前那一代：`~/.ai-gateway`）。
pub fn legacy_store_dir() -> PathBuf {
    config::home_dir().join(LEGACY_DIR_NAME)
}

/// 是否已完成过迁移。
pub fn already_migrated(target: &Path) -> bool {
    target.join(MARKER_FILE).is_file()
}

/// 写迁移完成标记（内容记录来源与时间，便于排查）。
fn write_marker(target: &Path, from: &Path) -> std::io::Result<()> {
    std::fs::create_dir_all(target)?;
    config::atomic_write(
        &target.join(MARKER_FILE),
        &format!(
            "migrated_from={}\nmigrated_at={}\n",
            from.display(),
            config::utc_iso()
        ),
    )
}

/// 执行一次迁移（幂等，可重复调用）。
///
/// 方向：`~/.ai-gateway`（更名后那一代）→ `~/.wb-switch`（0.8.x 线一直在用、
/// 且拆分后两版共用）。见文件头注释。
pub fn migrate_store_dir() -> MigrationOutcome {
    // 环境变量覆盖 = 用户刻意隔离，不迁移
    if std::env::var_os("AI_GATEWAY_HOME").is_some_and(|v| !v.is_empty()) {
        return MigrationOutcome::Skipped("已通过 AI_GATEWAY_HOME 指定数据目录");
    }
    migrate_from_to(&legacy_store_dir(), &config::store_dir())
}

/// 用量统计文件名（`gateway/gateway_data/usage.json`）。
///
/// 与 `gateway.rs` 的 `GATEWAY_STATE_DIR` 必须保持一致：用量文件按约定与账号池
/// `state.json` 同目录（见 go-gateway/README.md），改名会让合并规则失效。
const USAGE_FILE_NAME: &str = "usage.json";

/// 用量统计文件相对数据目录的路径。
fn usage_file_rel_path() -> PathBuf {
    Path::new("gateway").join("gateway_data").join(USAGE_FILE_NAME)
}

/// 网关配置文件相对数据目录的路径。
fn gateway_config_rel_path() -> PathBuf {
    Path::new("gateway").join("gateway_config.json")
}

/// 迁移的纯函数内核（不读环境变量，因此可被单元测试直接驱动）。
pub fn migrate_from_to(legacy: &Path, target: &Path) -> MigrationOutcome {
    if !legacy.is_dir() {
        return MigrationOutcome::Skipped("未发现旧数据目录");
    }
    if legacy == target {
        return MigrationOutcome::Skipped("新旧数据目录相同");
    }
    // 已迁移过就永不再跑：否则用户在新版本里删掉的账号会被旧目录「复活」
    if already_migrated(target) {
        return MigrationOutcome::Skipped("已完成过迁移");
    }

    // 账号库先做并集合并，再拷贝其余文件。
    // 合并而非跳过：目标目录可能已有一份不完整的账号库（如演示运行留下的），
    // 直接跳过会让旧目录里的真实账号永久不可见。
    let merged = merge_accounts_file(&legacy.join("accounts.json"), &target.join("accounts.json"));

    let entries = match copy_dir_merged(legacy, target) {
        Ok(n) => n,
        // 迁移失败不该阻断启动：用户仍可手动拷贝，或重新登录账号。
        // 不写标记，下次启动会重试。
        Err(e) => {
            eprintln!("[migrate] 数据目录迁移失败（不阻断启动）: {e}");
            return MigrationOutcome::Skipped("迁移过程出错");
        }
    };

    // 标记写失败也视为迁移未完成：下次启动重试（合并是幂等的，重试安全）
    if let Err(e) = write_marker(target, legacy) {
        eprintln!("[migrate] 写入迁移标记失败（下次启动将重试）: {e}");
        return MigrationOutcome::Skipped("迁移标记写入失败");
    }

    MigrationOutcome::Migrated {
        from: legacy.to_path_buf(),
        to: target.to_path_buf(),
        entries,
        accounts_merged: merged,
    }
}

/// 把旧账号库里的账号并入新账号库（按 `uid` 去重，已有条目保持不变）。
///
/// 返回新增的账号数。任一侧读取/解析失败时**不改动目标文件**（返回 0）——
/// 账号库是唯一真源，宁可少合并也不能写坏。
fn merge_accounts_file(legacy: &Path, target: &Path) -> usize {
    let Ok(legacy_text) = std::fs::read_to_string(legacy) else {
        return 0;
    };
    let Ok(legacy_accounts) = serde_json::from_str::<Vec<serde_json::Value>>(&legacy_text) else {
        return 0;
    };
    if legacy_accounts.is_empty() {
        return 0;
    }

    // 目标不存在或为空：直接由 copy_dir_merged 拷贝，这里不重复处理
    let mut target_accounts: Vec<serde_json::Value> = match std::fs::read_to_string(target) {
        Ok(text) => serde_json::from_str(&text).unwrap_or_default(),
        Err(_) => return 0,
    };

    // 身份键必须与账号库自身的判重口径一致：`(区域, uid)`。
    //
    // 只用 uid 而不是「uid → id 回退」：`id` 是账号库内部的随机标识，同一个账号
    // 被应用重新采集/富化后（如补上 `needs_relogin`）会换一个 `id`。若回退到 id，
    // 同一个账号会被算成两个不同的键，合并后出现重复条目 —— 实测踩到过。
    let existing: std::collections::HashSet<(String, String)> = target_accounts
        .iter()
        .filter_map(|a| account_identity(a))
        .collect();

    let mut added = 0usize;
    for acc in legacy_accounts {
        match account_identity(&acc) {
            Some(key) if !existing.contains(&key) => {
                target_accounts.push(acc);
                added += 1;
            }
            // 无 uid 的条目无法判重：跳过，避免把同一个账号重复塞进去
            _ => {}
        }
    }
    if added == 0 {
        return 0;
    }

    let Ok(content) = serde_json::to_string_pretty(&target_accounts) else {
        return 0;
    };
    match config::atomic_write(target, &content) {
        Ok(()) => added,
        Err(e) => {
            eprintln!("[migrate] 合并账号库失败（保留原文件）: {e}");
            0
        }
    }
}

/// 账号身份键：`(区域, uid)`，与 `account::upsert_collected_account` 的判重口径一致。
///
/// 区域来自 `domain` 后缀（`.cn` → 国服，其余 → 国际版）：两个区域的身份命名空间
/// **相互独立**，同一串 uid 可以同时存在于国服与国际版，跨区域永不合并。
fn account_identity(acc: &serde_json::Value) -> Option<(String, String)> {
    let uid = acc.get("uid").and_then(|v| v.as_str())?.trim();
    if uid.is_empty() {
        return None;
    }
    let region = acc
        .get("domain")
        .and_then(|v| v.as_str())
        .map(|d| {
            if d.trim_end().ends_with(".cn") {
                "cn"
            } else {
                "intl"
            }
        })
        .unwrap_or("cn");
    Some((region.to_string(), uid.to_string()))
}

/// 一个文件条目的合并策略。
///
/// **为什么需要分类**：初版对所有已存在条目一律跳过，把「无法判断合并语义」
/// 和「明明知道该怎么合并」混为一谈 —— 前者跳过是合理的保守，后者跳过就是
/// 数据丢失。分类之后，只有真正无从判断的那一类才退回「不覆盖」。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum FileKind {
    /// 计数器文件（`usage.json`）：逐维逐键逐日期取 max。
    Counter,
    /// 配置对象文件（`gateway_config.json`）：只补目标侧缺失的键。
    ConfigObject,
    /// 账号库：由 `merge_accounts_file` 先行处理，此处不重复介入。
    Accounts,
    /// 通用文件：语义未知，已存在则不覆盖。
    Opaque,
}

/// 判断一个条目该用哪种合并策略。
///
/// `rel` 是相对数据目录根的路径。用 `/` 归一化后与常量路径比较（`Path::join`
/// 在 Windows 上产出 `\`，直接比字符串会漏判）。路径常量与 `gateway.rs` 里
/// 真实使用的文件名保持一致 —— 两处一旦分叉，合并规则会静默失效。
fn classify_entry(rel: &str) -> FileKind {
    let normalized = rel.replace('\\', "/");
    if normalized == "accounts.json" {
        return FileKind::Accounts;
    }
    if normalized
        == usage_file_rel_path()
            .to_string_lossy()
            .replace('\\', "/")
    {
        return FileKind::Counter;
    }
    if normalized
        == gateway_config_rel_path()
            .to_string_lossy()
            .replace('\\', "/")
    {
        return FileKind::ConfigObject;
    }
    FileKind::Opaque
}

/// 递归拷贝目录内容，按文件类型分别合并（返回拷贝/改写的条目数）。
///
/// **原实现为什么选择「跳过」**：注释写的理由是「迁移中途失败后重试时，已拷贝的
/// 文件保持原样，不会用旧数据盖掉本轮已经写入的新内容」。这个担心本身是对的
/// （重试安全确实重要），但它把手段用错了 —— 避免「旧盖新」的正确做法是
/// **按类型做只增不减的合并**，而不是整文件跳过。跳过在重试场景下确实安全，
/// 代价却是首次迁移就把更新的一侧永久丢弃，且用户看不出来。
///
/// 注意 `to.exists()` 的 `continue` 原先在 `from.is_dir()` **之前**：
/// 目标只要有 `gateway/` 目录，其下所有文件都不再处理。所以本函数现在对目录
/// **始终递归**，只在文件层面做策略分派。
fn copy_dir_merged(src: &Path, dest: &Path) -> std::io::Result<usize> {
    copy_dir_merged_at(src, dest, Path::new(""))
}

/// `copy_dir_merged` 的实现体；`rel` 是当前层相对数据目录根的路径。
fn copy_dir_merged_at(src: &Path, dest: &Path, rel: &Path) -> std::io::Result<usize> {
    std::fs::create_dir_all(dest)?;
    let mut count = 0usize;
    for entry in std::fs::read_dir(src)? {
        let entry = entry?;
        let from = entry.path();
        let to = dest.join(entry.file_name());
        let child_rel = rel.join(entry.file_name());

        if from.is_dir() {
            // 目录一律递归下探：见函数头注释，跳过整个子树是本次事故的一部分。
            count += copy_dir_merged_at(&from, &to, &child_rel)?;
            continue;
        }

        // 目标不存在：直接拷贝（与原先一致，也天然满足「只增不减」）。
        if !to.exists() {
            std::fs::copy(&from, &to)?;
            count += 1;
            continue;
        }

        // 目标已存在：按类型决定怎么合。
        let changed = match classify_entry(&child_rel.to_string_lossy()) {
            // 账号库已由 merge_accounts_file 处理过，避免二次写入
            FileKind::Accounts => false,
            FileKind::Counter => merge_counter_file(&from, &to),
            FileKind::ConfigObject => merge_config_file(&from, &to),
            // 通用文件：无法判断合并语义（可能是用户的日志、备份、二进制等），
            // 猜错的代价是写坏用户数据，因此保持原行为「已存在则不覆盖」。
            // 这与初版的区别在于：这是**有意识**的保守选择，而不是顺手跳过。
            FileKind::Opaque => false,
        };
        if changed {
            count += 1;
        }
    }
    Ok(count)
}

/// 把两侧的 JSON 文本读出来并解析；任一侧缺失或解析失败都返回 `None`。
///
/// 解析失败时调用方**必须**保持目标文件原样：迁移宁可少合并，也绝不能写出
/// 半个损坏的 JSON —— 那会让程序直接起不来，比不合并严重得多。
fn read_json(path: &Path) -> Option<serde_json::Value> {
    let text = std::fs::read_to_string(path).ok()?;
    serde_json::from_str(&text).ok()
}

/// 合并计数器文件（`usage.json`）：逐维逐键逐日期取 max。
///
/// 返回是否改动了目标文件（内容无变化时不写盘，保证幂等且不扰动时间戳）。
///
/// **为什么取 max 而不是相加或覆盖**：
/// - 相加：两侧同源分叉，重叠部分会被算两遍，统计虚增；
/// - 覆盖：丢掉被覆盖那一侧独有的数据 —— 这正是本次要修的事故；
/// - max：分叉后两侧都只累加不减少，较大的一侧必然已包含另一侧覆盖的全部请求，
///   因此 max 等价于「两边都算上」且不重复计数。
///
/// 结构上是 `{days, models, accounts, accountModels}` 四层嵌套的计数对象，
/// 由 `merge_counters_value` 递归处理；顶层其余字段（`version` / `savedAt`）
/// 取目标侧 —— 它们是元信息不是计数，max 无意义。
fn merge_counter_file(legacy: &Path, target: &Path) -> bool {
    let (Some(legacy_json), Some(target_json)) = (read_json(legacy), read_json(target)) else {
        return false;
    };
    let (Some(legacy_obj), Some(mut target_obj)) =
        (legacy_json.as_object(), target_json.as_object().cloned())
    else {
        return false;
    };

    let mut mutated = false;
    for (key, legacy_val) in legacy_obj {
        // version / savedAt 等元信息不参与合并：保留目标侧（同 ConfigObject 的理由）
        if !matches!(
            key.as_str(),
            "days" | "models" | "accounts" | "accountModels"
        ) {
            continue;
        }
        match target_obj.get_mut(key) {
            // 目标缺这一整个维度：从旧侧补上（只增不减）
            None => {
                target_obj.insert(key.clone(), legacy_val.clone());
                mutated = true;
            }
            Some(slot) => {
                if merge_counters_value(slot, legacy_val) {
                    mutated = true;
                }
            }
        }
    }

    if !mutated {
        return false;
    }
    write_json(target, &serde_json::Value::Object(target_obj))
}

/// 递归合并两个计数结构，把 `legacy` 里更大的值写进 `target`。
///
/// 返回是否发生了改动。`Counters` 是叶子（全部字段为整数），中间层是
/// `键 -> 下一层` 的映射。判断顺序上是「先看是不是计数叶子」——两边都是对象
/// 时才继续下探，避免把计数对象错当映射层拆开。
fn merge_counters_value(target: &mut serde_json::Value, legacy: &serde_json::Value) -> bool {
    let target_is_leaf = is_counter_leaf(target);
    let legacy_is_leaf = is_counter_leaf(legacy);

    // 两侧都是计数叶子：逐字段取 max
    if target_is_leaf && legacy_is_leaf {
        return merge_counter_leaf(target, legacy);
    }
    // 只有一侧是计数叶子：结构不一致（正常情况下不会出现）。此时绝不能走下面的
    // 映射层分支 —— 那会把日期键塞进计数对象里，产出 `{"input":5,"2026-09-16":{…}}`
    // 这种半损坏的 JSON。保留目标侧即可（只增不减优先保证「不写坏」）。
    if target_is_leaf || legacy_is_leaf {
        return false;
    }

    let (Some(target_map), Some(legacy_map)) = (target.as_object_mut(), legacy.as_object()) else {
        // 结构不一致（一侧是标量/数组，另一侧是对象）：无法安全合并，保留目标侧。
        // 不做「取大的那个整体替换」——类型都不同，猜错就是写坏统计。
        return false;
    };

    let mut mutated = false;
    for (key, legacy_val) in legacy_map {
        match target_map.get_mut(key) {
            None => {
                target_map.insert(key.clone(), legacy_val.clone());
                mutated = true;
            }
            Some(slot) => {
                if merge_counters_value(slot, legacy_val) {
                    mutated = true;
                }
            }
        }
    }
    mutated
}

/// 是否是一组计数的叶子节点：非空对象，且所有值是整数。
///
/// 用「全部值都是整数」而不是「含 input/output 键」来判定，是因为后者会把
/// 未来新增的维度误判成叶子；前者对结构更宽容，且空对象被排除（空对象更像
/// 空的映射层）。
fn is_counter_leaf(v: &serde_json::Value) -> bool {
    let Some(map) = v.as_object() else {
        return false;
    };
    !map.is_empty() && map.values().all(|x| x.is_i64() || x.is_u64() || x.is_f64())
}

/// 一组 Counters 逐字段取 max。
///
/// 遍历**旧侧实际出现的键**，而不是只认 `COUNTER_FIELDS`：上游将来新增一个
/// 计数维度（或老版本文件里多出某个字段）时，这样能自动一起取 max；
/// 写死字段名会把它静默漏掉 —— 那正是「只增不减」要禁止的丢失。
fn merge_counter_leaf(target: &mut serde_json::Value, legacy: &serde_json::Value) -> bool {
    let mut mutated = false;
    let Some(legacy_map) = legacy.as_object() else {
        return false;
    };
    for (field, legacy_val) in legacy_map {
        let Some(legacy_num) = json_as_i64(legacy_val) else {
            continue;
        };
        let target_num = target.get(field).and_then(json_as_i64);
        // 目标缺该字段，或旧侧更大 → 取旧侧
        if target_num.is_none_or(|t| legacy_num > t) {
            if let Some(obj) = target.as_object_mut() {
                obj.insert(field.clone(), serde_json::json!(legacy_num));
                mutated = true;
            }
        }
    }
    mutated
}

/// JSON 数字取 i64（容忍浮点写法，截断为整数）。
fn json_as_i64(v: &serde_json::Value) -> Option<i64> {
    v.as_i64().or_else(|| v.as_f64().map(|f| f as i64))
}

/// 合并配置对象文件（`gateway_config.json`）：只补目标侧缺失的键。
///
/// **为什么不覆盖已有的值**：目标目录（`.wb-switch`）是 0.8.x 线一直在用、
/// 此刻仍在被写入的那一份，其中的值是用户**当前正在用**的配置，比旧的
/// `.ai-gateway` 更权威。反过来，目标侧缺失的键（实测有 `allowed_model`
/// —— 单一模型锁定 —— 和 `manual_uids`）是旧实现整键丢掉的，必须补回来。
///
/// 只做**顶层**键的补充，不递归合并嵌套对象：嵌套对象里的值同样是用户在用的
/// 配置，逐叶子取并集容易造出「一半旧一半新」的自相矛盾配置。
fn merge_config_file(legacy: &Path, target: &Path) -> bool {
    let (Some(legacy_json), Some(target_json)) = (read_json(legacy), read_json(target)) else {
        return false;
    };
    let (Some(legacy_obj), Some(mut target_obj)) =
        (legacy_json.as_object(), target_json.as_object().cloned())
    else {
        return false;
    };

    let mut mutated = false;
    for (key, value) in legacy_obj {
        if !target_obj.contains_key(key) {
            target_obj.insert(key.clone(), value.clone());
            mutated = true;
        }
    }
    if !mutated {
        return false;
    }
    write_json(target, &serde_json::Value::Object(target_obj))
}

/// 序列化并原子写回；失败只告警不改判定（迁移不因合并失败而中断）。
fn write_json(path: &Path, value: &serde_json::Value) -> bool {
    let Ok(content) = serde_json::to_string_pretty(value) else {
        return false;
    };
    match config::atomic_write(path, &content) {
        Ok(()) => true,
        Err(e) => {
            eprintln!("[migrate] 合并 {} 失败（保留原文件）: {e}", path.display());
            false
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 一组 Counters 的已知字段名（与 go-gateway `usage.Counters` 的 JSON tag 对齐）。
    ///
    /// 仅测试使用：生产代码刻意**不**写死字段名（见 `merge_counter_leaf`），
    /// 这里用它来断言「已知维度的口径」与做只增不减的通用校验。
    const COUNTER_FIELDS: [&str; 5] = ["input", "output", "cacheRead", "cacheWrite", "records"];

    /// 造一个独立的测试目录对（旧 / 新）。
    fn pair(tag: &str) -> (PathBuf, PathBuf, PathBuf) {
        let base = std::env::temp_dir().join(format!(
            "ai-gateway-mig-{tag}-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .subsec_nanos()
        ));
        let _ = std::fs::remove_dir_all(&base);
        std::fs::create_dir_all(&base).unwrap();
        // 方向：legacy = 更名后那一代（.ai-gateway），target = 共用的 .wb-switch。
        let legacy = base.join(".ai-gateway");
        let target = base.join(".wb-switch");
        (base, legacy, target)
    }

    #[test]
    fn 合并拷贝不覆盖已存在文件() {
        let (base, src, dest) = pair("copy");
        std::fs::create_dir_all(src.join("nested")).unwrap();
        std::fs::write(src.join("a.txt"), "old-a").unwrap();
        std::fs::write(src.join("b.txt"), "b").unwrap();
        std::fs::write(src.join("nested").join("c.txt"), "c").unwrap();

        // 目标已有 a.txt：必须保留目标内容
        std::fs::create_dir_all(&dest).unwrap();
        std::fs::write(dest.join("a.txt"), "new-a").unwrap();

        let count = copy_dir_merged(&src, &dest).unwrap();
        assert_eq!(count, 2, "只应拷贝 b.txt 与 nested");
        assert_eq!(std::fs::read_to_string(dest.join("a.txt")).unwrap(), "new-a");
        assert_eq!(std::fs::read_to_string(dest.join("b.txt")).unwrap(), "b");
        assert_eq!(
            std::fs::read_to_string(dest.join("nested").join("c.txt")).unwrap(),
            "c"
        );
        let _ = std::fs::remove_dir_all(&base);
    }

    #[test]
    fn 端到端迁移把旧目录内容搬到新目录且保留旧目录() {
        let (base, legacy, target) = pair("e2e");
        std::fs::create_dir_all(legacy.join("gateway")).unwrap();
        std::fs::write(legacy.join("accounts.json"), r#"[{"uid":"u1"}]"#).unwrap();
        std::fs::write(legacy.join("gateway").join("gateway_config.json"), "{}").unwrap();

        let outcome = migrate_from_to(&legacy, &target);
        assert!(outcome.migrated(), "应完成迁移：{outcome:?}");
        let accounts = std::fs::read_to_string(target.join("accounts.json")).unwrap();
        assert!(accounts.contains("u1"));
        assert!(target.join("gateway").join("gateway_config.json").exists());
        assert!(
            legacy.join("accounts.json").exists(),
            "旧目录必须保留，用户回退旧版时数据还在"
        );

        // 幂等：再跑一次应跳过（已写完成标记）
        assert_eq!(
            migrate_from_to(&legacy, &target),
            MigrationOutcome::Skipped("已完成过迁移")
        );
        let _ = std::fs::remove_dir_all(&base);
    }

    /// **回归测试**：这是实测踩到的真实数据不可见事故。
    ///
    /// 一次演示模式的运行在空的新目录下建出了一份**不完整**的账号库（2 个账号），
    /// 而旧版本用「目标已有数据就跳过」作判据 —— 于是旧目录里真正的 8 个账号
    /// 从此再也搬不过来。现在改为「并集合并 + 显式完成标记」。
    #[test]
    fn 目标已有不完整账号库时仍能并入旧账号() {
        let (base, legacy, target) = pair("partial");
        std::fs::create_dir_all(&legacy).unwrap();
        std::fs::create_dir_all(&target).unwrap();

        // 旧目录：8 个账号
        let legacy_accounts: Vec<serde_json::Value> = (1..=8)
            .map(|i| serde_json::json!({"uid": format!("u{i}"), "nickname": format!("号{i}")}))
            .collect();
        std::fs::write(
            legacy.join("accounts.json"),
            serde_json::to_string(&legacy_accounts).unwrap(),
        )
        .unwrap();

        // 新目录：演示运行留下的 2 个账号（是旧库的子集）
        std::fs::write(
            target.join("accounts.json"),
            r#"[{"uid":"u1","nickname":"号1"},{"uid":"u2","nickname":"号2"}]"#,
        )
        .unwrap();

        let outcome = migrate_from_to(&legacy, &target);
        assert!(outcome.migrated(), "目标已有数据也必须完成迁移：{outcome:?}");

        let merged: Vec<serde_json::Value> = serde_json::from_str(
            &std::fs::read_to_string(target.join("accounts.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(merged.len(), 8, "8 个旧账号必须全部可见，实际 {}", merged.len());
        for i in 1..=8 {
            assert!(
                merged.iter().any(|a| a["uid"] == format!("u{i}")),
                "账号 u{i} 丢失"
            );
        }
        let _ = std::fs::remove_dir_all(&base);
    }

    #[test]
    fn 迁移完成后新版本里删除的账号不会被旧目录复活() {
        let (base, legacy, target) = pair("no-resurrect");
        std::fs::create_dir_all(&legacy).unwrap();
        std::fs::write(
            legacy.join("accounts.json"),
            r#"[{"uid":"u1"},{"uid":"u2"}]"#,
        )
        .unwrap();

        assert!(migrate_from_to(&legacy, &target).migrated());

        // 用户在新版本里删掉了 u2
        std::fs::write(target.join("accounts.json"), r#"[{"uid":"u1"}]"#).unwrap();

        // 再次启动：不得把 u2 搬回来
        assert_eq!(
            migrate_from_to(&legacy, &target),
            MigrationOutcome::Skipped("已完成过迁移")
        );
        let after = std::fs::read_to_string(target.join("accounts.json")).unwrap();
        assert!(!after.contains("u2"), "已删除的账号不得被旧目录复活");
        let _ = std::fs::remove_dir_all(&base);
    }

    #[test]
    fn 账号合并按_uid_去重且保留新库里的条目() {
        let (base, legacy, target) = pair("dedup");
        std::fs::create_dir_all(&legacy).unwrap();
        std::fs::create_dir_all(&target).unwrap();
        std::fs::write(
            legacy.join("accounts.json"),
            r#"[{"uid":"u1","nickname":"旧名"},{"uid":"u2"}]"#,
        )
        .unwrap();
        // 新库里 u1 已被用户改过备注：不得被旧值覆盖
        std::fs::write(
            target.join("accounts.json"),
            r#"[{"uid":"u1","nickname":"新名"}]"#,
        )
        .unwrap();

        let added = merge_accounts_file(
            &legacy.join("accounts.json"),
            &target.join("accounts.json"),
        );
        assert_eq!(added, 1, "只有 u2 是新增的");

        let merged: Vec<serde_json::Value> = serde_json::from_str(
            &std::fs::read_to_string(target.join("accounts.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(merged.len(), 2);
        let u1 = merged.iter().find(|a| a["uid"] == "u1").unwrap();
        assert_eq!(u1["nickname"], "新名", "新库里的条目不得被旧值覆盖");
        let _ = std::fs::remove_dir_all(&base);
    }

    #[test]
    fn 账号库损坏时不写坏目标文件() {
        let (base, legacy, target) = pair("corrupt");
        std::fs::create_dir_all(&legacy).unwrap();
        std::fs::create_dir_all(&target).unwrap();
        std::fs::write(legacy.join("accounts.json"), "not json at all").unwrap();
        std::fs::write(target.join("accounts.json"), r#"[{"uid":"keep"}]"#).unwrap();

        let added = merge_accounts_file(
            &legacy.join("accounts.json"),
            &target.join("accounts.json"),
        );
        assert_eq!(added, 0);
        let text = std::fs::read_to_string(target.join("accounts.json")).unwrap();
        assert!(text.contains("keep"), "目标文件必须保持原样");
        let _ = std::fs::remove_dir_all(&base);
    }

    #[test]
    fn 无_uid_的条目不入库避免重复() {
        let (base, legacy, target) = pair("nouid");
        std::fs::create_dir_all(&legacy).unwrap();
        std::fs::create_dir_all(&target).unwrap();
        std::fs::write(legacy.join("accounts.json"), r#"[{"nickname":"无标识"}]"#).unwrap();
        std::fs::write(target.join("accounts.json"), r#"[{"uid":"u1"}]"#).unwrap();

        let added = merge_accounts_file(
            &legacy.join("accounts.json"),
            &target.join("accounts.json"),
        );
        assert_eq!(added, 0, "无 uid 无法判重，宁可不并入");
        let _ = std::fs::remove_dir_all(&base);
    }

    #[test]
    fn 旧目录不存在时跳过() {
        let (base, _, target) = pair("absent");
        let outcome = migrate_from_to(&base.join("nope"), &target);
        assert_eq!(outcome, MigrationOutcome::Skipped("未发现旧数据目录"));
        let _ = std::fs::remove_dir_all(&base);
    }

    #[test]
    fn 新旧目录相同时跳过() {
        let base = std::env::temp_dir().join(format!("ai-gateway-mig-same-{}", std::process::id()));
        let _ = std::fs::create_dir_all(&base);
        let outcome = migrate_from_to(&base, &base);
        assert_eq!(outcome, MigrationOutcome::Skipped("新旧数据目录相同"));
        let _ = std::fs::remove_dir_all(&base);
    }

    #[test]
    fn 完成标记的读写() {
        let (base, legacy, target) = pair("marker");
        std::fs::create_dir_all(&legacy).unwrap();
        assert!(!already_migrated(&target), "标记未写时应为假");
        write_marker(&target, &legacy).unwrap();
        assert!(already_migrated(&target));
        let text = std::fs::read_to_string(target.join(MARKER_FILE)).unwrap();
        assert!(text.contains("migrated_from="));
        assert!(text.contains("migrated_at="));
        let _ = std::fs::remove_dir_all(&base);
    }

    #[test]
    fn 描述文案包含关键信息() {
        let outcome = MigrationOutcome::Migrated {
            from: PathBuf::from("C:\\old"),
            to: PathBuf::from("C:\\new"),
            entries: 5,
            accounts_merged: 3,
        };
        let text = outcome.describe();
        assert!(text.contains('5'));
        assert!(text.contains("3 个账号"));
        assert!(text.contains("C:\\old"));
        assert!(text.contains("C:\\new"));
        assert!(text.contains("旧目录保留"), "必须说明旧目录未被删除");

        let skipped = MigrationOutcome::Skipped("未发现旧数据目录");
        assert!(skipped.describe().contains("未发现旧数据目录"));
        assert!(!skipped.migrated());
    }

    #[test]
    fn 旧目录名常量为更名后那一代() {
        assert_eq!(LEGACY_DIR_NAME, ".ai-gateway");
        assert!(legacy_store_dir().ends_with(".ai-gateway"));
    }

    /// 迁移标记文件名必须与更名前那一代**不同**。
    ///
    /// 同名会让「已跑过老迁移的机器」被误判为已完成：老标记当年写在
    /// `.ai-gateway`，而现在的目标目录是 `.wb-switch`，两者不可混用。
    #[test]
    fn 迁移标记名与更名前那一代不冲突() {
        assert_ne!(MARKER_FILE, ".migrated-from-wb-switch");
        assert_eq!(MARKER_FILE, ".migrated-from-ai-gateway");
    }

    #[test]
    fn 账号身份键为_区域_加_uid() {
        // 国服
        assert_eq!(
            account_identity(&serde_json::json!({"uid":"a","domain":"www.workbuddy.cn"})),
            Some(("cn".to_string(), "a".to_string()))
        );
        // 国际版
        assert_eq!(
            account_identity(&serde_json::json!({"uid":"a","domain":"www.workbuddy.ai"})),
            Some(("intl".to_string(), "a".to_string()))
        );
        // 缺 domain：按国服（与 Region::of 的默认口径一致）
        assert_eq!(
            account_identity(&serde_json::json!({"uid":"a"})),
            Some(("cn".to_string(), "a".to_string()))
        );
        // 无 uid / 空 uid：无法判重
        assert_eq!(account_identity(&serde_json::json!({"id":"x"})), None);
        assert_eq!(account_identity(&serde_json::json!({"uid":"  "})), None);
    }

    /// **回归测试**：同一个账号被应用重新采集/富化后 `id` 会变，
    /// 若判重回退到 `id`，合并后就会出现重复条目（实测踩到过）。
    #[test]
    fn 同一账号_id_变化时仍判为同一个不重复合并() {
        let (base, legacy, target) = pair("id-drift");
        std::fs::create_dir_all(&legacy).unwrap();
        std::fs::create_dir_all(&target).unwrap();

        // 旧库：原始条目，无 needs_relogin
        std::fs::write(
            legacy.join("accounts.json"),
            r#"[{"uid":"u1","id":"old-id","email":"a@b.c"}]"#,
        )
        .unwrap();
        // 新库：应用富化过同一账号（id 变了、多了 needs_relogin）
        std::fs::write(
            target.join("accounts.json"),
            r#"[{"uid":"u1","id":"new-id","email":"a@b.c","needs_relogin":true}]"#,
        )
        .unwrap();

        let added = merge_accounts_file(
            &legacy.join("accounts.json"),
            &target.join("accounts.json"),
        );
        assert_eq!(added, 0, "同一 (区域, uid) 不应被重复并入");

        let merged: Vec<serde_json::Value> = serde_json::from_str(
            &std::fs::read_to_string(target.join("accounts.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(merged.len(), 1, "不得出现重复账号，实际 {} 条", merged.len());
        // 富化后的字段必须保留（新库条目优先）
        assert_eq!(merged[0]["needs_relogin"], true);
        assert_eq!(merged[0]["id"], "new-id");
        let _ = std::fs::remove_dir_all(&base);
    }

    #[test]
    fn 跨区域的同名_uid_视为不同账号() {
        let (base, legacy, target) = pair("region");
        std::fs::create_dir_all(&legacy).unwrap();
        std::fs::create_dir_all(&target).unwrap();
        std::fs::write(
            legacy.join("accounts.json"),
            r#"[{"uid":"same","domain":"www.workbuddy.ai"}]"#,
        )
        .unwrap();
        std::fs::write(
            target.join("accounts.json"),
            r#"[{"uid":"same","domain":"www.workbuddy.cn"}]"#,
        )
        .unwrap();

        let added = merge_accounts_file(
            &legacy.join("accounts.json"),
            &target.join("accounts.json"),
        );
        assert_eq!(added, 1, "两区域的身份命名空间相互独立，不得互相覆盖");
        let _ = std::fs::remove_dir_all(&base);
    }

    // ===================================================================
    // 「合并而非跳过」的回归测试（2026-09-16 数据丢失事故）
    // ===================================================================

    /// 读一个 JSON 文件。
    fn read_json_file(path: &Path) -> serde_json::Value {
        serde_json::from_str(&std::fs::read_to_string(path).unwrap()).unwrap()
    }

    /// 递归断言**只增不减**：合并结果的每个计数叶子的**每个数值字段**都不小于
    /// 任何一侧，且任一侧有的键在合并结果里都还在。
    ///
    /// 这是本次改动最核心的不变量 —— 用户已经在旧数据上损失过一次，
    /// 合并逻辑绝不允许再丢任何一侧的数据。
    ///
    /// 遍历的是实际出现的键（而非 `COUNTER_FIELDS`），所以上游将来新增的
    /// 计数维度也会被这条断言覆盖。
    fn assert_not_decreased(merged: &serde_json::Value, sides: &[&serde_json::Value], ctx: &str) {
        if is_counter_leaf(merged) {
            let fields: Vec<&String> = merged.as_object().unwrap().keys().collect();
            for field in fields {
                let m = merged.get(field).and_then(json_as_i64).unwrap_or(0);
                for (i, side) in sides.iter().enumerate() {
                    let v = side.get(field).and_then(json_as_i64).unwrap_or(0);
                    assert!(
                        m >= v,
                        "{ctx}: 字段 {field} 合并后 {m} < 第 {i} 侧 {v} —— 丢了数据"
                    );
                }
            }
            return;
        }
        let Some(map) = merged.as_object() else { return };
        // 任一侧有的键，合并结果必须还在
        for (i, side) in sides.iter().enumerate() {
            if let Some(sm) = side.as_object() {
                for key in sm.keys() {
                    assert!(
                        map.contains_key(key),
                        "{ctx}: 第 {i} 侧的键 {key} 在合并结果里消失了"
                    );
                }
            }
        }
        for (key, child) in map {
            let subs: Vec<&serde_json::Value> = sides.iter().filter_map(|s| s.get(key)).collect();
            if subs.is_empty() {
                continue;
            }
            assert_not_decreased(child, &subs, &format!("{ctx}/{key}"));
        }
    }

    /// 把两份 usage.json 放进旧/新目录并跑完整迁移，返回合并后的 JSON。
    fn migrate_with_usage(tag: &str, legacy_json: &serde_json::Value, target_json: &serde_json::Value) -> (PathBuf, serde_json::Value) {
        let (base, legacy, target) = pair(tag);
        let rel = usage_file_rel_path();
        for root in [&legacy, &target] {
            let dir = root.join(rel.parent().unwrap());
            std::fs::create_dir_all(&dir).unwrap();
        }
        std::fs::write(
            legacy.join(&rel),
            serde_json::to_string_pretty(legacy_json).unwrap(),
        )
        .unwrap();
        std::fs::write(
            target.join(&rel),
            serde_json::to_string_pretty(target_json).unwrap(),
        )
        .unwrap();

        let outcome = migrate_from_to(&legacy, &target);
        assert!(outcome.migrated(), "迁移必须完成：{outcome:?}");
        let merged = read_json_file(&target.join(&rel));
        (base, merged)
    }

    /// 计数器文件的一天的计量（贴近 go-gateway `usage.Counters` 的真实形状）。
    fn counters(input: i64, output: i64, cache_read: i64, records: i64) -> serde_json::Value {
        serde_json::json!({
            "input": input,
            "output": output,
            "cacheRead": cache_read,
            "cacheWrite": 0,
            "records": records,
        })
    }

    /// **核心回归（对应所有者实测事故）**：两侧 `usage.json` 同源分叉后各自累加，
    /// 合并后每个维度都必须 ≥ 两侧，并且逐日期取到的是两侧的较大值。
    ///
    /// 数字取自所有者机器上两份真实文件里 2026-09-16 那一天的分叉：
    /// 新侧（`.wb-switch` 的旧快照）input 仅 292,798,415 / records 669，
    /// 旧侧（`.ai-gateway`）有 1,809,167,437 / 5,619。旧实现在这里整文件
    /// `continue`，旧侧那一天的增量全部丢失。
    #[test]
    fn 计数器文件合并后每个维度都不小于两侧() {
        let legacy_json = serde_json::json!({
            "version": 1,
            "savedAt": "2026-09-16T21:48:17+08:00",
            "days": {
                "2026-09-13": counters(62_323_745, 180_000, 60_000_000, 110),
                "2026-09-16": counters(1_809_167_437, 5_431_731, 1_745_539_712, 5_619),
            },
        });
        let target_json = serde_json::json!({
            "version": 1,
            "savedAt": "2026-09-16T22:34:55+08:00",
            "days": {
                "2026-09-13": counters(62_323_745, 180_000, 60_000_000, 110),
                "2026-09-16": counters(292_798_415, 800_000, 275_177_216, 669),
                // 新侧独有的一天：合并后必须保留（只增不减的另一半）
                "2026-09-17": counters(7_777, 11, 22, 1),
            },
        });

        let (base, merged) = migrate_with_usage("counter-max", &legacy_json, &target_json);

        // 只增不减（逐叶子、逐键）
        assert_not_decreased(&merged, &[&legacy_json, &target_json], "usage");

        // 逐日期取到两侧的较大值
        let days = &merged["days"];
        assert_eq!(
            days["2026-09-16"]["input"].as_i64().unwrap(),
            1_809_167_437,
            "分叉那天必须取旧侧（更大）的值，这正是被丢掉的那 15.2 亿"
        );
        assert_eq!(days["2026-09-16"]["records"].as_i64().unwrap(), 5_619);
        assert_eq!(
            days["2026-09-13"]["input"].as_i64().unwrap(),
            62_323_745,
            "两侧一致的那天不应被改动"
        );
        assert_eq!(
            days["2026-09-17"]["records"].as_i64().unwrap(),
            1,
            "新侧独有的一天必须保留"
        );

        // 「取 max」绝不能退化成「相加」：分叉那天两侧都不是 0，相加会翻倍虚增
        assert_ne!(
            days["2026-09-16"]["input"].as_i64().unwrap(),
            1_809_167_437 + 292_798_415,
            "不得把两侧相加（重叠部分会算两遍）"
        );

        // 元信息保留目标侧
        assert_eq!(merged["savedAt"], target_json["savedAt"]);

        let _ = std::fs::remove_dir_all(&base);
    }

    /// 幂等：对同一个目标重复合并，结果必须逐字节不变。
    ///
    /// 这条很重要：迁移标记写失败时下次启动会**重跑**整个迁移，若合并不幂等，
    /// 重跑就会把数字放大（例如误用相加）。
    #[test]
    fn 计数器合并是幂等的() {
        let legacy_json = serde_json::json!({
            "version": 1,
            "days": { "2026-09-16": counters(1_809_167_437, 5_431_731, 1_745_539_712, 5_619) },
        });
        let target_json = serde_json::json!({
            "version": 1,
            "days": { "2026-09-16": counters(292_798_415, 800_000, 275_177_216, 669) },
        });

        let (base, legacy, target) = pair("counter-idem");
        let rel = usage_file_rel_path();
        for root in [&legacy, &target] {
            std::fs::create_dir_all(root.join(rel.parent().unwrap())).unwrap();
        }
        std::fs::write(
            legacy.join(&rel),
            serde_json::to_string_pretty(&legacy_json).unwrap(),
        )
        .unwrap();
        std::fs::write(
            target.join(&rel),
            serde_json::to_string_pretty(&target_json).unwrap(),
        )
        .unwrap();

        // 第一次合并
        assert!(merge_counter_file(&legacy.join(&rel), &target.join(&rel)));
        let first = std::fs::read_to_string(target.join(&rel)).unwrap();

        // 再来一次（模拟标记写失败后的重跑）
        assert!(
            !merge_counter_file(&legacy.join(&rel), &target.join(&rel)),
            "第二次不应再产生改动（幂等）"
        );
        let second = std::fs::read_to_string(target.join(&rel)).unwrap();
        assert_eq!(first, second, "重复合并结果必须逐字节一致");

        // 数字不能被放大
        let merged = read_json_file(&target.join(&rel));
        assert_eq!(
            merged["days"]["2026-09-16"]["input"].as_i64().unwrap(),
            1_809_167_437
        );

        let _ = std::fs::remove_dir_all(&base);
    }

    /// 配置合并：目标缺的键要补回来，目标已有的值**不能**被覆盖。
    ///
    /// 实测丢的是 `allowed_model`（单一模型锁定）与 `manual_uids` 两个整键——
    /// 它们只在 `.ai-gateway` 里有，旧实现因为「文件已存在」把整个文件跳过了。
    #[test]
    fn 配置合并补回缺失键且不覆盖目标已有值() {
        let (base, legacy, target) = pair("cfg-merge");
        let rel = gateway_config_rel_path();
        std::fs::create_dir_all(legacy.join(rel.parent().unwrap())).unwrap();
        std::fs::create_dir_all(target.join(rel.parent().unwrap())).unwrap();

        let legacy_json = serde_json::json!({
            "enabled": true,
            "mode": "rotation",
            "port": 7864,
            // 目标侧缺失的两个键（实测丢的就是这类）
            "allowed_model": "被封装的模型标识",
            "manual_uids": ["u1", "u2"],
            "last_status": "started",
        });
        let target_json = serde_json::json!({
            "enabled": true,
            // 目标侧的值是用户当前在用的，更权威：必须保留
            "mode": "balance",
            "port": 9000,
        });
        std::fs::write(
            legacy.join(&rel),
            serde_json::to_string_pretty(&legacy_json).unwrap(),
        )
        .unwrap();
        std::fs::write(
            target.join(&rel),
            serde_json::to_string_pretty(&target_json).unwrap(),
        )
        .unwrap();

        assert!(migrate_from_to(&legacy, &target).migrated());
        let merged = read_json_file(&target.join(&rel));

        // 补回缺失键
        assert_eq!(merged["allowed_model"], "被封装的模型标识");
        assert_eq!(merged["manual_uids"], serde_json::json!(["u1", "u2"]));
        assert_eq!(
            merged["last_status"], "started",
            "目标缺的键也要补（否则状态字段丢失）"
        );
        // 不覆盖已有值
        assert_eq!(merged["mode"], "balance", "目标侧已有的值不得被旧值覆盖");
        assert_eq!(merged["port"].as_i64().unwrap(), 9000, "同上");
        // 目标原本就有的键一个都不能少
        assert_eq!(merged["enabled"], true);

        let _ = std::fs::remove_dir_all(&base);
    }

    /// 目录已存在于目标时仍必须**递归下探**。
    ///
    /// 这是旧实现更隐蔽的一面：`if to.exists() { continue; }` 发生在
    /// `from.is_dir()` 判断**之前**，所以目标只要有 `gateway/` 目录，它下面的
    /// `gateway_data/usage.json`、`gateway_config.json` 一个都搬不过来 ——
    /// 「跳过」实际是整个子树级的。
    #[test]
    fn 目标已有目录时仍递归下探并拷贝其下文件() {
        let (base, legacy, target) = pair("subtree");
        // 目标已有 gateway/ 目录，且里面有配置文件（没有 gateway_data/）
        std::fs::create_dir_all(target.join("gateway")).unwrap();
        std::fs::write(target.join("gateway").join("gateway_config.json"), "{}").unwrap();

        // 旧侧：同目录下多出一个全新的子目录 + 文件
        let rel = usage_file_rel_path();
        std::fs::create_dir_all(legacy.join(rel.parent().unwrap())).unwrap();
        std::fs::write(legacy.join(&rel), r#"{"version":1,"days":{}}"#).unwrap();
        std::fs::write(legacy.join("gateway").join("gateway_config.json"), "{}").unwrap();

        assert!(migrate_from_to(&legacy, &target).migrated());
        assert!(
            target.join(&rel).exists(),
            "目标已有 gateway/ 目录时，其下缺失的 usage.json 仍必须被搬过来"
        );

        let _ = std::fs::remove_dir_all(&base);
    }

    /// 通用文件保持「已存在则不覆盖」——这是**有意识**的保守选择。
    ///
    /// 无法判断合并语义的文件（用户日志、备份、二进制等）猜错就是写坏数据，
    /// 因此维持原行为；与初版的区别是这不再是「顺手跳过」而是分派结果。
    #[test]
    fn 通用文件已存在时保持不覆盖() {
        let (base, legacy, target) = pair("opaque");
        std::fs::create_dir_all(&legacy).unwrap();
        std::fs::create_dir_all(&target).unwrap();
        std::fs::write(legacy.join("auto_checkin_logs.json"), r#"{"legacy":true}"#).unwrap();
        std::fs::write(target.join("auto_checkin_logs.json"), r#"{"target":true}"#).unwrap();

        assert!(migrate_from_to(&legacy, &target).migrated());
        let kept = std::fs::read_to_string(target.join("auto_checkin_logs.json")).unwrap();
        assert!(
            kept.contains("target"),
            "通用文件不得被旧侧覆盖，实际内容：{kept}"
        );

        let _ = std::fs::remove_dir_all(&base);
    }

    /// 只有一侧存在时：旧侧有、新侧无 → 正常拷贝；新侧有、旧侧无 → 不动。
    #[test]
    fn 只有一侧存在的文件按原语义处理() {
        let (base, legacy, target) = pair("one-side");
        std::fs::create_dir_all(&legacy).unwrap();
        std::fs::create_dir_all(&target).unwrap();
        std::fs::write(legacy.join("only-legacy.txt"), "L").unwrap();
        std::fs::write(target.join("only-target.txt"), "T").unwrap();

        assert!(migrate_from_to(&legacy, &target).migrated());
        assert_eq!(
            std::fs::read_to_string(target.join("only-legacy.txt")).unwrap(),
            "L",
            "旧侧独有文件必须被拷贝"
        );
        assert_eq!(
            std::fs::read_to_string(target.join("only-target.txt")).unwrap(),
            "T",
            "新侧独有文件必须保持原样"
        );

        let _ = std::fs::remove_dir_all(&base);
    }

    /// 任一侧 JSON 损坏 → 绝不写坏目标文件，且**不阻断迁移**。
    ///
    /// 迁移失败比不合并严重得多（用户连程序都起不来），所以这里的取舍是
    /// 「宁可少合并」——保留目标原样、继续把别的文件搬完。
    #[test]
    fn 计数器文件损坏时不写坏目标且不阻断迁移() {
        for (tag, legacy_bad) in [("bad-legacy", true), ("bad-target", false)] {
            let (base, legacy, target) = pair(tag);
            let rel = usage_file_rel_path();
            for root in [&legacy, &target] {
                std::fs::create_dir_all(root.join(rel.parent().unwrap())).unwrap();
            }
            let good = serde_json::to_string_pretty(&serde_json::json!({
                "version": 1,
                "days": { "2026-09-16": counters(1_809_167_437, 5_431_731, 1_745_539_712, 5_619) },
            }))
            .unwrap();
            // 一侧写损坏内容，另一侧写合法内容
            if legacy_bad {
                std::fs::write(legacy.join(&rel), "{ 这不是合法 JSON").unwrap();
                std::fs::write(target.join(&rel), &good).unwrap();
            } else {
                std::fs::write(legacy.join(&rel), &good).unwrap();
                std::fs::write(target.join(&rel), "{ 这不是合法 JSON").unwrap();
            }
            // 另放一个文件，用来证明迁移没有因为合并失败而中断
            std::fs::write(legacy.join("witness.txt"), "w").unwrap();

            let outcome = migrate_from_to(&legacy, &target);
            assert!(
                outcome.migrated(),
                "[{tag}] 单个文件合并失败不得阻断整体迁移：{outcome:?}"
            );
            assert!(
                target.join("witness.txt").exists(),
                "[{tag}] 迁移必须继续完成其余文件"
            );

            // 损坏的那一侧内容不得被写进目标
            let after = std::fs::read_to_string(target.join(&rel)).unwrap();
            if legacy_bad {
                assert_eq!(after, good, "[{tag}] 旧侧损坏时目标必须保持原样");
                let _ = serde_json::from_str::<serde_json::Value>(&after)
                    .expect("目标必须仍是合法 JSON");
            } else {
                assert_eq!(
                    after, "{ 这不是合法 JSON",
                    "[{tag}] 目标侧本来就损坏时应原样保留，不得被改写或写坏"
                );
            }

            let _ = std::fs::remove_dir_all(&base);
        }
    }

    /// 配置 / 计数器文件结构类型不一致时不得写坏（如一侧被人手工改成了数组）。
    #[test]
    fn 结构类型不一致时不写坏文件() {
        let (base, legacy, target) = pair("shape");
        let rel = usage_file_rel_path();
        for root in [&legacy, &target] {
            std::fs::create_dir_all(root.join(rel.parent().unwrap())).unwrap();
        }
        // 目标侧 days 是对象，旧侧是数组 —— 结构冲突
        std::fs::write(
            target.join(&rel),
            r#"{"version":1,"days":{"2026-09-16":{"input":5,"output":1,"cacheRead":0,"cacheWrite":0,"records":1}}}"#,
        )
        .unwrap();
        std::fs::write(
            legacy.join(&rel),
            r#"{"version":1,"days":[{"input":999}]}"#,
        )
        .unwrap();

        let before = std::fs::read_to_string(target.join(&rel)).unwrap();
        assert!(!merge_counter_file(&legacy.join(&rel), &target.join(&rel)));
        let after = std::fs::read_to_string(target.join(&rel)).unwrap();
        assert_eq!(before, after, "结构冲突时必须原样保留目标文件");
        // 且仍是合法 JSON
        let parsed: serde_json::Value = serde_json::from_str(&after).expect("不得写坏");
        // 计数对象里不得被塞进日期键（那会把计数叶子变成半损坏结构）
        let leaf = &parsed["days"]["2026-09-16"];
        let leaf_keys: Vec<&String> = leaf.as_object().expect("叶子仍是对象").keys().collect();
        for key in leaf_keys {
            assert!(
                COUNTER_FIELDS.contains(&key.as_str()),
                "计数对象里出现了非计数字段 {key}，说明结构被写坏"
            );
        }

        let _ = std::fs::remove_dir_all(&base);
    }

    /// 计数器里出现**未知字段**（上游将来新增的维度）时也必须取 max，不得漏掉。
    ///
    /// 写死字段名会让新维度被静默丢弃 —— 那正是「只增不减」要禁止的丢失。
    #[test]
    fn 计数器未知字段也参与取max() {
        let (base, legacy, target) = pair("counter-unknown");
        let rel = usage_file_rel_path();
        for root in [&legacy, &target] {
            std::fs::create_dir_all(root.join(rel.parent().unwrap())).unwrap();
        }
        // 旧侧多一个上游新增的维度 "reasoning"，且数值更大
        std::fs::write(
            legacy.join(&rel),
            r#"{"version":1,"days":{"2026-09-16":{"input":900,"output":1,"cacheRead":0,"cacheWrite":0,"records":5,"reasoning":777}}}"#,
        )
        .unwrap();
        std::fs::write(
            target.join(&rel),
            r#"{"version":1,"days":{"2026-09-16":{"input":100,"output":1,"cacheRead":0,"cacheWrite":0,"records":2,"reasoning":3}}}"#,
        )
        .unwrap();

        assert!(merge_counter_file(&legacy.join(&rel), &target.join(&rel)));
        let merged = read_json_file(&target.join(&rel));
        let leaf = &merged["days"]["2026-09-16"];
        assert_eq!(leaf["input"].as_i64().unwrap(), 900);
        assert_eq!(
            leaf["reasoning"].as_i64().unwrap(),
            777,
            "未知字段也必须取 max，不得被漏掉"
        );
        // 目标独有的未知字段不能被删
        assert_eq!(leaf["records"].as_i64().unwrap(), 5);

        let _ = std::fs::remove_dir_all(&base);
    }

    /// 路径分派：三个特殊文件走各自的合并策略，其余一律 Opaque。
    #[test]
    fn 文件类型分派正确() {
        assert_eq!(classify_entry("accounts.json"), FileKind::Accounts);
        assert_eq!(
            classify_entry("gateway\\gateway_data\\usage.json"),
            FileKind::Counter,
            "Windows 分隔符也必须识别"
        );
        assert_eq!(
            classify_entry("gateway/gateway_data/usage.json"),
            FileKind::Counter
        );
        assert_eq!(
            classify_entry("gateway\\gateway_config.json"),
            FileKind::ConfigObject
        );
        // 非顶层同名文件不参与（避免误伤用户子目录里的同名文件）
        assert_eq!(classify_entry("backups/accounts.json"), FileKind::Opaque);
        assert_eq!(
            classify_entry("gateway/gateway_data/state.json"),
            FileKind::Opaque,
            "账号池 state.json 语义未知，不合并"
        );
        assert_eq!(classify_entry("usage.json"), FileKind::Opaque);
    }

    /// 账号库合并语义**不得**被本次改动改坏：仍是 `(区域, uid)` 去重且保留目标侧。
    ///
    /// 完整走一遍 `migrate_from_to`（而不只是直接调 `merge_accounts_file`），
    /// 确认新加的分类逻辑没有把账号库的合并抢走或覆盖掉。
    #[test]
    fn 账号库经完整迁移后仍是区域加uid去重且保留目标侧() {
        let (base, legacy, target) = pair("acct-e2e");
        std::fs::create_dir_all(&legacy).unwrap();
        std::fs::create_dir_all(&target).unwrap();
        std::fs::write(
            legacy.join("accounts.json"),
            r#"[{"uid":"a","domain":"www.workbuddy.cn","nickname":"旧名"},
                {"uid":"b","domain":"www.workbuddy.cn"},
                {"uid":"a","domain":"www.workbuddy.ai","nickname":"国际版同名"}]"#,
        )
        .unwrap();
        std::fs::write(
            target.join("accounts.json"),
            r#"[{"uid":"a","domain":"www.workbuddy.cn","nickname":"新名"}]"#,
        )
        .unwrap();

        assert!(migrate_from_to(&legacy, &target).migrated());
        let merged: Vec<serde_json::Value> =
            serde_json::from_str(&std::fs::read_to_string(target.join("accounts.json")).unwrap())
                .unwrap();

        assert_eq!(merged.len(), 3, "cn 的 a、b 与国际版的 a 共 3 条，实际 {}", merged.len());
        let cn_a = merged
            .iter()
            .find(|x| x["uid"] == "a" && x["domain"] == "www.workbuddy.cn")
            .expect("cn 的 a 必须存在");
        assert_eq!(cn_a["nickname"], "新名", "目标侧条目不得被旧值覆盖");
        assert!(
            merged
                .iter()
                .any(|x| x["uid"] == "a" && x["domain"] == "www.workbuddy.ai"),
            "跨区域同名 uid 必须视为不同账号"
        );
        assert!(merged.iter().any(|x| x["uid"] == "b"), "新账号必须并入");

        let _ = std::fs::remove_dir_all(&base);
    }

    /// **真实数据形状**：把所有者真实的两份文件（副本）跑一遍完整迁移，
    /// 断言只增不减、且按日期取到两侧较大值。
    ///
    /// 需要环境变量 `AI_GATEWAY_MIGRATE_FIXTURE_DIR` 指向一个目录，其下形如：
    /// ```text
    /// <FIXTURE>/legacy/.ai-gateway/…   （更名后那一代，含更新更全的数据）
    /// <FIXTURE>/target/.wb-switch/…    （目标侧，可能是 1.0.0 时代的旧快照）
    /// ```
    /// 未设置时跳过（CI 上没有这份数据，也不该有 —— 仓库是公开的）。
    ///
    /// 用的是**副本**：先把 fixture 拷进临时目录再迁移，绝不改动来源文件，
    /// 更不碰 `~/.wb-switch` / `~/.ai-gateway`。
    #[test]
    fn 真实数据副本上合并且只增不减() {
        let Ok(fixture) = std::env::var("AI_GATEWAY_MIGRATE_FIXTURE_DIR") else {
            eprintln!("跳过：未设置 AI_GATEWAY_MIGRATE_FIXTURE_DIR");
            return;
        };
        let fixture = PathBuf::from(fixture);
        let src_legacy = fixture.join("legacy").join(".ai-gateway");
        let src_target = fixture.join("target").join(".wb-switch");
        if !src_legacy.is_dir() || !src_target.is_dir() {
            eprintln!("跳过：fixture 目录结构不符合预期");
            return;
        }

        // 拷进临时目录再迁移 —— 只读地对待来源数据
        let (base, legacy, target) = pair("real");
        copy_dir_merged(&src_legacy, &legacy).unwrap();
        copy_dir_merged(&src_target, &target).unwrap();

        let rel_usage = usage_file_rel_path();
        let rel_cfg = gateway_config_rel_path();
        let legacy_usage = read_json_file(&src_legacy.join(&rel_usage));
        let target_usage_before = read_json_file(&src_target.join(&rel_usage));
        let legacy_cfg = read_json_file(&src_legacy.join(&rel_cfg));
        let target_cfg_before = read_json_file(&src_target.join(&rel_cfg));

        let outcome = migrate_from_to(&legacy, &target);
        assert!(outcome.migrated(), "真实数据迁移必须成功：{outcome:?}");

        // 1) 只增不减
        let merged_usage = read_json_file(&target.join(&rel_usage));
        assert_not_decreased(
            &merged_usage,
            &[&legacy_usage, &target_usage_before],
            "real-usage",
        );

        // 2) 逐日期取两侧较大值
        if let (Some(l_days), Some(t_days), Some(m_days)) = (
            legacy_usage["days"].as_object(),
            target_usage_before["days"].as_object(),
            merged_usage["days"].as_object(),
        ) {
            for (day, l_val) in l_days {
                let Some(m_val) = m_days.get(day) else {
                    panic!("日期 {day} 在合并结果里消失了");
                };
                let l_in = l_val["input"].as_i64().unwrap_or(0);
                let t_in = t_days.get(day).and_then(|d| d["input"].as_i64()).unwrap_or(0);
                assert_eq!(
                    m_val["input"].as_i64().unwrap_or(0),
                    l_in.max(t_in),
                    "{day} 的 input 必须是两侧的较大值"
                );
            }
            // 汇总口径：合并后必须 ≥ 两侧
            for field in COUNTER_FIELDS {
                let m: i64 = m_days.values().map(|d| d[field].as_i64().unwrap_or(0)).sum();
                let l: i64 = l_days.values().map(|d| d[field].as_i64().unwrap_or(0)).sum();
                let t: i64 = t_days.values().map(|d| d[field].as_i64().unwrap_or(0)).sum();
                assert!(m >= l && m >= t, "字段 {field}: 合并 {m} 必须 ≥ 旧 {l} / 新 {t}");
                eprintln!("[real] days.{field}: 旧={l} 新={t} 合并={m}");
            }
        }

        // 3) 配置：缺失键补回、已有值不被覆盖
        let merged_cfg = read_json_file(&target.join(&rel_cfg));
        if let (Some(l_cfg), Some(t_cfg), Some(m_cfg)) = (
            legacy_cfg.as_object(),
            target_cfg_before.as_object(),
            merged_cfg.as_object(),
        ) {
            for key in l_cfg.keys() {
                assert!(m_cfg.contains_key(key), "配置键 {key} 被丢掉了");
            }
            for (key, t_val) in t_cfg {
                assert_eq!(
                    m_cfg.get(key),
                    Some(t_val),
                    "配置键 {key} 的值被旧侧覆盖了（目标侧更权威）"
                );
            }
            eprintln!(
                "[real] gateway_config: 旧侧 {} 键 / 新侧 {} 键 / 合并 {} 键",
                l_cfg.len(),
                t_cfg.len(),
                m_cfg.len()
            );
        }

        let _ = std::fs::remove_dir_all(&base);
    }
}
