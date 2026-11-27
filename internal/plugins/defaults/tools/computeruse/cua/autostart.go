package cua

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"

	"github.com/nickheyer/icebath/internal/config"
	cuconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/config"
	"github.com/nickheyer/icebath/pkg/proc"
)

// Name of the Windows logon task that serves the driver
const autostartTask = "cua-driver-serve"

// Exec commands of a scheduled task's XML in any namespace and encoding, false when the bytes don't parse
func taskCommands(raw []byte) ([]string, bool) {
	var r io.Reader = bytes.NewReader(raw)
	switch {
	case bytes.HasPrefix(raw, []byte{0xff, 0xfe}), bytes.HasPrefix(raw, []byte{'<', 0}):
		r = transform.NewReader(r, unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder())
	case bytes.HasPrefix(raw, []byte{0xfe, 0xff}), bytes.HasPrefix(raw, []byte{0, '<'}):
		r = transform.NewReader(r, unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewDecoder())
	case bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}):
		r = bytes.NewReader(raw[3:])
	}
	d := xml.NewDecoder(r)
	d.Strict = true
	d.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		l := strings.ToLower(label)
		if strings.HasPrefix(l, "utf-16") || l == "utf-8" || l == "us-ascii" || l == "ascii" {
			return input, nil
		}
		enc, err := htmlindex.Get(l)
		if err != nil {
			return nil, err
		}
		return transform.NewReader(input, enc.NewDecoder()), nil
	}
	type frame struct {
		name     string
		text     strings.Builder
		children bool
	}
	var stack []*frame
	var commands []string
	root := false
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if len(stack) == 0 {
				if root {
					return nil, false
				}
				root = true
			} else {
				stack[len(stack)-1].children = true
			}
			stack = append(stack, &frame{name: t.Name.Local})
		case xml.CharData:
			if n := len(stack); n > 0 && !stack[n-1].children {
				stack[n-1].text.Write(t)
			}
		case xml.EndElement:
			n := len(stack)
			if top := stack[n-1]; top.name == "Command" && n >= 3 && stack[n-2].name == "Exec" {
				commands = append(commands, top.text.String())
			}
			stack = stack[:n-1]
		}
	}
	return commands, root
}

// Whether the logon task runs this very binary; a task left by another version is not ready
func (d *Driver) autostartRegistered(cfg *config.Config, binary string) bool {
	if !windows || binary == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "schtasks.exe", "/Query", "/TN", autostartTask, "/XML")
	proc.HideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	commands, ok := taskCommands(out)
	if !ok {
		return false
	}
	for _, c := range commands {
		if strings.EqualFold(strings.Trim(strings.TrimSpace(c), `"`), binary) {
			return true
		}
	}
	return false
}

// Literal of PowerShell's single quotes
func psQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// Registers the logon task for the binary when the profile wants it, through an elevated run of the driver; true when nothing needs fixing
func (d *Driver) repairAutostart(ctx context.Context, cfg *config.Config, p Printer, binary string, progress bool) bool {
	if !windows || !cfg.GetBool(cuconfig.Autostart) {
		return true
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return false
	}
	if d.autostartRegistered(cfg, resolved) {
		return true
	}
	ps, err := exec.LookPath("powershell")
	if err != nil {
		if ps, err = exec.LookPath("powershell.exe"); err != nil {
			ps = "powershell"
		}
	}
	if progress {
		p.Info("Setting up cua-driver to start when you sign in to Windows.")
	} else {
		p.Info("Repairing how cua-driver starts when you sign in to Windows.")
	}
	command := "$exe = " + psQuote(resolved) + "; $proc = Start-Process -FilePath $exe -ArgumentList @('autostart','enable') -Verb RunAs -Wait " +
		"-PassThru -ErrorAction Stop; exit $proc.ExitCode"
	out, err := run(ctx, []string{ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", command}, 300*time.Second, d.Env(cfg), nil)
	if err != nil {
		if _, timedOut := errors.AsType[*timeoutError](err); timedOut {
			return p.fail("Setting up cua-driver to start when you sign in took too long.")
		}
		return p.fail("Could not set up cua-driver to start when you sign in: " + err.Error() + ".")
	}
	if out.code == 0 {
		return d.autostartRegistered(cfg, resolved)
	}
	p.Warning("Could not set up cua-driver to start when you sign in.")
	p.tail(out)
	p.Info("To do it yourself, run this in PowerShell as administrator: & " + psQuote(resolved) + " autostart enable")
	return false
}
