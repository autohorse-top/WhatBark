# 失败语料库

每次真实返工/评审击杀/环境踩坑，按模板入殓一条。**入殓的还是尸体，出殡的是规则**（dev-evolve 收编）。

## 模板
```
## F-<序号> <一句话>（日期，agent）
- 现象：
- 根因：
- 修复：
- 防再犯规则候选：
```

## 已入殓

## F-001 外部 API 返回的 sha 未校验字符集，直接进 Path 参与 rmtree（2026-10-28，reviewer）
- 现象：PipeHorse `agent/pipehorse_agent/repocheck.py`（3f9107c）中 `head.sha`（GitHub API JSON）仅 `isinstance(str)` 检查（L157-161），即用于 `target = cache / sha`（L268）。sha=".." 时 `target` 为缓存父目录，`if target.exists(): shutil.rmtree(target)`（L289-290）整目录删除——实验证实缓存目录与父目录 marker 全灭；线上 ph-repos 卷（容器内 /data）同暴露面。
- 根因：外部输入（API 响应 JSON，且 `PIPEHORSE_GITHUB_API` 可指向任意端点）进路径组件前无字符集边界检查，违反 AGENTS.md §6。
- 修复：sha 做 hex 校验（`re.fullmatch(r"[0-9a-fA-F]{7,64}", sha)`，否则拒绝返回 None），并对 `target` 做 resolve 包含性断言（必须落在 cache 内）；补恶意 sha 测试用例。
- 防再犯规则候选：凡外部字符串入 `Path` 参与 `shutil.rmtree`/`os.replace` 前必须 (1) 字符集校验 (2) 目标 resolve 包含性检查；reviewer 对"缓存/删除"类代码必须构造越界输入实测。

## F-002 评审者误报"缺 prune 测试"——基于截断 diff 输出下结论（2026-10-28，reviewer）
- 现象：评审 refine-batch 时我断言 `test_repocheck.py` 缺 G1-1 声明的 prune 用例，要求补测试。
- 根因：基于 `git show | head` 的截断 diff 输出判断"测试不存在"，未 Read 实际文件；实际 `test_resolve_prune_keeps_newest`（L252）与 `test_resolve_no_prune_when_under_limit`（L279）均存在。
- 修复：回读文件后当场撤销该结论，仅保留真实缺口（恶意 sha、.git 后缀、query、git+ssh 等用例仍缺）。
- 防再犯规则候选：任何"缺 X"类判断必须对实际文件 grep/Read 验证，diff 输出（尤其经管道截断的）不得作为存在性证据。
