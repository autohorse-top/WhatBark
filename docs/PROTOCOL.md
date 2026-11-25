# whatbark Protocol（v1 / v2）

服务端（`whatbark serve`）与客户端（`whatbark agent`、浏览器、未来的 C/MCU 客户端）
之间的通信契约。本文档是实现**其他语言客户端**的权威参考。

## 1. 传输

- HTTP/1.1，JSON（UTF-8）。错误统一形如 `{"error": "<message>"}` + 非 2xx 状态码。
- 可选 WebSocket：`/ws`，服务端 → 客户端的实时事件（控制台 UI 使用；**agent 不用 WS，只轮询**）。
- 请求体上限：普通 JSON 1 MiB；`POST /api/files` 与产物上传动态跟随 `storage_max_file_bytes`（默认 10 MiB）+ 2 MiB multipart 开销（默认 12 MiB，可经 `/api/settings` 调整）；`POST /api/me/avatar` 3 MiB。

## 2. 版本协商（protocol-v2）

所有 `/api/*` 请求和 `/ws` 握手可携带请求头：

```
X-Whatbark-Api: <int>
```

| 情况 | 行为 |
|---|---|
| 不带该头 | 按 legacy v1 处理，一律放行（向后兼容） |
| 版本 ∈ [api_min, api_max] | 正常处理 |
| 版本越界（如 0、3、99） | `426` + `{"error":"api.version.mismatch","request":<v>,"supported":[1,2]}` |
| 值不是整数 | `400` + `{"error":"api.version.malformed","header":"X-Whatbark-Api"}` |

**发现端点**（公共，无需鉴权，仅受限流保护）：

```
GET /api/info
→ 200 {"api_min":1,"api_max":2,"server_version":"<build>","registration":false}
```

- `server_version`：构建时经 `-ldflags -X whatbark/internal/api.ServerVersion=...` 注入
  （`make build` 用 `git describe --tags --always --dirty`，失败回退 `dev`）。
- `registration`：自助注册开关（§7），admin 配置 `registration_token` 后为 `true`。
- 客户端最佳实践：先 `GET /api/info` 取 `api_max`，再用该值声明 `X-Whatbark-Api`。
- 当前 Go agent 固定声明 v2。

作用域：`/api/` JSON 链（位于限流之后、鉴权之前，因此版本错误不会表现为 401）
与 `/ws` 握手前。`GET /api/files/{name}` 与 `GET /api/avatars/{name}` **豁免**——
签名 URL 供浏览器 `<img>`/`<a>` 直接使用，无法携带自定义头。

## 3. 鉴权

- REST：`Authorization: Bearer <token>`。
- token 分两类，服务端统一按 SHA-256 哈希匹配：
  - **管理员 token**：`serve -admin-token` 指定；未指定则首次启动自动生成（日志打印一次、存 settings 表）；
    多管理员经 `POST /api/admin/tokens` 创建，明文只返回一次。
  - **员工 token**：`POST /api/employees`（admin 专用）时由服务端生成，明文只返回一次。
- WebSocket：token 放在 `Sec-WebSocket-Protocol` 子协议字段；
  legacy `?token=` 查询参数仍可用，可用 `WHATBARK_DISABLE_LEGACY_QUERY_TOKEN=1` 关闭。
- 文件/头像 URL：短时效 HMAC 签名 `?exp=&sig=`（浏览器静态资源无法带 Authorization 头）。
- 权限：非 admin 只能访问自己的资源（任务/消息/路由），越权返回 `403`。
- 例外：`POST /api/register`（§7）是公共端点，无 Bearer 头；凭据是请求体中的
  注册 token，成功后签发的员工 token 再走常规鉴权。

## 4. Agent 端点契约

任务状态机：`queued → running → done | failed`，外加 `canceled`（可来自 queued/running）。running 任务可被 `POST /api/tasks/{id}/preempt` 打回 `queued`（优先级抢占，见 §4.2）。

### 4.1 引导

| 端点 | 方法 | 请求 | 响应 |
|---|---|---|---|
| `/api/me` | GET | — | `{admin:false, employee_id, employee:{id,name,role,kind,department_id,status,tool,tool_args,workdir,avatar?}, subordinates:[]}` |
| `/api/agent/attach` | POST | `{initial:true}`（首连）或 `{}`（心跳） | `{ok, employee_id, name}`；`initial` 时附 `requeued:<n>` 并把该员工遗留 running 任务重入队 |

约束：仅 `kind=ai` 员工可用（human/admin 返回 403）。

### 4.2 任务生命周期

任务对象（taskRow）字段：

```json
{
  "id": 12, "employee_id": 3, "kind": "workflow|adhoc", "status": "queued",
  "priority": 5, "input": "...", "output": "", "requested_by": 1,
  "created_at": "2026-11-13T00:00:00Z", "started_at": null, "finished_at": null,
  "project_id": 1, "group_id": 2, "employee_name": "Bob"
}
```

（`project_id`/`group_id` 可缺省；`output` 在 queued 阶段为空。）

| 端点 | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| `/api/tasks?employee=<id>&status=queued` | GET | — | `{tasks:[taskRow]}` | 取自己队列；agent 选 priority 最高、同值取 id 最小 |
| `/api/tasks/{id}/start` | POST | — | `{ok, status:"running"}` | 非法迁移 → 409 |
| `/api/agent/output` | POST | `{task_id, line}` | `{ok}` | 逐行上报工具输出；终态任务拒绝（403） |
| `/api/tasks/{id}/finish` | POST | `{output}` | `{ok, status:"done"}` | 触发下游工作流节点 |
| `/api/tasks/{id}/fail` | POST | `{output}` | `{ok, status:"failed"}` | 同上，失败分支 |
| `/api/tasks/{id}` | GET | — | taskRow | **取消监视器**：agent 轮询此端点发现 `status=canceled` 后杀本地进程 |
| `/api/tasks/{id}/preempt` | POST | `{reason?}` | `{ok, status:"queued"}` | 把 running 任务打回队列（priority 保留），非 running → 409 |
| `/api/routes` | GET | `?employee_id=<id>`（可选，仅 admin 可查他人） | `{employee_id, reporting:[...], dispatch:[...], chat:[...]}` | 上报链 / 可派发 / 可聊天员工 id 列表 |
| `/api/employees` | GET | — | `{employees:[{id,name,role,kind,department_id,status,email,avatar?}]}` | 目录；`tool/tool_args/workdir` 仅 admin 可见 |

取消（由 boss/requester/owner 或 leader 链调用 `POST /api/tasks/{id}/cancel {reason?}`，
agent 侧通过 4.2 的轮询感知，不直接调用 cancel）。

**任务/消息 kind 值域与 review 唤醒（leader orchestration）**

- task kind ∈ `dispatch | ask | scheduled | selfreview | review`（数据库 CHECK 双层约束；
  消息 kind 另含 `report | answer | chat | summary | system`）。旧库启动时自动表重建升级。
- `review` 为系统专用 kind（路由层 `from == 0` 才放行，员工无法伪造）：当任务
  kind=dispatch 到达终态 `done|failed`，且 requester 为 AI 员工时，FinishTask 会向
  requester 注入一条 review 叫醒（任务 + 消息一次落库；priority=0 不参与抢占、
  requested_by=0 完成时不自动回执；body 携带下游任务 id/状态/output 尾部与
  "同一问题最多再派 2 次，超了上报老板"指导语）。canceled 不唤醒。
- 频控：settings 键 `review_max_per_hour`（整数 [0,1000]，默认 30，0=禁用）限制每
  requester 每小时 review 注入数，超限跳过并记审计（`review.skip.throttled`）。
- review 任务完成即链条终点：无回执消息、不再二次唤醒。

### 4.3 Agent 循环语义（参考实现 `internal/agent`）

1. `GET /api/me` 鉴权 + 引导（写本地 identity/platform 上下文文件）。
2. `POST /api/agent/attach {initial:true}`（遗留任务重入队）。
3. 心跳 goroutine：每 **15s** `POST /api/agent/attach {}`。
4. 主循环：每 **5s** 轮询队列 → 选最高优先级（tie → 最小 id）：
   - `POST start` → 本地执行工具；
   - 每行 stdout 经 `POST /api/agent/output` 上报；
   - **取消监视**：每 1s `GET /api/tasks/{id}`，见 `canceled` 杀进程；
   - **抢占监视**：发现更高优先级 queued 任务 → `POST preempt` 后去跑新任务；
   - 成功 `POST finish` / 失败 `POST fail`。
5. 瞬态工具错误（限流/529/网络抖动）按指数退避重试（默认 2s 起、上限 30s、
   默认 2 次重试）；取消/抢占/关停期间不重试。

## 5. 错误码约定

| 状态码 | 含义 |
|---|---|
| 400 | 参数错误 / 版本头格式错误 |
| 401 | 缺 token 或 token 无效；注册 token 错误（`registration.token`） |
| 403 | 越权（非 owner/非 admin/非 ai 员工）；自助注册未开启（`registration.disabled`） |
| 404 | 资源不存在 |
| 409 | 状态迁移非法（如对 done 任务再 start）；员工名重复（`registration.name_taken`） |
| 413 | 请求体超限 |
| 426 | 协议版本不匹配（见 §2） |
| 429 | 触发限流 |
| 500 | 服务端错误（body 为 `{"error": ...}` 或错误文本） |

## 6. 受限 profile（单片机 / 资源受限客户端）

一个最小 C 客户端（ESP32 等）只需以下端点即可完整工作：

1. `GET /api/info`（可选，探测版本）
2. `GET /api/me`
3. `POST /api/agent/attach`
4. `GET /api/tasks?employee=N&status=queued`
5. `POST /api/tasks/{id}/start`
6. `POST /api/agent/output`
7. `POST /api/tasks/{id}/finish` / `fail`
8. `GET /api/tasks/{id}`（取消监视；轮询可放宽到 ≥2s）

可全部省略：WebSocket、文件/头像上传与签名 URL、工作流/项目/日程/群组/消息
管理端点、审计与设置；需要自助注册的 MCU 另加 `POST /api/register`（§7），
拿到一次性员工 token 后按上述 8 端点工作。建议：
- 保持任务 `input`/`output` 在几 KB 内；输出按行流式上报，勿攒大块；
- 固定声明 `X-Whatbark-Api: 2`，收到 426 时降级重试或不启动（按产品策略）；
- 仅需一个 HTTP 客户端（HTTP/1.1 + JSON + SHA-256 可选，token 由服务端保管哈希）。

## 7. 自助注册（P2，已实现）

设备/AI 员工可在不依赖 admin 的情况下自助加入组织。开关与凭据由 admin 控制：

- **启用**：`POST /api/settings {"registration_token":"<token>"}`（admin 专属）。
  token 规则：`[A-Za-z0-9_-]{16,128}`；服务端只存 SHA-256 摘要，`GET /api/settings`
  回显 `"<set>"`。
- **关闭**：`POST /api/settings {"registration_token":""}`（置空即禁用）。
- **发现**：`GET /api/info` 的 `registration` 字段实时反映开关状态（§2）。

端点（公共，无 Bearer 头；仍经过版本协商 / 限流 / CORS，见 §2）：

```
POST /api/register
X-Whatbark-Api: 2
{"token":"<注册 token>","name":"dev-01","role":"field-sensor",
 "kind":"ai","department_id":3,"email":"ops@example.com"}
→ 201 {"id":7,"name":"dev-01","role":"field-sensor","kind":"ai",
       "department_id":3,"token":"<一次性员工 token>"}
```

- `kind` 可缺省（默认 `ai`，也接受 `human`）；`email` 可选（≤254 字符，作通知邮箱）。
- `department_id` 必填且必须已存在；注册即入该部门，并自动加入部门频道
  （与 admin `POST /api/employees` 行为一致，S31）。
- 返回的员工 token 明文只出现这一次（服务端只存哈希，与 §3 员工 token 同模型），
  设备端需自行持久化。
- 成功注册写审计行 `register.create`（actor = 新员工）；失败尝试（403/401）写
  `register.fail`（actor = anon），便于 admin 事后查看爆破/探测行为（H-7）。

错误码：

| 状态码 | 条件 |
|---|---|
| 400 | 字段校验失败（name/role/kind/department_id/email） |
| 401 | 注册 token 缺失或错误（`registration.token`） |
| 403 | 注册未开启（`registration.disabled`） |
| 409 | 员工名已被占用（`registration.name_taken`） |
| 426 | 协议版本不匹配（§2） |

安全模型：注册 token 是共享口令——知道 token 与部门 id 即可加入该组织，
因此只应用于可信网络/受控设备；限流按无 token 请求的源 IP 兜底（§2/§5）。

## 8. 部署模型：单租户

一个 `whatbark serve` 进程 = 一个组织（一个 SQLite 库、一套 token）。
"多组织"发生在**设备侧**：一台设备/一个 agent 可持有多个
`(server_url, token)` 凭据，分别为多个组织的服务器工作；协议层不做跨服务器
身份联邦，各组织独立升级（因此版本协商按请求头进行，§2）。
