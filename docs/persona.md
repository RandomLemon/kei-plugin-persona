# persona.md — 人格系统（第 8 章）

本文覆盖第 8 章：人格预设库、绑定与解析优先级、运行时覆盖与 `/persona` 命令、系统提示词模板、历史渲染、回复清洗。配置键名与默认值以 [`configuration.md`](configuration.md) §10.1 为唯一权威；LLM 请求构造见 [`llm.md`](llm.md) §9.1。

## 8.1 预设库

`personas` 是 `map[string]Persona`，值支持两种形态：

- **字符串**：等价于只写 `prompt`。
- **映射**：字段如下。

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `prompt` | string | 无（必填） | 注入 `{{persona}}` 的人格描述文本 |
| `display_name` | string | = 预设名 | 该人格显示名，用于 `{{persona_name}}`、历史中自己发言的渲染名 |
| `temperature` | float | `llm_temperature` | 该人格的采样温度覆盖 |
| `max_tokens` | int | `llm_max_tokens` | 该人格的最大生成 token 覆盖 |
| `skip_token` | string | `llm_skip_token` | 该人格的 skip 哨兵覆盖 |

示例：

```yaml
personas:
  default:
    prompt: 普通的群友，说话随意。
  tsundere:
    prompt: 傲娇，嘴硬但对人不错，偶尔吐槽。
    display_name: 小傲娇
    temperature: 0.9
  deadpan: 冷淡、话少，常用短句。
```

## 8.2 命名与校验

- `default_persona` 默认 `default`。
- 必须存在 `personas[default_persona]`，否则 `Setup` 返回错误并点名缺失的预设。
- `bindings[].persona` 与 `/persona switch <name>` 引用的名字必须存在，否则报错（配置阶段）或命令回绝（运行阶段）。
- `prompt` 为空的条目视为错误并点名。
- 校验错误文案见 [`configuration.md`](configuration.md) §10.4。

## 8.3 绑定与解析优先级

人格解析顺序固定为：

```text
运行时覆盖（Storage / 内存） > bindings 命中 > default_persona
```

`bindings` 是一组行，每行 `{platform, bot_id, channel_id, persona}`：

- 行的每个**非空**字段（`platform`/`bot_id`/`channel_id`）必须与事件对应字段相等才算命中；未写的字段不参与匹配。
- `channel_id` **必填**（空则配置非法）。
- 多行命中时取**非空字段最多**的一行；仍并列则取列表中靠前的一行。

示例绑定与「谁生效」判定：

```yaml
bindings:
  - { channel_id: "g1", persona: tsundere }
  - { platform: "feishu", channel_id: "g1", persona: deadpan }
  - { platform: "feishu", bot_id: "feishu-main", channel_id: "g2", persona: deadpan }
```

| 事件 | 命中行 | 生效人格 |
| --- | --- | --- |
| 平台 `onebot`、频道 `g1` | 第 1 行（1 个非空字段） | `tsundere` |
| 平台 `feishu`、频道 `g1` | 第 1、2 行（第 2 行非空字段更多） | `deadpan` |
| 平台 `feishu`、bot `feishu-main`、频道 `g2` | 第 1、3 行（第 3 行非空字段更多） | `deadpan` |
| 平台 `onebot`、频道 `g9` | 无 | `default_persona` |
| 平台 `onebot`、频道 `g1`，且已 `/persona switch default` | 覆盖优先 | `default` |

## 8.4 运行时覆盖与 `/persona` 命令

命令注册（逐字）：

```go
reg.OnCommand("persona", p.handleCommand,
	bot.WithAdmin(), bot.WithPriority(100), bot.WithID("persona:admin"))
```

`WithAdmin()` 需要核心 `auth.admin_users` 配置 + Auth 中间件，按 `Event.Sender.ID` 精确比较（见 [`architecture.md`](architecture.md) 第 6 章）。

| 子命令 | 行为 |
| --- | --- |
| `status` | 输出固定字段顺序的一行（见下） |
| `switch` | 打印当前预设名 + 来源 |
| `switch <name>` | 校验存在 → 写运行时覆盖 → 持久化 → `epoch++` |
| `on` | 置 `disabled=false`，持久化 |
| `off` | 置 `disabled=true`，停止定时器，`epoch++`，持久化 |
| `reset` | 清历史/覆盖/计数器，置 `on`，`epoch++`，持久化 |
| `policy` | 打印群聊/私聊名单模式与名单条数 |
| `policy <group\|private> <mode>` | 设置该侧模式（`off`/`open`/`whitelist`/`blacklist`）→ 写穿透持久化 |
| `list` | 打印群聊/私聊名单 |
| `list <group\|private>` | 打印单侧名单 |
| `list <group\|private> add\|del <id>` | 追加/移除名单项 → 写穿透持久化 |
| 未知子命令 | 回用法文本 |

命令回复用注入的 `Reply`；命令**不做 LLM 调用、不进历史**。

用法文本（逐字）：

```text
用法: /persona status | switch [name] | on | off | reset | policy [group|private mode] | list [group|private [add|del id]]
```

`policy`/`list` 的输出行（逐字，语义与空名单行为见 [`participation.md`](participation.md) §7.8）：

```text
/persona policy                  → persona: group=open(0) · private=off(0)
/persona policy group whitelist  → persona: group=whitelist(0)
/persona list                    → persona: group=[g1 g2] private=[]
/persona list group add g9       → persona: group=[g1 g2 g9]
```

- 这两个子命令同样只走 `bot.WithAdmin()`（仅管理员），不做 LLM 调用、不进历史。
- 参数非法或缺参回用法文本；`add` 已存在、`del` 不存在均幂等（回同一行，不产生写穿透）。
- 策略是**插件级**（不是每会话），持久化键 `persona:policy`（[`architecture.md`](architecture.md) §4.4）。

**`/persona status` 输出（字段顺序固定，`·` 分隔，逐字）**：

```text
persona: 开 · persona=tsundere(override) · 历史 18 条 · 近 1 小时回复 3/6 · 上次回复 42s 前 · llm 错误 0 · 已跳 12
```

字段含义依次为：开关（`开`/`关`）、当前人格与来源（`(override)`/`(binding)`/`(default)`，无覆盖时为 `(binding)` 或 `(default)`）、当前历史条数、近 1 小时回复数/上限（`3/6`）、距上次回复的相对时间（`42s 前`，无回复时写 `从未`）、LLM 错误数、跳过数。

**`/persona switch`（无参数）输出（逐字）**：

```text
persona: persona=tsundere 来源=override
```

`来源=` 取值固定为 `override`/`binding`/`default`。

## 8.5 系统提示词模板

`persona_template` 默认值请参考 `persona.go` 中的 `defaultPersonaTemplate` 字段。

**占位符全集（恰好这 11 个）**：

| 占位符 | 取值 |
| --- | --- |
| `{{persona}}` | 当前人格的 `prompt` |
| `{{persona_name}}` | 当前人格名 |
| `{{chat_kind}}` | 会话类型文案：群聊为 `群聊`、私聊为 `私聊` |
| `{{channel_name}}` | `ev.Channel.Name`（私聊取发送者显示名）；空则回落 `{{channel_id}}`；再空则回落 `私聊` |
| `{{channel_id}}` | `ev.Channel.ID` |
| `{{platform}}` | `ev.Platform` |
| `{{bot_name}}` | `ev.BotID` |
| `{{now}}` | 按 `random_timezone` 的当前时间，格式 `2006-01-02 15:04` |
| `{{last_sender}}` | 历史中最后一条 `Self == false` 的 `Turn.Name`（空则用其 `UserID`）；没有他人消息时回落 `未知` |
| `{{max_chars}}` | `reply_max_chars` |
| `{{skip_token}}` | 当前人格生效的 skip 哨兵 |

渲染规则：

- 用 `strings.NewReplacer` **一次性**替换全部占位符，避免替换值互相污染。
- 未知占位符**原样保留**（不做删除、不报错）。
- `{{now}}` 用 `random_timezone` 解析出的时区。

## 8.6 历史渲染

`Turn` 结构：

```go
// TurnPart 是 Turn 中的一段内容，按原消息段顺序排列。
type TurnPart struct {
	Kind string // "text" 或 "image"
	Text string // Kind=="text" 时有效
	URL  string // Kind=="image" 时有效；抽取不到 URL 时为空串
}

type Turn struct {
	At     time.Time
	UserID string
	Name   string
	Text   string
	Self   bool // 是否本插件自己发送
	IsBot  bool // 发送者是否为机器人

	Parts []TurnPart // 原消息的分段结构（含图片槽位）；为空表示无图片段，按 Text 渲染
}
```

- **单条渲染**：`<显示名>: <文本>`，显示名取 `Name` → `UserID` → `未知`。自己发送的用当前人格 `display_name` 渲染；LLM 侧无角色区分（全部作为 user 消息的一部分）。
- **`renderMessage(*bot.Message)`**（返回扁平文本与分段结构 `[]TurnPart`，单遍产出）：
  1. 按顺序拼接 `SegText`/`SegMarkdown` 的 `Data[bot.KeyText]`；
  2. 每个 `SegAt` 写 `@<name 或 user_id> `（`Data[bot.KeyUserName]` 为空时用 `Data[bot.KeyUserID]`）；
  3. 每个 `SegImage` 在扁平文本里写 `[图片]`（与 `llm_vision_enabled` 无关，多图多占位），并在 `Parts` 的同一位置追加一个 `image` 段（`URL` 由 `imageSegURL` 抽取，抽取不到为空串）；相邻文本合并成一个 `text` 段；
  4. 文本为空时按首个非文本段回落：`[图片]`/`[表情]`/`[文件]`/`[卡片]`/`[引用]`/`[消息]`；纯图片消息不走这条回落，其扁平文本即 `[图片]`、`Parts` 为单个 `image` 段；
  5. 无图片段时 `Parts` 为 `nil`，调用方把整个条目按扁平文本渲染成单个 `text` 块。
- **`imageSegURL(bot.Segment)`**：抽取单个图片段中可用的 URL（`Data[bot.KeyURL]` 非空即取；否则仅当 `Data[bot.KeyFile]` 以 `http://`/`https://` 开头才取）；**仅供多模态注入用**（见 [`llm.md`](llm.md) §9.1）。下载与 base64 编码在 `vision.go`（见 [`architecture.md`](architecture.md) §5），失败时该槽位回落字面量 `[图片]`（也不回落成原始 URL）。自己发送的 `Turn` 不带 `Parts`。
- **最终 user 消息内容** = 首块历史头 + 其后按时间序每条历史自己的块。头按会话类型渲染：群聊 `[群聊记录]`、私聊 `[私聊记录]`；每条历史的文本合并成一个 `text` 块（该条首个 `text` 块带 `<显示名>: ` 前缀），图片在它原本的消息位置产出 `image_url` 块（槽位无可用 data URL 时以字面量 `[图片]` 留在文本里）。
- **裁剪**：超过 `context_max_messages` 条、或超过 `llm_history_max_chars` 字符（按 **rune** 计）时从最旧丢弃，**始终保留最新一条**。
- `Turn.At` 取 `ev.Time`，为零值时取 `p.now()`。

完整示例块（三条历史，其中最新一条带一张可取图片）：

```text
[{"type":"text","text":"[群聊记录]"},
 {"type":"text","text":"张三: 今晚谁去打球"},
 {"type":"text","text":"李四: 我可能不行"},
 {"type":"text","text":"我: 打球可以啊"},
 {"type":"text","text":"张三: 那定八点，看这个"},
 {"type":"image_url","image_url":{"url":"data:image/png;base64,..."}}]
```

（`我` 用当前人格 `display_name` 渲染。`llm_vision_enabled=false`、图片不可取、或该槽位超出 `llm_vision_max_images` 时，最后一个块不存在，那条历史的文本块为 `"张三: 那定八点，看这个[图片]"`。）

`channelState.distinctHumans(now, d)` 只统计 `Self == false && IsBot == false` 且时间在窗口内的 `Turn`，按 `UserID` 去重（`UserID` 为空时按下标计一条）。

## 8.7 回复清洗管线

对 `choices[0].message.content` 逐步处理，任一步丢弃即返回对应 reason 且不发送：

| 步 | 操作 | 丢弃时 reason |
| --- | --- | --- |
| 1 | 取 `choices[0].message.content`，首尾去空白 | — |
| 2 | 结果为空 → 丢弃 | `empty_reply` |
| 3 | 含当前人格生效的 `skip_token` → 丢弃 | `skipped_by_llm` |
| 4 | 成对包裹的 `"` / `“”` / `「」` / `『』` / `'` 剥掉一层 | — |
| 5 | 所有换行换成空格（群聊一行话），连续空格压成一个 | — |
| 6 | 按 rune 截断到 `reply_max_chars`（硬截断，不加省略号） | — |
| 7 | `reply_dedupe=true` 且与最近 3 条自己发送内容完全相同 → 丢弃 | `duplicate_reply` |
| 8 | `reply_mention_sender=true` 且本轮触发是寻址消息 → 在文本段前插入 `pkg/message.At(发送者 ID)` 段（**仅群聊**；私聊不加） | — |

输入 → 输出样例：

| 输入 | 输出 |
| --- | --- |
| `"打球可以啊"` | `打球可以啊` |
| `「你才不知道」` | `你才不知道` |
| `行吧\n那我去` | `行吧 那我去` |
| `  好   的  ` | `好 的`（首尾去空白 + 连续空格压缩） |
| `[SKIP]` | 丢弃（reason `skipped_by_llm`） |
| `（空串）` | 丢弃（reason `empty_reply`） |
| 超过 `reply_max_chars` 的长文本 | 硬截断到上限 |
| 与上一条自己发言完全相同且 `reply_dedupe=true` | 丢弃（reason `duplicate_reply`） |
