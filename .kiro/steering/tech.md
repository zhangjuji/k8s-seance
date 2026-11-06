# Tech

- 开发工具：默认用 Kiro CLI 写代码，模型是 claude-opus-5.5，effort 为 medium。命令示例：`kiro-cli chat --agent seance-dev --model claude-opus-5.5 --effort medium`。`.kiro/agents/seance-dev.json` 的 `model` 字段设为 claude-opus-5.5；agent 配置没有 effort 字段，默认 effort 由工作区设置 `.kiro/settings/cli.json` 的 `chat.modelDefaults`（`output_config.effort`）给出，命令行 `--effort` 优先。历史记录：基础任务的启动器和任务 20 的 14 次嵌套测试用的是 claude-haiku-5.5（见 docs/how-we-used-kiro.md），那是有记录的例外。
- 语言/运行时：Go 1.26+（与 `go.mod` 的 `go 1.26.0` 一致），编译为单个二进制（评委下载即跑）。模块管理用 Go modules。
- 服务端：`net/http` 标准库（单进程，本地 `localhost:6660`），SSE 流式输出。
- 前端：`html/template` 渲染页面，htmx（`htmx.min.js`）负责交互，另有 `sse.js`（SSE 连接）和 `memory.js`（遗忘倒计时），都用 `go:embed` 打进二进制。CSS 纯手写，暗色主题，不引入 React、Vue 等前端框架，也不引入任何前端构建链。
- K8s：`k8s.io/client-go`，只读调用 `CoreV1().Pods().List/Get/Watch`、`Pods().GetLogs`、`Events().List`。只读 RBAC 只授予 `get/list/watch`。除 `internal/rite`（`--allow-resurrect` 时的施法）外，代码中禁止任何 Create/Update/Patch/Delete/Evict 调用，由 `internal/readonly_test.go` 守护；施法权限在 `demo/rbac-resurrect.yaml`（命名空间 Role）。
- LLM：OpenAI 兼容的 Chat Completions 接口（`stream=true`），用 `net/http` 自己实现，不依赖重 SDK。
  - 配置来源：环境变量 `SEANCE_LLM_BASE_URL`、`SEANCE_LLM_MODEL`、`SEANCE_LLM_API_KEY`，或配置文件 `~/.k8s-seance/config.yaml`（环境变量优先）。
  - 不预设模型厂商，没有默认 base URL 或模型名；接口地址和 key 由巨基之后提供。
  - 开发阶段用 mock Provider（与录制/回放合并，见 design.md），不需要 key；`--mock`、`--replay` 或 `SEANCE_LLM_MODE` 切换。
- 存储：本地 JSON 文件（`~/.k8s-seance/graveyard.json`），不用数据库。
- 配置文件解析：`gopkg.in/yaml.v3`。除 client-go 外只计划引入这一个第三方库，其余尽量用标准库。
- 测试：`go test ./...`，表驱动测试；死因分类器和证据打包器必须有单元测试（用 fixtures）。
- 演示集群：kind；`demo/` 下的 manifest 只用公开小镜像（如 busybox / polinux/stress）。
- 常用命令：`go run ./cmd/seance`、`go test ./...`、`go run ./cmd/seance --fixtures --replay`、`./demo/kill-them-all.sh`。
- 代码、标识符、提交信息用英文；面向用户的文案中英文都可，演示用英文。
