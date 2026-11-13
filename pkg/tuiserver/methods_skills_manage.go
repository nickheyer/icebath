package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// One skills.manage action handler given the raw query param
type skillsAction func(ctx context.Context, rid any, params map[string]any, query any) (map[string]any, error)

// Skills hub runner over the home of the scoped profile
func skillsHubCLI(ctx context.Context) *skills.HubCLI {
	return skills.NewHubCLI(skills.NewScope(home.Dir(ctx), nil))
}

// Skill names of the scoped profile grouped by category
func skillsListAction(ctx context.Context, rid any, _ map[string]any, _ any) (map[string]any, error) {
	return ok(rid, map[string]any{"skills": Banner.AvailableSkills(ctx)}), nil
}

// Top twenty hub hits of the query, every hit failing the row build as SkillMeta has no describe
func skillsSearch(ctx context.Context, rid any, _ map[string]any, query any) (map[string]any, error) {
	text, isText := query.(string)
	if !isText {
		return nil, fmt.Errorf("'%s' object has no attribute 'strip'", pyrepr.TypeName(query))
	}
	x := skillsHubCLI(ctx)
	if raw := x.Hub.UnifiedSearch(ctx, text, x.ResolvedSources(), "all", 20); len(raw) > 0 {
		return nil, errors.New("'SkillMeta' object has no attribute 'describe'")
	}
	return ok(rid, map[string]any{"results": []any{}}), nil
}

// Installs without prompts, a failure answering 5031 with the last printed line and the log tail
func skillsInstall(ctx context.Context, rid any, _ map[string]any, query any) (map[string]any, error) {
	identifier, isText := query.(string)
	if !isText {
		return nil, fmt.Errorf("'%s' object has no attribute 'rstrip'", pyrepr.TypeName(query))
	}
	var mu sync.Mutex
	var lines []string
	captured := &pyrich.Console{Width: 80, Capture: func(raw string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, raw)
	}}
	x := skillsHubCLI(ctx)
	verdict, err := x.DoInstall(ctx, identifier, skills.HubInstallOptions{SkipConfirm: true, Console: captured})
	if err != nil {
		return nil, err
	}
	installed := verdict == skills.HubOK
	if verdict == skills.HubNoop {
		builtin, err := x.S.BundledSkillForInstall(identifier)
		if err != nil {
			return nil, err
		}
		installed = builtin != ""
	}
	if installed {
		return ok(rid, map[string]any{"installed": true, "name": query}), nil
	}
	mu.Lock()
	tail := lines[max(0, len(lines)-12):]
	mu.Unlock()
	log := pystr.Strip(strings.Join(tail, "\n"))
	message := "skill install failed"
	var logValue any
	if log != "" {
		split := pystr.Splitlines(log, false)
		message, logValue = split[len(split)-1], log
	}
	return rpcErr(rid, 5031, message, map[string]any{"installed": false, "name": query, "log": logValue}), nil
}

// Go int nearest a Python int, the clamp leaving page arithmetic unchanged
func clampedInt(n *big.Int) int {
	if n.IsInt64() && n.Int64() >= math.MinInt && n.Int64() <= math.MaxInt {
		return int(n.Int64())
	}
	if n.Sign() > 0 {
		return math.MaxInt
	}
	return math.MinInt
}

// One hub browse page, the page param first, then a digit query, then page one
func skillsBrowse(ctx context.Context, rid any, params map[string]any, query any) (map[string]any, error) {
	rawPage := params["page"]
	if !pyrepr.Truthy(rawPage) {
		rawPage = 0
	}
	page, err := pyint.Of(rawPage)
	if err != nil {
		return nil, err
	}
	if page.Sign() == 0 {
		text, isText := query.(string)
		if !isText {
			return nil, fmt.Errorf("'%s' object has no attribute 'isdigit'", pyrepr.TypeName(query))
		}
		page = big.NewInt(1)
		if pystr.StrIsDigit(text) {
			if page, err = pyint.Of(text); err != nil {
				return nil, err
			}
		}
	}
	rawSize, present := params["page_size"]
	if !present {
		rawSize = 20
	}
	size, err := pyint.Of(rawSize)
	if err != nil {
		return nil, err
	}
	return ok(rid, skillsHubCLI(ctx).BrowseSkills(ctx, clampedInt(page), clampedInt(size), "all")), nil
}

// Hub metadata and SKILL.md preview of the query, an empty info when it resolves nowhere
func skillsInspect(ctx context.Context, rid any, _ map[string]any, query any) (map[string]any, error) {
	switch q := query.(type) {
	case string:
		if info := skillsHubCLI(ctx).InspectSkill(ctx, q); info != nil {
			return ok(rid, map[string]any{"info": info}), nil
		}
		return ok(rid, map[string]any{"info": map[string]any{}}), nil
	case []any:
		for _, item := range q {
			if item == "/" {
				return ok(rid, map[string]any{"info": map[string]any{}}), nil
			}
		}
		return nil, errors.New("'list' object has no attribute 'strip'")
	case map[string]any:
		if _, slash := q["/"]; slash {
			return ok(rid, map[string]any{"info": map[string]any{}}), nil
		}
		return nil, errors.New("'dict' object has no attribute 'strip'")
	}
	return nil, fmt.Errorf("argument of type '%s' is not iterable", pyrepr.TypeName(query))
}

// Action table of skills.manage
var skillsActions = map[string]skillsAction{
	"list": skillsListAction, "search": skillsSearch, "install": skillsInstall, "browse": skillsBrowse, "inspect": skillsInspect,
}

// Skills hub backend listing the profile skills or searching, browsing, inspecting and installing from the hub
func skillsManage(ctx context.Context, rid any, params map[string]any, _ *tuigateway.Session) (map[string]any, error) {
	query, present := params["query"]
	if !present {
		query = ""
	}
	return runAction(rid, params, skillsActions, "skills", func(h skillsAction) (map[string]any, error) { return h(ctx, rid, params, query) })
}

func init() {
	scopedRPC(toolsRPCRegistry, "skills.manage", 5024, rpcShape{}, skillsManage)
}
