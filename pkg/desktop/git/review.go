package git

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/desktop/files"
	"github.com/nickheyer/icebath/pkg/desktop/hardening"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

const (
	commitContextDiffMaxChars     = 120_000
	commitContextUntrackedMax     = 80
	reviewFileCap                 = 2_000
	untrackedLineCountConcurrency = 16
	untrackedLineCountMaxBytes    = 1024 * 1024
	gitDiffTimeout                = 30 * time.Second
)

var rootCollator = struct {
	sync.Mutex
	c *collate.Collator
}{c: collate.New(language.Und)}

// Ordering of String.prototype.localeCompare under the root locale
func localeCompare(a, b string) int {
	rootCollator.Lock()
	defer rootCollator.Unlock()
	return rootCollator.c.CompareString(a, b)
}

var (
	renameBraceRe  = jsRe(`^(.*)\{(.*) => (.*)\}(.*)$`)
	doubleSlashRe  = regexp.MustCompile(`/{2,}`)
	originPrefixRe = regexp.MustCompile(`^origin/`)
	trunkRefRe     = regexp.MustCompile(`^refs/(?:heads|remotes/origin)/`)
)

// New path of a rename the diff summary reports as old => new or dir/{old => new}/f
func resolveRenamePath(raw string) string {
	path := jsTrim(raw)
	if !strings.Contains(path, " => ") {
		return path
	}
	if m := renameBraceRe.FindStringSubmatch(path); m != nil {
		return doubleSlashRe.ReplaceAllString(m[1]+m[3]+m[4], "/")
	}
	parts := strings.Split(path, " => ")
	return jsTrim(parts[len(parts)-1])
}

type lineCounts struct{ added, removed int }

// Line counts of a diff summary by resolved path, binary files counting zero
func countsByPath(summary DiffResult) map[string]lineCounts {
	out := map[string]lineCounts{}
	for _, f := range summary.Files {
		c := lineCounts{}
		if !f.Binary {
			c = lineCounts{f.Insertions, f.Deletions}
		}
		out[resolveRenamePath(f.File)] = c
	}
	return out
}

// Lines of an untracked file as an all add diff counts them, zero for binary, large or unreadable files
func untrackedInsertions(cwd, relPath string) int {
	full := hostPath.join(cwd, relPath)
	st, err := os.Stat(full)
	if err != nil || !st.Mode().IsRegular() || st.Size() > untrackedLineCountMaxBytes {
		return 0
	}
	buf, err := os.ReadFile(full)
	if err != nil || bytes.IndexByte(buf, 0) >= 0 {
		return 0
	}
	lines := bytes.Count(buf, []byte{'\n'})
	if len(buf) > 0 && buf[len(buf)-1] != '\n' {
		lines++
	}
	return lines
}

// Text cut to maxChars UTF-16 units with a note of how many were omitted
func capText(value string, maxChars int, label string) string {
	units := utf16.Encode([]rune(value))
	if len(units) <= maxChars {
		return value
	}
	return string(utf16.Decode(units[:maxChars])) + "\n# " + label + ": " + strconv.Itoa(len(units)-maxChars) + " chars omitted\n"
}

// Counts lines of each untracked review row in batches of sixteen
func fillUntrackedCounts(cwd string, rows []ReviewFile) {
	var pending []int
	for i, f := range rows {
		if f.Status == "?" && f.Added == 0 && f.Removed == 0 {
			pending = append(pending, i)
		}
	}
	for start := 0; start < len(pending); start += untrackedLineCountConcurrency {
		var wg sync.WaitGroup
		for _, idx := range pending[start:min(start+untrackedLineCountConcurrency, len(pending))] {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				rows[i].Added = untrackedInsertions(cwd, rows[i].Path)
			}(idx)
		}
		wg.Wait()
	}
}

// Merge base for all branch changes, against origin/HEAD first and then the common trunk names
func branchBase(g *simpleGit) *string {
	var candidates []string
	if head, err := g.revparse("--abbrev-ref", "origin/HEAD"); err == nil {
		if h := jsTrim(head); h != "" {
			candidates = append(candidates, h)
		}
	}
	candidates = append(candidates, "origin/main", "origin/master", "main", "master")
	for _, ref := range candidates {
		if out, err := g.raw("merge-base", "HEAD", ref); err == nil {
			if base := jsTrim(out); base != "" {
				return &base
			}
		}
	}
	return nil
}

// Default branch name from the remote HEAD, then local and remote trunk refs, nil when none answers
func defaultBranchName(g *simpleGit) *string {
	if head, err := g.revparse("--abbrev-ref", "origin/HEAD"); err == nil {
		if h := jsTrim(head); h != "" && h != "origin/HEAD" {
			name := originPrefixRe.ReplaceAllString(h, "")
			return &name
		}
	}
	for _, ref := range []string{"refs/heads/main", "refs/heads/master", "refs/remotes/origin/main", "refs/remotes/origin/master"} {
		if _, err := g.raw("rev-parse", "--verify", "--quiet", ref); err == nil {
			name := trunkRefRe.ReplaceAllString(ref, "")
			return &name
		}
	}
	return nil
}

// Single letter of a status row, the staged code ahead of the worktree code and untracked first
func statusLetter(f FileStatus) string {
	if f.Index == "?" || f.WorkingDir == "?" {
		return "?"
	}
	code := f.WorkingDir
	if f.Index != "" && f.Index != " " {
		code = f.Index
	}
	if code == "" {
		code = "M"
	}
	return jsUpper(code)
}

func isStaged(f FileStatus) bool { return f.Index != "" && f.Index != " " && f.Index != "?" }

// One changed path of the review pane
type ReviewFile struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Status  string `json:"status"`
	Staged  bool   `json:"staged"`
}

// Review pane file list with the base it was diffed against
type ReviewList struct {
	Files []ReviewFile `json:"files"`
	Base  any          `json:"base"`
}

func sortReviewFiles(rows []ReviewFile) {
	sort.SliceStable(rows, func(i, j int) bool { return localeCompare(rows[i].Path, rows[j].Path) < 0 })
}

// Changed files of a scope, uncommitted by default, branch against its merge base, lastTurn against a baseline
func reviewList(repoPath, scope, baseRef any, gitBin string) (ReviewList, error) {
	empty := ReviewList{Files: []ReviewFile{}}
	cwd, err := hardening.ResolveRequestedPathForIpc(repoPath, "Review list", "")
	if err != nil {
		return empty, nil
	}
	g, err := gitFor(cwd, gitBin)
	if err != nil {
		return empty, err
	}
	scopeName, _ := scope.(string)
	if scopeName == "branch" || scopeName == "lastTurn" {
		var base any = baseRef
		rangeArg := ""
		if scopeName == "branch" {
			base = nil
			if b := branchBase(g); b != nil {
				base = *b
			}
		}
		if !jsTruthy(base) {
			return empty, nil
		}
		rangeArg = jsString(base)
		if scopeName == "branch" {
			rangeArg += "...HEAD"
		}
		summary, err := g.diffSummary(rangeArg)
		if err != nil {
			return empty, nil
		}
		rows := []ReviewFile{}
		for _, f := range summary.Files[:min(len(summary.Files), reviewFileCap)] {
			row := ReviewFile{Path: resolveRenamePath(f.File), Status: "M"}
			if !f.Binary {
				row.Added, row.Removed = f.Insertions, f.Deletions
			}
			rows = append(rows, row)
		}
		if scopeName == "lastTurn" && len(rows) < reviewFileCap {
			status, err := g.status("--untracked-files=normal")
			if err != nil {
				return empty, nil
			}
			known := map[string]bool{}
			for _, r := range rows {
				known[r.Path] = true
			}
			for _, p := range status.NotAdded {
				if len(rows) >= reviewFileCap {
					break
				}
				if !known[p] {
					rows = append(rows, ReviewFile{Path: p, Status: "?"})
					known[p] = true
				}
			}
		}
		sortReviewFiles(rows)
		fillUntrackedCounts(cwd, rows)
		return ReviewList{Files: rows, Base: base}, nil
	}

	var (
		wg               sync.WaitGroup
		status           StatusResult
		staged, unstaged DiffResult
		errs             [3]error
	)
	wg.Add(3)
	go func() { defer wg.Done(); status, errs[0] = g.status("--untracked-files=normal") }()
	go func() { defer wg.Done(); staged, errs[1] = g.diffSummary("--cached") }()
	go func() { defer wg.Done(); unstaged, errs[2] = g.diffSummary() }()
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || errs[2] != nil {
		return empty, nil
	}
	stagedCounts, unstagedCounts := countsByPath(staged), countsByPath(unstaged)
	rows := []ReviewFile{}
	for _, f := range status.Files[:min(len(status.Files), reviewFileCap)] {
		p := resolveRenamePath(f.Path)
		sc, uc := stagedCounts[p], unstagedCounts[p]
		rows = append(rows, ReviewFile{Path: p, Added: sc.added + uc.added, Removed: sc.removed + uc.removed, Status: statusLetter(f), Staged: isStaged(f)})
	}
	sortReviewFiles(rows)
	fillUntrackedCounts(cwd, rows)
	return ReviewList{Files: rows, Base: nil}, nil
}

// All add diff of an untracked file through diff --no-index, empty when git cannot run
func untrackedDiff(gitBin, cwd, filePath string) string {
	bin := gitBin
	if bin == "" {
		bin = "git"
	}
	res, err := files.ExecGit(bin, []string{"diff", "--no-index", "--", "/dev/null", filePath}, cwd, nil, gitDiffTimeout)
	if err != nil {
		return ""
	}
	return res.Stdout
}

// Unified diff of one file for a scope, an all add diff for an untracked file
func reviewDiff(repoPath, filePath, scope, baseRef, staged any, gitBin string) (string, error) {
	cwd, err := hardening.ResolveRequestedPathForIpc(repoPath, "Review diff", "")
	if err != nil {
		return "", nil
	}
	g, err := gitFor(cwd, gitBin)
	if err != nil {
		return "", err
	}
	fp := jsString(filePath)
	safe := func(args ...string) string {
		out, err := g.diff(args...)
		if err != nil {
			return ""
		}
		return out
	}
	scopeName, _ := scope.(string)
	switch {
	case scopeName == "branch":
		if base := branchBase(g); base != nil {
			return safe(*base+"...HEAD", "--", fp), nil
		}
		return "", nil
	case scopeName == "lastTurn":
		if jsTruthy(baseRef) {
			return safe(jsString(baseRef), "--", fp), nil
		}
		return "", nil
	case jsTruthy(staged):
		return safe("--cached", "--", fp), nil
	}
	if worktree := safe("--", fp); jsTrim(worktree) != "" {
		return worktree, nil
	}
	return untrackedDiff(gitBin, cwd, fp), nil
}

// Working tree against HEAD diff of one file, an all add diff only for a file git does not track
func fileDiffVsHead(repoPath, filePath any, gitBin string) (string, error) {
	cwd, err := hardening.ResolveRequestedPathForIpc(repoPath, "File diff", "")
	if err != nil {
		return "", nil
	}
	g, err := gitFor(cwd, gitBin)
	if err != nil {
		return "", err
	}
	fp := jsString(filePath)
	if head, err := g.diff("HEAD", "--", fp); err == nil && jsTrim(head) != "" {
		return head, nil
	}
	status, err := g.raw("status", "--porcelain", "--", fp)
	if err != nil || !strings.HasPrefix(jsTrim(status), "??") {
		return "", nil
	}
	return untrackedDiff(gitBin, cwd, fp), nil
}

// Answer of a review mutation
type OkResult struct {
	Ok bool `json:"ok"`
}

// Git runner for a mutation, rejecting a refused path or a missing directory
func mutationGit(repoPath any, purpose, gitBin string) (*simpleGit, string, error) {
	cwd, err := hardening.ResolveRequestedPathForIpc(repoPath, purpose, "")
	if err != nil {
		return nil, "", err
	}
	g, err := gitFor(cwd, gitBin)
	return g, cwd, err
}

func reviewStage(repoPath, filePath any, gitBin string) (OkResult, error) {
	g, _, err := mutationGit(repoPath, "Review stage", gitBin)
	if err != nil {
		return OkResult{}, err
	}
	args := []string{"add", "-A"}
	if jsTruthy(filePath) {
		args = []string{"add", "--", jsString(filePath)}
	}
	if _, err := g.raw(args...); err != nil {
		return OkResult{}, err
	}
	return OkResult{Ok: true}, nil
}

func reviewUnstage(repoPath, filePath any, gitBin string) (OkResult, error) {
	g, _, err := mutationGit(repoPath, "Review unstage", gitBin)
	if err != nil {
		return OkResult{}, err
	}
	args := []string{"reset", "-q", "HEAD"}
	if jsTruthy(filePath) {
		args = []string{"reset", "-q", "HEAD", "--", jsString(filePath)}
	}
	if _, err := g.raw(args...); err != nil {
		return OkResult{}, err
	}
	return OkResult{Ok: true}, nil
}

// Restores tracked files to HEAD and removes untracked ones, for one path or the whole tree
func reviewRevert(repoPath, filePath any, gitBin string) (OkResult, error) {
	g, _, err := mutationGit(repoPath, "Review revert", gitBin)
	if err != nil {
		return OkResult{}, err
	}
	if jsTruthy(filePath) {
		fp := jsString(filePath)
		g.raw("checkout", "HEAD", "--", fp)
		g.raw("clean", "-fd", "--", fp)
	} else {
		g.raw("checkout", "HEAD", "--", ".")
		g.raw("clean", "-fd")
	}
	return OkResult{Ok: true}, nil
}

// Commit sha of a ref, HEAD by default, nil when it does not resolve
func reviewRevParse(repoPath, ref any, gitBin string) *string {
	cwd, err := hardening.ResolveRequestedPathForIpc(repoPath, "Review rev-parse", "")
	if err != nil {
		return nil
	}
	g, err := gitFor(cwd, gitBin)
	if err != nil {
		return nil
	}
	target := "HEAD"
	if jsTruthy(ref) {
		target = jsString(ref)
	}
	out, err := g.revparse(target)
	if err != nil || out == "" {
		return nil
	}
	return &out
}

// Pushes the current branch, setting origin upstream on its first push
func pushCurrent(g *simpleGit) error {
	status, err := g.status()
	if err != nil {
		return err
	}
	if status.Tracking != nil && *status.Tracking != "" {
		return g.push()
	}
	if status.Current != nil && *status.Current != "" {
		_, err = g.raw("push", "-u", "origin", *status.Current)
	}
	return err
}

// Commits staged changes, everything when nothing is staged, then pushes when asked
func reviewCommit(repoPath, message, push any, gitBin string) (OkResult, error) {
	g, _, err := mutationGit(repoPath, "Review commit", gitBin)
	if err != nil {
		return OkResult{}, err
	}
	status, err := g.status()
	if err != nil {
		return OkResult{}, err
	}
	if len(status.Staged) == 0 {
		if _, err := g.raw("add", "-A"); err != nil {
			return OkResult{}, err
		}
	}
	if err := g.commit(message); err != nil {
		return OkResult{}, err
	}
	if jsTruthy(push) {
		if err := pushCurrent(g); err != nil {
			return OkResult{}, err
		}
	}
	return OkResult{Ok: true}, nil
}

// What a commit message is drafted from, the diff to land and recent subjects
type CommitContext struct {
	Diff   string `json:"diff"`
	Recent string `json:"recent"`
}

// Capped diff of what the commit will hold, untracked names listed, and the last ten subjects
func reviewCommitContext(repoPath any, gitBin string) (CommitContext, error) {
	cwd, err := hardening.ResolveRequestedPathForIpc(repoPath, "Review commit context", "")
	if err != nil {
		return CommitContext{}, nil
	}
	g, err := gitFor(cwd, gitBin)
	if err != nil {
		return CommitContext{}, err
	}
	status, err := g.status()
	if err != nil {
		return CommitContext{}, nil
	}
	diffArg := "HEAD"
	if len(status.Staged) > 0 {
		diffArg = "--cached"
	}
	raw, err := g.diff(diffArg)
	if err != nil {
		raw = ""
	}
	diff := capText(raw, commitContextDiffMaxChars, "diff truncated for commit-message generation")
	if untracked := status.NotAdded; len(untracked) > 0 {
		visible := untracked[:min(len(untracked), commitContextUntrackedMax)]
		lines := make([]string, len(visible))
		for i, p := range visible {
			lines[i] = "#   " + p
		}
		note := "\n# New (untracked) files:\n" + strings.Join(lines, "\n") + "\n"
		if omitted := len(untracked) - len(visible); omitted > 0 {
			note += "#   ... " + strconv.Itoa(omitted) + " more omitted\n"
		}
		diff += note
	}
	recent, err := g.raw("log", "-n", "10", "--pretty=format:%s")
	if err != nil {
		recent = ""
	}
	return CommitContext{Diff: diff, Recent: jsTrim(recent)}, nil
}

func reviewPush(repoPath any, gitBin string) (OkResult, error) {
	g, _, err := mutationGit(repoPath, "Review push", gitBin)
	if err != nil {
		return OkResult{}, err
	}
	if err := pushCurrent(g); err != nil {
		return OkResult{}, err
	}
	return OkResult{Ok: true}, nil
}

// Gh readiness and the pull request of the current branch
type ShipInfo struct {
	GhReady bool `json:"ghReady"`
	Pr      any  `json:"pr"`
}

// Whether gh is installed and signed in, and the pull request this branch already has
func reviewShipInfo(repoPath any, ghBin string) ShipInfo {
	cwd, err := hardening.ResolveRequestedPathForIpc(repoPath, "Review ship info", "")
	if err != nil {
		return ShipInfo{}
	}
	if !runGh([]string{"auth", "status"}, cwd, ghBin).ok {
		return ShipInfo{}
	}
	view := runGh([]string{"pr", "view", "--json", "url,state,number"}, cwd, ghBin)
	if !view.ok {
		return ShipInfo{GhReady: true}
	}
	pr, err := parseJSON(view.stdout)
	if err != nil || !jsTruthy(pr) || !jsTruthy(jsGet(pr, "url")) {
		return ShipInfo{GhReady: true}
	}
	out := &jsObj{vals: map[string]any{}}
	for _, key := range []string{"url", "state", "number"} {
		if v, present := pr.(*jsObj).vals[key]; present {
			out.keys = append(out.keys, key)
			out.vals[key] = v
		}
	}
	return ShipInfo{GhReady: true, Pr: out}
}

const (
	prQueryBranchChunk = 50
	prQueryBranchCap   = 300
	prNodeFields       = "number state isDraft isCrossRepository title url headRefName"
)

// GraphQL document asking for the pull requests of each branch and each number by alias
func prQueryFor(owner, name string, branches []string, numbers []float64) string {
	var fields []string
	for i, branch := range branches {
		fields = append(fields, "b"+strconv.Itoa(i)+": pullRequests(headRefName: "+jsJSONString(branch)+", first: 5, "+
			"orderBy: {field: CREATED_AT, direction: DESC}) "+"{ nodes { "+prNodeFields+" } }")
	}
	for i, number := range numbers {
		fields = append(fields, "n"+strconv.Itoa(i)+": pullRequest(number: "+jsNumberString(number)+") { "+prNodeFields+" }")
	}
	return "query { repository(owner: " + jsJSONString(owner) + ", name: " + jsJSONString(name) + ") {\n" + strings.Join(fields, "\n") + "\n} }"
}

// Pull request of one branch as the sidebar badges it
type PrBadge struct {
	Branch string  `json:"branch"`
	Draft  bool    `json:"draft"`
	Number float64 `json:"number"`
	State  string  `json:"state"`
	Title  string  `json:"title"`
	URL    string  `json:"url"`
}

func textOrEmpty(v any) string {
	if !jsTruthy(v) {
		return ""
	}
	return jsString(v)
}

func prPayload(pr any) PrBadge {
	number := jsNumber(jsGet(pr, "number"))
	if !jsTruthy(number) {
		number = 0
	}
	return PrBadge{
		Branch: jsString(jsGet(pr, "headRefName")),
		Draft:  jsTruthy(jsGet(pr, "isDraft")),
		Number: number,
		State:  jsLower(textOrEmpty(jsGet(pr, "state"))),
		Title:  textOrEmpty(jsGet(pr, "title")),
		URL:    textOrEmpty(jsGet(pr, "url")),
	}
}

// Gh readiness and the pull requests found for the asked branches and numbers
type PrList struct {
	GhReady bool      `json:"ghReady"`
	Prs     []PrBadge `json:"prs"`
}

// Pull requests of one GraphQL answer in alias order, a malformed member ending the chunk
func chunkPrs(repository any) []PrBadge {
	var out []PrBadge
	for _, key := range jsKeys(repository) {
		member := jsIndex(repository, key)
		var pr any
		if strings.HasPrefix(key, "n") {
			pr = member
		} else {
			switch nodes := jsGet(member, "nodes").(type) {
			case nil:
			case []any:
				for _, node := range nodes {
					if jsTruthy(node) && !jsTruthy(jsGet(node, "isCrossRepository")) {
						pr = node
						break
					}
				}
			default:
				return out
			}
		}
		if jsTruthy(jsGet(pr, "headRefName")) {
			out = append(out, prPayload(pr))
		}
	}
	return out
}

// Pull request of each branch and number, one GraphQL request per fifty, fork pull requests skipped by branch
func reviewPrList(repoPath, branches, numbers any, ghBin string) (PrList, error) {
	empty := PrList{Prs: []PrBadge{}}
	cwd, err := hardening.ResolveRequestedPathForIpc(repoPath, "Review PR list", "")
	if err != nil {
		return empty, nil
	}
	var wanted []string
	if jsTruthy(branches) {
		list, ok := branches.([]any)
		if !ok {
			return empty, errors.New("(branches || []).filter is not a function")
		}
		seen := map[string]bool{}
		for _, b := range list {
			if s := jsString(b); jsTruthy(b) && !seen[s] {
				seen[s] = true
				wanted = append(wanted, s)
			}
		}
	}
	wanted = wanted[:min(len(wanted), prQueryBranchCap)]
	var byNumber []float64
	if jsTruthy(numbers) {
		list, ok := numbers.([]any)
		if !ok {
			return empty, errors.New("(numbers || []).map is not a function")
		}
		seen := map[float64]bool{}
		for _, v := range list {
			if n := jsNumber(v); jsTruthy(n) && !seen[n] {
				seen[n] = true
				byNumber = append(byNumber, n)
			}
		}
	}
	byNumber = byNumber[:min(len(byNumber), prQueryBranchCap)]
	if len(wanted) == 0 && len(byNumber) == 0 {
		return empty, nil
	}
	repo := runGh([]string{"repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner"}, cwd, ghBin)
	parts := strings.Split(jsTrim(repo.stdout), "/")
	owner, name := parts[0], ""
	if len(parts) > 1 {
		name = parts[1]
	}
	if !repo.ok || owner == "" || name == "" {
		return empty, nil
	}
	type chunk struct {
		branches []string
		numbers  []float64
	}
	var chunks []chunk
	for start := 0; start < len(wanted); start += prQueryBranchChunk {
		chunks = append(chunks, chunk{branches: wanted[start:min(start+prQueryBranchChunk, len(wanted))]})
	}
	for start := 0; start < len(byNumber); start += prQueryBranchChunk {
		chunks = append(chunks, chunk{numbers: byNumber[start:min(start+prQueryBranchChunk, len(byNumber))]})
	}
	prs := []PrBadge{}
	for _, c := range chunks {
		res := runGh([]string{"api", "graphql", "-f", "query=" + prQueryFor(owner, name, c.branches, c.numbers)}, cwd, ghBin)
		if !res.ok {
			continue
		}
		parsed, err := parseJSON(res.stdout)
		if err != nil {
			continue
		}
		prs = append(prs, chunkPrs(jsGet(jsGet(parsed, "data"), "repository"))...)
	}
	return PrList{GhReady: true, Prs: prs}, nil
}

// Url of a pull request gh created
type CreatedPr struct {
	URL string `json:"url"`
}

// Pushes the branch and opens a pull request filled from its commits, failing with gh's own reason
func reviewCreatePr(repoPath any, gitBin, ghBin string) (CreatedPr, error) {
	cwd, err := hardening.ResolveRequestedPathForIpc(repoPath, "Review create PR", "")
	if err != nil {
		return CreatedPr{}, err
	}
	reviewPush(repoPath, gitBin)
	created := runGh([]string{"pr", "create", "--fill"}, cwd, ghBin)
	if !created.ok {
		reason := jsTrim(created.stderr)
		if reason == "" {
			reason = "is gh installed and authenticated?"
		}
		return CreatedPr{}, errors.New("gh pr create failed: " + reason)
	}
	url := ""
	for _, line := range strings.Split(jsTrim(created.stdout), "\n") {
		if line != "" {
			url = line
		}
	}
	return CreatedPr{URL: url}, nil
}

// One changed path of the coding rail
type RepoStatusFile struct {
	Path       string `json:"path"`
	Staged     bool   `json:"staged"`
	Unstaged   bool   `json:"unstaged"`
	Untracked  bool   `json:"untracked"`
	Conflicted bool   `json:"conflicted"`
}

// Compact working tree state of the composer coding rail
type RepoStatus struct {
	Branch        *string          `json:"branch"`
	DefaultBranch *string          `json:"defaultBranch"`
	Detached      bool             `json:"detached"`
	Ahead         int              `json:"ahead"`
	Behind        int              `json:"behind"`
	Staged        int              `json:"staged"`
	Unstaged      int              `json:"unstaged"`
	Untracked     int              `json:"untracked"`
	Conflicted    int              `json:"conflicted"`
	Changed       int              `json:"changed"`
	Added         int              `json:"added"`
	Removed       int              `json:"removed"`
	Files         []RepoStatusFile `json:"files"`
}

// Branch, ahead and behind, change counts, line totals with untracked files and a capped file list, nil off a repository
func repoStatus(repoPath any, gitBin string) *RepoStatus {
	cwd, err := hardening.ResolveRequestedPathForIpc(repoPath, "Repo status", "")
	if err != nil {
		return nil
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return nil
	}
	g, err := gitFor(cwd, gitBin)
	if err != nil {
		return nil
	}
	status, err := g.status("--untracked-files=normal")
	if err != nil {
		return nil
	}
	rows := make([]RepoStatusFile, 0, len(status.Files))
	staged, unstaged := 0, 0
	for _, f := range status.Files {
		row := RepoStatusFile{
			Path:       f.Path,
			Staged:     isStaged(f),
			Unstaged:   f.WorkingDir != "" && f.WorkingDir != " " && f.WorkingDir != "?",
			Untracked:  f.Index == "?" || f.WorkingDir == "?",
			Conflicted: f.Index == "U" || f.WorkingDir == "U",
		}
		if row.Staged {
			staged++
		}
		if row.Unstaged {
			unstaged++
		}
		rows = append(rows, row)
	}
	result := &RepoStatus{
		DefaultBranch: defaultBranchName(g),
		Detached:      status.Detached,
		Ahead:         status.Ahead,
		Behind:        status.Behind,
		Staged:        staged,
		Unstaged:      unstaged,
		Untracked:     len(status.NotAdded),
		Conflicted:    len(status.Conflicted),
		Changed:       len(rows),
		Files:         rows[:min(len(rows), 200)],
	}
	if !status.Detached && status.Current != nil && *status.Current != "" {
		result.Branch = status.Current
	}
	if summary, err := g.diffSummary("HEAD"); err == nil {
		result.Added, result.Removed = summary.Insertions, summary.Deletions
	}
	untracked := status.NotAdded[:min(len(status.NotAdded), 500)]
	for start := 0; start < len(untracked); start += untrackedLineCountConcurrency {
		batch := untracked[start:min(start+untrackedLineCountConcurrency, len(untracked))]
		counts := make([]int, len(batch))
		var wg sync.WaitGroup
		for i, p := range batch {
			wg.Add(1)
			go func(i int, p string) {
				defer wg.Done()
				counts[i] = untrackedInsertions(cwd, p)
			}(i, p)
		}
		wg.Wait()
		for _, n := range counts {
			result.Added += n
		}
	}
	return result
}
