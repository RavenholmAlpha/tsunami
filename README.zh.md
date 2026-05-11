<div align="center">

# 🌊 TSUNAMI

**基于 TLS 1.3 的高性能多路复用代理协议**

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue?style=flat-square)](LICENSE)
[![Release](https://img.shields.io/github/v/release/RavenholmAlpha/tsunami?style=flat-square&color=brightgreen)](https://github.com/RavenholmAlpha/tsunami/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/RavenholmAlpha/tsunami/ci-cd.yml?style=flat-square&label=CI)](https://github.com/RavenholmAlpha/tsunami/actions)
[![LINUX DO](https://img.shields.io/badge/LINUX%20DO-Community-blue?style=flat-square)](https://linux.do)

*与标准 HTTPS 在线路上完全一致 — DPI 只能看到一个普通的 TLS 1.3 连接。*

---

[English](README.md) | **中文** | [日本語](README.ja.md)

[特性](#特性) · [快速开始](#快速开始) · [部署](#一键部署) · [架构](#架构) · [文档](#文档)

</div>

## 为什么选择 TSUNAMI？

| 现有协议的问题 | TSUNAMI 的解决方案 |
|:------|:------|
| 在 TLS 之上再加自定义加密 — 重复开销，容易被 DPI 识别 | **仅使用 TLS 1.3** — 无自定义加密，与 HTTPS 线路完全一致 |
| 固定数据包大小和可预测的握手 | **可编程填充** — 服务端推送的每包大小分布 |
| 每个流一个连接 — 频繁握手，可检测模式 | **强制多路复用** — 所有流共享同一个 TLS 连接 |
| 主动探测可暴露协议身份 | **透明回退** — 认证失败时代理到真实 HTTP 后端 |

## 特性

<table>
<tr>
<td width="50%">

🔐 **TLS 1.3 传输**
纯 TCP + TLS 1.3，ALPN `h2`，前向安全。

🎭 **uTLS 指纹伪装**
模拟 Chrome / Firefox / Safari ClientHello，击败 JA3/JA4。

🧩 **强制多路复用**
会话-流架构。所有代理连接共享同一个 TLS 连接。

⚡ **Surge 自动扩展**
默认单连接；高负载时自动多连接。无数据包重排。

</td>
<td width="50%">

🛡️ **回退反探测**
认证失败 → 透明代理到真实 HTTP 后端。主动探测者只能看到正常网站。

📐 **可编程填充**
服务端推送的填充方案控制数据包大小和空闲保活。无需客户端更新即可更改。

🌐 **UDP-over-TCP**
通过多路复用流中的 UoT v2 帧进行 UDP 中继。

📦 **最少依赖**
纯 Go + `golang.org/x` + uTLS。单个静态二进制文件，跨平台编译。

</td>
</tr>
</table>

## 快速开始

### 一键部署

在任何 Linux 服务器上（Ubuntu / Debian / CentOS / RHEL）：

```bash
# wget
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash

# 或 curl
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash
```

**非交互式 Let's Encrypt 部署：**

```bash
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  TSUNAMI_PUBLIC_HOST=your-domain.com \
  TSUNAMI_LETSENCRYPT=y \
  TSUNAMI_ACME_EMAIL=you@example.com \
  sudo -E bash
```

安装完成后，脚本会打印可直接使用的客户端命令：

```
╔══════════════════════════════════════════════════════════════╗
║           TSUNAMI  部署完成 ✓                                ║
╠══════════════════════════════════════════════════════════════╣
║  服务器 : example.com:443                                    ║
║  密码   : xK9f2m...8kPq                                     ║
║  TLS    : Let's Encrypt（自动续期）                           ║
║  状态   : ● 运行中                                           ║
╠══════════════════════════════════════════════════════════════╣
║  客户端命令：                                                 ║
║  tsunami-client \                                           ║
║    --server example.com:443 \                               ║
║    --password '...' \                                       ║
║    --sni example.com \                                      ║
║    --socks 127.0.0.1:1080 \                                 ║
║    --http 127.0.0.1:8080                                    ║
╚══════════════════════════════════════════════════════════════╝
```

### 服务管理

```bash
# 安装管理脚本
sudo wget -qO /usr/local/bin/tsunami-manage \
  https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh \
  && sudo chmod +x /usr/local/bin/tsunami-manage

# 命令
sudo tsunami-manage status       # 服务状态
sudo tsunami-manage client       # 显示连接信息
sudo tsunami-manage update       # 更新到最新版本
sudo tsunami-manage config       # 重新配置
sudo tsunami-manage restart      # 重启服务
sudo tsunami-manage logs         # 跟踪日志
sudo tsunami-manage cert         # Let's Encrypt 证书状态
sudo tsunami-manage uninstall    # 删除所有内容
```

### 从源码构建

```bash
go build -trimpath -ldflags="-s -w" -o tsunami-server ./cmd/tsunami-server/
go build -trimpath -ldflags="-s -w" -o tsunami-client ./cmd/tsunami-client/
```

### 手动使用

<details>
<summary><b>服务端</b></summary>

```bash
# 最简配置 — 自动生成自签名证书
./tsunami-server \
  --listen :443 \
  --password "your-strong-password" \
  --fallback 127.0.0.1:8080

# 使用 TLS 证书
./tsunami-server \
  --listen :443 \
  --cert /path/to/cert.pem \
  --key /path/to/key.pem \
  --password "your-strong-password" \
  --fallback 127.0.0.1:8080

# 使用 JSON 配置
./tsunami-server --config /etc/tsunami/config.json
```

| 参数 | 默认值 | 描述 |
|:-----|:--------|:------------|
| `--listen` | `:443` | 服务器监听地址 |
| `--cert` | — | TLS 证书文件（PEM） |
| `--key` | — | TLS 私钥文件（PEM） |
| `--password` | *（必填）* | 认证密码 |
| `--fallback` | — | 回退 HTTP 后端 |
| `--config` | — | JSON 配置文件 |

</details>

<details>
<summary><b>客户端</b></summary>

```bash
./tsunami-client \
  --server your-server.com:443 \
  --password "your-strong-password" \
  --socks 127.0.0.1:1080 \
  --http 127.0.0.1:8080
```

| 参数 | 默认值 | 描述 |
|:-----|:--------|:------------|
| `--server` | *（必填）* | 服务器地址 `host:port` |
| `--password` | *（必填）* | 认证密码 |
| `--sni` | *（服务器主机名）* | TLS SNI 覆盖 |
| `--skip-verify` | `false` | 跳过 TLS 证书验证 |
| `--fingerprint` | `chrome` | `chrome` / `firefox` / `safari` / `random` / `none` |
| `--socks` | `127.0.0.1:1080` | 本地 SOCKS5 代理地址 |
| `--http` | `127.0.0.1:8080` | 本地 HTTP 代理地址 |
| `--max-connections` | `4` | 最大 TLS 连接数（Surge） |
| `--threshold` | `8` | Surge Layer 2 流阈值 |

</details>

<details>
<summary><b>验证</b></summary>

```bash
# 测试 SOCKS5
curl -x socks5h://127.0.0.1:1080 https://httpbin.org/ip

# 测试 HTTP 代理
curl -x http://127.0.0.1:8080 https://httpbin.org/ip

# 版本
./tsunami-server --version
./tsunami-client --version
```

</details>

## 架构

### 协议栈

| 层 | 职责 |
|:------|:--------------|
| **TLS 1.3** | 加密、前向安全、ALPN 协商 |
| **认证** | SHA-256 密码哈希 + 随机填充 → 恒定时间验证 |
| **会话** | 7 字节帧头、命令分发、填充引擎 |
| **流** | 多路复用代理连接、SOCKS5 风格寻址 |
| **Surge** | 自适应 Layer 1 → Layer 2 连接扩展 |

### 反检测

| 攻击向量 | 防御措施 |
|:------|:------|
| **DPI** | 纯 TLS 1.3 + ALPN `h2`，端口 443 |
| **JA3/JA4 指纹** | uTLS 模拟真实浏览器 ClientHello |
| **主动探测** | 认证失败时回退到真实 HTTP 后端 |
| **流量分析** | 可编程的服务端推送填充 |
| **时序攻击** | 恒定时间密码比较 |
| **连接指纹** | 默认单连接；仅在负载高时多连接 |

### Surge：自适应连接扩展

- **Layer 1**（默认）：所有流 → 1 个 TLS 连接
- **Layer 2**（自动）：并发流超过阈值 → 分布到最多 `max-connections` 个 TLS 连接
- 每个流固定在一个连接上 — 零数据包重排

## 项目结构

```
tsunami/
├── cmd/
│   ├── tsunami-server/        服务端二进制
│   └── tsunami-client/        客户端二进制（SOCKS5 + HTTP）
├── pkg/
│   ├── protocol/              线路格式、帧、认证、会话、流
│   ├── padding/               可编程填充引擎
│   ├── mux/                   会话池和多路复用
│   ├── surge/                 自适应连接扩展
│   ├── fallback/              认证失败回退处理器
│   ├── uot/                   UDP-over-TCP 中继（UoT v2）
│   ├── transport/             TLS/TCP 配置和调优
│   ├── proxy/                 SOCKS5 和 HTTP 代理服务器
│   ├── client/                客户端 API
│   ├── server/                服务端实现
│   ├── control/               控制面板（适配器/中间件/用户存储）
│   └── config/                配置加载
├── scripts/
│   └── install.sh             一键部署脚本
├── tests/                     集成测试
├── build/                     跨平台构建脚本
├── docs/                      设计文档
└── go.mod
```

## 环境变量

用于非交互式/自动化部署：

| 变量 | 默认值 | 描述 |
|:---------|:--------|:------------|
| `TSUNAMI_LISTEN` | `:443` | 服务器监听地址 |
| `TSUNAMI_PASSWORD` | *（自动生成）* | 客户端密码 |
| `TSUNAMI_PUBLIC_HOST` | *（交互输入）* | 服务器域名/公网主机名 |
| `TSUNAMI_CERT_FILE` | — | 手动 TLS 证书路径 |
| `TSUNAMI_KEY_FILE` | — | 手动 TLS 私钥路径 |
| `TSUNAMI_FALLBACK` | — | 回退 HTTP 后端地址 |
| `TSUNAMI_LETSENCRYPT` | *（交互输入）* | `y` 自动确认 Let's Encrypt |
| `TSUNAMI_ACME_EMAIL` | — | Let's Encrypt 通知邮箱 |
| `TSUNAMI_VERSION` | `latest` | 要安装的版本号（`v1.2.3`） |
| `TSUNAMI_ASSUME_YES` | — | `1` 跳过所有交互提示 |

## 文档

| 文档 | 描述 |
|:---------|:------------|
| [协议规范](docs/protocol.zh.md) | 线路格式、帧、命令、认证 |
| [填充方案](docs/padding.zh.md) | 可编程填充系统语法和配置 |
| [Surge 设计](docs/surge.zh.md) | 自适应连接扩展架构 |
| [内置前置层](docs/fronting.md) | 类 Caddy 的 HTTPS/HTTP2/WebSocket 前置模式 |
| [部署指南](docs/deployment.zh.md) | 一键安装、Let's Encrypt、环境变量 |
| [控制面板](docs/control-plane.zh.md) | 面板适配器、中间件、用户存储 |
| [构建系统](build/README.zh.md) | 跨平台构建脚本 |

## 测试

```bash
# 单元测试（带竞态检测）
go test -race ./pkg/...

# 集成测试
go test ./tests/...

# 所有测试
go test ./...
```

## 许可证

[MIT](LICENSE)

## 鸣谢

协议设计灵感来源于 [anytls-go](https://github.com/anytls/anytls-go)。
