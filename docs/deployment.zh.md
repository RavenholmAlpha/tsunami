# 一键部署

> [English](deployment.md) | **中文** | [日本語](deployment.ja.md)

本项目包含一个 Linux 安装脚本 `scripts/install.sh`。它会安装 `tsunami-server`、写入 `/etc/tsunami/config.json`、创建 systemd 服务单元、启动服务，并打印可直接使用的客户端命令。

## 交互式安装

从 GitHub 安装（推荐）：

```bash
# wget
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash

# curl
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash
```

从本地源码安装：

```bash
sudo bash scripts/install.sh
```

在交互式终端中，不带命令运行脚本会打开管理菜单。选择 **安装/重新安装服务** 即可从菜单部署；如果要跳过菜单直接安装，运行 `sudo bash scripts/install.sh install`。

安装程序会询问：

- 监听地址，默认 `:443`
- 服务器域名（提供后触发 Let's Encrypt）
- 回退后端，可选
- Surge 最大连接数和阈值

如果提供了域名，安装程序会使用 certbot **自动申请 Let's Encrypt 证书**。如果未提供域名且未设置证书路径，服务器使用自签名证书。

## Let's Encrypt（自动 TLS）

在安装时提供域名后：

1. 自动安装 certbot（通过 apt/yum/dnf/snap）
2. 使用 `certbot certonly --standalone` 申请证书
3. 配置续期钩子，TSUNAMI 在续期时停止/启动
4. 证书路径写入 `config.json`

**要求：**
- 端口 80 必须开放（用于 HTTP-01 验证）
- DNS 必须在运行安装程序之前指向此服务器
- 续期自动进行（约每 60 天，<10 秒停机时间）

### 非交互式 Let's Encrypt 部署

```bash
# 使用 wget
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  TSUNAMI_PUBLIC_HOST=example.com \
  TSUNAMI_PASSWORD='change-this-password' \
  TSUNAMI_LETSENCRYPT=y \
  TSUNAMI_ACME_EMAIL=you@example.com \
  TSUNAMI_FALLBACK=127.0.0.1:8080 \
  sudo -E bash

# 使用 curl
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  TSUNAMI_PUBLIC_HOST=example.com \
  TSUNAMI_PASSWORD='change-this-password' \
  TSUNAMI_LETSENCRYPT=y \
  TSUNAMI_ACME_EMAIL=you@example.com \
  TSUNAMI_FALLBACK=127.0.0.1:8080 \
  sudo -E bash
```

### 非交互式手动证书部署

```bash
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh \
  | sudo env \
      TSUNAMI_ASSUME_YES=1 \
      TSUNAMI_PUBLIC_HOST=example.com \
      TSUNAMI_LISTEN=:443 \
      TSUNAMI_PASSWORD='change-this-password' \
      TSUNAMI_CERT_FILE=/etc/letsencrypt/live/example.com/fullchain.pem \
      TSUNAMI_KEY_FILE=/etc/letsencrypt/live/example.com/privkey.pem \
      TSUNAMI_FALLBACK=127.0.0.1:8080 \
      bash
```

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
sudo tsunami-manage config
sudo tsunami-manage update
sudo tsunami-manage restart
sudo tsunami-manage logs
sudo tsunami-manage client    # 显示连接信息面板
sudo tsunami-manage cert      # 显示 Let's Encrypt 证书状态
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

`/etc/tsunami/config.json` 仅存储 SHA-256 令牌哈希。原始客户端密码写入 `/etc/tsunami/client-command.txt` 和 `/etc/tsunami/install.env`；两个文件的权限均为 `0600`。

## 客户端示例

安装后，显示连接信息面板：

```bash
sudo tsunami-manage client
```

面板显示服务器地址、密码、TLS 模式、服务状态和可直接复制的客户端命令：

```
╔══════════════════════════════════════════════════════════════╗
║           TSUNAMI  部署完成 ✓                                ║
╠══════════════════════════════════════════════════════════════╣
║  服务器  : example.com:443                                   ║
║  密码    : xK9f2m...8kPq                                    ║
║  TLS     : Let's Encrypt（自动续期）                         ║
║  状态    : ● 运行中                                          ║
╠══════════════════════════════════════════════════════════════╣
║  客户端命令：                                                 ║
║  tsunami-client \                                           ║
║    --server example.com:443 \                               ║
║    --password 'xK9f2m...' \                                 ║
║    --sni example.com \                                      ║
║    --socks 127.0.0.1:1080 \                                 ║
║    --http 127.0.0.1:8080                                    ║
╚══════════════════════════════════════════════════════════════╝
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
| `TSUNAMI_PUBLIC_HOST` | *（交互输入）* | 服务器域名/公网主机名 |
| `TSUNAMI_CERT_FILE` | *（无）* | 手动 TLS 证书路径 |
| `TSUNAMI_KEY_FILE` | *（无）* | 手动 TLS 私钥路径 |
| `TSUNAMI_FALLBACK` | *（无）* | 回退 HTTP 后端 |
| `TSUNAMI_LETSENCRYPT` | *（交互输入）* | `y` 自动确认 Let's Encrypt |
| `TSUNAMI_ACME_EMAIL` | *（无）* | Let's Encrypt 通知邮箱 |
| `TSUNAMI_VERSION` | `latest` | 要安装的版本 |
| `TSUNAMI_ASSUME_YES` | *（无）* | `1` 跳过所有交互提示 |
