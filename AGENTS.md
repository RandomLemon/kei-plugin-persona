# AGENTS.md

`kei-plugin-persona` 仓库的全局硬性规则与结构概览。**具体设计、接口契约、实现方式不在这里**：按领域拆到 [`docs/`](docs/README.md)（文档索引见 [`docs/README.md`](docs/README.md) §1）。改动某领域前，先读该领域文档与既有实现。

## 1. 项目定位

- 项目名 `kei-plugin-persona`，模块路径 `github.com/RandomLemon/kei-plugin-persona`，根包 `persona`。
- 接入形态：进程内插件。`init()` 调 `bot.RegisterPlugin(&Plugin{})`，宿主空导入 `import _ "github.com/RandomLemon/kei-plugin-persona"` + 配置 `plugins.persona.enabled: true` 即启用；kei 核心零改动。
- 定位与边界（做什么、不做什么、与 kei 的关系）以 [`docs/architecture.md`](docs/architecture.md) 第 1 章为唯一口径；交付物现状见 [`docs/roadmap.md`](docs/roadmap.md) §12.1。

## 2. 硬性规则

### 2.1 分层与依赖

- 只依赖 `github.com/RandomLemon/kei/pkg/bot`（唯一公开 SDK）与 `github.com/RandomLemon/kei/pkg/message`（消息段构建器）；禁止 import `github.com/RandomLemon/kei/internal/...`。
- 禁止任何平台名分支（`switch platform`、`if platform == "feishu"` 等）：本插件只处理 `bot.MessageGroup`，与具体平台无关。
- 配置与密钥只能经 `PluginContext.Config` 读取；禁止直接读环境变量、配置文件或 `os.Getenv`。
- `init()` 只注册插件，禁止在 `init()` 内读取配置或建立依赖；所有运行期依赖只能在 `Setup`/`Start` 经 `bot.PluginContextFrom(ctx)` 取得。
- 禁止用 Go `plugin` 包；扩展走进程内注册。
- 与 kei 核心的逐条契约对应（插件注册、权限裁剪、规则匹配、超时、配置来源等）见 [`docs/architecture.md`](docs/architecture.md) 第 6 章。

### 2.2 技术栈与依赖

- Go 1.25+（`go.mod` 为 `go 1.25.0`）。
- 优先标准库：`context`、`log/slog`、`net/http`、`encoding/json`、`sync`、`math/rand`、`time`、`strings`、`unicode/utf8`。
- 零第三方运行时依赖，LLM 调用用纯 `net/http`；不引入任何 LLM SDK。
- 唯一非标准库依赖是 `github.com/RandomLemon/kei`（提供 `pkg/bot`、`pkg/message`）。
- 本地开发用 `replace github.com/RandomLemon/kei => ../kei` 指向本地检出（本仓库契约版本为上游 HEAD，见 [`docs/architecture.md`](docs/architecture.md) 第 6 节）。
- 开发环境：`flake.nix` + `.envrc`（direnv `use flake` → `nix develop`）提供 Go/gopls/gotools/golangci-lint/dlv 工具链，`GOTOOLCHAIN=local`。flake 只提供 `devShell` 与 `formatter`：构建依赖同级 kei 检出，nix 沙箱内没有该目录，故不提供 `packages`/`checks`。

### 2.3 运行时契约

- 所有阻塞操作必须带 `context.Context`，不得存在无超时等待。
- **Handler 必须非阻塞**：禁止在 Handler 内做 LLM 或网络调用或任何阻塞等待；只允许记历史、判决策、布防定时器，毫秒级返回。
- 禁止用 Handler 的 `ctx` 派生异步链路；异步工作一律用 `Start` 阶段保存的插件级 ctx 派生，禁止 `context.Background()`。
- 所有 goroutine 必须有退出机制，必须能被 `Stop` 终止，禁止泄漏。
- 所有共享状态挂在 `Plugin` 实例上，禁止包级可变状态；`map` 并发访问必须加锁。
- 错误必须返回，禁止用 panic 表达运行期失败（仅启动阶段不可恢复错误可返回 error 阻止启动）。
- 时间统一 UTC；展示与 `{{now}}` 渲染时按 `random_timezone` 转换。

上述规则的依据与细节（Handler 非阻塞的三条理由、生命周期与阶段 ctx、协程与并发上限、状态与持久化）见 [`docs/architecture.md`](docs/architecture.md) 第 2-4 章。

## 3. 质量门（合并前必须全绿）

命令清单、执行环境（devShell、`GOTOOLCHAIN=local`）、单元测试矩阵与端到端联调步骤见 [`docs/testing.md`](docs/testing.md) 第 11 章。

## 4. 代码约定

1. 包名小写、简短，不使用下划线。
2. 公开符号必须有文档注释。
3. 所有错误必须返回，不得 panic，除非是启动阶段不可恢复错误。
4. 所有 goroutine 必须有退出机制，避免泄漏。
5. 所有 `map` 并发访问必须加锁。
6. 所有时间使用 `time.Time`，时区统一 UTC。
7. 所有 ID 使用字符串，不假设是数字。
8. 日志使用 `log/slog`，字段化输出；键名与值风格沿用 kei 核心（小写、下划线）。
9. 测试使用标准库 `testing`，不引入第三方 mock 库。
10. 禁止在插件中引入 `internal/` 包。
11. commit message 与 tag message 必须用英文撰写。

## 5. 项目结构与文档索引

- 目录树、每个源文件与测试文件的职责：见 [`docs/architecture.md`](docs/architecture.md) 第 5 章。
- 文档索引（全仓库唯一一份）：见 [`docs/README.md`](docs/README.md) §1。

## 6. 工作流

- 改代码前先读对应领域文档与既有实现，沿用仓库既有模式；禁止并存第二套约定。
- 修改导出符号前先查全部调用点并迁移所有调用方，不留兼容垫片。
- 设计歧义以 `docs/` 为准；文档未覆盖时取最简单、可测试的方案。
- 文档与流程约定（变更流程、章号与跨文档引用、逐字字面量、配置键权威、状态口径）见 [`docs/README.md`](docs/README.md)（§1 索引、§3 约定）。
