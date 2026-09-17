//! 本地积分观察快照与统计投影。
//!
//! WorkBuddy 只返回当前资源余额，没有可复用的历史账单序列。因此这里把
//! 成功查询到的余额保存为本地观察值，再用相邻快照的正向下降量推导“观察到
//! 的消耗”。首次快照和余额增加只建立新的基线，不产生负数消耗。

use chrono::{Datelike, Duration as ChronoDuration, Local, NaiveDate, TimeZone};
use serde_json::{json, Value};
use std::cmp::Reverse;
use std::collections::HashMap;
use std::sync::Mutex;

use crate::modules::account::{account_display_name, load_accounts};
use crate::modules::config::{
    atomic_write, credit_usage_snapshots_file, load_checkin_logs, now_ms, record_retention_days,
    store_dir,
};
use crate::modules::official_usage;

pub const CREDIT_SNAPSHOT_RETENTION_DAYS: i64 = 90;
pub const CREDIT_SNAPSHOT_MAX_RECORDS: usize = 5_000;
pub const CREDIT_SNAPSHOT_DEDUPE_WINDOW_MS: i64 = 5 * 60 * 1000;
const CREDIT_STATS_MAX_EVENTS: usize = 200;

static SNAPSHOT_WRITE_LOCK: Mutex<()> = Mutex::new(());

#[derive(Clone, Debug)]
struct Snapshot {
    ts: i64,
    account_id: String,
    account_name: String,
    total: f64,
    remaining: f64,
    /// 包子集：`packageCode → remaining`。空 = 该快照没有包级明细（老数据）。
    ///
    /// 为什么必须存：聚合值（total/remaining）分不清「某个包没读到」与「余额真的少了」。
    /// 实测（所有者真实数据）上游会偶发返回**不完整的包列表** ——
    /// 例如某次只返回 1 个 30 的包，聚合 total 就从 380 掉到 30，
    /// 被当成「积分消耗 -350」记了一条；下次读到完整列表又记「+350 增长」，
    /// 而余额其实一整天都是 380（幻影配对）。
    ///
    /// 有了包级明细才能按**包的身份**判断：某 packageCode 这次不在、下次又回来
    /// ⇒ 读数失败（包不会凭空消失再回来），不是消耗。
    packages: std::collections::BTreeMap<String, f64>,
    /// 写入这条快照时，它是否已被判为「不可信读数」。
    ///
    /// 为什么要把判定结果存下来（而不是下次再算）：区分「抖动后的恢复」与
    /// 「真实消耗后的重新发放」靠的是**中间那次的证据** ——
    ///   · 抖动：`p380` 整个不在（包消失）
    ///   · 真消耗：`p380` 在，只是 remaining 变成 30
    /// 两次读数当下就能分辨，但事后只看数值（都回到 380）无法区分。
    /// 故把「上次是可疑读数」记进快照，回升那次据此判定为**恢复**而非发放。
    suspect: bool,
}

#[derive(Clone, Debug)]
struct UsageEvent {
    ts: i64,
    date: String,
    account_id: String,
    amount: f64,
}

#[derive(Clone, Debug)]
struct CheckinEvent {
    ts: i64,
    date: String,
    account_id: Option<String>,
    account_name: String,
    result: String,
    error: Option<String>,
}

fn non_empty_string(value: Option<&Value>) -> Option<String> {
    value
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(String::from)
}

fn number(value: Option<&Value>) -> Option<f64> {
    match value {
        Some(Value::Number(value)) => value.as_f64(),
        Some(Value::String(value)) => value.trim().parse::<f64>().ok(),
        _ => None,
    }
}

fn snapshot_from_value(value: &Value) -> Option<Snapshot> {
    let account_id = non_empty_string(value.get("accountId"))?;
    let ts = value.get("ts").and_then(Value::as_i64)?;
    let total = number(value.get("total"))?.max(0.0);
    let remaining = number(value.get("remaining"))?.max(0.0);
    // 包级明细可选：老快照没有这个字段，解析成空 map（= 无判据，退回聚合值比较）。
    let packages = value
        .get("packages")
        .and_then(Value::as_object)
        .map(|map| {
            map.iter()
                .filter_map(|(k, v)| number(Some(v)).map(|n| (k.clone(), n)))
                .collect()
        })
        .unwrap_or_default();
    Some(Snapshot {
        ts,
        account_id,
        account_name: non_empty_string(value.get("accountName"))
            .unwrap_or_else(|| "unknown".to_string()),
        total,
        remaining,
        packages,
        // 老快照没有这个字段 → 视为可信（当时没有判据，不该倒推怀疑历史数据）。
        suspect: value.get("suspect").and_then(Value::as_bool).unwrap_or(false),
    })
}

fn snapshot_value(
    ts: i64,
    account_id: &str,
    account_name: &str,
    total: f64,
    remaining: f64,
) -> Value {
    json!({
        "ts": ts,
        "accountId": account_id,
        "accountName": account_name,
        "total": total.max(0.0),
        "remaining": remaining.max(0.0),
    })
}

/// 带包级明细的快照值。
///
/// 包级明细是本次修复的核心依据（见 `reading_trust`），因此新快照一律带上。
/// **包级为空时不写 `packages` 字段** —— 保持与老快照同形，
/// 避免下游把「空 map」误读成「这个账号一个包都没有」。
///
/// `suspect` 为 true 表示这次读数已被判为不完整：写进快照后，
/// 下一次回升时才能区分「抖动恢复」与「真实发放」（见 `reading_trust` 判据 3）。
/// false 时不写该字段，保持文件紧凑（绝大多数快照都是可信的）。
fn snapshot_value_with_packages(
    ts: i64,
    account_id: &str,
    account_name: &str,
    total: f64,
    remaining: f64,
    packages: &std::collections::BTreeMap<String, f64>,
    suspect: bool,
) -> Value {
    let mut value = snapshot_value(ts, account_id, account_name, total, remaining);
    if !packages.is_empty() {
        value["packages"] = json!(packages);
    }
    if suspect {
        value["suspect"] = json!(true);
    }
    value
}

/// 读取本地观察快照；文件缺失或损坏时返回空列表。
pub fn load_snapshots() -> Vec<Value> {
    let path = credit_usage_snapshots_file();
    if let Ok(text) = std::fs::read_to_string(path) {
        if let Ok(Value::Array(items)) = serde_json::from_str::<Value>(&text) {
            return items;
        }
    }
    vec![]
}

fn should_suppress_duplicate(
    snapshots: &[Value],
    account_id: &str,
    total: f64,
    remaining: f64,
    at_ms: i64,
) -> bool {
    let Some(latest) = snapshots
        .iter()
        .filter_map(snapshot_from_value)
        .filter(|snapshot| snapshot.account_id == account_id)
        .max_by_key(|snapshot| snapshot.ts)
    else {
        return false;
    };

    latest.ts <= at_ms
        && at_ms - latest.ts <= CREDIT_SNAPSHOT_DEDUPE_WINDOW_MS
        && (latest.total - total).abs() < f64::EPSILON
        && (latest.remaining - remaining).abs() < f64::EPSILON
}

fn normalize_snapshots(snapshots: &[Value], at_ms: i64) -> Vec<Value> {
    // 保留天数来自设置项（默认 60 天，设置页可调）
    let keep_days = record_retention_days();
    let cutoff = at_ms.saturating_sub(keep_days * 24 * 3600 * 1000);
    let mut kept: Vec<Value> = snapshots
        .iter()
        .filter_map(|value| {
            let snapshot = snapshot_from_value(value)?;
            (snapshot.ts >= cutoff && snapshot.ts <= at_ms).then_some(value.clone())
        })
        .collect();
    if kept.len() > CREDIT_SNAPSHOT_MAX_RECORDS {
        kept.drain(..kept.len() - CREDIT_SNAPSHOT_MAX_RECORDS);
    }
    kept
}

/// 相邻快照之间的积分变化，连同它的来源判据。
///
/// 为什么要把「变化量」和「来源」放在一起算：来源的判据是**容量（total）
/// 有没有跟着变**，而容量只存在于前后两个快照里。留在 `record_snapshot`
/// 里就地算，是唯一能同时看到三个数的位置。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CreditDelta {
    /// 余额变化（正为增长、负为消耗）。
    pub amount: i64,
    /// 容量变化（正为新增额度、负为额度回收/到期）。
    pub capacity: i64,
}

/// 判定一次积分变化的**来源类别**。
///
/// 重要：这里判定的是「余额是怎么动的」这一**可观测量**，不是「哪个任务发的奖励」。
///
/// 为什么不能给出任务名：上游资源接口
///（`resource_summary`，见 `credits.rs`）只返回容量 / 余额 / 到期时间三类数字，
/// 没有任何「这笔积分由哪个任务产生」的字段或账单流水。实测真实数据里
/// 33 条积分增长记录中只有 1 条能在同账号 ±30 分钟内找到任务记录 ——
/// 按时间邻近去「认领」来源，等于把 32 条无据可依的记录也贴上任务名，
/// 那正是**编造**。因此这里只如实记录可验证的判据。
///
/// 判据（全部来自实测真实快照，见交付报告）：
///   - 余额上升且容量同步上升 → `grant`：账号拿到了**新增额度**
///     （实测 158 次上升中 141 次容量与余额增量完全相等，其余差额是到账前
///     已被消耗的部分 —— 例如容量 +1650 而余额 +1620.73）。
///   - 余额下降且容量不变   → `consume`：纯消耗（实测 434 次，容量变化恒为 0）。
///   - 余额下降且容量同降   → `expire`：额度被回收/到期
///     （实测 5 次，例如某积分包容量 -100、余额 -79，即包内还剩 79 分就整包失效）。
///   - 其余                 → `adjust`：无法归入以上三类的调整。
///
/// 把 5000 条真实快照按相邻对回放，597 次变化全部落进前三类、0 次 adjust、
/// 0 次自相矛盾（余额涨却判成 consume/expire 之类）。
///
/// 注：`capacity` 为 0 而 `amount` 为正时归 `adjust` 而不是 `grant` ——
/// 「容量没变但余额涨了」意味着积分是**退回来**的（如失败调用返还），
/// 不是新增额度；把它说成 grant 会让用户以为额度包变多了。
pub fn classify_credit_source(amount: i64, capacity: i64) -> &'static str {
    use crate::modules::account_records::{
        CREDIT_SOURCE_ADJUST, CREDIT_SOURCE_CONSUME, CREDIT_SOURCE_EXPIRE, CREDIT_SOURCE_GRANT,
    };
    if amount > 0 && capacity > 0 && capacity >= amount {
        return CREDIT_SOURCE_GRANT;
    }
    if amount < 0 && capacity == 0 {
        return CREDIT_SOURCE_CONSUME;
    }
    if amount < 0 && capacity < 0 {
        return CREDIT_SOURCE_EXPIRE;
    }
    CREDIT_SOURCE_ADJUST
}

/// 一次积分变化对应的展示文案（标题 + 说明）。
///
/// 为什么标题要区分来源、而不是继续用「积分增长 / 积分消耗」两个词：
/// 那正是用户抱怨的现象 —— 记录里只写「积分增长 +100」，看不出这 100 是
/// 新增额度还是别的。标题里带上来源类别，用户一眼能分清性质；
/// `detail` 再补上**原始判据**（余额与容量各变了多少），
/// 让他能自己核对，而不是只能相信我们的结论。
///
/// detail 里刻意写出容量变化：这正是「为什么判成这一类」的证据。
/// 只给结论不给判据，用户无法区分「系统算错了」和「口径与我想的不同」。
fn credit_record_text(source: &str, delta: &CreditDelta) -> (&'static str, String) {
    use crate::modules::account_records::{
        CREDIT_SOURCE_ADJUST, CREDIT_SOURCE_CONSUME, CREDIT_SOURCE_EXPIRE, CREDIT_SOURCE_GRANT,
    };
    match source {
        CREDIT_SOURCE_GRANT => (
            "积分增长 · 额度发放",
            format!(
                "余额 +{}，额度容量 +{}（新增积分包到账）",
                delta.amount, delta.capacity
            ),
        ),
        CREDIT_SOURCE_CONSUME => (
            "积分消耗 · 调用扣减",
            format!("余额 {}，额度容量不变（纯消耗）", delta.amount),
        ),
        CREDIT_SOURCE_EXPIRE => (
            "积分减少 · 额度到期",
            format!(
                "余额 {}，额度容量 {}（积分包被回收，包内剩余一并失效）",
                delta.amount, delta.capacity
            ),
        ),
        CREDIT_SOURCE_ADJUST => (
            "积分调整",
            format!(
                "余额 {}，额度容量 {}（无法归入发放/消耗/到期）",
                delta.amount, delta.capacity
            ),
        ),
        // 理论上不可达：source 只由 classify_credit_source 产生。
        // 兜底而不 panic —— 记录是旁路观测数据，文案未知不该让主流程崩。
        other => (
            "积分变化",
            format!("余额 {}，来源 {}（未识别）", delta.amount, other),
        ),
    }
}

/// 计算相邻两次快照之间的积分变化；无变化时返回 `None`。
///
/// 从 `record_snapshot` 里抽出来是为了**可被单测直接覆盖**：
/// 原来的判据内联在写盘流程中（需要构造快照文件、锁、数据目录），
/// 想断言「容量同增才算 grant」就得跑一整套 IO。纯函数化之后，
/// 判据本身可以被逐分支钉死，写盘路径只剩下调用。
///
/// 用 `Option` 而不是返回 amount=0：调用方只在 `Some` 时才写记录，
/// 让「没有变化就不写」这件事由类型表达，而不是靠调用方记得判零。
pub fn credit_delta(prev_remaining: f64, prev_total: f64, remaining: f64, total: f64) -> Option<CreditDelta> {
    // 四舍五入到整数：积分通常是整数，浮点误差会造出 -0.0000001 这类噪音
    let amount = (remaining - prev_remaining).round() as i64;
    if amount == 0 {
        return None;
    }
    Some(CreditDelta {
        amount,
        capacity: (total - prev_total).round() as i64,
    })
}

/// 一次读数的可信度判定结果。
#[derive(Debug, PartialEq, Eq)]
enum ReadingTrust {
    /// 读数正常，可按 credit_delta 记为真实变化。
    Ok,
    /// 读数不完整（上游返回的包列表缺了东西），**不可据此记积分变化**。
    /// 携带人话原因，供 detail 使用。
    Incomplete(&'static str),
}

/// 判断本次读数是否可信 —— 用**包级证据**，不靠「数值大小像不像异常」猜。
///
/// 背景（所有者真实数据实测）：上游会偶发返回**不完整的包列表**，
/// 导致聚合 total/remaining 凭空下跌再涨回，记录里出现 `-350` / `+350` 的幻影配对，
/// 而余额其实一整天没变。上游**没有**「积分消耗明细」接口可查，
/// 所以只能靠包的身份来分辨「没读到」与「真的少了」。
///
/// 三条判据，任一成立即判为读数不完整：
///
/// 1. **包凭空消失**：上次存在、这次不见了的 `packageCode`。
///    包不会自己消失又回来 —— 真到期有 `expireAt` 为证，真消耗只减 `remaining` 不删包。
///    故「少包」直接说明这次没读到它们。
///    （只在**两侧都有包级明细**时可用；老快照没有明细则跳过这条。）
///
/// 2. **容量不应因消耗而减少**：`total`（额度容量）是各包的**总额度**，
///    消耗只减 `remaining`，**不会**让 `total` 变小。
///    所以 `total` 下降必然意味着**包集合变了**（少读到包 / 包被回收），而非用掉额度。
///    `total` 上升是正常的（新包到账）；`total` 不变也正常。
///
/// 3. **上一次是不可信读数，本次回升 ⇒ 是恢复而非发放**：
///    只看**紧邻**的上一次会漏掉回升那一侧 —— 序列 `380 → 30 → 380` 里，
///    第三次是「total 上升」，判据 2 放行；而它上一次（30）的包里没有 p380，
///    判据 1 也看不出「消失」。于是 `+350` 那一半照样被记下来。
///    故读 `prev.suspect`：上一次已被判可疑、且本次数值比它高 ⇒ 判定为恢复。
///
///    为什么用「上一次是否可疑」而不是「数值是否回到更早的值」：
///    后者无法区分抖动恢复与「真消耗后又发放」——两者数值都可能回到原处。
///    而这两者在**中间那次读数当下**就能分辨（包消失 = 抖动；包在但余额跌 = 真消耗），
///    所以把判定结果存进快照，回升时直接读，不做事后猜测。
///
/// 注意判据 2 **不依赖**包级明细，因此对老快照也生效 ——
/// 这正是本次幻影记录的直接原因：`total` 与 `remaining` 同时从 380 掉到 30。
///
/// 为什么不用「归零/暴跌幅度」当判据：消耗快时余额确实可能快速下降甚至归零，
/// 那会误杀真实消耗（所有者明确指出过这一点）。上面三条都是**结构性证据**，与幅度无关。
fn reading_trust(
    prev: &Snapshot,
    now_total: f64,
    now_remaining: f64,
    now_packages: &std::collections::BTreeMap<String, f64>,
) -> ReadingTrust {
    let dropped = now_remaining < prev.remaining - f64::EPSILON;

    // 判据 1：有包级明细时，看有没有包凭空消失。
    if !prev.packages.is_empty() {
        let vanished: Vec<&str> = prev
            .packages
            .keys()
            .filter(|code| !now_packages.contains_key(*code))
            .map(String::as_str)
            .collect();
        if !vanished.is_empty() {
            // 只有「余额也在跌」时才需要怀疑；余额不跌而包少了，说明只是没读到。
            // 若余额同时在跌，仍可能是真消耗 + 真到期，故只在**容量也跌**时定性。
            let capacity_dropped = now_total < prev.total - f64::EPSILON;
            if capacity_dropped || dropped {
                return ReadingTrust::Incomplete("上游本次返回的积分包列表不完整");
            }
        }
    }

    // 判据 2：total（额度容量）不应因消耗而减少。
    if now_total < prev.total - f64::EPSILON {
        return ReadingTrust::Incomplete("额度容量意外减少，上游本次返回的积分包列表不完整");
    }

    // 判据 3：上一次已判可疑、本次数值回升 ⇒ 这是**恢复**，不是发放。
    if prev.suspect && (now_total > prev.total + f64::EPSILON || !dropped) {
        return ReadingTrust::Incomplete("上一次为不完整读数，本次为恢复");
    }

    ReadingTrust::Ok
}

/// 写入一个成功的资源观察值。
///
/// 同一账号同一资源值在短时间内只保留一条；资源值发生变化时立即保留，
/// 这样余额下降可以归因到新快照。返回值表示本次是否实际写入。
///
/// `packages` 是本次读到的包子集（`packageCode → remaining`），
/// 用于分辨「包没读到」与「余额真的少了」（见 `reading_trust`）。
/// 传空 map 也能工作，只是失去判据 1（判据 2 仍生效）。
pub fn record_snapshot(
    account_id: &str,
    account_name: &str,
    total: f64,
    remaining: f64,
    packages: std::collections::BTreeMap<String, f64>,
) -> bool {
    let account_id = account_id.trim();
    if account_id.is_empty() {
        return false;
    }

    let at_ms = now_ms();
    let _guard = SNAPSHOT_WRITE_LOCK.lock().unwrap();
    let snapshots = load_snapshots();
    if should_suppress_duplicate(&snapshots, account_id, total, remaining, at_ms) {
        return false;
    }

    // 记一条积分变化事件（供单账号记录视图）。
    //
    // 为什么在这里记而不是在调用方：本函数已经拿到了「上一快照」与「本次余额」，
    // 差值就在这里最自然；调用方（签到、切换、巡检）各自算差值会口径不一。
    // 只在**余额确实变化**时记录，避免每 15 分钟的巡检刷出一堆 amount=0 的噪音。
    //
    // **但变化的前提是这次读数可信**：上游会偶发返回不完整的包列表，
    // 若不判可信度就会记出「-380 消耗」+「+380 发放」的幻影配对（见 reading_trust 注释）。
    // 本次读数是否可疑 —— 供写入快照（下一次回升时据此判定「恢复」而非「发放」）。
    let mut suspect = false;
    if let Some(prev) = snapshots
        .iter()
        .filter_map(snapshot_from_value)
        .filter(|s| s.account_id == account_id)
        .max_by_key(|s| s.ts)
    {
        match reading_trust(&prev, total, remaining, &packages) {
            ReadingTrust::Incomplete(why) => {
                suspect = true;
                // 读数不完整 → **不记积分变化**（记了就是幻影）。
                // 但仍打一行日志：这是上游行为异常，排查时需要线索，
                // 且不能静默 —— 否则「记录里少了一条」会被当成功能坏了。
                eprintln!(
                    "[积分统计] 跳过一次不可信读数（{why}）：账号 {} 上次 total={} remaining={}，本次 total={} remaining={}",
                    &account_id[..account_id.len().min(8)],
                    prev.total,
                    prev.remaining,
                    total,
                    remaining,
                );
            }
            ReadingTrust::Ok => {
                if let Some(delta) = credit_delta(prev.remaining, prev.total, remaining, total) {
                    let source = classify_credit_source(delta.amount, delta.capacity);
                    let (title, detail) = credit_record_text(source, &delta);
                    crate::modules::account_records::add_credit_record(
                        account_id,
                        account_name.trim(),
                        title,
                        delta.amount,
                        &detail,
                        Some(source),
                    );
                }
            }
        }
    }

    let mut kept = normalize_snapshots(&snapshots, at_ms);
    // 不可信读数**仍然写入快照**：它是「上游这次返回了什么」的事实，
    // 丢掉会让下一次比较失去基准（而且下一次可能才是完整的那次）。
    // 被抑制的只是「据此推断积分变化」这一步。
    kept.push(snapshot_value_with_packages(
        at_ms,
        account_id,
        account_name.trim(),
        total,
        remaining,
        &packages,
        suspect,
    ));
    if kept.len() > CREDIT_SNAPSHOT_MAX_RECORDS {
        kept.drain(..kept.len() - CREDIT_SNAPSHOT_MAX_RECORDS);
    }

    if let Err(error) = std::fs::create_dir_all(store_dir()).and_then(|_| {
        let content = serde_json::to_string_pretty(&kept).unwrap_or_default();
        atomic_write(&credit_usage_snapshots_file(), &content)
    }) {
        eprintln!("[积分统计] 保存积分快照失败: {error}");
        return false;
    }
    true
}

fn local_date(ts: i64) -> Option<NaiveDate> {
    Local
        .timestamp_millis_opt(ts)
        .single()
        .map(|date| date.date_naive())
}

fn local_date_string(ts: i64) -> Option<String> {
    local_date(ts).map(|date| date.format("%Y-%m-%d").to_string())
}

/// 生成逐日观察序列（daily_start..=today，无数据的天补 0）；无快照起点时返回空数组。
fn local_daily_series(
    daily: &HashMap<String, f64>,
    daily_start: Option<NaiveDate>,
    today: NaiveDate,
) -> Vec<Value> {
    let Some(start) = daily_start else {
        return Vec::new();
    };
    let mut points = Vec::new();
    let mut date = start;
    while date <= today {
        let key = date.format("%Y-%m-%d").to_string();
        points.push(json!({
            "date": key,
            "usage": daily.get(&key).copied().unwrap_or(0.0),
        }));
        date += ChronoDuration::days(1);
    }
    points
}

fn parse_checkin_event(value: &Value) -> Option<CheckinEvent> {
    let ts = value
        .get("ts")
        .and_then(Value::as_i64)
        .or_else(|| crate::modules::config::norm_ts(value.get("ts")))?;
    Some(CheckinEvent {
        ts,
        date: local_date_string(ts)?,
        account_id: non_empty_string(value.get("accountId")),
        account_name: non_empty_string(value.get("email")).unwrap_or_else(|| "unknown".to_string()),
        result: non_empty_string(value.get("result")).unwrap_or_else(|| "error".to_string()),
        error: non_empty_string(value.get("error")),
    })
}

fn parse_snapshots(values: &[Value], at_ms: i64) -> Vec<Snapshot> {
    // 保留天数来自设置项（与 normalize_snapshots 同一口径）
    let keep_days = record_retention_days();
    let cutoff = at_ms.saturating_sub(keep_days * 24 * 3600 * 1000);
    let mut snapshots: Vec<Snapshot> = values
        .iter()
        .filter_map(snapshot_from_value)
        .filter(|snapshot| snapshot.ts >= cutoff && snapshot.ts <= at_ms)
        .collect();
    snapshots.sort_by_key(|snapshot| snapshot.ts);
    snapshots
}

fn usage_in_windows(date: &str, today: NaiveDate) -> (bool, bool, bool) {
    let Some(date) = NaiveDate::parse_from_str(date, "%Y-%m-%d").ok() else {
        return (false, false, false);
    };
    let distance = (today - date).num_days();
    (
        distance == 0,
        (0..7).contains(&distance),
        date.year() == today.year() && date.month() == today.month(),
    )
}

fn add_account_name(
    account_ids: &mut Vec<String>,
    account_names: &mut HashMap<String, String>,
    account_id: String,
    account_name: String,
) {
    if !account_ids.contains(&account_id) {
        account_ids.push(account_id.clone());
    }
    let should_replace = account_names
        .get(&account_id)
        .map(|name| name == "unknown" && account_name != "unknown")
        .unwrap_or(true);
    if should_replace {
        account_names.insert(account_id, account_name);
    }
}

fn checkin_identity(event: &CheckinEvent) -> Option<String> {
    if let Some(account_id) = event.account_id.as_ref() {
        return Some(format!("account:{account_id}"));
    }
    (event.account_name != "unknown").then(|| format!("legacy:{}", event.account_name))
}

fn build_statistics(
    snapshot_values: &[Value],
    checkin_values: &[Value],
    accounts: &[Value],
    at_ms: i64,
) -> Value {
    let snapshots = parse_snapshots(snapshot_values, at_ms);
    let today = local_date(at_ms).unwrap_or_else(|| Local::now().date_naive());
    // 与签到日志的清理口径保持一致（同一设置项）
    let checkin_cutoff = at_ms.saturating_sub(record_retention_days() * 24 * 3600 * 1000);
    let checkins: Vec<CheckinEvent> = checkin_values
        .iter()
        .filter_map(parse_checkin_event)
        .filter(|event| event.ts >= checkin_cutoff && event.ts <= at_ms)
        .collect();

    let mut account_ids = Vec::new();
    let mut current_account_ids = Vec::new();
    let mut account_names = HashMap::new();
    for account in accounts {
        if let Some(id) = non_empty_string(account.get("id")) {
            if !current_account_ids.contains(&id) {
                current_account_ids.push(id.clone());
            }
            add_account_name(
                &mut account_ids,
                &mut account_names,
                id,
                account_display_name(account),
            );
        }
    }
    for snapshot in &snapshots {
        add_account_name(
            &mut account_ids,
            &mut account_names,
            snapshot.account_id.clone(),
            snapshot.account_name.clone(),
        );
    }
    for event in &checkins {
        if let Some(account_id) = &event.account_id {
            add_account_name(
                &mut account_ids,
                &mut account_names,
                account_id.clone(),
                event.account_name.clone(),
            );
        }
    }

    let mut by_account: HashMap<String, Vec<Snapshot>> = HashMap::new();
    for snapshot in snapshots {
        by_account
            .entry(snapshot.account_id.clone())
            .or_default()
            .push(snapshot);
    }
    let coverage_start_at = by_account
        .values()
        .flat_map(|snapshots| snapshots.iter().map(|snapshot| snapshot.ts))
        .min();

    let mut usage_events = Vec::new();
    let mut usage_totals: HashMap<String, (f64, f64, f64)> = HashMap::new();
    let mut daily_usage: HashMap<String, HashMap<String, f64>> = HashMap::new();
    let mut latest_snapshots = HashMap::new();
    for (account_id, mut snapshots) in by_account {
        snapshots.sort_by_key(|snapshot: &Snapshot| snapshot.ts);
        if let Some(latest) = snapshots.last() {
            latest_snapshots.insert(account_id.clone(), latest.clone());
        }
        for pair in snapshots.windows(2) {
            let previous = &pair[0];
            let current = &pair[1];
            let amount = previous.remaining - current.remaining;
            if amount <= f64::EPSILON {
                continue;
            }
            let Some(date) = local_date_string(current.ts) else {
                continue;
            };
            let entry = usage_totals.entry(account_id.clone()).or_default();
            let (today_usage, week_usage, month_usage) = usage_in_windows(&date, today);
            if today_usage {
                entry.0 += amount;
            }
            if week_usage {
                entry.1 += amount;
            }
            if month_usage {
                entry.2 += amount;
            }
            *daily_usage
                .entry(account_id.clone())
                .or_default()
                .entry(date.clone())
                .or_default() += amount;
            usage_events.push(UsageEvent {
                ts: current.ts,
                date,
                account_id: account_id.clone(),
                amount,
            });
        }
    }

    let mut checkin_today_latest: HashMap<String, (i64, String)> = HashMap::new();
    let mut today_success = 0;
    let mut today_already = 0;
    let mut today_failed = 0;
    for event in &checkins {
        if event.date != today.format("%Y-%m-%d").to_string() {
            continue;
        }
        match event.result.as_str() {
            "success" => today_success += 1,
            "already" => today_already += 1,
            _ => today_failed += 1,
        }
        if let Some(identity) = checkin_identity(event) {
            if checkin_today_latest
                .get(&identity)
                .map(|(ts, _)| *ts <= event.ts)
                .unwrap_or(true)
            {
                checkin_today_latest.insert(identity, (event.ts, event.result.clone()));
            }
        }
    }

    let today_checked_in_accounts = checkin_today_latest
        .values()
        .filter(|(_, result)| result == "success" || result == "already")
        .count();
    let today_key = today.format("%Y-%m-%d").to_string();

    let mut current_remaining = 0.0;
    let mut current_capacity = 0.0;
    for account_id in &current_account_ids {
        if let Some(snapshot) = latest_snapshots.get(account_id) {
            current_remaining += snapshot.remaining;
            current_capacity += snapshot.total;
        }
    }

    let mut last_checkins: HashMap<String, CheckinEvent> = HashMap::new();
    for event in &checkins {
        let Some(account_id) = &event.account_id else {
            continue;
        };
        if last_checkins
            .get(account_id)
            .map(|current: &CheckinEvent| current.ts <= event.ts)
            .unwrap_or(true)
        {
            last_checkins.insert(account_id.clone(), event.clone());
        }
    }

    // 逐日序列起点：全局最早快照与保留窗口下界的较大者；无快照时为 None（返回空序列）
    let daily_start = coverage_start_at.and_then(local_date).map(|coverage_date| {
        let earliest = today - ChronoDuration::days(record_retention_days() - 1);
        coverage_date.max(earliest)
    });
    let empty_account_daily: HashMap<String, f64> = HashMap::new();

    let account_summaries: Vec<Value> = account_ids
        .iter()
        .map(|account_id| {
            let latest = latest_snapshots.get(account_id);
            let is_current = current_account_ids.contains(account_id);
            let (usage_today, usage_week, usage_month) = usage_totals
                .get(account_id)
                .copied()
                .unwrap_or_default();
            let today_checkin = checkins
                .iter()
                .filter(|event| {
                    event.account_id.as_deref() == Some(account_id.as_str())
                        && event.date == today_key
                })
                .max_by_key(|event| event.ts);
            let last_checkin = last_checkins.get(account_id);
            json!({
                "accountId": account_id,
                "accountName": account_names.get(account_id).cloned().unwrap_or_else(|| "unknown".to_string()),
                "isCurrent": is_current,
                "currentRemaining": is_current.then(|| latest.map(|snapshot| snapshot.remaining)).flatten(),
                "totalCapacity": is_current.then(|| latest.map(|snapshot| snapshot.total)).flatten(),
                "lastSnapshotAt": latest.map(|snapshot| snapshot.ts),
                "usageToday": usage_today,
                "usage7Days": usage_week,
                "usageThisMonth": usage_month,
                "checkedInToday": today_checkin.map(|event| event.result == "success" || event.result == "already"),
                "checkinStatusToday": today_checkin.map(|event| event.result.clone()),
                "lastCheckinAt": last_checkin.map(|event| event.ts),
                "lastCheckinResult": last_checkin.map(|event| event.result.clone()),
                "daily": local_daily_series(
                    daily_usage.get(account_id).unwrap_or(&empty_account_daily),
                    daily_start,
                    today,
                ),
            })
        })
        .collect();

    let mut aggregate_daily: HashMap<String, f64> = HashMap::new();
    for account_daily in daily_usage.values() {
        for (date, amount) in account_daily {
            *aggregate_daily.entry(date.clone()).or_insert(0.0) += amount;
        }
    }
    let daily = local_daily_series(&aggregate_daily, daily_start, today);

    let mut events: Vec<(i64, Value)> = usage_events
        .iter()
        .map(|event| {
            (
                event.ts,
                json!({
                    "kind": "usage",
                    "ts": event.ts,
                    "date": event.date,
                    "accountId": event.account_id,
                    "accountName": account_names.get(&event.account_id).cloned().unwrap_or_else(|| "unknown".to_string()),
                    "amount": event.amount,
                }),
            )
        })
        .collect();
    events.extend(checkins.iter().map(|event| {
        (
            event.ts,
            json!({
                "kind": "checkin",
                "ts": event.ts,
                "date": event.date,
                "accountId": event.account_id,
                "accountName": event.account_id.as_ref().and_then(|id| account_names.get(id)).cloned().unwrap_or_else(|| event.account_name.clone()),
                "result": event.result,
                "error": event.error,
            }),
        )
    }));
    events.sort_by_key(|event| Reverse(event.0));
    let recent_events: Vec<Value> = events
        .into_iter()
        .take(CREDIT_STATS_MAX_EVENTS)
        .map(|(_, event)| event)
        .collect();

    json!({
        "generatedAt": at_ms,
        "retentionDays": record_retention_days(),
        "coverageStartAt": coverage_start_at,
        "summary": {
            "currentRemaining": current_remaining,
            "currentCapacity": current_capacity,
            "usageToday": usage_totals.values().map(|value| value.0).sum::<f64>(),
            "usage7Days": usage_totals.values().map(|value| value.1).sum::<f64>(),
            "usageThisMonth": usage_totals.values().map(|value| value.2).sum::<f64>(),
            "todayCheckedInAccounts": today_checked_in_accounts,
            "todaySuccess": today_success,
            "todayAlready": today_already,
            "todayFailed": today_failed,
        },
        "daily": daily,
        "accounts": account_summaries,
        "events": recent_events,
    })
}

/// 返回本地快照、账号列表、签到日志与官方请求用量的统一统计投影。
///
/// `refresh = false` 时官方用量读本地缓存，不打用量接口；
/// 只有统计页「刷新统计」传入 `refresh = true` 才会重新采集。
pub async fn get_statistics(refresh: bool) -> Value {
    let at_ms = now_ms();
    let accounts = load_accounts();
    let mut statistics =
        build_statistics(&load_snapshots(), &load_checkin_logs(), &accounts, at_ms);
    statistics["officialUsage"] =
        official_usage::official_usage_for_statistics(&accounts, at_ms, refresh).await;
    statistics
}

#[cfg(test)]
mod tests {
    use super::*;

    fn at_local_date(days_ago: i64, hour: u32) -> i64 {
        let today = Local::now().date_naive() - ChronoDuration::days(days_ago);
        Local
            .with_ymd_and_hms(today.year(), today.month(), today.day(), hour, 0, 0)
            .single()
            .expect("valid local test date")
            .timestamp_millis()
    }

    fn snap(ts: i64, remaining: f64) -> Value {
        snapshot_value(ts, "account-1", "one@example.com", 100.0, remaining)
    }

    #[test]
    fn first_observation_does_not_create_usage() {
        let now = at_local_date(0, 12);
        let stats = build_statistics(&[snap(now, 100.0)], &[], &[], now);

        assert_eq!(stats["summary"]["usageToday"], 0.0);
        assert_eq!(stats["daily"][0]["usage"], 0.0);
    }

    #[test]
    fn positive_decreases_are_assigned_to_the_newer_local_day() {
        let now = at_local_date(0, 12);
        let yesterday = at_local_date(1, 12);
        let stats = build_statistics(&[snap(yesterday, 100.0), snap(now, 70.0)], &[], &[], now);

        assert_eq!(stats["summary"]["usageToday"], 30.0);
        assert_eq!(stats["summary"]["usage7Days"], 30.0);
        assert_eq!(
            stats["daily"].as_array().unwrap().last().unwrap()["usage"],
            30.0
        );
        assert_eq!(stats["events"][0]["kind"], "usage");
        assert_eq!(stats["events"][0]["amount"], 30.0);
    }

    #[test]
    fn increases_reset_the_baseline_without_negative_usage() {
        let now = at_local_date(0, 12);
        let earlier = at_local_date(2, 12);
        let yesterday = at_local_date(1, 12);
        let stats = build_statistics(
            &[
                snap(earlier, 100.0),
                snap(yesterday, 125.0),
                snap(now, 115.0),
            ],
            &[],
            &[],
            now,
        );

        assert_eq!(stats["summary"]["usageToday"], 10.0);
        assert_eq!(stats["summary"]["usage7Days"], 10.0);
        assert_eq!(stats["events"].as_array().unwrap().len(), 1);
    }

    #[test]
    fn retention_and_month_windows_use_local_calendar_dates() {
        let now = at_local_date(0, 12);
        let old = at_local_date(CREDIT_SNAPSHOT_RETENTION_DAYS + 1, 12);
        let stats = build_statistics(&[snap(old, 100.0), snap(now, 80.0)], &[], &[], now);

        assert_eq!(stats["summary"]["usage7Days"], 0.0);
        assert_eq!(stats["summary"]["usageThisMonth"], 0.0);
        assert_eq!(stats["coverageStartAt"], now);
    }

    #[test]
    fn checkin_events_are_separate_from_credit_usage() {
        let now = at_local_date(0, 12);
        let logs = vec![json!({
            "ts": now,
            "accountId": "account-1",
            "email": "one@example.com",
            "result": "success",
        })];
        let stats = build_statistics(
            &[snap(now - 60_000, 100.0), snap(now, 90.0)],
            &logs,
            &[],
            now,
        );

        assert_eq!(stats["summary"]["usageToday"], 10.0);
        assert_eq!(stats["summary"]["todayCheckedInAccounts"], 1);
        assert_eq!(stats["events"].as_array().unwrap().len(), 2);
        let event_kinds: Vec<&str> = stats["events"]
            .as_array()
            .unwrap()
            .iter()
            .filter_map(|event| event["kind"].as_str())
            .collect();
        assert!(event_kinds.contains(&"checkin"));
        assert!(event_kinds.contains(&"usage"));
    }

    #[test]
    fn checkins_without_identity_are_kept_as_events_but_not_counted_as_accounts() {
        let now = at_local_date(0, 12);
        let logs = vec![json!({
            "ts": now,
            "result": "success",
        })];
        let stats = build_statistics(&[], &logs, &[], now);

        assert_eq!(stats["summary"]["todayCheckedInAccounts"], 0);
        assert_eq!(stats["events"][0]["kind"], "checkin");
    }

    #[test]
    fn historical_only_accounts_do_not_look_current() {
        let now = at_local_date(0, 12);
        let stats = build_statistics(
            &[snap(now - 60_000, 100.0), snap(now, 80.0)],
            &[],
            &[json!({"id": "current-account"})],
            now,
        );

        assert_eq!(stats["summary"]["currentRemaining"], 0.0);
        let historical = stats["accounts"]
            .as_array()
            .unwrap()
            .iter()
            .find(|account| account["accountId"] == "account-1")
            .expect("historical account summary");
        assert_eq!(historical["isCurrent"], false);
        assert!(historical["currentRemaining"].is_null());
        assert_eq!(historical["lastSnapshotAt"], now);
    }

    #[test]
    fn snapshot_normalization_filters_old_values_and_caps_records() {
        let now = at_local_date(0, 12);
        let old = snap(
            now - (CREDIT_SNAPSHOT_RETENTION_DAYS + 1) * 24 * 3600 * 1000,
            100.0,
        );
        let mut values = vec![old];
        values.extend((0..(CREDIT_SNAPSHOT_MAX_RECORDS + 1)).map(|index| {
            snap(
                now - (CREDIT_SNAPSHOT_MAX_RECORDS as i64 - index as i64) * 1_000,
                100.0,
            )
        }));

        let normalized = normalize_snapshots(&values, now);

        assert_eq!(normalized.len(), CREDIT_SNAPSHOT_MAX_RECORDS);
        assert!(normalized.iter().all(|value| {
            snapshot_from_value(value)
                .map(|snapshot| {
                    snapshot.ts >= now - CREDIT_SNAPSHOT_RETENTION_DAYS * 24 * 3600 * 1000
                })
                .unwrap_or(false)
        }));
    }

    #[test]
    fn same_value_is_suppressed_only_inside_the_short_window() {
        let now = at_local_date(0, 12);
        let previous = snap(now - CREDIT_SNAPSHOT_DEDUPE_WINDOW_MS - 1, 100.0);
        assert!(!should_suppress_duplicate(
            std::slice::from_ref(&previous),
            "account-1",
            100.0,
            100.0,
            now,
        ));
        assert!(should_suppress_duplicate(
            &[snap(now - CREDIT_SNAPSHOT_DEDUPE_WINDOW_MS + 1, 100.0)],
            "account-1",
            100.0,
            100.0,
            now,
        ));
    }

    // -----------------------------------------------------------------------
    // 积分来源判定：区分「额度发放 / 调用扣减 / 额度到期 / 其他调整」
    //
    // 这些判据直接来自实测真实快照（见交付报告）：
    //   上升 158 次中 141 次容量与余额增量相等 → grant
    //   下降 433 次容量不变                    → consume
    //   下降   5 次容量同降                    → expire
    // -----------------------------------------------------------------------

    /// 真实样例：新积分包到账，容量 +100、余额 +100。
    #[test]
    fn capacity_and_balance_rising_together_is_a_grant() {
        let delta = credit_delta(1000.0, 1000.0, 1100.0, 1100.0).expect("应有变化");
        assert_eq!(delta.amount, 100);
        assert_eq!(delta.capacity, 100);
        assert_eq!(
            classify_credit_source(delta.amount, delta.capacity),
            crate::modules::account_records::CREDIT_SOURCE_GRANT
        );
    }

    /// 真实样例（acc-6091…，2026-09-16）：容量 +1650 而余额只 +1620.73 ——
    /// 到账与本次快照之间已经消耗掉一部分。这仍是 grant，不是 adjust。
    #[test]
    fn grant_still_holds_when_part_of_the_grant_was_already_spent() {
        let delta = credit_delta(4656.62, 4700.0, 6277.35, 6350.0).expect("应有变化");
        assert_eq!(delta.amount, 1621);
        assert_eq!(delta.capacity, 1650);
        assert_eq!(
            classify_credit_source(delta.amount, delta.capacity),
            crate::modules::account_records::CREDIT_SOURCE_GRANT,
            "容量增量大于余额增量（到账后已消耗）仍属额度发放"
        );
    }

    /// 真实样例：余额降、容量不变 = 纯消耗（实测 433 次全部如此）。
    #[test]
    fn balance_falling_with_stable_capacity_is_consumption() {
        let delta = credit_delta(5000.0, 5000.0, 4969.0, 5000.0).expect("应有变化");
        assert_eq!(delta.amount, -31);
        assert_eq!(delta.capacity, 0);
        assert_eq!(
            classify_credit_source(delta.amount, delta.capacity),
            crate::modules::account_records::CREDIT_SOURCE_CONSUME
        );
    }

    /// 真实样例（acc-da16…，2026-09-14）：某积分包容量 -100、余额 -79 ——
    /// 包内还剩 79 分就整包失效，那些分是**到期蒸发**而不是被调用消耗掉。
    #[test]
    fn balance_and_capacity_falling_together_is_expiry() {
        let delta = credit_delta(3000.0, 3000.0, 2921.0, 2900.0).expect("应有变化");
        assert_eq!(delta.amount, -79);
        assert_eq!(delta.capacity, -100);
        assert_eq!(
            classify_credit_source(delta.amount, delta.capacity),
            crate::modules::account_records::CREDIT_SOURCE_EXPIRE,
            "容量同降说明是额度被回收，不是消耗"
        );
    }

    /// 容量没变而余额涨了：积分是**退回来**的，不是新增额度。
    /// 若判成 grant，用户会以为额度包变多了 —— 那是不实描述。
    #[test]
    fn balance_rising_without_capacity_is_an_adjustment_not_a_grant() {
        let delta = credit_delta(1000.0, 1000.0, 1100.0, 1000.0).expect("应有变化");
        assert_eq!(delta.amount, 100);
        assert_eq!(delta.capacity, 0);
        assert_eq!(
            classify_credit_source(delta.amount, delta.capacity),
            crate::modules::account_records::CREDIT_SOURCE_ADJUST
        );
    }

    #[test]
    fn unchanged_balance_yields_no_delta() {
        // 没有变化就不该记一条 amount=0 的噪音（巡检每 15 分钟一次）
        assert!(credit_delta(100.0, 100.0, 100.0, 100.0).is_none());
        // 浮点误差也要被 round 吸收掉
        assert!(credit_delta(100.0, 100.0, 100.0000001, 100.0).is_none());
    }

    /// 端到端：写入第二个快照后，积分记录必须带上来源，且是**可读回**的。
    #[test]
    fn second_snapshot_writes_a_credit_record_with_source() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("credit-source-e2e");

        // 首个快照只建立基线，不产生记录
        assert!(record_snapshot("acc-src", "n", 1000.0, 1000.0, Default::default()));
        let v = crate::modules::account_records::query_records("acc-src", 0, 0, &[], 100);
        assert_eq!(v.get("total").and_then(Value::as_u64), Some(0), "首个快照不该产生记录");

        // 余额 +100 且容量 +100 → 额度发放
        assert!(record_snapshot("acc-src", "n", 1100.0, 1100.0, Default::default()));
        let v = crate::modules::account_records::query_records(
            "acc-src",
            0,
            0,
            &[crate::modules::account_records::KIND_CREDIT.to_string()],
            100,
        );
        let recs = v.get("records").and_then(Value::as_array).unwrap();
        assert_eq!(recs.len(), 1, "应写入一条积分记录");
        assert_eq!(
            recs[0].get("source").and_then(Value::as_str),
            Some(crate::modules::account_records::CREDIT_SOURCE_GRANT)
        );
        assert_eq!(recs[0].get("amount").and_then(Value::as_i64), Some(100));
        // 标题要能自解释，而不是笼统的「积分增长」
        let title = recs[0].get("title").and_then(Value::as_str).unwrap_or("");
        assert!(title.contains("额度发放"), "标题应含来源，实际: {title}");
        // detail 要给出判据（容量变化），让用户能自行核对
        let detail = recs[0].get("detail").and_then(Value::as_str).unwrap_or("");
        assert!(detail.contains("容量"), "detail 应写明容量判据，实际: {detail}");
    }

    /// 端到端：纯消耗要记成 consume，而不是笼统的「积分消耗」。
    #[test]
    fn pure_consumption_snapshot_records_consume_source() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("credit-consume-e2e");
        assert!(record_snapshot("acc-c", "n", 1000.0, 1000.0, Default::default()));
        assert!(record_snapshot("acc-c", "n", 1000.0, 900.0, Default::default()));

        let v = crate::modules::account_records::query_records(
            "acc-c",
            0,
            0,
            &[crate::modules::account_records::KIND_CREDIT.to_string()],
            100,
        );
        let recs = v.get("records").and_then(Value::as_array).unwrap();
        // 先钉住条数：首条快照只建立基线不产生记录，故这里恰好 1 条。
        // 有条数断言时按下标取值才是确定的（同 ts 的顺序问题只影响多条时）。
        assert_eq!(recs.len(), 1, "首个快照不该产生记录，应恰好 1 条");
        assert_eq!(
            recs[0].get("source").and_then(Value::as_str),
            Some(crate::modules::account_records::CREDIT_SOURCE_CONSUME)
        );
        assert_eq!(recs[0].get("amount").and_then(Value::as_i64), Some(-100));
    }

    // -----------------------------------------------------------------------
    // 幻影积分记录（所有者实测反馈：「发放的积分根本没有变化 但是这里记录
    // 却是 -380 然后又 +380」）
    //
    // 根因：上游偶发返回**不完整的包列表**，聚合 total/remaining 凭空下跌再涨回，
    // 被当成「消耗」+「发放」记成一对。下面用 owner 的**真实数值**做回归。
    // -----------------------------------------------------------------------

    /// 构造一份包子集。
    fn packs(entries: &[(&str, f64)]) -> std::collections::BTreeMap<String, f64> {
        entries.iter().map(|(k, v)| (k.to_string(), *v)).collect()
    }

    /// 判据 2（核心）：`total`（额度容量）不应因消耗而减少。
    ///
    /// owner 真实数据：某次上游只返回 1 个 30 的包 → total 从 380 掉到 30，
    /// 于是被记成「-350 积分消耗」。但**容量不会因为消耗而变小**，
    /// 所以 total 下降本身就证明「这次没读全」。
    #[test]
    fn reading_trust_rejects_dropped_capacity() {
        let prev = snapshot_from_value(&json!({
            "accountId": "a", "ts": 1, "total": 380.0, "remaining": 380.0
        }))
        .unwrap();
        // total 380 → 30（同时 remaining 也跌）⇒ 必须判为不可信
        assert_eq!(
            reading_trust(&prev, 30.0, 30.0, &packs(&[("p30", 30.0)])),
            ReadingTrust::Incomplete("额度容量意外减少，上游本次返回的积分包列表不完整"),
        );
        // total 380 → 0 ⇒ 同样不可信
        assert!(matches!(
            reading_trust(&prev, 0.0, 0.0, &packs(&[])),
            ReadingTrust::Incomplete(_)
        ));
    }

    /// 正常消耗：`total` 不变、只 `remaining` 跌 ⇒ 必须放行（否则会误杀真实消耗）。
    ///
    /// 这是本判据**不能**用「跌幅大小」代替的原因：消耗快时余额掉得快是正常的。
    #[test]
    fn reading_trust_allows_real_consumption() {
        let prev = snapshot_from_value(&json!({
            "accountId": "a", "ts": 1, "total": 1000.0, "remaining": 1000.0
        }))
        .unwrap();
        // 容量没变、余额跌到 0（消耗快）⇒ 真实消耗，放行
        assert_eq!(reading_trust(&prev, 1000.0, 0.0, &packs(&[("p1", 0.0)])), ReadingTrust::Ok);
        // 容量没变、余额小跌 ⇒ 放行
        assert_eq!(reading_trust(&prev, 1000.0, 900.0, &packs(&[("p1", 900.0)])), ReadingTrust::Ok);
    }

    /// 判据 1：包凭空消失（下次又回来）⇒ 读数失败。
    ///
    /// 包不会自己消失再回来 —— 真到期有 expireAt 为证，真消耗只减 remaining 不删包。
    #[test]
    fn reading_trust_rejects_vanished_package() {
        let prev = snapshot_from_value(&json!({
            "accountId": "a", "ts": 1, "total": 380.0, "remaining": 380.0,
            "packages": { "p380": 380.0 }
        }))
        .unwrap();
        // 这次只剩 p30，p380 不见了，且余额也跌 ⇒ 不可信
        assert!(matches!(
            reading_trust(&prev, 30.0, 30.0, &packs(&[("p30", 30.0)])),
            ReadingTrust::Incomplete(_)
        ));
    }

    /// 真实到期（包消失 + 容量跌）在**没有 expireAt 证据**时也会被判不可信 ——
    /// 这是刻意的保守：宁可漏记一次真到期，也不要刷出一对幻影记录。
    /// （真到期会在下一次读数稳定后由「容量确实少了且不再回来」体现。）
    #[test]
    fn reading_trust_is_conservative_when_package_disappears() {
        let prev = snapshot_from_value(&json!({
            "accountId": "a", "ts": 1, "total": 1000.0, "remaining": 900.0,
            "packages": { "p1": 500.0, "p2": 400.0 }
        }))
        .unwrap();
        // p2 消失、总容量 1000→500
        assert!(matches!(
            reading_trust(&prev, 500.0, 500.0, &packs(&[("p1", 500.0)])),
            ReadingTrust::Incomplete(_)
        ));
    }

    /// 新包到账（total 上升）必须放行 —— 那是真实的额度发放。
    #[test]
    fn reading_trust_allows_grant() {
        let prev = snapshot_from_value(&json!({
            "accountId": "a", "ts": 1, "total": 380.0, "remaining": 100.0,
            "packages": { "p380": 100.0 }
        }))
        .unwrap();
        assert_eq!(
            reading_trust(&prev, 760.0, 480.0, &packs(&[("p380", 100.0), ("p380b", 380.0)])),
            ReadingTrust::Ok
        );
    }

    /// 老快照没有包级明细时判据 2 仍生效（向后兼容：老数据也能被保护）。
    #[test]
    fn reading_trust_works_without_package_detail() {
        let prev = snapshot_from_value(&json!({
            "accountId": "a", "ts": 1, "total": 380.0, "remaining": 380.0
        }))
        .unwrap();
        assert!(prev.packages.is_empty(), "老快照应解析成空包集");
        // 无包级明细 → 判据 1 跳过，判据 2 仍拦下 total 下降
        assert!(matches!(
            reading_trust(&prev, 30.0, 30.0, &packs(&[])),
            ReadingTrust::Incomplete(_)
        ));
        // 无包级明细 + total 不变 → 放行
        assert_eq!(reading_trust(&prev, 380.0, 200.0, &packs(&[])), ReadingTrust::Ok);
    }

    /// 端到端：owner 的幻影配对**不再产生任何积分记录**。
    ///
    /// 复现真实序列：380 → 30（只读到 1 个包）→ 380（读全了）。
    /// 修复前会记「-350 消耗」+「+350 增长」；修复后应为 0 条。
    #[test]
    fn phantom_pair_is_not_recorded() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("credit-phantom-pair");

        // 基线：完整读到 380 的包
        assert!(record_snapshot("acc-ph", "n", 380.0, 380.0, packs(&[("p380", 380.0)])));
        // 上游这次只返回了 30 的包（不完整）—— 修复前会记「-350 消耗」
        assert!(record_snapshot("acc-ph", "n", 30.0, 30.0, packs(&[("p30", 30.0)])));
        // 下次读全了 —— 修复前会记「+350 增长」
        assert!(record_snapshot("acc-ph", "n", 380.0, 380.0, packs(&[("p380", 380.0)])));

        let v = crate::modules::account_records::query_records(
            "acc-ph",
            0,
            0,
            &[crate::modules::account_records::KIND_CREDIT.to_string()],
            100,
        );
        let recs = v.get("records").and_then(Value::as_array).unwrap();
        assert_eq!(
            recs.len(),
            0,
            "幻影配对不该产生任何积分记录，实际产生了 {} 条: {:?}",
            recs.len(),
            recs.iter()
                .map(|r| (r.get("amount"), r.get("title")))
                .collect::<Vec<_>>()
        );
    }

    /// 对照：真实消耗在同样流程下**仍要**被记录（不能因为修幻影而把真消耗也吞掉）。
    #[test]
    fn real_consumption_still_recorded_after_fix() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("credit-real-consume");

        assert!(record_snapshot("acc-rc", "n", 380.0, 380.0, packs(&[("p380", 380.0)])));
        // 容量不变、余额跌 350 ⇒ 真实消耗，必须记
        assert!(record_snapshot("acc-rc", "n", 380.0, 30.0, packs(&[("p380", 30.0)])));

        let v = crate::modules::account_records::query_records(
            "acc-rc",
            0,
            0,
            &[crate::modules::account_records::KIND_CREDIT.to_string()],
            100,
        );
        let recs = v.get("records").and_then(Value::as_array).unwrap();
        assert_eq!(recs.len(), 1, "真实消耗必须被记录");
        assert_eq!(recs[0].get("amount").and_then(Value::as_i64), Some(-350));
        assert_eq!(
            recs[0].get("source").and_then(Value::as_str),
            Some(crate::modules::account_records::CREDIT_SOURCE_CONSUME)
        );
    }

    /// **最容易误伤的一条**：真实消耗之后又有真实发放，必须**两条都记**。
    ///
    /// 序列：`380 → 30（真消耗，包还在）→ 760（真发放，来了新包）`
    /// 若判据 3 写成「数值回升 = 恢复」就会把第二条件吞掉 —— 这正是我第一版
    /// 用「回到旧值」做判据时的错误。用 `suspect` 标志才能区分：
    ///   · 真消耗那次**不可疑**（包在、只是余额少）→ 下一次回升是**发放**，要记
    ///   · 抖动那次**可疑**（包消失）→ 下一次回升是**恢复**，不记
    #[test]
    fn real_grant_after_real_consumption_is_still_recorded() {
        let _iso =
            crate::modules::config::test_isolation::Isolated::new("credit-grant-after-consume");

        assert!(record_snapshot("acc-gc", "n", 380.0, 380.0, packs(&[("p380", 380.0)])));
        // 真消耗：包还在，余额跌到 30（不可疑）
        assert!(record_snapshot("acc-gc", "n", 380.0, 30.0, packs(&[("p380", 30.0)])));
        // 真发放：来了一个新包，总容量 380 → 760
        assert!(record_snapshot(
            "acc-gc",
            "n",
            760.0,
            410.0,
            packs(&[("p380", 30.0), ("p380b", 380.0)])
        ));

        let v = crate::modules::account_records::query_records(
            "acc-gc",
            0,
            0,
            &[crate::modules::account_records::KIND_CREDIT.to_string()],
            100,
        );
        let recs = v.get("records").and_then(Value::as_array).unwrap();
        assert_eq!(
            recs.len(),
            2,
            "真消耗 + 真发放都要记，实际 {:?}",
            recs.iter()
                .map(|r| (r.get("amount"), r.get("title")))
                .collect::<Vec<_>>()
        );

        // **不要按下标断言顺序**：三条快照在同一毫秒内写完时 `ts` 相同，
        // 而 query_records 用稳定排序按 ts 降序 —— 同 ts 会保留插入顺序，
        // 于是 recs[0] 是 -350 而不是 +380。
        //
        // 这一点在 Linux CI 上暴露（本地 Windows 通过）：
        // `real_grant_after_real_consumption_is_still_recorded` 报
        // 「最新一条应是发放 +380，left: Some(-350)」。
        // 顺序在同 ts 下不是被测语义（两条记录都写对了才是），
        // 所以按**内容**定位，让断言与平台时钟精度无关。
        let find = |amount: i64| {
            recs.iter()
                .find(|r| r.get("amount").and_then(Value::as_i64) == Some(amount))
                .unwrap_or_else(|| panic!("找不到 amount={amount} 的记录，实际 {recs:?}"))
        };
        let consumed = find(-350);
        assert!(
            consumed
                .get("title")
                .and_then(Value::as_str)
                .unwrap_or("")
                .contains("调用扣减"),
            "消耗那条应是「调用扣减」，实际 {:?}",
            consumed.get("title")
        );
        let granted = find(380);
        assert!(
            granted
                .get("title")
                .and_then(Value::as_str)
                .unwrap_or("")
                .contains("额度发放"),
            "发放那条应是「额度发放」，实际 {:?}",
            granted.get("title")
        );
    }

    /// 新快照要带包级明细（否则判据 1 永远用不上）。
    #[test]
    fn new_snapshot_carries_package_detail() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("credit-snapshot-packages");
        assert!(record_snapshot(
            "acc-pk",
            "n",
            380.0,
            380.0,
            packs(&[("p380", 380.0)])
        ));
        let snapshots = load_snapshots();
        let mine = snapshots
            .iter()
            .filter_map(snapshot_from_value)
            .find(|s| s.account_id == "acc-pk")
            .expect("应写入快照");
        assert_eq!(mine.packages.get("p380"), Some(&380.0));
    }
}
