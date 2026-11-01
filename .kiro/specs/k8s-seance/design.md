# Design Document

## Overview

单进程本地 Web 应用，用 Go 编写，编译为单个二进制。浏览器访问 `http://localhost:6660`。后端通过当前 kubeconfig 只读读取 K8s 数据，生成 `PodAutopsy`，用确定性规则分类死因，再把精简后的证据包交给 LLM 扮演亡灵；结束时用 LLM + 固定模板生成复盘。

### 形态选择：Web（本地）而不是 CLI

- 选 Web：视频里画面更有记忆点（烛光、雾气、墓碑），评委 3 秒看懂；也能顺带争取「看起来好」的印象。
- 选 Go 单二进制：评委下载即跑，不需要安装其他运行时；`go run ./cmd/seance --fixtures --replay` 即可试用。样例数据用 `go:embed` 打进二进制，不依赖仓库目录。
- 不选纯 CLI：终端聊天在视频里不出彩；但核心逻辑都在 `internal/` 包里，以后加 `seance summon <pod>` 子命令只需几十行（可选加分项）。
- 不做云端部署：评委用 `--fixtures --replay` 本地即可试用，零托管成本。如需在线 demo，可选部署静态回放版（非 MVP）。

## Architecture

```mermaid
flowchart LR
  UI[Web UI<br/>internal/web: html/template + htmx] -- REST + SSE --> S[internal/web/server.go<br/>net/http]
  S --> C[k8s/collector<br/>read-only, client-go]
  S --> F[fixtures loader<br/>go:embed]
  C --> A[PodAutopsy]
  F --> A
  A --> K[autopsy/classifier<br/>rules, no LLM]
  A --> E[autopsy/evidence<br/>truncate, dedupe, redact]
  K --> P[spirit/persona + chat]
  E --> P
  P --> L[llm/client<br/>OpenAI-compatible, net/http]
  L --> B[llm/budget]
  L -.replay.-> R[llm/replay]
  P --> M[postmortem/generator]
  M --> G[graveyard/store<br/>JSON file, optional]
```

### 降灵会时序

```mermaid
sequenceDiagram
  participant U as User
  participant W as Web UI
  participant S as Server
  participant K as K8s API (read-only)
  participant L as LLM
  U->>W: 点击 Pod「召唤」
  W->>S: POST /api/seances {namespace, pod}
  S->>K: Pods.Get / Events.List / Pods.GetLogs(previous=true)
  S->>S: classify + build evidence
  S->>L: system(persona+evidence) + "opening"
  L-->>W: SSE 流式开场白（含 [E#] 引用）
  U->>W: 追问
  W->>S: POST /api/seances/:id/messages
  S->>L: 历史 + 问题（受 budget 限制）
  L-->>W: SSE 回答
  U->>W: 结束降灵会
  W->>S: POST /api/seances/:id/postmortem
  S->>L: postmortem prompt（证据 + 对话摘要）
  S-->>W: Markdown 复盘
```

## Components and Interfaces

### k8s/collector.go

```go
func ListSpirits(ctx context.Context, cs kubernetes.Interface, namespace string) ([]SpiritSummary, error)
func CollectAutopsy(ctx context.Context, cs kubernetes.Interface, namespace, pod string) (*autopsy.PodAutopsy, error)
```
- 使用 `k8s.io/client-go/kubernetes` 的 `CoreV1()`：`Pods(ns).List`、`Pods(ns).Get`、`Pods(ns).GetLogs(name, &corev1.PodLogOptions{TailLines, Previous})`、`Events(ns).List`（`FieldSelector: "involvedObject.name=<pod>"`）。
- 所需 RBAC（`demo/rbac-readonly.yaml`）：`pods`、`pods/log`、`events` 的 `get/list/watch`。
- `kubernetes.Interface` 作为参数传入，单元测试可换成 fake clientset，不连真集群。
- 只读守卫测试：`go test` 扫描 `internal/` 下所有 `.go` 源码（排除测试文件自身），禁止 `Create`、`Update`、`UpdateStatus`、`Patch`、`Delete`、`DeleteCollection`、`Evict` 方法调用。不把 `Replace` 列入规则，避免误伤 `strings.Replace`。

### autopsy/model.go、classifier.go

`model.go` 定义 `PodAutopsy` 及相关类型（见 Data Models），是采集层与其他模块之间的唯一数据契约。

```go
func Classify(a *PodAutopsy) Classification
```
规则顺序：OOMKilled → ImagePullBackOff/ErrImagePull → CrashLoopBackOff → ProbeFailure（附加）→ Error → Unknown。纯函数，表驱动测试，fixtures 驱动。

### autopsy/evidence.go

```go
func BuildEvidence(a *PodAutopsy, opts EvidenceOptions) ([]Evidence, error)
// EvidenceOptions{TailLines int; MaxChars int}
```
- 产出编号证据 `E1..En`：状态/退出码、limits、镜像、每条事件、日志关键行（最后 N 行 + 含 error/panic/exception/killed 的行）。
- 去重连续重复行；正则脱敏（`token`、`password`、`secret`、`Bearer xxx`、AWS key 形状等）→ `[REDACTED]`。
- 总字符数上限（默认约 6k token 对应的字符数）。

### spirit/persona.go + chat.go

- System prompt 要点（`internal/spirit/prompts/persona.md`，通过 `go:embed` 读入）：
  - 你是 Pod `<name>` 的亡灵，第一人称，阴森、简短（≤ 120 词），带一点黑色幽默。
  - 只能根据下方证据说话，每个事实后标 `[E#]`；证据里没有的，说「冥界没有留下记录」。
  - 用户问修复时，跳出一半角色，给出明确、可执行的建议，但不声称已执行。
  - 语言跟随用户。
- 死因专属「声线」小抄（按 cause 注入一段）：OOMKilled「窒息/越来越重」、CrashLoop「一次次复活又倒下」、ImagePull「我从未真正活过」、Probe「他们以为我死了，于是真的杀了我」。
- 前端解析 `[E#]`，渲染成可点开的证据卡片。

### llm/client.go、budget.go、replay.go

- 一个 OpenAI 兼容 Chat Completions 客户端，`stream=true`，用 `net/http` 直接实现：发送 POST 请求，逐行读取 `data: {...}` 的 SSE 分片，遇到 `data: [DONE]` 结束。不依赖任何 LLM SDK。
- 接口形如 `Stream(ctx context.Context, req ChatRequest, onDelta func(string) error) (Usage, error)`，`onDelta` 把增量文本转发给 SSE。
- 配置来源见下一节「配置」；mock 与录制/回放见「Mock 与录制/回放」。
- 超时 15 秒，失败重试 1 次（见 Error Handling）。
- 成本控制：
  - 证据包代替原始日志；对话历史只保留最近 4 轮 + 证据。
  - `budget.go`：每个 séance 默认最多 6 次调用、单次输入 ≤ 6k token；聊天调用（开场白、追问）输出 ≤ 400 token，复盘调用（`TurnIndex == PostmortemTurnIndex`）输出 ≤ 1500 token，复盘也只算一次调用。超限返回固定台词。
  - 开场白按 `(podUID, restartCount, model)` 缓存，重复召唤不重复花钱。
  - 复盘的时间线、证据小节由代码直接从 `PodAutopsy` 填，LLM 只写摘要、根因、建议和行动项。
- `replay.go`：`--replay` 时读取 `fixtures/replay/*.json`（经 `go:embed` 打包）。开场白（turnIndex 0）和复盘（turnIndex 100）按 `turnIndex` 匹配；追问按问题文本匹配：请求的最后一条用户消息经 `normalizeQuestion` 规范化后，与录音里的 `lastUser` 比较，多条命中时取 turnIndex 最小的，请求的 turnIndex 不必与录音相同。`SEANCE_RECORD=1` 时写入。

#### 配置

- 不预设任何模型厂商，也没有默认 base URL 或模型名。巨基之后提供接口地址和 API key。
- 配置项：base URL、API key、模型名，三者都必填（mock 模式除外）。
- 来源优先级：环境变量 `SEANCE_LLM_BASE_URL`、`SEANCE_LLM_API_KEY`、`SEANCE_LLM_MODEL` 优先；未设置时读取配置文件 `~/.k8s-seance/config.yaml`。
- 配置文件格式：

  ```yaml
  llm:
    base_url: <OpenAI 兼容接口地址，例如 https://example.invalid/v1>
    api_key: <your key>   # 建议用环境变量；若写入文件，文件权限设为 0600
    model: <模型名>
  ```
- 启动时校验：任一项缺失且未启用 mock / replay 时，日志和界面提示需要配置，并自动进入 mock 模式，不报错退出。
- 客户端只依赖 OpenAI 兼容的 `POST {base_url}/chat/completions`（`stream=true`）协议，换任何兼容服务都只改配置。

#### Mock 与录制/回放（同一个 Provider）

`internal/llm` 定义一个接口，三种实现共用：

```go
type Provider interface {
    Stream(ctx context.Context, req ChatRequest, onDelta func(string) error) (Usage, error)
}
// HTTPProvider：真实的 OpenAI 兼容接口
// ReplayProvider：读取 fixtures/replay/*.json。开场白和复盘按 turnIndex 回放，追问按规范化后的问题文本回放；找不到录音时退回 MockProvider
// MockProvider：按死因 + 证据拼出固定台词（模板），逐字模拟流式输出，带 [E#] 引用
```

- 模式选择：`SEANCE_LLM_MODE=http|replay|mock`，或命令行 `--replay` / `--mock`；默认：配置齐全则 `http`，否则 `mock`。
- 开发阶段默认用 mock：不需要任何 key 就能把对话、复盘、界面全部跑通，测试也用 mock。
- 拿到真实接口后，用 `SEANCE_RECORD=1` 把真实回答录成 `fixtures/replay/`，演示和评委试用走 replay，不联网、不花钱。
- 成本上限（budget）对三种 Provider 都生效，mock/replay 下照样计数，方便提前验证上限逻辑。

### postmortem/generator.go

```go
func GeneratePostmortem(ctx context.Context, a *autopsy.PodAutopsy, cls autopsy.Classification, transcript []Turn, llm llm.Streamer) (string /* markdown */, error)
```
模板 `template.md`（`go:embed`）固定 7 小节：Summary / Timeline / Root Cause / Evidence / Impact / Remediation / Action Items，另有一行 `> Last words: "..."`。

### graveyard/store.go（可选）

`~/.k8s-seance/graveyard.json`，追加写 `Tombstone` 列表。写入前整文件读取、合并、写临时文件后原子替换，避免半截文件。

### internal/web/server.go、templates/、static/

- `net/http` 提供路由和 SSE 端点；页面用 `html/template` 渲染，交互用 htmx（单个 js 文件，`go:embed` 打包）。
- 样式为手写 CSS（`static/style.css`），纯暗色主题，烛光和雾气效果用 CSS 动画实现。
- 不引入 React、Vue 等前端框架，也不引入任何前端构建链。

### 命令行入口 cmd/seance/main.go

- 解析 `--fixtures`、`--replay` 等参数，启动 `internal/web` 服务（默认 `localhost:6660`）。
- 后续 `seance summon <pod>` 等子命令复用 `internal/` 包。

### HTTP API

| Method | Path | 说明 |
| --- | --- | --- |
| GET | `/api/spirits?ns=default` | 亡灵列表 |
| POST | `/api/seances` | 召唤，返回 séance id |
| GET | `/api/seances/:id/stream` | SSE（开场白与回答） |
| POST | `/api/seances/:id/messages` | 追问 |
| POST | `/api/seances/:id/postmortem` | 生成复盘 |
| GET | `/api/graveyard` | 墓碑列表（可选） |

### demo/

- `kind-config.yaml`：单节点。
- `oom.yaml`：`memory-glutton`，`polinux/stress`（或同类公开镜像），limits.memory=64Mi，申请 256M。
- `crashloop.yaml`：`restless-spirit`，`busybox`，`command: ["sh","-c","echo 'FATAL: config /etc/app/config.yaml not found'; exit 1"]`。
- `imagepull.yaml`：`lost-soul`，`image: nginx:does-not-exist-666`。
- （可选）`probe.yaml`：liveness 指向不存在的路径。
- `kill-them-all.sh`：建 `seance-demo` 命名空间、apply、等待状态；`--cleanup` 只删该命名空间。
- `rbac-readonly.yaml`。

## Data Models

```go
type CauseOfDeath string

const (
    CauseOOMKilled        CauseOfDeath = "OOMKilled"
    CauseCrashLoopBackOff CauseOfDeath = "CrashLoopBackOff"
    CauseImagePullBackOff CauseOfDeath = "ImagePullBackOff"
    CauseError            CauseOfDeath = "Error"
    CauseProbeFailure     CauseOfDeath = "ProbeFailure"
    CauseUnknown          CauseOfDeath = "Unknown"
)

type ContainerState struct {
    Kind       string     // "running" | "waiting" | "terminated"
    Reason     string
    Message    string
    ExitCode   *int32     // nil 表示无退出码
    StartedAt  *time.Time
    FinishedAt *time.Time
}

type ContainerAutopsy struct {
    Name         string
    Image        string
    RestartCount int32
    State        ContainerState
    LastState    *ContainerState
    Limits       map[string]string
    Requests     map[string]string
    LogTail      []string
    PreviousLogTail []string // nil 表示「无记录」（Req 2.5）
}

type EventRecord struct {
    Type           string
    Reason         string
    Message        string
    Count          int32
    FirstTimestamp *time.Time
    LastTimestamp  *time.Time
}

type PodAutopsy struct {
    Namespace   string
    Name        string
    UID         string
    CreatedAt   time.Time
    Phase       string
    NodeName    string
    OwnerKind   string
    OwnerName   string
    Containers  []ContainerAutopsy
    Events      []EventRecord
    CollectedAt time.Time
    Source      string // "cluster" | "fixture"
}

type Classification struct {
    Cause        CauseOfDeath
    Secondary    []CauseOfDeath
    LastExitCode *int32
}

type Evidence struct {
    ID        string     // "E1"
    Kind      string     // "status" | "event" | "log" | "spec"
    Text      string
    Timestamp *time.Time
}

type Tombstone struct {
    PodName    string
    Namespace  string
    BornAt     time.Time
    DiedAt     time.Time
    Cause      CauseOfDeath
    Epitaph    string
    PostmortemMD string
}
```

JSON 持久化（fixtures 与墓地文件）使用结构体标签 `json:"..."`，字段名采用 camelCase，与 fixtures 的 JSON 文件保持一致。

## Error Handling

- kubeconfig 缺失 / API 不通：列表页显示错误 + 「进入样例模式」按钮（Req 1.3）。
- 权限不足（403）：提示需要的只读 RBAC，并给出 `demo/rbac-readonly.yaml` 路径。
- 无上一次日志：记为无记录（Req 2.5），即 `PreviousLogTail` 为 nil。
- LLM 失败或超时（15s）：重试 1 次；仍失败则用按死因的预写台词 + 证据列表兜底，复盘走纯模板。
- 预算耗尽：固定台词（Req 6.3）。
- Pod 在会话中被删除：使用已采集的 `PodAutopsy` 继续。
- 函数统一返回 `error`，错误用 `fmt.Errorf("...: %w", err)` 包装，调用方用 `errors.Is` / `errors.As` 判断 403、404 等类型。

## Testing Strategy

用 `go test ./...` 运行，测试与源码同包放置（`*_test.go`）。

- 表驱动单元测试：
  - `classifier`：每种死因至少 1 个 fixture + 边界（137 但无 OOM reason、CrashLoop 且 lastState OOMKilled）。
  - `evidence`：截断、去重、脱敏。
  - `budget`：调用次数和 token 上限。
- 只读守卫测试：扫描 `internal/` 源码，禁止 `Create`、`Update`、`Patch`、`Delete`、`Evict` 等写方法调用。
- 采集层测试：用 client-go 的 fake clientset 构造 Pod、Event、日志数据，不连真集群。
- 提示词快照测试：在 replay 模式下跑 3 个 fixture，断言开场白含正确死因关键词和至少一个 `[E#]`，并与 golden 文件比对。
- 集成冒烟：kind 集群 + `kill-them-all.sh`，脚本化检查 `/api/spirits` 返回 3 个且分类正确。
- 手工验收：按 spec.md 第 7 节逐条打勾。
