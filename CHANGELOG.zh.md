# 更新日志

> [English](CHANGELOG.md) | **中文** | [日本語](CHANGELOG.ja.md)

本项目的所有重要变更都将记录在此文件中。

本格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
并且本项目遵循 [语义化版本](https://semver.org/lang/zh-CN/spec/v2.0.0.html)。

## [未发布]

### 修复
- 重新运行安装向导（`tsunami-manage config`）改不了任何设置。它先问“是否复用现有设置？”，默认是“是”，而选“是”会跳过全部问题，于是上一次部署的域名、TLS 方式和站点伪装被原样沿用。现在向导逐项提问，每一项以当前值为默认：按 Enter 保留，输入新值就修改。
- 对那个问题回答“否”也没用：站点伪装菜单总是预选“本地站点”，并把上一次的上游地址当默认，一路按 Enter 会把 remote 站点变成指向远程地址的 local 站点；fronting 路径和密钥还会重新生成，已发出去的客户端命令随之失效。现在菜单预选当前模式，上游默认值只在模式不变时提供，路径、密钥和密码都会保留。
- 任何一次重新配置都会把回退后端、Surge 上限和填充方案重置为默认值，因为它们从未写入 `install.env`。现在这些设置会被保存；由旧版安装脚本写出的部署，会在第一次重新运行时从 `config.json` 里恢复它们。
- 在管理菜单里打开状态或客户端信息，会把保存的设置加载进当前 shell，向导随后把它们误当成命令行指定的值，跳过 TLS 和站点两个问题。现在保存的设置单独读取，只有真正的环境变量才会跳过提问。
- 本地站点上游的提问在输入结束后会无限重复；现在连续十次空回答后放弃。
- 域名输错之后，下一次提示会丢失默认值，按 Enter 就选中了自签名测试模式。现在默认值会保留。

### 变更
- 自己设置的站点名称在更换域名后保留；由旧域名派生的名称会按新域名重新派生。
- 非交互式安装（`TSUNAMI_ASSUME_YES=1`，或没有终端的管道方式）行为不变：不读取上一次部署。

## [1.3.1] - 2026-11-03

### 变更
- 安装脚本不再自动探测本机封面站点。`local` 站点伪装现在必须显式填写上游 host:port（向导提示或 `TSUNAMI_SITE_UPSTREAM`），避免静默反代到非预期的本地服务。非交互模式下 `TSUNAMI_SITE_MODE=local` 缺少上游会直接失败，不再默认 `127.0.0.1:8080`。

## [1.3.0] - 2026-11-03

### 新增
- **站点伪装**：未认证的 fronting 请求按 Caddy `reverse_proxy` 的语义处理——`local` 反代到本机 HTTP 端点（同一 IP 上同时运行真实站点），`remote` 反代到其他网站（借内容，默认把 Host 改写为上游），`builtin` 保留内置页面。通过 `fronting.site`（`mode`、`upstream`、`preserve_host`、`insecure_skip_verify`）或新增的 `--front-site-mode` / `--front-site-upstream` / `--front-site-preserve-host` / `--front-site-insecure-skip-verify` 参数配置；旧的 `decoy_proxy` 键与 `--front-decoy-proxy` 参数保留为弃用别名。
- 反代响应保持前置层形态：剥离 fronting 认证头、强制 `Server` 为配置值、移除 `Alt-Svc`（不提供 HTTP/3）、透传 WebSocket 升级；上游失败时回退到内置页面并返回 200，不泄露代理错误。
- 安装脚本新增 `advanced` 命令 / `TSUNAMI_ADVANCED=1`，在不干扰快速流程的前提下开放详细配置（监听地址、fallback、Surge、自定义 fronting 值、padding 方案）。

### 变更
- 填充改为按**每条流**应用，而不是按会话：每条被代理的流都从自己的填充周期开始，因此每个新的内层 TLS 握手（客户端 ClientHello、服务端证书飞行）都会被掩盖，而不再只掩盖长连接上的第一次。
- 填充方案 v2 新增 `stream_bytes`（每流填充字节预算）、`ctl`（控制写入规则，整个会话生命周期持续生效）、方向性 `c<idx>`/`s<idx>` 覆盖，以及 `~min-max` pad-to 段：把整次写入作为单条记录填充到合理尺寸，而不是拆分。新的默认方案把 ClientHello 填充到 0.9–1.2 KiB、服务端飞行填充到 2.6–4.2 KiB、控制帧填充到 40–160 B。
- 客户端在 `SETTINGS` 帧中声明 `padding-v2=1`；服务端只向声明该能力的客户端推送 v2 方案文本，因此混合版本会话仍可正常工作。
- 客户端会话池不再保留永久空闲连接：`MinIdleSession` 默认改为 0，空闲会话在随机化超时后关闭；应用层心跳改为 25–55 秒随机并在有流量时不发送，不再固定 15 秒一次。
- 安装脚本加固：默认 TLS 模式为 Let's Encrypt；自签证书必须显式开启开发模式；校验域名/SNI；生成唯一的 fronting 路径、独立的 fronting secret 与派生的伪装站点名，并同时写入服务端配置和客户端命令；移除硬编码填充方案，改用服务端内置默认值。
- 新增 `security` 安装脚本命令（菜单项 9），运行部署安全自检，发现问题时返回非零。
- 安装脚本快速向导：全新交互式安装只询问"域名 → TLS 模式 → 站点伪装"，并以单次 "Press Enter to install" 确认结束；已有安装会提示复用保存的配置；本地真实站点上游通过 `ss`/`lsof` 自动探测；其余（监听地址、Surge、fallback、fronting path/secret、站点名）全部自动生成。`security` 自检现在也会报告站点伪装模式与上游。
- README、部署与填充文档已同步更新。

### 修复
- 安装脚本不再为公网部署静默生成自签证书、占位 SNI 或带 `--skip-verify` 的客户端命令。
- 填充不再在会话前 8 次写入后停止，导致后续流的内层 TLS 握手失去掩盖。
- WebSocket 前置现在确实只在 TLS ALPN 扩展中声明 `http/1.1`。此前 uTLS 的 ClientHello 补丁在握手前被静默丢弃，客户端会发送浏览器默认的 `h2,http/1.1` 却使用 HTTP/1.1，导致面向支持 HTTP/2 的服务端（包括 TSUNAMI 自己的服务端）的 WebSocket 隧道失败。新增了 WebSocket ALPN 覆盖与浏览器默认 ALPN 列表的回归测试。
- 前置层的重放保护改为**验签通过后**才写入 nonce，并给缓存加上限，未认证探测无法再无限增长缓存（内存 DoS）。同时服务端只使用单个前置密钥（显式 `fronting.secret`，或作为弃用回退的第一个静态用户密码），不再把每个用户密码都当作前置 key，消除了未认证请求的 O(用户数) HMAC 成本。
- 目标地址策略改为只解析一次并拨号校验后的 IP；补充拦截 CGNAT/云元数据、基准测试、组播、IPv6 zone、未指定/广播等网段，堵住 DNS rebinding/SSRF 绕过。UoT 使用同一套解析后校验逻辑。
- 每个会话默认最多 256 条并发流（`--max-streams` / `max_streams`），每流接收队列限制为 4 帧，中继缓冲降为 64 KiB；打开流后 10 秒内不发送目标地址会被关闭。大量空 SYN 无法再耗尽内存或 goroutine。

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

[未发布]: https://github.com/RavenholmAlpha/tsunami/compare/v1.2.1...HEAD
[1.2.1]: https://github.com/RavenholmAlpha/tsunami/compare/v1.2.0...v1.2.1
[1.2.0]: https://github.com/RavenholmAlpha/tsunami/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/RavenholmAlpha/tsunami/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/RavenholmAlpha/tsunami/releases/tag/v1.0.0
