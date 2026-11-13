package git

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/files"
	"github.com/nickheyer/icebath/pkg/desktop/hardening"
)

const worktreeGitTimeout = 30 * time.Second

// Git command that exited non zero, its message stderr or the exit code
type GitCommandError struct {
	Message string
	Stderr  string
}

func (e *GitCommandError) Error() string { return e.Message }

// Stdout of a git command run hidden with a thirty second deadline, rejected on a non zero exit
func runGit(gitBin string, args []string, cwd string) (string, error) {
	res, err := files.ExecGit(gitBin, args, cwd, nil, worktreeGitTimeout)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		msg := res.Stderr
		if msg == "" {
			msg = "git exited " + strconv.Itoa(res.Code)
		}
		return "", &GitCommandError{Message: msg, Stderr: res.Stderr}
	}
	return res.Stdout, nil
}

// Stderr a failed git command carried, a timed out one included, empty for a spawn failure
func gitErrorStderr(err error) string {
	var gitErr *GitCommandError
	if errors.As(err, &gitErr) {
		return gitErr.Stderr
	}
	var timeout *files.GitTimeoutError
	if errors.As(err, &timeout) {
		return timeout.Stderr
	}
	return ""
}

// Trimmed stdout of a git command, empty when it fails
func gitLine(gitBin string, args []string, cwd string) string {
	out, err := runGit(gitBin, args, cwd)
	if err != nil {
		return ""
	}
	return jsTrim(out)
}

// True when the git command exits zero, the reading a quiet probe needs
func gitOk(gitBin string, args []string, cwd string) bool {
	_, err := runGit(gitBin, args, cwd)
	return err == nil
}

// Fetches one remote branch into its tracking ref by explicit refspec, true when the remote has it
func fetchTrackingRef(gitBin, root, remote, branch string) bool {
	return gitOk(gitBin, []string{"fetch", remote, "+refs/heads/" + branch + ":refs/remotes/" + remote + "/" + branch}, root)
}

type parsedWorktree struct {
	path     string
	branch   *string
	detached bool
	locked   bool
}

var headsPrefixRe = regexp.MustCompile(`^refs/heads/`)

// Records of git worktree list --porcelain, the main worktree first
func parseWorktrees(out string) []parsedWorktree {
	var trees []parsedWorktree
	var cur *parsedWorktree
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			if cur != nil {
				trees = append(trees, *cur)
			}
			cur = &parsedWorktree{path: jsTrim(line[9:])}
		case cur == nil:
		case strings.HasPrefix(line, "branch "):
			b := headsPrefixRe.ReplaceAllString(jsTrim(line[7:]), "")
			cur.branch = &b
		case line == "detached":
			cur.detached = true
		case strings.HasPrefix(line, "locked"):
			cur.locked = true
		}
	}
	if cur != nil {
		trees = append(trees, *cur)
	}
	return trees
}

// One checkout of a repository
type Worktree struct {
	Path     string  `json:"path"`
	Branch   *string `json:"branch"`
	IsMain   bool    `json:"isMain"`
	Detached bool    `json:"detached"`
	Locked   bool    `json:"locked"`
}

// Worktrees of a repository, empty for a refused path or a folder git cannot list
func listWorktrees(repoPath any, gitBin string) []Worktree {
	out := []Worktree{}
	resolved, err := hardening.ResolveRequestedPathForIpc(repoPath, "Worktree list", "")
	if err != nil {
		return out
	}
	text, err := runGit(gitBin, []string{"worktree", "list", "--porcelain"}, resolved)
	if err != nil {
		return out
	}
	for i, t := range parseWorktrees(text) {
		out = append(out, Worktree{Path: t.path, Branch: t.branch, IsMain: i == 0, Detached: t.detached, Locked: t.locked})
	}
	return out
}

var (
	branchSpaceRe   = regexp.MustCompile(jsSpaceClass + `+`)
	branchInvalidRe = regexp.MustCompile(`[^\w./-]`)
	branchDashesRe  = regexp.MustCompile(`-{2,}`)
	branchSlashesRe = regexp.MustCompile(`/{2,}`)
	branchDotsRe    = regexp.MustCompile(`\.{2,}`)
	branchEdgesRe   = regexp.MustCompile(`^[-./]+|[-./]+$`)
	slugInvalidRe   = regexp.MustCompile(`[^a-z0-9]+`)
	slugEdgesRe     = regexp.MustCompile(`^-+|-+$`)
	slugTailRe      = regexp.MustCompile(`-+$`)
	alreadyExistsRe = regexp.MustCompile(`(?i)already exists`)
)

// Git ref safe branch name, spaces to dashes and forbidden characters dropped, empty when nothing usable remains
func sanitizeBranch(name any) string {
	s := ""
	if jsTruthy(name) {
		s = jsString(name)
	}
	s = branchSpaceRe.ReplaceAllString(s, "-")
	s = branchInvalidRe.ReplaceAllString(s, "")
	s = branchDashesRe.ReplaceAllString(s, "-")
	s = branchSlashesRe.ReplaceAllString(s, "/")
	s = branchDotsRe.ReplaceAllString(s, ".")
	return branchEdgesRe.ReplaceAllString(s, "")
}

// Lowercase dash separated folder name of at most forty characters, work when nothing remains
func slugify(name any) string {
	s := ""
	if jsTruthy(name) {
		s = jsString(name)
	}
	s = slugInvalidRe.ReplaceAllString(jsLower(jsTrim(s)), "-")
	s = slugEdgesRe.ReplaceAllString(s, "")
	s = slugTailRe.ReplaceAllString(s[:min(len(s), 40)], "")
	if s == "" {
		return "work"
	}
	return s
}

var trunkBranches = []string{"main", "master"}

// Remote a remote tracking ref belongs to, empty when the name is not one in this repository
func remoteOfRef(gitBin, cwd, name string) string {
	if !strings.Contains(name, "/") {
		return ""
	}
	if !gitOk(gitBin, []string{"show-ref", "--verify", "--quiet", "refs/remotes/" + name}, cwd) {
		return ""
	}
	return name[:strings.Index(name, "/")]
}

// Trunk branch from origin/HEAD, init.defaultBranch, then a local main or master, empty when none
func defaultBranch(gitBin, cwd string) string {
	if remote := originPrefixRe.ReplaceAllString(gitLine(gitBin, []string{"symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"}, cwd), ""); remote != "" {
		return remote
	}
	if configured := gitLine(gitBin, []string{"config", "--get", "init.defaultBranch"}, cwd); configured != "" {
		return configured
	}
	for _, branch := range trunkBranches {
		if gitLine(gitBin, []string{"show-ref", "--verify", "refs/heads/" + branch}, cwd) != "" {
			return branch
		}
	}
	return ""
}

// Makes the folder a repository with an empty root commit when it is not one or has no HEAD yet
func ensureGitRepo(gitBin, dir string) error {
	needsRoot := false
	inside, err := runGit(gitBin, []string{"rev-parse", "--is-inside-work-tree"}, dir)
	if err == nil {
		if jsTrim(inside) != "true" {
			_, err = runGit(gitBin, []string{"init"}, dir)
			needsRoot = err == nil
		} else if _, headErr := runGit(gitBin, []string{"rev-parse", "--verify", "HEAD"}, dir); headErr != nil {
			needsRoot = true
		}
	}
	if err != nil {
		if _, err := runGit(gitBin, []string{"init"}, dir); err != nil {
			return err
		}
		needsRoot = true
	}
	if needsRoot {
		_, err := runGit(gitBin, []string{"-c", "user.email=hermes@localhost", "-c", "user.name=Icebath", "commit", "--allow-empty", "-m", "Initial commit"}, dir)
		return err
	}
	return nil
}

// Main worktree root so new worktrees nest under the primary checkout
func mainRoot(gitBin, cwd string) string {
	for _, t := range listWorktrees(cwd, gitBin) {
		if t.IsMain {
			return t.Path
		}
	}
	return cwd
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// First of base, base-2, base-3 and on that does not exist yet
func uniqueDir(base string) string {
	dir := base
	for n := 1; pathExists(dir); {
		n++
		dir = base + "-" + strconv.Itoa(n)
	}
	return dir
}

// Checkout a worktree add made
type AddedWorktree struct {
	Path     string `json:"path"`
	Branch   string `json:"branch"`
	RepoRoot string `json:"repoRoot"`
}

// Worktree for an existing local or remote branch, the trunk switched in place instead
func addExistingBranchWorktree(gitBin, root string, name any) (AddedWorktree, error) {
	requested := sanitizeBranch(name)
	if requested == "" {
		return AddedWorktree{}, errors.New("Branch name is required.")
	}
	remote := remoteOfRef(gitBin, root, requested)
	fetched := false
	if remote == "" && strings.Contains(requested, "/") && !gitOk(gitBin, []string{"show-ref", "--verify", "--quiet", "refs/heads/" + requested}, root) {
		maybeRemote := requested[:strings.Index(requested, "/")]
		if gitLine(gitBin, []string{"remote", "get-url", maybeRemote}, root) != "" && fetchTrackingRef(gitBin, root, maybeRemote, requested[len(maybeRemote)+1:]) {
			remote = maybeRemote
			fetched = true
		}
	}
	branch := requested
	if remote != "" {
		branch = requested[len(remote)+1:]
	}
	if remote == "" && branch == defaultBranch(gitBin, root) {
		if _, err := runGit(gitBin, []string{"switch", branch}, root); err != nil {
			return AddedWorktree{}, err
		}
		return AddedWorktree{Path: root, Branch: branch, RepoRoot: root}, nil
	}
	dir := uniqueDir(hostPath.join(root, ".worktrees", slugify(branch)))
	if remote != "" {
		fetched = fetched || fetchTrackingRef(gitBin, root, remote, branch)
		if !gitOk(gitBin, []string{"worktree", "add", "--track", "-b", branch, dir, requested}, root) {
			if _, err := runGit(gitBin, []string{"worktree", "add", "-b", branch, dir, requested}, root); err != nil {
				return AddedWorktree{}, err
			}
			if fetched {
				gitOk(gitBin, []string{"remote", "set-branches", "--add", remote, branch}, root)
				gitOk(gitBin, []string{"branch", "--set-upstream-to=" + requested, branch}, root)
			}
		}
		return AddedWorktree{Path: dir, Branch: branch, RepoRoot: root}, nil
	}
	if _, err := runGit(gitBin, []string{"worktree", "add", dir, branch}, root); err != nil {
		return AddedWorktree{}, err
	}
	return AddedWorktree{Path: dir, Branch: branch, RepoRoot: root}, nil
}

// New worktree under .worktrees of the main checkout, the folder made a repository first when needed
func addWorktree(repoPath, options any, gitBin string) (AddedWorktree, error) {
	resolved, err := hardening.ResolveRequestedPathForIpc(repoPath, "Worktree add", "")
	if err != nil {
		return AddedWorktree{}, err
	}
	if err := ensureGitRepo(gitBin, resolved); err != nil {
		return AddedWorktree{}, err
	}
	root := mainRoot(gitBin, resolved)
	opts := jsObject(options)
	if jsTruthy(opts["existingBranch"]) {
		return addExistingBranchWorktree(gitBin, root, opts["existingBranch"])
	}
	nameInput := opts["name"]
	if !jsTruthy(nameInput) {
		nameInput = "work-" + strconv.FormatInt(time.Now().UnixMilli(), 36)
	}
	slug := slugify(nameInput)
	branch := sanitizeBranch(opts["branch"])
	if branch == "" {
		branch = "icebath/" + slug
	}
	dir := uniqueDir(hostPath.join(root, ".worktrees", slug))
	args := []string{"worktree", "add", "-b", branch, dir}
	if jsTruthy(opts["base"]) {
		base := jsString(opts["base"])
		if strings.HasPrefix(base, "origin/") {
			remoteBranch := base[len("origin/"):]
			if gitOk(gitBin, []string{"check-ref-format", "--branch", remoteBranch}, root) {
				fetchTrackingRef(gitBin, root, "origin", remoteBranch)
			}
			args = append(args, "--no-track")
		}
		args = append(args, base)
	}
	if _, err := runGit(gitBin, args, root); err != nil {
		if !alreadyExistsRe.MatchString(gitErrorStderr(err)) {
			return AddedWorktree{}, err
		}
		if _, err := runGit(gitBin, []string{"worktree", "add", dir, branch}, root); err != nil {
			return AddedWorktree{}, err
		}
	}
	return AddedWorktree{Path: dir, Branch: branch, RepoRoot: root}, nil
}

// Path a worktree remove deleted
type RemovedWorktree struct {
	Removed string `json:"removed"`
}

// Removes a linked worktree from the main checkout, forced when asked
func removeWorktree(repoPath, worktreePath, options any, gitBin string) (RemovedWorktree, error) {
	resolvedRepo, err := hardening.ResolveRequestedPathForIpc(repoPath, "Worktree remove (repo)", "")
	if err != nil {
		return RemovedWorktree{}, err
	}
	resolvedTree, err := hardening.ResolveRequestedPathForIpc(worktreePath, "Worktree remove (tree)", "")
	if err != nil {
		return RemovedWorktree{}, err
	}
	root := mainRoot(gitBin, resolvedRepo)
	args := []string{"worktree", "remove"}
	if jsTruthy(jsObject(options)["force"]) {
		args = append(args, "--force")
	}
	if _, err := runGit(gitBin, append(args, resolvedTree), root); err != nil {
		return RemovedWorktree{}, err
	}
	return RemovedWorktree{Removed: resolvedTree}, nil
}

// One branch the convert to worktree picker offers
type Branch struct {
	Name         string  `json:"name"`
	CheckedOut   bool    `json:"checkedOut"`
	IsDefault    bool    `json:"isDefault"`
	IsRemote     bool    `json:"isRemote"`
	WorktreePath *string `json:"worktreePath"`
}

func refNames(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if t := jsTrim(line); t != "" {
			names = append(names, t)
		}
	}
	return names
}

// Local heads most recently committed first, then remote branches with no local head, each with its worktree
func listBranches(repoPath any, gitBin string) []Branch {
	out := []Branch{}
	resolved, err := hardening.ResolveRequestedPathForIpc(repoPath, "Branch list", "")
	if err != nil {
		return out
	}
	var wg sync.WaitGroup
	var localOut, remoteOut string
	var localErr, remoteErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		localOut, localErr = runGit(gitBin, []string{"for-each-ref", "--format=%(refname:short)", "--sort=-committerdate", "refs/heads"}, resolved)
	}()
	go func() {
		defer wg.Done()
		remoteOut, remoteErr = runGit(gitBin, []string{"for-each-ref", "--format=%(refname:short)", "--sort=-committerdate", "refs/remotes"}, resolved)
	}()
	wg.Wait()
	if localErr != nil || remoteErr != nil {
		return out
	}
	pathByBranch := map[string]string{}
	for _, t := range listWorktrees(resolved, gitBin) {
		if t.Branch != nil && *t.Branch != "" {
			pathByBranch[*t.Branch] = t.Path
		}
	}
	trunk := defaultBranch(gitBin, resolved)
	locals := refNames(localOut)
	localSet := map[string]bool{}
	for _, name := range locals {
		localSet[name] = true
	}
	for _, name := range locals {
		b := Branch{Name: name, IsDefault: trunk != "" && name == trunk}
		if p, ok := pathByBranch[name]; ok {
			b.CheckedOut = true
			if p != "" {
				b.WorktreePath = &p
			}
		}
		out = append(out, b)
	}
	for _, name := range refNames(remoteOut) {
		if strings.HasSuffix(name, "/HEAD") || localSet[name[strings.Index(name, "/")+1:]] {
			continue
		}
		out = append(out, Branch{Name: name, IsRemote: true})
	}
	return out
}

// Branch a switch checked out, nil for a folder that is not a repository
type BranchSwitch struct {
	Branch *string `json:"branch"`
}

// Switches the checkout to a sanitized branch, a no op answering null outside a repository
func switchBranch(repoPath, branch any, gitBin string) (BranchSwitch, error) {
	resolved, err := hardening.ResolveRequestedPathForIpc(repoPath, "Branch switch", "")
	if err != nil {
		return BranchSwitch{}, err
	}
	inside := "false"
	if out, err := runGit(gitBin, []string{"rev-parse", "--is-inside-work-tree"}, resolved); err == nil {
		inside = jsTrim(out)
	}
	if inside != "true" {
		return BranchSwitch{}, nil
	}
	target := sanitizeBranch(branch)
	if target == "" {
		return BranchSwitch{}, errors.New("Branch name is required.")
	}
	if _, err := runGit(gitBin, []string{"switch", target}, resolved); err != nil {
		return BranchSwitch{}, err
	}
	return BranchSwitch{Branch: &target}, nil
}

// One ref a new worktree can start from
type BaseBranch struct {
	Name      string `json:"name"`
	IsRemote  bool   `json:"isRemote"`
	IsDefault bool   `json:"isDefault"`
}

// Local heads and remote tracking refs most recently committed first, the default flagged
func listBaseBranches(repoPath any, gitBin string) []BaseBranch {
	result := []BaseBranch{}
	resolved, err := hardening.ResolveRequestedPathForIpc(repoPath, "Base branch list", "")
	if err != nil {
		return result
	}
	out, err := runGit(gitBin, []string{"for-each-ref", "--format=%(refname:short)\t%(committerdate:iso)", "--sort=-committerdate", "refs/heads", "refs/remotes"}, resolved)
	if err != nil {
		return result
	}
	remoteDefault := gitLine(gitBin, []string{"symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"}, resolved)
	localDefault := defaultBranch(gitBin, resolved)
	for _, line := range refNames(out) {
		name, _, _ := strings.Cut(line, "\t")
		result = append(result, BaseBranch{
			Name:      name,
			IsRemote:  strings.HasPrefix(name, "origin/"),
			IsDefault: (remoteDefault != "" && name == remoteDefault) || (remoteDefault == "" && localDefault != "" && name == localDefault),
		})
	}
	return result
}
