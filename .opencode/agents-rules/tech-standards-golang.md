# Go 技术标准

## 环境与命令
- 工具链：go 1.22+（brew install go）。
- 诊断五连：`go build ./...`、`go vet ./...`、`go test ./... -race`、`go fmt -l .`（零输出）、`gofmt -s` 惯用（golangci-lint 可用时以它为主）。
- 版本语义：go.mod 声明 go 版本；`go mod tidy` 收口。

## 并发铁律
- 每个 goroutine 有终止条件与错误回收（errgroup 或显式 channel）。
- `context.Context` 惯例：第一参数、不用 struct 字段携带、超时在拓扑根部设定。
- channel：单向类型声明在接口处；关闭权唯一持有；select 阻塞处必须回答"是否会永久卡死"。
- 共享状态三选一：锁 / channel 归属 / 不可变拷贝——写明用了哪种。

## 错误与日志
- 错误包装保留链路：`fmt.Errorf("...: %w", err)`；`errors.Is/As` 判等。
- 结构化日志（slog）优于 fmt.Print；生产路径禁 panic。

## 常见坑清单（评审红队军火备案）
- 循环变量捕获（<1.22 语义）、map 并发写、defer 在循环里的累积、time.After 在循环里的泄漏、切片别名导致的隐写。
- 接口 nil 陷阱（非 nil interface 包 nil 指针）。
