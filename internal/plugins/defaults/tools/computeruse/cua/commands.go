package cua

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	cuconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/proc"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Why the profile's computer_use.backend names no provider there is, nil for the built-in one
func Unknown(cfg *config.Config) error {
	name := strings.TrimSpace(cfg.GetString(cuconfig.Backend))
	if name == "" || name == cuconfig.Cua {
		return nil
	}
	return fmt.Errorf("computer_use.backend is '%s', but that computer-use provider is not installed (available: %s). Pick a backend in /settings "+
		"under Computer use.", name, cuconfig.Cua)
}

// One probe of the driver's own doctor
type Probe struct {
	Label   string `json:"label"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// How ready computer use is on this machine, as the permissions command and the status show it
type Status struct {
	Platform                  string  `json:"platform"`
	PlatformSupported         bool    `json:"platform_supported"`
	Installed                 bool    `json:"installed"`
	Version                   *string `json:"version"`
	Ready                     *bool   `json:"ready"`
	CanGrant                  bool    `json:"can_grant"`
	Checks                    []Probe `json:"checks"`
	Source                    any     `json:"source"`
	Error                     *string `json:"error"`
	Accessibility             *bool   `json:"accessibility"`
	ScreenRecording           *bool   `json:"screen_recording"`
	ScreenRecordingCapturable *bool   `json:"screen_recording_capturable"`
}

// Readiness of the profile's provider: for the driver its version, its doctor's probes, and on macOS its privacy grants
func (d *Driver) Status(ctx context.Context, cfg *config.Config, given string) (*Status, error) {
	st := &Status{Platform: runtime.GOOS, PlatformSupported: Supported(), CanGrant: darwin, Checks: []Probe{}}
	if err := Unknown(cfg); err != nil {
		text := err.Error()
		st.PlatformSupported, st.CanGrant, st.Error = true, false, &text
		return st, nil
	}
	binary, err := d.Command(given)
	if err != nil {
		return nil, err
	}
	if st.Installed = binary != ""; !st.Installed {
		return st, nil
	}
	if out, err := d.run(ctx, cfg, binary, 5*time.Second, "--version"); err == nil {
		if v := strings.TrimSpace(out.stdout); v != "" {
			st.Version = &v
		}
	}
	ok, probes, found := d.doctorProbes(ctx, cfg, binary)
	if found {
		st.Checks = probes
	}
	switch {
	case darwin:
		d.grants(ctx, cfg, binary, st)
		if st.Error == nil {
			ready := st.Accessibility != nil && *st.Accessibility && st.ScreenRecording != nil && *st.ScreenRecording
			st.Ready = &ready
		}
	case found:
		st.Ready = &ok
	}
	return st, nil
}

// Verdict and probes of `cua-driver doctor --json`, found false when it gave none
func (d *Driver) doctorProbes(ctx context.Context, cfg *config.Config, binary string) (bool, []Probe, bool) {
	out, err := d.run(ctx, cfg, binary, 12*time.Second, "doctor", "--json")
	if err != nil {
		return false, nil, false
	}
	obj := object(out.stdout)
	if obj == nil {
		return false, nil, false
	}
	probes := []Probe{}
	listed, _ := obj["probes"].([]any)
	for _, raw := range listed {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		probes = append(probes, Probe{Label: textField(p, "label"), Status: textField(p, "status"), Message: textField(p, "message")})
	}
	return config.Truthy(obj["ok"]), probes, true
}

func textField(m map[string]any, key string) string {
	if v, ok := m[key]; ok && v != nil {
		return show(v)
	}
	return ""
}

// The macOS grants `cua-driver permissions status --json` reports
func (d *Driver) grants(ctx context.Context, cfg *config.Config, binary string, st *Status) {
	out, err := d.run(ctx, cfg, binary, 10*time.Second, "permissions", "status", "--json")
	if err != nil {
		text := "cua-driver permissions status failed: " + err.Error()
		if _, timedOut := errors.AsType[*timeoutError](err); timedOut {
			text = "cua-driver permissions status timed out"
		}
		st.Error = &text
		return
	}
	obj := object(out.stdout)
	for key, target := range map[string]**bool{"accessibility": &st.Accessibility, "screen_recording": &st.ScreenRecording,
		"screen_recording_capturable": &st.ScreenRecordingCapturable} {
		if v, ok := obj[key].(bool); ok {
			*target = &v
		}
	}
	if source, ok := obj["source"].(map[string]any); ok {
		st.Source = source
	}
}

// Rows of the grants macOS keeps for the driver, by the field of the status that reports them
var tcc = []struct{ field, service string }{{"accessibility", "Accessibility"}, {"screen_recording", "ScreenCapture"}}

// How to reset grants System Settings shows on that no longer match the installed driver's signature
func staleGrantHint(missing ...string) string {
	var resets []string
	for _, f := range missing {
		for _, row := range tcc {
			if row.field == f {
				resets = append(resets, "tccutil reset "+row.service+" "+bundleID)
			}
		}
	}
	if len(resets) == 0 {
		return ""
	}
	return "If System Settings already shows CuaDriver ON, the stored grant is stale (it no longer matches the installed driver's signature): run `" +
		strings.Join(resets, " && ") + "`, then `icebath computer-use permissions grant`."
}

// Says what a provider other than the driver is, ending the command, false for the driver
func unknownProvider(cfg *config.Config, out io.Writer) (int, bool) {
	if err := Unknown(cfg); err != nil {
		fmt.Fprintln(out, "Computer Use: "+err.Error())
		return 2, true
	}
	return 0, false
}

// `computer-use install`: installs or repairs the pinned driver
func (d *Driver) InstallCommand(ctx context.Context, cfg *config.Config, p Printer) (int, error) {
	ok, err := d.Install(ctx, cfg, p, true)
	if err != nil || !ok {
		return 1, err
	}
	return 0, nil
}

// `computer-use status`: the driver selected, where it came from, its contract, and on Linux the serve daemons that are not listening
func (d *Driver) StatusCommand(ctx context.Context, cfg *config.Config, out io.Writer) (int, error) {
	if code, ended := unknownProvider(cfg, out); ended {
		return code, nil
	}
	binary, err := d.Command("")
	if err != nil {
		return 0, err
	}
	override := d.Override()
	if binary == "" {
		fmt.Fprintln(out, "cua-driver: not installed")
		fmt.Fprintln(out, "  Run: icebath computer-use install")
		return 1, nil
	}
	c, err := d.Contract(ctx, cfg, binary)
	if err != nil {
		return 0, err
	}
	line := "cua-driver: installed at " + binary
	if override != "" {
		line += " [custom binary from " + cuconfig.DriverCommandEnv + "]"
	}
	if version := firstLine(c.Version, 120); version != "" {
		line += " (" + version + ")"
	}
	fmt.Fprintln(out, line)
	if !c.Ready {
		fmt.Fprintln(out, "  ⚠ Repair required: "+cmp.Or(c.Reason, "runtime contract is incomplete"))
		if override != "" {
			fmt.Fprintln(out, "    Update the binary selected by "+cuconfig.DriverCommandEnv+", or unset the override and run: icebath computer-use install --upgrade")
		} else {
			fmt.Fprintln(out, "    Run: icebath computer-use install")
		}
		return 1, nil
	}
	code := 0
	if linux {
		for _, u := range units("") {
			if !u.serve {
				continue
			}
			if listening, known := d.listening(ctx, cfg, binary, u.socket); known && !listening {
				where := cmp.Or(u.socket, "the default socket")
				fmt.Fprintf(out, "  ✗ Daemon unit %s: no `cua-driver serve` is listening on %s\n", u.unit, where)
				fmt.Fprintf(out, "    Check: systemctl --user status %s  (reinstalling the driver does not start it)\n", u.unit)
				code = 1
			}
		}
	}
	if override != "" {
		fmt.Fprintln(out, "  ✓ Runtime contract ready (externally managed).")
	} else {
		fmt.Fprintln(out, "  ✓ Runtime contract ready (Icebath PM pin).")
	}
	return code, nil
}

// First line of text that is not blank, cut to the limit
func firstLine(text string, limit int) string {
	for _, line := range strings.Split(text, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return strs.Head(s, limit)
		}
	}
	return ""
}

// Whether a serve daemon answers on the socket, unknown when the probe itself failed
func (d *Driver) listening(ctx context.Context, cfg *config.Config, binary, socket string) (bool, bool) {
	args := []string{"status"}
	if socket != "" {
		args = append(args, "--socket", socket)
	}
	out, err := d.run(ctx, cfg, binary, 3*time.Second, args...)
	switch {
	case err != nil:
		return false, false
	case out.code == 0:
		return true, true
	case strings.Contains(strings.ToLower(out.stdout+"\n"+out.stderr), "not running"):
		return false, true
	}
	return false, false
}

func glyph(v *bool) string {
	switch {
	case v == nil:
		return "•"
	case *v:
		return "✅"
	}
	return "❌"
}

// `computer-use permissions status`: the grants of the provider as JSON or as a checklist
func (d *Driver) PermissionsCommand(ctx context.Context, cfg *config.Config, out io.Writer, asJSON bool) (int, error) {
	if !asJSON {
		if code, ended := unknownProvider(cfg, out); ended {
			return code, nil
		}
	}
	st, err := d.Status(ctx, cfg, "")
	if err != nil {
		return 0, err
	}
	ready := st.Ready != nil && *st.Ready
	code := 1
	if ready {
		code = 0
	}
	if asJSON {
		text, err := sortedJSON(st)
		if err != nil {
			return 0, err
		}
		fmt.Fprintln(out, text)
		return code, nil
	}
	if !st.PlatformSupported {
		fmt.Fprintln(out, "Computer Use is not supported on "+st.Platform+".")
		return 1, nil
	}
	if !st.Installed {
		fmt.Fprintln(out, "cua-driver: not installed. Run: icebath computer-use install")
		return 1, nil
	}
	version := "installed"
	if st.Version != nil {
		version = *st.Version
	}
	fmt.Fprintf(out, "cua-driver: %s (%s)\n", version, st.Platform)
	if st.CanGrant {
		fmt.Fprintf(out, "  %s Accessibility\n", glyph(st.Accessibility))
		fmt.Fprintf(out, "  %s Screen Recording\n", glyph(st.ScreenRecording))
		if !ready {
			fmt.Fprintln(out, "  Grant: icebath computer-use permissions grant")
			var missing []string
			for field, v := range map[string]*bool{"accessibility": st.Accessibility, "screen_recording": st.ScreenRecording} {
				if v != nil && !*v {
					missing = append(missing, field)
				}
			}
			if hint := staleGrantHint(sortedStrings(missing)...); hint != "" {
				fmt.Fprintln(out, "  "+hint)
			}
		}
	} else {
		fmt.Fprintf(out, "  %s driver health (no permission toggles on %s)\n", glyph(st.Ready), st.Platform)
	}
	for _, c := range st.Checks {
		if c.Status != "ok" {
			fmt.Fprintf(out, "  ⚠ %s: %s\n", c.Label, c.Message)
		}
	}
	if st.Error != nil {
		fmt.Fprintln(out, "  ⚠ "+*st.Error)
	}
	return code, nil
}

func sortedStrings(items []string) []string {
	out := []string{}
	for _, f := range []string{"accessibility", "screen_recording"} {
		for _, item := range items {
			if item == f {
				out = append(out, f)
			}
		}
	}
	return out
}

// Value as indented JSON with its keys sorted
func sortedJSON(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return "", err
	}
	text, err := json.MarshalIndent(generic, "", "  ")
	return string(text), err
}

// `computer-use permissions grant`: asks macOS for the grants, the dialog attributed to the driver
func (d *Driver) GrantCommand(ctx context.Context, cfg *config.Config, out, errOut io.Writer) (int, error) {
	if code, ended := unknownProvider(cfg, out); ended {
		return code, nil
	}
	if !darwin {
		fmt.Fprintln(out, "Computer Use permissions are a macOS concept; nothing to grant here.")
		return 64, nil
	}
	binary, err := d.Command("")
	if err != nil {
		return 0, err
	}
	if binary == "" {
		fmt.Fprintln(out, "cua-driver: not installed. Run: icebath computer-use install")
		return 2, nil
	}
	fmt.Fprintln(out, "Requesting Accessibility + Screen Recording for CuaDriver.\nmacOS will show a dialog attributed to CuaDriver ("+bundleID+
		") — approve it, then return here.")
	cmd := exec.CommandContext(ctx, binary, "permissions", "grant")
	cmd.Env = terminal.Environ(d.Env(cfg))
	cmd.Stdout, cmd.Stderr = out, errOut
	err = cmd.Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return proc.ExitCode(exit.ProcessState), nil
	}
	if err != nil {
		fmt.Fprintf(errOut, "cua-driver permissions grant failed: %s\n", err)
		return 2, nil
	}
	return 0, nil
}
