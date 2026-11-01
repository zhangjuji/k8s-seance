# Structure

```
/
├── .kiro/                 # specs + steering（必须提交，不能 gitignore）
├── cmd/
│   └── seance/
│       └── main.go        # 入口：解析 --fixtures/--replay，启动本地 Web 服务
├── internal/
│   ├── k8s/               # collector.go：client-go 只读采集；rbac 说明
│   ├── autopsy/           # model.go（PodAutopsy 等类型）、classifier.go（死因规则）、evidence.go（证据打包、截断、脱敏）
│   ├── spirit/            # persona.go（人设提示词）、chat.go（对话、证据引用）、prompts/*.md（go:embed）
│   ├── postmortem/        # generator.go、template.md（go:embed）
│   ├── llm/               # provider.go（Provider 接口）、client.go（HTTPProvider，OpenAI 兼容，net/http）、mock.go（MockProvider）、replay.go（ReplayProvider）、budget.go（调用/token 上限）
│   ├── graveyard/         # store.go（JSON 文件，可选功能）
│   └── web/               # server.go（net/http 路由 + SSE）
│       ├── templates/     # *.html（html/template）
│       └── static/        # htmx 单个 js 文件、style.css（go:embed）
├── fixtures/              # embed.go（go:embed 打包）；录好的 3 个亡灵：oom.json、crashloop.json、imagepull.json + replay/
├── demo/                  # kind 配置、3 份制造死亡的 manifest、kill-them-all.sh、rbac-readonly.yaml
├── docs/                  # 博客草稿、截图
├── go.mod
├── LICENSE                # MIT
└── README.md
```

约定：
- 采集层输出统一的 `autopsy.PodAutopsy` 对象，其他包只依赖它，不直接调 client-go。
- 提示词放在 `internal/spirit/prompts/*.md`，改提示词要同时更新快照测试（golden 文件）。
- 新功能先在 `.kiro/specs/k8s-seance/` 更新需求和任务，再写代码。
