# idp-host

IDP TELOS 的 Agent Hosting & Coordination：`idp-host` 一个二进制，同时是 API 与 Worker。设计决策见 [`docs/adr/ADR-0001-host-design.md`](docs/adr/ADR-0001-host-design.md)。

状态：**M1（PATH A 全链路）与 M2（可对外服务）已完成并实测**：Agent 定义与不可变发布（四眼）、Skill 注册与发布（嵌入 agent release）、Session / Thread / Run、持久化 Run 状态机与事件日志、租约 + fencing 的 Worker（可独立扩缩）、Go 自研 harness、OpenAI 兼容 / Anthropic / Gemini 三个模型适配器、经 idp-mcp 网关的工具调用（静态凭据或 RFC 8693 委派，与真实 Keycloak 联调通过）、用户确认、租户级配额与预算、AG-UI 流式接口、RLS、审计、指标、追踪、TLS、Kubernetes 清单；契约 `idp.telos.host` 1.12.0 已发布。**M3、M4、M5 完成**（ADR 第 11–13 节）：子 agent（父 Run 委派出的子 Run，`AWAITING_SUBAGENT`，D22）、Skill 资源（内联与按需 `resource__read`，D23）、可配置的 Skill 合并规则（D24）、授权码 + PKCE 登录与 EER 工作负载客户端的 OIDC 端到端（D25）、评估门禁（`POST /api/v1/releases/{id}/evaluate`，`EVALUATION_REQUIRED`，D26）、输入护栏（不可信内容定界与注入检测，D27）、输出过滤（租户与发布件的禁用模式、PII 脱敏、外部分类器，`GUARD_BLOCKED`，D28）、模型路由（cost / latency / capability，D29）、A2A 服务端与客户端（`/a2a/agents/{ref}`，远端子 agent，D30）、外部 harness worker（provider 种类 `harness-worker`，`deploy/harness-worker/worker.py`，D31）。**D32 完成**（ADR 第 14 节）：发布件 `context.cmi = true` 时运行创建以调用方身份向 idp-cmi 解析 ContextPackage，条目作为 developer 上下文交给模型，`contextPackageId` 记在运行上；后续步骤复验包（`context.onStale`：continue / refresh / fail，CMI D24）。真实模型门禁在本地 Qwen（OpenAI 与 Anthropic 两种线上格式）上通过。

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
| `api/openapi.yaml`、`api/contracts/` | OpenAPI 3.1（测试保证与路由一致）；契约 `idp.telos.host` 1.12.0 的权威副本与一致性测试（`go test ./internal/run -run TestContract`） |
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
  - name: local
    kind: openai
    baseUrl: http://127.0.0.1:8080/v1
    models:                          # D52: what the host knows about the models this provider serves (key: the release's model name)
      - name: qwen3.8-flash-next-iq3_xxs
        contextWindow: 131072        # tokens. REQUIRED for D52 to apply to this model (see below); without it nothing changes
        maxOutputTokens: 32768       # D60: the most one answer may write, sent with every request (default 32768; 16..1048576, below contextWindow); a release can lower it, never raise it
        runawayRetry:                # D60: what the one retry of an answer that did not end changes in the request; all optional
          temperature: 0.7           #   (0, 2]; default 0.7; used when the release sets none, a release's own temperature above 0 is kept
          presencePenalty: 0.5       #   [0, 2]; default 0.5; sent to OpenAI-compatible providers only (the other adapters have no such field)
          # keepSampling: true       #   the retry repeats the request unchanged (a server that decodes greedily then loops again)
        context:                     # all optional; the defaults below are derived from the window
          waterMarkShare: 0.6        #   (0.2, 0.9]  above this share of the window older content is shortened further, oldest first
          hintShare: 0.8             #   (0.3, 0.95] above this share the budget hint tells the model the context is filling
          toolResultBytes: 16384     #   the most of one tool result the model is given; default window/8 in bytes, within 4 KiB..64 KiB
          keepToolResults: 6         #   the newest tool results (and the calls that asked for them) that are shown whole; default 6
          oldResultBytes: 1200       #   an older result longer than this is shown as head and tail; default 1200 at 131,072 (scaled, 512..4096)
          argumentBytes: 1200        #   the same for a string value in an older tool call's arguments; default oldResultBytes
  - {name: claude, kind: anthropic, baseUrl: https://api.anthropic.com, apiKeyFile: /run/secrets/anthropic,
     idleTimeoutSeconds: 60,        # D46: a response silent this long between two reads is cut and counts as a transport error (default 60)
     retry: {attempts: 3,           # D45: requests in all per candidate model before the next candidate or failure, 1..10 (default 3; 1 = never retry)
             backoffSeconds: 1,     #      the wait before the first retry, doubling after that, with jitter (default 1)
             maxBackoffSeconds: 20, #      cap of a computed wait (default 20)
             maxRetryAfterSeconds: 60},     # a provider's Retry-After above this is not waited for (default 60, at most 600)
     tracePropagation: false}       # D57: send the W3C traceparent of the run's trace to this provider (default: yes only for external: false)
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
                                 # D48: the same for a run whose release can delegate to a sub-agent that qualifies (at any depth D22 allows), or
                                 # to a remote A2A agent, or to a sub-agent whose release cannot be read; the events' "via" names which one
                                 # D51: the list is for OUR OWN outward tools (web__*). A tool of an external upstream is outward without being
                                 # listed: the gateway marks it in tools/list (_meta "io.idp.telos/external": true), and the HOST reads the marker
                                 # (only that key, only the boolean true) when a run is created, with the caller's credential, and again each turn
    confirmTools: ["web__web_open_url"]   # D44: patterns of the tools whose calls always wait for the member, whatever the release says
    neverResendTools: ["sandbox_write*"]  # D47: patterns of the tools whose call that was in progress when a worker stopped is never sent again, whatever
                                          # the gateway's annotations say (see below)
    parallelCalls: 4             # D53: how many calls of one step that only read (the gateway lists readOnlyHint true) run at once toward this gateway;
                                 # 0 is the default (4), 1 runs every call one at a time as before, at most 32; this is the number of requests one
                                 # run's step may have in flight at the gateway, so the gateway and the upstreams behind it must take that many
    sequentialTools: ["db_*"]    # D53: patterns of the tools that never run beside another call although the gateway lists them read-only
                                 # (an upstream with a rate limit, reads that share a cursor); not neverResendTools, which is about repeating a call
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

**一步里只读的调用同时执行（ADR D53）。** 模型在一步里要求几个工具调用时，**连续的**、都是自动的（没有待确认、没有被拒、不是调用者的工具、不是子 agent）、且网关现在列出为 `readOnlyHint: true`（只幂等不算，没有标注不算）、运维没有在 `sequentialTools` 里点名的网关调用（和 HOST 自己的 `resource__read`）构成一组，同时执行，最多 `parallelCalls` 个在途（网关条目下，缺省 4，1 即关闭、与以前逐字节相同，最多 32）；其余的调用照旧一个一个按顺序执行。线程与事件日志里的工具消息仍按调用的顺序，不论谁先做完（每个回答在它与它之前的都完成时立刻落库）；一个调用失败不影响同组的别的调用；期限、取消、停止到达组里每一个调用（在途的记为被打断，没开始的记为 not run 或留给下一个 worker）；worker 死在组的中间，接手的 worker 对 `EXECUTING` 的只读调用照 D47 重发。`sequentialTools` 不是 `neverResendTools`：前者说"这个工具不与别的调用同时执行"（上游有限流、读共用游标），后者说"worker 死时它做到一半不要再发"，两者互不蕴含。**部署要知道：** 同时在途的请求数是 `parallelCalls`，网关（idp-mcp 对平台客户端缺省没有限流）与上游要承受得住；idp-web 的 PDF 读取只有 `IDP_WEB_PDF_CONCURRENCY`（缺省 2）个槽，满了等 2 秒就回 `pdf_busy`，所以要么把 idp-web 那个网关的 `parallelCalls` 设成不超过槽数，要么提高槽数并把 Pod 的内存上限跟着调。**今天的任务里这几乎省不下时间**：本机模型上 637 步里 634 步只要求一个调用（见 ADR D53）。

**模型的上下文窗口（ADR D52）。** 要让长任务不撞窗口，部署必须给用到的每个模型配 `contextWindow`（provider 条目下的 `models[]`，键是 `provider` 名加发布件里的模型名）。配了以后：HOST 每次调用前按 token 估算输入，用上一步模型服务报告的用量校正；估算超过水位（`waterMarkShare`，缺省 0.6）时把更早的工具结果与更早的工具调用参数继续缩短（最老的先缩，一次缩到水位的六分之五以下，之后的步骤里已缩短的不再变长）；超过 `hintShare`（缺省 0.8）时在每步的预算提示里告诉模型窗口已用多少、该收尾；估算说即使缩到底也装不下时，不调用模型，直接以 D40 的消息结束运行。没有 `contextWindow` 的模型一切照旧，与 D40 逐字节相同：三个数字用 16 KiB、6、1,200 字节，参数不缩短，没有提示，也不会在调用前拒绝；这样的模型在窗口满时仍然只能靠模型服务报错（三家适配器现在都认得）。三个阈值的缺省值都按窗口推出，131,072 token 时恰是 D40 的数字；设置放在 HOST 配置而不在发布件里：发布件不能给自己放宽运维定的上限。其余字段的范围见 `internal/config`，写错的值在启动时被拒。平台的 compose（`deploy/compose/e2e.sh` 生成的 `host.yaml`）与 kind（`deploy/kubernetes/kind/kustomization.yaml` 里的 `host.yaml`）目前没有 `models`，要得到这个行为，需要在 `local` 条目下加所用模型的条目与 `contextWindow`。

provider 的 `retry` 与 `idleTimeoutSeconds`（ADR D45、D46）都可以不配：可重试的错误（HTTP 408、429、5xx、传输失败、流静默超时）先对同一个候选模型重试，等待从 1 秒起翻倍、封顶 20 秒、带抖动，提供者的 `Retry-After` 更长时取它（超过 `maxRetryAfterSeconds` 就不等：换下一个候选或失败）；已经流出文字之后不重试；等待随运行的期限与取消结束。设置放在 provider 条目而不在发布件里：限流策略是运维知道的事，发布件作者不应能为自己的 agent 多烧预算。运行的期限与取消也到达进行中的模型与工具调用：期限到了运行在约一秒内以 `RUN_TIMEOUT` 结束，所有者取消的运行在约一秒内（每秒向库询问一次）以 `CANCELED` 结束。

**回退候选（ADR D56，修订 D45）。** 主候选的错误按类别决定换不换下一个候选，不用配置：候选自己的问题换（401、403、404、402、413、额度用尽——HTTP 的 `insufficient_quota` 与 Anthropic 的 "credit balance is too low" 不再重试、立刻换；流里的认证、权限、找不到、额度错误），请求自己的问题不换（其余 400、422、其余 4xx），提供者内容策略的拒绝（`content_policy_violation`、`content_filter`）也不换：把被一家拒绝的内容送给另一家等于绕过它的策略。`context_length` 只换到**配置里 `models[].contextWindow` 更大**的候选（没配窗口的候选不比、不问）。每一步记下失败的候选：步骤行的 `failed_candidates`（只有错误码，迁移 0012）、`model.usage` 的 `failedCandidates`、`model.error` 的 `outcome`（`retry`、`next_candidate`、`stop`）与 `fault`。

**worker 里的追踪（ADR D57）。** 配了 `OTEL_EXPORTER_OTLP_ENDPOINT`（或 `…_TRACES_ENDPOINT`）时，worker 的每个回合是接在创建运行的请求之后的 span（`idp.run.turn`），每次模型请求（`idp.model.call`）与每个网关工具调用（`idp.tool.call`）是它的子 span，属性只有运行 ID、步、提供者与模型、工具名、结果与 D54 的时间，没有提示、参数、结果；没有收集端时什么都不做。运行存着创建它的请求的 `traceparent`（`runs.traceparent`，迁移 0013；运行的表示不变）。`traceparent` 发给网关（外部工具除外，idp-mcp D51）；发给模型提供者**只在** provider 条目 `tracePropagation: true` 时，不写则等于 `external: false`：外部提供者默认不发，因为它标识我们的请求。

**输出上限与不会结束的回答（ADR D60）。** 每个模型请求都带输出上限（`max_tokens`）：发布件的 `model.maxOutputTokens`（或运行选项 `idp.host/maxOutputTokens`）更低时用它；否则用 provider 条目下 `models[].maxOutputTokens`；都没有就用缺省 32,768，**但只在该模型配了 `contextWindow` 时**；有 `contextWindow` 时再不超过窗口减去这次请求（校正后的估算乘 1.05）之后剩下的。窗口未知又没有明说上限时不发 `max_tokens`，与以前逐字节相同。运维给了值，发布件只能调低、不能调高；没给值时 32,768 只是 HOST 的缺省，发布件明说更高的值照准。32,768 是按本机模型的录得运行定的：122 个长运行里 3,769 个有输出的步，99.9 分位 20,507、最长 24,107 token（多数是推理），两次重复循环写了 35,351 与 35,850 token；**这个数不是预算，只防循环**，对这个模型也只比 8 MiB 的流上限早停约 10 秒，真正的变化是下面两条。回答被上限截断、而窗口并不接近满（上限不是被窗口缩小的；输入加上限离窗口不到 1/16 则算接近满；服务端实际生成的不到上限的九成则是它自己的上下文满了）时，错误码是 `output_runaway`，不是 D40 的"窗口满"；流超过 8 MiB 也是 `output_runaway`，不再是 `decode`。它在同一候选上重试一次（不论 `retry.attempts`，至多两次请求；已经流出文字就不重试），重试改变采样：温度 0.7（发布件自己的温度大于 0 时保留）与 `presence_penalty` 0.5——Strata 没有 `sampling` 配置时是贪心解码，同样的请求会再循环一次；`runawayRetry.keepSampling: true` 关掉这个改变。两次都不结束，运行以 `MODEL_ERROR` 结束，消息写明 `output_runaway`、上限与来源、请求数；被截断的回答带的工具调用一个也不执行（D55）。事件 `idp.host/model.error {…, code: "output_runaway", outcome, limit, completionTokens}`、`idp.host/model.retry {…, temperature, presencePenalty}`，`model.usage.attemptTimings[]` 里每次尝试的 `completionTokens`。**要得到循环防护，给用到的模型配 `contextWindow`（缺省 32,768，按窗口钳制）或 `maxOutputTokens`（明说的值，窗口未知时照发）**；两者都没有的模型行为与 D60 之前相同（不发 `max_tokens`，只有 8 MiB 的流上限，超限现在是 `output_runaway` 并重试）。平台的 compose 与 kind 目前没有 `models`。

**被输出上限截断的回答（ADR D55，修订 D40）。** 回答因输出上限或窗口被截断时，它带着的工具调用一个也不执行（参数哪怕恰好是合法的 JSON），存下的会话只留已说出的文字，运行以 `MODEL_ERROR` 结束，消息与事件 `idp.host/model.cutoff` 说明原因。

**worker 中断时进行中的工具调用（ADR D47，修订 D3）。** worker 停止或崩溃时，HOST 分不清"已经执行完但结果丢了"和"根本没发出去"，所以接手的 turn 对记为 `EXECUTING` 的网关工具调用只在重发无害时才重发：网关现在列出的该工具带 `readOnlyHint` 或 `idempotentHint`（`tools/list` 的 `annotations`），且没有被网关条目的 `neverResendTools`（模式语法与校验同 `externalTools`，匹配即永不重发，不看标注）点名。其余调用（没有标注、标注为假、网关已不再列出该工具、任何子智能体调用）不重发：模型收到一条工具错误，说明调用被中断、结果未知、可能已经做了也可能没有、重做之前先核实；运行的事件流记 `idp.host/tool.interrupted {toolCallId, tool, resent, reason}`（`resent: true` 的是被重发的那次重复；`reason` 为 `worker_stopped`，期限与取消打断的调用为 `deadline`、`canceled`），指标 `idp_host_tool_interruptions_total{resent, reason}` 同样计数。助手消息与它的工具调用记录在同一个事务里写入，工具调用的最终状态与它的工具消息也是；旧数据里缺的记录或工具消息，续跑时补建并照常执行。收到 SIGTERM 的 worker 不再认领运行，进行中的调用（模型调用、工具调用、远程智能体调用）做完，不开始任何新的调用，在下一个边界把运行交还（运行仍是 `RUNNING`，下一个 worker 立刻认领，不等租约过期）；超过 `IDP_HOST_STOP_GRACE`（缺省 `20s`；必须小于平台的终止宽限期：Kubernetes 清单的 `terminationGracePeriodSeconds` 为 30 秒，compose 的 host-worker 为 `stop_grace_period: 30s`）仍在进行的才被中断；`0` 表示立即中断。

`cmi.remember` 是纯配置：运营方打开后，**所有** `context.cmi = true` 的 agent 都会被记住（契约不变；按 agent 退出需要改契约，见 ADR D33）。写入在运行终态之后异步进行，失败只记日志并计入 `idp_host_memory_writes_total{outcome}`（written / replayed / rejected / failed / auth_failed / no_credential），绝不影响运行。

**网关标出的外部工具（ADR D51）。** 合作方上游的工具不必再抄进 `externalTools`：idp-mcp 在 `tools/list` 里给这类工具的 `_meta` 标 `"io.idp.telos/external": true`，HOST 只读这一个键、只认 JSON 布尔 `true`，别的值与别的键都不算，当作与运维列出的对外工具一样：创建运行时（用调用者自己的凭据，每个网关至多一次 `tools/list`，超时 `CatalogueTimeout`，缺省 8 秒）发现发布件允许这样的工具，附件与 CMI 上下文按上限筛，`via` 写明是网关标记的哪些工具；读不到网关的目录按对外算；运行时结果按 `wrap`、引用记为来源、子 agent 同一规则。创建时没有筛过、而网关事后才把被调的工具标为外部的运行，那次调用被拒（`idp.host/tool.denied`，`marked: true`），运行继续。`externalTools` 仍用于我们自己的对外工具（如 `web__*`）。契约不变。

**知识检索工具、引用编号与问题改写（ADR D65–D68，契约 1.12.0，KB8）。** 发布件的 `context` 增两个开关。`tools` 按名字开启 HOST 自己的内置工具（不经网关，同 `resource__read`）：`knowledge__search {query, entities?, asOf?, knowledgeBases?, docTypes?}`、`knowledge__read {documentId, locator?}`（页、幻灯片、标题路径、工作表加范围，各取其一）、`knowledge__find {documentId, text}`；它们要 `context.cmi = true`，`toolExecution: caller` 的运行没有。参数严格校验（键名、类型、长度、列表至多 8 项），错误只告诉模型 HOST 自己的话（404 = "not found or not visible"）。每次调用向 CMI 取一个密封的包，条目带本次运行内连续的引用编号（运行开头那个包的知识条目是 `[1]…`，工具接着往下编；计数器从运行的事件里读，所以接手与恢复不重编），每个包记一条 `idp.host/context.package {packageId, tool, query | documentId+locator | documentId+text, itemCount, firstIndex}`；`contextPackageId` 仍是开头那个包。结果照常包成不可信内容（发布件写 `toolResults: off` 也按 `wrap`，记 `guard.raised`），带对外工具或可能用外部模型的运行按创建时记下的敏感上限筛选，扣下的记 `idp.host/context.withheld`（`source: knowledge_tool`）；结果受 D40 的大小限制，在条目边界截断，只给显示出来的条目编号。CMI 以成员的身份调用：委派模式下创建运行时换得的成员令牌（`cmi.auth.sessionBound` 时带会话内续期用的 refresh token）加密存在运行里（迁移 0014），运行结束时清除；没有成员令牌的委派运行不会改用 HOST 自己的密钥。`rewrite: model` 时，线程里已有前文的运行在开头那次解析之前，由发布件自己的模型经 HOST 受管的模型调用（低思考强度、输出上限 128 token、总时限 `IDP_HOST_REWRITE_TIMEOUT`，缺省 8 秒）把最后一问改写成一句不依赖上文的话，答案必须是非空的一行、至多 300 个字符，否则以及失败、超时都用原话；实际的查询记在 `idp.host/context.rewrite` 与 `context.resolved` 的 `query` 里。缺省 `off`，行为与以前相同。

**运行的来源与引用核对（ADR D49，契约 1.11.0）。** 发布件的工具匹配网关的 `externalTools`（D44）时，HOST 读它的结果里的 `structuredContent.citations`（`{n, title, url, fetchedAt?, sha256?}`），把校验过的条目记成本次运行的来源：编号唯一、至多 100 条、只存标题与地址（不存摘录与页面正文）、标题是不可信的文字，只作数据保存和展示，不会进入任何模型请求；本地子 agent 找到的来源也记入父运行。运行完成时，HOST 在最终回答里找 `[n]`（`[1][2]`、`[1, 2]`；Markdown 链接、代码、`[x]`、`[2026]`、`[1-3]` 都不是引用），在来源里的记 `cited: true`，不在的列入 `unverifiedCitations`；只标记，不拦截、不改回答；既没有对外工具也没有来源的运行不检查。运行的表示（`GET /v1/runs/{id}`、线程的运行列表、`subruns`）多出可选的 `sources`（按 `n` 排序）与 `unverifiedCitations`；运行结束时事件流里有一条 `idp.host/run.sources`（完整列表与丢弃、冲突的计数）。没有新的设置；存储在迁移 `0010_run_sources.sql` 的表 `run_sources`（按租户的行级安全）。

## 许可证

本仓库采用 [PolyForm Noncommercial License 1.0.0](LICENSE)：允许非商业用途，禁止任何商业使用。
