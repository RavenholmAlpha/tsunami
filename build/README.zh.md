# TSUNAMI 构建系统

> [English](README.md) | **中文** | [日本語](README.ja.md)

`tsunami-server`、`tsunami-client` 和面板托管节点 `tsunami-node` 的自动化跨平台构建脚本。

## 输出结构

```
build/
├── v1.0.0/                      # 版本化发布
│   ├── linux-amd64/
│   │   ├── tsunami-server
│   │   ├── tsunami-client
│   │   └── tsunami-node
│   ├── linux-arm64/
│   │   ├── tsunami-server
│   │   ├── tsunami-client
│   │   └── tsunami-node
│   ├── windows-amd64/
│   │   ├── tsunami-server.exe
│   │   ├── tsunami-client.exe
│   │   └── tsunami-node.exe
│   ├── darwin-amd64/
│   │   ├── tsunami-server
│   │   ├── tsunami-client
│   │   └── tsunami-node
│   ├── darwin-arm64/
│   │   ├── tsunami-server
│   │   ├── tsunami-client
│   │   └── tsunami-node
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

## 发布打包（CI）

`.github/workflows/ci-cd.yml` 为 `build/targets.sh` 中的每个目标各开一个任务，完成构建、打包和冒烟测试。推送 `vX.Y.Z` 标签会发布 GitHub Release，附带全部压缩包和 `checksums.sha256`；标签含连字符（`v1.2.3-rc.1`）时发布为预发布版本。拉取请求和 `main` 会用临时版本号运行同样的任务，但不发布。

| 系列 | 目标 | 压缩包 |
|:--|:--|:--|
| Linux | `linux-amd64`、`linux-arm64`、`linux-386`、`linux-armv7` | `tsunami-<版本>-<目标>.tar.gz`，含 server、client、node |
| Windows | `windows-amd64`、`windows-386` | `tsunami-<版本>-<目标>.zip`，含 server、client、node |
| macOS | `darwin-amd64`、`darwin-arm64` | `tsunami-<版本>-<目标>.tar.gz`，含 server、client、node |
| OpenWrt | `openwrt-x86_64`、`openwrt-x86`、`openwrt-aarch64`、`openwrt-armv7` | `tsunami-<版本>-<目标>.tar.gz`，含 client、procd 启动脚本和配置示例，见 [docs/openwrt.md](../docs/openwrt.md) |

`linux-amd64` 和 `linux-arm64` 的压缩包名是 `scripts/install.sh` 与 `scripts/install-node.sh` 的下载依据，不能改动。

在本地执行同样的步骤（需要 Bash、Go 和 Python 3.8+）：

```bash
bash build/package.sh -v v1.2.3 -t openwrt-aarch64 -o dist
bash build/smoke.sh -t openwrt-aarch64 -v v1.2.3 dist/tsunami-v1.2.3-openwrt-aarch64.tar.gz
bash build/checksums.sh dist
```

`SOURCE_DATE_EPOCH` 固定所有时间戳。提交、Go 工具链和 zlib 相同时，重复打包得到的压缩包逐字节一致。宿主机能运行二进制时（原生运行，或 ARM 目标通过 `qemu-user`），冒烟测试会执行 `--version`，否则只检查内容和架构。

## 鸣谢

协议设计灵感来源于 [anytls-go](https://github.com/anytls/anytls-go)。
