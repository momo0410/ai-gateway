import { useCallback, useEffect, useState, type ReactElement, type ReactNode } from "react";
import { toast } from "sonner";
import { ArrowUpCircle, CircleCheck, ExternalLink, Loader2, RefreshCw, Save } from "lucide-react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import * as api from "@/lib/api";
import {
  DEFAULT_PROXY_SCOPE,
  PROXY_SCOPE_FIELDS,
  proxyScopeOf,
  proxyScopeSummary,
} from "@/lib/proxy-scope";
import { getThemePreference, setThemePreference, type ThemePreference } from "@/lib/theme";
import type {
  AutoRotateConfig,
  CheckinConfig,
  CheckinLog,
  GatewayConfig,
  GatewayTaskName,
  GatewayTaskRuntime,
  GithubConfig,
  ProxyScope,
  RotateLog,
  RotateStatus,
  UpdateInfo,
} from "@/lib/types";
import { GITHUB_RELEASE_URL, GITHUB_REPOSITORY_URL, openReleaseUrl } from "@/lib/update";
import { cn } from "@/lib/utils";
import { useVisibilityInterval } from "@/lib/use-visibility-interval";
import { UpdateInstallDialog } from "@/components/update-install-dialog";
import { DemoAction } from "@/components/demo-action";
import { useAccountsStore } from "@/stores/accounts";

interface SettingsGroupProps {
  id: string;
  title: string;
  children: ReactNode;
}

function SettingsGroup({ id, title, children }: SettingsGroupProps) {
  return (
    <section className="min-w-0 space-y-2.5" aria-labelledby={id}>
      <div className="px-1">
        <h2 id={id} className="text-[13px] font-medium leading-5">
          {title}
        </h2>
      </div>
      <Card className="min-w-0 gap-0 overflow-hidden rounded-xl py-0 shadow-none">{children}</Card>
    </section>
  );
}

function SettingsRow({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <div
      className={cn(
        "mx-4 flex min-w-0 items-center justify-between gap-3 border-b border-border/50 px-0 py-2.5 sm:mx-5",
        className,
      )}
    >
      {children}
    </div>
  );
}

interface SettingsFieldRowProps {
  label: ReactNode;
  description?: ReactNode;
  htmlFor?: string;
  children: ReactNode;
  className?: string;
  operational?: boolean;
}

function SettingsFieldRow({
  label,
  description,
  htmlFor,
  children,
  className,
  operational = false,
}: SettingsFieldRowProps) {
  return (
    <SettingsRow className={cn("flex-col items-stretch gap-2 sm:flex-row sm:items-center", className)}>
      <div className="min-w-0 flex-1">
        {htmlFor ? (
          <Label htmlFor={htmlFor} className="text-[13px] leading-4">
            {label}
          </Label>
        ) : (
          <div className="text-[13px] font-medium leading-4">{label}</div>
        )}
        {description && (
          <p className="mt-0.5 text-xs leading-4 text-muted-foreground/75">{description}</p>
        )}
      </div>
      <div className="flex min-w-0 w-full shrink-0 justify-end sm:w-auto">
        {operational ? <DemoAction className="w-full sm:w-auto">{children as ReactElement}</DemoAction> : children}
      </div>
    </SettingsRow>
  );
}

function formatTime(ts: number): string {
  try {
    return new Date(ts).toLocaleString("zh-CN", {
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    });
  } catch {
    return String(ts);
  }
}

function logLabel(result: string): { text: string; tone: "success" | "warning" | "error" } {
  switch (result) {
    case "success":
      return { text: "签到成功", tone: "success" };
    case "already":
      return { text: "已签到", tone: "warning" };
    default:
      return { text: "失败", tone: "error" };
  }
}

/** 自动签到配置 + 一键签到 + 日志。 */
function AutoCheckinCard() {
  const [cfg, setCfg] = useState<CheckinConfig | null>(null);
  const [logs, setLogs] = useState<CheckinLog[]>([]);
  // 日志标题里的天数取自「记录保留」设置，避免与真实清理口径不一致
  const [retentionDays, setRetentionDays] = useState<number | null>(null);
  const [saving, setSaving] = useState(false);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ type: "ok" | "err"; text: string } | null>(null);

  useEffect(() => {
    void load();
  }, []);

  async function load() {
    try {
      const [c, l, r] = await Promise.all([
        api.getAutoCheckinConfig(),
        api.getCheckinLogs(),
        // 保留设置失败不应影响签到日志展示，故单独 catch
        api.getRecordRetention().catch(() => null),
      ]);
      setCfg(c);
      setLogs(l.logs);
      if (r) setRetentionDays(r.days);
    } catch (e) {
      setMsg({ type: "err", text: api.asError(e) });
    }
  }

  async function save() {
    if (!cfg) return;
    setSaving(true);
    setMsg(null);
    try {
      const saved = await api.saveAutoCheckinConfig(cfg);
      setCfg(saved);
      setMsg({ type: "ok", text: "配置已保存" });
    } catch (e) {
      setMsg({ type: "err", text: api.asError(e) });
    } finally {
      setSaving(false);
    }
  }

  async function checkinAllNow() {
    setBusy(true);
    setMsg(null);
    try {
      const res = await api.checkinAll();
      if (res.status === "skipped" && res.reason === "already_running") {
        setMsg({ type: "err", text: "签到任务正在进行，请稍后再试" });
        return;
      }
      const ok = res.accounts.filter((a) => a.result === "success").length;
      const already = res.accounts.filter((a) => a.result === "already").length;
      const err = res.accounts.filter((a) => a.result === "error").length;
      const detail = res.accounts
        .filter((a) => a.result === "error")
        .map((a) => `${a.email}（${a.error}）`)
        .join("；");
      setMsg({
        type: err > 0 ? "err" : "ok",
        text: `签到完成：成功 ${ok}，已签 ${already}，失败 ${err}${detail ? `。${detail}` : ""}`,
      });
      void load();
    } catch (e) {
      setMsg({ type: "err", text: api.asError(e) });
    } finally {
      setBusy(false);
    }
  }

  function setNum(key: keyof CheckinConfig, value: string) {
    if (!cfg) return;
    setCfg({ ...cfg, [key]: Number(value) });
  }

  return (
    <SettingsGroup
      id="settings-auto-checkin"
      title="自动签到"
    >
      <CardContent className="space-y-0 p-0">
        {cfg ? (
          <>
            <SettingsFieldRow
              label="启用自动签到"
              description="启动时立即核验服务端状态，未签到账号会自动补签；仅覆盖国服账号"
              htmlFor="ac-enabled"
              operational
            >
              <Switch
                id="ac-enabled"
                checked={cfg.enabled}
                onCheckedChange={(v) => setCfg({ ...cfg, enabled: v })}
              />
            </SettingsFieldRow>

            <SettingsFieldRow
              label="保活阈值"
              description="天；0 表示每天无条件刷新"
              htmlFor="ac-keep"
              operational
            >
              <Input
                id="ac-keep"
                className="w-full sm:w-48"
                type="number"
                min={0}
                max={90}
                value={cfg.keepalive_days}
                onChange={(e) => setNum("keepalive_days", e.target.value)}
              />
            </SettingsFieldRow>
            <SettingsFieldRow label="惰性刷新" description="小时" htmlFor="ac-lazy" operational>
              <Input
                id="ac-lazy"
                className="w-full sm:w-48"
                type="number"
                min={1}
                max={72}
                value={cfg.lazy_refresh_hours}
                onChange={(e) => setNum("lazy_refresh_hours", e.target.value)}
              />
            </SettingsFieldRow>

            <div className="flex flex-wrap gap-2 border-b-0 border-border/60 px-4 py-3 sm:px-5">
              <DemoAction><Button size="sm" onClick={save} disabled={saving}>
                {saving ? <Loader2 className="animate-spin" /> : <Save />}保存配置
              </Button></DemoAction>
              <DemoAction><Button size="sm" variant="outline" onClick={checkinAllNow} disabled={busy}>
                {busy ? <Loader2 className="animate-spin" /> : <CircleCheck />}全部立即签到
              </Button></DemoAction>
            </div>
          </>
        ) : (
          <p className="px-4 py-3 text-sm text-muted-foreground sm:px-5">加载配置中…</p>
        )}

        {msg && (
          <Alert
            variant={msg.type === "err" ? "destructive" : "default"}
            className="!w-auto mx-4 my-4 sm:mx-5"
          >
            <AlertDescription>{msg.text}</AlertDescription>
          </Alert>
        )}

        <div className="px-4 py-3 sm:px-5">
          {/* 天数必须与「记录保留」设置一致：写死 30 天会在用户改成 60 天后骗人 */}
          <p className="mb-2 text-[13px] font-medium">
            签到日志{retentionDays ? `（最近 ${retentionDays} 天）` : ""}
          </p>
          {logs.length === 0 ? (
            <p className="py-3 text-center text-sm text-muted-foreground">暂无签到记录</p>
          ) : (
            <div className="max-h-64 overflow-y-auto pr-1">
              {[...logs].reverse().map((l, i) => {
                const tone = logLabel(l.result);
                return (
                  <div
                    key={i}
                    className="flex items-center justify-between border-b border-border/60 py-2 text-xs last:border-b-0"
                  >
                    <div className="min-w-0 flex-1 truncate">
                      <span className="font-medium">{l.email}</span>
                      {l.error && <span className="text-destructive">（{l.error}）</span>}
                    </div>
                    <div className="ml-2 flex shrink-0 items-center gap-2">
                      <span
                        className={
                          tone.tone === "error"
                            ? "text-destructive"
                            : tone.tone === "warning"
                              ? "text-amber-600"
                              : "text-emerald-600"
                        }
                      >
                        {tone.text}
                      </span>
                      <span className="text-muted-foreground">{formatTime(l.ts)}</span>
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      </CardContent>
    </SettingsGroup>
  );
}

/**
 * 一次「立即执行」之后留给用户看的结果。
 *
 * 为什么需要它，而不只是一条 toast：`ran=true` 只说明**网关的入口被调到了**，
 * 完全没说这一轮跑了几个账号、成了几个、跳过了什么。所有者点 trial 时看到的
 * 就是「已触发过一轮」这一句 —— 既不知道是不是真执行了，也不知道结果，
 * 而 toast 几秒后消失，回头再看什么都没有。所以结果要**留在卡片上**。
 */
type TaskRunOutcome = {
  /** 与 ran 严格对应：真的跑了一轮 / 被前置条件挡下。 */
  ran: boolean;
  /** 一句话结论（用户只需要读这一行）。 */
  headline: string;
  /** 结论的依据，逐条列出（账号数、跳过原因、失败原因）。 */
  details: string[];
  /** 结果基调：ok=跑了且无失败；warn=跑了但有失败，或没跑（正常跳过）；err=调用失败。 */
  tone: "ok" | "warn" | "err";
  at: number;
};

/**
 * 任务名 → 该任务写进「账号记录」的标题。
 *
 * 必须与 Go 侧**逐字一致**，否则回读不到任何记录，界面就会永远显示
 * 「没有写入记录」——而且不报错。出处：
 *   - `activity.go`   `Records.Task(a.UID, "活跃上报", ...)`
 *   - `nightowl.go`   `Records.Task(a.UID, "夜猫子任务", ...)`
 *   - `school.go`     `Records.Task(a.UID, "开学季活动", ...)`
 *   - `trial.go`      `Records.Task(a.UID, "trial 加油包", ...)`
 *
 * 注意与设置页显示名**不同**（那边叫「国际版 trial 加油包」）：
 * 显示名面向用户，这个面向数据，混用会静默回读失败。
 */
const TASK_RECORD_TITLE: Record<GatewayTaskName, string> = {
  activity: "活跃上报",
  nightowl: "夜猫子任务",
  school: "开学季活动",
  trial: "trial 加油包",
};

/** 跳过原因码 → 面向用户的解释（与 Go `scheduler.TaskRunResult.Skip` 一一对应）。 */
function skipExplanation(skip: string | null | undefined): string | null {
  switch (skip) {
    case "already_running":
      // 纯文本，不用 Markdown 记号：这里渲染进 Alert 正文，`**x**` 会原样显示成
      // 两个星号（实测确认），看起来像没写完的富文本。
      return "该任务上一轮还在执行，本次没有重复触发（这是防重入，不是故障）。一轮活跃上报按「账号数 × 条数」串行跑，大账号池下可达数分钟。";
    case "outside_window":
      return "当前不在该任务的生效时段内，上游不会计入本次执行。";
    default:
      return null;
  }
}

/**
 * 回读本轮任务真正写下的记录，换算成「跑了几个账号、结果如何」。
 *
 * 为什么要回读记录而不是只看 `ran`：`ran` 是**入口级**的布尔值（`RunTaskByName`
 * 执行完就返回 true），它不携带任何账号维度信息。而用户问的是「跑了几个 / 结果」。
 * 记录文件是唯一有账号粒度、且网关与宿主**同一个文件**（`account_records.json`）
 * 的出处，不必新增接口。
 *
 * 时间的处理：网关与宿主各有自己的时钟，`since` 往前放宽 2 分钟吸收偏差，
 * 宁可多带进上一轮的记录（下面按标题严格过滤），也不能漏掉本轮刚写的。
 *
 * 返回 null 表示「查不到」——与「查到了但为空」是两件事，界面必须分开说：
 * 前者是接口不可用，后者是本轮确实没有需要记录的变化（记录按天去重、
 * 且仅在成功/失败/重要跳过时才写）。
 */
async function loadRunRecords(
  task: GatewayTaskName,
  since: number,
): Promise<{ title: string; result: string; accountName: string; detail: string }[] | null> {
  try {
    const res = await api.getAccountRecords({
      from: Math.max(0, since - 120_000),
      kinds: ["task"],
      limit: 500,
    });
    // `records` 不是数组时必须返回 null（= 拿不到），**不能**退化成空数组。
    // 空数组在调用方那里等于「本轮没有账号产生新记录」——那是一个**结论**；
    // 而接口返回了形状不对的东西时我们其实什么都不知道。用 `?? []` 会把
    // 「读不到」谎报成「确实没有」，正是本次要修的那类问题。
    if (!Array.isArray(res?.records)) return null;
    const title = TASK_RECORD_TITLE[task];
    return res.records
      .filter((r) => (r.title ?? "").trim() === title)
      .map((r) => ({
        title: r.title ?? "",
        result: r.result ?? "",
        accountName: r.accountName ?? "",
        detail: r.detail ?? "",
      }));
  } catch {
    return null;
  }
}

/**
 * 把回读到的记录归纳成一句结论 + 若干明细。
 *
 * 结果码的取值来自 `records.go`：`success` / `failed` / `already` / `info`。
 * 其中 `already` 是**幂等成功**（如 trial「本周期已领取过」）——所有者明确要求
 * 这种情况要如实说「无需重复执行」，而不是含糊地说「已触发一轮」。
 */
function summarizeRunRecords(
  records: { result: string; accountName: string; detail: string }[],
): { headline: string; details: string[] } {
  const success = records.filter((r) => r.result === "success");
  const already = records.filter((r) => r.result === "already");
  const failed = records.filter((r) => r.result === "failed");
  const total = records.length;

  const details: string[] = [];
  if (failed.length > 0) {
    // 失败必须点名到账号 + 原因，否则用户不知道该去处理谁。
    for (const r of failed.slice(0, 3)) {
      details.push(`失败 · ${r.accountName || "未知账号"}${r.detail ? `：${r.detail}` : ""}`);
    }
    if (failed.length > 3) details.push(`…另有 ${failed.length - 3} 个账号失败`);
  }
  if (already.length > 0) {
    const sample = already[0];
    details.push(
      `幂等跳过 ${already.length} 个（无需重复执行）${sample.detail ? `：${sample.detail}` : ""}`,
    );
  }
  if (success.length > 0) {
    details.push(`成功 ${success.length} 个${success[0].detail ? `：${success[0].detail}` : ""}`);
  }

  // 结论的措辞按「有没有真的产生变化」分级 —— 这正是所有者要区分的东西。
  let headline: string;
  if (failed.length > 0) {
    headline = `已执行：${total} 个账号有结果，其中 ${failed.length} 个失败`;
  } else if (success.length > 0) {
    headline = `已执行：${success.length} 个账号有新结果`;
  } else if (already.length > 0) {
    headline = `已执行，但无需重复执行：${already.length} 个账号此前已完成`;
  } else {
    headline = `已执行：${total} 个账号有记录`;
  }
  return { headline, details };
}

/**
 * 「正在执行」面板：任务跑起来之后，界面要一直能看到「在跑什么、跑到哪了」。
 *
 * 为什么必须补这块（所有者明确要求「账号卡片上要能看到正在执行的任务」）：
 * 此前点「立即执行」只有按钮上转一个圈，跑一轮活跃上报要 40 秒以上（实测
 * 19 个账号 41.7s），期间用户完全不知道跑到第几个号、还要等多久，
 * 只能盯着一个没有信息量的 loading 干等。
 *
 * 数据来自 `gateway_status().taskRuntime`，进度取自**网关自己写的账号记录**
 * （`account_records.json`）—— 不为进度另造一套账本，理由见 Rust 侧
 * `task_processed_ids` 的注释。
 *
 * 进度文案刻意写成「已记录 N / M」而不是「已完成 N / M」：
 * 记录只在「成功且有新变化 / 失败 / 重要跳过」时写（且按天去重），
 * 因此 N 是**下界**，说成「已完成」会在没新记录时显示成卡住不动，反而误导。
 */
function TaskRunningPanel({ runtime, taskLabel }: { runtime: GatewayTaskRuntime; taskLabel: string }) {
  const elapsed = Math.max(0, Math.round((runtime.elapsedMs ?? 0) / 1000));
  const elapsedText = elapsed >= 60 ? `${Math.floor(elapsed / 60)} 分 ${elapsed % 60} 秒` : `${elapsed} 秒`;
  const total = runtime.total ?? 0;
  const processed = runtime.processed ?? 0;
  // 百分比只在有分母时算：total=0（如账号全被禁用）时给 0 而不是 NaN 宽度
  const percent = total > 0 ? Math.min(100, Math.round((processed / total) * 100)) : 0;

  return (
    <div className="px-4 pb-3 sm:px-5">
      <div
        className="rounded-lg border border-primary/25 bg-primary/5 px-3 py-2.5"
        role="status"
        aria-live="polite"
        aria-label={`正在执行：${taskLabel}`}
      >
        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
          <Loader2 className="size-3.5 shrink-0 animate-spin text-primary" aria-hidden="true" />
          <span className="text-[13px] font-medium text-foreground">正在执行：{taskLabel}</span>
          <span className="text-[11px] tabular-nums text-muted-foreground">已运行 {elapsedText}</span>
        </div>
        {total > 0 ? (
          <>
            <div className="mt-2 flex items-baseline justify-between gap-2">
              <span className="text-[11px] tabular-nums text-muted-foreground">
                已记录 {processed} / {total} 个账号
              </span>
              <span className="text-[11px] tabular-nums text-muted-foreground">{percent}%</span>
            </div>
            {/* 进度条用既有主题 token（bg-primary/bg-muted），不自造配色 */}
            <div className="mt-1 h-1 overflow-hidden rounded-full bg-muted" aria-hidden="true">
              <div className="h-full rounded-full bg-primary transition-[width]" style={{ width: `${percent}%` }} />
            </div>
          </>
        ) : null}
        <p className="mt-1.5 text-[11px] leading-4 text-muted-foreground">
          任务在网关侧逐账号串行执行。执行期间后台自动同步会**推迟**重启网关，
          以免打断本轮任务；期间的账号变更会在任务结束后生效。
        </p>
      </div>
    </div>
  );
}

/**
 * 自动养号任务卡片：4 个任务的开关 / 执行时刻 / 立即执行。
 *
 * 「立即执行」的结果反馈是本卡片的核心难点（所有者实测反馈）：
 * 他点 trial 后只看到「已触发过一轮」，既不知道是否真的执行、也不知道结果。
 * 因此这里把 `ran`（真的跑了没有）、`skip`（跳过原因码）与**回读到的账号级记录**
 * 三者拼成一条留在界面上的结论，而不是一句转瞬即逝的 toast。
 */
/**
 * 「立即执行」的结果面板：一行结论 + 若干明细，留在卡片上直到下次执行。
 *
 * 为什么不用 toast 承担这件事：toast 是**瞬时**的，而「跑了几个账号、跳过几个、
 * 为什么跳过」是需要边看边核对的（用户往往要切到网关页或账号记录页去对照）。
 * 用既有 shadcn `Alert` 而不是自造样式块：本页的保存/错误反馈一直用它，
 * 卡片/尺寸/配色因此与全站一致（所有者多次强调「不要自创风格」）。
 *
 * `at` 显示具体时刻：用户据此判断这条结果是不是自己刚点的那一次
 *（尤其是等了很久之后回到本页时）。
 */
function TaskOutcomePanel({ outcome, taskLabel }: { outcome: TaskRunOutcome; taskLabel: string }) {
  const time = new Date(outcome.at).toLocaleTimeString("zh-CN", { hour12: false });
  return (
    <div className="px-4 pb-3 sm:px-5">
      <Alert
        variant={outcome.tone === "err" ? "destructive" : "default"}
        className="!w-auto"
        aria-label={`${taskLabel} 的执行结果`}
      >
        <AlertDescription>
          <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
            <span className="text-[13px] font-medium">{outcome.headline}</span>
            <span className="text-[11px] tabular-nums text-muted-foreground">
              {time} · {outcome.ran ? "已执行" : "未执行"}
            </span>
          </div>
          {outcome.details.length > 0 ? (
            <ul className="mt-1 list-disc space-y-0.5 pl-4 text-[11px] leading-4 text-muted-foreground">
              {outcome.details.map((line, index) => (
                <li key={index}>{line}</li>
              ))}
            </ul>
          ) : null}
        </AlertDescription>
      </Alert>
    </div>
  );
}

/**
 * 自动养号任务：活跃上报 / 夜猫子 / 开学季 / 国际版 trial。
 *
 * 为什么单独一张卡而不塞进「自动签到」：这 4 个任务跑在**网关**里（不是宿主里），
 * 配置项落在 gateway_config.json 并转写进网关的 config.json；与宿主的自动签到
 * 是两条独立的链路。混在一起会让「改了不生效」变得无从排查。
 *
 * 为什么每个任务都写明前置条件：它们都会在条件不满足时静默跳过
 *（夜猫子限时段、开学季限活动期、活跃上报与开学季只跑国服、trial 只跑国际版）。
 * 不写清楚，用户点「立即执行」看不到任何变化，只会以为功能坏了。
 *
 * 「立即执行」结果反馈的设计见上方 TaskRunOutcome / loadRunRecords 的说明。
 */
function AutoCareTasksCard() {
  const [cfg, setCfg] = useState<GatewayConfig | null>(null);
  const [saving, setSaving] = useState(false);
  /** 正在「立即执行」的任务名（用于按任务显示 loading）。 */
  const [running, setRunning] = useState<string | null>(null);
  const [msg, setMsg] = useState<{ type: "ok" | "err" | "warn"; text: string } | null>(null);
  /**
   * 每个任务最近一次「立即执行」的结果，**留在卡片上**直到下次执行。
   *
   * 用 map 而不是单个值：4 个任务各有各的结果，用户常常连着点几个再回头对比。
   * 单值会让先前那个任务的结果凭空消失，看起来像「没执行过」。
   */
  const [outcomes, setOutcomes] = useState<Partial<Record<GatewayTaskName, TaskRunOutcome>>>({});

  /**
   * 网关侧正在执行的养号任务（含进度）。
   *
   * 为什么不能只靠本地 `running` state：那个状态只覆盖「本页发起的这次请求」，
   * 一旦用户切走页面再切回来、或任务由别处（账号卡片菜单）触发，本地状态就是空的，
   * 而任务其实还在跑。网关侧状态是唯一权威来源。
   */
  const [runtime, setRuntime] = useState<GatewayTaskRuntime | null>(null);

  const load = useCallback(async () => {
    try {
      const res = await api.getGatewayConfig();
      setCfg(res.config);
    } catch (e) {
      setMsg({ type: "err", text: api.asError(e) });
    }
  }, []);

  /**
   * 拉取任务运行态。
   *
   * 失败**静默**：这是旁路观测数据，网关没起来时它本来就查不到，
   * 为此弹错误提示只会制造噪音（配置读取失败已经由 load() 报过了）。
   */
  const loadRuntime = useCallback(async () => {
    try {
      const status = await api.getGatewayStatus();
      setRuntime(status.taskRuntime ?? null);
    } catch {
      setRuntime(null);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // 任务运行态轮询（2 秒）。
  //
  // 周期取 2 秒的取舍：进度来自网关写的账号记录，账号之间本就间隔 400ms～
  // 数秒（刻意防风控），更密的轮询只会白打请求、并不会让进度更准。
  //
  // 用 useVisibilityInterval：窗口隐藏/收进托盘时**销毁**定时器，不在后台空转
  //（与账号页、网关页同一做法）。
  useVisibilityInterval(() => void loadRuntime(), 2000, {
    onResume: () => void loadRuntime(),
  });

  async function save() {
    if (!cfg) return;
    setSaving(true);
    setMsg(null);
    try {
      const res = await api.saveGatewayConfig({
        activity_hours: cfg.activity_hours,
        nightowl_hours: cfg.nightowl_hours,
        school_hours: cfg.school_hours,
        trial_hours: cfg.trial_hours,
        activity_enabled: cfg.activity_enabled,
        nightowl_enabled: cfg.nightowl_enabled,
        school_enabled: cfg.school_enabled,
        trial_enabled: cfg.trial_enabled,
        activity_report_count: cfg.activity_report_count,
        // 自定义系统提示词：与养号任务排程同一份配置、同一个保存按钮。
        // 漏传这两个字段的话，用户在这里的改动会在保存时被**静默丢弃**
        //（save_gateway_config 只覆盖传入的键），表现为「改了没用」。
        prompt_mode: cfg.prompt_mode,
        prompt_file: cfg.prompt_file,
      });
      setCfg(res.config);
      // 说清楚「还要重启」：网关只在启动时读一次 config.json，
      // 不提示的话用户会以为保存没生效，反复点保存。
      setMsg({ type: "ok", text: "配置已保存。重启网关后生效（可在「兼容网关」页重启）" });
    } catch (e) {
      setMsg({ type: "err", text: api.asError(e) });
    } finally {
      setSaving(false);
    }
  }

  /**
   * 立即执行一轮任务，并把**可复核的结果**留在卡片上。
   *
   * 三条分支各自说清「发生了什么」，而不是笼统地报「已触发」：
   *   1. 调用失败（网关没起来等）→ err，给出后端原文；
   *   2. `ran=false` + `skip` → **正常结果**，如实说明为什么没执行
   *      （夜猫子不在时段 / 上一轮还在跑），这是所有者明确点出的语义；
   *   3. `ran=true` → 回读账号记录，得出「跑了几个账号、成了几个、跳过几个」。
   *
   * 为什么要回读（第 3 步）而不能只信 `ran`：`ran` 是入口级布尔值，
   * `RunTaskByName` 执行完就返回 true，**不含任何账号维度信息**。而用户问的
   * 恰恰是「跑了几个 / 结果如何」。记录文件是唯一有账号粒度、且网关与宿主
   * 读写**同一个文件**的出处，因此不需要新增接口。
   */
  async function runNow(task: GatewayTaskName) {
    setRunning(task);
    setMsg(null);
    // 记录起点时间：执行完据此回读**本轮新写**的记录，而不是把历史全都算进来。
    const startedAt = Date.now();
    try {
      const res = await api.runGatewayTask(task);
      if (!res.ok) {
        const text = res.error || "执行失败";
        setOutcomes((prev) => ({
          ...prev,
          [task]: {
            ran: false,
            headline: "执行失败",
            details: [text],
            tone: "err",
            at: Date.now(),
          },
        }));
        setMsg({ type: "err", text });
        return;
      }

      if (!res.ran) {
        // 被前置条件挡下是**正常结果**而非错误：用 warn 而不是 err，
        // 否则用户会以为功能坏了。原因码翻成人话，后端 message 优先。
        const explain = skipExplanation(res.skip);
        const headline = res.message || "本次未执行（前置条件不满足）";
        setOutcomes((prev) => ({
          ...prev,
          [task]: {
            ran: false,
            headline: `未执行：${headline}`,
            details: [
              // 说清「没执行」不等于「没触发」：请求确实到了网关，是它决定不跑。
              "请求已送达网关，网关按前置条件主动跳过（不是失败）。",
              ...(explain ? [explain] : []),
              ...(res.skip ? [`跳过原因码：${res.skip}`] : []),
            ],
            tone: "warn",
            at: Date.now(),
          },
        }));
        setMsg({ type: "warn", text: headline });
        return;
      }

      // ran=true：回读本轮真正写下的账号级记录。
      const records = await loadRunRecords(task, startedAt);
      if (records === null) {
        setOutcomes((prev) => ({
          ...prev,
          [task]: {
            ran: true,
            headline: "已执行一轮（结果明细读取失败）",
            details: [
              res.message || "网关已跑完这一轮。",
              "无法读取账号记录，因此看不到本轮跑了哪些账号；结果可能已经写入，请到「账号管理 → 查看记录」确认。",
            ],
            tone: "warn",
            at: Date.now(),
          },
        }));
        setMsg({ type: "ok", text: res.message || "已执行一轮" });
        return;
      }

      if (records.length === 0) {
        // 网关确实跑了，但没有写任何记录。这是**正常**的：记录只在
        // 成功且无变化 / 失败 / 重要跳过时才写，且按天去重（TaskDaily）。
        // 必须说清楚，否则用户会以为「跑了但没生效」。
        setOutcomes((prev) => ({
          ...prev,
          [task]: {
            ran: true,
            headline: "已执行一轮：本轮没有账号产生新记录",
            details: [
              res.message || "网关已跑完这一轮。",
              "记录只在「成功且有新变化 / 失败 / 重要跳过」时写入，同一账号同一结果每天最多一条 —— 因此这里为空通常表示本周期该做的都已完成。",
            ],
            tone: "ok",
            at: Date.now(),
          },
        }));
        setMsg({ type: "ok", text: res.message || "已执行一轮" });
        return;
      }

      const { headline, details } = summarizeRunRecords(records);
      const hasFailure = records.some((r) => r.result === "failed");
      setOutcomes((prev) => ({
        ...prev,
        [task]: {
          ran: true,
          headline,
          details,
          // 有失败时降级成 warn：结论行仍然是「已执行」，但基调要提醒用户去看明细。
          tone: hasFailure ? "warn" : "ok",
          at: Date.now(),
        },
      }));
      setMsg({ type: hasFailure ? "warn" : "ok", text: headline });
    } catch (e) {
      const text = api.asError(e);
      setOutcomes((prev) => ({
        ...prev,
        [task]: { ran: false, headline: "执行失败", details: [text], tone: "err", at: Date.now() },
      }));
      setMsg({ type: "err", text });
    } finally {
      setRunning(null);
    }
  }

  /** 更新某个任务的时点列表（输入框是逗号分隔的小时）。 */
  function setHours(key: keyof GatewayConfig, value: string) {
    if (!cfg) return;
    const hours = value
      .split(/[,，\s]+/)
      .map((s) => Number(s.trim()))
      .filter((n) => Number.isInteger(n) && n >= 0 && n <= 23);
    setCfg({ ...cfg, [key]: hours });
  }

  /** 时点列表 → 输入框文本。 */
  function hoursText(hours?: number[]): string {
    return (hours ?? []).join(", ");
  }

  /** 取某个任务最近一次的执行结果（没有则 undefined，不渲染结果面板）。 */
  function outcomeFor(task: GatewayTaskName): TaskRunOutcome | undefined {
    return outcomes[task];
  }

  const tasks: {
    name: GatewayTaskName;
    label: string;
    enabledKey: keyof GatewayConfig;
    hoursKey: keyof GatewayConfig;
    description: ReactNode;
    /** 前置条件说明（界面必须写清楚，否则「立即执行」没反应像是坏了）。 */
    note: string;
    /** 额外的数值配置（只有活跃上报有）。 */
    extra?: ReactNode;
  }[] = [
    {
      name: "activity",
      label: "活跃上报",
      enabledKey: "activity_enabled",
      hoursKey: "activity_hours",
      description: "每个账号发送对话活跃事件，点亮连登天数并解锁领养猫猫的前置条件",
      note: "仅国服账号。签到只恢复余额，连登天数必须靠本任务点亮。",
      extra: cfg ? (
        <SettingsFieldRow
          label="每号每日条数"
          description="条；默认 3。单条偶发被服务端丢弃，多条提高点亮成功率"
          htmlFor="care-activity-count"
          operational
        >
          <Input
            id="care-activity-count"
            className="w-full sm:w-48"
            type="number"
            min={1}
            max={20}
            value={cfg.activity_report_count ?? 3}
            onChange={(e) =>
              setCfg({ ...cfg, activity_report_count: Number(e.target.value) })
            }
          />
        </SettingsFieldRow>
      ) : null,
    },
    {
      name: "nightowl",
      label: "夜猫子任务",
      enabledKey: "nightowl_enabled",
      hoursKey: "nightowl_hours",
      description: "在夜猫时段内补一次任务，点亮仅在夜间计入的成长任务",
      note: "只在 23:00–08:00（北京时间）内有效，时段外点击「立即执行」会被跳过并提示原因。仅国服账号。",
    },
    {
      name: "school",
      label: "开学季活动",
      enabledKey: "school_enabled",
      hoursKey: "school_hours",
      description: "领取活动里已达标的奖励",
      note: "限时活动。仅领取已达标的任务奖励，不伪造学生认证 / 邀请等动作；活动下线后自动跳过。仅国服账号。",
    },
    {
      name: "trial",
      label: "国际版 trial 加油包",
      enabledKey: "trial_enabled",
      hoursKey: "trial_hours",
      description: "为国际版账号领取 trial 加油包",
      note: "仅国际版账号（国服无此入口）。已领取过的账号会被幂等跳过，可每天重试。",
    },
  ];

  return (
    <SettingsGroup id="settings-care-tasks" title="自动养号任务">
      <CardContent className="space-y-0 p-0">
        <p className="border-b border-border/60 bg-muted/25 px-4 py-3 text-xs leading-5 text-muted-foreground sm:px-5">
          这 4 个任务由<b className="text-foreground">兼容网关</b>执行。改完配置需要重启网关才会生效；
          「立即执行」会立刻让网关跑一轮，便于验证配置是否正确。
        </p>

        {cfg ? (
          <>
            {tasks.map((task) => (
              <div key={task.name} className="border-b border-border/60">
                <SettingsFieldRow
                  label={task.label}
                  description={task.description}
                  htmlFor={`care-${task.name}-enabled`}
                  operational
                >
                  <Switch
                    id={`care-${task.name}-enabled`}
                    checked={cfg[task.enabledKey] !== false}
                    onCheckedChange={(v) => setCfg({ ...cfg, [task.enabledKey]: v })}
                  />
                </SettingsFieldRow>

                <SettingsFieldRow
                  label="执行时刻"
                  description="小时，可填多个用逗号分隔（0-23）；例如 9, 21"
                  htmlFor={`care-${task.name}-hours`}
                  operational
                >
                  <Input
                    id={`care-${task.name}-hours`}
                    className="w-full sm:w-48"
                    value={hoursText(cfg[task.hoursKey] as number[] | undefined)}
                    onChange={(e) => setHours(task.hoursKey, e.target.value)}
                  />
                </SettingsFieldRow>

                {/* 额外数值配置排在按钮之前：按钮行是这一组的收尾，
                    插在它后面会让「立即执行」看起来属于下一个配置项。 */}
                {task.extra}

                <div className="flex flex-wrap items-center gap-2 px-4 py-3 sm:px-5">
                  <DemoAction>
                    <Button
                      size="sm"
                      variant="outline"
                      // 任一任务在跑就禁用全部按钮：这些任务是**整轮**触发
                      //（作用于全部账号），并发触发只会被网关的防重入挡下。
                      // 这里同时看本地 running 与网关侧 runtime —— 后者才能覆盖
                      //「任务由别处触发 / 本页刚刷新」的情况。
                      disabled={running !== null || Boolean(runtime?.running)}
                      onClick={() => void runNow(task.name)}
                    >
                      {running === task.name || runtime?.task === task.name ? (
                        <Loader2 className="animate-spin" />
                      ) : (
                        <RefreshCw />
                      )}
                      立即执行
                    </Button>
                  </DemoAction>
                  <span className="text-xs leading-4 text-muted-foreground/75">{task.note}</span>
                </div>

                {/* 「正在执行」面板紧跟按钮行：它是**当下**的状态，
                    与下方「上一次的结果」是两件事（一个是进行中、一个是已结束）。
                    只在本任务正在跑时渲染 —— 放在每个任务下会让 4 个任务各显示一遍
                    同一个全局状态，用户会误以为「4 个任务在同时跑」。 */}
                {runtime?.running && runtime.task === task.name ? (
                  <TaskRunningPanel runtime={runtime} taskLabel={task.label} />
                ) : null}

                {/* 执行结果留在卡片上（而非只在 toast 里）：所有者反馈「点了之后
                    不知道是否真的执行了、也不知道结果」，而 toast 几秒后消失，
                    回头再想核对就什么都没有了。
                    放在按钮行**下方**、属于本任务的块内 —— 每个任务各有各的结果，
                    不能合并成一条全局提示（那样 4 个任务的结果会互相覆盖）。 */}
                {outcomeFor(task.name) ? (
                  <TaskOutcomePanel
                    outcome={outcomeFor(task.name) as TaskRunOutcome}
                    taskLabel={task.label}
                  />
                ) : null}
              </div>
            ))}

            {/* ---- 自定义系统提示词 ----
                与上面 4 个「养号任务」是**两件不同的事**（那个改的是排程，
                这个改的是网关转发请求时的 system 消息），但同属「网关配置」、
                共用同一个保存按钮与同一份 gateway_config，因此放在同一张卡里，
                用一条分隔线划清边界。

                必须做成显式开关而不是「填了路径就自动启用」：缺省必须是
                透传（passthrough），否则既有用户升级后 system 会被静默替换 ——
                人设、项目约定、工具说明全丢，且从请求上看不出是网关动的手。 */}
            <div className="border-t border-border/60">
              <SettingsFieldRow
                label="使用自定义系统提示词"
                description="开启后用网关自带的提示词替换客户端发出的 system 消息；关闭时原样透传（默认）"
                htmlFor="gateway-prompt-mode"
                operational
              >
                <Switch
                  id="gateway-prompt-mode"
                  checked={cfg.prompt_mode === "custom"}
                  onCheckedChange={(v) =>
                    setCfg({ ...cfg, prompt_mode: v ? "custom" : "passthrough" })
                  }
                />
              </SettingsFieldRow>

              <SettingsFieldRow
                label="提示词文件"
                description="留空 = 用网关内置的默认提示词；填了但读不到会导致网关启动失败"
                htmlFor="gateway-prompt-file"
                operational
              >
                <Input
                  id="gateway-prompt-file"
                  className="w-full sm:w-96"
                  placeholder="D:\\prompts\\mine.md"
                  // 关闭时仍允许编辑：这样「先填好文件、稍后再开启」是可行的。
                  // 置灰的话用户得先开开关（那一刻文件还是空的 → 网关起不来）。
                  value={cfg.prompt_file ?? ""}
                  onChange={(e) => setCfg({ ...cfg, prompt_file: e.target.value })}
                />
              </SettingsFieldRow>

              {cfg.prompt_mode === "custom" ? (
                <p className="border-b border-border/60 bg-muted/25 px-4 py-2.5 text-[11px] leading-4 text-muted-foreground sm:px-5">
                  开启后客户端的 system / developer 消息会被<b className="text-foreground">整体替换</b>。
                  文件留空则使用网关内置提示词；填了路径但文件不存在或内容为空时，网关会拒绝启动。
                </p>
              ) : null}
            </div>

            <div className="flex flex-wrap gap-2 px-4 py-3 sm:px-5">
              <DemoAction>
                <Button size="sm" onClick={save} disabled={saving}>
                  {saving ? <Loader2 className="animate-spin" /> : <Save />}保存配置
                </Button>
              </DemoAction>
            </div>
          </>
        ) : (
          <p className="px-4 py-3 text-sm text-muted-foreground sm:px-5">加载配置中…</p>
        )}

        {msg && (
          <Alert
            variant={msg.type === "err" ? "destructive" : "default"}
            className="!w-auto mx-4 my-4 sm:mx-5"
          >
            <AlertDescription>{msg.text}</AlertDescription>
          </Alert>
        )}
      </CardContent>
    </SettingsGroup>
  );
}

/** 自动轮换配置（CodeBuddy CLI）+ 手动检查 + 日志。 */
function AutoRotateCard() {
  const [cfg, setCfg] = useState<AutoRotateConfig | null>(null);
  const [status, setStatus] = useState<RotateStatus | null>(null);
  const [logs, setLogs] = useState<RotateLog[]>([]);
  const [saving, setSaving] = useState(false);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ type: "ok" | "err"; text: string } | null>(null);

  useEffect(() => {
    void load();
  }, []);

  async function load() {
    try {
      const [c, s, l] = await Promise.all([
        api.getAutoRotateConfig(),
        api.getRotateStatus(),
        api.getRotateLogs(),
      ]);
      setCfg(c);
      setStatus(s);
      setLogs(l.logs);
    } catch (e) {
      setMsg({ type: "err", text: api.asError(e) });
    }
  }

  async function save() {
    if (!cfg) return;
    setSaving(true);
    setMsg(null);
    try {
      const saved = await api.saveAutoRotateConfig(cfg);
      setCfg(saved);
      setMsg({ type: "ok", text: "配置已保存" });
    } catch (e) {
      setMsg({ type: "err", text: api.asError(e) });
    } finally {
      setSaving(false);
    }
  }

  async function runNow() {
    setBusy(true);
    setMsg(null);
    try {
      const res = await api.runRotate();
      setMsg({
        type: res.status === "error" ? "err" : "ok",
        text:
          res.status === "switched"
            ? `已切换到 ${res.to ?? "目标账号"}`
            : res.status === "disabled"
              ? "自动轮换未启用（请在下方开启后重试）"
              : (res.reason ?? `检查完成：${res.status}`),
      });
      void load();
    } catch (e) {
      setMsg({ type: "err", text: api.asError(e) });
    } finally {
      setBusy(false);
    }
  }

  function setNum(key: keyof AutoRotateConfig, value: string) {
    if (!cfg) return;
    setCfg({ ...cfg, [key]: Number(value) });
  }

  function actionLabel(action: string): { text: string; tone: "success" | "warning" | "error" } {
    switch (action) {
      case "switched":
        return { text: "已切换", tone: "success" };
      case "skipped":
        return { text: "未切换", tone: "warning" };
      case "disabled":
        return { text: "未启用", tone: "warning" };
      case "error":
        return { text: "出错", tone: "error" };
      default:
        return { text: action, tone: "warning" };
    }
  }

  return (
    <SettingsGroup
      id="settings-auto-rotate"
      title="CodeBuddy CLI 自动轮换"
    >
      <CardContent className="space-y-0 p-0">
        {status && (
          <div className="flex flex-wrap items-center gap-x-4 gap-y-1 border-b border-border/60 bg-muted/25 px-4 py-3 text-xs text-muted-foreground sm:px-5">
            <span>
              当前 CLI 账号：
              <b className="text-foreground">{status.activeAccountName ?? "未配置"}</b>
            </span>
            {status.lastCheckAt && <span>上次检查 {formatTime(status.lastCheckAt)}</span>}
            {status.lastSwitchAt && <span>上次切换 {formatTime(status.lastSwitchAt)}</span>}
            {!status.cliConfigured && (
              <span className="text-destructive">未接入 CodeBuddy CLI（请先到账号页安装 helper）</span>
            )}
          </div>
        )}

        {cfg ? (
          <>
            <SettingsFieldRow
              label="启用自动轮换"
              description="开启后按下方间隔自动检查并切换 CodeBuddy CLI 账号"
              htmlFor="ar-enabled"
              operational
            >
              <Switch
                id="ar-enabled"
                checked={cfg.enabled}
                onCheckedChange={(v) => setCfg({ ...cfg, enabled: v })}
              />
            </SettingsFieldRow>

            <SettingsFieldRow label="检查间隔" description="分钟" htmlFor="ar-interval" operational>
              <Input
                id="ar-interval"
                className="w-full sm:w-48"
                type="number"
                min={1}
                max={1440}
                value={cfg.check_interval_minutes}
                onChange={(e) => setNum("check_interval_minutes", e.target.value)}
              />
            </SettingsFieldRow>
            <SettingsFieldRow label="切换冷却" description="分钟" htmlFor="ar-cooldown" operational>
              <Input
                id="ar-cooldown"
                className="w-full sm:w-48"
                type="number"
                min={1}
                max={1440}
                value={cfg.cooldown_minutes}
                onChange={(e) => setNum("cooldown_minutes", e.target.value)}
              />
            </SettingsFieldRow>
            <SettingsFieldRow label="到期差异阈值" description="小时" htmlFor="ar-gap" operational>
              <Input
                id="ar-gap"
                className="w-full sm:w-48"
                type="number"
                min={0}
                max={720}
                value={cfg.min_gap_hours}
                onChange={(e) => setNum("min_gap_hours", e.target.value)}
              />
            </SettingsFieldRow>
            <SettingsFieldRow label="到期紧迫阈值" description="小时" htmlFor="ar-urgency" operational>
              <Input
                id="ar-urgency"
                className="w-full sm:w-48"
                type="number"
                min={0}
                max={720}
                value={cfg.min_urgency_hours}
                onChange={(e) => setNum("min_urgency_hours", e.target.value)}
              />
            </SettingsFieldRow>
            <SettingsFieldRow label="活跃保护" description="分钟" htmlFor="ar-guard" operational>
              <Input
                id="ar-guard"
                className="w-full sm:w-48"
                type="number"
                min={0}
                max={1440}
                value={cfg.active_guard_minutes}
                onChange={(e) => setNum("active_guard_minutes", e.target.value)}
              />
            </SettingsFieldRow>
            <SettingsFieldRow label="最小剩余积分" description="低于此值时不切换" htmlFor="ar-min" operational>
              <Input
                id="ar-min"
                className="w-full sm:w-48"
                type="number"
                min={0}
                value={cfg.min_remaining_credits}
                onChange={(e) => setNum("min_remaining_credits", e.target.value)}
              />
            </SettingsFieldRow>
            <p className="border-b border-border/60 px-4 py-3 text-[13px] leading-5 text-muted-foreground sm:px-5">
              切换时机：目标账号剩余到期时间少于「紧迫阈值」且比当前账号早超过「差异阈值」，且最近「活跃保护」分钟内 CLI 无对话、目标剩余积分不低于「最小剩余积分」。
            </p>

            <div className="flex flex-wrap gap-2 border-b-0 border-border/60 px-4 py-3 sm:px-5">
              <DemoAction><Button size="sm" onClick={save} disabled={saving}>
                {saving ? <Loader2 className="animate-spin" /> : <Save />}保存配置
              </Button></DemoAction>
              <DemoAction><Button size="sm" variant="outline" onClick={runNow} disabled={busy}>
                {busy ? <Loader2 className="animate-spin" /> : <RefreshCw />}立即检查一次
              </Button></DemoAction>
            </div>
          </>
        ) : (
          <p className="px-4 py-3 text-sm text-muted-foreground sm:px-5">加载配置中…</p>
        )}

        {msg && (
          <Alert
            variant={msg.type === "err" ? "destructive" : "default"}
            className="!w-auto mx-4 my-4 sm:mx-5"
          >
            <AlertDescription>{msg.text}</AlertDescription>
          </Alert>
        )}

        <div className="px-4 py-3 sm:px-5">
          <p className="mb-2 text-[13px] font-medium">轮换日志（最近 200 条）</p>
          {logs.length === 0 ? (
            <p className="py-3 text-center text-sm text-muted-foreground">暂无轮换记录</p>
          ) : (
            <div className="max-h-64 overflow-y-auto pr-1">
              {logs.map((l, i) => {
                const tone = actionLabel(l.action);
                return (
                  <div
                    key={i}
                    className="flex items-center justify-between border-b border-border/60 py-2 text-xs last:border-b-0"
                  >
                    <div className="min-w-0 flex-1 truncate">
                      {l.action === "switched" && l.from && l.to && (
                        <span className="font-medium">
                          {l.from.name ?? l.from.id} → {l.to.name ?? l.to.id}
                        </span>
                      )}
                      {l.reason && <span className="text-muted-foreground">（{l.reason}）</span>}
                    </div>
                    <div className="ml-2 flex shrink-0 items-center gap-2">
                      <span
                        className={
                          tone.tone === "error"
                            ? "text-destructive"
                            : tone.tone === "success"
                              ? "text-emerald-600"
                              : "text-amber-600"
                        }
                      >
                        {tone.text}
                      </span>
                      <span className="text-muted-foreground">{formatTime(l.ts)}</span>
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      </CardContent>
    </SettingsGroup>
  );
}

/** 权限检测卡片：确认本 App 是否有权写入 WorkBuddy 认证文件。 */
function PermissionCheckCard() {
  const authFile = useAuthFile();
  const [checking, setChecking] = useState(false);
  const [result, setResult] = useState<null | { ok: boolean; text: string }>(null);

  async function runCheck() {
    setChecking(true);
    setResult(null);
    try {
      const res = await api.checkAuthPermission();
      setResult({
        ok: res.ok,
        text: res.ok
          ? res.message ?? "认证目录可写，权限正常"
          : `${res.error}（${res.dir ?? ""}）`,
      });
    } catch (e) {
      setResult({ ok: false, text: api.asError(e) });
    } finally {
      setChecking(false);
    }
  }

  return (
    <SettingsGroup
      id="settings-permission"
      title="权限检测"
    >
      <CardContent className="space-y-0 p-0">
        <div className="break-all border-b border-border/60 bg-muted/25 px-4 py-3 font-mono text-[11px] leading-5 text-muted-foreground sm:px-5">
          {authFile || "认证文件路径未获取"}
        </div>
        <div className="flex flex-wrap gap-2 border-b-0 border-border/60 px-4 py-3 sm:px-5">
          <DemoAction><Button size="sm" onClick={runCheck} disabled={checking}>
            {checking ? "检测中…" : "检测权限"}
          </Button></DemoAction>
          <DemoAction><Button
            size="sm"
            variant="outline"
            onClick={() => void api.openPermissionSettings("all_files")}
          >
            打开完全磁盘访问
          </Button></DemoAction>
          <DemoAction><Button
            size="sm"
            variant="outline"
            onClick={() => void api.openPermissionSettings("app_management")}
          >
            打开 App 管理
          </Button></DemoAction>
          <DemoAction><Button size="sm" variant="outline" onClick={() => void api.revealAppInFinder()}>
            在 Finder 中显示
          </Button></DemoAction>
        </div>

        {result && (
          <Alert variant={result.ok ? "default" : "destructive"} className="!w-auto mx-4 my-4 sm:mx-5">
            <AlertDescription>{result.text}</AlertDescription>
          </Alert>
        )}
        {result && !result.ok && (
          <div className="mx-4 mb-4 border-l-2 border-destructive/50 bg-muted/30 px-3 py-2.5 text-xs text-muted-foreground sm:mx-5">
            <p className="mb-1 font-medium text-foreground">如何授权（拖拽方式）：</p>
            <ol className="list-decimal space-y-1 pl-4">
              <li>点上方「打开完全磁盘访问」</li>
              <li>再点「在 Finder 中显示」打开 ai-gateway 所在位置</li>
              <li>
                把 <b>ai-gateway.app</b> 从 Finder <b>直接拖进</b>完全磁盘访问的列表区域
                （即使没有提示框，拖入即生效），然后打开它的开关
              </li>
              <li>回到本页点「检测权限」，或直接重试切换</li>
            </ol>
          </div>
        )}
      </CardContent>
    </SettingsGroup>
  );
}

function useAuthFile(): string | undefined {
  return useAccountsStore((s) => s.status?.authFile);
}

/**
 * 网络代理：**独立成一个配置区**（所有者诉求原话「代理单独开一个配置，
 * 三个选项 Github 国内版 国际版 加上描述」）。
 *
 * 为什么从「自动更新」卡片里搬出来，而不是原地加三个勾选框：
 *
 *  1. 代理的作用范围本轮已经**超出更新**（国际版/国服账号的上游请求都归它管），
 *     继续挂在「自动更新」下会让人以为它只影响更新 —— 那正是上一轮文案反复
 *     改措辞想解决的误解。
 *  2. 「自动更新」卡片在 **webui（浏览器打开宿主页面）下整块不渲染**
 *     （见页面底部的 `api.isWebui() ? null : <UpdateCard/>`），因为浏览器里
 *     不能安装桌面更新包。而代理配置是**纯宿主配置**，与能不能装更新无关 ——
 *     留在那张卡里等于「用浏览器打开时根本配不了代理」。
 *
 * 三格开关的语义差别很大，因此每格都必须带**描述**（所有者本次明确要求
 * 「加上描述」）：只说「国内版」用户无从判断该不该开。
 */
function NetworkProxyCard() {
  const [msg, setMsg] = useState<{ type: "ok" | "err"; text: string } | null>(null);
  const [githubConfig, setGithubConfig] = useState<GithubConfig>({});
  const [proxyUrl, setProxyUrl] = useState("");
  // 三个开关的初值是**默认值**而不是全 false：老配置里没有 proxy_scope 字段，
  // 用全 false 初始化会让界面在加载完成前把「国际版」显示成关闭（而后端实际
  // 是开着的）—— 一帧的假象也足以让人误判成「我的开关被重置了」。
  const [proxyScope, setProxyScope] = useState<ProxyScope>(DEFAULT_PROXY_SCOPE);
  const [proxySaving, setProxySaving] = useState(false);

  useEffect(() => {
    let cancelled = false;
    void api
      .getGithubConfig()
      .then((config) => {
        if (cancelled) return;
        setGithubConfig(config);
        setProxyUrl(config.proxy ?? "");
        // 逐键兜底：老配置没有 proxy_scope，必须回落默认值（见 proxyScopeOf）。
        setProxyScope(proxyScopeOf(config));
      })
      .catch((e) => {
        if (!cancelled) setMsg({ type: "err", text: api.asError(e) });
      });
    return () => {
      cancelled = true;
    };
  }, []);

  async function saveProxy() {
    const value = proxyUrl.trim();
    if (value) {
      try {
        const parsed = new URL(value);
        if (!parsed.hostname || !["http:", "https:"].includes(parsed.protocol)) {
          throw new Error("unsupported proxy protocol");
        }
      } catch {
        setMsg({ type: "err", text: "代理地址格式不正确，请填写 HTTP/HTTPS 地址，例如 http://127.0.0.1:7897" });
        return;
      }
    }

    setProxySaving(true);
    setMsg(null);
    try {
      // proxy_scope **必须一起提交**：后端保存接口写的是整份 github_config.json，
      // 漏传这个字段会让它按默认值补齐 —— 表现成「用户关掉国际版开关、一保存
      // 地址又自己开了」，而界面上看不出是谁改的。
      const saved = await api.saveGithubConfig({
        ...githubConfig,
        proxy: value,
        proxy_scope: proxyScope,
      });
      setGithubConfig(saved);
      setProxyUrl(saved.proxy ?? "");
      // 以**回读值**为准而不是提交值：后端可能有归一化（例如 trim），
      // 界面必须显示真正落盘的那一份，否则用户看到的是自己以为的结果。
      setProxyScope(proxyScopeOf(saved));
      setMsg({
        type: "ok",
        // 提示按**实际生效的开关**生成，而不是写死一句：用户关掉某一格之后
        // 仍看到「国际版会使用它」会以为开关没生效 —— 那是界面在撒谎。
        text: value
          ? proxyScopeSummary(proxyScopeOf(saved))
          : "未填写代理地址，全部直连（开关状态已保留）",
      });
    } catch (e) {
      setMsg({ type: "err", text: api.asError(e) });
    } finally {
      setProxySaving(false);
    }
  }

  return (
    <SettingsGroup id="settings-network-proxy" title="网络代理">
      <CardContent className="space-y-0 p-0">
        <SettingsFieldRow
          label="网络代理地址"
          // 该代理的适用范围在此前几轮里反复收窄过（先只服务 GitHub 更新、
          // 后来扩到国际版上游、再收窄成「仅国际版」）。本轮起范围不再写死在
          // 文案里，而是由下方**三个独立开关**控制 —— 因此这段描述只说
          // 「填一次、范围见下面开关」。复述出来的范围一旦与开关状态不符，
          // 就是界面在撒谎（而这正是前几轮反复改措辞的原因）。
          description="地址只填一次，下面的开关决定哪些范围使用它。留空表示全部直连。"
          htmlFor="update-proxy"
          className="bg-muted/25"
          operational
        >
          <Input
            id="update-proxy"
            className="w-full sm:w-80"
            value={proxyUrl}
            onChange={(event) => setProxyUrl(event.target.value)}
            placeholder="例如 http://127.0.0.1:7897"
            spellCheck={false}
            autoComplete="off"
          />
        </SettingsFieldRow>

        {/*
          三个独立开关（Github / 国内版 / 国际版）。

          为什么用 Checkbox 而不是 Switch：所有者在本仓库明确要求过「都改为勾选
          而不是手写」（见 AGENTS.md 的 UI Component Policy），且三行并列时勾选框
          比拨动开关更容易一眼看出「哪些被选中」。
        */}
        <div className="border-b border-border/60 bg-muted/25">
          {PROXY_SCOPE_FIELDS.map((field) => (
            <label
              key={field.key}
              htmlFor={field.id}
              data-slot="proxy-scope-row"
              data-scope-key={field.key}
              className="flex cursor-pointer items-start gap-2.5 border-b border-border/40 px-4 py-2.5 last:border-b-0 sm:px-5"
            >
              <Checkbox
                id={field.id}
                className="mt-0.5"
                checked={proxyScope[field.key]}
                onCheckedChange={(checked) =>
                  // checked 可能是 "indeterminate"（Radix 的三态）：一律按
                  // 「非 true 即 false」处理，避免把中间态写进配置。
                  setProxyScope((prev) => ({ ...prev, [field.key]: checked === true }))
                }
                aria-label={`代理范围：${field.label}`}
              />
              <span className="min-w-0 flex-1">
                <span className="block text-[13px] font-medium leading-4">{field.label}</span>
                <span className="mt-0.5 block text-xs leading-4 text-muted-foreground/75">
                  {field.description}
                </span>
              </span>
            </label>
          ))}
        </div>

        <div className="flex flex-wrap gap-2 border-b-0 border-border/60 px-4 py-3 sm:px-5">
          <DemoAction><Button size="sm" variant="outline" onClick={() => void saveProxy()} disabled={proxySaving}>
            {proxySaving ? <Loader2 className="animate-spin" /> : <Save />}
            保存代理
          </Button></DemoAction>
        </div>

        {msg && (
          <Alert
            variant={msg.type === "err" ? "destructive" : "default"}
            className="!w-auto mx-4 mb-4 sm:mx-5"
          >
            <AlertDescription>{msg.text}</AlertDescription>
          </Alert>
        )}
      </CardContent>
    </SettingsGroup>
  );
}

/** 自动更新：检查公开 GitHub Releases 源 + 安装签名更新。 */
function UpdateCard() {
  const version = useAccountsStore((s) => s.status?.version);
  const [info, setInfo] = useState<UpdateInfo | null>(null);
  const [checking, setChecking] = useState(false);
  const [installOpen, setInstallOpen] = useState(false);
  const [msg, setMsg] = useState<{ type: "ok" | "err"; text: string } | null>(null);
  // 「检查更新」按钮用的代理地址。**只读**：编辑入口在「网络代理」卡片里
  //（那块配置在 webui 下也要能改，而本卡片在 webui 下整块不渲染）。
  const [proxyUrl, setProxyUrl] = useState("");

  useEffect(() => {
    let cancelled = false;
    void api
      .getGithubConfig()
      .then((config) => {
        if (cancelled) return;
        setProxyUrl(config.proxy ?? "");
      })
      .catch((e) => {
        if (!cancelled) setMsg({ type: "err", text: api.asError(e) });
      });
    return () => {
      cancelled = true;
    };
  }, []);

  async function check() {
    setChecking(true);
    setMsg(null);
    try {
      // 传 null 而不是空串，让后端按「用户没填」处理；是否真的走代理由
      // Github 那个开关在后端决定（关掉时手动检查也必须直连）。
      const r = await api.checkUpdate(proxyUrl.trim() || undefined, true);
      setInfo(r);
      if (!r.ok) {
        setMsg({ type: "err", text: r.message || r.error || "检查失败" });
      }
    } catch (e) {
      setMsg({ type: "err", text: api.asError(e) });
    } finally {
      setChecking(false);
    }
  }

  return (
    <SettingsGroup
      id="settings-updates"
      title="自动更新"
    >
      <CardContent className="space-y-0 p-0">
        <div className="border-b border-border/60 px-4 py-3 text-sm sm:px-5">
          当前版本：<span className="font-mono">v{version || "?"}</span>
        </div>

        <div className="flex min-w-0 items-center justify-between gap-3 border-b border-border/60 bg-muted/25 px-4 py-3 text-sm sm:px-5">
          <div className="min-w-0 flex-1">
            <div className="font-medium">公开更新源</div>
            <div className="truncate text-xs text-muted-foreground">{GITHUB_REPOSITORY_URL}</div>
          </div>
          <DemoAction><Button
            variant="ghost"
            size="icon"
            title="打开 GitHub Release"
            onClick={() => void openReleaseUrl(GITHUB_RELEASE_URL)}
          >
            <ExternalLink />
          </Button></DemoAction>
        </div>

        {/*
          代理地址与三个开关**已搬到「网络代理」卡片**（见 NetworkProxyCard）。
          留在这里的重复控件会让两处状态各存一份 —— 在其中一处改完保存，
          另一处仍显示旧值，用户无法判断哪个是真的。
          本卡片只保留「用当前配置的代理检查一次更新」这个动作。
        */}

        <div className="flex flex-wrap gap-2 border-b-0 border-border/60 px-4 py-3 sm:px-5">
          <DemoAction><Button size="sm" variant="outline" onClick={check} disabled={checking}>
            {checking ? <Loader2 className="animate-spin" /> : <RefreshCw />}
            检查更新
          </Button></DemoAction>
        </div>

        {info?.ok && (
          <Alert variant="default" className={cn("!w-auto mx-4 my-4 sm:mx-5", info.hasUpdate && "border-primary/35 bg-primary/[0.06]")}>
            {info.hasUpdate && <ArrowUpCircle className="text-primary" />}
            <AlertDescription className="space-y-2">
              <AlertTitle className={cn(info.hasUpdate && "text-primary")}>{info.hasUpdate ? "发现新版本" : "更新检查完成"}</AlertTitle>
              <div className="text-sm">
                {info.hasUpdate
                  ? `发现新版本 v${info.latest}（当前 v${info.current}）`
                  : `已是最新版本 v${info.current}`}
                {info.releaseName && <span className="text-muted-foreground"> · {info.releaseName}</span>}
              </div>
              {info.hasUpdate && (
                <DemoAction><Button size="sm" onClick={() => setInstallOpen(true)}>
                  <ArrowUpCircle />
                  立即升级
                </Button></DemoAction>
              )}
              {info.releaseUrl && (
                <DemoAction><Button
                  variant="link"
                  size="sm"
                  className="h-auto p-0"
                  onClick={() => void openReleaseUrl(info.releaseUrl)}
                >
                  打开 GitHub Release
                </Button></DemoAction>
              )}
            </AlertDescription>
          </Alert>
        )}
        {msg && (
          <Alert
            variant={msg.type === "err" ? "destructive" : "default"}
            className="!w-auto mx-4 my-4 sm:mx-5"
          >
            <AlertDescription>{msg.text}</AlertDescription>
          </Alert>
        )}
        <UpdateInstallDialog
          open={installOpen}
          onOpenChange={setInstallOpen}
          update={info}
        />
      </CardContent>
    </SettingsGroup>
  );
}

/** 开机自启（仅桌面端渲染）：开关直接反映系统自启注册状态，切换立即生效。 */
function StartupCard() {
  const [enabled, setEnabled] = useState<boolean | null>(null);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ type: "ok" | "err"; text: string } | null>(null);

  useEffect(() => {
    let cancelled = false;
    void api
      .getLaunchAtLoginEnabled()
      .then((value) => {
        if (!cancelled) setEnabled(value);
      })
      .catch((e) => {
        if (!cancelled) setMsg({ type: "err", text: api.asError(e) });
      });
    return () => {
      cancelled = true;
    };
  }, []);

  async function onToggle(value: boolean) {
    if (busy || enabled === null) return;
    const previous = enabled;
    setBusy(true);
    setMsg(null);
    try {
      // 后端回读 OS 权威状态；即使与请求一致，也以回读值显示。
      const authoritative = await api.setLaunchAtLoginEnabled(value);
      setEnabled(authoritative);
      setMsg({ type: "ok", text: authoritative ? "已开启开机自启" : "已关闭开机自启" });
    } catch (e) {
      // 失败时恢复到最后一次确认的状态，并显示可读错误。
      setEnabled(previous);
      setMsg({ type: "err", text: api.asError(e) });
    } finally {
      setBusy(false);
    }
  }

  return (
    <SettingsGroup
      id="settings-startup"
      title="启动设置"
    >
      <CardContent className="space-y-0 p-0">
        <SettingsFieldRow
          className="border-b-0"
          label="开机时静默启动到托盘"
          description="开关直接反映系统登录项状态；之后可从托盘「打开主界面」恢复"
          htmlFor="startup-silent"
          operational
        >
          <Switch
            id="startup-silent"
            checked={enabled ?? false}
            disabled={busy || enabled === null}
            onCheckedChange={(v) => void onToggle(v)}
            aria-label="开机时静默启动到托盘"
          />
        </SettingsFieldRow>

        {msg && (
          <Alert
            variant={msg.type === "err" ? "destructive" : "default"}
            className="!w-auto mx-4 my-4 sm:mx-5"
          >
            <AlertDescription>{msg.text}</AlertDescription>
          </Alert>
        )}
      </CardContent>
    </SettingsGroup>
  );
}

/** 外观：主题选择（持久化到 localStorage）。 */
function AppearanceCard() {
  const [theme, setTheme] = useState<ThemePreference>(getThemePreference);

  function onThemeChange(value: string) {
    if (value !== "system" && value !== "light" && value !== "dark") return;
    setThemePreference(value);
    setTheme(value);
  }

  return (
    <SettingsGroup
      id="settings-appearance"
      title="外观"
    >
      <CardContent className="space-y-0 p-0">
        <SettingsFieldRow
          className="border-b-0"
          label="主题"
          description="选择浅色、深色，或跟随系统外观自动切换"
          htmlFor="appearance-theme"
        >
          <Select value={theme} onValueChange={onThemeChange}>
            <SelectTrigger id="appearance-theme" size="sm" className="w-full sm:w-40" aria-label="主题">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="system">系统</SelectItem>
              <SelectItem value="light">浅色</SelectItem>
              <SelectItem value="dark">深色</SelectItem>
            </SelectContent>
          </Select>
        </SettingsFieldRow>
      </CardContent>
    </SettingsGroup>
  );
}

/**
 * 多应用环境配置：Trae Work / Trae / 豆包 的安装路径与豆包端点。
 *
 * 为什么需要手动指定路径：客户端安装位置五花八门（自定义盘符、绿色版），
 * exe 发现链的 6 级回退仍可能在部分机器上落空。此时让用户直接给出路径，
 * 比让他反复重装客户端现实得多。
 */
function AppEnvCard() {
  const APPS = [
    { kind: "TraeWork", label: "Trae Work", hint: "TRAE SOLO CN.exe" },
    { kind: "Trae", label: "Trae", hint: "Trae CN.exe" },
    { kind: "Doubao", label: "豆包", hint: "Doubao.exe" },
  ] as const;

  const [envs, setEnvs] = useState<Record<string, api.AppEnvStatus | null>>({});
  const [paths, setPaths] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    const results = await Promise.all(
      APPS.map(async (app) => {
        try {
          return [app.kind, await api.appEnvCheck(app.kind)] as const;
        } catch {
          return [app.kind, null] as const;
        }
      }),
    );
    const nextEnvs: Record<string, api.AppEnvStatus | null> = {};
    const nextPaths: Record<string, string> = {};
    for (const [kind, status] of results) {
      nextEnvs[kind] = status;
      nextPaths[kind] = status?.manualPath ?? "";
    }
    setEnvs(nextEnvs);
    setPaths(nextPaths);
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const savePath = async (kind: string) => {
    setBusy(true);
    try {
      await api.appSetManualPath(kind, paths[kind] ?? "");
      toast.success("已保存安装路径");
      await load();
    } catch (e) {
      toast.error(api.asError(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <SettingsGroup id="settings-app-env" title="应用环境">
      <CardContent className="space-y-0 p-0">
        <p className="px-4 pt-4 text-xs text-muted-foreground sm:px-5">
          Trae Work / Trae / 豆包的安装路径。自动探测失败时可在此手动指定。
        </p>
        {APPS.map((app, index) => {
          const env = envs[app.kind];
          return (
            <div
              key={app.kind}
              className={cn(
                "space-y-2 px-4 py-4 sm:px-5",
                index < APPS.length - 1 && "border-b border-border/60",
              )}
            >
              <div className="flex flex-wrap items-center gap-2">
                <span className="text-sm font-medium">{app.label}</span>
                {env ? (
                  <>
                    <Badge variant={env.installed ? "secondary" : "outline"}>
                      {env.installed ? "已安装" : "未检测到"}
                    </Badge>
                    <Badge variant={env.running ? "secondary" : "outline"}>
                      {env.running ? "运行中" : "未运行"}
                    </Badge>
                    <span className="text-xs text-muted-foreground">
                      快照 {env.snapshotCount} 个
                    </span>
                  </>
                ) : (
                  <Badge variant="outline">检测失败</Badge>
                )}
              </div>
              {env?.exePath && (
                <p className="break-all font-mono text-xs text-muted-foreground">{env.exePath}</p>
              )}
              <div className="flex gap-2">
                <Input
                  value={paths[app.kind] ?? ""}
                  onChange={(e) =>
                    setPaths((prev) => ({ ...prev, [app.kind]: e.target.value }))
                  }
                  placeholder={`手动指定路径，例如 D:\\Programs\\${app.hint}`}
                  className="font-mono text-xs"
                  aria-label={`${app.label} 安装路径`}
                />
                <Button
                  size="sm"
                  variant="outline"
                  disabled={busy}
                  onClick={() => void savePath(app.kind)}
                >
                  保存
                </Button>
              </div>
            </div>
          );
        })}
      </CardContent>
    </SettingsGroup>
  );
}

/**
 * 本地代理：设备身份隔离与凭证抓取。
 *
 * 代理会**改写系统代理设置**，停止时还原为用户原有的值。因此界面必须把
 * 「当前是否在运行」「原有代理是什么」明确展示出来 —— 用户最怕的是
 * 「用了这个功能之后网断了，还不知道为什么」。
 */
function ProxyCard() {
  const [config, setConfig] = useState<api.ProxyConfigView | null>(null);
  const [running, setRunning] = useState(false);
  const [port, setPort] = useState("");
  const [domains, setDomains] = useState("");
  const [cert, setCert] = useState<Awaited<ReturnType<typeof api.proxyCertStatus>> | null>(null);
  const [logs, setLogs] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const [cfg, status, certStatus] = await Promise.all([
        api.proxyConfig(),
        api.proxyStatus(),
        api.proxyCertStatus(),
      ]);
      setConfig(cfg);
      setRunning(status.running);
      setCert(certStatus);
      setPort((current) => current || String(cfg.port));
      setDomains((current) => current || cfg.domains);
    } catch (e) {
      // 代理状态读取失败不打扰用户（可能是首次运行、证书目录还没建）
      console.error(api.asError(e));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // 代理日志实时推送：只保留最近 200 行，避免长时间运行把内存吃满
  useEffect(() => {
    if (!api.isDesktop()) return;
    let disposed = false;
    let unlisten: (() => void) | undefined;
    void import("@tauri-apps/api/event").then(async ({ listen }) => {
      const stop = await listen<{ line: string }>("proxy-log", (event) => {
        if (disposed) return;
        setLogs((prev) => [...prev, event.payload.line].slice(-200));
      });
      if (disposed) stop();
      else unlisten = stop;
    });
    return () => {
      disposed = true;
      unlisten?.();
    };
  }, []);

  const start = async () => {
    setBusy(true);
    try {
      const parsed = Number(port);
      if (!Number.isInteger(parsed) || parsed < 1 || parsed > 65535) {
        throw new Error("端口必须是 1-65535 之间的整数");
      }
      await api.proxyStart(parsed, domains);
      toast.success(`代理已启动，系统代理已指向 127.0.0.1:${parsed}`);
      await load();
    } catch (e) {
      toast.error(api.asError(e));
    } finally {
      setBusy(false);
    }
  };

  const stop = async () => {
    setBusy(true);
    try {
      await api.proxyStop();
      toast.success("代理已停止，系统代理已还原");
      await load();
    } catch (e) {
      toast.error(api.asError(e));
    } finally {
      setBusy(false);
    }
  };

  const genCert = async () => {
    setBusy(true);
    try {
      const res = await api.proxyCertGenerate();
      toast.success(`CA 证书已就绪：${res.caCerPath}`);
      await load();
    } catch (e) {
      toast.error(api.asError(e));
    } finally {
      setBusy(false);
    }
  };

  const captureLocal = async () => {
    setBusy(true);
    try {
      const res = await api.proxyCaptureLocal();
      if (res.ok) toast.success(res.message ?? "已从本机捕获凭证");
      else toast.warning(res.message ?? "未在本机找到可捕获的凭证");
    } catch (e) {
      toast.error(api.asError(e));
    } finally {
      setBusy(false);
    }
  };

  const existing = config?.existingSystemProxy;

  return (
    <SettingsGroup id="settings-proxy" title="本地代理">
      <CardContent className="space-y-0 p-0">
        <p className="px-4 pt-4 text-xs text-muted-foreground sm:px-5">
          拦截目标域名的请求，为每个账号注入独立设备标识，并自动抓取登录凭证。
          代理运行期间会接管系统代理设置，停止时还原。
        </p>

        <SettingsFieldRow
          label="运行状态"
          description={
            existing && existing[0]
              ? `检测到系统原有代理：${existing[1]}（停止时会还原为它）`
              : "当前未检测到系统代理，停止时会清空代理设置"
          }
        >
          <Badge variant={running ? "secondary" : "outline"}>{running ? "运行中" : "已停止"}</Badge>
        </SettingsFieldRow>

        <SettingsFieldRow label="监听端口" description="仅监听 127.0.0.1，不对局域网开放">
          <Input
            value={port}
            onChange={(e) => setPort(e.target.value)}
            disabled={running}
            className="w-full sm:w-32"
            aria-label="代理端口"
          />
        </SettingsFieldRow>

        <SettingsFieldRow
          label="拦截域名"
          description="逗号分隔，按后缀匹配；未命中的请求透明转发，不影响其他应用上网"
        >
          <Input
            value={domains}
            onChange={(e) => setDomains(e.target.value)}
            disabled={running}
            className="w-full font-mono text-xs sm:w-80"
            aria-label="拦截域名"
          />
        </SettingsFieldRow>

        <SettingsFieldRow
          label="CA 证书"
          description={
            cert?.caExists
              ? "已生成。需在系统中信任后 HTTPS 拦截才生效"
              : "尚未生成。首次启动代理时会自动创建"
          }
        >
          <div className="flex gap-2">
            <Button size="sm" variant="outline" disabled={busy} onClick={() => void genCert()}>
              生成
            </Button>
          </div>
        </SettingsFieldRow>

        {cert && !cert.caExists && (
          <Alert className="!w-auto mx-4 my-3 sm:mx-5">
            <AlertDescription className="text-xs">{cert.hint}</AlertDescription>
          </Alert>
        )}

        <div className="flex flex-wrap gap-2 px-4 py-4 sm:px-5">
          {running ? (
            <Button size="sm" variant="outline" disabled={busy} onClick={() => void stop()}>
              <Loader2 className={cn("size-3.5", busy && "animate-spin")} />
              停止代理
            </Button>
          ) : (
            <Button size="sm" disabled={busy} onClick={() => void start()}>
              <Loader2 className={cn("size-3.5", busy && "animate-spin")} />
              启动代理
            </Button>
          )}
          <Button size="sm" variant="outline" disabled={busy} onClick={() => void captureLocal()}>
            从本机捕获凭证
          </Button>
          <Button size="sm" variant="ghost" disabled={busy} onClick={() => void load()}>
            <RefreshCw className="size-3.5" />
            刷新状态
          </Button>
        </div>

        {logs.length > 0 && (
          <div className="px-4 pb-4 sm:px-5">
            <div className="mb-1 text-xs font-medium">代理日志</div>
            <pre className="max-h-40 overflow-auto rounded-lg border border-border/60 bg-muted/40 p-2 text-[11px] leading-5">
              {logs.join("\n")}
            </pre>
          </div>
        )}
      </CardContent>
    </SettingsGroup>
  );
}

/**
 * 计划任务：把签到 / 保活 / 额度巡检注册为 Windows 计划任务。
 *
 * 为什么需要系统级计划任务而不只靠应用内调度：应用内调度只在应用运行时有效。
 * 用户不会 24 小时开着这个工具，而「每天签到」必须每天都发生。
 */
function ScheduledTaskCard() {
  const [tasks, setTasks] = useState<api.TaskStatusItem[]>([]);
  const [times, setTimes] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const res = await api.taskStatus();
      setTasks(res.tasks);
      setTimes((prev) => {
        const next = { ...prev };
        for (const task of res.tasks) {
          if (next[task.kind] === undefined) {
            // 已注册的沿用系统里的时间；未注册的给个合理默认
            next[task.kind] = task.registered && task.time ? task.time : defaultTimeFor(task.kind);
          }
        }
        return next;
      });
    } catch (e) {
      toast.error(api.asError(e));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const register = async (kind: string) => {
    setBusy(true);
    try {
      const res = await api.taskRegister(kind, times[kind] ?? "09:00");
      toast.success(res.message);
      await load();
    } catch (e) {
      toast.error(api.asError(e));
    } finally {
      setBusy(false);
    }
  };

  const unregister = async (kind: string) => {
    setBusy(true);
    try {
      await api.taskUnregister(kind);
      toast.success("已删除计划任务");
      await load();
    } catch (e) {
      toast.error(api.asError(e));
    } finally {
      setBusy(false);
    }
  };

  const runNow = async (kind: string) => {
    setBusy(true);
    try {
      const res = await api.taskRunNow(kind);
      if (res.ok) toast.success("任务执行完成");
      else toast.warning(`任务执行结束但返回非零（退出码 ${res.exitCode}），请查看日志`);
    } catch (e) {
      toast.error(api.asError(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <SettingsGroup id="settings-tasks" title="计划任务">
      <CardContent className="space-y-0 p-0">
        <p className="px-4 pt-4 text-xs text-muted-foreground sm:px-5">
          注册为 Windows 计划任务后，即使应用没在运行也会按时执行。
          应用内调度仍然生效，两者互补。
        </p>
        {tasks.map((task, index) => (
          <div
            key={task.kind}
            className={cn(
              "space-y-2 px-4 py-4 sm:px-5",
              index < tasks.length - 1 && "border-b border-border/60",
            )}
          >
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-sm font-medium">{task.label}</span>
              <Badge variant={task.registered ? "secondary" : "outline"}>
                {task.registered ? `已注册 ${task.time || ""}`.trim() : "未注册"}
              </Badge>
            </div>
            {task.error && (
              <p className="text-xs text-destructive">{task.error}</p>
            )}
            <div className="flex flex-wrap items-center gap-2">
              <Input
                value={times[task.kind] ?? ""}
                onChange={(e) =>
                  setTimes((prev) => ({ ...prev, [task.kind]: e.target.value }))
                }
                placeholder="09:00"
                className="w-24"
                aria-label={`${task.label} 执行时间`}
              />
              <Button
                size="sm"
                variant="outline"
                disabled={busy}
                onClick={() => void register(task.kind)}
              >
                {task.registered ? "更新时间" : "注册"}
              </Button>
              {task.registered && (
                <Button
                  size="sm"
                  variant="ghost"
                  disabled={busy}
                  onClick={() => void unregister(task.kind)}
                >
                  删除
                </Button>
              )}
              <Button size="sm" variant="ghost" disabled={busy} onClick={() => void runNow(task.kind)}>
                立即执行
              </Button>
            </div>
          </div>
        ))}
        {tasks.length === 0 && (
          <p className="px-4 py-4 text-xs text-muted-foreground sm:px-5">
            正在读取计划任务状态…
          </p>
        )}
      </CardContent>
    </SettingsGroup>
  );
}

/** 各任务的默认执行时间。 */
function defaultTimeFor(kind: string): string {
  switch (kind) {
    case "trae_checkin":
      return "09:00";
    case "doubao_renew":
      return "09:00";
    case "doubao_quota":
      return "09:30";
    default:
      return "09:00";
  }
}

/** 设置页：自动签到配置 / 权限检测 / 更新配置。 */
export default function SettingsPage() {
  return (
    <div className="mx-auto min-w-0 w-full max-w-3xl px-4 py-6 sm:px-6 sm:py-8">
      <header className="mb-10 sm:mb-12">
        <h1 className="text-2xl font-semibold tracking-tight">设置</h1>
        <p className="mt-2 text-sm leading-6 text-muted-foreground">自动签到、权限检测与自动更新配置。</p>
      </header>

      <div className="min-w-0 space-y-12">
        <AppearanceCard />
        <AppEnvCard />
        <NetworkProxyCard />
        <ProxyCard />
        <ScheduledTaskCard />
        <RecordRetentionCard />
        <PermissionCheckCard />
        <AutoCheckinCard />
        <AutoCareTasksCard />
        <AutoRotateCard />
        {api.isDesktop() || api.isDemoMode() ? <StartupCard /> : null}
        {api.isWebui() && !api.isDemoMode() ? null : <UpdateCard />}
      </div>
    </div>
  );
}

/**
 * 记录保留：签到日志 / 积分快照 / 任务记录保留多久。
 *
 * 为什么做成勾选而不是输入框：保留天数是粗粒度选择，
 * 常用档位就那么几个；手填既容易填错（0、负数、极大值），
 * 也要用户自己去想「填多少合适」。
 * 后端仍会做区间归一化（1..3650），越界值会被夹到合法范围并回显真实值。
 */
function RecordRetentionCard() {
  const [setting, setSetting] = useState<api.RecordRetentionSetting | null>(null);
  const [saving, setSaving] = useState(false);
  const [msg, setMsg] = useState<{ type: "ok" | "err"; text: string } | null>(null);

  useEffect(() => {
    let alive = true;
    void (async () => {
      try {
        const res = await api.getRecordRetention();
        if (alive) setSetting(res);
      } catch (e) {
        if (alive) setMsg({ type: "err", text: api.asError(e) });
      }
    })();
    return () => {
      alive = false;
    };
  }, []);

  async function choose(days: number) {
    if (saving || setting?.days === days) return;
    setSaving(true);
    setMsg(null);
    try {
      const res = await api.saveRecordRetention(days);
      // 用后端返回的实际生效值刷新，避免界面与真实行为不一致
      setSetting((prev) => (prev ? { ...prev, days: res.days } : prev));
      setMsg({ type: "ok", text: `已保存：保留 ${res.days} 天` });
    } catch (e) {
      setMsg({ type: "err", text: api.asError(e) });
    } finally {
      setSaving(false);
    }
  }

  return (
    <SettingsGroup id="settings-retention" title="记录保留">
      <SettingsFieldRow
        label="本地记录保留天数"
        description="签到日志、积分快照与任务记录都会按此天数清理；超出部分在下次写入时自动删除。"
      >
        {setting ? (
          <div className="flex flex-wrap items-center gap-1.5">
            {setting.presets.map((p) => (
              <Button
                key={p.days}
                type="button"
                size="sm"
                variant={setting.days === p.days ? "default" : "outline"}
                className="h-7 px-2.5 text-xs"
                disabled={saving}
                onClick={() => void choose(p.days)}
              >
                {p.label}
              </Button>
            ))}
          </div>
        ) : (
          <span className="text-xs text-muted-foreground">加载中…</span>
        )}
      </SettingsFieldRow>
      {setting && !setting.presets.some((p) => p.days === setting.days) && (
        <SettingsRow>
          <div className="text-xs text-muted-foreground">
            当前为自定义值：<span className="font-medium text-foreground">{setting.days}</span> 天
            （可选范围 {setting.minDays}–{setting.maxDays}）
          </div>
        </SettingsRow>
      )}
      {msg && (
        <SettingsRow>
          <div
            className={cn(
              "text-xs",
              msg.type === "ok" ? "text-emerald-600 dark:text-emerald-500" : "text-destructive",
            )}
          >
            {msg.text}
          </div>
        </SettingsRow>
      )}
    </SettingsGroup>
  );
}
