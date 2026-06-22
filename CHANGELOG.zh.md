# 更新日志

> [English](CHANGELOG.md) | **中文** | [日本語](CHANGELOG.ja.md)

本项目的所有重要变更都将记录在此文件中。

本格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
并且本项目遵循 [语义化版本](https://semver.org/lang/zh-CN/spec/v2.0.0.html)。

## [1.2.1] - 2026-06-22

### 修复
- `wget -qO- ... | sudo bash` 这类管道安装命令现在会在存在控制终端时打开交互式菜单。
- 管道交互运行时，安装脚本会从 `/dev/tty` 读取提示输入；完全非交互环境仍保持非交互安装路径。

## [1.2.0] - 2026-06-22

### 新增
- 为本地 `scripts/install.sh` 和 `tsunami-manage` 运行新增交互式部署管理菜单。
- 写入配置前展示配置摘要，并对密码做遮蔽显示。
- 为安装、重新配置、更新、卸载和删除配置流程新增确认提示。
- 新增安装脚本行为测试，覆盖菜单分发、确认处理、状态默认值和非交互兼容性。

### 变更
- 交互式终端中不带参数运行现在会打开管理菜单；管道/非 TTY 安装仍保持原有安装路径。
- 重新配置会复用之前 `/etc/tsunami/install.env` 中的值作为提示默认值，同时保持 `TSUNAMI_*` 环境变量优先。
- 部署文档补充菜单使用方式和显式命令模式说明。

## [1.1.0] - 2026-05-08

### 新增
- **uTLS 指纹伪装** — 客户端模拟 Chrome/Firefox/Safari TLS ClientHello 以击败 JA3/JA4 指纹检测（`--fingerprint` 参数）
- **Let's Encrypt 自动证书** — 一键部署，自动申请和续期证书（通过 certbot）
- **连接信息面板** — 安装脚本在部署完成后打印可直接使用的客户端命令
- `CHANGELOG.md`、`SECURITY.md`、`CONTRIBUTING.md`
- CI/CD：`golangci-lint`、`shellcheck`、`govulncheck` 安全扫描
- CI/CD：通过 Codecov 的测试覆盖率报告
- 构建脚本和文档现已纳入 Git 跟踪（`build/`）

### 变更
- CI/CD 流水线从 2 阶段升级到 4 阶段（检查 → 测试 → 安全 → 发布）
- 集成测试现在会阻止发布（移除 `continue-on-error`）
- `.gitignore` 精简 — 构建脚本/文档已跟踪，仅忽略构建输出
- 安装脚本重写，支持 Let's Encrypt、certbot 续期钩子和管理命令

### 修复
- CI/CD 发布任务中的 Tar 打包自包含风险
- 缺失的 LICENSE 文件（MIT）

### 安全
- 移除包含硬编码服务器凭据的脚本
- 在 CI/CD 流水线中添加 `govulncheck`

## [1.0.0] - 2026-05-07

### 新增
- 初始版本
- TLS 1.3 传输，ALPN `h2`，前向安全
- 强制多路复用（会话-流架构）
- Surge 自适应连接扩展（Layer 1 → Layer 2）
- 可编程服务端推送填充方案
- 认证失败时透明回退（反主动探测）
- UDP-over-TCP 中继（UoT v2）
- SOCKS5 和 HTTP 代理支持
- 一键 Linux 部署脚本（`install.sh`）
- 跨平台构建（linux/amd64、linux/arm64、windows/amd64、darwin/amd64、darwin/arm64）
- 集成测试套件（端到端、多流、认证失败、会话重用、大数据传输）

[1.2.1]: https://github.com/RavenholmAlpha/tsunami/compare/v1.2.0...v1.2.1
[1.2.0]: https://github.com/RavenholmAlpha/tsunami/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/RavenholmAlpha/tsunami/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/RavenholmAlpha/tsunami/releases/tag/v1.0.0
