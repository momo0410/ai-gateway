import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import {
  Activity,
  AlertTriangle,
  ArrowDown,
  ArrowUp,
  ArrowUpDown,
  Bot,
  Check,
  CheckCircle2,
  ChevronDown,
  ChevronRight,
  Copy,
  Filter,
  Globe,
  LayoutGrid,
  ListFilter,
  Loader2,
  Play,
  Recycle,
  RefreshCw,
  RotateCw,
  Rows3,
  Save,
  Server,
  Shuffle,
  Skull,
  Square,
  UserRound,
  Wand2,
  X,
  Zap,
} from "lucide-react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Separator } from "@/components/ui/separator";
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Switch } from "@/components/ui/switch";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import * as api from "@/lib/api";
import { useAccountsStore } from "@/stores/accounts";
import { useVisibilityInterval } from "@/lib/use-visibility-interval";
import type {
  AccountMeta,
  CreditExpiry,
  CreditResource,
  CreditStatistics,
  GatewayConfig,
  GatewayMode,
  GatewayPoolAccount,
  GatewayPortCheck,
  GatewayPortHolder,
  GatewayStatus,
  GatewayStatusAccount,
  GatewayUsageGroup,
  GatewayUsageResult,
  GatewayUsageSnapshot,
} from "@/lib/types";
import { cn } from "@/lib/utils";
import { toast } from "sonner";

interface SectionProps {
  title: string;
  description?: string;
  children: React.ReactNode;
}

function Section({
  title,
  description,
  children,
  className,
}: SectionProps & { className?: string }) {
  return (
    // flex-col + Card 的 flex-1：让同一栅格行内的卡片**等高**。
    // 否则左右两栏内容量不同（左边 4 个状态块、右边 5 行设置）时高度参差，
    // 视觉上像没对齐的拼贴。
    <section className={cn("flex min-w-0 flex-col space-y-2.5", className)}>
      <div className="px-1">
        <h2 className="text-[13px] font-medium leading-5">{title}</h2>
        {description ? (
          <p className="mt-0.5 text-xs text-muted-foreground">{description}</p>
        ) : null}
      </div>
      <Card className="min-w-0 flex-1 gap-0 overflow-hidden rounded-xl py-0 shadow-none">{children}</Card>
    </section>
  );
}

function Row({ children, className }: { children: React.ReactNode; className?: string }) {
  return (
    <div
      className={cn(
        "mx-4 flex min-w-0 flex-wrap items-center justify-between gap-3 border-b border-border/50 py-2.5 last:border-b-0 sm:mx-5",
        className,
      )}
    >
      {children}
    </div>
  );
}

function Stat({
  label,
  value,
  hint,
  tone,
}: {
  label: string;
  value: React.ReactNode;
  hint?: React.ReactNode;
  tone?: "ok" | "warn" | "off";
}) {
  return (
    <div className="min-w-0 rounded-lg border border-border/60 px-3 py-2">
      <div className="text-[11px] text-muted-foreground">{label}</div>
      <div
        className={cn(
          "mt-0.5 truncate text-[15px] font-medium tabular-nums",
          tone === "ok" && "text-emerald-600 dark:text-emerald-400",
          tone === "warn" && "text-amber-600 dark:text-amber-400",
          tone === "off" && "text-muted-foreground",
        )}
      >
        {value}
      </div>
      {hint ? <div className="mt-0.5 truncate text-[11px] tabular-nums text-muted-foreground">{hint}</div> : null}
    </div>
  );
}

/** 网关 Token 用量的统计范围选项。 */
type UsageRangeKey = "today" | "7d" | "30d" | "all";

const USAGE_RANGE_OPTIONS: { key: UsageRangeKey; label: string; days?: number }[] = [
  { key: "today", label: "今日", days: 1 },
  { key: "7d", label: "近 7 天", days: 7 },
  { key: "30d", label: "近 30 天", days: 30 },
  { key: "all", label: "全部" },
];

const exactTokenFormatter = new Intl.NumberFormat("en-US");

/**
 * 「Token 用量」区块里承载口径信息的**次级文字**（「共 N 个」、行内数值、行内 meta）
 * 统一用的前景色。
 *
 * 为什么不能用既有的 `text-muted-foreground`：诊断对真实渲染取色实测，「共 N 个」
 * 的对比度只有 **2.97:1**，而 WCAG AA 正文要求 **4.5:1**。更要命的是它是整个用量
 * 区块里**唯一**携带「筛掉了几行 / 这是谁的口径」的元素 —— 最该被看见的字最淡，
 * 正是所有者「看不出变化」的放大因素之一。meta 行原先是 0.8 不透明度，同理。
 *
 * `--muted-strong` 是本轮为它新增的 token（`src/index.css`，oklch 0.42），
 * 比 `--muted-foreground`（0.52）深一档；在 `--background`（1 0 0）与 `--card`
 * 上分别约 7.5:1 / 7.9:1，留足余量。
 *
 * 为什么不直接用 `text-foreground`：那样次级文字与主文字（模型名 / 账号名）
 * 同色，层级会塌掉 —— 修对比度不该以牺牲信息层级为代价。
 */
const USAGE_STRONG_TEXT = "text-muted-strong";

/**
 * 「按模型 / 按账号」两个列表在 420px 高度内大概能完整显示的行数。
 *
 * 用途只有一个：决定要不要显示底部那行「共 N 个，列表可滚动」。行高约 56px
 * （名称行 + 条 + meta，再加 py-2），420 / 56 = 7.5 —— 取 7 是**保守**值：
 * 刚好 7 行时不给提示（此时列表末尾与容器底边几乎重合，用户能看出到底了），
 * 超过 7 行才提示，避免把"还有内容"这件事变成常驻噪音。
 */
const USAGE_LIST_VISIBLE_ROWS = 7;

/**
 * 网关页布局，持久化到 localStorage。
 *
 * - `classic`（默认）：**旧版布局** —— 账号池与「Token 用量」各自独立成块，
 *   账号卡片只显示池运行态。保持原样是为了「不改变既有习惯」：升级后打开
 *   看到的仍是熟悉的样子。
 * - `merged`：**新版布局** —— 把用量数据直接并进账号卡片（每张卡片多出
 *   「消耗积分 / 消耗 Token / 调用次数」），顶上一行日期筛选统一控制；
 *   看「谁在跑、各烧了多少」时不必在上下两块之间来回对照。
 *
 * 默认旧版 + 需要手动点击才切换：新布局信息密度高，属可选偏好，
 * 不该在用户没要求时改变默认观感。
 */
type GatewayLayout = "classic" | "merged";

const LAYOUT_STORAGE_KEY = "ai-gateway.gateway-layout";

/**
 * 把网关配置里的 `allowed_model` 归一化成字符串数组。
 *
 * **必须同时吃两种形状**：老配置里这个键是单值字符串
 *（实测所有者本机的 gateway_config.json 就是
 * `"allowed_model": "deepseek-v4.1-flash"`），新配置是数组。
 * 只认数组会让老配置在界面上显示成「全部」（= 不限制），而网关实际仍在限制 ——
 * 用户看到的是「我明明限制了，界面却说没限制」，比报错更难排查。
 *
 * 归一化：逐项 trim、丢弃空项、去重（与 Rust 侧 `allowed_models_of` 同一口径）。
 */
function normalizeAllowedModels(raw: string[] | string | null | undefined): string[] {
  const list = Array.isArray(raw) ? raw : typeof raw === "string" && raw.trim() !== "" ? [raw] : [];
  const out: string[] = [];
  for (const item of list) {
    if (typeof item !== "string") continue;
    const trimmed = item.trim();
    if (trimmed !== "" && !out.includes(trimmed)) out.push(trimmed);
  }
  return out;
}

/**
 * 「放行模型」控件：**多选**，默认「全部」。
 *
 * 与 Token 用量区块的「模型筛选」（`ModelFilter`，`data-slot="usage-model-filter"`）
 * 是**两个完全不同的东西**，刻意做成两个互不影响的控件：
 *
 * | | 放行模型（本控件，写配置） | 模型筛选（Token 用量区块） |
 * |---|---|---|
 * | 作用面 | **服务端**：网关只放行勾选的模型，其他被 400 拒绝 | **前端**：只改变本页展示哪些模型的用量 |
 * | 影响范围 | 真实影响客户端能否调用 | 只影响本页的**阅读**，不改任何配置、不写盘 |
 * | 生效方式 | 保存进 config，网关重启后生效 | 立即生效，仅本页 |
 * | 默认 | 全部放行（空集） | 全部显示（空集） |
 * | 模式 | **三个模式都有** | **三个模式都有** |
 *
 * 两者默认都是「全部」，但语义与后果完全不同 —— 所以标签必须说清是哪一种：
 * 本控件的标题是「放行模型」且说明里写明「网关会拒绝其它模型」，
 * 而 `ModelFilter` 的标题是「全部模型」（描述的是展示范围）。
 *
 * 为什么用 `DropdownMenuCheckboxItem` 而不是一排 toggle：所有者本机
 * `/v1/models` 实测返回 **30 个模型**，全部铺开会把区块挤成一面墙；
 * 而多选又必须能一眼看出「当前选了哪些」，纯下拉单选做不到。
 *
 * 为什么选中后菜单**不关闭**：勾选列表要连着点好几项，每点一次都关掉会让人
 * 反复重开（所有者原话就是嫌麻烦：「都改为勾选而不是手写，太麻烦了」）。
 */
function AllowedModelPicker({
  options,
  selected,
  onChange,
  onRefresh,
  refreshing,
}: {
  /** 可选模型（来自网关 /v1/models；取不到时为当前已选值）。 */
  options: string[];
  /** 已选模型；空数组 = 全部（不限制），这是默认值。 */
  selected: string[];
  onChange: (next: string[]) => void;
  onRefresh: () => void;
  refreshing: boolean;
}) {
  const all = selected.length === 0;
  const label = all
    ? "全部模型"
    : selected.length === 1
      ? selected[0]
      : `已限制 ${selected.length} 个`;

  function toggle(model: string) {
    // 从「全部」开始勾一个 = **只放行这一个**（把「全部」收窄成一项）。
    //
    // 与 ModelFilter 同一考虑：最常用的意图是「我只想放行某一个 / 某几个」。
    // 若从「全部」点一项等于把它排除，用户得连点 29 次才能达到「只放行一个」。
    if (all) {
      onChange([model]);
      return;
    }
    const next = selected.includes(model)
      ? selected.filter((m) => m !== model)
      : [...selected, model];
    // 取消到空 = 回到「全部放行」。**不能**理解成「一个都不放行」——
    // 那等于网关拒绝所有请求，绝不会是用户的本意。
    onChange(next);
  }

  // 已选但不在可选列表里的模型也要能看见（否则网关列表变更后用户无从取消）。
  const missing = selected.filter((m) => !options.includes(m));

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          variant="outline"
          size="sm"
          className="h-8 w-56 shrink-0 justify-start gap-1.5 px-2.5 text-xs"
          aria-label={`放行模型：${label}`}
          data-slot="allowed-model-picker"
        >
          <Filter className="size-3.5" />
          <span className="min-w-0 flex-1 truncate text-left">{label}</span>
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="max-h-80 w-64 overflow-y-auto">
        <DropdownMenuLabel>放行模型（网关会拒绝其它模型）</DropdownMenuLabel>
        <DropdownMenuItem
          onSelect={(e) => {
            // 勾选列表要连着点多项，选中后不关闭菜单。
            e.preventDefault();
            onChange([]);
          }}
          aria-label="放行模型：全部模型"
        >
          <span className="flex size-4 shrink-0 items-center justify-center">
            {all ? <Check className="size-3.5" /> : null}
          </span>
          <span className="min-w-0 flex-1 truncate">全部模型（不限制）</span>
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        {options.length === 0 && missing.length === 0 ? (
          <div className="px-2.5 py-2 text-xs text-muted-foreground">
            暂无模型列表，可点右侧刷新按钮重试
          </div>
        ) : null}
        {/* 已选但已不在列表里的排在最前：它们仍然生效，用户需要能看到并取消。 */}
        {missing.map((model) => (
          <DropdownMenuCheckboxItem
            key={`missing-${model}`}
            checked
            onSelect={(e) => e.preventDefault()}
            onCheckedChange={() => toggle(model)}
            aria-label={`放行模型：${model}`}
          >
            <span className="min-w-0 flex-1 truncate">{model}</span>
            <span className="shrink-0 text-[11px] text-muted-foreground">不在当前列表</span>
          </DropdownMenuCheckboxItem>
        ))}
        {options.map((model) => (
          <DropdownMenuCheckboxItem
            key={model}
            checked={selected.includes(model)}
            onSelect={(e) => e.preventDefault()}
            onCheckedChange={() => toggle(model)}
            aria-label={`放行模型：${model}`}
          >
            <span className="min-w-0 flex-1 truncate">{model}</span>
          </DropdownMenuCheckboxItem>
        ))}
        <DropdownMenuSeparator />
        <DropdownMenuItem
          onSelect={(e) => {
            e.preventDefault();
            onRefresh();
          }}
          aria-label="刷新模型列表"
        >
          <RefreshCw className={cn("size-3.5", refreshing && "animate-spin")} />
          刷新模型列表
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/** 大数紧凑展示（与 Token 统计页的 K/M/B 风格一致）。 */
function formatUsageCompact(value: number): string {
  const abs = Math.abs(value);
  if (abs >= 1_000_000_000) return `${(value / 1_000_000_000).toFixed(1)}B`;
  if (abs >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`;
  if (abs >= 1_000) return `${(value / 1_000).toFixed(1)}K`;
  return exactTokenFormatter.format(value);
}

/** 一组数值的最大值（至少为 1，避免除零）。 */
function maxOf(values: number[]): number {
  return values.reduce((max, value) => Math.max(max, value), 1);
}

/**
 * 用量分布行：名称 + 占比条 + 数值（可带底部说明）。
 *
 * 「Token 用量」区块的双向联动交叉筛选就挂在这里：给了 `onToggle` 的行变成真正的
 * `<button>` 且带 `aria-pressed`，可点、可 Tab、可读屏。
 *
 * 为什么 `onToggle` 是**可选**的（不是让所有调用点都能点）：
 *   - 「按模型 / 按账号」两个列表要能互相筛选；
 *   - 而单账号的模型明细（`AccountModelDetail`）已随「账号池选中」一起移除，
 *     本页现在只有这一处调用点会接线。
 *   因此交互能力仍由调用点显式开启，保持「可点 / 不可点」的语义显式化。
 *
 * 视觉上可点/不可点**完全一致**（只叠加选中态与 hover）：所有者明确要求
 * 「现有信息一条都不能删」，所以 label / 占比条 / 数值 / meta 的排版与字号
 * 一个都不动，只在最外层换标签。
 */
function UsageBarRow({
  label,
  value,
  max,
  meta,
  selected = false,
  onToggle,
  dataSlot,
  valueSlot,
}: {
  label: string;
  value: number;
  max: number;
  meta?: string;
  /** 是否处于选中态（仅 `onToggle` 存在时有意义）。 */
  selected?: boolean;
  /**
   * 点击回调（再点一次 = 取消选择）。为 undefined 时本行退化为纯展示 `<div>` ——
   * 与本次改动之前逐字一致。
   */
  onToggle?: () => void;
  /** 标记本行属于哪个列表，供测试与调试定位（不影响样式）。 */
  dataSlot?: string;
  /**
   * 数值 span 的 `data-slot`。
   *
   * 为什么数值要单独可定位：「点账号 → 左侧数值换成该账号在该模型上的量」是本轮
   * 语义修正的**核心断言**，而行的 `innerText` 把数值与 meta 混在一起，从整行文本里
   * 抠数字会在「数值恰好也是 meta 里某个数字的子串」时误判。给数值一个稳定定位点，
   * 测试才能断言「这个 span 的文字本身变了」，而不是「整行里出现过这个数」。
   */
  valueSlot?: string;
}) {
  const percent = max > 0 ? Math.max(3, Math.round((value / max) * 100)) : 0;
  // 可点时必须用真实的 <button>（而不是给 div 挂 onClick）：键盘 Tab / Enter 与
  // 读屏都依赖原生语义。与账号池卡片 (`PoolAccountRow`) 同一套约定。
  const Root = onToggle ? "button" : "div";
  const rootProps = onToggle
    ? {
        type: "button" as const,
        onClick: onToggle,
        // aria-pressed 表达「这一项正被用作筛选条件」，与项目既有做法一致。
        "aria-pressed": selected,
        // 选中态用 --primary 系：本项目踩过「拿 secondary 当选中态、与 outline 只差
        // 4% 亮度，看起来永远停在全部上」的坑，故选中一律走 primary（浅底 + 中描边
        // + 加粗）。非选中态**不加任何底色**，保证未选时观感与改动前一致。
        className: cn(
          "block w-full cursor-pointer space-y-1.5 px-4 py-2 text-left transition-colors sm:px-5",
          "hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset",
          selected && "bg-primary/10 font-medium ring-1 ring-inset ring-primary/40",
        ),
        title: selected ? "再次点击取消按此项筛选" : `点击按此项筛选（只显示与它相关的${label}）`,
      }
    : { className: "space-y-1.5 px-4 py-2 sm:px-5" };
  return (
    <Root {...(rootProps as Record<string, unknown>)} data-slot={dataSlot}>
      <div className="flex items-baseline justify-between gap-3 text-xs">
        <span className="flex min-w-0 items-center gap-1.5">
          {/*
            行首勾选框（**装饰性**，不是可交互控件）。

            为什么不用 Radix `Checkbox`（本项目 UI 规范首选 shadcn 组件）：整行本身
            已经是 `<button>`，在按钮里再放一个 `role="checkbox"` 的真实控件是嵌套
            交互元素（HTML 不允许，React 会报 validateDOMNesting），并且会出现
            「点勾选框」与「点行」两种说不清的语义。所有者原话是「我勾选右边账号」
            —— 他要的是**看得出自己勾了哪个**，而不是要一个独立的复选框。因此状态
            语义仍由行上的 `aria-pressed` 承担（读屏已可读），这个方框只补他眼睛要找
            的那个标记，故标 `aria-hidden` 明确叫读屏别重复念。

            未选时的空框也**必须画出来**：只画选中态的话，用户仍然看不出「这里能
            勾」，就又回到「行里什么都没有、以为要点别处」的老困惑。
          */}
          <span
            aria-hidden="true"
            data-slot="usage-row-checkbox"
            data-checked={selected ? "true" : "false"}
            className={cn(
              "flex size-3.5 shrink-0 items-center justify-center rounded-[4px] border text-[9px] leading-none",
              selected
                ? "border-primary bg-primary text-primary-foreground"
                : "border-border bg-card text-transparent",
            )}
          >
            <Check className="size-2.5" strokeWidth={3} />
          </span>
          <span className="min-w-0 truncate">{label}</span>
        </span>
        {/*
          数值与 meta 的前景色从 `text-muted-foreground`（oklch 0.52）换成
          `USAGE_STRONG_TEXT`（oklch 0.42）：诊断实测「共 N 个」只有 2.97:1，
          远低于 WCAG AA 的 4.5:1，而它恰恰是**唯一携带口径信息的元素**。
          数值与 meta 同理 —— 换口径后它们就是「这个账号用了多少」的唯一载体，
          最该被看清的字不该是最淡的。
        */}
        <span data-slot={valueSlot} className={cn("shrink-0 tabular-nums", USAGE_STRONG_TEXT)}>
          {formatUsageCompact(value)}
        </span>
      </div>
      <div className="h-1.5 overflow-hidden rounded-full bg-muted">
        <div className="h-full rounded-full bg-primary/70" style={{ width: `${percent}%` }} />
      </div>
      {meta ? (
        <div data-slot="usage-row-meta" className={cn("truncate text-[11px]", USAGE_STRONG_TEXT)}>
          {meta}
        </div>
      ) : null}
    </Root>
  );
}

/**
 * 模型筛选控件：**多选**，默认「全部」。
 *
 * 与「放行模型」（`allowed_model`，`AllowedModelPicker`，三个模式都有）的分工 ——
 * 这是两个完全不同层面的东西，刻意做成两个互不影响的控件：
 *
 * | | 放行模型（allowed_model） | 模型筛选（本控件） |
 * |---|---|---|
 * | 作用面 | **服务端**：网关只放行勾选的那几个模型，其他被 400 拒绝 | **前端**：只改变这里展示哪些模型的用量 |
 * | 影响范围 | 真实影响客户端能否调用 | 只影响本页的**阅读**，不改任何配置、不写盘 |
 * | 生效方式 | 保存进 config，需重启网关 | 立即生效，仅本页 |
 * | 模式 | **三个模式都有**（限制模型与用哪些账号正交） | **三个模式都有**（看用量与工作模式无关） |
 *
 * 因此这里的勾选**不会**、也不该被写进 `allowed_model`：把「我只想看 glm 的用量」
 * 变成「网关拒绝其它模型」会静默掐断客户端请求，是最危险的一类耦合。
 * `verify-pool-progress-usage.cjs` 用回读 `/api/gateway/config` 锁死这一点。
 *
 * 为什么用下拉 + 勾选列表而不是一排 toggle 按钮：所有者本机的 `/v1/models`
 * 实测返回 **30 个模型**，全部铺开会把区块顶部挤成一面墙；而多选又必须能
 * 一眼看出「当前选了哪些」，纯下拉单选做不到。
 */
function ModelFilter({
  options,
  selected,
  onChange,
  totalModels,
}: {
  /** 可选模型（取自当前范围内的用量数据，按用量降序）。 */
  options: string[];
  /** 已选模型；空数组 = 全部（默认）。 */
  selected: string[];
  onChange: (next: string[]) => void;
  /** 全部模型数（未筛选时），用于「全部」行上的计数。 */
  totalModels: number;
}) {
  const all = selected.length === 0;
  const label = all
    ? "全部模型"
    : selected.length === 1
      ? selected[0]
      : `已选 ${selected.length} 个模型`;

  function toggle(model: string) {
    // 从「全部」开始勾一个 = **只看这一个**（把「全部」隐含的集合收窄成一项）。
    //
    // 为什么不做成「勾掉这一个、留下其余」：所有者本机 `/v1/models` 实测返回
    // **30 个模型**，而「想看某一个模型的用量」是最常见的意图。若从「全部」点
    // 一项等于把它排除，用户就得连点 29 次才能看到单个模型 —— 相反方向的
    // 需求（「全部但除了某一个」）远没那么常见，且可以用一次「全部模型」+
    // 逐个取消来达成。
    //
    // 视觉上仍然是勾选框（勾选态由 selected 决定），只是从「全部」这一步
    // 走的是「收窄到此项」而不是「排除此项」。
    if (all) {
      onChange([model]);
      return;
    }
    const next = selected.includes(model)
      ? selected.filter((m) => m !== model)
      : [...selected, model];
    // 取消到空 = 回到「全部」，而不是「一个都不显示」——
    // 后者在语义上等于把整页清零，几乎不会是用户想要的。
    onChange(next.length === 0 ? [] : next);
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          variant="outline"
          size="sm"
          className="h-7 shrink-0 gap-1.5 px-2 text-xs"
          aria-label={`模型筛选：${label}`}
          data-slot="usage-model-filter"
        >
          <ListFilter className="size-3.5" />
          {label}
          {!all ? <span className="text-muted-foreground">/ {totalModels}</span> : null}
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="max-h-80 w-60 overflow-y-auto">
        <DropdownMenuItem
          onSelect={(e) => {
            // 勾选列表要连着点多项，选中后不关闭菜单。
            e.preventDefault();
            onChange([]);
          }}
          aria-label="模型筛选：全部模型"
        >
          <span className="flex size-4 shrink-0 items-center justify-center">
            {all ? <Check className="size-3.5" /> : null}
          </span>
          <span className="min-w-0 flex-1 truncate">全部模型</span>
          <span className="shrink-0 text-[11px] tabular-nums text-muted-foreground">
            {totalModels} 个
          </span>
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        {options.length === 0 ? (
          <div className="px-2.5 py-2 text-xs text-muted-foreground">该范围内暂无模型数据</div>
        ) : (
          options.map((model) => {
            const checked = !all && selected.includes(model);
            return (
              <DropdownMenuItem
                key={model}
                onSelect={(e) => {
                  e.preventDefault();
                  toggle(model);
                }}
                aria-label={`模型筛选：${model}`}
                aria-checked={checked}
                // role=menuitemcheckbox 让读屏软件播报勾选态；Radix 的
                // DropdownMenuCheckboxItem 语义相同，但本项目未引入该导出
                //（ui/dropdown-menu.tsx 只导出了 6 个部件），故用既有部件 + 属性表达。
                role="menuitemcheckbox"
              >
                <span className="flex size-4 shrink-0 items-center justify-center">
                  {checked ? <Check className="size-3.5" /> : null}
                </span>
                <span className="min-w-0 flex-1 truncate font-mono text-xs">{model}</span>
              </DropdownMenuItem>
            );
          })
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/**
 * 「取不到用量」时该显示哪一句。
 *
 * 把这几档判定（读取中 / 未运行 / 不可达 / 网关不支持统计 / 真错误）抽出来，
 * 是为了让判定顺序**有唯一出处**：新版用量区块与它内部的重试分支都走这里。
 * 顺序本身是有讲究的 —— 「网关未运行」要排在「不可达」之前，否则未启动时
 * 会显示成「已启动但无法读取」，把「没开」说成「开了但坏了」。
 *
 * 返回 null 表示「有数据可展示」，调用方继续渲染正文。
 */
function usageUnavailableText({
  usageLoading,
  snapshot,
  running,
  usage,
}: {
  usageLoading: boolean;
  snapshot: GatewayUsageSnapshot | null;
  running: boolean;
  usage: GatewayUsageResult | null;
}): string | null {
  if (usageLoading && !snapshot) return "正在读取网关用量…";
  if ((usage && !usage.running) || !running) {
    return "网关未运行，启动后这里会展示经网关请求的 Token 用量";
  }
  if (!usage?.reachable) {
    return `网关已启动但暂时无法读取用量${usage?.error ? `：${usage.error}` : ""}`;
  }
  if (snapshot?.enabled === false) {
    return "当前网关可执行文件不支持用量统计，请更新网关后重试";
  }
  if (snapshot) return null;
  return `无法读取网关用量${usage?.error ? `：${usage.error}` : ""}`;
}

/**
 * 用量区块的「无数据」占位行。
 *
 * 图标按档位区分：读取中是转圈、未运行是勾（不是故障）、其余是三角警示。
 * 为什么不让调用方各自传图标：同一句话配不同图标会让两处看起来像两种状态。
 */
function UsagePlaceholder({ text }: { text: string }) {
  const Icon = text.startsWith("正在读取") ? Loader2 : text.startsWith("网关未运行") ? CheckCircle2 : Activity;
  return (
    <Row className="justify-center">
      <div className="flex items-center gap-2 py-4 text-xs text-muted-foreground">
        <Icon className={cn("size-3.5", Icon === Loader2 && "animate-spin")} />
        {text}
      </div>
    </Row>
  );
}

/** 从监听地址（":7863" / "0.0.0.0:7863"）解析端口。 */
function portOf(listen: string | undefined): number {
  if (!listen) return 0;
  const m = listen.match(/(\d{1,5})\s*$/);
  return m ? Number(m[1]) : 0;
}

/** 端口合法性：1-65535，且不是 1024 以下的特权端口（可用但有提示）。 */
function validatePort(value: number): string | null {
  if (!Number.isInteger(value) || value < 1 || value > 65535) return "端口需在 1-65535 之间";
  return null;
}

/** 把「距今毫秒数」格式化成「刚刚 / 12 秒前 / 3 分钟前」。 */
function formatRelativeTime(deltaMs: number): string {
  const sec = Math.max(0, Math.floor(deltaMs / 1000));
  if (sec < 5) return "刚刚";
  if (sec < 60) return `${sec} 秒前`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min} 分钟前`;
  const hour = Math.floor(min / 60);
  if (hour < 24) return `${hour} 小时前`;
  return `${Math.floor(hour / 24)} 天前`;
}

/**
 * 把剩余秒数格式化成中文时长；**一分钟以内给到秒，一小时以内给到分+秒**。
 *
 * 为什么必须补这两档（这是本轮实测暴露的真实缺陷）：
 * 模型冷却是 429 限流的**短冷却**，真实值就是 42 秒 / 65 秒 / 192 秒这个量级。
 * 旧实现只有「分钟」一档且向上取整，于是 42 秒显示成「1 分钟」、192 秒显示成
 * 「4 分钟」—— 那既不是剩余时间也不是它的上界，用户据此判断「还要等多久」
 * 会系统性偏大（192 秒被读成 4 分钟）。草稿 `账号池表格-终稿v4.html` 给的
 * 也正是「剩 42 秒」「剩 3 分 12 秒」这种精度。
 *
 * 账号级冷却（余额欠费 / 熔断）是分钟到小时级，仍走小时/分钟档，
 * 既有界面的显示口径不受影响。
 */
function formatRemaining(sec: number): string {
  if (sec <= 0) return "即将恢复";
  // 向上取整到秒：42.3 秒显示「43 秒」而不是「42 秒」—— 少报会让用户早一瞬重试，
  // 正好撞上还没到期的冷却。
  const total = Math.ceil(sec);
  if (total < 60) return `${total} 秒`;
  if (total < 3600) {
    const m = Math.floor(total / 60);
    const s = total % 60;
    return s === 0 ? `${m} 分钟` : `${m} 分 ${s} 秒`;
  }
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  if (minutes > 0) return `${hours} 小时 ${minutes} 分钟`;
  return `${hours} 小时`;
}

/** 把冷却截止时刻格式化成「09-15 13:25」（本地时区）。 */
function formatUntil(iso?: string): string | null {
  if (!iso) return null;
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return null;
  const p = (n: number) => String(n).padStart(2, "0");
  return `${p(t.getMonth() + 1)}-${p(t.getDate())} ${p(t.getHours())}:${p(t.getMinutes())}`;
}

/**
 * 积分格式化：与账号管理页卡片逐字一致的输出（最多两位小数 + 千分位）。
 *
 * 刻意不 import 账号卡片的同名函数：那个文件正在被并行修改，
 * 跨文件引用会把两处改动耦合成一个编译单元；而「显示成什么样」必须一致，
 * 因此这里保持同一实现而不是各写各的。
 */
const creditFormatter = new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 2 });

/** 积分的到期时间：`09/15 到期`；无到期时间 = 长期有效（与账号卡片同口径）。 */
function formatCreditExpiry(ts: number | null | undefined): string {
  if (!ts) return "长期有效";
  const date = new Date(ts);
  if (Number.isNaN(date.getTime())) return "长期有效";
  return `${String(date.getMonth() + 1).padStart(2, "0")}/${String(date.getDate()).padStart(2, "0")} 到期`;
}

/**
 * 只给日期的短格式（`09/15`）。
 *
 * 为什么与上面那个并存：勾选列表的指标行已经有「到期」这个列标签，
 * 再用带后缀的版本会渲染成「到期 10/16 到期」—— 一句话里两个「到期」，
 * 既啰嗦又容易读成两件事。
 */
function formatShortDate(ts: number | null | undefined): string {
  if (!ts) return "长期有效";
  const date = new Date(ts);
  if (Number.isNaN(date.getTime())) return "长期有效";
  return `${String(date.getMonth() + 1).padStart(2, "0")}/${String(date.getDate()).padStart(2, "0")}`;
}

/** 还有剩余的资源包，按到期时间升序（已用完的隐藏、同到期日按原序）—— 与账号卡片同一排序口径。 */
function usableResources(credit?: CreditExpiry): CreditResource[] {
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

/**
 * Token 到期时间：`09/15` 短格式。
 *
 * 用短格式是因为它要与「积分 / 到期 / 资源包」挤在同一行指标里；
 * 精确时刻放 tooltip。这里查的是**登录 Token** 的到期（`AccountMeta.expiresAt`），
 * 与上面那个「积分到期」不是一回事 —— 前者决定这个号还能不能被网关调用，
 * 后者只决定额度什么时候作废，混在一列里会让人误判。
 *
 * **过期不加「已过期」**：access token 是短期凭证，过期后宿主与网关会用
 * refresh token 自动换新（`refresh::ensure_fresh_token` / `auth.Auth.NeedsRefresh`），
 * 是正常自愈状态而非故障。这里曾经把过去的日期标成「已过期」，让「这次刷新
 * 还没跑到」看起来像账号坏了；真正不可自愈的情形（上游拒绝）由本行上方的
 * 「需重新登录」徽标负责报警，不需要在这里重复喊一次。
 */
function formatTokenExpiry(expiresAt: number | null | undefined): string {
  if (typeof expiresAt !== "number" || expiresAt <= 0) return "未知";
  const date = new Date(expiresAt);
  if (Number.isNaN(date.getTime())) return "未知";
  return `${String(date.getMonth() + 1).padStart(2, "0")}/${String(date.getDate()).padStart(2, "0")}`;
}

/**
 * 冷却原因说明：区分「账号级（余额欠费）」与「模型级（单一模型限流）」。
 *
 * 这两种状态此前在界面上都只显示"冷却中"，用户无法判断是该充值还是换个模型就好。
 */
function coolReasonText(acc: GatewayPoolAccount): string {
  if (acc.disabled) return acc.reason || "已禁用";
  if (acc.cool_kind === "hard_credit") return "余额不足（积分欠费），等签到或充值后恢复";
  if (acc.cool_kind === "soft_rate") return "账号被限速，短暂冷却后自动恢复";
  if (acc.cool_kind === "breaker") return "连续失败触发熔断，按退避时间恢复";
  return acc.reason || "冷却中";
}

/**
 * 「排队中」的说明文案。
 *
 * 这一档最容易被误读成故障：账号健康、积分充足，只是到期档位比当前生效档位晚，
 * 因此暂时轮不到。必须把机制说清楚，否则用户会以为账号丢了或没生效。
 */
function queuedReasonText(acc: GatewayPoolAccount): string {
  const day = acc.expire_day ? `（本账号到期 ${acc.expire_day}）` : "";
  return (
    `账号本身健康、积分充足，但到期档位${day}晚于当前正在使用的那一档。` +
    `网关按「先烧快过期额度」分层选号，只把流量给最早到期的那一组；` +
    `前面档位被用尽或冷却后，本账号会自动开始承接流量。`
  );
}

/**
 * 账号级冷却的短标签。
 *
 * 用短标签而不是把 `coolReasonText()` 整句塞进「状态」列：状态列要能一眼扫完，
 * 长句会把列宽撑到挤掉「消耗」列。整句作为悬浮说明保留（信息一条不少）。
 *
 * **未知 kind 返回空串**（而不是「冷却中」）：本函数有三处调用，
 * 其中两处是**拼接**——`状态标签 · <label>` 与 `账号级冷却（<label>）`。
 * 状态标签在冷却时本身就是「冷却中」，兜底再返回「冷却中」会拼出
 * 「冷却中 · 冷却中 42 秒」（实测造数据时确实这么显示过）；
 * 括号那处则会变成「账号级冷却（）」。
 * 空串 + 调用处判空，两处都能退化成正常文案。
 *
 * 真实取值只有两个（Go 侧 `CoolKind.String()`）：`hard_credit`（余额不足，
 * 冷却到次日 04:00 等签到）与 `soft_rate`（429 短冷却）。
 * `breaker` 是宿主侧熔断的表述，一并保留以免回归。
 */
function poolCoolKindLabel(kind: string | undefined): string {
  if (kind === "hard_credit") return "余额欠费";
  if (kind === "breaker") return "熔断";
  if (kind === "soft_rate") return "账号限速";
  return "";
}

/**
 * 账号池可排序的列。`#`（消耗排名）**不在**其中 —— 它跟着消耗列走，不是独立排序。
 */
type PoolSortKey = "account" | "state" | "tokens" | "records" | "credits" | "balance" | "expiry";

/**
 * 这一行在该列上是不是「拿不到值」（界面上显示为「—」/「未知」）。真值 → 恒排最后。
 *
 * 判定刻意与**显示口径逐条对齐**（`total > 0 ? … : "—"` 之类）：排序与显示若
 * 用两套「有没有值」的判据，就会出现「显示 — 的行却排在 0 的前面」这种
 * 无法解释的顺序。
 *
 * 为什么必须在**方向翻转之外**单独判定（本实现第一版就在这里错了）：
 * 「消耗积分」在「全部」范围里就是 null（积分快照只留 30 天，给不出可信值），
 * 若让 null 参与数值比较（当成 0），降序时它们沉底看起来正常，**升序时却会全部
 * 冒到最前面** —— 用户点一下升序，看到一屏「—」，会以为整张表坏了。
 * 实测现象：点「消耗 Token」升序后，完全没有用量的 uid-4 冲到了第一位。
 * 「未知」既不是最大也不是最小，它就不该参与大小比较。
 */
function poolCellBlank(key: PoolSortKey, row: PoolRowMetrics): boolean {
  if (key === "tokens") return row.total <= 0;
  if (key === "records") return row.records <= 0;
  if (key === "credits") return row.creditUsed === null || row.creditUsed <= 0;
  if (key === "balance") return row.balance === null;
  // 「到期档位」无档位 = 永远轮不到它，与「未知」同性质：不参与大小比较。
  if (key === "expiry") return !Number.isFinite(row.expiryKey);
  return false;
}

const POOL_SORT_OPTIONS = {
  /** 比较函数；`< 0` = left 排在前面。拿不到值的情形已由 poolCellBlank 提前排除。 */
  account: (l: PoolRowMetrics, r: PoolRowMetrics) => poolAccountName(l.acc).localeCompare(poolAccountName(r.acc), "zh-CN"),
  /**
   * 「状态」排序按**可用性分层**而不是按标签字面：健康 → 排队 → 账号冷却 → 已禁用。
   *
   * 为什么不用标签 `localeCompare`：那样「已禁用 / 排队中 / 健康 / 冷却中」会按拼音排，
   * 得到「健康、冷却中、排队中、已禁用」这种与运维直觉无关的顺序 ——
   * 而点这一列的人想问的是「哪些号还能接流量」。
   *
   * 同一档内再按「有没有模型冷却」排（无冷却的在前）：同显示「健康」的两个号里，
   * 一个模型全通、另一个有两个模型被限流，后者显然更该排在后面。
   */
  state: (l: PoolRowMetrics, r: PoolRowMetrics) =>
    l.stateRank - r.stateRank || l.modelCoolCount - r.modelCoolCount,
  tokens: (l: PoolRowMetrics, r: PoolRowMetrics) => l.total - r.total,
  records: (l: PoolRowMetrics, r: PoolRowMetrics) => l.records - r.records,
  credits: (l: PoolRowMetrics, r: PoolRowMetrics) => (l.creditUsed ?? 0) - (r.creditUsed ?? 0),
  balance: (l: PoolRowMetrics, r: PoolRowMetrics) => (l.balance ?? 0) - (r.balance ?? 0),
  expiry: (l: PoolRowMetrics, r: PoolRowMetrics) => l.expiryKey - r.expiryKey,
} satisfies Record<PoolSortKey, (l: PoolRowMetrics, r: PoolRowMetrics) => number>;

const POOL_SORT_DEFAULT_DIR: Record<PoolSortKey, "asc" | "desc"> = {
  account: "asc",
  state: "asc",
  tokens: "desc",
  records: "desc",
  credits: "desc",
  balance: "desc",
  // 升序 = 最快到期的排前面（选号就是按这个分层）
  expiry: "asc",
};

/** 可点排序的表头单元格（自带 `aria-sort`，读屏能播报当前排序方向）。 */
function PoolSortHeader({
  label,
  sortKey,
  align = "left",
  className,
  title,
  activeKey,
  dir,
  onToggle,
}: {
  label: React.ReactNode;
  sortKey: PoolSortKey;
  align?: "left" | "right";
  className?: string;
  title?: string;
  activeKey: PoolSortKey;
  dir: "asc" | "desc";
  onToggle: (key: PoolSortKey) => void;
}) {
  const active = activeKey === sortKey;
  const Icon = !active ? ArrowUpDown : dir === "asc" ? ArrowUp : ArrowDown;
  return (
    <th
      scope="col"
      // aria-sort 只在**当前排序列**上出现：给每一列都写 "none" 是合法但啰嗦的，
      // 读屏会逐列播报「未排序」，反而盖住了真正生效的那一列。
      aria-sort={active ? (dir === "asc" ? "ascending" : "descending") : undefined}
      className={cn(
        "whitespace-nowrap px-2.5 py-1.5 text-[11px] font-medium text-muted-foreground",
        align === "right" ? "text-right" : "text-left",
        className,
      )}
    >
      {/* 排序必须用真实的 <button>（不是给 th 挂 onClick）：键盘 Tab 与读屏
          都依赖原生语义，且焦点框能让人看出「这一列可以点」。 */}
      <button
        type="button"
        onClick={() => onToggle(sortKey)}
        title={title}
        className={cn(
          "inline-flex items-center gap-1 rounded transition-colors hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/40",
          active ? "text-primary" : "text-muted-foreground",
        )}
      >
        {label}
        <Icon className={cn("size-3 shrink-0", active ? "opacity-100" : "opacity-45")} aria-hidden="true" />
      </button>
    </th>
  );
}

/** 概览行里点开二级详情的开关（真正的 `<button>`，带 `aria-expanded` + `aria-controls`）。 */
function PoolExpandToggle({
  open,
  controls,
  onClick,
}: {
  open: boolean;
  controls: string;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-expanded={open}
      aria-controls={controls}
      aria-label={open ? "收起该账号详情" : "展开该账号详情"}
      className={cn(
        // 展开态用 **--primary** 系。此前踩过「用 secondary 当选中/进行态，
        // 与 outline 只差 4% 亮度，看起来永远停在默认态」的坑，故不重复。
        "inline-flex size-4 items-center justify-center rounded transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/40",
        open ? "bg-primary/15 text-primary" : "text-muted-foreground hover:bg-muted hover:text-foreground",
      )}
    >
      {open ? (
        <ChevronDown className="size-3.5" aria-hidden="true" />
      ) : (
        <ChevronRight className="size-3.5" aria-hidden="true" />
      )}
    </button>
  );
}

/**
 * 「消耗 Token」列里的迷你占比条。
 *
 * 为什么这里可以有、而卡片不行：那是**每行一个进度条图**（所有者明确否掉的
 * 重元素）；这里是一个 3px 高、40px 宽的细条，只表达「这一行相对最高消耗占多少」，
 * 不抢行高也不抢注意力。
 *
 * 刻意**不设最小宽度**（旧版用量行有 3% 下限）：给 1 token 也画成 3% 会让
 * 「几乎没用」看起来像「用了不少」。真为 0 就是空条 —— 那正是要传达的信息。
 */
function PoolUsageBar({ percent }: { percent: number }) {
  return (
    // data-slot 是本项目标记「这个部件是什么」的既有约定，测试按它取元素。
    <span
      data-slot="pool-usage-bar"
      className="inline-block h-[3px] w-10 shrink-0 overflow-hidden rounded-full bg-muted align-middle"
      title={`占本池最高消耗的 ${percent}%`}
    >
      <i className="block h-full rounded-full bg-primary" style={{ width: `${percent}%` }} />
    </span>
  );
}

/** 每个池账号在当前日期范围内的派生指标（表格的列值都从这里取，保证排序与显示同源）。 */
interface PoolRowMetrics {
  acc: GatewayPoolAccount;
  /** 该账号在当前范围内的 Token 消耗（无数据 = 0）。 */
  total: number;
  /** 该账号在当前范围内的调用次数（无数据 = 0）。 */
  records: number;
  /** 消耗占比（0-100，相对本池最高消耗）。 */
  percent: number;
  /** 消耗排名（1 = 最高；跟着消耗走，与当前排序无关）。 */
  rank: number;
  /** 当前范围内的积分消耗；null = 该范围给不出可信值（「全部」）。 */
  creditUsed: number | null;
  /** 今日消耗积分；null = 宿主没给积分统计（旧版后端，或还没拉到）。 */
  creditToday: number | null;
  /** 积分余额（网关侧快照）；null = 未取到。 */
  balance: number | null;
  /** 到期档位的可比较键（毫秒；无档位 = +∞ ⇒ 恒排最后）。 */
  expiryKey: number;
  /**
   * 状态分层（越小越能用）：健康 1 → 排队 2 → 账号冷却 3 → 已禁用 4。
   *
   * 分层口径**与「状态」列的标签逐条对齐**；模型冷却不进这一层，它另有一列
   * `modelCoolCount`。理由：点「状态」列排序的人想问的是「哪些号还能接流量」，
   * 而那个答案就写在状态列里。若把「只有模型冷却」的号单独划一档，界面上会出现
   * 「显示健康的行排在排队中之后」这种按列头解释不通的顺序。
   */
  stateRank: number;
  /** 模型级冷却的模型数（0 = 无）。同一状态档内的次序键：无冷却的排前面。 */
  modelCoolCount: number;
}

/**
 * 状态分层与「状态」列的标签**同源**，避免排序与显示各算一遍。
 *
 * 优先级：禁用 > 账号级冷却 > 排队 > 健康 —— 与状态标签的判定顺序逐字相同。
 */
function poolStateRank(acc: GatewayPoolAccount): number {
  if (acc.disabled) return 4;
  if (acc.cooling) return 3;
  if (acc.queued) return 2;
  return 1;
}

/** 到期档位的可比较键：`YYYY-MM-DD` → 本地零点毫秒；无档位 = +∞（恒排最后）。 */
function poolExpiryKey(expireDay: string | undefined): number {
  if (!expireDay) return Number.POSITIVE_INFINITY;
  const t = new Date(`${expireDay}T00:00:00`).getTime();
  return Number.isNaN(t) ? Number.POSITIVE_INFINITY : t;
}

/**
 * 「最近成功」的显示文案。
 *
 * 用**绝对时刻**（`09-15 13:25`）而不是「12 秒前」：本页每秒都有一次 tick 驱动相对
 * 时间，但那是页面级状态；把 tick 传进每一行会让整张表每秒重渲染一遍 ——
 * 十几行 × 每秒一次是没必要的开销，而「最近一次成功是什么时候」看绝对时刻更准。
 *
 * **必须显式挡掉 Go 的零值时间**：`pool.Status.LastSuccessTime` 带
 * `json:",omitempty"`，但 `time.Time` 是结构体、**永远不为空**，因此从未成功过的
 * 账号会把它序列化成 `"0001-01-01T00:00:00Z"` 发过来。旧实现直接 `new Date()` 格式化，
 * 于是界面上出现「最近成功 01-01 08:05」这种**编造的日期** —— 真实数据实测截图里
 * 就有一行是这样（账号从未成功过，却看起来像在元旦早上成功过）。
 * 判据取「年份 ≤ 1」而不是「等于 1」：Go 零值就是 0001 年，任何真实时间都远大于它。
 */
function poolLastSuccessText(acc: GatewayPoolAccount): string {
  const raw = acc.last_success;
  if (!raw) return "—";
  const t = new Date(raw);
  if (Number.isNaN(t.getTime()) || t.getUTCFullYear() <= 1) return "—";
  return formatUntil(raw) ?? "—";
}

/**
 * 「多久以前用过」的**相对**文案，供状态列的「N 分钟前用过」使用。
 *
 * 与 `poolLastSuccessText` 的分工：那个给二级行的**绝对时刻**（`09-15 13:25`），
 * 用于精确核对；这个给概览行的**相对说法**，用于一眼判断「刚才是不是它」。
 * 两者都要：绝对时刻回答「具体几点」，相对说法回答「离现在近不近」。
 *
 * 为什么不用页面级 tick 驱动它：本页每秒都有一次 tick（驱动冷却倒计时），
 * 但把 tick 传进每一行会让整张表每秒重渲染一遍。这里刻意**只按已有渲染机会更新**
 * —— 「几分钟前」这种粒度不需要秒级刷新，网关状态本身就是 5 秒轮询来的。
 *
 * 档位取「秒 / 分钟 / 小时 / 天」，与冷却倒计时的 `formatRemaining` 口径一致，
 * 避免同一页出现两套时间说法。
 */
function poolLastUsedAgo(acc: GatewayPoolAccount): string | null {
  const raw = acc.last_success;
  if (!raw) return null;
  const t = new Date(raw);
  if (Number.isNaN(t.getTime()) || t.getUTCFullYear() <= 1) return null;
  const sec = Math.floor((Date.now() - t.getTime()) / 1000);
  // 时钟偏差可能让「刚成功」算出负数，按 0 处理（显示「刚刚」而不是「-3 秒前」）
  if (sec < 0) return "刚刚";
  if (sec < 60) return "刚刚";
  if (sec < 3600) return `${Math.floor(sec / 60)} 分钟前`;
  if (sec < 86400) return `${Math.floor(sec / 3600)} 小时前`;
  return `${Math.floor(sec / 86400)} 天前`;
}

/**
 * 模型冷却角标的**悬浮提示**正文（也与角标的原生 `title` 共用，保证两处文案一致）。
 *
 * 文案口径是本轮的硬要求，逐句都有原因：
 *   1. 先列出**是哪几个模型**、各自还剩多久 —— 不展开就能看到，这是角标存在的理由；
 *   2. 必须点明「**仅这些模型不可用，其它模型照常可用**」—— 否则用户会误以为这号废了；
 *   3. 若该账号**同时**有账号级冷却，补一句「账号冷却是整号不可用（换模型也不行）」——
 *      这两种冷却长得像但后果完全相反，是整块界面最容易误读的地方。
 */
function PoolModelCoolingTip({ acc }: { acc: GatewayPoolAccount }) {
  const modelCools = acc.model_cooling ?? [];
  return (
    <div className="flex flex-col gap-1">
      <div className="font-medium">
        <AlertTriangle className="mr-1 inline size-3 align-[-2px]" aria-hidden="true" />
        {modelCools.length} 个模型正在限流
      </div>
      {modelCools.map((mc) => (
        <div key={mc.model} className="flex items-center justify-between gap-3">
          <span className="font-mono">{mc.model}</span>
          <span className="shrink-0 tabular-nums opacity-85">
            {typeof mc.remaining_sec === "number" && mc.remaining_sec > 0
              ? `剩 ${formatRemaining(mc.remaining_sec)}`
              : "即将恢复"}
          </span>
        </div>
      ))}
      {/* 分隔线用 bg-current/15 而不是 border-current：Tailwind v4 里 `border-*` 默认
          只设宽度，颜色要靠 border-color 工具类；而 border-current 是否被识别成颜色
          取决于版本，写错就退化成「看不见的线」。一条 1px 的 div 没有这个歧义。 */}
      <div className="my-0.5 h-px bg-current opacity-15" aria-hidden="true" />
      <div className="opacity-75">
        仅这些模型不可用，其它模型照常可用；到期自动恢复。点本行可看完整详情。
      </div>
      {acc.cooling ? (
        <>
          <div className="my-0.5 h-px bg-current opacity-15" aria-hidden="true" />
          <div className="opacity-75">
            注意：本账号还有<strong className="font-medium">账号级冷却</strong>
            （{poolCoolKindLabel(acc.cool_kind)}），那是<strong className="font-medium">整号不可用</strong>
            （换模型也不行），与模型限流是两回事。
          </div>
        </>
      ) : null}
    </div>
  );
}

/**
 * 角标的原生 `title` 兜底文案（纯文本版，供 `title` 与读屏使用）。
 *
 * 为什么与上面的 JSX 提示并存：Radix 的 Tooltip 需要一个已挂载的 Provider 与
 * 悬浮/聚焦事件才会出现，而 `title` 在任何情况下都在（键盘 focus、鼠标悬停、
 * 无 JS 环境）。两处文案口径一致，不构成「信息两套」。
 */
function modelCoolingTitle(acc: GatewayPoolAccount): string {
  const modelCools = acc.model_cooling ?? [];
  const lines = modelCools.map(
    (mc) =>
      `${mc.model}（${
        typeof mc.remaining_sec === "number" && mc.remaining_sec > 0
          ? `剩 ${formatRemaining(mc.remaining_sec)}`
          : "即将恢复"
      }）`,
  );
  const head = `${modelCools.length} 个模型正在限流：${lines.join("、")}`;
  const tail = "仅这些模型不可用，其它模型照常可用；到期自动恢复。";
  // 括号里只有在**拿得到具体原因**时才写 —— 否则会出现「账号级冷却（）」这种空括号。
  const coolKind = poolCoolKindLabel(acc.cool_kind);
  const accountLevel = acc.cooling
    ? `注意：本账号还有账号级冷却${coolKind ? `（${coolKind}）` : ""}，那是整号不可用（换模型也不行）。`
    : "";
  return [head, tail, accountLevel].filter(Boolean).join(" ");
}

function poolAccountName(acc: GatewayPoolAccount): string {
  return acc.nickname || acc.uid;
}

/**
 * 账号池「概览行 + 二级展开行」。
 *
 * 形态在本轮由所有者定为**表格**（草案 `dist/账号池表格-终稿v4.html` 已逐条确认）：
 * 一账号一行的概览行（箭头 / # / 账号 / 状态 / 消耗 Token / 调用 / 消耗积分 /
 * 积分余额 / 到期档位），点箭头展开二级行（模型冷却完整列表 + 运行态 + 积分 + UID）。
 *
 * **两类冷却必须分开显示**（这是本轮最容易做错的地方）：
 *   - 账号级（`cooling` / `cool_kind`）→ 「状态」列直接显示（它决定整号能不能接流量，
 *     最该一眼看到），文案是「余额欠费 / 熔断 / 账号限速 / 冷却中 + 剩余秒」；
 *   - 模型级（`model_cooling[]`）→ 「状态」列只给**角标计数**（`⚠ N 模型`），
 *     悬浮提示列出是哪几个、还剩多久，二级行里有完整列表。
 *   两者可同时存在，且后果完全不同：账号级是「换模型也不行」，模型级是
 *   「换模型照常可用」。角标的悬浮提示会按这个口径点明（见下）。
 */
function PoolAccountRow({
  metrics,
  expanded,
  onToggle,
  detailId,
  usageRangeLabel,
  creditStatsLoaded,
  stickySessions,
}: {
  metrics: PoolRowMetrics;
  expanded: boolean;
  onToggle: () => void;
  /** 二级行的 DOM id —— 供 `aria-controls` 指向，读屏才知道展开的是哪一块。 */
  detailId: string;
  /** 当前日期筛选的中文名（列头 tooltip 与「今日消耗积分」的口径说明都要用）。 */
  usageRangeLabel: string;
  /** 宿主是否提供了积分统计；false 时积分消耗列显示「—」而不是假装 0。 */
  creditStatsLoaded: boolean;
  /** 网关侧的**池级**粘性会话绑定数（后端没有逐账号口径）。 */
  stickySessions: number;
}) {
  const { acc, total, records, percent, rank, creditUsed, creditToday, balance, expiryKey } = metrics;
  const modelCools = acc.model_cooling ?? [];
  const coolingModelCount = modelCools.length;
  /**
   * 「正在使用」—— 该账号此刻有在途请求（`in_flight > 0`）。
   *
   * 为什么用这个而不是找「当前账号」字段：`/status` **没有**那样的字段。
   * 网关是**按请求选号**的，同一时刻可能多个账号都在服务，所以「当前账号」是单数
   * 这个前提本身不成立；`in_flight` 才是真实的「此刻在干活」信号
   *（在此之前它只出现在展开后的二级行里）。
   *
   * `> 0` 而不是 `!= null`：后端给的是计数（缺省 0），0 表示没有在途请求 —— 那才是
   * 「没在用」，不能因为字段存在就点亮。
   */
  const inFlight = acc.in_flight ?? 0;
  const inUse = inFlight > 0;
  /** 空闲时的「N 分钟前用过」；从未成功过则为 null（不编造日期）。 */
  const lastUsedAgo = inUse ? null : poolLastUsedAgo(acc);
  // 状态标签的优先级：禁用 > 账号级冷却 > 排队 > 健康。
  //
  // 「排队」单独作为一档，因为它最容易让人误判：账号本身完全健康、积分充足，
  // 只是到期档位比当前生效档位晚，所以暂时轮不到（用户看到「健康」却在用量里
  // 找不到它，就会以为账号丢了）。
  const state = acc.disabled
    ? { label: "已禁用", cls: "bg-destructive/10 text-destructive" }
    : acc.cooling
      ? { label: "冷却中", cls: "bg-amber-500/10 text-amber-600 dark:text-amber-400" }
      : acc.queued
        ? { label: "排队中", cls: "bg-muted text-muted-foreground" }
        : { label: "健康", cls: "bg-emerald-500/10 text-emerald-600 dark:text-emerald-400" };
  // 到期日就是选号分层档位：同一 expire_day 的账号在均衡时同级（平均分摊）。
  const expiry = acc.expire_day
    ? { label: acc.expire_day.slice(5), title: `最近到期积分：${acc.expire_day}（同一天的账号同级平均分摊）` }
    : { label: "未知", title: "尚未取到积分到期信息：会排在其他账号之后，仅在它们不可用时才使用" };
  // 「#」是**消耗排名**（第 1 名 = 消耗最高），不是行号 —— 这样即使按别的列排序，
  // 也能立刻看出「这个号在消耗上排第几」。无消耗数据时不给排名（否则会显示一批并列第 1）。
  const rankLabel = rank > 0 ? String(rank) : "—";

  return (
    <>
      <tr
        data-slot="pool-row"
        data-uid={acc.uid}
        // `data-in-use` 供 UI 测试直接锁定「哪几行正在被使用」，
        // 不必去解析徽标文案（文案会随「带数字」等需求变动）。
        data-in-use={inUse ? "true" : undefined}
        className={cn(
          "border-b border-border/40 transition-colors",
          // 展开态用 --primary 系（与展开箭头、排序激活态同一套，不用 secondary）。
          expanded ? "pool-row-expanded bg-primary/[0.04]" : "hover:bg-muted/40",
          /*
            正在使用的行：**整行左侧一条蓝竖线 + 极淡蓝底**（见 index.css 的
            `.pool-row-inuse` —— 为什么用 inset 阴影而不是 border-left、
            为什么色条不能只靠状态列的徽标，都写在那里）。

            注意顺序：本类放在展开态之后，两者同时成立时由
            `.pool-row-inuse.pool-row-expanded` 那条规则接管底色，
            不会出现「展开的号恰好在使用中就把展开色吃掉」。
          */
          inUse && "pool-row-inuse",
        )}
      >
        <td className="px-1.5 py-1">
          <PoolExpandToggle open={expanded} controls={detailId} onClick={onToggle} />
        </td>
        <td className="w-8 px-1.5 py-1">
          <span
            className={cn(
              "inline-flex size-[17px] items-center justify-center rounded-[5px] text-[10px] font-medium tabular-nums",
              rank > 0 && rank <= 3 ? "bg-primary/15 text-primary" : "bg-muted text-muted-foreground",
            )}
            title="消耗排名（第 1 名 = 本范围内消耗最高）"
          >
            {rankLabel}
          </span>
        </td>
        <td className="max-w-[168px] px-2.5 py-0.5">
          {/* 行高刻意钉在「一行名字 + 一行 uid 前缀」≈ 28px（leading-4 + leading-3）：
              草稿要求概览行约 30px/行、一屏十几个账号。不给显式 leading 的话
              Tailwind 的默认行高（1.5×）会把两行撑到 34px，行高直接多出 20%。 */}
          <div className="truncate text-[12.5px] font-medium leading-4" title={acc.nickname || acc.uid}>
            {acc.nickname || acc.uid}
          </div>
          {/* uid 在概览行给短前缀（完整值在二级行「积分」块里，信息不丢），
              因为 36 字符的 uuid 会把「账号」列撑到吃掉「消耗」列的宽度。 */}
          <div className="truncate font-mono text-[10px] leading-3 text-muted-foreground/80" title={acc.uid}>
            {acc.uid.slice(0, 8)}
          </div>
        </td>
        <td className="px-2.5 py-1">
          <span className="inline-flex flex-wrap items-center gap-1">
            {/*
              「使用中 N」放在**状态列最前**：它是「此刻在不在干活」，与后面的
              「健康 / 排队中 / 冷却中」是**两个不同维度**（可用性 vs 活跃度），
              后者回答「能不能用」，前者回答「正在不被在用」。放最前是因为它变化最快、
              最需要一眼看到（本行的左色条也是同一个信号，两者一起用）。
            */}
            {inUse ? (
              <span
                data-slot="pool-inuse-badge"
                className="pool-inuse shrink-0 gap-1 rounded-full border px-2 py-[1px] text-[11px] font-semibold"
                title={`正在使用：此刻有 ${inFlight} 个请求在途（说明流量正落在这个账号上）`}
              >
                <span className="pool-inuse-dot" aria-hidden="true" />
                使用中 {inFlight}
              </span>
            ) : null}
            {/* 账号级冷却 —— 决定整号能不能接流量，直接显示。
                带 `data-slot="pool-state"`：状态列里现在有**三块**（使用中徽标 /
                这一块 / 模型冷却角标），按 DOM 顺序取「第一个 span」已经不稳
                —— 本行加上使用中徽标后，既有用例的 `span > span` 取到的是徽标。
                给稳定的钩子，让断言认语义而不是认位置。 */}
            <span
              data-slot="pool-state"
              className={cn("shrink-0 whitespace-nowrap rounded-md px-1.5 py-0.5 text-[11px] font-medium", state.cls)}
              title={acc.queued && !acc.cooling && !acc.disabled ? queuedReasonText(acc) : coolReasonText(acc)}
            >
              {state.label}
              {/* 只有拿得到具体原因时才追加「 · 原因」——未知 kind 时
                  `poolCoolKindLabel` 返回空串，这里若不判空就会拼出
                  「冷却中 · 42 秒」这种多一个点的文案。 */}
              {acc.cooling && poolCoolKindLabel(acc.cool_kind)
                ? ` · ${poolCoolKindLabel(acc.cool_kind)}`
                : ""}
              {acc.cooling && typeof acc.cool_remaining_sec === "number" && acc.cool_remaining_sec > 0
                ? ` ${formatRemaining(acc.cool_remaining_sec)}`
                : ""}
            </span>
            {/* 模型级冷却 —— 只有角标计数 + 悬浮明细（避免在概览行铺开一长串模型名）。 */}
            {coolingModelCount > 0 ? (
              <Tooltip>
                <TooltipTrigger asChild>
                  {/*
                    角标用**真实的 `<button>`**（外层 Badge 只提供既有 shadcn 样式）：
                    Radix 的 Tooltip 在 focus 时也会打开，键盘用户因此同样能看到明细；
                    用 span 的话这一处提示对键盘与读屏完全不可达。

                    刻意**不用** `title`：Radix 的提示会先出现，原生 `title` 约一秒后再
                    叠一个几乎一样的黑框，看起来像两个提示打架。兜底文案改用 `aria-label`
                    （读屏必然读到，视觉上不出第二个框）。

                    用 `data-pool-badge` 而不是 `data-slot`：Badge 与 TooltipTrigger 这对
                    asChild 组合会合并 props，`data-slot` 会被 Radix 的值占掉，测试按它
                    取元素会取到别的部件（既有的区域标签就踩过这个坑）。
                  */}
                  <Badge
                    asChild
                    variant="outline"
                    className="pool-mbadge gap-1 py-0 text-[11px] font-medium"
                  >
                    <button
                      type="button"
                      data-pool-badge="model-cooling"
                      aria-label={modelCoolingTitle(acc)}
                    >
                      <AlertTriangle className="size-3 shrink-0" aria-hidden="true" />
                      {coolingModelCount} 模型
                    </button>
                  </Badge>
                </TooltipTrigger>
                <TooltipContent side="bottom" align="start" className="max-w-sm">
                  <PoolModelCoolingTip acc={acc} />
                </TooltipContent>
              </Tooltip>
            ) : null}
            {/*
              空闲账号的「N 分钟前用过」—— 回答「刚才是不是它」。
              这在**没有在途请求时**是唯一能给的线索：此刻确实没在用它，
              但用户往往想知道「上一个成功是不是这个号」，否则流量一停就完全看不出动向。
              有在途请求时不显示（「使用中 N」已经更强，两个一起出是冗余）。
              `last_success` 从未有过（Go 零值时间）时 `poolLastUsedAgo` 返回 null，
              这里就不渲染 —— **不给编造的日期**（该坑的说明见 poolLastSuccessText 注释）。
            */}
            {!inUse && lastUsedAgo ? (
              <span
                data-slot="pool-last-used"
                className="shrink-0 whitespace-nowrap text-[10.5px] text-muted-foreground"
                title={`最近一次成功：${poolLastSuccessText(acc)}`}
              >
                {lastUsedAgo}用过
              </span>
            ) : null}
          </span>
        </td>
        <td className="px-2.5 py-1 text-right">
          <div className="flex items-center justify-end gap-1.5">
            <span
              className={cn("tabular-nums", total > 0 ? "text-foreground/90" : "text-muted-foreground")}
              title={`${exactTokenFormatter.format(total)} tokens（${usageRangeLabel}）`}
            >
              {total > 0 ? formatUsageCompact(total) : "—"}
            </span>
            <PoolUsageBar percent={percent} />
          </div>
        </td>
        <td
          className={cn("px-2.5 py-1 text-right tabular-nums", records > 0 ? "text-foreground/90" : "text-muted-foreground")}
          title={records > 0 ? `${exactTokenFormatter.format(records)} 次成功请求（${usageRangeLabel}）` : undefined}
        >
          {records > 0 ? exactTokenFormatter.format(records) : "—"}
        </td>
        <td
          className={cn(
            "px-2.5 py-1 text-right tabular-nums",
            creditUsed && creditUsed > 0 ? "text-foreground/90" : "text-muted-foreground",
          )}
          title={
            creditUsed === null
              ? "「全部」范围给不出可信的积分消耗：积分快照只保留 30 天"
              : `账号库积分记录里累计的消耗（${usageRangeLabel}）`
          }
        >
          {creditUsed !== null && creditUsed > 0 ? exactTokenFormatter.format(Math.round(creditUsed)) : "—"}
        </td>
        <td className="px-2.5 py-1 text-right">
          <span
            className={cn("tabular-nums", balance !== null ? "text-foreground/90" : "text-muted-foreground")}
            title="网关侧记录的最新积分余额（与账号管理页的实时查询口径不同，可能差一个刷新周期）"
          >
            {balance !== null ? exactTokenFormatter.format(balance) : "—"}
          </span>
        </td>
        <td className="px-2.5 py-1">
          <span
            className={cn("tabular-nums", Number.isFinite(expiryKey) ? "text-foreground/90" : "text-muted-foreground")}
            title={expiry.title}
          >
            {expiry.label}
          </span>
        </td>
      </tr>

      {expanded ? (
        <tr data-slot="pool-detail-row" data-uid={acc.uid} className="border-b border-border/40 bg-muted/25">
          <td colSpan={9} className="px-0">
            <div id={detailId} className="flex flex-col gap-3 py-2.5 pl-12 pr-3">
              {/* ---------- 1. 模型冷却：完整列表 ---------- */}
              <div className="flex flex-col gap-1.5">
                <div className="flex flex-wrap items-center gap-1.5 text-[10.5px] font-medium uppercase tracking-wide text-muted-foreground">
                  模型冷却
                  {coolingModelCount > 0 ? (
                    <span className="font-normal normal-case tracking-normal text-muted-foreground">
                      {coolingModelCount} 个 · 仅这些模型不可用，换模型仍可用
                    </span>
                  ) : null}
                </div>
                {coolingModelCount > 0 ? (
                  <div className="flex flex-col gap-1">
                    {modelCools.map((mc) => (
                      <div
                        key={mc.model}
                        data-slot="pool-model-cooling-item"
                        className="grid grid-cols-[minmax(0,1fr)_auto_auto] items-center gap-2.5 rounded-lg border border-amber-500/25 bg-amber-500/[0.07] px-2.5 py-1 text-[11.5px]"
                      >
                        <span className="truncate font-mono font-medium" title={mc.model}>
                          {mc.model}
                        </span>
                        <span className="shrink-0 tabular-nums text-amber-700 dark:text-amber-400">
                          {typeof mc.remaining_sec === "number" && mc.remaining_sec > 0
                            ? `剩 ${formatRemaining(mc.remaining_sec)}`
                            : "即将恢复"}
                        </span>
                        <span className="max-w-[300px] truncate text-[10.5px] text-muted-foreground" title={mc.reason || mc.model}>
                          {mc.reason || `${formatUntil(mc.until) ?? "到期"}后自动恢复`}
                        </span>
                      </div>
                    ))}
                  </div>
                ) : (
                  <div className="text-[11px] text-muted-foreground">该账号当前没有模型级限流</div>
                )}
              </div>

              {/* ---------- 2. 运行态 ---------- */}
              <div className="flex flex-col gap-1.5">
                <div className="text-[10.5px] font-medium uppercase tracking-wide text-muted-foreground">运行态</div>
                <div className="grid grid-cols-[repeat(auto-fill,minmax(172px,1fr))] gap-x-4 gap-y-0.5 text-[11.5px]">
                  <PoolKV k="在途请求" v={String(acc.in_flight ?? 0)} />
                  <PoolKV
                    k="成功 / 失败"
                    v={`${typeof acc.success_count === "number" ? exactTokenFormatter.format(acc.success_count) : "—"} / ${
                      typeof acc.err_total === "number" ? exactTokenFormatter.format(acc.err_total) : "—"
                    }`}
                  />
                  <PoolKV k="最近成功" v={poolLastSuccessText(acc)} />
                  {/* 冷却原因在「状态」列已有短标签，这里给完整一句（同样是账号级冷却）。 */}
                  <PoolKV k="账号冷却" v={acc.cooling ? coolReasonText(acc) : "—"} />
                  {/* 粘性会话是**池级**计数（网关 /status 的 sticky_sessions），不是逐账号的。
                      标成「池级」而不是编一个逐账号数字：后端拿不到按账号的绑定数。 */}
                  <PoolKV k="粘性会话" v={`${stickySessions}（池级）`} />
                </div>
              </div>

              {/* ---------- 3. 积分 ---------- */}
              <div className="flex flex-col gap-1.5">
                <div className="text-[10.5px] font-medium uppercase tracking-wide text-muted-foreground">积分</div>
                <div className="grid grid-cols-[repeat(auto-fill,minmax(172px,1fr))] gap-x-4 gap-y-0.5 text-[11.5px]">
                  <PoolKV
                    k="今日消耗积分"
                    v={
                      // 刻意给**今日**这一档，而不是复述上面的筛选范围：`CreditStatisticsResource`
                      // 里只有今日 / 近 7 天 / 本月三个窗口，「近 30 天」与「全部」都落回本月，
                      // 复述筛选范围会把「本月」说成「近 30 天」。列名如实写「今日」。
                      creditStatsLoaded
                        ? exactTokenFormatter.format(Math.round(creditToday ?? 0))
                        : "—"
                    }
                  />
                  <PoolKV
                    k="剩余积分"
                    v={balance !== null ? exactTokenFormatter.format(balance) : "—"}
                  />
                  <PoolKV k="最近到期" v={acc.expire_day ?? "未知"} />
                  <PoolKV k="UID" v={acc.uid} mono />
                </div>
              </div>
            </div>
          </td>
        </tr>
      ) : null}
    </>
  );
}

/** 二级行里的一对「标签 / 值」。 */
function PoolKV({ k, v, mono }: { k: string; v: string; mono?: boolean }) {
  return (
    <span className="flex min-w-0 items-baseline gap-2">
      <span className="shrink-0 text-muted-foreground">{k}</span>
      <span className={cn("min-w-0 truncate tabular-nums", mono && "font-mono")} title={v}>
        {v}
      </span>
    </span>
  );
}

/**
 * 手动模式勾选列表里的一行账号。
 *
 * 两行式布局的取舍：所有者要求「积分 / 是否国际版 / 到期时间 / 资源包」都要
 * **直接看到**（明确否掉了藏在悬浮里），四项信息塞进原来的一行必然换行错乱。
 * 因此改成「第一行 = 勾选框 + 账号名 + 状态标记」「第二行 = 四个指标」，
 * 行高固定两行、指标用等宽数字右对齐，账号再多也是整齐的一列，不会参差。
 *
 * 指标一律「取不到就显示 —」而不是隐藏：同一列里有的行少一项时，
 * 剩下的项会错位到别的列上，看起来像数据串了。
 */
function ManualAccountOption({
  account,
  checked,
  onToggle,
  meta,
  credit,
  creditLoading,
}: {
  account: GatewayStatusAccount;
  checked: boolean;
  onToggle: () => void;
  /** 账号库元信息（区域 / Token 到期）—— 网关 /status 不提供这些。 */
  meta?: AccountMeta;
  /** 逐账号积分（与账号管理页卡片同源：`POST /api/credits`）。 */
  credit?: CreditExpiry;
  creditLoading?: boolean;
}) {
  const name = account.nickname || account.uid.slice(0, 8);
  const isIntl = meta?.regionKey === "intl";
  const resources = usableResources(credit);
  // 积分只认账号卡片那一份 `totalRemaining`（`POST /api/credits`）——
  // 刻意不拿网关 `/status` 的 `credits` 做兜底：那是网关启动/巡检时写入的
  // 快照值，与账号页的实时查询**不是同一个时刻**，两者混用会让同一个号
  // 在两页显示不同余额。宁可显示「—」也不显示一个口径不同的数。
  const remaining = credit?.ok ? credit.totalRemaining : undefined;
  const balanceText = typeof remaining === "number" ? creditFormatter.format(remaining) : "—";
  // 最近到期的资源包：与账号卡片一样只取最快过期的那个，其余进 tooltip。
  const soonest = resources[0];
  const expiringText = soonest ? formatShortDate(soonest.expireAt) : "—";
  const resourcesText = credit?.ok ? `${resources.length} 个积分包` : "—";
  const detailTitle = [
    `积分 ${balanceText}`,
    credit?.ok ? `资源包 ${resources.length} 个` : "资源包未知",
    ...resources.slice(0, 6).map((r) => {
      const pkg = r.packageName || r.packageCode || "积分包";
      return `${pkg}：${creditFormatter.format(r.remaining)}（${formatCreditExpiry(r.expireAt)}）`;
    }),
    meta?.expiresAt
      ? `Token 到期 ${new Date(meta.expiresAt).toLocaleString("zh-CN")}`
      : "Token 到期未知",
    meta?.region ? `区域 ${meta.region}` : "",
  ]
    .filter(Boolean)
    .join("\n");

  return (
    <label
      className={cn(
        "flex min-w-0 cursor-pointer flex-col gap-1 border-b border-border/50 px-3 py-2 last:border-b-0 hover:bg-accent/50",
        checked && "bg-accent/30",
      )}
    >
      <span className="flex min-w-0 items-center gap-2.5">
        <Checkbox
          checked={checked}
          onCheckedChange={onToggle}
          aria-label={`选择账号 ${name}`}
        />
        <span className="min-w-0 flex-1 truncate text-xs font-medium">{name}</span>
        {/* 国际版用文字标签而不是纯图标：所有者要的是「是否国际版」可读，
            图标需要先学会才能认，等于没说。
            元信息还没加载出来时显示「—」而**不是**默认成「国服」——
            区域是从登录域名推导的（`account_meta`），拿不到就无从判断，
            把它写成「国服」是在断言一件没验证过的事，国际版账号会被标错。 */}
        {!meta ? (
          <Badge
            variant="secondary"
            className="h-4 shrink-0 px-1 text-[10px] text-muted-foreground/60"
            title="账号元信息尚未加载，暂时无法判断区域"
          >
            —
          </Badge>
        ) : isIntl ? (
          <Badge
            variant="outline"
            className="h-4 shrink-0 gap-1 border-sky-500/30 bg-sky-500/10 px-1 text-[10px] text-sky-700"
            title={`国际版账号（${meta.region ?? "workbuddy.ai"}）`}
          >
            <Globe className="size-3" aria-hidden="true" />
            国际版
          </Badge>
        ) : (
          <Badge
            variant="secondary"
            className="h-4 shrink-0 px-1 text-[10px] text-muted-foreground"
            title={`国服账号（${meta.region ?? "copilot.tencent.com"}）`}
          >
            国服
          </Badge>
        )}
        {account.needsRelogin ? (
          <Badge variant="outline" className="h-4 shrink-0 px-1 text-[10px] text-destructive">
            需重新登录
          </Badge>
        ) : null}
      </span>
      {/* 指标行：4 项等宽网格。用 grid 而不是 flex-wrap —— 换行会让
          「到期」跑到「资源包」下面，同一屏里两行的列位就对不齐了。 */}
      <span
        className="grid min-w-0 grid-cols-[minmax(0,1.2fr)_minmax(0,1fr)_minmax(0,1fr)_minmax(0,1.3fr)] items-baseline gap-x-3 pl-[26px] text-[11px] tabular-nums"
        title={detailTitle}
      >
        <span className="min-w-0 truncate">
          <span className="text-muted-foreground/70">积分 </span>
          <span className={cn("font-medium", creditLoading && "text-muted-foreground/60")}>
            {creditLoading && !credit ? "…" : balanceText}
          </span>
        </span>
        <span className="min-w-0 truncate text-muted-foreground">
          <span className="text-muted-foreground/70">到期 </span>
          {expiringText}
        </span>
        <span className="min-w-0 truncate text-muted-foreground">
          <span className="text-muted-foreground/70">资源包 </span>
          {resourcesText}
        </span>
        <span className="min-w-0 truncate text-muted-foreground">
          <span className="text-muted-foreground/70">Token </span>
          {formatTokenExpiry(meta?.expiresAt)}
        </span>
      </span>
    </label>
  );
}

export default function GatewayPage() {
  const [status, setStatus] = useState<GatewayStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const [port, setPort] = useState(7863);
  const [apiKey, setApiKey] = useState("");
  const [autoStart, setAutoStart] = useState(false);
  /** 网关工作模式：balance 自动 / manual 手动（勾选账号）/ rotation 积分轮转 */
  const [mode, setMode] = useState<GatewayMode>("balance");
  /**
   * 手动模式下勾选的账号 uid 列表（可多选）。
   *
   * 为什么是多选而不是单选：多个账号组成池子、内部仍按到期日分层自动均衡，
   * 这是最常用的用法；只勾一个即等价于旧的「指定账号」。
   */
  const [manualUids, setManualUids] = useState<string[]>([]);
  /**
   * 「限制使用的模型」白名单（多选；**空数组 = 不限制 = 全部**，默认）。
   *
   * 为什么是多选：所有者原话「三个模式,都改为新增一个 筛选模型的功能,默认是全部,
   * 可以多选模型(限制使用的模型)」。单选表达不了「这几个模型我都放行」。
   *
   * 与 `modelFilter`（Token 用量区块的展示筛选）的分工 —— 见上方 `ModelFilter`
   * 的说明：那个只改变**本页展示哪些模型的用量**，不写配置；这个是**服务端放行**
   * 限制，网关会真的拒绝名单外的模型（400 model_not_allowed）。
   *
   * 为什么三个工作模式都显示（此前只在积分轮转下）：限制的是「放行哪些模型」，
   * 与「用哪些账号」是正交的两件事 —— 自动模式下同样可能只想放行几个模型。
   */
  const [allowedModels, setAllowedModels] = useState<string[]>([]);
  /** 网关支持的模型列表（用于模型下拉；取不到时退化为自由输入）。 */
  const [modelOptions, setModelOptions] = useState<string[]>([]);
  /** 模型列表是否正在加载（驱动刷新按钮的转圈）。 */
  const [modelsLoading, setModelsLoading] = useState(false);

  /**
   * 页面布局，持久化到 localStorage（与账号页「紧凑模式」同一套做法：
   * 惰性初始化 + try/catch —— 受限 WebView 里 localStorage 可能不可写）。
   * 默认 `classic`（旧版），用户点击页头按钮才切到 `merged`（新版）。
   */
  const [layout, setLayout] = useState<GatewayLayout>(() => {
    if (typeof window === "undefined") return "classic";
    try {
      return window.localStorage.getItem(LAYOUT_STORAGE_KEY) === "merged" ? "merged" : "classic";
    } catch {
      return "classic";
    }
  });

  function toggleLayout() {
    setLayout((current) => {
      const next: GatewayLayout = current === "classic" ? "merged" : "classic";
      try {
        window.localStorage.setItem(LAYOUT_STORAGE_KEY, next);
      } catch {
        /* 存储不可用时静默：仅本次生效 */
      }
      return next;
    });
  }
  /** 端口可用性检测结果（null = 尚未检测/正在检测）。 */
  const [portCheck, setPortCheck] = useState<GatewayPortCheck | null>(null);
  const [checkingPort, setCheckingPort] = useState(false);
  /**
   * 待确认「结束占用进程」的目标（null = 对话框关闭）。
   *
   * 刻意做成两步：杀进程是不可逆操作，必须先让用户看到
   * 「是哪个进程（名字 + PID + 路径）」再确认，而不是点一下就直接杀。
   */
  const [killTarget, setKillTarget] = useState<GatewayPortCheck | null>(null);
  /** 对话框里展示的占用者：点开对话框后按需查询（不在热路径上查）。 */
  const [killHolder, setKillHolder] = useState<GatewayPortHolder | null>(null);
  const [killHolderLoading, setKillHolderLoading] = useState(false);

  /** 网关 Token 用量：范围选择、数据与加载态。默认「今日」——看用量多为盯当天消耗。 */
  const [usageRange, setUsageRange] = useState<UsageRangeKey>("today");
  const [usage, setUsage] = useState<GatewayUsageResult | null>(null);
  const [usageLoading, setUsageLoading] = useState(true);
  /** 手动刷新触发的自增序号（同范围下重新拉取）。 */
  const [usageNonce, setUsageNonce] = useState(0);
  /** 用量数据「上次成功更新」的时刻（毫秒）；未成功过则为 null。 */
  const [usageUpdatedAt, setUsageUpdatedAt] = useState<number | null>(null);
  /** 每秒自增，驱动「上次更新 xx 秒前」的相对时间重新渲染。 */
  const [nowTick, setNowTick] = useState(() => Date.now());

  /**
   * 用量展示的模型筛选（空数组 = 全部，默认）。
   *
   * 与 `allowedModels` 的分工见上方 `ModelFilter` 的说明：那个是**服务端放行**
   * 限制（写进配置、影响客户端能否调用），这个是**本页只读展示**的收窄。
   * 两者刻意不共用状态 —— 合并会让「我只想看一眼 glm 的用量」变成
   * 「网关拒绝其它模型」，静默掐断客户端请求。
   *
   * 放在页面级而不是区块内部：三个工作模式下都要有这个控件，且账号池卡片、
   * 「按模型 / 按账号」、新版账号明细三处共用同一份筛选，状态必须唯一。
   */
  const [modelFilter, setModelFilter] = useState<string[]>([]);

  /**
   * 「Token 用量」区块里的**双向联动交叉筛选**。
   *
   * 所有者原话：「他不是左右两侧面板么?左边是模型,右边是账号,我希望可以**点击右边
   * 账号筛选左侧模型,点击左侧模型筛选右侧账号**」，并要求旧版（classic）也支持
   *（「旧版的那个 token 用量也要支持这个功能」）。
   *
   * 两者是**同一个筛选器的两个入口**，不是两个独立状态 —— 所以放在一个 state 里：
   * 若拆成 `selectedModel` + `selectedAccount` 两个 state，就得在每一处收尾手工保证
   * 「设置其一时另一个被清掉」，漏一处就会出现「按 A 账号 + B 模型」这种既不是
   * 点账号、也不是点模型意图的组合（详见下面 usageCrossFilter 的注释）。
   *
   * 均以「再点一次 = 取消选择」的 toggle 语义工作。
   *
   * 注意：账号池那一侧本轮已**没有**任何「选中」状态（所有者决定：去掉选中功能，
   * 展开是就地看详情、不是挑一个对象当筛选条件）。因此这里不存在与账号池
   * 抢状态的问题 —— 账号池的行展开状态是 `poolExpandedUids`，只影响它自己。
   */
  const [usageCrossFilter, setUsageCrossFilter] = useState<
    { kind: "model"; key: string } | { kind: "account"; key: string } | null
  >(null);

  /** 把交叉筛选切到「这个模型」；已经是它则取消（toggle）。 */
  function toggleUsageModelFilter(key: string) {
    setUsageCrossFilter((current) =>
      current?.kind === "model" && current.key === key ? null : { kind: "model", key },
    );
  }

  /** 把交叉筛选切到「这个账号」；已经是它则取消（toggle）。 */
  function toggleUsageAccountFilter(key: string) {
    setUsageCrossFilter((current) =>
      current?.kind === "account" && current.key === key ? null : { kind: "account", key },
    );
  }

  /**
   * 积分消耗统计（按账号给 今日 / 近 7 天 / 本月 三个时间窗）。
   *
   * 复用宿主既有的 `/api/credits/stats`（`credit_usage.rs`），**不自己造口径**：
   * 该实现已正确处理「签到/补发导致余额上升不算消耗」这类边界。
   *
   * 独立于网关状态：只要宿主服务在跑就能拿到，与网关是否启动无关。
   * 拿不到（如旧版后端无此接口）就不显示消耗列，其余功能不受影响。
   */
  const [creditStats, setCreditStats] = useState<CreditStatistics | null>(null);
  useEffect(() => {
    let cancelled = false;
    api
      .getCreditStatistics()
      .then((res) => {
        if (!cancelled) setCreditStats(res);
      })
      .catch(() => {
        if (!cancelled) setCreditStats(null);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  /**
   * 账号元信息与逐账号积分，直接复用账号管理页那个 store。
   *
   * 为什么必须复用而不是在本页另拉一份：账号卡片（`account-card.tsx`）展示的
   * 「剩余积分 / N 个积分包 / 到期时间」全部来自这里的 `creditMap`，其数据源是
   * `POST /api/credits`（`credits.rs::get_credit_expiry`）。本页若自己算积分，
   * 就会出现「同一个号在两页显示不同余额」这种最不可解释的偏差 ——
   * 而积分又是选号分层与轮转的判据，两套口径会直接误导用户。
   *
   * 这里只读 store，不新增请求路径；`ensureCredits` 本身会跳过已缓存的账号。
   */
  const storeAccounts = useAccountsStore((s) => s.accounts);
  const creditMap = useAccountsStore((s) => s.creditMap);
  const creditLoadingMap = useAccountsStore((s) => s.creditLoadingMap);
  const ensureCredits = useAccountsStore((s) => s.ensureCredits);
  const fetchAllAccounts = useAccountsStore((s) => s.fetchAll);

  // 直接打开网关页（未经过账号页）时 store 还是空的：补一次加载。
  // 依赖里只有长度与 store 的稳定 action，因此账号库确实为空时也只会跑一次，
  // 不会变成轮询。
  useEffect(() => {
    if (storeAccounts.length === 0) void fetchAllAccounts();
  }, [storeAccounts.length, fetchAllAccounts]);

  useEffect(() => {
    if (storeAccounts.length === 0) return;
    void ensureCredits(storeAccounts.map((account) => account.id));
  }, [storeAccounts, ensureCredits]);

  /**
   * 网关池 uid → 账号库元信息。
   *
   * 两个 id 空间必须显式换算，不能想当然认为相等：
   *  - 网关池的 uid = 账号库的 `uid`（`gateway.rs::build_auth_doc` 用 uid 命名
   *    凭证文件 `workbuddy-{uid}.json`，Go 侧池也按 `Auth.UID` 建索引）
   *  - 积分快照 / `/api/credits/stats` 的 `accountId` = 账号库的 `id`
   *    （`credits.rs` 里 `account.get("id")` 落快照，实测本机 26 个快照账号
   *    全部命中 `accounts[].id`、0 个命中 `uid`）
   * 二者是不同的 uuid，混用会让所有按账号的查询静默返回空。
   */
  const accountByUid = useMemo(() => {
    const map = new Map<string, AccountMeta>();
    for (const account of storeAccounts) {
      if (account.uid) map.set(account.uid, account);
    }
    return map;
  }, [storeAccounts]);

  /** 账号库 id → uid（把积分快照的 id 口径换算回网关池的 uid 口径）。 */
  const uidByAccountId = useMemo(() => {
    const map = new Map<string, string>();
    for (const account of storeAccounts) {
      if (account.uid) map.set(account.id, account.uid);
    }
    return map;
  }, [storeAccounts]);

  /**
   * 网关池 uid → 该账号的积分明细 / 加载态。
   *
   * 单独做一层 uid 索引而不是在渲染里链式查两次：列表每行都要用，
   * 放在渲染函数里会变成 N 次 Map 构造，账号一多就是每帧的固定开销。
   */
  const creditByUid = useMemo(() => {
    const map = new Map<string, CreditExpiry>();
    for (const account of storeAccounts) {
      if (!account.uid) continue;
      const credit = creditMap[account.id];
      if (credit) map.set(account.uid, credit);
    }
    return map;
  }, [storeAccounts, creditMap]);

  const creditLoadingByUid = useMemo(() => {
    const map = new Map<string, boolean>();
    for (const account of storeAccounts) {
      if (!account.uid) continue;
      map.set(account.uid, Boolean(creditLoadingMap[account.id]));
    }
    return map;
  }, [storeAccounts, creditLoadingMap]);

  /**
   * 端口 / API Key 是否存在「已编辑但未保存」的内容。
   *
   * 5 秒轮询会用后端配置刷新界面；若无条件覆盖，用户正在输入的内容会被
   * 中途改回去。因此在用户编辑期间暂停对这两个文本字段的覆盖。
   * 模式与自动启动是开关型操作，改为即时保存，不受此影响。
   */
  const dirtyRef = useRef(false);

  const applyConfig = useCallback((cfg: GatewayConfig) => {
    if (!dirtyRef.current) {
      setPort(cfg.port || portOf(cfg.listen) || 7863);
      setApiKey(cfg.api_key || "");
    }
    setAutoStart(Boolean(cfg.auto_start));
    setMode(cfg.mode === "manual" ? "manual" : cfg.mode === "rotation" ? "rotation" : "balance");
    // 勾选列表：新字段优先；旧配置只有 pinned_uid 时读作「只勾了那一个」
    setManualUids(
      Array.isArray(cfg.manual_uids) && cfg.manual_uids.length > 0
        ? cfg.manual_uids.filter((u): u is string => typeof u === "string" && u.trim() !== "")
        : cfg.pinned_uid
          ? [cfg.pinned_uid]
          : [],
    );
    setAllowedModels(normalizeAllowedModels(cfg.allowed_model));
  }, []);

  /** 手动模式的可选账号：排除已禁用与需重登的（它们不会进池，勾了也没用）。 */
  const availableAccounts = useMemo(
    () =>
      (status?.accounts ?? []).filter(
        (a) => !a.disabled && !a.needsRelogin && typeof a.uid === "string" && a.uid !== "",
      ),
    [status?.accounts],
  );

  /**
   * 切换工作模式并立即生效。
   *
   * 两件事必须一起做，否则用户看到的是「点了没反应」：
   *  1. 模式属于开关型设置：若只改本地状态而等用户点「保存」，5 秒后的轮询会用
   *     后端旧值把它覆盖回负载均衡（用户看到的「点了一会又跳回去」）。
   *  2. 网关账号池是**启动时**扫描凭证目录建立的，光写配置不会改变池内容，
   *     因此必须重导出凭证并重启网关才真正生效。
   * `switchGatewayMode` 在 core 里把「保存 + 重导出 + 按需重启」合成一步。
   */
  async function changeMode(next: GatewayMode) {
    // 切到手动模式时若还没勾账号，默认勾上当前选中的（或第一个可用）账号 ——
    // 否则用户点完立刻看到「未勾选任何账号」的报错，多一步无谓操作。
    const uids =
      next === "manual"
        ? manualUids.length > 0
          ? manualUids
          : availableAccounts.slice(0, 1).map((a) => a.uid)
        : [];
    if (next === "manual" && uids.length === 0) {
      toast.error("手动模式需要先勾选至少一个账号（当前账号库没有可用账号）");
      return;
    }
    setMode(next);
    setManualUids(uids);
    try {
      const res = await api.switchGatewayMode(next, uids);
      if (res.reloaded) {
        toast.success(
          next === "manual"
            ? `已切换为手动模式（${uids.length} 个账号）并重启网关`
            : next === "rotation"
              ? "已切换为积分轮转并重启网关"
              : "已切换为自动模式并重启网关",
          next === "rotation"
            ? { description: "将只用一个账号，烧到不可用才轮转到下一个" }
            : undefined,
        );
      }
      await refresh();
    } catch (e) {
      toast.error(api.asError(e));
      await refresh(); // 回滚为后端真实状态
    }
  }

  /**
   * 手动模式下改勾选：立即重导出凭证并按需重启网关。
   *
   * 为什么每次勾选都立即生效而不是等「保存」：账号池是启动时建立的，
   * 只改本地状态会让用户以为勾了就生效，实际池子没变。
   */
  async function changeManualUids(next: string[]) {
    setManualUids(next);
    if (next.length === 0) {
      // 交给「至少勾一个」的提示，不发请求（后端也会拒绝）
      return;
    }
    try {
      await api.switchGatewayMode("manual", next);
      await refresh();
    } catch (e) {
      toast.error(api.asError(e));
      await refresh();
    }
  }

  /** 勾/取消勾一个账号。 */
  async function toggleManualUid(uid: string) {
    const next = manualUids.includes(uid)
      ? manualUids.filter((u) => u !== uid)
      : [...manualUids, uid];
    await changeManualUids(next);
  }

  /**
   * 切换「随 App 启动」。同样是开关型设置，立即持久化，
   * 否则 5 秒轮询会用后端旧值拨回开关。
   */
  async function changeAutoStart(next: boolean) {
    setAutoStart(next);
    try {
      await api.saveGatewayConfig({ auto_start: next });
      await refresh();
    } catch (e) {
      toast.error(api.asError(e));
      await refresh();
    }
  }

  /**
   * 设置/清除「限制使用的模型」白名单并立即生效。
   *
   * 后端在网关运行时会自动重启它（限制由网关启动时读取），因此这里只需一次调用；
   * 失败时 refresh() 回滚为后端真实值，避免界面显示与实际不符。
   *
   * 空数组 = 解除限制（全部放行）。文案刻意点明「网关会拒绝其它模型」——
   * 这是**服务端**限制，与 Token 用量区块那个只改展示的「模型筛选」不是一回事，
   * 用户分不清就会以为只是换个看法，结果客户端被 400。
   */
  async function changeAllowedModels(next: string[]) {
    setAllowedModels(next);
    try {
      const res = await api.setAllowedModels(next);
      const count = next.length;
      toast.success(
        count === 0
          ? "已解除模型限制（客户端可调用任意模型）"
          : count === 1
            ? `已限制只放行 ${next[0]}，网关会拒绝其它模型`
            : `已限制只放行 ${count} 个模型，网关会拒绝其它模型`,
        res.reloaded
          ? { description: "网关已重启以生效" }
          : count > 0
            ? { description: next.join("、") }
            : undefined,
      );
      await refresh();
    } catch (e) {
      toast.error(api.asError(e));
      await refresh();
    }
  }

  /** 拉取网关支持的模型列表（供「放行模型」勾选列表使用）。 */
  const loadModels = useCallback(async () => {
    setModelsLoading(true);
    try {
      const list = await api.getGatewayModels();
      setModelOptions(list.map((m) => m.id).filter(Boolean).sort());
    } catch {
      // 取不到就保留原列表：勾选列表里仍有当前已选值兜底（标「不在当前列表」）
    } finally {
      setModelsLoading(false);
    }
  }, []);

  const refresh = useCallback(async () => {
    try {
      const s = await api.getGatewayStatus();
      setStatus(s);
      applyConfig(s.config);
      setError(null);
    } catch (e) {
      setError(api.asError(e));
    } finally {
      setLoading(false);
    }
  }, [applyConfig]);

  // 网关状态轮询（5 秒）。
  //
  // 用 useVisibilityInterval 而非裸 setInterval：窗口隐藏/收进托盘时**销毁**定时器，
  // 避免后台空转（裸 setInterval 只在组件卸载时被清理，隐藏时仍在跑）。
  useVisibilityInterval(() => void refresh(), 5000, { onResume: () => void refresh() });

  // Token 用量按所选范围拉取。
  //
  // 首次挂载拉一次模型列表，供「放行模型」勾选列表使用。
  // 只在挂载时拉：模型列表来自网关动态接口，5 秒轮询里重复请求没有意义；
  // 用户需要最新列表时可点菜单里的刷新项。
  useEffect(() => {
    void loadModels();
  }, [loadModels]);

  // 自动刷新：用量区块需要周期更新（网关在持续接请求），但**不**适合挤进 status 的
  // 5 秒轮询 —— 聚合响应可能较大。这里用独立的 30 秒周期，既能自动跟进，
  // 又不会让「切页即请求」把开销放大。
  useEffect(() => {
    let cancelled = false;
    setUsageLoading(true);
    const days = USAGE_RANGE_OPTIONS.find((option) => option.key === usageRange)?.days;
    api
      .getGatewayUsage(days)
      .then((res) => {
        if (!cancelled) {
          setUsage(res);
          // 只在拿到有效快照时更新"上次更新"，失败不该刷新这个时间戳
          // （否则界面会显示一个刚更新过、但其实是错误结果的时刻）。
          if (res.usage) setUsageUpdatedAt(Date.now());
        }
      })
      .catch((e) => {
        if (!cancelled) {
          setUsage({ running: false, reachable: false, usage: null, error: api.asError(e) });
        }
      })
      .finally(() => {
        if (!cancelled) setUsageLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [usageRange, usageNonce]);

  // 用量自动刷新（30 秒一轮）。
  //
  // 只在网关确实在跑时才轮询：未启动时数据不会变，白打请求。
  // 这里直接读 status?.running 而不用下面的 `running` 变量 —— 后者在组件更下方
  // 才声明，在此处引用会命中 TDZ。
  // 隐藏时由 useVisibilityInterval 销毁定时器；恢复可见时立刻补一次（onResume），
  // 让用户切回来就能看到最新用量，而不必再等 30 秒。
  // immediate: false —— 首次拉取由上面依赖 usageNonce 的 effect 负责
  //（它挂载即跑），这里只做周期轮询，否则会重复请求一次。
  useVisibilityInterval(() => setUsageNonce((n) => n + 1), 30_000, {
    enabled: Boolean(status?.running),
    immediate: false,
    onResume: () => setUsageNonce((n) => n + 1),
  });

  // 每秒 tick 一次，只为让「上次更新 x 秒前」这类相对时间保持新鲜。
  // 与用量请求解耦：不额外发请求，仅触发一次廉价的重渲染。
  //
  // 隐藏时一并停掉：看不见的界面不需要刷新相对时间，1 秒一次的定时器
  // 在后台长期空转属于纯浪费。
  // immediate 无所谓（只影响初始值），保持默认 true 让时间戳立刻对齐。
  useVisibilityInterval(() => setNowTick(Date.now()), 1000);

  // 端口变化后防抖检测可用性。
  // 网关正跑在自己的端口上时该端口必然「被占用」，此时不报冲突。
  useEffect(() => {
    const invalid = validatePort(port);
    if (invalid) {
      setPortCheck(null);
      return;
    }
    let cancelled = false;
    setCheckingPort(true);
    const timer = window.setTimeout(async () => {
      try {
        const res = await api.checkGatewayPort(port);
        if (!cancelled) setPortCheck(res);
      } catch {
        if (!cancelled) setPortCheck(null);
      } finally {
        if (!cancelled) setCheckingPort(false);
      }
    }, 350);
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
      setCheckingPort(false);
    };
  }, [port, status?.running]);

  /** 自动挑一个空闲端口。 */
  async function pickFreePort() {
    setCheckingPort(true);
    try {
      // 从当前端口往后找；当前端口自身被网关占用时也能跳过
      for (let candidate = Math.max(port, 1024); candidate < port + 60; candidate += 1) {
        const res = await api.checkGatewayPort(candidate);
        if (res.available || res.inUseByGateway) {
          setPort(candidate);
          setPortCheck(res);
          toast.success(`已选择端口 ${candidate}`);
          return;
        }
      }
      toast.error("未找到空闲端口，请手动指定");
    } catch (e) {
      toast.error(api.asError(e));
    } finally {
      setCheckingPort(false);
    }
  }

  /**
   * 打开「结束占用进程」对话框，并**按需**查询占用者。
   *
   * 为什么不在端口检测时就查：那是页面挂载/端口变化的路径，会 spawn
   * netstat+tasklist+powershell 三个控制台进程，把「打开页面」变成主线程卡顿
   *（实测导致界面未响应，并因频繁创建控制台进程耗尽 desktop heap 而抛
   * 0xc0000142）。这里改成用户显式点击后才查，开销只付一次。
   */
  async function openKillDialog(target: GatewayPortCheck) {
    setKillTarget(target);
    setKillHolderLoading(true);
    setKillHolder(null);
    try {
      const res = await api.getGatewayPortHolder(target.port);
      setKillHolder(res.holder);
    } catch {
      // 查不到占用者不阻断：对话框会提示「无法识别」，用户仍可尝试结束
      setKillHolder(null);
    } finally {
      setKillHolderLoading(false);
    }
  }

  /**
   * 结束占用端口的进程（由确认对话框调用）。
   *
   * 成功后立刻重新检测端口：让用户直接看到「已可用」，
   * 而不是自己再点一次「检测」才知道结果。
   */
  async function killPortHolder() {
    const target = killTarget;
    if (!target) return;
    setBusy("kill-port");
    try {
      const res = await api.killGatewayPortHolder(target.port);
      toast.success(res.message || `端口 ${target.port} 已释放`);
      setKillTarget(null);
      const check = await api.checkGatewayPort(target.port);
      setPortCheck(check);
    } catch (e) {
      // 失败时保留对话框：用户可能需要换个端口，或去看权限问题
      toast.error(api.asError(e));
    } finally {
      setBusy(null);
    }
  }

  /** 端口状态文案与配色。 */
  const portState = (() => {
    const invalid = validatePort(port);
    if (invalid) return { label: invalid, tone: "bad" as const };
    if (checkingPort || !portCheck) return { label: "检测中…", tone: "muted" as const };
    // 网关自己正跑在该端口上时，端口「被占用」是正常的
    if (portCheck.inUseByGateway) return { label: "当前网关正在使用", tone: "ok" as const };
    if (portCheck.available) {
      return portCheck.reserved
        ? { label: "可用（特权端口，可能需管理员权限）", tone: "warn" as const }
        : { label: "可用", tone: "ok" as const };
    }
    return {
      label: portCheck.suggest ? `已被占用，建议改用 ${portCheck.suggest}` : "已被占用",
      tone: "bad" as const,
    };
  })();

  async function run(label: string, fn: () => Promise<unknown>) {
    setBusy(label);
    try {
      await fn();
      await refresh();
    } catch (e) {
      toast.error(api.asError(e));
    } finally {
      setBusy(null);
    }
  }

  const pool = status?.pool ?? null;
  const poolAccounts = pool?.accounts ?? [];
  /** 处于模型冷却的账号数（用于区块顶部的统一说明，替代逐账号重复提示）。 */
  const poolModelCooledCount = poolAccounts.filter((a) => (a.model_cooling?.length ?? 0) > 0).length;
  /**
   * 正因档位更晚而排队、但本身健康的账号数。
   *
   * 排除已禁用/账号级冷却的：它们的不可用另有原因，混进来会让「N 个在排队」
   * 这个数字无法解释（用户会以为排队是它们不可用的原因）。
   */
  const poolQueuedCount = poolAccounts.filter(
    (a) => a.queued && !a.disabled && !a.cooling,
  ).length;
  const running = Boolean(status?.running);
  /** 因需重新登录被排除出账号池的账号（后端同步时不导出其凭证）。 */
  const excludedAccounts = status?.excludedAccounts ?? [];

  // 用量区块的派生数据。
  const usageSnapshot = usage?.usage ?? null;
  const usageSummary = usageSnapshot?.summary ?? null;
  const usageModelsAll = usageSnapshot?.models ?? [];
  const usageAccountsAll = usageSnapshot?.accounts ?? [];
  /** 账号 → 该账号用过的模型明细（网关 /usage 的 accountModels）。 */
  const usageAccountModels = usageSnapshot?.accountModels;
  const usageDaily = usageSnapshot?.daily ?? [];
  const usageMaxDaily = maxOf(usageDaily.map((d) => d.total));

  /**
   * 模型筛选（多选，空 = 全部）作用到**三处**展示上，且只作用于展示：
   *   1. 账号池卡片与进度条/排序（先按模型收窄每个账号的用量，再取最大值归一）
   *   2. 「Token 用量」区块的按模型列表
   *   3. 选中单账号时的模型明细
   * 顶部 4 个汇总数字（总 Token / 输入 / 输出 / 调用次数）**不**跟着变：
   * 它们是「网关总量」的概览，若跟着收窄，用户就再也看不到真实总量，
   * 而且会与「共 N 次调用」这句自相矛盾。下面另给一行「已筛选」说明。
   */
  const modelFilterSet = useMemo(() => new Set(modelFilter), [modelFilter]);
  const modelFilterActive = modelFilter.length > 0;

  /** 可选的模型清单：取自当前范围内的数据（按用量降序，网关已排好）。 */
  const modelFilterOptions = useMemo(
    () => usageModelsAll.map((m) => m.key),
    [usageModelsAll],
  );

  /** 按模型筛选后的账号用量：逐账号把不在筛选内的模型剔掉后重新合计。 */
  const usageAccounts = useMemo(() => {
    if (!modelFilterActive) return usageAccountsAll;
    // 没有 accountModels（旧版网关）时无法做「账号 × 模型」收窄：
    // 此时**原样返回**而不是清零 —— 清零会让所有账号看起来都没用量，
    // 那是在编造一个「筛选后为空」的假结论。界面另给降级说明。
    if (!usageAccountModels) return usageAccountsAll;
    return usageAccountsAll.map((account) => {
      const models = (usageAccountModels[account.key] ?? []).filter((m) =>
        modelFilterSet.has(m.key),
      );
      if (models.length === 0) {
        // 该账号在此筛选下确实没有任何选中模型的用量：归零（这是**真结论**，
        // 与上面「拿不到明细」不同）。键必须保留，否则界面上这个账号会消失。
        return { ...account, total: 0, records: 0, input: 0, output: 0, cacheWrite: 0, cacheRead: 0 };
      }
      const pick = (field: "total" | "records" | "input" | "output" | "cacheWrite" | "cacheRead") =>
        models.reduce((sum, m) => sum + (m[field] ?? 0), 0);
      return {
        ...account,
        total: pick("total"),
        records: pick("records"),
        input: pick("input"),
        output: pick("output"),
        cacheWrite: pick("cacheWrite"),
        cacheRead: pick("cacheRead"),
      };
    });
  }, [usageAccountsAll, usageAccountModels, modelFilterActive, modelFilterSet]);

  /** 第一级（ModelFilter）之后的模型列表 —— 交叉筛选的**输入**。 */
  const usageModels = useMemo(
    () => (modelFilterActive ? usageModelsAll.filter((m) => modelFilterSet.has(m.key)) : usageModelsAll),
    [usageModelsAll, modelFilterActive, modelFilterSet],
  );

  /**
   * 模型 → 用过它的账号 uid 集合（由 `accountModels` 反转而来）。
   *
   * 这是「点模型 → 右侧只剩用过它的账号」**唯一**的数据来源。网关只给了
   * `accountModels`（账号 → 模型），**没有**反向的「模型 → 账号」；而且反向关系
   * 无法从 `models` 与 `accounts` 这两份各自聚合的结果还原 —— 各自求和之后交叉
   * 关系就已经丢了（只知道「甲账号共 3 万」「glm 共 4 万」，推不出「甲账号用过
   * glm 吗」）。这一点 `types.ts` 里 `accountModels` 的注释已经写明。
   *
   * 因此这里在前端把 `accountModels` **反转一次**得到反向索引，而不是新增后端字段：
   *   - 反转的输入就是网关已有的权威交叉累计（Go 侧 `usage.go` 记录时直接累计），
   *     结果与「网关再给一份反向索引」完全等价；
   *   - 规模 = 账号数 × 每号模型数（所有者真实数据：11 个账号 × 1 个模型），
   *     每次用量快照变化后重算一次，代价可忽略；
   *   - 让网关再加一个 `modelAccounts` 字段等于把同一份事实存两遍，两处口径
   *     必然漂移（本项目已有太多「两处各算一遍然后对不上」的教训）。
   *
   * 只在 `usageAccountModels` 存在时构造；缺字段（旧版网关）时得到空 Map，
   * 界面据此走「拿不到明细」的降级分支，而不是假装筛出了空结果。
   */
  const accountKeysByModel = useMemo(() => {
    const map = new Map<string, Set<string>>();
    if (!usageAccountModels) return map;
    for (const [uid, models] of Object.entries(usageAccountModels)) {
      for (const model of models) {
        const users = map.get(model.key);
        if (users) users.add(uid);
        else map.set(model.key, new Set([uid]));
      }
    }
    return map;
  }, [usageAccountModels]);

  /**
   * 交叉明细的**覆盖度**：`accountModels` 的求和 与 各聚合 total 的差额。
   *
   * 为什么必须有这一步（真实数据实测发现，不是假想问题）：
   * 「账号 × 模型」交叉累计是网关**较新**才有的能力。所有者本机真实
   * `usage.json`（只读副本）里，`models`/`accounts` 覆盖 09-13 ~ 09-17 共 5 天，
   * 而 `accountModels` 只有 09-16 / 09-17 两天 —— 升级前的历史用量只有
   * 「按模型」「按账号」两个各自聚合的结果，交叉维度是**空的**。
   *
   * 后果（若不判覆盖度）：真实数据下点那 7 个「只在 09-13~09-15 有量」的账号，
   * 左侧会得到空列表，界面就会说「该账号没有用过任何模型」—— 这是**假结论**，
   * 用户明明看着右边写着它有 2287 次调用。把「明细缺失」说成「确实没用过」，
   * 正是本次需求里最不能犯的错。
   *
   * 判定方式：在同一统计范围内比较「交叉求和」与「该维度聚合 total」。
   *   - 相等 → 交叉明细完整，空列表可以放心解释成「确实没用过」；
   *   - 交叉求和更小 → 明细**部分缺失**，界面必须说明列表可能不全，
   *     并在列表为空时明确讲「拿不到明细」而不是「没有用过」。
   *
   * 只在两个方向各算一次（账号数 + 模型数），代价可忽略。
   */
  const usageCrossCoverage = useMemo(() => {
    const accountCross = new Map<string, number>();
    const modelCross = new Map<string, number>();
    if (!usageAccountModels) return { accountCross, modelCross };
    for (const [uid, models] of Object.entries(usageAccountModels)) {
      let accountSum = 0;
      for (const model of models) {
        const total = model.total ?? 0;
        accountSum += total;
        modelCross.set(model.key, (modelCross.get(model.key) ?? 0) + total);
      }
      accountCross.set(uid, accountSum);
    }
    return { accountCross, modelCross };
  }, [usageAccountModels]);

  /**
   * 当前被点的那一项，交叉明细是否**不完整**（缺失量 > 0）。
   *
   * 返回 `{ known, expected }`：known = 交叉明细求和，expected = 该维度的聚合 total。
   *   - `known === 0 && expected > 0` → **完全没有明细**（历史用量），
   *     此时绝不能断言「它没用过任何模型」；
   *   - `0 < known < expected` → 明细**部分缺失**，列表可用但可能不全。
   */
  const usageCrossMissing = useMemo(() => {
    if (!usageCrossFilter || !usageAccountModels) return null;
    const { accountCross, modelCross } = usageCrossCoverage;
    if (usageCrossFilter.kind === "account") {
      const expected = usageAccountsAll.find((a) => a.key === usageCrossFilter.key)?.total ?? 0;
      return { known: accountCross.get(usageCrossFilter.key) ?? 0, expected };
    }
    const expected = usageModelsAll.find((m) => m.key === usageCrossFilter.key)?.total ?? 0;
    return { known: modelCross.get(usageCrossFilter.key) ?? 0, expected };
  }, [usageCrossFilter, usageAccountModels, usageCrossCoverage, usageAccountsAll, usageModelsAll]);

  /** 交叉明细对该项**完全缺失**（用来区分「确实没用过」与「拿不到明细」）。 */
  const usageCrossItemBlank = Boolean(
    usageCrossMissing && usageCrossMissing.expected > 0 && usageCrossMissing.known === 0,
  );
  /** 交叉明细对该项**部分缺失**（列表能用，但要提示可能不全）。 */
  const usageCrossItemPartial = Boolean(
    usageCrossMissing && usageCrossMissing.known > 0 && usageCrossMissing.known < usageCrossMissing.expected,
  );

  const usageCrossModel = usageCrossFilter?.kind === "model" ? usageCrossFilter.key : "";
  const usageCrossAccount = usageCrossFilter?.kind === "account" ? usageCrossFilter.key : "";
  const usageCrossActive = usageCrossFilter !== null;
  /**
   * 交叉筛选已选中，但网关没给 `accountModels` —— 此时**不施加**第二级筛选。
   *
   * 为什么不退回「用别的数据凑一个近似结果」：`models` / `accounts` 里根本没有
   * 交叉信息，任何凑法都是在编造。界面改用明确文案说明「拿不到明细」，这比给出
   * 一个看似有理、实则错误的结果安全得多。
   */
  const usageCrossDegraded = usageCrossActive && !usageAccountModels;

  /**
   * 两级筛选的**叠加语义**（这里是最容易出「筛不出东西却不知道为什么」的地方，
   * 所以先把语义定死再实现）。
   *
   *   第一级 = 顶部 `ModelFilter` 多选下拉 → 「我要看哪几个模型」
   *   第二级 = 本区块内的点选（点账号 / 点模型）→ 「我要看与这一项相关的数据」
   *
   * 两者**既不是二选一，也不是并集**，而是「第一级先定出可见集合，第二级在这个
   * 集合内再收窄」：
   *   - 点账号 → 左侧 = 该账号用过的模型 ∩ ModelFilter 选中的模型
   *   - 点模型 → 右侧 = 用过该模型的账号；而这些账号的数值仍只算 ModelFilter
   *     选中模型的那部分（因为 `usageAccounts` 已经是第一级的结果）
   *
   * 为什么第二级不「覆盖」第一级：ModelFilter 是用户在顶部**明确声明**的全局意图，
   * 若点个账号就把它顶掉，用户回到顶部会看到勾选还在、数据却已不是那个意思 ——
   * 那才是真正的「不知道为什么」。叠加虽可能筛出空集，但空集是**可解释**的：
   * 提示条会同时列出两级条件与各自的命中情况（见下方 usageCrossNote），
   * 用户一眼能看出是哪一级把数据掐没了。
   *
   * 为什么第二级是「单一选择」而不是多选：所有者的原话是「点击右边账号筛选左侧
   * 模型，点击左侧模型筛选右侧账号」—— 每一次点击都是**一个**条件。做成多选后
   * 「点第二个账号」就有了「替换 / 追加」两义，而追加会让两侧列表迅速收敛到
   * 空集（多账号 × 多模型的交集通常为空），那不是他描述的行为。
   */
  const usageModelsScoped = useMemo(() => {
    if (!usageCrossFilter || !usageAccountModels) return usageModels;
    if (usageCrossFilter.kind === "model") {
      // 点模型：左侧收窄到被点的这一个（它必然在 usageModels 里，否则点不到）。
      return usageModels.filter((m) => m.key === usageCrossFilter.key);
    }
    // 点账号：左侧只剩该账号用过的模型；没给明细时上面已提前返回。
    const owned = new Set((usageAccountModels[usageCrossFilter.key] ?? []).map((m) => m.key));
    return usageModels.filter((m) => owned.has(m.key));
  }, [usageModels, usageCrossFilter, usageAccountModels]);

  const usageAccountsScoped = useMemo(() => {
    if (!usageCrossFilter || !usageAccountModels) return usageAccounts;
    if (usageCrossFilter.kind === "account") {
      // 点账号：右侧收窄到被点的这一个（它必然在 usageAccounts 里，否则点不到）。
      return usageAccounts.filter((a) => a.key === usageCrossFilter.key);
    }
    // 点模型：右侧只剩用过该模型的账号 —— 靠上面反转出的反向索引，不靠数值大小
    // 猜（用 `total > 0` 猜会在「该账号在此范围外/被第一级筛掉」时给出错误结论）。
    const users = accountKeysByModel.get(usageCrossFilter.key);
    if (!users) return [];
    return usageAccounts.filter((a) => users.has(a.key));
  }, [usageAccounts, usageCrossFilter, usageAccountModels, accountKeysByModel]);

  const usageScopeAccount = usageCrossFilter?.kind === "account" ? usageCrossFilter.key : "";
  const usageScopeModel = usageCrossFilter?.kind === "model" ? usageCrossFilter.key : "";

  /**
   * `accountModels` 的二级索引：uid → (模型名 → 该账号在该模型上的计量)。
   *
   * 为什么单独建这一层：换口径后**每一行**都要按「另一侧的那一项」反查一次
   * （点账号 → 左侧每个模型查一次；点模型 → 右侧每个账号查一次）。直接在原始数组里
   * `find` 是 O(行数 × 该账号模型数)，真实数据下模型 30 个、账号 18 个，每次交互就
   * 多出几百次线性扫描；建索引一次 O(总数)，之后每行 O(1)。
   *
   * 索引值就是网关给的 `GatewayUsageGroup`（含 total/records/input/output…），
   * 因此换口径时可以把这些字段**整体**铺到行上，不需要任何前端二次累加 ——
   * 少一次自己算，就少一处口径漂移的机会（本项目的老教训）。
   */
  const crossByUidModel = useMemo(() => {
    const map = new Map<string, Map<string, GatewayUsageGroup>>();
    if (!usageAccountModels) return map;
    for (const [uid, models] of Object.entries(usageAccountModels)) {
      const inner = new Map<string, GatewayUsageGroup>();
      for (const model of models) inner.set(model.key, model);
      map.set(uid, inner);
    }
    return map;
  }, [usageAccountModels]);

  /**
   * 「点账号 → 左侧每行的数值换成该账号在该模型上的量」—— 本次语义修正的**核心**。
   *
   * 之前这里只做了「筛掉不相关的行」，没做「换值」：留下的行仍显示全局聚合
   * `model.total`。单模型范围下筛完还是那一行、数字逐字相同，于是所有者点了账号
   * 看不到任何变化，说出「点击右侧账号,但是左侧模型的消耗量并没有变,所以我才说不对」。
   *
   * 现在的语义（所有者已确认）：
   *   - 列表内容：仍筛掉无关行（不变）；
   *   - **每行数值**：换成该账号 × 该模型的量。数据取自网关的 `accountModels`，
   *     即「由网关侧记录时直接累计」的权威交叉值，前端不做任何二次推导；
   *   - **meta（调用/输入/输出）**：同样换成该账号的 —— 只换总数而留着全局调用次数
   *     会造出「一亿 tokens 却 0 次调用」这种自相矛盾的行；
   *   - **占比条基准**：换成该账号内的峰值（见 usageMaxModelScoped）。
   *
   * 关于「明细缺失」这条兜底路径（真实数据里必然遇到，不能丢）：
   * `accountModels` 可能只覆盖部分日期 —— 所有者真实 `usage.json` 里
   * `models`/`accounts` 覆盖 09-13~09-17 共 5 天，而 `accountModels` 只有 09-16/09-17。
   * 因此这里**只对「明细里确实有这一格」的行换值**：查不到就原样保留该行的全局数值，
   * 绝不清零。清零等于把「网关没记明细」渲染成「该账号在这个模型上没用量」—— 那正是
   * 本次需求里最不能犯的假结论（用户右边明明看到该账号有 2287 次调用）。
   * 真正「查不到」另有两条出口：该账号在明细里完全缺席时列表本就为空（走
   * usageCrossItemBlank 的「拿不到明细」文案），部分缺席时走 usageCrossItemPartial
   * 的「列表可能不全」提示。
   */
  const usageModelsDisplay = useMemo(() => {
    if (!usageScopeAccount || !usageAccountModels) return usageModelsScoped;
    const byModel = crossByUidModel.get(usageScopeAccount);
    if (!byModel) return usageModelsScoped;
    return usageModelsScoped.map((model) => {
      const hit = byModel.get(model.key);
      // 查不到 → 原样返回（保留全局值），而不是清零。理由见上面的长注释。
      if (!hit) return model;
      return {
        ...model,
        total: hit.total ?? 0,
        records: hit.records ?? 0,
        input: hit.input ?? 0,
        output: hit.output ?? 0,
        cacheWrite: hit.cacheWrite ?? 0,
        cacheRead: hit.cacheRead ?? 0,
      };
    });
  }, [usageModelsScoped, usageScopeAccount, usageAccountModels, crossByUidModel]);

  /**
   * 「点模型 → 右侧每行的数值换成该模型在该账号上的量」—— 反方向同理。
   *
   * 所有者的要求是**双向**的（「点账号换模型的数字，点模型换账号的数字」）。
   * 反查的格子在正常情况下必然存在（右侧列表就是从 accountKeysByModel 筛出来的），
   * 但这里仍按「查不到就不动」处理 —— 与正向保持同一条兜底规则，避免两个方向在
   * 边界上给出不同答案。
   */
  const usageAccountsDisplay = useMemo(() => {
    if (!usageScopeModel || !usageAccountModels) return usageAccountsScoped;
    return usageAccountsScoped.map((account) => {
      const hit = crossByUidModel.get(account.key)?.get(usageScopeModel);
      if (!hit) return account;
      return {
        ...account,
        total: hit.total ?? 0,
        records: hit.records ?? 0,
        input: hit.input ?? 0,
        output: hit.output ?? 0,
        cacheWrite: hit.cacheWrite ?? 0,
        cacheRead: hit.cacheRead ?? 0,
      };
    });
  }, [usageAccountsScoped, usageScopeModel, usageAccountModels, crossByUidModel]);

  /**
   * 占比条的百分**基准**：随**口径**走，而不是永远用全局峰值。
   *
   *   - 未点任何一项 → 全局峰值，回答「谁占整体大头」；
   *   - 点账号 → **该账号内**的峰值。回答「在这个号里谁占大头」。
   *     这同时解决了旧基准的一个视觉死角：被点的那个号里所有模型的条长之和本就只是
   *     全局的一小块，条**普遍很短**，看起来像「这个号几乎没用」；换成账号内归一后
   *     条长重新铺满，内部结构一眼可辨；
   *   - 点模型 → **该模型在各账号上**的峰值（右侧列表）。回答「这个模型主要被谁用了」。
   *
   * 与「筛选」的分界（刻意划定的，有测试锁着）：基准只随**口径**（今日/近 7 天/…、
   * 点了哪一项）变，**不随**顶部 ModelFilter 变 —— ModelFilter 是「我要看哪几个模型」
   * 的展示收窄，若基准也跟着收窄，勾一个模型就会让所有条重新顶满，用户反而看不出
   * 「它在总量里占多少」。
   *
   * 取峰值时一律用**未经行筛选**的完整集合（该账号的全部模型 / 用过该模型的所有
   * 账号）：若只用筛出来的那几行，被筛掉的那个恰好是峰值时，剩下的条会莫名其妙顶满。
   *
   * 明细缺失时退回全局基准：拿不到交叉明细就得不到「该账号内的峰值」，而用 0 当基准
   * 会让所有条宽归零 —— 等于把「拿不到明细」画成「没有用量」。
   */
  const usageMaxModelScoped = useMemo(() => {
    if (usageScopeAccount && usageAccountModels) {
      const owned = usageAccountModels[usageScopeAccount];
      if (owned && owned.length > 0) return maxOf(owned.map((m) => m.total));
    }
    return maxOf(usageModelsAll.map((m) => m.total));
  }, [usageScopeAccount, usageAccountModels, usageModelsAll]);

  const usageMaxAccountScoped = useMemo(() => {
    if (usageScopeModel && usageAccountModels) {
      const values: number[] = [];
      for (const inner of crossByUidModel.values()) {
        const hit = inner.get(usageScopeModel);
        if (hit) values.push(hit.total ?? 0);
      }
      if (values.length > 0) return maxOf(values);
    }
    return maxOf(usageAccountsAll.map((a) => a.total));
  }, [usageScopeModel, usageAccountModels, crossByUidModel, usageAccountsAll]);

  /**
   * 顶部四张汇总卡的口径。
   *
   * 所有者明确选了「跟着变」：点了账号后界面进入「这个账号的视角」，上面四张卡与
   * 下面两个列表处处一致。反面（不变）会造出「上面 1238.3M、下面 199.7M」两个数字
   * 并存的局面 —— 那正是所有者本轮抱怨的同一类问题的镜像。
   *
   * 为什么「跟着变」之外还必须再加一条常驻口径条（见 JSX 里的
   * `data-slot="usage-scope-bar"`）：只把数字换掉而不说明换成了什么口径，用户会以为
   * **总量掉了 / 数据丢了**。口径条把「当前只统计这一个账号」写在数字紧上方，
   * 两处互为解释。这是所有者确认过的设计（草稿「当前口径：账号 <uid 前 8 位>」）。
   *
   * 取值优先级：明细里有该格 → 用交叉值；明细缺失 → 退回该账号在该范围内的聚合值
   * （`usageAccountsAll` 里的 total/records/…）。退而求其次而不是显示 0，与上面两条
   * 兜底规则一致：**绝不把「明细缺失」渲染成「没有用量」**。
   */
  const usageSummaryDisplay = useMemo(() => {
    if (!usageCrossFilter || !usageSummary) return usageSummary;
    if (usageScopeAccount) {
      const hit = crossByUidModel.get(usageScopeAccount);
      if (hit) {
        const sum = (pick: (m: GatewayUsageGroup) => number) =>
          [...hit.values()].reduce((acc, m) => acc + (pick(m) ?? 0), 0);
        const input = sum((m) => m.input);
        const cacheRead = sum((m) => m.cacheRead);
        return {
          ...usageSummary,
          total: sum((m) => m.total),
          records: sum((m) => m.records),
          input,
          output: sum((m) => m.output),
          cacheWrite: sum((m) => m.cacheWrite),
          cacheRead,
          uncachedInput: sum((m) => m.uncachedInput),
          // 命中率要跟着重算：直接沿用全局值会让「这个号」的输入与命中率对不上。
          cacheHitRate: input > 0 ? cacheRead / input : null,
        };
      }
      const account = usageAccountsAll.find((a) => a.key === usageScopeAccount);
      return account ? { ...usageSummary, ...account, key: undefined } : usageSummary;
    }
    if (usageScopeModel) {
      const model = usageModelsAll.find((m) => m.key === usageScopeModel);
      return model ? { ...usageSummary, ...model, key: undefined } : usageSummary;
    }
    return usageSummary;
  }, [
    usageCrossFilter,
    usageSummary,
    usageScopeAccount,
    usageScopeModel,
    crossByUidModel,
    usageAccountsAll,
    usageModelsAll,
  ]);

  const usageNickname = useMemo(() => {
    const map = new Map<string, string>();
    for (const account of status?.accounts ?? []) {
      map.set(account.uid, account.nickname || account.uid.slice(0, 8));
    }
    return map;
  }, [status?.accounts]);

  /**
   * 「当前正在按什么筛选」的可视提示（提示条正文 + 一键清除按钮）。
   *
   * 为什么这条**必须**存在：点了账号后左边少了一半模型，若页面上没有任何说明，
   * 用户只会认为「数据丢了 / 页面出错了」—— 他不会想到是自己点出来的。
   * 尤其本区块有两个可点的列表，误触的概率并不低。
   *
   * 文案刻意包含三件事，缺一不可：
   *   1. 点的是**哪个**账号 / 模型（否则选了两个名字相近的模型时分不清）；
   *   2. 另一侧因此**收窄到了几个**（「左侧只剩它用过的 2 个模型」）—— 这是本次
   *      点击的直接后果，也是用户确认「点对了」的依据；
   *   3. 结果是空集时**明确指出是哪一级掐掉的** —— 两级筛选叠加后最常见的困惑
   *      就是「筛不出东西但不知道为什么」，这里把两个集合的规模都报出来。
   */
  const usageCrossNote = useMemo(() => {
    if (!usageCrossFilter) return null;
    // 分子分母都必须落在**被收窄的那一侧**，即「另一侧」：
    //   点模型 → 收窄的是右侧账号列表 → 分母 = 全量账号数
    //   点账号 → 收窄的是左侧模型列表 → 分母 = 全量模型数
    // 这里曾经写反（点模型时分母取全量**模型**数），真实数据下渲染成
    // 「右侧只显示用过它的账号（8 / 1）」—— 8 个账号配 1 个模型的分母，
    // 读起来像是「8 个里只有 1 个」，与事实相反。分子分母必须同维度。
    const total = usageCrossFilter.kind === "model" ? usageAccountsAll.length : usageModelsAll.length;
    const visible = usageCrossFilter.kind === "model" ? usageAccountsScoped.length : usageModelsScoped.length;
    const visibleLabel = usageCrossFilter.kind === "model" ? "账号" : "模型";

    if (usageCrossDegraded) {
      return {
        text: `已点击${usageCrossFilter.kind === "model" ? "模型" : "账号"}「${usageCrossFilter.key}」，但当前网关未提供「账号 × 模型」明细，无法据此筛选。`,
        hint: "请更新网关后重试；下面的列表仍是未筛选的全量数据。",
        emptyReason: "",
      };
    }

    // 该项**有用量但交叉明细完全缺失**（真实数据里升级前的历史用量就是这样）：
    // 此时空列表的含义是「拿不到明细」，**不是**「它没用过」。
    // 把它说成「没有用过任何模型」是本次最容易犯、也最伤人的假结论 ——
    // 用户右边明明看到它有 2287 次调用。
    if (usageCrossItemBlank && visible === 0) {
      return {
        text:
          usageCrossFilter.kind === "account"
            ? `已点击账号「${usageNickname.get(usageCrossFilter.key) ?? usageCrossFilter.key.slice(0, 8)}」，但网关没有记录它在「账号 × 模型」里的明细，无法列出它用过哪些模型。`
            : `已点击模型「${usageCrossFilter.key}」，但网关没有记录它在「账号 × 模型」里的明细，无法列出哪些账号用过它。`,
        hint: "该账号/模型的用量发生在网关开始记录交叉明细之前；换更近的统计范围（如「今日」）可看到明细。",
        emptyReason: `此范围内它有 ${exactTokenFormatter.format(usageCrossMissing?.expected ?? 0)} tokens 的用量，只是明细缺失。`,
      };
    }

    // 部分缺失：列表可用但可能不全，必须提示，否则用户会以为「它就只用了这一个模型」。
    const partialHint = usageCrossItemPartial
      ? `注意：网关只记录了部分「账号 × 模型」明细（${formatUsageCompact(usageCrossMissing?.known ?? 0)} / ${formatUsageCompact(usageCrossMissing?.expected ?? 0)}），以下列表可能不全。`
      : "再次点击同一项可取消选择。";

    // 两个维度都为 0 说明第一级（ModelFilter）就把数据掐没了：
    // 被点的这项在 ModelFilter 选中的模型里一个都没用上。
    const emptyReason =
      visible === 0 && total === 0
        ? "该范围内没有任何用量数据。"
        : visible === 0
          ? modelFilterActive
            ? `它用过的${visibleLabel}都不在顶部「模型筛选」选中的 ${modelFilter.length} 个模型里 —— 请放宽或清除模型筛选。`
            : `它没有用过任何${visibleLabel}。`
          : "";
    return {
      text:
        usageCrossFilter.kind === "account"
          ? `已点击账号「${usageNickname.get(usageCrossFilter.key) ?? usageCrossFilter.key.slice(0, 8)}」，左侧只显示它用过的模型（${visible} / ${total}）。`
          : `已点击模型「${usageCrossFilter.key}」，右侧只显示用过它的账号（${visible} / ${total}）。`,
      hint: partialHint,
      emptyReason,
    };
  }, [
    usageCrossFilter,
    usageCrossDegraded,
    usageCrossItemBlank,
    usageCrossItemPartial,
    usageCrossMissing,
    usageModelsAll,
    usageAccountsAll,
    usageModelsScoped,
    usageAccountsScoped,
    usageNickname,
    modelFilterActive,
    modelFilter,
  ]);

  /** uid → 该账号在所选范围内的 Token 用量（卡片直接取用）。 */
  const usageByUid = useMemo(
    () => new Map(usageAccounts.map((u) => [u.key, u])),
    [usageAccounts],
  );

  /**
   * 网关池 uid → 该账号在所选范围内的**积分消耗**。
   *
   * 后端的 `/api/credits/stats` 直接给了三个时间窗（usageToday / usage7Days /
   * usageThisMonth），与本页的日期筛选一一对应，因此这里按当前筛选取值即可，
   * 不需要前端再做时间切分 —— 口径统一交给 `credit_usage.rs`，避免两处算法不一致。
   *
   * 注意「全部」没有对应字段：积分快照本身只保留 30 天（retentionDays），
   * 取 usageThisMonth 会低估，故「全部」不显示消耗（用 0 表示无数据）。
   *
   * 键必须换算成 uid：统计接口按账号库 `id` 分组，而卡片查的是池 `uid`。
   * 此前直接 `map.set(a.accountId, ...)` 再按 uid 查，两个 uuid 空间对不上，
   * 于是「消耗积分」恒为 0 —— 表现为整列都是「—」，看起来像「这些号都没消耗」，
   * 而实际是查错了键。账号库尚未加载时映射为空，同样退化为不显示（而非显示 0）。
   */
  const creditUsedByUid = useMemo(() => {
    const map = new Map<string, number>();
    for (const a of creditStats?.accounts ?? []) {
      const uid = uidByAccountId.get(a.accountId);
      if (!uid) continue;
      const used =
        usageRange === "today"
          ? a.usageToday
          : usageRange === "7d"
            ? a.usage7Days
            : usageRange === "30d"
              ? a.usageThisMonth
              : 0; // 「全部」：快照只留 30 天，给不出可信值，故不显示
      map.set(uid, used ?? 0);
    }
    return map;
  }, [creditStats, usageRange, uidByAccountId]);

  /** 当前日期筛选的中文名，用于表格列头的 tooltip。 */
  const usageRangeLabel = USAGE_RANGE_OPTIONS.find((o) => o.key === usageRange)?.label ?? "统计范围";

  /**
   * 网关池 uid → **今日**消耗积分（二级行「积分」块用）。
   *
   * 为什么不复用 `creditUsedByUid`：那一份跟着上面的日期筛选走，而二级行的
   * 「今日消耗积分」是**固定档**（列名已经写明「今日」）。`CreditStatisticsResource`
   * 只有今日 / 近 7 天 / 本月三个窗口——「近 30 天」与「全部」都落回本月，
   * 所以二级行不能复述筛选范围，否则「本月」会被说成「近 30 天」。
   *
   * 与上面同样的键换算：统计接口按账号库 `id` 分组，而池里是 `uid`。
   */
  const creditTodayByUid = useMemo(() => {
    const map = new Map<string, number>();
    for (const a of creditStats?.accounts ?? []) {
      const uid = uidByAccountId.get(a.accountId);
      if (!uid) continue;
      map.set(uid, a.usageToday ?? 0);
    }
    return map;
  }, [creditStats, uidByAccountId]);

  /**
   * 账号池表格的行指标：**当前排序与列显示的唯一数据源**。
   *
   * 为什么排序与显示必须同源：`#`（消耗排名）与「消耗 Token」列若各算一遍，
   * 一旦有一处口径不同（比如排名把无数据的算 0、列却显示「—」），
   * 就会出现「第 3 名显示 0」这种自相矛盾的行，而这正是最难解释的一类 bug。
   *
   * 这几步刻意**不用 useMemo**：数据量是账号数（十几条），而本页每秒都有
   * 一次重渲染（驱动「上次更新 x 秒前」），缓存省下的遍历远小于维护依赖数组的
   * 成本 —— 更别说依赖写错时会拿到上一轮的旧排序，那才是真麻烦。
   */
  const poolRows: PoolRowMetrics[] = (() => {
    // 「消耗占比条」的 100% 参照物 = **本批账号里的最高消耗**。
    //
    // 不用固定上限（如「按额度算百分比」）：所有者的要求是「谁的最长就以谁为
    // 参照物」。固定上限在用量普遍很低时会让所有条都缩成一条线，反而看不出
    // 相对高低 —— 而「谁烧得多」正是这列要回答的问题。
    // maxOf 的下限是 1，因此下面算比例时不会除零。
    const totals = poolAccounts.map((acc) => usageByUid.get(acc.uid)?.total ?? 0);
    const max = maxOf(totals);
    const rows = poolAccounts.map((acc) => {
      const usageRow = usageByUid.get(acc.uid);
      const total = usageRow?.total ?? 0;
      // 「全部」范围给不出可信的积分消耗（积分快照只保留 30 天），显示「—」而不是 0——
      // 0 会被读成「这个号今天没消耗」，而事实是「这个范围查不到」。
      const creditUsed =
        usageRange === "all" ? null : (creditUsedByUid.get(acc.uid) ?? (creditStats ? 0 : null));
      return {
        acc,
        total,
        records: usageRow?.records ?? 0,
        percent: Math.round((total / max) * 100),
        // 排名先占位，下面按消耗排序后统一回填。
        rank: 0,
        creditUsed,
        creditToday: creditStats ? (creditTodayByUid.get(acc.uid) ?? 0) : null,
        balance: typeof acc.credits === "number" ? acc.credits : null,
        expiryKey: poolExpiryKey(acc.expire_day),
        stateRank: poolStateRank(acc),
        modelCoolCount: (acc.model_cooling ?? []).length,
      } satisfies PoolRowMetrics;
    });
    // 「#」列是**消耗排名**（第 1 名 = 消耗最高），不是行号 —— 这样即使按别的列
    // 排序，也能立刻看出「这个号在消耗上排第几」。并列时同名次之后跳号。
    [...rows]
      .filter((row) => row.total > 0)
      .sort((left, right) => right.total - left.total)
      .forEach((row, index) => {
        row.rank = index + 1;
      });
    return rows;
  })();

  /**
   * 表格的排序状态。默认「消耗 Token 降序」（所有者确认过的默认）。
   *
   * 旧版（classic）走另一条路（见 `poolRowMetricsForClassic`），不读这个状态：
   * 旧版的契约是「默认观感与升级前逐字一致」，不能被排序悄悄改掉顺序。
   */
  const [poolSort, setPoolSort] = useState<{ key: PoolSortKey; dir: "asc" | "desc" }>({
    key: "tokens",
    dir: "desc",
  });

  /** 点表头：同一列再点一次切升降序，换列则用该列的默认方向。 */
  function togglePoolSort(key: PoolSortKey) {
    setPoolSort((current) =>
      current.key === key
        ? { key, dir: current.dir === "asc" ? "desc" : "asc" }
        : { key, dir: POOL_SORT_DEFAULT_DIR[key] },
    );
  }

  /** 展开的行（uid 集合）—— 可同时展开多行，便于对比两三个账号。 */
  const [poolExpandedUids, setPoolExpandedUids] = useState<string[]>([]);
  const poolExpandedSet = new Set(poolExpandedUids);
  const poolAllExpanded = poolAccounts.length > 0 && poolExpandedUids.length === poolAccounts.length;

  function togglePoolExpanded(uid: string) {
    setPoolExpandedUids((current) =>
      current.includes(uid) ? current.filter((item) => item !== uid) : [...current, uid],
    );
  }

  /**
   * 表格展示顺序：按当前排序列 + 方向排序。
   *
   * **「拿不到值」的行永远垫底，且不随升降序翻转**（判定见 `poolCellBlank`）。
   * 这是本实现第一版真实踩到的坑：把「无数据」当成 0 参与比较，升序时它们会全部
   * 冲到最前面 —— 一屏「—」看起来像表坏了（实测 uid-4 冲到了第一位）。
   *
   * 比较结果相同时不交换（`sort` 在 V8 上是稳定的）——「还没拿到数据」与
   * 「真的都没消耗」都不会把网关原序打乱成随机顺序。
   */
  const poolSortedRows = [...poolRows].sort((left, right) => {
    const leftBlank = poolCellBlank(poolSort.key, left);
    const rightBlank = poolCellBlank(poolSort.key, right);
    // 两个都空 → 视为相等（保持网关原序）；只有一个空 → 它垫底。
    if (leftBlank || rightBlank) {
      if (leftBlank && rightBlank) return 0;
      return leftBlank ? 1 : -1;
    }
    const diff = POOL_SORT_OPTIONS[poolSort.key](left, right);
    return poolSort.dir === "asc" ? diff : -diff;
  });

  const endpoint = status?.openaiBase ?? "";
  const endpointHint = useMemo(() => {
    if (!endpoint) return "";
    return `OPENAI_BASE_URL=${endpoint}`;
  }, [endpoint]);

  async function copyEndpoint() {
    if (!endpoint) return;
    try {
      await navigator.clipboard.writeText(endpoint);
      toast.success("已复制接口地址");
    } catch {
      toast.error("复制失败，请手动选择文本");
    }
  }

  return (
    // 流式布局：内容随窗口铺满（减去侧栏），只保留内边距。
    //
    // 留白的演进（按 2340px 屏、侧栏 220px 计算，每侧留白）：
    //   max-w-3xl (768px)    → 676px
    //   max-w-[1180px]       → 470px   ← 用户反馈"还是很多留白"
    //   max-w-[1800px]       → 160px   ← 用户反馈"还是留白太多"
    //   去除上限（本版）      → 0（仅 px-8 内边距）
    // 结论：只要保留居中定宽，宽屏上就一定有留白；本页内容（状态块、设置行、
    // 账号池卡片、用量表）都是横向铺开的行式布局，能自然拉伸，故完全放开。
    // 账号管理页同步做了相同处理，两页留白表现保持一致。
    // Radix Tooltip 必须有 TooltipProvider 祖先，否则抛错导致整页白屏
    //（本页此前没有 Tooltip，故一直没有该 Provider；本轮新增了页头 tooltip）。
    <TooltipProvider delayDuration={250}>
    <div className="w-full space-y-6 px-5 py-6 sm:px-8 sm:py-8">
      <header className="flex min-w-0 items-start justify-between gap-3">
        <div className="min-w-0">
          <h1 className="flex items-center gap-2 text-lg font-medium leading-6">
            <Server className="size-4.5 shrink-0" />
            兼容网关
          </h1>
          <p className="mt-1 text-xs text-muted-foreground">
            把账号库里的账号变成 OpenAI 兼容接口，供任意 SDK / 客户端使用。
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-1">
          <Button
            variant="ghost"
            size="icon"
            className="shrink-0"
            onClick={() => void refresh()}
            aria-label="刷新"
            disabled={busy !== null}
          >
            <RefreshCw className={cn("size-4", busy === "refresh" && "animate-spin")} />
          </Button>
        </div>
      </header>

      {error ? (
        <Alert variant="destructive">
          <AlertTriangle />
          <AlertTitle>无法读取网关状态</AlertTitle>
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      ) : null}

      {status && !status.exeFound ? (
        <Alert>
          <AlertTriangle />
          <AlertTitle>未找到网关可执行文件</AlertTitle>
          <AlertDescription>
            请把 <code className="font-mono">gateway.exe</code> 放到 ai-gateway
            同目录，或用环境变量 <code className="font-mono">AI_GATEWAY_ROUTER_BIN</code> 指定路径。
          </AlertDescription>
        </Alert>
      ) : null}

      {/* 运行状态与接口配置并排：两者都是窄内容（状态块 + 若干设置行），
          在宽屏上各占一列比上下堆叠更省纵向空间，也把横向空间用起来。 */}
      <div className="grid min-w-0 gap-6 xl:grid-cols-2">
        <Section title="运行状态" description="账号池状态每 5 秒自动刷新">
        {excludedAccounts.length > 0 && (
          <div className="mx-4 mt-3 rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-900 sm:mx-5">
            <div className="flex items-center gap-1.5 font-medium">
              <AlertTriangle className="size-3.5" />
              {excludedAccounts.length} 个账号需重新登录，已排除出账号池
            </div>
            <div className="mt-1 leading-5 opacity-90">
              {excludedAccounts.map((a) => a.nickname || a.uid).join("、")}
              ：refresh token 已失效，继续使用只会让每次请求失败一次。请到「账号管理」页重新登录，
              恢复后会自动重新加入账号池。
            </div>
          </div>
        )}
        <div className="mx-4 grid grid-cols-2 gap-2 py-3 sm:mx-5 sm:grid-cols-4">
          <Stat
            label="服务"
            value={loading ? "…" : running ? (status?.reachable ? "运行中" : "已启动") : "未运行"}
            tone={running ? "ok" : "off"}
          />
          <Stat label="健康账号" value={pool?.healthy ?? "—"} tone={(pool?.healthy ?? 0) > 0 ? "ok" : "warn"} />
          <Stat label="冷却 / 禁用" value={`${pool?.cooling ?? 0} / ${pool?.disabled ?? 0}`} tone="warn" />
          <Stat label="粘性会话" value={pool?.sticky_sessions ?? 0} />
        </div>

        <Row>
          <div className="min-w-0">
            <div className="text-[13px]">OpenAI 兼容接口</div>
            <div className="mt-0.5 truncate font-mono text-[11px] text-muted-foreground">
              {endpoint || "—"}
            </div>
          </div>
          <div className="flex shrink-0 items-center gap-1.5">
            <Button
              variant="ghost"
              size="icon"
              className="size-7"
              onClick={() => void copyEndpoint()}
              disabled={!endpoint}
              aria-label="复制接口地址"
            >
              <Copy className="size-3.5" />
            </Button>
            {running ? (
              <>
                <Button
                  variant="outline"
                  size="sm"
                  className="h-7 gap-1.5 text-xs"
                  onClick={() => void run("restart", () => api.restartGateway())}
                  disabled={busy !== null}
                >
                  {busy === "restart" ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCw className="size-3.5" />}
                  重启
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  className="h-7 gap-1.5 text-xs"
                  onClick={() => void run("stop", () => api.stopGateway())}
                  disabled={busy !== null}
                >
                  {busy === "stop" ? <Loader2 className="size-3.5 animate-spin" /> : <Square className="size-3.5" />}
                  停止
                </Button>
              </>
            ) : (
              <Button
                size="sm"
                className="h-7 gap-1.5 text-xs"
                onClick={() => void run("start", () => api.startGateway(port))}
                disabled={busy !== null || !status?.exeFound || portState.tone === "bad"}
              >
                {busy === "start" ? <Loader2 className="size-3.5 animate-spin" /> : <Play className="size-3.5" />}
                启动网关
              </Button>
            )}
          </div>
        </Row>

        <Row>
          <div className="min-w-0">
            <div className="text-[13px]">账号同步</div>
            <div className="mt-0.5 text-[11px] text-muted-foreground">
              账号库 {status?.accountsInLibrary ?? "—"} 个账号 · 变更会自动同步
              {running ? "，网关运行中会自动重启以加载新账号" : ""}
            </div>
          </div>
          <Button
            variant="outline"
            size="sm"
            className="h-7 shrink-0 gap-1.5 text-xs"
            onClick={() => void run("sync", () => api.syncGatewayAccounts(true))}
            disabled={busy !== null}
          >
            {busy === "sync" ? <Loader2 className="size-3.5 animate-spin" /> : <Zap className="size-3.5" />}
            立即同步
          </Button>
        </Row>
        </Section>

        <Section title="接口配置">
        {/* 工作模式 */}
        <Row className="flex-col items-stretch gap-2 sm:flex-row sm:items-center">
          <div className="min-w-0">
            <div className="text-[13px]">工作模式</div>
            <div className="mt-0.5 text-[11px] text-muted-foreground">
              {mode === "balance"
                ? "全部账号按到期日分层自动均衡（点击即时生效）"
                : mode === "rotation"
                  ? "只用一个账号烧到不可用，再换按到期日排序的下一个（点击即时生效）"
                  : "只使用下方勾选的账号，池内仍自动均衡（点击即时生效）"}
            </div>
          </div>
          <div className="flex shrink-0 items-center gap-1.5">
            <Button
              variant={mode === "balance" ? "default" : "outline"}
              size="sm"
              className="h-8 gap-1.5 px-2.5 text-xs"
              onClick={() => void changeMode("balance")}
            >
              <Shuffle className="size-3.5" />
              自动
            </Button>
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant={mode === "rotation" ? "default" : "outline"}
                  size="sm"
                  className="h-8 gap-1.5 px-2.5 text-xs"
                  onClick={() => void changeMode("rotation")}
                >
                  <Recycle className="size-3.5" />
                  积分轮转
                </Button>
              </TooltipTrigger>
              <TooltipContent side="top" className="max-w-[280px]">
                单一模型 + 积分轮转：始终只用一个账号，把它烧到不可用
                （余额耗尽 / 被限流 / 熔断）才换下一个 ——
                换的是按积分到期日排序的下一个，仍然优先烧最快过期的额度。
              </TooltipContent>
            </Tooltip>
            <Button
              variant={mode === "manual" ? "default" : "outline"}
              size="sm"
              className="h-8 gap-1.5 px-2.5 text-xs"
              onClick={() => void changeMode("manual")}
            >
              <UserRound className="size-3.5" />
              手动
            </Button>
          </div>
        </Row>

        {/*
          手动模式：勾选参与账号池的账号（可多选）。
          用勾选列表而不是单选下拉：多个账号组成池子、内部仍自动均衡，
          这是最常用的用法；勾一个即等价于旧的「指定账号」。
        */}
        {mode === "manual" ? (
          <Row className="flex-col items-stretch gap-2">
            <div className="flex min-w-0 items-center justify-between gap-3">
              <div className="min-w-0">
                <Label className="text-[13px] font-normal">参与账号池的账号</Label>
                <div className="mt-0.5 text-[11px] text-muted-foreground">
                  {availableAccounts.length
                    ? `已勾选 ${manualUids.length} / 可选 ${availableAccounts.length} 个；池内仍按到期日分层自动均衡`
                    : "账号库为空"}
                </div>
              </div>
              {availableAccounts.length > 0 && (
                <div className="flex shrink-0 items-center gap-1.5">
                  <Button
                    variant="outline"
                    size="sm"
                    className="h-7 px-2 text-xs"
                    onClick={() => void changeManualUids(availableAccounts.map((a) => a.uid))}
                  >
                    全选
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    className="h-7 px-2 text-xs"
                    onClick={() => void changeManualUids([])}
                  >
                    清空
                  </Button>
                </div>
              )}
            </div>
            {availableAccounts.length === 0 ? (
              <div className="rounded-lg border border-dashed px-3 py-4 text-center text-xs text-muted-foreground">
                账号库为空，请先在「账号管理」添加账号。
              </div>
            ) : (
              <div className="max-h-56 min-w-0 overflow-y-auto rounded-lg border">
                {availableAccounts.map((a) => (
                  <ManualAccountOption
                    key={a.uid}
                    account={a}
                    checked={manualUids.includes(a.uid)}
                    onToggle={() => void toggleManualUid(a.uid)}
                    meta={accountByUid.get(a.uid)}
                    credit={creditByUid.get(a.uid)}
                    creditLoading={creditLoadingByUid.get(a.uid)}
                  />
                ))}
              </div>
            )}
            {manualUids.length === 0 ? (
              <p className="text-[11px] text-amber-600 dark:text-amber-500">
                至少勾选一个账号，否则网关启动时会因账号池为空而失败。
              </p>
            ) : null}
          </Row>
        ) : null}

        {/*
          放行模型（限制使用的模型）：**三个工作模式都有**，多选，默认全部。

          为什么不跟工作模式绑定（此前只在积分轮转下）：它限制的是「网关放行
          哪些模型」，与「用哪些账号」是正交的两件事 —— 自动/手动模式下同样
          可能只想放行某几个模型（例如只想让客户端用国内模型）。

          措辞刻意用「放行模型」而不是「模型筛选」：后者已经被 Token 用量区块
          那个**只改展示、不写配置**的控件占用，两者语义完全不同（那个不写盘，
          这个会让网关真的 400 拒绝）。标题 + 说明里都点明「网关会拒绝其它模型」，
          用户才不会以为这只是换个看法。
        */}
        <Row className="flex-col items-stretch gap-2 sm:flex-row sm:items-center">
          <div className="min-w-0">
            <Label htmlFor="gw-allowed-models" className="text-[13px] font-normal">
              放行模型
            </Label>
            <div className="mt-0.5 text-[11px] text-muted-foreground">
              {allowedModels.length === 0
                ? "全部放行 —— 客户端可调用任意模型"
                : `只放行 ${allowedModels.join("、")}，其它模型会被网关拒绝（400）`}
            </div>
          </div>
          <AllowedModelPicker
            options={modelOptions}
            selected={allowedModels}
            onChange={(next) => void changeAllowedModels(next)}
            onRefresh={() => void loadModels()}
            refreshing={modelsLoading}
          />
        </Row>

        <Row className="flex-col items-stretch gap-2 sm:flex-row sm:items-center">
          <div className="min-w-0">
            <Label htmlFor="gw-port" className="text-[13px] font-normal">
              服务端口
            </Label>
            <div className="mt-0.5 flex items-center gap-1.5 text-[11px]">
              <span
                className={cn(
                  "size-1.5 shrink-0 rounded-full",
                  portState.tone === "ok" && "bg-emerald-500",
                  portState.tone === "warn" && "bg-amber-500",
                  portState.tone === "bad" && "bg-destructive",
                  portState.tone === "muted" && "bg-muted-foreground/40",
                )}
                aria-hidden="true"
              />
              <span
                className={cn(
                  "text-muted-foreground",
                  portState.tone === "bad" && "text-destructive",
                  portState.tone === "warn" && "text-amber-600 dark:text-amber-400",
                )}
              >
                {portState.label}
              </span>
            </div>
          </div>
          <div className="flex shrink-0 items-center gap-1.5">
            <Input
              id="gw-port"
              type="number"
              min={1}
              max={65535}
              value={Number.isFinite(port) ? port : ""}
              onChange={(e) => {
                dirtyRef.current = true;
                setPort(Number(e.target.value));
              }}
              placeholder="7863"
              className="h-8 w-24 font-mono text-xs"
              aria-invalid={portState.tone === "bad"}
            />
            <Button
              variant="outline"
              size="sm"
              className="h-8 gap-1.5 px-2 text-xs"
              onClick={() => void pickFreePort()}
              disabled={checkingPort}
              title="自动挑一个空闲端口"
            >
              {checkingPort ? (
                <Loader2 className="size-3.5 animate-spin" />
              ) : (
                <Wand2 className="size-3.5" />
              )}
              自动
            </Button>
            {portCheck?.suggest && !portCheck.available ? (
              <Button
                variant="outline"
                size="sm"
                className="h-8 px-2 text-xs"
                onClick={() => setPort(portCheck.suggest as number)}
              >
                用 {portCheck.suggest}
              </Button>
            ) : null}
            {/*
              端口被占用时提供「结束占用进程」：用户看到「已被占用」的下一步
              必然是「谁占着？能不能关掉？」，不给入口就只能自己去翻任务管理器。
              仅在确实被占用、且不是本网关自己在用时显示（后者应走「停止网关」）。
            */}
            {portCheck && !portCheck.available && !portCheck.inUseByGateway ? (
              <Button
                variant="outline"
                size="sm"
                className="h-8 gap-1.5 px-2 text-xs text-destructive hover:text-destructive"
                onClick={() => void openKillDialog(portCheck)}
                disabled={busy !== null}
                title="结束占用该端口的进程，然后由本网关接管"
              >
                <Skull className="size-3.5" />
                结束占用进程
              </Button>
            ) : null}
          </div>
        </Row>
        <Row>
          <div className="min-w-0">
            <Label htmlFor="gw-key" className="text-[13px] font-normal">
              API Key
            </Label>
            <div className="mt-0.5 text-[11px] text-muted-foreground">留空不鉴权；公网部署务必设置</div>
          </div>
          <Input
            id="gw-key"
            value={apiKey}
            onChange={(e) => {
              dirtyRef.current = true;
              setApiKey(e.target.value);
            }}
            placeholder="sk-..."
            className="h-8 w-44 shrink-0 font-mono text-xs"
          />
        </Row>
        <Row>
          <div className="min-w-0">
            <div className="text-[13px]">随 App 启动</div>
            <div className="mt-0.5 text-[11px] text-muted-foreground">打开本应用时自动启动网关</div>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            <Switch checked={autoStart} onCheckedChange={(v) => void changeAutoStart(v)} />
            <Button
              variant="outline"
              size="sm"
              className="h-7 gap-1.5 text-xs"
              onClick={() =>
                // 保存按钮只管文本字段（端口 / API Key）：
                // 模式与自动启动是开关型，已在点击时即时保存，不在这里重复提交。
                void run("save", async () => {
                  dirtyRef.current = false;
                  await api.saveGatewayConfig({ port, api_key: apiKey });
                  toast.success("配置已保存");
                })
              }
              disabled={busy !== null}
            >
              {busy === "save" ? <Loader2 className="size-3.5 animate-spin" /> : <Save className="size-3.5" />}
              保存
            </Button>
          </div>
        </Row>
        </Section>
      </div>

      <Section title="账号池" description={pool ? `网关侧运行态（redis=${pool.redis_mode ?? "noop"}）` : "启动网关后可见"}>
        {poolAccounts.length > 0 ? (
          <>
            {/* 全局说明只写一次。此前每个账号都重复渲染「模型冷却只影响上述模型…」，
                14 个账号就是 14 遍相同文案，是纯噪音。 */}
            {poolModelCooledCount > 0 ? (
              <div className="mx-4 mt-2 rounded-lg bg-sky-500/10 px-3 py-2 text-[11px] leading-5 text-sky-800 dark:text-sky-300 sm:mx-5">
                有 {poolModelCooledCount} 个账号处于<strong className="font-medium">模型冷却</strong>
                ：仅下列标出的模型暂不可用，这些账号的<strong className="font-medium">其他模型仍会正常参与负载均衡</strong>，
                冷却到期后自动恢复。
              </div>
            ) : null}
            {/* 排队汇总：回答「N 个账号为什么没有流量」。
                这些账号本身健康、积分充足，只是到期档位更晚 —— 不说明的话，
                用户会以为账号丢了或没生效。 */}
            {poolQueuedCount > 0 ? (
              <div className="mx-4 mt-2 rounded-lg bg-muted/50 px-3 py-2 text-[11px] leading-5 text-muted-foreground sm:mx-5">
                另有 {poolQueuedCount} 个账号<strong className="font-medium">排队中</strong>
                ：它们健康且积分充足，只是到期档位晚于当前正在使用的档位。
                分层选号会优先消耗最快过期的额度，前面档位用尽或冷却后它们会自动承接流量。
              </div>
            ) : null}
            {/* 表格的一句引导：行可以展开。
                不说明的话没人会去点一行看起来纯展示的表 —— 这个交互就白做了。 */}
            <div className="mx-4 mt-2 text-[11px] text-muted-foreground/80 sm:mx-5">
              点击行首的箭头可展开<strong className="font-normal text-foreground/80">该账号的模型冷却 / 运行态 / 积分详情</strong>（可同时展开多行）；
              点表头可切换排序，默认按消耗降序。
            </div>

            {/* 账号池**恒为表格**（所有者本轮的形态决定：卡片 → 表格 + 二级展开）。
                页头那个「布局切换」已按所有者要求归位到 **Token 用量**区块，
                且**只切那一块** —— 因此这里不再有「旧版卡片 / 新版表格」的分叉：
                账号池的形态是唯一的，切换按钮不再影响它。 */}
            <div className="p-3 sm:p-4">
                {/* 工具栏：日期筛选（统一控制三个消耗列）+ 全部展开 / 全部收起。 */}
                <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
                  <div className="flex min-w-0 flex-wrap items-center gap-2">
                    <span className="text-[13px] font-medium">账号池</span>
                    <span className="text-[11.5px] tabular-nums text-muted-foreground">
                      {poolAccounts.length} 个 ·{" "}
                      <span className="text-primary">{pool?.healthy ?? poolAccounts.length} 可用</span>
                      {poolModelCooledCount > 0 ? ` / ${poolModelCooledCount} 模型限流` : ""}
                      {pool?.cooling ? ` / ${pool.cooling} 冷却` : ""}
                      {poolQueuedCount > 0 ? ` / ${poolQueuedCount} 排队` : ""}
                    </span>
                  </div>
                  <div className="flex flex-wrap items-center gap-1.5">
                    {/* 日期筛选：与旧版口径完全一致（USAGE_RANGE_OPTIONS 同一份定义），
                        统一控制「消耗 Token / 调用 / 消耗积分」三列。 */}
                    {USAGE_RANGE_OPTIONS.map((option) => (
                      <Button
                        key={option.key}
                        variant={usageRange === option.key ? "default" : "outline"}
                        size="sm"
                        className="h-7 px-2 text-xs"
                        onClick={() => setUsageRange(option.key)}
                        title={`三个消耗列（消耗 Token / 调用 / 消耗积分）都按「${option.label}」统计`}
                      >
                        {option.label}
                      </Button>
                    ))}
                    <span className="mx-0.5 h-4 w-px bg-border" aria-hidden="true" />
                    <Button
                      variant="outline"
                      size="sm"
                      className="h-7 gap-1 px-2 text-xs"
                      data-slot="pool-expand-all"
                      // 已经全展开 ⇒ 收起全部；否则展开全部。
                      // 常驻一个按钮（而不是展开时换成另一个按钮）是为了位置稳定：
                      // 它在两次点击之间不移动，用户不必重新找它。
                      onClick={() =>
                        setPoolExpandedUids(poolAllExpanded ? [] : poolAccounts.map((acc) => acc.uid))
                      }
                      aria-label={poolAllExpanded ? "全部收起账号详情" : "全部展开账号详情"}
                    >
                      {poolAllExpanded ? (
                        <>
                          <ChevronDown className="size-3.5" aria-hidden="true" />
                          全部收起
                        </>
                      ) : (
                        <>
                          <ChevronRight className="size-3.5" aria-hidden="true" />
                          全部展开
                        </>
                      )}
                    </Button>
                  </div>
                </div>

                {/* 横向滚动容器：列多（9 列）而窄屏一定会溢出 —— 不给容器加
                    overflow-x-auto 的话，溢出的列会被 Card 的 overflow-hidden **裁掉**，
                    表现为「余额和到期档位莫名其妙不见了」。
                    关键列（账号 / 状态 / 消耗）都排在前面，窄屏下先看到的正是它们。 */}
                <div className="overflow-x-auto rounded-xl border border-border/60">
                  <table data-slot="pool-table" className="w-full min-w-[860px] border-collapse text-[12px]">
                    <thead>
                      <tr className="border-b border-border/60 bg-muted/55">
                        <th scope="col" className="w-7 px-1.5 py-1.5" aria-label="展开" />
                        <th
                          scope="col"
                          className="w-8 px-1.5 py-1.5 text-left text-[11px] font-medium text-muted-foreground"
                          title="消耗排名（第 1 名 = 本范围内消耗最高；它跟着消耗列走，不是行号，也不可排序）"
                        >
                          #
                        </th>
                        <PoolSortHeader label="账号" sortKey="account" activeKey={poolSort.key} dir={poolSort.dir} onToggle={togglePoolSort} />
                        <PoolSortHeader
                          label="状态"
                          sortKey="state"
                          activeKey={poolSort.key}
                          dir={poolSort.dir}
                          onToggle={togglePoolSort}
                          title="按可用性排序：健康 → 排队 → 仅模型限流 → 账号冷却 → 已禁用"
                        />
                        <PoolSortHeader
                          label="消耗 Token"
                          sortKey="tokens"
                          align="right"
                          activeKey={poolSort.key}
                          dir={poolSort.dir}
                          onToggle={togglePoolSort}
                          title={`按「${usageRangeLabel}」的 Token 消耗排序（默认降序）`}
                        />
                        <PoolSortHeader
                          label="调用"
                          sortKey="records"
                          align="right"
                          activeKey={poolSort.key}
                          dir={poolSort.dir}
                          onToggle={togglePoolSort}
                          title={`按「${usageRangeLabel}」的成功调用次数排序`}
                        />
                        <PoolSortHeader
                          label="消耗积分"
                          sortKey="credits"
                          align="right"
                          activeKey={poolSort.key}
                          dir={poolSort.dir}
                          onToggle={togglePoolSort}
                          title="按账号库积分记录的消耗排序（「全部」范围给不出可信值，恒排最后）"
                        />
                        <PoolSortHeader
                          label="积分余额"
                          sortKey="balance"
                          align="right"
                          activeKey={poolSort.key}
                          dir={poolSort.dir}
                          onToggle={togglePoolSort}
                          title="按网关侧记录的最新积分余额排序（看谁快用完）"
                        />
                        <PoolSortHeader
                          label="到期档位"
                          sortKey="expiry"
                          activeKey={poolSort.key}
                          dir={poolSort.dir}
                          onToggle={togglePoolSort}
                          title="按最近到期积分档位排序；无档位（到期未知）恒排最后"
                        />
                      </tr>
                    </thead>
                    <tbody>
                      {poolSortedRows.map((row) => (
                        <PoolAccountRow
                          key={row.acc.uid}
                          metrics={row}
                          expanded={poolExpandedSet.has(row.acc.uid)}
                          onToggle={() => togglePoolExpanded(row.acc.uid)}
                          detailId={`pool-detail-${row.acc.uid}`}
                          usageRangeLabel={usageRangeLabel}
                          creditStatsLoaded={Boolean(creditStats)}
                          stickySessions={pool?.sticky_sessions ?? 0}
                        />
                      ))}
                    </tbody>
                  </table>
                </div>
            </div>
          </>
        ) : (
          <Row className="justify-center">
            <div className="flex items-center gap-2 py-4 text-xs text-muted-foreground">
              {running ? (
                <>
                  <Activity className="size-3.5" />
                  账号池为空，请先同步账号并重启网关
                </>
              ) : (
                <>
                  <CheckCircle2 className="size-3.5" />
                  网关未运行
                </>
              )}
            </div>
          </Row>
        )}
      </Section>

      {/* Token 用量区块：**新旧两版完全共用**。
          所有者本轮明确否掉了「新版另做一套布局」的做法（原话：「默认跟旧版的
          一样，只不过左侧可以通过点击账号的方式来显示…模型 token 信息」），
          因此这里不再按 layout 分叉 —— 新版唯一的差异是账号池那边多出的
          「表格 + 二级展开」，它就在账号池区块内就地展开，不影响本区块。 */}
      <Section
        title="Token 用量"
        description="经网关成功请求的上游用量，按模型 / 账号 / 日期聚合（网关重启后保留）"
      >
        <Row>
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
              {/*
                「统计范围」后面接上**当前口径**（今日 · 账号X）。
                草稿里所有者确认过这一条：只写「今日」时，用户看到汇总卡变小会以为是
                「今天量少」，而不会想到「其实还叠加了账号口径」。把两项条件并排写出
                （「统计范围 · 今日 · 账号 <uid 前 8 位>」），「为什么数字是这个」就不必再猜。
              */}
              <span className="text-[13px]">
                统计范围
                <span className="text-muted-foreground">
                  {" · "}
                  {usageRangeLabel}
                  {usageScopeAccount
                    ? ` · 账号 ${usageNickname.get(usageScopeAccount) ?? usageScopeAccount.slice(0, 8)}`
                    : usageScopeModel
                      ? ` · 模型 ${usageScopeModel}`
                      : ""}
                </span>
              </span>
              {/* 上次更新时间：让用户知道看到的数据有多新，不必反复手点刷新。
                  相对时间由每秒 tick 驱动；悬停可见精确时刻。 */}
              {usageUpdatedAt ? (
                <span
                  className="text-[11px] tabular-nums text-muted-foreground"
                  title={`上次更新：${new Date(usageUpdatedAt).toLocaleString("zh-CN")}`}
                >
                  上次更新 {formatRelativeTime(nowTick - usageUpdatedAt)}
                  {usageLoading ? " · 更新中…" : ""}
                </span>
              ) : null}
            </div>
            {/*
              这一行也不再是「网关总量」，而是**当前口径下**的合计 —— 否则它会与紧下方
              的汇总卡自相矛盾（一个按全局算、一个按账号算）。口径换了这句话却不变，
              是本区块最容易骗到人的地方。
            */}
            <div className="mt-0.5 text-[11px] tabular-nums text-muted-foreground">
              {usageSummaryDisplay
                ? `共 ${exactTokenFormatter.format(usageSummaryDisplay.records)} 次调用 · 合计 ${exactTokenFormatter.format(usageSummaryDisplay.total)} tokens${
                    usageCrossFilter ? "（当前口径）" : ""
                  }`
                : "等待网关数据"}
            </div>
          </div>
          <div className="flex shrink-0 items-center gap-1.5">
            {/* 模型筛选：默认「全部」、可多选，**三个工作模式下都在**。
                与「放行模型」（allowed_model）的分工见 ModelFilter 的说明 ——
                那个是服务端放行限制（写进配置、影响客户端能否调用），
                这个是本页只读展示的收窄，不碰任何配置。 */}
            <ModelFilter
              options={modelFilterOptions}
              selected={modelFilter}
              onChange={setModelFilter}
              totalModels={modelFilterOptions.length}
            />
            {USAGE_RANGE_OPTIONS.map((option) => (
              <Button
                key={option.key}
                variant={usageRange === option.key ? "default" : "outline"}
                size="sm"
                className="h-7 px-2 text-xs"
                onClick={() => setUsageRange(option.key)}
              >
                {option.label}
              </Button>
            ))}
            <Button
              variant="ghost"
              size="icon"
              className="size-7"
              onClick={() => setUsageNonce((value) => value + 1)}
              disabled={usageLoading}
              aria-label="刷新用量"
            >
              <RefreshCw className={cn("size-3.5", usageLoading && "animate-spin")} />
            </Button>
            {/* 布局切换：**旧版 ↔ 新版 Token 用量**（所有者本轮明确纠正：上轮说的
                「新版」指的是 Token 用量新旧版，不是账号池）。

                为什么从页头搬到这里：它只切本区块，挂在页头会让用户以为整页都跟着
                换，进而以为账号池也有两套 —— 那正是本轮要纠正的误解。放在本区块头部
                与日期范围按钮同排，作用域不言自明。
                图标按钮 + tooltip 的范式不变（与账号页「紧凑/宽松」一致），页头因此
                不会变宽。状态仍持久化到同一个 localStorage 键，老用户的记忆不丢。 */}
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-7"
                  onClick={toggleLayout}
                  data-slot="usage-layout-toggle"
                  aria-label={layout === "classic" ? "切换到新版 Token 用量" : "切换到旧版 Token 用量"}
                >
                  {layout === "classic" ? <LayoutGrid className="size-3.5" /> : <Rows3 className="size-3.5" />}
                </Button>
              </TooltipTrigger>
              <TooltipContent side="left">
                {layout === "classic"
                  ? "新版 Token 用量：加「当前口径」条与列头筛选徽标，并就地解释「筛了但没变少」是正确结果（会记住选择）"
                  : "旧版 Token 用量：只有左右两栏 + 每日用量柱图（会记住选择）"}
              </TooltipContent>
            </Tooltip>
          </div>
        </Row>

        {/* 当前口径条：**常驻**（只要点了某一项就显示）。
            为什么必须常驻、而不是像旧实现那样"长度变了才显示"：旧的条件
            （`scoped.length !== all.length`）在「今日 + 单模型」下恰好不成立 ——
            而所有者正是在这个范围下点的账号。于是他点了之后**页面上一个字都没变**，
            得出「功能没生效」的结论。反馈不能依赖"数据恰好变少了"这种偶然条件。
            口径条紧贴汇总卡上方：汇总卡已换成该账号的数字（所有者选的「跟着变」），
            这里明说口径，避免用户以为**总量掉了**。 */}
        {usageCrossFilter && !usageCrossDegraded ? (
          <div
            data-slot="usage-scope-bar"
            className="mx-4 mt-2 flex flex-wrap items-center gap-x-2 gap-y-1 rounded-lg border border-primary/30 bg-primary/5 px-3 py-1.5 text-[11px] leading-5 sm:mx-5"
          >
            <span className="font-medium text-primary" data-slot="usage-scope-label">
              当前口径：
              {usageScopeAccount
                ? `账号 ${usageNickname.get(usageScopeAccount) ?? usageScopeAccount.slice(0, 8)}`
                : `模型 ${usageScopeModel}`}
            </span>
            <span className="text-muted-foreground">
              顶部汇总与左右列表<strong className="font-medium">都只统计这一项</strong>
              {usageScopeAccount ? "在该范围内的用量" : "在各账号上的用量"}，
              不是网关总量
            </span>
            <button
              type="button"
              className="ml-auto shrink-0 cursor-pointer font-medium text-primary underline-offset-2 hover:underline"
              onClick={() => setUsageCrossFilter(null)}
              data-slot="usage-scope-reset"
            >
              ✕ 恢复全部口径
            </button>
          </div>
        ) : null}

        {/* 筛选生效的说明：数字收窄了却不说清楚，用户会以为数据丢了。 */}
        {modelFilterActive ? (
          <div className="mx-4 mt-2 rounded-lg bg-muted/50 px-3 py-2 text-[11px] leading-5 text-muted-foreground sm:mx-5">
            已筛选 <strong className="font-medium text-foreground/80">{modelFilter.length}</strong> 个模型
            （{modelFilter.join("、")}）：下方「按模型 / 按账号」与账号明细只统计这些模型；
            上方的汇总数字与「每日用量」仍是当前口径下的全量，不随模型筛选变化。
          </div>
        ) : null}

        {/* 交叉筛选的提示条 + 一键清除。
            没有它，用户点完账号看到左侧少了一半模型，只会以为数据丢了 —— 他不会
            想到是自己点出来的。这里必须同时给出「点了什么」「另一侧剩几个」，
            并在两边都为 0 时点明是哪一级掐掉的（两级叠加最常见的困惑）。
            用 primary 系而不是 muted：它要和上面那条 ModelFilter 说明**区分开** ——
            两条同时出现时，用户得能一眼看出「哪条是我刚点出来的」。

            **新旧版的差异就在这一条**（所有者已确认：布局切换指的是 Token 用量新旧版）：
              - 新版：正常收窄时**不再重复**这条远处的提示条 —— 它的信息已被列头徽标
                （「已按账号筛选 ✕」，就在列表标题上）与顶部口径条就地承担，
                留着只是同一句话在页面上出现两遍；
              - 旧版：保持既有观感（这条提示条就是旧版的反馈形态），一条不动。

            但**下面这几种状态一律两版都显示**，因为这条提示条是那些信息**唯一**的
            载体，新版把它去掉就会真的丢信息：
              - 网关没给 accountModels（`usageCrossDegraded`）：无法筛选，必须说明；
              - 明细完全缺失（`usageCrossItemBlank`）：要说「拿不到明细」而不是
                「没用过」，这是本次最不能犯的假结论；
              - 明细部分缺失（`usageCrossItemPartial`）：要提示列表可能不全；
              - 结果为空的解释（`emptyReason`）：两级叠加筛空时必须点明是哪一级掐的。 */}
        {usageCrossNote &&
        (layout === "classic" ||
          usageCrossDegraded ||
          usageCrossItemBlank ||
          usageCrossItemPartial ||
          Boolean(usageCrossNote.emptyReason)) ? (
          <div
            data-slot="usage-cross-filter-note"
            className="mx-4 mt-2 flex flex-wrap items-center justify-between gap-2 rounded-lg border border-primary/30 bg-primary/5 px-3 py-2 text-[11px] leading-5 text-muted-foreground sm:mx-5"
          >
            <div className="min-w-0">
              <span>{usageCrossNote.text}</span>
              {usageCrossNote.emptyReason ? (
                <span className="ml-1 text-foreground/80">{usageCrossNote.emptyReason}</span>
              ) : null}
              <span className={cn("ml-1", USAGE_STRONG_TEXT)}>{usageCrossNote.hint}</span>
            </div>
            <Button
              variant="outline"
              size="sm"
              className="h-6 shrink-0 px-2 text-[11px]"
              onClick={() => setUsageCrossFilter(null)}
              data-slot="usage-cross-filter-clear"
            >
              清除筛选
            </Button>
          </div>
        ) : null}

        {(() => {
          // 判定顺序与占位文案统一走 usageUnavailableText，避免「新版一套、
          // 旧版另一套」的措辞漂移（例如把「网关没开」说成「开了但坏了」）。
          const unavailable = usageUnavailableText({ usageLoading, snapshot: usageSnapshot, running, usage });
          if (unavailable) return <UsagePlaceholder text={unavailable} />;
          if (!usageSnapshot || !usageSummary) {
            return <UsagePlaceholder text={`无法读取网关用量${usage?.error ? `：${usage.error}` : ""}`} />;
          }
          // 换口径后的汇总。绑成局部常量而不是就地读 `usageSummaryDisplay`：后者
          // 类型上带 null（数据未加载时就是 null），而这里刚判过 `usageSummary` 存在
          // —— 用局部常量能让 TS 一并收窄（否则要写八处 `?.`），也把「这一段渲染的
          // 就是当前口径下的数字」这件事写在最显眼处。
          const scoped = usageSummaryDisplay ?? usageSummary;
          return (
          <>
            <div className="mx-4 grid grid-cols-2 gap-2 py-3 sm:mx-5 sm:grid-cols-4">
              {/*
                汇总卡用换口径后的 `scoped`：点了账号/模型后**跟着换口径**
                （所有者明确选的「跟着变」）。不换的话会出现「上面写着网关总量
                1238.3M、下面写着该账号 199.7M」两个数字并存，用户无法判断哪个是
                哪个 —— 那正是本轮抱怨问题的镜像。换口径的安全性由紧上方的常驻
                口径条兜底：它明说「只统计这一项，不是网关总量」，故不会误以为
                总量掉了。

                标签也随口径变（「总 Token」→「该账号 Token」）：只换数字不换标签，
                用户仍会把它读成网关总量。
              */}
              <Stat
                label={usageScopeAccount ? "该账号 Token" : usageScopeModel ? "该模型 Token" : "总 Token"}
                value={formatUsageCompact(scoped.total)}
                hint={exactTokenFormatter.format(scoped.total)}
              />
              <Stat
                label="输入"
                value={formatUsageCompact(scoped.input)}
                hint={
                  scoped.cacheHitRate != null
                    ? `缓存命中率 ${(scoped.cacheHitRate * 100).toFixed(1)}%`
                    : "无缓存读取数据"
                }
              />
              <Stat
                label="输出"
                value={formatUsageCompact(scoped.output)}
                hint={`缓存写入 ${formatUsageCompact(scoped.cacheWrite)}`}
              />
              <Stat
                label="调用次数"
                value={exactTokenFormatter.format(scoped.records)}
                hint="成功请求"
              />
            </div>

            {/* 左右两栏就是双向联动的两个入口：点右侧账号 → 左侧只剩该账号用过的
                模型；点左侧模型 → 右侧只剩用过该模型的账号。再点一次取消。

                左「按模型」在**前**、右「按账号」在**后**，与所有者描述的顺序
                （「左边是模型,右边是账号」）一致，栅格顺序不动。

                计数用 `usageModelsScoped` / `usageAccountsScoped`（第二级筛选后），
                因为此时用户看到的就是这几条 —— 用未筛选的数会与列表长度对不上，
                那正是「数字与内容矛盾」的经典困惑源。

                数值/条宽/meta 一律取 `usageModelsDisplay` / `usageAccountsDisplay`
                两条**换口径后**的派生数组（它们只在「另一侧被点选」时才与 Scoped 不同）：
                Scoped 决定「显示哪几行」，Display 决定「这几行写什么数」。两者分开是
                刻意的 —— 换口径不该影响"筛掉谁"，筛行也不该顺手改数值。 */}
            <div className="grid gap-4 border-t border-border/50 pb-2 pt-3 sm:grid-cols-2">
              <div className="min-w-0">
                <div className="px-4 text-[12px] font-medium text-muted-foreground sm:px-5">
                  <span className="flex flex-wrap items-center gap-x-1.5 gap-y-1">
                    <span>按模型</span>
                    {usageModelsDisplay.length > 0 ? (
                      <span className={cn("font-normal tabular-nums", USAGE_STRONG_TEXT)} data-slot="usage-model-count">
                        共 {usageModelsDisplay.length} 个
                        {usageCrossActive && usageModelsAll.length !== usageModelsDisplay.length
                          ? `（全部 ${usageModelsAll.length}）`
                          : ""}
                      </span>
                    ) : null}
                    {/*
                      「已按账号筛选 ✕」徽标：**始终**显示，只要**账号**被点选。

                      为什么去掉旧版那个 `scoped.length !== all.length` 条件：
                      在「今日 + 单模型」下该条件恒为假（筛完还是那 1 个模型），
                      于是**唯一**能提示「筛选已生效」的锚点恰好缺席 —— 这正是所有者
                      「看不出变化」的主因之一。反馈必须由「用户点了什么」决定，
                      而不能由「数据恰好变少了」这种偶然结果决定。

                      为什么条件必须是 `usageCrossAccount`（账号）而不是 `usageCrossModel`：
                      左栏是「按模型」，**能收窄它的只有"点了账号"**；点模型收窄的是
                      右栏。这里最初误写成 `usageCrossModel`，结果是「点账号后左侧
                      徽标不出现」—— 与旧版那个 bug 症状完全相同（都是"用户点了、
                      但该出现的反馈没出现"），只是换了个条件形式。
                      这是被 verify-usage-crossfilter.cjs 的「列头徽标始终显示」当场测出来的。

                      徽标本身就是清除按钮（点它即恢复全部口径），与顶部口径条上的
                      「恢复全部口径」是同一状态的两条退路 —— 用户视线在列表里时
                      不必再往上找。
                    */}
                    {usageCrossAccount ? (
                      <Button
                        variant="default"
                        size="sm"
                        className="h-5 cursor-pointer gap-0.5 px-1.5 text-[10px] font-medium"
                        onClick={() => setUsageCrossFilter(null)}
                        data-slot="usage-model-filter-badge"
                        title="清除「按账号筛选」，恢复全部账号的口径"
                      >
                        已按账号筛选
                        <X className="size-2.5" strokeWidth={3} />
                      </Button>
                    ) : null}
                  </span>
                </div>
                {/*
                  高度 288px（max-h-72）→ 420px（max-h-[420px]）。
                  为什么必须改：288px 只装得下 4 行，所有者真实数据「全部」范围下
                  账号有 18 个 —— 第 5 行往后在**未滚动时用真实鼠标点不到**
                  （`elementFromPoint` 命中的是别的元素）。这不是"看不出变化"，
                  是**根本点不到**，比前者更严重。
                  底部另给一行滚动提示，让"还有更多"这件事本身可见。
                */}
                <div className="mt-1 max-h-[420px] overflow-y-auto" data-slot="usage-model-list">
                  {usageModelsDisplay.length > 0 ? (
                    usageModelsDisplay.map((model) => (
                      <UsageBarRow
                        key={model.key}
                        dataSlot="usage-model-row"
                        valueSlot="usage-model-value"
                        label={model.key}
                        value={model.total}
                        max={usageMaxModelScoped}
                        meta={`${exactTokenFormatter.format(model.records)} 次调用 · 输入 ${formatUsageCompact(model.input)} / 输出 ${formatUsageCompact(model.output)}`}
                        selected={usageCrossModel === model.key}
                        onToggle={() => toggleUsageModelFilter(model.key)}
                      />
                    ))
                  ) : (
                    // 空结果必须给可读说明，且要说清**为什么**空 —— 两级筛选叠加后
                    // 「什么都没有」与「筛没了」是两件事，后者要告诉用户放宽哪一级。
                    // 另外「确实没用过」与「网关没记明细」也必须分开（见 usageCrossItemBlank）。
                    <div className="px-4 py-2 text-xs text-muted-foreground sm:px-5">
                      {usageCrossItemBlank
                        ? "网关没有记录该账号的「账号 × 模型」明细（用量发生在开始记录之前）"
                        : usageCrossActive && !usageCrossDegraded
                          ? usageCrossAccount
                            ? `该账号没有用过任何模型${modelFilterActive ? "（在当前模型筛选范围内）" : ""}`
                            : "该范围内暂无数据"
                          : "该范围内暂无数据"}
                    </div>
                  )}
                </div>
                {/* 滚动提示：只在真的装不下时才说「可滚动」，否则是噪音。 */}
                {usageModelsDisplay.length > USAGE_LIST_VISIBLE_ROWS ? (
                  <div
                    className={cn("mt-1 text-center text-[10px]", USAGE_STRONG_TEXT)}
                    data-slot="usage-model-scroll-hint"
                  >
                    共 {usageModelsDisplay.length} 个，列表可滚动 · 已按用量降序
                  </div>
                ) : null}
                {/*
                  「换口径已生效」的就地说明。
                  为什么必须有：即便数字已经真的换了（这是本轮的核心修复），当该账号
                  在范围内**只用一个模型**时，列表**行数**仍不变（1 行 → 1 行）。
                  所有者上一轮正是看着"行数没变"判定"没生效"的 —— 数字变了但行数没变
                  时，用户仍可能怀疑。这里把"变了什么"直接写出来（原全局 X → 现 Y），
                  把最后一点"是不是没反应"的疑虑当场解释掉。
                  只在**两版都出现**（不限 layout）：这是正确性说明，不是观感增强。
                */}
                {usageCrossAccount && usageModelsDisplay.length > 0 ? (() => {
                  const shown = usageModelsDisplay[0];
                  const global = usageModelsAll.find((m) => m.key === shown.key);
                  if (!global || global.total === shown.total) return null;
                  return (
                    <div
                      className="mx-4 mt-1.5 rounded-lg bg-info/5 px-2.5 py-1.5 text-[10.5px] leading-5 sm:mx-5"
                      data-slot="usage-rebased-hint"
                    >
                      ✓ 数字已换成<strong className="font-medium">该账号在此模型上的</strong>用量
                      （原全局 {formatUsageCompact(global.total)} → 现 {formatUsageCompact(shown.total)}），
                      小字与条宽也按该账号重算。
                    </div>
                  );
                })() : null}
              </div>
              <div className="min-w-0">
                <div className="px-4 text-[12px] font-medium text-muted-foreground sm:px-5">
                  <span className="flex flex-wrap items-center gap-x-1.5 gap-y-1">
                    <span>按账号</span>
                    {usageAccountsDisplay.length > 0 ? (
                      <span className={cn("font-normal tabular-nums", USAGE_STRONG_TEXT)} data-slot="usage-account-count">
                        共 {usageAccountsDisplay.length} 个
                        {usageCrossActive && usageAccountsAll.length !== usageAccountsDisplay.length
                          ? `（全部 ${usageAccountsAll.length}）`
                          : ""}
                      </span>
                    ) : null}
                    {/* 反方向的同一枚徽标：点了模型后右侧「按账号」也要明说口径。 */}
                    {usageCrossModel ? (
                      <Button
                        variant="default"
                        size="sm"
                        className="h-5 cursor-pointer gap-0.5 px-1.5 text-[10px] font-medium"
                        onClick={() => setUsageCrossFilter(null)}
                        data-slot="usage-account-filter-badge"
                        title="清除「按模型筛选」，恢复全部模型的口径"
                      >
                        已按模型筛选
                        <X className="size-2.5" strokeWidth={3} />
                      </Button>
                    ) : null}
                  </span>
                </div>
                {/*
                  这里**不能**截断成前 5 个：账号池的均衡效果正是靠这个列表观察的。
                  原先写死 slice(0, 5)，导致 8 个账号都在正常轮转、界面却只显示 5 个，
                  用户据此误判「负载均衡只用到 5 个账号」。
                  改为全量展示并加滚动上限（高度受限，避免账号多时把页面撑得过长）。
                */}
                <div className="mt-1 max-h-[420px] overflow-y-auto" data-slot="usage-account-list">
                  {usageAccountsDisplay.length > 0 ? (
                    usageAccountsDisplay.map((account) => (
                      <UsageBarRow
                        key={account.key}
                        dataSlot="usage-account-row"
                        valueSlot="usage-account-value"
                        label={usageNickname.get(account.key) ?? `${account.key.slice(0, 8)}…`}
                        value={account.total}
                        max={usageMaxAccountScoped}
                        meta={`${exactTokenFormatter.format(account.records)} 次调用 · ${account.key.slice(0, 8)}`}
                        selected={usageCrossAccount === account.key}
                        onToggle={() => toggleUsageAccountFilter(account.key)}
                      />
                    ))
                  ) : (
                    <div className="px-4 py-2 text-xs text-muted-foreground sm:px-5">
                      {usageCrossItemBlank
                        ? "网关没有记录该模型的「账号 × 模型」明细（用量发生在开始记录之前）"
                        : usageCrossActive && !usageCrossDegraded
                          ? usageCrossModel
                            ? `没有账号用过「${usageCrossModel}」${modelFilterActive ? "（在当前模型筛选范围内）" : ""}`
                            : "该范围内暂无数据"
                          : "该范围内暂无数据"}
                    </div>
                  )}
                </div>
                {usageAccountsDisplay.length > USAGE_LIST_VISIBLE_ROWS ? (
                  <div
                    className={cn("mt-1 text-center text-[10px]", USAGE_STRONG_TEXT)}
                    data-slot="usage-account-scroll-hint"
                  >
                    共 {usageAccountsDisplay.length} 个，列表可滚动 · 已按用量降序
                  </div>
                ) : null}
              </div>
            </div>

            {usageDaily.length > 0 ? (
              <div className="border-t border-border/50 px-4 pb-3 pt-3 sm:px-5">
                <div className="text-[12px] font-medium text-muted-foreground">每日用量</div>
                <div className="mt-2 flex h-16 items-end gap-1">
                  {usageDaily.slice(-30).map((day) => (
                    <div
                      key={day.key}
                      className="flex h-full flex-1 items-end"
                      title={`${day.key} · ${exactTokenFormatter.format(day.total)} tokens · ${day.records} 次调用`}
                    >
                      <div
                        className="w-full rounded-t-[3px] bg-primary/60 transition-colors hover:bg-primary"
                        style={{ height: `${Math.max(4, Math.round((day.total / usageMaxDaily) * 100))}%` }}
                      />
                    </div>
                  ))}
                </div>
                <div className="mt-1 flex justify-between text-[10px] text-muted-foreground">
                  <span>{usageDaily[Math.max(0, usageDaily.length - 30)]?.key}</span>
                  <span>{usageDaily[usageDaily.length - 1]?.key}</span>
                </div>
              </div>
            ) : null}
          </>
          );
        })()}
      </Section>

      <Section title="客户端接入" description="把网关接入本机已安装的 AI 客户端，或按标准环境变量接入">
        <div className="space-y-4 p-4 sm:p-5">
          <div className="flex flex-wrap items-center justify-between gap-2.5 rounded-xl border border-primary/30 bg-primary/5 p-3 text-xs">
            <div className="flex items-center gap-2">
              <Bot className="size-4 text-primary shrink-0" />
              <span>现已提供独立的「智能体管理」页面，支持 12 类智能体的多模型多选与一键批量更新。</span>
            </div>
            <Button size="sm" variant="outline" className="h-7 text-xs font-medium" asChild>
              <Link to="/agents">前往智能体管理 →</Link>
            </Button>
          </div>

          <Separator className="my-1" />

          <div>
            <h3 className="text-[13px] font-medium leading-5">手动环境变量配置</h3>
            <p className="mt-0.5 text-xs text-muted-foreground">
              如需在其他第三方工具、SDK 或自建服务中使用网关，可设置以下环境变量：
            </p>
            <div className="mt-2.5 space-y-2.5">
              <div>
                <div className="mb-1 text-[11px] font-medium text-muted-foreground">
                  OpenAI 兼容接口（/v1/chat/completions 与 /v1/models）
                </div>
                <pre className="overflow-x-auto rounded-lg bg-muted/50 px-3 py-2 text-[11px] leading-relaxed">
                  <code>{endpointHint || "OPENAI_BASE_URL=http://127.0.0.1:7863/v1"}
{`OPENAI_API_KEY=${apiKey || "<你的 api_key>"}`}</code>
                </pre>
              </div>
              <div>
                <div className="mb-1 text-[11px] font-medium text-muted-foreground">
                  Anthropic Messages 接口（Claude Code 与 Claude Desktop，/v1/messages）
                </div>
                <pre className="overflow-x-auto rounded-lg bg-muted/50 px-3 py-2 text-[11px] leading-relaxed">
                  <code>{`ANTHROPIC_BASE_URL=http://127.0.0.1:${port || 7863}
ANTHROPIC_AUTH_TOKEN=${apiKey || "<你的 api_key>"}`}</code>
                </pre>
              </div>
            </div>
            <p className="mt-2 text-[11px] text-muted-foreground">
              同时支持 <code className="font-mono">POST /v1/responses</code>（兼容新版 Codex CLI 0.146+），现有主流 AI 客户端均可零改造对接。
            </p>
          </div>
        </div>
      </Section>

      <Section title="诊断">
        <Row>
          <div className="min-w-0">
            <div className="text-[13px]">网关账号凭证目录</div>
            <div className="mt-0.5 break-all font-mono text-[11px] text-muted-foreground">
              {status?.authDir || "—"}
            </div>
          </div>
        </Row>
        <Row>
          <div className="min-w-0">
            <div className="flex items-center gap-2 text-[13px]">
              网关可执行文件
              <Badge variant="secondary" className="text-[10px]">
                {status?.exeSource === "embedded"
                  ? "内嵌"
                  : status?.exeSource === "env"
                    ? "环境变量"
                    : "外部文件"}
              </Badge>
            </div>
            <div className="mt-0.5 break-all font-mono text-[11px] text-muted-foreground">
              {status?.exePath || "未找到"}
            </div>
            {status?.exeSource === "embedded" ? (
              <div className="mt-0.5 text-[11px] text-muted-foreground">
                随主程序分发，首次使用自动释放到本机缓存
              </div>
            ) : null}
          </div>
          <Badge variant={status?.exeFound ? "secondary" : "destructive"} className="shrink-0 text-[10px]">
            {status?.exeFound ? "已就绪" : "缺失"}
          </Badge>
        </Row>
      </Section>
    </div>
    {/*
      端口占用确认对话框：杀进程不可逆，必须先展示「谁占着」再让用户决定。
      第三方进程（非本项目）给出更强的警告 —— 用户可能正在用那个程序。
    */}
    <Dialog open={killTarget !== null} onOpenChange={(open) => { if (!open) setKillTarget(null); }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>结束占用进程？</DialogTitle>
          <DialogDescription>
            端口 <span className="font-mono font-medium">{killTarget?.port}</span> 正被以下进程占用。
            结束它之后，本网关即可接管该端口。
          </DialogDescription>
        </DialogHeader>
        {killHolderLoading ? (
          <div className="flex items-center gap-2 rounded-lg border bg-muted/40 px-3 py-3 text-xs text-muted-foreground">
            <Loader2 className="size-3.5 animate-spin" />
            正在识别占用进程…
          </div>
        ) : killHolder ? (
          <div className="min-w-0 space-y-1.5 rounded-lg border bg-muted/40 px-3 py-2.5 text-xs">
            <div className="flex min-w-0 items-baseline gap-2">
              <span className="shrink-0 text-muted-foreground">进程</span>
              <span className="min-w-0 truncate font-medium">{killHolder.name}</span>
            </div>
            <div className="flex min-w-0 items-baseline gap-2">
              <span className="shrink-0 text-muted-foreground">PID</span>
              <span className="font-mono">{killHolder.pid}</span>
            </div>
            {killHolder.path ? (
              <div className="flex min-w-0 items-baseline gap-2">
                <span className="shrink-0 text-muted-foreground">路径</span>
                <span className="min-w-0 break-all font-mono text-[11px]">{killHolder.path}</span>
              </div>
            ) : null}
          </div>
        ) : (
          <p className="text-xs text-muted-foreground">
            无法识别占用该端口的进程（可能需要管理员权限）。结束操作可能失败。
          </p>
        )}
        {killHolder && !killHolder.ours ? (
          <p className="text-xs text-destructive">
            该进程不属于本程序，可能是你正在使用的其他软件。结束它可能导致那个程序异常退出。
          </p>
        ) : null}
        <DialogFooter className="gap-2">
          <Button variant="outline" size="sm" onClick={() => setKillTarget(null)} disabled={busy !== null}>
            取消
          </Button>
          <Button
            variant="destructive"
            size="sm"
            className="gap-1.5"
            onClick={() => void killPortHolder()}
            disabled={busy !== null}
          >
            {busy === "kill-port" ? <Loader2 className="size-3.5 animate-spin" /> : <Skull className="size-3.5" />}
            确认结束进程
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
    </TooltipProvider>
  );
}
