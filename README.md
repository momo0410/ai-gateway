<div align="center">

# AI Gateway

**账号管理 + OpenAI 兼容网关，一个桌面应用搞定**

[![License](https://img.shields.io/badge/License-MIT-green.svg)](./LICENSE)
[![Platform](https://img.shields.io/badge/Platform-macOS%20%7C%20Windows-0078D4.svg)](#系统要求)
[![Tauri](https://img.shields.io/badge/Tauri-2.x-24C8DB.svg)](https://tauri.app)
[![Gateway](https://img.shields.io/badge/API-OpenAI%20Compatible-412991.svg)](#兼容网关)

把 [workbuddy-switch](https://github.com/changexbc/workbuddy-switch) 的账号管理能力
与 [workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 的 OpenAI 兼容网关，
整合进同一个桌面应用：**一个安装包、一个界面、一个进程树**。

[功能特性](#功能特性) · [架构](#架构设计) · [快速开始](#快速开始) · [使用指南](#使用指南) · [常见问题](#常见问题) · [上游与许可](#上游来源与许可证)

</div>

---

## 项目简介

腾讯 CodeBuddy / WorkBuddy 客户端本身不提供 OpenAI 形态的开放接口。若想在
第三方 SDK、IDE 插件或自建服务里使用自己的账号额度，通常需要一段「把客户端凭证
变成标准 API」的胶水层，并同时管理多个账号的登录、保活与额度。

本项目把两件原本分离的事合并到一处：

| 能力 | 说明 |
|---|---|
| **账号生命周期** | 扫码登录、导入、切换、签到、Token 保活、积分到期监控 |
| **API 网关** | 把账号池暴露为 OpenAI 兼容接口，供任意客户端零改造接入 |

两者共享同一份账号库：在图形界面里新增的账号，会自动进入网关的账号池，无需手工
同步或重启容器。

> **来源声明**：本项目是**整合改造**而非从零开发。绝大多数代码来自上述两个上游项目，
> 整合部分（网关页面、账号同步、单文件内嵌、单实例保护、按到期日分层选号及若干
> 缺陷修复）由 [momo0410](https://github.com/momo0410) 完成。
> 详见 [上游来源与许可证](#上游来源与许可证)。

---

## 功能特性

### 账号管理

源自 [workbuddy-switch](https://github.com/changexbc/workbuddy-switch)。

| 模块 | 能力 |
|---|---|
| 账号入库 | OAuth 扫码登录、从本机客户端导入、手动添加 Token |
| 账号切换 | WorkBuddy 客户端、CodeBuddy CLI、CodeBuddy CN IDE 三套登录态互相独立切换 |
| 自动签到 | 启动即核验，运行期周期性补签，保留 30 天签到日志 |
| Token 保活 | 惰性刷新（操作前低于阈值即刷新）+ 每日保活，避免 refresh token 过期失效 |
| 积分监控 | 查询各账号积分资源、剩余量与到期时间，7 天内到期高亮并优先排序 |
| 积分统计 | 汇总官方请求用量：每日趋势、模型分布、账号消耗、请求明细 |
| Token 统计 | WorkBuddy / WorkBuddy AI（国际版）/ CodeBuddy CLI / CodeBuddy CN IDE 四个来源独立统计输入、输出、缓存读写、调用次数 |
| 会话复制 | 将当前账号的会话以新 ID 复制给目标账号（含 jsonl 正文、数据库索引、edge-sync 注册） |
| 自动轮换 | 定时把「积分最紧迫」的账号设为后续会话默认账号，避免额度过期浪费 |

### Trae 账号管理

支持 **Trae Work** 与 **Trae**（Trae CN IDE）两个应用。两者是同一套 icube 内核的
VS Code fork，**共用同一份账号库**，但登录态快照互相独立 —— 同一账号可以在两个
应用里分别切换，互不影响。

| 模块 | 能力 |
|---|---|
| 账号入库 | 粘贴 Cloud-IDE-JWT、本机使用痕迹自动发现（双应用合并） |
| 账号切换 | 备份现场 → 关闭客户端 → 恢复目标快照 → 启动；保留一代备份可回退 |
| 一键签到 | `status` 预检 → `claim`（仅网络异常重试）→ 错误分类 → 冷却落盘 |
| 积分归属 | 三层兜底：claim 奖励字段 → 复查余额差值 → 旧行为 |
| 设备指纹 | 按 uid 确定性派生 `device_id` / `session_id` / `market_user_id`，实现账号间设备隔离 |
| 设备重置 | 6 层机器标识重置（`machineid` / 遥测 / aha 设备 / TinyStorage / 注册表 MachineGuid / webview 追踪） |

**两套 uid 体系（最容易踩的坑）**：`storage.json` 里
`iCubeAuthInfo://icube-dc:<uid>` 的 uid 属于**账户中心编号体系**，而账号库与 JWT
`data.id` 属于 **Cloud-IDE 编号体系**。实测同一账号两者完全不同
（`dc=199439841787403` vs `Cloud-IDE=2328112497170937`）。因此本机发现必须由
**使用痕迹**（`icube_gtm.users`、`state.vscdb` 的 `solo.mobile.allowControl` 等）
推导 Cloud-IDE uid；推导失败时明确标记「无法确认」并**拒绝入池** ——
宁可不给候选，也不产生一个永远登录不上的重复账号。

**登录态快照覆盖 15 个物理路径**（README 常说的「9 类核心文件」）：
`storage.json`、`state.vscdb` 及其 WAL/SHM/backup 边车、`machineid`、`aha/`、
`Preferences`、`Local State`、`Local Storage/leveldb`、`Network/`、
`Partitions/trae-webview`、`Session Storage` 等。两个细节是硬性要求：

- **恢复前必须删除现场残留的 `state.vscdb-wal` / `-shm`**：客户端强杀后 WAL 未
  checkpoint，恢复时若保留，SQLite 启动会把**切换前账号**的登录证据回放回新库 ——
  表现为「切换后账号没变」
- **备份必须包含 WAL/SHM 边车**：客户端常被强杀，最新登录写入可能还在边车里，
  漏拷会丢数据

### 豆包账号管理

| 模块 | 能力 |
|---|---|
| 账号入库 | 手动录入、本地代理抓包自动回写 |
| 登录态切换 | 多 Profile 快照（Chromium 布局），含版本校验、单代回滚、防误覆盖守卫 |
| 会话保活 | 启动客户端 → 等待落盘 → 优雅关闭，触发服务端 30 天滑动续期 |
| 会话探活 | 两段式：权威探活（会员额度接口）+ 保活探活（回收服务端下发的新凭证） |
| 会员额度 | 精确解析 + 宽容兜底两段式；支持单账号查询与全量巡检 |
| 对话备份 | 客户端状态备份/恢复（IndexedDB + DoubaoStorage） |
| 对话导出 | 官方 IM API 拉取正文，输出 markdown + json |

**为什么保活靠「启动客户端」而不是 HTTP**：实测（豆包 Chromium 147）
`Local State` 的 `os_crypt.encrypted_key` 经 DPAPI + AES-256-GCM 解出的**仍是
二进制密文** —— 客户端在 Chromium 的 `v10` 之外还有一层客户端级加密，离线拿不到
明文 `sessionid`。而字节 passport 是 30 天**滑动**续期：客户端带有效会话上线一次，
服务端就顺延。所以保活 = 启动 → 等待 → 优雅关闭；cookie 解密只用于诊断。

**抓包凭证回写有三条硬约束**（都来自实测踩坑）：

- **目标账号取抓包文件自己的 uid**：早期用「uid 探测链」定位目标，而凭证来自抓包
  文件，两者来源不同，实测导致两个账号拿到了**同一个** sessionid（跨账号污染）
- **绝不自动建号**：浏览器网页版与其他字节系应用也会产生豆包 cookie，
  无差别建号会污染账号池
- **幂等**：`session_id`、`sid_guard`、`ttwid` 三者都没变时不写盘，
  否则每 20 秒的轮询会把文件时间戳刷得毫无意义

**多 Profile 遍历不可省**：豆包自带账号隔离，登录会话可能位于**任意** Profile。
只抓 `Default` 会漏掉活跃会话，恢复后客户端打开的活跃 Profile 未登录 ——
这是实测根因，不是理论担忧。恢复前还会校验快照 `schemaVersion` 与 leveldb 的
`CURRENT` → `MANIFEST` 完整性，并修复 `Local State` 的活跃 Profile 指针。

> **对话正文存在豆包云端**，按账号归属。本地备份的是客户端状态（会话列表缓存、
> 技能配置）；恢复并重新登录后完整历史会从云端重新同步。需要把对话带走时用
> 「导出对话」，它直接调官方接口拉取正文。

### 智能体管理

把网关一键接入本机已安装的 AI 客户端（独立页面）。

| 模块 | 能力 |
|---|---|
| 客户端探测 | 自动识别 12 类客户端的安装目录、配置文件与版本 |
| 一键接入 | 按客户端实际协议写入网关地址、API Key 与所选模型，写入前自动备份 |
| 多协议适配 | Anthropic Messages（Claude 系）、OpenAI Chat、OpenAI Responses（Codex / Grok） |
| 模型注入 | 支持多模型；Claude 系按 Sonnet / Opus / Haiku / Fable 四槽位映射 |
| 批量更新 | 「一键更新所有已安装智能体」逐客户端容错执行，单个失败不影响其他 |
| 历史回滚 | 每次写入生成时间戳备份，页面内一键恢复至任意历史版本 |

支持的客户端：Claude Code / Claude Desktop / Codex / DeepSeek Harness / OpenCode /
Pi / Grok Build / ZCode / Kimi Code / OpenClaw / Hermes Agent / MiniMax Code。

> **MiniMax Code** 走 Anthropic Messages 协议（其配置用 `@ai-sdk/anthropic` 适配器）。
> 接入时在 `~/.minimax/config.yaml` 的 `provider` 块注册本网关并把 `defaultModel`
> 指向它；**原有的 MiniMax 官方 provider（含 `managed-login` 登录态）原样保留** ——
> 官方账号与本网关可同时存在、随时切换。

### 兼容网关

源自 [workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)。

- **OpenAI 兼容接口**：`POST /v1/chat/completions`（流式 / 非流式）、`GET /v1/models`
- **OpenAI Responses 接口**：`POST /v1/responses`（兼容 Codex CLI 0.146+）
- **Anthropic Messages 接口**：`POST /v1/messages`（Claude Code / Claude Desktop 3P；请求与 SSE 双向转译、Tool Use 结构转换、Claude 槽位名自动翻译为上游模型名）
- **账号池调度**：按积分到期日分层选号 —— 先烧快过期额度，同一天到期的账号平均分摊
- **积分到期巡检**：每 15 分钟刷新余额与到期日，驱动上面的分层选号
- **熔断与冷却**：429/404 软冷却、余额不足硬冷却至次日 04:00、连续失败指数退避熔断、在途租约限流
- **模型级限流隔离**：识别上游 `429 code=6004`，只冷却**单个模型**（按账号+模型记），
  冷却时长取报错里的重置时间，并在账号池里与「余额欠费」分开显示
- **会话粘性**：同一会话尽量绑定同一账号，TTL 滚动续期，失败自动解绑
- **定时任务**：每日 09:00 / 21:00 签到 + 余额查询解冻；22:00 全账号 Token 刷新保活
- **猫猫旅行**：随签到时点自动巡检（详见下节）
- **出站脱敏**：请求体黑名单指纹字段清洗（可关闭）
- **状态持久化**：池状态本地原子落盘，可选 Upstash Redis 镜像

#### 按到期日分层选号

对上游网关最重要的一处改动。上游原为「三因子加权随机」（积分比例 ×10 +
闲置补偿 + 成功率 ×3），问题是**快过期的额度仍会被分走一部分流量，而额度一旦
过期就是净损失**。改后是两级策略：

| 层级 | 规则 | 目的 |
|---|---|---|
| **1. 到期分层** | 按「最近到期积分」的到期日把候选分组，只保留最早到期的那一档 | 让「先烧快过期额度」成为**确定性**行为，而非概率行为 |
| **2. 档内挑选** | 同档内按「闲置补偿 + 成功率」加权取 Top5，再加权随机 | 同一天到期的账号**平均分摊** |

几个关键设计：

- **用「日」而非精确时刻做分层键**：上游额度按天失效，同一天到期的账号视为同一档
- **档内权重去掉 credits 项**：同档意味着紧迫度相同，若仍按积分加权，高积分账号会
  长期倾斜，与「同档平均分摊」相悖。闲置补偿与成功率作为小幅微调保留（前者防止
  某个号被完全闲置，后者让持续报错的号自然让出流量），二者量级远小于原 credits 的 ×10
- **到期日未知的账号排最后**：只在其它账号都不可用时才轮到它们
- **无到期信息时自动回退**：所有候选都没有到期数据时，退回原三因子口径

到期档位会显示在网关页的账号池里（`到期 MM-DD` / `到期未知`）。

> 到期日随消费实时变化 —— 某账号把快过期额度烧完后，档位会跳到下一档并立刻让出
> 流量。因此引入独立的**积分到期巡检**（默认 15 分钟，账号间隔 300ms），只刷新
> 余额与到期日，不签到、不改账号状态。到期日会持久化进 `state.json`，重启后立刻
> 恢复分层，无需等首轮巡检。
>
> 分层依据有两个来源，按新鲜度覆盖：① 凭证文件里的 `credit` 块（本应用查询后写入）
> ② 网关自身的巡检。因此本应用导出凭证时会**原样透传 `credit` 块** —— 丢掉它会让
> 每次账号同步都把依据抹掉一次，表现为「分层均衡时灵时不灵」。

#### 网关工作模式

可在「兼容网关」页面随时切换，**点击即时生效**（自动重导出凭证并按需重启网关），
两种模式都完整保留熔断、冷却、会话粘性：

| 模式 | 行为 | 适用场景 |
|---|---|---|
| **负载均衡**（默认） | 先打最近到期的积分，同一天到期的账号平均分摊；自动跳过冷却 / 熔断中的账号 | 多账号均衡使用，避免积分过期作废 |
| **指定账号** | 只使用你选定的那一个账号 | 固定身份、单独消耗某账号额度、排查单个账号问题 |

> 实现方式：网关依据凭证目录建立账号池，指定账号模式只需**只导出该账号的凭证**。
> 因此无需改动网关注册逻辑，也不会损失其任何治理能力。切换时旧凭证会被自动清理。
>
> 由于账号池是网关**启动时**扫描凭证目录建立的，模式切换必须重导出凭证并重启子进程
> 才真正生效 —— 这一步已由核心层的 `switch_mode` 合并完成，用户点一下即可，无需手动重启。

#### 需重新登录的账号会被排除出账号池

refresh token 被服务端明确拒绝（如 `12153 Offline user session not found`）时，账号会被
标记「需重新登录」。这类凭证**不会**写入网关凭证目录：

- 网关池里不会再出现它，避免每次请求都白跑一轮再换号（表现为「网关一直用失效账号调模型」）
- 网关页「运行状态」会给出提示，列出被排除的账号，并指引到「账号管理」页重新登录
- 重新登录成功后标记被清除，下一次自动同步（30 秒一轮）会把它重新放回池中

> 标记与「哪些账号该导出」都参与账号指纹，因此标记翻转本身就会触发重新同步，
> 不需要手动点「立即同步」。
>
> **网络失败不等于需要重新登录**：请求根本没发出去（网络不可达 / 代理未启动，
> 响应 `code=-1`）时只记录瞬时错误，不会打上该标记。早期版本曾把两者混为一谈，
> 结果一次代理抖动就让整批国际版账号被误判为失效并移出账号池；升级后启动时会
> 自动清理这类历史误报标记（真正的失效凭证不受影响）。

#### 两种"不可用"的区分：余额欠费 vs 模型冷却

账号池里的账号可能因两种**完全不同**的原因暂时不可用，界面与路由都必须分开对待：

| 类型 | 触发 | 影响范围 | 恢复方式 |
|---|---|---|---|
| **余额欠费** | `402` / 余额关键词 | **整个账号** | 签到或充值（硬冷却至次日 04:00） |
| **模型冷却** | `429 code=6004` | **仅该账号的那个模型** | 到上游给出的重置时间 |

为什么必须区分：`6004` 的报错文案明确写着「您也可以切换其他模型继续使用」——
即该账号**其他模型仍然可用**。若按账号整体冷却，会连带浪费这些仍可用的额度；
反过来，若只当普通 429 处理（固定 60 秒软冷却），则 60 秒后继续撞限流，
而真实重置时间常达数小时。

实现要点：

- **冷却按 `uid + model` 记账**，不碰账号级状态；账号池里该账号仍显示「健康」
- **到期时间取自报错文案**（`ParseResetTime`，支持 `UTC+8` / `UTC-5` / `Z` / 无时区按本地，
  且只接受未来时刻）；解析不出才回退固定软冷却，并在界面上标注
- **路由三个入口都过滤**：普通选号、粘性命中、全冷却兜底 —— 少任何一个都会撞回同一个 6004
- **不喂熔断器**：模型限流是配额信号而非账号故障，喂熔断会把整个账号封掉
- **持久化进 `state.json`**：重置时间常达数小时，而网关会因切换工作模式/重启而重启；
  不持久化则重启即遗忘，立刻重新撞同一批 6004

界面上，账号行下方会展开一块明细区（健康账号不显示）：

```
[模型冷却] deepseek-v4.1-flash          09-15 13:25 恢复（剩 6 小时 40 分钟）
模型冷却只影响上述模型，该账号的其他模型仍可使用。
```

余额欠费则在同一位置显示 `[余额欠费] 余额不足（积分欠费），等签到或充值后恢复`，
两种情形可同时出现。
#### 猫猫旅行

随签到时点（09:00 / 21:00）对每个可用账号推进一趟状态机：

| 账号状态 | 动作 |
|---|---|
| 尚未领养 | 同意协议 + 领养（App 侧每轮巡检主动探测，不依赖派猫失败） |
| 空闲（idle） | 派出 |
| 在途（traveling） | 跳过 |
| 到站（arrived） | 领取奖励 |

- **主动领养**：App 侧每轮旅行巡检先查有无猫，无猫即领养（同意协议 → `buddy/first`）。
  此前 App 侧只在「派猫失败且报错含 no active buddy」时才走到领养，而账号一旦
  `daily_limit_reached` 就会提前返回，**永远到不了领养** —— 表现为「说好会自动领养却没有」。
  领养门槛（对话轮次）未达时按业务预期当日不再重试，失败则下一轮再来。
- 账号间限速 800ms，避免触发风控
- 禁用账号跳过；查询失败仅跳过该账号本轮（不强刷 Token，交给 22:00 保活）
- 派出地点固定为「古镇客栈」（4 个地点收益/时长区间完全相同，无最优解）

> 网关侧与 App 侧都有旅行入口，二者调用同一上游接口且按自然日幂等
>（每日上限 1 次/天），同时开启不会重复派猫。

### 整合增强

本项目在整合过程中新增或修复的部分：

| 能力 | 说明 |
|---|---|
| **账号自动同步** | 账号库变更后自动推送到网关凭证目录；网关运行中则自动重启加载。约 30 秒内生效 |
| **模式切换即时生效** | 「负载均衡 ↔ 指定账号」点击即生效，无需手动重启网关 |
| **凭证元数据保活** | 同步时保留网关写入的 `credit` 块，避免分层选号依据被抹掉 |
| **端口自由选择** | 界面内直接改端口，实时检测占用并给出建议，可一键切换空闲端口 |
| **用量面板全量展示** | 「按账号/按模型」不再截断成前 5 条（此前 8 个账号都在正常轮转、界面只显示 5 个，会被误读成「负载均衡只用了 5 个账号」） |
| **单文件分发** | 网关二进制 gzip 压缩后编进主程序，运行时按内容指纹释放到缓存，**只需分发一个 exe** |
| **网关随应用启动** | 勾选「随 App 启动」后，App 启动即在 setup 阶段拉起网关。此前该设置因**判定条件恒假**（要求一个从未被写入的 `enabled` 字段为真）且启动入口未接线，从未生效 |
| **单实例保护** | 重复启动不会开出第二个窗口，而是聚焦（必要时从托盘唤回）已有实例 |
| **官方身份校验** | 用网关 `/healthz` 的 `service` 标识确认应答者身份，避免「假启动成功」 |
| **双区域支持** | 国服（codebuddy.cn）与国际版（workbuddy.ai）账号可共存于同一账号库，按账号 `domain` 自动路由 |
| **智能体一键接入** | 12 类客户端自动写入网关配置（多协议 + 多模型），写入前自动备份、可回滚 |
| **客户端按区重启** | 切换账号时按账号区域关闭/启动对应客户端（国服 WorkBuddy / 国际版 WorkBuddy AI 互不干扰） |
| **官方用量按区取数** | 国际版账号的官方请求用量与积分查询走 workbuddy.ai 域名，不再误发国服域名被拒 |

关于**单实例保护**的必要性：应用启动后会运行 8 个后台任务（签到、保活、自动轮换、
旅行派发/领取、网关同步等），它们都会写同一份账号库。若允许多开，多个实例会并发
写入导致 Token 被旧值覆盖；同时网关子进程的托管状态是进程内变量，实例之间互不可见，
会重复启停造成端口冲突与孤儿进程。

### 桌面集成

- **系统托盘**：关闭主窗口不退出，隐藏到托盘继续运行（后台任务与网关不受影响）
- **托盘菜单**：打开主界面 / 打开 GitHub / 一键签到 / 轻量模式 / 退出应用
- **开机自启**：可选，自启时静默进入托盘，不弹窗打扰
- **自动更新**：基于 Tauri updater，更新包经签名校验

---

## 服务区域

支持**国服**与**国际版**两个区域，两边的账号可同时存在于同一账号库。

| | 国服 | 国际版 |
|---|---|---|
| 网页 / API | `www.codebuddy.cn` | `www.workbuddy.ai` |
| 聊天端点 | `copilot.tencent.com`（与 API **分域**） | `www.workbuddy.ai`（**同域**） |
| 凭证 `domain` | `www.workbuddy.cn` | `www.workbuddy.ai` |
| 本机认证文件 | `workbuddy-desktop.info` | `workbuddy-desktop-ai.info` |
| OAuth 平台标识 | `workbuddy` | `workbuddy-ai` |
| 自动签到 / 自动旅行 | ✅ 默认参与 | ⛔ 默认跳过（见下） |
| 代表模型 | `deepseek-v4-flash`、`glm-5.2`、`kimi-k2.7` | `gpt-5.6-*`、`gemini-3.5-flash`、`deepseek-v4.1-flash` |

**区域判定**：按账号库 `domain` 字段后缀（`.cn` → 国服，`.ai` → 国际版）。
网关与客户端的所有请求都据此选择域名，无需手工切换配置。

**添加账号**：「账号管理」页的「OAuth 登录」会先选服务区域，再按该区域的域名与
平台标识发起登录。**两区域的登录方式不同**：

| | 国服 | 国际版 |
|---|---|---|
| 登录方式 | 微信 / 企业微信**扫码** | **Google / GitHub / X 三方授权**，另有账号密码、企业 SSO、Tencent OneID |
| 授权页 | 自建登录页 | Keycloak realm（`/auth/realms/copilot/...`）内的 iframe |

> 国际版没有扫码。流程仍是「申请 state → 浏览器授权 → 轮询取 token」，只是浏览器
> 里那一步是三方联合登录。实测（2026-09）：`platform=workbuddy-ai` 走完授权后
> `/v2/plugin/auth/token` 正常返回凭证（`domain=www.workbuddy.ai`），可正常入库。
>
> 该 token 端点**一次性消费**：取到一次后再次轮询会退回 `11217 login ing`，
> 因此应用会在拿到 token 后立即入库并重试拉取账号信息。

**一键导入**：「账号管理」页的「从本机导入」会扫描**三类来源**，把本机登录过的账号
一次找齐：

| 来源 | 位置 | 说明 |
|---|---|---|
| 当前登录态 | `auth/workbuddy-desktop.info`、`workbuddy-desktop-ai.info` | 每区域各 1 个（固定文件名） |
| 历史登录快照 | `auth/workbuddy-desktop[-ai].<时间>.<pid>.<uuid>.info` | 客户端每次登录/切换时留存 |
| 切换备份 | `~/.wb-switch/backups/*.info` | 本工具每次切换账号前的备份 |

弹框里可勾选任意多个账号批量导入，并标注每个候选的来源、区域与凭证可用性
（可保活 / 仅 access 有效 / 凭证已过期），已在账号库中的会标出「已在账号库」或「将更新」。

> 旧版的「导入本机账号」只读两个固定文件名，因此**每区域最多只能拿到当前登录的 1 个账号**，
> 历史登录过的账号完全没有入口。
>
> **同一账号只保留凭证最新的一份**：同一 `(区域, uid)` 可能有多份文件，按「凭证可用性 →
> 到期时间 → 文件修改时间」排序取最优。这样不会让一份 refresh token 已被轮换掉的旧快照
> 顶掉有效凭证。跨区域永不合并（两区域 uid 命名空间独立）。
>
> 快照里的 token 会过期：国服 access token 60 天 / refresh token 90 天，国际版约 1 年。
> 已完全过期的候选仍可导入，但需要重新登录才能保活。

账号卡片上会给国际版账号打一个「国际版」标记，国服账号不加标记。

> **两个区域的身份命名空间相互独立**：同一串 uid（或同一个邮箱）可以同时存在于
> 国服与国际版。账号库的身份匹配因此**带区域**，跨区域永不互相覆盖。

### 国际版账号默认不参与自动签到与猫猫旅行

国际版（`workbuddy.ai`）的相关接口目前尚未提供真实数据，实测（2026-09）：

| 接口 | 国际版实测返回 |
|---|---|
| `POST /v2/billing/meter/checkin-activity-status` | `200`，但 `active: false`、`today_checked_in: false`、`daily_credit: 0` |
| `GET /activity/growth/buddy/travel/status` | `200`，但 `data: {}`（没有 `state` 字段） |
| `GET /activity/growth/buddy/travel/config` | `200`，但 `data: {}`（没有地点列表） |

也就是说，对国际版账号签到**拿不到积分**，派猫猫旅行也只会落到「查询旅行状态失败」
并每 30 分钟重试一次。因此：

- **自动签到 / 自动旅行**：只对国服账号执行（**硬绑定**，非开关）
- **一键签到 / 托盘「一键签到」**：同样只覆盖国服账号
- **账号卡片**：国际版账号不显示「已签到 / 未签到」标签（不发无效请求）
- **token 保活不受影响**：国际版账号照常刷新 token（刷新接口是可用的）

> 该范围早期曾是配置项（`region_scope` = `"cn" | "all"`），但国际版接口始终没有
> 数据，留着开关只会让用户切到 `all` 后得到一堆无意义的失败日志，故已移除。
> 历史配置里残留的 `region_scope` 会被忽略。
>
> 若上游日后补齐了国际版接口，可手动把 `~/.wb-switch/gateway/gateway_native_config.json`
> 的 `schedule.checkin_scope` 改成 `"all"`，让**网关侧**的签到与旅行覆盖国际版
> （默认 `"cn"`，不在界面上暴露）。App 侧无此开关。

> **模型名不通用**：两区域模型名不同（国服 `deepseek-v4-flash` / 国际版 `deepseek-v4.1-flash`）。
> `/v1/models` 返回两区域模型的并集，但**某个名称能否用取决于实际选中的账号属于哪个区域**；
> 不匹配时上游返回 `11102 model service info not found`，网关会自动换号重试。
> 需要精确控制时，请在网关页选择「**指定账号**」模式并锁定对应区域的账号。

## 架构设计

```
┌──────────────────── ai-gateway.exe（单一可执行文件）────────────────────┐
│                                                                        │
│  桌面壳（Tauri 2 / Rust）                                               │
│  ├─ 主窗口：内嵌 React 前端（账号管理 · Token 统计 · 积分统计 · 兼容网关）│
│  ├─ 系统托盘与单实例保护                                                │
│  └─ 后台任务：签到 · 保活 · 自动轮换 · 旅行 · 账号同步                   │
│                                                                        │
│  ai-gateway-core（Rust 库）                                              │
│  ├─ account / auth_file / switch / session …   账号与登录态             │
│  ├─ checkin / refresh / rotate / travel …      定时任务                 │
│  ├─ gateway.rs                                 网关托管与账号桥接        │
│  └─ gateway_embed.rs                           内嵌网关的释放与缓存      │
│                                                                        │
│  内嵌网关二进制（Go，gzip 压缩，构建期写入）                              │
│  └─ 运行时释放为 ~/.wb-switch/gateway/bin/gateway-<指纹>.exe            │
└────────────────────────────────────────────────────────────────────────┘
            │                                        │
            ▼                                        ▼
  ~/.wb-switch/accounts.json              ~/.wb-switch/gateway/
     （账号库 · 唯一真源）                    └─ gateway_auths/
                                                 （网关凭证 · 派生素材）
```

**两个存储为什么分开**：账号库与网关凭证的格式不同 —— 时间戳单位分别为毫秒与秒，
字段集也不同（账号库含 `profile_raw`、`auth_raw` 等客户端字段）。网关是独立进程，
需要自己能读懂的凭证目录。因此二者由自动同步机制保持一致：账号库是唯一真源，
网关凭证是派生素材。

**同步规则**：
- 账号库 → 网关：按 uid 生成嵌套形凭证；账号删除或模式切换后自动清理残留
- 网关 → 账号库：网关自身刷新 Token 后，按过期时间较新者回写（空 refresh token 不覆盖已有值）
- 内容无变化时不写盘，避免无意义的文件时间戳变动与网关重启

---

## 快速开始

### 系统要求

| 平台 | 要求 |
|---|---|
| Windows 10 / 11（x64） | 需 [WebView2 Runtime](https://developer.microsoft.com/microsoft-edge/webview2/)（一般已内置） |
| macOS（Apple Silicon） | 直接运行，首次打开需 `xattr -cr` 放行（见下） |
| macOS（Intel） | 同上，下载 `_x64.dmg` |
| 磁盘 | 约 50 MB |
| 其他 | 无需安装 Docker、Node.js 或 Go（从安装包运行时） |

### 安装方式一：安装包（推荐）

从 [Releases](https://github.com/momo0410/ai-gateway/releases/latest) 下载：

| 文件 | 说明 |
|---|---|
| `ai-gateway_<版本>_aarch64.dmg` | macOS（Apple Silicon）磁盘映像，拖入「应用程序」即安装 |
| `ai-gateway_<版本>_x64.dmg` | macOS（Intel）磁盘映像，同上 |
| `AI.Gateway_<版本>_x64-setup.exe` | Windows 安装向导，自动创建开始菜单与卸载项 |
| `AI.Gateway_<版本>_x64_en-US.msi` | Windows MSI 包，适合批量部署 |
| `AI_Gateway_<版本>_portable.zip` | Windows 便携版，解压即用，不写入注册表 |
| `AI.Gateway_<版本>_amd64.deb` | Linux（Debian/Ubuntu）安装包 |
| `AI.Gateway_<版本>_amd64.AppImage` | Linux 免安装可执行文件 |

### 安装方式二：便携版（Windows，免安装）

下载 `AI_Gateway_<版本>_portable.zip`，解压后双击 `ai-gateway.exe`
即可运行，不写入注册表。

> `WebView2Loader.dll` 必须与 `ai-gateway.exe` 位于同一目录，请勿删除。

### 更新到新版本

- **安装版（推荐）**：直接双击新版安装包即可**覆盖更新** —— 安装器不会询问是否
  卸载，安装目录沿用上次位置（自定义目录同样适用），只更新程序文件并把注册表版本
  提升到新版本；安装前会自动结束正在运行的旧版本。
- **便携版**：解压新版便携包，把 `ai-gateway.exe` 覆盖到程序目录
  （`WebView2Loader.dll` 无需更换），重启应用即可。
- **应用内更新**：应用检测到新版本后会提示下载并运行安装包，流程与双击安装包一致。

> 0.6.1 起安装器支持覆盖更新（自定义 NSIS 模板，见 `src-tauri/installer.nsi`）；
> 从更早版本升级到 0.6.1 时同样无需手动卸载。

### 安装方式三：从源码构建

需要 Go ≥ 1.22、Node.js ≥ 20、Rust 工具链（Windows 需 MSVC 工具链以链接 WebView2）。

> 网关源码随仓库分发在 `go-gateway/`，无需另行 clone 上游、也不需要打补丁。

```bash
# 1) 构建网关（Go）—— 产物落到 crates/ai-gateway-core/embedded/，
#    cargo build 时由 build.rs 压缩内嵌进主程序
sh scripts/build-gateway.sh                              # 当前平台
GOOS=windows GOARCH=amd64 sh scripts/build-gateway.sh    # 交叉编译到指定平台
GOOS=darwin  GOARCH=arm64 sh scripts/build-gateway.sh

# 2) 前端 + 桌面应用
npm ci
npm run tauri -- build --bundles app                     # macOS（产出 .app）
npm run tauri -- build --bundles nsis,msi                # Windows

# 3) macOS 额外产出 dmg（无头环境也能打，不依赖 Finder）
sh scripts/make-dmg.sh <版本> <aarch64|x86_64> \
  "target/release/bundle/macos/AI Gateway.app"
```

> macOS 产物为 adhoc 签名（无 Apple 开发者证书），首次打开若提示「已损坏」，执行
> `xattr -cr "/Applications/AI Gateway.app"` 放行。

开发调试命令：

```bash
npm install
npm run tauri dev        # 开发模式
npm run build            # 前端类型检查与构建
npm run tauri build      # 构建当前平台安装包
```

### 发布新版本

推一个 `v*` tag 即可，`.github/workflows/release.yml` 会构建 Windows x64 / macOS arm64 /
Linux x64 三个平台并建 Release。日常 push 走 `.github/workflows/ci.yml`，三平台编译校验 + 单测 + Go 网关单测，不产出安装包。

**npm 版（webui）发布**：

1. CI 在 tag 发布时编译 server 二进制，作为平台包 `ai-gateway-win32-x64` 发布到 npm registry
2. `cd npm && npm publish`（包名 `ai-gateway`，postinstall 从平台包复制二进制到 `bin/`，不依赖 GitHub）

### 目录结构

```
src-tauri/src/       # Tauri command 薄包装与托盘
crates/
  ai-gateway-core/    # 核心逻辑：账号/认证/切换/会话/签到/刷新/更新/配置/智能体接入
  ai-gateway-server/  # HTTP server + CLI：axum API + rust-embed 前端
  ai-gateway-router/ # 网关内核（Rust 移植版，chat_completions 仍为占位）
src/                 # 前端：components/pages/lib（api.ts 双通道：Tauri invoke / HTTP fetch）
go-gateway/          # Go 版网关源码（当前实际构建依赖，编译后内嵌）
npm/                 # npm 包：package.json + bin + scripts/install.js
scripts/             # 构建与发布脚本
```

### 隐私注意事项

- 仓库不提交本地数据（`accounts.json`、认证文件、密钥、token 由 `.gitignore` 排除）
- 发布前用 `git grep` 扫描 token 模式（`ghp_`/`npm_`/`gho_` 等）

---

## 使用指南

### 账号管理

1. 打开应用，进入「账号管理」页面
2. 点击「OAuth 登录」，先选服务区域：国服用微信 / 企业微信扫码，国际版用
   Google / GitHub / X 授权；也可「从本机导入」批量找回本机登录过的账号
   （当前登录态 + 历史快照 + 切换备份，可多选）
3. 账号卡片显示登录状态、签到状态、积分余额与到期时间
4. 「切换」按钮可将该账号写入 WorkBuddy 客户端 / CodeBuddy CLI / CodeBuddy CN IDE
   - **CodeBuddy CLI**：写入 `~/.codebuddy/settings.json` 的 `env.CODEBUDDY_AUTH_TOKEN`
     （保留该文件中的其他配置）。只更新**后续加载会话**使用的默认账号，不会切换
     正在运行的会话；请由 ACP 重新加载会话，或重启 CodeBuddy CLI 后生效。普通 CLI
     在同一进程内执行 `/resume` 不保证重新读取认证配置。

### 智能体管理

1. 进入「智能体管理」页面，应用会自动探测本机 12 类客户端（安装目录、配置文件与版本）
2. 在顶部「分发模型配置」中选择要注入的模型：排第 1 位的自动作为默认主模型；
   Claude 系客户端按 Sonnet / Opus / Haiku / Fable 四个槽位顺序映射
3. 单卡片「一键接入」只写入该客户端；顶部「一键更新所有已安装智能体」逐客户端执行，
   单个失败不会影响其他客户端
4. 每次写入前自动备份到 `~/.wb-switch/agent-backups/<客户端>/<时间戳>/`，
   卡片「备份历史」中可查看并一键回滚

> 接入会覆盖各客户端现有网关配置（每次均自动备份）。以 Claude Desktop 为例，
> 应用使用独立的 3P profile（`configLibrary` 中的独立 uuid），与 CC Switch 等
> 其他工具的配置互不覆盖。

### 启用网关

1. 进入「兼容网关」页面
2. 设置**服务端口**（默认 `7863`）— 输入框右侧会实时显示端口是否可用
   - 若显示「已被占用」，点击「自动」自动挑选空闲端口，或点建议端口一键切换
3. 设置 **API Key**（留空表示不鉴权；公网部署务必设置）
4. 选择**工作模式**：负载均衡 / 指定账号（后者需选择具体账号，**点击即时生效**）
5. 点击「启动网关」

账号池区域会显示每个账号的到期档位：`到期 MM-DD` 是分层依据（同一天的账号同级
平均分摊），`到期未知` 表示尚未取到信息、会排在最后。

### 客户端接入

启动成功后，页面会显示完整的接入地址，可直接复制。以环境变量方式接入：

```bash
export OPENAI_BASE_URL=http://127.0.0.1:7863/v1
export OPENAI_API_KEY=<你设置的 api_key>
```

验证连通性：

```bash
curl $OPENAI_BASE_URL/models -H "Authorization: Bearer $OPENAI_API_KEY"

curl $OPENAI_BASE_URL/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}'
```

现有 OpenAI SDK / 客户端通常只需替换 `base_url` 与 `api_key` 即可使用。

Anthropic Messages 协议（Claude Code / Claude Desktop 等）使用：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:7863
export ANTHROPIC_AUTH_TOKEN=<你设置的 api_key>
```

> 也可直接在「智能体管理」页面一键写入上述配置（含模型与认证，自动备份）。

### 网关配置项

配置文件位于 `~/.wb-switch/gateway/gateway_config.json`，也可在界面中修改：

| 字段 | 默认 | 说明 |
|---|---|---|
| `port` | `7863` | 服务端口（权威字段，`listen` 由它派生） |
| `api_key` | 空 | 接口鉴权密钥；空 = 不鉴权 |
| `mode` | `balance` | 工作模式：`balance` 负载均衡 / `pinned` 指定账号 |
| `pinned_uid` | `null` | 指定账号模式锁定的账号 uid |
| `auto_start` | `false` | 随应用启动自动拉起网关 |
| `checkin_enabled` | `true` | 是否启用签到排程（猫猫旅行随之启停） |
| `keepalive_enabled` | `true` | 是否启用 Token 保活排程 |
| `activity_hours` | `[10]` | 活跃上报时点：点亮**连登天数**并解锁领养前置 |
| `nightowl_hours` | `[1]` | 夜猫子任务时点（仅在 23:00–08:00 北京时间内计入） |
| `school_hours` | `[12]` | 开学季活动时点（限时活动，只领已达标的奖励） |
| `trial_hours` | `[9, 21]` | 国际版 trial 加油包领取时点（仅国际版账号） |
| `activity_enabled` / `nightowl_enabled` / `school_enabled` / `trial_enabled` | `true` | 上述 4 个养号任务的启用开关 |
| `activity_report_count` | `3` | 每号每日活跃上报条数（上限 20） |

> 这 4 个养号任务在「设置 → 自动养号任务」中配置，每个任务都带「立即执行」按钮
> 便于改完立刻验证（走网关的 `POST /tasks/run`）。**改完需要重启网关才生效**。

转换成网关格式后写入 `gateway_native_config.json`，其中这些项需手动编辑：

| 字段 | 默认 | 说明 |
|---|---|---|
| `schedule.checkin_scope` | `"cn"` | 网关侧签到 / 旅行的区域范围；`"all"` = 含国际版 |
| `pool.credit_refresh_interval` | `"15m"` | 积分到期巡检周期 |
| `pool.credit_refresh_enabled` | `true` | 是否启用积分到期巡检 |
| `pool.idle_weight_per_hour` | `0.5` | 档内权重的闲置补偿 |
| `pool.idle_weight_max` | `5.0` | 闲置补偿封顶 |
| `pool.max_in_flight` | `3` | 单账号在途租约上限 |
| `pool.breaker_threshold` | `3` | 连续失败几次触发熔断 |

> 关闭积分巡检后，分层选号将只依赖签到与本应用同步的数据，到期档位更新会明显滞后。

### Trae / 豆包 使用指南

#### Trae

1. 进入「Trae 账号」页面，用顶部标签切换 **Trae Work** / **Trae**
2. 添加账号二选一：
   - **粘贴 JWT**：从客户端登录态里取出 `Cloud-IDE-JWT` 贴进去，账号 id 自动解析
   - **发现本机账号**：从客户端使用痕迹推导。标记为「无法确认」的候选**不会**入池
     （它的编号属于账户中心体系，与账号库不是同一套编号）
3. 账号列表里可执行：**切换**、**保存登录态**、**重置设备指纹**、**清除冷却**
4. 点「一键签到」跑一轮；失败的账号按错误类型落冷却，不会反复撞限流

> **保存登录态** = 把客户端当前的登录状态存进该账号的快照槽。
> **切换** = 先备份现场、再恢复目标账号的快照，并保留一代备份可回退。

#### 豆包

1. 进入「豆包账号」页面
2. 获取凭证（二选一）：
   - **本地代理抓包**（推荐）：在「设置 → 本地代理」启动代理并信任 CA，
     然后用豆包客户端访问一次，凭证会自动抓取并回写（页面每 20 秒轮询一次）
   - **手动录入**：点「编辑」填写 `sessionid` / `sid_guard` / `ttwid`
3. 账号列表里可执行：**探活**、**查询额度**、**备份/恢复对话状态**、
   **导出对话**（markdown + json，输出到 `~/.wb-switch/exports/`）
4. 顶部按钮：「保活」（启动客户端触发会话续期）、「探活续期」（HTTP 探测）、
   「额度巡检」（批量查询）、「诊断」（排查为什么读不到凭证）

> 抓包回写**只认抓包文件自己的 uid**，且**绝不自动创建账号** ——
> 浏览器网页版与其他字节系应用也会产生豆包 cookie，无差别建号会污染账号池。

#### 本地代理

代理拦截目标域名的请求，为每个账号注入独立设备标识，并自动抓取登录凭证。

- **启动前会记下你原有的系统代理**，停止时原样还原；界面会显示检测到的原值
- **CA 证书**需安装到「受信任的根证书颁发机构」，否则 HTTPS 拦截会因证书不受信而失败
- 代理**意外崩溃**时会立刻还原系统代理（否则应用还在但系统代理指向死端口，本机断网）
- 未命中的域名透明转发，不影响其他应用上网

#### 计划任务

「设置 → 计划任务」可把 Trae 签到 / 豆包保活 / 额度巡检注册为 Windows 计划任务，
**即使应用没在运行也会按时执行**。可设置执行时间、随时删除，或点「立即执行」验证配置。

### 托盘与单实例

- **关闭窗口**：隐藏到托盘而非退出，后台任务与网关继续运行
- **再次启动**：聚焦已有窗口；若窗口已隐藏到托盘则自动唤回，不会开启第二个实例
- **彻底退出**：右键托盘图标 → 「退出应用」（网关子进程会一并结束）

---

## 数据与备份

| 内容 | 路径 | 说明 |
|---|---|---|
| 账号库 | `~/.wb-switch/accounts.json` | **唯一真源**，含所有账号凭证，建议单独备份 |
| Trae 账号库 | `~/.wb-switch/trae_accounts.json` | Trae Work 与 Trae 共用（两者登录态独立） |
| 豆包账号库 | `~/.wb-switch/doubao_accounts.json` | 含会话凭证（等价于密码，勿分享） |
| 网关凭证 | `~/.wb-switch/gateway/gateway_auths/` | 由账号库派生，删除后可自动重建 |
| 网关配置 | `~/.wb-switch/gateway/gateway_config.json` | 端口、API Key、模式等 |
| 网关原生配置 | `~/.wb-switch/gateway/gateway_native_config.json` | 转换后交给网关进程的配置 |
| 内嵌网关副本 | `~/.wb-switch/gateway/bin/` | 按内容指纹命名，版本升级后自动更新 |
| 登录态快照 | `~/.wb-switch/profiles*/` | Trae 系与豆包各一套，含一代 `.bak` 回退 |
| 豆包对话备份 | `~/.wb-switch/doubao_chats/` | 客户端状态（对话正文在云端） |
| 对话导出 | `~/.wb-switch/exports/` | markdown + json |
| 代理抓包日志 | `~/.wb-switch/logs/` | 凭证已脱敏，但可能含其他请求信息 |
| CA 证书 | `~/.wb-switch/certs/` | 自签 CA，安装后请妥善保管私钥 |
| 智能体配置备份 | `~/.wb-switch/agent-backups/` | 一键接入前自动备份，可随时回滚 |
| 签到 / 轮换日志 | `~/.wb-switch/*_logs.json` | 最多保留 30 天 |

> `accounts.json` 与 `doubao_accounts.json` 包含可直接登录的凭证，请勿分享或提交到版本库。

### 数据目录与版本拆分（2026-09-16）

应用数据统一放在 **`~/.wb-switch/`**，老版本（`WorkBuddy Switch Gateway`，0.8.x
线）与本版本共用这一份账号库与网关状态 —— 在任一版本里新增的账号，另一个版本
会直接看到，无需手工同步。

1.0.0 更名时曾把数据目录换成 `~/.ai-gateway` 并做一次性迁移。由于本仓库此后与
老仓库**并行维护**、两版常同时装在一台机器上，那个迁移会让两边各自变成一份
快照、互相看不见对方新增的账号，因此目录已统一回 `.wb-switch`。

**从 1.0.0 / 1.0.1 升级**时，首次启动会自动把 `~/.ai-gateway` 里的数据迁移进来：

- **复制式**迁移：旧目录**原样保留**，可随时回退
- **并集合并**：账号库按 `(区域, uid)` 去重合并，已有条目不被覆盖
- **只迁移一次**：完成标记写在 `~/.wb-switch/.migrated-from-ai-gateway`，
  之后你在某一版里删除的账号不会被另一版「复活」
- 设了 `AI_GATEWAY_HOME` 时不迁移（隔离环境不该被真实数据污染）

> 签名私钥位于 `~/.wb-switch/wb-switch-updater.key`（与数据目录同处）。
> `scripts/build-signed.ps1` 会自动回退到更名那一代的 `~/.ai-gateway/ai-gateway-updater.*`；
> 需要迁移时请手动复制过去并改名为 `wb-switch-updater.*` —— 脚本刻意不自动搬运私钥。

---

## 常见问题

**Q：网关启动失败，提示端口被占用？**
> 常见于本机已有其他服务占用同一端口（例如先前用 Docker 部署过网关）。
> 在「兼容网关」页面点「自动」换一个空闲端口即可。注意 Docker 映射的端口在
> 部分 Windows 配置下无法通过常规方式探测，本应用已采用「试绑 + 主动连接」双重
> 判定来提高识别率，但若仍提示启动失败，请手动指定其他端口。

**Q：提示「找不到 WebView2Loader.dll」？**
> 系统缺少 WebView2 相关组件。本仓库的安装包已内置该加载器；若使用免安装版，
> 请确认 `WebView2Loader.dll` 与主程序位于同一目录。仍报错则需安装
> [WebView2 Runtime](https://developer.microsoft.com/microsoft-edge/webview2/)。

**Q：添加了新账号，但网关里看不到？**
> 正常情况下约 30 秒内自动同步。若网关正在运行，同步后会短暂重启以加载新账号
> （这是网关只在启动时扫描凭证目录的设计所限）。也可在页面点「立即同步」手动触发。

**Q：窗口关闭后应用消失了吗？**
> 没有。窗口被隐藏到系统托盘，后台任务与网关仍在运行。右键托盘图标可重新打开
> 主界面或彻底退出。

**Q：国服与国际版账号能混用吗？**
> 可以放在同一账号库，按 `domain` 自动路由。但**模型名不通用**：混合账号池下
> 用某个区域的模型名请求，可能被路由到另一区域账号而返回 `11102`，网关会自动
> 换号重试（表现为偶发变慢）。需要稳定时，用「指定账号」模式锁定对应区域的账号。

**Q：为什么国际版账号不会被自动签到、也看不到旅行标签？**
> 国际版的签到与成长中心接口目前不返回真实数据（签到状态恒为未签到且
> `daily_credit: 0`，旅行 `status`/`config` 恒返回空 `data`）。所以自动签到、
> 自动旅行、一键签到都**硬绑定为仅国服**，卡片上也只给国际版加「国际版」标记
> 而不显示签到标签。token 保活不受影响。

**Q：账号池里的「到期」档位是什么意思？**
> 那是该账号「最近到期积分」的到期日，也是负载均衡的**分层依据**：网关优先把流量
> 导向最早到期的那一档，同一天到期的账号平均分摊，避免积分过期作废。
> 显示「到期未知」表示还没取到到期信息，这类账号排在最后。
> 档位由积分到期巡检每 15 分钟刷新，积分烧完后会自动跳到下一档。

**Q：切换工作模式需要手动重启网关吗？**
> 不需要。账号池是网关**启动时**扫描凭证目录建立的，因此模式切换必须重导出凭证
> 并重启子进程才真正生效 —— 这一步已由 `switch_mode` 自动完成（保存配置 →
> 重导出 → 按需重启）。若网关当时没在运行，则只做前两步，下次启动自然是新池。

**Q：账号卡片显示「需重新登录」，但网关还在用这个账号？**
> 已修复。refresh token 被服务端拒绝后，账号会被标记「需重新登录」，并**不再写入
> 网关凭证目录** —— 网关池里不会再有它，也就不会每次请求都拿失效凭证去试一轮。
> 网关页「运行状态」会列出被排除的账号；到「账号管理」页重新登录后，标记自动清除，
> 下一轮同步（30 秒）就会把它放回池中。
>
> 注意**网络失败不算**需重新登录：请求没发出去（网络不可达 / 代理未启动）只记录
> 瞬时错误，不会打标记。早期版本把两者混为一谈，一次代理抖动就会让整批账号被误判
> 失效并移出账号池；升级后启动时会自动清理这类历史误报标记。

**Q：「从本机导入」能找回历史登录过的账号吗？**
> 能。它会扫描当前登录态（每区域 1 个固定文件）、客户端留存的历史登录快照
> （`workbuddy-desktop[-ai].<时间>.<pid>.<uuid>.info`）以及本工具的切换备份
> （`~/.wb-switch/backups/`），可在弹框里一次勾选多个账号导入。
>
> 同一 `(区域, uid)` 的多份文件只保留**凭证最新**的一份（按可用性 → 到期时间 →
> 文件修改时间取优），避免旧快照里已被轮换的 refresh token 顶掉有效凭证。
> 快照 token 会过期（国服 refresh token 90 天，国际版约 1 年），已过期的候选
> 仍可导入，但需要重新登录才能保活。

**Q：国际版的模型列表为什么是固定的？**
> 上游 `/console/enterprises/personal/models` 在国际版返回 500，无法动态拉取，
> 因此国际版模型来自内置静态表（取自客户端本地配置 `acc-product-config-v3.json`）。
> 上游新增模型时需要同步更新该表。

**Q：能同时运行上游的 ai-gateway 吗？**
> 可以。两者的应用标识与安装目录不同，互不冲突。

---

## 开发

### 项目结构

```
crates/ai-gateway-core/        核心逻辑（不依赖 Tauri，可被桌面端与 HTTP 服务复用）
  src/modules/account.rs        账号存储
  src/modules/app_profile.rs    5 应用 × 3 快照布局的档案表（多应用扩展）
  src/modules/auth_file.rs      认证文件读写 + 本机历史登录态扫描（本项目扩展）
  src/modules/refresh.rs        Token 刷新与保活（含传输层失败与凭证失效的区分）
  src/modules/agent_import.rs   智能体一键接入与配置生成（本项目新增）
  src/modules/gateway.rs        网关托管与账号桥接（本项目新增）
  src/modules/gateway_embed.rs  内嵌网关的释放与缓存（本项目新增）
  src/modules/travel.rs         猫猫旅行（App 侧）
  src/modules/yaml_lite.rs      轻量 YAML 读写（本项目新增）
  src/modules/switcher/         登录态切换器（多应用扩展）
    mod.rs                       动作编排 + 进度回调 + 防误覆盖守卫
    copy.rs                      替换语义拷贝 + 单代回滚 + 槽位解析
    icube.rs                     Trae 系快照（15 项白名单、WAL 边车、对称恢复）
    chromium.rs                  豆包快照（多 Profile、版本校验、活跃 Profile 修复）
    proc.rs                      三层优雅关闭（WM_CLOSE → 温和 taskkill → 强制）
    locate.rs                    6 级 exe 发现回退链
    machine.rs                   6 层设备标识重置
  src/modules/trae_account.rs   Trae 账号库与 JWT 解析（多应用扩展）
  src/modules/trae_device.rs    Trae 账号级设备指纹确定性派生（多应用扩展）
  src/modules/trae_checkin.rs   Trae 签到（错误分类、冷却、积分三层兜底）
  src/modules/trae_discover.rs  双应用本机账号发现（两套 uid 体系）
  src/modules/doubao_account.rs 豆包账号池与凭证（含抓包回写）（多应用扩展）
  src/modules/doubao_session.rs 豆包保活与两段式探活（多应用扩展）
  src/modules/doubao_quota.rs   豆包会员额度（精确解析 + 宽容兜底）
  src/modules/doubao_chats.rs   豆包对话备份与官方 IM API 导出
  src/modules/device_proxy/     MITM 设备代理（多应用扩展）
    mod.rs                       代理生命周期 + 事件 trait
    handler.rs                   请求拦截、JWT 与豆包凭证捕获
    ca.rs                        自签 CA 与按域名签发叶子证书
    upstream.rs                  上游连接（透传用户 VPN）
    sys_proxy.rs                 Windows 系统代理编排（停止时原样还原）
    bypass.rs                    OAuth 域名直连豁免
    logger.rs                    抓包日志（凭证脱敏）
    ws.rs                        WebSocket 观测桥接
    local_capture.rs             本机离线凭证捕获
  src/modules/scheduler.rs      Windows 计划任务（多应用扩展）
  src/modules/cli_task.rs       CLI 任务模式（--task-run，刻意不启动 Tauri）
  src/modules/migrate_store.rs  数据目录迁移（复制式、幂等）（多应用扩展）
  build.rs                      构建期压缩内嵌网关（本项目新增）
crates/ai-gateway-router/      网关内核（Rust 版）
crates/ai-gateway-server/      HTTP 服务形态（npm / webui）
src/                          React 前端
  src/pages/GatewayPage.tsx     兼容网关页面（本项目新增）
  src/pages/AgentsPage.tsx      智能体管理页面（本项目新增）
  src/pages/TraePage.tsx        Trae 账号页面（多应用扩展）
  src/pages/DoubaoPage.tsx      豆包账号页面（多应用扩展）
  src/components/import-local-dialog.tsx  从本机批量导入账号（本项目新增）
src-tauri/                    桌面壳（Tauri 2）
  src/tray.rs                   托盘与单实例行为
  src/commands.rs               WorkBuddy 系命令
  src/commands_apps.rs          Trae / 豆包 / 计划任务命令（多应用扩展）
  src/commands_proxy.rs         本地代理命令（多应用扩展）
scripts/build-single.ps1      构建单一可执行文件（本项目新增）
```

### 测试

```bash
cargo test --workspace          # 核心逻辑 + 桌面端单元测试
npm run build                   # 前端类型检查与构建
```

> Windows x64 上实测 `cargo test --workspace` 全部通过（454 个核心用例 + 55 个网关用例）。
> 构建需要 **MSVC 工具链**（`stable-x86_64-pc-windows-msvc`，Tauri 依赖它链接
> WebView2）；若需安装，可用
> `winget install --id Microsoft.VisualStudio.2022.BuildTools --override "--quiet --add Microsoft.VisualStudio.Workload.VCTools --includeRecommended"`。

网关侧（Go）自带完整测试套件；应用补丁后：

```bash
cd path/to/workbuddy2api && go test ./...
```

> 网关侧测试已在 Windows x64 上实测，`go test ./...` 当前全部通过。

---

## 对上游的改动

本项目对 `workbuddy2api`（Go 网关）的改动**已直接合入 `go-gateway/`**，
随仓库一并分发（源码基线为上游 `cfb1713`，叠加下列改动）。

改动内容：

**到期分层选号**（`internal/pool/pool.go`，本补丁最重要的一处改动）

- `entry.expireAt` 保存账号「最近到期积分」的到期时刻；`expiryDayKey()` 按本地
  时区取到期日作为分层键
- `earliestExpiryTierLocked()`：只保留最早到期的一档；未知到期排最后；
  全员未知时不分档（回退原口径）
- `tierWeightOf()`：档内权重去掉 credits 项，只留闲置补偿 + 成功率
- `SetExpiry()` / `SetCreditsAndExpiry()` 由巡检回填到期日
- `Status.SoonestExpireAt` / `ExpireDay` 暴露给界面显示档位
- `stateAccount.ExpireAt` 持久化到期日，重启后立刻恢复分层

**到期日解析**（`internal/upstream/client.go`，分层选号的数据来源）

- `resourcePackage` 结构抽出，新增 `remain()` / `expiryUnix()`
- `parseExpiryUnix()`：到期字段在不同区域/套餐上出现过数字与字符串两种形态，
  故声明为 `any` 并逐一兼容 —— epoch 秒、epoch 毫秒、`2006-01-02 15:04:05`、
  RFC3339、纯日期（按当日 23:59:59 计）
- 优先取 `DeductionEndTime`（额度真正失效时刻），回退 `ExpiredTime` / `CycleEndTime`
- `UserResourceDetail()` 一次请求同时返回余额与最近到期时刻，不额外打上游；
  `UserResource()` 保留为薄封装
- **只有仍有剩余的套餐才计入到期压力**

**积分到期巡检**（`internal/scheduler/scheduler.go`、`cmd/server/main.go`）

- 新增 `RunCreditRefreshLoop` / `RunCreditRefreshNow` / `refreshCreditsWithGap`：
  周期性刷新余额与到期日（不签到、不解冻），账号间隔 300ms
- `RunCheckinNow` 改用 `UserResourceDetail`
- 新增 `DefaultCreditRefreshInterval = 15m` 与 `pool.credit_refresh_enabled` 开关

**`credit` 元数据的解析与写回**（`internal/auth/auth.go`）

- `Auth.SoonestExpireAt` 字段；`creditBlock` 解析凭证里的 `credit` 块（嵌套形与扁平形都支持）
- `normalizeEpoch()`：上游混用秒 / 毫秒，按量级统一成秒
- `SaveAtomic()` 保留 `credit` 块 —— 否则 token 刷新重写凭证时会丢掉到期信息

**国际版区域路由**（`internal/upstream/`）

- `client.go`：新增 `IsIntl()` 区域判定（按 `auth.Domain` 后缀）与 `BaseIntl` 字段；
  `chatBase()` / `billingBase()` 改为**按账号区域返回域名**
- `headers.go`：`Origin` / `Referer` 跟随账号区域
  （国服 `codebuddy.cn`、国际版 `workbuddy.ai`）

**国际版模型表与签到范围**（`internal/server/handler.go`、`cmd/server/config.go`）

- 新增国际版静态模型表，`/v1/models` 返回两区域并集
  （国际版的模型列表接口返回 500，无法动态拉取）
- 新增 `schedule.checkin_scope`（`cn` 缺省 / `all`）与 `checkinScopeAllows()`：
  网关侧签到与猫猫旅行默认**跳过国际版账号**；token 保活不受该开关限制

**Anthropic Messages 与 Responses 兼容层**（`internal/server/`，本项目新增）

- `POST /v1/messages`：Anthropic 请求与 SSE 双向转译为 OpenAI Chat，
  覆盖 system / tool_use / tool_result / thinking 与完整流式事件序列；
  客户端传入的 Claude 槽位名（`claude-sonnet-5` 等）自动翻译为上游模型名
- `POST /v1/responses`：Responses 事件序列完整（含 `response.output_item.done`，
  Codex 0.146 依赖该事件收录并显示回复）
- 抽出共享的「选号 → 轮换 → 转发」流程（`forward.go`），三种协议入口共用同一
  账号池调度、熔断冷却、会话粘性与用量统计

**出站脱敏增强**（`internal/upstream/sanitize.go`）

- 新增两条上游指纹（命中即 HTTP 400 code=11128）：
  Claude Code 2.1.260 系统提示中的官方仓库链接、Codex CLI instructions 中的
  "led by OpenAI" 归属句；均按「最小改写、语义不变」原则处理

补丁基于上游 `cfb1713` 生成，已验证可在更新的上游提交上干净应用并编译通过。

> 若你只使用国服，可跳过该补丁，功能与上游一致。

**请求体超限显式报错**（`internal/server/handler.go` 等三处协议入口）

修复长对话报 `unexpected EOF` 的问题（Issue #5 后续）：

- 原先 `readLimitedBody` 用 `io.LimitReader(8MB)` 读取，读满即返回，调用方无法区分
  「读完了」与「被截断了」。截断后的字节不是合法 JSON，`prepareBody` 解析失败后仍
  原样透传给上游，上游解码报 `11101 Unmarshal chat params failed with error:
  unexpected EOF` —— 客户端只看到「请求参数有误」，无法定位到是网关截断
- 现在上限提到 32MB（长对话很容易突破 8MB），并多读 1 字节判定越界；超限返回 **413**
  并说明原因，错误码按协议区分（OpenAI `payload_too_large` / Anthropic
  `request_too_large`），三个协议入口行为一致

**熔断期状态画像修正**（`internal/pool/pool.go`）

- `cool_remaining_sec` 原先只看 `until`、`cool_kind` 取可能早已失效的历史值，导致
  熔断中的账号显示成「冷却中 · 剩余 0 秒 · 余额不足」——即使它余额充足。现在按
  **真正决定恢复的那个截止**（两截止取较晚者）计算剩余与类型，熔断期显示 `breaker`
- 实现注记：`healthy()` 要求 `until` 与 `breakerUntil` **都**过期（AND 关系），故恢复
  时刻是**较晚**者；而既有 `expiry()` 取的是**较早**者（供全冷却兜底挑「最快有可能
  恢复」的号去试）。两者语义相反，故新增 `recoveryAt()` 而非复用 `expiry()`
**模型级限流识别与隔离**（`internal/upstream/client.go`、`internal/pool/pool.go`、`internal/server/`）

- 新增 `ErrModelRate` 分类与 `IsModelRateLimited()`：识别 `429 code=6004`（业务码为主、
  文案兜底，且**仅 429 下生效**，避免误判 5xx 里的同名字样）
- 新增 `ParseResetTime()`：从报错文案解析重置时刻，兼容 `UTC+8` / `UTC+08:00` / `UTC-5` /
  `Z` / 无时区（按本地），且只接受未来时刻（防重放旧日志写入已失效冷却）
- `pool` 新增按 `uid+model` 的冷却表：`CooldownModel()` / `PickForModel()` /
  `PickByUIDForModel()`，`Status.ModelCooling` 暴露给 `/status`；
  路由的普通选号、粘性命中、全冷却兜底三个入口**都**按模型过滤
- 冷却**不喂熔断器**（配额信号≠账号故障），并**持久化进 `state.json`**
- 前端账号行下方新增明细区，区分「余额欠费」与「模型冷却」并显示模型名 + 恢复时间
## 上游来源与许可证

本项目基于以下两个开源项目整合改造，**绝大部分代码来自上游**：

| 项目 | 作者 | 提供的部分 | 许可证 |
|---|---|---|---|
| [workbuddy-switch](https://github.com/changexbc/workbuddy-switch) | [changexbc](https://github.com/changexbc) | 桌面 GUI 外壳、账号管理、签到、积分与 Token 统计、托盘、CLI 切换等全部界面与核心逻辑 | **MIT** |
| [workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) | [Sliverkiss](https://github.com/Sliverkiss) | OpenAI 兼容网关（账号池轮转、熔断冷却、会话粘性、SSE 规范化、猫猫旅行等） | **MIT** |

两个上游项目均采用 **MIT 许可证**，允许使用、修改与再分发。本仓库已保留其原始
版权声明（见 [`LICENSE`](./LICENSE)），并在此基础上补充整合部分的版权声明。

> 本仓库是**独立整合作品**，与上述两个上游项目相互独立、各自演进。
> 上游的后续更新不会被自动合入；对网关的改动已直接体现在 `go-gateway/` 源码中。
> 本项目不代表上游作者的立场或背书。

整合部分（本项目新增）同样以 MIT 许可证发布。逐项来源说明与改动清单见
[`NOTICE`](./NOTICE)。

### 许可证

```
MIT License

Copyright (c) 2026 ai-gateway        （ai-gateway 原作者）
Copyright (c) 2026 Sliverkiss        （workbuddy2api 原作者）
Copyright (c) 2026 momo0410          （本项目整合部分）
```

完整条款见 [`LICENSE`](./LICENSE)。

---

## 赞赏

如果这个项目对你有帮助，欢迎请作者喝杯快乐水 ☕

<div align="center">
<img src="src/assets/donate-qr.jpg" alt="赞赏码" width="220" />
</div>

---

## 免责声明

- 本项目为**非官方**工具，与腾讯公司及 CodeBuddy / WorkBuddy 官方无任何关联。
- 项目通过 OAuth 设备授权使用**用户本人**的账号凭证，不提供、不托管任何账号。
- 使用本项目需遵守 CodeBuddy / WorkBuddy 的服务条款。因使用本项目产生的
  账号封禁、条款违约等风险由使用者自行承担。
- 本项目仅供学习与研究使用，请勿用于商业用途或大规模分发。
- 作者不对因使用本项目造成的任何直接或间接损失负责。

---

<div align="center">

如果这个项目对你有帮助，欢迎给上游项目点个 Star ⭐

[workbuddy-switch](https://github.com/changexbc/workbuddy-switch) ·
[workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)

</div>
