# TSUNAMI 构建系统

> [English](README.md) | **中文** | [日本語](README.ja.md)

`tsunami-server` 和 `tsunami-client` 的自动化跨平台构建脚本。

## 输出结构

```
build/
├── v1.0.0/                      # 版本化发布
│   ├── linux-amd64/
│   │   ├── tsunami-server
│   │   └── tsunami-client
│   ├── linux-arm64/
│   │   ├── tsunami-server
│   │   └── tsunami-client
│   ├── windows-amd64/
│   │   ├── tsunami-server.exe
│   │   └── tsunami-client.exe
│   ├── darwin-amd64/
│   │   ├── tsunami-server
│   │   └── tsunami-client
│   ├── darwin-arm64/
│   │   ├── tsunami-server
│   │   └── tsunami-client
│   └── checksums.sha256
├── v1.1.0/                      # 另一个版本
│   └── ...
├── build.ps1                    # Windows PowerShell 脚本
├── build.sh                     # Linux/macOS Bash 脚本
├── Makefile                     # 基于 Make 的构建（Linux/macOS）
└── README.md
```

## 快速开始

### Windows（PowerShell）

```powershell
# 构建所有平台，版本 1.0.0
.\build.ps1 -Version 1.0.0

# 仅构建特定平台
.\build.ps1 -Version 1.0.0 -Platforms "linux-amd64,windows-amd64"

# 清理 + 重建
.\build.ps1 -Version 1.0.0 -Clean
```

### Linux / macOS（Bash）

```bash
chmod +x build.sh

# 构建所有平台，版本 1.0.0
./build.sh -v 1.0.0

# 构建特定平台
./build.sh -v 1.0.0 -p linux-amd64,linux-arm64

# 清理 + 重建
./build.sh -v 1.0.0 -c
```

### Makefile

```bash
# 构建所有平台
make VERSION=1.0.0

# 仅构建 Linux 目标
make VERSION=1.0.0 linux

# 仅构建 Windows 目标
make VERSION=1.0.0 windows

# 清理
make VERSION=1.0.0 clean
```

## 支持的平台

| GOOS    | GOARCH | 目标            |
|---------|--------|-----------------|
| linux   | amd64  | linux-amd64     |
| linux   | arm64  | linux-arm64     |
| windows | amd64  | windows-amd64   |
| darwin  | amd64  | darwin-amd64    |
| darwin  | arm64  | darwin-arm64    |

## 构建特性

- **版本注入**：`main.version`、`main.commit`、`main.buildTime` 通过 `-ldflags` 嵌入
- **精简二进制**：`-s -w` 标志移除调试信息以减小体积
- **可复现构建**：`-trimpath` 确保确定性输出
- **SHA-256 校验和**：自动生成 `checksums.sha256` 用于完整性验证
- **禁用 CGO**：纯 Go 静态二进制，最大可移植性

## 参数

### PowerShell（`build.ps1`）

| 参数            | 默认值                                                         | 描述                    |
|-----------------|----------------------------------------------------------------|-------------------------|
| `-Version`      | `dev`                                                          | 语义版本字符串          |
| `-Platforms`    | `linux-amd64,linux-arm64,windows-amd64,darwin-amd64,darwin-arm64` | 逗号分隔的目标平台   |
| `-LDFlags`      | `""`                                                           | 额外的 ldflags          |
| `-Clean`        | `$false`                                                       | 构建前删除输出          |
| `-SkipChecksum` | `$false`                                                       | 跳过校验和生成          |

### Bash（`build.sh`）

| 标志   | 默认值                                                         | 描述                    |
|--------|----------------------------------------------------------------|-------------------------|
| `-v`   | `dev`                                                          | 语义版本字符串          |
| `-p`   | `linux-amd64,linux-arm64,windows-amd64,darwin-amd64,darwin-arm64` | 逗号分隔的目标平台   |
| `-l`   | `""`                                                           | 额外的 ldflags          |
| `-c`   | 关闭                                                           | 构建前清理              |
| `-s`   | 关闭                                                           | 跳过校验和生成          |

## 鸣谢

协议设计灵感来源于 [anytls-go](https://github.com/anytls/anytls-go)。
