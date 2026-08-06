# light-llm-gateway 开源化设计

## 1. 目标

创建一个与 siu 完全独立的轻量 OpenAI-compatible LLM gateway 项目，目录为：

`/Users/huayuanlin/GolandProjects/light-llm-gateway`

项目应当能够在不依赖 siu 源码、私有基础设施或父级仓库目录的情况下完成构建、测试和运行。

“轻量”是首要约束：首版只承担请求代理、模型别名、重试与故障转移，不内置数据库、消息队列、数据仓库或业务管理后台。

## 2. 首版范围

### 保留能力

- OpenAI-compatible HTTP API：
  - `GET /v1/models`
  - `POST /v1/chat/completions`
  - `POST /v1/embeddings`
- OpenAI-compatible provider 配置：base URL、环境变量 API key、请求超时。
- 模型 alias：客户端使用稳定的别名，网关将其解析到 provider/model。
- provider priority chain：同一 alias 可配置多个 provider，并按优先级访问。
- retry：同一 provider 的可配置重试、退避、超时和可重试状态码。
- failover：provider 重试耗尽后切换到下一个 provider。
- 流式响应：首个 chunk 发出后发生的上游失败不切换 provider，保持响应语义可控。
- 基础健康检查、结构化日志和可选的指标/追踪。
- Docker 构建和本地开发配置。

### 明确不包含

首版不包含以下运行时能力：

- PostgreSQL 账户、团队、API Key 和多租户数据模型。
- Kafka usage producer/consumer。
- ClickHouse usage 落库。
- siu protobuf、toolkit、内部日志、ID、readyz 或 operation tracking 包。
- siu 管理后台、内部域名、私有镜像和单体仓库构建上下文。

## 3. 认证与扩展边界

认证通过小而稳定的接口注入，不让核心代理逻辑依赖具体账户系统：

```go
type Authenticator interface {
    Authenticate(context.Context, *http.Request) (Principal, error)
}
```

首版提供：

- `NoopAuthenticator`：默认可用，适合本地开发或由外层网络负责认证的部署。
- `StaticTokenAuthenticator`：从环境变量或构造参数读取 Bearer token，适合轻量生产部署。

未来可以在不修改代理执行路径的情况下增加 PostgreSQL API Key、多租户或外部 IAM 实现。

Usage 事件同样只通过接口暴露：

```go
type UsageSink interface {
    Record(context.Context, UsageEvent) error
}
```

首版提供 `NoopUsageSink`。事件在请求完成后构造，sink 失败不得影响已经完成的模型响应，只记录日志和指标。未来可添加 Kafka、HTTP 或数据库实现。

## 4. 结构设计

建议按职责组织为独立包：

- `cmd/gateway`：进程入口和 CLI。
- `internal/config`：配置文件、环境变量和启动校验。
- `internal/gateway`：HTTP 路由、请求处理和 OpenAI 响应适配。
- `internal/provider`：OpenAI-compatible 上游客户端和流式传输。
- `internal/routing`：alias 解析、provider priority chain 和模型列表。
- `internal/retry`：重试退避和 retryable error 判断。
- `internal/auth`：认证接口及内置实现。
- `internal/usage`：usage 类型、Noop sink 和扩展接口。
- `internal/observability`：日志、指标和 tracing 的轻量适配。

包之间通过普通 Go 接口通信，避免把未来的 PostgreSQL/Kafka 实现放入核心依赖图。

## 5. 配置原则

配置使用公开、无品牌绑定的命名：

- 监听地址和健康检查地址。
- API key 认证模式和静态 token 环境变量名。
- provider 类型、base URL、API key 环境变量、请求超时。
- alias 到 provider/model 的映射。
- retry 和 failover 参数。
- 日志级别和可选观测配置。

示例配置不得包含真实凭证。默认配置必须可以在本地通过环境变量替换上游 token，不要求修改代码。

## 6. 错误和运行语义

- 客户端输入错误返回 OpenAI 风格错误 JSON。
- 未认证请求返回 401，认证失败不泄露 token 或账户存在性信息。
- alias 或模型不可用返回明确的 4xx 错误。
- 上游连接、超时和配置的可重试状态码进入 retry/failover 流程。
- 已开始流式响应后不进行跨 provider 重试；记录最终失败原因。
- 上游响应错误保留可诊断的 status 和 request id，但过滤敏感 header 和凭证。
- usage sink、指标或 tracing 失败不阻断模型请求。

## 7. 文档和开源工程文件

新项目补齐以下内容：

- `README.md`：项目定位、快速启动、配置、API、alias/failover 示例和流式语义。
- `LICENSE`：Apache-2.0。
- `CONTRIBUTING.md`：开发环境、测试和提交规范。
- `SECURITY.md`：漏洞报告方式和凭证处理要求。
- `CODE_OF_CONDUCT.md`：社区参与规范。
- `.env.example` 或无敏感信息的配置模板。
- `Dockerfile` 和本地 `docker-compose.yaml`，构建上下文仅为新项目目录。
- `docs/architecture.md`：核心组件和依赖边界。
- `docs/extending.md`：Authenticator 与 UsageSink 扩展方式。
- CI 配置：格式化、测试、静态检查和构建。

所有示例、日志名称、二进制名称和文档标题统一使用 `light-llm-gateway`，不保留 siu 品牌或内部环境信息。

## 8. 低成本质量优化

- 清理历史兼容字段和 siu 前缀，统一配置命名。
- 配置错误包含字段路径和可操作的修复提示。
- 日志包含 request id、上游耗时、重试次数和最终 provider。
- 覆盖无认证、静态 token、alias 解析、上游失败、重试、故障转移和流式中断测试。
- 添加最小 HTTP E2E，验证独立二进制可以代理本地 mock upstream。
- 确认 `go.mod` 不含本地 `replace`，Docker 构建不读取父目录文件。

## 9. 验收标准

1. 在新项目目录执行 `go test ./...` 可以完成测试，不需要 siu 工作区。
2. 在新项目目录执行 `go build ./cmd/gateway` 可以完成构建。
3. 使用 Docker 单独构建时不需要 `../toolkit`、`../protos` 或私有镜像。
4. 本地 mock upstream 下，chat、embeddings、models、alias、retry 和 failover 均有可重复验证路径。
5. 默认启动不需要 PostgreSQL、Kafka 或 ClickHouse。
6. 新增认证或 usage sink 时只需实现公开接口，不需要修改代理核心流程。
7. 仓库内搜索不到内部 token、私有服务地址、siu 单体路径或未脱敏的凭证示例。

## 10. 许可证

采用 Apache License 2.0。它允许个人和商业使用，并提供明确的专利授权和商标免责条款，适合作为基础设施网关的开源许可证。
