# XBoard 独立节点

`tsunami-node` 是 TSUNAMI 的独立节点后端。它参考 V2bX 的控制面轮询、守护与运维模式，但数据面直接运行 TSUNAMI，不嵌入、不修改、也不依赖 Xray、sing-box 或 V2bX。

```text
XBoard: 用户、套餐、节点配置、流量账本
                  |
     /api/v2/server/tsunami/*
                  |
       tsunami-node 独立守护进程
                  |
          TSUNAMI 私有协议数据面
```

面板不解析 TSUNAMI 帧或会话状态。节点从面板取得监听、TLS、前置层、用户和限制策略；本地仅保存如何联系面板和节点身份凭据。

## 快速安装

在 XBoard 为 TSUNAMI 节点调用 `issueEnrollment` 后，响应会返回一次性 token 和可直接执行的 `install_command`。该命令已经带上面板地址、节点 ID 与短期 token；无需在服务器上手工编辑配置。需要覆盖节点 ID 时，才设置 `TSUNAMI_NODE_ID`。

```bash
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install-node.sh \
  | sudo env \
      TSUNAMI_XBOARD_URL=https://panel.example.com \
      TSUNAMI_NODE_ID=42 \
      TSUNAMI_ENROLLMENT_TOKEN='<short-lived-one-time-token>' \
      bash
```

安装器会：

1. 安装 `/usr/local/bin/tsunami-node` 和 `/usr/local/bin/tsunami-node-manage`。
2. 写入 `/etc/tsunami/node.json` 与 `systemd` 服务。
3. 用短期 token 注册一次，获得长期 Bearer credential。
4. 以 `0600` 权限把长期 credential 原子写入 `/etc/tsunami/node.credential`。
5. 从配置文件移除 enrollment token 后重启节点。

因此一次性 token 不会在常规节点配置中长期保存。XBoard 只保存 credential 的 SHA-256 哈希，管理员可随时轮换或吊销它。

## 节点配置

`tsunami-node` 的本地配置只有 bootstrap 信息，不要添加 `server` 或 `users` 块。面板下发的数据面配置是唯一事实来源。

```json
{
  "xboard": {
    "base_url": "https://panel.example.com",
    "node_id": "42",
    "enrollment_token": "short-lived-one-time-token",
    "credential_file": "/etc/tsunami/node.credential",
    "sync_interval": "1m",
    "config_interval": "1m",
    "request_timeout": "15s"
  }
}
```

| 字段 | 默认值 | 说明 |
|:--|:--|:--|
| `xboard.base_url` | 必填 | XBoard 基地址，不含结尾 `/` |
| `xboard.node_id` | 必填 | TSUNAMI 节点 ID |
| `xboard.enrollment_token` | 首次必填 | 仅能注册一次的短期 token；已有 credential 后可删除 |
| `xboard.credential_file` | 建议 `/etc/tsunami/node.credential` | 由节点写入的长期凭据，权限应为 `0600` |
| `xboard.credential` | 可选 | 直接传入长期凭据，仅适合临时环境，不建议持久化到配置 |
| `xboard.sync_interval` | `1m` | 用户同步、在线 IP 上报和流量上报间隔 |
| `xboard.config_interval` | `sync_interval` | 节点数据面配置检查间隔 |
| `xboard.request_timeout` | `15s` | 单个面板 HTTP 请求超时 |

`tsunami-server` 继续服务静态用户配置；若其读取到 `xboard.base_url`，会退出并提示改用 `tsunami-node`。

## 管理命令

```bash
sudo tsunami-node-manage status
sudo tsunami-node-manage restart
sudo tsunami-node-manage logs
sudo tsunami-node-manage sync
sudo tsunami-node-manage update
sudo tsunami-node-manage config
sudo tsunami-node-manage uninstall
```

`sync` 向运行中的进程发送 `SIGHUP`，立即拉取配置、用户和待上报流量，不会因为正常同步而重启服务。

## 面板契约

这是 TSUNAMI 私有 XBoard 扩展的契约，不是对公开 XBoard API 的兼容层。

### 节点注册和鉴权

```http
POST /api/v2/server/tsunami/enroll
Content-Type: application/json

{
  "node_id": "42",
  "enrollment_token": "short-lived-one-time-token"
}
```

成功后返回长期凭据：

```json
{
  "node_id": "42",
  "credential": "long-lived-random-bearer-token"
}
```

后续受保护请求统一使用：

```http
Authorization: Bearer long-lived-random-bearer-token
```

接口如下：

| 方法 | 路径 | 用途 |
|:--|:--|:--|
| `POST` | `/api/v2/server/tsunami/enroll` | 以一次性 token 注册节点 |
| `GET` | `/api/v2/server/tsunami/config` | 获取数据面配置 |
| `GET` | `/api/v2/server/tsunami/users` | 获取完整用户快照 |
| `POST` | `/api/v2/server/tsunami/report` | 上报流量和在线 IP |
| `POST` | `/api/v2/server/tsunami/device/admit` | 会话握手准入 |
| `POST` | `/api/v2/server/tsunami/device/renew` | 刷新活跃会话租约 |
| `POST` | `/api/v2/server/tsunami/device/release` | 会话关闭时释放租约 |

`config` 和 `users` 使用 `ETag`。节点发送 `If-None-Match`；`304` 保留当前状态，`200` 才应用返回结果。特别地，`200 {"users":[]}` 是有效完整快照，节点会原子清空所有本地用户。

### 数据面配置

`GET /api/v2/server/tsunami/config` 的结构：

```json
{
  "revision": "sha256-revision",
  "config": {
    "listen": "0.0.0.0:443",
    "tls": {
      "cert_file": "/etc/letsencrypt/live/node/fullchain.pem",
      "key_file": "/etc/letsencrypt/live/node/privkey.pem"
    },
    "transport": { "mode": "h2", "path": "/assets/update" },
    "surge": { "mode": "auto", "max_connections": 4, "threshold": 8 },
    "fronting": {
      "enabled": true,
      "path": "/assets/update",
      "secret": "node-fronting-secret",
      "server_header": "Caddy",
      "site_name": "Welcome"
    },
    "fallback": "127.0.0.1:8080",
    "padding_scheme": "stop=8\\n0=30-30",
    "allow_all": false,
    "device_limit_mode": "ip"
  }
}
```

节点只在 `revision` 改变时关闭旧监听器、关闭连接并建立新实例。关闭连接会立即触发租约释放；进程异常时仍由 300 秒 lease TTL 兜底。

支持的 `transport.mode` 为 `raw`、`h2` 和 `websocket`。`h2`/`websocket` 必须启用 `fronting` 并提供 `fronting.secret`；`raw` 不允许启用 fronting。

### 用户、流量和在线设备

用户快照只包含节点执行鉴权所需字段：

```json
{
  "revision": "sha256-revision",
  "users": [
    {
      "id": "1001",
      "uuid": "0b43dd99-0df8-4c30-91be-030a5d49279d",
      "speed_limit": 100,
      "device_limit": 3
    }
  ]
}
```

节点使用 `SHA-256(uuid)` 作为 TSUNAMI 握手认证 hash，不接收或保存用户 UUID 的派生密码。`speed_limit` 按 XBoard 的 Mbps 转为每秒字节数。

流量与在线 IP 使用一个可重试的批次：

```json
{
  "report_id": "retry-safe-random-id",
  "traffic": { "1001": [8192, 16384] },
  "alive": { "1001": ["203.0.113.7", "2001:db8::7"] }
}
```

数组顺序为 `[upload, download]`。同一未确认流量批次重试时始终复用同一 `report_id`，XBoard 以其去重，避免超时重发造成重复计费。`alive` 只是 XBoard 在线设备展示状态，不承担严格准入职责。

## 设备限制与连接限制

设备限制在 TSUNAMI 握手成功、但服务器发送 `ServerSettings` 确认之前执行。面板故障、IP 异常或超过限制时，节点直接关闭会话，不向客户端发送成功确认。

`protocol_settings.security.device_limit_mode` 决定 XBoard 的 Redis 租约 slot 语义：

| 模式 | slot | 行为 |
|:--|:--|:--|
| `ip` | `ip:<normalized-ip>` | 宽松设备限制。同一用户、相同 IPv4/IPv6 归一化 IP 的多个 TSUNAMI 会话只占一个设备位。 |
| `session` | `session:<session-id>` | 严格限制。每条 TSUNAMI 会话均占一个设备位，同 IP 多会话也会消耗多个位，等同连接限制。 |

会话租约 TTL 为 300 秒，节点默认每 90 秒续租。会话正常结束、节点热更新或停止时会主动调用 release。IPv4、IPv6、`[IPv6]:port` 与 IPv4-mapped IPv6 均会归一化后再计数。

## 生命周期

1. 节点确保已注册，拉取初始 config 和 users。
2. 初始用户快照验证成功后，按远端 config 建立监听器。
3. 用户快照 `200` 原子更新，`304` 不修改当前用户；同步失败保留旧版本。
4. config revision 变化时替换整个数据面实例；未变化或 `304` 不重启。
5. 流量、在线 IP 和设备租约始终在数据面会话路径处理。
6. 正常停止时先停止轮询与监听、关闭活跃会话并释放 lease，最后尽力上报一次未确认流量。
