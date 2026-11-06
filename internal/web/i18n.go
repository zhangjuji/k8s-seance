package web

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/zhangjuji/k8s-seance/internal/llm"
)

// UI language selection (Requirement 10). The interface language of a request
// is, in order: a valid ?lang= value, the seance_lang cookie, the -lang default
// (Options.Lang), the Accept-Language header, and Chinese when there is no header.
const (
	// langParam is the query parameter of the language switch link.
	langParam = "lang"
	// langCookie remembers the language picked with ?lang=.
	langCookie = "seance_lang"
	// langCookieMaxAge is one year, in seconds.
	langCookieMaxAge = 365 * 24 * 60 * 60
)

// uiLangs lists the interface languages. Each one gets its own template set.
var uiLangs = []string{llm.LangZH, llm.LangEN}

// catalog holds every interface string by language and key. Templates read it
// through {{T "key"}} and {{Tf "key" args}}, Go code through tr and trf. Both
// languages must have the same keys (TestCatalogKeysMatch). The postmortem body
// is English in both languages and is not translated.
var catalog = map[string]map[string]string{
	llm.LangZH: {
		"html_lang":        "zh-CN",
		"app_name":         "K8s 降灵会",
		"nav_cluster":      "集群亡灵",
		"nav_fixtures":     "样例模式",
		"nav_graveyard":    "祭坛",
		"lang_switch":      "English",
		"lang_switch_aria": "切换界面语言",
		"lang_name_zh":     "中文",
		"lang_name_en":     "英文",

		"list_title":          "亡灵列表",
		"fixture_banner":      "样例模式：亡灵来自内置 fixtures，不连接集群。样例不区分命名空间，列出全部 %d 个亡灵。",
		"ns_label":            "命名空间",
		"ns_switch":           "切换",
		"fixtures_link":       "进入样例模式（离线 fixtures，不连接集群）",
		"col_name":            "名称",
		"col_ns":              "命名空间",
		"col_cause":           "死因",
		"col_restarts":        "重启次数",
		"col_last_death":      "最近死亡（UTC）",
		"summon":              "召唤",
		"no_spirits":          "这里没有亡灵。",
		"cluster_down":        "连不上集群：%s",
		"cluster_no_conf":     "未配置集群连接",
		"read_spirits_failed": "读取亡灵失败：%s",

		"col_memory":          "记忆",
		"memory_none":         "无事件记录",
		"memory_left":         "%s后遗忘",
		"memory_gone":         "已被遗忘",
		"memory_lt1":          "不到 1 分钟",
		"memory_h":            "小时",
		"memory_min":          "分钟",
		"memory_banner":       "有亡灵的死因证据不到 15 分钟就会被 Kubernetes 删除：先召唤、生成复盘。",
		"memory_fixture_note": "样例模式：遗忘倒计时从每个亡灵的录制时刻算起，不是从现在算起。",
		"memory_title":        "遗忘倒计时",
		"memory_key":          "最先消散的关键记忆：",
		"memory_key_at":       "Kubernetes 删除它的时间（UTC）：%s",
		"memory_all_at":       "全部事件消散的时间（UTC）：%s",
		"memory_previous":     "容器 %s 处于 CrashLoopBackOff：--previous 日志只有一份，下次重启就会被覆盖。",
		"memory_pod_gone":     "Pod 一旦被删除，状态、事件和日志都会一起消失。",
		"memory_fixture_asof": "样例模式：倒计时从录制时刻 %s 算起。",
		"memory_keep":         "留住记忆：生成复盘",

		"bell_connecting":  "正在挂起招魂铃……",
		"bell_ready":       "招魂铃已挂好，正在聆听新的亡灵。",
		"bell_trouble":     "连接中断，正在重连……",
		"bell_arrived":     "新亡灵：%s，死因 %s，已重启 %d 次。",
		"bell_worse":       "%s 又死了一次：重启次数 %d → %d（%s）。",
		"bell_worse_cause": "%s 又死了一次：重启次数 %d → %d，死因 %s → %s。",
		"bell_summon_now":  "立刻召唤",
		"bell_dismiss":     "知道了",
		"bell_aria":        "招魂铃提示",
		"bell_sound_on":    "铃声：开",
		"bell_sound_off":   "铃声：关",
		"mass_title":       "集体墓葬：%d 个 Pod 死于 %s",
		"mass_pods":        "同葬的亡灵：",

		"mass_hint_oomkilled":        "一起 OOM：先查 memory limit，再查最近一次发布有没有内存回归。",
		"mass_hint_crashloopbackoff": "一起崩溃循环：先查最近的发布、共用的 ConfigMap 和 Secret，以及它们共同依赖的服务。",
		"mass_hint_imagepullbackoff": "一起拉不到镜像：先查镜像仓库能否访问、拉取凭据，以及 tag 是否存在。",
		"mass_hint_probefailure":     "一起探针失败：先查探针配置，以及共同依赖（数据库、下游服务）的延迟。",
		"mass_hint_error":            "一起出错退出：先查共用的配置和依赖，以及最近的变更。",
		"mass_hint_unknown":          "死因不明：先查节点状态和这个命名空间的 Warning 事件。",

		"died_label":      "死亡时间（UTC）：%s",
		"meter":           "仪式之力：已用 %d / %d 次",
		"meter_js":        "仪式之力：已用 {used} / {total} 次",
		"who_spirit":      "亡灵",
		"who_user":        "你",
		"q_how_died":      "你是怎么死的？",
		"q_fix":           "怎么修复？",
		"ask_placeholder": "问亡灵一个问题",
		"ask_button":      "问",
		"finish":          "结束降灵会，生成复盘",
		"evidence":        "证据",
		"lang_mismatch":   "这场降灵会用%s进行。切换语言只影响界面。",
		"no_answer":       "亡灵没有应答，请稍后再试。",
		"pm_timeout":      "复盘生成超时，请稍后再试。",

		"board_title":   "降灵板",
		"board_yes":     "是",
		"board_no":      "否",
		"board_bye":     "再会",
		"board_hint":    "符文是本场的证据编号。亡灵引用 [E#] 时乩板移过去；回答结束时指向「是」，谎言检测发现矛盾时指向「否」；生成复盘后指向「再会」。",
		"board_said_js": "乩板指向：{rune}",
		"list_tagline":  "每个死去的 Pod 都留下了遗言：退出码、事件和日志。召唤它，让它亲口说。",

		"doubt_title":    "亡灵可能在说谎",
		"doubt_note":     "以下说法在证据里对不上。没有列出的话也只表示没有发现矛盾，不表示已验证。",
		"doubt_citation": "引用 %s 不在证据列表里",
		"doubt_memory":   "内存数量 %s 在证据里找不到",
		"doubt_exitCode": "退出码说法「%s」与证据不符",
		"doubt_restarts": "重启次数说法「%s」与证据不符",
		"doubt_time":     "时间 %s 与证据和死亡时间都对不上",

		"pm_aria":        "复盘",
		"pm_copy":        "复制",
		"pm_copied":      "已复制",
		"pm_copy_failed": "复制失败",
		"pm_download":    "下载 .md",
		"pm_tomb":        "已供上祭坛，它不会被遗忘",
		"pm_note":        "这份复盘由模板生成，没有调用亡灵的 LLM（原因：%s）。",

		"lineage_title":        "家族与转世",
		"lineage_owner":        "家族：%s",
		"lineage_inferred":     "（由 ReplicaSet %s 推断）",
		"lineage_v_instance":   "判断：实例问题。同一版本还有活着的同胞，问题出在这个实例身上。比较两边的节点：亡灵在 %s，活着的同胞在 %s。",
		"lineage_v_regression": "判断：发布改坏了。其他版本还有活着的 Pod，而本版本无一存活。",
		"lineage_v_family":     "判断：全家都死了。查它们共享的依赖与配置。",
		"lineage_v_alone":      "判断：没有同胞，也没有其他版本可以比较。",
		"lineage_revision":     "本版本 %s=%s · 存活 %d/%d",
		"lineage_self":         "亡灵",
		"lineage_siblings":     "同胞",
		"lineage_no_siblings":  "没有同胞。",
		"lineage_alive":        "活着",
		"lineage_dead":         "死了",
		"lineage_age_older":    "前世（更早的版本）",
		"lineage_age_newer":    "转世（更新的版本）",
		"lineage_alive_count":  "存活 %d/%d",
		"lineage_col_field":    "字段",
		"lineage_col_other":    "那个版本",
		"lineage_col_this":     "本版本",
		"lineage_hidden":       "变了（值已隐藏）",
		"lineage_no_diff":      "比较的字段没有差异。",
		"lineage_none_val":     "（无）",
		"lineage_none":         "没有家族记录：样例模式不列出 Pod，或者 Pod 列表读取失败。",
		"lineage_on_node":      "节点 %s",

		"spell_button":      "念出复活咒语",
		"spell_aria":        "复活咒语",
		"spell_note":        "咒语只根据已采集的尸检生成，没有调用 LLM，也没有访问集群。本工具不会执行任何命令：每一步都由你看过后自己运行，会写集群的命令先用 --dry-run=server 试一遍。",
		"spell_diagnosis":   "诊断",
		"spell_target":      "施咒对象",
		"spell_ns":          "命名空间",
		"spell_container":   "容器",
		"spell_inferred":    "推断",
		"spell_steps":       "步骤",
		"spell_step_check":  "只读 · 查看",
		"spell_step_file":   "补丁文件",
		"spell_step_cast":   "施法 · 会写集群",
		"spell_step_verify": "只读 · 确认",
		"spell_patch":       "补丁",
		"spell_download":    "下载 %s",
		"spell_caveats":     "注意事项",
		"spell_copy":        "复制",
		"spell_copied":      "已复制",
		"spell_copy_failed": "复制失败",
		"spell_no_patch":    "这道咒语没有补丁文件。",

		"gy_title":        "祭坛 · Ofrenda",
		"gy_intro":        "每个生成过复盘的亡灵都供在这座祭坛上。最近死去的在最上层，它们的证据已经保存在复盘里。",
		"gy_empty":        "祭坛还是空的。召唤一个亡灵并生成复盘，它的照片就会供在这里。",
		"gy_off":          "祭坛未开启（-graveyard 为空）",
		"gy_read_failed":  "读取祭坛失败，请检查祭坛文件（-graveyard）。",
		"gy_read_error":   "读取祭坛失败",
		"born":            "生：%s",
		"died":            "卒：%s",
		"tomb_back":       "← 回到祭坛",
		"tomb_title":      "祭坛上的 %s",
		"pm_heading":      "复盘",
		"altar_tier":      "祭台第 %d 层",
		"tomb_remembered": "它的复盘供在祭坛上：即使 Kubernetes 删掉了事件和日志，它也不会被遗忘。",
		"enshrined":       "🌼 已供奉",
		"enshrined_title": "复盘已供上祭坛，证据不会丢失",
		// Offerings and returns (Requirement 18).
		"return_mark":         "🕯️ 第 %d 次归来",
		"return_mark_title":   "复盘已供上祭坛之后它又死了：之前的修复可能没有生效",
		"bell_return_self":    "%s 归来了：复盘供上祭坛之后它又死了，这是第 %d 次归来（%s）。之前的修复可能没有生效。",
		"bell_return_family":  "%s 归来了：%s 家族在祭坛上的 %s 之后又死了一个，这是第 %d 次归来（%s）。之前的修复可能没有生效。",
		"bell_open_photo":     "看它的照片",
		"altar_returns":       "归来 %d 次",
		"tomb_returned":       "它已经归来 %d 次：之前的修复可能没有生效。",
		"tomb_return_item":    "%s 卒于 %s（%s）",
		"tomb_family":         "家族：%s",
		"offerings_heading":   "供品",
		"offerings_none":      "这块墓碑没有供品。",
		"offering_death":      "死状",
		"offering_last_words": "遗言",
		"offering_change":     "两世之间的改动",
		"offering_limit":      "内存上限",
		"offering_image":      "拉不到的镜像",
		"offering_warning":    "第一个 Warning 事件",
		"offering_evidence":   "当时的证据 %s",
	},
	llm.LangEN: {
		"html_lang":        "en",
		"app_name":         "K8s Séance",
		"nav_cluster":      "Cluster spirits",
		"nav_fixtures":     "Sample mode",
		"nav_graveyard":    "Ofrenda",
		"lang_switch":      "中文",
		"lang_switch_aria": "Switch the interface language",
		"lang_name_zh":     "Chinese",
		"lang_name_en":     "English",

		"list_title":          "Restless spirits",
		"fixture_banner":      "Sample mode: the spirits come from the built-in fixtures, and no cluster is contacted. Samples ignore the namespace, so all %d spirits are listed.",
		"ns_label":            "Namespace",
		"ns_switch":           "Switch",
		"fixtures_link":       "Open sample mode (offline fixtures, no cluster)",
		"col_name":            "Name",
		"col_ns":              "Namespace",
		"col_cause":           "Cause of death",
		"col_restarts":        "Restarts",
		"col_last_death":      "Last death (UTC)",
		"summon":              "Summon",
		"no_spirits":          "No spirits here.",
		"cluster_down":        "Cannot reach the cluster: %s",
		"cluster_no_conf":     "no cluster connection is configured",
		"read_spirits_failed": "Could not read the spirits: %s",

		"col_memory":          "Memory",
		"memory_none":         "No event record",
		"memory_left":         "forgotten in %s",
		"memory_gone":         "Forgotten",
		"memory_lt1":          "under 1 min",
		"memory_h":            "h",
		"memory_min":          "min",
		"memory_banner":       "Kubernetes deletes the evidence of a spirit's death in less than 15 minutes. Summon it and write the postmortem first.",
		"memory_fixture_note": "Sample mode: each countdown starts at the moment the spirit was recorded, not now.",
		"memory_title":        "Fading memory",
		"memory_key":          "First key memory to fade:",
		"memory_key_at":       "Kubernetes deletes it at (UTC): %s",
		"memory_all_at":       "All events are gone at (UTC): %s",
		"memory_previous":     "Container %s is in CrashLoopBackOff. Only one --previous log is kept, and the next restart overwrites it.",
		"memory_pod_gone":     "Once the Pod is deleted, its status, events and logs all go with it.",
		"memory_fixture_asof": "Sample mode: the countdown starts at the recording time, %s.",
		"memory_keep":         "Keep the memory: write the postmortem",

		"bell_connecting":  "Hanging the summoning bell…",
		"bell_ready":       "The summoning bell is hung. Listening for new spirits.",
		"bell_trouble":     "Connection lost. Reconnecting…",
		"bell_arrived":     "New spirit: %s, cause %s, restarts: %d.",
		"bell_worse":       "%s died again: restarts %d → %d (%s).",
		"bell_worse_cause": "%s died again: restarts %d → %d, cause %s → %s.",
		"bell_summon_now":  "Summon now",
		"bell_dismiss":     "Dismiss",
		"bell_aria":        "Summoning bell notices",
		"bell_sound_on":    "Bell sound: on",
		"bell_sound_off":   "Bell sound: off",
		"mass_title":       "Mass grave: %d Pods died of %s",
		"mass_pods":        "Buried together:",

		"mass_hint_oomkilled":        "OOMs together: check the memory limits first, then look for a memory regression in the latest release.",
		"mass_hint_crashloopbackoff": "Crash loops together: check the latest release, the shared ConfigMaps and Secrets, and the services they all depend on.",
		"mass_hint_imagepullbackoff": "Image pulls failing together: check that the registry is reachable, the pull credentials, and that the tag exists.",
		"mass_hint_probefailure":     "Probes failing together: check the probe settings and the latency of shared dependencies such as databases and downstream services.",
		"mass_hint_error":            "Error exits together: check the shared configuration and dependencies, and the latest changes.",
		"mass_hint_unknown":          "Cause unknown: check the node status and the Warning events in this namespace.",

		"died_label":      "Time of death (UTC): %s",
		"meter":           "Ritual power: %d / %d calls used",
		"meter_js":        "Ritual power: {used} / {total} calls used",
		"who_spirit":      "Spirit",
		"who_user":        "You",
		"q_how_died":      "How did you die?",
		"q_fix":           "How do I fix it?",
		"ask_placeholder": "Ask the spirit a question",
		"ask_button":      "Ask",
		"finish":          "End the séance and write the postmortem",
		"evidence":        "Evidence",
		"lang_mismatch":   "This séance is held in %s. Switching the language only changes the interface.",
		"no_answer":       "The spirit did not answer. Try again later.",
		"pm_timeout":      "The postmortem timed out. Try again later.",

		"board_title":   "Spirit board",
		"board_yes":     "YES",
		"board_no":      "NO",
		"board_bye":     "GOODBYE",
		"board_hint":    "The runes are this séance's evidence IDs. The planchette moves to each [E#] the spirit cites. When an answer ends it points to YES, or to NO when the fact check found a contradiction. After the postmortem it says GOODBYE.",
		"board_said_js": "The planchette points to {rune}",
		"list_tagline":  "Every dead Pod leaves last words: exit codes, events and logs. Summon one and let it tell you.",

		"doubt_title":    "The spirit may be lying",
		"doubt_note":     "These claims do not match the evidence. Anything not listed only means no contradiction was found, not that it is verified.",
		"doubt_citation": "Citation %s is not in the evidence list",
		"doubt_memory":   "Memory quantity %s is not in the evidence",
		"doubt_exitCode": "Exit code claim \"%s\" does not match the evidence",
		"doubt_restarts": "Restart count claim \"%s\" does not match the evidence",
		"doubt_time":     "Time %s matches neither the evidence nor the time of death",

		"pm_aria":        "Postmortem",
		"pm_copy":        "Copy",
		"pm_copied":      "Copied",
		"pm_copy_failed": "Copy failed",
		"pm_download":    "Download .md",
		"pm_tomb":        "On the ofrenda. It will not be forgotten",
		"pm_note":        "This postmortem comes from the rule template. The spirit's LLM was not called (reason: %s).",

		"lineage_title":        "Family and rebirths",
		"lineage_owner":        "Family: %s",
		"lineage_inferred":     " (inferred from ReplicaSet %s)",
		"lineage_v_instance":   "Verdict: an instance problem. A sibling of the same revision is alive, so the trouble is with this instance. Compare the nodes: the spirit ran on %s, the living siblings on %s.",
		"lineage_v_regression": "Verdict: the release broke it. Another revision has living Pods, and none of this revision is alive.",
		"lineage_v_family":     "Verdict: the whole family is dead. Check the dependencies and configuration they share.",
		"lineage_v_alone":      "Verdict: no siblings and no other revisions to compare with.",
		"lineage_revision":     "This revision %s=%s · alive %d/%d",
		"lineage_self":         "spirit",
		"lineage_siblings":     "Siblings",
		"lineage_no_siblings":  "No siblings.",
		"lineage_alive":        "alive",
		"lineage_dead":         "dead",
		"lineage_age_older":    "Former life (older revision)",
		"lineage_age_newer":    "Later life (newer revision)",
		"lineage_alive_count":  "alive %d/%d",
		"lineage_col_field":    "Field",
		"lineage_col_other":    "That revision",
		"lineage_col_this":     "This revision",
		"lineage_hidden":       "changed (value hidden)",
		"lineage_no_diff":      "No difference in the compared fields.",
		"lineage_none_val":     "(none)",
		"lineage_none":         "No family record: sample mode does not list Pods, or the Pod list could not be read.",
		"lineage_on_node":      "node %s",

		"spell_button":      "Speak the resurrection spell",
		"spell_aria":        "Resurrection spell",
		"spell_note":        "The spell is built only from the collected autopsy. No LLM was called and the cluster was not contacted. This tool runs no command: you read each step and run it yourself, and every command that writes to the cluster is rehearsed first with --dry-run=server.",
		"spell_diagnosis":   "Diagnosis",
		"spell_target":      "Target",
		"spell_ns":          "namespace",
		"spell_container":   "container",
		"spell_inferred":    "inferred",
		"spell_steps":       "Steps",
		"spell_step_check":  "Read-only · look",
		"spell_step_file":   "Patch file",
		"spell_step_cast":   "Cast · writes to the cluster",
		"spell_step_verify": "Read-only · confirm",
		"spell_patch":       "Patch",
		"spell_download":    "Download %s",
		"spell_caveats":     "Caveats",
		"spell_copy":        "Copy",
		"spell_copied":      "Copied",
		"spell_copy_failed": "Copy failed",
		"spell_no_patch":    "This spell has no patch file.",

		"gy_title":            "Ofrenda",
		"gy_intro":            "Every spirit with a postmortem is on this altar. The most recent deaths are on the top tier. Their evidence is kept in the postmortem.",
		"gy_empty":            "The ofrenda is empty. Summon a spirit and generate its postmortem to place its photo here.",
		"gy_off":              "The ofrenda is off (-graveyard is empty).",
		"gy_read_failed":      "Could not read the ofrenda. Check the file given by -graveyard.",
		"gy_read_error":       "Could not read the ofrenda",
		"born":                "Born: %s",
		"died":                "Died: %s",
		"tomb_back":           "← Back to the ofrenda",
		"tomb_title":          "%s on the ofrenda",
		"pm_heading":          "Postmortem",
		"altar_tier":          "Altar tier %d",
		"tomb_remembered":     "Its postmortem is on the ofrenda. Even after Kubernetes deletes its events and logs, it will not be forgotten.",
		"enshrined":           "🌼 Enshrined",
		"enshrined_title":     "The postmortem is on the ofrenda, so the evidence is kept",
		"return_mark":         "🕯️ Return #%d",
		"return_mark_title":   "It died again after its postmortem was placed on the ofrenda: the earlier fix may not have worked",
		"bell_return_self":    "%s returned: it died again after its postmortem was placed on the ofrenda. Return #%d (%s). The earlier fix may not have worked.",
		"bell_return_family":  "%s returned: another Pod of %s died after %s was placed on the ofrenda. Return #%d (%s). The earlier fix may not have worked.",
		"bell_open_photo":     "Open its photo",
		"altar_returns":       "Returned %d times",
		"tomb_returned":       "It has returned %d times: the earlier fix may not have worked.",
		"tomb_return_item":    "%s died at %s (%s)",
		"tomb_family":         "Family: %s",
		"offerings_heading":   "Offerings",
		"offerings_none":      "This tombstone has no offerings.",
		"offering_death":      "How it died",
		"offering_last_words": "Last words",
		"offering_change":     "The change between two lives",
		"offering_limit":      "Memory limit",
		"offering_image":      "The image that could not be pulled",
		"offering_warning":    "First Warning event",
		"offering_evidence":   "evidence %s at the time",
	},
}

// tr returns the catalog text of key in lang. An unknown language uses Chinese,
// and a missing key returns the key itself, so a gap shows up on the page.
func tr(lang, key string) string {
	if s, ok := catalog[uiLang(lang)][key]; ok {
		return s
	}
	return key
}

// trf formats the catalog text of key in lang with args, like fmt.Sprintf.
func trf(lang, key string, args ...any) string {
	return fmt.Sprintf(tr(lang, key), args...)
}

// templateFuncs binds T and Tf to one language for one template set.
func templateFuncs(lang string) template.FuncMap {
	return template.FuncMap{
		"T":  func(key string) string { return tr(lang, key) },
		"Tf": func(key string, args ...any) string { return trf(lang, key, args...) },
	}
}

// uiLang returns lang when it is an interface language, and Chinese otherwise.
func uiLang(lang string) string {
	if l := llm.NormalizeLang(lang); l != "" {
		return l
	}
	return llm.LangZH
}

// requestLang picks the interface language of r without changing anything.
func (s *Server) requestLang(r *http.Request) string {
	if l := llm.NormalizeLang(r.URL.Query().Get(langParam)); l != "" {
		return l
	}
	if c, err := r.Cookie(langCookie); err == nil {
		if l := llm.NormalizeLang(c.Value); l != "" {
			return l
		}
	}
	if s.opts.Lang != "" {
		return s.opts.Lang
	}
	if h := strings.TrimSpace(r.Header.Get("Accept-Language")); h != "" {
		return acceptLanguage(h)
	}
	return llm.LangZH
}

// pageLang is requestLang for a page. When the request carries a valid ?lang=,
// it also sets the seance_lang cookie, so later pages keep the choice.
func (s *Server) pageLang(w http.ResponseWriter, r *http.Request) string {
	if l := llm.NormalizeLang(r.URL.Query().Get(langParam)); l != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     langCookie,
			Value:    l,
			Path:     "/",
			MaxAge:   langCookieMaxAge,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		return l
	}
	return s.requestLang(r)
}

// acceptLanguage reads an Accept-Language header that is present. Tags are
// ranked by their q value, highest first, keeping the header order on ties. The
// first zh or en tag decides. When neither appears, the result is English,
// because a browser that asks for neither is more likely to read English.
// A tag with q=0 is refused and skipped.
func acceptLanguage(header string) string {
	type tag struct {
		lang string
		q    float64
	}
	var tags []tag
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		for _, p := range strings.Split(params, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && strings.TrimSpace(k) == "q" {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = f
				}
			}
		}
		if q <= 0 {
			continue
		}
		tags = append(tags, tag{lang: llm.NormalizeLang(name), q: q})
	}
	slices.SortStableFunc(tags, func(a, b tag) int {
		switch {
		case a.q > b.q:
			return -1
		case a.q < b.q:
			return 1
		}
		return 0
	})
	for _, t := range tags {
		if t.lang != "" {
			return t.lang
		}
	}
	return llm.LangEN
}

// chrome is the part of every page that the layout needs: the language and the
// switch link to the other language.
type chrome struct {
	Lang      string
	SwitchURL string
}

// chromeFor builds the layout data for a page in lang. The switch link keeps
// the path and the other query parameters and sets lang to the other language.
func chromeFor(r *http.Request, lang string) chrome {
	other := llm.LangEN
	if lang == llm.LangEN {
		other = llm.LangZH
	}
	q := r.URL.Query()
	q.Set(langParam, other)
	u := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
	return chrome{Lang: lang, SwitchURL: u.String()}
}
