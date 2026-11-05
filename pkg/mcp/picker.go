package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	pickerStatusNotInstalled   = "available"
	pickerStatusDisabled       = "installed (disabled)"
	pickerStatusEnabled        = "enabled"
	pickerStatusCustomEnabled  = "custom — enabled"
	pickerStatusCustomDisabled = "custom — disabled"
)

// Seam over the cli configure command the picker opens for an installed server
var pickerConfigure = cmdMCPConfigure

// Picker row, Entry set for catalog rows and nil for custom servers
type PickerRow struct {
	Name        string
	Description string
	Status      string
	Entry       *CatalogEntry
}

func (r PickerRow) IsCustom() bool { return r.Entry == nil }

// Catalog rows with install status followed by custom configured servers sorted by name
func BuildPickerRows(ctx context.Context) ([]PickerRow, error) {
	entries, err := ListCatalog()
	if err != nil {
		return nil, err
	}
	servers, err := getMCPServers(ctx, nil)
	if err != nil {
		return nil, err
	}
	catalogNames := map[string]bool{}
	var rows []PickerRow
	for _, e := range entries {
		catalogNames[e.Name] = true
		cfg, installed := servers[e.Name]
		status := pickerStatusNotInstalled
		if installed {
			status = pickerStatusDisabled
			if yTruthy(cfg) {
				on, err := serverBlockEnabled(cfg)
				if err != nil {
					return nil, err
				}
				if on {
					status = pickerStatusEnabled
				}
			}
		}
		rows = append(rows, PickerRow{Name: e.Name, Description: e.Description, Status: status, Entry: e})
	}
	for _, name := range orderedKeys(servers) {
		if catalogNames[name] {
			continue
		}
		cfg := servers[name]
		on, err := serverBlockEnabled(cfg)
		if err != nil {
			return nil, err
		}
		status := pickerStatusCustomDisabled
		if on {
			status = pickerStatusCustomEnabled
		}
		m, _, _ := yDict(cfg)
		rows = append(rows, PickerRow{Name: name, Description: pyStr(yOr(yOr(m["url"], m["command"]), "(no transport)")), Status: status})
	}
	return rows, nil
}

// Pads s with spaces to width code points like str.ljust
func pyLjust(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

func formatPickerRow(row PickerRow) string {
	return fmt.Sprintf("%s %s %s", pyLjust(row.Name, 18), pyLjust(row.Status, 24), row.Description)
}

// Sets the enabled flag of an installed server, reporting a missing server instead of writing
func pickerEnableDisable(ctx context.Context, ui TerminalUI, name string, enable bool) error {
	h := home.Dir(ctx)
	cfg, err := config.ReadUser(h)
	if err != nil {
		return err
	}
	servers, _, ok := yDict(yOr(cfg["mcp_servers"], map[string]any{}))
	if !ok {
		return fmt.Errorf("'%s' object has no attribute 'get'", yTypeName(cfg["mcp_servers"]))
	}
	server := servers[name]
	if !yTruthy(server) {
		say(ui, fmt.Sprintf("  '%s' is not installed.", name), ansiRed)
		return nil
	}
	block, _, ok := yDict(server)
	if !ok {
		return fmt.Errorf("'%s' object does not support item assignment", yTypeName(server))
	}
	block["enabled"] = enable
	servers[name] = block
	cfg["mcp_servers"] = servers
	if err := config.SaveUser(h, cfg); err != nil {
		return err
	}
	state := "disabled"
	if enable {
		state = "enabled"
	}
	say(ui, fmt.Sprintf("  ✓ '%s' %s. Start a new icebath session for changes to take effect.", name, state))
	return nil
}

// Removes a custom server after confirmation, reporting one that is not configured
func pickerRemoveCustom(ctx context.Context, ui TerminalUI, name string) error {
	installed, err := IsInstalled(ctx, name)
	if err != nil {
		return err
	}
	if !installed {
		say(ui, fmt.Sprintf("  '%s' is not configured.", name), ansiRed)
		return nil
	}
	if !ui.YesNo(fmt.Sprintf("Remove '%s' from mcp_servers?", name), false) {
		return nil
	}
	if _, err := RemoveServer(ctx, name); err != nil {
		return err
	}
	say(ui, fmt.Sprintf("  ✓ Removed '%s'", name))
	return nil
}

// Installs an entry enabled, printing a catalog failure instead of returning it
func pickerInstall(ctx context.Context, env *tools.Env, ui TerminalUI, entry *CatalogEntry, verb string) (bool, error) {
	err := InstallEntry(ctx, env, ui, entry, true, nil)
	var ce *CatalogError
	if errors.As(err, &ce) {
		say(ui, fmt.Sprintf("  ✗ %s failed: %s", verb, ce.Msg), ansiRed)
		return false, nil
	}
	return err == nil, err
}

// Uninstalls a catalog server after confirmation, keeping its credentials in .env
func pickerUninstall(ctx context.Context, ui TerminalUI, name string) error {
	if !ui.YesNo(fmt.Sprintf("Uninstall '%s'?", name), false) {
		return nil
	}
	removed, err := UninstallEntry(ctx, name, true)
	if err != nil {
		return err
	}
	if removed {
		say(ui, fmt.Sprintf("  ✓ Uninstalled '%s'. Credentials in .env preserved — delete manually if no longer needed.", name))
	} else {
		say(ui, fmt.Sprintf("  '%s' was not installed", name), ansiDim)
	}
	return nil
}

type pickerAction struct {
	label string
	run   func() error
}

// Single select over the action labels running the chosen callback
func runPickerSubmenu(ui TerminalUI, title string, actions []pickerAction) error {
	labels := make([]string, len(actions))
	for i, a := range actions {
		labels[i] = a.label
	}
	if choice, ok := ui.SingleSelect(title, labels); ok {
		return actions[choice].run()
	}
	return nil
}

// Installs, enables or offers the action submenu for a picked row by its current status
func handlePickerRow(ctx context.Context, env *tools.Env, ui TerminalUI, row PickerRow) error {
	configure := func() error { pickerConfigure(ctx, env, ui, row.Name); return nil }
	if row.Entry != nil {
		installed, err := IsInstalled(ctx, row.Name)
		if err != nil {
			return err
		}
		if !installed {
			_, err := pickerInstall(ctx, env, ui, row.Entry, "install")
			return err
		}
		enabled, err := IsEnabled(ctx, row.Name)
		if err != nil {
			return err
		}
		if !enabled {
			return pickerEnableDisable(ctx, ui, row.Name, true)
		}
	}
	if row.IsCustom() {
		enabled, err := IsEnabled(ctx, row.Name)
		if err != nil {
			return err
		}
		toggle := "Disable"
		if !enabled {
			toggle = "Enable"
		}
		return runPickerSubmenu(ui, fmt.Sprintf("Action for '%s' (custom)", row.Name), []pickerAction{
			{"Configure tools (probe server + re-pick)", configure},
			{toggle, func() error {
				now, err := IsEnabled(ctx, row.Name)
				if err != nil {
					return err
				}
				return pickerEnableDisable(ctx, ui, row.Name, !now)
			}},
			{"Remove from config", func() error { return pickerRemoveCustom(ctx, ui, row.Name) }},
		})
	}
	fmt.Fprintln(ui.Out())
	say(ui, fmt.Sprintf("  '%s' is already enabled.", row.Name), ansiDim)
	return runPickerSubmenu(ui, fmt.Sprintf("Action for '%s'", row.Name), []pickerAction{
		{"Configure tools (probe server + re-pick)", configure},
		{"Disable (keep config, stop loading on next session)", func() error { return pickerEnableDisable(ctx, ui, row.Name, false) }},
		{"Uninstall (remove config and any cloned files)", func() error { return pickerUninstall(ctx, ui, row.Name) }},
		{"Reinstall (re-clone, re-prompt for credentials)", func() error {
			_, err := pickerInstall(ctx, env, ui, row.Entry, "reinstall")
			return err
		}},
	})
}

// Plain text catalog dump with future manifest warnings, the catalog command and the non interactive picker
func printPickerRows(ui TerminalUI, rows []PickerRow) {
	out := ui.Out()
	fmt.Fprintln(out)
	if len(rows) == 0 {
		say(ui, "  No MCPs in the catalog or configured.", ansiDim)
		fmt.Fprintln(out)
		return
	}
	say(ui, "  MCP Catalog + configured servers:", ansiCyan+ansiBold)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %s %s Description\n", pyLjust("Name", 18), pyLjust("Status", 24))
	fmt.Fprintf(out, "  %s %s %s\n", strings.Repeat("-", 18), strings.Repeat("-", 24), strings.Repeat("-", 11))
	for _, row := range rows {
		fmt.Fprintf(out, "  %s\n", formatPickerRow(row))
	}
	fmt.Fprintln(out)
	say(ui, "  Install: icebath mcp install <name>    Picker: icebath mcp", ansiDim)
	var future []CatalogDiagnostic
	for _, d := range CatalogDiagnostics() {
		if d.Kind == "future_manifest" {
			future = append(future, d)
		}
	}
	if len(future) > 0 {
		fmt.Fprintln(out)
		for _, d := range future {
			say(ui, fmt.Sprintf("  ⚠ '%s' requires a newer icebath — run `icebath update` to install this entry.", d.Name), ansiYellow)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out)
}

// Prints the curated catalog and custom servers without interaction
func ShowCatalog(ctx context.Context, ui TerminalUI) error {
	rows, err := BuildPickerRows(ctx)
	if err != nil {
		return err
	}
	printPickerRows(ui, rows)
	return nil
}

// Interactive selector that re-renders after each action until cancelled, a text dump off a terminal
func RunPicker(ctx context.Context, env *tools.Env, ui TerminalUI) error {
	for {
		rows, err := BuildPickerRows(ctx)
		if err != nil {
			return err
		}
		if len(rows) == 0 || !ui.Interactive() {
			printPickerRows(ui, rows)
			return nil
		}
		labels := make([]string, len(rows))
		for i, r := range rows {
			labels[i] = formatPickerRow(r)
		}
		idx, ok := ui.SingleSelect("MCP Catalog  —  ↑↓ navigate  ENTER act on entry  ESC/q quit", labels)
		if !ok {
			return nil
		}
		if err := handlePickerRow(ctx, env, ui, rows[idx]); err != nil {
			return err
		}
	}
}

// Non interactive install of a catalog entry by name, exit code 0 on success and 1 otherwise
func InstallByName(ctx context.Context, env *tools.Env, ui TerminalUI, identifier string) (int, error) {
	entry, err := GetEntry(identifier)
	if err != nil {
		return 1, err
	}
	if entry == nil {
		say(ui, fmt.Sprintf("  ✗ '%s' is not in the catalog. Run `icebath mcp catalog` to see available entries.", identifier), ansiRed)
		return 1, nil
	}
	ok, err := pickerInstall(ctx, env, ui, entry, "install")
	if err != nil || !ok {
		return 1, err
	}
	return 0, nil
}
