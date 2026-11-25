# Python 技术标准

## 环境与命令
- 工具链：python 3.13（venv/uv 管理）；`pytest -q`、`mypy .`、`ruff check .`、`ruff format --check .` 门禁。
- 依赖：pyproject.toml 锁版本；`pip install -e .` 可装；私网无镜像时禁新依赖。

## 类型与结构
- 函数签名全类型标注；Optional/Union 显式；容器类型参数化（`list[str]` 不写裸 `list`）。
- dataclass/Pydantic 建模边界数据；protocol 定义接口（不继承式扩展）。
- 模块职责单一；`__init__.py` 只装配；工具脚本归 scripts/ 且有 shebang 与参数解析。

## 常见坑清单（评审红队军火备案）
- 可变默认参数、`list[x:y]` 与原对象共享、闭包晚绑定、`is` 表达式比较值、异常吞吃（裸 except）、f-string 进日志热路径、全局状态线程不安全、`open` 不带 encoding（跨平台）。
- dataclass 的 `__eq__` 副作用（字段 mutability）；`typing` 泛型运行时错误隐藏在注解层下的欺骗感。
