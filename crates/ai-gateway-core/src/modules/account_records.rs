//! 账号记录：把「任务执行 / 积分变化 / Token 消耗」三类事件汇到一处，
//! 供「单账号记录」视图按账号 + 日期查询。
//!
//! 为什么要统一：这三类数据此前散落在不同文件、字段各异、口径不一 ——
//! 签到日志只记「成功/失败」、积分只有快照没有事件、Token 统计按来源分文件。
//! 用户想看「这个账号昨天干了什么、扣了多少」需要翻三个地方，且无法按账号聚合。
//!
//! 设计要点：
//!
//! 1. **单一追加式事件流**（`account_records.json`）。每条记录自带
//!    `accountId` / `ts` / `kind`，查询时按账号与日期区间过滤。
//! 2. **只追加、不改写历史**。清理只按时间与条数裁剪（见 `normalize_records`），
//!    避免出现「昨天看到的记录今天变了」。
//! 3. **写入绝不 panic**。记录是旁路观测数据，写失败不能影响主流程
//!    （签到、聊天、领奖都必须照常完成）。
//! 4. **保留天数复用 `record_retention_days()`**，与签到日志/积分快照同一口径。

use std::sync::Mutex;

use serde_json::{json, Value};

use crate::modules::config::{
    atomic_write, load_checkin_logs, now_ms, record_retention_days, store_dir,
};

/// 记录上限：防止文件无限增长。
///
/// 取 20000 而非更小：按每天每账号 10 条、20 个账号估算，
/// 60 天约 12000 条，留出余量。超过时按时间从旧到新裁剪。
pub const ACCOUNT_RECORD_MAX: usize = 20_000;

/// 事件类型。
pub const KIND_TASK: &str = "task";
pub const KIND_CREDIT: &str = "credit";
pub const KIND_TOKEN: &str = "token";

/// 积分变化的「来源」取值。
///
/// 为什么需要一个独立字段而不是继续写在 title 里：title 是给人读的短标题
/// （「积分增长」「积分消耗」），来源是**另一条正交信息** —— 同一个「积分增长」
/// 既可能来自新积分包到账，也可能是返还。两者都塞进 title 会让标题在
/// 「积分增长 · 额度发放」这类拼接里越写越长，且前端无法据此稳定地筛选/配色。
///
/// 这些取值描述的是**观测到的事实**，而不是猜测的任务名：
/// 上游资源接口只返回容量与余额，不返回「这笔积分由哪个任务产生」，
/// 因此这里如实记录「余额是怎么动的」，而不是编造一个任务名。
/// 具体判据见 [`crate::modules::credit_usage::classify_credit_source`]。
pub const CREDIT_SOURCE_GRANT: &str = "grant";
pub const CREDIT_SOURCE_CONSUME: &str = "consume";
pub const CREDIT_SOURCE_EXPIRE: &str = "expire";
pub const CREDIT_SOURCE_ADJUST: &str = "adjust";

static RECORD_WRITE_LOCK: Mutex<()> = Mutex::new(());

/// 账号记录文件路径。
pub fn account_records_file() -> std::path::PathBuf {
    store_dir().join("account_records.json")
}

/// 一条账号记录。
///
/// 字段刻意扁平（而非嵌套 detail 对象）：前端要按账号/日期/类型筛选，
/// 扁平结构让过滤逻辑简单且不易漏字段。
#[derive(Debug, Clone)]
pub struct AccountRecord {
    pub ts: i64,
    pub account_id: String,
    pub account_name: String,
    /// task | credit | token
    pub kind: String,
    /// 事件标题，如「自动签到」「积分消耗」「chat 调用」。
    pub title: String,
    /// 结果：success | failed | already | info
    pub result: String,
    /// 数值变化：积分增减、Token 数量；无则为 0。
    pub amount: i64,
    /// 补充说明（错误信息、模型名等）。
    pub detail: String,
    /// 积分变化的来源（grant | consume | expire | adjust）。
    ///
    /// 用 `Option` 而不是空字符串：**历史记录里根本没有这个字段**，
    /// 若用空串表示「无来源」，读回来的老记录与「来源未知」就无法区分，
    /// 前端也就没法决定该不该渲染这一栏。`None` 一律表示「这条记录没有来源信息」
    /// （历史记录，以及 task / token 两类记录 —— 来源只对积分变化有意义）。
    pub source: Option<String>,
}

impl AccountRecord {
    pub fn to_json(&self) -> Value {
        let mut value = json!({
            "ts": self.ts,
            "accountId": self.account_id,
            "accountName": self.account_name,
            "kind": self.kind,
            "title": self.title,
            "result": self.result,
            "amount": self.amount,
            "detail": self.detail,
        });
        // 只有确实有来源时才写这个键。
        //
        // 为什么不是写 `"source": null`：task / token 记录不该凭空多出一个恒为
        // null 的字段 —— Go 侧是**按原始字节**透传宿主记录的（records.go 的 load），
        // 给它不需要的键只会让两边 diff 变噪声，也看不出「谁有来源」。
        if let Some(source) = &self.source {
            value["source"] = json!(source);
        }
        value
    }
}

/// 追加一条任务执行记录。
///
/// 任务记录不带来源：它的 `title` 本身就是任务名（如「自动签到」「夜猫子任务」），
/// 再记一个 source 只是把同一件事写两遍。
pub fn add_task_record(account_id: &str, account_name: &str, title: &str, result: &str, detail: &str) {
    push(AccountRecord {
        ts: now_ms(),
        account_id: account_id.to_string(),
        account_name: account_name.to_string(),
        kind: KIND_TASK.to_string(),
        title: title.to_string(),
        result: result.to_string(),
        amount: 0,
        detail: detail.to_string(),
        source: None,
    });
}

/// 追加一条积分变化记录（amount 正为增长、负为消耗）。
///
/// `source` 是这个账号**为什么**变分（见 [`CREDIT_SOURCE_GRANT`] 等）。
/// 为什么必须由调用方传进来、而不是在这里按 amount 正负猜：
/// 「余额涨了」本身分不出「签到/任务发的奖励」与「买的积分包到账」，
/// 只有掌握前后快照的调用方（`credit_usage::record_snapshot`）才能判断容量有没有一起变。
/// 在这里按符号猜，等于把「任务奖励」这个**未经证实的结论**写进记录。
pub fn add_credit_record(
    account_id: &str,
    account_name: &str,
    title: &str,
    amount: i64,
    detail: &str,
    source: Option<&str>,
) {
    push(AccountRecord {
        ts: now_ms(),
        account_id: account_id.to_string(),
        account_name: account_name.to_string(),
        kind: KIND_CREDIT.to_string(),
        title: title.to_string(),
        result: if amount >= 0 { "success" } else { "info" }.to_string(),
        amount,
        detail: detail.to_string(),
        source: source
            .map(str::trim)
            .filter(|value| !value.is_empty())
            .map(String::from),
    });
}

/// 追加一条 Token 消耗记录。
///
/// 同样不带 source：`title` 里已经带了模型名（真正的来源维度）。
pub fn add_token_record(
    account_id: &str,
    account_name: &str,
    model: &str,
    tokens: i64,
    detail: &str,
) {
    push(AccountRecord {
        ts: now_ms(),
        account_id: account_id.to_string(),
        account_name: account_name.to_string(),
        kind: KIND_TOKEN.to_string(),
        title: if model.is_empty() {
            "Token 消耗".to_string()
        } else {
            format!("Token 消耗 · {model}")
        },
        result: "info".to_string(),
        amount: tokens,
        detail: detail.to_string(),
        source: None,
    });
}

/// 追加一条记录（读-改-写，加锁串行化）。
///
/// 写失败静默忽略：记录是旁路观测数据，不能让「记不下来」影响签到/聊天等主流程。
fn push(record: AccountRecord) {
    let _guard = RECORD_WRITE_LOCK.lock().unwrap_or_else(|e| e.into_inner());
    let mut records = load_raw();
    records.push(record.to_json());
    let normalized = normalize_records(&records, now_ms(), record_retention_days());
    let content = serde_json::to_string_pretty(&normalized).unwrap_or_default();
    let _ = atomic_write(&account_records_file(), &content);
}

/// 读取原始记录（不做过滤，容错）。
fn load_raw() -> Vec<Value> {
    std::fs::read_to_string(account_records_file())
        .ok()
        .and_then(|text| serde_json::from_str::<Value>(&text).ok())
        .and_then(|v| match v {
            Value::Array(items) => Some(items),
            _ => None,
        })
        .unwrap_or_default()
}

/// 清理：按保留天数裁剪过期记录，并按上限截断最旧的。
///
/// 与签到日志同一套思路（先按时间、再按条数），保证两个文件的行为一致。
pub fn normalize_records(records: &[Value], at_ms: i64, keep_days: i64) -> Vec<Value> {
    let days = if keep_days <= 0 { 1 } else { keep_days };
    let cutoff = at_ms.saturating_sub(days * 24 * 3600 * 1000);
    let mut kept: Vec<Value> = records
        .iter()
        .filter(|r| r.get("ts").and_then(Value::as_i64).unwrap_or(0) >= cutoff)
        .cloned()
        .collect();
    if kept.len() > ACCOUNT_RECORD_MAX {
        kept.drain(..kept.len() - ACCOUNT_RECORD_MAX);
    }
    kept
}

/// 查询账号记录。
///
/// 参数：
///   - `account_id`：为空表示全部账号
///   - `from_ms` / `to_ms`：时间区间（含端点）；为 0 表示不限
///   - `kinds`：为空表示全部类型
///
/// 返回按时间**倒序**（最新在前），并附各类型计数，便于前端展示概览。
pub fn query_records(
    account_id: &str,
    from_ms: i64,
    to_ms: i64,
    kinds: &[String],
    limit: usize,
) -> Value {
    let at = now_ms();
    let keep_days = record_retention_days();
    let all = normalize_records(&load_raw(), at, keep_days);

    let mut matched: Vec<&Value> = all
        .iter()
        .filter(|r| {
            let ts = r.get("ts").and_then(Value::as_i64).unwrap_or(0);
            if from_ms > 0 && ts < from_ms {
                return false;
            }
            if to_ms > 0 && ts > to_ms {
                return false;
            }
            if !account_id.is_empty()
                && r.get("accountId").and_then(Value::as_str).unwrap_or("") != account_id
            {
                return false;
            }
            if !kinds.is_empty() {
                let k = r.get("kind").and_then(Value::as_str).unwrap_or("");
                if !kinds.iter().any(|x| x == k) {
                    return false;
                }
            }
            true
        })
        .collect();

    // 倒序：最新的记录最有用，用户打开页面先看到今天发生的事
    matched.sort_by_key(|r| std::cmp::Reverse(r.get("ts").and_then(Value::as_i64).unwrap_or(0)));

    // 概览统计基于**过滤后**的全量，而不是截断后的列表 ——
    // 否则「共 N 条」会随 limit 变化，用户会困惑。
    let mut task_count = 0usize;
    let mut credit_net: i64 = 0;
    let mut token_sum: i64 = 0;
    for r in &matched {
        match r.get("kind").and_then(Value::as_str).unwrap_or("") {
            KIND_TASK => task_count += 1,
            KIND_CREDIT => credit_net += r.get("amount").and_then(Value::as_i64).unwrap_or(0),
            KIND_TOKEN => token_sum += r.get("amount").and_then(Value::as_i64).unwrap_or(0),
            _ => {}
        }
    }

    let total = matched.len();
    let limited: Vec<Value> = matched
        .into_iter()
        .take(if limit == 0 { total } else { limit })
        .cloned()
        .collect();

    json!({
        "records": limited,
        "total": total,
        "summary": {
            "taskCount": task_count,
            "creditNet": credit_net,
            "tokenSum": token_sum,
        },
        "retentionDays": keep_days,
    })
}

/// 把历史签到日志回填成账号记录（一次性迁移）。
///
/// 为什么要回填：用户此前已积累了大量签到日志，若新视图只显示迁移后的数据，
/// 用户会以为「以前的记录丢了」。回填后新旧数据在同一视图里连续可见。
///
/// 幂等：已回填过的签到日志会被标记（`backfilled` 字段写在记录里，
/// 用 ts + accountId 去重），重复调用不会产生重复记录。
pub fn backfill_from_checkin_logs() -> std::io::Result<usize> {
    let _guard = RECORD_WRITE_LOCK.lock().unwrap_or_else(|e| e.into_inner());
    let mut records = load_raw();
    let existing: std::collections::HashSet<(i64, String)> = records
        .iter()
        .filter(|r| r.get("kind").and_then(Value::as_str) == Some(KIND_TASK))
        .map(|r| {
            (
                r.get("ts").and_then(Value::as_i64).unwrap_or(0),
                r.get("accountId").and_then(Value::as_str).unwrap_or("").to_string(),
            )
        })
        .collect();

    let mut added = 0usize;
    for log in load_checkin_logs() {
        let ts = log.get("ts").and_then(Value::as_i64).unwrap_or(0);
        let account_id = log
            .get("accountId")
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_string();
        if ts == 0 || account_id.is_empty() {
            continue;
        }
        if existing.contains(&(ts, account_id.clone())) {
            continue;
        }
        let result = log.get("result").and_then(Value::as_str).unwrap_or("info");
        records.push(json!({
            "ts": ts,
            "accountId": account_id,
            "accountName": log.get("email").and_then(Value::as_str).unwrap_or(""),
            "kind": KIND_TASK,
            "title": "自动签到",
            "result": result,
            "amount": 0,
            "detail": log.get("error").and_then(Value::as_str).unwrap_or(""),
        }));
        added += 1;
    }

    if added == 0 {
        return Ok(0);
    }
    let normalized = normalize_records(&records, now_ms(), record_retention_days());
    let content = serde_json::to_string_pretty(&normalized).unwrap_or_default();
    atomic_write(&account_records_file(), &content)?;
    Ok(added)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::modules::config::test_isolation::Isolated;

    fn rec(ts: i64, account_id: &str, kind: &str, amount: i64) -> Value {
        json!({
            "ts": ts, "accountId": account_id, "accountName": "n",
            "kind": kind, "title": "t", "result": "success",
            "amount": amount, "detail": "",
        })
    }

    /// 直接往当前隔离目录写一条记录（绕过 push 的 now_ms，便于构造时间序列）。
    fn push_at(ts: i64, account_id: &str, kind: &str, title: &str, amount: i64) {
        let mut records = load_raw();
        records.push(json!({
            "ts": ts, "accountId": account_id, "accountName": account_id,
            "kind": kind, "title": title, "result": "success",
            "amount": amount, "detail": "",
        }));
        let normalized = normalize_records(&records, now_ms(), 3650);
        let content = serde_json::to_string_pretty(&normalized).unwrap_or_default();
        atomic_write(&account_records_file(), &content).expect("写入测试记录应成功");
    }

    // -----------------------------------------------------------------------
    // 清理：按保留天数裁剪 + 按条数截断
    // -----------------------------------------------------------------------

    #[test]
    fn normalize_drops_records_older_than_retention() {
        let now = 1_700_000_000_000i64;
        let day = 24 * 3600 * 1000;
        let records = vec![
            rec(now - 8 * day, "a", KIND_TASK, 0),
            rec(now - 6 * day, "a", KIND_TASK, 0),
        ];
        let kept = normalize_records(&records, now, 7);
        assert_eq!(kept.len(), 1, "只应保留 7 天内的记录");
        assert_eq!(kept[0].get("ts").and_then(Value::as_i64), Some(now - 6 * day));
    }

    #[test]
    fn normalize_truncates_to_max_keeping_newest() {
        let now = 1_700_000_000_000i64;
        let records: Vec<Value> = (0..(ACCOUNT_RECORD_MAX + 50))
            .map(|i| rec(now - ACCOUNT_RECORD_MAX as i64 + i as i64, "a", KIND_TASK, 0))
            .collect();
        let kept = normalize_records(&records, now, 3650);
        assert_eq!(kept.len(), ACCOUNT_RECORD_MAX, "应截断到上限");
        // 保留的应是**最新**的一批（截掉最旧的 50 条）
        let first_ts = kept[0].get("ts").and_then(Value::as_i64).unwrap_or(0);
        assert_eq!(
            first_ts,
            records[50].get("ts").and_then(Value::as_i64).unwrap_or(0),
            "应截掉最旧的 50 条"
        );
    }

    #[test]
    fn normalize_treats_non_positive_retention_as_one_day() {
        // 防御性：保留天数非法时不应把记录清空（那等于静默关闭功能）
        let now = 1_700_000_000_000i64;
        let records = vec![rec(now - 3600 * 1000, "a", KIND_TASK, 0)];
        let kept = normalize_records(&records, now, 0);
        assert_eq!(kept.len(), 1, "非法保留天数应退化为 1 天而不是清空");
    }

    // -----------------------------------------------------------------------
    // 查询：账号 / 时间 / 类型三个维度
    // -----------------------------------------------------------------------

    #[test]
    fn query_filters_by_account() {
        let _iso = Isolated::new("records-by-account");
        let now = now_ms();
        push_at(now, "a", KIND_TASK, "签到", 0);
        push_at(now, "b", KIND_TASK, "签到", 0);

        let v = query_records("a", 0, 0, &[], 100);
        assert_eq!(v.get("total").and_then(Value::as_u64), Some(1));
        let recs = v.get("records").and_then(Value::as_array).unwrap();
        assert_eq!(recs[0].get("accountId").and_then(Value::as_str), Some("a"));
    }

    #[test]
    fn query_filters_by_time_range() {
        let _iso = Isolated::new("records-by-time");
        let now = now_ms();
        let day = 24 * 3600 * 1000;
        push_at(now - 3 * day, "old", KIND_TASK, "旧", 0);
        push_at(now, "new", KIND_TASK, "新", 0);

        let v = query_records("", now - day, 0, &[], 100);
        assert_eq!(v.get("total").and_then(Value::as_u64), Some(1), "只应命中最新的");
        // 区间上界同样生效
        let v2 = query_records("", 0, now - day, &[], 100);
        assert_eq!(v2.get("total").and_then(Value::as_u64), Some(1), "上界应排除最新的");
    }

    #[test]
    fn query_filters_by_kind() {
        let _iso = Isolated::new("records-by-kind");
        let now = now_ms();
        push_at(now, "a", KIND_TASK, "签到", 0);
        push_at(now, "a", KIND_CREDIT, "消耗", -10);

        let v = query_records("", 0, 0, &[KIND_CREDIT.to_string()], 100);
        assert_eq!(v.get("total").and_then(Value::as_u64), Some(1));
        let recs = v.get("records").and_then(Value::as_array).unwrap();
        assert_eq!(recs[0].get("kind").and_then(Value::as_str), Some(KIND_CREDIT));
    }

    #[test]
    fn query_returns_newest_first() {
        let _iso = Isolated::new("records-order");
        let now = now_ms();
        push_at(now - 5000, "a", KIND_TASK, "旧", 0);
        push_at(now, "a", KIND_TASK, "新", 0);
        let v = query_records("", 0, 0, &[], 100);
        let recs = v.get("records").and_then(Value::as_array).unwrap();
        assert_eq!(
            recs[0].get("title").and_then(Value::as_str),
            Some("新"),
            "最新的应排最前"
        );
    }

    #[test]
    fn query_summary_uses_full_set_not_limited() {
        // 概览必须基于过滤后的全量，否则「共 N 条」会随 limit 变化，用户会困惑
        let _iso = Isolated::new("records-summary");
        let now = now_ms();
        for i in 0..10 {
            push_at(now + i, "a", KIND_CREDIT, "消耗", -1);
        }
        let v = query_records("", 0, 0, &[], 3); // limit=3
        assert_eq!(
            v.get("records").and_then(Value::as_array).map(Vec::len),
            Some(3)
        );
        assert_eq!(
            v.get("total").and_then(Value::as_u64),
            Some(10),
            "总数应为 10 而非 3"
        );
        assert_eq!(
            v.pointer("/summary/creditNet").and_then(Value::as_i64),
            Some(-10),
            "净变化应为全量 -10"
        );
    }

    #[test]
    fn query_summary_separates_kinds() {
        let _iso = Isolated::new("records-summary-kinds");
        let now = now_ms();
        push_at(now, "a", KIND_TASK, "签到", 0);
        push_at(now, "a", KIND_CREDIT, "增长", 50);
        push_at(now, "a", KIND_TOKEN, "Token", 1234);
        let v = query_records("", 0, 0, &[], 100);
        assert_eq!(
            v.pointer("/summary/taskCount").and_then(Value::as_u64),
            Some(1)
        );
        assert_eq!(
            v.pointer("/summary/creditNet").and_then(Value::as_i64),
            Some(50)
        );
        assert_eq!(
            v.pointer("/summary/tokenSum").and_then(Value::as_i64),
            Some(1234)
        );
    }

    #[test]
    fn query_reports_retention_days() {
        let _iso = Isolated::new("records-retention");
        let v = query_records("", 0, 0, &[], 100);
        assert_eq!(
            v.get("retentionDays").and_then(Value::as_i64),
            Some(crate::modules::config::RECORD_RETENTION_DEFAULT_DAYS),
            "应回报真实保留天数，前端日期筛选依赖它"
        );
    }

    #[test]
    fn query_on_empty_store_returns_empty_not_error() {
        let _iso = Isolated::new("records-empty");
        let v = query_records("", 0, 0, &[], 100);
        assert_eq!(v.get("total").and_then(Value::as_u64), Some(0));
        assert_eq!(
            v.get("records").and_then(Value::as_array).map(Vec::len),
            Some(0)
        );
    }

    // -----------------------------------------------------------------------
    // 三类记录写入（供视图分组展示）
    // -----------------------------------------------------------------------

    #[test]
    fn add_helpers_write_expected_kinds() {
        let _iso = Isolated::new("records-add-helpers");
        add_task_record("a", "na", "自动签到", "success", "");
        add_credit_record("a", "na", "积分消耗", -12, "", Some(CREDIT_SOURCE_CONSUME));
        add_token_record("a", "na", "glm-5.2", 1234, "");

        let v = query_records("a", 0, 0, &[], 100);
        assert_eq!(v.get("total").and_then(Value::as_u64), Some(3));
        assert_eq!(
            v.pointer("/summary/creditNet").and_then(Value::as_i64),
            Some(-12)
        );
        assert_eq!(
            v.pointer("/summary/tokenSum").and_then(Value::as_i64),
            Some(1234)
        );
        // Token 记录的标题应带模型名，便于用户区分调用来源
        let recs = v.get("records").and_then(Value::as_array).unwrap();
        let token_rec = recs
            .iter()
            .find(|r| r.get("kind").and_then(Value::as_str) == Some(KIND_TOKEN))
            .expect("应有 token 记录");
        assert!(
            token_rec
                .get("title")
                .and_then(Value::as_str)
                .unwrap_or("")
                .contains("glm-5.2"),
            "Token 记录标题应含模型名"
        );
    }

    // -----------------------------------------------------------------------
    // 来源（source）：积分记录必须能看出「为什么变分」
    // -----------------------------------------------------------------------

    #[test]
    fn credit_record_persists_source() {
        let _iso = Isolated::new("records-credit-source");
        add_credit_record("a", "na", "积分增长 · 额度发放", 100, "余额 +100", Some(CREDIT_SOURCE_GRANT));

        let v = query_records("a", 0, 0, &[KIND_CREDIT.to_string()], 100);
        let recs = v.get("records").and_then(Value::as_array).unwrap();
        assert_eq!(
            recs[0].get("source").and_then(Value::as_str),
            Some(CREDIT_SOURCE_GRANT),
            "来源必须被写入并可从查询结果读回"
        );
    }

    #[test]
    fn credit_record_without_source_omits_the_key() {
        // 调用方没给来源时不该凭空写一个 null：Go 侧按原始字节透传，
        // 多余的键只会让两边 diff 变噪声。
        let _iso = Isolated::new("records-credit-no-source");
        add_credit_record("a", "na", "积分调整", 5, "", None);

        let v = query_records("a", 0, 0, &[], 100);
        let recs = v.get("records").and_then(Value::as_array).unwrap();
        assert!(
            recs[0].get("source").is_none(),
            "无来源时不应出现 source 键，实际: {:?}",
            recs[0].get("source")
        );
    }

    #[test]
    fn blank_source_is_treated_as_absent() {
        // 空串/空白与 None 同义：否则前端会拿到 "" 并渲染成空徽标。
        let _iso = Isolated::new("records-credit-blank-source");
        add_credit_record("a", "na", "t", 5, "", Some("   "));
        let v = query_records("a", 0, 0, &[], 100);
        let recs = v.get("records").and_then(Value::as_array).unwrap();
        assert!(recs[0].get("source").is_none(), "空白来源应被规整为「无」");
    }

    #[test]
    fn task_and_token_records_never_carry_source() {
        // 来源只对积分变化有意义：任务标题本身就是任务名，Token 的标题带模型名。
        let _iso = Isolated::new("records-no-source-other-kinds");
        add_task_record("a", "na", "自动签到", "success", "");
        add_token_record("a", "na", "glm-5.2", 10, "");
        let v = query_records("a", 0, 0, &[], 100);
        for r in v.get("records").and_then(Value::as_array).unwrap() {
            assert!(r.get("source").is_none(), "非积分记录不该有 source: {r:?}");
        }
    }

    #[test]
    fn legacy_records_without_source_still_query_and_normalize() {
        // **老记录兼容**：升级前写入的积分记录没有 source 字段。
        // 它们必须照常被查询返回（不能被过滤掉、也不能让解析失败退化成空集），
        // 且缺失字段不能被补成 null 之类的值 —— 前端靠「有没有这个键」决定渲不渲染。
        let _iso = Isolated::new("records-legacy-no-source");
        let now = now_ms();
        let legacy = json!([
            {
                "ts": now, "accountId": "old", "accountName": "老账号",
                "kind": KIND_CREDIT, "title": "积分增长", "result": "success",
                "amount": 100, "detail": ""
            }
        ]);
        atomic_write(
            &account_records_file(),
            &serde_json::to_string_pretty(&legacy).unwrap(),
        )
        .expect("写入老记录应成功");

        let v = query_records("old", 0, 0, &[], 100);
        assert_eq!(v.get("total").and_then(Value::as_u64), Some(1), "老记录必须仍可查到");
        let recs = v.get("records").and_then(Value::as_array).unwrap();
        assert_eq!(recs[0].get("title").and_then(Value::as_str), Some("积分增长"));
        assert!(
            recs[0].get("source").is_none(),
            "老记录不该被凭空补出来源字段"
        );
    }

    #[test]
    fn mixed_legacy_and_new_records_coexist() {
        // 真实场景：文件里既有升级前的老记录，也有带来源的新记录。
        // 查询必须同时返回两者，且新记录的 source 不受老记录影响。
        let _iso = Isolated::new("records-mixed-source");
        let now = now_ms();
        let legacy = json!([
            {
                "ts": now - 1000, "accountId": "a", "accountName": "n",
                "kind": KIND_CREDIT, "title": "积分增长", "result": "success",
                "amount": 50, "detail": ""
            }
        ]);
        atomic_write(
            &account_records_file(),
            &serde_json::to_string_pretty(&legacy).unwrap(),
        )
        .unwrap();
        add_credit_record("a", "n", "积分增长 · 额度发放", 100, "", Some(CREDIT_SOURCE_GRANT));

        let v = query_records("a", 0, 0, &[KIND_CREDIT.to_string()], 100);
        assert_eq!(v.get("total").and_then(Value::as_u64), Some(2));
        let recs = v.get("records").and_then(Value::as_array).unwrap();
        // 倒序：新记录在前
        assert_eq!(recs[0].get("source").and_then(Value::as_str), Some(CREDIT_SOURCE_GRANT));
        assert!(recs[1].get("source").is_none(), "老记录仍应无 source");
        // 净变化把两者都算进去
        assert_eq!(v.pointer("/summary/creditNet").and_then(Value::as_i64), Some(150));
    }

    // -----------------------------------------------------------------------
    // 回填：历史签到日志 → 账号记录
    // -----------------------------------------------------------------------

    #[test]
    fn backfill_imports_checkin_logs() {
        let _iso = Isolated::new("records-backfill");
        let now = now_ms();
        crate::modules::config::add_checkin_log(&json!({
            "accountId": "a", "email": "na", "result": "success", "ts": now,
        }));

        let added = backfill_from_checkin_logs().expect("回填应成功");
        assert_eq!(added, 1);

        let v = query_records("a", 0, 0, &[], 100);
        assert_eq!(v.get("total").and_then(Value::as_u64), Some(1));
        let recs = v.get("records").and_then(Value::as_array).unwrap();
        assert_eq!(
            recs[0].get("title").and_then(Value::as_str),
            Some("自动签到")
        );
    }

    #[test]
    fn backfill_is_idempotent() {
        // 关键：重复回填不能产生重复记录（前端每次打开页面都可能调用）
        let _iso = Isolated::new("records-backfill-idem");
        let now = now_ms();
        crate::modules::config::add_checkin_log(&json!({
            "accountId": "a", "email": "na", "result": "success", "ts": now,
        }));

        let first = backfill_from_checkin_logs().expect("首次回填应成功");
        let second = backfill_from_checkin_logs().expect("二次回填应成功");
        assert_eq!(first, 1);
        assert_eq!(second, 0, "二次回填不应新增");

        let v = query_records("", 0, 0, &[], 100);
        assert_eq!(v.get("total").and_then(Value::as_u64), Some(1), "不应有重复");
    }
}
