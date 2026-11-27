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
	"github.com/nickheyer/icebath/pkg/strs"
)

var (
	statusGlyph  = map[string]string{"pass": "✅", "fail": "❌", "skip": "⏭️"}
	overallGlyph = map[string]string{"ok": "✅", "degraded": "⚠️", "failed": "❌"}
	versionRe    = regexp.MustCompile(`(\p{Nd}+\.\p{Nd}+\.\p{Nd}+(?:[-+][\p{L}\p{N}_.]+)?)`)
	versionToken = regexp.MustCompile(`(\p{Nd}+\.\p{Nd}+(?:\.\p{Nd}+)?(?:[-+][\p{L}\p{N}_.]+)?)`)
	// Check of each grant, by the field of check_permissions it reads
	tccChecks = map[string]string{"tcc_accessibility": "accessibility", "tcc_screen_recording": "screen_recording"}
)

const (
	tccHint         = "Grant %s to CuaDriver in System Settings → Privacy & Security."
	zeroDisplayMsg  = "ScreenCaptureKit reachable but 0 shareable display(s) — every capture will return 0x0."
	zeroDisplayHint = "Wake the built-in display, connect a monitor or HDMI dummy dongle (e.g. Headless Ghost), or enable a virtual display (Screen " +
		"Sharing/VNC, BetterDisplay). Verify with `system_profiler SPDisplaysDataType`."
	prunedUnitMsg  = "Exec targets %s which no longer exists — a cua-driver upgrade pruned that release directory"
	prunedUnitHint = "Point the %s at ~/.cua-driver/packages/current/cua-driver — `current` survives upgrades; versioned packages/releases/<version>/ " +
		"dirs are pruned (only the last 5 are kept)"
	deadDaemonMsg = "%s is configured to run `cua-driver serve` but no daemon is listening on %s — the unit is not running (crash loop, stopped, or " +
		"never started)"
	deadDaemonHint = "Check `systemctl --user status %s` / `journalctl --user -u %s`; a driver reinstall does not start the daemon and cannot fix a " +
		"broken unit"
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
	return "['" + strings.Join(keys, "', '") + "']"
}

// Report of a health_report result, unavailable when it was refused or carries no report
func reportOf(result map[string]any) (map[string]any, error) {
	if result["isError"] == true {
		return nil, &unavailable{firstText(result, "health_report returned isError=true")}
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
		return nil, &unavailable{"health_report structuredContent lacks schema_version/overall/checks (keys=" + keysOf(sc) + ")"}
	}
	return nil, &exchangeError{"health_report response carried neither structuredContent nor a parseable JSON text block. Result keys: " + keysOf(result)}
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
			tail := "(empty)"
			if lines := c.stderr.last(3); len(lines) > 0 {
				tail = "['" + strings.Join(lines, "', '") + "']"
			}
			return &exchangeError{fmt.Sprintf("cua-driver mcp produced no response for '%s'. stderr tail: %s", method, tail)}
		case errors.Is(err, context.DeadlineExceeded):
			return &exchangeError{fmt.Sprintf("cua-driver mcp did not answer '%s' within %gs", method, timeout.Seconds())}
		case err != nil:
			return &exchangeError{fmt.Sprintf("%s JSON-RPC error: %s", method, err)}
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
		return "fail", "--version failed: " + err.Error()
	}
	text := strings.TrimSpace(out.stdout + out.stderr)
	if out.code != 0 && text == "" {
		return "fail", fmt.Sprintf("--version exited %d", out.code)
	}
	status := "pass"
	if out.code != 0 {
		status = "fail"
	}
	if m := versionRe.FindStringSubmatch(text); m != nil {
		return status, m[1]
	}
	return status, cmp.Or(firstLine(text, 1<<30), "unknown")
}

// Row of one grant check_permissions reports
func grantRow(p *probes, platform, field, label string, bound bool) check {
	if p.permissions == nil {
		if p.permErr != "" {
			return check{"status": "fail", "message": p.permErr}
		}
		return check{"status": "skip", "message": "check_permissions unavailable"}
	}
	granted, ok := p.permissions[field].(bool)
	if !ok {
		if bound && platform != "darwin" {
			return check{"status": "skip", "message": "not applicable on " + platform}
		}
		return check{"status": "skip", "message": field + " field absent from check_permissions"}
	}
	if !granted {
		return check{"status": "fail", "message": label + " is not granted.", "hint": fmt.Sprintf(tccHint, label), "data": map[string]any{field: false}}
	}
	data := map[string]any{field: true}
	if field == "screen_recording" {
		data["screen_recording_capturable"] = p.permissions["screen_recording_capturable"]
		if data["screen_recording_capturable"] == false {
			return check{"status": "fail", "message": "Screen Recording granted but not capturable.", "data": data,
				"hint": "Screen Recording permission may need a restart of CuaDriver or a re-grant in System Settings."}
		}
	}
	return check{"status": "pass", "message": label + " is granted.", "data": data}
}

// Accessibility capability from the list_apps probe, else from the accessibility grant
func axRow(p *probes) check {
	granted := p.permissions != nil && p.permissions["accessibility"] == true
	switch {
	case p.appsProbed && p.appsOK:
		message := "list_apps succeeded"
		if p.apps != nil {
			message += fmt.Sprintf(" (%d apps)", *p.apps)
		}
		return check{"status": "pass", "message": message}
	case p.appsProbed && p.appsErr != "":
		return check{"status": "fail", "message": p.appsErr}
	case p.appsProbed:
		message := "list_apps failed"
		if granted {
			message += " despite accessibility grant"
		}
		return check{"status": "fail", "message": message}
	case granted:
		return check{"status": "pass", "message": "inferred from accessibility grant (list_apps not probed)"}
	}
	return check{"status": "skip", "message": "not probed"}
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
	short := strings.TrimSpace(cmp.Or(reason, "health_report unavailable"))
	if utf8.RuneCountInString(short) > 160 {
		short = strs.Head(short, 157) + "..."
	}
	message := version
	if status == "pass" {
		message = "cua-driver " + version
	} else if message == "" {
		message = "version unknown"
	}
	platformRow := check{"status": "pass", "message": "platform=" + platform}
	if !slices.Contains([]string{"darwin", "linux", "windows"}, platform) {
		platformRow = check{"status": "fail", "message": "platform=" + platform + " (unsupported)"}
	}
	rows := []struct {
		name string
		row  check
	}{
		{"binary_version", check{"status": status, "message": message}},
		{"platform_supported", platformRow},
		{"session_active", check{"status": "skip", "message": "not probed (doctor does not open a cua session)"}},
		{"tcc_accessibility", grantRow(p, platform, "accessibility", "Accessibility", false)},
		{"tcc_screen_recording", grantRow(p, platform, "screen_recording", "Screen Recording", true)},
		{"ax_capability", axRow(p)},
		{"health_report_path", check{"status": "skip", "message": "fallback composite (cua-driver 0.10 unclassified health_report); cause: " + short}},
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
		"fallback_reason": cmp.Or(reason, "health_report unavailable")}, nil
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
		r["checks"] = append(checks, map[string]any{"name": "daemon unit (" + u.unit + ")", "status": "fail", "message": fmt.Sprintf(prunedUnitMsg, u.exec),
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
			checks = append(checks, map[string]any{"name": "daemon (" + u.unit + ")", "status": "pass", "message": "cua-driver serve is listening on " + shown})
			continue
		}
		checks = append(checks, map[string]any{"name": "daemon (" + u.unit + ")", "status": "fail", "message": fmt.Sprintf(deadDaemonMsg, u.unit, shown),
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
	platform, reported, overall := show(or(r, "platform", "?")), or(r, "driver_version", "?"), show(or(r, "overall", "?"))
	cli := ""
	if v := identity["cli_version"]; config.Truthy(v) {
		cli = show(v)
	}
	red, yellow, green, reset, dim := "", "", "", "", ""
	if _, known := overallGlyph[overall]; color && known {
		red, yellow, green, reset, dim = "\033[31m", "\033[33m", "\033[32m", "\033[0m", "\033[2m"
	}
	tone := map[string]string{"failed": red, "degraded": yellow, "ok": green}[overall]
	tones := map[string]string{"pass": green, "fail": red, "skip": dim}
	lines := []string{fmt.Sprintf("%s cua-driver %s on %s — %s%s%s", cmp.Or(overallGlyph[overall], "•"), cmp.Or(cli, show(reported)), platform, tone, overall, reset)}
	if b := identity["resolved_binary"]; config.Truthy(b) {
		lines = append(lines, fmt.Sprintf("  %sbinary: %s%s", dim, show(b), reset))
	}
	if rv := show(reported); cli != "" && config.Truthy(reported) && !strings.Contains(cli, rv) && !strings.Contains(rv, cli) {
		lines = append(lines, fmt.Sprintf("  %s--version: %s%s", dim, cli, reset), fmt.Sprintf("  %shealth_report.driver_version: %s%s", dim, rv, reset))
	}
	if len(environment) > 0 {
		lines = append(lines, fmt.Sprintf("  %senvironment: current CLI process%s", dim, reset),
			fmt.Sprintf("  %sgateway environment was not checked; active gateway computer_use sessions use that process environment%s", dim, reset))
	}
	if identity["version_mismatch"] == true {
		lines = append(lines, fmt.Sprintf("  %s⚠️ version mismatch: health_report says '%s' but binary --version is '%s'%s", yellow, show(reported), cli, reset),
			fmt.Sprintf("  %s→ trust --version / packages/current for debugging; health_report's binary_version check can lag on Windows%s", dim, reset))
	}
	for _, c := range checksOf(r) {
		status := show(or(c, "status", "?"))
		message := ""
		if m := c["message"]; config.Truthy(m) {
			message = show(m)
		}
		lines = append(lines, fmt.Sprintf("  %s %s%s%s: %s", cmp.Or(statusGlyph[status], "•"), tones[status], show(or(c, "name", "?")), reset, message))
		if h := c["hint"]; config.Truthy(h) {
			lines = append(lines, fmt.Sprintf("      → %s%s%s", dim, show(h), reset))
		}
		if data, ok := c["data"].(map[string]any); ok {
			for _, k := range slices.Sorted(func(yield func(string) bool) {
				for k := range data {
					if !yield(k) {
						return
					}
				}
			}) {
				value := "null"
				if data[k] != nil {
					value = show(data[k])
				}
				lines = append(lines, fmt.Sprintf("      %s%s=%s%s", dim, k, value, reset))
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
		fmt.Fprintf(out, "cua-driver: not installed (looked for 'cua-driver (PATH and canonical install paths)').\n  Run: icebath computer-use install\n")
		return 2, nil
	}
	r, err := d.healthReport(ctx, cfg, binary, include, skip)
	if denied, ok := errors.AsType[*unavailable](err); ok {
		r, err = d.fallback(ctx, cfg, binary, denied.msg)
	}
	if spawn, ok := errors.AsType[*spawnError](err); ok {
		fmt.Fprintf(errOut, "cua-driver could not be started from '%s': %v\n  The Icebath runtime cannot execute this binary; the tool may still work "+
			"because the\n  shell resolves a different copy on PATH. Fix: install cua-driver outside the protected directory\n  (e.g. the upstream "+
			"installer's default under your user profile) or point ICEBATH_CUA_DRIVER_CMD at\n  a copy the runtime can execute, then re-run `icebath "+
			"computer-use doctor`.\n", binary, spawn)
		return 2, nil
	}
	if exchange, ok := errors.AsType[*exchangeError](err); ok {
		fmt.Fprintf(errOut, "cua-driver health_report failed: %v\n", exchange)
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
