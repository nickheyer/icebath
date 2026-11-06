package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyast"
	"github.com/nickheyer/icebath/pkg/pm"
)

func TestRuntimeRebindOfIcebathCoreFailsAdmission(t *testing.T) {
	root := t.TempDir()
	bad := vtPlugin(t, root, vtManifest(map[string]any{"name": "bad"}), "import run_agent\n"+
		"from .compat import server as gateway_server\n"+
		"def _wrap(cls, name, fn):\n"+
		"    setattr(cls, name, fn)\n"+
		"def register(ctx):\n"+
		"    run_agent.AIAgent.run_conversation = lambda *a, **k: None\n"+
		"    _wrap(run_agent.AIAgent, '_replace_primary_openai_client', print)\n"+
		"    srv = gateway_server()\n"+
		"    setattr(srv, 'handle_request', print)\n"+
		"    import hermes_cli.models_catalog_static as m\n"+
		"    m._PROVIDER_MODELS['x'] = ['y']\n")
	vtWrite(t, filepath.Join(bad, "compat.py"), "from importlib import import_module\ndef server():\n    return import_module('tui_gateway.server')\n")
	good := vtPlugin(t, root, vtManifest(map[string]any{"name": "good"}), "from hermes_cli.config import load_config\n"+
		"from .tools import helper\n"+
		"class Box:\n"+
		"    def set(self, v):\n"+
		"        self.v = v\n"+
		"def register(ctx):\n"+
		"    cfg = load_config()\n"+
		"    cfg.cached = True\n"+
		"    Box().set(1)\n"+
		"    ctx.register_hook('pre_llm_call', helper)\n")
	vtWrite(t, filepath.Join(good, "tools", "__init__.py"), "def helper(**kw):\n    return None\n")
	vtWrite(t, filepath.Join(good, "tests", "test_x.py"),
		"from unittest import mock\nimport run_agent\nrun_agent.AIAgent.x = 1\nmock.patch('run_agent.AIAgent.y')\n")
	got, err := coreOverrideFindings(bad)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"run_agent.AIAgent.run_conversation (__init__.py:6)",
		"_wrap(run_agent.AIAgent, ...) (__init__.py:7)",
		"setattr(srv, ...) (__init__.py:9)",
		"m._PROVIDER_MODELS[...] (__init__.py:11)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bad findings %q", got)
	}
	if got, err := coreOverrideFindings(good); err != nil || len(got) != 0 {
		t.Fatalf("good findings %q %v", got, err)
	}
	report := &ValidationReport{}
	if err := checkCoreOverride(report, bad); err != nil || report.OK() ||
		!strings.HasPrefix(report.Checks[0].Detail, "rebinds Icebath core at runtime (use a public hook, middleware or provider profile): run_agent") {
		t.Fatalf("report %+v %v", report.Checks, err)
	}
}

func TestCoreOverrideThroughAMethodPatchHelper(t *testing.T) {
	bad := vtPlugin(t, t.TempDir(), vtManifest(map[string]any{"name": "bad"}), "from agent import auxiliary_client\n"+
		"class Patches:\n"+
		"    def bind(self, target, name, fn):\n"+
		"        setattr(target, name, fn)\n"+
		"def register(ctx):\n"+
		"    Patches().bind(auxiliary_client, '_relay_sync_stream', print)\n")
	got, err := coreOverrideFindings(bad)
	if err != nil || !reflect.DeepEqual(got, []string{"bind(auxiliary_client, ...) (__init__.py:6)"}) {
		t.Fatalf("findings %q %v", got, err)
	}
}

// Plugin with a minimal manifest, a default __init__.py and the given files
func piPlugin(t *testing.T, root string, files map[string]string) string {
	t.Helper()
	vtWrite(t, filepath.Join(root, "plugin.yaml"), "description: x\nname: "+filepath.Base(root)+"\nversion: 1.0.0\n")
	if _, ok := files["__init__.py"]; !ok {
		vtWrite(t, filepath.Join(root, "__init__.py"), "def register(ctx):\n    pass\n")
	}
	for rel, text := range files {
		vtWrite(t, filepath.Join(root, filepath.FromSlash(rel)), text)
	}
	return root
}

func piValidate(t *testing.T, dir string) *ValidationReport {
	t.Helper()
	report, err := validatePluginDir(context.Background(), dir, vtProbe(t))
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func piWarnings(r *ValidationReport, prefix string) []string {
	out := []string{}
	for _, w := range r.Warnings {
		if strings.HasPrefix(w, prefix) {
			out = append(out, w)
		}
	}
	return out
}

func piSites(t *testing.T, warnings []string) []string {
	t.Helper()
	if len(warnings) != 1 {
		t.Fatalf("want one warning, got %q", warnings)
	}
	_, after, _ := strings.Cut(warnings[0], "release): ")
	before, _, _ := strings.Cut(after, ". Use ctx.*")
	return strings.Split(before, "; ")
}

func TestPrivateCoreImportWarnsWithFileAndLineButValidatePasses(t *testing.T) {
	plugin := piPlugin(t, filepath.Join(t.TempDir(), "router"), map[string]string{"lib/agent_runner.py": "import json\n" +
		"def run():\n" +
		"    from tools.tirith_security import ensure_installed\n" +
		"    from hermes_cli.config import _private_helper\n" +
		"    return ensure_installed, _private_helper\n"})
	report := piValidate(t, plugin)
	if !report.OK() {
		t.Fatal(report.Failures())
	}
	warnings := piWarnings(report, "private core imports")
	if len(warnings) != 1 {
		t.Fatalf("%q", report.Warnings)
	}
	for _, want := range []string{"lib/agent_runner.py:3 tools.tirith_security.ensure_installed",
		"lib/agent_runner.py:4 hermes_cli.config._private_helper", "catalog rule 9"} {
		if !strings.Contains(warnings[0], want) {
			t.Fatalf("%q lacks %q", warnings[0], want)
		}
	}
}

func TestDocumentedPublicStdlibAndOwnPackageImportsAreNotFlagged(t *testing.T) {
	plugin := piPlugin(t, filepath.Join(t.TempDir(), "clean"), map[string]string{
		"__init__.py": "import os, json\n" +
			"from typing import TYPE_CHECKING\n" +
			"import requests\n" +
			"from agent.memory_provider import MemoryProvider\n" +
			"from gateway.platforms import base\n" +
			"from hermes_cli.plugins import PluginContext\n" +
			"from clean_lib.helpers import local_helper\n" +
			"from .client import Client\n" +
			"if TYPE_CHECKING:\n" +
			"    from run_agent import AIAgent\n" +
			"def register(ctx):\n" +
			"    from hermes_cli.plugins import get_plugin_manager\n" +
			"    return get_plugin_manager\n",
		"clean_lib/__init__.py": "",
		"clean_lib/helpers.py":  "def local_helper():\n    pass\n",
		"client.py":             "class Client:\n    pass\n",
	})
	files, err := privateParsedFiles(plugin)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := privateImportFindings(files)
	if err != nil || !reflect.DeepEqual(hits, []string{"__init__.py:12 hermes_cli.plugins.get_plugin_manager"}) {
		t.Fatalf("%q %v", hits, err)
	}
	sites := piSites(t, piWarnings(piValidate(t, plugin), "private core imports"))
	if !reflect.DeepEqual(sites, []string{"__init__.py:12 hermes_cli.plugins.get_plugin_manager"}) {
		t.Fatalf("%q", sites)
	}
}

// Whether a hermes reference module binds the name at module level, as hasattr on the imported module sees it
func referenceDefines(t *testing.T, module, name string) bool {
	t.Helper()
	base := filepath.Join(append([]string{hermesReferenceTree}, strings.Split(module, ".")...)...)
	data, err := os.ReadFile(base + ".py")
	if err != nil {
		if data, err = os.ReadFile(filepath.Join(base, "__init__.py")); err != nil {
			t.Fatalf("%s is not in the reference tree", module)
		}
		if _, err := os.Stat(filepath.Join(base, name+".py")); err == nil {
			return true
		}
	}
	tree, err := pyast.Parse(string(data))
	if err != nil {
		t.Fatal(err)
	}
	var binds func(stmts []*pyast.Node) bool
	binds = func(stmts []*pyast.Node) bool {
		for _, s := range stmts {
			if n, _ := s.Str("name"); s.Is("FunctionDef", "AsyncFunctionDef", "ClassDef") && n == name {
				return true
			}
			for _, tgt := range append(s.Children("targets"), s.Child("target")) {
				if id, _ := tgt.Str("id"); tgt.Is("Name") && id == name {
					return true
				}
			}
			for _, a := range s.Children("names") {
				n, _ := a.Str("name")
				if as, ok := a.Str("asname"); ok {
					n = as
				}
				if s.Is("ImportFrom") && n == name {
					return true
				}
			}
			for _, field := range []string{"body", "orelse", "finalbody"} {
				if !s.Is("FunctionDef", "AsyncFunctionDef", "ClassDef") && binds(s.Children(field)) {
					return true
				}
			}
			for _, h := range s.Children("handlers") {
				if binds(h.Children("body")) {
					return true
				}
			}
		}
		return false
	}
	return binds(tree.Children("body"))
}

func TestPlatformPluginSeamsArePublicAndExist(t *testing.T) {
	seams := [][2]string{{"gateway.platforms.access_policy_mixin", "OwnAccessPolicyMixin"}, {"tools.approval", "resolve_gateway_approval"},
		{"tools.clarify_gateway", "get_pending"}, {"tools.clarify_gateway", "resolve_gateway_clarify"},
		{"tools.clarify_gateway", "mark_awaiting_text"}, {"gateway.platforms.helpers", "platform_http_client"},
		{"gateway.platforms.base", "ssrf_redirect_guard"}, {"tools.url_safety", "create_ssrf_safe_async_client"},
		{"tools.url_safety", "is_safe_url"}, {"gateway.platforms._http_client_limits", "platform_httpx_limits"},
		{"icebath_cli.gateway_setup_wizard", "print_setup_header"}, {"hermes_cli.gateway_setup_wizard", "confirm_reconfigure"},
		{"hermes_cli.gateway_setup_wizard", "offer_home_channel"}, {"hermes_cli.gateway_setup_wizard", "save_env_values"},
		{"hermes_cli.gateway_setup_wizard", "prompt_csv"}}
	var imports strings.Builder
	for _, s := range seams {
		module := strings.Replace(s[0], "icebath_cli.", "hermes_cli.", 1)
		if !referenceDefines(t, module, s[1]) {
			t.Fatalf("%s.%s", module, s[1])
		}
		imports.WriteString("from " + s[0] + " import " + s[1] + "\n")
	}
	plugin := piPlugin(t, filepath.Join(t.TempDir(), "chatplat"), map[string]string{"adapter.py": imports.String()})
	if w := piWarnings(piValidate(t, plugin), "private core imports"); len(w) != 0 {
		t.Fatalf("%q", w)
	}
}

func TestSkillCatalogSeamsArePublic(t *testing.T) {
	seams := [][2]string{{"tools.skills_tool", "find_all_skills"}, {"tools.skills_tool", "sort_skills"},
		{"tools.skills_tool", "skill_search_dirs"}, {"tools.skills_tool", "locate_skill"},
		{"tools.skills_tool", "skill_lookup_path_error"}, {"tools.skills_sync", "dir_hash"}, {"tools.skill_usage", "read_skill_name"}}
	var imports strings.Builder
	for _, s := range seams {
		if !referenceDefines(t, s[0], s[1]) {
			t.Fatalf("%s.%s", s[0], s[1])
		}
		imports.WriteString("from " + s[0] + " import " + s[1] + "\n")
	}
	plugin := piPlugin(t, filepath.Join(t.TempDir(), "catalog"), map[string]string{"view.py": imports.String()})
	if w := piWarnings(piValidate(t, plugin), "private core imports"); len(w) != 0 {
		t.Fatalf("%q", w)
	}
}

func TestDocumentedSessionHelperAndOwnInstallPathFallbackAreNotFlagged(t *testing.T) {
	plugin := piPlugin(t, filepath.Join(t.TempDir(), "openltm"), map[string]string{
		"__init__.py": "def register(ctx):\n" +
			"    from gateway.session_context import get_session_env\n" +
			"    try:\n" +
			"        from . import _db\n" +
			"    except ImportError:\n" +
			"        import plugins.memory.openltm._db as _db\n" +
			"        from plugins.memory.openltm import _db\n" +
			"    from hermes_cli.gateway_client import _session_ticket\n" +
			"    return get_session_env, _db, _session_ticket\n",
		"_db.py": "",
	})
	sites := piSites(t, piWarnings(piValidate(t, plugin), "private core imports"))
	if !reflect.DeepEqual(sites, []string{"__init__.py:8 hermes_cli.gateway_client._session_ticket"}) {
		t.Fatalf("%q", sites)
	}
}

func TestPrivateMethodCallsOnCoreHandlesWarnButOwnHelpersDoNot(t *testing.T) {
	plugin := piPlugin(t, filepath.Join(t.TempDir(), "sync"), map[string]string{"engine.py": "class Store:\n" +
		"    def _execute_write(self, fn):\n" +
		"        return self._execute_write(fn)\n" +
		"def apply(db, runner, store, route):\n" +
		"    rows = db._row_to_message_dict(None)\n" +
		"    runner._evict_cached_agent(route)\n" +
		"    return store._execute_write(rows)\n"})
	report := piValidate(t, plugin)
	if !report.OK() {
		t.Fatal(report.Failures())
	}
	sites := piSites(t, piWarnings(report, "private core attribute access"))
	if !reflect.DeepEqual(sites, []string{"engine.py:5 db._row_to_message_dict", "engine.py:6 runner._evict_cached_agent"}) {
		t.Fatalf("%q", sites)
	}
}

func TestIsolationHostAuditReadsTheHostBoundaryTable(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{"platformprobe": platformPlugin, "fine": probePlugin} {
		vtWrite(t, filepath.Join(root, name, "plugin.yaml"), "name: "+name+"\nversion: '1.0'\n")
		vtWrite(t, filepath.Join(root, name, "__init__.py"), body)
	}
	fine, err := auditPluginDir(filepath.Join(root, "fine"), nil)
	if err != nil || fine.Verdict != VerdictHost {
		t.Fatalf("fine %+v %v", fine, err)
	}
	blocked, err := auditPluginDir(filepath.Join(root, "platformprobe"), nil)
	if err != nil || blocked.Verdict != VerdictInProcess || !strings.Contains(blocked.Reasons[0], "register_platform") {
		t.Fatalf("blocked %+v %v", blocked, err)
	}
}

func TestIsolationVisitorFlagsPatchesRegistryCallsAndSyntaxErrors(t *testing.T) {
	visit, msg, err := isolationVisitSource("x.py", "x = (\n", false)
	if err != nil || visit != nil || msg != "'(' was never closed" {
		t.Fatalf("%v %q %v", visit, msg, err)
	}
	src := "import tools.registry as reg\nfrom gateway import platform_registry\nimport providers\n" +
		"reg.TOOLS = {}\nsetattr(providers, 'x', 1)\nreg.register('t')\nplatform_registry.register('p')\n" +
		"providers.register_provider(object())\nctx.register_hook('pre_llm_call', f)\nctx.emit('e')\n" +
		"class P:\n    def create_client(self):\n        pass\n"
	visit, _, err = isolationVisitSource("m.py", src, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"m.py:4 patches tools.registry.TOOLS (an Icebath module attribute; the host's copy is not the one Icebath runs)",
		"m.py:5 setattr() on providers (patches an Icebath module)",
		"m.py:6 calls tools.registry.register() directly instead of through ctx (registers in the host, not in Icebath)",
		"m.py:7 calls gateway.platform_registry.register() directly instead of through ctx (registers in the host, not in Icebath)",
		"m.py:11 P.create_client() returns a live SDK client (model-provider profiles cross the host as data + methods)",
	}
	if !reflect.DeepEqual(visit.Reasons, want) {
		t.Fatalf("%q", visit.Reasons)
	}
	if !visit.CtxMethods["register_hook"] || !visit.CtxMethods["emit"] || !visit.CtxMethods["register_provider"] {
		t.Fatalf("%v", visit.CtxMethods)
	}
}

func TestPluginValidateTreatsTheModelSetupHelpersAsPublic(t *testing.T) {
	dir := t.TempDir()
	vtWrite(t, filepath.Join(dir, "plugin.yaml"), "name: fake-provider\nkind: model-provider\n")
	vtWrite(t, filepath.Join(dir, "__init__.py"), "from hermes_cli.model_setup import persist_model_selection, note_setup_failure\n"+
		"from agent.secret_scope import secret_source_label\n"+
		"from hermes_cli.model_setup_flows_common import _persist_model\n")
	files, err := privateParsedFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := privateImportFindings(files)
	if err != nil || !reflect.DeepEqual(hits, []string{"__init__.py:3 hermes_cli.model_setup_flows_common._persist_model"}) {
		t.Fatalf("%q %v", hits, err)
	}
}

func TestInstallDepsProbeImportsFromTheSyncedEnvironment(t *testing.T) {
	tmp := t.TempDir()
	deps := filepath.Join(tmp, "synced-site-packages")
	vtWrite(t, filepath.Join(deps, "probe_only_dep.py"), "VALUE = 1\n")
	plugin := vtPlugin(t, tmp, map[string]any{"name": "needs-dep", "version": "1.0.0", "description": "d"},
		"import probe_only_dep\n\ndef register(ctx):\n    pass\n")
	python := vtProbe(t).Command[0]
	synced := append(os.Environ(), "PYTHONPATH="+deps)
	origSync, origPython, origSelected, origCommand, origEnv := pmSyncVenv, pmProjectPython, pmSelectedVenv, pmVenvCommand, pmActivationEnvironment
	t.Cleanup(func() {
		pmSyncVenv, pmProjectPython, pmSelectedVenv, pmVenvCommand, pmActivationEnvironment = origSync, origPython, origSelected, origCommand, origEnv
	})
	pmSyncVenv = func(context.Context, pm.SyncVenvOptions) error { return nil }
	pmProjectPython = func(context.Context, string) (string, error) { return python, nil }
	pmSelectedVenv = func(context.Context, string) (string, error) { return filepath.Join(tmp, "synced-venv"), nil }
	pmVenvCommand = func(_ context.Context, _, _ string, options []string) ([]string, error) {
		return append([]string{python}, options...), nil
	}
	pmActivationEnvironment = func(context.Context, string) ([]string, error) { return synced, nil }
	var out, errOut strings.Builder
	err := CmdValidate(context.Background(), plugin, true, true, &out, &errOut)
	var exit *CommandExit
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	var doc struct {
		Checks []struct {
			Name   string `json:"name"`
			OK     bool   `json:"ok"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("%v %s %s", err, out.String(), errOut.String())
	}
	for _, c := range doc.Checks {
		if c.Name == "capability probe" {
			if !c.OK {
				t.Fatal(c.Detail)
			}
			return
		}
	}
	t.Fatalf("no capability probe check in %s", out.String())
}
