# 一键部署

> [English](deployment.md) | **中文** | [日本語](deployment.ja.md)

本项目包含一个 Linux 安装脚本 `scripts/install.sh`。它会安装 `tsunami-server`、写入 `/etc/tsunami/config.json`、创建 systemd 服务单元、启动服务，并打印可直接使用的客户端命令。

`scripts/install-node.sh` 是单独的 XBoard TSUNAMI 节点安装器。它安装 `tsunami-node`、写入 `/etc/tsunami/node.json`、注册长期节点凭据并创建 `tsunami-node.service`；不要把它与静态用户服务端安装器混用。节点的完整安装命令、凭据生命周期和设备限制说明见 [XBoard 独立节点](xboard-node.zh.md)。

## 交互式安装

从 GitHub 安装（推荐）：

```bash
# wget
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash

# curl
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash
```

在交互式终端中，这些管道命令会通过 `/dev/tty` 读取输入并打开管理菜单。在自动化或没有控制终端的环境中，同一命令仍会保持非交互安装路径，**必须显式指定 TLS 方式**（站点伪装在未设置 `TSUNAMI_SITE_MODE` 时默认使用内置页面；见下方环境变量）。未指定 TLS 方式时安装会以清晰的错误信息失败，而不会静默生成自签名证书。

从本地源码安装：

```bash
sudo bash scripts/install.sh
```

在交互式终端中，不带命令运行脚本会打开管理菜单。选择 **安装/重新安装（快捷）** 即可从菜单部署；如果要跳过菜单直接安装，运行 `sudo bash scripts/install.sh install`。

### 快捷向导

全新交互式安装只有三个问题和一次确认：

1. **域名** —— 必须解析到本服务器；默认提供机器 FQDN。IP 地址以及 `example.com`、`your-domain.example`、`localhost` 等占位/保留名称会被拒绝。
2. **TLS 方式** —— `[1] Let's Encrypt（推荐，默认）[2] 现有证书和私钥文件 [3] 自签名（不安全，仅测试）`。
3. **站点伪装** —— 未认证访问者看到的内容（见下一节）。
4. **按 Enter 安装** —— 唯一的确认步骤，随后打印客户端面板。

其余全部自动完成：监听 `:443`、Surge 默认值（4/8）、随机密码、空回退后端、生成的 fronting 路径/密钥/站点名，以及服务端内置填充方案。如果存在上一次部署的 `install.env`，向导会先提供 **复用现有设置** 并跳过问题。

安装程序**绝不会静默回退到自签名证书**。Let's Encrypt 模式在域名校验通过后使用 certbot 申请证书；手动模式使用你提供的证书路径；自签名模式会打印明确警告（“可被轻易识别、不提供 MITM 防护”）并要求确认。非交互运行时，自签名模式还额外要求 `TSUNAMI_TLS_MODE=self-signed TSUNAMI_DEV=1`。

#### 高级提示

旧的低层问题（监听地址、回退后端、Surge 最大连接数/阈值、自定义 fronting 路径/密钥/站点名、填充方案 JSON）只在 `TSUNAMI_ADVANCED=1` 或 `advanced` 命令下询问：

```bash
sudo bash scripts/install.sh advanced
sudo TSUNAMI_ADVANCED=1 bash scripts/install.sh config
```

快捷流程中所有 `TSUNAMI_*` 环境变量覆盖仍然有效，自动化无需使用高级提示。

## 站点伪装

Fronting 会为普通（未认证）请求提供 `fronting.site` 配置块。安装程序根据向导第三个问题自动写入：

| 模式 | 行为 | 适用场景 |
|:--|:--|:--|
| `local` | 反向代理到本服务器上的真实站点；默认上游 `http://127.0.0.1:8080`，会自动预选 80/8080/3000/8000 端口上的监听。`preserve_host: true` | 同一 IP 上有真实网站（推荐） |
| `remote` | 借用其他网站的内容；`preserve_host: false` 会把 Host 头重写为上游 | 本地没有真实站点，并接受上游站点的条款与可用性 |
| `builtin` | 生成的占位页面（最不真实，最容易被聚类识别） | 仅用于快速测试 |

写入 `/etc/tsunami/config.json` 的 JSON 结构：

```json
{
  "server": {
    "fronting": {
      "enabled": true,
      "path": "/static/js/main.1a2b3c4d.js",
      "secret": "0123456789abcdef0123456789abcdef",
      "site_name": "Example",
      "decoy_proxy": "",
      "site": {
        "mode": "local",
        "upstream": "http://127.0.0.1:8080",
        "preserve_host": true,
        "insecure_skip_verify": false
      }
    }
  }
}
```

环境变量覆盖：`TSUNAMI_SITE_MODE`、`TSUNAMI_SITE_UPSTREAM`、`TSUNAMI_SITE_PRESERVE_HOST`、`TSUNAMI_SITE_INSECURE_SKIP_VERIFY`。`TSUNAMI_DECOY_PROXY` 保留为旧版别名，映射为 `remote` + 上游。

## Let's Encrypt（自动 TLS）

选择 Let's Encrypt 并在安装时提供真实域名后：

1. 自动安装 certbot（通过 apt/yum/dnf/snap）
2. 使用 `certbot certonly --standalone` 申请证书
3. 配置续期钩子，TSUNAMI 在续期时停止/启动
4. 证书路径写入 `config.json`

**要求：**
- 端口 80 必须开放（用于 HTTP-01 验证）
- DNS 必须在运行安装程序之前指向此服务器
- 续期自动进行（约每 60 天，<10 秒停机时间）

### 非交互式 Let's Encrypt 部署

以下示例使用 `example.com` 作为占位符。请替换为你的真实域名；安装程序会拒绝保留/占位名称和 IP 地址。

```bash
# 使用 wget
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  TSUNAMI_PUBLIC_HOST=example.com \
  TSUNAMI_PASSWORD='change-this-password' \
  TSUNAMI_LETSENCRYPT=y \
  TSUNAMI_ACME_EMAIL=you@example.com \
  TSUNAMI_SITE_MODE=local \
  TSUNAMI_SITE_UPSTREAM=http://127.0.0.1:8080 \
  TSUNAMI_FALLBACK=127.0.0.1:8080 \
  sudo -E bash

# 使用 curl
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  TSUNAMI_PUBLIC_HOST=example.com \
  TSUNAMI_PASSWORD='change-this-password' \
  TSUNAMI_LETSENCRYPT=y \
  TSUNAMI_ACME_EMAIL=you@example.com \
  TSUNAMI_SITE_MODE=local \
  TSUNAMI_SITE_UPSTREAM=http://127.0.0.1:8080 \
  TSUNAMI_FALLBACK=127.0.0.1:8080 \
  sudo -E bash
```

### 非交互式手动证书部署

```bash
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh \
  | sudo env \
      TSUNAMI_ASSUME_YES=1 \
      TSUNAMI_TLS_MODE=manual \
      TSUNAMI_PUBLIC_HOST=example.com \
      TSUNAMI_LISTEN=:443 \
      TSUNAMI_PASSWORD='change-this-password' \
      TSUNAMI_CERT_FILE=/etc/letsencrypt/live/example.com/fullchain.pem \
      TSUNAMI_KEY_FILE=/etc/letsencrypt/live/example.com/privkey.pem \
      TSUNAMI_SITE_MODE=remote \
      TSUNAMI_SITE_UPSTREAM=https://www.wikipedia.org \
      TSUNAMI_FALLBACK=127.0.0.1:8080 \
      bash
```

### 非交互式自签名部署（仅限本地测试）

```bash
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  sudo env \
      TSUNAMI_ASSUME_YES=1 \
      TSUNAMI_TLS_MODE=self-signed \
      TSUNAMI_DEV=1 \
      TSUNAMI_PUBLIC_HOST=dev.tsunami.lan \
      TSUNAMI_PASSWORD='change-this-password' \
      bash
```

该模式在设计中就是不安全的：客户端命令会带上 `--skip-verify`，从而禁用证书校验且不提供任何 MITM 防护。切勿将自签名部署暴露到公网。

安装特定版本：

```bash
sudo env TSUNAMI_VERSION=v1.2.3 bash scripts/install.sh
```

在源码目录内运行时，如果 Go 可用，安装程序会构建当前源码。否则使用 `build/linux-*`（如果存在），然后回退到 GitHub Releases。

## 管理

远程安装后，下载管理脚本：

```bash
sudo wget -qO /usr/local/bin/tsunami-manage \
  https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh \
  && sudo chmod +x /usr/local/bin/tsunami-manage
```

然后使用：

```bash
sudo tsunami-manage           # 打开交互式菜单
sudo tsunami-manage status
sudo tsunami-manage config     # 快捷向导（可复用现有设置）
sudo tsunami-manage advanced   # 高级提示
sudo tsunami-manage update
sudo tsunami-manage restart
sudo tsunami-manage logs
sudo tsunami-manage client    # 显示连接信息面板
sudo tsunami-manage cert      # 显示 Let's Encrypt 证书状态
sudo tsunami-manage security  # 运行部署安全自检
sudo tsunami-manage uninstall
```

显式命令仍然适合自动化使用；交互式终端中不带参数运行会打开菜单。

或从源码目录：

```bash
sudo bash scripts/install.sh status
```

生成的文件：

```text
/usr/local/bin/tsunami-server
/usr/local/bin/tsunami-client
/etc/tsunami/config.json
/etc/tsunami/client-command.txt
/etc/tsunami/install.env
/etc/systemd/system/tsunami-server.service
```

`/etc/tsunami/config.json` 仅存储 SHA-256 令牌哈希。原始客户端密码写入 `/etc/tsunami/client-command.txt` 和 `/etc/tsunami/install.env`；配置目录权限为 `0700`，这些文件权限为 `0600`。生成的 fronting 路径/密钥、站点名称和 `server.fronting.site` 站点伪装块保存在 `config.json` 中。

`security` 命令会针对证书选择、SNI、fronting 状态、fronting 路径唯一性、独立 fronting 密钥、站点伪装模式与上游、客户端命令中的 `--skip-verify` 以及文件权限报告 PASS/WARN/FAIL。配置不安全时命令会以非零状态退出。具体来说：

- `local` 站点 → PASS，并提示同一 IP 应提供真实站点。
- `remote` 站点 → PASS，并提示注意上游站点的条款与可用性。
- `builtin` 站点 → WARN（“容易被聚类识别”）。
- `local`/`remote` 缺少上游或模式非法 → FAIL。

## 客户端示例

安装后，显示连接信息面板：

```bash
sudo tsunami-manage client
```

面板显示服务器地址、密码（面板中已脱敏）、TLS 模式、fronting 路径、站点伪装模式/上游、服务状态和可直接复制的客户端命令：

```
╔═══════════════════════════════════════════════╗
║ TSUNAMI Deploy Complete                       ║
║ Server       : example.com:443                ║
║ Password     : xK9f2m...8kPq                  ║
║ TLS          : Let's Encrypt (auto-renew)     ║
║ Fronting     : enabled                        ║
║ Front path   : /static/js/main.1a2b3c4d.js    ║
║ Site         : local -> http://127.0.0.1:8080 ║
║ Decoy site   : Example                        ║
║ Status       : running                        ║
║ Config       : /etc/tsunami/config.json       ║
╚═══════════════════════════════════════════════╝

Client command (copy-paste, also saved at /etc/tsunami/client-command.txt):
tsunami-client --server example.com:443 --password '...' --sni example.com --fronting --front-path /static/js/main.1a2b3c4d.js --front-secret '...' --socks 127.0.0.1:1080 --http 127.0.0.1:8080
```

## uTLS 指纹伪装

客户端使用 [uTLS](https://github.com/refraction-networking/utls) 默认模拟 Chrome TLS ClientHello 指纹。这使得连接对 DPI 系统来说与 Chrome HTTPS 流量无法区分。

支持的指纹：

| 参数值 | 描述 |
|:-----------|:------------|
| `chrome` | Chrome（默认） |
| `firefox` | Firefox |
| `safari` | Safari |
| `random` | 随机化 |
| `none` | 标准 Go crypto/tls（无伪装） |

```bash
# 使用 Firefox 指纹
tsunami-client --server example.com:443 --password '...' --fingerprint firefox

# 禁用指纹伪装
tsunami-client --server example.com:443 --password '...' --fingerprint none
```

## 环境变量

| 变量 | 默认值 | 描述 |
|:---------|:--------|:------------|
| `TSUNAMI_LISTEN` | `:443` | 服务器监听地址 |
| `TSUNAMI_PASSWORD` | *（自动生成）* | 客户端密码 |
| `TSUNAMI_PUBLIC_HOST` | *（交互输入）* | 真实公网域名；不能是 IP 或占位域名 |
| `TSUNAMI_TLS_MODE` | `letsencrypt` | `letsencrypt`、`manual` 或 `self-signed` |
| `TSUNAMI_DEV` | *（无）* | 非交互式自签名模式必须设为 `1` |
| `TSUNAMI_CERT_FILE` | *（无）* | 手动 TLS 证书路径 |
| `TSUNAMI_KEY_FILE` | *（无）* | 手动 TLS 私钥路径 |
| `TSUNAMI_FALLBACK` | *（无）* | 回退 HTTP 后端 |
| `TSUNAMI_LETSENCRYPT` | *（交互输入）* | `y` 非交互地选择 Let's Encrypt |
| `TSUNAMI_ACME_EMAIL` | *（无）* | Let's Encrypt 通知邮箱 |
| `TSUNAMI_FRONTING` | `1` | `0` 禁用 fronting（不适合公网部署） |
| `TSUNAMI_FRONT_PATH` | *（自动生成）* | fronting HTTP 路径，例如 `/assets/app.1a2b3c4d.js` |
| `TSUNAMI_FRONT_SECRET` | *（自动生成）* | 独立的 fronting HMAC 密钥 |
| `TSUNAMI_SITE_NAME` | *（自动派生）* | 生成页面显示的站点名称 |
| `TSUNAMI_SITE_MODE` | `builtin`（非交互） | 站点伪装：`local`、`remote` 或 `builtin` |
| `TSUNAMI_SITE_UPSTREAM` | `local` 默认 `http://127.0.0.1:8080` | `local`/`remote` 的上游 HTTP(S) 源站 |
| `TSUNAMI_SITE_PRESERVE_HOST` | `local` 默认 `true` | 是否保留访问者的 `Host` 头 |
| `TSUNAMI_SITE_INSECURE_SKIP_VERIFY` | `false` | 是否跳过 HTTPS 上游的 TLS 校验 |
| `TSUNAMI_DECOY_PROXY` | *（无）* | 旧版别名：映射为 `remote` + 上游 |
| `TSUNAMI_SNI` | `TSUNAMI_PUBLIC_HOST` | 客户端命令使用的 TLS SNI |
| `TSUNAMI_PADDING_SCHEME_JSON` | *（服务端默认值）* | 覆盖服务端默认填充方案 |
| `TSUNAMI_ADVANCED` | *（无）* | `1` 在向导中询问高级参数 |
| `TSUNAMI_VERSION` | `latest` | 要安装的版本 |
| `TSUNAMI_ASSUME_YES` | *（无）* | `1` 跳过所有交互提示 |
