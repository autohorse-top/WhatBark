---
description: 三绿门禁——构建、静态检查、测试一键全跑
agent: devops-engineer
---

在当前项目执行三绿门禁（$ARGUMENTS 可传子目录，默认全仓）：

1. 识别项目栈（Go / Python / 双栈），任一栈缺失对应工具链时给安装方案不静默跳过。
2. Go 侧：`go build ./... && go vet ./... && go test ./... -race`；Python 侧：`pytest -q && mypy . && ruff check .`。
3. 失败项：给出最短修复路径（文件:行 + 定位）；一关失败就地修复再重跑整张清单（不只原条目）。
4. 全绿后给摘要（耗时/覆盖命令清单），及"未覆盖的风险点"诚实清单。
