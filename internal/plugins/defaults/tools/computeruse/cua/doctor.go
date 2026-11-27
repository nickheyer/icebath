package cua

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	cuconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

var (
	// Mark of a check by its status, a skipped one said in words
	statusMark = map[string]string{"pass": "✓", "fail": "✗"}
	// Mark and words of the whole report by its verdict
	overallMark  = map[string]string{"ok": "✓", "degraded": "⚠", "failed": "✗"}
	overallSaid  = map[string]string{"ok": "works", "degraded": "works only in part", "failed": "does not work"}
	versionRe    = regexp.MustCompile(`(\p{Nd}+\.\p{Nd}+\.\p{Nd}+(?:[-+][\p{L}\p{N}_.]+)?)`)
	versionToken = regexp.MustCompile(`(\p{Nd}+\.\p{Nd}+(?:\.\p{Nd}+)?(?:[-+][\p{L}\p{N}_.]+)?)`)
	// Check of each grant, by the field of check_permissions it reads
	tccChecks = map[string]string{"tcc_accessibility": "accessibility", "tcc_screen_recording": "screen_recording"}
)

const (
	tccHint         = "Grant %s to CuaDriver in System Settings › Privacy & Security."
	zeroDisplayMsg  = "macOS can capture the screen, but it has no display to share, so every screenshot comes out empty."
	zeroDisplayHint = "Wake the built-in display, connect a monitor or a dummy HDMI plug such as Headless Ghost, or turn on a virtual display with " +
		"Screen Sharing, VNC or BetterDisplay. Check the displays with system_profiler SPDisplaysDataType."
	prunedUnitMsg  = "It runs %s, which an update of cua-driver removed."
	prunedUnitHint = "Point the %s at ~/.cua-driver/packages/current/cua-driver, which stays in place across updates. The folders of older " +
		"versions are removed, and only the last 5 are kept."
	deadDaemonMsg  = "%s should run cua-driver serve, but nothing answers on %s, so the service crashed, was stopped or never started."
	deadDaemonHint = "Check it with systemctl --user status %s or journalctl --user -u %s. Installing the driver again neither starts the " +
		"service nor repairs it."
)

// A health_report the driver denied or that is no real report, which a report composed from probes stands in for
type unavailable struct{ msg string }

func (e *unavailable) Error() string { return e.msg }

// One check of a report
type check = map[string]any

// An MCP exchange of the doctor that failed
type exchangeError struct{ msg string }

func (e *exchangeError) Error() string { return e.msg }

func validReport(v any) bool {
	o, ok := v.(map[string]any)
	if !ok {
		return false
	}
	_, schema := o["schema_version"]
	_, overall := o["overall"]
	_, checks := o["checks"].([]any)
	return schema && overall && checks
}

// Text of each text part of a tools/call result
func texts(result map[string]any) []string {
	var out []string
	content, _ := result["content"].([]any)
	for _, raw := range content {
		if part, ok := raw.(map[string]any); ok && part["type"] == "text" {
			out = append(out, textField(part, "text"))
		}
	}
	return out
}

func firstText(result map[string]any, def string) string {
	for _, t := range texts(result) {
		if s := strings.TrimSpace(t); s != "" {
			return s
		}
	}
	return def
}

func keysOf(m map[string]any) string {
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	})
	return strings.Join(keys, ", ")
}

// Report of a health_report result, unavailable when it was refused or carries no report
func reportOf(result map[string]any) (map[string]any, error) {
	if result["isError"] == true {
		return nil, &unavailable{firstText(result, "cua-driver refused to give its health report")}
	}
	if validReport(result["structuredContent"]) {
		return result["structuredContent"].(map[string]any), nil
	}
	for _, text := range texts(result) {
		var parsed any
		if json.Unmarshal([]byte(text), &parsed) == nil && validReport(parsed) {
			return parsed.(map[string]any), nil
		}
	}
	if sc, ok := result["structuredContent"].(map[string]any); ok {
		return nil, &unavailable{"the health report of cua-driver lacks the parts icebath reads and holds only " + keysOf(sc)}
	}
	return nil, &exchangeError{"cua-driver answered without a health report icebath can read. Its answer holds only " + keysOf(result)}
}

// One request of an exchange, named in what its failure says
type step func(method string, call func(context.Context) error) error

// Runs fn on a fresh MCP session of the driver's mcp verb, each request answering within the timeout
func (d *Driver) exchange(ctx context.Context, cfg *config.Config, binary string, timeout time.Duration, fn func(c *client, do step) error) error {
	c, err := open(binary, []string{"mcp"}, d.Env(cfg), d.host.Supervisor)
	if err != nil {
		return &spawnError{err}
	}
	defer c.close()
	return fn(c, func(method string, call func(context.Context) error) error {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		switch err := call(ctx); {
		case errors.Is(err, errClosed):
			if lines := c.stderr.last(3); len(lines) > 0 {
				return &exchangeError{fmt.Sprintf("cua-driver stopped without answering “%s”. It last printed: %s", method, strings.Join(lines, " / "))}
			}
			return &exchangeError{fmt.Sprintf("cua-driver stopped without answering “%s” and printed nothing", method)}
		case errors.Is(err, context.DeadlineExceeded):
			return &exchangeError{fmt.Sprintf("cua-driver did not answer “%s” within %s", method, strs.Span(timeout))}
		case err != nil:
			return &exchangeError{fmt.Sprintf("cua-driver refused “%s”: %s", method, err)}
		}
		return nil
	})
}

// The driver's own health_report
func (d *Driver) healthReport(ctx context.Context, cfg *config.Config, binary string, include, skip []string) (map[string]any, error) {
	args := map[string]any{}
	if len(include) > 0 {
		args["include"] = include
	}
	if len(skip) > 0 {
		args["skip"] = skip
	}
	var result map[string]any
	err := d.exchange(ctx, cfg, binary, 12*time.Second, func(c *client, do step) error {
		if err := do("initialize", func(ctx context.Context) error { _, err := c.initialize(ctx); return err }); err != nil {
			return err
		}
		return do("tools/call", func(ctx context.Context) (err error) { result, err = c.call(ctx, "health_report", args); return err })
	})
	if err != nil {
		return nil, err
	}
	return reportOf(result)
}

// What the probes the fallback report is composed from found
type probes struct {
	version     any
	permissions map[string]any
	permErr     string
	appsProbed  bool
	appsOK      bool
	appsErr     string
	apps        *int
}

// One session reading the version initialize gives, check_permissions and a list_apps probe
func (d *Driver) probe(ctx context.Context, cfg *config.Config, binary string) (*probes, error) {
	p := &probes{}
	err := d.exchange(ctx, cfg, binary, 12*time.Second, func(c *client, do step) error {
		var init map[string]any
		if err := do("initialize", func(ctx context.Context) (err error) { init, err = c.initialize(ctx); return err }); err != nil {
			return err
		}
		if info, ok := init["serverInfo"].(map[string]any); ok {
			p.version = info["version"]
		}
		tool := func(name string) (map[string]any, string, error) {
			var result map[string]any
			err := do("tools/call", func(ctx context.Context) (err error) { result, err = c.call(ctx, name, nil); return err })
			if exchange, ok := errors.AsType[*exchangeError](err); ok {
				return nil, exchange.msg, nil
			}
			if err != nil {
				return nil, "", err
			}
			if result["isError"] == true {
				return nil, firstText(result, name+" isError"), nil
			}
			return result, "", nil
		}
		perms, permErr, err := tool("check_permissions")
		if err != nil {
			return err
		}
		p.permErr = permErr
		if perms != nil {
			if p.permissions, _ = perms["structuredContent"].(map[string]any); p.permissions == nil {
				p.permissions = map[string]any{}
			}
		}
		apps, appsErr, err := tool("list_apps")
		if err != nil {
			return err
		}
		p.appsProbed, p.appsOK, p.appsErr = true, apps != nil, appsErr
		if sc, ok := apps["structuredContent"].(map[string]any); ok {
			if list, ok := sc["apps"].([]any); ok {
				n := len(list)
				p.apps = &n
			}
		}
		return nil
	})
	return p, err
}

// Status and version, or why there is none, of the driver's version verb
func (d *Driver) versionCheck(ctx context.Context, cfg *config.Config, binary string) (string, string) {
	out, err := d.run(ctx, cfg, binary, 5*time.Second, "--version")
	if err != nil {
		return "fail", "cua-driver --version failed: " + err.Error() + "."
	}
	text := strings.TrimSpace(out.stdout + out.stderr)
	if out.code != 0 && text == "" {
		return "fail", fmt.Sprintf("cua-driver --version ended with exit code %d.", out.code)
	}
	status := "pass"
	if out.code != 0 {
		status = "fail"
	}
	if m := versionRe.FindStringSubmatch(text); m != nil {
		return status, m[1]
	}
	return status, cmp.Or(firstLine(text, 1<<30), "Unknown")
}

// Row of one grant check_permissions reports
func grantRow(p *probes, platform, field, label string, bound bool) check {
	if p.permissions == nil {
		if p.permErr != "" {
			return check{"status": "fail", "message": sentence(p.permErr)}
		}
		return check{"status": "skip", "message": "cua-driver could not report its permissions."}
	}
	granted, ok := p.permissions[field].(bool)
	if !ok {
		if bound && platform != "darwin" {
			return check{"status": "skip", "message": "Not needed on " + system(platform) + "."}
		}
		return check{"status": "skip", "message": "cua-driver did not report whether " + label + " is granted."}
	}
	if !granted {
		return check{"status": "fail", "message": label + " is not granted.", "hint": fmt.Sprintf(tccHint, label), "data": map[string]any{field: false}}
	}
	data := map[string]any{field: true}
	if field == "screen_recording" {
		data["screen_recording_capturable"] = p.permissions["screen_recording_capturable"]
		if data["screen_recording_capturable"] == false {
			return check{"status": "fail", "message": "Screen Recording is granted, but the screen still cannot be captured.", "data": data,
				"hint": "Restart CuaDriver, or grant Screen Recording again in System Settings."}
		}
	}
	return check{"status": "pass", "message": label + " is granted.", "data": data}
}

// Accessibility capability from the list_apps probe, else from the accessibility grant
func axRow(p *probes) check {
	granted := p.permissions != nil && p.permissions["accessibility"] == true
	switch {
	case p.appsProbed && p.appsOK:
		message := "cua-driver could list the open apps."
		if p.apps != nil {
			message = "cua-driver listed " + strs.Count(*p.apps, "open app") + "."
		}
		return check{"status": "pass", "message": message}
	case p.appsProbed && p.appsErr != "":
		return check{"status": "fail", "message": sentence(p.appsErr)}
	case p.appsProbed:
		message := "cua-driver could not list the open apps."
		if granted {
			message = "cua-driver could not list the open apps, even though Accessibility is granted."
		}
		return check{"status": "fail", "message": message}
	case granted:
		return check{"status": "pass", "message": "Accessibility is granted, so this should work. Listing the open apps was not tried."}
	}
	return check{"status": "skip", "message": "Not checked."}
}

// Report of schema version 1 composed from CLI and MCP probes, for a driver whose health_report was denied
func (d *Driver) fallback(ctx context.Context, cfg *config.Config, binary, reason string) (map[string]any, error) {
	platform := runtime.GOOS
	status, version := d.versionCheck(ctx, cfg, binary)
	p, err := d.probe(ctx, cfg, binary)
	if err != nil {
		return nil, err
	}
	if v := show(p.version); p.version != nil && v != "" {
		status, version = "pass", v
	}
	short := strings.TrimSpace(cmp.Or(reason, "cua-driver gave no health report"))
	if utf8.RuneCountInString(short) > 160 {
		short = strs.Head(short, 159) + "…"
	}
	message := version
	if status == "pass" {
		message = "cua-driver " + version
	} else if message == "" {
		message = "Version unknown."
	}
	platformRow := check{"status": "pass", "message": system(platform) + "."}
	if !slices.Contains([]string{"darwin", "linux", "windows"}, platform) {
		platformRow = check{"status": "fail", "message": "Computer use does not work on " + system(platform) + "."}
	}
	rows := []struct {
		name string
		row  check
	}{
		{"binary_version", check{"status": status, "message": message}},
		{"platform_supported", platformRow},
		{"session_active", check{"status": "skip", "message": "Not checked, as the doctor starts no session of cua-driver."}},
		{"tcc_accessibility", grantRow(p, platform, "accessibility", "Accessibility", false)},
		{"tcc_screen_recording", grantRow(p, platform, "screen_recording", "Screen Recording", true)},
		{"ax_capability", axRow(p)},
		{"health_report_path", check{"status": "skip", "message": "These checks come from icebath itself, as older drivers such as 0.10 give no " +
			"health report it can read. " + sentence(short)}},
	}
	if out, err := d.run(ctx, cfg, binary, 8*time.Second, "doctor"); err == nil {
		if text := strings.TrimSpace(out.stdout + out.stderr); text != "" {
			row := check{"status": "skip", "message": strings.TrimSpace(strings.Split(text, "\n")[0]), "data": map[string]any{"snippet": strs.Head(text, 2000)}}
			if strings.Contains(strings.ToLower(text), "[ok") || strings.Contains(text, "ok  ]") {
				row["status"] = "pass"
			}
			rows = append(rows, struct {
				name string
				row  check
			}{"cli_doctor", row})
		}
	}
	checks := []any{}
	statuses := map[string]any{}
	failed := false
	for _, r := range rows {
		r.row["name"] = r.name
		checks = append(checks, r.row)
		statuses[r.name] = r.row["status"]
		failed = failed || r.row["status"] == "fail"
	}
	overall := "degraded"
	switch tcc := statuses["tcc_accessibility"]; {
	case statuses["binary_version"] != "pass":
		overall = "failed"
	case (tcc == "pass" || tcc == "skip" || tcc == nil) && !failed:
		overall = "ok"
	}
	driver := version
	if status != "pass" && driver == "" {
		driver = "?"
	}
	return map[string]any{"schema_version": "1", "platform": platform, "driver_version": driver, "overall": overall, "checks": checks, "fallback": true,
		"fallback_reason": cmp.Or(reason, "cua-driver gave no health report")}, nil
}

func checksOf(r map[string]any) []map[string]any {
	list, _ := r["checks"].([]any)
	var out []map[string]any
	for _, c := range list {
		if o, ok := c.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

func degrade(r map[string]any) {
	if r["overall"] == "ok" {
		r["overall"] = "degraded"
	}
}

// Fails a passing screen capture check that found no display to capture
func displayGuard(r map[string]any) {
	for _, c := range checksOf(r) {
		data, _ := c["data"].(map[string]any)
		if c["name"] == "screen_capture_capability" && c["status"] == "pass" && (data["display_count"] == 0.0 || data["display_count"] == false) {
			c["status"], c["message"], c["hint"] = "fail", zeroDisplayMsg, zeroDisplayHint
			degrade(r)
		}
	}
}

// Adds to every failed grant check how to reset a stale grant
func staleGrantGuard(r map[string]any) {
	for _, c := range checksOf(r) {
		name, _ := c["name"].(string)
		if field := tccChecks[name]; field != "" && c["status"] == "fail" {
			c["hint"] = strings.TrimSpace(textField(c, "hint") + " " + staleGrantHint(field))
		}
	}
}

// Adds a failed check for each daemon unit whose command an upgrade pruned
func staleUnitGuard(r map[string]any) {
	if !linux {
		return
	}
	for _, u := range staleUnits("") {
		checks, ok := r["checks"].([]any)
		if !ok {
			continue
		}
		r["checks"] = append(checks, map[string]any{"name": "service file " + u.unit, "status": "fail", "message": fmt.Sprintf(prunedUnitMsg, u.exec),
			"hint": fmt.Sprintf(prunedUnitHint, u.kind)})
		degrade(r)
	}
}

// Adds a check of each serve daemon unit's socket on Linux
func (d *Driver) daemonGuard(ctx context.Context, cfg *config.Config, r map[string]any, binary string) {
	checks, ok := r["checks"].([]any)
	if !linux || !ok {
		return
	}
	for _, u := range units("") {
		if !u.serve {
			continue
		}
		listening, known := d.listening(ctx, cfg, binary, u.socket)
		if !known {
			continue
		}
		shown := cmp.Or(u.socket, "the default socket")
		if listening {
			checks = append(checks, map[string]any{"name": "service " + u.unit, "status": "pass", "message": "cua-driver serve answers on " + shown + "."})
			continue
		}
		checks = append(checks, map[string]any{"name": "service " + u.unit, "status": "fail", "message": fmt.Sprintf(deadDaemonMsg, u.unit, shown),
			"hint": fmt.Sprintf(deadDaemonHint, u.unit, u.unit)})
		degrade(r)
	}
	r["checks"] = checks
}

func tokenOf(text string) string {
	if m := versionToken.FindStringSubmatch(text); text != "" && m != nil {
		return m[1]
	}
	return strings.ToLower(strings.TrimSpace(text))
}

// The binary's own version beside the one its health report gives
func (d *Driver) identity(ctx context.Context, cfg *config.Config, binary string, r map[string]any) map[string]any {
	cli := ""
	if out, err := d.run(ctx, cfg, binary, 5*time.Second, "--version"); err == nil {
		cli = firstLine(cmp.Or(out.stdout, out.stderr), 1<<30)
	}
	reported := ""
	if v := r["driver_version"]; config.Truthy(v) {
		reported = show(v)
	}
	tc, tr := tokenOf(cli), tokenOf(reported)
	return map[string]any{"resolved_binary": binary, "cli_version": strs.Nullable(cli), "health_report_driver_version": strs.Nullable(reported),
		"version_mismatch": tc != "" && tr != "" && tc != tr}
}

func or(m map[string]any, k string, def any) any {
	if v, ok := m[k]; ok && v != nil {
		return v
	}
	return def
}

// The report as the driver prints it: identity, environment, then each check with its hint and data
func printReport(w io.Writer, r map[string]any, color bool, identity, environment map[string]any) {
	platform, reported, overall := system(show(or(r, "platform", ""))), or(r, "driver_version", ""), show(or(r, "overall", ""))
	cli := ""
	if v := identity["cli_version"]; config.Truthy(v) {
		cli = show(v)
	}
	red, yellow, green, reset, dim := "", "", "", "", ""
	if _, known := overallMark[overall]; color && known {
		red, yellow, green, reset, dim = "\033[31m", "\033[33m", "\033[32m", "\033[0m", "\033[2m"
	}
	tone := map[string]string{"failed": red, "degraded": yellow, "ok": green}[overall]
	tones := map[string]string{"pass": green, "fail": red, "skip": dim}
	driver := strings.TrimSpace("cua-driver " + cmp.Or(cli, show(reported)))
	if platform != "" {
		driver += " on " + platform
	}
	head := driver + " reports “" + overall + "”."
	if said, known := overallSaid[overall]; known {
		head = overallMark[overall] + " " + driver + " " + said + "."
	}
	lines := []string{tone + head + reset}
	if b := identity["resolved_binary"]; config.Truthy(b) {
		lines = append(lines, fmt.Sprintf("  %sProgram: %s%s", dim, show(b), reset))
	}
	if rv := show(reported); cli != "" && config.Truthy(reported) && !strings.Contains(cli, rv) && !strings.Contains(rv, cli) {
		lines = append(lines, fmt.Sprintf("  %sVersion the program gives: %s%s", dim, cli, reset),
			fmt.Sprintf("  %sVersion its health report gives: %s%s", dim, rv, reset))
	}
	if len(environment) > 0 {
		lines = append(lines, fmt.Sprintf("  %sThese checks ran with the variables of this command, not with those of the icebath server.%s", dim, reset),
			fmt.Sprintf("  %sThe agent's computer use runs in the server, so its results there may differ.%s", dim, reset))
	}
	if identity["version_mismatch"] == true {
		lines = append(lines, fmt.Sprintf("  %s⚠ The health report says version %s, but the program says %s.%s", yellow, show(reported), cli, reset),
			fmt.Sprintf("    %sTrust what the program says. On Windows the health report can lag behind.%s", dim, reset))
	}
	for _, c := range checksOf(r) {
		status := show(or(c, "status", ""))
		message := ""
		if m := c["message"]; config.Truthy(m) {
			message = show(m)
		}
		name := show(or(c, "name", "check"))
		switch mark, marked := statusMark[status]; {
		case marked:
			lines = append(lines, fmt.Sprintf("  %s %s%s%s: %s", mark, tones[status], name, reset, message))
		case status == "skip":
			lines = append(lines, fmt.Sprintf("  %sSkipped %s%s: %s", tones[status], name, reset, message))
		default:
			lines = append(lines, fmt.Sprintf("  %s: %s", name, message))
		}
		if h := c["hint"]; config.Truthy(h) {
			lines = append(lines, fmt.Sprintf("    %s%s%s", dim, show(h), reset))
		}
		if data, ok := c["data"].(map[string]any); ok {
			for _, k := range slices.Sorted(func(yield func(string) bool) {
				for k := range data {
					if !yield(k) {
						return
					}
				}
			}) {
				value := "None"
				switch v := data[k].(type) {
				case nil:
				case bool:
					value = map[bool]string{true: "Yes", false: "No"}[v]
				default:
					value = show(v)
				}
				lines = append(lines, fmt.Sprintf("    %s%s: %s%s", dim, k, value, reset))
			}
		}
	}
	fmt.Fprintln(w, strings.Join(lines, "\n"))
}

// `computer-use doctor`: the driver's health report, or one composed from probes, with the checks icebath adds; 0 when ok, 1 when degraded
// or failed, 2 when the driver is missing or can't be asked
func (d *Driver) Doctor(ctx context.Context, cfg *config.Config, out, errOut io.Writer, include, skip []string, asJSON, color bool) (int, error) {
	if code, ended := unknownProvider(cfg, out); ended {
		return code, nil
	}
	binary, err := d.Command("")
	if err != nil {
		return 0, err
	}
	if binary == "" {
		fmt.Fprintln(out, "✗ cua-driver is not installed. It is not on your PATH, nor where it is usually installed.")
		fmt.Fprintln(out, "  Install it with icebath computer-use install.")
		return 2, nil
	}
	r, err := d.healthReport(ctx, cfg, binary, include, skip)
	if denied, ok := errors.AsType[*unavailable](err); ok {
		r, err = d.fallback(ctx, cfg, binary, denied.msg)
	}
	if spawn, ok := errors.AsType[*spawnError](err); ok {
		fmt.Fprintf(errOut, "✗ cua-driver at %s cannot be started: %v.\n", binary, spawn)
		fmt.Fprintln(errOut, "  The agent's computer use may still work if your shell finds another copy on your PATH.")
		fmt.Fprintln(errOut, "  Install cua-driver outside the protected folder, such as with its own installer under your user folder, or point "+
			cuconfig.DriverCommandEnv+" at a copy that can run.")
		fmt.Fprintln(errOut, "  Then run icebath computer-use doctor again.")
		return 2, nil
	}
	if exchange, ok := errors.AsType[*exchangeError](err); ok {
		fmt.Fprintln(errOut, "✗ cua-driver did not give its health report. "+sentence(exchange.Error()))
		return 2, nil
	}
	if err != nil {
		return 0, err
	}
	displayGuard(r)
	staleGrantGuard(r)
	staleUnitGuard(r)
	d.daemonGuard(ctx, cfg, r, binary)
	identity := d.identity(ctx, cfg, binary, r)
	var environment map[string]any
	if r["platform"] == "linux" && os.Getenv("WAYLAND_DISPLAY") != "" {
		environment = map[string]any{"scope": "cli_process", "gateway_environment_checked": false}
	}
	if asJSON {
		payload := map[string]any{"icebath_identity": identity}
		for k, v := range r {
			payload[k] = v
		}
		if environment != nil {
			payload["icebath_environment"] = environment
		}
		text, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return 0, err
		}
		fmt.Fprintln(out, string(text))
	} else {
		printReport(out, r, color, identity, environment)
	}
	if r["overall"] == "ok" {
		return 0, nil
	}
	return 1, nil
}
