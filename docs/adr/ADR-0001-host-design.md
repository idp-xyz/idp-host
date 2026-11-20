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
| D3 | **AgentRun 是持久化状态机，每个 Run 一条追加式事件日志**：`queued → running → (awaiting_input ⇄ running)* → completed | failed | canceled`；每次迁移一个数据库事务；事件按 Run 内 `seq` 单调编号，游标可续。Worker 以租约 + 心跳 + fencing epoch 认领（`SELECT … FOR UPDATE SKIP LOCKED`），租约过期可被重领，旧 Worker 的提交被拒（D46 修订：运行的期限与取消到达进行中的调用，不再只在步与步之间检查）。模型调用不是事务，重领后可能重复的是模型调用（多花一次费用）和只读或幂等的工具调用；其他工具调用至多执行一次：执行中被中断时，HOST 分不清"已经做了但结果丢了"和"没发出去"，所以不再发出，把"被中断、结果未知"作为工具错误交给模型，并记 `idp.host/tool.interrupted`（D47 修订；只读与幂等按网关目录里的标注判断，运维可用 `neverResendTools` 收紧）。助手消息与它的工具调用记录在同一个事务里写入；工具调用先落库再执行，执行结果落库前该步不算完成；worker 收到停止信号后给进行中的调用一段宽限期做完（D47） | 可续跑、可审计、多副本安全；这是 PoC 验证过的骨架 |
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
| D35 | **运行期限之后不再执行任何东西，确认请求最迟随运行期限过期**（修订 D7）：(1) 确认请求的过期时间取"请求时刻 + `confirmationTimeoutSeconds`"与运行期限中较早的一个，`tool_calls.confirmation_expires_at`、`CUSTOM idp.host/confirmation.required` 与 `RUN_FINISHED` 中断里的 `expiresAt` 都是这个值；(2) Worker 在期限之后认领运行时，直接以 `RUN_TIMEOUT` 失败：不执行已确认的调用（它保持 CONFIRMED、没有结果），也不调网关和模型；(3) 期限之后的确认或拒绝得到 409 `the run exceeded its time limit`，不记录任何东西，请求由过期清理（每 5 秒）收尾，运行随即以 `RUN_TIMEOUT` 结束。确认时限短于运行剩余时间时行为不变：请求过期后运行继续，模型收到"确认请求已过期" | 期限是运行的边界：过了期限运行只能失败，这时再执行工具只留下副作用，而需要确认的工具往往正是有副作用的那些。过期时间封顶后，中断给出的就是真正有效的截止时间（Workspace 收件箱据此显示过期时间），超期的运行在期限后几秒内结束，不再挂到确认超时。不改契约：字段与形状不变，只是取值不晚于期限。**D46 修订**：期限也截断进行中的模型与工具调用，运行在期限后约一秒内以 `RUN_TIMEOUT` 结束，不必等调用自己返回 |

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

## 19. D37：成员文件经票据进出运行（2026-11-13 决策与实施，平台 ADR-0002 阶段 1）

起因：office 档位（平台 ADR-0002 阶段 0）让 Agent 能在沙箱里做 Word / Excel / PowerPoint，但成员既交不出自己的文件，也拿不到做好的文件。字节不能经过模型（会进上下文、调用记录和网关审计，25 MB 的文件也放不进去），凭据也不能进模型的输入。

| # | 决策 | 理由 |
|---|---|---|
| D37 | **RunOptions 增 `idp.host/attachments` 与 `idp.host/delivery`（契约 1.8.0）**：调用方（idp-workspace）为每个附件给出 `{ref, name, mediaType, size, sha256, ticket}`，再给一张写票据 `{ticket}`；票据由文件的所有者系统签发，HOST 不解读。HOST 把票据从 forwardedProps 中取出，加密存入 `runs.file_tickets`（与网关凭据同一把 `IDP_HOST_CREDENTIAL_KEY`，运行结束、取消时与凭据一同清除），运行的 input 只留附件的描述，幂等摘要也只含描述（重签的票据重放同一个请求）。模型只被告知 `attachment:N  "名称"  类型  大小`，以及可以用 `output` 交付。网关工具的输入 Schema 用 `format` 声明哪个顶层属性接收文件：`idp-attachment` 收 `attachment:N`，`idp-delivery` 收 `output`；HOST 在允许列表与确认之后、发往网关之前把它们换成票据，别的工具、别的属性一律不换。记录的工具调用、确认页、事件与对话里都是模型写的引用。交付调用成功且结果带 `structuredContent.deliveredFile` 时追加事件 `idp.host/file.delivered {toolCallId, ref, name, mediaType, size, sha256}`。附件与交付要求 HOST 执行工具（`toolExecution: caller` 时 422），没有凭据密钥时 409；子 agent 运行拿不到票据 | 票据自带授权、可验证、短时、范围最小，替换放在确认之后，模型、外部模型、事件日志、网关调用记录都碰不到它（平台 ADR-0002 F3）。用 Schema 的 `format` 声明而不是按工具名硬编码：HOST 不认识 idp-sandbox，而只有经网关四眼登记、目录被钉住的工具才可能声明它；没有声明的工具即使参数里写着 `output` 也拿不到票据 |

**实施**：`internal/run`（`Attachment`、`Delivery`、`FileTickets`、`WithoutFileOptions`，选项严格解析：至多 10 个、ref 互不相同、名称一行可打印且不含路径分隔符、票据为三段 base64url 且至多 8192 字符、多余字段拒绝）；`internal/api`（`createRun` 的拒绝与去除）；`adapters/store/postgres`（迁移 0008 的 `file_tickets`，`FileTickets` 解密，终态与取消时清除）；`internal/harness`（`fileProperties`、`fileArguments`、`filesNote`、`delivered`）；契约 1.8.0（`FileAttachment`、`FileDelivery`；`attachments` 的条目在选项里额外要求 `ticket`）。

**验证**：单元（`internal/run`：选项的接受与 16 种拒绝、去除后的 forwardedProps 不含票据；契约：示例往返、8 种新拒绝用例 schema 与 API 一致；`internal/harness`：完整的创建→导入→运行→交付流程中网关只在声明的属性里收到票据，模型的输入、事件、调用记录与对话里都没有票据，`file.delivered` 带上 ref；越界的 `attachment:2`、伪造的票据串、`attachment:0`、非字符串、未许可的交付、`output` 以外的目标都在发出前拒绝；没有声明的工具里写 `output`、`attachment:1` 原样发出）；集成（`TestHostEndToEnd/file-tickets`，真实 PostgreSQL：存下的 input 不含票据、`file_tickets` 是密文，重签票据的重放返回同一个运行，沙箱收到第一次请求的票据，事件与工具调用记录不含票据，运行结束后 `file_tickets` 为 NULL，`caller` 加交付被拒）。

**局限**：票据在有效期内、在 HOST 的进程内存里仍然是可用的凭据；运行若长于票据的有效期（由签发方定），之后的导入导出会被拒绝。敏感级别与外部模型的上限（平台 ADR-0002 F8）不在本决策内。

## 20. D38：运行的委派凭据在用户会话内续期（2026-11-13 决策与实施）

起因（真实模型试用发现）：bob 在工作台让 office-assistant 做一张 Excel，运行以 STEP_LIMIT 结束。运行的网关凭据是在创建时由 bob 的令牌换来的（工作台换成 HOST 受众，HOST 再换成网关受众），Keycloak 换出的令牌最多活到原令牌剩下的时间：bob 登录 7 分钟后发的请求，凭据只剩 2 分 53 秒。本地模型一步要想几十秒到一分半，第 8 次调用起网关一律回 401，HOST 把它当普通工具错误交给模型，模型又重试了 22 次，直到 30 步用完。

| # | 决策 | 理由 |
|---|---|---|
| D38 | **委派凭据绑定用户会话并在运行中续期**。网关的 `auth` 增加 `sessionBound`（只用于 delegation）：令牌交换请求 `requested_token_type=refresh_token`，HOST 拿到网关凭据的同时拿到一个绑定在用户会话上的 refresh token，与凭据一起加密存入 `runs.credential_refresh`（迁移 0009），运行结束或取消时与凭据一同清除，子 agent 运行沿用父运行的。Worker 在每一轮开始与每次网关调用前取用凭据：离过期不到 1 分钟就先用 refresh token 续期并存回。网关仍然以 401 拒绝时，续期一次、重发一次（401 发生在鉴权，工具没有执行，重发安全）；续期被拒（`invalid_grant`：用户已退出、会话闲置超时或到达最长时限、被撤销）或续期后仍被拒，运行立即以 `CREDENTIAL_EXPIRED` 结束，原因写进这次工具调用的结果与运行的失败消息，**不再把凭据错误交给模型**。授权服务器暂时不答而现有凭据还有效时，照用现有的。没有 `sessionBound` 的网关行为不变（过期即结束，原因同样明确）。调用方（idp-workspace D25）的交换也要绑定会话：Keycloak 只允许对"在用户会话里有客户端会话"的令牌再交换出 refresh token | 运行代表用户行事，能代表多久应该由用户的会话决定，而不是由用户发请求那一刻手里令牌还剩几分钟决定。refresh token 留在 HOST 内、加密、随运行清除，不交给网关与模型；用户退出登录，运行在下一次网关调用时就停下，权限随会话一起收回。凭据错误交给模型只会被当成工具故障反复重试 |

**Keycloak 设置（已实测 26.8.1）**：客户端 `idp-host` 与 `idp-workspace` 的 `standard.token.exchange.enableRefreshRequestedTokenType = SAME_SESSION`（管理台：Allow refresh token in Standard Token Exchange = Same session）。实测：直接交换用户登录令牌可得 refresh token；交换"已经是交换所得"的令牌，只有前一次交换也请求了 refresh token（从而在会话里留下客户端会话）才行，否则 `creating a new session is needed`。续期得到的令牌用户、受众、会话不变，有效期是客户端的完整寿命；用户登出后续期得到 `invalid_grant / Session not active`。会话闲置时限默认 30 分钟、最长 10 小时：运行中的续期本身就是活动。

**实施**：`internal/credential`（`Exchanger.SessionBound`、`Refresh`、`ErrSessionEnded`、`Token.RefreshToken`）；`internal/mcpclient`（`RPCError.HTTPStatus`、`Unauthorized`：网关以 401 带 JSON-RPC 错误体拒绝凭据）；`internal/config`（`sessionBound`，static 模式拒绝）；`adapters/store/postgres`（迁移 0009、`RunCredential`、`SaveCredential`，创建、子运行、终态与取消时的封存与清除）；`internal/harness`（`gatewayBearer`、`credentialFailure`；列工具与调用工具时的续期与一次重发）；`cmd/idp-host`（`gateways[].Refresh`）。

**验证**：单元（`internal/credential`：会话绑定的交换请求并保存 refresh token，续期、服务端不轮换时沿用旧的、被拒的 refresh token 报告会话结束、服务器不可达不算会话结束；`internal/mcpclient`：带 JSON-RPC 体的 401 被识别；`internal/harness`：快过期的凭据在网关看到之前续期并保存，网关拒绝后续期并重发，会话结束时运行在下一次网关调用停下、模型只被调用一次、工具调用以原因作答，无法续期时照旧结束）；集成（`TestHostEndToEnd/session-bound-delegation`，真实 PostgreSQL：refresh token 加密随运行存下，网关只认续期后的凭据而运行完成，运行结束后清除）；平台（PATH I：等待确认中的真实运行持有 refresh token；PATH K：对平台的 Keycloak 走一遍工作台与 HOST 的会话绑定交换、续期、登出后被拒）。

**局限**：会话最长时限（默认 10 小时）是硬上限；工作台的运行跟随者（D18）在确认等待之后重连 HOST 时用的仍是创建时的令牌，长时间等待后可能读不到事件（另行处理）。

## 21. D39：网关拒绝参数的原因交给模型（2026-11-13）

起因（真实模型试用）：office-assistant 两次调用 `sandbox_run` 漏了必填的 `language`，网关按钉住的 Schema 拒绝并在 `data.reason` 里写明"missing property 'language'"，HOST 却只把"Invalid arguments for tool …"交给模型，模型猜不出错在哪，白白用掉两步。

| # | 决策 | 理由 |
|---|---|---|
| D39 | 网关以 -32602 拒绝参数时，HOST 把 `data.reason`（至多 600 字节）接在错误后面交给模型。本运行的文件票据在交给模型的任何工具结果与错误里都替换成 `[file ticket]` | 原因说的是模型自己写的参数，模型据此一步就能改对；拒绝信息可能引用参数值，而参数里可能已换成票据（D37），所以一律替换 |

**验证**：`internal/harness` 的 `TestRefusedArgumentsReachTheModelWithTheReason`、`TestTicketsNeverReachTheModelThroughAnError`（fakemcp 新增按网关格式回 -32602 与 `data.reason` 的 `Refuse`）。

## 22. D40：模型读到的工具结果有上限，装不下时说明原因（2026-11-13 发现，2026-11-17 决策与实施）

起因（真实模型试用）：bob 上传一份 282 KB 的报价表（xlsx）让 office-assistant 处理，两次都没有结果。模型的上下文窗口是 131,072 token，HOST 每一步都把会话里所有工具结果原样交给模型，单个结果的上限是 64 KiB。

- **在已经跑过三次运行的会话里**：第 1 步的输入就有 89,323 token，第 15 步 129,919，第 16 步模型服务回 400。运行以 `MODEL_ERROR: model call failed: status_400` 结束；bob 在同一会话重试三次，每次都在第 1 步得到同样的 400，看不出原因，也看不出该换会话。
- **换了新会话**：从 2,073 token 开始，几次打印表格内容的结果各有 15–35 KB，第 28 步输入 129,992 token，回答写了 1,072 token 就被截断（`finish_reason=length`），运行却记为 `COMPLETED`，没有交付文件；随后三次追问都是同样的"截断但完成"。

两个会话里的工具结果分别有 196 KB 和 213 KB，录得的密度是 0.27–0.85 token/字节（数字密集的表格最高，中位 0.45）。

| # | 决策 | 理由 |
|---|---|---|
| D40 | **模型读到的工具结果有上限；装不下时，运行说明原因。** ① 单个网关工具结果交给模型的上限从 64 KiB 降到 16 KiB，在字符边界截断，并写明共多少字节、显示了多少、请缩小输出。② 每一步交给模型的会话里，最近 6 个工具结果保持原样；更早且超过 1,200 字节的只给头 2/3 与尾 1/3，中间写明省略了多少字节、需要时缩小范围重跑。skill 的参考文档（`resource__read`）与出错的结果不缩短。存下来的会话与事件不变，变的只是模型这一步读到的内容。③ 回答因长度被截断且没有工具调用时，运行以 `MODEL_ERROR` 结束，消息写明回答被截断、当时的输入 token 数与步数，不再记为完成。④ OpenAI 兼容的模型服务以 400 拒绝、正文表明请求超出上下文窗口时，这一步的错误码是 `context_length`，运行的失败消息写明会话已装不下模型的上下文窗口、请开新会话或一次少做一些；其余 400 仍是 `status_400` | 旧的工具结果模型已经读过并据此行动，需要时可以再取；整份留着只会把后面的步骤挤掉。参考文档是 skill 要模型只读一次的，缩短了它只能再读一遍。被截断的回答不是回答：记为完成，用户看到的是半句话和一件没做完的事。超出窗口不是模型服务的故障而是这个会话的状态，重试没有用，用户要知道该怎么办。正文只用来分类，不记录也不交给任何人，D9 的"只有错误码"不变 |

**按录下的两个会话回放**（真实代码，量的是 `guardedMessages` 实际交给模型的内容；token 数按录下的每步增长折算，是估计值；结果被缩短后模型的做法也会不同，这里只比大小）：

| 会话 | 时刻 | 工具结果（字节） | 输入 token（录得 → D40 之后的估计） |
|---|---|---|---|
| 新会话 | 第 28 步，回答被截断 | 213,299 → 84,686 | 129,992 → 约 46,000（窗口的 35%） |
| 已有三次运行的会话 | 第 4 次运行第 16 步，400 | 196,394 → 127,536 | 约 131,500 → 约 106,000（81%） |

**取舍与边界**：

- 上限按字节算，不按 token：HOST 不知道各个模型的分词。最坏情况是最近 6 个结果都是 16 KiB 的数字表格，约 8 万 token；窗口比这小的模型仍会超出，由 ③④ 说明原因。
- 每当一个结果滑出"最近 6 个"被缩短，它之后的内容在模型服务的前缀缓存里就失效，那一步要重新处理最近几个结果。在参考主机的模型服务上，一步在开始写回答之前的耗时从约 2.5 秒变成约 4.5 秒（两次真实运行第 8 步之后各步的耗时对回答 token 数的回归；生成速度不变，约 125 token/秒）。
- 16 KiB、6 个、1,200 字节是 `Harness` 的默认值，没有配置项。
- 被截断但带有工具调用的回答不在 ③ 之内，行为不变。
- ④ 只在 OpenAI 兼容适配器里实现，认的是 OpenAI、vLLM、llama.cpp 与参考主机上 Strata 的说法；Anthropic 与 Gemini 适配器仍报 `status_400`。说法不在其列的服务也仍报 `status_400`，`tests/live` 的用例会把它查出来。

**实施**：`internal/harness`（`Harness.KeepToolResults`、`OldToolResultBytes`；`oldToolResults`、`shortenOld`，`guardedMessages` 在输入防护之前缩短；`loop` 里 ③④ 的两条失败消息）；`internal/mcpclient`（`CallResult.Text` 在字符边界截断并写明字节数）；`internal/model`（`CodeContextLength`、`ClassifyBody`）；`adapters/providers/openai`（非 200 时读至多 64 KiB 正文用于分类）；`tests/fixtures/fakemodel`（`Turn.CutOff`、`Turn.ErrorCode`）；`tests/live`（`TestLiveRequestTooLongIsToldApart`）。

**验证**：单元（`internal/harness`：`TestOldToolResultsAreShortenedForTheModel`，旧的长结果只剩头尾，参考文档、最近的结果与短结果不变，存下来的会话不变；`TestCutOffAnswerIsNotACompletedRun`；`TestRequestTooLongForTheModelSaysSo`，`context_length_exceeded` 的 400 得到说明，别的 400 仍是 `status_400`；`internal/model`：`TestClassifyBody`；`internal/mcpclient`：`TestResultText`）。去掉 ②③④ 中任何一处实现，对应的测试失败。`make test`、`make test-integration`（真实 PostgreSQL）通过。回放见上表。

**真实模型**（2026-11-17，compose 平台，只换了 `idp-host` 的镜像；模型服务 Strata 0.1.27，`qwen3.8-flash-next-iq3_xxs`，窗口 131,072）：

- bob 用同一份报价表、同一句话在新会话里重跑：50 步里输入最高 72,702 token（窗口的 55%），第 22 步之后随着旧结果被缩短有升有降，没有 400，也没有被截断的回答。**这次运行仍然没有交付文件**：它以 `STEP_LIMIT` 结束，模型 50 步都在写解析这份表的脚本（见局限）。
- 那个此前每次都在第 1 步得到 400 的会话：第 1 步输入 113,148 token（86%），正常作答。上表的估计（约 106,000）偏低约 6%。
- 模型服务对装不下的请求的回答是 `prompt (N tokens) + max tokens (M) exceeds the context (131072); requests are never truncated`，不是事先按 llama.cpp 的格式写的那几种说法；补上之后 `TestLiveRequestTooLongIsToldApart`（真实适配器，2 MiB 的请求）通过，去掉则失败。OpenAI、vLLM、llama.cpp 三种说法是按各自公开的错误格式写的，没有对着真实服务核对过。
- 没有做的：让一次平台上的运行真的超出窗口并看到 ④ 的失败消息。这条路径分两半验证过（真实服务到适配器的错误码，错误码到运行的失败消息）。

**局限**：

- 长会话仍然会增长。模型自己写的工具调用参数（脚本）不缩短，在那个已有三次运行的会话里有 140 KB；每个旧结果也还留约 1.3 KB。那个会话现在占窗口的 86%，再跑一次就会超出，届时用户得到 ④ 的说明。把旧的轮次压缩成摘要是另一个决定。
- 这份报价表的任务在步数上限里没有做完。上限提到 120 步、45 分钟后再跑一次（新会话，2026-11-17 04:38–05:12 UTC）：120 步、约 34 分钟，输入最高 119,448 token（窗口的 91%），6 个工具结果被截断，没有 400，也没有交付文件。117 次 `sandbox_run` 花在抽出表格、反复打印和重写解析上，`sandbox_export_file` 一次都没有。步数不是缺口。指引已改成先交付再看，写进当前的 officecli skill 与 office-assistant 发布件；这次运行用的还是改之前的那一版。
- 用改过的指引又跑了一次（新会话，同一份表、同一句话，2026-11-17 05:25–05:55 UTC）：仍然用满 120 步，约 29 分钟，输入最高 116,863 token（窗口的 89%），以 `STEP_LIMIT` 结束，`sandbox_export_file` 一次都没有。不同的是文件做出来了：第 94 步建出工作簿，第 114 步 `officecli validate` 通过，剩下的 6 步用在核对和调格式上。模型不知道自己还剩几步：harness 只用步数上限来终止运行，没有把上限或已用的步数告诉模型，skill 里也只说步数有限。用户在同一会话里追问一句之后，下一次运行用 13 步、114 秒交付了文件（245,916 字节，10 个工作表），其中前 11 步还在校验和调整；这时会话占窗口的 93%。交付的文件对照源表核对过：2,872 行运价的每个价格都与源表一致，769 行附加费都能在源表对应的工作表里找到；重量档 `1000+` 有两处写成了 `1000`，渠道目录的物流商一列 17 行里有 11 行是空的或填成了分组标题。指引让模型把文件做了出来，没有让它在一次运行里交付。
- 工作台的跟随者原来 10 分钟就放弃（idp-workspace D11）。这次运行的收件箱是"failed / STEP_LIMIT"，不再是"result unavailable"。跟随改到运行自己的 `deadlineAt` 之后（idp-workspace D26）。

## 23. D41：告诉模型还剩几步，最后两步只交付，步数用尽时交出所校验的文件（2026-11-17）

D40 局限里的报价表，在指引改成「先交付」之后又跑了一次：第 94 步做出工作簿，第 114 步 `officecli validate` 通过，剩下的步数用在核对和调格式上，120 步用尽，`sandbox_export_file` 一次都没有。文件还在工作区里。模型不知道自己走到了第几步；到上限时 harness 只结束运行，不看工作区。同一会话里追问一句，下一次运行才把文件交出去，而且前 11 步仍在改格式。

| # | 决策 | 理由 |
|---|---|---|
| D41 | **预算、收尾、兜底。** ① 每一步交给模型的内容里加一条不落库的说明：这是第 N 步、共 M 步、这一步之后还剩几步、离运行期限还剩多久。② M≥3 且这一步是最后两步之一，并且这个 agent 有声明了 `idp-delivery` 的工具时，这一步只把这些交付工具交给模型；别的调用记为拒绝，原因是运行正在收尾、只剩交付。短于 3 步的运行不留收尾段，否则没有步数做正事。③ 下一步会超过步数上限时，在记 `STEP_LIMIT` 之前，若这次请求允许交付、对话里能找到沙箱工作区、并且列文件、运行、交付三个工具都在：列出文件，跳过这次导入成功的和已经交付成功的，对其余 `.docx` / `.xlsx` / `.pptx` 用 `language: office` 跑 `officecli validate`，通过的以 `target: output` 交付。至多检查 8 个、交付 5 个。记录里仍是 `output`，票据只在发往网关时换上（D37）。校验没通过、没有工作区、office 语言跑不了，就什么都不交，失败消息与原来相同。交出去了，失败码仍是 `STEP_LIMIT`，消息写明交了几个已校验的文件；`idp.host/file.delivered` 照常记，收件箱因此既有失败原因也有文件。兜底不等待确认：运行已经不能再走一步，交付的授权是这次请求上的写票据。期限已过则不做。 | 模型看不见上限，就会把最后几步花在打磨上。只靠提示不够：追问时它仍先改了 11 步才导出，所以最后两步拿掉别的工具。即便如此它仍可能不调用导出；文件已经能校验、人在等，就由 harness 交出去。不把运行改记为完成：工作没在上限内收束，文件是兜底交的。沙箱看不到步数，也不能自己把文件推给成员（D37 的票据在 HOST） |

**实施**：`internal/harness`（`budgetNote`、`modelTools`、`wrappingUp`，`guardedMessages` 把说明加在这一步会话的末尾、不写入线程，这样前面的对话仍是稳定前缀；`dispatch` 在收尾步拒绝非交付调用；`loop` 在 `ErrStepLimit` 时 `salvage`，工作区取最后一次成功的沙箱调用，被拒绝的调用不算）。office 镜像里看表结构的命令是 idp-sandbox D16，skill 文案在 `deploy/compose/e2e.sh`。

**验证**：`TestBudgetIsToldAndNotStored`（说明出现在交给模型的内容里，不写入会话）；`TestLastStepsOfferOnlyDelivery`（4 步里的后两步只给出交付工具，再调用运行被拒绝，没有工作区时不兜底）；`TestStepLimitDeliversTheValidatedFile`（导入的、空的、非 Office 的、校验失败的都不交，校验通过的按写票据交出去，记录里仍是 `output`）；`TestStepLimitSkipsAFileAlreadyDelivered`；`TestStepLimitWithoutDeliveryDoesNotTouchTheWorkspace`。去掉兜底或收尾，对应的测试失败。

**真实模型**（2026-11-17，compose 平台）：同一份报价表、同一句话在新会话里用 84 步、约 27 分钟完成，第 78 步模型自己交付了工作簿（185,707 字节），输入最高占窗口的 71%，没有走到上限。兜底在真实网关与沙箱上的触发见 D42。

## 24. D42：步数用尽时也交出 PDF 与 Markdown（2026-11-17）

office 档位能读写 PDF 了（idp-sandbox D17），成员也会要 Markdown。D41 的兜底只认 Word、Excel、PowerPoint。

| # | 决策 | 理由 |
|---|---|---|
| D42 | **兜底交付扩到 PDF 与 Markdown。** 工作区里这次会话没有导入过、也没有交付过的文件：`.docx` / `.xlsx` / `.pptx` 仍先过 `officecli validate`；`.pdf` 先过 `pdftext --check`（能打开、至少一页）；`.md` 只交 `out/` 下的，不跑检查。`out/` 下的排在前面，上限不变（至多检查 8 个、交付 5 个）。路径先规整再比较：模型导入时写的 `./in/a.pdf`、`/work/in/a.pdf` 与列出来的 `in/a.pdf` 是同一个文件。失败消息改为 `the run reached its limit of N steps; 2 files were delivered from the workspace as they stood`，不再说"已校验" | PDF 与 Office 文件一样是成品格式，能打开就值得交。Markdown 没有可跑的检查，又是模型记笔记的格式，把工作区里所有 `.md` 都交出去会把草稿也交给成员；`out/` 是 skill 让模型放成品的地方，放进去就是模型自己说"这是给用户的"。消息原来说"已校验"，对 Markdown 不成立；"as they stood"说的是事实：文件是上限那一刻的样子，不是 agent 说做完了的样子。成员附来的 PDF 现在也是候选格式，路径写法不同就认不出它是导入的，会把成员自己的文件再交回去，所以先规整 |

**实施**：`internal/harness`（`salvageable`、`workspacePath`；`salvage` 按它们筛选、排序、决定是否先跑检查）。

**验证**：`TestStepLimitDeliversPDFAndMarkdownToo`（`out/` 下的 PDF 与 Markdown、别处能打开的 PDF 交出去；打不开的 PDF、`out/` 之外的 Markdown、CSV、以 `./in/…` 导入的成员文件都不交；Markdown 不跑检查；三个 `file.delivered`）。去掉路径规整或 `out/` 的限制，这个测试失败。D41 的测试改用新的消息。

**在真实网关与沙箱上触发了一次**（2026-11-17，compose 平台，真实模型）：bob 上传一份 Markdown，让 office-assistant 排成 PDF、另写一份摘要，放在 `out/`，先不交付；运行完成，没有交付。随后给 office-assistant 发布一个步数上限为 2 的发布件（短于 3 步的运行没有收尾段），bob 在同一会话追问一件需要三次运行的事。模型走完两步后，harness 依次调用 `sandbox_list_files`、`sandbox_export_file`（摘要，不检查）、`sandbox_run`（`pdftext --check 'out/派送价格说明.pdf'`，退出码 0）、`sandbox_export_file`（PDF）；运行以 `STEP_LIMIT` 结束，消息是"…2 files were delivered from the workspace as they stood"，两个 `file.delivered`，收件箱是"failed"并列出两个文件，bob 下载到的两个文件与记录的 SHA-256 一致，导入的那份 Markdown 没有被交回，运行结束后票据已清除。之后把原来的发布件重新设为当前。这也是 D41 的兜底第一次在真实平台上运行；此前只有对假网关的单元测试。


## 25. D43：高于上限的附件不交给可能使用外部模型的发布件（2026-11-17，契约 1.9.0，平台 ADR-0002 F8）

D37 让成员的文件经票据进出运行：字节不经过模型。但 agent 用 `officecli view`、`pdftext` 读出来的内容照样作为工具结果进入模型上下文，模型在企业外面时就跟着出去了。平台 ADR-0002 第 7 节写过：这一条必须和文件通道一起上线，或者先只允许本地模型的发布件使用附件。此前两样都没做。

| # | 决策 | 理由 |
|---|---|---|
| D43 | **附件带敏感级别；发布件可能把内容送到企业之外时，级别高于租户上限的附件不给这次运行。** ① `FileAttachment` 增可选的 `sensitivity`（`public`、`internal`、`confidential`、`restricted`，与 idp-cmi 的级别相同；省略即 `internal`；别的值 422）。② 配置里 provider 增 `external`，**省略即 `true`**：只有运维写了 `external: false` 的 provider 才算企业自有。③ 配置增 `files.externalModelCeiling`（缺省 `internal`）与 `files.tenants`（按租户 id 覆盖）。④ 创建运行时，HOST 看这个发布件可能用到的 provider：模型、回退、路由候选；引用了远端 A2A 子 agent 的也算。其中有一个不是企业自有，级别高于上限的附件就扣下：票据当场丢弃（不进 `runs.file_tickets`），运行的 input 里没有它，`attachment:N` 只给留下的文件编号；调用方 context 里加一条 `idp.host/attachments.withheld`，告诉模型哪些文件名、什么级别没有给它，让它如实告诉用户而不是去找；事件流每个文件记一条 `idp.host/attachment.withheld {ref, name, sensitivity, ceiling, externalModels}`，日志记一行 | 拦在票据这里，是因为没有票据沙箱就取不到文件，内容也就读不出来、进不了模型：不需要去识别工具结果里哪些字来自哪个文件。provider 缺省算外部：漏写一行的后果应当是少给文件，而不是把机密文件送出去。回退和路由候选都算：一次运行最后落在哪个模型上，创建时不知道。远端 A2A 子 agent 算外部：委派时父 agent 会把读到的内容写进任务里。上限放在 HOST 的配置而不是去问 CMI 的租户策略（idp-cmi D29 的 `externalModelCeiling`）：HOST 创建运行时不该多一次对 CMI 的依赖；两处现在要运维各配一次 |

**契约**：`idp.telos.host` 1.8.0 → **1.9.0**，只有向后兼容的新增（FileAttachment 的可选 `sensitivity`）。OpenAPI 同步。一致性测试新增带级别的示例与 3 种拒绝（未知级别、空字符串、数字）。只认识 1.8.0 的 HOST 会拒绝带 `sensitivity` 的附件；idp-workspace 因此只在级别不是 `internal` 时发送（idp-workspace D28）。

**没有做的**：
- **本地子 agent 不查**：发布件引用的本地子 agent 用什么模型，创建父运行时没有去读它的发布件。父 agent 读了机密文件再委派给一个用外部模型的本地子 agent，内容会跟着任务文字过去。子 agent 运行本身拿不到票据（D37）。
- **沙箱工作区跨运行存在**：同一个成员在同一个线程里先用本地模型的 agent 导入了机密文件，30 分钟内再让一个用外部模型的 agent 接着用同一个工作区，文件还在。工作区 id 只在那个线程的对话里。
- **对话记忆的事实抽取**（D34）用的 provider 不在检查之内：它读的是用户消息，不读工具结果。
- 没有界面：成员怎么给文件定级见 idp-workspace D28（上传时的参数）。
- 上限没有和 CMI 的租户策略打通。

**验证**：`internal/api` 的 `TestFilesAboveTheCeilingAreWithheldFromExternalModels`（外部模型、自有模型、自有模型加外部回退、加外部路由候选、加远端 A2A 子 agent、什么都没声明、上限 public 与 restricted、按租户的上限；告诉模型的话里只有文件名与级别）；`internal/run` 的 `TestAttachmentSensitivity`；`internal/config` 的 `TestProvidersAreExternalUnlessDeclaredAndTheFileCeiling`；`TestHostEndToEnd/attachments-above-the-ceiling`（PostgreSQL：存下的 input 里没有被扣下的文件，它的票据没有出现在任何一次网关调用里，模型写的 `attachment:1` 换成的是留下那个文件的票据，事件与模型读到的说明都在；租户上限调到 confidential、或把 provider 声明为自有之后同一个请求两个文件都给）。平台 compose 的 `host.yaml` 给本机模型写了 `external: false`；整套验收没有重跑。


## 26. D44：对外工具与敏感上限——附件和 CMI 上下文条目都按上限筛，确认由 HOST 配置强制（2026-11-19，契约 1.10.0，平台 ADR-0003 D7、D8）

修订 D43（只筛附件、只看模型）和 D32 的局限说明（"HOST 不再按 sensitivity 过滤"）。

起因：平台要给 agent 一个能上网搜索、读网页的工具（平台 ADR-0003）。这类工具把内容送出企业：搜索词、要读的地址、模型写进参数里的任何东西。D43 只防"附件跟着外部模型出去"，留下三处口子：（1）一个用企业自有模型的发布件，只要允许了对外工具，就和用外部模型一样能把读到的内容送出去，而 D43 不管它；（2）CMI 上下文条目带着 `sensitivity`，HOST 从来没读过它，D32 的局限写着"这由 CMI 的上限与策略负责"，但 CMI 不知道这个运行会不会联网；（3）运行中刷新上下文（`onStale = refresh`）会把所有 CMI 条目整批换掉，不经过任何筛选；子运行还原样继承父运行的 CMI 条目，一个不能联网的父 agent 因此能把机密条目交给能联网的子 agent。另外，`web_open_url` 之类的工具要成员确认，而确认策略写在发布件里，作者漏写，模型就能直接读任意地址，这违反"控制在模型之外"。

| # | 决策 | 理由 |
|---|---|---|
| D44-1 | **配置：`gateways[]` 增 `externalTools` 与 `confirmTools`**，值都是工具名模式的列表（`path.Match` 语法，匹配网关暴露的工具名，如 `web__*`）。加载配置时校验：空串或写错的模式使 HOST 起不来；写错的键名照旧被 `KnownFields` 拒绝 | 哪些工具会把内容送出企业，是部署的事实，不是发布件作者的声明；发布件不可变而配置会变，所以放在 HOST 配置里 |
| D44-2 | **"这个发布件有对外工具"**：取 `doc.Effective().Tools`（含技能带来的工具），其网关的 `externalTools` 中任一模式匹配任一允许的工具名，或允许列表是 `*`。网关由发布件的 `tools.gateway` 决定，别的网关的模式不算 | 创建运行时网关会列出哪些工具还不知道，`*` 只能按最坏情况算（宁可多扣）；技能的工具合并进来才是运行真正能调的 |
| D44-3 | **筛选的触发条件有两个，对象有两类**：触发 = 可能使用外部模型（D43 的判断，原样）或有对外工具；对象 = 附件（D43）与 CMI 上下文条目（新）。两个触发都成立时，事件里的 `trigger` 写 `external-model+external-tools` | 一个条件成立，内容就可能出企业；筛选的规则对附件和条目应当一致 |
| D44-4 | **CMI 条目：级别高于上限，或级别为空、不是 CMI 的四个级别之一，一律不给运行。** 上限与附件用同一个（`files.externalModelCeiling`，可按租户）。**与附件不同：条目没有级别不按 `internal` 算。** 不给的条目不进运行的 context，模型只被告知条数与级别，写成和 D43 的 `idp.host/attachments.withheld` 同样做法的一条 context（描述 `idp.host/cmi.withheld`）；未知级别的字符串不抄进说明（它来自外面，会进入模型读的文字）。`SensitivityRank` 对未知级别返回 0（当成 public），所以筛选自己先判断 `ok` | 附件的级别来自所有者系统，缺省 `internal` 是约定；CMI 条目缺级别或级别不认识是 CMI 一侧出了错，放行就是把错误变成泄露 |
| D44-5 | **刷新也筛，用创建运行时记下的上限。** 运行的 input（不属于契约）增 `contextCeiling`、`contextTrigger`、`externalTools`；`contextCeiling` 为空表示这个运行没有筛过。`verifyContext` 刷新时用同一个上限，并换掉上一次的说明条目，不叠加；刷新带来的扣下同样记 `idp.host/context.withheld` | 运行中途改配置不应改变已创建运行的规则；刷新是放行的第二条路，不堵就等于没堵 |
| D44-6 | **重放的幂等不变。** 存储层按 `context.resolved` 事件的 `items` 从重放请求的哈希里去掉末尾的 CMI 条目；筛选后末尾是"保留的条目加一条说明"。事件增 `appended`（末尾实际追加的条数），存储层优先读它，没有时（D44 之前的运行）回退到 `items`。`items` 仍是 CMI 返回的条数，idp-workspace 在显示它 | 同一个请求重放得到同一个运行；CMI 每次取到的条目可能不同，筛选后的条数也随之不同，哈希必须比较去掉末尾之后的请求 |
| D44-7 | **事件 `idp.host/context.withheld {count, byLevel, ceiling, trigger}`**；`byLevel` 的键是四个级别和 `unknown`。常量在 `internal/agui` | 审计要能回答"为什么这个运行没读到这些条目"，又不重复条目的内容 |
| D44-8 | **`toolResults: off` 在有对外工具的运行里按 `wrap` 处理**，运行写一条 `idp.host/guard.raised {configured, applied, reason}`（每个 turn 一次）。`Normalize` 已把缺省定为 `wrap`，所以这只影响作者显式写了 `off` 的发布件。在运行时覆盖而不是发布时拒绝：发布件不可变，HOST 配置会变，一个今天合法的发布件明天可能因配置加了 `externalTools` 而带上对外工具 | 对外工具读回来的内容是最不可信的一类；"关掉防护"不应当是发布件作者能对它做的事 |
| D44-9 | **强制确认**：分发工具调用时，网关工具名匹配该网关 `confirmTools` 的调用一律进入 `PENDING_CONFIRMATION`，不看发布件的 `confirmation`。`confirmation.required` 事件多一个 `requiredBy: "host"`。只对网关工具生效：调用方的前端工具和 HOST 的保留工具不是网关的 | 控制在模型之外，也在发布件作者之外 |
| D44-10 | **子运行**：`CreateChildRun` 原样继承父运行的 context。子运行的发布件有对外工具、而父运行的 context 没有筛过（`contextCeiling` 为空）时，继承来的、描述以 `CMI ` 开头的条目全部去掉，加一条只写条数的说明，子运行的流上记 `idp.host/context.withheld {inherited: true, byLevel: {unknown: n}}`。条目的文字里不带级别，所以只能全部扣下。父运行筛过的，条目都在上限以下，不再处理。存储层不认识网关配置，由 harness 把各网关的 `externalTools` 随 `ChildRunInput` 交过去 | 不能联网的父 agent 不应成为机密条目走向能联网的子 agent 的路 |

**契约**：`idp.telos.host` 1.9.0 → **1.10.0，只改描述**，没有任何形状变化。D32 之后 `ContextPolicy` 的描述只说取上下文，现在写明按敏感级别筛选与新事件；事件名与事件字段本来就不属于 schema（自定义事件）。因为文件不再与已发布的 1.9.0 逐字节相同，所以发新版本。`$id`、`contractID`、示例（只有描述行变）、OpenAPI 文字、README 同步；idp-contracts 加 1.10.0 目录与注册表条目，`tools/validate.py` 37 项不变量通过（含 1.10.0 对所有旧版本的"只增不减"检查）。原计划写的"schema 里有一处描述说 HOST 不按 sensitivity 过滤"并不在 schema 里，而在本文 D32 的局限里；schema 里改的是 `ContextPolicy` 的描述。

**取舍与边界**：
- **所有对外工具一视同仁。** `externalTools` 不区分"搜索词出去"和"整页内容出去"；筛的是运行读到的内容，不是工具的参数。
- **宁可多扣。** `*` 算对外；没有级别算超限；子运行的继承条目全部扣下。运维要放开，就在配置里不列 `externalTools`。
- **`toolExecution: caller` 的运行也照筛。** 调用方自己的工具不碰网关，但创建运行时不为此改规则：多扣的代价小于漏判的代价。
- **子运行只看对外工具，不看模型。** 子运行的发布件用外部模型、父运行没筛过时，仍照 D43 的局限不查（本地子 agent 的模型）。
- 任务文字带出数据（父 agent 把读到的内容写进委派给子 agent 的任务）的缺口仍在，留到阶段 2（平台 ADR-0003 附录 C 第 2 节）。
- 附件的 `idp.host/attachment.withheld` 事件多了 `trigger`；说明文字改为"模型或工具"。

**实施**：`internal/config`（`Gateway.ExternalTools`、`ConfirmTools`、加载时校验）；`internal/agent`（`ToolPolicy.MatchesAny`、`MatchesTool`）；`internal/cmiclient/screen.go`（`Screen`、`Screening`、`DropInherited`）；`internal/api`（`FilePolicy.ExternalTools`、`hasExternalTools`、`trigger`，`createRun` 记录 input，`resolveContext` 筛选）；`internal/run`（`Input.ContextCeiling`、`ContextTrigger`、`ExternalTools`；`ChildRunInput.ExternalTools`）；`internal/harness`（`Gateway.ExternalTools/ConfirmTools`、`dispatch`、`guardedMessages`、`verifyContext`）；`adapters/store/postgres`（`contextItems` 读 `appended`；`CreateChildRun`）；`cmd/idp-host`。

**验证**：`internal/config` 的 `TestGatewayToolPatternsAreValidatedAtLoad`；`internal/agent` 的 `TestToolPolicyMatchesExternalToolPatterns`；`internal/cmiclient` 的 `TestScreenWithholdsAboveTheCeilingAndFailsClosedOnUnknownLevels`、`TestScreeningTellsTheModelCountsAndLevelsOnly`、`TestDropInheritedRemovesEveryCMIEntryAndSaysHowMany`；`internal/api` 的 `TestTheTriggerIsAnExternalModelOrAnExternalTool`、`TestFilesAboveTheCeilingAreWithheldFromRunsWithExternalTools`、`TestResolveContextScreensCMIEntriesOfAnExternalRun`；`adapters/store/postgres` 的 `TestContextItemsReadsTheNumberOfEntriesAppended`；`internal/harness` 的 `TestHostConfigurationForcesConfirmationOfNamedTools`、`TestToolResultsAreWrappedWhenTheRunHasExternalToolsWhateverTheReleaseSays`、`TestRefreshedContextIsScreenedWithTheCeilingRecordedAtCreation`、`TestChildRunsAreCreatedWithTheGatewaysExternalToolPatterns`；`TestHostEndToEnd/cmi-above-the-ceiling`（PostgreSQL：外部模型与外部工具两种触发、自有模型且无对外工具时不筛、同一请求重放得到同一个运行，包括 CMI 第二次答得不同的情形，子运行得不到父运行的条目）。每条规则去掉一次、看对应测试失败的记录见实施时的报告。D43 的测试没有改动。集成测试里 `cmi-context` 与 `cmi-verify-at-steps` 的假 CMI 条目补了 `sensitivity: internal`，否则新规则会把它们当成没有级别而扣下。

**没有做的**：
- 平台 compose 与 kind 上的验收（`host.yaml` 的 `externalTools`、`confirmTools`，PATH M、N）不在这一步。
- 没有界面：被扣下的条数只在事件流与模型的回答里。
- idp-web 侧的每次运行调用上限做不到（调用者断言里没有运行号），由步数上限兜底（平台 ADR-0003 附录 C 第 2 节第 4 条）。

## 27. D45：模型提供者的可重试错误先重试再换候选，等待不越过运行（2026-11-19 决策与实施）

修订 D29（路由与回退）；与 D46 一起修订 D3。

起因：复现测试 `TestARunSurvivesOneRetryableErrorOfTheOnlyConfiguredModel`（R2）：一个候选模型答了一次 429（`Retry-After: 1`）、503 或断开连接，之后是好的，运行却以 `MODEL_ERROR` 失败，只发过 1 次请求。HOST 对可重试的错误（408、429、5xx、传输失败）原来只做一件事：不等待，立刻换路由里的下一个候选；只配一个模型时，一次 429 就让运行失败，而 `Retry-After` 根本没读。D40、D41 的真实模型运行是在单个本地模型上跑 84 到 120 次模型调用、约半小时一个运行：这样的运行一次抖动就全部作废，抖动的概率随调用次数线性增加。

| # | 决策 | 理由 |
|---|---|---|
| D45-1 | **同一个候选先重试，再换下一个。** 每个候选至多 `attempts` 次请求（含第一次，缺省 3）；用完仍失败才走原来的路：下一个候选，或运行以 `MODEL_ERROR` 失败。不可重试的错误（400 等）行为不变；"非可重试的 4xx 使回退循环停止"这一条原有行为不动（见局限） | 重试是对同一个端点的短暂故障的回答，换候选是对它持续故障的回答，两者的代价不同 |
| D45-2 | **等待：从 1 秒起每次翻倍，封顶 20 秒，带抖动**（取计算值的一半到全部之间的随机值，让同时失败的运行不同时回来）。提供者的 `Retry-After`（秒数或 HTTP 日期）比计算值长时取它，上限 60 秒；**`Retry-After` 超过上限就不在这里等**：有下一个候选就立刻试它，没有就失败，消息写明"提供者要求等待 N 秒，比本机愿意等的 M 秒更长" | 提供者说了多久后再来，比我们猜的准；但一个无限长的等待会把运行（和它占的租约）交给提供者 |
| D45-3 | **流出了文字就不重试。** 已写进事件日志的文字增量收不回来：一次尝试在第一个文字增量之后失败，不再对同一候选重试（原来的换下一个候选照旧，被放弃的消息补上 `TEXT_MESSAGE_END`，日志保持良构）。只有推理和工具调用片段不写进日志，它们之后、文字之前的失败仍可重试。严格输出模式（`guards.output.strict`）也按"收到过文字"算，不为它另设规则 | 客户端已经读到的文字不能撤回；重试会让同一条回答出现两次或两个版本 |
| D45-4 | **等待不越过运行。** 等待在运行的 `callCtx` 下进行（D46）：期限到了运行以 `RUN_TIMEOUT` 结束，所有者取消则以 `CANCELED` 结束，都不是等到等待结束。等待会超过期限而还有下一个候选时，不等，直接试下一个 | 期限是硬的（D35） |
| D45-5 | **每步的请求数有上界且看得见**：上界 = 候选数 × `attempts`。每次等待之前写一条 `idp.host/model.retry {step, provider, model, attempt, of, code, waitMs, retryAfterMs}`；`idp.host/model.usage` 增 `attempts`（这一步一共发了多少次请求）。失败的尝试没有用量、不花 token，**也不占运行的步数**（重试在同一步里，`StartStep` 只调一次）；成功的那一步不再带前面失败的 `error_code`（原来回退成功后行里仍留着前一个候选的错误码） | 运维要能回答"这个运行是不是撞上了抖动"；预算只按真正的模型调用算 |
| D45-6 | **设置放在 HOST 配置的 provider 条目**（`retry: {attempts 1..10, backoffSeconds, maxBackoffSeconds, maxRetryAfterSeconds}`），缺省不用配；加载时校验范围，写错的键名照旧被 `KnownFields` 拒绝。不放进发布件 | 哪个提供者怎样限流，是运维知道的事；放进发布件，作者就能给自己的 agent 多烧预算 |
| D45-7 | **`Retry-After` 从各适配器的 HTTP 响应带到 harness**：`model.Error` 增 `RetryAfter`，由 `model.ClassifyHTTP` 读（只对可重试的状态）；OpenAI、Anthropic、Gemini 与 harness-worker 四个适配器都改用它。无法解析的值忽略，超过 24 小时的按 24 小时算 | 适配器是唯一看得见响应头的地方 |

**取舍与边界**
- 缺省 3 次、1 秒起，是"撑过一次短暂故障"的量，不是"等一次长时间故障"的量：长时间故障仍然换候选或失败。要等得更久的运维调 `attempts` 与 `maxBackoffSeconds`。
- 失败的尝试之后换候选时，路由器的时延观测按每次尝试记，一个反复抖动的候选在 `latency` 策略下会被排到后面，这是想要的。
- **不可重试的 4xx 使回退循环停止**（主候选答 400，不试后面的候选）：分析里的旁注，不在本决策范围，没有改。它在多候选且各候选对同一请求的接受度不同（例如上下文窗口不同）时会有影响。

**实施**：`internal/model`（`Error.RetryAfter`、`ClassifyHTTP`、`ParseRetryAfter`、`RetryPolicy`）；`adapters/providers/*`；`internal/config`（`Provider.Retry`）；`cmd/idp-host`；`internal/harness`（`callModel` 的两层循环、`Harness.Retry`）；`internal/agui`（`CustomModelRetry`）。契约不变：事件名与事件字段不属于 schema（同 D44）。

**验证**：复现 `TestARunSurvivesOneRetryableErrorOfTheOnlyConfiguredModel` 通过（未改断言）。新增：`TestRequestsToOneCandidateAreBoundedAndEachWaitIsReported`、`TestTheUsageEventSaysHowManyRequestsTheStepMade`、`TestAFailedAttemptConsumesNoStepAndNoTokens`、`TestARetryAfterLongerThanTheBackoffIsWaitedFor`（秒与日期）、`TestARetryAfterBeyondTheCapIsNotWaitedFor`、`TestTheNextCandidateIsTriedAfterTheRetriesOfTheFirstAreUsedUp`、`TestANonRetryableErrorIsNotRetried`、`TestNoRetryAfterTheFirstStreamedDelta`、`TestAFailureAfterReasoningButBeforeTextIsStillRetried`、`TestTextStreamedByAFailedCandidateIsClosedWhenTheNextCandidateAnswers`、`TestTheWaitBetweenAttemptsEndsAtACancel`、`TestTheWaitBetweenAttemptsEndsAtTheDeadline`、`TestAWaitThatWouldOutliveTheDeadlineMovesToTheNextCandidate`；`internal/model` 的 `TestParseRetryAfter`、`TestClassifyHTTPCarriesRetryAfter`、`TestRetryPolicyWait`；四个适配器的 `TestARetryAfterOfTheProviderReachesTheError`；`internal/config` 的 `TestProviderRetryAndIdleSettings`。每个控制手工拿掉一次、看对应测试失败、再恢复（D46 末尾的表）。

**没有做的**：没有对真实模型验证（只用测试替身）；没有熔断（连续失败的提供者仍然每个运行各试各的）；没有按错误类型区分策略（429 与 5xx 同一套）；不可重试 4xx 的回退行为（见上）。

## 28. D46：期限与取消到达进行中的调用（2026-11-19 决策与实施）

修订 D3（运行状态机与租约）、D35（期限之后不再执行任何东西）。

起因：复现测试 `TestARunEndsWithinAFewSecondsOfItsDeadlineWhateverItIsWaitingFor`、`TestACancelledRunEndsWithinAFewSecondsWhateverItIsWaitingFor`（R3）。期限只在认领时和循环开头检查，一次进行中的调用不受它约束：模型调用没有总时限（只有 2 分钟的响应头时限），响应头之后停住的流没有任何上限，而心跳一直续着租约；网关工具只受 `ToolTimeout`（60 秒）约束；运行中的取消只在下一次 `StartStep` 时才被看到，运行在这一步里结束就丢了。D40、D41 的运行是半小时、上百次调用，期限与取消要靠它们到达调用，而不是等调用自己回来。

| # | 决策 | 理由 |
|---|---|---|
| D46-1 | **每个调用都在 `callCtx` 下进行。** `turn.ctx`（worker 的）只用于写库；`turn.callCtx` 在 worker 的 ctx 结束时、运行的期限到时（`context.AfterFunc` 式的计时器，原因 `errDeadline`）、所有者要求取消时（原因 `errCancelRequested`）结束。模型调用、网关 `tools/list` 与 `tools/call`、A2A、CMI（`Verify`、`Resolve`）、凭据续期、输出分类器都用它 | 结束之后还要写的东西（步、工具调用、运行的结局）必须能写，所以写库不用它 |
| D46-2 | **取消靠 harness 自己问库。** 运行进行中每 `CancelPoll`（缺省 1 秒）调一次 `Store.CancelRequested`（主键上的一次不加锁读，同时校验认领）。认领已失效（`ErrFenced`）也结束 `callCtx` | 心跳是租约的三分之一（缺省 10 秒），达不到"几秒内"；一次主键读每秒一次，比每步一次更便宜的办法就没有了。它同时让内存存储的单元测试不需要 worker |
| D46-3 | **期限：`RUN_TIMEOUT`，失败码与消息同原来的期限路径**（"the run exceeded its time limit"），在期限后约一秒内结束。**取消：`CANCELED`**，同 `StartStep` 处发现取消的路径 | 不为新路径造新的终态 |
| D46-4 | **空闲超时：流在两次读取之间静默超过 `idleTimeoutSeconds`（缺省 60 秒，provider 条目）就取消请求**，错误是可重试的传输错误并带 `Idle` 标记，步的错误码为 `idle_timeout`；若还没有文字，按 D45-3 重试。适配器在响应头之后对读取计时，每次读到数据就重新计时：仍在发送的流不受限 | 响应头之后的停滞原来没有任何上界 |
| D46-5 | **取消与最后一次模型调用。规则：答案存入之前看到的取消生效，之后看到的来得太晚。** 具体：模型调用返回后、`AppendMessages` 存答案之前，harness 再同步问一次库（除监视之外）；取消已请求则不存答案，已流出的文字补上 `TEXT_MESSAGE_END`，步记 `canceled`（token 照记），运行以 `CANCELED` 结束。答案的 `AppendMessages` 返回之后才到的取消，运行照常 `COMPLETED`（答案已存、可见，`Finish` 在同一次 worker 回合里紧接着发生）。两次检查之间仍有一个窗口（毫秒级），文档如实写明，不假装原子 | 取消不能因为答案恰好在最后一步到达而丢；又不能撤回已经存下并展示给用户的答案 |
| D46-6 | **被截断的调用留下一致的状态。** 模型调用被期限或取消截断：已流出的文字补 `TEXT_MESSAGE_END`；步的行以错误码 `deadline` / `canceled` / `idle_timeout` 结束，三者可分；运行以 `RUN_ERROR` 收尾。工具调用被截断：调用记为 **`FAILED`**，结果写 `interrupted: …，工具可能已经完成也可能没有`，并照常追加工具消息（线程里每个工具调用都有回答，下一个运行不会被提供者以"tool_calls 后面没有 tool 消息"拒绝）；同一步里还没执行的 `CONFIRMED` 调用记为 `FAILED`（`not run: …`），被拒的 `DENIED`。**没有为"被中断"新增状态**：新状态要改表的 CHECK 约束（迁移）与契约，而 `FAILED` + 结果文字已经把事实说清楚；`finished_at` 因此被设置，工具调用接口里看得见 | 不改 schema、不改契约；运行已经终止，不存在"恢复时怎么办" |
| D46-7 | **worker 自己的 ctx 结束（关停）或认领丢失时什么都不写**，同原来的 `ErrFenced`：`EXECUTING` 的调用留在 `EXECUTING`，恢复时怎么处理是 R1 的事，本决策不改 | 范围 |
| D46-8 | **步数用尽时的交付与确认等待照旧。** 交付用同一个 `execute`，同样受期限约束（原来就在期限后不调用）；等待确认的运行不占 worker，`Run` 返回时监视随之停止；确认请求的过期时间仍取"请求时刻 + `confirmationTimeoutSeconds`"与期限中较早的（D35） | 本决策不改 D35，只让它对进行中的调用也成立 |

**取舍与边界**
- 取消每秒一次库读，运行数多时是每秒每个运行一次主键读；`CancelPoll` 目前不进配置文件（缺省值够用，需要时再开）。
- 被取消时等待确认的调用（`PENDING_CONFIRMATION`）不在这里处理，与原来取消一个正在运行的运行时一样。
- harness-worker 提供者也受空闲超时约束；它的协议没有心跳，一个思考很久不发数据的外部 harness 要把 `idleTimeoutSeconds` 调大。
- 空闲超时对"仍在发 keep-alive 注释行"的服务器不触发（读到了字节）。

**实施**：`internal/harness`（`turn.callCtx`、`watch`、`settle`、`interruptedCall`、`abandon`、`callModel`、`loop`）；`internal/model`（`IdleWatch`）；`adapters/providers/*`（`IdleTimeout`）；`adapters/store/postgres`（`CancelRequested`）；`internal/config`（`idleTimeoutSeconds`）。契约不变。

**验证**：复现 `TestARunEndsWithinAFewSecondsOfItsDeadlineWhateverItIsWaitingFor`、`TestACancelledRunEndsWithinAFewSecondsWhateverItIsWaitingFor` 通过（未改断言）。新增：`TestAStalledStreamIsCutByTheIdleTimeoutAndRetried`、`TestAStreamThatStallsAfterTextIsCutNotRetriedAndClosed`、`TestAnInterruptedModelCallLeavesTheStepAndTheEventLogConsistent`（期限、取消）、`TestACancelDuringTheFinalModelCallEndsTheRunCanceledUnlessTheAnswerWasStored`（两侧）、`TestAnInterruptedToolCallIsRecordedAsSuchAndTheRestOfTheStepIsAnswered`（期限、取消）、`TestARunWaitingForConfirmationIsNotAffectedByTheWatch`、`TestIdleWatchCutsASilentBody`、四个适配器的 `TestAStreamThatStopsSendingFailsAsAnIdleTransportError`；集成（真实 PostgreSQL）`TestACancelOfARunningRunReachesTheModelCallThroughTheStoreOnPostgres`（store.CancelRun + worker 心跳）、`TestACancelThroughTheEndpointInterruptsAToolCallOnPostgres`（HTTP 取消接口）、`TestTheDeadlineReachesAModelCallOnPostgres`。

**拿掉一个控制、看测试失败（D45、D46 共用）**

| 控制 | 拿掉的东西 | 失败的测试 |
|---|---|---|
| 重试本身（D45-1） | `callModel`：重试条件恒为否（不重试同一候选） | `TestRequestsToOneCandidateAreBoundedAndEachWaitIsReported`、`TestTheUsageEventSaysHowManyRequestsTheStepMade`、`TestAFailedAttemptConsumesNoStepAndNoTokens`、`TestTheNextCandidateIsTriedAfterTheRetriesOfTheFirstAreUsedUp`、`TestARetryAfterLongerThanTheBackoffIsWaitedFor`、`TestARetryAfterBeyondTheCapIsNotWaitedFor`、复现 `TestARunSurvivesOneRetryableErrorOfTheOnlyConfiguredModel` 三个子例 |
| 遵守 `Retry-After`（D45-2） | `RetryPolicy.Wait`：`max(d, retryAfter)` 改为 `d` | `TestRetryPolicyWait`、`TestARetryAfterLongerThanTheBackoffIsWaitedFor`、复现 R2（429 子例） |
| `Retry-After` 上限（D45-2） | `RetryPolicy.Wait`：去掉超过 `MaxRetryAfter` 就返回不等待 | `TestRetryPolicyWait`、`TestARetryAfterBeyondTheCapIsNotWaitedFor` |
| 流出文字后不重试（D45-3） | 去掉重试条件里的 `s.streamed()` | `TestNoRetryAfterTheFirstStreamedDelta`、`TestAStreamThatStallsAfterTextIsCutNotRetriedAndClosed` |
| 等待随期限与取消结束（D45-4） | `turn.sleep`：去掉 `callCtx.Done()` 分支 | `TestTheWaitBetweenAttemptsEndsAtACancel`、`TestTheWaitBetweenAttemptsEndsAtTheDeadline` |
| 等待会越过期限时先试下一个候选（D45-4） | 去掉 `!h.Now().Add(wait).Before(d)` 的分支 | `TestAWaitThatWouldOutliveTheDeadlineMovesToTheNextCandidate` |
| 失败尝试的消息补 `TEXT_MESSAGE_END`（D45-3、D46-6） | 去掉失败尝试后的 `TextMessageEnd` | `TestNoRetryAfterTheFirstStreamedDelta`、`TestTextStreamedByAFailedCandidateIsClosedWhenTheNextCandidateAnswers`、`TestAStreamThatStallsAfterTextIsCutNotRetriedAndClosed`、`TestAnInterruptedModelCallLeavesTheStepAndTheEventLogConsistent` |
| 成功的步不带前面失败的错误码（D45-5） | 去掉成功时的 `st.ErrorCode = ""` | `TestAFailedAttemptConsumesNoStepAndNoTokens` |
| `model.usage` 的 `attempts`（D45-5） | 去掉 `"attempts"` 字段 | `TestTheUsageEventSaysHowManyRequestsTheStepMade`、`TestTheNextCandidateIsTriedAfterTheRetriesOfTheFirstAreUsedUp` |
| `model.retry` 事件（D45-5） | 事件名换成 `model.routed` | `TestRequestsToOneCandidateAreBoundedAndEachWaitIsReported`、`TestAStalledStreamIsCutByTheIdleTimeoutAndRetried` |
| 失败消息写明提供者要求等待多久（D45-2） | 去掉 `askedWait` 的附加说明 | `TestARetryAfterBeyondTheCapIsNotWaitedFor` |
| `Retry-After` 从响应带到错误（D45-7） | `ClassifyHTTP` 不设 `RetryAfter` | `TestClassifyHTTPCarriesRetryAfter`，以及 openai、anthropic、gemini、harnessworker 各自的 `TestARetryAfterOfTheProviderReachesTheError` |
| 配置校验（D45-6、D46-4） | `Load` 不调 `Provider.validate` | `TestProviderRetryAndIdleSettings` |
| 空闲超时（D46-4） | 四个适配器各把 `NewIdleWatch(p.idle(), …)` 改成 `NewIdleWatch(0, …)`（不监视） | 各适配器的 `TestAStreamThatStopsSendingFailsAsAnIdleTransportError`，harness 的 `TestAStalledStreamIsCutByTheIdleTimeoutAndRetried` 与 `TestAStreamThatStallsAfterTextIsCutNotRetriedAndClosed`（都挂住，40 秒测试超时后失败） |
| 期限计时器（D46-1） | `watch`：不创建期限计时器 | 复现 `TestARunEndsWithinAFewSecondsOfItsDeadlineWhateverItIsWaitingFor`、`TestTheWaitBetweenAttemptsEndsAtTheDeadline`、`TestAnInterruptedModelCallLeavesTheStepAndTheEventLogConsistent`、`TestAnInterruptedToolCallIsRecordedAsSuchAndTheRestOfTheStepIsAnswered`；集成 `TestTheDeadlineReachesAModelCallOnPostgres` |
| 取消监视（D46-2） | `watch`：发现取消请求时不结束 `callCtx` | 复现 `TestACancelledRunEndsWithinAFewSecondsWhateverItIsWaitingFor`、`TestTheWaitBetweenAttemptsEndsAtACancel`、`TestAnInterruptedModelCallLeavesTheStepAndTheEventLogConsistent`、`TestAnInterruptedToolCallIsRecordedAsSuchAndTheRestOfTheStepIsAnswered` |
| 存储的 `CancelRequested`（D46-2） | Postgres 的 `CancelRequested` 恒答否 | 集成 `TestACancelOfARunningRunReachesTheModelCallThroughTheStoreOnPostgres`、`TestACancelThroughTheEndpointInterruptsAToolCallOnPostgres`（运行以 COMPLETED 结束） |
| 存答案之前的取消检查（D46-5） | 去掉 `AppendMessages` 之前的同步 `CancelRequested`（轮询调成一小时） | `TestACancelDuringTheFinalModelCallEndsTheRunCanceledUnlessTheAnswerWasStored` |
| 工具调用走 `callCtx`（D46-1） | `execute`：`CallTool` 用 `t.ctx` | 复现 R3 取消的工具子例（`TestACancelledRunEndsWithinAFewSecondsWhateverItIsWaitingFor`）、`TestAnInterruptedToolCallIsRecordedAsSuchAndTheRestOfTheStepIsAnswered` |
| 被中断的工具调用记为 FAILED（D46-6） | `interruptedCall` 不记录 | `TestAnInterruptedToolCallIsRecordedAsSuchAndTheRestOfTheStepIsAnswered` |
| 同一步其余调用得到回答（D46-6） | `dispatch` 不调 `abandon` | `TestAnInterruptedToolCallIsRecordedAsSuchAndTheRestOfTheStepIsAnswered` |
| 被中断的模型调用结束步的行（D46-6） | `loop`：被中断时不 `FinishStep` | `TestAnInterruptedModelCallLeavesTheStepAndTheEventLogConsistent` |

**没有做的**：（R1、R4 的复现测试在 `internal/harness/repro_pending_test.go` 与 `tests/integration/repro_pending_test.go`，构建标签 `repro`，不属于 `make test`；它们故意失败，用 `go test -tags repro ./internal/harness/ ./tests/integration/` 运行。）R1（恢复时重做 `EXECUTING` 的调用；助手消息与工具调用记录分两次写；后来决定并实施为 D47）与 R4（被截断的回答使运行失败）按决定不在此处理，对应的复现测试仍然失败；没有在 compose 或 kind 上、也没有用真实模型验证；没有为被中断的工具调用新增状态（见 D46-6）；`CancelPoll` 没有进配置。

## 29. D47：worker 中断时进行中的工具调用——只读与幂等的重发，其余不重发并告诉模型（2026-11-19 决策与实施）

修订 D3（运行状态机与租约）；承接 D46-7（"恢复时怎么处理是 R1 的事"）。决策文件：`idp-host-r1-tool-call-resume-decision.md`（选择点 1 取 (c)，选择点 2 现在不做）。

起因：复现测试 R1(a)、R1(b)（`repro` 构建标签下，本决策把它们改为按决定断言并移出标签）。R1(a)：HOST 在调用网关之前把工具调用标为 `EXECUTING`，网关返回后才写结果；worker 在这两步之间停止（崩溃，或收到 SIGTERM 而被滚动发布杀掉），下一个 worker 认领运行时对 `EXECUTING` 的调用照常再执行一次：网关收到同一个 `tools/call` 2 次、运行仍记为完成，在真实 PostgreSQL 上同样复现；窗口是工具调用的时长（缺省最长 60 秒）。D3 写的"不会重复工具调用"不成立。R1(b)：助手消息（`AppendMessages`）与它的工具调用记录（`RecordToolCalls`）是两个事务，在中间停止会留下一条带工具调用、却没有任何调用记录的助手消息，下一个 worker 再调模型时被提供者拒绝（400），运行失败；窗口是几毫秒。

绕不开的事实：HOST 分不清"已经执行完但结果丢了"和"根本没发出去"，所以"恰好执行一次"做不到，只能选"至少一次"（中断后重发，副作用可能做两次）或"至多一次"（中断后不重发，调用可能其实没执行）。D47 按工具的性质选。

| # | 决策 | 理由 |
|---|---|---|
| D47-1 | **续跑时发现 `EXECUTING` 的网关工具调用，只有在重发无害时才重发**：该工具在网关**现在**的 `tools/list` 里带 `readOnlyHint` 或 `idempotentHint`（缺省为假），且没有被所属网关的 `neverResendTools` 点名。重发的那次照常执行（`CONFIRMED` 的路径），事件 `idp.host/tool.interrupted {toolCallId, tool, resent: true, reason: "worker_stopped"}` 先写入事件流，运维看得见每一次重复 | 重发只读或幂等的调用没有害处，运行无感知地继续 |
| D47-2 | **其余的不重发**：没有标注、标注为假、网关已不再列出该工具（或发布件已不再允许它）、被 `neverResendTools` 点名、任何子智能体调用（远程 A2A：HOST 看不见也撤不回远程智能体做了什么；见 D47-5）。调用记为 `FAILED`，结果与工具消息为 `interrupted: the turn that was running this call ended while it was running (its worker stopped, died or lost the database); the tool may or may not have completed it, so its outcome is unknown. Check whether it was done before doing it again`，随同事件 `idp.host/tool.interrupted {…, resent: false, reason: "worker_stopped"}` 在同一个事务里写入；运行继续，模型读到这条工具错误后自己决定核实还是放弃 | 措辞沿用 D46-6 的"工具可能已经完成也可能没有"，两种情形读起来一致；`FAILED` 加结果文字，不新增状态（不改表的 CHECK 约束与契约，同 D46-6） |
| D47-3 | **`reason` 区分中断的原因**：`worker_stopped`（调用所在的 turn 在结果落库之前结束了：worker 停止、崩溃，或同一个 worker 在一次存储暂时出错后重新进入这个运行——`Run` 因存储错误返回、租约过期后被重新认领，这是会发生的；所以消息写"turn 结束了"而不是"worker 停止了"。由接手的 turn 记，事件里的 `reason` 名字不变）；`deadline`、`canceled`（D46 的期限与取消打断了调用，原来只有 `FAILED` 的结果文字，现在同一个事件也记，`resent` 恒为 `false`） | 一个事件名，运维一处查全部"调用被打断" |
| D47-4 | **工具的性质来自网关目录里的标注，运维可用 `neverResendTools` 强制"不重发"**。`mcpclient.Tool` 读 `annotations.readOnlyHint`、`idempotentHint`；harness 在每个 turn 开头的 `tools/list` 里建表，所以"现在列出的"是接手的那个 worker 这一次问到的。配置 `gateways[].neverResendTools`，模式语法、匹配函数（`agent.MatchesTool`）与加载时校验同 `externalTools`、`confirmTools`（D44）；匹配即不重发，不看标注。配置只能收紧，不能把没有标注的工具放宽 | 幂等性上游最清楚；标错的代价是"重复执行一次"，与今天的行为一样，不会更糟（见取舍）；运维要对个别工具收紧时有一个开关 |
| D47-5 | **HOST 的保留工具与子智能体**：`resource__read` 只读发布件自己的内容，视为只读（重发；它本来不进 `EXECUTING`）。**任何子智能体调用**（远程 A2A，或本地）在 `EXECUTING` 里被发现都不重发，按 D47-2 回答。本地子智能体调用本该是 `DELEGATED`、不会是 `EXECUTING`；对一个不该存在的状态，安全的答案是"不再发出"（再发一次会多出一个子运行，不是无害的） | 保留工具不经过网关目录 |
| D47-6 | **助手消息与它的工具调用记录在同一个事务里写入**：`Store.AppendAssistantTurn(msg, calls, events)`；harness 先 `classify`（限额、前端工具、允许列表、确认策略，不写库），再一次写入，再 `dispatch` 执行。原来的 `AppendMessages`、`RecordToolCalls` 保留（别处还在用：步数用尽时的交付 D41、旧数据的补建） | 这样"带工具调用的助手消息没有调用记录"不再可能发生 |
| D47-7 | **旧数据的补建**：续跑时线程以一条带工具调用的助手消息结束、而这些调用没有 `tool_call` 行（`run.Context.RecordedToolCallIDs`：该消息的调用里已有记录的 id），就为缺的创建行（`classify` 决定初始状态，含确认策略与限额），然后照 `dispatch` 执行。这些调用确实没发出过，不算重复，不写 `tool.interrupted`。它们的 `TOOL_CALL_*` 事件是随助手消息写过的，补建只写决定类事件（`tool.denied`、`confirmation.required`），所以跟着事件流的客户端不会看到重复的 `TOOL_CALL`。按"该消息的 id"而不是"本运行的行"查，所以线程里上一个运行留下的前端工具调用不会被当成缺记录 | 修复已经写下的数据，不要求迁移 |
| D47-8 | **停机时不制造中断：进行中的调用做完，不开始任何新的，在下一个边界交出运行。** worker 收到停止信号（SIGTERM）后：(a) 不再认领；(b) 通过 `run.WithStop(ctx, stop)`（一个独立于取消的通道，`Harness.Run` 不带它时行为不变）告诉进行中的 `Harness.Run`：进行中的调用，不论是模型调用、网关调用、远程智能体调用还是 D45 的重试等待之外的任何调用，都做完；不开始新的模型调用、工具调用、子智能体调用，也不重试；在下一个边界（D47-9）把所存的一切留在一致的状态，然后交出认领（D47-10）；(c) 超过 `StopGrace`（`IDP_HOST_STOP_GRACE`，缺省 20 秒，`0` = 立即中断，原来的行为）仍在进行的被中断，同 D46：调用留在 `EXECUTING`，租约照常过期，接手的 turn 按 D47-1、D47-2 处理。`Worker.Run` 在宽限期之后一定返回；租约在宽限期内照常心跳 | 一次正常的滚动发布不应让每个进行中的调用都变成"结果未知"；也不应让一个在宽限期第 19 秒开始的调用在第 20 秒被切断 |
| D47-9 | **边界与每个边界留下的状态**（`errStopped`；进行中的调用总是先做完、它的答案先落库）。**1. 回合开始**（`tools/list` 之前）：什么都没做，线程不变。**2. 循环顶部**（`StartStep` 之前）：之前每一步都已存好，每个调用都有回答；不开始新的一步。**3. 一次模型请求失败之后、重试或换候选之前，以及 D45 的等待中**：等待立即结束，不再发请求；这一步的行记 `FinishStep`，错误码 `stopped`，没有消息；下一个 worker 开始新的一步（这一步照常计入步数）。**4. 一步的助手消息与调用记录已存好、调用还没开始**：这是合法的静止状态——调用留在 `CONFIRMED`（`DENIED` 的调用照常回答，因为回答不开始任何东西；`FRONTEND` 与 `PENDING_CONFIRMATION` 不动），下一个 worker 的 `resolveDecided` 执行它们。**5. 每个调用开始之前**（`execute` 的入口，也覆盖重发、子智能体、步数用尽时的交付）：该调用与其后的留在原状态。有等待成员确认的调用时，回合照常以 `AWAITING_INPUT` 和 `RUN_FINISHED` 的 interrupt 结束，不交出（客户端要看到 interrupt；未开始的 `CONFIRMED` 调用在决定之后的续跑里执行）。没有的边界：一次模型调用或工具调用进行中途不会停（那是宽限期之后的切断，D46 的路径） | 每个边界之后线程里不会有"没有回答的工具调用"，也不会有"没有记录的调用" |
| D47-10 | **交出认领**：存储新增 `Release(ctx, claim)`：把租约结束时间设为现在，运行仍是 `RUNNING`（客户端看不到状态变化，不是失败），下一次 `Claim` 立刻能认领，不必等租约过期；按认领的 epoch 受围栏（与其他写入相同，认领已失效返回 `ErrFenced`），交出之后旧认领的所有写入都被拒。只用于 D47-8；宽限期之后 `ctx` 已结束就不交出（租约自己过期，同崩溃） | 租约缺省 30 秒，不交出，停机造成的就是 30 秒的停顿 |
| D47-11 | **工具调用的最终状态与它的工具消息是一次写入**：`Store.AnswerToolCall(id, state, result, isError, msg, events)` 在一个事务里改调用的状态与结果、追加工具消息、写事件（含 D47-3 的中断事件与 `TOOL_CALL_RESULT`）。原来 `answer` 是 `UpdateToolCall` 加 `AppendMessages` 两个事务，在两者之间停止会留下已完成而线程里没有回答的调用，下一次模型请求被提供者拒绝，与 R1(b) 同类。`DENIED` 的记录在分发时就带着原因（`ToolCall.Result`，记录时写入），所以没来得及回答的 `DENIED` 调用由续跑的 worker 用同一个原因回答（`Context` 的未解决调用现在包括 `DENIED`） | 同 D47-6 |
| D47-12 | **旧数据里缺的工具消息**：续跑时线程以带工具调用的助手消息结束（其后只有工具消息），其中某个调用处于最终状态（`DONE`、`FAILED`、`DENIED`、`REJECTED`、`EXPIRED`）而线程里没有它的工具消息，就用存下的结果补一条（`FAILED` 等错误带"调用没有成功"的错误说明）并写 `TOOL_CALL_RESULT`。D47-7 的补建与这一条合在 `repairLastTurn`；`run.Context.LastCalls` 是最后一条带工具调用的助手消息的全部记录（任何状态），取代原来的 `RecordedToolCallIDs`。`FRONTEND`（调用方回答）、`PENDING_CONFIRMATION`、`CONFIRMED`、`EXECUTING`、`DELEGATED` 不是最终状态，不在此列 | 修复已经写下的数据 |
| D47-13 | **计数**：服务已有 Prometheus 指标（`idp_host_*`），所以加 `idp_host_tool_interruptions_total{resent, reason}`：每个被发现打断的调用一次（`resent` 为 `true`/`false`，`reason` 为 `worker_stopped`、`deadline`、`canceled`）。harness 经可选接口 `ToolInterrupted(resent, reason)` 调用，没有实现它的 `Metrics` 不受影响；没有新依赖 | 事件看得见每一次，指标让运维不必翻事件流 |

**Kubernetes 清单**：`host.yaml` 的 Deployment 原来没有写 `terminationGracePeriodSeconds`，取 Kubernetes 的缺省 30 秒，缺省宽限期 20 秒已经小于它，所以不必改值；但这个依赖是隐含的，清单现在把 30 秒写明并在 ConfigMap 里写 `IDP_HOST_STOP_GRACE: "20s"`，旁注两者的关系。compose 的 host-worker 用 Docker 的缺省 10 秒就会在宽限期内被杀，所以给它加了 `stop_grace_period: 30s`。API 进程的 HTTP 关停另有 20 秒（`srv.Shutdown`），与宽限期同时开始，不叠加。

**取舍与边界**
- **为什么这里信任标注，而 D44 不信 `openWorldHint`**：D44 的判断是安全性质——一个标错的"不对外"会让机密内容离开企业，后果不可逆，所以由运维在 HOST 配置里声明。幂等性不同：标错的后果是"重复执行一次"，与 D47 之前每一次中断的行为相同，不会更糟；而工具是否幂等，上游最清楚，网关的目录里这些标注是钉住并经过评审的。这里留一个只能收紧的配置项（`neverResendTools`），不留放宽的。
- 重发不保证没有副作用：标错了 `idempotentHint` 的工具会被执行两次；`readOnlyHint` 也只是声明。`idp.host/tool.interrupted {resent: true}` 让这种事看得见。
- 不重发时调用可能其实已经完成；模型被告知并自己核实，核实用的是它手里的工具，没有则没有办法。运行不会因此失败。
- 一个 turn 的 `tools/list` 失败仍使运行失败（原有行为），所以"目录里现在没有"只在目录能读到时成立。
- 宽限期（20 秒）短于缺省的 `ToolTimeout`（60 秒）：超过 20 秒的调用在停机时仍可能被中断，按 D47-2 处理。要更长的话运维同时调 `IDP_HOST_STOP_GRACE` 与终止宽限期。
- 步数用尽时的交付（D41）经过同一个 `execute`，所以停止后不再开始；`verifyContext`（CMI）与输出分类器是步内的短调用，不单独检查。
- 崩溃（进程被杀）不经过宽限期，只有 D47-1、D47-2 的处理。
- 契约不变：事件名与事件字段不属于 schema（同 D44、D45）；工具调用的状态没有新增（`FAILED` + 结果文字）。

**实施**：`internal/mcpclient`（`Tool.Annotations`、`ToolAnnotations`、`SafeToRepeat`）；`internal/config`（`Gateway.NeverResendTools`、加载时校验）；`internal/agui`（`CustomToolInterrupted`）；`internal/run`（`Context.LastCalls`、`WithStop`、`StopChan`）；`internal/harness`（`Gateway.NeverResendTools`、`turn.repeatable`、`mayResend`、`resumeExecuting`、`interruptedEvent`、`answerWith`、`classify`/`dispatch` 的拆分、`repairLastTurn`、`stopping`、`errStopped` 与五个边界、`settle` 交出认领、`Store.AppendAssistantTurn`、`AnswerToolCall`、`Release`、`interruptionMetrics`）；`adapters/store/postgres`（`AppendAssistantTurn`、`AnswerToolCall`、`Release`、`insertToolCalls` 写 `result`、`Context` 读 `LastCalls` 并把 `DENIED` 算作未解决）；`internal/worker`（`StopGrace`、`Runner`、停止通道、`ToolInterrupted` 指标）；`cmd/idp-host`（`IDP_HOST_STOP_GRACE`、`NeverResendTools` 的装配）；`deploy/kubernetes/host.yaml`、`deploy/compose.yaml`；README。

**验证**：`internal/harness/resume_test.go`（内存存储）：`TestAToolCallThatWasExecutingWhenTheWorkerDiedIsSentAgainOnlyWhenRepeatingIsHarmless`（8 种情形各两种死法：无标注、两个标注都为假、只读、幂等、`neverResendTools` 压住只读与幂等、不相干的模式不影响、网关已不再列出；断言网关收到的次数、事件、模型读到的文字）、`TestARemoteAgentCallThatWasExecutingWhenTheWorkerDiedIsNotSentAgain`、`TestACallCutShortByACancelIsAnnouncedWithItsReason`、`TestAWorkerThatDiesWhenTheAssistantMessageIsStoredLeavesNoCallsWithoutRecords`（原 R1(b)）、`TestAnAssistantMessageStoredWithoutItsToolCallRecordsIsRepairedOnResume`（旧数据）、`TestARepairedToolCallStillWaitsForTheConfirmationTheReleaseRequires`；`internal/mcpclient` 的 `TestListToolsReadsTheAnnotationsThatSayACallMayBeRepeated`；`internal/config` 的 `TestNeverResendToolsIsReadAndValidatedAtLoad`；`internal/harness/stop_test.go`（停止与交出，内存存储，不用真信号：测试关停止通道、取消 ctx 代表宽限期结束）：`TestAStopWhileAToolCallRunsLetsItFinishStartsNothingAndHandsTheRunOver`、`TestAStopWhileAModelCallRunsStoresItsAnswerStartsNoToolCallAndHandsTheRunOver`、`TestAStopDuringTheWaitBetweenModelAttemptsEndsTheWaitAndHandsTheRunOver`、`TestAStopWhileAModelRequestFailsStartsNoRetry`、`TestAStopDuringTheLastCallOfAStepStartsNoNewModelCall`、`TestAStopBeforeAnythingWasDoneHandsTheRunOverAtOnce`、`TestAStopAnswersTheDeniedCallsOfTheStepAndLeavesTheOthersForTheNextWorker`、`TestADeniedCallThatWasNotAnsweredIsAnsweredByTheNextWorkerWithItsReason`、`TestAStopDoesNotHideTheConfirmationsOfTheStep`、`TestPastTheGracePeriodACallInProgressIsCutAsBefore`、`TestARunWithoutAStopNeverGivesItsClaimUp`（每个停止的测试都断言网关与模型没有新请求、认领已交出、运行仍是 `RUNNING`，第二个 worker 接手后每个工具恰好执行一次并完成）；`resume_test.go` 增 `TestAWorkerThatDiesWhenAToolCallIsAnsweredLeavesNoFinishedCallWithoutItsMessage`（先在两次写入的旧实现上复现：调用 `DONE`、线程里没有工具消息、下一次模型请求被拒；改成一次写入后通过）、`TestAFinishedToolCallWithoutItsToolMessageIsAnsweredFromItsStoredResultOnResume`、`TestEveryFinalStateOfAToolCallWithoutItsMessageIsRepaired`（五个最终状态）、`TestASubagentCallThatWasExecutingWhenTheWorkerDiedIsNotSentAgain`（远程与本地）、`TestInterruptedToolCallsAreCountedByResentAndReason`；`internal/worker` 的 `TestAWorkerToldToStopLetsTheRunInProgressFinishWithinTheGracePeriod`、`TestAWorkerCutsTheRunShortWhenTheGracePeriodIsOver`、`TestAWorkerWithoutAGracePeriodCutsTheRunShortAtOnce`、`TestAWorkerTellsTheRunInProgressToStopWithoutCancellingIt`；`tests/integration/resume_test.go`（真实 PostgreSQL、真实租约与 Claim）：`TestANonIdempotentToolCallThatWasExecutingWhenTheWorkerDiedIsNotSentAgainOnPostgres`（原 R1(a)）、`TestAReadOnlyToolCallThatWasExecutingWhenTheWorkerDiedIsSentAgainOnPostgres`、`TestAnAssistantMessageWithoutToolCallRowsIsRepairedOnResumeOnPostgres`、`TestTheAssistantMessageAndItsToolCallRecordsAreWrittenInOneTransactionOnPostgres`、`TestAWorkerToldToStopLetsTheToolCallInProgressFinishOnPostgres`、`TestAWorkerToldToStopCutsTheToolCallShortAfterTheGracePeriodOnPostgres`、`TestAStopHandsTheRunOverAtOnceWithoutStartingAnotherCallOnPostgres`（租约一分钟，停止后立刻 `Claim` 成功）、`TestAFinishedToolCallWithoutItsToolMessageIsRepairedOnResumeOnPostgres`、`TestAToolCallsFinalStateAndItsToolMessageAreWrittenInOneTransactionOnPostgres`（含 `Release` 后旧认领被围栏）。`repro_pending_test.go` 里只剩 R4。每个控制手工拿掉一次、看对应测试失败、再恢复：

**拿掉一个控制、看测试失败**

| 控制 | 拿掉的东西 | 失败的测试 |
|---|---|---|
| 按标注判断（D47-1、D47-2） | `mayResend`：网关工具一律返回可重发 | `TestAToolCallThatWasExecutingWhenTheWorkerDiedIsSentAgainOnlyWhenRepeatingIsHarmless`（无标注、两个标注都为假、网关已不再列出等子例）；PostgreSQL：`TestANonIdempotentToolCallThatWasExecutingWhenTheWorkerDiedIsNotSentAgainOnPostgres`、`TestAWorkerToldToStopCutsTheToolCallShortAfterTheGracePeriodOnPostgres` |
| 读标注（D47-4） | `mcpclient.Tool.Annotations` 不解析 | `TestListToolsReadsTheAnnotationsThatSayACallMayBeRepeated`、同上的只读与幂等子例 |
| `neverResendTools`（D47-4） | `mayResend`：不看 `NeverResendTools` | 同上的两个 `neverResendTools` 子例 |
| `neverResendTools` 的校验（D47-4） | `config.Load`：校验表里去掉这一项 | `TestNeverResendToolsIsReadAndValidatedAtLoad` |
| 远程 A2A 不重发（D47-2） | `mayResend`：子智能体一律可重发 | `TestARemoteAgentCallThatWasExecutingWhenTheWorkerDiedIsNotSentAgain` |
| 续跑时按 D47 处理 `EXECUTING`（D47-1、D47-2） | `resolveDecided`：`EXECUTING` 回到原来的一律 `execute` | 上面的表测试、远程 A2A 测试；PostgreSQL 的非幂等、只读、宽限期之后三个 |
| 事件（D47-1、D47-2） | 续跑时不写 `tool.interrupted`（事件换成别的名字） | 表测试、远程 A2A 测试 |
| `reason`（D47-3） | D46 的中断事件不带 `deadline`/`canceled` | `TestACallCutShortByACancelIsAnnouncedWithItsReason` |
| 一个事务（D47-6） | `loop`：助手消息与调用记录分成 `AppendMessages` + `RecordToolCalls` | `TestAWorkerThatDiesWhenTheAssistantMessageIsStoredLeavesNoCallsWithoutRecords` |
| PostgreSQL 的一个事务（D47-6） | `AppendAssistantTurn`：消息先单独写入 | `TestTheAssistantMessageAndItsToolCallRecordsAreWrittenInOneTransactionOnPostgres` |
| 补建（D47-7） | `proceed`：不调用 `repairUnrecordedCalls` | `TestAnAssistantMessageStoredWithoutItsToolCallRecordsIsRepairedOnResume`、`TestARepairedToolCallStillWaitsForTheConfirmationTheReleaseRequires`；PostgreSQL 的补建测试 |
| 补建走确认策略（D47-7） | 补建的调用一律 `CONFIRMED` | `TestARepairedToolCallStillWaitsForTheConfirmationTheReleaseRequires` |
| 只补缺的（D47-7） | `repairUnrecordedCalls`：对已有记录的调用也补建 | 表测试、远程 A2A 测试（唯一键冲突使运行失败） |
| 读已有记录的 id（D47-7） | PostgreSQL `Context` 不填 `RecordedToolCallIDs` | PostgreSQL 的三个中断测试与一个事务测试（每个调用都被当成缺记录） |
| 停止后不开始新调用（D47-8、D47-9 边界 5） | `execute` 入口的停止检查 | `TestAStopWhileAToolCallRunsLetsItFinishStartsNothingAndHandsTheRunOver`、`TestAStopWhileAModelCallRunsStoresItsAnswerStartsNoToolCallAndHandsTheRunOver`、`TestAStopAnswersTheDeniedCallsOfTheStepAndLeavesTheOthersForTheNextWorker`、`TestAStopDoesNotHideTheConfirmationsOfTheStep` |
| 停止后不开始新的一步（边界 2） | 循环顶部的停止检查 | `TestAStopDuringTheLastCallOfAStepStartsNoNewModelCall` |
| 回合开始的检查（边界 1） | `proceed` 开头的停止检查 | `TestAStopBeforeAnythingWasDoneHandsTheRunOverAtOnce`（`tools/list` 也没有发出） |
| 模型请求失败后不重试不换候选（边界 3） | `callModel` 失败后的停止检查 | `TestAStopWhileAModelRequestFailsStartsNoRetry` |
| 重试等待随停止结束（边界 3） | `turn.sleep` 去掉停止分支 | `TestAStopDuringTheWaitBetweenModelAttemptsEndsTheWaitAndHandsTheRunOver` |
| 交出认领（D47-10） | `settle` 不调 `Release` | 上面所有"交出"的测试（6 个） |
| 确认不被停止藏起来（D47-9 边界 4） | `dispatch`：有确认时也走交出 | `TestAStopDoesNotHideTheConfirmationsOfTheStep` |
| 工具调用的状态与消息一次写入（D47-11） | `answer` 拆回 `UpdateToolCall` + `AppendMessages` | `TestAWorkerThatDiesWhenAToolCallIsAnsweredLeavesNoFinishedCallWithoutItsMessage`（先复现，后修复） |
| 没来得及回答的 `DENIED` 调用（D47-11） | `resolveDecided` 不处理 `DENIED` | `TestADeniedCallThatWasNotAnsweredIsAnsweredByTheNextWorkerWithItsReason` |
| 补建不重复回答（D47-12） | 补建不要求 `finished_at` | 同上（同一个调用得到两条工具消息） |
| 补工具消息（D47-12） | `repairLastTurn` 不补最终状态调用的消息 | `TestAFinishedToolCallWithoutItsToolMessageIsAnsweredFromItsStoredResultOnResume`、`TestEveryFinalStateOfAToolCallWithoutItsMessageIsRepaired` |
| 任何子智能体不重发（D47-5） | 本地子智能体又可重发 | `TestASubagentCallThatWasExecutingWhenTheWorkerDiedIsNotSentAgain`（本地） |
| 计数（D47-13） | 中断时不调 `ToolInterrupted` | `TestInterruptedToolCallsAreCountedByResentAndReason` |
| worker 告诉运行停止（D47-8） | `serve` 不带 `run.WithStop` | `TestAWorkerTellsTheRunInProgressToStopWithoutCancellingIt`；PostgreSQL 的 `TestAStopHandsTheRunOverAtOnceWithoutStartingAnotherCallOnPostgres` |
| PostgreSQL `Release`（D47-10） | `UPDATE` 不结束租约 | `TestAStopHandsTheRunOverAtOnceWithoutStartingAnotherCallOnPostgres`、`TestAToolCallsFinalStateAndItsToolMessageAreWrittenInOneTransactionOnPostgres` |
| PostgreSQL 的 `LastCalls`（D47-12） | `Context` 不填 | `TestAFinishedToolCallWithoutItsToolMessageIsRepairedOnResumeOnPostgres` 及两个事务测试 |
| PostgreSQL 的一次写入（D47-11） | `AnswerToolCall` 先单独写状态 | `TestAToolCallsFinalStateAndItsToolMessageAreWrittenInOneTransactionOnPostgres` |
| 宽限期（D47-8） | `Worker.Run`：`StopGrace` 不起作用（立即中断） | `TestAWorkerToldToStopLetsTheRunInProgressFinishWithinTheGracePeriod`、`TestAWorkerCutsTheRunShortWhenTheGracePeriodIsOver`；PostgreSQL 的 `TestAWorkerToldToStopLetsTheToolCallInProgressFinishOnPostgres` |
| 宽限期之后一定返回（D47-8） | 宽限期的计时器永不触发 | `TestAWorkerCutsTheRunShortWhenTheGracePeriodIsOver` |
| 停止后不再认领（D47-8） | 认领循环按宽限期的 ctx 而不是停止的 ctx 判断 | `TestAWorkerToldToStopLetsTheRunInProgressFinishWithinTheGracePeriod`（运行结束后 worker 不返回） |

`DENIED` 记录在 PostgreSQL 上、`resolveDecided` 对它的回答没有单独的 PostgreSQL 测试（内存存储测试覆盖逻辑，`Context` 把 `DENIED` 算作未解决这一处只有编译与审阅）。`cmd/idp-host` 里 `IDP_HOST_STOP_GRACE` 与 `NeverResendTools` 的装配没有自动化测试（该包没有测试文件），只做了编译与代码审阅。

**没有做的**：
- **端到端幂等键**（选择点 2）：HOST 带稳定的键（运行号加调用号）、网关转发、上游去重，要改 HOST、网关与每个上游，上游还要保存结果用于去重；对现有两个上游，D47-1 已经够用。接第三方服务时再议（有的第三方接口自己支持幂等键，那时网关可以映射过去），记在对外互通的 ADR 里。
- 没有在 compose 或 kind 上验收，没有用真实模型验证；没有在真实的 SIGTERM 下运行进程（宽限期用 `Worker.Run` 的 ctx 取消模拟）。

## 30. D48：能把内容送出企业的不只是运行自己的发布件，还有它能委派到的子 agent（2026-11-19 决策与实施，平台 ADR-0003 D7 的"已知缺口"、附录 D 任务 H2）

修订 D43 的"没有做的"第一条（本地子 agent 不查）与 D44 取舍里的"子运行只看对外工具，不看模型"，以及"任务文字带出数据的缺口仍在"。决策依据：平台 `ADR-0003-appendix-d-phase2-plan.md` 第 4.2 节、选择点 5（取 (a)）。

起因：一个没有对外工具的 agent（用企业自有模型）拿到机密的 CMI 条目和高于上限的附件。它的发布件引用了一个有联网搜索的本地子 agent，模型调用 `agent__<名字>`，委派的任务是它自己写的文字。D44-10 只管子运行**继承**什么，不管父运行**写**什么：父 agent 读了机密条目，把"并购方案里写的 X 公司……"写进任务，子 agent 就拿它去搜。远程 A2A 子 agent 更直接：任务文字离开 HOST。任务文字是自由文本，在委派时筛不了。能做到的是让父运行从头就看不到高于上限的东西。

**D48 之前，哪些情形已经触发筛选**（附件与 CMI 条目共用一个触发：`FilePolicy.trigger`，`attachments.go:71`，由 `createRun` 在 `api.go:603`、`606` 用于附件，在 `api.go:620`（写入 input）与 `721`（`resolveContext`）用于 CMI）：

| 发布件能把内容送到… | 附件 | CMI 条目 | 依据（D48 之前的代码与测试） |
|---|---|---|---|
| 外部模型（自己的 `model.provider`） | 筛 | 筛 | `externalModels`（`attachments.go:32`）；`TestFilesAboveTheCeilingAreWithheldFromExternalModels`、`TestResolveContextScreensCMIEntriesOfAnExternalRun`、集成 `cmi-above-the-ceiling` 步骤 1 |
| 回退、路由候选 | 筛 | 筛 | `attachments.go:41`、`44-45`；同上测试里的两个子例（只测了附件，CMI 走同一个触发） |
| 对外工具 | 筛 | 筛 | `hasExternalTools`（`attachments.go:65`）；`TestTheTriggerIsAnExternalModelOrAnExternalTool`、`TestFilesAboveTheCeilingAreWithheldFromRunsWithExternalTools`、集成步骤 3 |
| 远程 A2A 子 agent | 筛 | 筛 | `externalModels` 把它记为 `a2a:<cardUrl>`（`attachments.go:50-51`），所以触发为 `external-model`；附件有测试（`attachments_test.go:49`），CMI 没有单独的测试，是同一个触发 |
| **本地子 agent，其发布件用外部模型** | **不筛** | **不筛** | `externalModels` 只看 `doc.Definition.Subagents` 里的 `a2a`；D43 "没有做的"第一条 |
| **本地子 agent，其发布件有对外工具** | **不筛** | **不筛**（只有子运行继承时的 D44-10） | `hasExternalTools` 只看自己的 `doc.Effective().Tools`；D44-10 在 `CreateChildRun` 里 |
| **本地子 agent 的子 agent（任意层）** | **不筛** | **不筛** | 同上 |

所以缺的格子是后三行（各两格）。ADR 里 D43 的验证写到"加远端 A2A 子 agent"，代码与测试都支持；"本地子 agent 不查"写在 D43 的"没有做的"里，也正是这里补上的。

| # | 决策 | 理由 |
|---|---|---|
| D48-1 | **创建运行时，这个运行算"可能把内容送出企业"，当且仅当：发布件自己符合 D43/D44 的条件，或它能委派到的某个本地子 agent 符合（递归：子 agent 的子 agent），或它引用了远程 A2A 子 agent，或有本地子 agent 无法读取（算对外）。** 判断仍然只在一处：`FilePolicy.outward`（`internal/api/attachments.go`），返回 `Outward{Trigger, Models, Tools, Via}`；`createRun` 算一次，附件筛选（`screen`）、`input.ContextCeiling/ContextTrigger/ContextVia/ExternalTools`、CMI 筛选（`resolveContext`）、事件全部用它的结果。触发多一个值 `subagent`（可与 `external-model`、`external-tools` 用 `+` 连接） | 父运行看不到高于上限的东西，就写不进任务。不在委派时查任务文字：自由文本筛不了 |
| D48-2 | **发布件对子 agent 只嵌入身份（名字、`agentId`、版本、摘要，D22），不嵌入它的模型、工具允许列表和它自己的子 agent。** 所以创建运行时按摘要读取被引用的发布件：`Store.GetReleaseByDigest(tenant, agentId, digest)`，一次主键内的查询，只读 `agent_releases` 的 envelope，状态 `APPROVED` 或 `SUPERSEDED`（与 `CreateChildRun` 能运行的发布件一致）。读取在 `FilePolicy.Release` 上注入，缺省接到存储；每个运行一次，没有子 agent 引用的发布件不读 | 发布件是不可变、按摘要寻址的，所以读到的就是委派时会运行的那一份；没有往发布件里再嵌一份摘要，不改契约、不改已发布的发布件 |
| D48-3 | **读不到就算对外（fail closed）**：没有注入读取函数、发布件不存在、已不是可运行状态、envelope 为空或损坏，都按"该子 agent 对外"处理，事件里写 `its release cannot be read` | 与 D44 对未知级别的处理一致：不知道，就当会出去 |
| D48-4 | **递归按 D22 的规则走，并有界。** D22：级别 d 的运行只有在 d < 它自己发布件的 `limits.maxSubagentDepth`（缺省 1，最大 5）时才有 `agent__*` 工具。所以走到的深度恰是运行能到达的深度，不是一个固定的层数：限制使某个子 agent 不可达时不计入（不会无故扣数据）。三道界：(1) 每个发布件的限制 ≤ 5（`Validate`），走法另设硬上限 `agent.MaxSubagentDepth`，超过的链按"无法判断"算对外；(2) 同一个（摘要，深度）只读一次，环（A 引用 B 引用 A，D22 在编译期按名字与 agentId 拒绝自引用，不同版本间的环不会出现，但存储里可能有）因此终止；(3) 一次判断最多读 `maxSubagentLoads`（64）个发布件，超过按"无法判断"算对外 | 终止与有界不依赖数据是否干净 |
| D48-5 | **事件与告诉模型的话保持 D44 的形状，只增加原因。** `idp.host/attachment.withheld` 与 `idp.host/context.withheld` 在原因来自子 agent 或远程 agent 时多一个 `via`：字符串列表，每项一个原因，指名到子 agent 的路径（`web`、`research/web`）：`sub-agent web: may call an external tool`、`sub-agent ask: may use an external model (cloud)`、`remote agent partner: the task text leaves the HOST`、`sub-agent ghost: its release cannot be read, so it counts as sending content out`。`trigger` 含 `subagent`。模型读到的说明不变（只有条数、文件名与级别）。`via` 只有名字与固定的原因，没有任何被扣下内容。运维因此能回答"这个 agent 为什么读不到机密记忆" | 事件是运维的审计面（D44-7） |
| D48-6 | **刷新照筛，原因随运行记下。** `run.Input` 增 `contextVia`（不属于契约），与 `contextCeiling`、`contextTrigger` 同时在创建时写入；`verifyContext` 刷新时用它们，`idp.host/context.withheld` 带同样的 `via`。子运行继承父运行的 `contextCeiling`、`contextTrigger`、`contextVia`，所以父运行被筛过，子运行拿到的就是筛过的 | D44-5：运行中途改配置不改变已创建运行的规则 |
| D48-7 | **D44-10 保留，作为第二道。** 父运行创建后、委派前，运维把工具加进 `externalTools`（或父运行是在 D48 之前创建的），父运行的 context 没有筛过，子运行的发布件有对外工具时，继承来的 CMI 条目仍然全部去掉。D48 之后这是配置在运行期间改变的情形，不是常规路径 | 两道各自成立，互不依赖 |

**决定在创建运行时做，不冻结进发布件。** 与 D44 对 `toolResults` 的理由一致：发布件不可变，而 `externalTools` 与 provider 的 `external` 是 HOST 配置，会变。冻结在编译时，一个今天无害的子 agent 明天因运维把它的工具列为对外而变得有害，发布件里的旧结论就成了漏洞；反过来，运维取消列出之后发布件还在扣数据，只有重新发布才能解开。运行创建时按当时的配置算，运行记下结果（`contextCeiling`、`contextTrigger`、`contextVia`），之后不变。代价是每次创建带子 agent 的运行多读几行 `agent_releases`（每个子 agent 一次，已按摘要定位），没有子 agent 引用的运行没有额外开销。

**取舍与代价**：
- **引用了能联网的子 agent 的 agent，自己再也读不到高于上限的机密数据。** 这是这个决定的本意，也是它的代价：一个"先读内部资料再决定搜什么"的 agent，一旦挂上联网子 agent，就读不到机密条目与附件了。运维有三个办法：(1) 把联网子 agent 拆出去，让需要机密数据的 agent 不引用它，让用户在两个 agent 之间自己传递（用户自己写进对话的内容不在这个控制之内，见"没有做的"）；(2) 把租户的 `files.tenants` 上限调高到该级别，代价是这个租户所有对外运行都能看到这一级；(3) 不再把工具列入 `externalTools`（只在确实不会把内容送出企业时）。事件里的 `via` 指明是哪个子 agent 造成的，所以第一步总是知道该拆谁。
- **宁可多扣。** 读不到、深度超界、读取过多一律算对外；子 agent 的 `*` 允许列表算对外（D44-2）。
- **不看子 agent 会不会真的被调用。** 引用即算，因为模型何时调用创建时不知道。
- **`toolExecution: caller` 的运行照样算**（D44 同样处理）：调用方模式没有 `agent__*` 工具，多扣的代价小于漏判。
- **契约不变。** 事件字段不属于 schema（同 D44、D45）；`run.Input` 不属于契约。

**实施**：`internal/api/attachments.go`（`Outward`、`outward`、`screen` 改为接受 `Outward`、`withheldEvents`、`FilePolicy.Release`）；`internal/api/subagents.go`（`viaSubagents`，有界的递归）；`internal/api/api.go`（`createRun` 只算一次）；`adapters/store/postgres/agents.go`（`GetReleaseByDigest`）；`adapters/store/postgres/runs.go`（子运行继承 `contextVia`）；`internal/run`（`Input.ContextVia`）；`internal/cmiclient/screen.go`（`Screening.Event` 带 `via`）；`internal/harness`（刷新事件带 `via`）；`internal/agui`（`ScreenSubagent`）。没有改配置项与 README 的设置段：没有新设置；README 的 `externalTools` 与 `files` 的说明加了一句。

**验证**：`internal/api` 的 `TestARunIsOutwardWhenAReachableSubagentOrARemoteAgentIs`（无子 agent、对照：自有模型且无对外工具的子 agent、对外工具、外部模型、两层、三层、D22 使之不可达的第三层、远程 agent、两层之下的远程 agent、读不到、自己的对外工具加子 agent、运维不再列出该工具、没有读取函数）、`TestTheWalkOverSubagentsTerminatesAndIsBounded`（环、超过 D22 最大深度的链、很宽的树的读取数）、`TestFilesAboveTheCeilingAreWithheldFromAParentWhoseSubagentIsOutward`（附件与 `via`，对照）、`TestCMIEntriesAreScreenedForAParentWhoseSubagentIsOutward`；`internal/harness` 的 `TestRefreshedContextOfAParentWithAnOutwardSubagentIsScreenedAndSaysWhy`；`TestHostEndToEnd/cmi-above-the-ceiling`（PostgreSQL，步骤 4 改为 D44-10 作为第二道，步骤 5–9 是 D48：父 agent 没有对外工具、子 agent 有，附件与 CMI 条目，断言父运行与子运行的每一次模型请求都不含被扣下的条目与票据，存下的 input 里没有被扣下的文件，事件带 `via` 并指名子 agent，同一请求重放得到同一个运行；对照：子 agent 用自有模型且没有对外工具，父运行与子运行都拿到所有数据；运维把工具移出 `externalTools` 后同一个（不可变的）发布件重新拿到数据；子 agent 的发布件读不到；远程 A2A 子 agent）。D44 步骤 4 原先断言的"父 agent 没有对外工具就拿到所有条目"正是这个缺口，现在不再成立，已改写。

**拿掉一个控制、看测试失败**

| 控制 | 拿掉的东西 | 失败的测试 |
|---|---|---|
| 整个子 agent 判断（D48-1） | `viaSubagents` 不走 | 集成 `cmi-above-the-ceiling`（父运行的模型请求里出现 `BODY-DEAL`）；`TestFilesAboveTheCeilingAreWithheldFromAParentWhoseSubagentIsOutward` 与矩阵里所有 `subagent` 的子例 |
| 递归（D48-4） | 只看直接子 agent，不进入它们的子 agent | `TestARunIsOutwardWhenAReachableSubagentOrARemoteAgentIs` 的"两层""三层""两层之下的远程 agent"，`TestTheWalkOverSubagentsTerminatesAndIsBounded` 的超深度链与宽树 |
| 远程 agent（D48-1、D48-5） | 不记 `remote agent` 的原因 | 矩阵里两个远程子例；集成步骤 9（`via` 里没有 `remote agent xpeer`） |
| 读不到算对外（D48-3） | 读取失败或没有读取函数时当作不对外 | 矩阵里"读不到"与"没有读取函数"；集成步骤 8 |
| 事件里的原因（D48-5） | 附件事件不写 `via` | `TestFilesAboveTheCeilingAreWithheldFromAParentWhoseSubagentIsOutward` |
| 事件里的原因，CMI 与刷新（D48-5、D48-6） | `Screening.Event` 不写 `via` | `TestCMIEntriesAreScreenedForAParentWhoseSubagentIsOutward`、`TestRefreshedContextOfAParentWithAnOutwardSubagentIsScreenedAndSaysWhy` |
| 原因随运行记下（D48-6） | `createRun` 不把 `outw.Via` 写进 input | 集成步骤 5（context 事件没有 `via`） |

**没有做的**：
- **沙箱工作区跨运行存在**：同一个成员在同一个线程里先用不对外的 agent 导入了机密文件，30 分钟内再让一个对外的 agent 接着用同一个工作区，文件还在（D43 记过，没有变）。
- **成员自己粘贴进对话的文字**：用户把机密内容写进消息，它就在运行的输入里，任何筛选都看不见；D48 只管 HOST 从文件与 CMI 取来的东西。
- 没有界面：被扣下的原因只在事件流里。
- 平台 compose 的验收（PATH N 之后的一条）不在 HOST 仓库内做，见交付说明。

## 31. D49：运行记下它的工具返回的网页来源，结束时核对回答里的引用编号（2026-11-19 决策与实施，契约 1.11.0，平台 ADR-0003 D9、附录 D 任务 H1 与 K1）

决策依据：平台 `ADR-0003-appendix-d-phase2-plan.md` 第 4.1 节、第 2 节第 1、2 条、选择点 4（取 (a)：只标记，不拦截）、第 9 节对存储与契约版本的说明；平台 `ADR-0003-web-search-and-fetch.md` D9 与第 11 节的两条风险（"完整性风险无法消除"、"模型可能虚构引用"）。

起因：idp-web 在工具结果的 `structuredContent.citations` 里给出 `{n, title, url, fetchedAt, sha256}`，D44 把这类工具记为 `externalTools`，但 HOST 只在沙箱的文件交付上读 `structuredContent`，运行记录里没有来源，成员只能相信模型写的 `[n]`。D49 让 HOST 从**本次运行自己的**工具结果里收集来源，运行结束时把回答里的 `[n]` 与来源对上：存在的标记为已引用，不存在的列出来。

| # | 决策 | 理由 |
|---|---|---|
| D49-1 | **收集：只读匹配本网关 `externalTools` 的工具的成功结果。** `execute` 在工具返回后读 `structuredContent.citations`（`run.ParseCitations`），结果交给存储，与该调用的最终状态、它的 tool 消息写在同一个事务里（`Store.AnswerToolCall` 多一个参数 `*run.SourceBatch`）。不匹配的工具（例如沙箱）即使返回同样的结构也不读；失败的结果不读 | 来源是"送出企业再带回来的外部内容"的记录；不是对外工具的结果，来源的含义不成立。写在同一事务里，所以一次调用只记一次：worker 在调用中途停止时，该调用的结果和来源都没有写，接手的 worker 重发（D47 允许只读工具）后记一次；调用已回答后停止则不会重发 |
| D49-2 | **校验，逐条；无效的条目丢弃并计数，不是运行的错误。** `n`：JSON 整数，1 到 1 000 000（idp-web 按调用者一直递增，不会回绕，所以上限取大）；`url`：字符串，不超过 2048 字节，能被解析，scheme 为 http 或 https（不分大小写），有主机，没有用户信息（`user:pw@`），不含空白与控制字符；`title`：必须是字符串，换行与制表符变空格，其余控制字符和格式字符（含双向文字覆盖符）去掉，空白折叠，超过 300 个字符截断并以省略号结尾（空字符串可以）；`fetchedAt`：有则必须是 RFC 3339，存为 UTC；`sha256`：有则必须是 64 位十六进制，存为小写；空串与 null 等于没有；对象里的其他字段（包括任何摘录）一律忽略，不存。`citations` 不是数组、结果没有 `structuredContent`：什么也不加，也不算无效条目。一个结果最多读前 100 条，其余计入"超过上限" | 模型读到的文字（工具的 `text`）不受这些影响，所以一个格式错的 `citations` 不会让运行失败，也不会改变模型看到的东西 |
| D49-3 | **合并：同一个 `n` 再出现，保留第一次的地址与第一个非空的标题；`fetchedAt` 取最早的；已有的 `sha256` 不被替换（没有时第一个出现的补上）。** 同一个 `n` 带**不同**的地址：保留第一个，计一次"冲突"，其余字段一概不取。每次运行最多 100 条来源：新的编号超出时不记，计入"超过上限"；已有的编号仍可合并。列表按 `n` 升序，`n` 在一次运行内唯一但不一定连续（idp-web 的 `n` 按调用者，同一成员同时跑两次运行时编号互相穿插） | 合并规则是幂等的：同一批再合并一次，除计数外什么也不变 |
| D49-4 | **`sha256` 的含义写死：它是"第一次读到这个来源时交给模型的那一段正文"的摘要，不是整页的摘要。** idp-web 的 `web_fetch` 对一页分段读取（`offset`），每一段的引用各有一个 `sha256`；同一个 `n` 读了几段，这里只留最先记录的那一个。所以它能证明"模型读到过这样一段"，不能用来校验整页 | 为每段留一个摘要要把来源变成列表的列表；成员核对的是"页面在哪里"，不是字节 |
| D49-5 | **冲突的已知来源：idp-web 为请求的地址编号，却在引用里写重定向之后的最终地址**（`citation.URL = c.page.FinalURL`，`handlers.go`），所以"先搜索得到 A，再读取时 A 重定向到 A'"会在同一个 `n` 下带来两个地址，被计为冲突。保留第一个（搜索给出的、成员可以打开的地址）。**冲突计数因此在有重定向的运行里不为零，它不是错误**；计数只用来发现编号体系本身出了问题（同一个 `n` 指向两个毫不相干的页面）。事件里只有次数，不带地址 | 平台计划写的"不该发生"在重定向时会发生；要让计数干净，要么 idp-web 的引用同时带请求地址与最终地址（平台仓库的改动，没有做），要么 HOST 把重定向的地址视为同一来源（HOST 无从判断，没有做） |
| D49-6 | **本地子 agent 的来源也记入父运行，子运行自己的列表仍归子运行。** 子 agent 的结果回到父运行的方式是它最后一条助手消息的文字（`ChildOutcome.Output`），这段文字里可以有 `[n]`；父运行的回答可以原样引用它们。子运行与父运行是同一个调用者（凭据继承），所以编号在同一个编号体系里，同一个 `n` 就是同一个来源。规则：父运行收集子运行的结果时（`collect`），把子运行的**来源列表**（不带计数，不带 `cited`）按 D49-3 合并进自己的，与那个 DELEGATED 调用的回答在同一个事务里。子运行已完成才合并；失败、取消的子运行没有答案文字，不合并。所以"回答里的每个 `[n]` 都能在**这次运行**的来源里查到"仍然成立。远程 A2A 子 agent 没有来源：它回的文字里的编号父运行查不到，会被列为无法核对，这是对的 | 最小的规则：不新增查询、不改子运行的数据；子运行的丢弃与冲突计数是子运行自己的事 |
| D49-7 | **标题只是数据，从不进入任何模型请求。** 来源只写进数据库、运行的表示和 `run.sources` 事件；`Store.Context` 不读它，`guardedMessages`、`filesNote`、预算说明、系统与 developer 文本、任务文字、子 agent 的输入、恢复后的请求、摘要都不碰它（没有代码路径把它读回 harness）。**要说清的一点：模型仍然读到工具自己的 `text`，而 idp-web 的 `text` 里就有标题**（`[n] 标题`），经 D27/D44 包上"不可信的外部内容"的标记；D49 没有改变这一点，也没有增加新的通道——记下来的列表是另一份，不被读回。测试里的网关把"注入的标题"只放在结构化结果里，其 `text` 用另一个标题，以便证明那份记录没有被读回 | 平台计划写的"标题绝不进入给模型的上下文"在字面上做不到（工具文本里本来就有），能保证的是"记下来的列表不进入" |
| D49-8 | **运行完成时核对，只标记，不拦截、不改回答。** `Store.CheckSources`（在 `Finish(COMPLETED)` 之前，同一事务里读来源、核对、存回、追加事件）：回答里写了、并且在来源里的编号，该来源 `cited = true`；回答里写了、不在来源里的编号，进入 `unverifiedCitations`。**没有对外工具（`Input.ExternalTools` 为假）并且没有来源的运行，整个检查跳过**：回答里的方括号数字不是引用，没有 `unverifiedCitations`、没有事件。有对外工具但什么也没读到、回答里却写了 `[3]`，正是这项检查要发现的情形。检查只在 COMPLETED 时做；失败、取消的运行保留已收集的来源（`cited` 全为假），没有 `run.sources` 事件 | 选择点 4：编号写错不等于内容有害，拦截会让成员拿不到答案 |
| D49-9 | **事件 `idp.host/run.sources`：运行结束时一次，在 `RUN_FINISHED` 之前**，值为 `{sources, unverifiedCitations, droppedInvalid, droppedOverLimit, conflicts, checked}`，`sources` 是完整列表。检查做过且有话可说（有来源、有无法核对的编号、或有丢弃）才写。**运行期间没有增量事件**：来源在回答出来之前没有"已引用"的含义，Workspace 与别的消费者在运行结束时才取（它跟随事件流到 `RUN_FINISHED`），增量事件要为每次工具调用多写一条，换不到任何调用方现在需要的东西。需要时可以加，契约不受影响（事件不属于 schema） | |

**引用标记的语法**（`run.CitationMarks`）。依据：平台的技能文本只要求"cite each source as [n]"（`deploy/compose/e2e.sh` 里 web-assistant 的说明；idp-web 的 README 只说明 `citations` 的字段）。取保守的最小语法：

| 是引用 | 不是引用（不记已引用，也不报无法核对） |
|---|---|
| `[3]`；`[1][2]`（两个标记）；`[1, 2]`、`[1,2]`（逗号分隔的列表，每个数字不带前导零、至多六位）；紧跟中文或标点的 `营业额[1]。`；行首的 `[2] 第二点`、`[1]: https://…` | `[x]`、`[N/A]`、`[TODO]`、`[ ]`、`[]`；`[2026]`（年份，见下）；`[0]`、`[01]`；范围 `[1-3]`、`[1–3]`（不展开，也不当作引用）；Markdown 链接 `[3](https://…)`、引用式链接 `[文字][1]`；图片；行内代码和围栏代码块（反引号或波浪线，不闭合的块到文末）里的一切；全角括号 `【1】`；`[1 见上]`、`[1;2]` |

还有三条限制，写在代码与测试里：
- **粘在 ASCII 字母、数字、下划线后面的标记**（`arr[7]`、`x_2[5]`）读作下标：只有当它指向已知来源时才记为已引用，永不报为无法核对。
- **不存在的编号只在 1 到 3 位数时才报**；更长的数字（`[123456]`）不报，因为像年份和别的数字。已知来源的编号不受这条限制（idp-web 按调用者递增，会超过 999）。
- **语法的盲区**：把数组写成 `[1, 2]` 的回答会被当作引用，没有来源也没有对外工具的运行不检查，有来源的运行可能多报一个；`【1】` 或 `^1` 之类别的写法不认，没认出来就既不标已引用也不报，是漏报方向的错误，不会冤枉成员。

**这项检查发现不了什么：** 来源存在、但并不支持那句话的引用；回答里完全没有标记的陈述；模型改写了页面内容；引用与模型读到的段落不是同一段（见 D49-4）。它只发现"这个编号根本不存在"。ADR-0003 第 11 节写明后者靠引用精确率的测量和成员核对。

**存储。** 迁移 `0010_run_sources.sql`：表 `run_sources(tenant_id, run_id, doc jsonb, updated_at)`，主键 `(tenant_id, run_id)`，外键到 `runs`，按租户的行级安全（`FORCE ROW LEVEL SECURITY` 与 `tenant_rows` 策略，同 `evaluations` 等表），`idp_host_app` 有 SELECT、INSERT、UPDATE。**为什么是一张表而不是 `runs` 的一列：**列出一个线程的运行（最多 500 行）读很多行，而来源最长有 100 条（最坏约 250 KiB 一行）；`GetRun`、`ListRuns`、`ListChildRuns`、幂等重放用一条 `run_id = ANY(...)` 查询补上。`doc` 是 `{items, droppedInvalid, droppedOverLimit, conflicts, checked, unverified}`，`items` 是 `run.Source` 的列表。**合并在 `owned` 取得的运行行锁下读、合并、写**，所以同一次运行的两个写者（不会同时有，但 fencing 之外多一层保证）不会互相覆盖。接手的 worker 从数据库读到前一个 worker 已经写下的来源，来源因此跨 worker 交接与确认后的恢复保留。

**供应商条款。** 只保存**标题与地址**（以及读取时间与摘要）；**搜索摘录（`excerpt`）和页面正文都不存**——不进数据库，不进事件。这与平台附录 D 第 9 节的要求一致（"摘录不存"）。**标题能否保存仍要按合同核实**（附录 B 的问题清单里已有这一条）：在核实之前，运营方可以不开启 `externalTools`（来源就不会记），HOST 没有单独的开关，因为来源的存在本身是 D9 的要求；如果合同不允许存标题，需要的是一个小改动：存 `title` 时留空，列表仍有地址。

**契约 `idp.telos.host` 1.11.0（向后兼容的小版本）：** `AgentRun` 增可选的 `sources`（最多 100 项，`RunSource`：`n`、`title`、`url`，可选 `fetchedAt`、`sha256`，`cited`）与 `unverifiedCitations`（整数，1 到 999，唯一）；新增定义 `RunSource`。示例增一个带来源与无法核对编号的已完成运行。事件名与字段不属于 schema（同 D44、D45）。生产者一致性测试：`internal/run` 的 `TestContract*`（定义的字段与 Go 结构一致，示例由 Go 值生成并往返，另有 `TestContractRejectsRunSourcesTheHostWouldNotRecord`：n 为 0 或字符串、ftp 与 javascript 地址、含空格的地址、大写与过短的 sha256、不是时间的 `fetchedAt`、过长标题、缺 `cited`、多余字段（摘录）、101 项、`unverifiedCitations` 里的 0、年份、重复、文字）。**对消费者：**`AgentRun` 对生产者是封闭的（`additionalProperties: false`），所以按 1.10.0 的 schema 严格校验 HOST 返回的运行的消费者会拒绝带 `sources` 的运行；idp-workspace 不这样做（见交付说明），它读 `GET /v1/runs/{id}` 时只取 `deadlineAt`，跟随事件流时忽略不认识的自定义事件，所以旧 Workspace 容忍新字段与新事件。**升级顺序：先 HOST，后 Workspace。**

**实施：**`internal/run/sources.go`（`Source`、`SourceBatch`、`SourceSet`、`ParseCitations`、`Merge`、`CitationMarks`、`CheckAtEnd`）；`internal/run/run.go`（`Run.Sources`、`Run.UnverifiedCitations`、`ChildOutcome.Sources`）；`internal/harness/harness.go`（`execute` 读 citations，`collect` 合并子运行的来源，`complete` 调 `CheckSources`；Store 接口的 `AnswerToolCall` 多一个参数，新增 `CheckSources`）；`adapters/store/postgres/run_sources.go` 与 `runs.go`；`migrations/0010_run_sources.sql`；`internal/agui`（`CustomRunSources`）；`api/contracts`、`api/openapi.yaml`；`tests/fixtures/fakemcp`（`Tool.HandleAs`：按调用者回答，idp-web 的来源编号按调用者）。没有新设置，没有新依赖。

**验证：**
- `internal/run`：`TestACitationIsRecordedOnlyWhenItsFieldsAreValid`（每一种无效，含 28 个反例）、`TestATitleIsCleanedAndCut`、`TestSourcesAreMergedByNumber`、`TestTheSourceListHoldsAtMostAHundred`、`TestAChildsBatchCarriesNoCountsAndNoCitedMarks`、`TestCitationMarksAreFoundByTheGrammarAndNothingElseIsReported`（两个方向，25 个例子）、`TestTheCheckMarksCitedSourcesAndSkipsARunWithNothingToCheck`。
- `internal/harness`（模型替身与网关替身，后者像 idp-web 一样按调用者编号）：`TestARunsSourcesAreExactlyThoseOfItsOwnToolResultsWhileOtherRunsOverlap`（三个运行同时在网关里，同一成员两个、另一成员一个，读的页面部分重叠，每个列表恰好是自己的）、`TestOnlyTheResultsOfExternalToolsAreReadForCitations`、`TestARunThatSearchedAndReadHasItsSourcesCitedAndUnverifiedMarked`、`TestInvalidCitationsAreDroppedAndCountedAndTheRunGoesOn`、`TestAtMostAHundredSourcesAreRecordedAndTheEventSaysHowManyWereDropped`、`TestANumberGivenAgainWithAnotherAddressKeepsTheFirstAndCountsAConflict`、`TestATitleThatIsAnInjectionIsStoredAndNeverReachesAModelRequest`（记录每个模型请求的原始请求体，含工具说明；经历等确认后恢复与子 agent 的来源合并）、`TestARunWithoutOutwardToolsAndWithoutSourcesIsNotChecked`（两个方向）、`TestSourcesSurviveAHandOverAndARepeatedReadOnlyCallIsRecordedOnce`、`TestAWorkerThatDiesAtTheEndOfTheRunLosesNoSources`、`TestTheSourcesOfASubagentAreAddedToTheParentsList`。
- PostgreSQL（`TestRunSourcesOnPostgres`）：三个运行并发、持久化、API 的表示（`GET /v1/runs/{id}`、线程的运行列表、子运行列表）、事件在 `RUN_FINISHED` 之前恰一次、没有任何标题进入模型请求、租户隔离（另一租户的 `GET` 为 404，在另一租户的事务里 `run_sources` 一行也看不到，写别的租户的行被拒）、存下的文档里没有页面正文、子 agent、worker 在只读调用中死去后由另一个 worker 接手（先前的来源还在，重发的读取只记一次，事件说明重发）、无对外工具无来源的运行不检查。

**拿掉一个控制、看测试失败**（每一项在实现里拿掉一次，跑下列测试，失败后恢复）

| 控制 | 拿掉的东西 | 失败的测试 |
|---|---|---|
| 来源按运行隔离（D49-1、D49-3） | harness 里改成按所有者累积（模拟"按调用者"的做法），把累积的列表交给存储 | `TestARunsSourcesAreExactlyThoseOfItsOwnToolResultsWhileOtherRunsOverlap`；`TestRunSourcesOnPostgres` 的并发运行、事件、子 agent、交接子例 |
| 只读对外工具的结果（D49-1） | `agent.MatchesTool(ExternalTools, …)` 改为恒真 | `TestOnlyTheResultsOfExternalToolsAreReadForCitations` |
| 来源的合并按编号（D49-3） | 去掉"编号已有"的分支 | `TestSourcesAreMergedByNumber`、`TestTheSourceListHoldsAtMostAHundred`，harness 里五个运行级测试（重复的只读调用因此重复记录） |
| 100 条上限 | 去掉上限判断 | `TestTheSourceListHoldsAtMostAHundred`、`TestAtMostAHundredSourcesAreRecordedAndTheEventSaysHowManyWereDropped` |
| 冲突计数 | 不计 | `TestSourcesAreMergedByNumber`、`TestANumberGivenAgainWithAnotherAddressKeepsTheFirstAndCountsAConflict` |
| 最早的 `fetchedAt`、不替换 `sha256`（D49-3、D49-4） | 各去掉一次 | `TestSourcesAreMergedByNumber` |
| 校验 | 不查 scheme；不去控制字符 | `TestACitationIsRecordedOnlyWhenItsFieldsAreValid`；`TestATitleIsCleanedAndCut` |
| 标题不进模型（D49-7） | 把来源的标题接到工具文本后面给模型 | `TestATitleThatIsAnInjectionIsStoredAndNeverReachesAModelRequest` |
| 子 agent 的来源并入父运行（D49-6） | `collect` 不传 `out.Sources`；存储的 `ChildOutcome` 不带来源 | `TestTheSourcesOfASubagentAreAddedToTheParentsList`、`TestRunSourcesOnPostgres` 的子 agent 子例 |
| 没有对外工具也没有来源就不检查（D49-8） | 去掉跳过的条件 | `TestTheCheckMarksCitedSourcesAndSkipsARunWithNothingToCheck`、`TestARunWithoutOutwardToolsAndWithoutSourcesIsNotChecked` |
| 无法核对的编号（D49-8） | 不报；放宽位数限制（年份被报）；链接、引用式链接、代码、粘连的下标各去掉一次 | 语法表里对应的例子 |
| 来源随交接保留（D49-1） | 存储合并时不先读已有的来源 | `TestRunSourcesOnPostgres` 的并发、事件、子 agent、交接子例 |
| 行级安全 | 迁移里去掉 `run_sources` 的 RLS | `TestRunSourcesOnPostgres` 的租户子例 |
| 运行的表示带来源 | `GetRun` 不补来源 | `TestRunSourcesOnPostgres` 的并发子例 |
| 事件 | 不追加 `run.sources` | `TestRunSourcesOnPostgres` 的事件与交接子例 |
| 契约 | schema 的 `unverifiedCitations` 改名；允许 ftp 地址 | `TestContractFieldsMatchGo`、`TestContractExamplesAreGeneratedAndValid`；`TestContractRejectsRunSourcesTheHostWouldNotRecord` |

**没有做的：**
- **"来源存在却不支持那句话"：**发现不了，见上。
- **回答没有标记的陈述：**不查。
- **不拦截：**`unverifiedCitations` 只是标记（选择点 4）。拦截要另议，并要有误报率的数据。
- **失败、取消的运行没有 `run.sources` 事件与检查：**来源仍在运行的表示里；需要时是小改动。
- **idp-web 的最终地址与请求地址不一致造成的冲突计数（D49-5）：**要改 idp-web 的引用或另议。
- **标题的合同核实：**见"供应商条款"。
- **Workspace 与平台：**收件箱详情的展示（WS1）、契约副本进 `idp-contracts`（K1）、compose 与 kind 的验收（P3）不在本仓库内做，见交付说明。
- **每次运行的调用上限、配额：**与本决定无关（计划第 2 节第 4 条）。

## 32. D50：流里的错误事件是提供者的错误，不是解码失败（2026-11-19 决策与实施）

修订 D45（哪些错误重试）；沿用 D40（提供者的正文不交给成员）。

起因：2026-11-19 基线测量里，同一个任务的两次运行，其中一次在第 9 次模型调用、180.4 秒后以 `MODEL_ERROR: model call failed: decode` 失败，步错误码 `decode`，0 token；前 8 次调用正常（最长 99 秒、14,000 个输出 token）。原因不明：HOST 没有留下任何消息。OpenAI 兼容适配器的 `flush` 对两种情况都返回 `decode`：数据块不是 JSON，以及数据块是 JSON 且带 `error` 对象，即提供者在一个以 HTTP 200 开始的流里报告失败，后一种几乎肯定是这次的情形，提供者自己的话被丢掉了。Anthropic 适配器（`event: error`）与 Gemini 适配器（`error` 对象）有同样的写法。

| # | 决策 | 理由 |
|---|---|---|
| D50-1 | **流里的 `error` 事件是提供者的错误，有自己的错误码。** OpenAI 兼容 `{"error":{message,type,code}}`（code 可为字符串或数字）、Anthropic `{"type":"error","error":{type,message}}`、Gemini `{"error":{code,message,status}}`，由 `model.ClassifyStreamError` 一处解析。OpenAI 适配器里服务器忽略 `stream:true` 而返回的 JSON 正文同样处理。harness-worker 的 `error` 事件原来就有自己的码，不变 | 同一个 `decode` 下是两件事：对方说了什么，和我们读不懂；运维要能分开 |
| D50-2 | **类别与重试。** `provider_overloaded`（`overloaded`、`unavailable`、503、529）、`provider_rate_limit`（`rate_limit`、429、`RESOURCE_EXHAUSTED`）、`provider_server`（`server_error`、`api_error`、`internal`、超时、5xx）可重试；`provider_error`（其余，包括 `insufficient_quota`、认证、`invalid_request_error`）不重试。类别只看提供者的 type、code、status，不看自由文字（自由文字只用来判断上下文窗口）。可重试的错误走 D45 的原规则，没有新规则：`streamer.streamed()` 为真（已有文字增量写进事件日志）就不重试同一候选，之前只有推理或工具调用片段的流照旧可以重试 | 一次重试比一次失败的 180 秒便宜；已写进日志的文字收不回来（D45-3） |
| D50-3 | **上下文窗口。** type、code 或消息里出现 D40 认得的标记（`context_length_exceeded`、`exceed_context_size_error`、`exceeds the context` 等），错误码是 `context_length`，运行的失败消息是 D40 的那句。标记表增加 Anthropic 的 `prompt is too long`，对 HTTP 400 同样生效 | 不是故障而是会话的状态，重试没有用 |
| D50-4 | **提供者的话怎么存、给谁看：只给运维，只在 HOST 日志。** `model.Error` 增 `Detail`：提供者的 type、code、status、message，每个字段至多 160 字符、合起来至多 `MaxDetail` = 300 字符，控制字符与不可打印字符换成空格、连续空格合并、非法 UTF-8 丢弃，超长以 `…` 结尾。`Detail` 只写进 HOST 日志的一行 Warn `model call failed`，带 `run`、`step`、`attempt`、`provider`、`model`、`code`、`detail`，运维凭运行 ID 即可找到；每次失败的尝试一行，大小有上界。事件 `idp.host/model.error {step, provider, model, attempt, code}` 每次失败的尝试一条，**没有任何提供者的文字**，只说"这一步有一次提供者报告的错误或读不懂的数据块"。运行的失败消息、步表、会话消息都不带它：成员看到固定的一句 `model call failed: provider_error: the model server reported an error while answering`（错误码因类别而异）。步表没有详情列，不为此改 schema | 事件流是运行所有者读的。提供者的错误文字不归我们展示：OpenAI 兼容服务器的认证错误会引出密钥的一部分（"Incorrect API key provided: sk-…abcd"），其他的会写出内部主机名或组织 ID。D40 的做法是成员读固定的句子、运维读详情，D50 沿用；放宽的只是详情现在有界、清洗过，并且存在日志里 |
| D50-5 | **真正读不懂的数据块仍是 `decode`**，现在带一句有界的位置说明（"a stream chunk is not JSON"、"content_block_delta without its block or delta"、"the stream is longer than the limit"），绝不含原始数据；同样只写入日志（事件只有错误码） | 两种情形仍可区分，且不泄漏载荷 |

**拿掉一个控制、看测试失败**：把 `detail` 放回事件：`TestTheProvidersWordsReachOnlyTheHostLog`、`TestANonRetryableStream…` 失败；`ClassifyStreamError` 一律返回 `decode`：`TestClassifyStreamError`、`TestStreamErrorDetailIsBoundedAndClean`、三个适配器各自的 `TestAStreamErrorIsTheProvidersAndKeepsItsWords`、harness 的 `TestARetryableStreamErrorBeforeTextIsRetried`、`TestAStreamErrorAfterTextIsNotRetried`、`TestANonRetryableStreamError…`、`TestAStreamErrorThatSaysTheContextIsFullIsTheContextWindowError`。

**仍不知道的**：那次 180.4 秒的失败，提供者到底说了什么（消息已丢，无法追溯），它是否真是流内错误事件，也可能是数据块损坏；本地模型服务（llama.cpp 一类）在长生成中途发出的错误事件的形状只按文档与适配器已有的解析推断，没有抓到真实样本。**下一次**：先在事件日志里找 `idp.host/model.error`（只有错误码，说明哪一步、第几次尝试），再用运行 ID 在 HOST 日志里找 `model call failed` 一行，其 `detail` 是提供者的话，或（错误码为 `decode` 时）读不懂的是哪一种数据块；若同一类 `provider_server` 反复出现，看模型服务自己的日志（那次 180 秒前后）。日志不是持久的审计记录：日志保留期短于要追查的时间，提供者的话就没了。

## 33. D51：网关标出的外部工具与运维列出的对外工具一视同仁（2026-11-19 决策与实施，idp-mcp 互通 ADR 步骤 X2 的 D-X5；契约不变）

修订 D44（谁算"对外工具"）、D48（子 agent 按同一规则判断）；沿用 D43、D44、D47、D49 的其余部分。

起因：idp-mcp 的 D50 至 D53 让网关能接入企业之外的上游（合作方的 MCP 服务）。网关在 `tools/list` 的回答里给这类上游的每个工具标一个键：工具对象的 `_meta` 里 `"io.idp.telos/external": true`，值取自审核时钉住的上游类别，上游自己写的同名键被覆盖或去掉。到此为止，HOST 只认运维在 `gateways[].externalTools` 里列的工具；每接一个合作方，运维就得把对方的工具名抄进 HOST 配置，漏一个，这个工具读到的内容就不再被当作"可能出企业"，附件和 CMI 上下文也不筛。D51 让网关的标记在运维的列表算数的每一处都算数。

**先说清楚今天谁在什么时候调 `tools/list`，缓存了什么（改动之前）**：

| 谁 | 何时 | 凭据 | 缓存 |
|---|---|---|---|
| worker 里的 `turn.prepareTools` | 每次 worker 认领运行、起一个 turn 时一次（每个 turn 一次，不是每一步；等待确认、被中断后接手的 turn 再来一次）；401 时用续期的凭据重来一次 | 运行存下的委派凭据（`RunCredential`）或网关的静态凭据 | 无。结果只活在这个 turn 里（`t.visible`、`t.repeatable`、`t.fileProps`、模型看到的工具表） |
| API 的 `createRun` | **从不调用**。它只换取凭据（`delegate`）并存进运行 | | |
| 子 agent 的 `CreateChildRun` | 不调用；子运行自己起 turn 时才调 | 继承父运行的凭据（同一网关）；别的网关没有凭据 | |

所以对外与否的判断（`FilePolicy.outward`，D44、D48）在创建运行时做，只看运维的静态模式和释放件的允许列表；标记要等 worker 的 `tools/list` 才知道，那时附件和 CMI 上下文早已定了。

| # | 决策 | 理由 |
|---|---|---|
| D51-1 | **`mcpclient.ListTools` 读标记：只读 `_meta` 里的 `io.idp.telos/external`，只有它是 JSON 布尔 `true` 才算**（`Tool.External`）。字符串 `"true"`、数字 1、`null`、`false`、对象、数组、大小写不同的键、`_meta` 不是对象、缺这个键，都是内部。读完即丢弃整个 `_meta`：`_meta` 的其余键来自上游，不可信，HOST 不读、不留、不转发 | 只有网关会设这个键；一个值得信任的键也只该有一种写法。不为它新增信任：上游数据仍是数据 |
| D51-2 | **创建运行时读目录，用调用者自己的凭据，对每个网关每次创建至多一次**（`API.catalogue`）。`createRun` 已有这个运行要用的凭据（委派凭据，或网关的静态凭据）；发布件有允许列表、网关已配置、运行由 HOST 执行工具（不是 `toolExecution: caller`）时，调 `tools/list`，超时 `CatalogueTimeout`（缺省 8 秒）。运维的模式已让发布件对外时不调（不需要）；没有允许列表或没有工具的发布件不调。`GatewayAuth` 增 `Endpoint`、`Client`、`StaticBearer`（`main` 照网关配置填）。这一步**不续期**凭据：续期可能轮换刷新令牌，会让运行存下的那个失效 | 唯一能在筛选之前知道标记的办法是问网关。用调用者自己的凭据，因为网关的目录因调用者而异（授权、可见性），HOST 自己的身份会看到别的目录。**代价与失败方式**：每次创建运行多一次到网关的往返（有工具的发布件），延迟加在创建请求上；每个网关每次创建只问一次，不论发布件与子 agent 有多少个；没有缓存，缓存会把"过时的知识"变成常态（见 D51-4）。网关慢或不通时创建请求最多多等超时那么久，运行仍会创建（见 D51-3），不会因此被拒 |
| D51-3 | **看不到就按对外算（失败即关闭）。** 目录读取失败（网关不通、超时、凭据被拒——包括会话绑定网关的首个访问令牌被拒，不续期、没有凭据、`GatewayAuth` 没有 `Client`）：发布件按"有对外工具"处理，附件与 CMI 上下文照 D44 筛，运行照常创建。`Outward.Via` 写 `gateway <名>: its tool catalogue could not be read when the run was created, so its tools count as sending content out`。子 agent 的发布件用另一个网关、而本次没有那个网关的凭据（静态凭据除外）时，同样按"读不到"算；这样的子运行本来也拿不到那个网关的凭据，多扣的代价是看得见的一行 `via` | 与 D44 对没有级别的条目一致：不知道就当作最坏。多扣只让一次运行少看到些机密内容，漏判让机密内容出企业 |
| D51-4 | **允许列表是工具名或 `*`（发布件校验如此），名字与 `*` 各自的规则**：名字在目录里且被标记，算对外；名字不在目录里（还不存在），算内部，因为不存在的东西不能被调用，真出现并被标记时由 D51-5 在 turn 里拦；`*` 按**目录现在**的内容算：有任何被标记的工具就对外，一个都没有就不是。**不**把"`*`"一律当作对外：运维的 `externalTools` 非空时 `*` 算对外（D44-2，不变）是因为那时运维说过有对外工具；标记没有这个先验，一律当对外会让每个允许 `*` 的发布件都被筛，而 D51-5 已经保证晚到的标记不会让未筛过的上下文出去 | 失败即关闭的另一半不是"多扣"，是"漏不了"：创建时可能错的知识，由 turn 里的闸兜住。一个无条件的规则会把一个窄的信号变成每个通配发布件的负担 |
| D51-5 | **turn 里的闸：运行创建时没有筛过，而网关现在把某个工具标为外部——这次调用被拒，运行继续。** `prepareTools` 在 turn 的 `tools/list` 里记下每个工具是否对外（运维的模式或标记），其中"只有标记、没有模式"的另记（`turn.marked`）。判断"筛过没有"：运行的 input 里 `externalTools` 为真（创建时就判为对外，或子运行有对外工具而继承的 CMI 条目已被去掉），或 `contextCeiling` 非空（因别的原因筛过，同一个上限，附件与 CMI 条目都按它筛过）。两者都不是，而被调的工具是"只有标记"的：该调用记为 `DENIED`，结果文字 `not run: the gateway now marks this tool as one that sends content outside the enterprise, and this run was created without its context being screened for such a tool; start a new run`，写 `idp.host/tool.denied {…, marked: true}`；**在询问确认之前**拒，模型看到这句工具错误，别的工具照常。工具仍列在模型的工具表里，所以模型会尝试并得到这句话，而不是一个"没有这个工具" | 选择"拒绝这次调用"而不是"让运行失败"：未筛过的上下文只有在内容被送出时才有害，拒绝这一次调用就挡住了；运行的其余部分（内部工具、回答）没有风险，失败会把成员已经等了的结果也扔掉。选择"留在工具表里"而不是"藏起来"：模型被明确告知原因，成员在运行里看得到，而不是一个悄悄缺失的能力。创建时有目录的话这一闸只在两次访问之间目录变了时才触发 |
| D51-6 | **标记在运维列表算数的每个地方都算数。** (a) 创建时"可能把内容送出企业"只在 `FilePolicy.outward`（经 `judgeTools`）一处算，附件、CMI 条目、`Input.ExternalTools`（D49 的"没有对外工具也没有来源就不检查"也读它）都由它来；(b) turn 里，`toolResults: off` 按 `wrap` 处理（D44-8）在"运行创建时判为对外"或"目录现在有可调用的对外工具"时生效，`idp.host/guard.raised` 多 `marked`：本 turn 目录里被标记的工具名；(c) D49 的来源收集看 `turn.external`（运维的模式或标记），不再只看模式；(d) 子运行：harness 把本 turn 目录里被标记的工具名（转义成 `path.Match` 模式）并入交给 `CreateChildRun` 的 `externalTools`，所以存储层判断子运行的发布件、去掉继承的 CMI 条目的规则不变（D44-10）；(e) 运维的 `confirmTools`、`neverResendTools`（D47）不变，标记不让工具被确认，也不影响重发 | "与运维列出的一样"是这条决定的全部；少了任何一处，标记的工具在那一处就是内部工具 |
| D51-7 | **子 agent（D48）同一规则。** `viaSubagents` 对子 agent 的发布件用同一个 `judgeTools`，同一个网关的目录一次创建只读一次，结果进 `via`：`sub-agent <路径>: may call an external tool (marked by the gateway: <名>)` 或 `(the catalogue of gateway <名> could not be read, so its tools count as sending content out)`。允许标记工具的子 agent 使父运行对外，父运行就不会读到会写进委派任务的机密内容 | D48 的理由不变：任务文字是自由文本，无法筛 |
| D51-8 | **事件说清是不是标记造成的。** 创建时，`Outward.Via` 先写自己发布件的原因：`tools marked by the gateway as sending content outside the enterprise: <至多 5 个名字，其余写"and N more">` 或目录读不到的那句；它进入 `idp.host/attachment.withheld`、`idp.host/context.withheld`（含刷新）的 `via`，进入 input 的 `contextVia` 与创建时的日志行。运维的模式造成的对外，`via` 与以前一样为空。`trigger` 的取值不变（`external-tools`）。turn 里：`guard.raised.marked`、`tool.denied.marked` | 运维要能回答"我没配置任何东西，为什么这个运行读不到机密附件"，答案是哪个网关标记了哪个工具；`via` 只放名字与原因，不放内容 |

**规则表（情形 → 结果）**

| 情形 | 创建运行时 | 运行的 turn 里 |
|---|---|---|
| 运维的 `externalTools` 匹配允许的工具，或允许 `*` 且运维列了模式 | 对外（D44），不读目录 | 与 D44 一样：包装结果、记来源 |
| 允许的某个名字在网关目录里被标记（`_meta` 键为布尔 `true`） | 对外；附件与 CMI 条目按上限筛；`via` 写出被标记的工具 | 结果按 `wrap`（`off` 也是）；引用记为来源；`guard.raised` 带 `marked` |
| 允许的名字在目录里，但没有标记，标记是字符串 `"true"`、1、`null`、`false`、对象、数组，或键写法不同 | 内部（与以前一样） | 内部 |
| 允许 `*`，目录里至少有一个被标记 | 对外 | 同上 |
| 允许 `*` 或某个名字，目录里现在没有被标记的匹配项 | 内部 | 内部；若 turn 里目录已变、出现被标记的工具：下一行 |
| 创建时没有筛过（既非对外，`contextCeiling` 也为空），turn 里的目录显示被调的工具只有标记没有模式 | | **拒绝这次调用**（`DENIED`，`tool.denied.marked`），不问确认，不到网关，运行继续 |
| 创建时因别的原因筛过（外部模型、子 agent、远端 agent），turn 里出现被标记的工具 | | 调用照常；结果包装，引用记为来源 |
| 目录读不到（网关不通、超时、凭据被拒、没有凭据、没有 `Client`）：对运行自己的发布件和对子 agent 的发布件 | 对外（失败即关闭）；`via` 写读不到 | 与"对外"一样；turn 里自己的 `tools/list` 失败仍是 `GATEWAY_ERROR`，不变 |
| 允许一个名字，网关目录里还没有它 | 内部 | 之后若被标记且运行未筛过：拒绝这次调用 |
| 子 agent 的发布件允许被标记的工具 | 父运行对外（`trigger` 含 `subagent`，`via` 写子 agent 路径与工具名） | 子运行起来时，harness 把标记的名字交给存储层，继承的 CMI 条目照 D44-10 去掉 |
| `toolExecution: caller` | 不读目录（运行不到网关）；运维的模式照旧 | 不适用 |
| 发布件没有允许列表或没有网关 | 不读目录 | |

**契约**：不变。`idp.telos.host` 1.11.0 的 schema 与示例不变，事件的新增字段（`via` 里的新条目文字、`guard.raised.marked`、`tool.denied.marked`）是自定义事件的可选字段，不在 schema 里；1.10.0 起 `ContextPolicy` 的描述把触发条件写成"HOST 配置 `gateways[].externalTools`"，与 D51 之后的行为不再逐字相符，是文字上的滞后，要不要为它发 1.11.1 由契约的维护者定。配置没有新字段、没有迁移、没有新指标。

**取舍与边界**：
- **创建请求多一次到网关的往返**（见 D51-2）。每个网关每次创建一次；有运维的模式命中、没有工具、`toolExecution: caller` 的运行不付。
- **`*` 看的是目录现在的内容**（D51-4）。创建后才出现的标记工具由 D51-5 拦，代价是一次被拒的调用，不是泄漏。
- **运维列表的"晚到"不拦。** D51-5 的拒绝只针对"只有标记"的工具：运维在运行创建之后才把某工具加进 `externalTools`（配置变了），该工具的调用仍照旧放行（D44-10 的第二道防线只管子运行）；这是 D44 已有的行为，D51 没有动它。若要同样拦，把 `t.marked[tc.Tool]` 换成 `t.external[tc.Tool]`，一行的改动，要不要另议
- **别的网关的目录只在静态凭据时读得到。** 子 agent 的发布件用另一个网关，运行没有那个网关的凭据，按"读不到"算（D51-3）。
- **会话绑定网关的首个访问令牌可能被网关拒**（D38 的测试网关就是这样），创建时按"读不到"算，运行多筛一次；turn 里 harness 会续期。要避免需要在创建时续期，会轮换刷新令牌，没有做。
- **没有缓存。** 每次创建重读；一个短的 TTL 缓存可以省往返，代价是创建时的知识更久、更常过时，D51-5 兜得住但会多拒几次调用。要不要加，等看到往返的实测延迟再定。
- **标记不改变确认与重发。** `confirmTools`、`neverResendTools` 仍是运维的事（D44-9、D47）。

**实施**：`internal/mcpclient`（`Tool.External`、`ExternalMetaKey`、`externalMarker`，`ListTools` 读后清掉 `_meta`）；`internal/api`（`Catalogue`、`judgeTools`、`toolVerdict`、`FilePolicy.outward` 与 `viaSubagents` 增 `look` 参数，`API.catalogue`、`listGateway`、`GatewayAuth.Endpoint/Client/StaticBearer`、`API.CatalogueTimeout`，`createRun` 在 `outward` 之前建查询）；`internal/harness`（`turn.external`、`turn.marked`、`screened`、`hasOutwardTools`、`callableMarked`、`externalToolPatterns`、`escapePattern`；`classify` 里的拒绝；`guardedMessages` 的 `wrap`；来源收集）；`cmd/idp-host/main.go`（填 `GatewayAuth`）；`tests/fixtures/fakemcp`（`Tool.Meta`、`Server.Lists()`）。

**验证**：`internal/mcpclient` 的 `TestListToolsReadsTheExternalMarkerAndNothingElseOfMeta`；`internal/api` 的 `TestTheGatewaysMarkerMakesAReleaseOutwardWithoutAnyOperatorPattern`、`TestTheMarkerAndAnUnreadableCatalogueCountForSubagents`、`TestTheCatalogueIsReadWithTheCallersCredentialOncePerGateway`；`internal/harness` 的 `TestAGatewayMarkedToolIsOutwardLikeOneTheOperatorListed`、`TestOnlyTheBooleanTrueUnderTheKeyMarksATool`、`TestAToolThatIsMarkedButWasNotWhenTheRunWasCreatedIsRefused`、`TestChildRunsAreCreatedWithTheToolsTheGatewayMarks`；`tests/integration` 的 `TestTheGatewaysExternalMarkerScreensARunOnPostgres`（没有任何运维配置：对照、四种不算数的标记、标记的运行附件与 CMI 条目被筛并且结果被包装、引用被记下、晚到的标记、网关读不到）。

**拿掉一个控制、看测试失败**：

| 控制 | 拿掉的做法 | 失败的测试 |
|---|---|---|
| 读标记（D51-1） | `ListTools` 一律把 `External` 置假 | `TestListToolsReadsTheExternalMarkerAndNothingElseOfMeta`、harness 的 `TestAGatewayMarkedToolIsOutwardLikeOneTheOperatorListed`、`TestAToolThatIsMarkedButWasNotWhenTheRunWasCreatedIsRefused`、`TestChildRunsAreCreatedWithTheToolsTheGatewayMarks`、api 的 `TestTheCatalogueIsReadWithTheCallersCredentialOncePerGateway` |
| 只认布尔 `true`（D51-1） | 字符串 `"true"` 也算 | `TestListToolsReadsTheExternalMarkerAndNothingElseOfMeta`、`TestOnlyTheBooleanTrueUnderTheKeyMarksATool` |
| 创建时用目录（D51-2、D51-4） | `judgeTools` 不看目录结果 | `TestTheGatewaysMarkerMakesAReleaseOutwardWithoutAnyOperatorPattern`、`TestTheMarkerAndAnUnreadableCatalogueCountForSubagents` |
| 创建时读目录（D51-2） | `createRun` 不建查询（`look` 为空） | `TestTheGatewaysExternalMarkerScreensARunOnPostgres`（没有读目录、附件与 CMI 条目没有被筛） |
| 失败即关闭（D51-3） | 目录读取失败按内部算 | `TestTheGatewaysMarkerMakesAReleaseOutwardWithoutAnyOperatorPattern`、`TestTheMarkerAndAnUnreadableCatalogueCountForSubagents` |
| 子 agent 看标记（D51-7） | 子 agent 的判断不传查询 | `TestTheMarkerAndAnUnreadableCatalogueCountForSubagents` |
| 每个网关只问一次（D51-2） | 不记已问的网关 | `TestTheCatalogueIsReadWithTheCallersCredentialOncePerGateway` |
| turn 里的闸（D51-5） | 去掉拒绝的分支 | `TestAToolThatIsMarkedButWasNotWhenTheRunWasCreatedIsRefused`、`TestTheGatewaysExternalMarkerScreensARunOnPostgres` 的晚到子例 |
| 别的原因筛过的运行不拒（D51-5） | `screened()` 只看 `externalTools` | `TestAToolThatIsMarkedButWasNotWhenTheRunWasCreatedIsRefused` |
| 结果包装（D51-6b） | `wrap` 只看创建时的 `externalTools` | `TestAToolThatIsMarkedButWasNotWhenTheRunWasCreatedIsRefused`（别的原因筛过的运行） |
| 来源收集（D51-6c） | 只看运维的模式 | `TestAGatewayMarkedToolIsOutwardLikeOneTheOperatorListed`、`TestAToolThatIsMarkedButWasNotWhenTheRunWasCreatedIsRefused` |
| 子运行（D51-6d） | 不把标记的名字交给存储层 | `TestChildRunsAreCreatedWithTheToolsTheGatewayMarks` |


**没有做的**：
- **平台的 compose 与 kind 验收**（合作方上游经 idp-mcp 的端到端）不在本仓库内。
- **目录的缓存**（见上）。
- **运维列表晚到时的拒绝**（见上）。
- **在创建时为会话绑定网关续期凭据**（见上）。
- **在界面上说明"为什么这个运行读不到机密内容"**：`via` 在事件流里，idp-workspace 的展示另议。
- **标记之外的上游信息**（上游自己的 `_meta`、注解）仍不被信任，也没有用到。

## 34. D52：上下文的账算对——窗口在配置里，按 token 估算，水位之上更早的内容逐步缩短，工具调用参数也缩短，快满时告诉模型（2026-11-19 决策与实施，依据 idp-host-context-management-proposal 阶段 A）

修订 D40（三个写死的数字按窗口推出，可配置；缩短不再只看工具结果）、D41（预算提示增加上下文一项）；沿用 D27、D44、D47、D49 的其余部分。契约不变（没有新的 REST 字段；事件 `idp.host/context.view` 与 D45 的事件一样不在契约里列举）。不改变模型被告知的意思：不摘要、不删除任何一轮，只是更早的、模型已经读过并据此行动的内容给它看得更短。

**测过什么**（基线：`.local/long-eval/baseline-eeaafeb-20261119`，15 次用例运行、21 轮、658 次有用量的模型调用，窗口 131,072 token；运行库转储恢复到临时库，按存下的消息重建每一步的请求）：

- **晚期请求里的东西**：输入超过 60,000 token 的 223 步里，估算的 token 构成是工具结果 54.7%、**模型自己写的工具调用参数 44.5%**、用户与助手的文字 0.8%。L2 第三轮第 33 步（126,514 token，窗口的 96.5%）：参数 173 KB、结果 157 KB（D40 缩短之后）、文字 4.5 KB。D40 缩短了结果而没有碰参数，所以参数是剩下的最大一块，每步原样重发。
- **密度**（每字节 token，按字节类别，在 Qwen 的分词上）：数字最贵，约一个字节一个 token（它把数字逐位切开）；字母、空格、标点约 0.25；多字节字符（中文、制表符号）每字节约 0.38。结果里数字多，所以 D40 记的 "0.27 到 0.85 token/字节" 是同一件事的两头。
- **每步增长**：相邻两步输入的增加占窗口的比例，中位数 0.5%，p90 2.4%，p99 7.9%，最大 9.9%（一个 16 KiB 的数字表）。L2 第三轮里超过窗口 80% 的两次：一次在 23 步的第 11 步（还剩 13 步），一次在 33 步的第 6 步（还剩 28 步）。
- **基线的第三轮**：85%、50%、97%（见提案 4.1）；第一、二轮 63%、71%、69%、76%。

**决策**

| # | 决策 | 数字从哪里来 |
|---|---|---|
| D52-1 | **窗口在 HOST 配置里。** provider 条目下 `models[]`（`name`、`contextWindow` 以 token 计，可选 `context:`），键是 `provider/模型名`。**没有 `contextWindow` 的模型，D52 的一切都不适用**：请求与 D40 逐字节相同（见下面的测试），参数不缩短，没有提示，没有调用前的拒绝，不写 `context.view` 事件；该条目里写的阈值只覆盖 D40 的三个数字。丢掉的是：窗口满时不能提前拒绝，也就是把一次必败的调用交给模型服务去拒，并且长会话仍旧只能靠 D40 的头尾缩短撑着。发布件里没有这些设置：发布件不可变而 HOST 配置不是；放在发布件里要升契约，而且要保证发布件不能超过运维允许的，不如不开这个口子。**按租户覆盖没有做**（HOST 配置没有租户维度，只有 `files` 有；窗口是模型的属性，不是租户的）。一个发布件有回退模型时，用候选里**已知窗口里最小的**（一个请求要装得下任何一个可能回答它的模型）；没有条目的候选不算，不会因为一个没配的回退把机制关掉 | 窗口只有运维知道 |
| D52-2 | **输入按 token 估算，不依赖分词器。** 每个字节按类别计：ASCII 字母与符号 0.275，数字 1.25，非 ASCII 字节 0.42；每条消息另加 34，每个请求加 20；指令、消息正文、工具调用的名字与参数、工具说明与参数 schema 都算。**这几个数是基线 658 个请求的拟合值再各加 10%**：拟合是按相对误差加权的非负最小二乘，特征是三类字节数（所有内容合并）与消息条数，请求按存下的消息重建（D40 当时的缩短规则、守卫的包裹），固定部分（工具说明）取每个会话第一个请求的用量与估算之差；按会话留一交叉验证，未放大的拟合值误差 p5 −11.8%、p95 +16%（所有步）。放大 10% 是为了估算多半高于而不是低于模型服务的计数。**只在一个模型的分词上测过**；别的分词多半更省（数字不逐位切），估算偏高，这是安全的一边，上一步的用量会把它校正回来。**误差**（回放测试 `TestReplayTheBaselineUnderTheRulesOfD52`，D40 当时发出的请求，658 步）：未校正，所有步 p5 −1.2%、中位数 +6.6%、p95 +26.0%、最大 +40.5%（小请求偏高：固定部分占比大）；输入超过 60,000 token 的 223 步 +0.1% 到 +15.7%（中位数 +12.1%）。**按上一步的用量校正后**：所有步 p1 −4.9%、p5 −2.1%、中位数 +0.3%、p95 +5.9%、p99 +16.3%、最大 +26.2%；超过 60,000 token 的 223 步 p1 −1.8%、中位数 +0.2%、p95 +3.3%、最大 +15.5% | 上面的拟合与误差 |
| D52-3 | **用上一步的用量校正，且校正有界。** 每步结束后，用模型服务报告的 prompt token 数与这一步发出的请求的**未校正**估算相比：比值先夹在 [0.5, 2]，因子取"旧因子与这个比值的平均"，再夹在 **[0.7, 1.5]**。因子初始为 1（一个运行的第一步没有校正）。报告为 0 的不算。所以一次怪的报告（0、百倍、没算缓存的计数）最多把因子推动 0.75，永远出不了 [0.7, 1.5]：估算不会被当成它自己的 0.7 倍以下，也不会超过 1.5 倍，水位与拒绝都不会因此失效。阶段 B 注意：Anthropic 开缓存后 `input_tokens` 不含缓存读取的部分，适配器的用量要把它加回去，否则因子会落到 0.7 的下限（D54 已做：Anthropic 适配器的 `PromptTokens` 现在是 `input_tokens` + 缓存读取 + 缓存写入） | 误差分布；有界是为了不被一次怪报告关掉 |
| D52-4 | **阈值按窗口推出，131,072 token 时恰是 D40 的数字，都可以按模型配置。** 单个结果上限 = 窗口的 1/8 字节（131,072 → 16,384；夹在 4 KiB 到 64 KiB，64 KiB 是 D40 之前的上限）；最近保持原样的结果数 = 6（上限随窗口走，所以最坏情形（密度 0.85 token/字节）的 6 个结果约占窗口的 64%，与窗口无关）；更早的结果与参数里的字符串超过多少字节只留头尾 = 窗口 × 75/8192（131,072 → 1,200；夹在 512 到 4,096）；水位 = 窗口的 **0.6**；提示 = 窗口的 **0.8**。优先级：运维为这个模型写的值，其次 `Harness` 里显式给的值（测试与嵌入用），其次推出的值。配置校验：窗口 4,096 到 16,777,216；水位 (0.2, 0.9]，提示 (0.3, 0.95]，水位低于提示；结果上限 1 KiB 到 1 MiB；保留个数 1 到 50；缩短阈值 200 字节到 1 MiB | 16 KiB、6、1,200 是 D40 在一个 131,072 的窗口上调出来的，这里只把它们按窗口放缩，**1,200 不是从测量得来的数**；0.6 的水位：回放里第三轮不到水位就停（见下表），并留下 20 个百分点给估算误差与一步的增长（p99 7.9%）；0.8 的提示见下 |
| D52-5 | **逐步缩短的规则**（`shape`）。每步从存下的线程出发重算，不改线程。**不动的**：系统部分（指令、守卫前言）、调用方的上下文条目与文件说明（它们不是线程）、每一条用户消息、助手的文字、出错的工具结果（那是 HOST 自己的话）。**"最新的"**：最近 `keepToolResults`（6）个工具结果，以及要了它们的那些调用的参数；一个还没有结果的调用算最新。可缩短的是工具结果与工具调用参数里的字符串值。顺序：<br>**0 级（D40 的规则，与窗口无关）**：不在最新 6 个之内、超过 `oldResultBytes` 的结果只留头 2/3 与尾 1/3（D40 的原样字节）；**有窗口时**，同样位置、同样规则，参数里超过 `argumentBytes` 的字符串值也如此。<br>**水位**：算出的估算（乘校正因子）高于水位时，依次做以下各级，每一级都从最老的一项做起，**估算降到低水位（水位的 5/6，即窗口的 50%）以下就停**，低于低水位不再多做：1 级，更早的项缩到 `oldResultBytes`/2；2 级，缩到 /4；3 级，缩到 /6（至少 128 字节）；4 级，**最新 6 个之中除最新 2 个以外的**项缩到 `oldResultBytes`，并且技能的参考文档（`resource__read`）也可以缩了；5 级，同一批缩到 /6。**底线**：最新的 2 个工具结果与它们的调用永远原样；每一项缩短后至少留 128 字节的头尾，并带标记；一级里没有一项能再缩（已经够短）就进下一级；5 级之后没有可缩的了，请求照常发出（估算若仍装不下，走 D52-8）。<br>**缩过的不再变长**：一个运行里，一项在某一步缩到多少，之后的步骤里不会比这更长；所以水位触发一次，往下缩到低水位，接下来的若干步（每步通常增长 0.5% 到 2.4%）请求中新缩的项之前的部分不变 | 低水位的目的是阶段 B：一步一缩、从最老的项缩起，每步都会改动请求的最前面，前缀缓存每步失效；一次缩到位，两次缩短之间前缀不变（`TestWhatAStepCutStaysCutAtTheNextSteps`）。代价：触发时多缩 10 个百分点的窗口。最新 2 个的底线：模型正在用的结果不动 |
| D52-6 | **工具调用的参数怎么缩短，才仍是每个提供者要的请求。** 各适配器发出的形式：OpenAI 兼容是 `arguments` 的 JSON 字符串；Anthropic 是 `tool_use.input`，适配器把参数 `Unmarshal` 成对象，不是 JSON 就拒发；Gemini 是 `functionCall.args`，同样 `Unmarshal` 成对象。所以缩短后的参数必须仍是合法的 JSON 对象。做法：逐个 token 重写，**键与值的顺序不变**，只把长于限额的字符串值（任意嵌套层）换成头 2/3、尾 1/3 加一句标记，其余值（数字按原文、布尔、null、短字符串）原样；HTML 字符不转义（`<`、`&` 不变成 `<`）；输入不是合法 JSON、末尾有多余数据、没有可缩的字符串、缩了也不更小，就原样返回。标记在字符串值里面：`[… N bytes of this earlier argument value are not shown. The call was made with the whole value; only this view of it is shortened, so do not take what is left for what was sent …]`，**说的是 N 字节没有显示，调用是用完整的值做的，看到的只是缩短的样子**，模型不会以为那次调用就是用缩短的值发的。每家适配器各有一个测试，把缩短后的消息交给它的 `body` 再解码，查 JSON 合法、是对象、键都在、标记在 | 三家的形式各查过适配器代码；"不会以为调用是用缩短的参数做的"是标记的第二句 |
| D52-7 | **记什么（G3）。** 存下来的线程、工具调用记录（`tool_calls.arguments`）、事件、来源都不动：缩短只发生在发给模型的视图上，视图由 `shape` 每步重算，复制了消息与调用的切片再改。**每一步**，有窗口的模型写一条事件 `idp.host/context.view {step, windowTokens, markTokens, estimateTokens（校正后）, rawEstimateTokens, factor, level（这一步最深做到几级）, shortened, hint, items}`，`items` 是 `[kind（result｜arguments）, id（工具消息 id｜工具调用 id）, bytes（原长）, limit（缩到的限额）, shown（给模型的长度）]`；这一步缩短的项与上一步相同时写 `itemsAsBefore: true` 代替列表。有存下的线程和这一条，每一步的视图可以重建：`Shorten(原文, limit)` 与 `ShortenArguments(原文, limit)` 的结果就是模型读到的（`TestTheRecordOfAStepRebuildsWhatTheModelRead` 逐项核对）。D40 原来不记缩短（"存下来的会话与事件不变"），没有窗口的模型仍不记 | G3 要求"模型看到的是什么有记录"，D40 没有记；最小的记法是每步一条 |
| D52-8 | **告诉模型，并在调用之前拒绝装不下的请求。** (a) 估算（校正后）达到提示线（窗口的 80%）时，预算提示（D41，最后一条开发者消息，每步变，不在线程里）加一句："Context: this conversation fills about N% of the model's context window (about X of W tokens); the run fails when the window is full and cannot go on. Do not start new work: finish what is under way, deliver what exists, say plainly what is not done, and suggest that the user start a new conversation for the rest."。它放在预算提示里已有的位置，**没有新增会变的消息，也没有进稳定前缀**：Anthropic 与 Gemini 的适配器仍把开发者消息并进 system，所以那里整个 system 每步变，这一点没有改好也没有变坏，留给阶段 B 第 1 项。(b) 逐级缩到底之后，若校正后的估算仍高于 `窗口 − max(最大输出 token, 256)` 的 1.05 倍（5% 是估算自己的余量，防止偏高的估算结束本来装得下的运行），**不调用模型**：这一步记为 `context_length`，运行以 `MODEL_ERROR` 结束，消息是 D40 的那句（"the conversation no longer fits the model's context window; start a new conversation, or ask for less at a time"），HOST 日志一行 Warn | 提示线 80%：基线里相邻两步的增长中位数 0.5%、p90 2.4%、p99 7.9%、最大 9.9%；从 80% 到满有 20 个百分点，典型情形（p90）约 8 步，够模型交付；**两个连续的最大步（各约 10%）只够 2 步，不够**。但提示是最后一道：水位 60% 之下的请求不会走到这里，回放里水位以下就停了（最高 59.5%）；所以保持 0.8，运维可调。没有测过模型收到这句话后怎么做 |
| D52-9 | **上下文窗口错误三家都认得，流式与非流式。** 原来 Anthropic 与 Gemini 适配器非 200 时丢弃正文，所以非流式的 400 永远是 `status_400`；现在三家都读（至多 64 KiB，只用来分类，不记录、不交给任何人）。各家文档里的形式：OpenAI 兼容——`context_length_exceeded`、"maximum context length"、llama.cpp 的 `exceed_context_size_error`（原有）；Anthropic——400 `invalid_request_error`，"prompt is too long: N tokens > M maximum"（原有标记），"input length and `max_tokens` exceed context limit: N + M > W"（新）；Gemini——400 `INVALID_ARGUMENT`，"The input token count (N) exceeds the maximum number of tokens allowed (M)"（新）。流里的错误事件（200 之后）走 D50 的 `ClassifyStreamError`，用同一张标记表。Anthropic 流以 `stop_reason: model_context_window_exceeded` 结束的算被窗口截断（`StopLength`），D40 ③ 照常以失败结束而不是记为完成。**只按文档写出的形式测过，没有抓到各家的真实样本** | 三个适配器各有 `TestContextWindowErrorsAreRecognisedStreamedAndNot` |

**G4（每条一个测试）**：缩短发生在发给模型的视图上，在 D27 的包裹、D44/D48/D51 的筛选之后，从不把内容移出包裹，从不去掉标记，从不碰 D49 的来源。

| 对象 | 怎么保证 | 测试 |
|---|---|---|
| D27 包裹 | 守卫 `ApplyView` 对**整个结果**扫描、判定、报告发现，被缩短的文字放进同一个包裹里；缩短后的结果仍以 `<untrusted source="tool_result">` 开头、以 `</untrusted>` 结尾，各只出现一次，缩短标记在包裹里面 | `TestAShortenedResultStaysInsideItsUntrustedBlock` |
| D27 发现 | 被缩短掉的中段里的注入语句仍被发现并报告一次；`block` 模式下仍被扣住（D40 的写法先缩再扫，中段里的语句扫不到；现在扫整份，这是行为上唯一的差别，只在中段含注入语句且被缩短时可见，方向是更严） | `TestAPhraseInTheCutMiddleIsStillFlaggedAndBlocked` |
| D44 | 发布件说 `off` 但运行有对外工具时仍包裹，缩短后也包裹 | `TestResultsStayWrappedWhenTheRunHasExternalToolsAndTheyAreShortened` |
| D44、D48、D51 的筛选 | 筛选在运行创建时做（`api`），发生在线程与上下文条目到达 harness 之前；harness 对上下文条目与文件说明一概不缩短，有窗口与没有窗口的请求里这一部分逐字节相同 | `TestContextEntriesAndTheFilesNoteAreNeverShortened` |
| D49 | 来源取自网关结果的 `structuredContent`，在缩短之前、与缩短无关；9 个长结果被缩短之后 9 个来源都在，答案里的 `[1]`、`[9]` 核对通过 | `TestSourcesAreCollectedFromTheWholeResultNotTheShortenedView` |
| 请求重放与幂等 | 请求哈希来自运行的输入，视图不参与；同一线程两次得到同样的请求，运行的输入不变 | `TestTheViewIsDeterministicAndLeavesTheRunInputAlone` |
| D47 续跑 | 重发的调用用存下的 `tool_calls.arguments`，从不用视图；构建视图不改线程 | `TestResumeSendsTheStoredArgumentsNeverTheShortenedView` |

**回放预测**（`TestReplayTheBaselineUnderTheRulesOfD52`；数据目录由 `tests/replay/export_baseline.sh` 从基线的转储导出，在仓库之外，环境变量 `IDP_CONTEXT_REPLAY_DIR`，没有它就跳过）：对基线三个 L2 会话的每一步，用存下的消息重建 D40 当时发出的请求，再按新规则重建，用代码里的 `guardedMessages`；校正因子按基线上一步的用量算，估算的偏差按那一步自己的"实际 / D40 估算"折回 token。下表是每一轮的最大输入占窗口的比例：

| 会话 | 轮 | 记录的 | + 参数缩短 | + 逐步缩短（默认，水位 0.6） | 水位 0.5 | 水位 0.7 |
|---|---|---|---|---|---|---|
| L2 第 1 遍 | 1 / 2 / **3** | 62.9 / 70.8 / **85.2** | 51.0 / 57.0 / 68.0 | 51.0 / 57.0 / **54.8** | 46.6 / 44.1 / 47.6 | 51.0 / 57.0 / 68.0 |
| L2 第 2 遍 | 1 / 2 / **3** | 2.3 / 22.1 / **49.9** | 2.3 / 21.9 / 44.0 | 2.3 / 21.9 / **44.0** | 同左 | 同左 |
| L2 第 3 遍 | 1 / 2 / **3** | 68.8 / 75.7 / **96.5** | 54.6 / 60.0 / 76.7 | 54.6 / 49.5 / **59.5** | 48.6 / 42.2 / 49.7 | 54.6 / 60.0 / 70.6 |

第三轮三遍中的最大值：**记录 96.5%；只加参数缩短 76.7%（−19.8 个百分点）；再加逐步缩短 59.5%（再 −17.2 个百分点）**。判据是不高于 80%。参数缩短单独就让最大值落到 80% 以下，逐步缩短让它停在水位附近，并为估算误差留出余量。L1 的三个单轮运行 48.9 / 68.3 / 55.2 降到 41.2 / 55.3 / 46.8；L0 的 26.9 / 27.6 降到 24.1 / 21.0；L5、L6 基本不变（≤10%，没有多少参数可缩）。

**这是预测，不是测量。** 模型看到缩短的内容后会做不同的事：步数、用的脚本、读不读文件都会变，上面每一步的内容是基线的，后面的步骤并不会照着发生。基线只有三轮，**五轮的版本回放不出来**；稳态的推断（水位把请求拉在 50% 到 60%，不可缩的部分是用户消息、助手的文字、最新 2 个结果，几轮之内增长很慢）要靠真实模型验证。尤其是"列出前面各轮做过的每一处修改"那一轮：模型靠的是助手的文字和缩短后的头尾，它被缩短的脚本已经说明了"调用是用完整的值做的"，但会不会因此漏掉或重做，只有重复调用这一项指标能回答。

**实测**（2026-11-19 至 20，本机模型，窗口 131,072，水位 60%；平台 `tools/long_eval`，每个用例三遍；之前 = `4628a1f`，没有本决策）：

| | 之前 | 本决策 |
|---|---|---|
| 五轮对话完成的轮次 | 15 轮里 7 轮：两遍在第 3 轮撞到窗口，之后每一轮都在第 1 步失败 | 15/15 |
| 第 3 轮单步最高占比 | 79%、100%、100% | 59%、60%、60% |
| 第 5 轮列出前几轮的修改 | 走到第 5 轮的一遍列全了 | 三遍都列全了 |
| 报价表单轮 | 3/3 交付 | 3/3 交付 |
| 校正后的估算误差 | — | 中位数约 0.1%；最大的一步 15% 到 29% |

- **预测与实测一致**：回放预测第 3 轮的最高占比约 60%，实测 59% 到 60%。
- **最长的一遍截到了底**：第 5 轮到了第 5 级，95 步里 51 步要做新的截短，单步占比 73%，压不回水位。这时"截一次管很多步"不成立，前缀每步都在变。再往后靠的是提示和上限，真正的出路是摘要（没有做）。
- **重复调用**：27 轮里共 10 次。其中 5 次是重新导出改过的文件，3 次是在任何内容被截短之前调了一个不存在的工具名，2 次出现在上面那个截到底的第 5 轮。只有这 2 次不能排除与截短有关。
- 平台的记录：`docs/adr/idp-host-context-management-proposal.md` 4.2 节（平台仓库本地）。

**拿掉一个控制、看测试失败**（每一行都是先改代码、跑测试、记下哪些测试失败、再还原）：

| 拿掉的控制 | 失败的测试 |
|---|---|
| 参数不缩短（`shape` 不把参数当可缩短项） | `TestOlderToolCallArgumentsAreShortenedAndStillValidJSON`、`TestOneOddUsageReportCannotSwitchTheMechanismOff`、`TestAShortenedResultStaysInsideItsUntrustedBlock`、`TestAnErrorResultIsNeverShortened`、`TestASkillResourceIsShortenedOnlyAtTheDeepestLevels`、`TestContextEntriesAndTheFilesNoteAreNeverShortened`、`TestResultsStay…` 等 |
| 水位被忽略（不触发逐步缩短） | `TestProgressiveShorteningGoesOldestFirst…`、`TestProgressiveShorteningStopsWhenNothingIsLeftToShorten`、`TestTheModelIsToldWhenTheContextIsFilling`、`TestResumeSends…`、`TestASkillResource…`、`TestContextEntries…` |
| 从最新的项缩起，不是最老的 | `TestProgressiveShorteningGoesOldestFirstUntilTheEstimateIsUnderTheMark`（原来的 40,000 窗口上缩到同一个限额，查不出来，所以这个测试改用 55,000：一部分项停在 256、一部分在 128） |
| 没有底线（最新 2 个也可缩） | `TestProgressiveShorteningStopsWhenNothingIsLeftToShorten` |
| 预算提示里不加上下文一句 | `TestTheModelIsToldWhenTheContextIsFilling` |
| 调用之前不拒绝 | `TestARequestThatCannotFitIsRefusedBeforeTheModelIsCalled` |
| 校正没有上下限 | `TestTheCorrectionFollowsUsageAndIsBounded`、`TestOneOddUsageReportCannotSwitchTheMechanismOff` |
| 参数标记去掉 | `TestShortenedArgumentsStayValidJSON`（三个子测试）、`TestArgumentsThatCannotOrNeedNotBeShortened…`、`TestOlderToolCallArgumentsAreShortenedAndStillValidJSON` |
| 参数按原始字节截断（不再是合法 JSON） | `TestShortenedArgumentsStayValidJSON`、`TestShortenedArgumentsMakeAValidRequest`（OpenAI、Anthropic、Gemini 三个适配器各一个）、`TestOlderToolCallArguments…` |
| 守卫扫描缩短后的视图而不是整个结果（G4 D27） | `TestAPhraseInTheCutMiddleIsStillFlaggedAndBlocked` |
| 缩短过的结果不再包裹（G4 D27、D44） | `TestAShortenedResultStaysInsideItsUntrustedBlock`、`TestResultsStayWrappedWhenTheRunHasExternalToolsAndTheyAreShortened`、`TestWithoutAContextWindowTheRequestsAreThoseOfD40` |
| 上下文条目也被缩短（G4 D44、D48、D51） | `TestContextEntriesAndTheFilesNoteAreNeverShortened` |
| 视图直接写进消息、不复制调用切片（线程被改，G3、D47） | `TestOlderToolCallArgumentsAreShortenedAndStillValidJSON`（存下的线程变了）、`TestResumeSendsTheStoredArgumentsNeverTheShortenedView`、`TestWhatAStepCutStaysCutAtTheNextSteps` |
| 来源改取自缩短后的结果（G4 D49；这条没有自然的一行改法，改的是：结果超过 1,200 字节就不读它的 `structuredContent`） | `TestSourcesAreCollectedFromTheWholeResultNotTheShortenedView` |
| 缩过的下一步又变长（没有粘性） | `TestWhatAStepCutStaysCutAtTheNextSteps` |
| 只看主模型的窗口，不看回退 | `TestTheSmallestKnownWindowOfTheCandidatesApplies` |
| 没有窗口也缩短参数 | `TestWithoutAContextWindowTheRequestsAreThoseOfD40`（与 D51 的请求逐字节比较） |
| 记录写错限额 | `TestTheRecordOfAStepRebuildsWhatTheModelRead` |
| Anthropic 适配器不读 400 的正文 | `TestContextWindowErrorsAreRecognisedStreamedAndNot`（anthropic） |
| Gemini 适配器不读 400 的正文 | `TestContextWindowErrorsAreRecognisedStreamedAndNot`（gemini） |
| 标记表里去掉 Anthropic 与 Gemini 的新标记 | `TestContextWindowErrorsAreRecognisedStreamedAndNot`（anthropic、gemini） |
| 按窗口推出的数字改了（131,072 不再给 1,200） | `TestThresholdsAreDerivedFromTheWindow` |

没有对应测试可拿掉的两项：D47 的"重发用存下的记录"是结构性的（`execute` 读记录，视图只在 `guardedMessages` 里存在），所以测试靠"线程不被改"那一行（上表"视图直接写进消息"一行）卡住；请求哈希在 `api`，不经过本代码，测试只证明视图确定、运行输入不变。每个被拿掉的控制都至少有一个测试失败，逐个做过，全部还原后 `make test` 与 `make test-integration` 通过。

**没有做的**：
- **提示词缓存**（阶段 B）：Anthropic 的缓存断点、缓存用量、稳定前缀与变化部分的分离（含把预算提示移出 system）都没有做；D52 只做了它的前提，一次缩短之后前缀不变。
- **摘要式压缩**（阶段 C）：没有。
- **真正的分词器**：估算只有一个模型的数据；换模型要重新拟合密度。
- **按发布件、按租户覆盖阈值**：见 D52-1。
- **提供者报告上下文窗口错误后的自动收缩重试**：D40 的失败照旧；估算与实际差得多时（因子到了上下限）会走到这里。
- **五轮 L2 与真实模型的测量**：由评测者运行。
- **各家上下文错误的真实样本**：只按文档形式测。
- **平台的 compose 与 kind 配置**要加 `models` 条目才得到这个行为（见 README），本仓库不含这些文件。

## 35. D54：测量先于优化——每一步记下首个输出前花的时间与提供者说的缓存复用（2026-11-19 决策与实施，阶段 B"提示缓存"之前；契约不变）

沿用 D45（`model.usage` 事件与尝试次数）、D46（空闲看门狗）、D52（用量校正）。**这是一条测量，不改变发出的请求，也不改变任何运行的行为**，除了下面 D54-5 的一处改正（只在 Anthropic 开了缓存时才有差别）。

**起因**：2026-11-19 在本机一个 OpenAI 兼容模型服务（llama.cpp 一类，单槽）上测得：一个长任务 85% 的时间在模型里，一步 7 到 13 秒，模型每秒写约 123 个 token，**全部 token 的 97% 是重发的输入**。能保留上一个请求的提示的服务可以复用公共前缀，只处理变化的部分；前缀在很早的地方变了（每步变的提示放在历史前面，D52 把老消息缩短），后面的全要重算。此前每步只记总延迟、提示 token 与输出 token，说不出一步里多少时间花在第一个 token 之前，也说不出多少提示被复用。阶段 B 要用这些数字来评判，所以先有数字。

| # | 决策 | 为什么 |
|---|---|---|
| D54-1 | **首个输出 = 请求发出之后，任何种类的第一个增量**：文字、推理文字、工具调用的名字或参数，都算；只有角色、没有内容的块（OpenAI 流的第一块，`content: ""` 或 `null`）与 Anthropic 的空文本块开始不算。**另记首个文字增量**（`firstTextMs`）。"请求发出"= 适配器把请求交给 HTTP 客户端之前的一刻，所以包括建连、服务端排队与处理提示。**选首个任何种类的增量**：它度量的是"提示处理完、模型开始写"，这是前缀缓存影响的那一段；只看文字则会把思考模型的推理时间和只发工具调用的步（没有文字）算成别的东西。两者有实质差别：思考模型的首个文字晚于首个输出，一个只调工具的步没有首个文字（缺失，不是 0 或总时间）。另记**首个输出到流结束**（`afterFirstMs`）= 生成阶段 | 一个数字回答"提示处理花了多久"，另一个回答"用户多久看到字" |
| D54-2 | **没有流就没有首个输出：记为缺失，绝不记为总时间。** OpenAI 适配器遇到 `application/json` 的回答（服务端忽略了 `stream: true`）、失败的请求、没有任何输出的流，三个时间都缺失。Anthropic、Gemini 适配器只会流式；它们收到的不是流时本来就解码失败，所以没有"非流式"这一种。`harnessworker` 适配器（子进程协议）不报，缺失 | 缺失与 0 不同；评估按缺失处理，不把它混进中位数 |
| D54-3 | **每次尝试一份，一步取成功的那一次。** harness 为每个请求（D45 的重试与回退都算）新开一个 `model.Stopwatch`，由适配器在发出请求、首个输出、首个文字、流结束时打点；事件 `model.usage` 在这一步用了不止一个请求时多一个 `attemptTimings: [{attempt, provider, model, outcome（ok 或错误码，同步骤行的 error_code）, latencyMs, firstOutputMs?, firstTextMs?, afterFirstMs?}]`；步骤行与事件顶层的时间是成功的那次的。失败的尝试若在失败前已有输出（流中途断了），它的 `firstOutputMs` 在，`afterFirstMs` 不在 | D45 已把"一步几个请求"记下，时间应当也是 |
| D54-4 | **提供者说的复用与处理，各家的文档里的字段**：<br>**OpenAI 兼容**：`usage.prompt_tokens_details.cached_tokens`（OpenAI 文档；流里需要 `stream_options.include_usage`，即 `usageInStream`，本来就有；缓存的是 `prompt_tokens` 的一部分）→ `cachedTokens`。<br>**llama.cpp**：它的 `usage` 也带 `prompt_tokens_details.cached_tokens`（`server-task.cpp` 的 `usage_json_oaicompat`，值是复用的提示 token 数），另外有自己的 `timings` 对象：`cache_n`（复用的提示 token）、`prompt_n`（实际处理的提示 token）、`prompt_ms`、`predicted_n`、`predicted_ms`（另有 `*_per_token_ms`、`*_per_second`，不记）。→ `server.{cacheN, promptN, promptMs, predictedN, predictedMs}`。**`timings` 不需要任何请求选项**：源码里最后一个响应一律带（`slot.stop != STOP_TYPE_NONE` 时填 `stats`），流式时挂在发出的最后一块上（开了 `include_usage` 时是 `choices: []` 的用量块，否则是结束块）；`timings_per_token: true` 才让**每一块**都带，**没有发送，也不需要发送**，所以请求与 D52 之前逐字节相同，没有别的副作用。每块都带时取最后一块。`return_progress` 与 `prompt_progress`（提示处理进度）不用。没有 `include_usage` 时 llama.cpp 仍发 `timings`，但没有 `usage`，`promptTokens` 仍是 0（不用 `timings` 推）。<br>**Anthropic**：`usage.cache_read_input_tokens` → `cachedTokens`，`usage.cache_creation_input_tokens` → `cacheCreationTokens`；在 `message_start` 的 `message.usage`，`message_delta` 的 `usage` 可能再给一遍累计值，后到的为准。**目前不发 `cache_control`，所以这两个字段通常是 0 或缺失；阶段 B 发了才有值。**<br>**Gemini**：`usageMetadata.cachedContentTokenCount` → `cachedTokens`（隐式缓存与显式缓存都在这个字段里）。<br>**未知字段一概忽略；没说的就是缺失，不是 0**（`cached_tokens: 0` 是测量，字段没有是没说）。这些数字没有在真实的 Anthropic、Gemini 服务上抓过样本，**适配器按文档写，测试用按文档写的样本** | 记下提供者的话，不自己推 |
| D54-5 | **改正：D52 的校正因子要对着窗口里装着的整个提示算，不是没缓存的那一部分。** 查过四家：OpenAI 的 `prompt_tokens` 包含缓存的部分（`cached_tokens` 是其中的一份）；llama.cpp 同（`prompt_tokens` 是 `n_tokens`，缓存的在里面）；Gemini 的 `promptTokenCount` 包含缓存内容；**只有 Anthropic 的 `input_tokens` 不含**缓存读取与缓存写入。Anthropic 适配器原来把 `input_tokens` 当 `PromptTokens`，一旦阶段 B 开了缓存，D52 的因子会被拉向 0.7 的下限（比如一个提示的九成读自缓存：比值 0.1，夹到 0.5，因子每步折半往下走），水位与拒绝都会按偏低的估算判断。**现在 Anthropic 适配器的 `PromptTokens` = `input_tokens` + `cache_read_input_tokens` + `cache_creation_input_tokens`**，`model.Usage.PromptTokens` 的文档写明"总是整个提示"。测试：适配器层（`TestPromptTokensIsTheWholePromptWhenPartOfItWasCached`，去掉改正就失败）与 harness 层（`TestD52sCorrectionSeesTheWholePromptWhenAnthropicSplitsIt`：同一个提示，无论供应商把它拆成缓存与非缓存，因子序列逐步相同；去掉改正，第 2 步起就不同）。**今天没有任何一次运行受影响**，因为没有发 `cache_control`；这是阶段 B 的前置 | 阶段 B 的 D52-3 备注已写这一条 |
| D54-6 | **记在哪里。** 事件 `idp.host/model.usage` 新增可选字段 `firstOutputMs`、`firstTextMs`、`afterFirstMs`、`cachedTokens`、`cacheCreationTokens`、`server{promptN, promptMs, cacheN, predictedN, predictedMs}`、`attemptTimings`，缺失的不写。步骤行（`run_steps`，迁移 `0011_step_timing.sql`）新增可空列 `first_output_ms`、`first_text_ms`、`after_first_ms`（毫秒，保留小数）、`cached_tokens`、`cache_creation_tokens`、`server_prompt_n`、`server_prompt_ms`、`server_cache_n`、`server_predicted_n`、`server_predicted_ms`；NULL 是不知道。评估从库与转储读这些列。**契约不变**：运行的表示（`GET /v1/runs/{id}`）不含步骤，事件 `idp.host/*` 不在契约里列举（与 D45、D52 一致） | 与 `latency_ms` 同处 |

**测过什么**：`internal/model` 的 `Stopwatch`（先发出再输出才有时间，首个文字与首个输出同一次读数，没有输出就缺失，nil 安全）；三个适配器各有：最后一块带用量与 `timings` 的流、没有它们的流（只有工具调用：首个文字缺失）、`cached_tokens: 0` 与字段缺失的区别、两次尝试各有自己的时间（OpenAI 另有非流式回答与只有 `timings` 没有 `usage` 的流）；harness：步骤与事件取成功的尝试，三次尝试的事件各记各的，没有提供者信息时字段缺失；PostgreSQL：步骤行读回，没说的是 NULL。**去掉首个输出的打点**（把适配器里的 `Output()`、`Text()` 调用去掉）：三个适配器各 3 到 4 个测试、harness 3 个测试失败。

**不知道的**：(1) 首个输出的时刻是适配器**读到**那一块的时刻，不是服务端写出的时刻；网络与代理的缓冲会让它偏晚，本机回环上可以忽略。(2) 一个服务端若把所有块攒到最后一起发（代理缓冲），首个输出就接近总时间，这与非流式不同，也无从分辨。(3) `cachedTokens` 与 llama.cpp 的 `cacheN` 在 llama.cpp 上是同一个数；留两份是因为别家只有前者。(4) Anthropic 与 Gemini 的字段没有对着真实服务验证。(5) 看不到提供者那一侧的排队时间（单槽服务上排在别的请求后面的时间算在首个输出里）。
