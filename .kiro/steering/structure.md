# Structure

```
/
├── .kiro/                    # specs + steering（必须提交，不能 gitignore）
├── cmd/
│   └── seance/
│       ├── main.go           # 入口：解析 --fixtures/--replay/--allow-resurrect 等，启动本地 Web 服务；summon 子命令
│       └── record.go         # 录制 LLM 回复到 fixtures/replay/
├── internal/
│   ├── k8s/                  # client-go 只读采集
│   │   ├── collector.go      # 列出亡灵、采集 PodAutopsy
│   │   ├── watch.go          # Pod watch（召唤铃，实时列表）
│   │   ├── lineage.go        # 家族与转世对比（同 Deployment/StatefulSet/DaemonSet 的 revision）
│   │   └── vitals.go         # 遗忘倒计时：关键证据何时被 Kubernetes 删除
│   ├── rite/                 # 施法：唯一会写集群的包，仅在 --allow-resurrect 时使用
│   │   └── rite.go
│   ├── resurrect/            # 复活咒语：纯规则生成修复步骤（不调 LLM，不写集群）
│   │   └── spell.go
│   ├── autopsy/              # 死因与证据
│   │   ├── model.go          # PodAutopsy 等类型
│   │   ├── classifier.go     # 死因规则（OOMKilled / CrashLoopBackOff / ImagePullBackOff / ...）
│   │   ├── evidence.go       # 证据打包、截断、脱敏
│   │   └── memory.go         # 首个 Warning 事件何时被遗忘
│   ├── spirit/               # 降灵对话
│   │   ├── persona.go        # 死亡时间、人设相关辅助
│   │   ├── prompts.go        # 系统提示词组装（go:embed prompts/）
│   │   ├── prompts/
│   │   │   ├── persona.md    # 人设提示词
│   │   │   └── voices/*.md   # 每种死因的第一人称语气（go:embed）
│   │   ├── chat.go           # 对话、证据引用
│   │   ├── factcheck.go      # 谎言检测：回复中的数字、引用、时间与证据比对
│   │   └── testdata/golden/  # 提示词与回复快照（golden）
│   ├── postmortem/           # 故障复盘
│   │   ├── generator.go      # 生成复盘（LLM 写摘要/根因/修复，其余由数据生成）
│   │   ├── rules.go          # 规则模板，LLM 不可用或预算用尽时兜底
│   │   └── template.md       # go:embed
│   ├── llm/                  # OpenAI 兼容接口，net/http 自己实现
│   │   ├── provider.go       # Provider 接口与模式选择
│   │   ├── config.go         # 配置：环境变量优先，读取 ~/.k8s-seance/config.yaml
│   │   ├── client.go         # HTTPProvider（stream=true）
│   │   ├── mock.go           # MockProvider（开发用，无需 key）
│   │   ├── replay.go         # ReplayProvider（读 fixtures/replay/）
│   │   └── budget.go         # 调用次数与 token 上限
│   ├── graveyard/            # 墓碑：本地 JSON 文件（可选功能）
│   │   ├── store.go          # 读写 graveyard.json
│   │   └── offerings.go      # 祭品：墓碑上的证据摘要
│   ├── web/                  # net/http 路由 + SSE
│   │   ├── server.go         # 路由、首页、召唤列表
│   │   ├── seance.go         # 降灵会会话状态
│   │   ├── seance_http.go    # 降灵会页面与接口
│   │   ├── spell_http.go     # 复活咒语页面
│   │   ├── rite_http.go      # 施法接口（仅 --allow-resurrect）
│   │   ├── live.go           # 召唤铃：SSE 实时推送
│   │   ├── source.go         # 数据源：fixtures 或集群
│   │   ├── memory.go         # 遗忘倒计时的页面展示
│   │   ├── altar.go          # 祭坛（ofrenda）
│   │   ├── graveyard_http.go # 墓地与墓碑页面
│   │   ├── lineage.go        # 家族面板的视图数据
│   │   ├── i18n.go           # 中英文文案与语言选择
│   │   ├── templates/        # *.html（html/template，go:embed）
│   │   └── static/           # htmx.min.js、sse.js、memory.js、style.css（go:embed）；VENDOR.md 记录 htmx 来源
│   └── readonly_test.go      # 守护：除 internal/rite 外禁止写集群调用
├── fixtures/                 # embed.go（go:embed 打包）
│   ├── oom.json、crashloop.json、imagepull.json   # 录好的 3 个亡灵
│   └── replay/               # LLM 录制回复（<pod>.json、<pod>-en.json）
├── demo/                     # 演示集群
│   ├── kind-config.yaml
│   ├── oom.yaml、crashloop.yaml、imagepull.yaml  # 制造死亡的 manifest
│   ├── second-life.yaml      # 滚动更新后死亡的家族
│   ├── kill-them-all.sh      # 一键部署并等待四个亡灵
│   ├── record-english.sh     # 录制英文回复
│   ├── rbac-readonly.yaml    # 只读 RBAC
│   └── rbac-resurrect.yaml   # 施法用的命名空间 Role（仅 seance-demo）
├── docs/                     # 博客草稿、截图
├── spec.md、linear-plan.md、linear-issues.json   # 项目根目录的规划文件（已提交）
├── go.mod、go.sum
├── LICENSE                   # MIT
└── README.md
```

约定：
- 采集层输出统一的 `autopsy.PodAutopsy` 对象，其他包只依赖它，不直接调 client-go。
- 提示词放在 `internal/spirit/prompts/`（含 `voices/`），改提示词要同时更新快照测试（`internal/spirit/testdata/golden/`）。
- 新功能先在 `.kiro/specs/k8s-seance/` 更新需求和任务，再写代码。
