# 控制面板适配

> [English](control-plane.md) | **中文** | [日本語](control-plane.ja.md)

本文档描述了将 TSUNAMI 节点连接到面板、控制台系统和未来订阅/客户端适配器的扩展点。

## 目标

- 保持 TSUNAMI 协议和中继代码独立于任何特定面板。
- 将来自 Xboard、V2board、自定义面板或本地文件的用户规范化为一个运行时模型。
- 通过中间件而非协议更改来允许特定适配器的行为。
- 支持动态用户有效性、每用户速度限制和使用量报告。

## 运行时流程

```text
面板或本地配置
  -> control.Adapter
  -> control.Middleware 链
  -> control.UserStore
  -> server.UserAuthenticator
  -> TSUNAMI 会话和流
```

适配器获取外部数据。中间件验证和转换数据。UserStore 原子性地应用完整或增量快照，这样活跃节点可以在不重建服务器的情况下刷新用户。

## 适配器契约

适配器实现：

```go
type Adapter interface {
    Name() string
    FetchSnapshot(ctx context.Context) (*Snapshot, error)
}
```

未来的适配器应放在协议代码之外。示例：

- `xboard.Adapter`：从 Xboard 获取用户、配额、过期时间和速度限制。
- `v2board.Adapter`：规范化 V2board 节点 API 载荷。
- `panel.Adapter`：连接到自定义 HTTPS 或 WebSocket 控制服务。
- `subscription.Adapter`：生成面向客户端的配置数据，而不更改服务器认证路径。

## 中间件契约

中间件实现：

```go
type Middleware interface {
    Name() string
    Apply(ctx context.Context, snapshot *Snapshot) error
}
```

使用中间件进行面板特定的清理和策略：

- 填充缺失的 ID 或名称
- 将面板速度单位转换为每秒字节数
- 过滤未分配到此节点的用户
- 验证唯一令牌哈希
- 执行节点级允许/拒绝规则

内置的 `NormalizeUsers` 和 `ValidateUsers` 中间件是有意通用的。

## 用户模型

`protocol.UserInfo` 现在支持面板导向的字段：

- `ID`、`Name`
- `Password` 用于本地/静态部署
- `TokenHash` 用于面板部署
- `Disabled`、`ExpiresAt`
- `Bandwidth`（Mbps）用于兼容性
- `SpeedLimitBps` 用于显式的每秒字节限制
- `QuotaBytes`、`UsedUploadBytes`、`UsedDownloadBytes`
- `MaxSessions`、`MaxDevices`
- `Metadata`

面板应优先使用 `TokenHash` 并避免发送明文密码。

## 流量钩子

服务器中继路径使用 `control.TrafficPolicy`。

- `UsageRecorder` 接收每用户上传/下载字节增量。
- `Limiter` 可以执行每用户全局速度限制。
- `UserLimiter` 是通用的每用户令牌桶。它首先使用 `SpeedLimitBps`，然后回退到 `Bandwidth`（Mbps）。

向面板报告使用量应实现为一个批量聚合增量并定期发送的记录器。

## 实现指南

从简单的翻译适配器开始。将面板特定的怪癖保留在中间件中。不要将 Xboard、V2board、Clash 或订阅细节添加到 `pkg/protocol` 或 `pkg/server`。

推荐的未来包：

```text
pkg/control/adapters/xboard
pkg/control/adapters/v2board
pkg/control/adapters/panel
pkg/control/subscription
```

TSUNAMI 服务器应继续仅消费 `UserAuthenticator` 和 `TrafficPolicy`。
