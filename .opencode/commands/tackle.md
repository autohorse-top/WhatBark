---
description: 开发攻坚——分解、实现、测试、评审、合并全流程
agent: dev-coordinator
---

对任务 "$ARGUMENTS" 执行完整攻坚循环：

1. 按 AGENTS.md 第 2 节写 TASK.md（可交付物/验收标准/技术栈边界/依赖约束），落盘到 ./.tasks/ 下新建目录。**验收标准写成原子清单**：每项 = 描述 + 验证命令 + passes 标志（初始 false）。
2. 检索 mem0（user_id=dev-orchestrator）既往相关教训。
3. 分解子任务并委派 backend-go / backend-python 实现（按协调者路由规则选栈）。
4. 委派 qa-engineer 把验收标准转成测试；实现与测试同轮交付。
5. **循环驱动**：在验收清单上循环（回收→跑验证命令→未过项退回→再循环），**每过一条验收项 git commit 一次**；直到清单全绿或落盘停（落盘停前也 commit 断点锚）。全程不向用户询问"是否继续/下一步"（AGENTS.md Nonstop 条款）。
6. 委派 code-reviewer 红队评审；阻塞级发现退回修复复审，纳入清单循环；修复即 commit。
7. 全绿门禁（go build/vet/test -race + pytest/mypy/ruff）后：`git tag task/<slug>-done`，宣告完成。
8. 更新 STATE.md、写回 mem0（三段协议收工条目）。

禁止：跳过任务定义直接写码；未过门禁宣告完成；"我认为能跑"入档。
