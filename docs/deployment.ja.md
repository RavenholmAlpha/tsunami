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

対話型ターミナルで実行した場合、これらのパイプコマンドは `/dev/tty` から入力を読み取り、管理メニューを開きます。自動化環境や制御端末がない環境では、同じコマンドは従来どおり非対話インストール経路を維持しますが、**TLS 方式の明示が必須**です（サイトカモフラージュは `TSUNAMI_SITE_MODE` 未指定時に内蔵ページへ既定します。下記環境変数を参照）。TLS 方式がない場合は自己署名証明書を暗黙に生成せず、明確なエラーで失敗します。

ローカルソースチェックアウトから：

```bash
sudo bash scripts/install.sh
```

対話型ターミナルでコマンドなしに実行すると、管理メニューが開きます。メニューからデプロイする場合は **Install / reinstall (quick)** を選び、すぐにインストールする場合は `sudo bash scripts/install.sh install` を実行します。

### クイックウィザード

新規の対話型インストールは質問3つと確認1回だけです：

1. **ドメイン** — このサーバーに解決される必要があります。デフォルトはマシンの FQDN。IP アドレスや `example.com`、`your-domain.example`、`localhost` などのプレースホルダー/予約名は拒否されます。
2. **TLS モード** — `[1] Let's Encrypt（推奨、デフォルト）[2] 既存の証明書/秘密鍵ファイル [3] 自己署名（安全でない、テスト専用）`。
3. **サイトカモフラージュ** — 未認証の訪問者に見える内容（次節）。
4. **Enter でインストール** — 唯一の確認。その後クライアントパネルを表示します。

その他はすべて自動です：リッスン `:443`、Surge デフォルト（4/8）、ランダムパスワード、空のフォールバック、生成された fronting パス/シークレット/サイト名、サーバー内蔵パディングスキーム。前回デプロイの `install.env` がある場合、ウィザードは最初に **既存設定の再利用** を提案し、質問をスキップします。

インストーラーは**自己署名証明書へ暗黙にフォールバックしません**。Let's Encrypt モードではドメイン検証後に certbot で証明書を取得し、手動モードでは指定した証明書パスを使用し、自己署名モードでは明確な警告（「容易に検出され、MITM 保護を提供しない」）を表示して確認を求めます。非対話実行では、自己署名モードに加えて `TSUNAMI_TLS_MODE=self-signed TSUNAMI_DEV=1` が必要です。

#### 詳細プロンプト

従来の低レベル質問（リッスンアドレス、フォールバック、Surge 最大接続数/閾値、カスタム fronting パス/シークレット/サイト名、パディングスキーム JSON）は `TSUNAMI_ADVANCED=1` または `advanced` コマンドの場合のみ質問します：

```bash
sudo bash scripts/install.sh advanced
sudo TSUNAMI_ADVANCED=1 bash scripts/install.sh config
```

クイックフローでもすべての `TSUNAMI_*` 環境変数オーバーライドは有効なので、自動化で詳細プロンプトは不要です。

## サイトカモフラージュ

Fronting は通常の（未認証の）リクエストに対して `fronting.site` ブロックを提供します。インストーラーはウィザードの3番目の質問から自動的に書き込みます：

| モード | 動作 | 使いどころ |
|:--|:--|:--|
| `local` | このサーバー上の実サイトへリバースプロキシ。デフォルト上流 `http://127.0.0.1:8080`、ポート 80/8080/3000/8000 のリスナーを自動検出して事前選択。`preserve_host: true` | 同じ IP で実サイトを運用している場合（推奨） |
| `remote` | 別の Web サイトのコンテンツを借用。`preserve_host: false` で Host ヘッダーを上流に書き換え | ローカルに実サイトがなく、上流サイトの規約と可用性を許容できる場合 |
| `builtin` | 生成されたプレースホルダーページ（最も不自然で、クラスタリングされやすい） | 簡単なテスト専用 |

`/etc/tsunami/config.json` に書き込まれる JSON：

```json
{
  "server": {
    "fronting": {
      "enabled": true,
      "path": "/static/js/main.1a2b3c4d.js",
      "secret": "0123456789abcdef0123456789abcdef",
      "site_name": "Example",
      "decoy_proxy": "",
      "site": {
        "mode": "local",
        "upstream": "http://127.0.0.1:8080",
        "preserve_host": true,
        "insecure_skip_verify": false
      }
    }
  }
}
```

環境変数オーバーライド：`TSUNAMI_SITE_MODE`、`TSUNAMI_SITE_UPSTREAM`、`TSUNAMI_SITE_PRESERVE_HOST`、`TSUNAMI_SITE_INSECURE_SKIP_VERIFY`。`TSUNAMI_DECOY_PROXY` は旧エイリアスとして `remote` + 上流にマップされます。

## Let's Encrypt（自動 TLS）

インストール時に Let's Encrypt を選択し、実際のドメイン名を提供すると：

1. certbot が自動的にインストール（apt/yum/dnf/snap 経由）
2. `certbot certonly --standalone` を使用して証明書をリクエスト
3. TSUNAMI が更新時に停止/開始するように更新フックを設定
4. 証明書パスが `config.json` に書き込まれる

**要件：**
- ポート 80 が開放されている必要あり（HTTP-01 チャレンジ用）
- インストーラーを実行する前に DNS がこのサーバーを指している必要あり
- 更新は自動的に行われる（約 60 日ごと、ダウンタイム <10 秒）

### 非対話型 Let's Encrypt デプロイ

以下の例では `example.com` をプレースホルダーとして使用しています。実際のドメインに置き換えてください。インストーラーは予約/プレースホルダー名と IP アドレスを拒否します。

```bash
# wget を使用
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  TSUNAMI_PUBLIC_HOST=example.com \
  TSUNAMI_PASSWORD='change-this-password' \
  TSUNAMI_LETSENCRYPT=y \
  TSUNAMI_ACME_EMAIL=you@example.com \
  TSUNAMI_SITE_MODE=local \
  TSUNAMI_SITE_UPSTREAM=http://127.0.0.1:8080 \
  TSUNAMI_FALLBACK=127.0.0.1:8080 \
  sudo -E bash

# curl を使用
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  TSUNAMI_PUBLIC_HOST=example.com \
  TSUNAMI_PASSWORD='change-this-password' \
  TSUNAMI_LETSENCRYPT=y \
  TSUNAMI_ACME_EMAIL=you@example.com \
  TSUNAMI_SITE_MODE=local \
  TSUNAMI_SITE_UPSTREAM=http://127.0.0.1:8080 \
  TSUNAMI_FALLBACK=127.0.0.1:8080 \
  sudo -E bash
```

### 非対話型手動証明書デプロイ

```bash
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh \
  | sudo env \
      TSUNAMI_ASSUME_YES=1 \
      TSUNAMI_TLS_MODE=manual \
      TSUNAMI_PUBLIC_HOST=example.com \
      TSUNAMI_LISTEN=:443 \
      TSUNAMI_PASSWORD='change-this-password' \
      TSUNAMI_CERT_FILE=/etc/letsencrypt/live/example.com/fullchain.pem \
      TSUNAMI_KEY_FILE=/etc/letsencrypt/live/example.com/privkey.pem \
      TSUNAMI_SITE_MODE=remote \
      TSUNAMI_SITE_UPSTREAM=https://www.wikipedia.org \
      TSUNAMI_FALLBACK=127.0.0.1:8080 \
      bash
```

### 非対話型自己署名デプロイ（ローカルテスト専用）

```bash
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  sudo env \
      TSUNAMI_ASSUME_YES=1 \
      TSUNAMI_TLS_MODE=self-signed \
      TSUNAMI_DEV=1 \
      TSUNAMI_PUBLIC_HOST=dev.tsunami.lan \
      TSUNAMI_PASSWORD='change-this-password' \
      bash
```

このモードは設計上安全ではありません。クライアントコマンドに `--skip-verify` が付与され、証明書検証が無効になり MITM 保護も提供されません。自己署名デプロイを公開インターネットに露出しないでください。

特定のリリースをインストール：

```bash
sudo env TSUNAMI_VERSION=v1.3.0 bash scripts/install.sh
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
sudo tsunami-manage config     # クイックウィザード（既存設定の再利用可）
sudo tsunami-manage advanced   # 詳細プロンプト
sudo tsunami-manage update
sudo tsunami-manage restart
sudo tsunami-manage logs
sudo tsunami-manage client    # 接続情報パネルを表示
sudo tsunami-manage cert      # Let's Encrypt 証明書状態を表示
sudo tsunami-manage security  # デプロイのセキュリティ自己診断を実行
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

`/etc/tsunami/config.json` には SHA-256 トークンハッシュのみが保存されます。生のクライアントパスワードは `/etc/tsunami/client-command.txt` と `/etc/tsunami/install.env` に書き込まれます。設定ディレクトリのパーミッションは `0700`、これらのファイルは `0600` です。生成された fronting パス/シークレット、サイト名、`server.fronting.site` サイトカモフラージュブロックは `config.json` に保存されます。

`security` コマンドは、証明書の選択、SNI、fronting 状態、fronting パスの一意性、独立した fronting シークレット、サイトカモフラージュのモードと上流、クライアントコマンドの `--skip-verify`、ファイル権限について PASS/WARN/FAIL を報告します。設定が安全でない場合は非ゼロで終了します。詳細：

- `local` サイト → PASS（同じ IP で実サイトを提供すべき旨を表示）。
- `remote` サイト → PASS（上流サイトの規約と可用性に注意）。
- `builtin` サイト → WARN（「クラスタリングされやすい」）。
- `local`/`remote` の上流欠落、または不正なモード → FAIL。

## クライアント例

インストール後、接続情報パネルを表示：

```bash
sudo tsunami-manage client
```

パネルにはサーバーアドレス、パスワード（パネル内ではマスク）、TLS モード、fronting パス、サイトカモフラージュのモード/上流、サービス状態、すぐにコピーできるクライアントコマンドが表示されます：

```
╔═══════════════════════════════════════════════╗
║ TSUNAMI Deploy Complete                       ║
║ Server       : example.com:443                ║
║ Password     : xK9f2m...8kPq                  ║
║ TLS          : Let's Encrypt (auto-renew)     ║
║ Fronting     : enabled                        ║
║ Front path   : /static/js/main.1a2b3c4d.js    ║
║ Site         : local -> http://127.0.0.1:8080 ║
║ Decoy site   : Example                        ║
║ Status       : running                        ║
║ Config       : /etc/tsunami/config.json       ║
╚═══════════════════════════════════════════════╝

Client command (copy-paste, also saved at /etc/tsunami/client-command.txt):
tsunami-client --server example.com:443 --password '...' --sni example.com --fronting --front-path /static/js/main.1a2b3c4d.js --front-secret '...' --socks 127.0.0.1:1080 --http 127.0.0.1:8080
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
| `TSUNAMI_PUBLIC_HOST` | *（プロンプト）* | 実際の公開ドメイン（IP やプレースホルダーは不可） |
| `TSUNAMI_TLS_MODE` | `letsencrypt` | `letsencrypt`、`manual`、`self-signed` |
| `TSUNAMI_DEV` | *（なし）* | 非対話の自己署名モードでは `1` が必須 |
| `TSUNAMI_CERT_FILE` | *（なし）* | 手動 TLS 証明書パス |
| `TSUNAMI_KEY_FILE` | *（なし）* | 手動 TLS 秘密鍵パス |
| `TSUNAMI_FALLBACK` | *（なし）* | フォールバック HTTP バックエンド |
| `TSUNAMI_LETSENCRYPT` | *（プロンプト）* | `y` で非対話的に Let's Encrypt を選択 |
| `TSUNAMI_ACME_EMAIL` | *（なし）* | Let's Encrypt 通知メール |
| `TSUNAMI_FRONTING` | `1` | `0` で fronting を無効化（公開デプロイ非推奨） |
| `TSUNAMI_FRONT_PATH` | *（自動生成）* | fronting HTTP パス（例：`/assets/app.1a2b3c4d.js`） |
| `TSUNAMI_FRONT_SECRET` | *（自動生成）* | 独立した fronting HMAC シークレット |
| `TSUNAMI_SITE_NAME` | *（自動導出）* | 生成ページに表示するサイト名 |
| `TSUNAMI_SITE_MODE` | `builtin`（非対話） | サイトカモフラージュ：`local`、`remote`、`builtin` |
| `TSUNAMI_SITE_UPSTREAM` | `local` は `http://127.0.0.1:8080` | `local`/`remote` の上流 HTTP(S) オリジン |
| `TSUNAMI_SITE_PRESERVE_HOST` | `local` は `true` | 訪問者の `Host` ヘッダーを保持するか |
| `TSUNAMI_SITE_INSECURE_SKIP_VERIFY` | `false` | HTTPS 上流の TLS 検証をスキップするか |
| `TSUNAMI_DECOY_PROXY` | *（なし）* | 旧エイリアス：`remote` + 上流にマップ |
| `TSUNAMI_SNI` | `TSUNAMI_PUBLIC_HOST` | クライアントコマンドが使う TLS SNI |
| `TSUNAMI_PADDING_SCHEME_JSON` | *（サーバーデフォルト）* | サーバーのデフォルトパディングスキームを上書き |
| `TSUNAMI_ADVANCED` | *（なし）* | `1` でウィザードに詳細プロンプトを表示 |
| `TSUNAMI_VERSION` | `latest` | インストールするリリースバージョン |
| `TSUNAMI_ASSUME_YES` | *（なし）* | `1` ですべての対話プロンプトをスキップ |
