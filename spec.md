# K8s 降灵会（K8s Séance）产品规格 · MVP

> Kiroween 2026 · Frankenstein 赛道 · 单人参赛（巨基）
> 提交截止：北京时间 2026-12-05 06:00（= 太平洋时间 2026-12-04 14:00）。内部目标：12/04 白天提交完。
> 版本：v0.1（2026-11-01）

## 1. 一句话产品

召唤一个已经挂掉的 K8s Pod 的亡灵，听它用第一人称、阴森的语气讲清楚「我是怎么死的」，聊完自动给你一份能直接贴进事故文档的故障复盘。

## 2. Frankenstein 卖点（缝了哪三块）

| 缝进来的部件 | 本来是什么 | 在怪物里干什么 |
| --- | --- | --- |
| K8s 排障 | 冷冰冰的 `kubectl describe` / `logs --previous` / Events / 退出码 | 亡灵的「记忆」和「尸检报告」，所有说法都必须有它作证 |
| 降灵会 / 角色扮演聊天 | 万圣节通灵仪式、LLM 人设对话 | 交互方式：你提问，亡灵回答，不用记命令 |
| 自动复盘（postmortem） | SRE 事后写的严肃文档 | 降灵会结束时的「验尸报告」，结构化可复制 |

看起来完全不搭的三样东西缝成一个能用的工具：好玩是外皮，排障和复盘是骨头。演示视频和 Devpost 文案都要直说这一点。

## 3. 目标用户

- 主要：值班的 SRE / 平台工程师 / 后端开发，Pod 挂了想快点知道原因，也要写复盘。
- 次要：刚学 K8s 的人，看不懂 OOMKilled、CrashLoopBackOff 这些状态，用对话的方式学。
- 评委：几秒钟内就要看懂这是什么、哪里好玩、确实能用。

## 4. 核心场景

1. **召唤**：打开本地网页，左侧列出集群里「死过」的 Pod（有 terminated 状态、重启次数 > 0、Waiting 原因异常），每个带死因徽章。点一个，进入降灵会。
2. **问话**：亡灵先自我介绍并讲死因（例：「内存……越来越重……然后内核把我掐断了。退出码 137。」）。用户可以追问「死前最后一句日志是什么？」「我该怎么救你？」。回答只能依据采集到的证据，并标出依据（哪条事件 / 哪行日志）。
3. **复盘**：点「结束降灵会」，生成 Markdown 复盘：摘要、时间线、根因、证据、影响、修复建议、后续行动项，支持一键复制和下载 `.md`。
4. **（可选）墓地**：每个聊过的亡灵变成一块墓碑：名字、生卒时间、死因、墓志铭（一句话），点开能看复盘。

## 5. MVP 功能范围

### 5.1 要做

- F1 只读采集：从当前 kubeconfig 上下文读取 Pod 状态、容器 `lastState`/`state`、退出码与 reason、重启次数、相关 Events、当前/上一次容器日志的尾部（默认 200 行）。
- F2 死因分类：确定性规则（不靠 LLM）识别 6 类：`OOMKilled`、`CrashLoopBackOff`、`ImagePullBackOff`/`ErrImagePull`、`Error`（非零退出码）、探针失败（Liveness/Readiness/Startup 事件）、`Unknown`。
- F3 亡灵对话：LLM 角色扮演，第一人称、阴森但讲清楚；中英文跟随用户提问语言（演示用英文）；流式输出。
- F4 证据约束：每次回答都附带「证据」引用；证据里没有的事要说「冥界没有留下记录」，不能编。
- F5 复盘生成：固定模板的 Markdown，复制 / 下载。
- F6 降灵会界面：单页网页，暗色主题、烛光/雾气的轻量 CSS 效果，死因徽章，聊天框。
- F7 离线样例模式：`--fixtures` 启动，用录好的 3 个亡灵（OOMKilled / CrashLoopBackOff / ImagePullBackOff）的 JSON 数据，不需要集群；也可配合「录制回放」的 LLM 回答，做到完全不联网也能演示。
- F8 制造死亡的脚本：`demo/` 下 3 份 manifest + 1 个脚本，在 kind 集群里一键制造三种死法。
- F9 成本控制：每次降灵会有调用上限和 token 上限；日志先截断、去重、脱敏再送 LLM。

### 5.2 明确不做

- 不写任何集群资源：不重启、不删 Pod、不改 Deployment（只读 RBAC）。「复活」只给建议，不执行。
- 不做用户登录、多租户、云端托管 SaaS。
- 不接 Prometheus / Loki / 告警系统等外部可观测性数据源。
- 不做多集群切换 UI（只用当前 kubeconfig context）。
- 不做历史持久化数据库（墓地用本地 JSON 文件）。
- 不做移动端适配、不做语音。

### 5.3 可选加分项（按优先级）

1. 故障墓地页面（F10，低优先级，时间够再做）。
2. 亡灵语音（浏览器自带 Web Speech API 朗读，零成本）。
3. Kiro hook：保存 `demo/*.yaml` 时自动校验 manifest；或保存提示词时跑提示词快照测试。用来展示 Kiro 用法。
4. 一个 CLI 别名 `seance summon <pod>`，在终端里打印复盘（复用同一个核心库）。

## 6. 演示脚本要点（总长 ≤ 2:50，留 10 秒余量）

| 时间 | 画面 | 要讲 / 要演示的 |
| --- | --- | --- |
| 0:00–0:15 | 黑屏 + 烛光，标题「K8s Séance」 | 钩子：「你的 Pod 死了。与其翻日志，不如直接问它。」 |
| 0:15–0:35 | 终端：`kind create cluster` 已建好，运行 `./demo/kill-them-all.sh`，`kubectl get pods` 出现 OOMKilled、CrashLoopBackOff、ImagePullBackOff | 三种死法是真实制造的，不是假数据 |
| 0:35–1:15 | 网页：亡灵列表 → 召唤 OOMKilled 的 `memory-glutton` | 亡灵讲死因；追问「最后一句日志」；展示回答下面的证据引用（exit code 137、limits 64Mi） |
| 1:15–1:40 | 召唤 CrashLoopBackOff 的 `restless-spirit` | 亡灵「一次次复活又倒下」，指出启动命令的报错行和重启次数 |
| 1:40–2:00 | 召唤 ImagePullBackOff 的 `lost-soul` | 「我从没真正活过」，指出拉不到的镜像 tag |
| 2:00–2:25 | 结束降灵会 → 复盘 Markdown，点复制 / 下载 | 「好玩」的外皮下是一份正经可用的复盘 |
| 2:25–2:40 | （若做了）故障墓地，3 块墓碑 | 记忆点画面 |
| 2:40–2:50 | Kiro 画面：`.kiro/specs`、steering、tasks 勾选进度 | 怎么用 Kiro 的 spec 驱动开发出来的；Frankenstein = K8s 排障 + 降灵会 + 自动复盘 |

录制备注：真实集群录一遍；同时备一份 `--fixtures` 模式的素材，集群或网络出问题时用它补镜头。视频不用受版权保护的音乐。

## 7. 验收标准（MVP 完成的定义）

1. 在全新 kind 集群里执行 `./demo/kill-them-all.sh` 后 2 分钟内，三个 Pod 分别处于 OOMKilled（或其 CrashLoop 中 lastState 为 OOMKilled）、CrashLoopBackOff、ImagePullBackOff。
2. 列表页能列出这 3 个 Pod，死因徽章分类 3/3 正确（规则判断，与 LLM 无关）。
3. 每个亡灵的首条回答都点出正确死因，并至少引用 1 条真实证据（退出码、事件或日志行）。
4. 问一个证据里没有的问题（例「谁删了你？」），亡灵回答没有记录，而不是编一个答案。
5. 复盘包含 7 个固定小节（摘要、时间线、根因、证据、影响、修复建议、行动项），可复制、可下载 `.md`。
6. 应用使用的 ServiceAccount / kubeconfig 只需要 `get`/`list`/`watch` pods、pods/log、events 权限；代码中没有任何写操作调用。
7. 没有集群、没有 LLM key 时，`--fixtures --replay` 能完整跑通演示流程。
8. 单次降灵会（5 轮对话 + 1 份复盘）的 LLM 调用 ≤ 6 次，输入 token ≤ 30k（可配置）；LLM 接口地址、key、模型名全部可配置，不绑定厂商；未配置时自动用 mock，整条流程仍能跑通。
9. 公开 GitHub 仓库：根目录有 `/.kiro`（specs + steering，未被 .gitignore）、开源 LICENSE（GitHub About 区能识别）、README 有运行说明。
10. 提交物：< 3 分钟的公开 YouTube 视频；Devpost 提交表填好 Frankenstein 赛道和「如何使用 Kiro」的说明；博客与 #KiroweenHackathon26 社交帖（帖子必须以该 hashtag 开头）。

## 8. 比赛规则核对（2026-11-01 查 Devpost 规则页）

- 提交期：2026-10-30 09:00 PT ～ 2026-12-04 14:00 PT；只能是这期间新建的项目。
- 必须用 Kiro（IDE / CLI / Web / Crew 均可）；仓库根目录必须有 `/.kiro`，不能放进 `.gitignore`。
- 仓库必须公开且带开源许可证文件（GitHub About 区可见）。
- 视频 < 3 分钟，公开上传 YouTube，展示项目真实运行，不能有未经授权的第三方商标或音乐。
- 提交表里要写「如何使用 Kiro」的说明（spec、steering、hooks、MCP 等）。
- 注意：规则页里**博客和社交帖属于加分奖项**（Bonus Blog Prize、Social Blitz），不是参赛硬性要求；但计划照做。博客必须写明是为 Kiroween 2026 而写；社交帖必须以 `#KiroweenHackathon26` 开头，并写对 Kiro 最喜欢的一点。
- Kiro 积分：积分申请已于 11-01 提交；待 Devpost 发 Didit 身份验证邮件并完成验证。积分码 12-04 前兑换。
