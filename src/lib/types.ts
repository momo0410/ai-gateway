// 与 Rust 后端命令返回结构对齐的类型定义（对照 server.py 各 API 响应）

/** 账号区域键；与后端 `Region::key()` 对齐。 */
export type AccountRegionKey = "cn" | "intl";

export interface AccountMeta {
  id: string;
  /** 服务区域展示名（"国服" / "国际版"），由 domain 后缀推导。 */
  region?: string;
  /** 区域键（"cn" / "intl"），便于样式与筛选。 */
  regionKey?: AccountRegionKey;
  uid: string | null;
  email: string | null;
  nickname: string | null;
  enterpriseName: string | null;
  expiresAt: number | null;
  refreshExpiresAt: number | null;
  refreshedAt: number | null;
  createdAt: number | null;
  needsRelogin: boolean;
  needsReloginReason: string | null;
  /**
   * 用户自定义备注（如「公司号」「备用」）。
   *
   * 为什么需要：授权进来的账号往往只带邮箱/手机号/随机 uid，光看这些认不出
   * 「这是谁的号、干什么用的」。备注只存本地，不参与登录。
   */
  note?: string | null;
  /**
   * 用户手动禁用：不进网关账号池。
   *
   * 注意：禁用的只是「接流量的资格」，签到 / 旅行 / 领奖等养号任务照跑 ——
   * 号暂时不接流量不等于不要额度与连登天数。这与网关内部因冷却/熔断而
   * 临时不可用完全不同：后者会自行恢复，前者只能由用户显式改回。
   */
  disabled?: boolean;
  /** 原始域名（如 www.workbuddy.ai / copilot.tencent.com）—— 排查时比区域标签更具体。 */
  domain?: string | null;
  /** 手机号（国服账号的真实身份线索；其 email 常为空）。 */
  phoneNumber?: string | null;
  /** 账号类型（personal / enterprise）—— 影响可用模型与额度口径。 */
  accountType?: string | null;
}

export interface AppStatus {
  running: boolean;
  authFile: string;
  current: {
    uid: string | null;
    nickname: string | null;
    email: string | null;
  } | null;
  appPath: string;
  version: string;
}

export interface OAuthStartResult {
  loginId: string;
  verificationUri: string;
  expiresIn: number;
  /** 本次登录会话所属区域；由后端回显，缺省视为国服。 */
  region?: AccountRegionKey;
}

export interface OAuthPollResult {
  done: boolean;
  result?: AccountMeta;
  error?: string;
}

/** 导出文件中的完整账号记录（含 token，仅导出命令返回；字段与账号库原始记录一致）。 */
export interface AccountRecord {
  id?: string;
  uid?: string | null;
  nickname?: string | null;
  email?: string | null;
  access_token?: string | null;
  refresh_token?: string | null;
  token_type?: string | null;
  domain?: string | null;
  expiresAt?: number | null;
  refreshExpiresAt?: number | null;
  auth_raw?: unknown;
  profile_raw?: unknown;
  createdAt?: number | null;
  [key: string]: unknown;
}

/** 导入文件账号的脱敏预览（不含 token）。 */
export interface ImportPreviewAccount {
  index: number;
  uid: string | null;
  nickname: string | null;
  email: string | null;
  hasToken: boolean;
}

/** 导入结果计数。 */
export interface ImportResult {
  ok: boolean;
  imported: number;
  skipped: number;
  overwritten: number;
}

/** 本机候选账号的来源类型。 */
export type LocalAccountSource = "current" | "snapshot" | "backup";

/** 本机候选账号的凭证可用性。 */
export type LocalAccountFreshness = "refreshable" | "access_only" | "expired";

/** 本机扫描发现的单个候选账号（不含 token）。 */
export interface LocalImportCandidate {
  /** 本次扫描结果中的序号。 */
  index: number;
  /** 来源文件绝对路径；导入时以此为准（跨扫描稳定）。 */
  path: string;
  /** 账号元数据（脱敏）。 */
  meta: AccountMeta;
  source: LocalAccountSource;
  /** 来源展示名：当前登录 / 历史快照 / 切换备份。 */
  sourceLabel: string;
  freshness: LocalAccountFreshness;
  /** 凭证可用性展示名。 */
  freshnessLabel: string;
  /** 同一账号在本机共有多少份文件（>1 表示还有更旧的重复快照）。 */
  duplicateCount: number;
  /** 是否已在账号库中。 */
  alreadyImported: boolean;
  /** 库中已有该账号，但本机这份凭证更新：导入会覆盖刷新。 */
  updatesStored: boolean;
  /** 来源文件最后修改时间（毫秒）。 */
  modifiedAt: number;
}

/** GET /api/import-local/scan 响应。 */
export interface LocalScanResult {
  ok: boolean;
  candidates: LocalImportCandidate[];
  total: number;
  /** 识别出的认证文件总数（含被去重掉的旧快照）。 */
  filesScanned: number;
  /** 可导入（凭证未完全过期）的候选数。 */
  usable: number;
  /** 认证文件目录。 */
  authDir: string;
  /** 本工具备份目录。 */
  backupDir: string;
}

/** POST /api/import-local/selected 响应。 */
export interface LocalImportResult {
  ok: boolean;
  imported: number;
  /** 新增账号数。 */
  added: number;
  /** 覆盖刷新既有账号数。 */
  updated: number;
  /** 逐个账号的结果明细。 */
  outcomes: Array<{
    name: string;
    region: string;
    source: LocalAccountSource;
    file: string;
    freshness: LocalAccountFreshness;
    updated: boolean;
  }>;
}

export interface Session {
  id: string;
  title: string;
  cwd: string;
  updatedAt: number;
  hasHistory: boolean;
  /** WorkBuddy playground（侧栏「任务」）；缺省视为空间会话。 */
  isPlayground?: boolean;
}

export interface CopyResult {
  id: string;
  newId: string;
  jsonlCopied: boolean;
  mappingWritten: boolean;
  backup: string;
}

export interface SwitchResult {
  ok: boolean;
  account: string;
  backup: string | null;
  sessionCopy?: {
    sourceUid: string;
    targetUid: string;
    copied: CopyResult[];
    errors?: { id: string; error: string }[];
  };
}

export interface CheckinConfig {
  enabled: boolean;
  /** Legacy persisted fields; accepted by the backend but ignored by scheduling. */
  start_hour?: number;
  end_hour?: number;
  keepalive_days: number;
  lazy_refresh_hours: number;
  /**
   * 历史字段：旧版本曾用 `"cn" | "all"` 控制覆盖区域。
   * 自动签到 / 自动旅行现已硬绑定为「仅国服」，后端忽略此字段，
   * 前端不再读取或写入，仅作为兼容旧配置文件保留类型定义。
   */
  region_scope?: "cn" | "all";
}

export interface CheckinLog {
  ts: number;
  accountId: string | null;
  email: string;
  result: string;
  error?: string;
}

export interface CheckinResult {
  result: string;
  error?: string;
}

export interface TravelConfig {
  enabled: boolean;
  /**
   * 历史字段：旧版本曾用 `"cn" | "all"` 控制覆盖区域。
   * 自动旅行现已硬绑定为「仅国服」，后端忽略此字段，
   * 前端不再读取或写入，仅作为兼容旧配置文件保留类型定义。
   */
  region_scope?: "cn" | "all";
}

export type TravelStatusLabel = "untraveled" | "no-buddy" | "traveling" | "finished" | "adopted" | "adopt-threshold";

export interface TravelStatus {
  label: TravelStatusLabel;
  rewardCredit: number | null;
  locationName?: string | null;
  arriveAt?: number | null;
  /** 后端给出的具体说明（如「领养需先积累对话轮次」），供卡片直接展示原因。 */
  message?: string | null;
  /** 跳过/结果原因，用于区分细分状态（adopt-threshold / no-buddy / daily-limit 等）。 */
  skip?: string | null;
}

export interface AutoRotateConfig {
  enabled: boolean;
  check_interval_minutes: number;
  cooldown_minutes: number;
  min_gap_hours: number;
  min_urgency_hours: number;
  active_guard_minutes: number;
  min_remaining_credits: number;
}

export interface RotateLog {
  ts: number;
  action: string;
  reason?: string | null;
  from?: { id: string; name?: string | null } | null;
  to?: { id: string; name?: string | null } | null;
}

export interface RotateStatus {
  config: AutoRotateConfig;
  cliConfigured: boolean;
  activeAccountId: string | null;
  activeAccountName: string | null;
  lastCheckAt: number | null;
  lastSwitchAt: number | null;
}

export interface CreditResource {
  packageCode: string | null;
  packageName: string | null;
  total: number;
  remaining: number;
  used: number;
  status: number | null;
  expireAt: number | null;
  expired: boolean;
  expiringSoon: boolean;
}

export interface CreditExpiry {
  ok: boolean;
  accountId?: string | null;
  accountName?: string;
  updatedAt?: number;
  totalCapacity?: number;
  totalRemaining?: number;
  expiringSoonRemaining?: number;
  expiredRemaining?: number;
  soonestExpireAt?: number | null;
  expiringSoon?: boolean;
  expired?: boolean;
  resources?: CreditResource[];
  error?: string;
}

export interface CreditStatsSummary {
  currentRemaining: number;
  currentCapacity: number;
  usageToday: number;
  usage7Days: number;
  usageThisMonth: number;
  todayCheckedInAccounts: number;
  todaySuccess: number;
  todayAlready: number;
  todayFailed: number;
}

export interface CreditStatsDailyPoint {
  date: string;
  usage: number;
  /** 官方用量按模型聚合（全量，不受请求明细条数限制）；本地观察口径下为空 */
  models?: { model: string; requestCount: number; credit: number }[];
}

export interface CreditStatsAccount {
  accountId: string;
  accountName: string;
  isCurrent: boolean;
  currentRemaining: number | null;
  totalCapacity: number | null;
  lastSnapshotAt: number | null;
  usageToday: number;
  usage7Days: number;
  usageThisMonth: number;
  checkedInToday: boolean | null;
  checkinStatusToday: string | null;
  lastCheckinAt: number | null;
  lastCheckinResult: string | null;
  /** 按账号的逐日观察消耗（缺省兼容旧后端）；官方可用时趋势图优先使用官方 daily */
  daily?: CreditStatsDailyPoint[];
}

export interface CreditStatsUsageEvent {
  kind: "usage";
  ts: number;
  date: string;
  accountId: string;
  accountName: string;
  amount: number;
}

export interface CreditStatsCheckinEvent {
  kind: "checkin";
  ts: number;
  date: string;
  accountId: string | null;
  accountName: string;
  result: string;
  error?: string | null;
}

export type CreditStatsEvent = CreditStatsUsageEvent | CreditStatsCheckinEvent;

export type CreditOfficialUsageStatus = "complete" | "partial" | "unavailable";

export interface CreditOfficialUsageSummary {
  usageToday: number;
  usage7Days: number;
  usageThisMonth: number;
}

export interface CreditOfficialUsageModel {
  model: string;
  requestCount: number;
  credit: number;
}

export interface CreditOfficialUsageAccount {
  accountId: string;
  accountName: string;
  ok: boolean;
  requestCount: number;
  detailTruncated: boolean;
  usageToday: number | null;
  usage7Days: number | null;
  usageThisMonth: number | null;
  error?: string | null;
  reportedTotal?: number | null;
  fetchedCount?: number;
  /** 缺省兼容旧后端响应。 */
  models?: CreditOfficialUsageModel[];
  /** 按账号的逐日官方消耗（全量聚合，不受 requests 明细上限影响；缺省兼容旧后端） */
  daily?: CreditStatsDailyPoint[];
}

export interface CreditOfficialUsageRequest {
  accountId: string;
  accountName: string;
  requestId: string;
  credit: number;
  model: string;
  client: string;
  requestTime: string;
}

export interface CreditOfficialUsageError {
  accountId: string;
  accountName: string;
  error: string;
}

export interface CreditOfficialUsage {
  status: CreditOfficialUsageStatus;
  rangeStart: string;
  rangeEnd: string;
  /** 官方用量最近一次采集时间；缓存命中时保持采集当时的时间。 */
  collectedAt?: number;
  summary: CreditOfficialUsageSummary;
  daily: CreditStatsDailyPoint[];
  accounts: CreditOfficialUsageAccount[];
  requests: CreditOfficialUsageRequest[];
  /** 官方全部有效请求按模型汇总；不受 requests 明细上限影响。 */
  models?: CreditOfficialUsageModel[];
  detailLimitPerAccount: number;
  errors: CreditOfficialUsageError[];
}

export interface CreditStatistics {
  generatedAt: number;
  retentionDays: number;
  coverageStartAt: number | null;
  summary: CreditStatsSummary;
  daily: CreditStatsDailyPoint[];
  accounts: CreditStatsAccount[];
  events: CreditStatsEvent[];
  /** 官方接口不可用时仍使用上述本地观察字段；缺省兼容旧后端。 */
  officialUsage?: CreditOfficialUsage;
}

export interface TokenStatsTotals { total: number; input: number; output: number; cacheRead: number; cacheWrite: number; uncachedInput: number; records: number; cacheHitRate: number | null; }
export interface TokenStatsGroup extends TokenStatsTotals { key: string; title?: string | null; project?: string; sessionId?: string; }
export interface TokenStatsSource { source: "workbuddy" | "workbuddy-ai" | "codebuddy-cli" | "codebuddy-ide"; summary: TokenStatsTotals; models: TokenStatsGroup[]; projects: TokenStatsGroup[]; sessions: TokenStatsGroup[]; daily: TokenStatsGroup[]; /** Optional model-specific daily series for trend filtering. */ dailyByModel?: Record<string, TokenStatsGroup[]>; hours: TokenStatsGroup[]; filesScanned: number; parseErrors: number; coverageStartAt?: number | null; coverageEndAt?: number | null; }
export interface TokenStatistics { generatedAt: number; rangeDays?: number | null; sources: TokenStatsSource[]; }

export interface CodeBuddyCliStatus {
  configured: boolean;
  authMode?: "settings-env";
  environmentOverride?: boolean;
  settingsPresent: boolean;
  helperPresent: boolean;
  helperSupportsAccountIds: boolean;
  helperCurrent?: boolean;
  migrationRequired?: boolean;
  syncPending?: boolean;
  activeIndex: number | null;
  activeAccountId: string | null;
  activeAccountName: string | null;
  accountCount: number;
  statePath: string;
}

export interface CodeBuddyCliSwitchResult {
  ok: boolean;
  configured: boolean;
  synced: boolean;
  verified?: boolean;
  authMode?: "settings-env";
  activeIndex?: number;
  activeAccountId?: string;
  source?: string;
  skipped?: boolean;
  message?: string;
  error?: string;
}

export interface CodeBuddyCliInstallResult {
  ok: boolean;
  configured: boolean;
  helperPresent: boolean;
  helperSupportsAccountIds: boolean;
  verified?: boolean;
  authMode?: "settings-env";
  message?: string;
  error?: string;
}

/**
 * 代理的**适用范围**（三个独立开关）。
 *
 * 为什么把「一个代理地址」拆成三个开关：同一个地址对不同用途的收益完全不同 ——
 * GitHub（检查更新 / 下载安装包）在国内基本必须走代理；国际版上游
 *（workbuddy.ai）国内直连实测 wsarecv 超时，也需要；而国服上游
 *（codebuddy.cn / copilot.tencent.com）直连即通，绕进代理只会多一跳延迟、
 * 多一个故障面（代理一挂，本来好好的国服账号跟着不可用）。
 *
 * 地址仍然只填一次（用户不该填三遍），三个开关只决定「哪些用途使用它」。
 */
export interface ProxyScope {
  /** 检查更新与下载安装包时是否使用代理。默认开。 */
  github: boolean;
  /**
   * 国服账号（*.workbuddy.cn / *.codebuddy.cn / copilot.tencent.com）的上游请求是否使用代理。
   *
   * 默认关：国内直连通常更快；且关了之后是**真直连**（连 HTTPS_PROXY 也不用）。
   */
  cn: boolean;
  /**
   * 国际版账号（*.workbuddy.ai / *.codebuddy.ai）的上游请求是否使用代理。
   *
   * 默认开：国内直连实测不稳定（wsarecv 超时），不走代理基本用不了。
   */
  intl: boolean;
}

export interface GithubConfig {
  owner?: string;
  repo?: string;
  proxy?: string;
  /**
   * 三个开关。**允许缺失**：老配置 / 老后端里没有这个字段，
   * 读取方必须用 `proxyScopeOf()` 兜底成默认值，不能自己当 false 处理。
   */
  proxy_scope?: Partial<ProxyScope> | null;
}

export interface UpdateInfo {
  ok: boolean;
  current?: string;
  latest?: string;
  latestTag?: string;
  hasUpdate?: boolean;
  releaseName?: string;
  releaseUrl?: string;
  publishedAt?: string;
  error?: string;
  message?: string;
}

/** CodeBuddy CN IDE（桌面客户端）状态；与 CodeBuddy CLI 独立。 */
export interface CodeBuddyCnIdeStatus {
  installed: boolean;
  running: boolean;
  dataDir: string | null;
  dbPath: string | null;
  dbExists: boolean;
  appPath: string | null;
  activeAccountId: string | null;
  activeAccountName: string | null;
  detectedFrom?: string;
  statePath?: string;
}

export interface CodeBuddyCnIdeSwitchResult {
  ok: boolean;
  account: string;
  accountId: string;
  dbPath?: string;
  restarted?: boolean;
  message?: string;
}


// ---------------------------------------------------------------------------
// 网关（workbuddy2api）集成
// ---------------------------------------------------------------------------

/** 网关配置（持久化在 ~/.wb-switch/gateway/gateway_config.json）。 */
/* 网关工作模式：
 * balance —— 负载均衡（默认）：账号池加权随机选号，自动避开冷却/熔断账号
 * pinned  —— 指定账号：只使用 pinned_uid 对应的那一个账号
 * rotation —— 单一模型 + 积分轮转：只用一个账号烧到不可用，再换按到期日
 *             排序的下一个（仍优先烧最快过期的额度）                      */
/**
 * 网关工作模式。
 *
 * - `balance` 自动：全部（未禁用的）账号参与，池内加权随机 + 到期日分层
 * - `manual`  手动：只使用勾选的账号，池内仍自动均衡
 * - `rotation` 积分轮转：单一模型烧号，按到期日换下一个
 *
 * `pinned` 是历史值，读作 `manual`（后端 `GatewayMode::from_str` 已兼容）。
 */
export type GatewayMode = "balance" | "manual" | "rotation";

export interface GatewayConfig {
  /** 是否已启用（启动过即为 true）。 */
  enabled: boolean;
  /** 网关工作模式。 */
  mode?: GatewayMode;
  /** 手动模式下勾选的账号 uid 列表（可多选）。 */
  manual_uids?: string[];
  /** 指定账号模式下锁定的账号 uid（旧字段，仅向后兼容）。 */
  pinned_uid?: string | null;
  /**
   * 「限制使用的模型」白名单（多选）。**空数组 = 不限制（默认，全部放行）**。
   *
   * 非空时网关**只放行名单内的模型**，其余一律 400 model_not_allowed。
   * **三个工作模式（自动 / 手动 / 积分轮转）都生效** —— 它限制的是「放行哪些
   * 模型」，与「用哪些账号」是正交的两件事。
   *
   * 联合类型里的 `string` 是**向后兼容**，不是冗余：老配置里这个键是单值字符串
   *（实测所有者本机的 gateway_config.json 就是 `"allowed_model":
   * "deepseek-v4.1-flash"`）。声明成 `string[]` 会让读取方以为可以直接
   * `.length` / `.map`，在老配置上运行时炸掉。
   */
  allowed_model?: string[] | string | null;
  /** 服务端口（权威字段，前端口选择器直接编辑它）。 */
  port: number;
  /** 监听地址，由 port 派生，如 ":7863"。 */
  listen: string;
  /** OpenAI 兼容接口的鉴权密钥；空 = 不鉴权。 */
  api_key: string;
  /** 随 App 启动而自动拉起。 */
  auto_start: boolean;
  last_status?: string | null;
  last_error?: string | null;

  // ---- 自动养号任务排程（写进网关 config.json 的 schedule 块）----
  //
  // 这些字段由宿主读取后转写到网关的 native config；网关只认它自己的 config.json，
  // 因此改这里必须重启网关才会生效。

  /** 活跃上报时点（小时列表，默认 [10]）：点亮连登天数并解锁领养前置。 */
  activity_hours?: number[];
  /** 夜猫子任务时点（默认 [1]）：仅在 23:00–08:00 北京时间内计入。 */
  nightowl_hours?: number[];
  /** 开学季活动任务时点（默认 [12]）：限时活动，只领取已达标的奖励。 */
  school_hours?: number[];
  /** 国际版 trial 加油包领取时点（默认 [9, 21]，仅国际版账号）。 */
  trial_hours?: number[];
  activity_enabled?: boolean;
  nightowl_enabled?: boolean;
  school_enabled?: boolean;
  trial_enabled?: boolean;
  /** 每号每日活跃上报条数（默认 3，上限 20）。 */
  activity_report_count?: number;

  // ---- 自定义系统提示词（写进网关 config.json 的 prompt 块）----
  //
  // 由宿主转写到网关 native config 的 prompt 块（Go 侧 Config.Prompt）。
  // 与 features.sanitize_blacklist_fingerprints 是**两层叠加、互不替代**：
  // 那个清洗消息里的指纹串，这个把 system/developer 消息整体替换。

  /**
   * 提示词模式；缺省 `"passthrough"` = 透传客户端原始 system（既有行为不变）。
   *
   * `"custom"` = 用网关自有提示词替换客户端的 system/developer 消息。
   * 缺省刻意不是 custom：老配置没有这个键，若缺省 custom，既有用户升级后
   * system 会被静默替换（人设、项目约定、工具说明全丢）。
   */
  prompt_mode?: "passthrough" | "custom";
  /** 自定义提示词文件路径；空 = 用网关内置默认提示词。 */
  prompt_file?: string;
}

/** 手动触发养号任务的结果（POST /api/gateway/task-run）。 */
export interface GatewayTaskRunResult {
  ok: boolean;
  /** 是否真的执行了一轮；false = 被前置条件挡下（见 skip / message）。 */
  ran: boolean;
  /**
   * 跳过原因码；`ran=true` 时为空。
   *
   * - `outside_window`：不在夜猫子时段（23:00–08:00 北京时间）
   * - `already_running`：该任务上一轮还在执行
   */
  skip?: string | null;
  /** 面向用户的中文说明，可直接显示。 */
  message: string;
  /** 调用失败的原因（网关未启动、任务名不认识等）；成功时为 null。 */
  error?: string | null;
}

/** 养号任务标识（与 Go 网关 /tasks/run 的 task 参数一一对应）。 */
export type GatewayTaskName = "activity" | "nightowl" | "school" | "trial";

/**
 * 成长任务的展示状态（Go 侧 growtask 的 View* 常量）。
 *
 * 与养号任务的 `ran/skip` 不同，成长任务是**逐任务**的结果，
 * 因此每个任务自己带状态与进度。
 */
export type GrowthTaskStatus =
  | "claimable"
  | "in_progress"
  | "not_accepted"
  | "accepted"
  | "claimed"
  | "unsupported"
  | "locked";

/** 成长任务列表里的一项（action=list）。 */
export interface GrowthTaskView {
  task_code: string;
  title?: string;
  /** 客户端操作指引（多用于 `unsupported` 的任务）。 */
  description?: string;
  /** 达成条件简述。 */
  task_desc?: string;
  /** 奖励积分 / 能量。 */
  credit?: number;
  energy?: number;
  status: GrowthTaskStatus;
  /** 状态的中文说明，界面可直接显示。 */
  status_text: string;
  accept_status?: string;
  /** 上游是否下发了进度。**未报名时为 false**（progress 为 null）。 */
  has_progress?: boolean;
  /** "当前/目标"，如 "3/5"；无进度时为空。 */
  progress?: string;
  claimable?: boolean;
  /** 能否被本工具自动完成。false 时只展示指引，不提供「一键完成」。 */
  automatable?: boolean;
  /** 该项需要**真实对话**才能推进（会消耗 token 与额度），界面应提示。 */
  needs_chat?: boolean;
  /** 本工具会执行什么动作（中文说明）。 */
  action_desc?: string;
  /** 无法自动完成时的原因说明。 */
  hint?: string;
}

/** 单项任务的执行结果（action=run）。 */
export interface GrowthTaskItemResult {
  task_code: string;
  title?: string;
  desc?: string;
  status: "done" | "skipped" | "error" | "unsupported";
  message: string;
  /** 动作前后进度（"当前/目标"）——「上报 200 ≠ 计分」的证据。 */
  progress_before?: string;
  progress_after?: string;
  claimed?: boolean;
  credit?: number;
  energy?: number;
  claim_error?: string;
}

/**
 * 成长任务「一键完成」的返回。
 *
 * 三种 action 的返回形状不同（list 带 tasks / run 带 items / run-all 带 results），
 * 故这里是**联合形状**而非各自独立的类型 —— 界面按 action 取用对应字段。
 * 失败时 `ok=false` + `error`，且**不抛 HTTP 错误**：部分成功也要能拿到已完成的部分。
 */
export interface GrowthTaskResult {
  ok: boolean;
  error?: string;
  /** action=list 时返回。 */
  accountId?: string;
  tasks?: GrowthTaskView[];
  total?: number;
  /** action=run（未指定 taskCode）时返回。 */
  items?: GrowthTaskItemResult[];
  /** action=run（指定 taskCode）时返回。 */
  item?: GrowthTaskItemResult;
  /** action=run-all 时返回。 */
  results?: Array<{
    uid: string;
    realm: string;
    skipped?: boolean;
    skip_reason?: string;
    items?: GrowthTaskItemResult[];
    claimed?: number;
    credit?: number;
    energy?: number;
    error?: string;
  }>;
  summary?: {
    accounts?: number;
    claimed?: number;
    credit?: number;
    energy?: number;
  };
}

/** 单个「账号+模型」的冷却记录（来自网关 /status 的 model_cooling）。 */
export interface GatewayModelCooling {
  /** 被限流的模型名。 */
  model: string;
  /** 冷却截止时刻（ISO 8601）。 */
  until?: string;
  /** 距到期的剩余秒数（后端已算好，避免前后端时钟偏差）。 */
  remaining_sec?: number;
  /** 面向用户的说明文案（含模型名与重置时间）。 */
  reason?: string;
  /** true = 到期时间取自上游报错文案；false = 解析失败，回退固定软冷却。 */
  reset_at_parsed?: boolean;
}

/** 网关账号池中的单个账号运行态（来自网关 /status）。 */
export interface GatewayPoolAccount {
  uid: string;
  nickname?: string;
  credits?: number;
  cooling?: boolean;
  cool_kind?: string;
  cool_remaining_sec?: number;
  disabled?: boolean;
  reason?: string;
  success_count?: number;
  err_total?: number;
  in_flight?: number;
  /**
   * 最近一次成功调用的时刻（ISO 8601；Go 侧 `pool.Status.LastSuccessTime`）。
   *
   * 与 `success_count` 的分工：计数回答「一共成了多少次」，本字段回答「上一次成
   * 是什么时候」—— 后者才能区分「一直在稳定成功」与「早就不再被选中了」
   * （计数是个只增不减的累计值，看不出停滞）。
   */
  last_success?: string;
  /** 最近一次失败的时刻（ISO 8601）；供「最近成功」旁证用。 */
  last_err?: string;
  /** 连续失败计数（熔断器输入；达到阈值即熔断）。 */
  breaker_fails?: number;
  /** 熔断截止时刻（ISO 8601）；非空且未过期 = 正在熔断期。 */
  breaker_until?: string;
  /** 「最近到期积分」的到期时刻（Unix 秒）；缺省 = 未知。 */
  soonest_expire_at?: number;
  /** 到期日（YYYY-MM-DD），即选号分层档位键；同一天的账号同级。 */
  expire_day?: string;
  /**
   * 是否正因「到期档位更晚」而排队等待（当前轮不到它）。
   *
   * 由网关按与选号**完全相同**的档位口径算出。语义是「现在轮不到」，
   * **不是故障** —— 前面档位被消耗或冷却后会自动进入路由。
   */
  queued?: boolean;
  /**
   * 该账号当前因「模型级限流」而冷却的模型（按到期时间升序）。
   *
   * 与 `cooling` 的区别（界面据此区分两种冷却）：
   * - `cooling` = 账号级：余额（积分）欠费或账号被限速，整号不可用
   * - `model_cooling` 非空 = 模型级：仅这些模型不可用，换模型仍可用
   * 两者可同时存在。
   */
  model_cooling?: GatewayModelCooling[];
}

/** 网关 /status 响应。 */
export interface GatewayPool {
  accounts?: GatewayPoolAccount[];
  total?: number;
  healthy?: number;
  cooling?: number;
  disabled?: number;
  in_flight_full?: number;
  sticky_sessions?: number;
  redis_mode?: string;
}

/** 网关状态里「可选账号」一项（手动模式勾选列表的数据源）。 */
export interface GatewayStatusAccount {
  uid: string;
  nickname?: string;
  expiresAt?: number;
  needsRelogin?: boolean;
  /** 用户手动禁用：勾选列表里不再展示（勾了也不会进池）。 */
  disabled?: boolean;
}

/** 网关综合状态。 */
export interface GatewayStatus {
  running: boolean;
  reachable: boolean;
  base: string;
  openaiBase: string;
  port: number;
  exePath: string | null;
  exeFound: boolean;
  /** 网关来源：embedded=内嵌在单个 exe 内 / env=环境变量指定 / external=外部文件。 */
  exeSource?: "embedded" | "env" | "external";
  /** 配置端口当前是否空闲（网关运行时该端口被自己占用，属正常）。 */
  portAvailable?: boolean;
  /** 当前工作模式。 */
  mode?: GatewayMode;
  /** 指定账号模式锁定的 uid。 */
  pinnedUid?: string | null;
  /** 可选账号列表（供手动模式的勾选列表使用）。 */
  accounts?: GatewayStatusAccount[];
  /**
   * 因「需重新登录」而被排除出网关账号池的账号。
   *
   * 这些账号的 refresh token 已被服务端拒绝，继续留在池里只会每次请求白跑一轮，
   * 因此同步时不会写入网关凭证目录；重新登录成功后会自动恢复。
   */
  excludedAccounts?: Array<{
    uid: string;
    nickname?: string;
    reason?: string | null;
  }>;
  authDir: string;
  accountsInLibrary: number;
  config: GatewayConfig;
  /**
   * 正在执行的养号任务（含进度）。
   *
   * 所有者明确要求「账号卡片上要能看到正在执行的任务」—— 此前点「立即执行」
   * 只有一个按钮转圈，看不到在跑什么、跑到哪、哪些账号在跑。
   */
  taskRuntime?: GatewayTaskRuntime;
  health: { reachable?: boolean; healthy?: boolean; detail?: unknown } | null;
  pool: GatewayPool | null;
}

/**
 * 正在执行的养号任务的运行态（来自 `GET /api/gateway/status` 的 `taskRuntime`）。
 *
 * 进度是**近似值**，口径如下（见 Rust 侧 `task_runtime`）：
 *   - `total` 是按账号库 + 任务区域规则算出的**预计**账号数；
 *   - `processed` / `processedIds` 来自统一事件流的**实际**已记录账号。
 * 两者可能短暂不等（网关账号池与账号库有极小时差），因此文案写成
 * 「已记录 N / M」而不是断言性的「已完成」。
 */
export interface GatewayTaskRuntime {
  /** false = 当前没有任务在跑；此时其余字段不保证存在。 */
  running: boolean;
  /** 任务标识（与 `GatewayTaskName` 对应）。 */
  task?: GatewayTaskName;
  /** 面向用户的任务中文名，可直接显示。 */
  label?: string;
  /** 开始时刻（毫秒时间戳）。 */
  startedAt?: number;
  /** 已运行毫秒数（后端算好，避免前后端时钟偏差）。 */
  elapsedMs?: number;
  /** 本轮预计遍历的账号数（进度分母）。 */
  total?: number;
  /** 已留下记录的账号数（进度分子）。 */
  processed?: number;
  /**
   * 已留下记录的账号在**宿主账号库里的 id**（不是网关 uid）。
   * 供账号卡片标记「这个号正在跑」。
   */
  processedIds?: string[];
}

/**
 * 账号卡片上的「本轮已跑」标记（由 `AccountsPage` 从 `taskRuntime` 推导后下发）。
 *
 * 为什么措辞是「已跑」而不是「正在跑」：后端只透出 `processedIds` —— 它是
 * **已经留下记录**的账号集合（见 Rust 侧 `task_runtime`），而 Go 侧记录是在
 * **处理完一个账号之后**才写（`scheduler/activity.go` 等）。也就是说，
 * 本轮**当前正在处理**的那个号还没进集合，后端也没有「当前是哪个号」这个字段。
 * 因此界面照实说「本轮已跑」，不编造一个后端并不提供的「正在跑这个号」。
 */
export interface AccountRunningTask {
  /** 任务中文名（取自 `taskRuntime.label`，如「活跃上报」）。 */
  label: string;
  /** 本轮已留下记录的账号数（分子）。近似值，口径见卡片悬停提示。 */
  processed?: number;
  /** 本轮预计遍历的账号数（分母）。 */
  total?: number;
}

/** GET /api/gateway/config 响应。 */
export interface GatewayConfigResult {
  config: GatewayConfig;
  exeFound: boolean;
  exePath: string | null;
  authDir: string;
}

/** POST /api/gateway/{start,restart} 响应。 */
export interface GatewayStartResult {
  started?: boolean;
  base?: string;
  port?: number;
  accounts?: number;
  health?: unknown;
}

/** POST /api/gateway/sync 响应。 */
export interface GatewaySyncResult {
  ok: boolean;
  accounts?: number;
  changed?: string[];
  updatedFromGateway?: string[];
  reloaded?: boolean;
  error?: string;
}

/** POST /api/gateway/mode 响应（切换模式并立即生效）。 */
export interface GatewayModeSwitchResult {
  ok: boolean;
  mode?: GatewayMode;
  pinnedUid?: string | null;
  /** 重导出后的账号数。 */
  accounts?: number;
  changed?: string[];
  /** 是否因模式变更重启了网关（未运行时为 false）。 */
  reloaded?: boolean;
  config?: GatewayConfig;
  error?: string;
}

/** POST /api/gateway/port-check 响应。 */
export interface GatewayPortCheck {
  port: number;
  available: boolean;
  /** 是否为 1024 以下的特权端口。 */
  reserved: boolean;
  /** 该端口当前是否被本网关自身占用。 */
  inUseByGateway: boolean;
  /** 端口被占用时给出的可用建议端口。 */
  suggest: number | null;
  /** 占用该端口的进程；查不到时为 null（权限不足或进程已退出）。 */
  holder: GatewayPortHolder | null;
}

/** 占用端口的进程信息。 */
export interface GatewayPortHolder {
  pid: number;
  name: string;
  /** 可执行文件完整路径；权限不足时为空串。 */
  path: string;
  /**
   * 是否为本项目自己的进程（网关 / 宿主 GUI）。
   *
   * 前端据此调整提示措辞：清理自己的旧进程是常见操作，
   * 而结束第三方进程需要更强的警告。
   */
  ours: boolean;
}

/** 网关 Token 用量中的一组计量（口径与本地 Token 统计页一致）。 */
export interface GatewayUsageTotals {
  /** input + output + cacheWrite（不含 cacheRead，避免重复计数）。 */
  total: number;
  /** 输入 token，已包含缓存读取。 */
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  uncachedInput: number;
  /** 计入统计的成功请求数。 */
  records: number;
  /** 缓存命中率 = cacheRead / input；无输入时为 null。 */
  cacheHitRate: number | null;
}

/** 带分组键的用量（模型名 / 账号 uid / 日期）。 */
export interface GatewayUsageGroup extends GatewayUsageTotals {
  key: string;
}

/** 网关 /usage 响应（enabled=false 表示该网关未启用统计）。 */
export interface GatewayUsageSnapshot {
  enabled: boolean;
  generatedAt: number;
  /** 统计范围（近 N 天）；null = 全部历史。 */
  rangeDays?: number | null;
  summary?: GatewayUsageTotals;
  models?: GatewayUsageGroup[];
  accounts?: GatewayUsageGroup[];
  /**
   * 账号 → 该账号用过的模型明细（键 = 池 uid）。
   *
   * 为什么不能由 `models` 与 `accounts` 前端现算：这两个维度各自聚合后，
   * 交叉关系已经丢失 —— 只知道「甲账号共 3 万」「glm-5.2 共 4 万」，
   * 无法还原「甲账号的 glm-5.2 用了多少」。由网关侧记录时直接累计，
   * 界面「按账号筛选看用了哪些模型」才有可信数据。
   *
   * 缺失（老版本网关）时按「无明细」处理，不回退到假的交叉结果。
   */
  accountModels?: Record<string, GatewayUsageGroup[]>;
  /** 按日期升序的日聚合。 */
  daily?: GatewayUsageGroup[];
  dailyByModel?: Record<string, GatewayUsageGroup[]>;
}

/** get_gateway_usage 的统一响应：网关不可达时 usage 为 null 且带 error。 */
export interface GatewayUsageResult {
  running: boolean;
  reachable: boolean;
  usage: GatewayUsageSnapshot | null;
  error: string | null;
}

// ---------------------------------------------------------------------------
// 智能体客户端一键导入（agent_import）
// ---------------------------------------------------------------------------

/** 单个 AI 客户端的探测状态。 */
export interface AgentClientTarget {
  /** 客户端标识：dsh / claude-code / claude-desktop / codex */
  id: string;
  /** 显示名称 */
  label: string;
  /** 是否检测到已安装 */
  installed: boolean;
  /** 是否已接入本网关 */
  configured: boolean;
  /** 主要配置文件的绝对路径 */
  configPath: string;
  /** 补充说明信息 */
  note: string;
  /** 探测到的版本号 */
  version?: string | null;
}

/** GET /api/gateway/agents 探测响应。 */
export interface AgentDetectionResult {
  /** 本机网关根地址，如 http://127.0.0.1:7863 */
  base: string;
  /** 网关配置中是否已设置 API Key */
  hasApiKey: boolean;
  /** 探测到的客户端列表 */
  targets: AgentClientTarget[];
}

/** 网关模型项。 */
export interface GatewayModelItem {
  id: string;
  name?: string;
  context_length?: number;
  max_output_tokens?: number;
  owned_by?: string;
}

/** POST /api/gateway/agents/import 接入响应。 */
export interface AgentImportResult {
  ok: boolean;
  target: string;
  backupDir: string;
  files: string[];
  models?: string[];
  model?: string;
}

/** 批量接入/一键更新响应。 */
export interface AgentBatchImportResult {
  ok: boolean;
  count: number;
  outcomes: Array<{
    target: string;
    backupDir: string;
    files: string[];
    models?: string[];
  }>;
  models: string[];
}

/** POST /api/gateway/agents/restore 恢复响应。 */
export interface AgentRestoreResult {
  ok: boolean;
  restored: number;
  backupId: string;
}

/** 备份记录项。 */
export interface AgentBackupItem {
  id: string;
  createdAt: number;
  path: string;
}

