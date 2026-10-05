# kei-plugin-persona

[`kei`](https://github.com/RandomLemon/kei) 的「LLM 人格代理」进程内插件：在群聊里按人格预设**偶尔**插话，像群里一个普通真人；私聊里只要对方开口就必回。

- 插件名 `persona`，模块 `github.com/RandomLemon/kei-plugin-persona`，Go 1.25+。
- 运行时零第三方依赖：纯标准库 `net/http` 调用 OpenAI 兼容的 `chat/completions`，不引入任何 LLM SDK；唯一依赖是 kei 的 `pkg/bot`、`pkg/message`。
- 与平台无关、与 kei 核心零改动：宿主空导入本包 + 配置 `plugins.persona.enabled: true` 即启用。

## 功能

| 能力 | 说明 |
| --- | --- |
| 群聊偶尔插话 | 按人格预设生成一句话；是否说话由可配置的接话决策模型决定（[§7](docs/participation.md)） |
| 私聊必回 | 私聊消息一律视为寻址（默认关闭，需显式开启）；不走随机路径 |
| 人格系统 | `personas` 预设库 + `bindings` 绑定，含运行时覆盖与解析优先级（[§8](docs/persona.md)） |
| 接话决策 | 过滤 → 寻址判定 → 随机参与 → 批处理窗口合并成一轮生成（[§7](docs/participation.md)） |
| 名单策略 | 群聊与私聊各一套模式 + 单列表（[§7.8](docs/participation.md)） |
| 管理命令 | `/persona` 系列，仅管理员，不调用 LLM、不进历史（[§8.4](docs/persona.md)） |
| 持久化 | 每会话的人格覆盖与开关、插件级名单策略写入 `bot.Storage`；历史与计数器只在内存，覆盖与策略的存续取决于宿主存储后端（默认 memory 重启即丢，`storage.type: sqlite`/`mysql` 保留） |
| 可观测 | 每条消息以 `Debug` 输出决策行（reason 词表见 [§7.7](docs/participation.md)），`/persona status` 汇总计数 |

设计口径、边界与非目标见 [`docs/`](docs/README.md)；硬性规则见 [`AGENTS.md`](AGENTS.md)。

## 使用方法

### 1. 接入宿主

宿主 `go.mod` 指向本插件（本地开发同时用 `replace` 指向同级 kei 检出）：

```go
require github.com/RandomLemon/kei-plugin-persona v0.0.0
// 本地开发：replace github.com/RandomLemon/kei-plugin-persona => ../kei-plugin-persona
```

宿主 `main` 空导入即完成注册：

```go
import (
	_ "github.com/RandomLemon/kei-plugin-persona" // 空导入即注册插件
	"github.com/RandomLemon/kei/pkg/kei"
)
```

配置里启用插件：

```yaml
plugins:
  persona:
    enabled: true
```

### 2. 最小配置

`personas` 与 `llm_model` 必填；密钥推荐用环境变量 `KEI_PLUGINS_PERSONA_LLM_API_KEY` 注入（可留空以接入本地/无鉴权推理服务）。全量键表与默认值见 [`docs/configuration.md` §10.1](docs/configuration.md)（唯一权威）。

```yaml
plugins:
  persona:
    enabled: true

    personas:
      default: "普通的群友：说话随意、口语、短。"
      tsundere:
        prompt: "傲娇：嘴硬但对人不错，偶尔吐槽。"
        temperature: 0.9

    default_persona: "default"
    bindings:
      - { channel_id: "g1", persona: tsundere }

    llm_base_url: "https://api.openai.com/v1"
    llm_model: "gpt-4o-mini"
    llm_api_key: ""                # 或 export KEI_PLUGINS_PERSONA_LLM_API_KEY=sk-xxxx

    group_policy: "open"           # 群聊默认全部参与
    private_policy: "off"          # 私聊默认关闭；要启用私聊需显式改 open
```

替换其他 OpenAI 兼容服务只需改 `llm_base_url` + `llm_model`（DeepSeek / Ollama / vLLM 示例见 [`docs/llm.md` §9.6](docs/llm.md)）。

### 3. 管理命令

`/persona` 需要核心 `auth.admin_users` + Auth 中间件（`WithAdmin()`）；用法文本与各子命令的逐字输出见 [`docs/persona.md` §8.4](docs/persona.md)。

| 子命令 | 行为 |
| --- | --- |
| `/persona status` | 输出开关、当前人格与来源、历史条数、小时配额、错误/跳过计数 |
| `/persona switch [name]` | 查看或切换当前会话人格（写运行时覆盖并持久化） |
| `/persona on` / `off` / `reset` | 开关本会话 / 清历史与覆盖 |
| `/persona policy [group\|private mode]` | 查看或设置名单模式 |
| `/persona list [group\|private [add\|del id]]` | 查看或增删名单项 |

## 项目结构

目录树与每个源文件、测试文件的职责（唯一口径）见 [`docs/architecture.md` §5](docs/architecture.md)。

## 开发

开发环境（`flake.nix` + `.envrc`，direnv 自动加载，`GOTOOLCHAIN=local`）与合并前质量门（命令清单与必过项）、端到端联调（mock 适配器 + 本地 LLM 桩服务）步骤见 [`docs/testing.md` 第 11 章](docs/testing.md)。

## 文档

`docs/` 是目标实现口径的唯一来源；文档索引（唯一一份，含章号与阅读顺序）见 [`docs/README.md`](docs/README.md) §1-§2。

## License

MIT，见 [`LICENSE`](LICENSE)。
