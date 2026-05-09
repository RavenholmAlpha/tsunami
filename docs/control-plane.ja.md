# コントロールプレーン適応

> [English](control-plane.md) | [中文](control-plane.zh.md) | **日本語**

本ドキュメントでは、TSUNAMI ノードをパネル、ボードシステム、将来のサブスクリプション/クライアントアダプターに接続するための拡張ポイントについて説明します。

## 目標

- TSUNAMI プロトコルとリレーコードを特定のパネルから独立に保つ。
- Xboard、V2board、カスタムパネル、ローカルファイルからのユーザーを 1 つのランタイムモデルに正規化。
- プロトコル変更ではなくミドルウェアを通じてアダプター固有の動作を許可。
- 動的なユーザー有効性、ユーザーごとの速度制限、使用量レポートをサポート。

## ランタイムフロー

```text
パネルまたはローカル設定
  -> control.Adapter
  -> control.Middleware チェーン
  -> control.UserStore
  -> server.UserAuthenticator
  -> TSUNAMI セッションとストリーム
```

アダプターは外部データを取得します。ミドルウェアがそれを検証・変換します。UserStore は完全または増分スナップショットをアトミックに適用するため、アクティブノードはサーバーを再構築せずにユーザーをリフレッシュできます。

## アダプター契約

アダプターが実装するインターフェース：

```go
type Adapter interface {
    Name() string
    FetchSnapshot(ctx context.Context) (*Snapshot, error)
}
```

将来のアダプターはプロトコルコードの外に配置すべきです。例：

- `xboard.Adapter`：Xboard からユーザー、クォータ、有効期限、速度制限を取得。
- `v2board.Adapter`：V2board ノード API ペイロードを正規化。
- `panel.Adapter`：カスタム HTTPS または WebSocket コントロールサービスに接続。
- `subscription.Adapter`：サーバー認証パスを変更せずにクライアント向け設定データを生成。

## ミドルウェア契約

ミドルウェアが実装するインターフェース：

```go
type Middleware interface {
    Name() string
    Apply(ctx context.Context, snapshot *Snapshot) error
}
```

ボード固有のクリーンアップとポリシーにミドルウェアを使用：

- 欠落している ID や名前を補填
- ボードの速度単位をバイト毎秒に変換
- このノードに割り当てられていないユーザーをフィルター
- 一意なトークンハッシュを検証
- ノードレベルの許可/拒否ルールを適用

組み込みの `NormalizeUsers` と `ValidateUsers` ミドルウェアは意図的に汎用的です。

## ユーザーモデル

`protocol.UserInfo` はパネル指向のフィールドをサポート：

- `ID`、`Name`
- `Password` — ローカル/静的デプロイ用
- `TokenHash` — パネルデプロイ用
- `Disabled`、`ExpiresAt`
- `Bandwidth`（Mbps）— 互換性用
- `SpeedLimitBps` — 明示的なバイト毎秒制限
- `QuotaBytes`、`UsedUploadBytes`、`UsedDownloadBytes`
- `MaxSessions`、`MaxDevices`
- `Metadata`

パネルは `TokenHash` を優先し、平文パスワードの送信を避けるべきです。

## トラフィックフック

サーバーリレーパスは `control.TrafficPolicy` を使用します。

- `UsageRecorder` はユーザーごとのアップロード/ダウンロードバイトデルタを受信。
- `Limiter` はユーザーごとのグローバル速度制限を適用可能。
- `UserLimiter` は汎用的なユーザーごとのトークンバケット。まず `SpeedLimitBps` を使用し、次に `Bandwidth`（Mbps）にフォールバック。

パネルへの使用量レポートは、デルタをバッチ集約して定期的に送信するレコーダーとして実装すべきです。

## 実装ガイダンス

アダプターは薄いトランスレーターから始めてください。ボード固有の癖はミドルウェアに保持してください。Xboard、V2board、Clash、サブスクリプションの詳細を `pkg/protocol` や `pkg/server` に追加しないでください。

推奨される将来のパッケージ：

```text
pkg/control/adapters/xboard
pkg/control/adapters/v2board
pkg/control/adapters/panel
pkg/control/subscription
```

TSUNAMI サーバーは `UserAuthenticator` と `TrafficPolicy` のみを消費し続けるべきです。
