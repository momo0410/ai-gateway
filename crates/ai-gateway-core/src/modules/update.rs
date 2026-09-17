//! 自动更新：检查公开 GitHub Releases 版本 + 更新源配置。
//!
//! 对照 server.py `load_github_config` / `save_github_config` /
//! `compare_versions` / `update_check`。下载安装走 tauri-plugin-updater（整包更新）。
//!
//! 版本检查不走 GitHub API（避免 60 次/小时/IP 限流）：
//! 1. 主端点：release 资产的 updater manifest（下载不计 API 配额）；
//! 2. 兜底端点：`/releases/latest` 的 302 `Location` 头解析 tag；
//! 3. 成功结果进程级缓存 6 小时，缓存命中不发网络请求。

use serde_json::{json, Value};
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Mutex;

use crate::modules::config::{
    atomic_write, http_request_raw, http_request_with_proxy, now_secs, store_dir,
};

/// 应用当前版本（来自 Cargo.toml package.version）。
pub const APP_VERSION: &str = env!("CARGO_PKG_VERSION");
/// 本整合版仓库所有者（与上游 changexbc/workbuddy-switch 区分开）。
pub const GITHUB_OWNER: &str = "momo0410";
/// 仓库名必须与 git 远端一致。应用显示名已改为 AI Gateway，GitHub 仓库
/// 也已在 2026-09-16 拆分为独立的 momo0410/ai-gateway（老仓库
/// workbuddy-switch-gateway 回退到 v0.8.5，只维护 0.8.x 线）。
///
/// 两仓库的 `releases/latest` 是**两个不同的指针**：写错会让检查更新与
/// 自动更新全部 404（v1.0.0 就踩过这个坑，见下方单测），或者把 1.x 用户
/// 降级回老仓库的 0.8.x。
pub const GITHUB_REPO: &str = "ai-gateway";

/// 成功结果缓存有效期（6 小时）。自动轮询（30 分钟）命中缓存，不发网络请求；
/// 设置页手动检查传 force=true 绕过缓存强制刷新。
const CACHE_TTL_SECS: i64 = 6 * 60 * 60;

/// 进程级内存缓存，只缓存 ok=true 的结果；失败不写缓存。
struct CachedCheck {
    checked_at: i64,
    value: Value,
}

static CACHE: Mutex<Option<CachedCheck>> = Mutex::new(None);

pub fn github_config_file() -> PathBuf {
    store_dir().join("github_config.json")
}

/// 代理的**作用范围**（三个独立开关，对应 `github_config.json` 的 `proxy_scope`）。
///
/// 为什么把「一个代理地址」拆成三个开关：同一个地址对不同用途的收益完全不同 ——
/// GitHub（检查更新 / 下载安装包）在国内必须走代理；国际版上游（workbuddy.ai）
/// 国内直连实测 wsarecv 超时，也需要；而国服上游（codebuddy.cn /
/// copilot.tencent.com）直连即通，绕进代理只会多一跳延迟、多一个故障面
///（代理一挂，本来好好的国服账号跟着不可用）。
///
/// 地址仍然只填一次（用户不该填三遍），三个开关只决定「哪些用途使用它」。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ProxyScope {
    /// 检查更新 / 下载安装包是否使用代理。
    pub github: bool,
    /// 国服账号（*.workbuddy.cn / *.codebuddy.cn / copilot.tencent.com）的上游请求是否使用代理。
    pub cn: bool,
    /// 国际版账号（*.workbuddy.ai / *.codebuddy.ai）的上游请求是否使用代理。
    pub intl: bool,
}

/// 三个开关的**默认值**，也是「老配置没有 `proxy_scope` 键」时的取值。
///
/// 刻意**不是**字面意义上的「全开」。原因（改这里之前务必读完）：
///
/// 本次改动之前，国服走的是 `http.ProxyFromEnvironment`（Go 侧 `newTransport(nil)`），
/// **根本不吃**用户在设置页填的那个显式代理；只有国际版吃（见 Go 侧 `SetProxy` 的
/// 区域分流）。所以「缺失 → 全开（cn=true）」会**把国服新绕进显式代理** ——
/// 那正是所有者上一轮明确要求消除的行为（「代理只对国际版生效，别让国内也走
/// 代理流量」），也违背「升级不得改变既有行为」这条硬要求。
///
/// 取默认值 = 升级前后逐字一致：
///   - github=true  → 更新检查 / 安装包下载照旧走代理（**代理不会静默失效**）；
///   - intl=true    → 国际版照旧走代理（国内直连不稳，实测 wsarecv 超时）；
///   - cn=false     → 国服照旧直连（不新绕代理）。
///
/// 与「全开」的唯一差别只有国服那一格，且改这一行即可翻转 —— 但它会改变
/// 已在运行的既有行为，属于产品决策，不是实现细节。
impl Default for ProxyScope {
    fn default() -> Self {
        Self {
            github: true,
            cn: false,
            intl: true,
        }
    }
}

/// 从配置值里解析三个开关（纯函数，便于单测）。
///
/// 逐键回落默认值，而不是「整块缺失才回落」：用户可能只改了其中一个开关，
/// 而老配置里可能只有部分键（更早的中间版本、或手工编辑过配置文件）。
/// 逐键兜底能保证**任何**残缺形状都不会让某一格意外变成 false ——
/// 那会让国际版的代理静默消失，现象是「升级后国际版账号开始超时」。
pub fn proxy_scope_of(cfg: &Value) -> ProxyScope {
    let d = ProxyScope::default();
    let block = cfg.get("proxy_scope");
    let flag = |key: &str, fallback: bool| -> bool {
        block
            .and_then(|b| b.get(key))
            .and_then(Value::as_bool)
            .unwrap_or(fallback)
    };
    ProxyScope {
        github: flag("github", d.github),
        cn: flag("cn", d.cn),
        intl: flag("intl", d.intl),
    }
}

/// 当前生效的代理作用范围（读 `github_config.json`）。
pub fn proxy_scope() -> ProxyScope {
    proxy_scope_of(&load_github_config())
}

/// 该配置里**已配置**的代理地址（空串 / 缺失 → None）。
pub fn configured_proxy(cfg: &Value) -> Option<String> {
    cfg.get("proxy")
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .map(str::to_string)
}

/// 读取更新源配置（兼容旧配置文件，但永不返回 token）。
pub fn load_github_config() -> Value {
    let mut owner = GITHUB_OWNER.to_string();
    let mut repo = GITHUB_REPO.to_string();
    let mut proxy = String::new();
    let mut scope = ProxyScope::default();
    let f = github_config_file();
    let mut should_normalize = false;
    if f.exists() {
        if let Ok(text) = std::fs::read_to_string(&f) {
            if let Ok(v) = serde_json::from_str::<Value>(&text) {
                if let Some(value) = v.get("owner").and_then(|v| v.as_str()) {
                    if !value.trim().is_empty() {
                        owner = value.to_string();
                    }
                }
                if let Some(value) = v.get("repo").and_then(|v| v.as_str()) {
                    if !value.trim().is_empty() {
                        repo = value.to_string();
                    }
                }
                if let Some(value) = v.get("proxy").and_then(|v| v.as_str()) {
                    proxy = value.trim().to_string();
                }
                // 缺失 proxy_scope = 老配置 → 取默认值（升级前后行为一致，见 ProxyScope）。
                // 出口恒定带这三个键：界面不必为「这次拿到的是不是缺失」分两条渲染路径。
                scope = proxy_scope_of(&v);
                should_normalize = v.get("token").is_some();
            }
        }
    }
    // 历史配置可能指向上游仓库（changexbc/workbuddy-switch）—— 那与本项目
    // 无关，必须迁走，否则会把用户更新成上游版本而丢失网关功能。
    //
    // **只迁移上游**，不再迁移 momo0410/workbuddy-switch-gateway：该项目现已
    // 拆为两个仓库，老仓库（workbuddy-switch-gateway）是**合法的另一个更新源**
    // （0.8.x 线）。曾经把本仓库旧名也一并迁移，是因为当年改名没改仓库；如今
    // 仓库真的拆开了，再迁移会把「刻意留在 0.8.x 的用户」静默拉回 1.x。
    //
    // 注意：更名脚本曾把这里两个不同的仓库名都替换成了 "ai-gateway"，
    // 使条件退化成恒等重复，于是上游配置根本不会被迁移。这里保持各自真实名字。
    if owner == "changexbc" && repo == "workbuddy-switch" {
        repo = GITHUB_REPO.to_string();
        should_normalize = true;
    }
    let normalized = json!({
        "owner": owner,
        "repo": repo,
        "proxy": proxy,
        "proxy_scope": {
            "github": scope.github,
            "cn": scope.cn,
            "intl": scope.intl,
        },
    });
    if should_normalize {
        let _ = atomic_write(
            &f,
            &serde_json::to_string_pretty(&normalized).unwrap_or_default(),
        );
    }
    normalized
}

/// 保存更新源配置；公开仓库不需要也不保存 GitHub token。
///
/// **必须一并保存 `proxy_scope`**：本函数写的是**整份** github_config.json
///（`atomic_write` 全量覆盖，不是 merge）。早先它只写 owner/repo/proxy 三个键，
/// 于是任何一次「保存代理地址」都会把 `proxy_scope` 静默抹掉；而读取侧对此的
/// 兜底是「缺失 → 默认值」，表现成「用户把国际版开关关掉、一保存地址又自己开了」。
/// 三个开关必须在这里原样透传。
pub fn save_github_config(cfg: &Value) -> std::io::Result<()> {
    let owner = cfg
        .get("owner")
        .and_then(|v| v.as_str())
        .filter(|v| !v.trim().is_empty())
        .unwrap_or(GITHUB_OWNER);
    let repo = cfg
        .get("repo")
        .and_then(|v| v.as_str())
        .filter(|v| !v.trim().is_empty())
        .unwrap_or(GITHUB_REPO);
    let proxy = cfg
        .get("proxy")
        .and_then(|v| v.as_str())
        .map(str::trim)
        .unwrap_or("");
    // 缺失 → 默认值（而不是「全 false」）：保存接口允许只传部分字段
    //（例如只改代理地址的旧版界面），缺键时按默认值补齐，与 load 同一口径。
    let scope = proxy_scope_of(cfg);
    let clean = json!({
        "owner": owner,
        "repo": repo,
        "proxy": proxy,
        "proxy_scope": {
            "github": scope.github,
            "cn": scope.cn,
            "intl": scope.intl,
        },
    });
    std::fs::create_dir_all(store_dir())?;
    atomic_write(
        &github_config_file(),
        &serde_json::to_string_pretty(&clean).unwrap_or_default(),
    )
}

fn version_tuple(v: &str) -> Vec<i64> {
    v.trim_start_matches('v')
        .split('.')
        .filter_map(|x| x.parse::<i64>().ok())
        .collect()
}

/// 版本比较：a > b 返回 1，a < b 返回 -1，相等返回 0。
pub fn compare_versions(a: &str, b: &str) -> i64 {
    let ta = version_tuple(a);
    let tb = version_tuple(b);
    for i in 0..ta.len().max(tb.len()) {
        let x = ta.get(i).copied().unwrap_or(0);
        let y = tb.get(i).copied().unwrap_or(0);
        if x != y {
            return if x > y { 1 } else { -1 };
        }
    }
    0
}

/// updater manifest 候选 URL（按优先级）。
///
/// 1. 合并后的 `latest.json`（含各平台）；
/// 2. 当前系统的 `latest-<os>-<arch>.json`。
pub fn updater_manifest_urls(owner: &str, repo: &str, os: &str, arch: &str) -> Vec<String> {
    let mut urls = vec![format!(
        "https://github.com/{owner}/{repo}/releases/latest/download/latest.json"
    )];
    urls.push(format!(
        "https://github.com/{owner}/{repo}/releases/latest/download/latest-{os}-{arch}.json"
    ));
    urls.dedup();
    urls
}

/// 主端点：拉取 updater manifest。成功返回解析后的 JSON（含 version / pub_date），
/// 失败返回可读错误信息。
async fn fetch_manifest_version(
    owner: &str,
    repo: &str,
    proxy: Option<&str>,
) -> Result<Value, String> {
    let mut headers = HashMap::new();
    headers.insert("Accept".to_string(), "application/json".to_string());
    headers.insert("User-Agent".to_string(), "ai-gateway".to_string());
    let mut last_err = "更新清单解析失败".to_string();
    for url in updater_manifest_urls(owner, repo, std::env::consts::OS, std::env::consts::ARCH) {
        let resp = http_request_with_proxy(&url, "GET", None, Some(&headers), proxy).await;
        let version = resp.get("version").and_then(|v| v.as_str()).unwrap_or("");
        if !version.trim().is_empty() {
            return Ok(resp);
        }
        last_err = resp
            .get("message")
            .and_then(|v| v.as_str())
            .unwrap_or("更新清单解析失败")
            .to_string();
    }
    Err(last_err)
}

/// 兜底端点：请求 `/releases/latest`，读 302 `Location` 头（形如
/// `.../releases/tag/v0.1.13`）解析 tag。不跟随重定向，避免拉到 HTML 页面。
/// 成功返回 tag，失败返回可读错误 + code（状态码或 -1）。
async fn fetch_latest_tag(
    owner: &str,
    repo: &str,
    proxy: Option<&str>,
) -> Result<String, (String, i64)> {
    let url = format!("https://github.com/{owner}/{repo}/releases/latest");
    let mut headers = HashMap::new();
    headers.insert("Accept".to_string(), "text/html".to_string());
    headers.insert("User-Agent".to_string(), "ai-gateway".to_string());
    let (status, resp_headers, body) =
        http_request_raw(&url, "GET", None, Some(&headers), proxy, false).await;

    if status == 0 {
        let msg = if body.trim().is_empty() {
            "网络请求失败".to_string()
        } else {
            body
        };
        return Err((msg, -1));
    }
    if status == 404 {
        // 无正式 release 或仓库不存在。
        return Err(("未找到可用的发布版本".to_string(), 404));
    }
    let location = resp_headers
        .iter()
        .find(|(k, _)| k.eq_ignore_ascii_case("location"))
        .map(|(_, v)| v.clone());
    let location = match location {
        Some(location) => location,
        None => {
            return Err((
                format!("无法获取发布页跳转地址（HTTP {status}）"),
                status as i64,
            ))
        }
    };
    let tag = location.rsplit('/').next().unwrap_or("").trim().to_string();
    if tag.is_empty() || !location.contains("/releases/tag/") {
        return Err(("无法解析发布版本标签".to_string(), -1));
    }
    Ok(tag)
}

/// 查询最新 Release，与本地版本对比。对照 server.py `update_check`。
///
/// `force=true` 绕过缓存强制刷新（设置页手动检查）；否则 6 小时内成功结果直接返回，
/// 不发网络请求。主端点 manifest 失败时自动走 302 兜底；两个端点都失败返回可读错误。
pub async fn update_check(proxy: Option<&str>, force: bool) -> Value {
    if !force {
        if let Some(cached) = CACHE.lock().unwrap().as_ref() {
            if now_secs() - cached.checked_at < CACHE_TTL_SECS {
                return cached.value.clone();
            }
        }
    }

    let cfg = load_github_config();
    let owner = cfg
        .get("owner")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    let repo = cfg
        .get("repo")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    let configured_proxy = configured_proxy(&cfg);
    let scope = proxy_scope_of(&cfg);
    // `proxy` 参数 = 调用方（设置页「检查更新」按钮）显式传入的地址。
    // 但**是否使用代理**由 `proxy_scope.github` 决定：用户把 Github 那个开关
    // 关掉之后，手动检查更新也必须直连 —— 否则「关了开关却仍在走代理」，
    // 而这条链路是所有者唯一能自查代理是否生效的入口，骗他代价最大。
    let proxy = if scope.github {
        proxy.or(configured_proxy.as_deref())
    } else {
        None
    };
    let release_url = format!("https://github.com/{owner}/{repo}/releases/latest");
    let current = APP_VERSION.to_string();

    // 主端点：updater manifest（release 资产下载，不计 GitHub API 配额）。
    if let Ok(manifest) = fetch_manifest_version(&owner, &repo, proxy).await {
        let version = manifest
            .get("version")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim();
        let latest = version.strip_prefix('v').unwrap_or(version).to_string();
        let tag = format!("v{latest}");
        let release_name = manifest
            .get("name")
            .and_then(|v| v.as_str())
            .unwrap_or(&tag)
            .to_string();
        let published_at = manifest
            .get("pub_date")
            .and_then(|v| v.as_str())
            .map(|s| s.to_string());
        let value = json!({
            "ok": true,
            "current": current,
            "latest": latest,
            "latestTag": tag,
            "hasUpdate": compare_versions(&latest, &current) > 0,
            "releaseName": release_name,
            "releaseUrl": release_url,
            "publishedAt": published_at,
            "checkedAt": now_secs(),
        });
        *CACHE.lock().unwrap() = Some(CachedCheck {
            checked_at: now_secs(),
            value: value.clone(),
        });
        return value;
    }

    // 兜底端点：`/releases/latest` 的 302 Location 头解析 tag（仅 manifest 失败时）。
    match fetch_latest_tag(&owner, &repo, proxy).await {
        Ok(tag) => {
            let latest = tag.strip_prefix('v').unwrap_or(&tag).to_string();
            let value = json!({
                "ok": true,
                "current": current,
                "latest": latest,
                "latestTag": tag,
                "hasUpdate": compare_versions(&latest, &current) > 0,
                "releaseName": tag.clone(),
                "releaseUrl": release_url,
                "checkedAt": now_secs(),
            });
            *CACHE.lock().unwrap() = Some(CachedCheck {
                checked_at: now_secs(),
                value: value.clone(),
            });
            value
        }
        Err((msg, code)) => json!({
            "ok": false,
            "error": msg,
            "message": format!("{msg}（code={code}）"),
            "releaseUrl": release_url,
        }),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn updater_manifest_urls_lists_merged_then_platform_specific() {
        let urls = updater_manifest_urls("momo0410", "ai-gateway", "windows", "x86_64");
        assert_eq!(
            urls,
            vec![
                "https://github.com/momo0410/ai-gateway/releases/latest/download/latest.json",
                "https://github.com/momo0410/ai-gateway/releases/latest/download/latest-windows-x86_64.json",
            ]
        );
    }

    /// 仓库名写错会让「检查更新」与自动更新同时 404，而这类错误**不会**在
    /// 构建或运行时报错，只会在用户点更新时静默失败（v1.0.0 发布后才发现
    /// 端点指向并不存在的仓库）。所以把仓库名钉在一起：
    /// 本模块常量、tauri.conf.json 的 updater endpoints、前端的 update.ts。
    ///
    /// 2026-09-16 拆分后本条同时防住**反向**错误：把新仓库的端点点回老仓库，
    /// 会让 1.x 用户看到老仓库 0.8.x 的 `releases/latest` 并被降级。
    #[test]
    fn repo_name_stays_in_sync_across_config_and_frontend() {
        const TAURI_CONF: &str = include_str!("../../../../src-tauri/tauri.conf.json");
        const FRONTEND_UPDATE_TS: &str = include_str!("../../../../src/lib/update.ts");
        let expected = format!("github.com/{GITHUB_OWNER}/{GITHUB_REPO}/releases/latest/download/");

        assert!(
            TAURI_CONF.contains(&expected),
            "tauri.conf.json 的 updater endpoints 必须指向 {expected}，否则客户端自动更新会 404"
        );
        assert!(
            FRONTEND_UPDATE_TS.contains(&format!("GITHUB_REPO = \"{GITHUB_REPO}\"")),
            "src/lib/update.ts 的 GITHUB_REPO 必须与 update.rs 的 GITHUB_REPO 一致（{GITHUB_REPO}）"
        );
        // 老仓库名不得再出现在这三个更新端点里：它现在属于 0.8.x 线，
        // 一旦混入就会把 1.x 用户降级（两个仓库的 releases/latest 是不同指针）。
        assert!(
            !expected.contains("workbuddy-switch-gateway"),
            "AI Gateway 的更新源不能指向老的 workbuddy-switch-gateway（那是 0.8.x 线）"
        );
    }

    /// 脚本里的默认仓库名必须与常量一致，否则本地发版会把清单写成另一个
    /// 仓库的 URL（构建期看不出来，只在客户端更新时静默失败）。
    #[test]
    fn gen_update_json_default_repo_matches_constant() {
        const GEN_UPDATE_JSON: &str = include_str!("../../../../scripts/gen-update-json.sh");
        assert!(
            GEN_UPDATE_JSON.contains(&format!("REPO=\"${{2:-{GITHUB_REPO}}}\"")),
            "scripts/gen-update-json.sh 的默认 REPO 必须与 GITHUB_REPO（{GITHUB_REPO}）一致"
        );
    }

    #[test]
    fn compare_versions_orders_semver() {
        assert_eq!(compare_versions("0.1.18", "0.1.18"), 0);
        assert_eq!(compare_versions("0.1.19", "0.1.18"), 1);
        assert_eq!(compare_versions("v0.1.17", "0.1.18"), -1);
    }

    // -----------------------------------------------------------------------
    // 代理的三个独立开关（proxy_scope）
    //
    // 本轮把「一个代理地址」拆成三个开关。三条必须守住的线：
    //   1. 老配置（没有 proxy_scope 键）= 逐字保持现状，**代理不得静默失效**；
    //   2. 三个开关能独立读写、能往返；
    //   3. 保存接口写的是**整份**文件，不能把 proxy_scope 抹掉。
    // -----------------------------------------------------------------------

    /// 默认值：github 开、国际版开、国服关。
    ///
    /// 为什么国服是关而不是开（这条最容易被后人"顺手改成全开"）：本次改动之前
    /// 国服走的是 http.ProxyFromEnvironment，**根本不吃**用户填的显式代理。
    /// 缺省成 true 会把所有既有用户的国服流量新绕进代理 —— 与所有者上一轮
    /// 「别让国内也走代理流量」的要求相反，且升级时不会有任何提示。
    #[test]
    fn proxy_scope_default_is_github_and_intl_on_cn_off() {
        let d = ProxyScope::default();
        assert!(d.github, "Github 默认必须是开的（所有者明确要求，且保持现状）");
        assert!(d.intl, "国际版默认必须是开的：国内直连实测 wsarecv 超时，关掉会劣化");
        assert!(
            !d.cn,
            "国服默认必须是关的：改动前国服不吃显式代理，开 = 升级后把国服新绕进代理"
        );
    }

    /// 老配置（没有 proxy_scope 键）= 默认值 = 现状。「代理不得静默失效」的落点。
    ///
    /// 这是本改动**最关键**的一条兼容断言：所有既有 github_config.json 都没有
    /// 这个键。若缺失被读成「全关」，用户升级后国际版代理会静默消失 ——
    /// 现象是「国际版账号突然开始超时」，而他从界面上完全看不出原因。
    #[test]
    fn proxy_scope_missing_key_falls_back_to_defaults() {
        let legacy = json!({"owner": "momo0410", "repo": "ai-gateway", "proxy": "http://127.0.0.1:7890"});
        let got = proxy_scope_of(&legacy);
        assert_eq!(
            got,
            ProxyScope::default(),
            "老配置缺 proxy_scope 时必须取默认值（而不是全关：那会让代理静默失效）"
        );
        // 逐键断言，让失败信息直接指出是哪一格错了。
        assert!(got.github, "老配置的 GitHub 代理必须仍然生效");
        assert!(got.intl, "老配置的国际版代理必须仍然生效（否则升级后国际版开始超时）");
        assert!(!got.cn, "老配置的国服必须仍然直连（升级不得改变既有行为）");
    }

    /// 残缺形状（null / 空对象 / 类型不对）逐键回落默认值，且**不 panic**。
    ///
    /// 为什么类型不对也不 panic：这份配置可能被用户手工编辑过。一个键写错就
    /// 让整个「检查更新」功能崩掉，比回落默认值糟糕得多（默认值本身就是现状）。
    #[test]
    fn proxy_scope_partial_and_bad_shapes_degrade() {
        let d = ProxyScope::default();
        // 整块为 null / 空对象 / 非对象：全部取默认。
        for block in [json!(null), json!({}), json!([]), json!("x"), json!(7)] {
            let cfg = json!({"proxy_scope": block});
            assert_eq!(
                proxy_scope_of(&cfg),
                d,
                "proxy_scope={block} 应整块取默认值"
            );
        }
        // 只有部分键：写了的生效，没写的取默认（而不是一律 false）。
        let only_cn = proxy_scope_of(&json!({"proxy_scope": {"cn": true}}));
        assert!(only_cn.cn, "写了的键必须生效");
        assert!(only_cn.intl, "没写的 intl 应取默认 true，不能变成 false");
        assert!(only_cn.github, "没写的 github 应取默认 true");

        let only_intl_off = proxy_scope_of(&json!({"proxy_scope": {"intl": false}}));
        assert!(!only_intl_off.intl, "显式 false 必须生效");
        assert!(!only_intl_off.cn, "没写的 cn 取默认 false");

        // 类型不对的键回落默认，不 panic；同一块里其它键照常生效。
        let mixed = proxy_scope_of(&json!({"proxy_scope": {"cn": "true", "intl": false}}));
        assert!(!mixed.cn, "类型不对的 cn 应回落默认（而不是猜成 true）");
        assert!(!mixed.intl, "同一块里类型正确的键仍要生效");
    }

    /// 三个开关能独立读出（四种组合逐一钉住）。
    #[test]
    fn proxy_scope_reads_all_four_combinations() {
        for (cn, intl) in [(false, false), (false, true), (true, false), (true, true)] {
            let cfg = json!({"proxy_scope": {"github": true, "cn": cn, "intl": intl}});
            let got = proxy_scope_of(&cfg);
            assert_eq!(
                (got.cn, got.intl),
                (cn, intl),
                "组合 cn={cn} intl={intl} 必须逐字读出"
            );
        }
    }

    /// 保存 → 读回：三个开关往返不丢，且**整份文件**都写对了。
    ///
    /// 为什么必须真的过一遍文件：`save_github_config` 用 atomic_write **全量覆盖**
    /// 整份 github_config.json。早先它只写 owner/repo/proxy 三个键，于是任何一次
    /// 「保存代理地址」都会把 proxy_scope 静默抹掉 —— 而读取侧对缺失的兜底是
    /// 默认值，表现成「用户把国际版开关关掉、一保存地址又自己开了」。
    /// 只测 `proxy_scope_of` 这个纯函数是发现不了那一层的。
    #[test]
    fn proxy_scope_survives_save_and_load_round_trip() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("proxy-scope-roundtrip");

        let submitted = json!({
            "owner": "momo0410",
            "repo": "ai-gateway",
            "proxy": "http://127.0.0.1:7897",
            "proxy_scope": {"github": false, "cn": true, "intl": false},
        });
        save_github_config(&submitted).expect("save");

        // 读回：三个开关必须逐字保留。
        let loaded = load_github_config();
        assert_eq!(
            proxy_scope_of(&loaded),
            ProxyScope { github: false, cn: true, intl: false },
            "保存后三个开关必须原样读回（save 全量覆盖，漏写就会被抹掉）"
        );
        assert_eq!(
            loaded.get("proxy").and_then(Value::as_str),
            Some("http://127.0.0.1:7897"),
            "代理地址同时要保住"
        );
        // 落盘的 JSON 文本里也必须真的有这个键（而不是靠读取侧兜底补出来的）。
        let text = std::fs::read_to_string(github_config_file()).expect("read file");
        assert!(
            text.contains("proxy_scope"),
            "proxy_scope 必须真的落盘，否则下次保存就会丢：{text}"
        );
    }

    /// 保存接口**缺 proxy_scope**（旧界面 / 只改地址）时不得把开关清成全关。
    ///
    /// 与 load 同一口径：缺失 → 默认值。若缺键被当作「全 false」，那么一个
    /// 只会保存 owner/repo/proxy 的旧版界面，只要用户点一次「保存代理」，
    /// 国际版的代理就被关掉了 —— 用户完全不知道发生了什么。
    #[test]
    fn proxy_scope_absent_on_save_falls_back_to_defaults() {
        let _iso = crate::modules::config::test_isolation::Isolated::new("proxy-scope-save-absent");

        // 旧界面只发这三个键。
        save_github_config(&json!({
            "owner": "momo0410",
            "repo": "ai-gateway",
            "proxy": "http://127.0.0.1:7897",
        }))
        .expect("save");

        let got = proxy_scope_of(&load_github_config());
        assert_eq!(
            got,
            ProxyScope::default(),
            "保存时缺 proxy_scope 必须取默认值（不能清成全关，否则代理静默失效）"
        );
        assert!(got.intl, "国际版开关必须仍为开");
    }

    /// `configured_proxy` 只认非空的代理地址；空串 / 缺失 / 空白一律 None。
    #[test]
    fn configured_proxy_ignores_blank_values() {
        assert_eq!(
            configured_proxy(&json!({"proxy": "http://127.0.0.1:7890"})).as_deref(),
            Some("http://127.0.0.1:7890")
        );
        for v in [json!({"proxy": ""}), json!({"proxy": "   "}), json!({}), json!({"proxy": null})] {
            assert_eq!(configured_proxy(&v), None, "空值不该被当成配了代理: {v}");
        }
        // 首尾空白要 trim：用户从别处复制地址时经常带上空格。
        assert_eq!(
            configured_proxy(&json!({"proxy": "  http://127.0.0.1:7890  "})).as_deref(),
            Some("http://127.0.0.1:7890")
        );
    }
}
