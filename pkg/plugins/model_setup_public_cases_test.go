package plugins

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestPluginValidateTreatsTheHelpersAsPublic(t *testing.T) {
	plugin := piPlugin(t, filepath.Join(t.TempDir(), "fake-provider"), map[string]string{
		"plugin.yaml": "name: fake-provider\nkind: model-provider\n",
		"__init__.py": "from hermes_cli.model_setup import persist_model_selection, note_setup_failure\n" +
			"from agent.secret_scope import secret_source_label\n" +
			"from hermes_cli.model_setup_flows_common import _persist_model\n",
	})
	files, err := privateParsedFiles(plugin)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := privateImportFindings(files)
	if err != nil || !reflect.DeepEqual(hits, []string{"__init__.py:3 hermes_cli.model_setup_flows_common._persist_model"}) {
		t.Fatalf("%q %v", hits, err)
	}
}
