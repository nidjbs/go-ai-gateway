---
name: release-e2e
description: 发布前强制 e2e 验证门禁。在每次发布/打 tag/部署生产之前必须运行，验证网关核心能力与可用性。当用户提到"发布"、"准备 release"、"打 tag"、"发布前验证"时使用。
---

# Release E2E 验证门禁

每次发布之前必须执行本 skill，验证网关核心能力全部通过后才能发布。任何一步失败即视为发布阻塞。

## 执行步骤

1. 确认工作区干净（`git status`），必要时先提交当前改动（脚本运行的是工作区代码）。
2. 运行统一门禁：

   ```bash
   ./scripts/release_check.sh
   ```

   该脚本按顺序执行：gofmt → go vet → go test → build → `e2e.sh`（核心能力）→ `e2e_apikey.sh`（认证/限流/配额）→ `e2e_release.sh`（策略/DLP/幂等/Admin/熔断）→ docker build（可选）。
3. 看到 `All release checks passed — safe to release.` 且退出码为 0，才允许继续发布动作。

## 核心能力清单（必须全部通过）

| 类别 | 覆盖内容 | 验证脚本 |
|---|---|---|
| 代码质量 | gofmt / go vet / 全量单测 / 编译 | release_check.sh |
| 操作端点 | healthz / livez / readyz / metrics / version | e2e.sh |
| 认证 | static token、api-key、错误 key 401 | e2e.sh + e2e_apikey.sh |
| 模型 & 路由 | /v1/models、alias 解析、未知模型 400 | e2e.sh |
| Chat | 非流式、多轮、工具调用 | e2e.sh |
| Chat 流式 | SSE 正常 / 首 chunk 后不降级 / 超时 | e2e.sh |
| Anthropic 适配 | 原生协议 chat / tool / stream / retry / failover | e2e.sh |
| 弹性 | 重试、失败降级（openai + anthropic + stream） | e2e.sh |
| Embeddings | 单输入、多输入、降级 | e2e.sh |
| 用量 | sqlite 持久化、usage 落库 | e2e.sh |
| Guardrails | 注入 / 越狱 / 中文 / 角色覆盖 429 | e2e.sh |
| 限流 & 配额 | rps 限流、每日 token 配额、Retry-After | e2e_apikey.sh |
| 路由策略 | loadbalance 加权分布、least_latency 切快 | e2e_release.sh |
| 输出 DLP | PII mask / reject（email、phone） | e2e_release.sh |
| 幂等 | Idempotency-Key 重放不重复计费 | e2e_release.sh |
| Admin | 运行时 revoke key → 401、revoked 列表、usage 查询 | e2e_release.sh |
| 熔断 | 故障后打开、不再调用上游 | e2e_release.sh |

## 通过标准

- `./scripts/release_check.sh` 退出码为 0。
- 输出包含 `All release checks passed — safe to release.`。
- 三个 e2e 脚本各自打印 `All ... cases passed.`。

## 失败处理

1. 停下，**不得发布**、不得打 tag、不得构建发布镜像。
2. 定位失败步骤：脚本输出显示卡在哪个 `[N/8]` 步骤；e2e 失败时 `scripts/e2e*.sh` 会自动 dump 相关进程日志（或设置 `KEEP_E2E_LOGS=1` 保留临时目录）。
3. 把失败原因和日志摘要报告给用户，修复后**重跑整个 `release_check.sh`**（不要只重跑失败的那一步）。
4. 全部通过后才继续发布。

## 注意事项

- 需要 Go、Bash、curl、Python 3（e2e 脚本依赖）；docker 可选（无 docker 时自动跳过容器构建）。
- e2e 脚本会在随机端口起真实 gateway + mock 上游，运行期间不要占用 8080/8081。
- 若改动影响 Redis 分布式路径，发布前建议额外用 `go test -race ./internal/...` 验证并发正确性。
