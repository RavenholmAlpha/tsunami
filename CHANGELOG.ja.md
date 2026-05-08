# 変更履歴

> [English](CHANGELOG.md) | [中文](CHANGELOG.zh.md) | **日本語**

本プロジェクトのすべての重要な変更はこのファイルに記録されます。

本フォーマットは [Keep a Changelog](https://keepachangelog.com/ja/1.1.0/) に基づいており、
本プロジェクトは [セマンティックバージョニング](https://semver.org/lang/ja/spec/v2.0.0.html) に準拠しています。

## [1.1.0] - 2026-05-08

### 追加
- **uTLS フィンガープリント模倣** — クライアントが Chrome/Firefox/Safari の TLS ClientHello を模倣し、JA3/JA4 フィンガープリントを無効化（`--fingerprint` フラグ）
- **Let's Encrypt 自動証明書** — 自動的な証明書発行と更新によるワンクリックデプロイ（certbot 経由）
- **接続情報パネル** — インストールスクリプトがデプロイ後にすぐ使えるクライアントコマンドを表示
- `CHANGELOG.md`、`SECURITY.md`、`CONTRIBUTING.md`
- CI/CD：`golangci-lint`、`shellcheck`、`govulncheck` セキュリティスキャン
- CI/CD：Codecov によるテストカバレッジレポート
- ビルドスクリプトとドキュメントが Git で追跡（`build/`）

### 変更
- CI/CD パイプラインを 2 段階から 4 段階にアップグレード（リント → テスト → セキュリティ → リリース）
- 統合テストがリリースをブロック（`continue-on-error` を削除）
- `.gitignore` 精緻化 — ビルドスクリプト/ドキュメントは追跡、ビルド出力のみ無視
- インストールスクリプトを書き直し、Let's Encrypt サポート、certbot 更新フック、管理コマンドを追加

### 修正
- CI/CD リリースジョブでの Tar パッケージ自己包含リスク
- 欠落していた LICENSE ファイル（MIT）

### セキュリティ
- ハードコードされたサーバー認証情報を含むスクリプトを削除
- CI/CD パイプラインに `govulncheck` を追加

## [1.0.0] - 2026-05-07

### 追加
- 初期リリース
- TLS 1.3 トランスポート、ALPN `h2`、前方秘匿性
- 強制多重化（セッション-ストリームアーキテクチャ）
- Surge アダプティブ接続スケーリング（Layer 1 → Layer 2）
- プログラマブルサーバープッシュパディングスキーム
- 認証失敗時の透過フォールバック（アクティブプロービング対策）
- UDP-over-TCP リレー（UoT v2）
- SOCKS5 および HTTP プロキシサポート
- ワンクリック Linux デプロイスクリプト（`install.sh`）
- クロスプラットフォームビルド（linux/amd64、linux/arm64、windows/amd64、darwin/amd64、darwin/arm64）
- 統合テストスイート（E2E、マルチストリーム、認証失敗、セッション再利用、大容量データ転送）

[1.1.0]: https://github.com/RavenholmAlpha/tsunami/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/RavenholmAlpha/tsunami/releases/tag/v1.0.0
