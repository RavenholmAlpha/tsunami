# TSUNAMI ビルドシステム

> [English](README.md) | [中文](README.zh.md) | **日本語**

`tsunami-server` と `tsunami-client` の自動化クロスプラットフォームビルドスクリプト。

## 出力構造

```
build/
├── v1.0.0/                      # バージョン付きリリース
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
├── v1.1.0/                      # 別のリリース
│   └── ...
├── build.ps1                    # Windows PowerShell スクリプト
├── build.sh                     # Linux/macOS Bash スクリプト
├── Makefile                     # Make ベースのビルド（Linux/macOS）
└── README.md
```

## クイックスタート

### Windows（PowerShell）

```powershell
# 全プラットフォームをビルド、バージョン 1.0.0
.\build.ps1 -Version 1.0.0

# 特定のプラットフォームのみビルド
.\build.ps1 -Version 1.0.0 -Platforms "linux-amd64,windows-amd64"

# クリーン + リビルド
.\build.ps1 -Version 1.0.0 -Clean
```

### Linux / macOS（Bash）

```bash
chmod +x build.sh

# 全プラットフォームをビルド、バージョン 1.0.0
./build.sh -v 1.0.0

# 特定のプラットフォームをビルド
./build.sh -v 1.0.0 -p linux-amd64,linux-arm64

# クリーン + リビルド
./build.sh -v 1.0.0 -c
```

### Makefile

```bash
# 全プラットフォームをビルド
make VERSION=1.0.0

# Linux ターゲットのみビルド
make VERSION=1.0.0 linux

# Windows ターゲットのみビルド
make VERSION=1.0.0 windows

# クリーン
make VERSION=1.0.0 clean
```

## サポートプラットフォーム

| GOOS    | GOARCH | ターゲット      |
|---------|--------|-----------------|
| linux   | amd64  | linux-amd64     |
| linux   | arm64  | linux-arm64     |
| windows | amd64  | windows-amd64   |
| darwin  | amd64  | darwin-amd64    |
| darwin  | arm64  | darwin-arm64    |

## ビルド機能

- **バージョン注入**：`main.version`、`main.commit`、`main.buildTime` が `-ldflags` 経由で埋め込み
- **ストリップバイナリ**：`-s -w` フラグでデバッグ情報を除去し小さなバイナリを生成
- **再現可能ビルド**：`-trimpath` で決定論的な出力を保証
- **SHA-256 チェックサム**：整合性検証用の `checksums.sha256` を自動生成
- **CGO 無効**：最大の移植性のための純粋な Go 静的バイナリ

## パラメータ

### PowerShell（`build.ps1`）

| パラメータ       | デフォルト                                                     | 説明                    |
|-----------------|----------------------------------------------------------------|-------------------------|
| `-Version`      | `dev`                                                          | セマンティックバージョン文字列 |
| `-Platforms`    | `linux-amd64,linux-arm64,windows-amd64,darwin-amd64,darwin-arm64` | カンマ区切りのターゲット |
| `-LDFlags`      | `""`                                                           | 追加の ldflags           |
| `-Clean`        | `$false`                                                       | ビルド前に出力を削除     |
| `-SkipChecksum` | `$false`                                                       | チェックサム生成をスキップ |

### Bash（`build.sh`）

| フラグ  | デフォルト                                                     | 説明                    |
|--------|----------------------------------------------------------------|-------------------------|
| `-v`   | `dev`                                                          | セマンティックバージョン文字列 |
| `-p`   | `linux-amd64,linux-arm64,windows-amd64,darwin-amd64,darwin-arm64` | カンマ区切りのターゲット |
| `-l`   | `""`                                                           | 追加の ldflags           |
| `-c`   | オフ                                                           | ビルド前にクリーン       |
| `-s`   | オフ                                                           | チェックサム生成をスキップ |

## リリースのパッケージング（CI）

`.github/workflows/ci-cd.yml` は `build/targets.sh` の各ターゲットについて、ビルド、パッケージング、スモークテストを 1 ジョブずつ実行します。`vX.Y.Z` タグをプッシュすると、すべてのアーカイブと `checksums.sha256` を添えた GitHub Release が公開されます。ハイフンを含むタグ（`v1.2.3-rc.1`）はプレリリースになります。プルリクエストと `main` では使い捨てのバージョンで同じジョブを実行し、公開はしません。

| 系統 | ターゲット | アーカイブ |
|:--|:--|:--|
| Linux | `linux-amd64`、`linux-arm64`、`linux-386`、`linux-armv7` | `tsunami-<バージョン>-<ターゲット>.tar.gz`（server、client、node） |
| Windows | `windows-amd64`、`windows-386` | `tsunami-<バージョン>-<ターゲット>.zip`（server、client、node） |
| macOS | `darwin-amd64`、`darwin-arm64` | `tsunami-<バージョン>-<ターゲット>.tar.gz`（server、client、node） |
| OpenWrt | `openwrt-x86_64`、`openwrt-x86`、`openwrt-aarch64`、`openwrt-armv7` | `tsunami-<バージョン>-<ターゲット>.tar.gz`（client、procd 起動スクリプト、設定例）。[docs/openwrt.md](../docs/openwrt.md) を参照 |

`linux-amd64` と `linux-arm64` のアーカイブ名は `scripts/install.sh` と `scripts/install-node.sh` がダウンロードに使うため、変更できません。

同じ手順をローカルで実行できます（Bash、Go、Python 3.8 以上が必要です）。

```bash
bash build/package.sh -v v1.2.3 -t openwrt-aarch64 -o dist
bash build/smoke.sh -t openwrt-aarch64 -v v1.2.3 dist/tsunami-v1.2.3-openwrt-aarch64.tar.gz
bash build/checksums.sh dist
```

`SOURCE_DATE_EPOCH` がすべてのタイムスタンプを固定します。コミット、Go ツールチェーン、zlib が同じなら、2 回パッケージングしても同一のアーカイブになります。ホストがバイナリを実行できる場合（ネイティブ、または ARM を `qemu-user` 経由で）、スモークテストは `--version` を実行します。それ以外は内容とアーキテクチャのみを確認します。

## 謝辞

プロトコル設計は [anytls-go](https://github.com/anytls/anytls-go) にインスパイアされました。
