package commands

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/spf13/cobra"
)

// Port of build_plugins_parser in hermes_cli/subcommands/plugins.py, every verb with its help, positionals and flags
func TestCLIPluginsTreeMatchesBuildPluginsParser(t *testing.T) {
	c := NewCLI()
	flag := func(name, short, help string) parserFlag { return parserFlag{name, short, "bool", "false", help} }
	json := func(help string) parserFlag { return flag("json", "", help) }
	assertParser(t, c, "plugins", "Manage and validate plugins",
		"Install, update, remove, list, or validate native Icebath plugins and portable Agent Plugins v1 packages. Portable packages install disabled.", "")
	assertParser(t, c, "plugins install", "Install a plugin from the curated catalog, a Git URL, or owner/repo", "",
		"  identifier   Bare plugin catalog entry name (see `icebath plugins search`), Git URL, or owner/repo shorthand (e.g. anpicasso/hermes-plugin-chrome-profiles)",
		flag("force", "f", "Reinstall over an existing plugin (from the same source, your untracked files are kept)"),
		parserFlag{"ref", "", "string", "", "Install exactly one immutable 40-character Git commit SHA"},
		flag("allow-removed", "", "DANGEROUS: bypass the catalog removed-plugin blocklist check"),
		flag("enable", "", "Auto-enable the plugin after install (skip confirmation prompt)"),
		flag("no-enable", "", "Install disabled (skip confirmation prompt); enable later with `icebath plugins enable <name>`"))
	assertParser(t, c, "plugins search", "Search the curated Icebath plugin catalog", "",
		"  term   Query matched against entry names, descriptions and declared tools (omit to list the whole catalog)", json("Print machine-readable JSON"))
	assertParser(t, c, "plugins browse", "List every curated plugin catalog entry", "", "")
	assertParser(t, c, "plugins validate", "Validate a plugin directory for catalog admission (CI gate)", "", "  path   Path to the plugin directory",
		json("Print machine-readable JSON (for CI)"))
	assertParser(t, c, "plugins update", "Update an installed plugin, or with no name every plugin that has an update", "",
		"  name   Plugin name to update (default: list every plugin with an update and offer them all)",
		flag("yes", "y", "With no name: update everything listed without the confirmation question (required without a terminal)"),
		flag("force", "f", "Apply the update even when the security scan returns a caution verdict (a dangerous verdict still blocks). Without it, a terminal asks first"))
	assertParser(t, c, "plugins adopt", "Adopt a self-cloned plugin dir into provenance tracking",
		"For plugin dirs you cloned yourself (no install record): read the git origin URL, write the provenance row, and become a tracked git install (check-updates + update).",
		"  name   Self-cloned plugin directory name")
	assertParser(t, c, "plugins trust-update-url", "Confirm a changed plugin update_url into the saved tag",
		"The ONLY path that moves a saved update_url tag. When a plugin's manifest changed its update_url (needs-fixing mismatch), running this trusts the new url after review. Never triggered automatically.",
		"  name   Plugin name")
	assertParser(t, c, "plugins check-updates", "Check whether installed plugins have updates (read-only)",
		"Standard, read-only update check for every installed plugin: saved-tag update_url feeds (with mismatch protection), git ls-remote for git installs, and a stateless PyPI probe for pip entry-point plugins. NEVER mutates anything — apply with `icebath plugins update <name>`.",
		"", json("Print machine-readable JSON (the receipt-section shape)"))
	assertParser(t, c, "plugins remove", "Remove an installed plugin", "", "  name   Plugin directory name to remove")
	assertParser(t, c, "plugins list", "List installed plugins", "", "",
		flag("enabled", "", "Show only enabled plugins"), flag("user", "", "Show only user-installed plugins (including git plugins)"),
		flag("no-bundled", "", "Hide bundled plugins"), flag("plain", "", "Print compact plain-text output instead of a Rich table"),
		json("Print machine-readable JSON"))
	assertParser(t, c, "plugins enable", "Enable a disabled plugin", "", "  name   Plugin name to enable",
		flag("allow-tool-override", "", "Grant this plugin permission to replace built-in tools (e.g. shell_exec, write_file). Skips the confirmation prompt."),
		flag("no-allow-tool-override", "", "Enable without granting built-in tool override (skip prompt)."))
	assertParser(t, c, "plugins disable", "Disable a plugin without removing it", "", "  name   Plugin name to disable")
	assertParser(t, c, "plugins capabilities", "Show declared vs granted capabilities per plugin",
		"Show each plugin's declared capabilities (from plugin.yaml) against what the user has granted. Capabilities are a consent and audit layer over host API surfaces — NOT a sandbox.",
		"  name   Plugin id to inspect (omit to list all plugins with capabilities)")
	assertParser(t, c, "plugins doctor", "Validate a plugin with the real runtime contracts", "",
		"  target   Plugin path or installed plugin id (default: current directory)", flag("ci", "", "Exit non-zero when validation reports an error"))
	assertParser(t, c, "plugins pack", "Declarative, shareable plugin sets (icebath-pack.yaml)",
		"Install, export, or inspect plugin packs — a single YAML file pinning a set of plugins to exact commit SHAs, with optional non-secret config seeds. Installing a pack fans out to ordinary pinned installs; capability consent stays per-plugin.", "")
	assertParser(t, c, "plugins pack install", "Review and install a pack from a file path or https URL", "",
		"  source   Path to a icebath-pack.yaml file, or an https:// URL", flag("force", "f", "Reinstall plugins that already exist"))
	assertParser(t, c, "plugins pack export", "Emit a pack YAML for the current install on stdout", "", "",
		flag("enabled-only", "", "Only include plugins currently in plugins.enabled"),
		parserFlag{"name", "", "string", "my-icebath-pack", "Pack name to embed in the exported YAML"})
	assertParser(t, c, "plugins pack show", "Dry-run: parse and display a pack without installing", "",
		"  source   Path to a icebath-pack.yaml file, or an https:// URL")
	assertParser(t, c, "plugins show", "Show details for a single plugin (including emits/listens)", "", "  name   Plugin name or key to show")
	for path, aliases := range map[string][]string{"plugins check-updates": {"check"}, "plugins remove": {"rm", "uninstall"}, "plugins list": {"ls"}, "plugins show": {"info"}} {
		if got := c.find(path).Aliases; strings.Join(got, ",") != strings.Join(aliases, ",") {
			t.Fatalf("%s aliases %v", path, got)
		}
	}
}

// Parser cases of plugins update with the mutually exclusive install and enable groups
func TestPluginsArgsParse(t *testing.T) {
	type upd struct {
		name       string
		yes, force bool
	}
	parse := func(path string, argv ...string) (any, error) {
		return pmParse(t, path, argv, func(cmd *cobra.Command, args []string) any {
			yes, _ := cmd.Flags().GetBool("yes")
			force, _ := cmd.Flags().GetBool("force")
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			return upd{name, yes, force}
		})
	}
	for argv, want := range map[string]upd{"update": {}, "update --yes": {yes: true}, "update -y": {yes: true}, "update cat -f": {name: "cat", force: true}} {
		got, err := parse("plugins update", append([]string{"plugins"}, strings.Fields(argv)...)...)
		if err != nil || got != want {
			t.Fatalf("%s = %+v %v", argv, got, err)
		}
	}
	for _, argv := range [][]string{{"plugins", "install", "x", "--enable", "--no-enable"},
		{"plugins", "enable", "x", "--allow-tool-override", "--no-allow-tool-override"}} {
		if _, err := parse(strings.Join(argv[:2], " "), argv...); err == nil {
			t.Fatalf("%v parsed", argv)
		}
	}
}

// The _action_update refusals end with exit 1 and an action outside the parser choices exits 2
func TestPluginsDispatchRefusals(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for argv, want := range map[string]string{
		"plugins update cat --yes": "Error: --yes only applies to `icebath plugins update` with no name.",
		"plugins update --force":   "Error: --force applies to one named plugin: `icebath plugins update <name> --force`.",
	} {
		c := NewCLI()
		var out bytes.Buffer
		c.find("plugins").Root().SetOut(&out)
		err := c.Execute(strings.Fields(argv))
		var exit *ExitError
		if !errors.As(err, &exit) || exit.Code != 1 || !strings.Contains(out.String(), want) {
			t.Fatalf("%s = %v %q", argv, err, out.String())
		}
	}
	err := NewCLI().Execute([]string{"plugins", "frobnicate"})
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 2 || !strings.Contains(exit.Msg, "'frobnicate' is not a `icebath plugins` command.") {
		t.Fatalf("frobnicate = %v", err)
	}
}

// Every plugins verb is bound and each alias reaches the handler its _PLUGIN_ACTIONS row names
func TestPluginsVerbsBoundAndAliasesDispatch(t *testing.T) {
	for _, path := range NewCLI().Unbound() {
		if path == "plugins" || strings.HasPrefix(path, "plugins ") {
			t.Errorf("%s is unbound", path)
		}
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"entries": [{"name": "probe", "repo": "https://github.com/owner/probe", "sha": "` + strings.Repeat("a", 40) + `", "description": "Probe catalog entry", "tier": "community"}]}`))
	}))
	defer srv.Close()
	saved := plugins.LiveCatalogURL
	plugins.LiveCatalogURL = srv.URL
	t.Cleanup(func() { plugins.LiveCatalogURL = saved })
	run := func(argv string) string {
		c := NewCLI()
		var out bytes.Buffer
		c.find("plugins").Root().SetOut(&out)
		c.Execute(strings.Fields(argv))
		return out.String()
	}
	if got := run("plugins info probe"); !strings.Contains(got, "Probe catalog entry") || !strings.Contains(got, "github.com/owner/probe") {
		t.Fatalf("info reached %q", got)
	}
	if got := run("plugins show probe"); !strings.Contains(got, "Plugin 'probe' not found.") {
		t.Fatalf("show reached %q", got)
	}
	for _, group := range [][]string{{"plugins list", "plugins ls"}, {"plugins remove ghost", "plugins rm ghost", "plugins uninstall ghost"},
		{"plugins check-updates --json", "plugins check --json"}, {"plugins search", "plugins browse"}} {
		want := run(group[0])
		if want == "" {
			t.Fatalf("%s printed nothing", group[0])
		}
		for _, alias := range group[1:] {
			if got := run(alias); got != want {
				t.Fatalf("%s = %q, %s = %q", alias, got, group[0], want)
			}
		}
	}
	if got := run("plugins search"); !strings.Contains(got, "probe") {
		t.Fatalf("search %q", got)
	}
}
