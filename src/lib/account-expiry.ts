// 「账号是不是真的不能用了」的**唯一判定口径**。
//
// 为什么必须集中在一处：账号卡片与兼容网关页都要回答同一个问题
//（「这个号还能不能自己恢复」），此前两处各写一套 —— 卡片比 `expiresAt`，
// 网关页比 `expiresAt`，于是同一个账号在两页同时喊「过期」，而它其实健康。
// 判定一旦分叉，修好一处也挡不住另一处继续误报。
//
// 核心概念：**access token 过期不是故障**。
//
// access token 是短期凭证，过期后宿主（`refresh::ensure_fresh_token`）与网关
// （`auth.Auth.NeedsRefresh`）都会自动拿 refresh token 换一个新的 ——
// 「已过期」恰恰是自动刷新的**触发条件**。所以把 `expiresAt < now` 渲染成
// 警告，等于把「这次刷新还没跑到」说成账号坏了。
//
// 实测（2026-09-16，所有者本机 21 个账号）：把每个 access token 的 JWT `exp`
// 解出来比对账号库 `expiresAt`，21/21 完全一致（毫秒），说明存储与单位都没错；
// 而「刷新一下就恢复正常」正是因为刷新成功后 `expiresAt` 变成了未来时刻。
//
// 真正需要人工介入的只有两种，它们都意味着「已经没有能换新 access token 的凭证」：
//   1. `needsRelogin`：上游明确拒绝（如 12153 session dead），refresh token 已被吊销；
//   2. refresh token 自己也过期了。

/**
 * 判定所需的最小字段集。
 *
 * 账号卡片传完整的 `AccountMeta`，网关页手里是 `GatewayStatusAccount`（网关
 * `/status` 的投影，只有 `needsRelogin`）叠加账号库元信息 `meta`，两边的形状
 * 不同，因此这里只要求这两个字段，由调用方拼好再传进来。
 */
export type ExpiryFields = {
  needsRelogin?: boolean;
  /** refresh token 到期时刻（毫秒）；`null` / `0` 都表示「未知」。 */
  refreshExpiresAt?: number | null;
  /** 上游拒绝的具体原因，仅用于 tooltip。 */
  needsReloginReason?: string | null;
};

/**
 * refresh token 是否**确认**已过期。
 *
 * 必须 `> 0` 才算数：`0` 是「未知」的哨兵值而非 1970 年 ——
 * `auth_file::build_auth_obj` 与 `codebuddy_cn_ide` 的导出在拿不到到期时间时
 * 都会写 `refreshExpiresAt: 0`。若把 0 当成已过期，会给一批本来健康的账号
 * 凭空加上「登录已失效」，等于用一个新误报替换旧误报。
 *
 * `null` / 缺失同理按「未知」处理：本地拿不到到期时间时不应替服务端下结论，
 * 凭证到底还能不能用由上游裁判（对照 `auth_file::credential_freshness`
 * 的同一取向：缺少到期时间的 refresh token 视为可用）。
 */
export function isRefreshTokenExpired(account: ExpiryFields): boolean {
  const exp = account.refreshExpiresAt;
  return typeof exp === "number" && exp > 0 && exp < Date.now();
}

/**
 * 需要人工重新登录时的报警内容；账号仍可自愈时返回 `null`。
 *
 * 返回 `null` 的两种情形，都是**刻意不报**：
 *  - access token 过期但 refresh token 还在 → 下次调用会自动换新，属正常状态；
 *  - 两个时间都未知 → 无法判断，宁可不说，也不误报（服务端才是最终裁判）。
 */
export function accountReloginAlarm(
  account: ExpiryFields,
): { label: string; title: string } | null {
  if (account.needsRelogin) {
    const reason = account.needsReloginReason ? `（${account.needsReloginReason}）` : "";
    return {
      label: "需重新登录",
      title: `上游已拒绝该凭证${reason}，自动刷新无法恢复，请重新登录`,
    };
  }
  if (isRefreshTokenExpired(account)) {
    return {
      label: "登录已失效",
      title: "refresh token 已过期，无法再自动续期，请重新登录",
    };
  }
  return null;
}
