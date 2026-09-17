pub mod account;
// 账号记录：任务 / 积分 / Token 三类事件的统一追加式流水，供单账号记录视图查询。
pub mod account_records;
pub mod agent_import;
// 多应用档案表（5 应用 × 3 快照布局）：切换流程的表驱动参数源。
pub mod app_profile;
pub mod auth_file;
pub mod checkin;
// CLI 任务模式（--task-run <key>，刻意不启动 Tauri）
pub mod cli_task;
pub mod codebuddy_cli;
pub mod codebuddy_cn_ide;
pub mod config;
pub mod credit_usage;
pub mod credits;
// MITM 设备身份代理（hyper 协议栈自建，不依赖 Tauri）：CA 签发 / 抓包改写 /
// 豆包与 Trae 凭证捕获 / 系统代理编排 / OAuth 直连豁免 / WS 桥接。
pub mod device_proxy;
// 豆包：账号池与凭证 / 会话保活 / 会员额度 / 对话备份导出
pub mod doubao_account;
pub mod doubao_chats;
pub mod doubao_quota;
pub mod doubao_session;
pub mod export_import;
pub mod gateway;
pub mod gateway_embed;
// 数据目录迁移（~/.ai-gateway → ~/.wb-switch，复制式、幂等）
pub mod migrate_store;
pub mod oauth;
pub mod official_usage;
pub mod process;
pub mod refresh;
pub mod rotate;
// Windows 计划任务（Trae 签到 / 豆包保活 / 额度巡检）
pub mod scheduler;
pub mod session;
pub mod switch;
// 登录态切换器（Trae 系 icube 布局 / 豆包 chromium 布局）
pub mod switcher;
pub mod token_stats;
// Trae：账号库 / JWT 解析 / 设备指纹 / 签到 / 本机发现
pub mod trae_account;
pub mod trae_checkin;
pub mod trae_device;
pub mod trae_discover;
pub mod travel;
pub mod update;
pub mod vscode_cn_inject;
pub mod yaml_lite;
