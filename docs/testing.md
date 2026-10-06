# testing.md — 质量与测试（第 11 章）

本文覆盖第 11 章：质量门、单元测试矩阵与注入缝、mock 适配器端到端联调、竞态与优雅关闭。完成定义：质量门全绿 + §11.2 矩阵全过 + §11.3 端到端可复现。

## 11.1 质量门

合并前必须全绿（本仓库质量门的唯一口径；[`../AGENTS.md`](../AGENTS.md) 只指向本节）：

```bash
gofmt -l .          # 必须无输出
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

命令在 `flake.nix` devShell 内执行（direnv `use flake` → `nix develop`；`go` 由 devShell 提供，`GOTOOLCHAIN=local`）。额外可选：`golangci-lint run`。

`go test -race ./...` 是**必过项**：本插件大量使用定时器与并发状态，竞态检测不可省。

## 11.2 单元测试矩阵

```text
用例 → 断言可观察结果
```

| 用例 | 断言 |
| --- | --- |
| 无发送者 | 决策结果 `reason=no_sender` |
| 非群聊（群 Handler 收到 `Kind=private`） | 决策结果 `reason=not_group` |
| 非私聊（私聊 Handler 收到 `Kind=group`） | 决策结果 `reason=not_private` |
| 群/私名单四模式（`off`/`open`/`whitelist`/`blacklist`）× 名单命中/未命中/空名单 | 放行或拒绝；拒绝 → `reason=not_allowed` |
| 被名单拒绝的群会话 / 私聊对端 | `reason=not_allowed`，且**不建会话状态、不进历史** |
| 私聊默认（`private_policy=off`） | `reason=not_allowed`，不发送 |
| 私聊 `private_policy=open` | 必回；`Target.Kind=private`、`Target.UserID=<发送者>`、`Target.ChannelID=""` |
| 私聊 `random_enabled=false` | 仍回复（私聊不走随机路径） |
| 私聊 `mention_reply_probability=0` | `reason=probability`，不发送 |
| 私聊 `mention_min_interval` 内第二条 | `reason=cooldown` |
| 私聊 `private_policy=whitelist` + 名单 | 名单内回复、名单外 `reason=not_allowed` |
| 私聊 `reply_mention_sender=true` | 回复只有文本段（私聊不加 At 段） |
| 两个私聊对端（`u1`/`u2`） | 会话键 `mock:bot1:user:<id>` 与覆盖键互不相同 |
| `persona:private` 规则注册 | 规则存在、`EventType=EventMessage`、`Kind=MessagePrivate` |
| `/persona policy`、`/persona list`（含 `add`/`del`/缺参/非法 scope/mode） | 输出行字面量一致；`add` 已存在、`del` 不存在幂等；非法参数回用法文本 |
| 策略写穿透 | `Storage` 键 `persona:policy` 值含新模式/名单 |
| `Start` 恢复策略（合法 / 非法模式 / `null` 列表 / `[]` 列表 / 坏 JSON / 无覆盖） | 合法值生效；非法模式与 `null` 列表回落配置默认值；`[]` 采用空名单；坏 JSON 不 panic 且回落默认值 |
| 机器人发送者且 `ignore_bots=true` | 决策结果 `reason=bot_sender` |
| 命令消息且 `respond_to_commands=false` | 决策结果 `reason=command` |
| `/persona` 命令 | 决策结果 `reason=command`，且**不进历史** |
| 文本为空 | 决策结果 `reason=empty_text` |
| 文本短于 `trigger_min_chars` | 决策结果 `reason=too_short` |
| 会话被 `/persona off` 后入站 | 决策结果 `reason=channel_off` |
| Storage 懒加载未完成时入站 | 决策结果 `reason=loading`，且本条被暂存：`Get` 放行后**补判一次**并回复（群聊回 `g1`、私聊回对端）；加载窗口内多条只补判最新一条 |
| 懒加载期间暂存的消息 × 加载到的覆盖 | `disabled=true` → 补判结果 `channel_off`、不发送；`persona` 覆盖在补判生成中生效 |
| 非寻址且 `random_enabled=false` | 决策结果 `reason=not_addressed` |
| `random_max_per_hour=0` | 决策结果 `reason=hour_quota`（随机插话关闭） |
| 寻址三种来源：`bot.SegAt` / `bot.SegReply` / `trigger_keywords` | 均判定为寻址（不给出 `not_addressed`） |
| `self_ids` 为空 vs 非空：非空时 At 到他人 ID | 空 → 寻址；非空且未命中 → 非寻址 |
| `self_ids` 非空且 At 命中 `Data[bot.KeyUserID]` | 判定为寻址 |
| `At` 的 `Data[bot.KeyUserID] == "all"`（`@全体成员`） | **任何情况下都不算寻址**：`self_ids` 为空时也不触发；同一消息另含 @他人（`self_ids` 为空）或 @本人（命中 `self_ids`）时仍判为寻址 |
| 列表键写成 YAML 裸数字（`self_ids`/`group_list`/`private_list`）或裸标量 / 逗号分隔字符串 | 元素经 `readStringList` 转字符串后生效，不落入空列表语义；`group_policy=whitelist` + `group_list: [群号]` 命中放行 |
| `random_probability=0` | 永不命中（`reason=probability`） |
| `random_probability=1`（其余条件满足） | 必定进入 `schedule()` |
| `random_min_participants` 不足 | 决策结果 `reason=min_participants` |
| 距 `lastReplyAt` < `random_cooldown`（注入 `p.now` 桩） | 决策结果 `reason=cooldown` |
| 寻址且距 `lastReplyAt` < `mention_min_interval` | 决策结果 `reason=cooldown` |
| 近 1 小时回复数达 `random_max_per_hour`（注入 `p.now`） | 决策结果 `reason=hour_quota` |
| `random_quiet_hours` 跨零点（如 `23:00-07:00`，注入 `p.now`） | 窗口内 `reason=quiet_hours`，窗口外放行 |
| `random_timezone` 影响 `{{now}}` 与静默判定 | 注入不同时区，静默判定边界随之移动 |
| 批处理窗口合并（`batch_window=50ms`，真实定时器） | 窗口内多条消息合并为**一次**生成；`batch_max_window` 上限生效 |
| `st.epoch` 变化（`/persona off` 后定时器回调） | 在途结果被丢弃，`reason=stale`，不发送 |
| 全局信号量占满（`limits_max_concurrent=1` 且已有在途） | `reason=semaphore_full`，不排队 |
| LLM 返回 skip token | `reason=skipped_by_llm` |
| LLM 返回空串 | `reason=empty_reply` |
| LLM 返回与最近自己发言重复且 `reply_dedupe=true` | `reason=duplicate_reply` |
| 发送失败 | `reason=send_error`，不重排、不重发 |
| `Start` 阶段 ctx 在阶段返回后被 cancel（模拟 kei 的 `defer cancel()`） | 插件级 ctx 仍可用：私聊照常回复（`context.WithoutCancel` 派生） |
| 模板渲染 11 个占位符 | 每个占位符被替换为预期值（含 `{{chat_kind}}` = `群聊`/`私聊`） |
| 模板含未知占位符（如 `{{unknown}}`） | 原样保留 |
| 历史渲染 6 种回落 | `[图片]` / `[表情]` / `[文件]` / `[卡片]` / `[引用]` / `[消息]` 分别命中 |
| 历史块头按会话类型 | 群聊 `[群聊记录]`、私聊 `[私聊记录]` |
| 历史裁剪 | 超 `context_max_messages` 或 `llm_history_max_chars`（rune）从最旧丢弃，保留最新一条 |
| 清洗管线 8 步（见 [`persona.md`](persona.md) §8.7） | 每条输入→输出样例一致 |
| 在途写穿透未完成时调用 `Stop`（`Set` 阻塞到 gate 关闭） | `Stop` 先拒绝新写入并等其落库、再取消插件级 ctx：写入不被取消，`Stop` 在其结束后才返回（持久化后端不丢最后一次覆盖/策略） |
| `/persona status` | 输出固定字段顺序一行 |
| `/persona switch` | 输出 `persona: persona=<name> 来源=<override\|binding\|default>` |
| `/persona on`、`/persona off` | 切换开关，触发 `Storage.Set`，关闭时递增 `st.epoch` |
| `/persona reset` | 清历史/覆盖/计数器，置为开启，递增 `st.epoch` |
| `llm.go` 用 `httptest.Server`：200 | 返回解析后的文本 content |
| `llm.go` 429 重试 / 500 重试 | 按 `llm_max_retries` 重试并最终成功 |
| `llm.go` 400 不重试 | 立即返回错误，不重试 |
| `llm.go` 非法 JSON | 返回错误 |
| `llm.go` 缺 choices | 返回错误 |
| `llm.go` 超时（`llm_timeout` 极短） | 返回错误，`reason=llm_error` |
| 多模态开关关闭（`llm_vision_enabled=false`） | 请求体 `messages[1].content` 为只含一个 `text` 块的数组，不含 `image_url`；文本与该历史下的 `renderHistoryBlock` 逐字相同 |
| 多模态开关开启（`llm_vision_enabled=true`） | 图片由插件下载后内联为 `data:<mime>;base64,<数据>`；请求体 `messages[1].content` 为数组，末元素为 `{"type":"image_url","image_url":{"url":"data:..."}}`；原始 URL 不出现在请求体中；每条消息最多 1 张、总数受 `llm_vision_max_images` 限制、被文本裁剪丢掉的条目里的图片不发 |
| 图片下载失败路径 | 非 2xx、非 `image/*`、格式不在白名单、超过 `llm_vision_max_image_bytes`、连接失败的图各自被丢弃（`reason` 分别 `status`/`not_image`/`format`/`too_large`/`fetch_error`），其余图片与文本照常发送，不回落到传 URL |
| 图片格式白名单（`llm_vision_allowed_formats=["png"]`，混合 png/jpeg） | 仅 png 进请求体；jpeg 被丢弃且 `reason=format`；未配置时不丢弃任何已识别格式 |
| 图片地址限制 | 回环/私网/链路本地/未指定/组播目标（含 DNS 解析结果）被拒；拨号时才校验（重定向同样受限）；`data:`/非 http(s) scheme 直接丢弃 |
| `llm_vision_max_images` / `llm_vision_max_image_bytes` 校验 | `-1` → `persona: 配置错误 llm_vision_max_images=-1: 必须 >= 0`；`0` → `persona: 配置错误 llm_vision_max_image_bytes=0: 必须 >= 1`；默认 `4` / `4194304`，`llm_vision_enabled` 默认 `false`；`llm_vision_max_images=0` 时不附加图片块；llm_vision_allowed_formats 无额外校验（缺省/空 = 不过滤） |
| `debug_prompts=true` 日志 | 请求日志中 data URL 为 `data:image/png;base64,<N bytes>`，不含完整 base64；响应日志不变 |

reason 词表（24 个）单测覆盖：`not_group`、`not_private`、`no_sender`、`not_allowed`、`bot_sender`、`command`、`empty_text`、`too_short`、`channel_off`、`loading`、`not_addressed`、`min_participants`、`cooldown`、`hour_quota`、`quiet_hours`、`inflight`、`probability`、`semaphore_full`、`skipped_by_llm`、`empty_reply`、`duplicate_reply`、`llm_error`、`send_error`、`stale`。

### 注入缝

必须在 `Plugin` 上定义（默认指向真实实现，测试替换为桩）：

| 字段 | 默认 | 用途 |
| --- | --- | --- |
| `p.now` (`func() time.Time`) | `time.Now` | 冷却、配额、静默时段的确定性时间 |
| `p.randFloat` (`func() float64`) | `rand.Float64` | 概率边界的确定性 |
| `p.completer` | `*openaiClient` | 屏蔽真实 LLM（实现 completer 接口），返回固定文本/错误 |
| `p.api` (`bot.BotAPI`) | 注入的 `PluginContext.Bot` | 捕获 `Send` 调用；命令测试用 `bot.NewNoopReply()` 断言 `PlainText()` |

禁止为测试引入第三方 mock 库，全部用标准库 testing + 手写桩。

## 11.3 端到端联调（mock 适配器）

以 kei 仓库检出为宿主，构建一个最小宿主 main：空导入本插件 + 启用 mock 适配器 + `plugins.persona`。装配走 kei 公开门面 `pkg/kei`（`kei.Run` 与 `cmd/bot` 是同一份实现），无需复制 `cmd/bot`：

```go
package main

import (
	"context"
	"log"

	_ "github.com/RandomLemon/kei-plugin-persona" // 空导入即注册插件
	_ "github.com/RandomLemon/kei/adapters/mock"
	"github.com/RandomLemon/kei/pkg/kei"
)

func main() {
	if err := kei.Run(context.Background(), kei.Options{ConfigFile: "configs/config.yaml"}); err != nil {
		log.Fatal(err)
	}
}
```

为稳定复现，配置 `random_probability: 1.0`、`mention_min_interval: 0s`、`random_cooldown: 0s`。

「重启后仍保留」这类断言需要持久化后端：kei 的默认 `storage.type: memory` 随进程消失，把宿主的 `storage: {type: sqlite, dsn: <文件路径>}`（或 `mysql`）配上才成立（见 [`architecture.md`](architecture.md) §4.7 与 kei `docs/configuration.md` §12.5）。sqlite 驱动经 cgo 编译，宿主构建需 `CGO_ENABLED=1` 与可用的 C 编译器（kei 的 devShell 已含 `gcc`）。想在同一进程里重复 `kei.Run` 模拟重启时，用 `kei.Options.Plugins: []bot.Plugin{&persona.Plugin{}}` 注入干净实例（与配置里的 `plugins.persona` 同名时注入优先，见 [`architecture.md`](architecture.md) 第 6 章）。

LLM 侧用**本地桩服务**（`python3 -m http.server` 不够，它不会返回 JSON）。20 行以内的桩要点：

```bash
python3 - <<'PY'
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get('content-length', 0)))
        body = json.dumps({"choices": [{"message": {"content": "打球可以啊"}}]})
        self.send_response(200)
        self.send_header('content-type', 'application/json')
        self.end_headers()
        self.wfile.write(body.encode())
    def log_message(self, *a): pass
HTTPServer(('127.0.0.1', 19090), H).serve_forever()
PY
```

命令序列：

1. 启动：`go run ./cmd/bot -config configs/config.yaml`（mock 适配器 + 本插件；`llm_base_url: http://127.0.0.1:19090/v1`）。
2. 注入第一条群消息：
   ```bash
   curl -XPOST 127.0.0.1:18080/inject -H 'content-type: application/json' \
     -d '{"text":"今晚谁去打球","user_id":"u1","user_name":"张三","channel_id":"g1"}'
   ```
   （`kind` 缺省即 `group`。）
3. 注入 `u2/李四` 的消息，使活跃人数达到 `random_min_participants`。
4. 观察发送：
   ```bash
   curl -sS 127.0.0.1:18080/sent | jq '.[-1].Request.Message.Segments[0].Data.text'
   ```
   期望得到一条 LLM 文本（桩服务返回 `打球可以啊`）。
5. 注入 `/persona status`（核心 `auth.admin_users: ["u1"]`）→ 期望输出含 `persona=` 与计数器。
6. 注入 `/persona off` 后再注入消息 → 期望 `/sent` 不再增长。
7. 私聊（配置 `private_policy: open`）：注入 `{"kind":"private","text":"在吗","user_id":"u1","user_name":"张三"}` → 期望 `/sent` 增长，且 `Target.Kind=private`、`Target.UserID=u1`、`Target.ChannelID` 为空（`curl -sS 127.0.0.1:18080/sent | jq '.[-1].Request.Target'`）。
8. 名单策略（配置 `group_policy: whitelist`、`group_list: ["g9"]`）：向 `g1` 注入消息 → `/sent` 不增长；向 `g9` 注入消息 → `/sent` 增长。再注入 `/persona policy`、`/persona list group add g9`（核心 `auth.admin_users: ["u1"]`）→ 比对 [`participation.md`](participation.md) §7.8 的字面量；配了持久化存储后端（见上）时，重启宿主后 `/persona policy` 应显示 `persona:policy` 覆盖值而非配置默认值——默认 memory 后端重启即丢，此时该断言不成立。

说明：mock 适配器的 HTTP `/inject` 支持 `kind`（缺省 `group`，可传 `private`；`private` 时不构造 `Channel`）与任意文本，但**无法构造 `bot.SegAt`**。因此「寻址（@/引用）」用例改用 Go 侧 `Adapter.Inject(ctx, ev)` 注入含任意 `Segments` 的事件（写在 `e2e_test.go`），HTTP 路径覆盖随机插话、命令与私聊（`e2e_test.go` 的 `TestE2EMockAdapterPrivate`）。`/sent` 的每条记录含完整 `Request.Target`，可断言 `Kind`/`ChannelID`/`UserID`（私聊发送 `ChannelID` 为空）。

## 11.4 竞态与优雅关闭

- **停止后不发送**：用假 `BotAPI` 计数，断言 `Stop` 之后不再有 `Send`。
- **及时返回**：在存在待生成协程时调用 `Stop`，断言在 15s 内返回（`wg.Wait` 配合插件级 ctx cancel）。
- **无定时器泄漏**：断言 `Stop` 停掉了全部 `time.AfterFunc` 布防的定时器（跟踪每个 `channelState.timer` 并在 `Stop` 时 `Stop()`）。
- **幂等**：重复调用 `Stop` 安全无 panic。
