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
		"nav_graveyard":    "墓地",
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
		"pm_tomb":        "已立碑，去墓地看看",
		"pm_note":        "这份复盘由模板生成，没有调用亡灵的 LLM（原因：%s）。",

		"gy_title":       "墓地",
		"gy_intro":       "每个生成过复盘的亡灵都在这里立了碑，最近死去的排在最前。",
		"gy_empty":       "墓地还是空的。召唤一个亡灵并生成复盘，它就会在这里立碑。",
		"gy_off":         "墓地未开启（-graveyard 为空）",
		"gy_read_failed": "读取墓地失败，请检查墓地文件。",
		"gy_read_error":  "读取墓地失败",
		"born":           "生：%s",
		"died":           "卒：%s",
		"tomb_back":      "← 回到墓地",
		"tomb_title":     "%s 之墓",
		"pm_heading":     "复盘",
	},
	llm.LangEN: {
		"html_lang":        "en",
		"app_name":         "K8s Séance",
		"nav_cluster":      "Cluster spirits",
		"nav_fixtures":     "Sample mode",
		"nav_graveyard":    "Graveyard",
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
		"pm_tomb":        "Buried. Visit the graveyard",
		"pm_note":        "This postmortem comes from the rule template. The spirit's LLM was not called (reason: %s).",

		"gy_title":       "Graveyard",
		"gy_intro":       "Every spirit with a postmortem has a tombstone here. The most recent death comes first.",
		"gy_empty":       "The graveyard is empty. Summon a spirit and generate its postmortem to raise a tombstone here.",
		"gy_off":         "The graveyard is off (-graveyard is empty).",
		"gy_read_failed": "Could not read the graveyard. Check the graveyard file.",
		"gy_read_error":  "Could not read the graveyard",
		"born":           "Born: %s",
		"died":           "Died: %s",
		"tomb_back":      "← Back to the graveyard",
		"tomb_title":     "Tomb of %s",
		"pm_heading":     "Postmortem",
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
