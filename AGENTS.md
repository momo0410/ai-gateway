<!-- TRELLIS:START -->
# 编码契约（Coding Contracts）

These instructions are for AI assistants working in this project.

本仓库曾由 Trellis 工具初始化。Trellis 的工作流骨架（`workflow.md`、`workspace/`、
`spec/cli/`、`.agents/skills/`、`.codex/agents/`）与 spec 目录均未随仓库分发，
相关引用已于 2026-09-12 清理。

当前生效的编码约定见下方各节（UI Component Policy、Git Commit Language）。

## Shell Policy（强制）

- 本仓库运行环境为 Windows。所有 shell 命令**必须**显式使用 PowerShell（`pwsh` / `powershell`），
  禁止调用 `bash` / `sh` / `zsh` / WSL 及其启动器 `C:\WINDOWS\system32\bash.exe`。
- 调用命令行工具时必须显式指定 shell 为 PowerShell，不要依赖宿主默认值：默认值在
  仅安装了 WSL 启动器、未安装 Linux 发行版的机器上会落到 `bash` 并直接失败。
- 路径一律使用 Windows 形式（`D:\workbuddy2api\...`）、`\` 作为分隔符；环境变量用
  `$env:NAME`。禁止 `curl | sh`、`export VAR=...`、`/dev/null` 等 POSIX 写法。
- 脚本、构建、测试命令一律写成 PowerShell 形式（如 `Get-ChildItem`、`Remove-Item -LiteralPath`、
  `$LASTEXITCODE`）。仓库内已有 `scripts/*.ps1` 的，优先复用而不是重写为 shell 脚本。
- 若某工具确实只提供 POSIX 方式，先确认 `pwsh` 下无等价方案，再在 `AGENTS.md` 记录原因，
  不得静默改用 bash。

Token 统计与网关的接口契约可直接查阅实现本身：

- `crates/ai-gateway-core/src/modules/token_stats.rs` — Token 统计聚合
  （`get_statistics(days: Option<i64>)`）
- `crates/ai-gateway-server/src/api.rs` — HTTP 路由（含 `GET /api/token-stats`）
- `src-tauri/src/commands.rs` — 对应的 Tauri 命令包装

<!-- TRELLIS:END -->

## UI Component Policy

- For frontend UI, prefer the project's existing shadcn components and compose them before writing custom interactive primitives.
- If a required component is missing, add the matching shadcn/Radix component and wrap it under `src/components/ui/` so styling, accessibility, focus management, and behavior stay consistent.
- Write a custom component only when shadcn components and their composition APIs cannot satisfy the requirement. Record the reason before doing so.
- Custom UI must still reuse the project's Rhea theme tokens, spacing, radii, states, and accessibility conventions. Do not substitute native interactive shortcuts such as `details/summary` when an appropriate shadcn component exists.

## 构建与签名（Build & Signing）

构建带 updater 签名的安装包时，**不要再去搜索私钥**，位置与用法如下（固定不变）：

- 一条命令：`pwsh scripts/build-signed.ps1`（仅校验密钥不构建：加 `-CheckOnly`）
- 签名私钥：`%USERPROFILE%\.ai-gateway\ai-gateway-updater.key`（minisign 私钥）
- 私钥口令：`%USERPROFILE%\.ai-gateway\ai-gateway-updater.password`
- 两者都在**仓库外**，`.gitignore` 已排除 `*.key`；**本仓库是公开仓库，严禁把口令写入任何被 git 跟踪的文件。**

背景（改动相关代码前务必了解，否则会重复踩坑）：

- `src-tauri/tauri.conf.json` 的 `createUpdaterArtifacts` **一直为 `true`**，因此
  `tauri build` 必须拿到私钥；`tauri.conf.json` 里的 `plugins.updater.pubkey`
  必须与私钥配对（keyid `217C0E2B4321D841`），否则客户端会拒绝更新包。
- `TAURI_SIGNING_PRIVATE_KEY` 的值必须是密钥**内容**，不是路径。传路径会报
  `failed to decode base64 secret key: Invalid symbol 58`（路径里的冒号）。
  读取方式：`(Get-Content -LiteralPath $key -Raw).Trim()`。
- **缺口令时 `tauri signer sign` 不报错，而是阻塞等待 stdin 输入**，表现为"构建卡住"；
  口令错误才会秒级报错。所以非交互场景务必先确认口令可用（`-CheckOnly` 就是为此）。
- 加密私钥的 keynum 偏移（54..62）与公钥（2..10）不同，**不能直接比对**；
  判断配对是否正确的唯一可靠方式是实际签一次，再比签名与公钥的 keyid。

## 验证与测试（强制，血泪教训）

所有者本机**正在运行这套软件**：`:7864` 是他的网关，`:43120` 是他运行 AI 对话的
DSH Desktop。**任何验证都不得影响这两个进程。**

### 绝对禁止：运行安装包 / 卸载器

包括 `AI-Gateway_*_setup.exe` 与 `uninstall.exe`，**加了 `/S` 更危险**。

原因：Tauri 的 NSIS 模板按**可执行文件名**匹配并结束进程，不比对路径：

```nsis
nsis_tauri_utils::FindProcessCurrentUser "${executableName}"
nsis_tauri_utils::KillProcessCurrentUser "${executableName}"
IfSilent kill_${UniqueID} 0        ; 静默模式下不询问，直接杀
```

主程序名 `ai-gateway.exe` 与所有者正在运行的实例**同名**，因此
「装一次 / 卸一次」就会杀掉他的实例；而它是 Job Object（`KILL_ON_JOB_CLOSE`）
的父进程，父进程一死，`:7864` 网关被系统连带回收 —— 一次误操作打掉两个服务。

**验证安装包内容请用解包**：内嵌网关是 exe 内的一个 gzip 流，
定位 `1F 8B 08` 魔数后解压即可与源码编译结果逐字节比对，无需安装。

### 绝对禁止：按进程名批量结束进程

```powershell
# 禁止
Get-Process -Name "gateway*" | Stop-Process
```

清理只允许**PID + 路径双条件**：

```powershell
$proc = Get-CimInstance Win32_Process -Filter "ProcessId=$pid" -ErrorAction SilentlyContinue
if ($proc -and $proc.ExecutablePath -like "*test-bin*") { Stop-Process -Id $pid -Force }
```

### 验证一律用独立实例：独立名 + 独立端口 + 独立数据目录

**陷阱**：`crates/ai-gateway-server`（server，约 15MB）与 `src-tauri`（GUI，约 30MB）
的 `[[bin]] name` **都叫 `ai-gateway`**，输出到同一个 `target\release\ai-gateway.exe`
互相覆盖。直接用它做接口测试可能拿到 **GUI** —— GUI 带单实例插件，
发现所有者的实例在跑就**自己静默退出**（无输出、不监听），
极易误判成「代码启动即退出」。

```powershell
# 1) 用独立 target 目录构建 server 版，避免覆盖 GUI 产物
$tdir = "D:\WishProject\WorkbuddySwitchAPi\test-bin\build-server"
cargo build --release -p ai-gateway-server --target-dir $tdir
# 产物约 15MB = server 版；30MB 就是 GUI，别用

# 2) 改名，确保与所有者的进程都不同名
Copy-Item "$tdir\release\ai-gateway.exe" "D:\WishProject\WorkbuddySwitchAPi\test-bin\wb2api-server.exe"

# 3) 独立数据目录 + 独立端口（避开 7864 / 43120 / 57890，用 5789x）
$home1 = "D:\WishProject\WorkbuddySwitchAPi\test-bin\inst-xxx"
New-Item -ItemType Directory -Force -Path $home1 | Out-Null
$env:AI_GATEWAY_HOME = $home1
$p = Start-Process -FilePath "...\wb2api-server.exe" -ArgumentList "serve","--port","57899","--no-open" `
    -PassThru -WindowStyle Hidden -RedirectStandardOutput "$home1\o.log" -RedirectStandardError "$home1\e.log"
"PID=$($p.Id)" | Out-File "$home1\pid.txt" -Encoding ascii
```

Go 侧的 `gateway.exe` 不存在上述覆盖问题，可直接构建到临时目录使用。

### 前端/UI 验证不需要起后端

用 `uitest/page-harness.cjs` + `uitest/mock-host-api.cjs`
（静态服务 + mock 宿主 API + CDP 驱动真实 Chrome）。

### 每次验证前后都自检

```powershell
foreach ($port in @(43120, 7864)) {
    $c = Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue
    if (-not $c) { Write-Warning "所有者的 :$port 未在运行 —— 立即停止并报告" }
}
```

详见 `uitest/README-验证约定.md`。

## Git Commit Language

- Use Conventional Commit type prefixes such as `feat:`, `fix:`, and `docs:`.
- Write the commit subject and body in Chinese by default. Use English only when the user explicitly requests it.
