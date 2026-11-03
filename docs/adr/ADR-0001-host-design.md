# ADR-0001　IDP HOST 的设计（Agent Hosting & Coordination）

| 项 | 内容 |
|---|---|
| 状态 | 草案（2026-11-03），待决策人确认 |
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
- Skill 注册与版本（M2：Skill = 指令片段 + 工具子集 + 资源，按 release 引用）；A2A 委派；SDK 形态的 harness；Gemini 等更多适配器；智能模型路由（按成本 / 延迟 / 能力）。
- 与 EER 的 Execution / AgentRun 契约、与 CMI 的 Context API、与 Workspace 的联调——对方不存在，只留 `executionRef` 与 `context` 的位置。
- 评估门禁（固定评估集上的回归）、提示词注入检测、内容安全过滤。

## 4. 工程结构

仓库 `idp-host`，Go module `idp-host`，单个可执行程序 `cmd/idp-host`（`serve` / `migrate` / `ensure-login` / `seed-tenant` / `dev-token`）。目录：`internal/{agent,run,harness,model,mcpclient,api,agui,authorization,observability,transport,canonical}`、`adapters/{identity,store/postgres,providers/openai,providers/anthropic}`、`api/{openapi.yaml,contracts/}`、`migrations/`、`tests/{integration,e2e,fixtures}`、`deploy/`。单元测试放在被测包旁边。

## 5. 后果

- 正面：HOST 的全部行为由发布件决定并可复现；工具治理不重复；流与重放同源；多副本安全；身份链路上网关能看见"谁代表谁"。
- 负面：基础代码与 idp-mcp 重复；自研 harness 要自己跟进各厂商 API 的演进；模型重跑的费用重复由租约长度与步数上限兜底，不是 exactly-once。

## 6. M1 实施结果（2026-11-03，WSL 实测）

**通过**
- 单元（`make test`，-race）：AG-UI 消息与事件；AgentRelease 的规范化、密封、篡改与未知字段拒绝；OpenAI 兼容与 Anthropic 适配器（流式文本、工具调用、推理文本、用量、非流式回退、错误脱敏、重定向不跟随、端点策略、Anthropic 的交替合并规则）；MCP 客户端（JSON 与 SSE 两种应答、错误映射）；harness 8 个场景（纯回答、自动工具、确认→确认/拒绝、拒绝的工具、步数上限、前端工具、回退与模型失败、fenced 后不落任何终态、缺凭据）；契约一致性（示例生成、字段集与 required 等于 Go 结构体、13 种 Release 与 8 种 Run 变体双向拒绝）；OpenAPI 与路由一致；Kubernetes 清单一致性。
- 集成（真实 PostgreSQL，`make test-integration`，10/10）：无令牌的健康端点；发布与四眼；幂等创建（重放 200、不同内容 409、同线程同时只允许一个 Run）；SSE 流与分页；经静态凭据的工具调用；确认（确认、拒绝、过期）与从中断事件 id 续传；排队中取消；租约过期被第二个 Worker 接管、旧 Worker 的写入与心跳被拒；20 个并发相同请求只产生一个 Run；委派模式下网关收到换取的令牌；AG-UI 端点（流、前端工具、客户端回传整段对话时按消息 id 与工具调用 id 去重）；跨租户不可见、管理员可读本租户。
- 端到端（`deploy/e2e.sh`，compose：HOST 镜像 + idp-mcp 的发布镜像 + 两个真实 MCP SDK 上游 + 模型替身容器，13/13）：健康与认证；发布；AG-UI 运行经真实网关调用 `minimal__sum`（TOOL_CALL_RESULT 为真实结果）、文本流式、Run 记录带 release 摘要、线程完整；REST 运行触发确认、所有者确认后执行 `minimal__echo`；网关的调用审计记录了 HOST 服务身份的两次调用；指标暴露且不含主体。
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
