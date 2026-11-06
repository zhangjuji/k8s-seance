package resurrect

import (
	"fmt"
	"strings"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// maxRollbackDiffs is how many template differences the diagnosis names.
const maxRollbackDiffs = 3

// rollback is the plan when the family verdict says the release broke it and an
// older revision is still alive (Requirement 16.5): roll back first, with a
// server-side dry run, and say that a rollback only stops the bleeding.
func (c *caster) rollback(v *autopsy.Version) {
	l := c.a.Lineage
	t := &c.sp.Target
	// The family names the controller exactly by its revision label.
	t.Kind, t.Name = l.OwnerKind, l.OwnerName
	t.Inferred = l.Inferred
	if l.Inferred {
		t.Note = fmt.Sprintf(c.t(
			"推断：Pod 属于 ReplicaSet %s，去掉 %s 后得到 Deployment %s。施法前请先用第一步确认它存在。",
			"Inferred: the Pod belongs to ReplicaSet %s; removing the %s gives Deployment %s. Confirm it exists with the first step before you cast."),
			l.ViaReplicaSet, l.HashLabel, l.OwnerName)
	} else {
		t.Note = ""
	}
	c.sp.Remedy = RemedyRollback
	c.sp.Incantation = c.t(
		"你的前世还在人间行走。先退回前世的模样活下来，再去找这一世错在哪里。",
		"Your former life still walks among the living. Return to its shape and live, then find what went wrong in this life.")

	famCite := c.citeText("lineage", "family "+l.Owner())
	verCite := c.citeText("lineage", "revision "+l.HashLabel+"="+v.Revision+" ")
	c.sp.Diagnosis = append(c.sp.Diagnosis,
		fmt.Sprintf(c.t(
			"家族判断：这次发布改坏了%s。本版本 %s=%s 没有一个活着，更早的版本 %s 还有 %d/%d 个活着%s。",
			"Family verdict: this release broke it%s. No Pod of this revision %s=%s is alive, while the older revision %s still has %d/%d alive%s."),
			famCite, l.HashLabel, l.Revision, v.Revision, v.Alive(), len(v.Pods), verCite))
	if len(v.Diffs) > 0 {
		n := min(len(v.Diffs), maxRollbackDiffs)
		parts := make([]string, 0, n)
		for _, d := range v.Diffs[:n] {
			parts = append(parts, autopsy.DiffText(d))
		}
		more := ""
		if len(v.Diffs) > n {
			more = fmt.Sprintf(c.t("（另有 %d 项）", " (and %d more)"), len(v.Diffs)-n)
		}
		c.sp.Diagnosis = append(c.sp.Diagnosis, fmt.Sprintf(c.t(
			"两版 Pod 模板的差异：%s%s%s。死因多半就在这里。",
			"The Pod templates differ in: %s%s%s. The cause of death is most likely there."),
			strings.Join(parts, "; "), more, verCite))
	} else {
		c.sp.Diagnosis = append(c.sp.Diagnosis, c.t(
			"比较过的字段里两版没有差异，变化可能在镜像内容、标签或注解里。",
			"The compared fields do not differ, so the change may be inside the image, or in labels or annotations."))
	}

	check := Step{Kind: StepCheck, Title: c.t(
		"查看发布历史，确认上一版就是还活着的那一版",
		"Look at the rollout history and confirm that the previous revision is the one still alive"),
		Commands: []string{"kubectl rollout history " + t.ref() + " " + c.nsFlag()}}
	if t.Kind == "Deployment" {
		check.Commands = append(check.Commands,
			"kubectl get replicasets "+c.nsFlag()+" -l "+shq(l.HashLabel+"="+v.Revision)+" -o wide")
	} else {
		check.Commands = append(check.Commands, "kubectl get controllerrevisions "+c.nsFlag())
	}
	c.sp.Steps = append(c.sp.Steps, check,
		writeStep(c.t("施法：回滚到上一版（止血）", "Cast: roll back to the previous revision (stop the bleeding)"),
			"kubectl rollout undo "+t.ref()+" "+c.nsFlag()),
		c.verifyStep())

	c.sp.Caveats = append(c.sp.Caveats,
		c.t("回滚只是止血。坏的改动还在你的清单或流水线里，下一次发布会把它带回来：先修好上面的差异，再重新发布。",
			"A rollback only stops the bleeding. The bad change is still in your manifest or pipeline, and the next release brings it back: fix the differences above, then release again."),
		c.t("rollout undo 回到上一个 revision。如果中间还有别的发布，先看第一步的历史，必要时加 --to-revision=N。",
			"rollout undo goes back to the previous revision. If other releases came in between, check the history from the first step and add --to-revision=N if needed."),
		c.t("回滚后这一版的 Pod（包括这个亡灵）会被删除，它的事件和日志也会消失：先生成复盘。",
			"After the rollback the Pods of this revision, this spirit included, are deleted, and their events and logs go with them: write the postmortem first."))
	c.inferredCaveat()
}
