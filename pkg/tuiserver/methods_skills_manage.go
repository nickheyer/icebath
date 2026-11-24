package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// One skills.manage action handler given the fitted params
type skillsAction func(ctx context.Context, rid any, p tuigateway.SkillsManageParams) (map[string]any, error)

// Skills hub runner over the home of the scoped profile
func skillsHubCLI(ctx context.Context) *skills.HubCLI {
	return skills.NewHubCLI(skills.NewScope(home.Dir(ctx), nil))
}

// Answer of a hub action
func skillsAnswer(rid any, result tuigateway.SkillsManageResult) map[string]any {
	return ok(rid, tuigateway.Plain(result))
}

// Skill names of the scoped profile grouped by category
func skillsListAction(ctx context.Context, rid any, _ tuigateway.SkillsManageParams) (map[string]any, error) {
	var categories map[string][]string
	if err := tuigateway.Loose(Banner.AvailableSkills(ctx), &categories); err != nil {
		return nil, err
	}
	if categories == nil {
		categories = map[string][]string{}
	}
	return skillsAnswer(rid, tuigateway.SkillsManageResult{Skills: categories}), nil
}

// Top twenty hub hits of the query as name and description rows
func skillsSearch(ctx context.Context, rid any, p tuigateway.SkillsManageParams) (map[string]any, error) {
	x := skillsHubCLI(ctx)
	rows := []tuigateway.SkillHubHit{}
	for _, meta := range x.Hub.UnifiedSearch(ctx, p.Query, x.ResolvedSources(), "all", 20) {
		rows = append(rows, tuigateway.SkillHubHit{Name: meta.Name, Description: meta.Description})
	}
	return skillsAnswer(rid, tuigateway.SkillsManageResult{Results: rows}), nil
}

// Installs without prompts, a failure answering 5031 with the last printed line and the log tail
func skillsInstall(ctx context.Context, rid any, p tuigateway.SkillsManageParams) (map[string]any, error) {
	identifier := p.Query
	var mu sync.Mutex
	var lines []string
	captured := &pyrich.Console{Width: 80, Capture: func(raw string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, pyrich.Plain(raw))
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
		return skillsAnswer(rid, tuigateway.SkillsManageResult{Installed: true, Name: identifier}), nil
	}
	mu.Lock()
	tail := lines[max(0, len(lines)-12):]
	mu.Unlock()
	log := strings.TrimSpace(strings.Join(tail, "\n"))
	message := "skill install failed"
	var logValue any
	if log != "" {
		split := pystr.Splitlines(log, false)
		message, logValue = strings.TrimPrefix(split[len(split)-1], "Error: "), log
	}
	return rpcErr(rid, 5031, message, map[string]any{"installed": false, "name": identifier, "log": logValue}), nil
}

// One hub browse page, the page param first, then a digit query, then page one
func skillsBrowse(ctx context.Context, rid any, p tuigateway.SkillsManageParams) (map[string]any, error) {
	page := int(p.Page)
	if page == 0 {
		page = 1
		if n, err := strconv.Atoi(p.Query); err == nil && n > 0 {
			page = n
		} else if errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(p.Query, "-") {
			page = math.MaxInt
		}
	}
	size := int(p.PageSize)
	if size == 0 {
		size = 20
	}
	browsed := skillsHubCLI(ctx).BrowseSkills(ctx, page, size, "all")
	items := make([]tuigateway.SkillBrowseItem, 0, len(browsed.Items))
	for _, item := range browsed.Items {
		items = append(items, tuigateway.SkillBrowseItem{Name: item.Name, Description: item.Description, Source: item.Source, Trust: item.Trust, Identifier: item.Identifier})
	}
	return skillsAnswer(rid, tuigateway.SkillsManageResult{Items: items, Page: int64(browsed.Page), TotalPages: int64(browsed.TotalPages), Total: int64(browsed.Total)}), nil
}

// Hub metadata and SKILL.md preview of the query with the profile's own install of it, empty when it resolves nowhere
func skillsInspect(ctx context.Context, rid any, p tuigateway.SkillsManageParams) (map[string]any, error) {
	scope := skills.NewScope(home.Dir(ctx), nil)
	if dir, found := scope.FindSkill(p.Query); found {
		info := tuigateway.SkillInspectInfo{Name: filepath.Base(dir), Source: "installed", Path: filepath.Join(dir, "SKILL.md")}
		for _, row := range scope.FindAllSkills(false) {
			if row["name"] == info.Name || row["name"] == p.Query {
				info.Category, _ = row["category"].(string)
				info.Description, _ = row["description"].(string)
				break
			}
		}
		return skillsAnswer(rid, tuigateway.SkillsManageResult{Info: &info}), nil
	}
	info := tuigateway.SkillInspectInfo{}
	if hub := skillsHubCLI(ctx).InspectSkill(ctx, p.Query); hub != nil {
		info = tuigateway.SkillInspectInfo{Name: hub.Name, Description: hub.Description, Source: hub.Source, Identifier: hub.Identifier,
			Tags: hub.Tags, SkillMdPreview: hub.SkillMDPreview}
	}
	return skillsAnswer(rid, tuigateway.SkillsManageResult{Info: &info}), nil
}

// Action table of skills.manage
var skillsActions = map[string]skillsAction{
	"list": skillsListAction, "search": skillsSearch, "install": skillsInstall, "browse": skillsBrowse, "inspect": skillsInspect,
}

// What the query names for each action that cannot run without one
var skillsQueryNeeds = map[tuigateway.SkillsAction]string{
	"install": "the skill to install", "inspect": "the skill to inspect", "search": "the search query",
}

// Skills hub backend listing the profile skills or searching, browsing, inspecting and installing from the hub
func skillsManage(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	p, err := tuigateway.SkillsManage.Params(params)
	if err != nil {
		return nil, err
	}
	if what, needs := skillsQueryNeeds[p.Action]; needs {
		raw := params["query"]
		if _, isText := raw.(string); !isText {
			return nil, fmt.Errorf("%s must be text, got %s", what, errchain.Kind(raw))
		}
		if strings.TrimSpace(p.Query) == "" {
			return nil, fmt.Errorf("%s is empty", what)
		}
	}
	return runAction(rid, params, skillsActions, "skills", func(h skillsAction) (map[string]any, error) { return h(ctx, rid, p) })
}

func init() {
	scopedRPC(toolsRPCRegistry, "skills.manage", 5024, rpcShape{}, skillsManage)
}
