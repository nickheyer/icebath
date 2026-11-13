package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
)

// Profile listing the router reads, the profiles_mod.list_profiles attribute hermes tests replace
var listProfilesForRouter = manage.ListProfiles

// Dashboard row of one profile, the module attribute hermes tests replace
var profileToDict = manage.ProfileToDict

// Profile rows with lazy skill counts, the directory scan fallback when the listing fails
func readProfiles(ctx context.Context, _ ...any) (*pyjson.Object, error) {
	infos, err := listProfilesForRouter(ctx, true)
	if err != nil {
		webServerLog().Error("GET /api/profiles failed; falling back to profile directory scan", "exc_info", err)
		return pyjson.ObjectOf("profiles", rowList(fallbackProfileDicts(ctx))), nil
	}
	rows := make([]any, len(infos))
	for i, p := range infos {
		rows[i] = profileToDict(p)
	}
	return pyjson.ObjectOf("profiles", rows), nil
}

// Profile listing coalesced across a poll burst before worker admission
var profilesRead = newCoalescedRead(readProfiles)

// Optional string field of a body, nil for None
func optField(body *pyjson.Object, key string) *string {
	if s, ok := body.GetOr(key, nil).(string); ok {
		return &s
	}
	return nil
}

// String list field of a body
func strListField(body *pyjson.Object, key string) []string {
	list, _ := body.GetOr(key, nil).([]any)
	out := make([]string, 0, len(list))
	for _, v := range list {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// Creates a profile then sets its model, MCP servers, skill selection and hub installs without failing the create
func createProfileEndpoint(ctx context.Context, c *Call) (any, error) {
	body := c.Object("body")
	name, _ := body.GetOr("name", nil).(string)
	cloneFromRaw := optField(body, "clone_from")
	cloneAll := body.GetOr("clone_all", false) == true
	explicitSource := ""
	if cloneFromRaw != nil {
		explicitSource = pystr.Strip(*cloneFromRaw)
	}
	var clone, cloneConfig bool
	var cloneFrom *string
	switch {
	case explicitSource != "":
		clone, cloneFrom, cloneConfig = true, &explicitSource, !cloneAll
	case cloneAll:
		def := "default"
		clone, cloneFrom, cloneConfig = true, &def, false
	default:
		clone = body.GetOr("clone_from_default", false) == true
		if clone {
			def := "default"
			cloneFrom = &def
		}
		cloneConfig = clone
	}
	soul := optField(body, "soul")
	path, err := func() (string, error) {
		path, err := manage.CreateProfile(ctx, name, manage.CreateOptions{CloneFrom: cloneFrom, CloneAll: cloneAll,
			CloneConfig: cloneConfig, NoSkills: body.GetOr("no_skills", false) == true, Description: optField(body, "description"),
			CloneChannels: body.GetOr("clone_channels", false) == true, Soul: soul})
		if err != nil {
			return "", err
		}
		if !clone {
			manage.SeedProfileSkills(ctx, path, true)
		}
		collision, err := profiles.CheckAliasCollision(name)
		if err != nil {
			return "", err
		}
		if collision == "" {
			if _, err := profiles.CreateWrapperScript(name, ""); err != nil {
				return "", err
			}
		}
		return path, nil
	}()
	if err := profileErrors(err, "POST /api/profiles failed", nil, []profileExc{excValue, excFileExists, excFileNotFound}); err != nil {
		return nil, err
	}

	providerName, model := "", ""
	if p := optField(body, "provider"); p != nil {
		providerName = pystr.Strip(*p)
	}
	if m := optField(body, "model"); m != nil {
		model = pystr.Strip(*m)
	}
	modelError := ""
	modelSet := false
	if providerName != "" && model != "" {
		modelSet = profileBestEffort(fmt.Sprintf("Setting model for new profile %s failed", name), false, func() (bool, error) {
			err := writeProfileModel(ctx, path, providerName, model, home.Process(), "")
			if h, ok := asHTTPException(err); ok {
				modelError = pyrepr.Str(h.Detail)
				return false, nil
			}
			return err == nil, err
		})
	}
	mcpServers, _ := body.GetOr("mcp_servers", nil).([]any)
	mcpWritten := 0
	if len(mcpServers) > 0 {
		mcpWritten = profileBestEffort(fmt.Sprintf("Writing MCP servers for new profile %s failed", name), 0, func() (int, error) {
			return writeProfileMCPServers(ctx, path, mcpServers)
		})
	}
	keepSkills := strListField(body, "keep_skills")
	skillsDisabled := 0
	if len(keepSkills) > 0 {
		skillsDisabled = profileBestEffort(fmt.Sprintf("Applying skill selection for new profile %s failed", name), 0, func() (int, error) {
			return disableUnselectedSkills(ctx, path, keepSkills)
		})
	}
	hubInstalls := []any{}
	for _, raw := range strListField(body, "hub_skills") {
		ident := pystr.Strip(raw)
		if ident == "" {
			continue
		}
		pid := profileBestEffort[any](fmt.Sprintf("Spawning hub-skill install %s for new profile %s failed", ident, name), nil, func() (any, error) {
			proc, err := spawnHermesAction(ctx, []string{"-p", name, "skills", "install", ident, "--yes"}, hubActionName("install", ident), nil)
			if err != nil {
				return nil, err
			}
			return proc.Pid(), nil
		})
		hubInstalls = append(hubInstalls, pyjson.ObjectOf("identifier", ident, "pid", pid))
	}
	return pyjson.ObjectOf("ok", true, "name", name, "path", path, "model_set", modelSet, "model_error", modelError,
		"mcp_written", mcpWritten, "skills_disabled", skillsDisabled, "hub_installs", hubInstalls, "soul_set", soul != nil), nil
}

// Value of fn or default when it is empty, onError when it raises
func orDefaultProfile(fn func() string, onError string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = onError
		}
	}()
	if v := fn(); v != "" {
		return v
	}
	return "default"
}

// Executable and argv of each Linux terminal emulator the open terminal route tries in order
func linuxTerminalCommands(command string) [][]string {
	sh := []string{"sh", "-lc", command}
	quoted := "sh -lc '" + command + "'"
	terminals := []struct {
		exe  string
		flag *string
	}{{"x-terminal-emulator", termFlag("-e")}, {"gnome-terminal", termFlag("--")}, {"konsole", termFlag("-e")},
		{"xfce4-terminal", nil}, {"mate-terminal", nil}, {"lxterminal", nil}, {"tilix", termFlag("-e")},
		{"alacritty", termFlag("-e")}, {"kitty", termFlag("")}, {"xterm", termFlag("-e")}}
	out := make([][]string, 0, len(terminals))
	for _, t := range terminals {
		if t.flag == nil {
			out = append(out, []string{t.exe, "-e", quoted})
			continue
		}
		argv := []string{t.exe}
		if *t.flag != "" {
			argv = append(argv, *t.flag)
		}
		out = append(out, append(argv, sh...))
	}
	return out
}

// Flag a terminal emulator takes before the sh argv
func termFlag(s string) *string { return &s }

// Starts argv with the parent's stdio and reaps it in the background
func popenDetachedProfile(argv []string) error {
	cmd := popenCommand(argv)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// Exit status of which for an executable, an error when which itself cannot run
func whichStatus(executable string) (int, error) {
	cmd := popenCommand([]string{"which", executable})
	cmd.Stdin = os.Stdin
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// Opens a terminal running the profile setup command on this host
func openProfileTerminal(command string) error {
	switch {
	case runtime.GOOS == "windows":
		return popenDetachedProfile([]string{"cmd.exe", "/c", "start", "", command})
	case runtime.GOOS == "darwin":
		escaped := strings.ReplaceAll(strings.ReplaceAll(command, `\`, `\\`), `"`, `\"`)
		return popenDetachedProfile([]string{"osascript", "-e",
			"tell application \"Terminal\"\nactivate\ndo script \"" + escaped + "\"\nend tell"})
	}
	for _, argv := range linuxTerminalCommands(command) {
		status, err := whichStatus(argv[0])
		if err != nil {
			return err
		}
		if status == 0 {
			return popenDetachedProfile(argv)
		}
	}
	return NewHTTPException(400, "No supported terminal emulator found")
}

// Text of a file read as utf-8-sig with universal newlines
func readTextUTF8Sig(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text, err := pystr.DecodeUTF8SigFile(raw)
	if err != nil {
		return "", err
	}
	return pystr.TranslateNewlines(text), nil
}

// Desktop appearance overlay bundled with an imported profile, raising when unreadable
func readDesktopOverlay(profileDir string) (any, error) {
	text, err := readTextUTF8Sig(filepath.Join(profileDir, "desktop.json"))
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(text)
}

// Whether a path is a regular file with symlinks followed
func isFilePath(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Router of the profile management routes, registered after the cross profile session reads
func profilesRouter() *APIRouter {
	r := NewAPIRouter("")
	r.Get("/api/profiles", &Endpoint{
		Name: "list_profiles_endpoint",
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return profilesRead.call(ctx)
		},
	})
	r.Post("/api/profiles", &Endpoint{
		Name:   "create_profile_endpoint",
		Body:   []BodyParam{Body("body", profileCreateModel)},
		Handle: createProfileEndpoint,
	})
	r.Get("/api/profiles/active", &Endpoint{
		Name: "get_active_profile_endpoint",
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return pyjson.ObjectOf(
				"active", orDefaultProfile(func() string { return profiles.GetActiveProfile("") }, "default"),
				"current", orDefaultProfile(func() string { return profiles.GetActiveProfileName(ctx) }, "custom")), nil
		},
	})
	r.Post("/api/profiles/active", &Endpoint{
		Name: "set_active_profile_endpoint",
		Body: []BodyParam{Body("body", profileActiveUpdateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			name, _ := c.Object("body").GetOr("name", nil).(string)
			err := profileErrors(profiles.SetActiveProfile(name), "POST /api/profiles/active failed",
				profileNotFoundDefault, profileBadRequestDefault)
			if err != nil {
				return nil, err
			}
			active, err := profiles.NormalizeProfileName(name)
			if err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("ok", true, "active", active), nil
		},
	})
	r.Get("/api/profiles/{name}/setup-command", &Endpoint{
		Name:   "get_profile_setup_command",
		Params: []Param{PathParam("name", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			command, err := profileSetupCommand(c.Str("name"))
			if err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("command", command), nil
		},
	})
	r.Post("/api/profiles/{name}/open-terminal", &Endpoint{
		Name:   "open_profile_terminal_endpoint",
		Params: []Param{PathParam("name", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			name := c.Str("name")
			command, err := profileSetupCommand(name)
			if err == nil {
				err = openProfileTerminal(command)
			}
			if err := profileErrors(err, fmt.Sprintf("POST /api/profiles/%s/open-terminal failed", name),
				profileNotFoundDefault, profileBadRequestDefault); err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("ok", true, "command", command), nil
		},
	})
	r.Patch("/api/profiles/{name}", &Endpoint{
		Name:   "rename_profile_endpoint",
		Params: []Param{PathParam("name", Str)},
		Body:   []BodyParam{Body("body", profileRenameModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			name := c.Str("name")
			newName, _ := c.Object("body").GetOr("new_name", nil).(string)
			path, err := manage.RenameProfile(ctx, name, newName)
			if err := profileErrors(err, fmt.Sprintf("PATCH /api/profiles/%s failed", name), profileNotFoundDefault,
				[]profileExc{excValue, excFileExists}); err != nil {
				return nil, err
			}
			if canon, err := profiles.NormalizeProfileName(name); err == nil && canon == "default" {
				return pyjson.ObjectOf("ok", true, "name", "default", "display_name", pystr.Strip(newName), "path", path), nil
			}
			canon, err := profiles.NormalizeProfileName(newName)
			if err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("ok", true, "name", canon, "path", path), nil
		},
	})
	r.Delete("/api/profiles/{name}", &Endpoint{
		Name:   "delete_profile_endpoint",
		Params: []Param{PathParam("name", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			name := c.Str("name")
			path, err := manage.DeleteProfile(ctx, name, true)
			err = profileErrors(err, fmt.Sprintf("DELETE /api/profiles/%s failed", name), profileNotFoundDefault, profileBadRequestDefault)
			var pending *manage.ProfileIdentitySettlementPending
			if errors.As(err, &pending) {
				return pyjson.ObjectOf("ok", true, "path", pending.Path, "identity_settled", false,
					"settlement_pending", true, "retry_command", pending.RetryCommand), nil
			}
			if err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("ok", true, "path", path), nil
		},
	})
	r.Get("/api/profiles/{name}/soul", &Endpoint{
		Name:   "get_profile_soul",
		Params: []Param{PathParam("name", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			dir, err := resolveProfileDir(c.Str("name"))
			if err != nil {
				return nil, err
			}
			soulPath := filepath.Join(dir, "SOUL.md")
			if !pathExistsFollow(soulPath) {
				return pyjson.ObjectOf("content", "", "exists", false), nil
			}
			content, err := readTextUTF8Sig(soulPath)
			if err != nil {
				if pypath.IsOSError(err) {
					return nil, NewHTTPException(500, "Could not read SOUL.md: "+pyErrText(err))
				}
				return nil, err
			}
			return pyjson.ObjectOf("content", content, "exists", true), nil
		},
	})
	r.Put("/api/profiles/{name}/soul", &Endpoint{
		Name:   "update_profile_soul",
		Params: []Param{PathParam("name", Str)},
		Body:   []BodyParam{Body("body", profileSoulUpdateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			name := c.Str("name")
			dir, err := resolveProfileDir(name)
			if err != nil {
				return nil, err
			}
			soulPath := filepath.Join(dir, "SOUL.md")
			content, _ := c.Object("body").GetOr("content", nil).(string)
			createMode := os.FileMode(0o644)
			err = pyfs.AtomicWriteTextWith(ctx, soulPath, content, pyfs.TextWrite{PreserveMode: true, CreateMode: &createMode})
			if err != nil {
				if pypath.IsOSError(err) {
					webServerLog().Error(fmt.Sprintf("PUT /api/profiles/%s/soul failed", name), "exc_info", err)
					return nil, NewHTTPException(500, "Could not write SOUL.md: "+pyErrText(err))
				}
				return nil, err
			}
			return pyjson.ObjectOf("ok", true), nil
		},
	})
	r.Put("/api/profiles/{name}/description", &Endpoint{
		Name:   "update_profile_description_endpoint",
		Params: []Param{PathParam("name", Str)},
		Body:   []BodyParam{Body("body", profileDescriptionUpdateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			name := c.Str("name")
			dir, err := resolveProfileDir(name)
			if err != nil {
				return nil, err
			}
			text := ""
			if d, ok := c.Object("body").GetOr("description", nil).(string); ok {
				text = pystr.Strip(d)
			}
			auto := false
			err = profiles.WriteProfileMeta(ctx, dir, profiles.MetaUpdate{Description: &text, DescriptionAuto: &auto})
			if err := profileErrors(err, fmt.Sprintf("PUT /api/profiles/%s/description failed", name), nil, nil); err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("ok", true, "description", text, "description_auto", false), nil
		},
	})
	r.Put("/api/profiles/{name}/model", &Endpoint{
		Name:   "update_profile_model_endpoint",
		Params: []Param{PathParam("name", Str)},
		Body:   []BodyParam{Body("body", profileModelUpdateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			name := c.Str("name")
			dir, err := resolveProfileDir(name)
			if err != nil {
				return nil, err
			}
			body := c.Object("body")
			providerName, _ := body.GetOr("provider", nil).(string)
			model, _ := body.GetOr("model", nil).(string)
			providerName, model = pystr.Strip(providerName), pystr.Strip(model)
			if providerName == "" || model == "" {
				return nil, NewHTTPException(400, "provider and model are required")
			}
			err = writeProfileModel(ctx, dir, providerName, model, "", "")
			if err := profileErrors(err, fmt.Sprintf("PUT /api/profiles/%s/model failed", name), nil, nil); err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("ok", true, "provider", providerName, "model", model), nil
		},
	})
	r.Post("/api/profiles/{name}/describe-auto", &Endpoint{
		Name:   "describe_profile_auto_endpoint",
		Params: []Param{PathParam("name", Str)},
		Body:   []BodyParam{Body("body", profileDescribeAutoModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			name := c.Str("name")
			if _, err := resolveProfileDir(name); err != nil {
				return nil, err
			}
			outcome, err := manage.DescribeProfile(ctx, name, c.Object("body").GetOr("overwrite", false) == true, 0)
			if err := profileErrors(err, fmt.Sprintf("POST /api/profiles/%s/describe-auto failed", name), nil, nil); err != nil {
				return nil, err
			}
			var description any
			if outcome.Description != nil {
				description = *outcome.Description
			}
			return pyjson.ObjectOf("ok", outcome.OK, "reason", outcome.Reason, "description", description,
				"description_auto", outcome.OK), nil
		},
	})
	r.Post("/api/profiles/{name}/export", &Endpoint{
		Name:   "export_profile_endpoint",
		Params: []Param{PathParam("name", Str)},
		Body:   []BodyParam{Body("body", profileExportModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			name := c.Str("name")
			body := c.Object("body")
			output, _ := body.GetOr("output", nil).(string)
			output = pystr.Strip(output)
			if output == "" {
				path, err := manage.GetProfileExportPath(name, "")
				switch {
				case err != nil && excValue.matches(err):
					return nil, NewHTTPException(400, pyErrText(err))
				case err != nil && pypath.IsOSError(err):
					return nil, NewHTTPException(500, "Could not create export directory: "+pyErrText(err))
				case err != nil:
					return nil, err
				}
				output = path
			}
			var extra []manage.ExportExtraFile
			if files, ok := body.GetOr("extra_files", nil).(*pyjson.Object); ok {
				for _, k := range files.Keys {
					content, _ := files.Vals[k].(string)
					extra = append(extra, manage.ExportExtraFile{Rel: k, Content: content})
				}
			}
			result, err := manage.ExportProfile(name, output, extra)
			if err := profileErrors(err, fmt.Sprintf("POST /api/profiles/%s/export failed", name),
				profileNotFoundDefault, profileBadRequestDefault); err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("ok", true, "archive", result), nil
		},
	})
	r.Post("/api/profiles/import", &Endpoint{
		Name: "import_profile_endpoint",
		Body: []BodyParam{Body("body", profileImportModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			body := c.Object("body")
			archive, _ := body.GetOr("archive", nil).(string)
			archive = pystr.Strip(archive)
			if archive == "" {
				return nil, NewHTTPException(400, "archive path is required")
			}
			importName := ""
			if n := optField(body, "name"); n != nil {
				importName = pystr.Strip(*n)
			}
			profileDir, err := manage.ImportProfile(archive, importName)
			if err := profileErrors(err, "POST /api/profiles/import failed", profileNotFoundDefault,
				[]profileExc{excValue, excFileExists}); err != nil {
				return nil, err
			}
			imported := filepath.Base(profileDir)
			profileBestEffort(fmt.Sprintf("Creating wrapper for imported profile %s failed", imported), struct{}{}, func() (struct{}, error) {
				collision, err := profiles.CheckAliasCollision(imported)
				if err != nil || collision != "" {
					return struct{}{}, err
				}
				_, err = profiles.CreateWrapperScript(imported, "")
				return struct{}{}, err
			})
			var desktopOverlay any
			if isFilePath(filepath.Join(profileDir, "desktop.json")) {
				desktopOverlay = profileBestEffort[any](fmt.Sprintf("Reading desktop.json from imported profile %s failed", imported), nil,
					func() (any, error) { return readDesktopOverlay(profileDir) })
			}
			return pyjson.ObjectOf("ok", true, "name", imported, "path", profileDir, "desktop", desktopOverlay), nil
		},
	})
	r.Get("/api/profiles/{name}/desktop-overlay", &Endpoint{
		Name:   "get_profile_desktop_overlay",
		Params: []Param{PathParam("name", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			dir, err := resolveProfileDir(c.Str("name"))
			if err != nil {
				return nil, err
			}
			if !isFilePath(filepath.Join(dir, "desktop.json")) {
				return pyjson.ObjectOf("exists", false, "desktop", nil), nil
			}
			overlay, err := readDesktopOverlay(dir)
			if err != nil {
				return nil, NewHTTPException(500, "Could not read desktop.json: "+pyErrText(err))
			}
			return pyjson.ObjectOf("exists", true, "desktop", overlay), nil
		},
	})
	return r
}
