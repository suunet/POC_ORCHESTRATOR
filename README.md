# POC_ORCHESTRATOR

gateflow：AIDD（仕様カタログを正本とし、遷移表と人間関門で進める開発プロセス）を、ローカルで自動で回すオーケストレーター。

- 設計：[docs/design.md](docs/design.md)
- 状態：M0 完了。次は M1（最小の1本）

## 開発

```sh
go test ./...                                  # テスト（ホストの Go）
go run ./cmd/gateflow check ids --project <dir> # ID整合チェック
./scripts/build.sh                             # 配布用のビルド（Docker。bin/ に出力）
```
