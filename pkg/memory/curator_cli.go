package memory

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// CuratorArgs holds every flag and positional of the curator subcommands
type CuratorArgs struct {
	Skill        string
	Skills       []string
	DryRun       bool
	Background   bool
	Synchronous  bool
	Consolidate  bool
	AllUnmanaged bool
	Yes          bool
	List         bool
	Compact      bool
	JSON         bool
	Days         *int
	Reason       string
	EntryID      string
	BackupID     string
	Limit        int
	Sort         string
	Provenance   string
}

// CuratorCLI runs the curator subcommands against a console
type CuratorCLI struct {
	Out io.Writer
	Err io.Writer
	In  *bufio.Reader
	Now func() time.Time
}

// Curator subcommand runner over the given streams
func NewCuratorCLI(out, errw io.Writer, in io.Reader) *CuratorCLI {
	return &CuratorCLI{Out: out, Err: errw, In: bufio.NewReader(in), Now: time.Now}
}

func (c *CuratorCLI) println(a ...any) { fmt.Fprintln(c.Out, a...) }

func (c *CuratorCLI) printf(format string, a ...any) { fmt.Fprintf(c.Out, format, a...) }

// Dispatches one curator subcommand and returns its exit code
func (c *CuratorCLI) Run(ctx context.Context, sub string, a CuratorArgs) (int, error) {
	handlers := map[string]func(context.Context, CuratorArgs) (int, error){
		"status": c.cmdStatus, "usage": c.cmdUsage, "run": c.cmdRun, "pause": c.cmdPause, "resume": c.cmdResume,
		"pin": c.cmdPin, "unpin": c.cmdUnpin, "lock": c.cmdLock, "unlock": c.cmdUnlock, "list-unmanaged": c.cmdListUnmanaged,
		"adopt": c.cmdAdopt, "restore": c.cmdRestore, "list-archived": c.cmdListArchived, "archive": c.cmdArchive,
		"prune": c.cmdPrune, "backup": c.cmdBackup, "rollback": c.cmdRollback, "ledger": c.cmdLedger, "purge": c.cmdPurge,
	}
	h, ok := handlers[sub]
	if !ok {
		return 2, fmt.Errorf("unknown curator subcommand %q", sub)
	}
	return h(ctx, a)
}

// Aware UTC instant of an ISO timestamp, false when unparseable
func curatorCLIParseTS(ts any) (time.Time, bool) {
	s, ok := ts.(string)
	if !ok {
		return time.Time{}, false
	}
	t, aware, err := pydatetime.ParseISOFormat(s)
	if err != nil {
		return time.Time{}, false
	}
	if !aware {
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	}
	return t, true
}

// Relative age of a timestamp such as 5m ago, never for empty values
func (c *CuratorCLI) fmtTS(ts any) string {
	if !pyrepr.Truthy(ts) {
		return "never"
	}
	dt, ok := curatorCLIParseTS(ts)
	if !ok {
		return pyStr(ts)
	}
	secs := int(c.Now().Sub(dt).Seconds())
	for _, u := range []struct {
		unit       string
		div, limit int
	}{{"s", 1, 60}, {"m", 60, 3600}, {"h", 3600, 86400}} {
		if secs < u.limit {
			return fmt.Sprintf("%d%s ago", pyFloorDiv(secs, u.div), u.unit)
		}
	}
	return fmt.Sprintf("%dd ago", pyFloorDiv(secs, 86400))
}

func pyFloorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// Asks the prompt and reports whether the answer was y or yes, printing cancel otherwise
func (c *CuratorCLI) confirm(prompt, cancel, eofPrefix string) bool {
	fmt.Fprint(c.Out, prompt)
	line, err := c.In.ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprint(c.Out, eofPrefix)
	} else if a := strings.ToLower(pyStrip(line)); a == "y" || a == "yes" {
		return true
	}
	c.println(cancel)
	return false
}

func curatorCLIInt(v any) int {
	n, err := pyint.Of(v)
	if err != nil {
		return 0
	}
	return curatorBigToInt(n)
}

func (c *CuratorCLI) printSkillRows(title string, rows []*pyjson.Object) {
	c.printf("\n%s:\n", title)
	for _, r := range rows {
		c.printf("  %s  activity=%3d  use=%3d  view=%3d  patches=%3d  last_activity=%s\n", curatorPad(pyStr(curatorGetOr(r, "name", nil)), 40, false),
			curatorCLIInt(curatorGetOr(r, "activity_count", 0)), curatorCLIInt(curatorGetOr(r, "use_count", 0)), curatorCLIInt(curatorGetOr(r, "view_count", 0)),
			curatorCLIInt(curatorGetOr(r, "patch_count", 0)), c.fmtTS(curatorGetOr(r, "last_activity_at", nil)))
	}
}

// Counts of curation eligible skills that carry no provenance marker
func (c *CuratorCLI) printUnmanagedSummary(ctx context.Context) {
	scope, _, _ := curatorScope(ctx)
	unmanaged, err := scope.UnmanagedReport()
	if err != nil || len(unmanaged) == 0 {
		return
	}
	legacy := 0
	for _, r := range unmanaged {
		if !pyrepr.Truthy(curatorGetOr(r, "has_provenance_key", nil)) {
			legacy++
		}
	}
	c.printf("\nunmanaged (no provenance marker): %d total\n", len(unmanaged))
	c.printf("  pre-dates marker    %d\n", legacy)
	c.printf("  foreground-created  %d\n", len(unmanaged)-legacy)
	c.println("  never auto-staled or archived — `icebath curator adopt <name>` hands one over")
}

func (c *CuratorCLI) printCuratorConfig(ctx context.Context) error {
	st, err := LoadCuratorState(ctx)
	if err != nil {
		return err
	}
	status := "DISABLED"
	switch {
	case pyrepr.Truthy(curatorGetOr(st, "paused", false)):
		status = "PAUSED"
	case CuratorIsEnabled(ctx):
		status = "ENABLED"
	}
	c.println("curator: " + status)
	c.printf("  runs:           %s\n", pyStr(curatorGetOr(st, "run_count", 0)))
	c.printf("  last run:       %s\n", c.fmtTS(curatorGetOr(st, "last_run_at", nil)))
	summary := "(none)"
	if v := curatorGetOr(st, "last_run_summary", nil); pyrepr.Truthy(v) {
		summary = pyStr(v)
	}
	lines := []string{summary}
	if strings.Contains(summary, "\n") {
		lines = pystr.Splitlines(summary, false)
	}
	if len(lines) == 0 {
		return fmt.Errorf("not enough values to unpack (expected at least 1, got 0)")
	}
	c.printf("  last summary:   %s\n", lines[0])
	for _, l := range lines[1:] {
		c.printf("                  %s\n", l)
	}
	if report := curatorGetOr(st, "last_report_path", nil); pyrepr.Truthy(report) {
		missing := " (missing)"
		if curatorExists(pyStr(report)) {
			missing = ""
		}
		c.printf("  last report:    %s%s\n", pyStr(report), missing)
	}
	ih := CuratorIntervalHours(ctx)
	if ih%24 == 0 && ih >= 24 {
		c.printf("  interval:       every %dd\n", ih/24)
	} else {
		c.printf("  interval:       every %dh\n", ih)
	}
	c.printf("  stale after:    %dd unused\n", CuratorStaleAfterDays(ctx))
	c.printf("  archive after:  %dd unused\n", CuratorArchiveAfterDays(ctx))
	if CuratorConsolidate(ctx) {
		c.println("  consolidate:    on")
	} else {
		c.println("  consolidate:    off (prune-only; LLM merge pass opt-in)")
	}
	return nil
}

func curatorSortKeyStr(r *pyjson.Object, keys ...string) string {
	for _, k := range keys {
		if v := curatorGetOr(r, k, nil); pyrepr.Truthy(v) {
			return pyStr(v)
		}
	}
	return ""
}

func (c *CuratorCLI) cmdStatus(ctx context.Context, _ CuratorArgs) (int, error) {
	if err := c.printCuratorConfig(ctx); err != nil {
		return 1, err
	}
	scope, _, _ := curatorScope(ctx)
	locked, err := scope.LockedSkillNames()
	if err != nil {
		return 1, err
	}
	if len(locked) > 0 {
		c.printf("\nlocked (%d): %s  (the agent cannot change these; `icebath curator unlock <skill>` lifts it)\n", len(locked), strings.Join(locked, ", "))
	}
	rows, err := scope.CuratedReport()
	if err != nil {
		return 1, err
	}
	if len(rows) == 0 {
		c.println("\nno curator-managed skills")
		c.printUnmanagedSummary(ctx)
		return 0, nil
	}
	byState := map[string][]*pyjson.Object{}
	var pinned []string
	agent, bundled := 0, 0
	for _, r := range rows {
		state := pyStr(curatorGetOr(r, "state", "active"))
		byState[state] = append(byState[state], r)
		if pyrepr.Truthy(curatorGetOr(r, "pinned", nil)) {
			pinned = append(pinned, pyStr(curatorGetOr(r, "name", nil)))
		}
		switch curatorGetOr(r, "provenance", "agent") {
		case "agent":
			agent++
		case "bundled":
			bundled++
		}
	}
	c.printf("\ncurator-managed skills: %d total  (agent-created=%d  bundled=%d)\n", len(rows), agent, bundled)
	for _, s := range []string{"active", "stale", "archived"} {
		c.printf("  %s %d\n", curatorPad(s, 10, false), len(byState[s]))
	}
	if len(pinned) > 0 {
		c.printf("\npinned (%d): %s\n", len(pinned), strings.Join(pinned, ", "))
	}
	c.printUnmanagedSummary(ctx)
	active := byState["active"]
	if len(active) == 0 {
		return 0, nil
	}
	recency := append([]*pyjson.Object{}, active...)
	sort.SliceStable(recency, func(i, j int) bool {
		return curatorSortKeyStr(recency[i], "last_activity_at", "created_at") < curatorSortKeyStr(recency[j], "last_activity_at", "created_at")
	})
	c.printSkillRows("least recently active (top 5)", recency[:min(5, len(recency))])
	freqLess := func(a, b *pyjson.Object) bool {
		fa, fb := curatorCount(a, "activity_count"), curatorCount(b, "activity_count")
		if fa != fb {
			return fa < fb
		}
		return curatorSortKeyStr(a, "last_activity_at") < curatorSortKeyStr(b, "last_activity_at")
	}
	most := append([]*pyjson.Object{}, active...)
	sort.SliceStable(most, func(i, j int) bool { return freqLess(most[j], most[i]) })
	most = most[:min(5, len(most))]
	if curatorCount(most[0], "activity_count") > 0 {
		c.printSkillRows("most active (top 5)", most)
	}
	least := append([]*pyjson.Object{}, active...)
	sort.SliceStable(least, func(i, j int) bool { return freqLess(least[i], least[j]) })
	c.printSkillRows("least active (top 5)", least[:min(5, len(least))])
	return 0, nil
}

func (c *CuratorCLI) cmdRun(ctx context.Context, a CuratorArgs) (int, error) {
	if !CuratorIsEnabled(ctx) {
		c.println("curator: disabled via config; enable with `curator.enabled: true`")
		return 1, nil
	}
	synchronous := a.Synchronous || !a.Background
	var consolidate *bool
	if a.Consolidate {
		on := true
		consolidate = &on
	}
	if a.DryRun {
		c.println("curator: running DRY-RUN (report only, no mutations)...")
	} else {
		c.println("curator: running review pass...")
	}
	if consolidate == nil && !CuratorConsolidate(ctx) {
		c.println("curator: consolidation is off — running prune-only (deterministic stale/archive). Pass --consolidate or set `curator.consolidate: true` to enable the LLM merge pass.")
	}
	res, err := RunCuratorReview(ctx, CuratorRunOptions{OnSummary: func(s string) { c.println(s) }, Synchronous: synchronous, DryRun: a.DryRun, Consolidate: consolidate})
	if err != nil {
		return 1, err
	}
	auto, _ := curatorGetOr(res, "auto_transitions", nil).(*pyjson.Object)
	if auto != nil && auto.Len() > 0 && a.DryRun {
		c.printf("auto (preview): %d candidate skill(s) — no transitions applied in dry-run\n", curatorCount(auto, "checked"))
	} else if auto != nil && auto.Len() > 0 {
		c.printf("auto: checked=%d stale=%d archived=%d reactivated=%d\n", curatorCount(auto, "checked"), curatorCount(auto, "marked_stale"),
			curatorCount(auto, "archived"), curatorCount(auto, "reactivated"))
	}
	if !synchronous {
		c.println("llm pass running in background — check `icebath curator status` later")
	}
	if a.DryRun {
		if synchronous {
			c.println("dry-run: no changes applied. Read the report with `icebath curator status` and run `icebath curator run` (no flag) to apply.")
		} else {
			c.println("dry-run: no changes applied. When the report lands, read it with `icebath curator status` and run `icebath curator run` (no flag) to apply.")
		}
	}
	return 0, nil
}

func (c *CuratorCLI) setPaused(ctx context.Context, paused bool) (int, error) {
	if err := SetCuratorPaused(ctx, paused); err != nil {
		return 1, err
	}
	if paused {
		c.println("curator: paused")
	} else {
		c.println("curator: resumed")
	}
	return 0, nil
}

func (c *CuratorCLI) cmdPause(ctx context.Context, _ CuratorArgs) (int, error) {
	return c.setPaused(ctx, true)
}
func (c *CuratorCLI) cmdResume(ctx context.Context, _ CuratorArgs) (int, error) {
	return c.setPaused(ctx, false)
}

var curatorPinMessages = map[bool][4]string{
	true: {"cannot pin (only agent-created skills participate in curation)",
		"could not pin '{skill}' — the skill is not curation-eligible (protected built-in or external). `icebath curator list-unmanaged` shows which skills the curator tracks.",
		"pinned '{skill}' (recorded; this skill is unmanaged — auto-transitions never consider it. Run `icebath curator adopt {skill}` to put it under curator management)",
		"pinned '{skill}' (will bypass auto-transitions)"},
	false: {"there's nothing to unpin (curator only tracks agent-created skills)",
		"could not unpin '{skill}' — the skill is not curation-eligible (protected built-in or external).",
		"unpinned '{skill}' (recorded; this skill is unmanaged — it was never under auto-transitions to begin with)",
		"unpinned '{skill}'"},
}

func (c *CuratorCLI) setPin(ctx context.Context, a CuratorArgs, pinned bool) (int, error) {
	scope, _, _ := curatorScope(ctx)
	msgs := curatorPinMessages[pinned]
	agentCreated, err := scope.IsAgentCreated(a.Skill)
	if err != nil {
		return 1, err
	}
	if !agentCreated {
		c.printf("curator: '%s' is bundled or hub-installed — %s\n", a.Skill, msgs[0])
		return 1, nil
	}
	if !scope.SetPinned(ctx, a.Skill, pinned) {
		c.println("curator: " + strings.ReplaceAll(msgs[1], "{skill}", a.Skill))
		return 1, nil
	}
	managed, err := scope.IsCuratorManaged(a.Skill)
	if err != nil {
		return 1, err
	}
	if !managed {
		c.println("curator: " + strings.ReplaceAll(msgs[2], "{skill}", a.Skill))
		return 0, nil
	}
	c.println("curator: " + strings.ReplaceAll(msgs[3], "{skill}", a.Skill))
	return 0, nil
}

func (c *CuratorCLI) cmdPin(ctx context.Context, a CuratorArgs) (int, error) {
	return c.setPin(ctx, a, true)
}
func (c *CuratorCLI) cmdUnpin(ctx context.Context, a CuratorArgs) (int, error) {
	return c.setPin(ctx, a, false)
}

// Records a user content lock under the skill directory name, refusing names that do not resolve
func (c *CuratorCLI) setLock(ctx context.Context, a CuratorArgs, locked bool) (int, error) {
	scope, _, _ := curatorScope(ctx)
	verb := "unlock"
	if locked {
		verb = "lock"
	}
	dir, ok := scope.FindSkill(a.Skill)
	if !ok {
		c.printf("curator: no skill named '%s' in the active profile — nothing to %s. `icebath skills list` shows the names.\n", a.Skill, verb)
		return 1, nil
	}
	key := filepath.Base(dir)
	if !scope.SetLocked(ctx, key, locked) {
		c.printf("curator: could not record the %s for '%s' (skills/.usage.json not writable).\n", verb, key)
		return 1, nil
	}
	if locked {
		c.printf("curator: locked '%s'. The agent can no longer change it: skill_manage, write_file, patch and terminal refuse, in normal turns and the background passes alike, and the curator won't archive it. It stays readable and auto-loadable, and you can still edit it yourself. Unlock with `icebath curator unlock %s`.\n", key, key)
	} else {
		c.printf("curator: unlocked '%s'. The agent may edit it again.\n", key)
	}
	return 0, nil
}

func (c *CuratorCLI) cmdLock(ctx context.Context, a CuratorArgs) (int, error) {
	return c.setLock(ctx, a, true)
}
func (c *CuratorCLI) cmdUnlock(ctx context.Context, a CuratorArgs) (int, error) {
	return c.setLock(ctx, a, false)
}

func (c *CuratorCLI) cmdListUnmanaged(ctx context.Context, _ CuratorArgs) (int, error) {
	scope, _, _ := curatorScope(ctx)
	rows, err := scope.UnmanagedReport()
	if err != nil {
		return 1, err
	}
	if len(rows) == 0 {
		c.println("curator: no unmanaged skills — every eligible skill is managed")
		return 0, nil
	}
	c.printf("unmanaged skills (%d):\n", len(rows))
	sorted := append([]*pyjson.Object{}, rows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return pyStr(curatorGetOr(sorted[i], "name", nil)) < pyStr(curatorGetOr(sorted[j], "name", nil))
	})
	for _, r := range sorted {
		why := "no marker"
		if pyrepr.Truthy(curatorGetOr(r, "has_provenance_key", nil)) {
			by := curatorGetOr(r, "created_by", nil)
			if !pyrepr.Truthy(by) {
				by = "null"
			}
			why = "created_by:" + pyStr(by)
		}
		c.printf("  %s activity=%4d  last_activity=%s  (%s)\n", curatorPad(pyStr(curatorGetOr(r, "name", nil)), 44, false),
			curatorCLIInt(curatorGetOr(r, "activity_count", 0)), curatorPad(c.fmtTS(curatorGetOr(r, "last_activity_at", nil)), 14, false), why)
	}
	c.println("\nadopt one with `icebath curator adopt <name>`, or all with `icebath curator adopt --all-unmanaged`")
	return 0, nil
}

func (c *CuratorCLI) cmdAdopt(ctx context.Context, a CuratorArgs) (int, error) {
	scope, _, _ := curatorScope(ctx)
	names := append([]string{}, a.Skills...)
	if a.AllUnmanaged {
		if len(names) > 0 {
			c.println("curator: pass either skill names or --all-unmanaged, not both")
			return 1, nil
		}
		var err error
		if names, err = scope.ListUnmanagedSkillNames(); err != nil {
			return 1, err
		}
		if len(names) == 0 {
			c.println("curator: no unmanaged skills to adopt")
			return 0, nil
		}
	}
	if len(names) == 0 {
		c.println("curator: name a skill to adopt, or pass --all-unmanaged")
		return 1, nil
	}
	if a.DryRun {
		c.printf("curator: would adopt %d skill(s) (dry run):\n", len(names))
		for _, n := range names {
			c.printf("  + %s\n", n)
		}
		return 0, nil
	}
	if a.AllUnmanaged && !a.Yes {
		c.printf("curator: adopt %d unmanaged skill(s) into curator management?\n", len(names))
		c.println("  they become eligible for automatic staleness + archival")
		if !c.confirm("  proceed? [y/N] ", "curator: aborted", "") {
			return 1, nil
		}
	}
	failed := 0
	for _, n := range names {
		ok, msg, err := scope.AdoptSkill(ctx, n)
		if err != nil {
			return 1, err
		}
		c.println("curator: " + msg)
		if !ok {
			failed++
		}
	}
	if len(names) > 1 {
		c.printf("curator: adopted %d/%d\n", len(names)-failed, len(names))
	}
	if failed > 0 {
		return 1, nil
	}
	return 0, nil
}

// Runs a skill mutation with the ledger actor set to user and maps its result
func (c *CuratorCLI) asUser(ctx context.Context, fn func(context.Context, string) (bool, string, error), skill string) (int, error) {
	ok, msg, err := fn(skills.WithLedgerActor(ctx, "user"), skill)
	if err != nil {
		return 1, err
	}
	c.println("curator: " + msg)
	if ok {
		return 0, nil
	}
	return 1, nil
}

func (c *CuratorCLI) cmdRestore(ctx context.Context, a CuratorArgs) (int, error) {
	scope, _, _ := curatorScope(ctx)
	return c.asUser(ctx, scope.RestoreSkill, a.Skill)
}

func (c *CuratorCLI) cmdArchive(ctx context.Context, a CuratorArgs) (int, error) {
	scope, _, _ := curatorScope(ctx)
	rec, err := scope.GetRecord(a.Skill)
	if err != nil {
		return 1, err
	}
	if pyrepr.Truthy(curatorGetOr(rec, "pinned", nil)) {
		c.printf("curator: '%s' is pinned — unpin first with `icebath curator unpin %s`\n", a.Skill, a.Skill)
		return 1, nil
	}
	return c.asUser(ctx, scope.ArchiveSkill, a.Skill)
}

// Days since last activity or creation, false when neither parses
func (c *CuratorCLI) idleDays(r *pyjson.Object) (int, bool) {
	ts := curatorGetOr(r, "last_activity_at", nil)
	if !pyrepr.Truthy(ts) {
		ts = curatorGetOr(r, "created_at", nil)
	}
	if !pyrepr.Truthy(ts) {
		return 0, false
	}
	dt, ok := curatorCLIParseTS(pyStr(ts))
	if !ok {
		return 0, false
	}
	return max(0, pyFloorDiv(int(c.Now().Sub(dt)/time.Second), 86400)), true
}

func (c *CuratorCLI) cmdPrune(ctx context.Context, a CuratorArgs) (int, error) {
	scope, _, _ := curatorScope(ctx)
	days := CuratorArchiveAfterDays(ctx)
	if a.Days != nil {
		days = *a.Days
	}
	if days < 1 {
		fmt.Fprintf(c.Err, "curator: --days must be >= 1 (got %d)\n", days)
		return 2, nil
	}
	rows, err := scope.CuratedReport()
	if err != nil {
		return 1, err
	}
	type cand struct {
		name string
		idle int
	}
	var cands []cand
	for _, r := range rows {
		if pyrepr.Truthy(curatorGetOr(r, "pinned", nil)) || curatorGetOr(r, "state", nil) == skills.StateArchived {
			continue
		}
		if idle, ok := c.idleDays(r); ok && idle >= days {
			cands = append(cands, cand{pyStr(curatorGetOr(r, "name", nil)), idle})
		}
	}
	if len(cands) == 0 {
		c.printf("curator: nothing to prune (no unpinned skills idle >= %dd)\n", days)
		return 0, nil
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].idle > cands[j].idle })
	c.printf("curator: %d skill(s) idle >= %dd:\n", len(cands), days)
	for _, cd := range cands {
		c.printf("  %s idle %dd\n", curatorPad(cd.name, 40, false), cd.idle)
	}
	if a.DryRun {
		c.println("\n(dry run — no changes made)")
		return 0, nil
	}
	if !a.Yes && !c.confirm(fmt.Sprintf("\nArchive %d skill(s)? [y/N] ", len(cands)), "curator: aborted", "\n") {
		return 1, nil
	}
	var failures [][2]string
	for _, cd := range cands {
		ok, msg, err := scope.ArchiveSkill(ctx, cd.name)
		if err != nil {
			return 1, err
		}
		if !ok {
			failures = append(failures, [2]string{cd.name, msg})
		}
	}
	c.printf("\ncurator: archived %d/%d\n", len(cands)-len(failures), len(cands))
	if len(failures) > 0 {
		c.println("failures:")
		for _, f := range failures {
			c.printf("  %s: %s\n", f[0], f[1])
		}
		return 1, nil
	}
	return 0, nil
}

func (c *CuratorCLI) cmdBackup(ctx context.Context, a CuratorArgs) (int, error) {
	if !CuratorBackupEnabled(ctx) {
		c.println("curator: backups are disabled via config (`curator.backup.enabled: false`); re-enable to snapshot")
		return 1, nil
	}
	reason := a.Reason
	if reason == "" {
		reason = "manual"
	}
	snap, ok, err := SnapshotSkills(ctx, reason, nil)
	if err != nil {
		return 1, err
	}
	if !ok {
		c.println("curator: snapshot failed — check logs (backup disabled or IO error)")
		return 1, nil
	}
	c.printf("curator: snapshot created at ~/.icebath/skills/.curator_backups/%s\n", filepath.Base(snap))
	return 0, nil
}

func (c *CuratorCLI) cmdLedger(ctx context.Context, a CuratorArgs) (int, error) {
	scope, _, _ := curatorScope(ctx)
	if a.Compact {
		entries, before, after, err := scope.LedgerCompact(ctx)
		if err != nil {
			return 1, err
		}
		blobs, freed, err := scope.LedgerGCBlobs(ctx)
		if err != nil {
			return 1, err
		}
		const mb = 1 << 20
		c.printf("curator: ledger compacted — %d entries, %.1f MB → %.1f MB; %d unreferenced blob(s) removed (%.1f MB)\n", entries,
			float64(before)/mb, float64(after)/mb, blobs, float64(freed)/mb)
		return 0, nil
	}
	limit := a.Limit
	if limit == 0 {
		limit = 20
	}
	rows, err := scope.LedgerListEntries(a.Skill, limit)
	if err != nil {
		return 1, err
	}
	if len(rows) == 0 {
		c.println("curator: ledger is empty (or skills.ledger is disabled).")
		return 0, nil
	}
	c.printf("%s %s %s %s skill\n", curatorPad("id", 14, false), curatorPad("when", 12, false), curatorPad("actor", 8, false), curatorPad("action", 12, false))
	for _, r := range rows {
		evidence := curatorGetOr(r, "evidence", nil)
		extra := ""
		if v := curatorGetOr(evidence, "absorbed_into", nil); pyrepr.Truthy(v) {
			extra = fmt.Sprintf("  → absorbed into '%s'", pyStr(v))
		} else if v := curatorGetOr(evidence, "rollback_target", nil); pyrepr.Truthy(v) {
			extra = "  → rollback of " + pyStr(v)
		}
		c.printf("%s %s %s %s %s%s\n", curatorPad(pyStr(curatorGetOr(r, "id", "?")), 14, false), curatorPad(c.fmtTS(curatorGetOr(r, "ts", nil)), 12, false),
			curatorPad(pyStr(curatorGetOr(r, "actor", "?")), 8, false), curatorPad(pyStr(curatorGetOr(r, "action", "?")), 12, false),
			pyStr(curatorGetOr(r, "skill", "?")), extra)
	}
	c.println("\nRoll back a single mutation with `icebath curator rollback <id>`; whole-tree snapshots remain available via `icebath curator rollback --list`.")
	return 0, nil
}

// Archive timestamp of an archived skill dir, the newer of its record archived_at and the dir mtime
func curatorArchivedTS(usage *pyjson.Object, p string) (float64, error) {
	st, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	mtime := float64(st.ModTime().UnixNano()) / 1e9
	rec := curatorGetOr(usage, skills.ReadSkillName(filepath.Join(p, "SKILL.md"), filepath.Base(p)), nil)
	if curatorGetOr(rec, "state", nil) != skills.StateArchived {
		return mtime, nil
	}
	raw := curatorGetOr(rec, "archived_at", nil)
	if !pyrepr.Truthy(raw) {
		return mtime, nil
	}
	at, ok := curatorCLIParseTS(pyStr(raw))
	if !ok {
		return mtime, nil
	}
	return max(float64(at.UnixNano())/1e9, mtime), nil
}

func (c *CuratorCLI) cmdPurge(ctx context.Context, a CuratorArgs) (int, error) {
	scope, _, h := curatorScope(ctx)
	ttl := 0
	if a.Days != nil {
		ttl = *a.Days
	} else {
		cfg, err := config.Load(h, configLookup(&tools.Env{Home: h}))
		if err != nil {
			return 1, err
		}
		v := curatorGetOr(curatorSubdict(map[string]any(cfg), "curator"), "archive_ttl_days", 0)
		if ttl, err = curatorIntOrZero(v); err != nil {
			return 1, err
		}
	}
	if ttl <= 0 {
		c.println("curator: purge disabled (curator.archive_ttl_days is 0). Set the config key or pass --days N to purge archives older than N days.")
		return 1, nil
	}
	root := filepath.Join(scope.SkillsDir(), ".archive")
	if !curatorExists(root) {
		c.println("curator: no archive directory — nothing to purge.")
		return 0, nil
	}
	cutoff := float64(c.Now().UnixNano())/1e9 - float64(ttl)*86400
	usage, err := scope.LoadUsage()
	if err != nil {
		return 1, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 1, err
	}
	var cands []string
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			continue
		}
		ts, err := curatorArchivedTS(usage, p)
		if err != nil {
			return 1, err
		}
		if ts < cutoff {
			cands = append(cands, p)
		}
	}
	sort.Strings(cands)
	if len(cands) == 0 {
		c.printf("curator: no archived skills older than %dd.\n", ttl)
		return 0, nil
	}
	c.printf("Archived skills older than %dd:\n", ttl)
	for _, p := range cands {
		c.printf("  %s\n", filepath.Base(p))
	}
	if a.DryRun {
		c.println("(dry run — nothing deleted)")
		return 0, nil
	}
	if !a.Yes && !c.confirm(fmt.Sprintf("Permanently delete %d archived skill(s)? [y/N] ", len(cands)), "cancelled", "\n") {
		return 1, nil
	}
	purged := 0
	for _, p := range cands {
		name := filepath.Base(p)
		var before []*pyjson.Object
		for _, item := range scope.LedgerCaptureBefore(ctx, p, true, name) {
			before = append(before, curatorEntry("path", item["path"], "sha256", item["sha256"]))
		}
		if err := os.RemoveAll(p); err != nil {
			c.printf("curator: failed to purge %s: %v\n", name, err)
			continue
		}
		scope.LedgerAppendEntry(ctx, "purge", name, before, nil, "user", curatorEntry("ttl_days", ttl))
		purged++
	}
	c.printf("curator: purged %d archived skill(s). Ledger entries recorded.\n", purged)
	return 0, nil
}

func (c *CuratorCLI) reportRollback(ok bool, msg string) int {
	if ok {
		c.println("curator: " + msg)
		return 0
	}
	c.println("curator: rollback failed — " + msg)
	return 1
}

// Restores exactly the files one ledger entry touched after showing it and confirming
func (c *CuratorCLI) rollbackLedgerEntry(ctx context.Context, a CuratorArgs) (int, error) {
	scope, _, _ := curatorScope(ctx)
	entry, err := scope.LedgerGetEntry(a.EntryID)
	if err != nil {
		return 1, err
	}
	if entry == nil {
		c.printf("curator: no ledger entry '%s'. See `icebath curator ledger` for entry ids, or use `--id <snapshot>` for whole-tree snapshot rollback.\n", a.EntryID)
		return 1, nil
	}
	c.printf("Rollback target: ledger entry %s\n", a.EntryID)
	for _, kv := range [][2]string{{"action", "  action: "}, {"skill", "  skill:  "}, {"actor", "  actor:  "}, {"ts", "  when:   "}} {
		c.printf("%s%s\n", kv[1], pyStr(curatorGetOr(entry, kv[0], "?")))
	}
	touched := map[string]bool{}
	for _, key := range []string{"before", "after"} {
		list, _ := curatorGetOr(entry, key, nil).([]any)
		for _, item := range list {
			touched[pyrepr.Repr(curatorGetOr(item, "path", nil))] = true
		}
	}
	c.printf("  files:  %d\n", len(touched))
	if !a.Yes && !c.confirm("Restore this mutation's before-state? [y/N] ", "cancelled", "\n") {
		return 1, nil
	}
	ok, msg, err := scope.LedgerRollbackEntry(ctx, a.EntryID)
	if err != nil {
		return 1, err
	}
	return c.reportRollback(ok, msg), nil
}

func (c *CuratorCLI) cmdRollback(ctx context.Context, a CuratorArgs) (int, error) {
	if a.EntryID != "" {
		return c.rollbackLedgerEntry(ctx, a)
	}
	if a.List {
		s, err := SummarizeCuratorBackups(ctx)
		if err != nil {
			return 1, err
		}
		c.println(s)
		return 0, nil
	}
	target, ok := curatorResolveBackup(ctx, a.BackupID)
	if !ok {
		backups, err := ListCuratorBackups(ctx)
		if err != nil {
			return 1, err
		}
		if len(backups) == 0 {
			c.println("curator: no snapshots exist yet. Take one with `icebath curator backup` or wait for the next curator run.")
		} else {
			query := "your query"
			if a.BackupID != "" {
				query = "id " + pyrepr.Repr(a.BackupID)
			}
			c.printf("curator: no snapshot matching %s.\n", query)
			c.println("Available:")
			s, err := SummarizeCuratorBackups(ctx)
			if err != nil {
				return 1, err
			}
			c.println(s)
		}
		return 1, nil
	}
	manifest, err := curatorReadManifest(target)
	if err != nil {
		return 1, err
	}
	c.printf("Rollback target: %s\n", filepath.Base(target))
	if pyrepr.Truthy(manifest) {
		c.printf("  reason:      %s\n", pyStr(curatorGetOr(manifest, "reason", "?")))
		c.printf("  created_at:  %s\n", pyStr(curatorGetOr(manifest, "created_at", "?")))
		c.printf("  skill files: %s\n", pyStr(curatorGetOr(manifest, "skill_files", "?")))
		cron := curatorGetOr(manifest, "cron_jobs", nil)
		if !pyrepr.Truthy(cron) {
			cron = pyjson.NewObject()
		}
		if curatorIsDict(cron) {
			if pyrepr.Truthy(curatorGetOr(cron, "backed_up", nil)) {
				c.printf("  cron jobs:   %s (will be restored for skill-link fields only)\n", pyStr(curatorGetOr(cron, "jobs_count", 0)))
			} else {
				c.printf("  cron jobs:   not in snapshot (%s)\n", pyStr(curatorGetOr(cron, "reason", "not captured")))
			}
		}
	}
	c.println("\nThis will replace the current ~/.icebath/skills/ tree (a safety snapshot of the current state is taken first so this is undoable). Cron jobs that still exist will have their skills/skill fields restored from the snapshot; all other cron fields are left alone.")
	if !a.Yes && !c.confirm("Proceed? [y/N] ", "cancelled", "\n") {
		return 1, nil
	}
	ok, msg, _ := RollbackCuratorSnapshot(ctx, filepath.Base(target))
	return c.reportRollback(ok, msg), nil
}

func (c *CuratorCLI) cmdListArchived(ctx context.Context, _ CuratorArgs) (int, error) {
	scope, _, _ := curatorScope(ctx)
	names, err := scope.ListArchivedSkillNames()
	if err != nil {
		return 1, err
	}
	if len(names) == 0 {
		c.println("curator: no archived skills")
	} else {
		c.println(strings.Join(names, "\n"))
	}
	return 0, nil
}

// Usage telemetry of every skill on disk with provenance, as a table or JSON
func (c *CuratorCLI) cmdUsage(ctx context.Context, a CuratorArgs) (int, error) {
	scope, _, _ := curatorScope(ctx)
	all, err := scope.UsageReport()
	if err != nil {
		return 1, err
	}
	rows := []*pyjson.Object{}
	for _, r := range all {
		if a.Provenance == "" || curatorGetOr(r, "provenance", nil) == a.Provenance {
			rows = append(rows, r)
		}
	}
	switch a.Sort {
	case "name":
		sort.SliceStable(rows, func(i, j int) bool {
			return pyStr(curatorGetOr(rows[i], "name", nil)) < pyStr(curatorGetOr(rows[j], "name", nil))
		})
	case "recent":
		sort.SliceStable(rows, func(i, j int) bool {
			return curatorSortKeyStr(rows[j], "last_activity_at") < curatorSortKeyStr(rows[i], "last_activity_at")
		})
	default:
		sort.SliceStable(rows, func(i, j int) bool {
			return curatorCount(rows[j], "activity_count") < curatorCount(rows[i], "activity_count")
		})
	}
	if a.JSON {
		s, err := pyjson.Dumps(curatorObjList(rows), pyjson.Indent(2), pyjson.EnsureASCII(false))
		if err != nil {
			return 1, err
		}
		c.println(s)
		return 0, nil
	}
	if len(rows) == 0 {
		c.println("curator: no skills found")
		return 0, nil
	}
	counts := map[string]int{}
	for _, r := range rows {
		counts[pyStr(curatorGetOr(r, "provenance", "agent"))]++
	}
	c.printf("skills: %d total  (agent=%d  bundled=%d  hub=%d  external=%d)\n\n", len(rows), counts["agent"], counts["bundled"], counts["hub"], counts["external"])
	c.printf("  %s  %s  %s  %s  %s  %s  last_activity\n", curatorPad("skill", 40, false), curatorPad("origin", 8, false), curatorPad("use", 4, true),
		curatorPad("view", 4, true), curatorPad("patch", 5, true), curatorPad("act", 4, true))
	for _, r := range rows {
		c.printf("  %s  %s  %4d  %4d  %5d  %4d  %s\n", curatorPad(curatorRunes(pyStr(curatorGetOr(r, "name", nil)), 40), 40, false),
			curatorPad(pyStr(curatorGetOr(r, "provenance", "agent")), 8, false), curatorCLIInt(curatorGetOr(r, "use_count", 0)),
			curatorCLIInt(curatorGetOr(r, "view_count", 0)), curatorCLIInt(curatorGetOr(r, "patch_count", 0)),
			curatorCLIInt(curatorGetOr(r, "activity_count", 0)), c.fmtTS(curatorGetOr(r, "last_activity_at", nil)))
	}
	return 0, nil
}

func registerCuratorCommands() {}
