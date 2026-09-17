import { useCallback, useEffect, useMemo, useState } from "react";
import {
  CircleAlert,
  CircleCheck,
  Coins,
  Layers,
  Loader2,
  RefreshCw,
  ScrollText,
  TrendingDown,
  TrendingUp,
  Zap,
} from "lucide-react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import * as api from "@/lib/api";
import { cn } from "@/lib/utils";

/**
 * 账号记录视图：把「任务执行 / 积分变化 / Token 消耗」三类事件汇到一处展示，
 * 支持按账号与日期区间筛选。
 *
 * 为什么要做统一视图：这三类数据此前散落在「签到日志」「积分统计」「Token 统计」
 * 三个页面，想看「这个账号昨天干了什么、扣了多少」需要来回翻，且无法按账号聚合。
 *
 * 日期筛选的默认范围取「今天」而不是「全部」：绝大多数时候用户关心的是
 * 最近发生了什么，而全部记录在小屏上会刷出几百条。
 *
 * 「今天」是**写死的**，不接受调用方覆盖（此前有个 `defaultRange` 参数，
 * 唯一的调用点传了 "30d" 把它覆盖掉，用户打开就看到近 30 天）。详见组件内注释。
 */
export type RecordKind = "task" | "credit" | "token";

/**
 * 类型筛选的取值：`"all"` 表示不按类型过滤。
 *
 * 为什么要引入显式的「全部」而不是保留 `RecordKind[]` 多选：多选时默认三类全中，
 * 三个按钮同时是绿色实心，看起来像「什么都选中了」而不是「没有过滤」，
 * 用户反而读不出当前到底筛了什么。类型之间本来就是互斥的观察视角（看任务、
 * 看积分、看 Token），所以单选 + 一个「全部」比多选更贴合意图，也让选中态
 * 与日期范围那组一样「永远只有一个绿」。
 */
type KindFilter = RecordKind | "all";

/** 类型筛选的按钮顺序；`all` 必须排在最前，与日期范围的语义对齐。 */
const KIND_FILTERS: KindFilter[] = ["all", "task", "credit", "token"];

/** 日期区间预设。 */
const RANGE_OPTIONS: { key: string; label: string; days: number | null }[] = [
  { key: "today", label: "今天", days: 0 },
  { key: "7d", label: "近 7 天", days: 7 },
  { key: "30d", label: "近 30 天", days: 30 },
  { key: "all", label: "全部", days: null },
];

/** 把日期（YYYY-MM-DD）转成本地时区的当天 00:00 毫秒时间戳。 */
function dayStartMs(dateStr: string): number {
  const [y, m, d] = dateStr.split("-").map(Number);
  if (!y || !m || !d) return 0;
  return new Date(y, m - 1, d, 0, 0, 0, 0).getTime();
}

/** 当天 23:59:59.999 的毫秒时间戳。 */
function dayEndMs(dateStr: string): number {
  const start = dayStartMs(dateStr);
  return start === 0 ? 0 : start + 24 * 3600 * 1000 - 1;
}

function dateKey(d: Date): string {
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

function daysAgoKey(days: number): string {
  const d = new Date();
  d.setDate(d.getDate() - days);
  return dateKey(d);
}

function formatTime(ts: number): string {
  const d = new Date(ts);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${p(d.getMonth() + 1)}/${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

/** 千分位格式化，便于读 Token 数量。 */
function formatNumber(n: number): string {
  return n.toLocaleString("zh-CN");
}

const KIND_META: Record<string, { label: string; icon: typeof Zap; tone: string }> = {
  // 「全部」不是记录类型，只是筛选器的一个取值；放进同一张表是为了让
  // 按钮的图标/文案与其它三项共用一份数据，避免两处各写一套标签而漂移。
  all: { label: "全部", icon: Layers, tone: "text-muted-foreground" },
  task: { label: "任务", icon: ScrollText, tone: "text-sky-600 dark:text-sky-400" },
  credit: { label: "积分", icon: Coins, tone: "text-amber-600 dark:text-amber-400" },
  token: { label: "Token", icon: Zap, tone: "text-violet-600 dark:text-violet-400" },
};

/**
 * 积分来源的展示标签。
 *
 * 为什么在前端再维护一份标签、而不是直接显示后端给的英文 source：
 * source 是**数据契约**（供筛选/统计用），它的取值必须稳定；展示文案则会变
 *（今天叫「额度发放」，明天可能改成「积分包到账」）。混用会让改文案变成改协议。
 *
 * 同样重要的是：表里**没有**的取值（以及 undefined）一律不渲染徽标 ——
 * 历史记录根本没有 source 字段，后端也不会回头补写（记录是只追加的事件流）。
 * 若不判空就渲染，老记录会显示成「来源：undefined」。
 */
const SOURCE_META: Record<string, { label: string; tone: string; title: string }> = {
  grant: {
    label: "额度发放",
    tone: "border-emerald-600/30 text-emerald-700 dark:text-emerald-400",
    title: "余额与额度容量同时增加：新的积分包到账",
  },
  consume: {
    label: "调用扣减",
    tone: "border-muted-foreground/30 text-muted-foreground",
    title: "余额减少而额度容量不变：被调用消耗",
  },
  expire: {
    label: "额度到期",
    tone: "border-destructive/30 text-destructive",
    title: "余额减少且额度容量同降：积分包被回收，包内剩余一并失效",
  },
  adjust: {
    label: "其他调整",
    tone: "border-amber-600/30 text-amber-700 dark:text-amber-400",
    title: "无法归入发放 / 消耗 / 到期的余额变化",
  },
};

export function AccountRecordsView({
  accounts,
  fixedAccountId,
  compact = false,
}: {
  /** 可筛选的账号列表；为空时只按「全部账号」查询。 */
  accounts: { id: string; name: string }[];
  /** 固定账号（用于账号详情内嵌）；提供时隐藏账号选择器。 */
  fixedAccountId?: string;
  /** 紧凑模式：减少内边距与标题，用于嵌在其它卡片里。 */
  compact?: boolean;
}) {
  // 日期范围**不可由调用方指定**，恒为「今天」。
  //
  // 为什么不保留一个 defaultRange 参数（此前是 `defaultRange = "today"`）：
  // 它唯一的调用点（account-card.tsx 的「查看记录」弹窗）传了 "30d"，
  // 于是用户打开记录看到的是近 30 天，而组件自己的注释写着「默认取今天」——
  // 参数的存在让「默认值」变成两处口径，且覆盖发生在离用户最远的地方。
  // 用户关心的是「今天发生了什么」，而近 30 天只是他偶尔才需要的一步操作
  // （点一下「近 30 天」按钮即可）。删掉这个入口，这类不一致就无法再被引入。
  const [rangeKey, setRangeKey] = useState("today");
  const [fromDate, setFromDate] = useState(() => dateKey(new Date()));
  const [toDate, setToDate] = useState(() => dateKey(new Date()));
  const [accountId, setAccountId] = useState(fixedAccountId ?? "");
  // 默认「全部」：不做类型过滤即等价于原先「三类全选」，但界面上只有一个绿，
  // 用户一眼能看出当前没有按类型筛。
  const [kind, setKind] = useState<KindFilter>("all");
  const [data, setData] = useState<api.AccountRecordsResult | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // 范围变化时同步起止日期，保证输入框里显示的与实际查询的一致
  useEffect(() => {
    const opt = RANGE_OPTIONS.find((o) => o.key === rangeKey);
    if (!opt) return;
    const today = dateKey(new Date());
    if (opt.days === 0) {
      setFromDate(today);
      setToDate(today);
    } else if (opt.days === null) {
      setFromDate(daysAgoKey(29));
      setToDate(today);
    } else {
      setFromDate(daysAgoKey(opt.days - 1));
      setToDate(today);
    }
  }, [rangeKey]);

  // 外部传入的固定账号变化时同步
  useEffect(() => {
    if (fixedAccountId !== undefined) setAccountId(fixedAccountId);
  }, [fixedAccountId]);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const from = dayStartMs(fromDate);
      const to = dayEndMs(toDate);
      const res = await api.getAccountRecords({
        accountId: accountId || undefined,
        from: from || undefined,
        to: to || undefined,
        // 后端的契约是「kinds 为空数组 = 全部类型」（account_records.rs:213），
        // 所以「全部」传 undefined（api.ts 会转成 null → 空数组），具体某一类传单元素。
        kinds: kind === "all" ? undefined : [kind],
        limit: 500,
      });
      setData(res);
    } catch (e) {
      setError(api.asError(e));
    } finally {
      setLoading(false);
    }
  }, [accountId, fromDate, toDate, kind]);

  useEffect(() => {
    void load();
  }, [load]);

  /** 首次打开时回填历史签到日志（幂等），让老数据也能在同一视图里看到。 */
  useEffect(() => {
    void (async () => {
      try {
        const res = await api.backfillAccountRecords();
        if (res.added > 0) await load();
      } catch {
        // 回填失败不影响主流程（可能是不支持该命令的旧后端）
      }
    })();
    // 只在挂载时执行一次
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const records = data?.records ?? [];

  return (
    <div className={cn("min-w-0 space-y-3", compact && "space-y-2")}>
      {/* 筛选条 */}
      <div className="flex min-w-0 flex-wrap items-end gap-2">
        {!fixedAccountId && accounts.length > 0 && (
          <div className="min-w-0">
            <Label htmlFor="rec-account" className="text-xs text-muted-foreground">
              账号
            </Label>
            <select
              id="rec-account"
              value={accountId}
              onChange={(e) => setAccountId(e.target.value)}
              className="mt-1 flex h-8 min-w-40 max-w-64 rounded-md border border-input bg-background px-2 text-xs"
            >
              <option value="">全部账号</option>
              {accounts.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.name || a.id.slice(0, 8)}
                </option>
              ))}
            </select>
          </div>
        )}
        <div className="min-w-0">
          <Label htmlFor="rec-from" className="text-xs text-muted-foreground">
            起始日期
          </Label>
          <Input
            id="rec-from"
            type="date"
            value={fromDate}
            onChange={(e) => setFromDate(e.target.value)}
            className="mt-1 h-8 w-36 text-xs"
          />
        </div>
        <div className="min-w-0">
          <Label htmlFor="rec-to" className="text-xs text-muted-foreground">
            结束日期
          </Label>
          <Input
            id="rec-to"
            type="date"
            value={toDate}
            onChange={(e) => setToDate(e.target.value)}
            className="mt-1 h-8 w-36 text-xs"
          />
        </div>
        <div className="flex flex-wrap items-center gap-1">
          {RANGE_OPTIONS.map((o) => (
            <Button
              key={o.key}
              type="button"
              size="sm"
              // 选中态 = `default`（绿色实心）；与类型筛选按钮保持同一视觉语言。
              variant={rangeKey === o.key ? "default" : "outline"}
              className="h-8 px-2.5 text-xs"
              aria-pressed={rangeKey === o.key}
              onClick={() => setRangeKey(o.key)}
            >
              {o.label}
            </Button>
          ))}
        </div>
        {/* 类型筛选：单选。组内恒有且只有一个绿色实心（默认最左的「全部」）。
            它与右边日期范围组各自最多一个绿 —— 两块绿同时出现是正常的，
            因为那确实是两个互不影响的筛选维度。 */}
        <div className="flex flex-wrap items-center gap-1">
          {KIND_FILTERS.map((k) => {
            const meta = KIND_META[k];
            const Icon = meta.icon;
            // 选中态用 `default`（绿色实心），与日期范围按钮、以及项目里其它
            // 切换型按钮（GatewayPage 的模式/用量范围、SettingsPage 的保留天数）
            // 保持同一套「选中 = 绿色」的视觉语言。
            //
            // 此前用 `secondary`（浅灰 bg-secondary）表示选中：它和未选中的
            // `outline`（白底描边）只差 4% 亮度，肉眼几乎分不出，于是点「任务 /
            // 积分 / Token」看起来毫无反应；屏幕上唯一明显的绿色只来自日期范围
            // 那一组（例如停在「全部」），这正是「选中态一直停在全部」的由来。
            const active = kind === k;
            return (
              <Button
                key={k}
                type="button"
                size="sm"
                variant={active ? "default" : "outline"}
                className="h-8 gap-1 px-2.5 text-xs"
                aria-pressed={active}
                onClick={() => setKind(k)}
                title={k === "all" ? "不按类型筛选" : `只看${meta.label}`}
              >
                {/* 未选中时才上类别色：选中后底色是绿色实心，
                    再叠 sky/amber/violet 会糊成一团，此时用按钮前景色（近白）更清楚。 */}
                <Icon className={cn("size-3.5", !active && meta.tone)} />
                {meta.label}
              </Button>
            );
          })}
        </div>
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="h-8 gap-1.5 px-2.5 text-xs"
          onClick={() => void load()}
          disabled={loading}
        >
          {loading ? <Loader2 className="size-3.5 animate-spin" /> : <RefreshCw className="size-3.5" />}
          刷新
        </Button>
      </div>

      {/* 概览 */}
      {data && (
        <div className="flex min-w-0 flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
          <span>
            共 <span className="font-medium text-foreground">{formatNumber(data.total)}</span> 条
          </span>
          <span>
            任务 <span className="font-medium text-foreground">{formatNumber(data.summary.taskCount)}</span> 次
          </span>
          <span>
            积分净变化{" "}
            <span
              className={cn(
                "font-medium",
                data.summary.creditNet > 0 && "text-emerald-600 dark:text-emerald-500",
                data.summary.creditNet < 0 && "text-destructive",
                data.summary.creditNet === 0 && "text-foreground",
              )}
            >
              {data.summary.creditNet > 0 ? "+" : ""}
              {formatNumber(data.summary.creditNet)}
            </span>
          </span>
          <span>
            Token{" "}
            <span className="font-medium text-foreground">{formatNumber(data.summary.tokenSum)}</span>
          </span>
          <span className="text-muted-foreground/70">保留 {data.retentionDays} 天</span>
        </div>
      )}

      {error && (
        <Alert variant="destructive">
          <CircleAlert />
          <AlertTitle>读取记录失败</AlertTitle>
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}

      {/* 列表 */}
      <Card className="min-w-0 gap-0 overflow-hidden rounded-xl py-0 shadow-none">
        {!compact && (
          <CardHeader className="border-b px-4 py-3 sm:px-5">
            <CardTitle className="text-[13px] font-medium leading-5">记录明细</CardTitle>
            <CardDescription className="text-xs">
              任务执行、积分变化与 Token 消耗按时间倒序排列；积分正数为增长、负数为消耗。
            </CardDescription>
          </CardHeader>
        )}
        <CardContent className="max-h-[560px] min-w-0 overflow-y-auto px-4 py-1 sm:px-5">
          {loading && records.length === 0 ? (
            <div className="flex items-center justify-center gap-2 py-10 text-sm text-muted-foreground">
              <Loader2 className="size-4 animate-spin" />
              正在读取…
            </div>
          ) : records.length === 0 ? (
            <div className="py-10 text-center text-sm text-muted-foreground">
              {accountId ? "该账号在此日期范围内暂无记录。" : "此日期范围内暂无记录。"}
            </div>
          ) : (
            records.map((r, i) => <RecordRow key={`${r.kind}-${r.ts}-${i}`} record={r} />)
          )}
        </CardContent>
      </Card>
    </div>
  );
}

/** 单条记录行。 */
function RecordRow({ record }: { record: api.AccountRecordItem }) {
  const meta = KIND_META[record.kind] ?? KIND_META.task;
  const Icon = meta.icon;
  const isFailed = record.result === "failed" || record.result === "error";
  // 来源徽标：只有积分记录、且后端确实给了已知来源时才渲染。
  // 老记录（无 source 字段）与 task/token 记录都落进 undefined → 不渲染。
  const source = record.kind === "credit" ? SOURCE_META[record.source ?? ""] : undefined;

  // 金额展示：积分带正负号，Token 只显示数值
  const amountText = useMemo(() => {
    if (record.kind === "token") return record.amount ? formatNumber(record.amount) : "";
    if (record.kind !== "credit" || record.amount === 0) return "";
    return `${record.amount > 0 ? "+" : ""}${formatNumber(record.amount)}`;
  }, [record]);

  return (
    <div className="flex min-w-0 items-start gap-3 border-b border-border/60 py-3 last:border-b-0">
      <span
        className={cn(
          "mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-full",
          isFailed ? "bg-destructive/10 text-destructive" : "bg-muted text-muted-foreground",
        )}
      >
        {isFailed ? <CircleAlert className="size-3.5" /> : <Icon className={cn("size-3.5", meta.tone)} />}
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 text-xs">
          <span className="flex min-w-0 flex-wrap items-center gap-1.5">
            <span className="font-medium">{record.title}</span>
            <Badge variant="outline" className="h-4 px-1 text-[10px] font-normal">
              {meta.label}
            </Badge>
            {/* 来源徽标：这就是「积分增加看不出是通过什么任务」的答案所在。
                它是数据里真实记录下来的来源类别，不是按时间邻近猜出来的任务名
                （上游不返回这种账单流水，详见后端 credit_usage::classify_credit_source）。 */}
            {source && (
              <Badge
                variant="outline"
                className={cn("h-4 px-1 text-[10px] font-normal", source.tone)}
                title={source.title}
              >
                {source.label}
              </Badge>
            )}
            {record.result === "success" && <CircleCheck className="size-3 text-emerald-600" />}
            {record.result === "already" && <CircleCheck className="size-3 text-amber-600" />}
          </span>
          {amountText && (
            <span
              className={cn(
                "flex shrink-0 items-center gap-1 font-medium",
                record.kind === "credit" && record.amount > 0 && "text-emerald-600 dark:text-emerald-500",
                record.kind === "credit" && record.amount < 0 && "text-destructive",
                record.kind === "token" && "text-violet-600 dark:text-violet-400",
              )}
            >
              {record.kind === "credit" &&
                (record.amount > 0 ? (
                  <TrendingUp className="size-3" />
                ) : (
                  <TrendingDown className="size-3" />
                ))}
              {amountText}
            </span>
          )}
        </div>
        <div className="mt-1 truncate text-[11px] text-muted-foreground">
          {record.accountName || record.accountId.slice(0, 8) || "未知账号"} · {formatTime(record.ts)}
          {record.detail ? ` · ${record.detail}` : ""}
        </div>
      </div>
    </div>
  );
}
