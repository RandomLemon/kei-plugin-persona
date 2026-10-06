# roadmap.md — 交付物现状与实现阶段（第 12 章）

本文覆盖第 12 章：当前交付物状态与后续实现阶段划分。

## 12.1 现状

**文档与代码均已落地。** 仓库包含 `AGENTS.md`、`docs/`、`LICENSE`，以及根包 `persona` 的完整实现（`register.go`、`plugin.go`、`config.go`、`persona.go`、`decision.go`、`state.go`、`llm.go`、`commands.go`、`policy.go`）与测试（`config_test.go`、`decision_test.go`、`llm_test.go`、`persona_test.go`、`policy_test.go`、`plugin_test.go`、`helpers_test.go`、`e2e_test.go`）。

`docs/` 是**验收基线**：实现必须与文档一致。配置键、提示词模板、决策参数、日志字段与 `/persona` 输出行都是逐字约定（范围与变更流程见 [`README.md`](README.md) 顶部与 §3）。

质量门与开发环境见 [`testing.md`](testing.md) §11.1。

## 12.2 实现阶段

以下阶段均已实现，保留作为实现顺序记录；每阶段都满足「可独立编译通过 + 测试全绿」。

### P1 骨架

- `go.mod`（`module github.com/RandomLemon/kei-plugin-persona`，`go 1.25.0`，`require github.com/RandomLemon/kei`）。
- `register.go`：`init()` 注册 + `Metadata`（见 [`architecture.md`](architecture.md) §3.1）。
- `plugin.go`：`Plugin` 结构、`Setup`/`Start`/`Stop`、从 `PluginContext` 装配 runtime 与注入缝。
- `config.go`：全部键读取与默认值、`loadConfig` 全部校验（见 [`configuration.md`](configuration.md) §10.4）。
- 注册两条规则 + 只记历史的 Handler（先不决策）+ `/persona status|on|off`。
- `config_test.go` 覆盖校验规则与固定错误文案。

### P2 决策

- `decision.go`：过滤、寻址判定、随机参与、批处理定时器（`onGroupMessage`/`schedule`/`onBatch`）。
- `state.go`：`channelState`、历史环、计数器、LRU、`st.epoch`、定时器管理。
- [`testing.md`](testing.md) §11.2 的决策类用例（reason 词表、寻址三种来源、概率边界、冷却/配额、静默时段、窗口合并、`st.epoch`）。

### P3 LLM

- `llm.go`：`completer` 接口与 `openaiClient`（请求构造、响应解析、超时、429/5xx 重试）。
- 回复清洗管线（[`persona.md`](persona.md) §8.7）。
- `llm_test.go`：`httptest.Server` 覆盖 200 / 429 / 500 / 400 / 非法 JSON / 缺 `choices` / 超时。

### P4 人格

- `persona.go`：`personas` 解析、`bindings` 匹配与优先级、`persona_template` 渲染、历史渲染。
- `/persona switch|reset` 子命令与持久化调用。
- `persona_test.go`：模板 11 个占位符、未知占位符保留、6 种回落占位符、清洗样例、命令输出。

### P5 持久化与联调

- Storage 懒加载（异步；未完成时把入站消息暂存进 `pending` 单槽、加载完成后补判，避免新会话第一条消息被丢弃，见 [`architecture.md`](architecture.md) §4.5）与写穿透（异步，1s 超时，失败 warn）。
- mock 适配器端到端（[`testing.md`](testing.md) §11.3）。
- 竞态与优雅关闭测试（[`testing.md`](testing.md) §11.4）。

### P6 私聊与名单策略

- `bot.WithKind(bot.MessagePrivate)` 私聊规则 `persona:private`：私聊视为寻址、必回（受 `mention_min_interval`/`mention_reply_probability` 约束），不进入随机路径（[`participation.md`](participation.md) §7.2-7.3）。
- 会话类型贯穿状态与渲染：`channelState.kind`/`peerUserID`、`{{chat_kind}}`（[`persona.md`](persona.md) §8.5）、历史块头 `[群聊记录]`/`[私聊记录]`（§8.6）、私聊发送目标 `message.Private` + `Target{UserID, Kind: MessagePrivate}`（§7.5）。
- `policy.go`：群聊/私聊各一套「模式 + 单列表」（`off`/`open`/`whitelist`/`blacklist`），`not_allowed` 在建立会话状态之前判定（[`participation.md`](participation.md) §7.8）。
- 配置键 `group_policy`/`group_list`/`private_policy`/`private_list`（[`configuration.md`](configuration.md) §10.1）与运行期命令 `/persona policy`、`/persona list`（[`persona.md`](persona.md) §8.4），写穿透持久化到 `persona:policy`，`Start` 同步恢复。

## 12.3 已知缺口

- **无插件级指标**：核心只暴露自身 Prometheus 指标；本插件的观测面是 `/persona status` 与结构化日志（[`participation.md`](participation.md) §7.7）。
- **多模态输入可选**：`llm_vision_enabled=true` 时插件下载最近入站图片（上限 `llm_vision_max_images`，每条消息最多 1 张，单张上限 `llm_vision_max_image_bytes`）并以 base64 内联发送给视觉模型，默认关闭（见 [`llm.md`](llm.md) §9.1）。`llm_vision_allowed_formats` 非空时只发送清单内的格式（跳过清单外格式，不转码）。
- **无流式输出**：单次阻塞式补全，不支持 SSE 流式。

## 12.4 假设与兜底

- 本次交付同时包含文档与代码（变更流程见 [`README.md`](README.md) 顶部）。
- 上游 kei 最新 tag 为 `v0.0.2`（`8747420`，早于当前 HEAD `f03daef` 三个提交：pluggable storage 与 `/manage` 命令命名空间）：宿主与本地开发用 `replace github.com/RandomLemon/kei => ../kei` 指向本地检出（契约版本与事实来源见 [`architecture.md`](architecture.md) §6）；上游出现更新的可用版本号时替换引用即可，不影响设计。
- 若 `pkg/bot` 在实现期缺少本设计所需 API（例如浮点读取），按 [`llm.md`](llm.md) §9.1 的写法改用 `Get` + 类型断言，不得引入第三方依赖。
