import { ArrowRight, Ban, CalendarCheck, Cat, Check, CircleCheck, Clock3, Coins, Copy, Ellipsis, Gift, Globe, GraduationCap, History, Info, Loader2, Moon, PencilLine, PlaneTakeoff, RefreshCw, Save, Sparkles, Star, Trash2, Zap } from "lucide-react";
import { useState, type ReactNode } from "react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { DemoAction } from "@/components/demo-action";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { CodeBuddyCnIdeMark, CodeBuddyMark, WorkBuddyMark } from "@/components/product-marks";
import * as api from "@/lib/api";
import { accountReloginAlarm } from "@/lib/account-expiry";
import { cn } from "@/lib/utils";
import { AccountRecordsView } from "@/components/account-records-view";
import { demoModeEnabled } from "@/lib/demo-mode";
import type { AccountMeta, AccountRunningTask, CreditExpiry, CreditResource, GatewayTaskName, TravelStatus } from "@/lib/types";

const AVATAR_TONES = [
  "bg-emerald-100 text-emerald-800",
  "bg-violet-100 text-violet-800",
  "bg-sky-100 text-sky-800",
  "bg-amber-100 text-amber-800",
  "bg-rose-100 text-rose-800",
  "bg-teal-100 text-teal-800",
] as const;

function avatarTone(name: string) {
  let hash = 0;
  for (let i = 0; i < name.length; i += 1) hash = (hash * 31 + name.charCodeAt(i)) >>> 0;
  return AVATAR_TONES[hash % AVATAR_TONES.length];
}

function formatCredits(value: number): string {
  if (!Number.isFinite(value)) return "—";
  return new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 2 }).format(value);
}

function formatCreditExpiry(ts: number | null): string {
  if (!ts) return "长期有效";
  const date = new Date(ts);
  if (Number.isNaN(date.getTime())) return "长期有效";
  return `${String(date.getMonth() + 1).padStart(2, "0")}/${String(date.getDate()).padStart(2, "0")} 到期`;
}

function formatFullDate(ts: number | null): string {
  if (!ts) return "—";
  const date = new Date(ts);
  if (Number.isNaN(date.getTime())) return "—";
  return date.toLocaleDateString("zh-CN", { year: "numeric", month: "2-digit", day: "2-digit" });
}

function formatCreditUpdatedAt(ts: number | undefined): string {
  if (!ts) return "—";
  const date = new Date(ts);
  if (Number.isNaN(date.getTime())) return "—";
  return `${String(date.getHours()).padStart(2, "0")}:${String(date.getMinutes()).padStart(2, "0")}`;
}

function expiryClass(expired: boolean, expiringSoon: boolean): string {
  if (expired) return "text-destructive";
  if (expiringSoon) return "text-orange-600";
  return "text-muted-foreground";
}

function creditResources(credit?: CreditExpiry): CreditResource[] {
  return (credit?.resources ?? [])
    .filter((resource) => resource.remaining > 0)
    .map((resource, index) => ({ resource, index }))
    .sort((left, right) => {
      const leftExpiry = left.resource.expireAt ?? Number.POSITIVE_INFINITY;
      const rightExpiry = right.resource.expireAt ?? Number.POSITIVE_INFINITY;
      return leftExpiry === rightExpiry ? left.index - right.index : leftExpiry - rightExpiry;
    })
    .map(({ resource }) => resource);
}

function accountIdentity(account: AccountMeta): string {
  if (account.email) {
    const [local, domain] = account.email.split("@");
    if (!domain) return account.email;
    return `${local.slice(0, 1)}${"*".repeat(Math.max(3, local.length - 1))}@${domain}`;
  }
  return account.uid ? `UID · ${account.uid}` : `ID · ${account.id}`;
}

/**
 * 账号详情弹窗的字段行：[标签, 值, 悬停说明]。
 *
 * 目的：回答「我授权进来的到底是哪个号」。此前卡片只显示昵称与 uid/邮箱，
 * 而实际数据里还有手机号（国服账号的真实线索，其 email 常为空）、原始域名、
 * 账号类型、创建时间等 —— 这些恰好是辨认账号的关键。
 *
 * 值缺失时返回空串，由调用方渲染成「—」，保证行高与字段顺序稳定
 * （不因某个字段缺失而跳行）。
 */
function accountDetailRows(account: AccountMeta): [string, string, string?][] {
  const fmt = (ts: number | null | undefined) =>
    typeof ts === "number" && ts > 0 ? new Date(ts).toLocaleString("zh-CN") : "";
  return [
    ["备注", account.note ?? "", "你自己填的标签，用于区分这是谁的号"],
    ["昵称", account.nickname ?? ""],
    ["邮箱", account.email ?? "", "国际版账号通常靠它辨认"],
    ["手机号", account.phoneNumber ?? "", "国服账号的邮箱常为空，手机号是主要线索"],
    ["UID", account.uid ?? ""],
    ["账号 ID", account.id, "本地账号库的主键（与上游 UID 不同）"],
    ["所属区域", account.region ?? "", "由登录域名推导：国服 / 国际版"],
    ["登录域名", account.domain ?? "", "排查问题时的确切域名，比区域标签更具体"],
    ["账号类型", account.accountType === "personal" ? "个人版" : account.accountType === "enterprise" ? "企业版" : (account.accountType ?? "")],
    ["企业 / 组织", account.enterpriseName ?? ""],
    ["Token 到期", fmt(account.expiresAt)],
    ["Refresh 到期", fmt(account.refreshExpiresAt), "超过此时间需重新登录"],
    ["上次刷新", fmt(account.refreshedAt)],
    ["加入时间", fmt(account.createdAt)],
    // 仅在异常时出现，避免平时多一行无意义的「正常」。
    ...(account.needsRelogin
      ? ([["状态", `需重新登录${account.needsReloginReason ? `（${account.needsReloginReason}）` : ""}`]] as [string, string, string?][])
      : []),
  ];
}

const chipClass = "rounded-md px-1.5 py-0 text-[11px] font-medium";

/**
 * 「本轮已跑」标记的悬停说明。
 *
 * 为什么必须解释进度口径：`processed` 是**下界** —— Go 侧记录只在「成功且有新
 * 变化 / 失败 / 重要跳过」时写，且按天去重（`records.TaskDaily`），所以一轮里
 * 没有新变化的账号不会留下记录。不说明的话，用户看到「3/14」长时间不动会
 * 以为卡死了，而那其实是正常现象（Rust 侧 `task_processed_ids` 与设置页
 * `TaskRunningPanel` 都对此有明确警告，这里保持同一口径）。
 */
function runningTaskTitle(task: AccountRunningTask): string {
  const progress =
    task.total && task.total > 0
      ? `本轮进度：已记录 ${task.processed ?? 0} / ${task.total} 个账号（近似值，无新变化的账号不写记录，数字可能停住不动）。`
      : "本轮进度暂不可用。";
  return `本账号已参与「${task.label}」本轮任务。${progress}点开右上角菜单可单独运行本账号的养护任务。`;
}

function travelIconChip({
  label,
  tooltip,
  variant,
}: {
  label: string;
  tooltip: string;
  variant: "secondary" | "success";
}) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge variant={variant} className={cn(chipClass, "px-1")} aria-label={label}>
          <PlaneTakeoff className="size-3.5" />
        </Badge>
      </TooltipTrigger>
      <TooltipContent side="top">{tooltip}</TooltipContent>
    </Tooltip>
  );
}

function formatTravelRemaining(arriveAt: number | null | undefined): string | null {
  if (!arriveAt || arriveAt <= 0) return null;
  const arriveMs = arriveAt > 1e12 ? arriveAt : arriveAt * 1000;
  const remainingMs = arriveMs - Date.now();
  if (remainingMs <= 0) return "即将到达";
  const totalMinutes = Math.max(1, Math.ceil(remainingMs / 60_000));
  const hours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;
  if (hours > 0 && minutes > 0) return `剩余 ${hours} 小时 ${minutes} 分钟`;
  if (hours > 0) return `剩余 ${hours} 小时`;
  return `剩余 ${minutes} 分钟`;
}

function travelTooltip(status: TravelStatus): string {
  const place = status.locationName?.trim();
  const credit = status.rewardCredit;
  const points = credit != null ? `+${credit}` : null;
  const remaining = formatTravelRemaining(status.arriveAt);
  if (status.label === "traveling") {
    const parts = [place, points ? `预计 ${points}` : "旅行中", remaining].filter(Boolean);
    return parts.length > 0 ? parts.join(" · ") : "旅行中";
  }
  if (status.label === "finished") {
    if (place && points) return `${place} · ${points}`;
    if (place) return `${place} · 已结束`;
    if (points) return `已结束 · ${points}`;
    return "已结束";
  }
  // 领养相关状态：把**具体原因**说清楚，而不是只说"无 Buddy"让用户猜。
  if (status.label === "adopted") {
    return status.rewardCredit != null
      ? `已领养 Buddy，获得 ${status.rewardCredit} 分；今日尚未派出`
      : "已领养 Buddy；今日尚未派出";
  }
  if (status.label === "adopt-threshold") {
    return "已尝试领养，但上游要求先积累足够的对话轮次；攒够后可再次领养（约 +300 分）";
  }
  if (status.label === "no-buddy") {
    return "尚无 Buddy，且本次领养未成功（可稍后重试）";
  }
  return "未旅行";
}

/** 按旅行状态渲染标签：领养状态 / 无 Buddy / 未旅行 / 旅行中 / 已结束。 */
function travelChip(status: TravelStatus | undefined) {
  if (!status) return null;
  switch (status.label) {
    // 领养相关状态一律用**带文字**的标签（而非旅行状态的纯图标）：
    // 「有没有猫」「为什么领不了」是用户要主动处理的信息，藏在 tooltip 里
    // 等于没说 —— 用户会反复点领养却不知道为什么失败。
    case "adopted":
      return (
        <Tooltip>
          <TooltipTrigger asChild>
            <Badge variant="success" className={chipClass} aria-label="已领养 Buddy">
              <Cat className="size-3.5" />
              已领养
            </Badge>
          </TooltipTrigger>
          <TooltipContent side="top">{travelTooltip(status)}</TooltipContent>
        </Tooltip>
      );
    case "adopt-threshold":
      return (
        <Tooltip>
          <TooltipTrigger asChild>
            <Badge variant="secondary" className={cn(chipClass, "text-muted-foreground")} aria-label="待攒对话后可领养">
              <Cat className="size-3.5" />
              待攒对话
            </Badge>
          </TooltipTrigger>
          <TooltipContent side="top">{travelTooltip(status)}</TooltipContent>
        </Tooltip>
      );
    case "no-buddy":
      return (
        <Tooltip>
          <TooltipTrigger asChild>
            <Badge variant="secondary" className={cn(chipClass, "text-muted-foreground")} aria-label="无 Buddy">
              <Cat className="size-3.5" />
              无 Buddy
            </Badge>
          </TooltipTrigger>
          <TooltipContent side="top">{travelTooltip(status)}</TooltipContent>
        </Tooltip>
      );
    case "traveling":
      return travelIconChip({ label: travelTooltip(status), tooltip: travelTooltip(status), variant: "secondary" });
    case "finished":
      return travelIconChip({ label: travelTooltip(status), tooltip: travelTooltip(status), variant: "success" });
    case "untraveled":
    default:
      return <Badge variant="secondary" className={cn(chipClass, "text-muted-foreground")}>未旅行</Badge>;
  }
}

/** 国际版（workbuddy.ai）账号标注；国服账号不显示，避免噪音。 */
function regionChip(account: AccountMeta) {
  if (account.regionKey !== "intl") return null;
  const label = account.region || "国际版";
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge
          variant="outline"
          className={cn(chipClass, "gap-1 border-sky-500/30 bg-sky-500/10 text-sky-700")}
          aria-label={`${label}账号`}
        >
          <Globe className="size-3" />
          {label}
        </Badge>
      </TooltipTrigger>
      <TooltipContent side="top">
        国际版账号（{account.regionKey === "intl" ? "workbuddy.ai" : ""}）· 不参与自动签到与自动旅行
      </TooltipContent>
    </Tooltip>
  );
}

/**
 * 账号菜单里「养护任务」这一组的适用性判定。
 *
 * 为什么需要它：菜单此前只列了刷新 Token / 签到 / 领养三项，而项目里还有活跃上报、
 * 夜猫子、开学季、国际版 trial 等养号动作；更要紧的是**这些动作并非对所有账号都成立**
 * —— 国际版没有签到与任务中心，夜猫子只在 23:00–08:00 计入，开学季是限时活动。
 * 菜单若照列不误，用户点下去只会得到一次无意义的失败请求，且不知道原因。
 *
 * 判定依据**取自后端真实门槛**，不是前端猜的：
 *   - 区域：`config.rs::account_supported_by_auto_tasks`（`Region::of(account) == Cn`），
 *     域名以 `.ai` 结尾即国际版；`travel.rs::accounts_in_scope` 与
 *     `checkin.rs::accounts_in_scope` 都用它。
 *   - 夜猫子时段：`nightowl.go` 的 `nightWindowStartHour/EndHour`（23:00–08:00 CST）。
 *   - 签到状态：`checkin.rs::checkin_account` 返回的 `result: "already"`。
 */
type TaskAvailability = {
  /** false = 置灰：该动作对此账号不成立。 */
  enabled: boolean;
  /**
   * 置灰原因；启用时为 null。
   *
   * 渲染成菜单项下方的第二行小字，并同时进 `title` 与 `aria-label`
   * （形如「手动签到（不可用：国际版没有签到接口，上游返回空数据）」）——
   * 同一句话三处复用，视觉、悬停、读屏各取所需，不必维护多份文案。
   */
  reason: string | null;
};

/** 账号是否为国际版。只认 regionKey；它缺失时回退到域名后缀（与后端口径一致）。 */
function isIntlAccount(account: AccountMeta): boolean {
  if (account.regionKey) return account.regionKey === "intl";
  return (account.domain ?? "").trim().toLowerCase().endsWith(".ai");
}

/** 当前是否处于夜猫时段（23:00–08:00 CST），与 Go 侧 `withinNightWindow` 同口径。 */
function withinNightWindow(now = new Date()): boolean {
  // 用 UTC+8 固定偏移换算，不依赖本机时区：窗口定义来自上游活动规则，
  // 换台机器不应改变判定（Go 侧同样刻意避开 tzdata）。
  const cstHour = (now.getUTCHours() + 8) % 24;
  return cstHour >= 23 || cstHour < 8;
}

/** 手动签到：仅国服；已签到时仍列出但置灰，避免菜单项凭空消失。 */
function checkinAvailability(account: AccountMeta, todayCheckedIn?: boolean): TaskAvailability {
  if (isIntlAccount(account)) {
    return { enabled: false, reason: "国际版没有签到接口，上游返回空数据" };
  }
  if (todayCheckedIn) {
    return { enabled: false, reason: "今日已签到，明天再来" };
  }
  return { enabled: true, reason: null };
}

/** 领养 Buddy：国服才有猫猫旅行与 Buddy 体系。 */
function adoptAvailability(account: AccountMeta): TaskAvailability {
  if (isIntlAccount(account)) {
    return { enabled: false, reason: "国际版没有 Buddy 领养入口" };
  }
  return { enabled: true, reason: null };
}

/**
 * 活跃上报：只有国服 growth 接口有真实数据。
 *
 * 与签到的区别：这里**不**因「今日已跑」而置灰 —— 上报可重复执行（幂等，
 * 且是连登的自检手段），没有「今日已做」这种终态。
 */
function activityAvailability(account: AccountMeta): TaskAvailability {
  if (isIntlAccount(account)) {
    return { enabled: false, reason: "国际版 growth 接口无数据，上报不会计入" };
  }
  return { enabled: true, reason: null };
}

/** 夜猫子：国服 + 仅在 23:00–08:00（北京时间）内由上游计入。 */
function nightOwlAvailability(account: AccountMeta, now?: Date): TaskAvailability {
  if (isIntlAccount(account)) {
    return { enabled: false, reason: "国际版 growth 接口无数据，上报不会计入" };
  }
  if (!withinNightWindow(now)) {
    return { enabled: false, reason: "仅 23:00–08:00（北京时间）计入，当前不在时段内" };
  }
  return { enabled: true, reason: null };
}

/**
 * 开学季活动：国服限时活动（国际版返回 404），且活动下线后清单为空。
 *
 * 活动是否在期只有问上游才知道，前端**不猜**：这里只按区域置灰，
 * 在期与否交给接口返回的中文说明（`school.go` 会写「活动不在期…」记录）。
 */
function schoolAvailability(account: AccountMeta): TaskAvailability {
  if (isIntlAccount(account)) {
    return { enabled: false, reason: "国际版无此活动（上游返回 404）" };
  }
  return { enabled: true, reason: null };
}

/** 国际版 trial 加油包：与其它任务相反，**只对国际版**成立。 */
function trialAvailability(account: AccountMeta): TaskAvailability {
  if (!isIntlAccount(account)) {
    return { enabled: false, reason: "国际版专享，国服无此端点" };
  }
  return { enabled: true, reason: null };
}

/**
 * 这个账号**有没有 Buddy**——菜单文案要回答的真正问题。
 *
 * 为什么不能直接读 `travelStatus.label`：那个标签回答的是「**今天旅行到哪一步**」，
 * 与「有没有猫」是两件事，而 `TravelStatusLabel` 里只有 2 个值能证明没猫。
 * 逐个对照后端 `travel.rs::display_label`（唯一出处）：
 *
 * | label | 后端判定 | 关于 Buddy 能推出什么 |
 * |---|---|---|
 * | `adopted` | `skip=="adopted"`（领养成功） | **有**（刚领到） |
 * | `traveling` | `ok && !claimed`（已派出） | **有**（没猫派不出去） |
 * | `finished` | `same_day && claimed`（含 daily-limit） | **有**（今天派过猫） |
 * | `no-buddy` | `skip=="no-buddy"`（领养失败） | 没（今日记录） |
 * | `adopt-threshold` | `skip=="adopt-threshold"`（轮次不够） | 没（今日记录） |
 * | `untraveled` | 其余全部 | **不知道** |
 *
 * 关键在最后一行。`untraveled` 是 `display_label` 的兜底分支，它同时覆盖
 * 「从来没有记录」「记录是昨天的」「查询报错（status-error / config-error /
 * location-unavailable / claim-error）」——这些都**不能**推出没有猫。
 * 尤其 `roll_cache_to_today` 跨日时只保留在途记录（`result_in_flight`），
 * 于是**每个有猫的账号在第二天都会退化成 `untraveled`**：昨天领的猫、
 * 昨天派完的猫，记录全被丢掉。此前界面正是在这里出错 —— 把 `untraveled`
 * 当成「没有 Buddy」，于是一个养了几个月猫的账号在新的一天里又显示
 * 「领养 Buddy」。
 *
 * `travelStatus === undefined` 同样是「不知道」，且它有三种成因（仍在查询 /
 * 查询失败 / 国际版根本不查 —— `AccountsPage.accountsInScope` 只查国服），
 * 三者都不该被当成「断言没有猫」。
 *
 * 结论：只有 `no-buddy` / `adopt-threshold` 敢说没有；`untraveled` 与
 * undefined 一律按「未知」处理，界面保守表述。
 */
type BuddyKnowledge = "has" | "none" | "unknown";

function buddyKnowledge(status: TravelStatus | undefined): BuddyKnowledge {
  if (!status) return "unknown";
  // skip 优先于 label：它是后端给的原值，而 label 是它的有损投影
  //（例如 has-buddy 与 adopted 都会落成「有」）。两处都认，任一条成立即可。
  if (status.skip === "has-buddy" || status.skip === "adopted" || status.skip === "daily-limit") {
    return "has";
  }
  if (status.skip === "no-buddy" || status.skip === "adopt-threshold") {
    return "none";
  }
  switch (status.label) {
    case "adopted":
    case "traveling":
    case "finished":
      return "has";
    case "no-buddy":
    case "adopt-threshold":
      return "none";
    default:
      return "unknown";
  }
}

/**
 * 领养菜单项的文案。三态各有明确措辞，**未知态绝不断言**：
 *
 *   - `has`     → 「重新检查 Buddy」
 *   - `none`    → 「领养 Buddy」（有依据，可以放心点）
 *   - `unknown` → 「检查 / 领养 Buddy」
 *
 * 未知态为什么是「检查 / 领养」而不是「领养 Buddy（状态未知）」：菜单项的第一行
 * 是用户扫视时的动作标识，把「领养」摆在最前面，对于一个**可能已经有猫**的账号
 * 仍然是误导 —— 只是把误导从「断言」降级成了「暗示」。改成动词中性的
 * 「检查 / 领养」，说的正是后端 `adopt_for_account` 的真实行为：它先
 * `fetch_has_buddy`，已有猫就直接返回 `has-buddy`（**不做任何写操作**），
 * 没有才领养。文案与行为因此严格对应。
 *
 * 「状态未知」这件事本身放在第二行小字里（见 `adoptMenuHint`）说清楚：第一行
 * 要短且稳定，第二行才适合承载解释。
 */
function adoptMenuLabel(knowledge: BuddyKnowledge): string {
  if (knowledge === "has") return "重新检查 Buddy";
  if (knowledge === "none") return "领养 Buddy";
  return "检查 / 领养 Buddy";
}

/**
 * 领养项的悬停说明：把「我们现在知道什么」说清楚，把**后端原话**带上。
 *
 * 来源优先级：`message`（后端 `display_record` 透出的具体原因，如
 * 「无 Buddy（领养需先积累对话轮次）」）> 本地按状态生成的兜底说明。
 * 后端有话说时一律以后端为准 —— 它才知道真实原因，本地只能反推。
 */
function adoptMenuHint(status: TravelStatus | undefined, knowledge: BuddyKnowledge): string | undefined {
  const backendMessage = status?.message?.trim();
  if (backendMessage) return backendMessage;
  if (knowledge === "has") {
    return "该账号已有 Buddy（今天已派出过或刚领养），点这里会重新查询一次";
  }
  if (knowledge === "unknown") {
    // 三种成因分开讲：用户据此判断「要不要等一等再点」。
    return status
      ? "该账号今天没有查到 Buddy 记录（本轮巡检可能未覆盖，或查询失败），不能据此断定它没有猫；点这里会先查询、没有才领养"
      : "Buddy 状态尚未查询完成（或该账号不在旅行巡检范围内），不能据此断定它没有猫；点这里会先查询、没有才领养";
  }
  return undefined;
}

/** 刷新 Token：两个区域都需要，不受区域限制。 */
function refreshTokenAvailability(): TaskAvailability {
  return { enabled: true, reason: null };
}

/**
 * 渲染一个养护任务菜单项。
 *
 * 置灰项与可点项**渲染成同一个组件**（只是换文案与 aria 属性），原因：
 *   - 需求要求「不显示或置灰给提示」，两者混用会让菜单长度随账号类型跳变；
 *   - 置灰项一定要把原因说出来 —— 只置灰不解释，用户会当成 bug（这正是
 *     所有者反馈的痛点）。原因既写进可见文案，也写进 title 与 aria-label。
 */
function careTaskItem({
  icon,
  label,
  availability,
  onSelect,
  disabled,
  busy,
  hint,
}: {
  icon: ReactNode;
  label: string;
  availability: TaskAvailability;
  onSelect: () => void;
  /** 卡片级的统一禁用（如 featuresDisabled / 父级未接线）。 */
  disabled?: boolean;
  /** 该项正在执行中，临时不可点但**不**算「不适用」。 */
  busy?: boolean;
  /**
   * 可点项的补充说明（第二行小字）。
   *
   * 与 `availability.reason` 的分工：那个解释「为什么点不了」，这个在**能点**的
   * 情况下补充「点下去会发生什么 / 我们目前知道什么」。两者都渲染在第二行，
   * 因为位置上它们从不同时出现（不可用时只讲原因，可用时才轮到提示）。
   */
  hint?: string;
}) {
  const blocked = disabled || !availability.enabled || busy;
  // 「执行中」与「不适用」是两种不同的置灰：前者是暂时的，必须说清楚，
  // 否则用户看到灰项会以为这个号不支持该任务。
  const reason = busy ? "正在执行，请稍候…" : availability.reason;
  // 第二行的唯一出处：不可用讲原因，可用讲提示。共用一行避免菜单项高度乱跳。
  const secondLine = reason ?? hint ?? null;
  return (
    <DropdownMenuItem
      className="items-start"
      disabled={blocked}
      title={reason ?? hint ?? undefined}
      // aria-label 让读屏软件也读到原因，而不是只有视觉上的灰。
      aria-label={reason ? `${label}（不可用：${reason}）` : label}
      aria-disabled={blocked}
      onSelect={onSelect}
    >
      <span className="mt-0.5 flex shrink-0">{icon}</span>
      <span className="min-w-0 flex-1">
        <span className="block truncate">{label}</span>
        {secondLine ? (
          <span className="mt-0.5 block whitespace-normal text-[11px] leading-4 text-muted-foreground">
            {secondLine}
          </span>
        ) : null}
      </span>
    </DropdownMenuItem>
  );
}

interface Props {
  account: AccountMeta;
  onDelete: (a: AccountMeta) => void;
  /** 备注保存成功后触发，供父级重新拉取账号列表（卡片自身不持有列表状态）。 */
  onNoteSaved?: () => void;
  /**
   * 切换账号的禁用状态。
   *
   * 禁用 = 不进网关账号池；签到 / 旅行 / 领奖等养号任务照跑。
   * 由父级实现（需要提示、刷新列表），卡片只负责触发。
   */
  onToggleDisabled?: (a: AccountMeta) => void;
  onCheckin?: (a: AccountMeta) => void;
  onRefresh?: (a: AccountMeta) => void;
  /** 领养 Buddy（仅领养，不派猫；与「一键旅行」的重叠部分单独暴露出来） */
  onAdopt?: (a: AccountMeta) => void;
  /**
   * 手动触发一轮养号任务（活跃上报 / 夜猫子 / 开学季 / trial）。
   *
   * 注意语义：这是**整轮**触发，作用于全部账号，不是只跑当前卡片这个号
   *（Go 侧 `RunTaskByName` 遍历账号池）。菜单用分组标题把这一点说清楚。
   */
  onRunTask?: (task: GatewayTaskName) => void;
  /** 正在执行的任务名；用于临时置灰并避免重复触发。 */
  taskRunning?: GatewayTaskName;
  /**
   * 本账号参与了**正在跑的那一轮**养号任务时下发的标记；否则为 null/undefined。
   *
   * 由父级按 `taskRuntime.processedIds.includes(account.id)` 判定 —— 注意是
   * **账号库 id**，不是网关 uid（两者在真实数据里不同，用 uid 会一个都对不上
   * 且不会报错）。不在本轮范围内的账号（区域不符 / 已禁用 / 需重登）由后端
   * 的 `total` 口径排除，父级因此也不会给它标记 —— 否则用户会以为所有号都在跑。
   */
  runningTask?: AccountRunningTask | null;
  onSwitch?: (a: AccountMeta) => void;
  todayCheckedIn?: boolean;
  /** 今日旅行状态（undefined=查询中/未知，不渲染标签） */
  travelStatus?: TravelStatus;
  credit?: CreditExpiry;
  creditLoading?: boolean;
  /** 该账号积分最近一次查询完成时间（时间戳） */
  creditUpdatedAt?: number;
  creditPriority?: boolean;
  workbuddyActive?: boolean;
  codebuddyCliConfigured?: boolean;
  codebuddyCliActive?: boolean;
  /** 任一 CodeBuddy CLI 账号切换正在进行，用于阻止并发切换。 */
  codebuddyCliBusy?: boolean;
  onSwitchCodebuddyCli?: (a: AccountMeta) => void;
  /** 当前卡片是否为正在切换的目标账号。 */
  codebuddyCliLoading?: boolean;
  /** CodeBuddy CN IDE 是否已安装（可切换）。 */
  codebuddyCnIdeAvailable?: boolean;
  codebuddyCnIdeActive?: boolean;
  codebuddyCnIdeBusy?: boolean;
  codebuddyCnIdeLoading?: boolean;
  onSwitchCodebuddyCnIde?: (a: AccountMeta) => void;
  featuresDisabled?: boolean;
  /** 紧凑模式：头部缩成一条、按钮图标化、无 footer */
  compact?: boolean;
}

function ProductCurrentState({ product, compact = false }: { product: "workbuddy" | "codebuddy" | "codebuddy-cn"; compact?: boolean }) {
  const title =
    product === "workbuddy"
      ? "WorkBuddy 当前账号"
      : product === "codebuddy-cn"
        ? "CodeBuddy IDE 当前账号"
        : "CodeBuddy CLI 当前账号";
  return (
    <span
      role="status"
      aria-label={title}
      title={title}
      className={cn(
        "inline-flex items-center gap-2 rounded-full border border-primary/25 bg-primary/10 px-2.5 text-primary shadow-[inset_0_1px_0_rgba(255,255,255,.8)]",
        compact ? "h-7 text-xs" : "h-9",
      )}
    >
      {product === "workbuddy" ? (
        <WorkBuddyMark size={compact ? 18 : 22} />
      ) : product === "codebuddy-cn" ? (
        <CodeBuddyCnIdeMark size={compact ? 18 : 22} />
      ) : (
        <CodeBuddyMark size={compact ? 18 : 22} />
      )}
      <Check className={compact ? "size-3.5" : "size-4"} strokeWidth={2.25} />
    </span>
  );
}

export function AccountCard({ account, onDelete, onNoteSaved, onToggleDisabled, onCheckin, onRefresh, onAdopt, onRunTask, taskRunning, runningTask, onSwitch, todayCheckedIn, travelStatus, credit, creditLoading, creditUpdatedAt, creditPriority, workbuddyActive, codebuddyCliConfigured, codebuddyCliActive, codebuddyCliBusy, onSwitchCodebuddyCli, codebuddyCliLoading, codebuddyCnIdeAvailable, codebuddyCnIdeActive, codebuddyCnIdeBusy, codebuddyCnIdeLoading, onSwitchCodebuddyCnIde, featuresDisabled = true, compact = false }: Props) {
  const [resourcesOpen, setResourcesOpen] = useState(false);
  /** 备注编辑弹窗；`noteDraft` 是受控输入（打开时用当前备注初始化）。 */
  const [noteOpen, setNoteOpen] = useState(false);
  const [noteDraft, setNoteDraft] = useState("");
  const [noteSaving, setNoteSaving] = useState(false);
  /** 账号详情弹窗：展示本地记录里能看出「这是谁的号」的全部字段。 */
  const [detailOpen, setDetailOpen] = useState(false);
  /** 账号记录弹窗：任务 / 积分 / Token 三类事件，带日期筛选。 */
  const [recordsOpen, setRecordsOpen] = useState(false);
  const name = account.nickname || account.uid || "未命名账号";
  /** 需重新登录时的报警内容；账号仍能自愈（access token 过期）时为 null。
   *  判定口径集中在 `@/lib/account-expiry`，与兼容网关页共用同一套。 */
  const reloginAlarm = accountReloginAlarm(account);
  const avatarClass = avatarTone(name);
  const resources = creditResources(credit);
  const visibleResources = resources.slice(0, 2);
  const expiringAmount = credit?.ok ? credit.expiringSoonRemaining ?? 0 : 0;
  /** 弹窗内展示还有剩余的资源包（已用完的隐藏），按到期时间升序 */
  const allResources = (credit?.resources ?? [])
    .filter((resource) => resource.remaining > 0)
    .map((resource, index) => ({ resource, index }))
    .sort((left, right) => {
      const leftExpiry = left.resource.expireAt ?? Number.POSITIVE_INFINITY;
      const rightExpiry = right.resource.expireAt ?? Number.POSITIVE_INFINITY;
      return leftExpiry === rightExpiry ? left.index - right.index : leftExpiry - rightExpiry;
    })
    .map(({ resource }) => resource);

  const activeProductCount = [workbuddyActive, codebuddyCliActive, codebuddyCnIdeActive].filter(Boolean).length;

  /** 保存备注（空串 = 清除），成功后关闭弹窗并让父级刷新列表。 */
  async function submitNote() {
    setNoteSaving(true);
    try {
      await api.setAccountNote(account.id, noteDraft.trim());
      toast.success(noteDraft.trim() ? "备注已保存" : "备注已清除");
      setNoteOpen(false);
      onNoteSaved?.();
    } catch (e) {
      toast.error(api.asError(e));
    } finally {
      setNoteSaving(false);
    }
  }

  const statusChips = (
    <>
      {/* 禁用标记放在最前：它是「这个号当前不接流量」的最强信号，
          比备注/区域等描述性标签更需要一眼看到。 */}
      {account.disabled ? (
        <Badge
          variant="outline"
          className={cn(chipClass, "gap-1 border-destructive/40 text-destructive")}
          title="已禁用：不进入网关账号池（签到等养号任务仍在运行）"
        >
          <Ban className="size-3 shrink-0" />
          已禁用
        </Badge>
      ) : null}
      {/* 「本账号已参与本轮任务」标记。紧跟在「已禁用」之后、描述性标签（备注/区域/
          签到）**之前**：它是秒级出现又消失的实时状态，而其余标签都是账号的稳定属性。
          刻意**不**排到「已禁用」前面 —— 那条标签的注释已写明「放最前」的既有约定，
          这里不去推翻它（两者可以并存：禁用的号照跑养号任务）。
          措辞用「本轮已跑」而不是「正在跑」：后端只给 `processedIds`（**已留下
          记录**的账号），Go 侧记录是在处理完一个账号之后才写，因此本轮正在处理的
          那个号还没进集合，后端也没有「当前是哪个号」这个字段。照实说「已跑」，
          不编造一个后端并不提供的状态（详见 types.ts 的 AccountRunningTask）。 */}
      {runningTask && (
        <Badge
          variant="success"
          className={cn(chipClass, "max-w-[12rem] gap-1")}
          aria-label={`本账号本轮已跑：${runningTask.label}`}
          title={runningTaskTitle(runningTask)}
        >
          {/* 转圈图标暗示「任务仍在进行」，让静态文字带上时间感 */}
          <Loader2 className="size-3 shrink-0 animate-spin" />
          <span className="truncate">本轮已跑 · {runningTask.label}</span>
        </Badge>
      )}
      {/* 备注放在最前面：它是用户自己起的标签，正是用来「一眼认出这是谁的号」的，
          排在区域/签到等自动状态之前才符合使用意图。
          
          用 chip-note（信息蓝）而不是默认的灰：灰色与「国服」这类自动状态同色，
          备注反而看不出是「我自己写的东西」（所有者反馈过不够明显）。
          蓝＝用户写的 / 绿＝系统状态，一眼可分。
          
          宽度上限：宽松 12rem，紧凑收到 7.5rem（120px）——紧凑列宽只有约 300px，
          而头部一行的「不可压缩」需求算下来已超 452px（详见下方 compact 头部注释），
          备注若不收窄会把三个产品按钮挤出可视区。超出部分省略号 + 悬停看全文。 */}
      {account.note ? (
        <Badge
          variant="outline"
          className={cn(chipClass, "chip-note gap-1", compact ? "max-w-[7.5rem]" : "max-w-[12rem]")}
          title={`备注：${account.note}`}
        >
          <PencilLine className="size-3 shrink-0" />
          <span className="truncate">{account.note}</span>
        </Badge>
      ) : null}
      {regionChip(account)}
      {todayCheckedIn !== undefined && (
        <Badge variant={todayCheckedIn ? "success" : "secondary"} className={cn(chipClass, !todayCheckedIn && "text-muted-foreground")}><CircleCheck /> {todayCheckedIn ? "已签到" : "未签到"}</Badge>
      )}
      {travelChip(travelStatus)}
      {/* 只在**无法自愈**时才报警：access token 过期会自动刷新，不该打扰用户；
          真要人工介入的只有「上游拒绝」与「refresh token 也过期」两种。 */}
      {reloginAlarm && (
        <Badge variant="warning" className={chipClass} title={reloginAlarm.title}>
          {reloginAlarm.label}
        </Badge>
      )}
      {creditPriority && (
        <Tooltip>
          <TooltipTrigger asChild>
            <Badge variant="warning" className={cn(chipClass, "px-1")} aria-label="建议优先">
              <Star className="size-3.5" />
            </Badge>
          </TooltipTrigger>
          <TooltipContent side="top">建议优先使用</TooltipContent>
        </Tooltip>
      )}
      {!compact && activeProductCount >= 2 && <Badge variant="secondary" className={cn(chipClass, "text-muted-foreground")}>{activeProductCount} 个工具正在使用</Badge>}
    </>
  );

  return (
    <TooltipProvider>
      {/* h-full：撑满栅格行高。配合父级 grid 的 auto-rows-fr，让同一排的卡片
          无论有几个积分包都等高（详情区是 flex-1，多余空间落在底部，
          底部操作栏因此始终对齐）。 */}
      <article className="flex h-full min-w-0 flex-col overflow-hidden rounded-2xl border border-border bg-card shadow-[0_1px_2px_rgba(15,23,42,.025),0_10px_28px_rgba(15,23,42,.035)] transition-shadow hover:shadow-[0_2px_4px_rgba(15,23,42,.04),0_14px_34px_rgba(15,23,42,.055)]">
      <header
        className={cn(
          "relative flex items-center border-b border-border",
          compact ? "min-h-[52px] px-3.5 py-1.5" : "min-h-[104px] px-5 py-3",
          workbuddyActive ? "bg-primary/5" : codebuddyCliActive ? "bg-muted/60" : "bg-muted/30",
        )}
      >
        <div className="pointer-events-none absolute inset-0 overflow-hidden">
          <div
            className={cn(
              "absolute -right-10 -top-16 rounded-full blur-2xl",
              compact ? "size-20" : "size-24",
              workbuddyActive ? "bg-primary/15" : codebuddyCliActive ? "bg-muted/50" : "bg-muted/30",
            )}
          />
          {workbuddyActive && (
            <div className={cn("absolute top-[64%] -translate-y-1/2 opacity-[0.075] saturate-50 grayscale-[10%]", codebuddyCliActive ? "right-[68px] rotate-[8deg]" : "right-5 rotate-[7deg]")}>
              <WorkBuddyMark size={compact ? 40 : 56} />
            </div>
          )}
          {codebuddyCliActive && (
            <div className={cn("absolute top-[63%] -translate-y-1/2 opacity-[0.065] saturate-50 grayscale-[18%]", workbuddyActive ? "right-1 -rotate-[8deg]" : "right-5 -rotate-[7deg]")}>
              <CodeBuddyMark size={compact ? 38 : 54} />
            </div>
          )}
        </div>

        {/* ⋯ 按钮的位置：紧凑头部改成两行后不能再垂直居中（会落在两行之间、
            与产品图标不在同一水平线）。改为贴第一行中心：header py-1.5(6px) +
            产品图标 28px 的一半(14px) ⇒ 约 20px。宽松模式仍贴右上角。 */}
        <div className={cn("absolute z-20", compact ? "right-2.5 top-5" : "right-3.5 top-3.5")}>
          {demoModeEnabled ? (
            <DemoAction>
              <Button variant="ghost" size="icon" className={cn("rounded-lg text-muted-foreground hover:text-foreground", compact ? "size-7" : "size-8")} aria-label={`管理账号 ${name}`} title="更多账号操作">
                <Ellipsis />
              </Button>
            </DemoAction>
          ) : (
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" size="icon" className={cn("rounded-lg text-muted-foreground hover:text-foreground", compact ? "size-7" : "size-8")} aria-label={`管理账号 ${name}`} title="更多账号操作">
                  <Ellipsis />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-64">
                {/* 「本账号」组：每一项都只作用于这张卡片的账号。
                    标题不可省 —— 下面还有一组是整轮触发，混在一起会被误读。 */}
                <DropdownMenuLabel>本账号养护</DropdownMenuLabel>
                {careTaskItem({
                  icon: <RefreshCw />,
                  label: "刷新 Token",
                  availability: refreshTokenAvailability(),
                  onSelect: () => onRefresh?.(account),
                  disabled: featuresDisabled || !onRefresh,
                })}
                {/* 签到：国际版不适用 → 置灰说明原因；今日已签到 → 置灰但**保留**该项。
                    此前是 `todayCheckedIn === false &&` 条件渲染，已签到时整项消失，
                    用户会以为功能没了（详见报告的设计取舍）。 */}
                {careTaskItem({
                  icon: todayCheckedIn ? <CircleCheck /> : <CalendarCheck />,
                  label: todayCheckedIn ? "手动签到（今日已完成）" : "手动签到",
                  availability: checkinAvailability(account, todayCheckedIn),
                  onSelect: () => onCheckin?.(account),
                  disabled: featuresDisabled || !onCheckin,
                })}
                {/* 领养：措辞随**已知的 Buddy 状态**变化，而不是随旅行状态变化。
                    为什么不是直接读 travelStatus.label：见上方 buddyKnowledge 的
                    完整对照表 —— `untraveled` 是后端兜底分支，跨日后有猫的账号
                    也会落成它，把「尚未查询 / 今日无记录」当成「没有猫」正是
                    所有者反馈的那个缺陷。未知态用「（状态未知）」如实说明，
                    既不断言没有猫，也不假装已经有猫。
                    「旅行巡检也会顺带领养」这点保留在菜单里说清，因为一键旅行
                    确实覆盖它。 */}
                {careTaskItem({
                  icon: <Cat />,
                  label: adoptMenuLabel(buddyKnowledge(travelStatus)),
                  availability: adoptAvailability(account),
                  onSelect: () => onAdopt?.(account),
                  disabled: featuresDisabled || !onAdopt,
                  hint: adoptMenuHint(travelStatus, buddyKnowledge(travelStatus)),
                })}
                {/* 以下 4 项是养号任务，均由网关（Go 侧）按账号区域过滤：
                    活跃上报 / 夜猫子 / 开学季只跑国服，trial 只跑国际版。
                    菜单项本身仍逐号列出 —— 目的是让用户看懂「这个号为什么不参与」，
                    这一组的可点项触发的是**整轮**任务，故用分组标题明确边界。 */}
                <DropdownMenuSeparator />
                <DropdownMenuLabel>养号任务（触发一整轮，作用于全部账号）</DropdownMenuLabel>
                {careTaskItem({
                  icon: <Zap />,
                  label: "活跃上报",
                  availability: activityAvailability(account),
                  onSelect: () => onRunTask?.("activity"),
                  disabled: featuresDisabled || !onRunTask || taskRunning !== undefined,
                  busy: taskRunning === "activity",
                })}
                {careTaskItem({
                  icon: <Moon />,
                  label: "夜猫子任务",
                  availability: nightOwlAvailability(account),
                  onSelect: () => onRunTask?.("nightowl"),
                  disabled: featuresDisabled || !onRunTask || taskRunning !== undefined,
                  busy: taskRunning === "nightowl",
                })}
                {careTaskItem({
                  icon: <GraduationCap />,
                  label: "开学季活动",
                  availability: schoolAvailability(account),
                  onSelect: () => onRunTask?.("school"),
                  disabled: featuresDisabled || !onRunTask || taskRunning !== undefined,
                  busy: taskRunning === "school",
                })}
                {careTaskItem({
                  icon: <Gift />,
                  label: "trial 加油包",
                  availability: trialAvailability(account),
                  onSelect: () => onRunTask?.("trial"),
                  disabled: featuresDisabled || !onRunTask || taskRunning !== undefined,
                  busy: taskRunning === "trial",
                })}
                <DropdownMenuSeparator />
                {/* 备注：授权进来的账号常只带邮箱/手机号/随机 uid，看不出「这是谁的号」，
                    因此给一个自定义标签。文案随是否已有备注变化，避免用户以为要重填。 */}
                <DropdownMenuItem onSelect={() => setNoteOpen(true)}>
                  <PencilLine />
                  {account.note ? "修改备注" : "添加备注"}
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => setDetailOpen(true)}>
                  <Info />
                  查看账号详情
                </DropdownMenuItem>
                {/* 记录入口放在账号菜单里而不是单独一页：用户想知道「这个号昨天
                    干了什么」时，视线就在这张卡片上，不该再去别处找。 */}
                <DropdownMenuItem onSelect={() => setRecordsOpen(true)}>
                  <History />
                  查看记录
                </DropdownMenuItem>
                <DropdownMenuSeparator />
                {/* 禁用/启用：禁用的只是「进网关账号池的资格」，
                    签到等养号任务照跑，因此文案强调「不接流量」而非「停用账号」。 */}
                <DropdownMenuItem
                  onSelect={() => {
                    onToggleDisabled?.(account);
                  }}
                >
                  {account.disabled ? (
                    <>
                      <CircleCheck />
                      启用（重新加入账号池）
                    </>
                  ) : (
                    <>
                      <Ban />
                      禁用（不进入账号池）
                    </>
                  )}
                </DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem className="text-destructive focus:bg-destructive/5 focus:text-destructive" onSelect={() => onDelete(account)}>
                  <Trash2 />删除账号
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          )}
        </div>

        {compact ? (
          /* 紧凑头部改为**两行**（所有者确认的方案 B / 2-B）：
             第一行 = 名字 + 三个产品切换图标 + ⋯；第二行 = 状态标签。
             
             为什么必须分行：紧凑列宽 ≈ 300px（栅格 minmax(min(100%,300px),1fr)），
             而原来一行要放的全部是 shrink-0（不可压缩）：
               备注 chip(max-w-12rem=192) + 已签到(62) + 需重登(66) + 三图标(92) + ⋯预留(40)
               ≈ 452px > 300px
             结果就是**三个产品按钮被挤出可视区**（所有者实测反馈「按钮都挤下去了」）。
             原实现用 `min-[420px]:flex` 把状态区整个藏掉来回避，代价是窄列下状态全丢；
             分行则两边都保住：图标不再被挤，状态也还能换行显示。
             
             状态行不再限 hidden/min-[420px]：分行后它有自己的整行宽度，
             窄列下换行即可，没有必要再藏（藏了就等于「紧凑模式看不到状态」）。 */
          <div className="relative z-10 flex w-full min-w-0 flex-col">
            <div className="flex w-full min-w-0 items-center gap-2 pr-10">
              <h3 className="min-w-0 flex-1 truncate text-[13px] font-semibold leading-5" title={name}>{name}</h3>
              <div className="ml-auto flex shrink-0 items-center gap-1">
              {workbuddyActive ? (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <span className="relative inline-flex size-7 items-center justify-center rounded-lg border border-primary/25 bg-primary/10 text-primary">
                      <WorkBuddyMark size={15} />
                      <span className="absolute -right-1 -top-1 flex size-3.5 items-center justify-center rounded-full bg-primary text-primary-foreground">
                        <Check className="size-2.5" strokeWidth={3} />
                      </span>
                    </span>
                  </TooltipTrigger>
                  <TooltipContent side="top">WorkBuddy 当前账号</TooltipContent>
                </Tooltip>
              ) : demoModeEnabled ? (
                <DemoAction>
                  <Button variant="outline" size="icon" className="size-7 rounded-lg" aria-label="设为 WorkBuddy 当前账号">
                    <WorkBuddyMark size={15} />
                  </Button>
                </DemoAction>
              ) : (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <Button variant="outline" size="icon" className="size-7 rounded-lg" disabled={featuresDisabled || !onSwitch} onClick={() => onSwitch?.(account)} aria-label="设为 WorkBuddy 当前账号">
                      <WorkBuddyMark size={15} />
                    </Button>
                  </TooltipTrigger>
                  <TooltipContent side="top">设为 WorkBuddy 当前账号（会重启 WorkBuddy）</TooltipContent>
                </Tooltip>
              )}
              {codebuddyCnIdeActive ? (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <span className="relative inline-flex size-7 items-center justify-center rounded-lg border border-primary/25 bg-primary/10 text-primary">
                      <CodeBuddyCnIdeMark size={15} />
                      <span className="absolute -right-1 -top-1 flex size-3.5 items-center justify-center rounded-full bg-primary text-primary-foreground">
                        <Check className="size-2.5" strokeWidth={3} />
                      </span>
                    </span>
                  </TooltipTrigger>
                  <TooltipContent side="top">CodeBuddy IDE 当前账号</TooltipContent>
                </Tooltip>
              ) : (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <Button variant="outline" size="icon" className="relative size-7 rounded-lg" disabled={featuresDisabled || !codebuddyCnIdeAvailable || !onSwitchCodebuddyCnIde || codebuddyCnIdeBusy} onClick={() => onSwitchCodebuddyCnIde?.(account)} aria-label={codebuddyCnIdeLoading ? "正在切换 CodeBuddy IDE" : "切换到 CodeBuddy IDE"} aria-busy={codebuddyCnIdeLoading}>
                      {codebuddyCnIdeLoading ? <Loader2 className="size-3.5 animate-spin" /> : <CodeBuddyCnIdeMark size={15} />}
                    </Button>
                  </TooltipTrigger>
                  <TooltipContent side="top">{codebuddyCnIdeAvailable ? "切换到 CodeBuddy IDE（会重启 IDE）" : "未检测到 CodeBuddy IDE"}</TooltipContent>
                </Tooltip>
              )}
              {codebuddyCliActive ? (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <span className="relative inline-flex size-7 items-center justify-center rounded-lg border border-primary/25 bg-primary/10 text-primary">
                      <CodeBuddyMark size={15} />
                      <span className="absolute -right-1 -top-1 flex size-3.5 items-center justify-center rounded-full bg-primary text-primary-foreground">
                        <Check className="size-2.5" strokeWidth={3} />
                      </span>
                    </span>
                  </TooltipTrigger>
                  <TooltipContent side="top">CodeBuddy CLI 当前账号</TooltipContent>
                </Tooltip>
              ) : (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <Button variant="outline" size="icon" className="size-7 rounded-lg" disabled={featuresDisabled || !codebuddyCliConfigured || !onSwitchCodebuddyCli || codebuddyCliBusy} onClick={() => onSwitchCodebuddyCli?.(account)} aria-label={codebuddyCliLoading ? "正在切换 CodeBuddy CLI 当前账号" : "设为 CodeBuddy CLI 当前账号"} aria-busy={codebuddyCliLoading}>
                      {codebuddyCliLoading ? <Loader2 className="size-3.5 animate-spin" /> : <CodeBuddyMark size={15} />}
                    </Button>
                  </TooltipTrigger>
                  <TooltipContent side="top">{codebuddyCliConfigured ? "设为 CodeBuddy CLI 当前账号" : "请先接入 CodeBuddy CLI"}</TooltipContent>
                </Tooltip>
              )}
              </div>
            </div>
            {/* 第二行：状态标签。紧凑列宽约 300px，这一行独占整宽后可换行，
                所以不再需要原来那个 `hidden min-[420px]:flex` 的回避手段。 */}
            <div className="mt-1.5 flex w-full min-w-0 flex-wrap items-center gap-1">{statusChips}</div>
          </div>
        ) : (
          <div className={cn("relative z-10 flex w-full min-w-0 items-center gap-3", workbuddyActive || codebuddyCliActive ? "pr-[112px]" : "pr-10")}>
            <div className={cn("flex size-12 shrink-0 items-center justify-center rounded-full text-base font-semibold ring-4 ring-white/65", avatarClass)}>{name.charAt(0).toUpperCase()}</div>
            <div className="min-w-0 flex-1">
              <h3 className="truncate text-sm font-semibold leading-5" title={name}>{name}</h3>
              <p className="mt-0.5 truncate text-xs leading-5 text-muted-foreground" title={account.email || account.uid || account.id}>{accountIdentity(account)}</p>
              <div className="mt-1.5 flex min-w-0 flex-wrap items-center gap-1.5">{statusChips}</div>
            </div>
          </div>
        )}
      </header>

      <section className={cn("flex min-w-0 flex-1 flex-col", compact ? "px-3.5 pb-3 pt-3" : "px-5 pb-4 pt-4")}>
        {creditLoading ? (
          <div className="flex items-center gap-2 py-3 text-sm text-muted-foreground"><Loader2 className="size-4 animate-spin" />积分查询中…</div>
        ) : !credit ? (
          <div className="py-3 text-sm text-muted-foreground">等待积分数据…</div>
        ) : !credit.ok ? (
          <div className="flex min-w-0 items-center gap-2 py-3 text-sm text-destructive" title={credit.error}>
            <Coins className="size-4 shrink-0" />
            <span className="min-w-0 truncate">{credit.error || "积分查询失败"}</span>
          </div>
        ) : (
          <>
            <div className="flex items-baseline gap-x-3 gap-y-1">
              <span className="flex items-center gap-1.5">
                <Sparkles className="size-4 shrink-0 stroke-[1.75] text-muted-foreground" aria-hidden="true" />
                <strong className={cn("font-semibold leading-none tabular-nums tracking-[-0.025em]", compact ? "text-[20px]" : "text-[22px]")} style={{ fontFamily: '"Bricolage Grotesque Variable", "SF Pro Display", ui-sans-serif, sans-serif' }}>{formatCredits(credit.totalRemaining ?? 0)}</strong>
              </span>
              <span className={cn("text-muted-foreground", compact ? "text-[11px]" : "text-xs")}>{resources.length} 个积分包</span>
              <div className={cn("ml-auto flex items-center gap-1.5 text-muted-foreground", compact ? "text-[11px]" : "text-xs")} title={expiringAmount > 0 ? `${formatCredits(expiringAmount)} 积分将在 7 天内到期` : resources[0]?.expireAt ? `最近到期 ${formatCreditExpiry(resources[0].expireAt).replace(" 到期", "")}` : "当前积分长期有效"}>
                <Clock3 className="size-3.5 shrink-0" />
                <span className="whitespace-nowrap tabular-nums">{creditUpdatedAt ? `${formatCreditUpdatedAt(creditUpdatedAt)} 更新` : "—"}</span>
              </div>
            </div>

            <div className={cn("text-[11px] font-medium text-muted-foreground", compact ? "mt-3" : "mt-4")}>近期到期</div>
            <div className={cn(compact ? "mt-1.5 space-y-2" : "mt-2 space-y-2.5")}>
              {visibleResources.length > 0 ? visibleResources.map((resource, index) => {
                const resourceName = resource.packageName || resource.packageCode || "积分包";
                const ratio = resource.total > 0 ? Math.min(100, Math.max(0, (resource.remaining / resource.total) * 100)) : 0;
                return (
                  <div key={`${resource.packageCode ?? "resource"}-${resource.expireAt ?? "none"}-${index}`} className="min-w-0" title={`${resourceName} · 剩余 ${formatCredits(resource.remaining)} / ${formatCredits(resource.total)} · ${formatCreditExpiry(resource.expireAt)}`}>
                    <div className={cn("grid min-w-0 grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-3", compact ? "text-[11px]" : "text-xs")}>
                      <span className={cn("rounded-lg bg-muted/80 font-medium tabular-nums text-foreground", compact ? "px-1.5 py-0.5" : "px-2 py-1")}>{formatCredits(resource.remaining)} 积分</span>
                      <span className="truncate text-muted-foreground">{resourceName}</span>
                      <span className={cn("whitespace-nowrap tabular-nums", expiryClass(resource.expired, resource.expiringSoon))}>{formatCreditExpiry(resource.expireAt)}</span>
                    </div>
                    <div className={cn("h-1 overflow-hidden rounded-full bg-muted", compact ? "mt-1" : "mt-1.5")} aria-hidden="true">
                      <div className={cn("h-full rounded-full", resource.expiringSoon || resource.expired ? "bg-orange-500" : "bg-primary")} style={{ width: `${ratio}%` }} />
                    </div>
                  </div>
                );
              }) : <div className="py-1 text-[11px] text-muted-foreground">暂无可用积分</div>}
            </div>

            {resources.length > 2 && (
              <button type="button" className={cn("inline-flex w-fit items-center gap-1.5 font-medium text-primary transition-colors hover:text-primary/80 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/30", compact ? "mt-2 text-[11px]" : "mt-3 text-xs")} onClick={() => setResourcesOpen(true)}>
                查看全部积分包
                <ArrowRight className="size-3.5" />
              </button>
            )}
          </>
        )}
      </section>

      {!compact && (
        <footer className="flex flex-wrap items-center gap-2.5 border-t px-5 py-2.5">
          {workbuddyActive ? <ProductCurrentState product="workbuddy" compact /> : demoModeEnabled ? (
            <DemoAction>
              <Button variant="outline" size="sm" className="h-7 rounded-full px-2.5 pr-3.5 text-xs" aria-label="设为 WorkBuddy 当前账号">
                <WorkBuddyMark size={18} /><span>设为当前</span>
              </Button>
            </DemoAction>
          ) : (
            <Tooltip>
              <TooltipTrigger asChild>
                <Button variant="outline" size="sm" className="h-7 rounded-full px-2.5 pr-3.5 text-xs" disabled={featuresDisabled || !onSwitch} onClick={() => onSwitch?.(account)} aria-label="设为 WorkBuddy 当前账号">
                  <WorkBuddyMark size={18} /><span>设为当前</span>
                </Button>
              </TooltipTrigger>
              <TooltipContent side="top">设为 WorkBuddy 当前账号（会重启 WorkBuddy）</TooltipContent>
            </Tooltip>
          )}
          {codebuddyCnIdeActive ? <ProductCurrentState product="codebuddy-cn" compact /> : (
            <Tooltip>
              <TooltipTrigger asChild>
                <Button variant="outline" size="sm" className="h-7 rounded-full px-2.5 pr-3.5 text-xs" disabled={featuresDisabled || !codebuddyCnIdeAvailable || !onSwitchCodebuddyCnIde || codebuddyCnIdeBusy} onClick={() => onSwitchCodebuddyCnIde?.(account)} aria-label={codebuddyCnIdeLoading ? "正在切换 CodeBuddy IDE" : "切换到 CodeBuddy IDE"} aria-busy={codebuddyCnIdeLoading}>
                  {codebuddyCnIdeLoading ? <Loader2 className="size-4 animate-spin" /> : <CodeBuddyCnIdeMark size={18} />}<span>{codebuddyCnIdeLoading ? "切换中…" : "IDE"}</span>
                </Button>
              </TooltipTrigger>
              <TooltipContent side="top">{codebuddyCnIdeAvailable ? "切换到 CodeBuddy IDE（会重启 IDE）" : "未检测到 CodeBuddy IDE"}</TooltipContent>
            </Tooltip>
          )}
          {codebuddyCliActive ? <ProductCurrentState product="codebuddy" compact /> : (
            <Tooltip>
              <TooltipTrigger asChild>
                <Button variant="outline" size="sm" className="h-7 rounded-full px-2.5 pr-3.5 text-xs" disabled={featuresDisabled || !codebuddyCliConfigured || !onSwitchCodebuddyCli || codebuddyCliBusy} onClick={() => onSwitchCodebuddyCli?.(account)} aria-label={codebuddyCliLoading ? "正在切换 CodeBuddy CLI 当前账号" : "设为 CodeBuddy CLI 当前账号"} aria-busy={codebuddyCliLoading}>
                  {codebuddyCliLoading ? <Loader2 className="size-4 animate-spin" /> : <CodeBuddyMark size={18} />}<span>{codebuddyCliLoading ? "切换中…" : "CLI 当前"}</span>
                </Button>
              </TooltipTrigger>
              <TooltipContent side="top">{codebuddyCliConfigured ? "设为 CodeBuddy CLI 当前账号" : "请先接入 CodeBuddy CLI"}</TooltipContent>
            </Tooltip>
          )}
        </footer>
      )}
      </article>

      <Dialog open={resourcesOpen} onOpenChange={setResourcesOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>全部积分包</DialogTitle>
            <DialogDescription>{name} · 共 {allResources.length} 个积分包</DialogDescription>
          </DialogHeader>
          {allResources.length === 0 ? (
            <div className="px-1 py-6 text-center text-sm text-muted-foreground">当前没有可展示的资源包。</div>
          ) : (
            <div className="max-h-[60vh] min-w-0 overflow-y-auto divide-y divide-border/60">
              {allResources.map((resource, index) => {
                const ratio = resource.total > 0 ? Math.min(100, Math.max(0, (resource.remaining / resource.total) * 100)) : 0;
                return (
                  <div key={`${resource.packageCode || resource.packageName || "resource"}-${index}`} className="min-w-0 py-3 first:pt-0 last:pb-0">
                    <div className="flex min-w-0 items-start justify-between gap-3">
                      <div className="min-w-0">
                        <div className="truncate text-sm font-medium">{resource.packageName || resource.packageCode || "未命名资源包"}</div>
                        <div className="mt-1 text-[11px] text-muted-foreground">
                          {resource.expired ? "已到期" : resource.expiringSoon ? "7 天内到期" : resource.expireAt ? `到期 ${formatFullDate(resource.expireAt)}` : "长期有效"}
                        </div>
                      </div>
                      <div className="shrink-0 text-right text-xs">
                        <div className="font-medium">{formatCredits(resource.remaining)} / {formatCredits(resource.total)}</div>
                        <div className="mt-1 text-[11px] text-muted-foreground">已用 {formatCredits(resource.used)}</div>
                      </div>
                    </div>
                    <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-muted" aria-hidden="true">
                      <div className={cn("h-full rounded-full", resource.expired ? "bg-destructive/60" : resource.expiringSoon ? "bg-orange-500/80" : "bg-primary/75")} style={{ width: `${ratio}%` }} />
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </DialogContent>
      </Dialog>

      {/* 备注编辑：让用户给账号起个自己认得出的名字。
          空串 = 清除（后端会删掉该字段，而不是留一个空值）。 */}
      <Dialog
        open={noteOpen}
        onOpenChange={(open) => {
          if (open) setNoteDraft(account.note ?? "");
          setNoteOpen(open);
        }}
      >
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>账号备注</DialogTitle>
            <DialogDescription>
              {name} · 备注只存在本机，用于区分「这是谁的号」；留空即清除。
            </DialogDescription>
          </DialogHeader>
          <Input
            value={noteDraft}
            onChange={(event) => setNoteDraft(event.target.value)}
            placeholder="例如：公司号 / 备用 / 张三"
            maxLength={40}
            spellCheck={false}
            autoComplete="off"
            onKeyDown={(event) => {
              // 回车即保存：备注是短文本，多一步点按钮没有意义。
              if (event.key === "Enter" && !noteSaving) {
                event.preventDefault();
                void submitNote();
              }
            }}
          />
          <DialogFooter>
            <Button variant="outline" onClick={() => setNoteOpen(false)} disabled={noteSaving}>
              取消
            </Button>
            <Button onClick={() => void submitNote()} disabled={noteSaving || noteDraft.trim() === (account.note ?? "")}>
              {noteSaving ? <Loader2 className="animate-spin" /> : <Save />}
              保存
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* 账号详情：把本地记录里能回答「这是谁的号」的字段集中展示。
          此前卡片只显示昵称 + uid/邮箱，用户看不出授权的是哪个账号。 */}
      <Dialog open={detailOpen} onOpenChange={setDetailOpen}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>账号详情</DialogTitle>
            <DialogDescription>{name}</DialogDescription>
          </DialogHeader>
          <div className="min-w-0 divide-y divide-border/60">
            {accountDetailRows(account).map(([label, value, hint]) => (
              <div key={label} className="flex min-w-0 items-start justify-between gap-4 py-2.5 first:pt-0 last:pb-0">
                <div className="shrink-0 text-xs text-muted-foreground" title={hint}>
                  {label}
                </div>
                <div className="min-w-0 flex-1 text-right">
                  {value ? (
                    <span className="break-all font-mono text-xs">{value}</span>
                  ) : (
                    <span className="text-xs text-muted-foreground/60">—</span>
                  )}
                </div>
              </div>
            ))}
          </div>
          <DialogFooter>
            {/* 复制 UID：排查问题时常要把它贴给别人，比手抄可靠。 */}
            <Button
              variant="outline"
              onClick={() => {
                void navigator.clipboard.writeText(account.uid || account.id);
                toast.success("已复制账号标识");
              }}
            >
              <Copy />
              复制 UID
            </Button>
            <Button onClick={() => setDetailOpen(false)}>关闭</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* 单账号记录：任务执行 / 积分变化 / Token 消耗，带日期筛选。
          用较宽的对话框（max-w-4xl）：这三类记录要在一屏里看清需要横向空间。 */}
      <Dialog open={recordsOpen} onOpenChange={setRecordsOpen}>
        <DialogContent className="sm:max-w-4xl">
          <DialogHeader>
            <DialogTitle>账号记录</DialogTitle>
            <DialogDescription>
              {name} 的任务执行、积分变化与 Token 消耗；可按日期区间筛选。
            </DialogDescription>
          </DialogHeader>
          <div className="min-w-0 max-h-[70vh] overflow-y-auto pr-1">
            <AccountRecordsView
              accounts={[]}
              fixedAccountId={account.id}
              compact
            />
          </div>
        </DialogContent>
      </Dialog>
    </TooltipProvider>
  );
}
