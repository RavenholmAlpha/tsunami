# ワンクリックデプロイ

> [English](deployment.md) | [中文](deployment.zh.md) | **日本語**

本プロジェクトには `scripts/install.sh` にある Linux インストーラーが含まれています。`tsunami-server` をインストールし、`/etc/tsunami/config.json` を書き込み、systemd ユニットを作成し、サービスを開始し、すぐに使えるクライアントコマンドを表示します。

## 対話型インストール

GitHub から（推奨）：

```bash
# wget
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash

# curl
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash
```

ローカルソースチェックアウトから：

```bash
sudo bash scripts/install.sh
```

対話型ターミナルでコマンドなしに実行すると、管理メニューが開きます。メニューからデプロイする場合は **Install or reinstall service** を選び、すぐにインストールする場合は `sudo bash scripts/install.sh install` を実行します。

インストーラーが確認する項目：

- リッスンアドレス、デフォルト `:443`
- サーバードメイン（提供すると Let's Encrypt がトリガー）
- フォールバックバックエンド、オプション
- Surge 最大接続数と閾値

ドメインが提供された場合、インストーラーは certbot を使用して **Let's Encrypt 証明書を自動的にリクエスト**します。ドメインが指定されず証明書パスも設定されていない場合、サーバーは自己署名証明書を使用します。

## Let's Encrypt（自動 TLS）

インストール時にドメイン名を提供すると：

1. certbot が自動的にインストール（apt/yum/dnf/snap 経由）
2. `certbot certonly --standalone` を使用して証明書をリクエスト
3. TSUNAMI が更新時に停止/開始するように更新フックを設定
4. 証明書パスが `config.json` に書き込まれる

**要件：**
- ポート 80 が開放されている必要あり（HTTP-01 チャレンジ用）
- インストーラーを実行する前に DNS がこのサーバーを指している必要あり
- 更新は自動的に行われる（約 60 日ごと、ダウンタイム <10 秒）

### 非対話型 Let's Encrypt デプロイ

```bash
# wget を使用
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  TSUNAMI_PUBLIC_HOST=example.com \
  TSUNAMI_PASSWORD='change-this-password' \
  TSUNAMI_LETSENCRYPT=y \
  TSUNAMI_ACME_EMAIL=you@example.com \
  TSUNAMI_FALLBACK=127.0.0.1:8080 \
  sudo -E bash

# curl を使用
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  TSUNAMI_PUBLIC_HOST=example.com \
  TSUNAMI_PASSWORD='change-this-password' \
  TSUNAMI_LETSENCRYPT=y \
  TSUNAMI_ACME_EMAIL=you@example.com \
  TSUNAMI_FALLBACK=127.0.0.1:8080 \
  sudo -E bash
```

### 非対話型手動証明書デプロイ

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

特定のリリースをインストール：

```bash
sudo env TSUNAMI_VERSION=v1.2.3 bash scripts/install.sh
```

ソースチェックアウト内で実行する場合、Go が利用可能であれば現在のソースをビルドします。そうでなければ `build/linux-*`（存在する場合）を使用し、GitHub Releases にフォールバックします。

## 管理

リモートインストール後、管理スクリプトを一度ダウンロード：

```bash
sudo wget -qO /usr/local/bin/tsunami-manage \
  https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh \
  && sudo chmod +x /usr/local/bin/tsunami-manage
```

使用方法：

```bash
sudo tsunami-manage           # 対話型メニューを開く
sudo tsunami-manage status
sudo tsunami-manage config
sudo tsunami-manage update
sudo tsunami-manage restart
sudo tsunami-manage logs
sudo tsunami-manage client    # 接続情報パネルを表示
sudo tsunami-manage cert      # Let's Encrypt 証明書状態を表示
sudo tsunami-manage uninstall
```

明示的なコマンドは引き続き自動化に使用できます。対話型ターミナルで引数なしに実行するとメニューが開きます。

またはソースチェックアウトから：

```bash
sudo bash scripts/install.sh status
```

生成されるファイル：

```text
/usr/local/bin/tsunami-server
/usr/local/bin/tsunami-client
/etc/tsunami/config.json
/etc/tsunami/client-command.txt
/etc/tsunami/install.env
/etc/systemd/system/tsunami-server.service
```

`/etc/tsunami/config.json` には SHA-256 トークンハッシュのみが保存されます。生のクライアントパスワードは `/etc/tsunami/client-command.txt` と `/etc/tsunami/install.env` に書き込まれ、両ファイルのパーミッションは `0600` です。

## クライアント例

インストール後、接続情報パネルを表示：

```bash
sudo tsunami-manage client
```

パネルにはサーバーアドレス、パスワード、TLS モード、サービス状態、すぐにコピーできるクライアントコマンドが表示されます：

```
╔══════════════════════════════════════════════════════════════╗
║           TSUNAMI  デプロイ完了 ✓                            ║
╠══════════════════════════════════════════════════════════════╣
║  サーバー  : example.com:443                                 ║
║  パスワード: xK9f2m...8kPq                                  ║
║  TLS      : Let's Encrypt（自動更新）                        ║
║  状態     : ● 稼働中                                         ║
╠══════════════════════════════════════════════════════════════╣
║  クライアントコマンド：                                       ║
║  tsunami-client \                                           ║
║    --server example.com:443 \                               ║
║    --password 'xK9f2m...' \                                 ║
║    --sni example.com \                                      ║
║    --socks 127.0.0.1:1080 \                                 ║
║    --http 127.0.0.1:8080                                    ║
╚══════════════════════════════════════════════════════════════╝
```

## uTLS フィンガープリント

クライアントは [uTLS](https://github.com/refraction-networking/utls) を使用して、デフォルトで Chrome TLS ClientHello フィンガープリントを模倣します。これにより DPI システムには Chrome HTTPS トラフィックと区別がつかなくなります。

サポートされるフィンガープリント：

| フラグ値 | 説明 |
|:-----------|:------------|
| `chrome` | Chrome（デフォルト） |
| `firefox` | Firefox |
| `safari` | Safari |
| `random` | ランダム化 |
| `none` | 標準 Go crypto/tls（模倣なし） |

```bash
# Firefox フィンガープリントを使用
tsunami-client --server example.com:443 --password '...' --fingerprint firefox

# フィンガープリント模倣を無効化
tsunami-client --server example.com:443 --password '...' --fingerprint none
```

## 環境変数

| 変数 | デフォルト | 説明 |
|:---------|:--------|:------------|
| `TSUNAMI_LISTEN` | `:443` | サーバーリッスンアドレス |
| `TSUNAMI_PASSWORD` | *（自動生成）* | クライアントパスワード |
| `TSUNAMI_PUBLIC_HOST` | *（プロンプト）* | サーバードメイン/パブリックホスト名 |
| `TSUNAMI_CERT_FILE` | *（なし）* | 手動 TLS 証明書パス |
| `TSUNAMI_KEY_FILE` | *（なし）* | 手動 TLS 秘密鍵パス |
| `TSUNAMI_FALLBACK` | *（なし）* | フォールバック HTTP バックエンド |
| `TSUNAMI_LETSENCRYPT` | *（プロンプト）* | `y` で Let's Encrypt を自動承認 |
| `TSUNAMI_ACME_EMAIL` | *（なし）* | Let's Encrypt 通知メール |
| `TSUNAMI_VERSION` | `latest` | インストールするリリースバージョン |
| `TSUNAMI_ASSUME_YES` | *（なし）* | `1` ですべての対話プロンプトをスキップ |
