# newx-gateway

统一入口（API Gateway）工程。目标是为 Web、Admin、MCP、OpenAPI 四类调用方提供唯一的入口边界：
路由、鉴权、限流、超时、协议适配、日志与链路追踪都在这里收敛，业务能力由下游服务提供。

## 当前状态

**P1 反代已打通。** 已完成：工程骨架、配置与契约、注册表、四类路由壳、healthz/readyz、
slog JSON 日志与 W3C traceparent 透传、伪造头剥离、统一错误出口、
**Nacos 服务发现（HTTP 开放 API）+ 随机权重负载均衡 + 灰度（version/tag）+ HTTP 反代**。

尚未接入：鉴权与 JWT 签发（`authstub`）、限流计数、MCP 工具实现。

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

| 路由 | 调用方 | 认证策略（接入后） | 本阶段行为 |
| --- | --- | --- | --- |
| `/web/{app_code}/v1/*` | Web、Electron | 用户 JWT + 应用 Scope + 基础限流 | 校验 app_code 后返回 `50101` |
| `/admin/{app_code}/v1/*` | 管理后台 | 管理员 JWT + RBAC + 审计 + 独立限流 | 校验 app_code 后返回 `50101` |
| `/mcp/{app_code}/*` | MCP Client | 独立凭证 + 工具白名单 + 严格超时 | 启用时返回 `50101`；禁用时不注册路由，返回 `40401` |
| `/open-api/{app_code}/v1/*` | 外部系统 | API Client + 签名 + 防重放 + 配额 | **默认关闭**，返回 `40401` |
| `/healthz` `/readyz` | 运维 | 无 | 见「健康检查」 |
| 其他 | — | 无 | 兜底返回 `40401` |

`app_code` 必须已在 `APPS` 中注册，否则返回 `40402` —— 未知前缀不会被透传到任意后端。
方法不匹配同样走兜底 `40401`，不暴露路由表结构。

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
- `GET /readyz`：默认只反映自身状态；`READYZ_PROBE_DOWNSTREAM=true` 时探测已注册下游，任一不可达则返回 `503` 且 `status=degraded`。

## 配置

全部配置项见 [.env.example](.env.example)。关键项：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `ENTRY_HTTP_ADDR` | `:8080` | 监听地址 |
| `ENTRY_ENV` | `dev` | `dev` / `production` / `test`；生产环境禁止开启签发桩 |
| `APPS` | `huobao=http://127.0.0.1:5679` | 应用注册表，`code=base_url`，逗号分隔 |
| `MCP_ENABLED` / `OPENAPI_ENABLED` | `true` / `false` | 通道开关 |
| `MAX_BODY_BYTES` | 1048576 | 普通请求体上限（语义层返回 `40002`） |
| `MAX_BODY_BYTES_UPLOAD` | 2147483648 | 协议层兜底上限 |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `json` | 日志级别与格式 |
| `READYZ_PROBE_DOWNSTREAM` | `false` | 就绪检查是否探测下游 |
| `TIMEOUT_*` / `RATE_*` | 见示例 | **已解析并校验，尚未参与转发**（反代阶段启用） |

配置在启动时一次性加载并校验：任何非法值都会打印全部错误并以退出码 `2` 退出，不做静默降级。

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
    ├── config/            环境变量解析与启动期校验
    ├── contracts/         契约：错误码、错误响应体、请求头、上下文键、路由家族
    ├── discovery/         服务发现：静态地址 + Nacos 开放 API（缓存、降级、后台刷新）
    ├── httpx/             基于 Hertz 上下文的读写工具与统一响应出口
    ├── lb/                随机权重负载均衡、灰度筛选（version/tag）与失败熔断
    ├── middleware/        恢复、追踪、日志、头部治理、体积限制 + 鉴权/范围/限流空位
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

### 超时分级

按请求类型选档（设计文档 §6）：

| 类型 | 判定 | 配置 |
| --- | --- | --- |
| 读 | `GET`/`HEAD`/`OPTIONS` | `TIMEOUT_READ`（15s） |
| 写 | 其它方法 | `TIMEOUT_WRITE`（60s） |
| 生成 | 路径命中 `TIMEOUT_GENERATE_PATHS` | `TIMEOUT_GENERATE`（900s） |
| 上传 | 路径命中 `TIMEOUT_UPLOAD_PATHS` | `TIMEOUT_UPLOAD`（1800s） |
| MCP 通道 | 家族级覆盖，优先于类型 | `TIMEOUT_MCP`（60s） |

拨号超时独立固定为 2s：拨号失败必须快速返回 `50201`，不占用整体预算。

### 失败熔断与重试

- **熔断**：实例连续失败达到 `BREAKER_FAIL_THRESHOLD` 后进入 `BREAKER_COOLDOWN` 冷却期，
  期间不参与选取；冷却结束自动放行一个试探请求，成功即恢复、失败则重新冷却。
  传输层失败与下游 5xx 都计入失败。
- **重试**：仅幂等方法（`GET`/`HEAD`/`OPTIONS`）在拨号失败或超时时重试，最多 1 次，
  优先换实例（单实例时重试同一实例）。写方法与生成类请求不自动重试。

### 错误映射

| 场景 | 错误码 |
| --- | --- |
| 拨号失败 / 连接被拒 / 连接被重置 / EOF | `50201` |
| 超过该类请求的整体预算 | `50401` |
| 其它传输错误 | `50202` |
| 全部实例处于熔断 | `50201` |
| 体积超限（普通与上传通道各有上限） | `40002` |

下游自身的 4xx/5xx **原样透传**（客户端错误处理不被入口破坏），仅在日志里归类为 `50202`。

## 当前范围与后续接入点

已交付：配置与契约、注册表、四类路由壳、健康检查、日志与链路、伪造头剥离、统一错误出口、
Nacos 服务发现、随机权重负载均衡、灰度筛选、HTTP 反代（流式请求/响应、Range 透传）、
按请求类型的超时分级、失败熔断、幂等重试、上传通道体积豁免。

尚未交付（按依赖顺序）：

1. **鉴权与授权范围**：补齐 `middleware.Authenticator` 的 JWT 本地校验，打开 `ScopeGuard.SetEnabled(true)`。
   签发方优先使用内置 `authstub`（仅 dev/CI），后续替换为 Admin Service，入口只换配置不改代码。
   与 yudao 对接时还需要支持它约定的 `login-user`（JSON）与 `tenant-id` 请求头。
2. **限流**：在 `middleware.Limiter` 中按主体维度计数，超限返回 `42901` 与 `Retry-After`；
   生成类额外受 `MAX_CONCURRENCY_GENERATE` 约束，超并发快速失败而非排队。
3. **按请求类型细化超时**：当前预算按路由家族近似划分，待路由元数据就绪后细化为
   读/写/生成/上传四类各自独立预算。

## 开发

```bash
go build ./...    # 编译
go vet ./...      # 静态检查
go test ./...     # 单元测试 + 端到端冒烟（internal/app/app_test.go）
```

`internal/app/app_test.go` 会启动真实监听并在 127.0.0.1 上完整验证：健康检查、四类路由语义、
未注册应用、关闭通道、伪造头剥离、反代转发与响应透传、上游路径改写、下游不可用、
下游错误透传、Range 206、体积限制、请求标识透传与日志字段完整性。

## 开发

```bash
go build ./...    # 编译
go vet ./...      # 静态检查
go test ./...     # 单元测试 + 端到端冒烟（internal/app/app_test.go）
```

`internal/app/app_test.go` 会启动真实监听并在 127.0.0.1 上完整验证：健康检查、四类路由语义、
未注册应用、关闭通道、伪造头剥离、错误体形状、体积限制、请求标识透传与日志字段完整性。
