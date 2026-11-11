package dashboard

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Result of a blocking git op, a failed mutation answered 400 with its message
func gitOp[T any](result T, err error) (T, error) {
	var named interface{ PyTypeName() string }
	if err != nil && errors.As(err, &named) && named.PyTypeName() == "RuntimeError" {
		detail := err.Error()
		if detail == "" {
			detail = "git operation failed"
		}
		return result, &causedHTTPException{NewHTTPException(400, detail), err}
	}
	return result, err
}

// Host path of a git route path or file, relative to cwd when one is given
func gitFsPath(path string, cwd *string) (string, error) {
	resolved, herr := tuigateway.FsPath(path, cwd, true)
	if herr != nil {
		return "", NewHTTPException(herr.Status, herr.Detail)
	}
	return resolved, nil
}

// Host path of a git route repository path
func gitPath(path string) (string, error) { return gitFsPath(path, nil) }

const ghAuthTTLS = 300.0

// Shared gh auth probe, the cached answer and the probe in flight
var ghAuth struct {
	mu       sync.Mutex
	cachedAt float64
	cached   *pyjson.Object
	probe    *ghAuthProbe
}

// One gh auth status run other requesters join
type ghAuthProbe struct {
	done    chan struct{}
	payload *pyjson.Object
	started float64
}

// Whether the gh CLI exists and has a logged in host
func probeGhAuth() *pyjson.Object {
	gh, err := exec.LookPath("gh")
	if err != nil || gh == "" {
		return pyjson.ObjectOf("available", false, "authenticated", false)
	}
	proc := tools.BoundedProbeRun([]string{gh, "auth", "status"}, 10*time.Second, nil, "", nil)
	return pyjson.ObjectOf("available", true, "authenticated", proc != nil && proc.Code == 0)
}

// Cached gh auth status for the composer pill, a refresh waiting out any probe started before it was asked
func ghAuthStatus(ctx context.Context, refresh bool) (any, error) {
	asked := pythread.Monotonic()
	ghAuth.mu.Lock()
	if !refresh && ghAuth.cached != nil && asked-ghAuth.cachedAt < ghAuthTTLS {
		cached := ghAuth.cached
		ghAuth.mu.Unlock()
		return cached, nil
	}
	ghAuth.mu.Unlock()
	var payload *pyjson.Object
	for {
		ghAuth.mu.Lock()
		if ghAuth.probe == nil {
			probe := &ghAuthProbe{done: make(chan struct{}), started: pythread.Monotonic()}
			ghAuth.probe = probe
			go func() {
				probe.payload = probeGhAuth()
				close(probe.done)
				ghAuth.mu.Lock()
				if ghAuth.probe == probe {
					ghAuth.probe = nil
				}
				ghAuth.mu.Unlock()
			}()
		}
		probe := ghAuth.probe
		ghAuth.mu.Unlock()
		select {
		case <-probe.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		payload = probe.payload
		if !refresh || probe.started >= asked {
			break
		}
	}
	ghAuth.mu.Lock()
	ghAuth.cachedAt, ghAuth.cached = pythread.Monotonic(), payload
	ghAuth.mu.Unlock()
	return payload, nil
}

// Diff of one file of a review scope after the hosted read guard admits it
func gitReviewDiff(path, file, scope string, base *string, staged bool) (any, error) {
	cwd, err := gitPath(path)
	if err != nil {
		return nil, err
	}
	target, err := gitFsPath(file, &cwd)
	if err != nil {
		return nil, err
	}
	if _, err := hostedFSReadGuard(target); err != nil {
		return nil, err
	}
	diff, err := gitOp(reviewDiff(cwd, target, scope, base, staged))
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("diff", diff), nil
}

// Diff of one file against HEAD after the hosted read guard admits it
func gitFileDiff(path, file string) (any, error) {
	cwd, err := gitPath(path)
	if err != nil {
		return nil, err
	}
	target, err := gitFsPath(file, &cwd)
	if err != nil {
		return nil, err
	}
	if _, err := hostedFSReadGuard(target); err != nil {
		return nil, err
	}
	diff, err := gitOp(fileDiffVsHead(cwd, target))
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("diff", diff), nil
}

// Commit context of a repo limited to the files a locked deployment may read
func gitCommitContext(path string) (any, error) {
	cwd, err := gitPath(path)
	if err != nil {
		return nil, err
	}
	root, err := hostedFSReadGuard(cwd)
	if err != nil {
		return nil, err
	}
	if root == "" {
		return reviewCommitContext(cwd, nil), nil
	}
	var guardErr error
	allowed := func(target string) bool {
		if guardErr != nil {
			return false
		}
		ok, err := hostedFSPathAllowed(root, target)
		if err != nil {
			guardErr = err
			return false
		}
		return ok
	}
	result := reviewCommitContext(cwd, allowed)
	if guardErr != nil {
		return nil, guardErr
	}
	return result, nil
}

// Path field of a git body resolved to its host path
func gitBodyPath(body *pyjson.Object, key string) (string, error) {
	v, _ := body.Get(key)
	s, _ := v.(string)
	return gitPath(s)
}

// Remote half of the desktop coding rail and review pane over the web_git helpers
func gitRouter() *APIRouter {
	r := NewAPIRouter("")
	pathParam := RequiredQuery("path", Str)
	r.Get("/api/git/status", &Endpoint{Name: "git_status_route", Params: []Param{pathParam},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			cwd, err := gitPath(c.Str("path"))
			if err != nil {
				return nil, err
			}
			return repoStatus(cwd), nil
		}})
	r.Get("/api/git/gh-auth", &Endpoint{Name: "gh_auth_status_route",
		Doc:    "``{\"available\", \"authenticated\"}`` for the `gh` CLI; cached 5 min\n    (``refresh=true`` bypasses so the pill withdraws right after a login).",
		Params: []Param{QueryParam("refresh", Bool, false)},
		Handle: func(ctx context.Context, c *Call) (any, error) { return ghAuthStatus(ctx, c.Bool("refresh")) }})
	r.Get("/api/git/worktrees", &Endpoint{Name: "git_worktrees_route", Params: []Param{pathParam},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			cwd, err := gitPath(c.Str("path"))
			if err != nil {
				return nil, err
			}
			rows := worktreeList(cwd)
			list := make([]any, len(rows))
			for i, row := range rows {
				list[i] = row
			}
			return pyjson.ObjectOf("worktrees", list), nil
		}})
	r.Get("/api/git/branches", &Endpoint{Name: "git_branches_route", Params: []Param{pathParam},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			cwd, err := gitPath(c.Str("path"))
			if err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("branches", branchList(cwd)), nil
		}})
	r.Get("/api/git/base-branches", &Endpoint{Name: "git_base_branches_route", Params: []Param{pathParam},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			cwd, err := gitPath(c.Str("path"))
			if err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("branches", baseBranchList(cwd)), nil
		}})
	r.Get("/api/git/review/list", &Endpoint{Name: "git_review_list_route",
		Params: []Param{pathParam, QueryParam("scope", Str, "uncommitted"), QueryParam("base", Optional(Str), nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			cwd, err := gitPath(c.Str("path"))
			if err != nil {
				return nil, err
			}
			return reviewList(cwd, c.Str("scope"), callOptStr(c, "base")), nil
		}})
	r.Get("/api/git/review/diff", &Endpoint{Name: "git_review_diff_route",
		Params: []Param{pathParam, RequiredQuery("file", Str), QueryParam("scope", Str, "uncommitted"),
			QueryParam("base", Optional(Str), nil), QueryParam("staged", Bool, false)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return gitReviewDiff(c.Str("path"), c.Str("file"), c.Str("scope"), callOptStr(c, "base"), c.Bool("staged"))
		}})
	r.Get("/api/git/file-diff", &Endpoint{Name: "git_file_diff_route", Params: []Param{pathParam, RequiredQuery("file", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) { return gitFileDiff(c.Str("path"), c.Str("file")) }})
	r.Get("/api/git/review/commit-context", &Endpoint{Name: "git_commit_context_route", Params: []Param{pathParam},
		Handle: func(ctx context.Context, c *Call) (any, error) { return gitCommitContext(c.Str("path")) }})
	r.Get("/api/git/review/rev-parse", &Endpoint{Name: "git_rev_parse_route",
		Params: []Param{pathParam, QueryParam("ref", Optional(Str), nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			cwd, err := gitPath(c.Str("path"))
			if err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("sha", reviewRevParse(cwd, callOptStr(c, "ref"))), nil
		}})
	r.Get("/api/git/review/ship-info", &Endpoint{Name: "git_ship_info_route", Params: []Param{pathParam},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			cwd, err := gitPath(c.Str("path"))
			if err != nil {
				return nil, err
			}
			return reviewShipInfo(cwd), nil
		}})
	r.Post("/api/git/review/pr-list", &Endpoint{Name: "git_pr_list_route", Body: []BodyParam{Body("body", gitPrListBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			body := c.Object("body")
			cwd, err := gitBodyPath(body, "path")
			if err != nil {
				return nil, err
			}
			var branches []string
			if raw, _ := body.Get("branches"); raw != nil {
				list, _ := raw.([]any)
				for _, b := range list {
					s, _ := b.(string)
					branches = append(branches, s)
				}
			}
			var numbers []int64
			if raw, _ := body.Get("numbers"); raw != nil {
				list, _ := raw.([]any)
				for _, n := range list {
					v, _ := n.(int64)
					numbers = append(numbers, v)
				}
			}
			return reviewPrList(cwd, branches, numbers), nil
		}})
	fileRoute := func(name string, op func(cwd string, file *string) (any, error)) *Endpoint {
		return &Endpoint{Name: name, Body: []BodyParam{Body("body", gitFileBodyModel)},
			Handle: func(ctx context.Context, c *Call) (any, error) {
				body := c.Object("body")
				cwd, err := gitBodyPath(body, "path")
				if err != nil {
					return nil, err
				}
				return gitOp(op(cwd, optionalStringField(body, "file")))
			}}
	}
	r.Post("/api/git/review/stage", fileRoute("git_stage_route", func(cwd string, file *string) (any, error) {
		return reviewStage(cwd, file)
	}))
	r.Post("/api/git/review/unstage", fileRoute("git_unstage_route", func(cwd string, file *string) (any, error) {
		return reviewUnstage(cwd, file)
	}))
	r.Post("/api/git/review/revert", fileRoute("git_revert_route", func(cwd string, file *string) (any, error) {
		return reviewRevert(cwd, file), nil
	}))
	r.Post("/api/git/review/commit", &Endpoint{Name: "git_commit_route", Body: []BodyParam{Body("body", gitCommitBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			body := c.Object("body")
			cwd, err := gitBodyPath(body, "path")
			if err != nil {
				return nil, err
			}
			message, _ := body.Get("message")
			push, _ := body.Get("push")
			text, _ := message.(string)
			return gitOp(reviewCommit(cwd, text, pyTruthy(push)))
		}})
	pathRoute := func(name string, op func(cwd string) (*pyjson.Object, error)) *Endpoint {
		return &Endpoint{Name: name, Body: []BodyParam{Body("body", gitPathBodyModel)},
			Handle: func(ctx context.Context, c *Call) (any, error) {
				cwd, err := gitBodyPath(c.Object("body"), "path")
				if err != nil {
					return nil, err
				}
				return gitOp(op(cwd))
			}}
	}
	r.Post("/api/git/review/push", pathRoute("git_push_route", reviewPush))
	r.Post("/api/git/review/create-pr", pathRoute("git_create_pr_route", reviewCreatePr))
	r.Post("/api/git/worktree/add", &Endpoint{Name: "git_worktree_add_route", Body: []BodyParam{Body("body", gitWorktreeAddBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			body := c.Object("body")
			options := pyjson.NewObject()
			for _, key := range []string{"name", "branch", "base", "existingBranch"} {
				if v, _ := body.Get(key); pyTruthy(v) {
					options.Set(key, v)
				}
			}
			cwd, err := gitBodyPath(body, "path")
			if err != nil {
				return nil, err
			}
			return gitOp(worktreeAdd(ctx, cwd, options))
		}})
	r.Post("/api/git/worktree/remove", &Endpoint{Name: "git_worktree_remove_route",
		Body: []BodyParam{Body("body", gitWorktreeRemoveBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			body := c.Object("body")
			cwd, err := gitBodyPath(body, "path")
			if err != nil {
				return nil, err
			}
			worktree, err := gitBodyPath(body, "worktreePath")
			if err != nil {
				return nil, err
			}
			force, _ := body.Get("force")
			return gitOp(worktreeRemove(cwd, worktree, pyTruthy(force)))
		}})
	r.Post("/api/git/branch/switch", &Endpoint{Name: "git_branch_switch_route", Body: []BodyParam{Body("body", gitBranchSwitchBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			body := c.Object("body")
			cwd, err := gitBodyPath(body, "path")
			if err != nil {
				return nil, err
			}
			branch, _ := body.Get("branch")
			text, _ := branch.(string)
			return gitOp(branchSwitch(cwd, text))
		}})
	return r
}

// Optional string query value, nil when absent
func callOptStr(c *Call, name string) *string {
	s, ok := c.Value(name).(string)
	if !ok {
		return nil
	}
	return &s
}
