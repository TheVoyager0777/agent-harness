# Contributing

## 构建与测试

```sh
cd go
make build    # 构建 ./harness (或 go build -o harness ./cmd/harness)
make test     # go test ./...
make vet      # go vet ./...
make fmt      # gofmt -w .
make check    # fmt+vet+test 全量
```

跨平台: `make release` 产出 linux/darwin/windows amd64 二进制到 dist/。

## 提交规范

Conventional Commits: `<type>(<scope>): <subject>`

- type: `feat` `fix` `refactor` `test` `docs` `build` `chore` `perf`
- scope: 包名/领域, 如 `tools` `ctx` `serve` `proc` `modes` `index`
- subject: 祈使句, ≤72 字符, 不加句号
- body: 说 "why" 而非 "what"; BREAKING CHANGE 脚注标破坏性变更

示例: `feat(tools): batched tool-call executor with keyed locks`

## 代码约定

- 包依赖单向: `config ← core ← model/tools/ctx/index ← proc ← modes ← serve ← cmd`
- 跨层引用用钩子注入(BusDeliver/AgentInject/StartControl/OnSpawned), 不反向 import
- 秘密不入库: endpoints.json/harness.json 里的 key 只留本地;
  新增秘密类文件先加 .gitignore
- 测试与代码同包(`*_test.go`), 网络一律 httptest/mock, 不依赖外部端点

## CI

`.github/workflows/ci.yml` 已启用: gofmt/vet/test/build on push+PR。
