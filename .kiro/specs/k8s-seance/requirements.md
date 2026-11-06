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
2. THE system SHALL 只调用 Kubernetes API 的 get、list、watch 类操作，代码中不包含任何 create、update、patch、delete 调用；唯一例外是 Requirement 19 的施法（`internal/rite`，默认关闭）。
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

### Requirement 10：英文演示路径

**User Story:** 作为看不懂中文的评委，我想用英文完整看完一场降灵会，这样几秒钟内就能看懂这个工具。

#### Acceptance Criteria

1. WHEN 浏览器首选语言是英文（`Accept-Language` 里第一个 zh/en 标签是 en，或两者都没有） THEN the system SHALL 用英文显示全部界面文字；没有 `Accept-Language` 时默认中文。
2. WHEN 用户点击顶栏的语言切换链接（`?lang=zh|en`） THEN the system SHALL 切换界面语言，并用 cookie 记住选择；启动参数 `-lang zh|en` 可固定默认语言。
3. WHEN 在英文界面召唤亡灵 THEN the system SHALL 以英文进行这场降灵会：英文开场请求、英文开场白（含 mock）、英文快捷问题、英文额度耗尽台词；回放读取 `fixtures/replay/<pod>-en.json`，没有录音时退回英文 mock。
4. WHEN 降灵会语言与当前界面语言不同 THEN the system SHALL 在页面上提示「切换语言只影响界面」。
5. 中文降灵会的系统提示词与原来逐字节一致，已有的 golden 快照不变。

### Requirement 11：谎言检测（证据核对）

**User Story:** 作为值班工程师，我想知道亡灵的哪句话和证据对不上，这样不会被 LLM 编出来的数字误导。

#### Acceptance Criteria

1. WHEN 亡灵的回答（开场白或追问）完成 THEN the system SHALL 用确定性规则核对其中的硬事实，不调用 LLM：`[E#]` 引用是否存在；内存数量（如 `256Mi`）、退出码、重启次数、时间戳是否出现在证据（或死亡时间）里。
2. WHEN 有对不上的说法 THEN the system SHALL 在该条回答下显示「亡灵可能在说谎」，逐条列出说法；JSON 接口（消息回复、séance 详情、SSE turn 事件）带 `doubts` 字段。
3. 修复建议句子里提到的新数值（如「建议调到 128Mi」）不算谎言，但引用、退出码、重启次数和时间照常核对。
4. 已录制的真实回放和全部 mock 回答必须零误报（有测试守护）。
5. 核对通过只表示「没有发现矛盾」，不表示「已验证」。

### Requirement 12：招魂铃（亡灵实时出现）

**User Story:** 作为值班 SRE，我想开着亡灵列表就能看到刚死的 Pod 出现，并立刻知道它是新死的、又死了一次，还是一批一起死的，这样不用反复刷新 `kubectl get pods`。

#### Acceptance Criteria

1. WHEN 列表页连接的是集群 THEN the system SHALL 用只读的 Pod watch 实时更新列表，无需刷新页面；样例模式不连接实时流。
2. WHEN 有 Pod 新变成亡灵 THEN the system SHALL 提示它的名字、死因和重启次数，高亮该行，并提供「立刻召唤」按钮；页面打开前就已死亡的 Pod 不提示。
3. WHEN 已有亡灵又死一次（重启次数增加或死因变化） THEN the system SHALL 提示重启次数从多少变成多少。
4. WHEN 60 秒内有 2 个及以上 Pod 以同一死因死亡 THEN the system SHALL 显示「集体墓葬」，列出这些 Pod，并按死因给出优先排查方向（如同时 OOM 先查 limit 和最近版本的内存回归）。
5. 招魂铃声音默认关闭，由用户点击开启；声音在浏览器内合成，不加载音频文件。所有动画遵守 `prefers-reduced-motion`。
6. 实时流断开时显示「连接中断，正在重连」，并自动重连。

### Requirement 13：复活咒语

**User Story:** 作为值班 SRE，我想要一份能直接复制、但我必须看过才会执行的修复方案，这样从「知道死因」到「修好」只差一步，而工具本身仍然不碰集群。

#### Acceptance Criteria

1. WHEN 用户在降灵会里点「念出复活咒语」 THEN the system SHALL 只根据已采集的尸检生成修复方案（不调用 LLM，不调用集群）：诊断（引用证据）、施咒对象、分步命令和补丁、注意事项。
2. THE system SHALL 把每一步标成「只读 · 查看」「补丁文件」「施法 · 会写集群」或「只读 · 确认」；每条会写集群的命令前面都有同一条命令的 `--dry-run=server` 版本。
3. THE system SHALL 不编造任何值：数值来自证据（如 memory limit 64Mi → 128Mi）；证据给不出的值（新的镜像 tag、本地配置文件、仓库账号）写成 `${NAME:?}` 形式的 shell 变量，未设置时命令直接报错停下。
4. 按死因给出方案：OOMKilled 有 limit 时调高 limit，没有 limit 时查节点内存；启动日志指出缺失文件时用 ConfigMap + subPath 挂载该文件；连不上依赖时查 Service/Endpoints；其他启动失败给 `kubectl debug --copy-to` 现场调试；镜像 tag 不存在、无权限、仓库不可达分别处理；探针超时调慢探针，探针 404/拒绝连接则不调慢、提示修路径或应用；证据不足时只给补全记录的命令。
5. 施咒对象从 ownerReference 推断（ReplicaSet → Deployment 时标注为推断）；裸 Pod 和 Job 提示改清单后重建，而不是原地 patch。
6. 补丁可以下载为 `resurrect-<pod>.yaml`，与命令里的文件名一致；补丁必须能被 strategic merge 正确应用到 `demo/` 下的清单（有测试守护）。

### Requirement 14：遗忘倒计时

**User Story:** 作为值班 SRE，我想知道一个亡灵的证据还能保留多久，这样能在 Kubernetes 把死因忘掉之前查清楚、写下复盘。

#### Acceptance Criteria

1. THE system SHALL 按「事件最后更新时间 + 事件 TTL」计算每条事件何时被遗忘；TTL 默认 1 小时（kube-apiserver 默认值），可用 `-event-ttl` 设置。
2. 亡灵的倒计时跟随最先过期的 Warning 事件：已停止重复的关键事件（如「镜像不存在」）会早于不断刷新的 BackOff 事件消失，它才是最先丢失的死因证据。没有 Warning 事件时显示「无事件记录」。
3. WHEN 倒计时低于 15 分钟 THEN 列表行变淡并显示横幅提醒「先召唤、生成复盘」；低于 5 分钟时闪烁；归零时显示「已被遗忘」。倒计时在浏览器里持续走动，实时列表刷新后继续。
4. 降灵会页显示遗忘面板：最先消散的关键记忆（链接到证据编号）、全部事件消散的时间；容器处于 CrashLoopBackOff 时提醒 `--previous` 日志只有一份，下次重启会被覆盖；提醒 Pod 删除后一切消失；提供「留住记忆：生成复盘」按钮。每张事件证据卡片单独倒计时。
5. 样例模式的倒计时按录制时刻计算，并在页面上说明。

### Requirement 15：亡灵祭坛（Ofrenda）

**User Story:** 作为用户，我想让写过复盘的亡灵被「记住」，并一眼看出哪些故障已经有复盘、哪些的证据还会丢。

#### Acceptance Criteria

1. 墓地页面以亡灵节（Día de los Muertos）祭坛的形式展示：彩色剪纸旗、阶梯式祭台（最近死亡的在最上层，每层 3、4、5 个）、带相框的「照片」、蜡烛和万寿菊花瓣小路，全部用 CSS 绘制。只使用民俗元素，不使用任何电影角色、歌曲、名称或画面（比赛规则禁止未经授权的第三方商标）。
2. WHEN 一个亡灵的复盘已在祭坛上 THEN 亡灵列表中该行显示「🌼 已供奉」，不再变淡，也不触发遗忘横幅：它的证据已经保存在复盘里。
3. 复盘面板的链接写作「已供上祭坛，它不会被遗忘」，导航与标题使用「祭坛 / Ofrenda」，路由保持 `/graveyard`。

### Requirement 16：转世对比（家族）

**User Story:** 作为值班 SRE，我想知道是这次发布改坏了，还是只是这一个实例出了事，这样能决定先回滚还是先查节点。

#### Acceptance Criteria

1. WHEN 召唤一个属于 Deployment、StatefulSet 或 DaemonSet 的亡灵 THEN the system SHALL 用一次只读的 Pod 列表找出同一家族的其他 Pod，按版本（pod-template-hash / controller-revision-hash）分成同胞（同版本）和其他版本（前世 / 转世）。
2. THE system SHALL 比较其他版本与亡灵版本的 Pod 模板：每个容器的镜像、命令、参数、环境变量、资源、探针、端口、挂载，以及 ServiceAccount、nodeSelector、卷。环境变量的值按证据规则脱敏，名字像密钥的变量不显示值；来自 Secret/ConfigMap 的只显示引用；隐藏的值变了要说「变了」但不显示。
3. THE system SHALL 给出判断：其他版本有活着的 Pod、而本版本无一存活 → 发布改坏了；有活着的同胞 → 实例问题（显示两边的节点）；全家都死 → 查共享的依赖与配置。
4. 家族信息作为证据（kind `lineage`）交给亡灵引用，谎言检测照常核对。降灵会页显示「家族与转世」面板：判断、同胞、各版本的存活数和差异表。
5. WHEN 判断为发布改坏且活着的是更早的版本 THEN 复活咒语优先给出 `rollout undo`（先 `--dry-run=server`），并说明回滚只是止血。
6. `demo/second-life.yaml`：健康的 v1 被 `kill-them-all.sh` 用一次坏的发布（`MODE=turbo`）弄坏，旧 Pod 继续活着（`maxUnavailable: 0`）。

### Requirement 17：降灵板

**User Story:** 作为用户，我想看到亡灵此刻在依据哪条证据说话，而且一眼看出它是否在撒谎。

#### Acceptance Criteria

1. 降灵会页有一块降灵板：板上的「符文」就是本次降灵会的证据编号，另有「是 / 否 / 再会」（YES / NO / GOODBYE）和数字行。英文界面称 spirit board，不使用 Ouija 商标。
2. WHEN 亡灵的回答流式输出并引用 [E#] THEN 乩板依次移到对应符文，并点亮右侧的证据卡片。
3. WHEN 一条回答结束 THEN 谎言检测发现矛盾时乩板移到「否」并抖动，否则移到「是」；生成复盘后移到「再会」。
4. 板可以折叠；遵守 `prefers-reduced-motion`；手机上不遮挡、不裁切。
5. 回答里的 `**粗体**` 和 `` `代码` `` 在转义之后渲染，不能注入任何标签。

### Requirement 18：供品与归来

**User Story:** 作为值班 SRE，我想在祭坛上一眼看到每次故障最关键的证据，并且在写过复盘的故障再次发生时立刻知道，这样能发现修复没有生效。

#### Acceptance Criteria

1. WHEN 一个亡灵被供上祭坛 THEN the system SHALL 同时保存最多 3 件「供品」：死状（原因与退出码）、遗言（第一行关键日志，没有就用最后一行）、发布改坏时两世之间的那一处改动，再按死因补上内存上限、拉不到的镜像或第一个 Warning 事件。供品来自已脱敏的证据，记下当时的证据编号。
2. 墓碑记下亡灵的家族（Deployment / StatefulSet / DaemonSet）。WHEN 已供奉的 Pod 在立碑之后又死了（死亡时间更新），或同一家族的另一个 Pod 在立碑之后死了 THEN the system SHALL 记为一次「归来」（同一 Pod 每个新的死亡时间算一次，家族里的其他 Pod 各算一次），写入祭坛文件。
3. 归来显示在：列表行（🕯️ 第 N 次归来，链接到它的照片）、招魂铃（单独的提示，替代普通的「又死了一次」，铃声敲两下）、祭坛照片（每次归来一支小蜡烛，最多 7 支）和照片页（提醒「之前的修复可能没有生效」并列出供品）。
4. 重新生成复盘会替换墓碑并把归来计数清零。
5. 样例模式不会产生归来（fixtures 的死亡时间早于立碑时间）。

### Requirement 19：施法（真的复活）

**User Story:** 作为在演示集群上玩的人，我想直接在降灵会里施法让亡灵复活，并亲眼看到它醒来。

#### Acceptance Criteria

1. 默认只读：只有以 `--allow-resurrect` 启动、且亡灵来自集群时，复活咒语面板才出现施法表单；否则显示原因（未开启 / 样例模式），施法接口返回 403。
2. 可施法的咒语：调高内存上限、挂载缺失的配置文件（内容由用户粘贴，存进 ConfigMap）、替换镜像（tag 由用户填写并校验）、回滚 Deployment 到仍活着的版本、放宽探针计时。需要密钥或调试会话的咒语只能阅读。目标必须是 Deployment / StatefulSet / DaemonSet。
3. 每次施法都可以先「试咒」：所有写请求带 `dryRun=All`，API 服务器校验但不保存。
4. 施法后页面只读地确认结果：家族里至少一个 Pod 运行且就绪满 20 秒、并且没有 Pod 在失败 → 复活；施法后新建的 Pod 又失败 → 又倒下了；否则等待。复活时，若亡灵已供奉，祭坛照片标记「已复活」。
5. 写操作只存在于 `internal/rite`，只有 `internal/web` 可以引用它（测试守护）；施法权限是 `demo/rbac-resurrect.yaml` 里只作用于 `seance-demo` 命名空间的 Role。

## Non-Functional Requirements

- 首屏加载 < 2 秒（本地）；亡灵首个 token 在 5 秒内开始输出（取决于模型）。
- 单个开发者 2～3 周可完成；依赖尽量少。
- 仓库公开，MIT 许可证，根目录保留 `/.kiro`。
