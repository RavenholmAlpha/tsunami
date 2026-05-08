# コントリビューションガイド

> [English](CONTRIBUTING.md) | [中文](CONTRIBUTING.zh.md) | **日本語**

TSUNAMI への貢献に興味をお持ちいただきありがとうございます！本ガイドでは、始め方をご説明します。

## 開発環境のセットアップ

### 前提条件

- [Go 1.25+](https://go.dev/dl/)
- Git

### ビルド

```bash
# クローン
git clone https://github.com/RavenholmAlpha/tsunami.git
cd tsunami

# ビルド
go build -trimpath -ldflags="-s -w" -o tsunami-server ./cmd/tsunami-server/
go build -trimpath -ldflags="-s -w" -o tsunami-client ./cmd/tsunami-client/

# または Makefile を使用（クロスプラットフォーム）
cd build && make
```

### テスト

```bash
# ユニットテスト（競合状態検出付き）
go test -race ./pkg/...

# 統合テスト
go test ./tests/...

# 全テスト
go test ./...

# リント
go vet ./...
```

## プロジェクト構造

```
tsunami/
├── cmd/                  CLI エントリポイント
│   ├── tsunami-server/
│   └── tsunami-client/
├── pkg/                  ライブラリパッケージ
│   ├── protocol/         ワイヤフォーマット、フレーム、認証、セッション、ストリーム
│   ├── padding/          プログラマブルパディングエンジン
│   ├── mux/              セッションプール & 多重化
│   ├── surge/            アダプティブ接続スケーリング
│   ├── fallback/         認証失敗フォールバックハンドラー
│   ├── uot/              UDP-over-TCP リレー
│   ├── transport/        TLS/TCP 設定、uTLS フィンガープリント
│   ├── proxy/            SOCKS5 & HTTP プロキシサーバー
│   ├── client/           クライアントサイド API
│   ├── server/           サーバー実装
│   ├── control/          コントロールプレーン（ミドルウェア、ユーザーストア）
│   └── config/           設定読み込み
├── scripts/              デプロイスクリプト
├── tests/                統合テスト
├── build/                クロスプラットフォームビルドスクリプト
└── docs/                 設計ドキュメント
```

## 変更の提出

### プルリクエストプロセス

1. リポジトリを **Fork**
2. `main` から**ブランチを作成**：`git checkout -b feat/my-feature`
3. 明確で説明的なコミットで**変更を加える**
4. **テストを実行**：`go test -race ./...`
5. **vet を実行**：`go vet ./...`
6. **プッシュ**してプルリクエストを開く

### コミットメッセージ

[Conventional Commits](https://www.conventionalcommits.org/) を使用：

```
feat: add UDP relay support
fix: resolve session leak on timeout
docs: update deployment guide
test: add padding scheme edge cases
refactor: simplify frame decoder
```

### コードスタイル

- 標準的な Go の慣習に従う（`gofmt`、`go vet`）
- パッケージは焦点を絞り、疎結合に保つ
- 新機能にはテストを追加
- 既存のコメントとドキュメントを維持

## 問題の報告

- [GitHub Issues](https://github.com/RavenholmAlpha/tsunami/issues) を使用
- **セキュリティ脆弱性**については [SECURITY.ja.md](SECURITY.ja.md) を参照

## ライセンス

貢献することにより、あなたの貢献が [MIT ライセンス](LICENSE) の下でライセンスされることに同意するものとします。
