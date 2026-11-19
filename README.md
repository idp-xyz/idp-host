# idp-host

IDP TELOS 的 Agent Hosting & Coordination：`idp-host` 一个二进制，同时是 API 与 Worker。设计决策见 [`docs/adr/ADR-0001-host-design.md`](docs/adr/ADR-0001-host-design.md)。

状态：**M1（PATH A 全链路）与 M2（可对外服务）已完成并实测**：Agent 定义与不可变发布（四眼）、Skill 注册与发布（嵌入 agent release）、Session / Thread / Run、持久化 Run 状态机与事件日志、租约 + fencing 的 Worker（可独立扩缩）、Go 自研 harness、OpenAI 兼容 / Anthropic / Gemini 三个模型适配器、经 idp-mcp 网关的工具调用（静态凭据或 RFC 8693 委派，与真实 Keycloak 联调通过）、用户确认、租户级配额与预算、AG-UI 流式接口、RLS、审计、指标、追踪、TLS、Kubernetes 清单；契约 `idp.telos.host` 1.10.0 已发布。**M3、M4、M5 完成**（ADR 第 11–13 节）：子 agent（父 Run 委派出的子 Run，`AWAITING_SUBAGENT`，D22）、Skill 资源（内联与按需 `resource__read`，D23）、可配置的 Skill 合并规则（D24）、授权码 + PKCE 登录与 EER 工作负载客户端的 OIDC 端到端（D25）、评估门禁（`POST /api/v1/releases/{id}/evaluate`，`EVALUATION_REQUIRED`，D26）、输入护栏（不可信内容定界与注入检测，D27）、输出过滤（租户与发布件的禁用模式、PII 脱敏、外部分类器，`GUARD_BLOCKED`，D28）、模型路由（cost / latency / capability，D29）、A2A 服务端与客户端（`/a2a/agents/{ref}`，远端子 agent，D30）、外部 harness worker（provider 种类 `harness-worker`，`deploy/harness-worker/worker.py`，D31）。**D32 完成**（ADR 第 14 节）：发布件 `context.cmi = true` 时运行创建以调用方身份向 idp-cmi 解析 ContextPackage，条目作为 developer 上下文交给模型，`contextPackageId` 记在运行上；后续步骤复验包（`context.onStale`：continue / refresh / fail，CMI D24）。真实模型门禁在本地 Qwen（OpenAI 与 Anthropic 两种线上格式）上通过。

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
- **HOST 不内建工具、不持有企业任务状态**：工具目录、授权、参数校验、审计在 idp-mcp；Goal / Task / Execution / Checkpoint 归 EER，Run 记录 EER 的 `executionRef`。
- **EER 的 Agent 回合由 HOST 提供（D19–D21）**：EER 的每个回合 = HOST 的一个 Run，用 `forwardedProps` 的 `idp.host/toolExecution: caller`（只给模型 EER 声明的能力、只调一次模型、调用提议交回 EER）、`idp.host/releaseDigest`（钉住发布件）、`idp.host/executionRef`、`idp.host/maxOutputTokens`。授权、审批、派发、补偿、验证仍在 EER。
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
| `api/openapi.yaml`、`api/contracts/` | OpenAPI 3.1（测试保证与路由一致）；契约 `idp.telos.host` 1.10.0 的权威副本与一致性测试（`go test ./internal/run -run TestContract`） |
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

CI 与 `deploy/e2e.sh` 里的第三方镜像（PostgreSQL、Keycloak、trivy）先由 `deploy/pull-image.sh` 显式拉取并重试（最多 5 次，间隔 15、30、60、120 秒；本机已有则跳过），因为公共 registry 对匿名拉取按来源地址限流，而 CI 的 runner 与许多项目共用地址。

`deploy/e2e.sh` 需要 idp-mcp 仓库构建的 `idp-mcp:dev` 与 `idp-fixture-upstream:local` 镜像（它们是 MCP 基础设施的发布件，HOST 只消费）。

配置文件（`IDP_HOST_CONFIG`）登记模型端点与网关：

```yaml
providers:
  - {name: local, kind: openai, baseUrl: http://127.0.0.1:8080/v1}
  - {name: claude, kind: anthropic, baseUrl: https://api.anthropic.com, apiKeyFile: /run/secrets/anthropic,
     idleTimeoutSeconds: 60,        # D46: a response silent this long between two reads is cut and counts as a transport error (default 60)
     retry: {attempts: 3,           # D45: requests in all per candidate model before the next candidate or failure, 1..10 (default 3; 1 = never retry)
             backoffSeconds: 1,     #      the wait before the first retry, doubling after that, with jitter (default 1)
             maxBackoffSeconds: 20, #      cap of a computed wait (default 20)
             maxRetryAfterSeconds: 60}}   # a provider's Retry-After above this is not waited for (default 60, at most 600)
gateways:
  - name: edge
    endpoint: https://mcp.example.com/mcp
    auth: {mode: delegation, tokenEndpoint: https://idp.example.com/realms/idp/protocol/openid-connect/token,
           clientId: idp-host, clientSecretFile: /run/secrets/host-client, audience: https://mcp.example.com/mcp, actorToken: true,
           sessionBound: true}   # D38: a run renews its credential until the user signs out (Keycloak: client attribute
                                 # standard.token.exchange.enableRefreshRequestedTokenType = SAME_SESSION, also on the caller's client)
    externalTools: ["web__*"]    # D44: patterns (path.Match) of the tools whose calls send content outside the enterprise; a release that may
                                 # call one (or allows "*") is screened against files.externalModelCeiling like one that may use an external
                                 # model: files and CMI context entries above the ceiling, or without a valid level, are not given to the run
    confirmTools: ["web__web_open_url"]   # D44: patterns of the tools whose calls always wait for the member, whatever the release says
    neverResendTools: ["sandbox_write*"]  # D47: patterns of the tools whose call that was in progress when a worker stopped is never sent again, whatever
                                          # the gateway's annotations say (see below)
files:     # D43, D44: what may reach a model or a tool outside the enterprise (providers are external unless they say external: false)
  externalModelCeiling: internal   # public | internal (default) | confidential | restricted
  tenants: {}                      # per-tenant overrides, by tenant id
cmi:   # optional (ADR-0001 D32): releases with context.cmi = true resolve a ContextPackage here at run creation
  endpoint: https://cmi.example.com
  auth: {mode: delegation, tokenEndpoint: https://idp.example.com/realms/idp/protocol/openid-connect/token,
         clientId: idp-host, clientSecretFile: /run/secrets/host-client, audience: idp-cmi}
  remember:   # optional (ADR-0001 D33): after a COMPLETED run of an agent with context.cmi = true, append one episodic memory to the CMI
    enabled: true
    source: idp-host        # the CMI source name, registered in the CMI first (default idp-host)
    ttlDays: 30             # validUntil = completion + ttlDays (default 30, 1..3650)
    sensitivity: internal   # public | internal | confidential | restricted (default internal)
    maxChars: 1500          # the user message and the answer are each cut to this many characters (default 1500, 1..8000)
    auth: {mode: static, apiKeyFile: /etc/idp/cmi-writer.key}   # the WRITER identity (CMI role cmi-writer); required when enabled; static | delegation, same shape as cmi.auth;
                                                                # a static key file is read on every write, so a refreshed token is picked up
```

provider 的 `retry` 与 `idleTimeoutSeconds`（ADR D45、D46）都可以不配：可重试的错误（HTTP 408、429、5xx、传输失败、流静默超时）先对同一个候选模型重试，等待从 1 秒起翻倍、封顶 20 秒、带抖动，提供者的 `Retry-After` 更长时取它（超过 `maxRetryAfterSeconds` 就不等：换下一个候选或失败）；已经流出文字之后不重试；等待随运行的期限与取消结束。设置放在 provider 条目而不在发布件里：限流策略是运维知道的事，发布件作者不应能为自己的 agent 多烧预算。运行的期限与取消也到达进行中的模型与工具调用：期限到了运行在约一秒内以 `RUN_TIMEOUT` 结束，所有者取消的运行在约一秒内（每秒向库询问一次）以 `CANCELED` 结束。

**worker 中断时进行中的工具调用（ADR D47，修订 D3）。** worker 停止或崩溃时，HOST 分不清"已经执行完但结果丢了"和"根本没发出去"，所以接手的 turn 对记为 `EXECUTING` 的网关工具调用只在重发无害时才重发：网关现在列出的该工具带 `readOnlyHint` 或 `idempotentHint`（`tools/list` 的 `annotations`），且没有被网关条目的 `neverResendTools`（模式语法与校验同 `externalTools`，匹配即永不重发，不看标注）点名。其余调用（没有标注、标注为假、网关已不再列出该工具、任何子智能体调用）不重发：模型收到一条工具错误，说明调用被中断、结果未知、可能已经做了也可能没有、重做之前先核实；运行的事件流记 `idp.host/tool.interrupted {toolCallId, tool, resent, reason}`（`resent: true` 的是被重发的那次重复；`reason` 为 `worker_stopped`，期限与取消打断的调用为 `deadline`、`canceled`），指标 `idp_host_tool_interruptions_total{resent, reason}` 同样计数。助手消息与它的工具调用记录在同一个事务里写入，工具调用的最终状态与它的工具消息也是；旧数据里缺的记录或工具消息，续跑时补建并照常执行。收到 SIGTERM 的 worker 不再认领运行，进行中的调用（模型调用、工具调用、远程智能体调用）做完，不开始任何新的调用，在下一个边界把运行交还（运行仍是 `RUNNING`，下一个 worker 立刻认领，不等租约过期）；超过 `IDP_HOST_STOP_GRACE`（缺省 `20s`；必须小于平台的终止宽限期：Kubernetes 清单的 `terminationGracePeriodSeconds` 为 30 秒，compose 的 host-worker 为 `stop_grace_period: 30s`）仍在进行的才被中断；`0` 表示立即中断。

`cmi.remember` 是纯配置：运营方打开后，**所有** `context.cmi = true` 的 agent 都会被记住（契约不变；按 agent 退出需要改契约，见 ADR D33）。写入在运行终态之后异步进行，失败只记日志并计入 `idp_host_memory_writes_total{outcome}`（written / replayed / rejected / failed / auth_failed / no_credential），绝不影响运行。

## 许可证

本仓库采用 [PolyForm Noncommercial License 1.0.0](LICENSE)：允许非商业用途，禁止任何商业使用。
