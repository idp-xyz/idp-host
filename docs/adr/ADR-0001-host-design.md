# ADR-0001　IDP HOST 的设计（Agent Hosting & Coordination）

| 项 | 内容 |
|---|---|
| 状态 | 已接受（2026-11-04；M1–M5 的决策均由决策人在 2026-11-03、11-04 作出并实施） |
| 范围 | `idp-host`：Agent 定义与发布、Session / Thread / AgentRun、Harness（推理循环）、Model Router、经 MCP Infrastructure 的工具调用、AG-UI / REST 接口 |
| 依据 | 《IDP TELOS System Architecture v1.0》《Enterprise Architecture Blueprint v1.2》《Repository & Engineering Architecture v1.0》；决策人 2026-11-03 的选择：M1 做 PATH A 全链路；HOST 只做"用户确认"，企业审批归 EER；模型适配器要覆盖全部并可扩展 |
| 参考 | MCP 2026-07-28；AG-UI 协议（事件与 RunAgentInput）；idp-mcp 的 ADR-0001（Release、RLS、四眼、自证、指标、TLS 的做法在这里沿用） |

## 1. 背景与边界

文档把 HOST 定为"智能体托管与运行中枢"：权威状态只有 **AgentDefinition / AgentRun / Session / Skill**；它不是企业任务的可靠执行引擎（Goal / Task / Plan / Execution / Checkpoint / Approval / Evidence 归 EER），不是工具注册与治理（归 MCP Infrastructure），不是记忆与上下文（归 CMI）。

五个子系统里目前只有 idp-mcp 存在。所以 M1 只能做文档里的 **PATH A**：调用方 → HOST ↔ 模型，工具经 MCP Infrastructure；EER / CMI / Workspace 不存在时留接口位。设计上不能堵死文档标注的 P0 开放问题（EER ↔ HOST 的 AgentRun / Execution 映射与恢复边界）：AgentRun 可被 `agent_run_id` 引用，HOST 不持久化任何任务 Checkpoint。

设计前先排除几种常见做法的已知弱点：
- HOST 自建工具目录与工具门，和 MCP Infrastructure 各管一套；
- "审批"在 HOST 里做成另一个人批准的企业审批，和 EER / Workspace 的审批中心冲突；
- 推理循环交给厂商 Agent SDK，于是每一步的落库、中断、续跑和逐个工具调用的门控都被 SDK 的循环隐藏，厂商侧工具（web search、code interpreter、厂商远程 MCP 连接器）还会绕过 MCP Infrastructure；
- 只有一个硬编码的 agent 和一段硬编码的提示词，没有定义、版本与发布；
- SSE 自造事件格式，和 Workspace 之间没有协议。

## 2. 决策

| # | 决策 | 理由 |
|---|---|---|
| D1 | **HOST 是 MCP Infrastructure 的 MCP 客户端，不内建工具**：工具目录 = 网关对该调用者的 `tools/list`；参数校验、授权、调用审计、漂移封锁都由网关负责。HOST 只做两件事：按 AgentRelease 的允许列表取子集，按确认策略决定是否先问用户 | 文档：HOST / EER → MCP Infrastructure 用标准 MCP；idp-mcp 已经把钉住目录、按主体授权、参数校验、azp/act 审计做完了，HOST 再做一套就是两套 |
| D2 | **AgentRelease：不可变、内容寻址的 Agent 定义发布**。AgentDefinition 是可编辑的草稿；发布时把它编译成规范化 JSON + SHA-256 的 Release（`idp.host.agent/v1`），审批人不能是创建人（四眼），一个 agent 同一时刻只有一个 current release。Release 内容：指令（system prompt）、模型配置（provider、model、参数、回退列表）、工具允许列表（网关暴露名或 `*`）、确认策略（默认 `auto` / `confirm`，按工具覆盖）、限制（每 Run 最大步数、每步最大工具调用数、输出 token 上限、Run 超时）。每个 Run 记录它运行的 release 摘要 | agent 的行为由模型、提示词、工具目录和策略共同决定，它们必须一起成为发布件，否则无法复现和回滚；和 idp-mcp 的 Release 同一纪律 |
| D3 | **AgentRun 是持久化状态机，每个 Run 一条追加式事件日志**：`queued → running → (awaiting_input ⇄ running)* → completed | failed | canceled`；每次迁移一个数据库事务；事件按 Run 内 `seq` 单调编号，游标可续。Worker 以租约 + 心跳 + fencing epoch 认领（`SELECT … FOR UPDATE SKIP LOCKED`），租约过期可被重领，旧 Worker 的提交被拒。模型调用不是事务：重领后重跑模型可能重复花费，但不会重复工具调用（工具调用先落库再执行，执行结果落库前该步不算完成） | 可续跑、可审计、多副本安全；这是 PoC 验证过的骨架 |
| D4 | **Harness 是接口，M1 实现为进程内 Go 循环**：一步 = 组装上下文（release 指令 + 线程消息 + 调用方给的 context）→ 调模型（流式）→ 若有工具调用：逐个过允许列表与确认策略 → 执行（经网关）或等待确认 → 下一步。每一步、每个工具调用、每个模型用量都是事件。SDK 形态的 harness（OpenAI Agents SDK、Claude Agent SDK）以后作为另一个实现接入，M1 不做 | 我们要的恰恰是 SDK 循环所隐藏的东西；接口留着，不堵死 |
| D5 | **事件日志是流的唯一来源**：模型的文本增量也落库（按 50 ms 或 32 个增量合并一行），SSE 从事件表读，游标 = 事件 id，`Last-Event-ID` 续传；任何副本都能服务任何 Run 的流 | 多副本下 API 副本和 Worker 副本不一定是同一个；"重放"与"实时"用同一条路，不会有两种真相 |
| D6 | **AG-UI 是 Workspace ↔ HOST 的协议**：`POST /ag-ui/agents/{agentId}` 收 RunAgentInput、回 AG-UI 事件的 SSE（`RUN_STARTED … RUN_FINISHED / RUN_ERROR`，`TEXT_MESSAGE_*`、`TOOL_CALL_*`、`TOOL_CALL_RESULT`、`STEP_*`、`CUSTOM`）；REST（`/v1/...`）管理 agent、release、session、thread、run、确认，`GET /v1/runs/{id}/events` 是同一事件流的续传入口。线程消息的内部形态就采用 AG-UI 的 Message（`id`、`role`、`content`、`toolCalls`、`toolCallId`），适配器负责与各厂商格式互转。RunAgentInput 里的 `tools` 是**前端工具**：HOST 从不执行它们，模型调用了就发 `TOOL_CALL_*` 然后结束本次 Run，结果由下一次 Run 的 `tool` 消息带回；`state` HOST 不持有，原样忽略 | 文档指定 AG-UI / Agent API；自造格式会让 Workspace 适配一次性的东西 |
| D7 | **确认，不是审批**：确认策略要求的工具调用让 Run 进入 `awaiting_input`，记录（工具名、参数规范化摘要、过期时间）；只有 **Run 的所有者**可以确认或拒绝，确认绑定参数摘要，过期视为拒绝；拒绝后模型收到"用户拒绝"的工具结果继续推理。企业审批（另一个人批准、证据、补偿）属于 EER 和 Workspace 的审批中心，HOST 不做。四眼原则只用于 AgentRelease 的发布 | 决策人 2026-11-03 同意；PATH A 没有业务写，用户对自己 agent 的动作把关就是 Claude Code / Cursor 那种权限提示 |
| D8 | **身份**：管理与数据接口用 OAuth bearer JWT（JWKS、issuer、audience、tenant / roles claim），与 idp-mcp 同一验证逻辑；租户隔离由 PostgreSQL **FORCE RLS** 执行。HOST 代用户调网关用 **RFC 8693 委派令牌**：HOST 是 OAuth 客户端，在 Run 创建时用用户的令牌换取 `aud` = 网关资源、`act` = HOST 的令牌，加密后随 Run 保存（AES-256-GCM，密钥来自 `IDP_HOST_CREDENTIAL_KEY[_FILE]`），Worker 执行工具时使用；令牌过期则 Run 失败并说明原因。开发模式另有 `static` 方式：HOST 用配置的网关 API Key（审计里的主体是这把 Key） | 网关的审计要能区分"用户直接调""HOST 代用户调"；idp-mcp 的 OIDC 模式已经验证了这条路 |
| D9 | **模型适配器是接口，M1 实现 OpenAI 兼容与 Anthropic Messages 两个，按名字注册、可继续增加**（Gemini 等）。适配器把厂商格式转成内部的 AG-UI Message / ToolCall，支持流式、工具调用、用量；传输策略：非回环必须 HTTPS、不跟随重定向、响应大小与时长有上限、不读环境代理、错误脱敏。Provider 在配置文件里登记（名字、类型、base URL、凭据文件、允许的模型）；AgentRelease 的模型配置引用 provider 名 + 模型 id + 回退列表。Model Router 在 M1 只做"按配置选 + 按序回退"，并记录每次调用的用量与时延 | 决策人要求全部兼容并可扩展；一个接口两个实现就把扩展点钉住了 |
| D10 | **HOST 不持有企业任务状态**：没有 Goal / Task / Plan / Execution / Checkpoint 表；Run 上预留可选的 `executionRef`（不透明字符串）给 EER 关联；所有工具调用都经网关，HOST 自己不执行任何副作用 | 文档的权威矩阵；也是 PoC 评估里第 1、2 条错位的修正 |
| D11 | **Worker 与 API 在同一个二进制里**（`idp-host serve`），`IDP_HOST_WORKERS` 控制并发数、0 表示只做 API；认领用 SKIP LOCKED，副本任意扩缩 | 部署简单；多副本安全来自数据库而不是拓扑 |
| D12 | **审计、指标、追踪与 idp-mcp 同款**：管理动作进只追加的 `audit_events`；Prometheus 文本指标在独立监听器（Run 计数、步数、模型时延与 token、工具调用结果、确认等待、流数）；OpenTelemetry 追踪用自写的 OTLP/HTTP JSON 导出器，traceparent 传给网关与模型端点。标签不含主体、令牌、提示词 | 同一套运维口径 |
| D13 | **依赖尽量少**：pgx、golang-jwt、OpenTelemetry API/SDK、yaml.v3；JWKS、迁移、规范化 JSON、SSE、模型客户端都自己写。与 idp-mcp 重复的基础代码（约 1.5k 行）**复制**而不是共享模块：仓库文档禁止跨仓库引用未发布的依赖，而发布一个 Go kit 要等第三个使用方出现 | 供应链面小；重复的代价已知、有限 |
| D14 | **契约**：M1 结束时发布 `idp.telos.host` 1.0.0（AgentRelease 文档与 Envelope、RunRecord；由 `api/contracts` 产出，带实现侧一致性测试，同 idp-mcp）；REST 与 AG-UI 入口由 `api/openapi.yaml` 描述并由测试保证与路由一致。AG-UI 本身是外部协议，按其 1.x 事件集实现，不登记为我们的契约 | 与 idp-contracts 的四条规则一致 |

## 3. 第一阶段范围（M1：PATH A 全链路）

**包含**
- 控制面（REST，管理角色）：Agent 定义的创建 / 修改、发布（编译 Release、四眼审批、设为 current、回退）、provider 列表（只读）、审计查询。
- 数据面：Session / Thread / Run 的创建与查询；Run 的 AG-UI 流与事件续传；确认与拒绝；取消。
- Worker：认领、租约、心跳、fencing；Harness 循环；模型适配器（OpenAI 兼容、Anthropic）；网关 MCP 客户端（`server/discover`、`tools/list`、`tools/call`，含 SSE 结果与 `_meta` 信封）；委派令牌换取与加密保存。
- PostgreSQL 迁移与 RLS；单元测试；真实 PostgreSQL 上的集成测试（多 Worker 竞争、租约过期重领、fencing、跨租户不可见、崩溃后续跑）；compose 端到端（HOST + idp-mcp 网关 + 真实 MCP SDK 上游 + 确定性假模型服务器，可选 Keycloak 委派）；可选的真实模型门禁（环境变量给出端点与模型，OpenAI 兼容与 Anthropic 各一组）。
- 镜像（scratch）、compose、Kubernetes 清单与一致性测试；指标；追踪；TLS。

**不包含（后续阶段）**
- Skill 注册与版本（M2：Skill = 指令片段 + 工具子集 + 资源，按 release 引用）；子 agent（父子 Run，AG-UI 的 `Subagent*` 事件）；租户级配额与预算（token、费用、并发 Run——`governance` 的首个内容；M1 只有每个 Run 的步数与时限）；A2A 委派；SDK 形态的 harness；google（Gemini）等更多适配器；智能模型路由（按成本 / 延迟 / 能力）。
- 与 EER 的 Execution / AgentRun 契约、与 CMI 的 Context API、与 Workspace 的联调——对方不存在，只留 `executionRef` 与 `context` 的位置。
- 评估门禁（固定评估集上的回归）、提示词注入检测、内容安全过滤。

## 4. 工程结构

仓库 `idp-host`，Go module `idp-host`，单个可执行程序 `cmd/idp-host`（`serve` = API + Worker 池、`worker` = 只跑 Worker 池、`migrate` / `ensure-login` / `seed-tenant` / `dev-token` / `new-key`）。目录：`internal/{agent,run,harness,worker,model,mcpclient,credential,config,api,agui,authorization,observability,transport,canonical}`、`adapters/{identity,store/postgres,providers/openai,providers/anthropic}`、`api/{openapi.yaml,contracts/}`、`migrations/`、`tests/{integration,e2e,live,deploy,fixtures}`、`deploy/`。单元测试放在被测包旁边。

《Repository & Engineering Architecture v1.0》第 02 节给 `idp-host` 建议了一棵目录树（建议稿，未签署）。实际目录按决策定，对照如下；命名不同处不改名，语义缺口进入第 3 节的后续阶段：

| 文档建议 | 实际 | 说明 |
|---|---|---|
| `cmd/host-api`、`cmd/host-worker` | `cmd/idp-host serve` / `cmd/idp-host worker` | 同一二进制的两种模式；Worker 副本可独立于 API 副本扩缩（D11） |
| `internal/agent/{registry,definition,instance,lifecycle}` | `internal/agent`（定义、发布 = lifecycle）、`internal/run`（= instance） | 一致 |
| `internal/harness/{runtime,reasoning,tool-calling,subagents}` | `internal/harness`；tool-calling 是"交给网关"（D1） | `subagents` 进入后续阶段 |
| `internal/model/{adapters,routing,usage}` | `internal/model` 接口 + `adapters/providers/*`；routing 只做回退；usage 在 `run_steps` 与指标 | 一致 |
| `adapters/{openai,anthropic,google,local}` | `adapters/providers/{openai,anthropic}` | google 待做；`local` 不需要——本地服务说的就是 OpenAI / Anthropic 格式 |
| `harness-workers/`（可选 Python Harness Worker） | 未做；`Harness` 是接口（D4） | 要做先定 HOST↔worker 协议 |
| `internal/session` | `sessions` 表（薄） | 等 Workspace 的需要 |
| `internal/skills`、`internal/federation` | 未做 | 后续阶段 |
| `internal/governance` | 分散在角色、发布里的确认策略、审计 | 租户级配额与预算进入后续阶段 |
| `internal/protocol` | `internal/agui` | 一致 |
| （无） | `internal/mcpclient`、`internal/credential`、`internal/canonical` | 来自 D1、D8：文档假设 HOST 自己做 tool-calling |

## 5. 后果

- 正面：HOST 的全部行为由发布件决定并可复现；工具治理不重复；流与重放同源；多副本安全；身份链路上网关能看见"谁代表谁"。
- 负面：基础代码与 idp-mcp 重复；自研 harness 要自己跟进各厂商 API 的演进；模型重跑的费用重复由租约长度与步数上限兜底，不是 exactly-once。

## 7. M2：可对外服务的 HOST（2026-11-03 决策）

决策人 2026-11-03 选择：M2 只做不依赖其他子系统的四项，然后转向 idp-eer 的 ADR。子 agent、评估门禁、A2A、SDK harness 等有消费方时再做。

| # | 决策 | 理由 |
|---|---|---|
| D15 | **租户级治理（governance）**：每个租户一份策略（`tenant_policies`，管理员维护）：最大并发 Run、每日 token 预算、每个 Run 的 token 上限默认值；用量从 `run_steps` 聚合（滚动 24 小时）。创建 Run 时超并发或预算已尽 → 429 `QUOTA_EXCEEDED`；每一步开始前再查一次，Run 中途耗尽 → FAILED `BUDGET_EXCEEDED`；Release 的 `limits.maxTokensPerRun` 覆盖租户默认值。没有策略的租户用保守默认（并发 10，预算不限）。管理 API 读写策略、读用量；指标记录拒绝次数 | M1 只有每个 Run 的步数与时限，对外服务必须有租户级的闸 |
| D16 | **与真实 Keycloak 联调委派**：`MODE=oidc` 的 compose 拓扑——Keycloak（启用 `token-exchange-standard`、`token-exchange-delegation`、`parameterized-scopes`）、网关记录改为 `jwt` 模式、HOST 用 JWKS 验用户令牌并以机密客户端 `idp-host` 换取委派令牌（`actor_token` = 自己的客户端凭据令牌，`audience` = 网关资源），用户令牌带 `may_act`。验收：网关的调用审计里主体是用户、`actor` 是 HOST | 关掉 M1 唯一的身份侧局限；IdP 侧的规则 idp-mcp 已验证过 |
| D17 | **Skill 注册与发布**：Skill = 指令片段 + 工具允许子集 + 确认覆盖，和 Agent 一样有草稿与四眼发布的 SkillRelease（`idp.host.skill/v1`）。AgentDefinition 以名字（可钉版本）引用 Skill；编译 AgentRelease 时把被引用的 SkillRelease 的内容与摘要**嵌入**进去，AgentRelease 仍然自包含、内容寻址；Run 时指令 = agent 指令 + 各 skill 指令，工具 = 并集，确认策略取更严格者。契约 `idp.telos.host` 升 1.1.0（新增 `skills`、SkillRelease，向后兼容） | 没有 Skill，每个 agent 都要复制提示词；嵌入而不是引用，发布件才可复现 |
| D18 | **google（Gemini）适配器**：`generateContent` 的 SSE 流（`alt=sse`）、`functionDeclarations` / `functionCall` / `functionResponse`、`usageMetadata`、thought 部分记为推理；工具调用没有 id，由适配器铸造并在回传 `functionResponse` 时按 id 找回函数名。测试替身加 Gemini 线上格式；真实端点门禁在有端点时运行 | 决策人要求全部兼容；Gemini 是第三种主流线上格式 |

**M2 范围**：D15–D18 及其测试（单元、集成、端到端），契约 1.1.0 发布到 idp-contracts，ADR 第 8 节记录实测结果。

## 8. M2 实施结果（2026-11-03，WSL 实测）

**通过**
- 单元：Gemini 适配器（流式文本、functionCall / functionResponse 往返、thought 作为推理、用量、错误脱敏、端点策略）；AgentRelease 嵌入 skill 的校验（摘要格式、引用一一对应）；契约 1.1.0 一致性（SkillRelease 示例经 `OpenSkill`、嵌入 skill 的 AgentRelease 示例经 `Open`，3 种只有实现能判定的交叉引用变体单独检查）。
- 集成（真实 PostgreSQL，12/12）：新增 **governance**（默认策略；并发上限 → 第二个 Run 429 `QUOTA_EXCEEDED`；用量视图；每日预算耗尽 → 429；每 Run 上限 → 第一步后 FAILED `BUDGET_EXCEEDED`；Release 的 `maxTokensPerRun` 覆盖租户默认；策略按租户隔离）和 **skills**（引用未发布 skill 的 agent 不能编译 → 409；skill 四眼发布；agent release 嵌入 skill 的摘要与版本；Run 看到 skill 带来的工具和确认规则，指令里出现 `## Skill: …`；skill 发新版不改已编译的 release；钉版本的引用停在钉住的版本）。
- 端到端（compose）：`MODE=apikey` 14/14；**`MODE=oidc` 14/14**——Keycloak 26.8.1（启用 standard exchange、delegation、parameterized-scopes），真实用户以密码授权登录，HOST 用 JWKS 验用户令牌，以机密客户端 `idp-host` 换取委派令牌（`actor_token` = 自己的客户端凭据令牌，用户令牌带 `may_act`），网关（jwt 模式）接受；**网关的调用审计里主体是用户的 `sub`、`actor` 是 `idp-host`**。
- 契约：`idp.telos.host` 1.1.0 发布，idp-contracts 的校验新增"次版本只做新增"的不变量（`idp_telos_host_1_1_0_is_additive_over_1_0_0`），88/88 用例、19/19 不变量。

**局限（不标 PASS）**
- Gemini 适配器只对测试替身验证过，没有真实 Gemini 端点（live 门禁接受 `IDP_LIVE_GEMINI_URL`，未运行）。
- 租户预算按滚动 24 小时的 token 数计，没有按模型的价格表与费用；并发上限是整租户的，没有按用户。
- Keycloak 在端到端里是 `start-dev`（HTTP）；生产形态（TLS、PostgreSQL、`start`）idp-mcp 验证过，HOST 侧未重复。用户登录用的是密码授权，不是授权码 + PKCE（idp-mcp 验证过授权码路径）。
- Skill 的"资源"（提示词之外的附件）未做；合并规则是固定的（指令拼接、工具并集、确认取严）。

## 9. HOST 作为 EER 的 Agent 回合提供方（2026-11-03 决策）

决策人 2026-11-03 选择"HOST 提供"：EER（Temporal + PostgreSQL，已实现）的 Agent 回合不再由 EER 自带的模型适配器计算，而由 HOST 提供；EER 侧的对应决策是 EER ADR-036（Proposed）。HOST 不做 EER 的事：授权、企业审批、派发、补偿、验证、审计都留在 EER。

| # | 决策 | 理由 |
|---|---|---|
| D19 | **调用方执行工具（caller mode）与调用方选项**：`RunAgentInput.forwardedProps` 里 `idp.host/` 命名空间的键是 HOST 的选项（其他键原样保存、忽略；未知的 `idp.host/` 键 → 422）。`idp.host/toolExecution: caller`：模型只看到调用方给的前端工具，不看 release 的网关工具，不换委派凭据、不碰网关，只调一次模型（`maxSteps = 1`），模型要的调用作为前端工具调用交回（`tool_calls` 记 `FRONTEND`）；模型若要未声明的工具，下一步撞步数上限 → `STEP_LIMIT`。`idp.host/releaseDigest`：运行该 agent 的这份已批准（或已被取代）的发布件，而不是当前发布件，没有 → 409。`idp.host/executionRef`：记录在 Run 上。`idp.host/maxOutputTokens`：压低本次 Run 的模型输出上限（只能更小）。这四项都进入幂等摘要。这些选项是契约 `idp.telos.host` 1.2.0 的 RunOptions（`urn:idp:telos:host:1.2.0#/$defs/RunOptions`），调用方依赖的是这个版本号，而不是 HOST 的某次提交 | ADR-033（EER）的"模型提议、Runtime 处置"与 AG-UI 前端工具语义一致；钉住摘要才能让 EER 的计划复现同一个 agent |
| D20 | **映射**：EER 的一个回合 = HOST 的一个 Run，新线程（threadId = runId），每次带完整历史（HOST 线程不承担恢复）；runId 由回合的 `request_id` 推出，Activity 重试即重放已记录的 Run，不重调模型；HOST 的可重试失败（`MODEL_ERROR` / `INTERNAL` / `RUN_TIMEOUT`）由 EER 换派生 runId 重来，次数有上限。EER 的能力作为前端工具（名字按 EER 的 `eer_<能力>_<摘要>` 规则），工具结果作为 `tool` 消息回传。HOST 的用量事件汇总为回合用量 | HOST 的 Run 本来就是持久、可重放的事件日志；EER 的恢复语义不依赖 HOST 的状态 |
| D21 | **身份**：回合的模型身份 = `provider_id: idp-host`、`model_id` = agent 名、`model_version` = release 摘要。EER 的 Intelligence worker 用自己的工作负载身份（OAuth 客户端凭据；开发时用静态令牌）调 HOST，角色 `host-user`；HOST 从令牌得出租户；不转发最终用户令牌（EER ADR-035）。caller mode 不需要网关凭据 | 令牌不跨持久边界；EER 的 Python 平面本来就不能持有用户令牌 |

## 10. EER 集成实施结果（2026-11-03，WSL 实测）

**通过**
- HOST 单元：`ParseOptions`（合法组合、`server` 归一、命名空间外的键忽略、8 种非法输入拒绝）；harness `TestCallerExecutesTools`（只给前端工具、零网关接触、一步、输出上限压到 300、未声明工具 → `STEP_LIMIT`）。
- 契约：`idp.telos.host` 1.2.0 新增 RunOptions（CONTRACT）。一致性测试：示例经 `run.ParseOptions` 解析后原样往返、`run.Options` 的 JSON 字段与 schema 一一对应、12 种 API 拒绝（422）的选项值 schema 同样拒绝（把 schema 的上限放宽后测试失败，确认它真的在比对）。idp-contracts 校验 106/106 用例、18/18 不变量，1.2.0 对 1.1.0、1.0.0 都只做了新增，发布副本与生产者逐字节相同。
- HOST 集成（真实 PostgreSQL，13/13）：新增 **caller-executes-tools**（AG-UI 带 `forwardedProps`；`executionRef` 记录；钉住第一份已批准的发布件；一步；上限 256；网关调用数不变；回传工具结果的第二个回合在新线程上给出含结果的最终答案；重放不重调模型；未知摘要 409；未知选项 422）。
- EER（分支 `host-agent-turn-provider`）：`HostModelRuntime` 适配器 + 配置（`EER_AGENT_MODEL_RUNTIME=idp-host`、`EER_HOST_*`）+ 组装；单元 156/156（新增 22：请求形态、调用 id 往返、身份失败即拒、重试与上限、HTTP 与 RUN_ERROR 的映射、发布件摘要核对、未声明工具与 HOST 自行执行即拒、令牌缓存与刷新、令牌不进载荷、出口白名单、超时）。
- **真实链路**（EER Activity → 本机 `idp-host serve` → 本地 Qwen 3.8 flash，`make intelligence-host-integration`，2/2）：第一回合 Qwen 经 HOST 提议 `calculator.add {"a":19,"b":23}`；同一请求再评估得到同一个 provider 调用 id（重放，HOST 只有一个 Run、一个模型步骤）；回传 `{"sum":42}` 的第二回合给出含 42 的最终答案；不存在的摘要 → `refused / provider_identity_mismatch`。HOST 侧每个回合一个 COMPLETED Run、`step_count = 1`、`toolExecution = caller`、`executionRef` 为 EER 的 execution id、摘要为钉住的发布件、`tool_calls` 只有 `FRONTEND` 记录（HOST 未执行任何工具）。

- **EER 全链路**（Temporal 1.7.3 开发服务器 → EER Go Runtime 的持久化本地 agent loop 工作流 → Python Intelligence worker（`EER_AGENT_MODEL_RUNTIME=idp-host`）→ HOST → 本地 Qwen，`make agent-turn-host-integration`，连续 5/5）：第一回合 Qwen 经 HOST 提议 `calculator.add {"a":19,"b":23}`，Go Runtime 在工作流里执行这个本地工具并把结果作为下一回合的历史，第二回合给出 42；响应的模型身份是 `idp-host` / `eer-agent` / 钉住的摘要。每个回合在 HOST 侧是一个 caller 模式的 Run（一步、`executionRef` = EER 的 execution id、只有 `FRONTEND` 工具记录）。早先用 EER 共用夹具（工具 schema 只有 `{"type":"object"}`）跑时，Qwen 第一回合猜错参数名（`{"numbers":[19,23]}`），被 Runtime 以 `invalid_arguments` 拒绝，下一回合自行纠正——即 Runtime 的处置权在这条链路上生效。

**局限（不标 PASS）**
- 工作负载身份只用了开发令牌；OAuth 客户端凭据路径只对测试替身验证过，没有对 Keycloak。
- EER 侧的 Ollama 回合适配器仍在（ADR-036 的单写者切换：HOST 适配器通过回合契约与循环场景后删除）。

## 6. M1 实施结果（2026-11-03，WSL 实测）

**通过**
- 单元（`make test`，-race）：AG-UI 消息与事件；AgentRelease 的规范化、密封、篡改与未知字段拒绝；OpenAI 兼容与 Anthropic 适配器（流式文本、工具调用、推理文本、用量、非流式回退、错误脱敏、重定向不跟随、端点策略、Anthropic 的交替合并规则）；MCP 客户端（JSON 与 SSE 两种应答、错误映射）；harness 8 个场景（纯回答、自动工具、确认→确认/拒绝、拒绝的工具、步数上限、前端工具、回退与模型失败、fenced 后不落任何终态、缺凭据）；契约一致性（示例生成、字段集与 required 等于 Go 结构体、13 种 Release 与 8 种 Run 变体双向拒绝）；OpenAPI 与路由一致；Kubernetes 清单一致性。
- 集成（真实 PostgreSQL，`make test-integration`，10/10）：无令牌的健康端点；发布与四眼；幂等创建（重放 200、不同内容 409、同线程同时只允许一个 Run）；SSE 流与分页；经静态凭据的工具调用；确认（确认、拒绝、过期）与从中断事件 id 续传；排队中取消；租约过期被第二个 Worker 接管、旧 Worker 的写入与心跳被拒；20 个并发相同请求只产生一个 Run；委派模式下网关收到换取的令牌；AG-UI 端点（流、前端工具、客户端回传整段对话时按消息 id 与工具调用 id 去重）；跨租户不可见、管理员可读本租户。
- 端到端（`deploy/e2e.sh`，compose：HOST 的 API 容器（`serve`，0 个 Worker）+ 独立的 Worker 容器（`idp-host worker`）+ idp-mcp 的发布镜像 + 两个真实 MCP SDK 上游 + 模型替身容器，14/14）：健康与认证；发布；AG-UI 运行经真实网关调用 `minimal__sum`（TOOL_CALL_RESULT 为真实结果）、文本流式、Run 记录带 release 摘要、线程完整；REST 运行触发确认、所有者确认后执行 `minimal__echo`；网关的调用审计记录了 HOST 服务身份的两次调用；API 进程与 Worker 进程各自暴露指标且不含主体。API 创建的 Run 由另一个进程认领并完成，证明 Worker 可独立扩缩。
- 真实模型门禁（`make test-live`，本地 Qwen 3.8 flash，OpenAI 兼容与 Anthropic 两种线上格式各 1 组）：流式文本、工具调用（`sum_weights {"weights_kg":[12.5, 0.75]}`）、工具结果回传后的最终答案、用量。

**实测发现并修复**
- 健康端点被认证中间件挡住（端到端发现），改为 `/healthz` `/readyz` `/version` 免认证。
- SSE 在指标与追踪中间件包裹下无法 flush（端到端发现），改用 `http.ResponseController`，中间件只需实现 `Unwrap`。
- 20 个并发相同请求竞争创建线程时出现唯一约束冲突返回 500（集成发现），改为在唯一冲突时重试事务，第二次读到已存在的线程与 Run 并重放。
- AG-UI 客户端每次回传整段对话，改标过的助手消息会被重复追加，改为按消息 id、工具调用 id 和已回答的 `toolCallId` 去重。
- 本机 Docker 下容器到达不了 `host.docker.internal`，模型替身改为 compose 内的容器（`deploy/fixtures/Dockerfile.fakemodel`）。

**局限（不标 PASS）**
- 端到端用的是网关的静态 API Key（审计主体是 HOST 的服务身份）；RFC 8693 委派只在集成测试里对假授权服务器验证过，与真实 Keycloak 的联调待 MODE=oidc 的拓扑（idp-mcp 已验证 IdP 侧）。
- Workspace、EER、CMI 不存在：`context` 与 `executionRef` 只是预留位；前端工具按 AG-UI 语义实现，但没有对真实 AG-UI 客户端联调；中断后续传时再次发 `RUN_STARTED` 是本项目的约定。
- MODEL=live 的端到端需要容器可达的模型端点，本机未跑；真实模型只在适配器层门禁过。
- Kubernetes 清单未在集群上应用。
- Skill 注册、A2A、SDK 形态的 harness、评估门禁均未开始（见第 3 节）。

## 11. M3–M5：后续阶段（2026-11-04 决策）

决策人 2026-11-04 选择把第 3 节列为"后续阶段"的全部条目做完，分三个里程碑：M3（子 agent 与 Skill 补全、补齐 M2 的局限）、M4（评估门禁与安全过滤）、M5（模型路由、A2A、SDK 形态的 harness）。与 CMI、Workspace 的联调仍因对方不存在而不做（CMI 后来在 idp-cmi 中建成，联调见第 14 节 D32）。

| # | 决策 | 理由 |
|---|---|---|
| D22 | **子 agent = 父 Run 委派出的子 Run**。AgentDefinition 以名字（可钉版本）引用其他 agent（`subagents`，每项带给模型看的 `description`），编译 release 时解析为被引用 agent 当时的当前发布件并嵌入其 `agentId`、`version`、`digest`（不嵌入内容，子 agent 的行为由它自己的发布件决定）。每个引用成为模型可见的一个工具 `agent__<name>`（参数 `{input}`）；模型调用它时，harness 在父 Run 的 claim 下创建子 Run：新线程（threadId = 子 runId）、同一所有者与租户、`parentRunId` 与 `depth` 记在子 Run 上、运行钉住的发布件、继承 `executionRef` 与调用方 context、同一网关时继承委派凭据；幂等键由父 runId 与 toolCallId 推出，重领后重放而不重建。父 Run 的工具调用记为 `DELEGATED`（带子 runId），父 Run 进入新状态 **`AWAITING_SUBAGENT`**：不可认领、计入并发、不发 `RUN_FINISHED`（父流不中断），只发 `CUSTOM idp.host/subagent.started`。子 Run 终止时在同一事务里给父 Run 追加 `idp.host/subagent.finished`，当父 Run 的全部子 Run 都终止后把父 Run 置回 `QUEUED`；子 Run 等待确认时父流收到 `idp.host/subagent.awaiting_input`，所有者用现有接口对子 Run 的工具调用做决定。父 Run 的下一轮把子 Run 的最终助手消息作为工具结果交给模型（子 Run 失败或取消 → 工具结果为错误）。取消父 Run 级联取消活动中的子 Run。深度由 `limits.maxSubagentDepth`（默认 1，最大 5）限制；扇出由 `maxToolCallsPerStep` 限制；caller 模式不提供子 agent 工具。`GET /v1/runs/{id}/subruns` 列出子 Run。AG-UI 1.x 没有标准的子 agent 事件，用 CUSTOM 事件与 AgentRun 的 `parentRunId` 表达 | 子 Run 复用全部已有纪律（状态机、事件日志、租约、确认、配额、审计），没有第二套执行路径；父 Run 不占 Worker 等待，死锁不可能；嵌入摘要而不嵌入内容，父发布件仍可复现而不臃肿 |
| D23 | **Skill 资源**：SkillDefinition 增加 `resources`（名字、`mediaType` 为 text/plain、text/markdown 或 application/json、内容、`inline`）。`inline`（默认）的资源在合并指令时以 `### Resource: <name>` 附在该 skill 指令之后；非 inline 的资源通过 HOST 内部工具 `resource__read`（参数 `{skill, name}`）按需读取——它读的是发布件自身的内容，不是外部副作用，不经网关，记为普通工具调用（状态 DONE）。单个资源 ≤ 256 KiB，一个 skill 的资源合计 ≤ 1 MiB，名字在 skill 内唯一。资源随 skill 发布件进入摘要 | 提示词之外的参考资料（示例、表格、规则）是 skill 的一部分，必须一起发布、一起复现；大的资料按需读才不吃掉上下文 |
| D24 | **Skill 合并规则可配置**：AgentDefinition 增加 `skillMerge`：`instructions` 为 `append`（默认，现状）、`prepend` 或 `placeholder`（agent 指令里的 `{{skills}}` 处）；`tools` 为 `union`（默认，现状）或 `ceiling`（agent 的 allow 是上限，skill 不能带来 allow 之外的工具，越界的 skill 工具在编译时报错）；`confirmation` 为 `strictest`（默认，现状）或 `agent`（agent 的按工具覆盖优先）。缺省值由 Normalize 填入，编译结果不变 | 固定规则对"skill 不得越权"的租户不够：工具上限与指令位置是治理要求，不是口味 |
| D25 | **补齐 M2 的局限**：(a) EER 的 Intelligence worker 以 OAuth 客户端凭据对真实 Keycloak 取令牌调 HOST，在 HOST 的 MODE=oidc compose 拓扑里加一个带服务账号的机密客户端 `eer-intelligence`（realm 角色 `host-user`、`tenant_id` 属性），`make intelligence-host-integration` 走这条路；(b) HOST 的 e2e 用户登录改为授权码 + PKCE（公共客户端 `host-app`，与 idp-mcp 的 e2e 同一做法），不再用密码授权；(c) 删除 EER 侧的 Ollama 回合适配器（ADR-036 第 6 条的条件已满足：HOST 适配器通过了回合契约与循环场景），Ollama 分类 provider 不动；(d) 真实 Gemini 端点门禁在决策人提供端点与密钥时运行，否则保持"未运行"。Keycloak 的生产形态（TLS、PostgreSQL、`start`）由 idp-mcp 验证过，HOST 不重复 | 这些是 M2 实测时标明的空白，补上才算对外服务 |
| D26 | **评估门禁**（M4）：AgentDefinition 增加 `evaluation`：固定评估集（每条用例 = 输入消息 + 期望：`contains` / `notContains` / `matches`（正则）/ `toolCalled` / `noToolCall` / `maxSteps`）和 `required`（默认 false）。`POST /api/v1/releases/{id}/evaluate` 对候选发布件按用例逐个创建**评估 Run**（`purpose = evaluation`，owner 为评估人，线程 `eval:<releaseId>:<n>`，钉住候选发布件的摘要，网关凭据同普通 Run），由 Worker 正常执行；评估结果（每条用例通过与否、实际输出摘要）记在发布件上（`evaluation_runs`）。`required` 为 true 的发布件在最近一次评估全部通过前不能被批准（409 `EVALUATION_REQUIRED`）；评估 Run 不计入租户并发但计入 token 预算 | 发布前跑一遍固定集是防止提示词或模型变更悄悄改变行为的最低门槛；用普通 Run 跑评估，评估看到的就是生产路径 |
| D27 | **输入护栏**（M4）：AgentDefinition 增加 `guards.input`：对工具结果与调用方 context 做确定性检测——指令注入模式（"ignore previous instructions"类句式、角色标记伪装如 `<|system|>` / `SYSTEM:`、嵌入的工具调用 JSON）、不可见与控制字符、超长重复。策略 `wrap`（默认：工具结果以"以下是不可信的工具输出"定界封装后交给模型，命中时发 `CUSTOM idp.host/guard.flagged`）、`block`（命中的内容替换为说明文字）、`off`。检测是规则，不是模型；规则集随 HOST 版本，命中记指标 | HOST 是唯一能看见"工具结果进入模型"这一步的地方；定界封装是零成本的基线 |
| D28 | **内容安全过滤**（M4）：`guards.output`：对模型输出做确定性过滤——租户策略里管理员维护的禁用模式（正则）、内置 PII 模式（邮箱、电话、带 Luhn 校验的卡号）的 `redact`，以及可选的外部分类端点（配置文件里登记，HTTP POST 文本得到 `allow / block` 与类别，超时即按 `onError` 策略放行或拦截）。流式输出按合并行增量检查；命中 `block` 时取消本次模型调用、Run 以 `GUARD_BLOCKED` 失败、已落库的增量后跟一条 `idp.host/guard.blocked` 事件；`strict: true` 时先缓冲再放行，代价是失去流式。租户策略的禁用模式对该租户所有 agent 生效 | 对外服务的最低要求；可插拔的外部分类器让更强的模型侧过滤接在同一个位置 |
| D29 | **模型路由**（M5）：ModelProfile 增加 `routing`：`strategy` 为 `ordered`（默认，现状 = 主 + 回退按序）、`cost`、`latency` 或 `capability`；`candidates` 带每个候选的 `costPer1kInput / costPer1kOutput`、`capabilities`（`tools`、`json`、`reasoning`、`vision`、`longContext`）与 `maxContextTokens`。路由器先按本次请求的需要过滤（需要工具则只留有 `tools` 的候选、估算的输入 token 不超过 `maxContextTokens`），再按策略排序：`cost` 按单价、`latency` 按进程内的 EWMA 时延与近期错误率（指标导出）、`capability` 按声明的能力数；排序结果就是本步的尝试顺序，`idp.host/model.routed` 事件记录决定与理由。路由器不学习、不调用外部服务 | 第 4 节的 `internal/model/routing` 空位；进程内的确定性路由器可测、可审计，够用到有真实多模型账单为止 |
| D30 | **A2A**（M5）：HOST 作为 A2A（Agent2Agent 协议 0.3，JSON-RPC over HTTP）服务端把每个已发布的 agent 暴露为 A2A agent：`/a2a/agents/{name}/.well-known/agent-card.json` 与 `/a2a/agents/{name}`（`message/send`、`message/stream`、`tasks/get`、`tasks/cancel`）；A2A task = HOST Run（`contextId` = 线程），任务状态由 Run 状态映射（`submitted / working / input-required / completed / failed / canceled`），最终助手消息为 artifact。认证与现有接口相同（bearer）。HOST 作为 A2A 客户端：子 agent 引用可以指向远端 agent card（`subagents[].a2a.cardUrl`），委派时以 `message/send`（阻塞）发起远端任务，凭据用 HOST 配置里登记的该远端的客户端凭据；远端任务进入 `input-required` 视为失败（跨 HOST 的确认不在本阶段）。端到端用 HOST 自己做对端验证 | 没有对端时也能让 HOST 的 agent 可被标准协议消费；远端委派复用 D22 的全部父子纪律 |
| D31 | **外部 harness 协议**（M5）：`Harness` 成为接口，进程内 Go 循环是实现之一；增加 `external` 实现：按 release 的 `harness.kind = external` 与 `harness.worker` 把一步的"推理"交给 HOST 配置里登记的 harness worker（HTTP：HOST POST 该步的上下文——指令、消息、可用工具、限制；worker 以 SSE 流回文本增量、工具调用与结束原因）。持久化、门控、确认、工具执行（经网关）、配额全部仍在 HOST；worker 只做模型调用，可以用任何 SDK（OpenAI Agents SDK、Claude Agent SDK）实现，但它的工具调用必须回到 HOST 执行，SDK 自带的工具与远程连接器不可用。随 HOST 提供一个 Python 参考 worker（不依赖厂商 SDK）与测试替身 | 第 4 节"要做先定 HOST↔worker 协议"；把边界放在"一步的推理"上，SDK 的循环就不再隐藏任何该落库与门控的东西 |

**契约**：D22–D24 是 `idp.telos.host` 1.3.0（AgentRun 增 `parentRunId`、`depth`、`purpose`，status 增 `AWAITING_SUBAGENT`；Definition 增 `subagents`、`skillMerge`、`limits.maxSubagentDepth`；SkillDefinition 增 `resources`；AgentRelease 增 `subagents`）；D26–D31 进入 1.4.0 / 1.5.0，均只做新增。实施结果记在第 12 节。

## 12. M3、M4 实施结果（2026-11-04，WSL 实测）

**M3（D22–D25）**
- 子 agent（D22）：`subagents` 引用在编译时解析为被引用 agent 当时发布件的摘要并嵌入；模型以 `agent__<name>` 工具委派；子 Run 有自己的线程、继承所有者、context、`executionRef`、追踪 id 与同网关的委派凭据；父 Run 进入 `AWAITING_SUBAGENT`（不可认领、计入并发、流不中断），子 Run 终止时在同一事务里唤醒父 Run；子 Run 等待确认时父流收到 `idp.host/subagent.awaiting_input`；取消父 Run 级联取消子 Run；子 Run 被直接取消时父 Run 得到工具错误并继续；租户并发把等待中的父 Run 与子 Run都算在内，配额不足时委派被拒绝为工具错误；深度由 `limits.maxSubagentDepth` 限制，caller 模式不提供子 agent 工具；`GET /v1/runs/{id}/subruns`。迁移 0004。
- Skill 资源（D23）：内联资源附在 skill 指令之后，按需资源经 HOST 内部工具 `resource__read` 读取，不经网关。
- Skill 合并规则（D24）：`skillMerge` 的 append / prepend / placeholder、union / ceiling（越界的 skill 工具在编译时 422）、strictest / agent。
- 补齐 M2 局限（D25）：(a) EER 的 Intelligence worker 以 OAuth 客户端凭据（Keycloak 机密客户端 `eer-intelligence`，服务账号，租户与角色为硬编码声明）调 HOST，`make intelligence-host-integration` 对 MODE=oidc 的 compose 栈 2/2 通过（工具调用往返、重放不重调模型、未批准发布件 → `provider_identity_mismatch`）；(b) e2e 的用户登录改为授权码 + PKCE（公共客户端 `host-app`），compose 端到端 18/18 通过（含 PKCE 声明、工作负载令牌声明、工作负载身份的 caller 模式运行、工作负载不是管理员）；(c) EER 侧的 Ollama 回合适配器已删除（EER PR #19，ADR-036 转为 Accepted；没有 `EER_HOST_*` 的 worker 只服务 Decision，拒绝 Agent Turn）；(d) 真实 Gemini 端点门禁仍未运行（无端点）。
- 契约 `idp.telos.host` 1.3.0 发布（SubagentRef、EmbeddedSubagent、SkillMerge、Resource；AgentRun 的 parentRunId、depth、AWAITING_SUBAGENT），对 1.2.0、1.1.0、1.0.0 只做新增。

**M4（D26–D28）**
- 评估门禁（D26）：`evaluation` 随发布件；`POST /api/v1/releases/{id}/evaluate` 把每条用例作为候选发布件的普通 Run 执行（`purpose = evaluation`，临时密封的候选信封，所有者为评估人，不计入租户并发），Run 终止时在同一事务里判定并记录；`required` 的发布件在最近一次评估 PASSED 前批准返回 409 `EVALUATION_REQUIRED`。迁移 0006。
- 输入护栏（D27）：工具结果与调用方 context 以 `<untrusted source=…>` 定界交给模型，并在消息前附一条说明；确定性规则检测指令覆盖、角色伪装、人格切换、嵌入的工具调用 JSON、泄露提示词请求、不可见字符、超长重复，命中发 `idp.host/guard.flagged`（每轮一次）；`block` 把命中内容替换为说明；线程里保存的工具消息不变；缺省 wrap。
- 输出过滤（D28）：租户策略的 `blockedPatterns`（迁移 0005）与发布件的 `patterns` 在流式输出中逐段检查，命中即取消模型调用、Run 以 `GUARD_BLOCKED` 失败并发 `idp.host/guard.blocked`；内置 email / phone / card（Luhn）脱敏作用于流式分段与最终消息；外部分类器在 HOST 配置的 `classifiers` 登记，`onError` 决定失败时放行或拦截；`strict` 先缓冲再放行。指标 `idp_host_guard_events_total`。
- 契约 1.4.0 发布（Guards、InputGuard、OutputGuard、Evaluation、EvalCase、Expectation；AgentRun 的 purpose）。

**验证**：单元（agent、guard、harness 共 13 个新用例）、契约（1.4.0：示例往返、字段一一对应、22 种新拒绝用例与 11 条交叉规则）、集成（真实 PostgreSQL：subagents、skill-resources-and-merge、guards、evaluation-gate 四个新场景，全部通过）、compose 端到端 MODE=oidc 18/18、idp-contracts 校验 148/148 用例、20/20 不变量，发布副本与生产者逐字节相同。

**局限（不标 PASS）**
- 输入护栏是规则，不是模型：改写措辞的注入不会被识别；定界与说明只是基线。
- 非 strict 的输出过滤在命中前已落库的增量不会撤回；脱敏按合并后的分段进行，跨分段的模式可能漏脱敏（strict 模式无此问题）。
- 评估运行用的是候选发布件的临时信封（无审批人），摘要与批准后的不同；评估结果绑定发布件 id。

## 13. M5 实施结果（2026-11-04，WSL 实测）

- 模型路由（D29）：`model.routing` 的 ordered / cost / latency / capability 四种策略；候选池 = 主模型 + 回退 + 候选（去重，候选元数据生效）；提供工具时只保留声明 `tools`（或未声明能力）的候选，估算输入超过 `maxContextTokens` 的候选被跳过；latency 用进程内 EWMA 与近 20 次错误率（失败视为慢），每次调用后观察；决定与原因记为 `idp.host/model.routed`；无 routing 时行为与 M1 相同。
- A2A（D30）：服务端 `GET /a2a/agents/{ref}/.well-known/agent-card.json`、`POST /a2a/agents/{ref}`（JSON-RPC 2.0：message/send 含 blocking 与 historyLength、message/stream 以 SSE 流出 JSON-RPC 响应、tasks/get、tasks/cancel；push 通知与 resubscribe 返回 UnsupportedOperation）；task = Run，contextId = 线程，状态映射 submitted / working / input-required / completed / failed / canceled，最终助手消息为 artifact，消息 metadata 里的 `idp.host/` 键作为 RunOptions（可钉发布件）；对等待中的任务再发消息返回 UnsupportedOperation 并指向确认接口。客户端：`subagents[].a2a.cardUrl` 指向远端 agent card，委派为阻塞的 message/send（超时 = 剩余运行时限，至多 10 分钟），结果直接作为工具结果（不建本地子 Run），凭据按 HOST 配置 `a2aPeers` 的 URL 前缀选取。集成测试以 HOST 自己为对端验证了全部方法与远端委派。
- 外部 harness（D31，实现上的调整）：没有给发布件加 `harness` 字段，而是把外部 harness worker 做成 provider 种类 `harness-worker`——发布件通过 `model.provider` 选择它，路由、回退、护栏、确认、工具执行、配额全部不变。协议：`POST /v1/turn`（指令、消息、工具、限制）→ SSE 事件 text / reasoning / tool_call / done / error。随仓库提供 `deploy/harness-worker/worker.py`（仅标准库，前接任意 OpenAI 兼容端点），其测试在 Go 里启动真实的 Python worker 对假模型完成一次工具调用与一次文本回合。
- 契约 1.5.0 发布（Routing、Candidate、A2ARef；EmbeddedSubagent 的 agentId / version / digest 对远端 agent 不再必填）。

**验证**：单元（model 路由器、harness 路由、harnessworker 协议与 Python 参考 worker）、契约（1.5.0：示例带路由与远端 agent，10 种新拒绝用例与交叉规则）、集成（新增 a2a 场景：card、send、阻塞 send、stream、input-required 与取消、错误码、metadata 选项、远端委派）全部通过。

**局限（不标 PASS）**
- 远端委派期间父 Run 的 Worker goroutine 阻塞在 HTTP 上（本地子 Run 不占 Worker）；远端任务进入 input-required 视为失败。
- A2A 只实现文本 part；没有 push 通知；agent card 需要承载方的 bearer（不是公开端点）。
- 路由器的 latency 统计只在本进程内，副本之间不共享。

## 14. D32：CMI 上下文联调（2026-11-04 决策与实施，WSL 实测）

idp-cmi（Context, Memory & Knowledge Infrastructure）M1 完成并发布契约 `idp.telos.cmi` 1.0.0 后，第 3 节与第 11 节留下的"与 CMI 的 Context API 联调"空位在 HOST 侧补上。

| # | 决策 | 理由 |
|---|---|---|
| D32 | **运行创建时从 CMI 取上下文**。AgentDefinition 增加 `context`（ContextPolicy）：`cmi = true` 时 HOST 在创建运行的请求里、入队之前，以调用方身份向配置文件 `cmi` 一节登记的 CMI 调 `POST /v1/context/resolve`——查询是最后一条用户消息（截至 4096 字符）、scope 是线程、`purpose` / `maxTokens` / `maxItems` / `kinds` / `sources` 来自策略；CMI 返回的密封 ContextPackage 的每个条目变成一条调用方 context（描述 = 种类、来源与其信任等级、观测时间、引用、标题；值 = 摘录），追加在调用方自己的 context 之后，由 harness 作为 developer 消息交给模型（并经 D27 的输入护栏）。包 id 记在 Run 的 `contextPackageId`，事件 `idp.host/context.resolved` 记包 id、摘要、条目数与 token 数。凭据与网关同款：`static`（一个 cmi-reader 凭据，开发用）或 `delegation`（RFC 8693 以调用方令牌换 CMI 受众的令牌）。CMI 不可用、拒绝或没有用户消息时按 `onError`：`skip`（缺省）照常运行并记 `idp.host/context.skipped`；`fail` 拒绝创建（502 `CONTEXT_UNAVAILABLE`）。幂等重放比较去掉 CMI 条目后的请求（CMI 条目每次都新取，不能让同一请求因此变成"另一个请求"）。 | 上下文由谁给、什么时候给是信任边界：由 HOST 在创建时以调用方身份取，CMI 的可见性（租户、ACL、敏感度上限）就是调用方自己的；模型看到的是不可变、可引用的包，审计能回答"模型当时看到了什么"（CMI ADR D5、D10）。放在创建时而不是每一步，是因为一次运行回答一个问题，中途再取会让同一运行的不同步看到不同事实。不用新的消息角色，而用现有的 context，是为了让护栏、EER 的 `ToolExecution = caller`、子 agent 继承（D22 继承调用方 context）全部不变。 |

**契约**：`idp.telos.host` 1.6.0——Definition 增 `context`（ContextPolicy：cmi、purpose、maxTokens、maxItems、kinds、sources、onError），AgentRun 增 `contextPackageId`；只做新增。

**实施**：`internal/cmiclient`（Resolve、ContextOf）、`internal/api` 的 `resolveContext`、配置 `cmi: {endpoint, caFile, auth}`、迁移 0007（`runs.context_package_id`）、`CreateRunInput.ContextPackageID / Extra`（RUN_STARTED 之后追加 resolved / skipped 事件）。

**验证**：单元与契约（1.6.0：示例发布件带 context 策略、示例运行带 contextPackageId，5 种新拒绝用例）、集成（新增 cmi-context 场景：假 CMI 收到 bearer、查询、线程 scope 与预算；模型看到两条 CMI developer 消息；运行记录 packageId 与 resolved 事件；同一请求重放同一运行；CMI 503 时 skip 并记 skipped 事件；onError = fail 时 502 CONTEXT_UNAVAILABLE；未配置 CMI 时 skip）全部通过。

**D32 补充（2026-11-04，CMI ADR-0001 D24 的 HOST 侧）**：ContextPolicy 增 `onStale`（continue 缺省 / refresh / fail）。Worker 在第二步及之后的每一步开始前以 HOST 自己的 cmi-reader 凭据调 CMI 的 `POST /v1/context/packages/{id}/verify`（每运行至多一次发现失效或无法回答）；包失效时记 `idp.host/context.stale`（含 staleItems、redacted、onStale），continue 照常，refresh 重新解析并替换运行里的 CMI 上下文（只在 static 凭据下可行，委派模式等同 continue），fail 以 `CONTEXT_STALE` 结束运行。契约 1.7.0 只加 `onStale`。集成测试：fail / continue / refresh 三种各一个两步运行。

**局限（不标 PASS）**
- 与真实 idp-cmi 进程的 compose 联调已在 idp-cmi 仓库完成（`HOST=1 bash deploy/e2e.sh`，idp-cmi ADR-0001 第 6 节，2026-11-04）：真实 HOST 运行的 `contextPackageId` 与事件摘要与 CMI 保存的包一致。委派模式对真实 Keycloak 未验证（static 模式已测）。
- 只在创建时取一次；后续用户在同一线程追问要再创建运行才会取新上下文。
- CMI 条目按 ContextPackage 的 excerpt 原样交给模型，HOST 不再按 sensitivity 过滤——这由 CMI 的 sensitivityCeiling 与策略负责。

## 15. D33：运行结束后写回 CMI 的情景记忆（2026-11-06 决策与实施）

D32 让运行"读"CMI；没有"写"，线程之外的后续运行就记不住这次对话。D33 补上写回：CMI 的上下文包里本来就有 `memory` 条目，HOST 只需在合适的时刻追加一条 episodic 记录（CMI ADR-0001 D3）。

| # | 决策 | 理由 |
|---|---|---|
| D33 | **COMPLETED 之后追加一条情景记忆**。配置 `cmi.remember`（可选，缺省关闭）：`enabled`、`source`（CMI 已登记的来源名，缺省 `idp-host`）、`ttlDays`（缺省 30，1..3650）、`sensitivity`（缺省 internal）、`maxChars`（缺省 1500，1..8000，用户消息与回答各自按 rune 截断）、`auth`（**写入者**身份，需要 CMI 角色 `cmi-writer`，形态同 `cmi.auth`：static 或 delegation，启用时必填；static 的密钥文件每次写入重新读取，轮换后的令牌即时生效）。条件：运行所属发布件 `context.cmi = true`、运行以 COMPLETED 结束、有非空的最终助手文本（最后一条助手消息且不带工具调用）与非空的最后一条用户消息、运行有所有者 subject；子 agent 运行与评估运行不写。记录：`scope {subject, <运行所有者的 sub>}`、`kind episodic`、`content "The user said: …\nThe assistant answered: …"`（不能写成"User: / Assistant:"：CMI 写入时的注入扫描把行首的 `assistant:` 判为 `role_impersonation`，记录会被隔离、不进上下文包；这是联调时发现的）`、标签 `conversation` 与 `agent-<名>`、`structured {runId, agent, threadId}`、`provenance {source, ref "run:<runId>", observedAt = 完成时间, recordedBy "idp-host"}`、`validity {validFrom, validUntil = 完成 + ttl}`、`sensitivity` 取自配置、status 省略（active）。`Idempotency-Key: host-run-<runId>`：重试与重放不会重复。写入在 `Store.Finish` 成功之后由独立 goroutine 完成（每次尝试 5 秒、最多 4 次、500 ms 起指数退避），不在运行的事务里，也不延后流的结束。5xx、429 与网络错误重试；其余 4xx（缺来源或角色的 403、422 等）不重试；200 重放与 409 视为成功。失败只记日志（运行号与结果，不含内容）并计入 `idp_host_memory_writes_total{outcome}`，**永远不改变运行**。 | 记忆的写入者是 HOST 这个有名有姓的、被登记的来源（可追溯、可按 `ref` 撤销、可被 CMI 的写入扫描与敏感度上限约束），而不是模型自己决定记什么：内容是不可信的用户文本与模型输出，由 CMI 在写入时扫描（D12），带注入标记的记录默认不进上下文包。写入者凭据与读取者凭据分开，读者不需要写权限，写者不需要读权限。放在终态之后是因为记忆是运行的副产品，不应成为运行成功的条件；幂等键以运行号为准，使租约丢失后的重放安全。 |

**取舍与边界**
- 这是**配置**，不是契约：运营方启用后，任何 `context.cmi = true` 的 agent 都会被记住。要让单个 agent 退出（或指定它自己的 TTL、敏感度），需要在 ContextPolicy 增字段，即改 `idp.telos.host` 契约，本决策不做。
- 摘要而非全文：只记最后一轮的问与答，各截到 `maxChars`；不记工具调用与中间步骤。对话内容会进入 CMI，保留期与可见性由 `ttlDays`、`sensitivity` 与 CMI 的策略决定；运营方应把它当作个人数据处理（CMI 的 subject 作用域只对该成员可见，可按 `ref run:<id>` 撤销与擦除）。
- 标签用 `agent-<名>` 而不是 `agent:<名>`：CMI 的标签语法是 `[a-z0-9][a-z0-9_.-]{0,63}`，不允许冒号，带冒号的标签会被 422 拒绝（对固定契约副本 1.4.0 的 MemoryRecord 校验已证实）。名字过长（超出 64 个字符）时不写这个标签，`structured.agent` 仍在。
- delegation 的写入者身份：Worker 里没有成员的原始令牌，只有创建时为网关换来的、封存在运行上的凭据。delegation 模式因此用该存储凭据作为 `subject_token` 换取写入者受众的令牌；运行没有网关凭据时（无网关工具）无法交换，计为 `no_credential` 而不写。生产上建议用 static 写入者（专用的 cmi-writer 服务账号）。
- 进程退出时仍在途的写入会丢失（计数不到）；下一次对同一运行的重放不会发生（运行已终态），所以这是有界的记忆缺口，不是一致性问题。

**实施**：`internal/config`（`cmi.remember` 校验与缺省）、`internal/cmiclient/remember.go`（Build、Cut、Write：重试、结果分类）、`internal/harness`（`complete` 之后的 `remember`、`exchange` 判定条件）、`internal/worker` 指标 `idp_host_memory_writes_total{outcome}`、`cmd/idp-host` 装配。

**验证**：单元——请求体对固定的 `idp.telos.cmi` 1.4.0 MemoryRecord 模式校验通过（并证明该模式会拒绝冒号标签、未知字段、空内容）；作用域、provenance、幂等键、validity、中文按 rune 截断；201/200/409 成功，5xx 重试 4 次后放弃，403/422/401 不重试，429 重试；static 密钥每次尝试重新读取；delegation 的 grant_type、subject_token、audience、actor_token；harness 层：失败、取消、无 `context.cmi`、关闭、无所有者、子 agent、评估、空回答或仅工具调用均不写，CMI 5xx/403 不改变 COMPLETED，写入不延后终态。配置校验：缺 auth、非法 ttl、非法 sensitivity、maxChars、source、未知键。

**局限（不标 PASS）**
- 未对真实 idp-cmi 进程与真实 Keycloak 做联调，也未扩展依赖 Docker/PostgreSQL 的集成场景；CMI 一侧的行为（角色、来源登记、标签语法）由其源码与固定契约副本核对。
- 没有为同一线程的回合做合并或摘要；每个完成的运行一条记录，合并由 CMI 的整理（consolidation）负责。

## 16. D34：只记用户说的事实（2026-11-09 决策与实施）

起因：CMI 的记忆评测（idp-cmi ADR §18）显示，D33 一轮一条"用户说了…助手答了…"在检索召回上没问题，问题是**过期值**：被更新过的属性有 93% 的提问会同时取回旧值；助手答错的话也会被当成记忆存下来。CMI 已提供事实写入 `POST /v1/memory/facts`（CMI D41：同一主体同一键只有一个现行值，新值关闭旧值的有效期）。

| # | 决策 | 理由 |
|---|---|---|
| D34 | **`cmi.remember.mode`：`turns`（默认，D33 不变）\| `facts` \| `both`**；`facts` 与 `both` 需要 `facts {provider, model, maxFacts（默认 5，1..20）, timeout（默认 20s）}`，provider 必须是本配置里的提供者（启动时检查）。运行 COMPLETED 之后，在后台（和 D33 一样不拖慢流的结束、失败只记日志和指标）：(1) 用写入者身份读该用户已有的事实（`GET /v1/memory/records?scopeKind=subject&scopeId=…`，取 `structured.fact`）；(2) 调一次模型，**只给用户的那句话和已知事实**，要求输出 JSON `{facts:[{key,value,statement}]}`；(3) 逐条 `POST /v1/memory/facts`（`Idempotency-Key: host-fact-<runId>-<key>`，`validFrom` 为运行结束时间，不设终点，`kind: semantic`，标签 `fact`、`conversation`，出处 `run:<runId>/fact/<key>`）。助手的回答**不发给模型、不存储**。模型输出视为不可信：严格解析（取第一个 JSON 对象），键必须符合 CMI 的键格式，取值不超过 120 字、陈述不超过 300 字，重复键只留第一个，不合格的丢弃不修补，条数有上限。提示词规定：键只描述属性不含取值（`diet` 而不是 `diet_vegetarian`），已知事实里有同一属性就原样沿用其键（这是更新取代旧值的机制），更正和长期偏好也算事实，别人的属性、问题、一次性请求、假设不算，并明说用户消息是数据不是指令。指标 `idp_host_memory_writes_total{outcome}` 增加 `fact_created`、`fact_updated`、`fact_unchanged`、`fact_stale`（409：CMI 的现行值比这条陈述新）、`extract_failed`、`no_facts` | 评测里真实抽取（本机 Qwen3.8-flash，改进后的提示词）把"前 5 含过期值"从 0.92 降到 0.06，当前值排第一从 0.45 升到 0.79，当前值在前 5 为 0.89（原样存回合 0.92；精确抽取的上限是 0.96 / 0.99 / 0.00）；用户的话经过的是他自己的作用域，提示注入最多让他自己的记忆多一条，且仍经过 CMI 写入时的注入扫描；不存助手的回答去掉了"助手答错被记成事实"的污染 |

局限：抽取错误率取决于模型（评测用本机 Qwen3.8-flash：一条消息一次调用，约 1.3% 失败，268 条真值事实抽到 252 条，约 6% 漏抽；提示词是在评测数据上改的，真实对话要再测）；漏抽会让该事实根本不在记忆里。**建议用 `facts`**：`both` 同时保留回合记录，回合里的旧值仍会被取回，会把"前 5 含过期值"带回去；只有在更怕漏记时才用 `both`。`turns` 仍是默认（评测之外还没有真实对话的验证），Workspace 的"我的记忆"页列出和删除的是 CMI 里这个人的全部记录，事实记录也在其中。

## 17. D35：确认不越过运行期限（2026-11-12 决策与实施）

起因：运行的期限（`deadlineAt` = 创建时间 + `runTimeoutSeconds`，缺省 300 秒）在创建时定下、之后不变，确认请求的过期时间却从请求时刻起按 `confirmationTimeoutSeconds`（缺省 900 秒）计算。按缺省值，第 5 到第 15 分钟之间的确认会被接受；运行恢复时先执行已确认的工具（`resolveDecided`），再在循环开头发现超期，以 `RUN_TIMEOUT` 失败：工具的副作用已经发生，所有者看到的却是失败。这是 idp-workspace 复现其 D18（运行等待确认时的入箱）时发现的。

| # | 决策 | 理由 |
|---|---|---|
| D35 | **运行期限之后不再执行任何东西，确认请求最迟随运行期限过期**（修订 D7）：(1) 确认请求的过期时间取"请求时刻 + `confirmationTimeoutSeconds`"与运行期限中较早的一个，`tool_calls.confirmation_expires_at`、`CUSTOM idp.host/confirmation.required` 与 `RUN_FINISHED` 中断里的 `expiresAt` 都是这个值；(2) Worker 在期限之后认领运行时，直接以 `RUN_TIMEOUT` 失败：不执行已确认的调用（它保持 CONFIRMED、没有结果），也不调网关和模型；(3) 期限之后的确认或拒绝得到 409 `the run exceeded its time limit`，不记录任何东西，请求由过期清理（每 5 秒）收尾，运行随即以 `RUN_TIMEOUT` 结束。确认时限短于运行剩余时间时行为不变：请求过期后运行继续，模型收到"确认请求已过期" | 期限是运行的边界：过了期限运行只能失败，这时再执行工具只留下副作用，而需要确认的工具往往正是有副作用的那些。过期时间封顶后，中断给出的就是真正有效的截止时间（Workspace 收件箱据此显示过期时间），超期的运行在期限后几秒内结束，不再挂到确认超时。不改契约：字段与形状不变，只是取值不晚于期限 |

**取舍与边界**
- 按缺省值（运行 300 秒、确认 900 秒），确认窗口就是运行 5 分钟里剩下的时间；需要更长等待的发布件应调大 `runTimeoutSeconds`。另一种做法是等待确认的时间不计入运行时限（恢复时顺延期限），那会改变 `runTimeoutSeconds` 与 `deadlineAt` 的含义，本决策不做。
- 所有者在期限前确认、Worker 在期限后才认领（积压或 Worker 重启）：调用同样不执行，运行以 `RUN_TIMEOUT` 失败。期限是硬的，不为已确认的调用留宽限。
- 本决策之前创建、过期时间晚于期限的请求：期限之后的决定同样得到 409、不会执行；运行要到请求自身过期时才由清理收尾。

**实施**：`internal/run`（`Overdue`）、`internal/harness`（`dispatch` 把过期时间封顶到期限；`Run` 认领后先查期限；循环里的期限判断改用 `Overdue`）、`adapters/store/postgres`（`DecideToolCall` 在期限之后按过期处理）。

**验证**：单元（`make test`，-race）新增两项：缺省确认时限下过期时间等于运行期限、中断的 `expiresAt` 相同，确认时限短于剩余时间时保留自己的过期时间；期限前已确认、期限后才认领的运行以 `RUN_TIMEOUT` 失败，工具没有被调用，调用保持 CONFIRMED。集成（真实 PostgreSQL 17，`make test-integration`，21/21）新增 confirmation-and-deadline：工具调用的 `confirmationExpiresAt` 等于运行的 `deadlineAt`，中断的 `expiresAt` 相同；期限之后确认得到 409 `the run exceeded its time limit`，调用仍待确认；清理过期请求、运行以 `RUN_TIMEOUT` 失败，echo 没有被调用；期限前确认、期限后认领同样失败且不调用。把三个生产文件恢复为修改前的版本，这三项测试都失败：过期时间是请求时刻加 900 秒；期限后才认领的运行先调用了 echo（结果 hi）再以 `RUN_TIMEOUT` 失败。

**局限（不标 PASS）**：没有在 compose 或 kind 的整平台上重放 idp-workspace 的两个复现场景。

## 18. D36：HOST 自己签发并续签对下游的凭据（2026-11-12 决策与实施）

起因（平台实测发现）：kind 上的平台部署满一小时后，助手就不再记住用户说过的话——界面一切正常（模型照常回答"好的，我记住了"），只有 HOST 的日志里一行 `memory not written … cmi answered HTTP 401`。原因是 `cmi.remember.auth` 只有 `delegation`（换调用者的令牌，但成员没有 `cmi-writer`）和 `static`（从文件读一个固定值）两种模式，于是部署脚本只能**在部署时取一个访问令牌塞进 Secret**；Keycloak 给它一小时寿命，过期后记忆静默失效。平台验收在部署后几分钟内跑完，永远在有效期内，所以从未暴露。

| # | 决策 | 理由 |
|---|---|---|
| D36 | **`auth.mode` 增加 `client_credentials`**：HOST 用自己的客户端身份（`tokenEndpoint`、`clientId`、`clientSecret[File]`、`audience`）向授权服务器取面向下游受众的令牌，首次使用时签发，并在过期前（缺省 30 秒）自动换新；`credential.Service` 持有它，并发安全，签发失败时保留手上的令牌、下次再试。`cmi.auth` 与 `cmi.remember.auth` 都接受这种模式 | 服务自己的身份就该由服务自己去取：配置一次（一个客户端密钥）之后不再过期。机制本就存在——调用 idp-mcp 网关时作为 `actor_token` 的那一份就是这么签发和缓存的，只是 CMI 这条路没接上。`ClientCredentials` 同时带上 `audience`（Keycloak 由客户端映射器决定受众并忽略该参数，其他授权服务器需要它） |

**实施**：`internal/credential`（`Service`，以及 `ClientCredentials` 带上 `audience`）、`internal/config`（两处校验与错误文案）、`cmd/idp-host`（`serviceCredential`，写入路径接 `remember.Key`，读取路径接新增的 `CMIToken`）、`internal/api` 与 `internal/harness`（`CMIToken` 优先于 `CMIKey`，取不到令牌时按"没有凭据"处理而不是用一个空值去调）。

**验证**：单元（`internal/credential`：有效期内只签发一次、临近过期自动换新、寿命 1 秒的令牌连续三次调用都拿到可用的、20 个并发调用只签发一次、被拒绝后下次仍能恢复、没有配置时报错；`internal/config`：读写两处的合法写法与四种缺字段的拒绝）；整平台（idp-telos-go `deploy/compose/e2e.sh`，PATH A–I 全过，其中 PATH E 写入记忆、PATH G 抽取事实都走这条新凭据）。

**局限（不标 PASS）**：没有在验收里跑满一小时去证明"过期后仍然工作"——那一点由单元测试（1 秒寿命的令牌）保证，验收只保证新路径本身可用；另外读取路径在 kind 上仍用 `delegation`（有意：按成员身份读，取回的才是他自己的记忆），`client_credentials` 的读取只在单元测试覆盖。
