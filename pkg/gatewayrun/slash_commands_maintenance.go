package gatewayrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Curator CLI run on argv writing its report, help and errors to out, assigned in cmd/icebath wiring
var RunCuratorSlash func(ctx context.Context, env *tools.Env, argv []string, out io.Writer)

// Redacted local debug report of the profile, assigned in cmd/icebath wiring
var GatewayDebugLocalReport func(ctx context.Context) (string, error)

// Summary only debug upload and its chat reply, assigned in cmd/icebath wiring
var GatewayDebugUploadReply func(ctx context.Context) (string, error)

// Messaging platforms allowed to start an update
var updateAllowedPlatforms = map[gateway.Platform]bool{
	gateway.PlatformTelegram: true, gateway.PlatformSlack: true, gateway.PlatformWhatsApp: true, gateway.PlatformSignal: true,
	gateway.PlatformMatrix: true, gateway.PlatformEmail: true, gateway.PlatformSMS: true, gateway.PlatformDingTalk: true,
	gateway.PlatformFeishu: true, gateway.PlatformWeCom: true, gateway.PlatformWeComCallback: true, gateway.PlatformWeixin: true,
	gateway.PlatformBlueBubbles: true, gateway.PlatformQQBot: true, gateway.PlatformLocal: true,
}

// Adapter refreshing platform side state that cached the skill list at startup
type skillGroupRefresher interface {
	RefreshSkillGroup(ctx context.Context) error
}

// Rescans the skills directory and queues the added and removed skills as a note for the next turn
func (r *GatewayRunner) handleReloadSkillsCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	reply, err := r.reloadSkillsReply(ctx, event)
	if err != nil {
		runLog().Warn(fmt.Sprintf("Skills reload failed: %s", pyrepr.ExceptionMessage(err)))
		return i18n.T(ctx, "gateway.reload_skills.failed", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	return reply, nil
}

// Reload skills body whose error becomes the failure reply
func (r *GatewayRunner) reloadSkillsReply(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	raw, err := r.runInExecutorWithContext(ctx, func(ctx context.Context) (any, error) {
		return skills.NewScope(home.Dir(ctx), nil).ReloadSkills(ctx)
	})
	if err != nil {
		return "", err
	}
	result, _ := raw.(map[string]any)
	added, removed := bundleMaps(result["added"]), bundleMaps(result["removed"])
	total, ok := result["total"]
	if !ok {
		total = 0
	}
	if r.adapters != nil {
		for _, adapter := range r.adapters.Values() {
			if refresher, ok := adapter.(skillGroupRefresher); ok {
				if err := refresher.RefreshSkillGroup(ctx); err != nil {
					runLog().Warn(fmt.Sprintf("Adapter %s refresh_skill_group raised: %s", adapter.Name(), pyrepr.ExceptionMessage(err)))
				}
			}
		}
	}
	lines := []string{i18n.T(ctx, "gateway.reload_skills.header", "", nil)}
	if len(added) == 0 && len(removed) == 0 {
		lines = append(lines, i18n.T(ctx, "gateway.reload_skills.no_new", "", nil),
			i18n.T(ctx, "gateway.reload_skills.total", "", map[string]any{"count": total}))
		return strings.Join(lines, "\n"), nil
	}
	fmtLine := func(item map[string]any) string {
		name, desc := item["name"], item["description"]
		if name == nil {
			name = ""
		}
		if pyrepr.Truthy(desc) {
			return i18n.T(ctx, "gateway.reload_skills.item_with_desc", "", map[string]any{"name": name, "desc": desc})
		}
		return i18n.T(ctx, "gateway.reload_skills.item_no_desc", "", map[string]any{"name": name})
	}
	sections := []string{"[USER INITIATED SKILLS RELOAD:"}
	for _, group := range []struct {
		key, header string
		items       []map[string]any
	}{{"gateway.reload_skills.added_header", "Added Skills:", added}, {"gateway.reload_skills.removed_header", "Removed Skills:", removed}} {
		if len(group.items) == 0 {
			continue
		}
		formatted := make([]string, len(group.items))
		for i, item := range group.items {
			formatted[i] = fmtLine(item)
		}
		lines = append(append(lines, i18n.T(ctx, group.key, "", nil)), formatted...)
		sections = append(append(sections, "", group.header), formatted...)
	}
	lines = append(lines, i18n.T(ctx, "gateway.reload_skills.total", "", map[string]any{"count": total}))
	sections = append(sections, "", "Use skills_list to see the updated catalog.]")
	sessionKey, err := r.sessionKeyForSource(ctx, event.Source)
	if err != nil {
		return "", err
	}
	if r.pendingSkillsReloadNotes == nil {
		r.pendingSkillsReloadNotes = map[string]string{}
	}
	if sessionKey != "" {
		r.pendingSkillsReloadNotes[sessionKey] = strings.Join(sections, "\n")
	}
	return strings.Join(lines, "\n"), nil
}

// Runs the curator CLI in the session's profile scope, status by default, its printed report the reply
func (r *GatewayRunner) handleCuratorCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	tokens, err := pyshlex.SplitPOSIX(event.GetCommandArgs(), false)
	if err != nil {
		return "", err
	}
	if len(tokens) == 0 {
		tokens = []string{"status"}
	}
	raw, err := r.runInExecutorWithContext(ctx, func(ctx context.Context) (any, error) {
		var out bytes.Buffer
		RunCuratorSlash(ctx, slashToolsEnv(ctx), tokens, &out)
		return pystr.Rstrip(out.String()), nil
	})
	if err != nil {
		return "", err
	}
	return raw.(string), nil
}

// Uploads the summary debug report, or returns it unsent for a local session's debug local
func (r *GatewayRunner) handleDebugCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	wantsLocal := false
	for _, w := range pystr.SplitWhitespace(event.GetCommandArgs(), -1) {
		if pystr.Lower(w) == "local" {
			wantsLocal = true
		}
	}
	if wantsLocal && event.Source.Platform != gateway.PlatformLocal {
		return i18n.T(ctx, "gateway.debug.local_refused", "", nil), nil
	}
	collect := GatewayDebugUploadReply
	if wantsLocal {
		collect = GatewayDebugLocalReport
	}
	raw, err := r.runInExecutorWithContext(ctx, func(ctx context.Context) (any, error) { return collect(ctx) })
	if err != nil {
		return "", err
	}
	return raw.(string), nil
}

// Spawns icebath update detached with marker files so this or the next gateway notifies the requester
func (r *GatewayRunner) handleUpdateCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	src := event.Source
	if !updateAllowedPlatforms[src.Platform] {
		entry := gateway.Registry.Get(ctx, string(src.Platform))
		if entry == nil || !entry.AllowUpdateCommand {
			return i18n.T(ctx, "gateway.update.platform_not_messaging", "", nil), nil
		}
	}
	h := home.Dir(ctx)
	if config.ManagedSystem(ctx, h) != "" {
		return "✗ " + config.FormatManagedMessage(ctx, h, i18n.T(ctx, "gateway.update.managed_action", "", nil)), nil
	}
	projectRoot := pm.RepoRoot()
	if resolved, err := filepath.EvalSymlinks(projectRoot); err == nil {
		projectRoot = resolved
	}
	if method, err := config.DetectInstallMethod(ctx, h, projectRoot); err == nil && method != "git" && method != "unknown" {
		return i18n.T(ctx, "gateway.update.not_applicable", "", map[string]any{"method": method,
			"command": config.RecommendedUpdateCommandForMethod(method)}), nil
	}
	if _, err := os.Stat(filepath.Join(projectRoot, ".git")); err != nil {
		return i18n.T(ctx, "gateway.update.not_git_repo", "", nil), nil
	}
	hermesCmd := resolveHermesBin()
	if len(hermesCmd) == 0 {
		return i18n.T(ctx, "gateway.update.hermes_cmd_not_found", "", nil), nil
	}
	pendingPath := filepath.Join(h, ".update_pending.json")
	outputPath := filepath.Join(h, ".update_output.txt")
	exitCodePath := filepath.Join(h, ".update_exit_code")
	sessionKey, err := r.sessionKeyForSource(ctx, src)
	if err != nil {
		return "", err
	}
	pending := pyjson.ObjectOf("platform", string(src.Platform), "chat_id", src.ChatID, "chat_type", src.ChatType,
		"user_id", optAny(src.UserID), "session_key", sessionKey, "timestamp", pydatetime.ISOFormatNaive(time.Now()))
	for _, kv := range []struct {
		key   string
		value *string
	}{{"thread_id", src.ThreadID}, {"message_id", event.MessageID}, {"profile", src.Profile}} {
		if kv.value != nil && *kv.value != "" {
			pending.Set(kv.key, *kv.value)
		}
	}
	text, err := pyjson.Dumps(pending)
	if err != nil {
		return "", err
	}
	tmpPending := filepath.Join(h, ".update_pending.tmp")
	if err := os.WriteFile(tmpPending, []byte(text), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPending, pendingPath); err != nil {
		return "", err
	}
	if err := os.Remove(exitCodePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := spawnDetachedUpdate(hermesCmd, outputPath, exitCodePath); err != nil {
		_ = os.Remove(pendingPath)
		_ = os.Remove(exitCodePath)
		return i18n.T(ctx, "gateway.update.start_failed", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	r.scheduleUpdateNotificationWatch(ctx)
	return i18n.T(ctx, "gateway.update.starting", "", nil), nil
}

// Reconnects MCP servers after a slash confirm, straight away once the user opted out of the prompt
func (r *GatewayRunner) handleReloadMCPCommand(ctx context.Context, event *gateway.MessageEvent) (*string, error) {
	sessionKey, err := r.sessionKeyForSource(ctx, event.Source)
	if err != nil {
		return nil, err
	}
	userConfig := r.readUserConfig(ctx)
	if approvals, ok := userConfig["approvals"].(map[string]any); ok {
		gate, present := approvals["mcp_reload_confirm"]
		if present && !pyrepr.Truthy(gate) {
			reply, err := r.executeMCPReload(ctx, event)
			if err != nil {
				return nil, err
			}
			return &reply, nil
		}
	}
	onConfirm := func(ctx context.Context, choice string) (string, error) {
		if choice == "cancel" {
			return i18n.T(ctx, "gateway.reload_mcp.cancelled", "", nil), nil
		}
		if choice == "always" {
			SaveConfigValue(ctx, "approvals.mcp_reload_confirm", false)
			runLog().Info(fmt.Sprintf("User opted out of /reload-mcp confirmation (session=%s)", sessionKey))
		}
		result, err := r.executeMCPReload(ctx, event)
		if err != nil {
			return "", err
		}
		if choice == "always" {
			return result + "\n\n" + i18n.T(ctx, "gateway.reload_mcp.always_followup", "", nil), nil
		}
		return result, nil
	}
	return r.requestSlashConfirm(ctx, event, "reload-mcp", "/reload-mcp", i18n.T(ctx, "gateway.reload_mcp.confirm_prompt", "", nil), onConfirm)
}
