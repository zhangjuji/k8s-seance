# Implementation Plan

开发默认用 Kiro CLI 写代码，模型是 claude-haiku-5.5。命令示例：`kiro-cli chat --model claude-haiku-5.5`。

- [ ] 1. 项目骨架与演示集群
  - [ ] 1.1 初始化 Go 仓库（Go 1.23+，`go mod init`），按 structure.md 建目录骨架，MIT LICENSE，README 骨架，确认 `.kiro/` 不在 `.gitignore`；等 Devpost 发 Didit 身份验证邮件并完成验证
    - _Requirements: 6.1_
  - [ ] 1.2 编写 `demo/kind-config.yaml`、`oom.yaml`、`crashloop.yaml`、`imagepull.yaml` 和 `kill-them-all.sh`（含 `--cleanup`）
    - _Requirements: 8.1, 8.2, 8.3_
  - [ ] 1.3 编写 `demo/rbac-readonly.yaml`
    - _Requirements: 8.4, 2.2_

- [ ] 2. 只读采集层
  - [ ] 2.1 定义 `PodAutopsy` 等类型（`internal/autopsy/model.go`）；实现 `ListSpirits` 的筛选逻辑
    - _Requirements: 1.1, 1.2, 1.4_
  - [ ] 2.2 实现 `CollectAutopsy`（client-go）：Pod 状态、容器 state/lastState、Events、当前与 previous 日志尾部
    - _Requirements: 2.1, 2.5_
  - [ ] 2.3 实现只读守卫测试：`go test` 扫描 `internal/` 源码，禁止 Create/Update/Patch/Delete/Evict 等写类 API 调用
    - _Requirements: 2.2_
  - [ ] 2.4 用真实 kind 集群录制 3 个 fixtures（`SEANCE_RECORD=1`），实现 `--fixtures` 加载（`fixtures/embed.go`）
    - _Requirements: 7.1, 7.3_

- [ ] 3. 尸检：死因分类与证据
  - [ ] 3.1 实现 `internal/autopsy/classifier.go` 规则与表驱动测试（6 类 + 边界用例）
    - _Requirements: 3.1, 3.2, 3.3, 3.4, 3.5, 3.6, 3.7_
  - [ ] 3.2 实现 `internal/autopsy/evidence.go`：编号、截断、去重、脱敏，及单元测试
    - _Requirements: 2.3, 2.4, 6.4_

- [ ] 4. LLM 接入与成本控制
  - [ ] 4.1 `internal/llm`：定义 `Provider` 接口；先实现 `MockProvider`（按死因 + 证据出固定台词、模拟流式），开发阶段默认用它
    - _Requirements: 7.4_
  - [ ] 4.2 `internal/llm/client.go`：`HTTPProvider`，用 net/http 实现 OpenAI 兼容流式客户端；base URL、API key、模型名从环境变量或 `~/.k8s-seance/config.yaml` 读，不预设厂商；超时与重试
    - _Requirements: 6.1_
  - [ ] 4.3 `internal/llm/budget.go`：调用次数 / token 上限、开场白缓存、超限台词
    - _Requirements: 6.2, 6.3_
  - [ ] 4.4 `internal/llm/replay.go`：`ReplayProvider`，录制与回放，缺录音时退回 mock
    - _Requirements: 7.2, 7.3, 7.5_
  - [ ] 4.5 拿到巨基提供的接口地址和 key 后，用真实接口跑 3 个亡灵并录制 `fixtures/replay/`
    - _Requirements: 6.1, 7.3_

- [ ] 5. 亡灵对话
  - [ ] 5.1 编写人设提示词与按死因的声线小抄（`internal/spirit/prompts/*.md`，用 go:embed 打包）
    - _Requirements: 4.1, 4.4, 4.5, 4.6_
  - [ ] 5.2 实现 séance 会话（`internal/spirit/persona.go`、`chat.go`）：开场白、追问、历史裁剪、`[E#]` 引用
    - _Requirements: 4.1, 4.2, 4.3_
  - [ ] 5.3 提示词快照测试（replay 模式，3 个 fixtures，golden 文件）
    - _Requirements: 4.1, 4.3, 4.4_

- [ ] 6. 故障复盘
  - [ ] 6.1 复盘模板 `template.md` 与 `internal/postmortem/generator.go`：时间线/证据由代码填，摘要/根因/建议/行动项由 LLM 写
    - _Requirements: 5.1, 5.2, 5.4_
  - [ ] 6.2 预算耗尽或 LLM 失败时的纯模板兜底
    - _Requirements: 6.3_

- [ ] 7. Web 界面（`internal/web`：net/http + html/template + htmx）
  - [ ] 7.1 亡灵列表页：死因徽章、命名空间切换、连接错误与样例模式入口
    - _Requirements: 1.2, 1.3, 1.4_
  - [ ] 7.2 降灵会页：SSE 流式聊天、证据卡片展开、修复建议
    - _Requirements: 4.2, 4.3, 4.5_
  - [ ] 7.3 复盘面板：展示 Markdown、复制、下载 `.md`
    - _Requirements: 5.3_
  - [ ] 7.4 暗色主题与烛光/雾气 CSS（`static/style.css`，手写）

- [ ]* 8. 故障墓地（可选）
  - [ ]* 8.1 `internal/graveyard/store.go` 与 `/api/graveyard`
    - _Requirements: 9.1_
  - [ ]* 8.2 墓地页面：墓碑网格、点开看复盘
    - _Requirements: 9.2, 9.3_

- [ ] 9. 验收与发布
  - [ ] 9.1 按 spec.md 第 7 节逐条验收（真实 kind 集群 + `go run ./cmd/seance --fixtures --replay`）
    - _Requirements: 1.1, 3.1, 3.2, 3.3, 4.4, 5.1, 6.2, 7.2, 8.1_
  - [ ] 9.2 README：一键运行、样例模式、只读 RBAC、成本说明、「How we used Kiro」
