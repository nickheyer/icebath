package tools

import (
	"context"
	"fmt"
	"io"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Entry of icebath tools and setup tools, first install skipping the menu, cfg receiving the saved platform lists
func (s *ToolsSetup) ToolsCommand(ns map[string]any, firstInstall bool, cfg config.Tree) error {
	if cfg == nil {
		var err error
		if cfg, err = toolsConfigLoad(s); err != nil {
			return err
		}
	}
	enabledPlatforms, err := getEnabledPlatforms(s)
	if err != nil {
		return err
	}
	s.println("")
	if pyTruthy(ns["summary"]) {
		return s.printToolsSummary(cfg, enabledPlatforms)
	}
	s.printColor("☤ Icebath Tool Configuration", cliui.Cyan, cliui.Bold)
	s.printColor("  Enable or disable tools per platform.", cliui.Dim)
	s.printColor("  Tools that need API keys will be configured when enabled.", cliui.Dim)
	s.printColor("  Guide: https://hermes-agent.nousresearch.com/docs/user-guide/features/tools", cliui.Dim)
	s.println("")
	if firstInstall {
		return s.firstInstallFlow(cfg, enabledPlatforms)
	}
	m := &toolsMenu{platformKeys: append([]string{}, enabledPlatforms...)}
	for _, pkey := range m.platformKeys {
		label, err := s.platformMenuLabel(cfg, pkey)
		if err != nil {
			return err
		}
		m.choices = append(m.choices, label)
	}
	m.globalIdx = m.addRow("Configure all platforms (global)", len(m.platformKeys) > 1)
	m.reconfigIdx = m.addRow("Reconfigure an existing tool's provider or API key", true)
	m.metricsIdx = m.addRow(sharedMetricsMenuLabel(cfg), true)
	m.mcpIdx = m.addRow("Configure MCP server tools", pyTruthy(cfg["mcp_servers"]))
	doneIdx := m.addRow("Done", true)
	for {
		idx, err := s.promptChoice("Select an option:", m.choices, 0)
		if err != nil {
			return err
		}
		if idx == doneIdx {
			break
		}
		if err := s.runToolsMenuRow(cfg, m, idx); err != nil {
			return err
		}
		s.println("")
	}
	s.println("")
	s.printColor("  Tool configuration saved to "+home.Display(s.home())+"/config.yaml", cliui.Dim)
	s.printColor("  Changes take effect on next 'icebath' or gateway restart.", cliui.Dim)
	s.println("")
	return nil
}

// The returning user menu, platform rows first then the extra rows, an absent row at index -1
type toolsMenu struct {
	platformKeys                               []string
	choices                                    []string
	globalIdx, reconfigIdx, metricsIdx, mcpIdx int
}

func (m *toolsMenu) addRow(label string, present bool) int {
	if !present {
		return -1
	}
	m.choices = append(m.choices, label)
	return len(m.choices) - 1
}

// Runs the menu row the user picked, relabelling the rows the change affects
func (s *ToolsSetup) runToolsMenuRow(cfg config.Tree, m *toolsMenu, idx int) error {
	switch idx {
	case m.reconfigIdx:
		return s.reconfigureTool(cfg, true)
	case m.metricsIdx:
		if err := s.configureSharedMetricsInteractive(cfg); err != nil {
			return err
		}
		m.choices[m.metricsIdx] = sharedMetricsMenuLabel(cfg)
		return nil
	case m.mcpIdx:
		return configureMCPToolsInteractive(s, cfg)
	case m.globalIdx:
		saved, err := s.configurePlatforms(cfg, m.platformKeys, true)
		if err != nil || !saved {
			return err
		}
		for ci, pk := range m.platformKeys {
			label, err := s.platformMenuLabel(cfg, pk)
			if err != nil {
				return err
			}
			m.choices[ci] = label
		}
		return nil
	}
	j, ok := pyListIndex(len(m.platformKeys), idx)
	if !ok {
		return fmt.Errorf("IndexError: list index out of range")
	}
	if _, err := s.configurePlatforms(cfg, []string{m.platformKeys[j]}, false); err != nil {
		return err
	}
	label, err := s.platformMenuLabel(cfg, m.platformKeys[j])
	if err != nil {
		return err
	}
	ci, _ := pyListIndex(len(m.choices), idx)
	m.choices[ci] = label
	return nil
}

// The icebath tools dispatcher, list enable and disable through the action command, post-setup through its hook runner
func CmdTools(ctx context.Context, env *Env, ns map[string]any, w io.Writer) (int, error) {
	s := &ToolsSetup{Ctx: ctx, Env: env, Con: toolsConfigConsoleFor(w)}
	switch action := nsString(ns, "tools_action"); action {
	case "list", "disable", "enable":
		platform := "cli"
		if _, ok := ns["platform"]; ok {
			platform = nsString(ns, "platform")
		}
		return 0, toolsDisableEnableCommand(s, action, platform, nsStringList(ns, "names"))
	case "post-setup":
		return s.RunPostSetupCommand(ns), nil
	}
	if !s.Con.StdinIsTTY() {
		fmt.Fprintln(toolsCommandStderr, "Error: 'icebath tools' requires an interactive terminal.\n"+
			"It cannot be run through a pipe or non-interactive subprocess.\n"+
			"Run it directly in your terminal instead.")
		return 1, nil
	}
	return 0, s.ToolsCommand(ns, false, nil)
}
