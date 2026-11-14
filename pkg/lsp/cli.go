package lsp

import (
	"context"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Help and description of the icebath lsp command
const (
	CommandHelp        = "Language Server Protocol management"
	CommandDescription = "Manage the LSP layer that powers post-write semantic diagnostics in write_file/patch."
)

// ArgSpec is the one optional argument of a subcommand, a store true flag or a positional
type ArgSpec struct {
	Name string
	Help string
	Flag bool
}

// SubcommandSpec is one icebath lsp subcommand in help order
type SubcommandSpec struct {
	Name string
	Help string
	Arg  *ArgSpec
}

// Subcommands lists the icebath lsp subcommands in help order
var Subcommands = []SubcommandSpec{
	{"status", "Show LSP service status", &ArgSpec{"--json", "Emit machine-readable JSON", true}},
	{"list", "List supported language servers", &ArgSpec{"--installed-only", "Only show servers whose binary is currently available", true}},
	{"install", "Install a server binary", &ArgSpec{"server", "Server id (e.g. pyright, gopls)", false}},
	{"install-all", "Install every server with a known auto-install recipe",
		&ArgSpec{"--include-manual", "Even attempt servers marked manual-install (best effort)", true}},
	{"restart", "Tear down running LSP clients (next edit re-spawns)", nil},
	{"which", "Print binary path for a server", &ArgSpec{"server", "Server id", false}},
}

// CommandArgs are the parsed arguments of icebath lsp
type CommandArgs struct {
	Subcommand    string
	JSON          bool
	InstalledOnly bool
	IncludeManual bool
	Server        string
}

// CommandContext is what the command reads from the running profile
type CommandContext struct {
	Runtime *Runtime
	Config  func() map[string]any
	Service func() (*Service, error)
}

var statusMarkers = map[string]string{"installed": "✓", "missing": "·", "manual-only": "?"}

// Server id to install recipe key where the two differ
var recipeAliases = map[string]string{
	"vue-language-server":   "@vue/language-server",
	"astro-language-server": "@astrojs/language-server",
	"dockerfile-ls":         "dockerfile-language-server-nodejs",
	"typescript":            "typescript-language-server",
}

func recipePkgFor(serverID string) string {
	if a, ok := recipeAliases[serverID]; ok {
		return a
	}
	return serverID
}

// RunCommand runs one icebath lsp subcommand and returns its exit code, 130 when ctx is interrupted first
func RunCommand(ctx context.Context, cc CommandContext, args CommandArgs, stdout, stderr io.Writer) int {
	sub := args.Subcommand
	if sub == "" {
		sub = "status"
	}
	handlers := map[string]func() (int, error){
		"status":      func() (int, error) { return cmdStatus(cc, args.JSON, stdout) },
		"list":        func() (int, error) { return cmdList(cc, args.InstalledOnly, stdout) },
		"install":     func() (int, error) { return cmdInstall(cc, args.Server, stdout, stderr) },
		"install-all": func() (int, error) { return cmdInstallAll(cc, args.IncludeManual, stdout) },
		"restart":     func() (int, error) { return cmdRestart(stdout) },
		"which":       func() (int, error) { return cmdWhich(cc, args.Server, stdout, stderr) },
	}
	handler, ok := handlers[sub]
	if !ok {
		fmt.Fprintf(stderr, "unknown lsp subcommand: %s\n", sub)
		return 2
	}
	type outcome struct {
		code int
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		code, err := handler()
		done <- outcome{code, err}
	}()
	select {
	case o := <-done:
		if o.err != nil {
			fmt.Fprintf(stderr, "%s: %s\n", errTypeName(o.err), errText(o.err))
			return 1
		}
		return o.code
	case <-ctx.Done():
		return 130
	}
}

// Config declared servers ahead of the built in registry
func allServers(cc CommandContext) []*ServerDef {
	var cfg map[string]any
	if cc.Config != nil {
		cfg = cc.Config()
	}
	return append(CustomServers(cc.Runtime, cfg["servers"]), Servers...)
}

func isBuiltin(s *ServerDef) bool {
	for _, b := range Servers {
		if b == s {
			return true
		}
	}
	return false
}

// Install status of a server, a custom one installed when its command resolves
func statusFor(cc CommandContext, serverID string) (string, error) {
	for _, s := range allServers(cc) {
		if s.ServerID == serverID && !isBuiltin(s) {
			cwd, err := pypath.Getcwd()
			if err != nil {
				return "", pyOSError(err, "")
			}
			spec, err := s.BuildSpawn(cwd, ServerContext{WorkspaceRoot: cwd, InstallStrategy: "manual", Runtime: cc.Runtime})
			if err != nil {
				return "", err
			}
			if spec != nil {
				return "installed", nil
			}
			return "manual-only", nil
		}
	}
	return DetectStatus(cc.Runtime, recipePkgFor(serverID))
}

// Python str of a two element tuple of strings
func tupleRepr(t [2]string) string {
	return "(" + pyrepr.StrRepr(t[0]) + ", " + pyrepr.StrRepr(t[1]) + ")"
}

// Left justified to a width in code points, as a str format spec pads
func ljust(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

func cmdStatus(cc CommandContext, emitJSON bool, stdout io.Writer) (int, error) {
	servers := allServers(cc)
	var svc *Service
	if cc.Service != nil {
		var err error
		if svc, err = cc.Service(); err != nil {
			return 0, err
		}
	}
	var st Status
	if svc != nil {
		st = svc.GetStatus()
	}
	if emitJSON {
		var info jsonObj
		if svc != nil {
			info = statusJSON(st)
		} else {
			info = jsonObj{{"enabled", false}}
		}
		registry := []any{}
		for _, s := range servers {
			status, err := statusFor(cc, s.ServerID)
			if err != nil {
				return 0, err
			}
			exts := make([]any, len(s.Extensions))
			for i, e := range s.Extensions {
				exts[i] = e
			}
			registry = append(registry, jsonObj{{"server_id", s.ServerID}, {"extensions", exts},
				{"description", s.Description}, {"binary_status", status}})
		}
		fmt.Fprint(stdout, pyJSONDumps(jsonObj{{"service", info}, {"registry", registry}}, 2)+"\n")
		return 0, nil
	}
	out := []string{"LSP Service", "===========", "  enabled:         " + pyrepr.Str(svc != nil && st.Enabled)}
	if svc != nil {
		out = append(out, "  wait_mode:       "+st.WaitMode,
			"  wait_timeout:    "+pyrepr.Float(st.WaitTimeout)+"s",
			"  install_strategy:"+st.InstallStrategy)
		if len(st.Clients) > 0 {
			out = append(out, fmt.Sprintf("  active clients:  %d", len(st.Clients)))
		} else {
			out = append(out, "  active clients:  none")
		}
		for _, c := range st.Clients {
			out = append(out, "    - "+ljust(c.ServerID, 20)+" state="+ljust(c.State, 10)+" root="+c.WorkspaceRoot)
		}
		if len(st.Broken) > 0 {
			out = append(out, fmt.Sprintf("  broken pairs:    %d", len(st.Broken)))
			for _, b := range st.Broken {
				out = append(out, "    - "+tupleRepr(b))
			}
		}
		if len(st.DisabledServers) > 0 {
			out = append(out, "  disabled in cfg: "+strings.Join(st.DisabledServers, ", "))
		}
		if len(st.UntrustedSkipped) > 0 {
			out = append(out, fmt.Sprintf("  untrusted skips: %d (add the workspace to lsp.trusted_workspaces)", len(st.UntrustedSkipped)))
			for _, u := range st.UntrustedSkipped {
				out = append(out, "    - "+ljust(u[0], 20)+" root="+u[1])
			}
		}
	}
	warnings, err := backendWarnings(cc)
	if err != nil {
		return 0, err
	}
	if len(warnings) > 0 {
		out = append(out, "", "Backend warnings", "================")
		for _, w := range warnings {
			out = append(out, "  ! "+w)
		}
	}
	out = append(out, "", "Registered Servers", "==================")
	for _, s := range servers {
		status, err := statusFor(cc, s.ServerID)
		if err != nil {
			return 0, err
		}
		shown := s.Extensions
		if len(shown) > 5 {
			shown = shown[:5]
		}
		extSummary := strings.Join(shown, ", ")
		if len(s.Extensions) > 5 {
			extSummary += fmt.Sprintf(", … (+%d)", len(s.Extensions)-5)
		}
		marker, ok := statusMarkers[status]
		if !ok {
			marker = " "
		}
		out = append(out, "  "+marker+" "+ljust(s.ServerID, 24)+" ["+ljust(status, 11)+"] "+extSummary)
		if s.Description != "" {
			gate := "  [trusted workspaces only]"
			if UntrustedSafeServers[s.ServerID] {
				gate = ""
			}
			out = append(out, "      "+s.Description+gate)
		}
	}
	fmt.Fprint(stdout, strings.Join(out, "\n")+"\n")
	return 0, nil
}

// Status snapshot in get_status key order
func statusJSON(st Status) jsonObj {
	clients := []any{}
	for _, c := range st.Clients {
		folders := make([]any, len(c.WorkspaceFolders))
		for i, f := range c.WorkspaceFolders {
			folders[i] = f
		}
		clients = append(clients, jsonObj{{"server_id", c.ServerID}, {"workspace_root", c.WorkspaceRoot},
			{"workspace_folders", folders}, {"state", c.State}, {"running", c.Running}})
	}
	pairs := func(ps [][2]string) []any {
		out := []any{}
		for _, p := range ps {
			out = append(out, []any{p[0], p[1]})
		}
		return out
	}
	strs := func(ss []string) []any {
		out := []any{}
		for _, s := range ss {
			out = append(out, s)
		}
		return out
	}
	var exclude any = strs(st.ExcludeRoots)
	if st.ExcludeRootsInvalid {
		exclude = "INVALID"
	}
	var installStrategy any = st.InstallStrategy
	if st.InstallStrategySet {
		installStrategy = st.InstallStrategyRaw
	}
	return jsonObj{
		{"enabled", st.Enabled}, {"wait_mode", st.WaitMode}, {"wait_timeout", st.WaitTimeout},
		{"install_strategy", installStrategy}, {"clients", clients}, {"broken", pairs(st.Broken)},
		{"disabled_servers", strs(st.DisabledServers)}, {"broken_retry_seconds", st.BrokenRetrySeconds},
		{"warmup_timeout", st.WarmupTimeout}, {"exclude_roots", exclude},
		{"trusted_workspaces", strs(st.TrustedWorkspaces)}, {"untrusted_skipped", pairs(st.UntrustedSkipped)},
	}
}

func cmdList(cc CommandContext, installedOnly bool, stdout io.Writer) (int, error) {
	for _, s := range allServers(cc) {
		status, err := statusFor(cc, s.ServerID)
		if err != nil {
			return 0, err
		}
		if !(installedOnly && status != "installed") {
			fmt.Fprintf(stdout, "%s [%s] %s\n", ljust(s.ServerID, 24), ljust(status, 11), strings.Join(s.Extensions, ","))
		}
	}
	return 0, nil
}

func cmdInstall(cc CommandContext, serverID string, stdout, stderr io.Writer) (int, error) {
	pkg := recipePkgFor(serverID)
	status, err := statusFor(cc, serverID)
	if err != nil {
		return 0, err
	}
	if status == "installed" {
		fmt.Fprintf(stdout, "%s already installed\n", serverID)
		return 0, nil
	}
	fmt.Fprintf(stdout, "installing %s (pkg=%s) ...\n", serverID, pkg)
	binPath, err := TryInstall(cc.Runtime, pkg, "auto")
	if err != nil {
		return 0, err
	}
	if binPath != "" {
		fmt.Fprintf(stdout, "installed: %s\n", binPath)
		return 0, nil
	}
	if r, ok := InstallRecipes[pkg]; ok && r.Strategy == "manual" {
		fmt.Fprintf(stderr, "%s: this server requires a manual install. See documentation.\n", serverID)
	} else {
		fmt.Fprintf(stderr, "%s: install failed (see logs).\n", serverID)
	}
	return 1, nil
}

func cmdInstallAll(cc CommandContext, includeManual bool, stdout io.Writer) (int, error) {
	rc := 0
	for _, s := range Servers {
		pkg := recipePkgFor(s.ServerID)
		recipe, ok := InstallRecipes[pkg]
		if !ok || (recipe.Strategy == "manual" && !includeManual) {
			continue
		}
		status, err := statusFor(cc, s.ServerID)
		if err != nil {
			return 0, err
		}
		if status == "installed" {
			fmt.Fprintf(stdout, "  %s already installed\n", ljust(s.ServerID, 24))
			continue
		}
		fmt.Fprintf(stdout, "  installing %s (pkg=%s) ... ", s.ServerID, pkg)
		path, err := TryInstall(cc.Runtime, pkg, "auto")
		if err != nil {
			return 0, err
		}
		if path != "" {
			fmt.Fprintf(stdout, "ok (%s)\n", path)
		} else {
			fmt.Fprint(stdout, "FAILED\n")
			rc = 1
		}
	}
	return rc, nil
}

func cmdRestart(stdout io.Writer) (int, error) {
	ShutdownServices()
	fmt.Fprint(stdout, "LSP service shut down. Next edit will respawn clients.\n")
	return 0, nil
}

func cmdWhich(cc CommandContext, serverID string, stdout, stderr io.Writer) (int, error) {
	resolved, err := ExistingBinary(cc.Runtime, recipeBin(serverID))
	if err != nil {
		return 0, err
	}
	if resolved != "" {
		fmt.Fprint(stdout, resolved+"\n")
		return 0, nil
	}
	fmt.Fprintf(stderr, "%s: not installed\n", serverID)
	return 1, nil
}

// Sidecar gaps that make a server spawn fine but emit nothing
func backendWarnings(cc CommandContext) ([]string, error) {
	bash, err := ExistingBinary(cc.Runtime, "bash-language-server")
	if err != nil {
		return nil, err
	}
	if bash != "" && pyWhich("shellcheck") == "" {
		return []string{"bash-language-server is installed but shellcheck is missing — " +
			"diagnostics will be empty (apt: shellcheck, brew: shellcheck, scoop: shellcheck)."}, nil
	}
	return nil, nil
}

// One key of an insertion ordered JSON object
type jsonKV struct {
	key string
	val any
}

// Insertion ordered JSON object
type jsonObj []jsonKV

// Python json.dumps with an indent and ensure_ascii
func pyJSONDumps(v any, indent int) string {
	var b strings.Builder
	writeJSON(&b, v, indent, 0)
	return b.String()
}

func writeJSON(b *strings.Builder, v any, indent, level int) {
	pad := func(l int) string { return "\n" + strings.Repeat(" ", indent*l) }
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int:
		fmt.Fprint(b, t)
	case int64:
		fmt.Fprint(b, t)
	case float64:
		switch s := pyrepr.Float(t); s {
		case "inf":
			b.WriteString("Infinity")
		case "-inf":
			b.WriteString("-Infinity")
		case "nan":
			b.WriteString("NaN")
		default:
			b.WriteString(s)
		}
	case string:
		writeJSONString(b, t)
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[")
		for i, e := range t {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(pad(level + 1))
			writeJSON(b, e, indent, level+1)
		}
		b.WriteString(pad(level) + "]")
	case jsonObj:
		if len(t) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{")
		for i, kv := range t {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(pad(level + 1))
			writeJSONString(b, kv.key)
			b.WriteString(": ")
			writeJSON(b, kv.val, indent, level+1)
		}
		b.WriteString(pad(level) + "}")
	default:
		writeJSONString(b, pyrepr.Str(t))
	}
}

// JSON string with every character outside printable ASCII escaped
func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r >= ' ' && r <= '~':
				b.WriteRune(r)
			case r > 0xFFFF:
				r -= 0x10000
				fmt.Fprintf(b, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
			default:
				fmt.Fprintf(b, `\u%04x`, r)
			}
		}
	}
	b.WriteByte('"')
}
