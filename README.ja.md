<div align="center">

# 🌊 TSUNAMI

**TLS 1.3 ベースの高性能多重化プロキシプロトコル**

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue?style=flat-square)](LICENSE)
[![Release](https://img.shields.io/github/v/release/RavenholmAlpha/tsunami?style=flat-square&color=brightgreen)](https://github.com/RavenholmAlpha/tsunami/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/RavenholmAlpha/tsunami/ci-cd.yml?style=flat-square&label=CI)](https://github.com/RavenholmAlpha/tsunami/actions)
[![LINUX DO](https://img.shields.io/badge/LINUX%20DO-Community-blue?style=flat-square)](https://linux.do)

*標準的な HTTPS とワイヤ上で完全に同一 — DPI からは通常の TLS 1.3 接続にしか見えません。*

---

[English](README.md) | [中文](README.zh.md) | **日本語**

[機能](#機能) · [クイックスタート](#クイックスタート) · [デプロイ](#ワンクリックデプロイ) · [アーキテクチャ](#アーキテクチャ) · [ドキュメント](#ドキュメント)

</div>

## なぜ TSUNAMI？

| 既存プロトコルの問題 | TSUNAMI のアプローチ |
|:------|:------|
| TLS 上にカスタム暗号化 — 二重のオーバーヘッド、DPI で容易に検出 | **TLS 1.3 のみ** — カスタム暗号なし、HTTPS と同一 |
| 固定パケットサイズと予測可能なハンドシェイク | **プログラマブルパディング** — サーバープッシュのパケットサイズ分布 |
| ストリームごとに 1 接続 — 頻繁なハンドシェイク、検出可能なパターン | **強制多重化** — すべてのストリームが 1 つの TLS 接続を共有 |
| アクティブプロービングでプロトコル識別 | **透過的フォールバック** — 認証失敗時は実際の HTTP バックエンドにプロキシ |

## 機能

<table>
<tr>
<td width="50%">

🔐 **TLS 1.3 トランスポート**
純粋な TCP + TLS 1.3、ALPN `h2`、前方秘匿性。

🎭 **uTLS フィンガープリント**
Chrome / Firefox / Safari の ClientHello を模倣。JA3/JA4 を無効化。

🧩 **強制多重化**
セッション-ストリームアーキテクチャ。すべてのプロキシ接続が 1 つの TLS 接続を共有。

⚡ **Surge 自動スケーリング**
デフォルトは単一接続、高負荷時に自動的に複数接続へ。パケットの並べ替えなし。

</td>
<td width="50%">

🛡️ **フォールバック耐プローブ**
認証失敗 → 実際の HTTP バックエンドに透過プロキシ。プローバーには通常の Web サイトに見える。

📐 **プログラマブルパディング**
サーバープッシュのパディングスキームがパケットサイズとアイドルキープアライブを制御。クライアント更新不要。

🌐 **UDP-over-TCP**
多重化ストリーム内の UoT v2 フレーミングによる UDP リレー。

📦 **最小依存関係**
純粋な Go + `golang.org/x` + uTLS。単一の静的バイナリ、どこでもクロスコンパイル可能。

</td>
</tr>
</table>

## クイックスタート

### ワンクリックデプロイ

任意の Linux サーバー（Ubuntu / Debian / CentOS / RHEL）で：

```bash
# wget
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash

# または curl
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash
```

**非対話型 Let's Encrypt デプロイ：**

```bash
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  TSUNAMI_PUBLIC_HOST=your-domain.com \
  TSUNAMI_LETSENCRYPT=y \
  TSUNAMI_ACME_EMAIL=you@example.com \
  sudo -E bash
```

インストール後、スクリプトはすぐに使えるクライアントコマンドを表示します：

```
╔══════════════════════════════════════════════════════════════╗
║           TSUNAMI  デプロイ完了 ✓                            ║
╠══════════════════════════════════════════════════════════════╣
║  サーバー : example.com:443                                  ║
║  パスワード: xK9f2m...8kPq                                  ║
║  TLS     : Let's Encrypt（自動更新）                         ║
║  状態    : ● 稼働中                                          ║
╠══════════════════════════════════════════════════════════════╣
║  クライアントコマンド：                                       ║
║  tsunami-client \                                           ║
║    --server example.com:443 \                               ║
║    --password '...' \                                       ║
║    --sni example.com \                                      ║
║    --socks 127.0.0.1:1080 \                                 ║
║    --http 127.0.0.1:8080                                    ║
╚══════════════════════════════════════════════════════════════╝
```

### サービス管理

```bash
# 管理スクリプトのインストール
sudo wget -qO /usr/local/bin/tsunami-manage \
  https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh \
  && sudo chmod +x /usr/local/bin/tsunami-manage

# コマンド
sudo tsunami-manage status       # サービス状態
sudo tsunami-manage client       # 接続情報を表示
sudo tsunami-manage update       # 最新版に更新
sudo tsunami-manage config       # 再設定
sudo tsunami-manage restart      # サービス再起動
sudo tsunami-manage logs         # ログ追跡
sudo tsunami-manage cert         # Let's Encrypt 証明書状態
sudo tsunami-manage uninstall    # すべて削除
```

### ソースからビルド

```bash
go build -trimpath -ldflags="-s -w" -o tsunami-server ./cmd/tsunami-server/
go build -trimpath -ldflags="-s -w" -o tsunami-client ./cmd/tsunami-client/
```

### 手動使用

<details>
<summary><b>サーバー</b></summary>

```bash
# 最小構成 — 自己署名証明書を自動生成
./tsunami-server \
  --listen :443 \
  --password "your-strong-password" \
  --fallback 127.0.0.1:8080

# 自己署名モードはローカルテスト専用です。公開環境では実ドメイン証明書と fronting を使ってください。

# 公開環境: 実ドメイン証明書 + fronting
./tsunami-server \
  --listen :443 \
  --cert /path/to/cert.pem \
  --key /path/to/key.pem \
  --password "your-strong-password" \
  --fronting \
  --front-decoy-proxy http://127.0.0.1:8080

# JSON 設定を使用
./tsunami-server --config /etc/tsunami/config.json
```

| フラグ | デフォルト | 説明 |
|:-----|:--------|:------------|
| `--listen` | `:443` | サーバーリッスンアドレス |
| `--cert` | — | TLS 証明書ファイル（PEM） |
| `--key` | — | TLS 秘密鍵ファイル（PEM） |
| `--password` | *（必須）* | 認証パスワード |
| `--fronting` | `false` | 公開リスナーを HTTPS/HTTP2/WebSocket fronting として動作させる |
| `--front-decoy-proxy` | — | 未認証 fronting リクエスト用の任意 HTTP(S) デコイ origin |
| `--fallback` | — | フォールバック HTTP バックエンド |
| `--config` | — | JSON 設定ファイル |

</details>

<details>
<summary><b>クライアント</b></summary>

```bash
./tsunami-client \
  --server your-server.com:443 \
  --password "your-strong-password" \
  --socks 127.0.0.1:1080 \
  --http 127.0.0.1:8080
```

| フラグ | デフォルト | 説明 |
|:-----|:--------|:------------|
| `--server` | *（必須）* | サーバーアドレス `host:port` |
| `--password` | *（必須）* | 認証パスワード |
| `--sni` | *（サーバーホスト）* | TLS SNI オーバーライド |
| `--skip-verify` | `false` | TLS 証明書検証をスキップ |
| `--fingerprint` | `chrome` | `chrome` / `firefox` / `safari` / `random` / `none` |
| `--socks` | `127.0.0.1:1080` | ローカル SOCKS5 プロキシアドレス |
| `--http` | `127.0.0.1:8080` | ローカル HTTP プロキシアドレス |
| `--max-connections` | `4` | 最大 TLS 接続数（Surge） |
| `--threshold` | `8` | Surge Layer 2 ストリーム閾値 |

</details>

<details>
<summary><b>検証</b></summary>

```bash
# SOCKS5 テスト
curl -x socks5h://127.0.0.1:1080 https://httpbin.org/ip

# HTTP プロキシテスト
curl -x http://127.0.0.1:8080 https://httpbin.org/ip

# バージョン
./tsunami-server --version
./tsunami-client --version
```

</details>

## アーキテクチャ

### プロトコルスタック

| レイヤー | 責務 |
|:------|:--------------|
| **TLS 1.3** | 暗号化、前方秘匿性、ALPN ネゴシエーション |
| **認証** | SHA-256 パスワードハッシュ + ランダムパディング → 定時間検証 |
| **セッション** | 7 バイトフレームヘッダー、コマンドディスパッチ、パディングエンジン |
| **ストリーム** | 多重化プロキシ接続、SOCKS5 スタイルアドレッシング |
| **Surge** | アダプティブ Layer 1 → Layer 2 接続スケーリング |

### 耐検出

| 攻撃ベクター | 防御 |
|:------|:------|
| **DPI** | 純粋な TLS 1.3 + ALPN `h2`、ポート 443 |
| **JA3/JA4 フィンガープリント** | uTLS で実際のブラウザ ClientHello を模倣 |
| **アクティブプロービング** | 認証失敗時に実際の HTTP バックエンドにフォールバック |
| **トラフィック分析** | プログラマブルなサーバープッシュパディング |
| **タイミング攻撃** | 定時間パスワード比較 |
| **接続フィンガープリント** | デフォルト単一接続、高負荷時のみ複数接続 |

### Surge：アダプティブ接続スケーリング

- **Layer 1**（デフォルト）：すべてのストリーム → 1 つの TLS 接続
- **Layer 2**（自動）：同時ストリーム数が閾値を超過 → 最大 `max-connections` の TLS 接続に分散
- 各ストリームは 1 つの接続に固定 — パケットの並べ替えゼロ

## プロジェクト構造

```
tsunami/
├── cmd/
│   ├── tsunami-server/        サーバーバイナリ
│   └── tsunami-client/        クライアントバイナリ（SOCKS5 + HTTP）
├── pkg/
│   ├── protocol/              ワイヤフォーマット、フレーム、認証、セッション、ストリーム
│   ├── padding/               プログラマブルパディングエンジン
│   ├── mux/                   セッションプール & 多重化
│   ├── surge/                 アダプティブ接続スケーリング
│   ├── fallback/              認証失敗フォールバックハンドラー
│   ├── uot/                   UDP-over-TCP リレー（UoT v2）
│   ├── transport/             TLS/TCP 設定 & チューニング
│   ├── proxy/                 SOCKS5 & HTTP プロキシサーバー
│   ├── client/                クライアントサイド API
│   ├── server/                サーバー実装
│   ├── control/               コントロールプレーン（アダプター/ミドルウェア/ユーザーストア）
│   └── config/                設定読み込み
├── scripts/
│   └── install.sh             ワンクリックデプロイスクリプト
├── tests/                     統合テスト
├── build/                     クロスプラットフォームビルドスクリプト
├── docs/                      設計ドキュメント
└── go.mod
```

## 環境変数

非対話型/自動デプロイ用：

| 変数 | デフォルト | 説明 |
|:---------|:--------|:------------|
| `TSUNAMI_LISTEN` | `:443` | サーバーリッスンアドレス |
| `TSUNAMI_PASSWORD` | *（自動生成）* | クライアントパスワード |
| `TSUNAMI_PUBLIC_HOST` | *（プロンプト）* | サーバードメイン/パブリックホスト名 |
| `TSUNAMI_CERT_FILE` | — | 手動 TLS 証明書パス |
| `TSUNAMI_KEY_FILE` | — | 手動 TLS 秘密鍵パス |
| `TSUNAMI_FALLBACK` | — | フォールバック HTTP バックエンドアドレス |
| `TSUNAMI_LETSENCRYPT` | *（プロンプト）* | `y` で Let's Encrypt を自動承認 |
| `TSUNAMI_ACME_EMAIL` | — | Let's Encrypt 通知メール |
| `TSUNAMI_VERSION` | `latest` | インストールするリリースバージョン（`v1.2.3`） |
| `TSUNAMI_ASSUME_YES` | — | `1` ですべての対話プロンプトをスキップ |

## ドキュメント

| ドキュメント | 説明 |
|:---------|:------------|
| [プロトコル仕様](docs/protocol.ja.md) | ワイヤフォーマット、フレーム、コマンド、認証 |
| [パディングスキーム](docs/padding.ja.md) | プログラマブルパディングシステムの構文と設定 |
| [Surge 設計](docs/surge.ja.md) | アダプティブ接続スケーリングアーキテクチャ |
| [デプロイガイド](docs/deployment.ja.md) | ワンクリックインストール、Let's Encrypt、環境変数 |
| [コントロールプレーン](docs/control-plane.ja.md) | パネルアダプター、ミドルウェア、ユーザーストア |
| [ビルドシステム](build/README.ja.md) | クロスプラットフォームビルドスクリプト |

## テスト

```bash
# ユニットテスト（競合状態検出付き）
go test -race ./pkg/...

# 統合テスト
go test ./tests/...

# 全テスト
go test ./...
```

## ライセンス

[MIT](LICENSE)

## 謝辞

プロトコル設計は [anytls-go](https://github.com/anytls/anytls-go) にインスパイアされました。
