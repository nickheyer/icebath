package plugins

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
	report, err := validatePluginDir(context.Background(), dir)
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
