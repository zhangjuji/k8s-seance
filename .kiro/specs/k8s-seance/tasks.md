# Implementation Plan

开发默认用 Kiro CLI 写代码，模型是 claude-haiku-5.5。命令示例：`kiro-cli chat --model claude-haiku-5.5`。

- [ ] 1. 项目骨架与演示集群
  - [ ] 1.1 初始化 Go 仓库（Go 1.26+，`go mod init`），按 structure.md 建目录骨架，MIT LICENSE，README 骨架，确认 `.kiro/` 不在 `.gitignore`；等 Devpost 发 Didit 身份验证邮件并完成验证
    - _Requirements: 6.1_
    - [x] repo skeleton, go.mod, LICENSE, README skeleton (done by Kiro)
    - [ ] Devpost Didit identity verification (human: juji)
  - [x] 1.2 编写 `demo/kind-config.yaml`、`oom.yaml`、`crashloop.yaml`、`imagepull.yaml` 和 `kill-them-all.sh`（含 `--cleanup`）
    - _Requirements: 8.1, 8.2, 8.3_
  - [x] 1.3 编写 `demo/rbac-readonly.yaml`
    - _Requirements: 8.4, 2.2_

- [x] 2. 只读采集层
  - [x] 2.1 定义 `PodAutopsy` 等类型（`internal/autopsy/model.go`）；实现 `ListSpirits` 的筛选逻辑
    - _Requirements: 1.1, 1.2, 1.4_
  - [x] 2.2 实现 `CollectAutopsy`（client-go）：Pod 状态、容器 state/lastState、Events、当前与 previous 日志尾部
    - _Requirements: 2.1, 2.5_
  - [x] 2.3 实现只读守卫测试：`go test` 扫描 `internal/` 源码，禁止 Create/Update/Patch/Delete/Evict 等写类 API 调用
    - _Requirements: 2.2_
  - [x] 2.4 用真实 kind 集群录制 3 个 fixtures（`SEANCE_RECORD=1`），实现 `--fixtures` 加载（`fixtures/embed.go`）
    - _Requirements: 7.1, 7.3_

- [x] 3. 尸检：死因分类与证据
  - [x] 3.1 实现 `internal/autopsy/classifier.go` 规则与表驱动测试（6 类 + 边界用例）
    - _Requirements: 3.1, 3.2, 3.3, 3.4, 3.5, 3.6, 3.7_
  - [x] 3.2 实现 `internal/autopsy/evidence.go`：编号、截断、去重、脱敏，及单元测试
    - _Requirements: 2.3, 2.4, 6.4_

- [ ] 4. LLM 接入与成本控制
  - [x] 4.1 `internal/llm`：定义 `Provider` 接口；先实现 `MockProvider`（按死因 + 证据出固定台词、模拟流式），开发阶段默认用它
    - _Requirements: 7.4_
  - [x] 4.2 `internal/llm/client.go`：`HTTPProvider`，用 net/http 实现 OpenAI 兼容流式客户端；base URL、API key、模型名从环境变量或 `~/.k8s-seance/config.yaml` 读，不预设厂商；超时与重试
    - _Requirements: 6.1_
  - [x] 4.3 `internal/llm/budget.go`：调用次数 / token 上限、开场白缓存、超限台词
    - _Requirements: 6.2, 6.3_
  - [x] 4.4 `internal/llm/replay.go`：`ReplayProvider`，录制与回放，缺录音时退回 mock
    - _Requirements: 7.2, 7.3, 7.5_
  - [x] 4.5 拿到巨基提供的接口地址和 key 后，用真实接口跑 3 个亡灵并录制 `fixtures/replay/`
    - _Requirements: 6.1, 7.3_

- [x] 5. 亡灵对话
  - [x] 5.1 编写人设提示词与按死因的声线小抄（`internal/spirit/prompts/*.md`，用 go:embed 打包）
    - _Requirements: 4.1, 4.4, 4.5, 4.6_
  - [x] 5.2 实现 séance 会话（`internal/spirit/persona.go`、`chat.go`）：开场白、追问、历史裁剪、`[E#]` 引用
    - _Requirements: 4.1, 4.2, 4.3_
  - [x] 5.3 提示词快照测试（replay 模式，3 个 fixtures，golden 文件）
    - _Requirements: 4.1, 4.3, 4.4_

- [x] 6. 故障复盘
  - [x] 6.1 复盘模板 `template.md` 与 `internal/postmortem/generator.go`：时间线/证据由代码填，摘要/根因/建议/行动项由 LLM 写
    - _Requirements: 5.1, 5.2, 5.4_
  - [x] 6.2 预算耗尽或 LLM 失败时的纯模板兜底
    - _Requirements: 6.3_

- [x] 7. Web 界面（`internal/web`：net/http + html/template + htmx）
  - [x] 7.1 亡灵列表页：死因徽章、命名空间切换、连接错误与样例模式入口
    - _Requirements: 1.2, 1.3, 1.4_
  - [x] 7.2 降灵会页：SSE 流式聊天、证据卡片展开、修复建议
    - _Requirements: 4.2, 4.3, 4.5_
  - [x] 7.3 复盘面板：展示 Markdown、复制、下载 `.md`
    - _Requirements: 5.3_
  - [x] 7.4 暗色主题与烛光/雾气 CSS（`static/style.css`，手写）

- [x]* 8. 故障墓地（可选）
  - [x]* 8.1 `internal/graveyard/store.go` 与 `/api/graveyard`
    - _Requirements: 9.1_
  - [x]* 8.2 墓地页面：墓碑网格、点开看复盘
    - _Requirements: 9.2, 9.3_

- [x] 10. 英文演示路径
  - [x] 10.1 `internal/llm`：`Lang` 字段、`NormalizeLang`、英文额度耗尽台词、开场缓存 key 加语言、英文 mock 开场白
    - _Requirements: 10.3_
  - [x] 10.2 `internal/spirit`：`NewSessionIn(lang, …)`、英文开场请求、`<pod>-en` 回放文件、提示词语言规则（中文提示词不变）
    - _Requirements: 10.3, 10.5_
  - [x] 10.3 `internal/web`：文案目录 `i18n.go`、每种语言一套模板、`?lang=` + cookie + `Accept-Language`、`-lang` 参数、语言不一致提示
    - _Requirements: 10.1, 10.2, 10.4_
  - [x] 10.4 用真实接口录制英文回放 `fixtures/replay/<pod>-en.json`（`demo/record-english.sh`，deepseek-flash）
    - _Requirements: 10.3_

- [ ] 11. 谎言检测
  - [ ] 11.1 `internal/spirit/factcheck.go`：引用、内存数量、退出码、重启次数、时间戳核对；建议句豁免数量；表驱动测试 + 录音零误报测试
    - _Requirements: 11.1, 11.3, 11.4_
  - [ ] 11.2 Web：回答下方显示「亡灵可能在说谎」，JSON / SSE 带 `doubts`
    - _Requirements: 11.2_

- [ ] 12. 招魂铃
  - [ ] 12.1 `internal/k8s/watch.go`：`WatchPods`（只读 watch，断开后退避重连并补发信号）
    - _Requirements: 12.1, 12.6_
  - [ ] 12.2 `/api/spirits/stream`：SSE，去抖后重新 list，对比出新亡灵 / 又死一次 / 集体墓葬
    - _Requirements: 12.1, 12.2, 12.3, 12.4_
  - [ ] 12.3 列表页：实时状态、提示条 + 立刻召唤、集体墓葬卡片、合成铃声开关、烛火闪动
    - _Requirements: 12.2, 12.3, 12.4, 12.5, 12.6_

- [ ] 13. 复活咒语
  - [ ] 13.1 `internal/resurrect`：按死因生成咒语（OOM、缺文件、依赖不通、其他崩溃、镜像 tag/权限/网络、探针、未知），施咒对象推断，`${VAR:?}` 占位，dry-run 在前
    - _Requirements: 13.1, 13.2, 13.3, 13.4, 13.5_
  - [ ] 13.2 测试：补丁用 strategic merge 应用到 `demo/oom.yaml`、`demo/crashloop.yaml` 后结果正确；每条写操作前有 dry-run；不编 tag；英文无中文
    - _Requirements: 13.2, 13.3, 13.6_
  - [ ] 13.3 降灵会页「念出复活咒语」面板、复制按钮、补丁下载；`demo/crashloop.yaml` 改为真的检查配置文件，挂上后能复活
    - _Requirements: 13.1, 13.6_

- [ ] 14. 遗忘倒计时
  - [ ] 14.1 `autopsy.Remember`：按事件 TTL 计算最先消散的 Warning 事件、全部事件消散时间；`RestartsSoon`；`-event-ttl`
    - _Requirements: 14.1, 14.2_
  - [ ] 14.2 列表「记忆」列、淡出与横幅、`static/memory.js` 浏览器倒计时；降灵会遗忘面板、证据卡片倒计时、留住记忆按钮
    - _Requirements: 14.3, 14.4, 14.5_

- [ ] 15. 亡灵祭坛
  - [ ] 15.1 墓地页改成祭坛：剪纸旗、阶梯祭台、相框照片、蜡烛、花瓣小路（纯 CSS），单个亡灵页同风格
    - _Requirements: 15.1, 15.3_
  - [ ] 15.2 已供奉的亡灵在列表里不再变淡、不触发遗忘横幅
    - _Requirements: 15.2_

- [ ] 16. 转世对比
  - [ ] 16.1 `k8s.CollectLineage`：家族、同胞、其他版本、模板差异（脱敏）；判断；作为 `lineage` 证据
    - _Requirements: 16.1, 16.2, 16.3, 16.4_
  - [ ] 16.2 降灵会「家族与转世」面板；mock 开场白提到活着的前世；复活咒语在发布改坏时先回滚
    - _Requirements: 16.4, 16.5_
  - [ ] 16.3 `demo/second-life.yaml` 与 `kill-them-all.sh` 的第四种死法
    - _Requirements: 16.6_

- [ ] 17. 降灵板与整体视觉打磨
  - [ ] 17.1 降灵板：证据符文、乩板随引用移动、是/否随谎言检测、再会随复盘
    - _Requirements: 17.1, 17.2, 17.3, 17.4_
  - [ ] 17.2 视觉打磨：遗忘面板平时折叠、快消散时自动展开；回答的 Markdown 粗体/代码安全渲染；列表页标语；手机端降灵板、差异表、导航
    - _Requirements: 17.4, 17.5_

- [ ] 18. 供品与归来
  - [ ] 18.1 `graveyard`：`Offering`、`OfferingsFor`、`Family`、`Return`、`Match`；立碑时保存家族与供品
    - _Requirements: 18.1, 18.2, 18.4_
  - [ ] 18.2 列表 / 招魂铃 / 祭坛 / 照片页显示归来与供品；`SpiritSummary.Family`
    - _Requirements: 18.2, 18.3, 18.5_

- [ ] 19. 施法（真的复活）
  - [ ] 19.1 `resurrect.Action`：可执行的咒语；`internal/rite`：patch / set-image / rollback / configmap-mount，支持 dry-run
    - _Requirements: 19.2, 19.3_
  - [ ] 19.2 `k8s.Vitals`：施法后只读确认复活；祭坛「已复活」
    - _Requirements: 19.4_
  - [ ] 19.3 `--allow-resurrect`、施法表单与结果、`demo/rbac-resurrect.yaml`、只读守卫只豁免 `internal/rite`
    - _Requirements: 19.1, 19.5_

- [ ] 9. 验收与发布
  - [x] 9.1 按 spec.md 第 7 节逐条验收（真实 kind 集群 + `go run ./cmd/seance --fixtures --replay`）
    - _Requirements: 1.1, 3.1, 3.2, 3.3, 4.4, 5.1, 6.2, 7.2, 8.1_
  - [ ] 9.3 第二次验收（2026-11-02，功能 8、10–19 之后）：见 `docs/acceptance-2026-11-02.md`；补上每场降灵会输入 ≤ 30k 的总量上限（§7.8）
    - _Requirements: 6.2, 7.2_
  - [x] 9.2 README：一键运行、样例模式、只读 RBAC、成本说明、「How we used Kiro」
