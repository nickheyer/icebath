package kanban

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	prRepoRE = regexp.MustCompile(`^(?:[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)$`)
	prURLRE  = regexp.MustCompile(`^(?:https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/pull/([1-9][0-9]*))$`)
	shaRE    = regexp.MustCompile(`^(?:[0-9a-f]{40})$`)
	deniedRE = regexp.MustCompile(`HTTP (40[134])`)
)

// Completion contract of a task, local-only by default, else an OWNER/REPO slug or an exact GitHub PR URL
func validateContract(value *string) (string, error) {
	if value == nil || *value == "local-only" {
		return "local-only", nil
	}
	if !prRepoRE.MatchString(*value) && !prURLRE.MatchString(*value) {
		return "", &ValueError{"completion_contract must be local-only, OWNER/REPO, or an exact GitHub PR URL"}
	}
	return *value, nil
}

// GitHub refused the acceptance read, an identity problem to fix rather than an infrastructure blip
type gateAuthError struct{ Msg string }

func (e *gateAuthError) Error() string { return e.Msg }

// Python exception raised while reading gh evidence, its class deciding whether the gate reports infra
type gatePyError struct{ Class, Msg string }

func (e *gatePyError) Error() string { return e.Class + ": " + e.Msg }

// Raises a Python exception class inside the acceptance reader
func gateRaise(class, format string, args ...any) {
	panic(&gatePyError{class, fmt.Sprintf(format, args...)})
}

// Subscript of a decoded JSON value with the KeyError, IndexError and TypeError dict and list subscripts raise
func jget(v any, key any) any {
	switch c := v.(type) {
	case *pyjson.Object:
		k, ok := key.(string)
		if !ok {
			gateRaise("KeyError", "%s", pyrepr.Repr(key))
		}
		val, found := c.Get(k)
		if !found {
			gateRaise("KeyError", "%s", pyrepr.Repr(key))
		}
		return val
	case []any:
		i, ok := key.(int)
		if !ok {
			gateRaise("TypeError", "list indices must be integers or slices, not %s", pyrepr.TypeName(key))
		}
		if i < 0 {
			i += len(c)
		}
		if i < 0 || i >= len(c) {
			gateRaise("IndexError", "list index out of range")
		}
		return c[i]
	}
	gateRaise("TypeError", "'%s' object is not subscriptable", pyrepr.TypeName(v))
	return nil
}

// dict.get of a decoded JSON value, AttributeError for a non dict
func jgetd(v any, key string, def any) any {
	o, ok := v.(*pyjson.Object)
	if !ok {
		gateRaise("AttributeError", "'%s' object has no attribute 'get'", pyrepr.TypeName(v))
	}
	if val, found := o.Get(key); found {
		return val
	}
	return def
}

// Iteration items of a decoded JSON value, keys for a dict, TypeError for a scalar
func jiter(v any) []any {
	switch c := v.(type) {
	case []any:
		return c
	case *pyjson.Object:
		out := make([]any, len(c.Keys))
		for i, k := range c.Keys {
			out[i] = k
		}
		return out
	case string:
		var out []any
		for _, r := range c {
			out = append(out, string(r))
		}
		return out
	}
	gateRaise("TypeError", "'%s' object is not iterable", pyrepr.TypeName(v))
	return nil
}

// String of a value re and quote accept, TypeError for any other type
func jstr(v any) string {
	s, ok := v.(string)
	if !ok {
		gateRaise("TypeError", "expected string or bytes-like object, got '%s'", pyrepr.TypeName(v))
	}
	return s
}

// The value or the fallback when the value is falsy
func jor(v, def any) any {
	if truthy(v) {
		return v
	}
	return def
}

// Numeric value of a bool, int or float for Python cross type comparison
func jnum(v any) (*big.Float, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return big.NewFloat(1), true
		}
		return big.NewFloat(0), true
	case int:
		return new(big.Float).SetInt64(int64(n)), true
	case int64:
		return new(big.Float).SetInt64(n), true
	case *big.Int:
		return new(big.Float).SetInt(n), true
	case float64:
		if math.IsNaN(n) {
			return nil, false
		}
		return big.NewFloat(n), true
	}
	return nil, false
}

// Python equality of decoded JSON values
func jeq(a, b any) bool {
	if an, ok := jnum(a); ok {
		bn, ok := jnum(b)
		return ok && an.Cmp(bn) == 0
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jeq(x[i], y[i]) {
				return false
			}
		}
		return true
	case *pyjson.Object:
		y, ok := b.(*pyjson.Object)
		if !ok || len(x.Keys) != len(y.Keys) {
			return false
		}
		for _, k := range x.Keys {
			yv, found := y.Get(k)
			if !found || !jeq(x.Vals[k], yv) {
				return false
			}
		}
		return true
	}
	return false
}

// Python less than of two ids, TypeError for values without an ordering
func jless(a, b any) bool {
	if an, ok := jnum(a); ok {
		if bn, ok := jnum(b); ok {
			return an.Cmp(bn) < 0
		}
	}
	if x, ok := a.(string); ok {
		if y, ok := b.(string); ok {
			return x < y
		}
	}
	gateRaise("TypeError", "'<' not supported between instances of '%s' and '%s'", pyrepr.TypeName(b), pyrepr.TypeName(a))
	return false
}

// Hash key of a value, TypeError for the unhashable list and dict
func jhash(v any) string {
	switch v.(type) {
	case []any, *pyjson.Object:
		gateRaise("TypeError", "unhashable type: '%s'", pyrepr.TypeName(v))
	}
	if n, ok := jnum(v); ok {
		return "n:" + n.Text('g', 64)
	}
	return pyrepr.Repr(v)
}

// One gh api call under the card identity, decoded, refusing an incomplete GraphQL answer
func ghAPI(ctx context.Context, endpoint string, query *string, paginate bool, profileHome *string) any {
	args := []string{"api", endpoint, "--hostname", "github.com"}
	if query != nil {
		args = append(args, "-f", "query="+*query)
	}
	if paginate {
		args = append(args, "--paginate", "--slurp")
	}
	env, err := ghEnv(ctx, profileHome)
	if err != nil {
		gateRaise("OSError", "%v", err)
	}
	argv := append(tools.LocateCommand("gh"), args...)
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	if env != nil {
		cmd.Env = make([]string, 0, len(env))
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if runCtx.Err() != nil {
		gateRaise("TimeoutExpired", "Command %s timed out after 30 seconds", pyrepr.Repr(argv))
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		short := endpoint
		if i := bytes.IndexByte([]byte(endpoint), '?'); i >= 0 {
			short = endpoint[:i]
		}
		if m := deniedRE.FindStringSubmatch(pystr.DecodeUTF8Replace(stderr.Bytes())); m != nil {
			panic(&gateAuthError{fmt.Sprintf("HTTP %s on %s", m[1], short)})
		}
		if exitErr.ExitCode() == 4 {
			panic(&gateAuthError{"gh has no login for " + short})
		}
		gateRaise("CalledProcessError", "Command %s returned non-zero exit status %d.", pyrepr.Repr(argv), exitErr.ExitCode())
	}
	if err != nil {
		gateRaise("OSError", "%v", err)
	}
	value, err := pyjson.Loads(pystr.DecodeUTF8Replace(stdout.Bytes()))
	if err != nil {
		gateRaise("ValueError", "%v", err)
	}
	if o, ok := value.(*pyjson.Object); ok {
		if e, _ := o.Get("errors"); truthy(e) {
			gateRaise("ValueError", "GitHub returned incomplete GraphQL evidence")
		}
	}
	return value
}

// Child env for gh carrying the card profile identity, nil keeping the ambient env for unassigned cards
func ghEnv(ctx context.Context, profileHome *string) (map[string]string, error) {
	if profileHome == nil || *profileHome == "" {
		return nil, nil
	}
	tenv := tools.ToolEnvFrom(ctx)
	if tenv == nil {
		tenv = &tools.Env{Home: home.Dir(ctx)}
	}
	base := tools.IcebathSubprocessEnv(tenv, true, nil)
	routed := tools.IsRoutedHome(*profileHome)
	if routed {
		delete(base, "GH_CONFIG_DIR")
	}
	env, err := tools.ServedProfileChildEnv(ctx, tenv, base, *profileHome, true)
	if err != nil {
		return nil, err
	}
	if routed {
		_, a := env["GH_TOKEN"]
		_, b := env["GITHUB_TOKEN"]
		_, c := env["GH_CONFIG_DIR"]
		if !a && !b && !c {
			env["GH_CONFIG_DIR"] = filepath.Join(*profileHome, "gh")
		}
	}
	return env, nil
}

// Home whose gh login must read the contract repo, nil for an unassigned card, an auth refusal when unresolvable
func assigneeProfileHome(assignee *string) *string {
	if assignee == nil || *assignee == "" {
		return nil
	}
	name, err := profiles.NormalizeProfileName(*assignee)
	if err == nil {
		var h string
		if h, err = profiles.ResolveProfileEnv(name); err == nil {
			return &h
		}
	}
	panic(&gateAuthError{fmt.Sprintf("assignee profile %s cannot be resolved", pyrepr.StrRepr(*assignee))})
}

// Required check of a branch, the context with its app id
type requiredCheck struct{ Context, AppID any }

// Python str of the check tuple, the sort key of the required set
func (r requiredCheck) sortKey() string {
	return "(" + pyrepr.Repr(r.Context) + ", " + pyrepr.Repr(r.AppID) + ")"
}

// Acceptance receipt of a PR completion contract from the required checks on its current head
func collectAcceptance(ctx context.Context, contract string, publishedPR any, assignee *string) (receipt *pyjson.Object, err error) {
	receipt = payload("ok", false, "classification", "missing", "head_sha", nil, "pr_url", publishedPR, "checks", []any{},
		"recovery", "Fix required failures, rerun infrastructure checks or wait, then retry completion. "+
			"Use kanban_block if human input is needed; receipts remain on the task event log.")
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		switch e := r.(type) {
		case *gateAuthError:
			login := "the ambient gh login"
			if assignee != nil && *assignee != "" {
				login = fmt.Sprintf("assignee profile %s's gh login", pyrepr.StrRepr(*assignee))
			}
			receipt.Set("classification", "auth")
			receipt.Set("detail", fmt.Sprintf("GitHub refused the acceptance read (%s) as %s; fix that profile's GitHub credentials/access to the repository, then retry completion.", e.Msg, login))
		case *gatePyError:
			switch e.Class {
			case "OSError", "TimeoutExpired", "CalledProcessError", "ValueError", "KeyError", "TypeError", "IndexError":
				receipt.Set("classification", "infra")
				receipt.Set("detail", "GitHub acceptance evidence unavailable or incomplete; check gh authentication/API access and retry.")
			default:
				receipt, err = nil, &RuntimeError{e.Error()}
			}
		default:
			panic(r)
		}
	}()
	profileHome := assigneeProfileHome(assignee)
	declared := prURLRE.MatchString(contract)
	var url any = publishedPR
	if declared {
		url = contract
	}
	urlText := ""
	if truthy(url) {
		urlText = jstr(url)
	}
	match := prURLRE.FindStringSubmatch(urlText)
	if match == nil || (!declared && match[1] != contract) || (declared && truthy(publishedPR) && !jeq(publishedPR, contract)) {
		receipt.Set("detail", "Supply metadata.published_pr matching the persisted completion contract.")
		return receipt, nil
	}
	repo, number := match[1], match[2]
	receipt.Set("pr_url", url)
	owner, name, _ := cutString(repo, "/")
	ownerJSON, _ := pyjson.Dumps(owner)
	nameJSON, _ := pyjson.Dumps(name)
	query := "{repository(owner:" + ownerJSON + ",name:" + nameJSON + "){pullRequest(number:" + number + "){headRefOid baseRefName state\n" +
		"            baseRef{branchProtectionRule{requiredStatusChecks{context app{databaseId}}}}}}}"
	repository := jget(jget(ghAPI(ctx, "graphql", &query, false, profileHome), "data"), "repository")
	if repository == nil {
		panic(&gateAuthError{"HTTP 404 on graphql " + repo})
	}
	pr := jget(repository, "pullRequest")
	sha, branch := jget(pr, "headRefOid"), jget(pr, "baseRefName")
	receipt.Set("head_sha", sha)
	state := jget(pr, "state")
	if !shaRE.MatchString(jstr(sha)) || !(jeq(state, "OPEN") || jeq(state, "MERGED")) {
		gateRaise("ValueError", "PR is closed or current head is unavailable")
	}
	protection := jor(jgetd(jor(jgetd(pr, "baseRef", nil), pyjson.NewObject()), "branchProtectionRule", nil), pyjson.NewObject())
	required := map[string]requiredCheck{}
	add := func(c, a any) {
		key := "(" + jhash(c) + ", " + jhash(a) + ")"
		if _, ok := required[key]; !ok {
			required[key] = requiredCheck{c, a}
		}
	}
	for _, r := range jiter(jgetd(protection, "requiredStatusChecks", []any{})) {
		add(jget(r, "context"), jgetd(jor(jgetd(r, "app", nil), pyjson.NewObject()), "databaseId", nil))
	}
	rules := ghAPI(ctx, fmt.Sprintf("repos/%s/rules/branches/%s?per_page=100", repo, pyurl.Quote(jstr(branch), "")), nil, true, profileHome)
	for _, page := range jiter(rules) {
		for _, rule := range jiter(page) {
			if jeq(jget(rule, "type"), "required_status_checks") {
				for _, r := range jiter(jget(jget(rule, "parameters"), "required_status_checks")) {
					add(jget(r, "context"), jgetd(r, "integration_id", nil))
				}
			}
		}
	}
	sorted := make([]requiredCheck, 0, len(required))
	for _, r := range required {
		sorted = append(sorted, r)
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].sortKey() < sorted[j].sortKey() })
	reqList := make([]any, len(sorted))
	for i, r := range sorted {
		reqList[i] = payload("context", r.Context, "app_id", r.AppID)
	}
	receipt.Set("required", reqList)
	if len(sorted) == 0 {
		receipt.Set("detail", "No repository-required checks are configured; explicitly use a local-only contract for non-CI tasks.")
		return receipt, nil
	}
	shaText := jstr(sha)
	pages := ghAPI(ctx, fmt.Sprintf("repos/%s/commits/%s/check-runs?per_page=100&filter=latest", repo, shaText), nil, true, profileHome)
	var runs []any
	for _, page := range jiter(pages) {
		runs = append(runs, jiter(jget(page, "check_runs"))...)
	}
	ids := map[string]bool{}
	for _, r := range runs {
		ids[jhash(jget(r, "id"))] = true
	}
	if !jeq(int64(len(ids)), jget(jget(pages, 0), "total_count")) {
		gateRaise("ValueError", "Incomplete check-run pagination")
	}
	var statuses []*pyjson.Object
	for _, page := range jiter(ghAPI(ctx, fmt.Sprintf("repos/%s/commits/%s/statuses?per_page=100", repo, shaText), nil, true, profileHome)) {
		for _, s := range jiter(page) {
			src, ok := s.(*pyjson.Object)
			if !ok {
				gateRaise("TypeError", "'%s' object is not a mapping", pyrepr.TypeName(s))
			}
			cp := pyjson.NewObject()
			for _, k := range src.Keys {
				cp.Set(k, src.Vals[k])
			}
			cp.Set("sha", sha)
			statuses = append(statuses, cp)
		}
	}
	var outcomes []string
	checks := []any{}
	for _, req := range sorted {
		anyApp := req.AppID == nil || jeq(req.AppID, int64(-1))
		var selected []any
		for _, r := range runs {
			if jeq(jget(r, "name"), req.Context) && (anyApp || jeq(jget(jget(r, "app"), "id"), req.AppID)) {
				selected = append(selected, r)
			}
		}
		if anyApp {
			var best any
			for _, s := range statuses {
				if jeq(jget(s, "context"), req.Context) && (best == nil || jless(jget(best, "id"), jget(s, "id"))) {
					best = s
				}
			}
			if best != nil {
				selected = append(selected, best)
			}
		}
		if len(selected) == 0 {
			outcomes = append(outcomes, "missing")
			checks = append(checks, payload("name", req.Context, "classification", "missing", "head_sha", sha))
		}
		for _, check := range selected {
			_, isRun := check.(*pyjson.Object).Get("conclusion")
			var outcome any
			if isRun {
				outcome = jgetd(check, "conclusion", nil)
			} else {
				outcome = jget(check, "state")
			}
			classification := classifyCheck(check, sha, outcome, isRun)
			outcomes = append(outcomes, classification)
			checks = append(checks, payload("name", req.Context, "id", jget(check, "id"),
				"url", jor(jgetd(check, "html_url", nil), jgetd(check, "target_url", nil)),
				"head_sha", jgetd(check, "head_sha", jgetd(check, "sha", nil)),
				"classification", classification, "conclusion", outcome))
		}
	}
	receipt.Set("checks", checks)
	current := ghAPI(ctx, fmt.Sprintf("repos/%s/pulls/%s", repo, number), nil, false, profileHome)
	if !jeq(jget(jget(current, "head"), "sha"), sha) || !jeq(jget(jget(current, "base"), "ref"), branch) ||
		(jeq(jget(current, "state"), "closed") && !truthy(jgetd(current, "merged", nil))) {
		receipt.Set("classification", "stale")
		receipt.Set("detail", "PR head/base changed while collecting evidence; retry.")
		return receipt, nil
	}
	final := "success"
	if len(outcomes) == 0 {
		final = "missing"
	}
	for _, x := range outcomes {
		if x != "success" {
			final = x
			break
		}
	}
	receipt.Set("classification", final)
	receipt.Set("ok", final == "success")
	return receipt, nil
}

// Splits a string at the first separator
func cutString(s, sep string) (string, string, bool) {
	i := bytes.Index([]byte(s), []byte(sep))
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// Classification of one check against the PR head, stale for another head and pending for an unfinished run
func classifyCheck(check, sha, outcome any, isRun bool) string {
	if !jeq(jgetd(check, "head_sha", jgetd(check, "sha", nil)), sha) {
		return "stale"
	}
	if isRun && !jeq(jgetd(check, "status", nil), "completed") {
		return "pending"
	}
	jhash(outcome)
	if s, ok := outcome.(string); ok {
		switch s {
		case "success", "failure", "pending":
			return s
		case "error":
			return "infra"
		}
	}
	return "infra"
}

// Ownership snapshot of a task the acceptance write must still match, nil when the task is gone
type acceptanceSnapshot struct{ RunID, Status, Contract any }

// Snapshot paired with the receipt collected for it
type PreparedAcceptance struct {
	Snapshot *acceptanceSnapshot
	Receipt  *pyjson.Object
}

// Current run, status and completion contract of a task, nil when no row matches
func acceptanceSnapshotOf(ctx context.Context, conn state.SQLiteConn, taskID string) (*acceptanceSnapshot, error) {
	row, err := QueryOne(ctx, conn, "SELECT current_run_id, status, completion_contract FROM tasks WHERE id=?", taskID)
	if err != nil || row == nil {
		return nil, err
	}
	return &acceptanceSnapshot{row.Get("current_run_id"), row.Get("status"), row.Get("completion_contract")}, nil
}

// Reports whether two snapshots hold equal values
func (s *acceptanceSnapshot) equal(o *acceptanceSnapshot) bool {
	if s == nil || o == nil {
		return s == nil && o == nil
	}
	return jeq(s.RunID, o.RunID) && jeq(s.Status, o.Status) && jeq(s.Contract, o.Contract)
}

// Collects PR acceptance for a completing task, nil for a local contract and refused when ownership moved
func prepareAcceptance(ctx context.Context, conn state.SQLiteConn, taskID string, expectedRunID *int64, metadata any) (prepared *PreparedAcceptance, refused bool, err error) {
	snapshot, err := acceptanceSnapshotOf(ctx, conn, taskID)
	if err != nil || snapshot == nil {
		return nil, err == nil, err
	}
	contract := snapshot.Contract
	if !truthy(contract) || contract == "local-only" {
		return nil, false, nil
	}
	st := snapshot.Status
	if !(jeq(st, "running") || jeq(st, "ready") || jeq(st, "blocked") || jeq(st, "review")) || (expectedRunID != nil && !jeq(snapshot.RunID, *expectedRunID)) {
		return nil, true, nil
	}
	var published any
	if isDict(metadata) {
		published, _ = dictGet(metadata, "published_pr")
	}
	if s, ok := published.(string); ok {
		if m := prURLRE.FindStringSubmatch(s); m != nil && jeq(contract, m[1]) {
			moved := false
			err := WriteTxn(ctx, conn, false, func() error {
				now, err := acceptanceSnapshotOf(ctx, conn, taskID)
				if err != nil {
					return err
				}
				if !now.equal(snapshot) {
					moved = true
					return nil
				}
				_, err = conn.ExecContext(ctx, "UPDATE tasks SET completion_contract=? WHERE id=?", s, taskID)
				return err
			})
			if err != nil || moved {
				return nil, moved, err
			}
			snapshot = &acceptanceSnapshot{snapshot.RunID, snapshot.Status, s}
			contract = s
		}
	}
	row, err := QueryOne(ctx, conn, "SELECT assignee FROM tasks WHERE id=?", taskID)
	if err != nil {
		return nil, false, err
	}
	if row == nil {
		return nil, false, &RuntimeError{"'NoneType' object is not subscriptable"}
	}
	receipt, err := collectAcceptance(ctx, cellStr(contract), published, cellOptStr(row.Get("assignee")))
	if err != nil {
		return nil, false, err
	}
	return &PreparedAcceptance{snapshot, receipt}, false, nil
}

// Records the acceptance receipt under the completion transaction and reports whether it passed
func recordAcceptance(ctx context.Context, conn state.SQLiteConn, taskID string, acceptance *PreparedAcceptance) (bool, error) {
	now, err := acceptanceSnapshotOf(ctx, conn, taskID)
	if err != nil || !now.equal(acceptance.Snapshot) {
		return false, err
	}
	runID, err := optInt(acceptance.Snapshot.RunID)
	if err != nil {
		return false, err
	}
	receipt := acceptance.Receipt
	if err := appendEvent(ctx, conn, taskID, "pr_acceptance", receipt, runID); err != nil {
		return false, err
	}
	ok := truthy(objGet(receipt, "ok"))
	if !ok {
		detail := ""
		if d, found := receipt.Get("detail"); found {
			detail = pyrepr.Str(d)
		}
		text := fmt.Sprintf("PR acceptance %s: %s %s", pyrepr.Str(objGet(receipt, "classification")), detail, pyrepr.Str(objGet(receipt, "recovery")))
		if _, err := conn.ExecContext(ctx, "UPDATE tasks SET last_failure_error=? WHERE id=?", text, taskID); err != nil {
			return false, err
		}
	}
	return ok, nil
}
