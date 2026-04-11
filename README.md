# TSUNAMI

> 基于 TLS over TCP 的高性能抗审查代理协议

TSUNAMI 是一个面向高审查环境（主要针对 GFW）的代理协议，使用 Go 语言实现，目标融入 [mihomo](https://github.com/MetaCubeX/mihomo) 内核生态。

## 特性

- **TLS 1.3 / TCP** — 纯 TCP 传输, 伪装为标准 HTTPS 流量
- **可编程 Padding** — 服务端动态下发包长分布策略，无需客户端升级即可更换流量特征
- **强制多路复用** — Session-Stream 架构，多个代理连接共享 TLS 连接
- **Surge 分层拥塞控制** — 默认单连接 + 自动多连接分流，单端口 443，无包乱序
- **Fallback** — 认证失败透明回落到正常 Web 服务，抗主动探测
- **UDP-over-TCP** — 通过 sing-box UoT v2 协议支持 UDP 代理
- **简单部署** — 最少 3 行配置即可启动

## 快速开始

### 服务端

```bash
# 编译
go build -o tsunami-server ./cmd/tsunami-server/

# 运行
./tsunami-server \
  --listen :443 \
  --cert /path/to/cert.pem \
  --key /path/to/key.pem \
  --password "your-strong-password" \
  --fallback 127.0.0.1:8080
```

### 客户端 (mihomo 配置)

```yaml
proxies:
  - name: "tsunami-hk"
    type: tsunami
    server: your-server.example.com
    port: 443
    password: "your-strong-password"
    sni: your-server.example.com
    udp: true
    surge:
      max-connections: 4
      threshold: 8
```

## 架构

```
TCP Proxy ─→ Stream ─→ Session ─→ TLS 1.3 ─→ TCP
                         │
                  ┌──────┴──────┐
                  │ Session Pool │ ← Surge Controller
                  └─────────────┘
```

### 协议栈

| 层 | 职责 |
|:---|:---|
| TLS 1.3 | 加密、前向安全、ALPN 伪装 |
| Session | 帧格式 (7B 帧头)、命令调度、Padding |
| Stream | 多路复用、代理连接生命周期 |
| Surge | 自适应连接管理 (Layer 1/2) |

### Surge 分层设计

```
Layer 1 (默认): 所有 Stream → 1 个 TLS 连接
Layer 2 (自动):  并发 Stream > 8 → 自动开启多连接分流
                每个 Stream 不拆分，无包乱序
                最多 4 个 TLS 连接（可配置）
```

## 项目结构

```
tsunami/
├── cmd/tsunami-server/        # 服务端可执行文件
├── pkg/
│   ├── protocol/              # 协议核心 (帧、命令、认证、设置)
│   ├── padding/               # 可编程 Padding 系统
│   ├── mux/                   # 连接多路复用池
│   ├── surge/                 # Surge 分层控制器
│   ├── fallback/              # Fallback 回落处理
│   ├── uot/                   # UDP-over-TCP (UoT v2)
│   ├── transport/             # TLS/TCP 配置与调优
│   ├── client/                # 客户端 API
│   ├── server/                # 服务端实现
│   └── config/                # 配置文件加载
└── go.mod
```

## 测试

```bash
go test ./...
```

## 协议规范

完整协议规范见项目文档。

## 与其他协议的对比

| | TSUNAMI | AnyTLS | Trojan | Hysteria 2 |
|:---|:---:|:---:|:---:|:---:|
| 传输层 | TLS/TCP | TLS/TCP | TLS/TCP | QUIC/UDP |
| 多路复用 | ✅ 强制 | ✅ 强制 | ❌ | ✅ QUIC |
| 可编程 Padding | ✅ | ✅ | ❌ | ❌ |
| 自动多连接分流 | ✅ Surge | ❌ | ❌ | ❌ |
| Fallback | ✅ | ✅ | ✅ | ⚠️ |
| UDP 支持 | UoT v2 | UoT v2 | ❌ | ✅ 原生 |

## License

MIT
