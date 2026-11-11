package dashboard

import (
	"context"
	"errors"
	"os"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Router listing the host directories a new dashboard chat can start in
func chatWorkspacesRouter() *APIRouter {
	r := NewAPIRouter("")
	r.Get("/api/chat/workspaces", &Endpoint{
		Name: "get_chat_workspaces",
		Doc: "Projects + discovered repos a fresh chat may start in; ``scan=1`` rescans the\n" +
			"    configured discovery roots on the host first (headless installs have no Desktop to do it).",
		Params: []Param{QueryParam("profile", Optional(Str), nil), QueryParam("scan", Bool, false)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			profile, scan := c.Str("profile"), c.Bool("scan")
			out, err := scopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
				return collectWorkspaces(sctx, optionalStr(c, "profile"), scan)
			})
			if err != nil {
				return nil, httpFailure(err, "GET /api/chat/workspaces failed", 500, "Failed to list workspaces", nil)
			}
			return out, nil
		},
	})
	return r
}

// Existing host directory a fresh chat cwd names, nil when unset and 400 for a dead or missing one
func resolveChatCwd(raw *string) (*string, error) {
	text := ""
	if raw != nil {
		text = pystr.Strip(*raw)
	}
	if text == "" {
		return nil, nil
	}
	resolved, err := pypath.Abspath(pypath.Expanduser(text))
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(resolved); err != nil || !fi.IsDir() {
		return nil, NewHTTPException(400, "Working directory does not exist: "+text)
	}
	return &resolved, nil
}

// Projects, discovered repos, default cwd and home of the profile, the discovery roots rescanned when asked
func collectWorkspaces(ctx context.Context, profile *string, scan bool) (*pyjson.Object, error) {
	db, err := openSessionDBForProfile(ctx, profile, true)
	if err != nil {
		var httpErr *HTTPException
		if !errors.As(err, &httpErr) || httpErr.StatusCode != 503 {
			return nil, err
		}
		db = nil
	}
	var projects []map[string]any
	var repos []map[string]any
	var policy map[string]any
	err = func() error {
		if db != nil {
			defer db.Close()
		}
		pdb, err := tools.OpenProjectsDB(ctx, &tools.Env{Home: home.Dir(ctx)})
		if err != nil {
			return err
		}
		defer pdb.Close()
		policy = tuiserver.RepoDiscoveryPolicy(ctx, nil)
		if scan && pyrepr.Truthy(policy["enabled"]) {
			tuiserver.ScanDiscoveredReposRemote(ctx, pdb, policy)
		}
		listed, err := pdb.ListProjects(false)
		if err != nil {
			return err
		}
		projects = make([]map[string]any, 0, len(listed))
		for _, p := range listed {
			projects = append(projects, p.AsMap())
		}
		repos, err = tuiserver.DiscoverReposPayload(ctx, db, pdb, false, pyrepr.Truthy(policy["enabled"]))
		return err
	}()
	if err != nil {
		return nil, err
	}
	params := map[string]any{}
	if profile != nil && *profile != "" {
		params["profile"] = *profile
	}
	defaultCwd, err := tuiserver.CompletionCwd(ctx, params)
	if err != nil {
		return nil, err
	}
	homeDir, err := pypath.PathHome()
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("projects", projects, "repos", repos, "default_cwd", defaultCwd, "home", homeDir,
		"scan_enabled", pyrepr.Truthy(policy["enabled"])), nil
}
