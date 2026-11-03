# idp-host

IDP TELOS 的 Agent Hosting & Coordination：`idp-host` 一个二进制，同时是 API 与 Worker。设计决策见 [`docs/adr/ADR-0001-host-design.md`](docs/adr/ADR-0001-host-design.md)。

状态：**M1（PATH A 全链路）与 M2（可对外服务）已完成并实测**：Agent 定义与不可变发布（四眼）、Skill 注册与发布（嵌入 agent release）、Session / Thread / Run、持久化 Run 状态机与事件日志、租约 + fencing 的 Worker（可独立扩缩）、Go 自研 harness、OpenAI 兼容 / Anthropic / Gemini 三个模型适配器、经 idp-mcp 网关的工具调用（静态凭据或 RFC 8693 委派，与真实 Keycloak 联调通过）、用户确认、租户级配额与预算、AG-UI 流式接口、RLS、审计、指标、追踪、TLS、Kubernetes 清单；契约 `idp.telos.host` 1.1.0 已发布。真实模型门禁在本地 Qwen（OpenAI 与 Anthropic 两种线上格式）上通过。

## 组成

```
调用方（Workspace / API）──AG-UI (SSE) / REST──▶ idp-host ──MCP (tools/list, tools/call)──▶ idp-mcp 网关 ──▶ 上游 MCP 服务器
                                                   │  模型适配器（OpenAI 兼容 / Anthropic）──▶ 模型端点
                                                   └─ PostgreSQL（FORCE RLS）：agents、releases、threads、runs、events、tool_calls、audit
```

- **AgentRelease**：Agent 的定义（指令、模型配置与回退、工具允许列表、确认策略、限制）编译成规范化 JSON + SHA-256 的不可变发布件；审批人不能是创建人；Run 记录它运行的 release 摘要。
- **Run 是状态机 + 事件日志**：`QUEUED → RUNNING → (AWAITING_INPUT ⇄ RUNNING)* → COMPLETED | FAILED | CANCELED`；每次迁移一个事务；AG-UI 事件（含流式文本增量）落库，SSE 从事件表读，`Last-Event-ID` 续传，任何副本都能服务任何 Run 的流。
- **Worker**：`SELECT … FOR UPDATE SKIP LOCKED` 认领、租约心跳、fencing epoch；旧 Worker 的写入被拒；副本任意扩缩。
- **Harness**：组装上下文 → 流式调模型 → 工具调用逐个过允许列表与确认策略 → 经网关执行 → 下一步；每步、每个调用、每次用量都是事件。前端工具（RunAgentInput.tools）由调用方执行。
- **HOST 不内建工具、不持有企业任务状态**：工具目录、授权、参数校验、审计在 idp-mcp；Goal / Task / Execution / Checkpoint 归 EER（预留 `executionRef`）。
- **确认，不是审批**：确认策略要求的工具调用让 Run 等待所有者本人确认（绑定参数摘要、有过期）；企业审批归 EER / Workspace。
- **身份**：OAuth bearer JWT（JWKS）；HOST 代用户调网关用 RFC 8693 委派令牌，Run 创建时换取、AES-256-GCM 加密保存、Worker 执行时使用；开发拓扑用静态 API Key。

## 目录

| 路径 | 内容 |
|---|---|
| `cmd/idp-host` | 可执行程序（`serve` = API + Worker 池；`worker` = 只跑 Worker 池，可独立扩缩；`migrate` / `ensure-login` / `seed-tenant` / `dev-token` / `new-key`；配置见文件头部注释） |
| `internal/agent` | AgentRelease：定义、校验、密封与打开 |
| `internal/run`、`harness`、`worker` | Run 模型；推理循环；认领与心跳 |
| `internal/agui`、`api` | AG-UI 消息、RunAgentInput 与事件；REST + AG-UI + SSE |
| `internal/model`、`adapters/providers/{openai,anthropic}` | 模型接口与两个适配器 |
| `internal/mcpclient` | 对网关的 MCP 2026-07-28 客户端 |
| `internal/credential`、`adapters/identity`、`internal/authorization` | 令牌交换与加密；JWT/JWKS 与开发令牌；角色 |
| `adapters/store/postgres`、`migrations/` | 仓库、迁移、RLS、租约与 fencing |
| `internal/config`、`internal/observability`、`internal/transport` | providers/gateways 配置文件；日志、追踪（自写 OTLP/HTTP JSON 导出器）、指标；HTTP、SSE、TLS |
| `api/openapi.yaml`、`api/contracts/` | OpenAPI 3.1（测试保证与路由一致）；契约 `idp.telos.host` 1.0.0 的权威副本与一致性测试（`go test ./internal/run -run TestContract`） |
| `tests/integration`、`tests/e2e`、`tests/live`、`tests/deploy`、`tests/fixtures` | 真实 PostgreSQL 上的进程内全链路；compose 端到端；真实模型门禁；Kubernetes 清单一致性；模型与 MCP 的测试替身 |
| `deploy/` | 镜像（scratch）、compose 拓扑（含 idp-mcp 的发布镜像）、端到端驱动脚本、Kubernetes 清单 |

## 使用

```bash
make test               # gofmt、vet、单元测试（-race）：适配器、harness、契约、OpenAPI、清单
make test-integration   # Docker 中起临时 PostgreSQL，跑全链路集成测试（API + Worker + 假模型 + 假网关）
bash deploy/e2e.sh      # 构建镜像，起 compose 拓扑（真实 idp-mcp 网关与 MCP SDK 上游、模型替身容器），跑端到端；KEEP=1 保留
MODE=oidc bash deploy/e2e.sh  # 同上，身份来自 Keycloak：用户令牌经 JWKS 校验，HOST 以 RFC 8693 委派令牌调网关（审计主体 = 用户，actor = idp-host）
IDP_LIVE_OPENAI_URL=http://127.0.0.1:8080/v1 IDP_LIVE_ANTHROPIC_URL=http://127.0.0.1:8080 IDP_LIVE_MODEL=<model> make test-live
go test ./internal/run -run TestContract -update   # 改了 AgentRelease / Run 后重新生成契约示例
```

`deploy/e2e.sh` 需要 idp-mcp 仓库构建的 `idp-mcp:dev` 与 `idp-fixture-upstream:local` 镜像（它们是 MCP 基础设施的发布件，HOST 只消费）。

配置文件（`IDP_HOST_CONFIG`）登记模型端点与网关：

```yaml
providers:
  - {name: local, kind: openai, baseUrl: http://127.0.0.1:8080/v1}
  - {name: claude, kind: anthropic, baseUrl: https://api.anthropic.com, apiKeyFile: /run/secrets/anthropic}
gateways:
  - name: edge
    endpoint: https://mcp.example.com/mcp
    auth: {mode: delegation, tokenEndpoint: https://idp.example.com/realms/idp/protocol/openid-connect/token,
           clientId: idp-host, clientSecretFile: /run/secrets/host-client, audience: https://mcp.example.com/mcp, actorToken: true}
```
