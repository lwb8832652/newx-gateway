# newx-gateway

统一入口（API Gateway）工程。目标是为 Web、Admin、MCP、OpenAPI 四类调用方提供唯一的入口边界：
路由、身份解析、超时、日志与链路追踪在这里收敛，业务能力与权限判断由下游服务提供。

## 当前状态

**P1 反代已打通。** 已完成：工程骨架、配置与契约、注册表、四类路由壳、healthz/readyz、
slog JSON 日志与 W3C traceparent 透传、伪造头剥离、统一错误出口、
**Nacos 服务发现（HTTP 开放 API）+ 随机权重负载均衡 + 灰度（version/tag）+ HTTP 反代**、
按请求类型的超时分级、失败熔断、幂等重试、
**认证（yudao 方式：远程校验 + `login-user` 头注入 + 本地缓存）**。

**yudao 行为补齐（2026-10-18）**：旧路径兼容（`LEGACY_ROUTES`，覆盖 `/admin-api/**`、
`/app-api/**`、`/infra/ws` 等）、WebSocket 反代（101 后劫持连接双向搬运）、
**AI SSE / MCP 长连接（响应体不设整体超时，事件实时下发）**、
入口 CORS（预检直接返回，并与下游响应头去重）、可信代理来源头透传、
错误体增加 yudao 兼容字段 `msg`、认证默认接口切到 yudao-cloud、
请求体上限与读超时对齐 yudao 网关。

当前范围不包括限流/生成并发控制、网关业务授权、MCP 工具实现及 OpenAPI 签名防重放；
Knife4j 文档聚合、SkyWalking、actuator、Nacos 动态路由仍未实现（见「与 yudao-gateway 的差异」）。
`ScopeGuard` 保持关闭；MCP 当前仅转发，OpenAPI 默认关闭。

## 技术栈

| 项 | 选型 |
| --- | --- |
| 语言 | Go 1.25 |
| HTTP 框架 | CloudWeGo Hertz v0.10.x |
| 日志 | 标准库 `log/slog`（JSON） |
| 配置 | 环境变量（可选 `--env-file`） |
| 依赖 | 仅 Hertz，无数据库、无 ORM、无 Redis |

## 快速开始

```bash
# 1. 可选：准备本地配置（已存在的进程环境变量优先，不会被文件覆盖）
cp .env.example .env

# 2. 启动
go run ./cmd/server --env-file .env
# 或指定监听地址
ENTRY_HTTP_ADDR=:8080 go run ./cmd/server

# 3. 自检
curl -s localhost:8080/healthz
curl -s localhost:8080/readyz
curl -s localhost:8080/web/huobao/v1/dramas
```

构建发布版并注入版本号：

```bash
go build -trimpath -ldflags "-s -w -X main.version=$(git describe --tags --always)" -o bin/newx-gateway ./cmd/server
```

## 路由规范

| 路由 | 调用方 | 认证策略（yudao 方式启用后） | 当前行为 |
| --- | --- | --- | --- |
| `/web/{app_code}/v1/*` | Web、Electron | `login-user`/`tenant-id` 头注入，是否拦截由下游决定 | 校验 app_code 后转发下游 |
| `/admin/{app_code}/v1/*` | 管理后台 | 同上（yudao 服务自带 RBAC） | 校验 app_code 后转发下游 |
| `/mcp/{app_code}/*` | MCP Client | 同上身份解析 + MCP 超时；工具授权由下游负责 | 启用时转发；禁用时返回 `40401` |
| `/open-api/{app_code}/v1/*` | 外部系统 | 当前未实现独立签名、防重放及配额 | **默认关闭**，返回 `40401` |
| `LEGACY_ROUTES` 命中的旧路径 | yudao 存量前端、Spring Boot Admin | 与所在家族一致 | 原样转发，不改写路径 |
| `/healthz` `/readyz` | 运维 | 无 | 见「健康检查」 |
| 其他 | — | 无 | 兜底返回 `40401` |

`app_code` 必须已在 `APPS` 中注册，否则返回 `40402` —— 未知前缀不会被透传到任意后端。
方法不匹配同样走兜底 `40401`，不暴露路由表结构。

配置 `LEGACY_ROUTES` 后，旧路径（如 `/admin-api/system/user/page`）会映射到注册表中的应用并
**原样转发**；`/admin/**`、`/mcp/message` 这类会先被 `/admin/{app_code}`、`/mcp/{app_code}`
参数段路由接住的路径，由各家族在「app_code 未注册」时回落到旧前缀匹配，匹配不到仍是 `40402`。

## 错误码

入口是错误响应的唯一出口，所有错误体形状一致：

```json
{ "code": 50101, "message": "not implemented", "request_id": "9f2c...", "data": null }
```

| code | HTTP | 含义 |
| --- | --- | --- |
| 40001 | 400 | 参数错误 |
| 40002 | 413 | 请求体过大 |
| 40101 / 40102 | 401 | 未认证 / Token 过期 |
| 40103 / 40301 | 403 | Scope 不足 / RBAC 拒绝 |
| 40401 / 40402 | 404 | 路由不存在 / 应用未注册 |
| 42901 | 429 | 限流 |
| 50001 | 500 | 入口内部错误 |
| 50101 | 501 | 路由已注册、处理器未接下游（脚手架阶段专用） |
| 50201 / 50202 / 50401 | 503 / 502 / 504 | 下游不可用 / 响应异常 / 超时 |

成功响应不包裹：接入反代后业务响应由下游原样透传，保持前端既有解析不变。

## 健康检查

- `GET /healthz`：**永不探测下游**。下游抖动导致入口实例被摘除，会把局部故障放大为整体故障。
- `GET /readyz`：默认只反映自身状态；`READYZ_PROBE_DOWNSTREAM=true` 时复用解析器和连接池探测静态/Nacos 应用。
  每个应用预算 2s，任一可用实例能完成 HTTP 交互即可；无实例、解析失败或全部不可达时返回 `503`、`status=degraded`。
  探测只验证连通性，HTTP 4xx/5xx 不等同不可达；Nacos 降级缓存仍可参与探测，不代表注册中心当前健康。

## 配置

全部配置项见 [.env.example](.env.example)。关键项：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `ENTRY_HTTP_ADDR` | `:8080` | 监听地址 |
| `ENTRY_ENV` | `dev` | `dev` / `production` / `test` |
| `APPS` | `huobao=http://127.0.0.1:5679` | 应用注册表，`code=base_url`，逗号分隔 |
| `LEGACY_ROUTES` | 空 | 旧路径兼容映射，`前缀=app_code`，逗号分隔；命中后原样转发 |
| `MCP_ENABLED` / `OPENAPI_ENABLED` | `true` / `false` | 通道开关 |
| `MAX_BODY_BYTES` | 10485760 | 普通请求体上限（语义层返回 `40002`），与 yudao 网关的 10MB 对齐 |
| `MAX_BODY_BYTES_UPLOAD` | 2147483648 | 协议层兜底上限 |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `json` | 日志级别与格式 |
| `READYZ_PROBE_DOWNSTREAM` | `false` | 就绪检查是否探测下游 |
| `AUTH_ENABLED` | `false` | 开启 yudao 方式身份解析；来源及超时配置见示例 |
| `AUTH_CHECK_PATH` | `/rpc-api/system/oauth2/token/check` | yudao-cloud 校验接口；单体版改为 `/system-api/oauth2/check-token` |
| `TIMEOUT_READ` / `TIMEOUT_WRITE` / `TIMEOUT_GENERATE` / `TIMEOUT_UPLOAD` / `TIMEOUT_MCP` | 见示例 | 已参与转发，请求预算覆盖响应流读取 |
| `TIMEOUT_STREAM` | `0` | SSE / MCP 长连接的整体预算，`0` 表示不设上限 |
| `TIMEOUT_WS_HANDSHAKE` | `10s` | WebSocket 握手（建连 + 101 响应）超时 |
| `STREAM_PATHS` | `/sse,stream,/events,/mcp` | 流式路径片段；`Accept: text/event-stream` 也会自动识别 |
| `TIMEOUT_STATIC` | `300s` | 预留，当前没有独立静态通道分类 |
| `RATE_*` / `MAX_CONCURRENCY_GENERATE` | 见示例 | 仅解析校验，按当前需求暂不执行限流或并发拦截 |

配置在启动时一次性加载并校验：任何非法值都会打印全部错误并以退出码 `2` 退出，不做静默降级。

### Nacos 服务发现

`DISCOVERY` 填注册中心 `nacos://host:port`，不带控制台的 `/nacos` 路径；
`APPS` 中的 `nacos://service-name` 则填已注册的服务名，允许与应用编码不同。
以下是独立测试配置示例，地址和服务名需替换为自己的环境：

```dotenv
DISCOVERY=nacos://nacos.example.internal:8848
DISCOVERY_NAMESPACE=
DISCOVERY_GROUP=NEWX_GATEWAY_SMOKE
DISCOVERY_REFRESH=1s
APPS=smoke=nacos://newx-smoke|/api/v1
READYZ_PROBE_DOWNSTREAM=true
AUTH_ENABLED=false
```

- 命名空间填 **ID**，留空表示 `public`；分组留空表示 `DEFAULT_GROUP`。
- `DISCOVERY_REFRESH` 同时是后台刷新间隔和缓存 TTL，默认 `5s`；`1s` 仅为本次测试值。
  拉取失败时保留旧缓存，但成功获取空实例列表会覆盖旧缓存。
- 网关仅通过 `/nacos/v1/ns/instance/list` 读取 HTTP 实例列表，不自动注册业务服务，
  也不使用 Nacos SDK/gRPC。实例必须由业务服务或部署流程注册，地址必须能从网关访问。
- 当前未实现 Nacos 用户名/密码登录、accessToken 获取或续期，也没有相应凭据配置项。
  本次联调环境接口鉴权关闭，未发送凭据；开启 Nacos 鉴权后的对接尚未验证。
  **不要为兼容当前实现而将无鉴权 Nacos 暴露到公网**，应先做好可信网络隔离。

### 真实 Nacos 联调记录（2026-10-17）

环境：远端 **Nacos 2.4.0**（standalone、MySQL），`public` 命名空间、独立临时分组，
本地真实网关进程及两个临时 HTTP 后端；`AUTH_ENABLED=false`，刷新间隔 `1s`。

| 场景 | 结果 |
| --- | --- |
| Nacos 存活与就绪 | 均为 HTTP 200 |
| 无实例 | 网关 `/readyz` 为 503，业务请求为 HTTP 503 / `50201` |
| 注册一个实例 | 自动发现并转发，`/readyz` 和业务请求均为 200；应用编码与服务名不同仍正常，路径和查询参数保留 |
| 注册两个实例 | 24 次请求分别到 A/B：11/13 次；仅证明两实例均参与转发，不是负载均衡统计验收 |
| 版本元数据 | `version: v1` 命中 A，`version: v2` 命中 B |
| 禁用 A | 后续 8 次请求全部到 B |
| 注销全部实例 | 缓存刷新后恢复 503 / `50201`，后端仍存活，证明不再使用旧实例 |
| 清理 | 测试实例、远端测试服务已删除，本地测试进程已停止；测试分组服务数为 0 |

本次结果为 **PASS**，没有改动已有业务服务、项目代码或持久配置。
这是“真实 Nacos + 真实网关 + 本地测试后端”，不是模拟 Nacos 测试；
但**不证明真实 Huobao/yudao 服务注册、登录/刷新/失效链路、Nacos 故障降级或生产性能通过**。

## 可观测性

请求结束后输出单行 JSON：

```json
{"time":"...","level":"INFO","msg":"request","request_id":"...","trace_id":"...","app_code":"huobao",
 "route":"web","method":"GET","downstream":"huobao","upstream_host":"127.0.0.1:5679","status":200,
 "code":0,"latency_ms":3.29,"req_bytes":0,"resp_bytes":76,"principal_id":"","retry":0,"error_code":0}
```

- `X-Request-Id` 合法时透传，非法或缺失时重新生成（32 位十六进制），并写入响应头便于前端对照。
- `traceparent` 按 W3C 规范解析，合法则沿用、否则生成新链路；缺失的标识不会导致请求被拒。
- `resp_bytes`：流式响应取已声明的 `Content-Length`，未知长度记 `-1`。
  日志中间件**绝不读取响应体**——读一次就会把流读干，导致客户端收到空响应。
- **绝不记录** Authorization/Cookie 凭据、请求与响应正文。

## 目录结构

```
newx-gateway/
├── go.mod / .gitignore / .env.example / README.md
├── cmd/server/            进程入口：配置加载、启动、信号与优雅退出
└── internal/
    ├── app/               装配层：中间件顺序、路由注册、协议层参数
    ├── auth/              认证（yudao 方式）：check-token 客户端、登录用户缓存、login-user 头注入
    ├── config/            环境变量解析与启动期校验
    ├── contracts/         契约：错误码、错误响应体、请求头、上下文键、路由家族
    ├── discovery/         服务发现：静态地址 + Nacos 开放 API（缓存、降级、后台刷新）
    ├── httpx/             基于 Hertz 上下文的读写工具与统一响应出口
    ├── lb/                随机权重负载均衡、灰度筛选（version/tag）与失败熔断
    ├── middleware/        恢复、追踪、日志、头部治理、体积限制 + 范围/限流空位
    ├── observability/     结构化日志与链路标识
    ├── proxy/             下游 HTTP 转发：流式请求/响应、超时分级、错误码映射
    ├── registry/          app_code 注册表（含上游路径改写）
    └── transport/         协议层：四类家族路由壳 + 健康检查 + 兜底
```

中间件顺序固定为 `Recover → Tracing → RequestLogger → HeaderGuard → BodyLimit → 家族中间件 → 处理器`，
理由见 `internal/middleware` 包注释。

## 转发与上游选择

一次 `/web/huobao/v1/dramas` 请求的处理顺序：

1. 从 URL **原始路径**解析 `app_code` 与剩余路径（不依赖框架路由参数，保留百分号编码）；
2. 查注册表，未注册直接 `40402`；
3. `discovery` 解析实例列表：静态地址或 Nacos（拉取失败时降级用旧缓存，无缓存才 `50201`）；
4. `lb.FilterGray` 按请求头 `version` / `tag` 筛选，无匹配则回退全量并打告警（语义与 yudao 网关一致）；
5. `Breaker` 摘除熔断中的实例；全部熔断时快速返回 `50201`，不再继续发送；
6. `lb.Pick` 随机 + 权重选一个实例；
7. 路径改写：剥掉家族前缀与版本段，拼上注册表里的上游路径前缀；
8. `proxy.Forward` 转发，请求体与响应体都**流式**搬运，不做整体缓冲。

灰度请求头：`version: 2.0` 命中实例 `metadata.version=2.0`；`tag: my-host` 定向到打了同标签的实例。

### 旧路径兼容与 WebSocket

`LEGACY_ROUTES` 把 yudao 的「路径即服务」规则映射到注册表，命中后路径与查询参数原样转发：

```dotenv
LEGACY_ROUTES=/admin-api/system=yudao-system,/admin-api/infra=yudao-infra,/app-api/system=yudao-system,/infra/ws=yudao-infra,/admin=yudao-infra
```

- 前缀做最长匹配（`/admin-api/system` 优先于 `/admin-api`），末尾斜杠会归一化。
- `/admin/**`、`/mcp/message` 会被参数段路由先接住，此时由家族的 `UnknownApp` 兜底回落到旧前缀；
  未命中旧前缀仍返回 `40402`，不会把未知路径透传出去。
- WebSocket（`Upgrade: websocket`）走单独链路：与上游完成 101 握手后调用 Hertz `Hijack`，
  把客户端连接交给双向 `io.Copy`；握手阶段保留 `Upgrade`/`Connection`/`Sec-WebSocket-*`
  等逐跳头，且不参与重试。握手被上游拒绝时按普通响应回写。

### AI SSE 与 MCP 长连接

- 判定：客户端声明 `Accept: text/event-stream`，或路径命中 `STREAM_PATHS`
  （默认 `/sse,stream,/events,/mcp`）。命中后响应体不再套用整体超时。
- `TIMEOUT_STREAM=0` 表示不设上限；连接与响应头阶段仍用常规预算，上游不响应时按 50401 快速失败。
- MCP 的 `Mcp-Session-Id`、`Last-Event-ID`、`Authorization` 等头原样透传，网关不解析协议内容。
- 旧版 HTTP+SSE：`GET /sse` + `POST /mcp/message`，通过
  `LEGACY_ROUTES=/sse=yudao-ai,/mcp/message=yudao-ai` 接入；
  新版 Streamable HTTP：`/mcp/{app_code}` 由 MCP 家族直接转发，POST 返回的 SSE 同样不受 60s 限制。
- 同一个服务如果既有 `/admin-api/**` 又有根路径 `/mcp`，需要两个应用编码分别配置上游前缀，
  例如 `yudao-ai=nacos://ai-server|/admin-api` 与 `yudao-ai-mcp=nacos://ai-server|/mcp`；
  这样 `/admin/yudao-ai/v1/ai/...` 走 `/admin-api/ai/...`，`/mcp/yudao-ai-mcp` 走 `/mcp`。
- 响应为 `text/event-stream` 且上游未设置 `X-Accel-Buffering` 时，入口补 `X-Accel-Buffering: no`。

### 超时分级

按请求类型选档（设计文档 §6）：

| 类型 | 判定 | 配置 |
| --- | --- | --- |
| 流式 | `Accept: text/event-stream` 或路径命中 `STREAM_PATHS` | `TIMEOUT_STREAM`（`0` = 不设上限，优先于其它档位） |
| 读 | `GET`/`HEAD`/`OPTIONS` | `TIMEOUT_READ`（60s） |
| 写 | 其它方法 | `TIMEOUT_WRITE`（60s） |
| 生成 | 路径命中 `TIMEOUT_GENERATE_PATHS` | `TIMEOUT_GENERATE`（900s） |
| 上传 | 路径命中 `TIMEOUT_UPLOAD_PATHS` | `TIMEOUT_UPLOAD`（1800s） |
| MCP 通道 | 家族级覆盖，优先于类型 | `TIMEOUT_MCP`（60s），但命中流式判定时改用 `TIMEOUT_STREAM` |
| WebSocket 握手 | `Upgrade: websocket` | `TIMEOUT_WS_HANDSHAKE`（10s），只覆盖建连与 101 响应 |

拨号超时独立固定为 2s：拨号失败必须快速返回 `50201`，不占用整体预算。

流式请求的“不设上限”只作用于**响应体**：连接与响应头阶段仍使用常规预算，
上游只建连不返回响应头时会按 50401 快速失败，不会永久占住连接。
响应为 `text/event-stream` 时入口会补 `X-Accel-Buffering: no`（若上游未设置），
避免前置 nginx 缓冲导致事件攒批。

### 失败熔断与重试

- **熔断**：实例连续失败达到 `BREAKER_FAIL_THRESHOLD` 后进入 `BREAKER_COOLDOWN` 冷却期，
  期间不参与选取；冷却结束自动放行一个试探请求，成功即恢复、失败则重新冷却。
  传输层失败与下游 5xx 都计入失败。
- **重试**：仅普通读取的 `GET`/`HEAD`/`OPTIONS` 在响应开始前遇到连接失败或超时时最多重试 1 次；
  优先换可用实例，没有备用时可重试同一实例，整体预算不重置。下游正常返回的 3xx/4xx/5xx 不触发重试。
  `TIMEOUT_GENERATE_PATHS`、`TIMEOUT_UPLOAD_PATHS` 命中时（含 URL 编码路径）以及写方法均不重试。
  这些请求使用单次 HTTP/1 连接，禁用连接复用与 HTTP/2 隐式重试；`Idempotency-Key` 仍只透传，不提供业务去重。
- **重定向**：代理不跟随下游 3xx，原样返回状态、`Location` 和正文；check-token 同样不跟随重定向，
  3xx 按认证校验失败处理，不从重定向目标获取身份。浏览器是否跟随 `Location` 由客户端控制，不代表网关已过滤目标 URL。

### 错误映射

| 场景 | 错误码 |
| --- | --- |
| 拨号失败 / 连接被拒 / 连接被重置 / EOF | `50201` |
| 超过该类请求的整体预算 | `50401` |
| 其它传输错误 | `50202` |
| 全部实例处于熔断 | `50201` |
| 体积超限（普通与上传通道各有上限） | `40002` |

下游自身的 4xx/5xx **原样透传**（客户端错误处理不被入口破坏），仅在日志里归类为 `50202`。
以上传输错误码只用于响应开始前；已开始回写的流不能改成 504 或再追加错误 JSON。
流中超时会取消上游并结束传输。Hertz v0.10.6 对未知长度 chunked 的读错误可能写正常结束块，
因此客户端收到部分内容后不一定能从 HTTP EOF 区分成功与截断；SSE/任务协议需用业务完成事件确认。
当前默认 netpoll 传输已开启客户端断开感知，可取消仍在读取的上游请求；切换传输实现时须重新验证。

## 认证（沿用 yudao-cloud 网关方式）

沿用 yudao-gateway 的身份传递与下游授权模式，不做本地 JWT 校验：

```
请求 → 剥离客户端 login-user，规范化非负整数 tenant-id（仅租户选择提示）
     → 无 Bearer Token？保留合法租户提示、不带身份放行
     → 有 Token → 按租户提示 + Token 查缓存（TTL 默认 1 分钟，含负缓存、并发合并）
               → 未命中调 check-token，带原租户提示
     → 有效 → 注入 login-user（JSON + URL-encode），用返回 tenantId 覆盖 tenant-id
     → 无效 / 已过期 / 鉴权方不可用 → 保留合法租户提示、不带身份放行
```

**关键点**：

- `login-user` 是**芋道框架的私有头**，不是 OAuth2/OIDC 标准。字段与 yudao 的 `LoginUser` 对齐：
  `id`/`userType`/`info`/`tenantId`/`scopes`/`expiresTime`。
  yudao 的网关和 Web starter 都注册了自定义 Jackson 时间序列化器，
  `expiresTime` 实际是 **JSON 数字 epoch 毫秒时间戳**，不是默认 ISO 字符串。
  check-token 同样支持数字毫秒；为兼容已有对接，仍接受带时区及本地时间字符串，
  其中无时区字符串按 Go 进程本地时区解释。
- **网关不做业务授权**：是否需要登录、有没有权限，由下游服务判断；`ScopeGuard` 不启用。
- 鉴权方用 `AUTH_APP_CODE`（APPS 应用编码，可映射不同的 Nacos 服务名）或 `AUTH_BASE_URL`（静态 HTTP/HTTPS 地址），二选一。
- `AUTH_CHECK_PATH` 默认 `/rpc-api/system/oauth2/token/check`（yudao-cloud 的 `OAuth2TokenCommonApi.URL_CHECK`）。
  单体版 ruoyi-vue-pro 用的是 `/system-api/oauth2/check-token`，必须显式覆盖；
  路径写错时接口返回 `code=401`，网关会按「令牌无效」静默放行，鉴权实际不生效。
- `tenant-id` 是客户端可选的不可信租户提示，不能据此判断用户身份；无 Token 的登录接口也能使用它。
  非法、负数及溢出值会被删除，显式 `0` 与未提供租户分别处理；有效身份以 check-token 的租户结果覆盖该头。
- 本地正/负缓存共用 10000 项容量上限，写入时限频清理过期项，达到容量时淘汰任意一项，
  避免一次性失效 Token 永久堆积；这是缓存内存管理，不是请求限流。
- 客户端 `login-user` 始终剥离，仅从有效校验结果重建；鉴权方不可用时不携带身份放行，
  所以下游必须自行保护需要登录的接口，且不应向公网开放可直接伪造身份头的访问路径。

## 当前范围与后续接入点

已交付：配置与契约、注册表、四类路由壳、健康检查、日志与链路、伪造头剥离、统一错误出口、
Nacos 服务发现、随机权重负载均衡、灰度筛选、HTTP 反代（流式请求/响应、Range 透传）、
按请求类型的超时分级、失败熔断、幂等重试、上传通道体积豁免、
认证（yudao 方式：远程校验 + `login-user` 头注入 + 本地缓存）、
旧路径兼容（`LEGACY_ROUTES`）、WebSocket 反代（含握手超时）、
AI SSE / MCP 长连接（请求侧 + 响应侧识别）、入口 CORS、可信代理来源头透传、
`/v3/api-docs` 路径重写。

当前明确不做：

- **限流和生成并发控制**：按当前需求暂缓；`RATE_*`、`MAX_CONCURRENCY_GENERATE` 仅保留配置，不执行拦截。
- **网关业务授权**：沿用 yudao 下游授权，不启用 `ScopeGuard`，不引入本地 JWT/authstub。
- **MCP 工具层与 OpenAPI 安全能力**：不在入口实现工具业务；MCP 仅转发，OpenAPI 默认关闭，
  启用开关本身不代表已具备签名、防重放或配额保护。

### 与 yudao-gateway 的差异（截至 2026-10-18）

已补齐：旧路径（`/admin-api/**`、`/app-api/**`、`/admin/**`、`/mcp/message`、`/infra/ws`）、
WebSocket（含握手超时）、AI SSE / MCP 长连接（含响应侧识别）、入口 CORS、
可信代理来源头、错误体 `msg`、鉴权默认接口、请求体上限与读超时、
**`/admin-api/{module}/v3/api-docs` 重写到 `/v3/api-docs`**。

仍未实现：

- **Knife4j 文档聚合 UI**（`/doc.html`）：单个服务的 `/v3/api-docs`、`/v3/api-docs/swagger-config`
  现在可以经网关访问，但网关不提供把多个服务拼在一起的聚合页面。
- **APM / actuator**：没有 SkyWalking 段与 `/actuator/**`；可观测性依赖结构化日志。
- **Nacos 动态路由与自注册**：`APPS`/`LEGACY_ROUTES` 变更需重启；网关自身不注册到 Nacos，
  Spring Boot Admin 的实例列表里看不到网关。
- **限流与生成并发**：仍只解析配置，不拦截。

部署验收仍需目标环境真实 yudao 服务、有效 Token 及信任网络联调；本地模拟 check-token/Nacos、
源码契约核对、离线 Jackson 互操作及上述真实 Nacos 发现联调均不能替代完整部署验证。
入口新增延迟 p95 <10ms 是压测目标，不是现有单元测试能证明的结论。

2026-10-18 安全收尾已修复并通过本地回归：

- 代理原样返回 301/302/303/307/308 的状态、Location 和正文，check-token 拒绝跟随这些重定向；
  默认、空和自定义认证客户端均有测试，重定向目标未收到网关请求。
- 生成/上传路径及写方法排除显式重试，并验证复用连接关闭时不会被底层重放；普通读取保留一次重试。
- Nacos 降级测试明确让 TTL 过期、断言真实失败拉取并返回旧缓存，上游恢复后重新拉取；
  这是本地模拟 Nacos 回归，不代表真实注册中心故障/重启已验收。
- 日志回归覆盖有效认证、认证失败、配置 URL 凭据、请求查询/头/正文、响应正文、Nacos 后台错误和 panic。
  不输出完整认证 URL、Nacos 错误正文或 panic 值；异常响应使用固定消息，日志保留请求关联、错误类型及调用栈。

以上回归、构建、vet 与 race 测试通过，不代表真实 yudao、生产性能或已运行进程自动更新；
已有网关进程须重新构建部署后才应用修复。

## 开发

```bash
go build ./...    # 编译
go vet ./...      # 静态检查
go test ./...     # 单元测试 + 端到端冒烟（internal/app/app_test.go）
```

`internal/app/app_test.go` 会启动真实 Hertz TCP 监听验证：健康检查、四类路由语义、
未注册应用、关闭通道、伪造头剥离、请求/响应流式转发、上游路径改写、下游不可用、
下游错误透传、Range 206、体积限制、请求标识透传与日志字段完整性，
以及租户提示、身份头数字时间、Nacos 应用别名、实际实例就绪探测、SSE 多块、流中超时与客户端断开取消。
check-token 与 Nacos 在这些测试中使用本地模拟服务。

并发与资源回归可运行 `go test -race ./...`。
