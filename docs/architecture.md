# architecture.md — 架构

本文覆盖第 1-6 章：项目定位与边界、架构与数据流、生命周期与并发模型、状态与持久化、目录与文件职责、与 kei 核心的契约对应。

## 1. 项目定位与边界

`kei-plugin-persona` 是 [`kei`](https://github.com/RandomLemon/kei) 的一个**进程内插件**（插件名 `persona`），在群聊里按人格预设偶尔插话，像群里一个普通真人。

**做什么**

- 作为 `bot.Plugin` 注册进 kei 引擎，监听群消息事件（`bot.EventMessage` + `bot.MessageGroup`）与私聊消息事件（`bot.EventMessage` + `bot.MessagePrivate`）。
- 维护每会话聊天历史，按「人格预设」渲染系统提示词，调用 OpenAI 兼容的 chat completions 接口生成一句话。
- 用一套可配置的接话决策模型决定「这一轮要不要说话、什么时候说」，包括 @ 寻址必回、私聊必回、随机参与、批处理窗口、冷却、小时配额、静默时段。
- 用「模式 + 单列表」的名单策略（`off`/`open`/`whitelist`/`blacklist`）按会话类型放行或拒绝：群聊按频道 ID、私聊按发送者 ID（见 [`participation.md`](participation.md) §7.8）。
- 提供 `/persona` 管理命令切换人格、开关、名单策略与查看状态。
- 用 `bot.Storage` 持久化每会话的运行时覆盖（人格、开关）与插件级名单策略。

**不做什么**

- 不做平台协议：与具体 IM 平台无关，只依赖 `pkg/bot` 抽象。
- 不做持久化数据库：只用 `bot.Storage` 键值存储，进程重启丢历史与计数器、保留覆盖与开关。
- 不引入 LLM SDK：LLM 调用用纯 `net/http`。
- 不引入第三方依赖：运行时仅标准库 + `github.com/RandomLemon/kei`。

**与 kei 的关系**

本插件是普通第三方进程内插件，与 kei 核心零耦合改动。kei 核心负责事件总线、路由、发送限流/降级/重试、权限裁剪、生命周期管理，本插件只消费 `PluginContext` 暴露的能力（见第 6 章）。

## 2. 架构与数据流

### 2.1 入站数据流与决策数据流

```text
                        ┌──────────────────────────── kei 核心 ────────────────────────────┐
平台/HTTP 注入 ──▶ Adapter ──▶ EventBus(分片保序) ──▶ Engine ──▶ Router ──▶ 命中规则
                                                                             │
                     ┌───────────────────────────────────────────────────────┘
                     ▼
        persona 消息 Handler（handleGroupMessage / handlePrivateMessage）
        · 只做：过滤 → 记历史 → 判决策 → 布防/并入定时器
        · 毫秒级返回，绝不在这里调用 LLM 或阻塞等待
                     │
                     ▼
        channelState（每会话状态，加锁保护）
                     │  定时器触发 onBatch
                     ▼
        后台生成协程 generate（由 Start 阶段保存的插件级 ctx 驱动）
        · 取全局信号量 → 解析人格 → 渲染提示词 → 调用 LLM
        · 回复清洗 → BotAPI.Send → 回写历史/计数器
                     │
                     ▼
        kei 核心发送链路（能力降级 → 令牌桶限流 → 退避重试）──▶ Adapter ──▶ 平台
```

两条路径的边界很清楚：

- **入站路径**（Handler 内）必须极快：事件总线按会话分片串行，一个 Handler 卡住会阻塞**同一会话**的后续事件；同时受 `RuleTimeout`（默认 10s）与 `EventTimeout`（默认 30s）约束。
- **决策路径**（后台协程）允许慢：LLM 调用、重试、发送都在这里，脱离 Handler 的 `ctx`，用插件级 `ctx` 派生超时。

### 2.2 Handler 必须非阻塞的三条理由

1. **规则超时**：`RuleTimeout` 默认 10s，Handler 超时会被中断，异步工作会丢。
2. **整条事件超时**：`EventTimeout` 默认 30s，Handler 慢会拖垮整条事件处理。
3. **会话保序**：事件总线按会话分片串行，Handler 阻塞会排队阻塞**同会话**的其它事件（不同会话并行）。

### 2.3 禁止用 Handler 的 ctx 做异步工作

Handler 收到的 `ctx` 在规则超时后会被取消。异步链路（定时器回调、生成协程、发送、Storage 写入）一律用 `Start` 阶段保存的插件级 `ctx` 派生 `context.WithTimeout`；**禁止 `context.Background()`**。插件级 `ctx` 在 `Stop` 时被 cancel，从而终止全部后台工作。

注意：`Start` 收到的同样是**阶段上下文**（阶段函数返回后 `defer cancel()` 立即取消，见 §3.1），因此插件级 `ctx` 必须写成 `context.WithCancel(context.WithoutCancel(ctx))`——直接保存阶段 ctx 会让全部异步链路当场失效。

## 3. 生命周期与并发模型

### 3.1 生命周期

`Plugin` 实例在 `init()` 时就得构造（`bot.RegisterPlugin(&Plugin{})`），那一刻没有配置也没有依赖。kei 生命周期固定为 `Setup → Start → Stop`，各阶段独立超时（默认各 15s）且带 panic 隔离；任一阶段返回错误都会阻止框架启动。

**`Setup(ctx, reg)`**

1. `pc, ok := bot.PluginContextFrom(ctx)`；取 `pc.Config`、`pc.Bot`、`pc.Logger`、`pc.Storage`。
2. `loadConfig(pc.Config)` 解析全部配置并逐条校验；配置非法即返回错误（kei 会终止启动，不做静默降级）。
3. 检查 `pc.HTTPClient != nil`（未声明 `network` 权限时为 nil）→ 否则返回错误 `persona: 需要 network 权限`。
4. 注册三条规则（逐字）：

```go
reg.OnEvent(bot.EventMessage, p.handleGroupMessage,
	bot.WithKind(bot.MessageGroup), bot.WithPriority(0), bot.WithID("persona:group"))

reg.OnEvent(bot.EventMessage, p.handlePrivateMessage,
	bot.WithKind(bot.MessagePrivate), bot.WithPriority(0), bot.WithID("persona:private"))

reg.OnCommand("persona", p.handleCommand,
	bot.WithAdmin(), bot.WithPriority(100), bot.WithID("persona:admin"))
```

5. 构造 runtime：注入缝 `now`/`randFloat`/`completer`、全局信号量、`map[string]*channelState`、名单策略 `policyState`（初值取自配置键，见 [`participation.md`](participation.md) §7.8）。

**`Start(ctx)`**

1. 保存插件级 `ctx` 与 `cancel`：`p.ctx, p.cancel = context.WithCancel(context.WithoutCancel(ctx))`。kei 传入的是**阶段上下文**（阶段函数返回后即被 cancel），直接用会让全部异步链路当场失效，故用 `context.WithoutCancel` 摘掉取消与超时（保留值），再由 `Stop` 经 `p.cancel` 终止后台工作。
2. 同步读取插件级名单策略覆盖（`loadPolicy`，`Storage.Get` 带 1s 超时，失败只 `Warn` 并保留配置默认值）。同步是为了避免「策略未就绪窗口」内误放行/误拒；这一步用**阶段 ctx**（受阶段 15s 预算约束）。
3. 异步恢复 Storage 覆盖（每会话懒加载，见第 4 章）。
4. 启动完成，等待事件。

**`Stop(ctx)`**

1. 标记关闭（`closing`，此后不再接受新的写穿透）并 `p.writeWG.Wait()`：**先**等在途写穿透落库，**再**取消插件级 ctx。写穿透的 ctx 派生自 `p.ctx`，顺序反了会让持久化后端（sqlite/mysql）上的最后一次覆盖/策略写入被当场取消丢弃（见 §4.6）。
2. `p.cancel()`：取消插件级 ctx，终止全部派生工作。
3. 停止全部 `channelState.timer`（每个 `time.Timer.Stop()`）。
4. `p.wg.Wait()`：等待全部生成协程退出。
5. 幂等：重复调用安全。不需要额外 flush——每次变更都已触发写穿透，第 1 步只等在途的那一次（见第 4 章）。

`register.go` 的注册与元信息（逐字）：

```go
var _ bot.Plugin = (*Plugin)(nil)

func init() { bot.RegisterPlugin(&Plugin{}) }

func (p *Plugin) Metadata() bot.Metadata {
	return bot.Metadata{
		Name:        "persona",
		Version:     "v0.1.0",
		Author:      "RandomLemon",
		Description: "LLM 人格代理：在群聊中按人格预设偶尔参与对话",
		Permissions: []bot.Permission{bot.PermNetwork, bot.PermStorage, bot.PermSendMessage},
	}
}
```

### 3.2 协程清单与并发上限

| 协程 | 生命周期 | 上限 |
| --- | --- | --- |
| 每会话生成协程 | 定时器触发时 `wg.Add(1)` 启动，`generate` 返回时 `Done` | 每会话至多 1 个（`inflight` 保证）；全局至多 `limits_max_concurrent` 个真正调用 LLM 的协程 |
| 写穿透持久化协程 | 每次状态变更时启动，写完后退出 | 每变更一个，数量由变更频率决定；单次 `Set` 带 1s 超时；由 `writeWG` 跟踪，`Stop` 在取消插件级 ctx 之前等它们结束 |

全局信号量 `limits_max_concurrent`（默认 2）用**非阻塞获取**（`TryAcquire`）；获取失败记 `semaphore_full` 并放弃本轮，不排队。

### 3.3 并发表

| 共享状态 | 保护方式 | 说明 |
| --- | --- | --- |
| `map[string]*channelState` | `sync.RWMutex`（挂在 `Plugin` 上） | 读多写少；新增/淘汰会话时写锁 |
| `channelState` 内部字段 | `channelState.mu sync.Mutex` | 每个会话独立锁，避免全局锁竞争 |
| 全局计数器（`/persona status` 汇总行） | `sync/atomic` | 只增不减的累计值 |
| 最近发送消息 ID 环（reply 寻址用） | `sync.Mutex` | 容量常量 128 的环形缓冲 |
| 写穿透关闭标记 `closing`（配 `writeWG`） | `sync.Mutex` + `sync.WaitGroup` | 写穿透与 `Stop` 用它串行化「是否已进入关闭」，保证在途写入不被取消（见 §3.1/§4.6） |

## 4. 状态与持久化

### 4.1 `channelState`

每会话一份状态，字段逐字如下（方法名见本节末）：

```go
type channelState struct {
	mu         sync.Mutex
	key        string // platform:botID:channelID（Channel 为空时 platform:botID:user:<senderID>）
	platform   string
	botID      string
	channelID  string
	kind       bot.MessageKind // 会话类型：group 或 private
	peerUserID string          // 私聊对端用户 ID（kind == private 时非空）
	history    []Turn      // 环形，上限 context_max_messages
	replyTimes []time.Time // 本会话近 1 小时回复时间窗，用于小时配额
	lastReplyAt time.Time
	awaiting   bool
	firstAt    time.Time
	lastMsgAt  time.Time
	timer      *time.Timer
	inflight   bool
	epoch      uint64
	disabled   bool
	persona    string // 运行时覆盖的人格名，空 = 未覆盖
	loaded     bool   // Storage 懒加载是否完成
	pending    *pendingInbound // 懒加载完成前暂存的待决入站消息（有界单槽）
	lastSenderID string // 最近一条入站消息的真人发送者 ID（用于 reply_mention_sender）
	lastAddressed bool  // 触发本轮生成的那条消息是否被判定为寻址
	pendingKind  string // "addressed" | "random"，本次生成的触发来源（日志用）
	replies    int // 已回复计数（累计）
	skips      int // 因 LLM 放弃/空回复/重复而跳过的计数
	llmErrors  int
	dropped    int // 信号量满或 stale 丢弃的计数
}
```

字段语义要点：

- `history` 环形，容量 `context_max_messages`，`appendHistory` 超出后从最旧丢弃。
- `replyTimes` 只保留近 1 小时（`repliesInWindow` 读取时顺带裁剪）。
- `epoch` 是「状态代次」：`/persona off`、`/persona switch`、`/persona reset` 递增。定时器回调与生成协程捕获发起时的 `epoch`，发现不一致就丢弃结果（reason `stale`）。
- `loaded` 表示 Storage 懒加载是否完成。未完成前 Handler 不阻塞等待：把本条消息**暂存进 `pending` 单槽**并记 reason `loading`，加载完成后由 `restoreState` 调 `decide` **补判一次**（见 §4.5）。
- `pending` 是「懒加载期间暂存待判消息」的有界单槽（`*pendingInbound{ev, text, epoch}`）：加载完成前同一会话只保留最新一条，更早的已进 `history`、仍随本轮生成交给 LLM。暂存/取走同在 `st.mu` 临界区内，消息要么立即判定、要么恰好补判一次，不会被吞掉；`epoch` 变化（`/persona reset`）时暂存作废。带 `pending` 的状态不参与 LRU 淘汰（见 §4.3）。
- `disabled` 表示该会话被 `/persona off` 关闭。
- `kind` 是会话类型（`bot.MessageGroup` / `bot.MessagePrivate`），决定寻址判定（私聊视为寻址）、发送目标（`message.Group` / `message.Private`）与提示词/历史渲染的会话类型文案。
- `peerUserID` 是私聊对端用户 ID，私聊发送目标填 `bot.Target.UserID`（`ChannelID` 留空）。

方法名固定为：`appendHistory(Turn)`、`snapshotHistory(max int) []Turn`、`repliesInWindow(now time.Time, d time.Duration) int`、`distinctHumans(now time.Time, d time.Duration) int`。计数器名固定为 `replies`、`skips`、`llmErrors`、`dropped`（另有全局原子计数用于 `/persona status` 的汇总行）。

### 4.2 会话键

群聊会话键为 `platform:botID:channelID`；当 `ev.Channel == nil`（私聊等无频道信息的事件）时回落 `platform:botID:user:<senderID>`。键写入 `channelState.key`，日志字段 `channel=<key>` 与覆盖键都由它派生。

### 4.3 LRU 淘汰

内存中最多跟踪 `context_max_channels`（默认 512）个会话。超限时按最近使用时间淘汰，**只淘汰非 `inflight`、非 `awaiting` 且无 `pending` 的状态**（有在途生成、已布防定时器或待补判消息的会话不淘汰；淘汰带 `pending` 的状态等于丢掉那条消息）。淘汰只释放内存，不影响已持久化的覆盖。

### 4.4 Storage 键与 JSON 形状

- 会话覆盖键：`persona:override:<会话键>`（会话键见 §4.2）。
- 会话覆盖值（JSON）：`{"persona":"...","disabled":false}`。`persona` 为空表示无覆盖，`disabled` 为 `true` 表示该会话被关闭。
- 插件级名单策略键：`persona:policy`。
- 名单策略值（JSON）：`{"group_mode":"open","group_list":[],"private_mode":"off","private_list":[]}`。语义见 [`participation.md`](participation.md) §7.8；列表为 `null`/缺键表示无覆盖（保留配置默认值），为 `[]` 表示显式清空。
- 无 TTL：覆盖、开关与名单策略的 `Set` 一律传 `ttl = 0`（永不过期）；能否跨进程重启保留取决于宿主存储后端（见 §4.7）。

### 4.5 懒加载异步化

首次见到某会话时**异步**触发一次 `Storage.Get`（读该会话的覆盖键）：

- 加载完成前，Handler 只记历史、不判决策：记 reason `loading`，同时把该条消息**暂存进 `channelState.pending` 单槽**（见 §4.1）。Handler 禁止阻塞，因此不能同步等待 `Get`。
- 加载完成（`finishLoad`）：写回 `persona`、`disabled`，置 `loaded = true`，并在同一临界区内取走 `pending`；取到则用插件级 ctx 调 `decide` **补判一次**（此时判定依据的是加载后的真实打开/关闭状态与人格覆盖，因此不会在 `/persona off` 的会话里发言）。补判只做内存判定与定时器布防，不做网络调用。
- 效果：新会话（首次出现、进程重启后、被 LRU 淘汰后）的**第一条消息不再被静默吞掉**；加载期间连发的消息只补判最新一条，更早的仍随本轮生成进入 LLM 上下文。
- 加载失败（含 `bot.ErrNotFound`）：视为无覆盖，同样置 `loaded = true` 并补判，只记 `Debug`/`Warn`。
- `p.ctx` 为 nil 或 `p.store` 为 nil 时跳过 `Get`，仍然走 `finishLoad`（补判照常生效）。

插件级名单策略是**例外**：在 `Start` 阶段同步读一次（见 §3.1），不参与每会话懒加载。

### 4.6 写穿透异步化

内存是唯一事实来源。每次覆盖/开关变更后**异步**启动一次 `Storage.Set`；名单策略变更（`setPolicyMode`/`addPolicyID`/`delPolicyID` 且真的发生变更时）走同形的写穿透：

- 写操作带 1s 超时（派生自插件级 ctx）。
- 失败只记 `Warn`，不重试、不回滚（内存仍为准）。
- `Stop` 会先置 `closing`（拒绝新写入）并 `writeWG.Wait()`，**然后**才取消插件级 ctx；因此「写入后立即关闭」在持久化后端上不会丢数据（内存后端观察不到这个差别）。每次写入自带 1s 超时，故这次等待有界。

### 4.7 重启语义

进程重启后：历史与计数器丢失（内存态，不落 Storage）；覆盖、开关与名单策略的存续取决于宿主的 `bot.Storage` 后端——`storage.type: memory`（默认）随进程消失，`sqlite`/`mysql` 等持久后端保留（见 §6 与 kei 仓库 `docs/plugin.md` §11.2）。

## 5. 目录与文件职责

```text
kei-plugin-persona/
├── go.mod           module github.com/RandomLemon/kei-plugin-persona（require kei）
├── flake.nix        nix devShell（go / gopls / golangci-lint / dlv / jq / curl）
├── flake.lock       flake 输入锁（nixpkgs）
├── .envrc           direnv：进入目录自动执行 `nix develop`
├── .gitignore       忽略 .direnv/、/bin/、*.test 等本地产物
├── register.go      init() 注册 + Metadata（插件名 persona、权限声明）
├── plugin.go        Plugin 结构、Setup/Start/Stop、runtime 装配
├── config.go        配置结构、读取与默认值、校验（loadConfig）
├── persona.go       人格预设解析、绑定匹配、提示词模板渲染
├── decision.go      接话决策：过滤、寻址判定、随机参与、批处理定时器
├── state.go         每会话状态、历史环、计数器、Storage 读写
├── llm.go           OpenAI 兼容客户端（请求/响应/重试/超时）
├── commands.go      /persona 管理命令
├── policy.go        插件级名单策略：模式判定、/persona policy|list 渲染、persona:policy 读写
├── config_test.go   配置解析与校验单测
├── decision_test.go 决策模型单测（过滤/寻址/随机/窗口/epoch/名单/私聊）
├── llm_test.go      LLM 客户端单测（httptest.Server）
├── persona_test.go  人格解析、模板渲染、历史渲染、清洗单测
├── policy_test.go   名单策略单测（模式矩阵/命令输出/持久化/启动恢复）
├── plugin_test.go   生命周期单测（阶段 ctx 取消后运行期仍可用、Stop 等待在途写穿透）
├── helpers_test.go  测试桩与测试环境构造
├── e2e_test.go      Mock 适配器端到端测试
├── docs/            设计文档（本目录即实现口径）
├── AGENTS.md        硬性规则与结构概览
└── LICENSE          MIT
```

| 文件 | 职责（一句话） |
| --- | --- |
| `register.go` | 插件自注册与 `Metadata`（名称、版本、作者、描述、权限声明）。 |
| `plugin.go` | `Plugin` 结构定义、`Setup`/`Start`/`Stop`、从 `PluginContext` 装配 runtime 与注入缝。 |
| `config.go` | 从 `*bot.Config` 读取全部键、填默认值、`loadConfig` 校验并返回 `config` 结构。 |
| `persona.go` | `personas`/`bindings` 解析、人格解析优先级、`persona_template` 渲染、历史渲染、回复清洗。 |
| `decision.go` | 群聊/私聊消息过滤、名单放行判定、寻址判定、随机参与判定、批处理定时器（`handleChat`/`schedule`/`onBatch`/`generate`）。 |
| `state.go` | `channelState`、历史环、计数器、LRU、Storage 懒加载/写穿透。 |
| `llm.go` | `completer` 接口与 `openaiClient` 实现（请求构造、响应解析、超时重试）。 |
| `commands.go` | `/persona status|persona|on|off|reset|policy|list` 子命令实现。 |
| `policy.go` | 插件级名单策略：`policyState`/`policyValue`、四值模式放行判定、`persona:policy` 读写、`/persona policy|list` 报告渲染。 |
| `*_test.go` | 按文件名的领域单测；`e2e_test.go` 走 mock 适配器端到端。 |

## 6. 与 kei 核心的契约对应

左列是本插件的假设/用法，右列是 kei 的事实来源（版本 `f03daef5bb52bec9eb6198126cc01e67324f1616`，即上游 HEAD；本仓库本地开发经 `go.mod` 的 `replace` 指向同级检出，见 [`../AGENTS.md`](../AGENTS.md) §2.2）。上游最新 tag `v0.0.2` 指向 `8747420`，早于 HEAD 三个提交；本表除 storage 两行外在该 tag 上同样成立。章节号与文件路径均按 HEAD 核对。

| 本插件的假设/用法 | kei 的事实来源 |
| --- | --- |
| 插件名 `persona` 在 `init()` 里经 `bot.RegisterPlugin` 注册；宿主空导入本包 + 配置 `plugins.persona.enabled: true` 即启用。另一种接入是经装配门面的 `kei.Options.Plugins` 注入 `&persona.Plugin{}`（注入实例一律启用；在配置里写 `enabled: false` 时启动失败，同名时注入优先、跳过注册表实例）；配置启用但未注册只记 warn `enabled plugin is not registered` | `pkg/bot/plugin.go` `RegisterPlugin`/`RegisteredPlugins`；`pkg/kei/assemble.go` `selectPlugins`；`docs/plugin.md` 第 9 章 |
| 插件生命周期固定 `Setup → Start → Stop`，各阶段默认 15s 超时、带 panic 隔离，任一阶段返回错误阻止启动 | `pkg/bot/plugin.go`；`internal/pluginmgr/manager.go` `defaultSetupTimeout`/`defaultStartTimeout`/`defaultStopTimeout` |
| `Start` 阶段可安全做一次同步 `Storage` 读（阶段预算 15s，本插件 `loadPolicy` 自设 1s 超时，超时只 `Warn`） | `internal/pluginmgr/manager.go` `defaultStartTimeout` |
| 传给 `Setup`/`Start`/`Stop` 的 ctx 都是**阶段上下文**：`run` 内 `defer cancel()`，阶段函数一返回就取消。故插件级 ctx 必须由 `context.WithoutCancel` 派生（本插件 `Start` 的做法） | `internal/pluginmgr/manager.go` `run` |
| `Metadata.Permissions` 决定依赖裁剪：未声明 `network` → `PluginContext.HTTPClient == nil`；未声明 `storage` → 注入拒绝式 `Storage`；未声明 `send_message` → `BotAPI.Send` 报 `engine: plugin %s lacks permission send_message` | `internal/pluginmgr/manager.go` `contextFor`（`storage.Denied()`）；`internal/engine/pluginapi.go` |
| `Reply` 所有插件都可用，不受 `send_message` 限制 | `pkg/bot/reply.go`；`internal/engine/pluginapi.go` |
| 本插件申请 `network`/`storage`/`send_message` 三项权限 | `register.go` 的 `Metadata`；权限常量见 `pkg/bot/plugin.go` |
| `WithKind(bot.MessageGroup)` 只比较 `ev.Message.Kind`；`ev.Message == nil` 时不命中 | `pkg/bot/registrar.go` `Rule.Matches` |
| `WithKind(bot.MessagePrivate)` 同样只比较 `ev.Message.Kind`，故群聊与私聊各注册一条规则（`persona:group` / `persona:private`），互不命中 | `pkg/bot/registrar.go` `Rule.Matches` |
| `WithAdmin()` 需核心 `auth.admin_users` + Auth 中间件，按 `Event.Sender.ID` 精确比较 | `docs/plugin.md` §9.2（`WithAdmin` 选项）；`docs/engine.md` §10.5（`isAdmin` 判定）；`docs/configuration.md` §12.1 |
| `OnCommand("persona", ...)` 的规则与群消息规则会被**同时执行**（同一事件命中多条规则全部执行，`Priority` 只影响顺序、不短路，错误 `errors.Join` 聚合），故 Handler 必须无条件丢弃 `Command.Name == "persona"` | `internal/router/router.go` `Dispatch` |
| 命令前缀默认 `["/"]`；`Command{Name,Args,Raw}`，`Name` 不含前缀；带 `/` 前缀的文本会被填 `ev.Command` | `internal/engine/engine.go`；`pkg/bot/event.go` |
| Handler 运行在事件总线分片 worker 中：同会话串行、不同会话并行（默认 4 worker × 256 队列） | `docs/engine.md` §7.5/§8.4；`internal/eventbus/bus.go` |
| `RuleTimeout` 默认 10s、`EventTimeout` 默认 30s；故 Handler 必须非阻塞 | `internal/engine/engine.go` |
| 发送走核心链路：能力降级、每 bot 令牌桶限流（`limits.send_rate`）、退避重试（`SendRetries` 默认 2），插件不重复实现 | `docs/engine.md` 7.7 |
| `bot.Target{Platform,BotID,ChannelID,Kind}` 显式构造发送目标（不用 `TargetFromEvent`，避免群聊带上 `UserID`） | `pkg/bot/adapter.go` |
| 私聊发送用 `message.Private(segs...)` + `bot.Target{Platform,BotID,UserID,Kind: bot.MessagePrivate}`（**不填** `ChannelID`） | `pkg/message/message.go`；`pkg/bot/adapter.go` |
| 优雅关闭：停适配器 → 排空事件总线 → 逆序 `Stop` 插件；`Stop` 之后再发送没有意义 | `docs/engine.md` 7.3 |
| 插件配置来自 `plugins.persona` 下除 `enabled` 的其余键，经 `PluginContext.Config` 读取 | `docs/configuration.md` 12.3；`internal/engine/engine.go` → `pluginmgr.Deps.Configs` |
| 环境变量 `KEI_PLUGINS_PERSONA_<KEY>` 把**扁平键**写入 `Settings`；故本插件全部用扁平 `snake_case` 键 | `internal/config/env.go` `applyPluginEnv`/`setSetting` |
| `bots[].plugins` 白名单由引擎自动收窄规则适用范围，插件无需自己过滤 bot | `docs/configuration.md` 12.2 |
| 插件不得读环境变量/文件，配置只经 `PluginContext.Config` | kei `AGENTS.md` 2.1 |
| 部分 `Config` 方法：`Get` 支持 `"a.b"` 多级路径、`Duration` 支持 `"90s"` 字符串与「数字=秒」、`Strings` 支持 `[]any` 与逗号分隔字符串；无 `Float`（浮点用 `Get` + 类型断言） | `pkg/bot/api.go` |
| `Config.Strings` 的 `[]any` 分支只保留 `string` 元素，**裸数字被静默丢弃**；故 `[]string` 键（`self_ids`/`trigger_keywords`/`group_list`/`private_list`）一律经本插件的 `readStringList` 读取 | `pkg/bot/api.go`；[`configuration.md`](configuration.md) §10.4 注 |
| OneBot 把 `@全体成员` 上报为 `at` 段 `Data[bot.KeyUserID] == "all"`（非真实用户 ID）；`at` 段标准键为 `qq`、部分实现用 `user_id`，两者适配器都归一为 `KeyUserID` | `kei/adapters/onebot/message.go` `convertSegment` |
| `Storage.Get` 键不存在时返回可被 `errors.Is(err, bot.ErrNotFound)` 识别的错误 | `pkg/bot/api.go` |
| `bot.Storage` 的后端由宿主选择：配置 `storage.type`（`memory` 默认 / `sqlite` / `mysql`）或 `kei.Options.Storage` 注入（非 nil 优先、由调用方拥有、`Run` 不关闭）；插件只见接口，各后端读写语义一致 | `pkg/kei/assemble.go` `buildStorage`；`pkg/kei/kei.go` `Options.Storage`；`internal/storage`；`docs/plugin.md` §11.2；`docs/configuration.md` §12.5 |
| `Storage.Set` 的 `ttl <= 0` 表示永不过期（本插件一律传 `0`）；内存后端重启即丢、持久后端保留（见 §4.7） | `pkg/bot/api.go`；`docs/plugin.md` §11.2 |
| 核心仅暴露自身指标（Prometheus 文本），插件的观测面是 `/persona status` 与结构化日志 | `internal/metrics`；本设计 §8.4 |
