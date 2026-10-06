# llm.md — LLM 客户端（第 9 章）

本文覆盖第 9 章：与 OpenAI 兼容服务的协议、请求/响应、超时重试与错误、长度与成本、密钥与日志安全、替换服务。配置键名与默认值以 [`configuration.md`](configuration.md) §10.1 为唯一权威；人格与提示词模板见 [`persona.md`](persona.md) 第 8 章。

## 9.1 协议与请求

- 端点：`POST {base_url}/chat/completions`（先 `strings.TrimRight(base_url, "/")`，再拼 `/chat/completions`）。
- 请求头：`Content-Type: application/json`；`llm_api_key` 非空时附加 `Authorization: Bearer {llm_api_key}`，留空则不带该头（本地/无鉴权推理服务）。再叠加 `llm_extra_headers`（**后**叠加，因此中继服务可覆盖 `Authorization`）。
- HTTP 客户端：注入 `PluginContext.HTTPClient`（未声明 `network` 权限时为 nil → `Setup` 直接报错 `persona: 需要 network 权限`）。纯 `net/http`，不引入任何 SDK。

请求体（逐字形状）：

```json
{"model":"<llm_model>","temperature":0.8,"max_tokens":200,
 "messages":[{"role":"system","content":"<系统提示词>"},
             {"role":"user","content":[{"type":"text","text":"[群聊记录]"},
                                       {"type":"text","text":"张三: 今晚谁去打球"},
                                       {"type":"text","text":"李四: 看这个"},
                                       {"type":"image_url","image_url":{"url":"data:image/jpeg;base64,/9j/4AAQ..."}},
                                       {"type":"text","text":"小傲娇: 打球可以啊"}]}]}
```

- `temperature`/`max_tokens` 取**当前人格**的覆盖值，缺失则回落全局键 `llm_temperature`/`llm_max_tokens`。
- `system` 的 `content` 恒为**字符串**，是 `persona_template` 渲染结果，包含 11 个占位符（`{{persona}}`、`{{persona_name}}`、`{{chat_kind}}`、`{{channel_name}}`、`{{channel_id}}`、`{{platform}}`、`{{bot_name}}`、`{{now}}`、`{{last_sender}}`、`{{max_chars}}`、`{{skip_token}}`），模板全文与取值见 [`persona.md`](persona.md) §8.5（本文不重复）。
- `user` 的 `content` 恒为**数组**：首块是历史头（群聊 `[群聊记录]`、私聊 `[私聊记录]`），其后按时间序**每条历史产出自己的块**——文本合并成一个 `text` 块（该条首个 `text` 块带 `<显示名>: ` 前缀），图片在它原本的消息位置产出一个 `{"type":"image_url","image_url":{"url":"..."}}` 块（块形状与示例见 [`persona.md`](persona.md) §8.6）。

图片块的来源与上限：图片由**插件自己下载**并内联为 `data:<mime>;base64,<数据>`，**不把原始 URL 交给 LLM**；下载用 `PluginContext.HTTPClient` 的 transport 克隆（`Proxy = nil`，`DialContext` 换成地址限制拨号器，见 [`architecture.md`](architecture.md) §5 `vision.go`），整批共用 10s 上限；拒绝回环/私网/链路本地/未指定/组播目标（含 DNS 解析结果）。只有 `SegImage` 的 `KeyURL` 参与；`KeyFile` 仅在取值为 `http(s)://` 开头时参与（本地路径/平台文件 ID 对远端 LLM 不可用，丢弃）。参与选取的是与文本块**同一裁剪结果**的历史（被 `llm_history_max_chars` 裁掉的最旧条目里的图片也不发），**每条消息取第一个带可用 URL 的图片段**（最多 1 张），跨消息按时间序取最后至多 `llm_vision_max_images` 个槽位（`0` 表示不附加）；未被选中或下载失败的槽位**回落字面量 `[图片]`**、留在该条历史的文本块里，而不是挪到尾部或回落成原始 URL。非 2xx、非 `image/*` MIME、超过 `llm_vision_max_image_bytes` 的图一律丢弃且不回落到 URL。`llm_vision_allowed_formats` 非空时只发送归一后格式名在清单内的图片（MIME 先取响应 `Content-Type`，非 `image/*` 时回落 `http.DetectContentType`），清单外的图丢弃（`reason=format`）且不回落到传 URL；不做格式转码。`llm_vision_enabled=false` 或 `llm_vision_max_images <= 0` 时不发任何图片块。图片块**不计入** `llm_history_max_chars`（该键只裁文本）。图片 URL 不持久化，进程重启即丢。

**注入缝（逐字）**：

```go
// completionRequest 是一次补全请求；System/User 分别对应 system 与 user 消息。
type completionRequest struct {
	System      string
	User        contentParts // user 消息的 content 数组（text + 可选 image_url）
	Temperature float64
	MaxTokens   int
}

// contentPart 是 OpenAI content 数组中的一个块：text 或 image_url。
type contentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *imageURLPart `json:"image_url,omitempty"`
}

// imageURLPart 是 image_url 块的内容。
type imageURLPart struct {
	URL string `json:"url"`
}

// contentParts 是 user 消息的 content（恒为数组）。
type contentParts []contentPart

// completer 屏蔽具体 LLM 服务，便于单测注入桩。
type completer interface {
	Complete(ctx context.Context, req completionRequest) (string, error)
}

type openaiClient struct { // 实现 completer
	baseURL string
	apiKey  string
	model   string
	headers map[string]string
	http    *http.Client
	log     *slog.Logger
	retries int
	timeout time.Duration // llm_timeout，每次尝试独立派生
	debug   bool          // debug_prompts：Debug 输出请求与响应
}
```

`Plugin.completer` 字段类型为 `completer`，默认为 `*openaiClient`；单测注入桩实现同一接口（见 [`testing.md`](testing.md) §11.2）。

## 9.2 响应解析

- 读体上限 **1 MiB**（`io.LimitReader`），超限视为错误。
- 解析 `choices`：
  - `choices` 为空，或缺少 `choices[0].message.content` → 视为错误（点名兼容「只返回 `reasoning_content` 的中继」）；
  - `content` 不是字符串 → 错误。
- 成功返回 `choices[0].message.content` 字符串，交由 [`persona.md`](persona.md) §8.7 清洗。

## 9.3 超时、重试与错误

- 每次尝试独立派生 `context.WithTimeout(p.ctx, llm_timeout)`（attempt ctx 派生自**插件级** `p.ctx`，**不是** Handler ctx）。
- 重试条件：仅 `429`、`5xx` 与网络错误重试；次数 `llm_max_retries`（默认 1）。
- 退避：`500ms * 2^attempt`，等待用 `select` 且可被 ctx 取消。
- 其它 `4xx` **不重试**。
- 失败只记 `Warn`（含状态码与 ≤256 字节的响应体片段），reason `llm_error`，**绝不因此发送任何消息**。
- 总耗时上界 = `(1 + llm_max_retries) * llm_timeout + 退避和`，不受核心 `RuleTimeout`/`EventTimeout` 约束（已脱离 Handler ctx）。

## 9.4 长度与成本控制

| 参数 | 作用 | 默认 |
| --- | --- | --- |
| `llm_max_tokens` | 限制**生成**长度 | `200` |
| `context_max_messages` | 限制**输入历史条数** | `20` |
| `llm_history_max_chars` | 限制**输入历史字符数**（rune） | `4000` |
| `llm_vision_max_images` | 单次请求附加的图片数 | `4` |
| `llm_vision_max_image_bytes` | 单张图片下载体积上限 | `4194304` |
| `llm_vision_allowed_formats` | 允许附加的图片格式白名单（空 = 不过滤） | `[]` |
| `reply_max_chars` | 对 LLM 输出做**二次硬截断** | `200` |

三者关系：输入长度由 `context_max_messages` + `llm_history_max_chars` 双重限制（超限从最旧丢弃、始终保留最新一条）；输出长度由 `llm_max_tokens` 限制，再由 `reply_max_chars` 兜底截断。默认值即推荐取值。图片体积由 `llm_vision_max_image_bytes` 逐张限制（超限丢弃，不截断）；图片格式由 `llm_vision_allowed_formats` 过滤（不在白名单的丢弃，不转码）。

## 9.5 安全与日志

- **永不记录 `llm_api_key`**。
- 日志只出现 `base_url` 的 **host** 与 `llm_model`，不出现完整 URL、不出现查询串。
- `debug_prompts=true` 时以 Debug 输出 LLM 请求与响应：`persona llm 请求`（请求体 JSON）与 `persona llm 响应`（`status` + 响应体 JSON），`body` 均按 rune 截断 2048，均带 `host`/`model`。
- `debug_prompts=true` 时请求体日志中的 data URL 折成 `data:<mime>;base64,<N bytes>`（N 为解码后字节数），完整 base64 从不落日志。
- `debug_prompts=true` 时也不记录 `Authorization` 头（`llm_extra_headers` 同理，只记头名不记值）。
- 失败响应体片段截断到 256 字节后再记录。

## 9.6 替换服务

只需改 `llm_base_url` + `llm_model`（密钥用 `KEI_PLUGINS_PERSONA_LLM_API_KEY` 注入；本地/无鉴权服务可留空 `llm_api_key`，此时不发送 `Authorization` 头）：

```yaml
# OpenAI
llm_base_url: "https://api.openai.com/v1"
llm_model: "gpt-4o-mini"

# DeepSeek
llm_base_url: "https://api.deepseek.com/v1"
llm_model: "deepseek-chat"

# Ollama（本地）
llm_base_url: "http://127.0.0.1:11434/v1"
llm_model: "qwen2.5:7b"

# 自建 vLLM
llm_base_url: "http://127.0.0.1:8000/v1"
llm_model: "Qwen/Qwen2.5-7B-Instruct"
```

中继服务需要额外鉴权头时用 `llm_extra_headers`；若中继用自定义 `Authorization`，后叠加的 `llm_extra_headers` 会覆盖默认值。
