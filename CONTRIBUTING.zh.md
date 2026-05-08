# 贡献指南

> [English](CONTRIBUTING.md) | **中文** | [日本語](CONTRIBUTING.ja.md)

感谢你有兴趣为 TSUNAMI 做出贡献！本指南将帮助你入门。

## 开发环境设置

### 前提条件

- [Go 1.25+](https://go.dev/dl/)
- Git

### 构建

```bash
# 克隆
git clone https://github.com/RavenholmAlpha/tsunami.git
cd tsunami

# 构建
go build -trimpath -ldflags="-s -w" -o tsunami-server ./cmd/tsunami-server/
go build -trimpath -ldflags="-s -w" -o tsunami-client ./cmd/tsunami-client/

# 或使用 Makefile（跨平台）
cd build && make
```

### 测试

```bash
# 单元测试（带竞态检测）
go test -race ./pkg/...

# 集成测试
go test ./tests/...

# 所有测试
go test ./...

# 代码检查
go vet ./...
```

## 项目结构

```
tsunami/
├── cmd/                  CLI 入口点
│   ├── tsunami-server/
│   └── tsunami-client/
├── pkg/                  库包
│   ├── protocol/         线路格式、帧、认证、会话、流
│   ├── padding/          可编程填充引擎
│   ├── mux/              会话池和多路复用
│   ├── surge/            自适应连接扩展
│   ├── fallback/         认证失败回退处理器
│   ├── uot/              UDP-over-TCP 中继
│   ├── transport/        TLS/TCP 配置，uTLS 指纹伪装
│   ├── proxy/            SOCKS5 和 HTTP 代理服务器
│   ├── client/           客户端 API
│   ├── server/           服务端实现
│   ├── control/          控制面板（中间件、用户存储）
│   └── config/           配置加载
├── scripts/              部署脚本
├── tests/                集成测试
├── build/                跨平台构建脚本
└── docs/                 设计文档
```

## 提交更改

### Pull Request 流程

1. **Fork** 仓库
2. 从 `main` **创建分支**：`git checkout -b feat/my-feature`
3. **进行更改**，使用清晰的描述性提交信息
4. **运行测试**：`go test -race ./...`
5. **运行 vet**：`go vet ./...`
6. **推送**并打开 Pull Request

### 提交信息

使用 [Conventional Commits](https://www.conventionalcommits.org/) 规范：

```
feat: add UDP relay support
fix: resolve session leak on timeout
docs: update deployment guide
test: add padding scheme edge cases
refactor: simplify frame decoder
```

### 代码风格

- 遵循标准 Go 规范（`gofmt`、`go vet`）
- 保持包的专注性和松耦合
- 为新功能添加测试
- 保留现有注释和文档

## 报告问题

- 使用 [GitHub Issues](https://github.com/RavenholmAlpha/tsunami/issues)
- 对于**安全漏洞**，请参阅 [SECURITY.zh.md](SECURITY.zh.md)

## 许可证

通过贡献，你同意你的贡献将根据 [MIT 许可证](LICENSE) 进行许可。
