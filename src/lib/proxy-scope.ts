import type { GithubConfig, ProxyScope } from "./types";

/**
 * 代理三个开关的**默认值**，也是「配置里没有 proxy_scope 字段」时的取值。
 *
 * 刻意**不是**字面意义上的「全开」。原因（改这里之前务必读完）：
 *
 * 本次改动之前，国服走的是 Go 的 `http.ProxyFromEnvironment`，**根本不吃**
 * 用户在设置页里填的那个显式代理；只有国际版吃（见 Go 侧 `SetProxy` 的区域分流）。
 * 所以「缺失 → 全开（cn=true）」会让所有既有用户升级后国服被**新绕进**代理 ——
 * 与所有者上一轮明确要求的「代理只对国际版生效，别让国内也走代理流量」相反。
 *
 * 取默认值 = 升级前后逐字一致：
 *   - github=true → 更新检查 / 安装包下载照旧走代理（**代理不会静默失效**）；
 *   - intl=true   → 国际版照旧走代理（国内直连实测 wsarecv 超时）；
 *   - cn=false    → 国服照旧直连（不新绕代理）。
 *
 * 与「全开」的唯一差别只有国服那一格，且改这一行即可翻转 —— 但它会改变
 * 已在运行的既有行为，属于产品决策，不是实现细节。
 */
export const DEFAULT_PROXY_SCOPE: ProxyScope = {
  github: true,
  cn: false,
  intl: true,
};

/**
 * 从后端配置里取三个开关，逐键兜底成默认值。
 *
 * 为什么逐键兜底而不是「整块缺失才兜底」：后端可能从更早的版本升级而来
 *（只写了部分键），配置文件也可能被手工编辑过。任何残缺形状都不该让某一格
 * 意外变成 false —— 那会让国际版的代理静默消失，现象是「升级后国际版账号
 * 开始超时」，而用户从界面上完全看不出原因。
 *
 * 只认真正的布尔值：字符串 "false" 这类脏值一律回落默认值（不猜），
 * 与后端 `proxy_scope_of` 的口径一致。
 */
export function proxyScopeOf(config: GithubConfig | null | undefined): ProxyScope {
  const raw = config?.proxy_scope;
  const flag = (key: keyof ProxyScope): boolean => {
    const value = raw?.[key];
    return typeof value === "boolean" ? value : DEFAULT_PROXY_SCOPE[key];
  };
  return { github: flag("github"), cn: flag("cn"), intl: flag("intl") };
}

/** 三个开关在界面上的文案（标题 + 描述），集中一处避免与保存提示漂移。 */
export const PROXY_SCOPE_FIELDS: ReadonlyArray<{
  key: keyof ProxyScope;
  /** 输入框 id / htmlFor，供测试与无障碍定位。 */
  id: string;
  label: string;
  description: string;
}> = [
  {
    key: "github",
    id: "proxy-scope-github",
    label: "Github",
    description: "检查更新与下载安装包时使用代理。默认开启；GitHub 在国内通常需要它。",
  },
  {
    key: "cn",
    id: "proxy-scope-cn",
    label: "国内版",
    description:
      "国服账号的上游请求走代理。国内直连通常更快，一般不需要开；关闭后是真直连，连 HTTPS_PROXY 等环境变量代理也不使用。",
  },
  {
    key: "intl",
    id: "proxy-scope-intl",
    label: "国际版",
    description: "国际版账号的上游请求走代理。国内直连不稳定，建议开启。",
  },
];

/**
 * 拼一句如实的保存提示（只说用户**当前**真正启用的范围）。
 *
 * 为什么必须按状态生成而不是写死一句：用户关掉国际版之后仍看到
 * 「国际版的上游请求会使用它」会以为开关没生效 —— 那是界面在撒谎。
 * 三者全关时直接说明「全部直连」，并提示地址仍保留（不是被清空了）。
 */
export function proxyScopeSummary(scope: ProxyScope): string {
  const on = PROXY_SCOPE_FIELDS.filter((f) => scope[f.key]).map((f) => f.label);
  if (on.length === 0) {
    return "当前三个范围都未使用代理（更新检查、国内版、国际版上游请求全部直连）";
  }
  if (on.length === PROXY_SCOPE_FIELDS.length) {
    return "代理已保存：更新检查与安装包下载、国内版与国际版账号的上游请求都会使用它";
  }
  return `代理已保存：仅${on.join("、")}使用它，其余范围直连`;
}
