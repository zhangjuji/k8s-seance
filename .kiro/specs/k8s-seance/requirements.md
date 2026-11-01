# Requirements Document

## Introduction

K8s 降灵会（K8s Séance）是一个本地运行的 Web 应用：用户从集群里挑一个已经死亡或反复死亡的 Pod，「召唤」它的亡灵，亡灵根据只读采集到的 Pod 状态、退出码、K8s 事件和日志，用第一人称、阴森的语气回答「你是怎么死的」，降灵会结束时自动生成结构化的故障复盘（postmortem）。没有集群时可用录好的样例数据演示。

本项目参加 Kiroween 2026 Frankenstein 赛道：把 K8s 排障、降灵会式角色扮演聊天和自动复盘这三样不相干的东西缝成一个能用的工具。

## Glossary

- **Spirit（亡灵）**：一个已终止、重启过或无法启动的 Pod 的拟人化身份。
- **PodAutopsy（尸检）**：采集层输出的统一数据对象，包含 Pod 元数据、容器状态、退出码、reason、重启次数、事件和日志尾部。
- **Cause of Death（死因）**：分类器给出的枚举值：`OOMKilled`、`CrashLoopBackOff`、`ImagePullBackOff`、`Error`、`ProbeFailure`、`Unknown`。
- **Evidence（证据）**：带编号的事实片段（如 `E3: exitCode=137`），亡灵回答时必须引用。
- **Séance（降灵会）**：与一个亡灵的一次对话会话。
- **Fixtures mode（样例模式）**：不连集群，读取 `fixtures/*.json` 的模式；Replay 模式额外回放录好的 LLM 输出。

## Requirements

### Requirement 1：列出亡灵

**User Story:** 作为值班 SRE，我想一打开就看到集群里死过的 Pod 和它们的死因，这样我能马上选一个开始排查。

#### Acceptance Criteria

1. WHEN 应用启动且当前 kubeconfig context 可用 THEN the system SHALL 列出所选命名空间中满足任一条件的 Pod：容器 `restartCount > 0`、存在 `lastState.terminated`、`state.terminated` 的 exitCode 非 0、或 `state.waiting.reason` 属于异常原因集合。
2. WHEN 列出 Pod THEN the system SHALL 为每个 Pod 显示名称、命名空间、死因徽章、重启次数和最近一次死亡时间。
3. IF kubeconfig 不可用或 API Server 无法连接 THEN the system SHALL 显示可读的错误提示，并提供切换到样例模式的入口。
4. WHERE 命名空间参数未指定 the system SHALL 默认使用 `default` 命名空间，并允许在界面上切换命名空间。

### Requirement 2：只读采集尸检数据

**User Story:** 作为 SRE，我希望工具只读我的集群，这样我敢在真实环境里用它。

#### Acceptance Criteria

1. WHEN 用户召唤一个亡灵 THEN the system SHALL 采集该 Pod 的 status、各容器 `state`/`lastState`、退出码、reason、message、资源 limits/requests、镜像、相关 Events，以及当前容器和上一次容器（`previous=true`）的日志尾部。
2. THE system SHALL 只调用 Kubernetes API 的 get、list、watch 类操作，代码中不包含任何 create、update、patch、delete 调用。
3. WHEN 日志超过配置的尾部行数（默认 200 行）THEN the system SHALL 截断并去除连续重复行。
4. WHEN 打包证据 THEN the system SHALL 对疑似密钥、令牌、密码的字符串做脱敏替换后再发送给 LLM。
5. IF 上一次容器日志不存在 THEN the system SHALL 将其记为「无记录」并继续，而不是报错中止。

### Requirement 3：死因分类

**User Story:** 作为用户，我希望死因判断是确定、可复现的，这样亡灵讲的故事有可靠的骨架。

#### Acceptance Criteria

1. WHEN 任一容器 `lastState.terminated.reason` 或 `state.terminated.reason` 为 `OOMKilled`（或 exitCode 为 137 且有 OOM 事件）THEN the system SHALL 将死因分类为 `OOMKilled`。
2. WHEN `state.waiting.reason` 为 `ImagePullBackOff` 或 `ErrImagePull` THEN the system SHALL 将死因分类为 `ImagePullBackOff`。
3. WHEN `state.waiting.reason` 为 `CrashLoopBackOff` 且不满足 1.1 的条件 THEN the system SHALL 将死因分类为 `CrashLoopBackOff`，并记录最近一次退出码。
   - 实测发现（真实 kind v1.37）：崩溃循环的容器常见的观察形态是 `state.terminated.reason=Error`、`restartCount>0`，并伴随 reason 为 `BackOff`、消息以 `Back-off restarting failed container` 开头的 Warning 事件；分类器同样将其判为 `CrashLoopBackOff`。
4. WHEN 存在 reason 为 `Unhealthy` 且消息包含 Liveness、Readiness 或 Startup probe 的事件 THEN the system SHALL 将探针失败作为死因或附加死因。
5. WHEN 容器以非 0 退出码终止且不匹配以上规则 THEN the system SHALL 分类为 `Error`。
6. IF 以上规则都不匹配 THEN the system SHALL 分类为 `Unknown`。
7. THE system SHALL 在不调用 LLM 的情况下完成死因分类。

### Requirement 4：与亡灵对话

**User Story:** 作为用户，我想像问人一样问亡灵「你是怎么死的」，这样不用记命令也能搞清楚原因，而且过程好玩。

#### Acceptance Criteria

1. WHEN 降灵会开始 THEN the system SHALL 由亡灵以第一人称做开场陈述，内容包含死因、关键证据和死亡时间。
2. WHEN 用户发送问题 THEN the system SHALL 以流式方式返回亡灵的回答。
3. THE system SHALL 让每条亡灵回答附带至少一个证据编号引用，并在界面上可展开查看对应原文。
4. IF 用户的问题无法由证据回答 THEN the system SHALL 让亡灵明确表示没有相关记录，并且不编造事实。
5. WHEN 用户询问如何修复 THEN the system SHALL 给出与死因对应的修复建议（例如调整 memory limit、修正镜像 tag、修正启动命令或探针配置），且只作为建议，不执行任何操作。
6. THE system SHALL 让亡灵的回答语言跟随用户提问的语言（中文或英文）。

### Requirement 5：生成故障复盘

**User Story:** 作为 SRE，我希望聊完就有一份能直接贴进事故文档的复盘，这样省掉事后写文档的时间。

#### Acceptance Criteria

1. WHEN 用户点击「结束降灵会」THEN the system SHALL 生成 Markdown 复盘，包含：摘要、时间线、根因、证据、影响、修复建议、后续行动项 7 个小节。
2. THE system SHALL 用正式、非角色扮演的语气书写复盘正文，另可附一句亡灵的「遗言」。
3. WHEN 复盘生成完成 THEN the system SHALL 提供复制到剪贴板和下载 `.md` 文件两个操作。
4. THE system SHALL 让时间线条目使用来自 Events 和容器状态的真实时间戳。

### Requirement 6：成本控制

**User Story:** 作为个人开发者，我希望每次降灵会花的钱可预期且很少，这样可以随便演示。

#### Acceptance Criteria

1. THE system SHALL 通过环境变量或配置文件（`~/.k8s-seance/config.yaml`，环境变量优先）配置 LLM 的 base URL、模型名和 API key，并支持任意 OpenAI 兼容接口，不预设默认厂商。
2. WHILE 一个降灵会进行中 the system SHALL 限制 LLM 调用次数（默认 6 次）和单次输入 token（默认 6k）。
3. IF 达到调用上限 THEN the system SHALL 提示「仪式之力已耗尽」，并仍然允许生成复盘（使用已有回答和规则模板）。
4. THE system SHALL 只把证据包（而不是完整日志）发送给 LLM。

### Requirement 7：离线样例与回放模式

**User Story:** 作为参赛者，我希望没有集群、没有网络时也能稳定演示，这样录视频和评委试用都不怕出意外。

#### Acceptance Criteria

1. WHEN 以 `--fixtures` 启动 THEN the system SHALL 从 `fixtures/` 读取 3 个样例亡灵（OOMKilled、CrashLoopBackOff、ImagePullBackOff），不连接任何集群。
2. WHEN 以 `--fixtures --replay` 启动 THEN the system SHALL 回放 `fixtures/replay/` 中录好的 LLM 输出，不发起任何网络请求。
3. WHERE 设置了 `SEANCE_RECORD=1` the system SHALL 将真实采集结果和 LLM 输出录制成 fixtures 格式。
4. IF LLM 的 base URL、API key 或模型名任一未配置，或以 `--mock` 启动 THEN the system SHALL 使用 mock Provider，按死因和证据生成带 `[E#]` 引用的固定台词并模拟流式输出，不发起任何网络请求。
5. WHEN 以 `--replay` 启动且某一轮没有录好的输出 THEN the system SHALL 退回 mock Provider 生成该轮回答。

### Requirement 8：演示集群与制造死亡

**User Story:** 作为参赛者，我想一条命令在本地集群里造出三种死法，这样演示是真实的。

#### Acceptance Criteria

1. WHEN 运行 `./demo/kill-them-all.sh` THEN the system SHALL 在 kind 集群中部署 3 个 workload，分别在 2 分钟内进入 OOMKilled、CrashLoopBackOff、ImagePullBackOff 状态。
2. THE system SHALL 只使用公开可拉取的小镜像来制造 OOMKilled 和 CrashLoopBackOff。
3. WHEN 运行 `./demo/kill-them-all.sh --cleanup` THEN the system SHALL 删除这 3 个 demo workload（仅限 demo 命名空间）。
4. THE system SHALL 提供只读 RBAC 示例（ClusterRole 仅含 pods、pods/log、events 的 get/list/watch）。

### Requirement 9（可选，低优先级）：故障墓地

**User Story:** 作为用户，我想把聊过的亡灵存成墓碑，这样能回看历次故障，也是演示里的记忆点。

#### Acceptance Criteria

1. WHEN 一个降灵会生成复盘 THEN the system SHALL 在本地 JSON 文件中新增一块墓碑：Pod 名、命名空间、生卒时间、死因、墓志铭、复盘内容。
2. WHEN 用户打开墓地页面 THEN the system SHALL 以墓碑网格展示所有记录，按死亡时间倒序。
3. WHEN 用户点击墓碑 THEN the system SHALL 显示该亡灵的复盘。

## Non-Functional Requirements

- 首屏加载 < 2 秒（本地）；亡灵首个 token 在 5 秒内开始输出（取决于模型）。
- 单个开发者 2～3 周可完成；依赖尽量少。
- 仓库公开，MIT 许可证，根目录保留 `/.kiro`。
