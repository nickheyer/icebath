package memory

import (
	"path/filepath"
	"testing"
)

func TestUserProviderDataclassSchemaCrossesThePluginHost(t *testing.T) {
	isolateRegistry(t)
	ctx, h := discHome(t)
	dir := discProvider(t, filepath.Join(h, "plugins"), "dcprobe")
	discWrite(t, filepath.Join(dir, "config_schema.py"), "from plugins.memory.config_schema import (\n"+
		"    KIND_SELECT, STORAGE_HONCHO_HOST_BLOCK, ProviderConfigSchema, ProviderField, ProviderFieldOption,\n)\n"+
		"CONFIG_SCHEMA = ProviderConfigSchema(name='dcprobe', label='Probe', storage=STORAGE_HONCHO_HOST_BLOCK, fields=(\n"+
		"    ProviderField(key='env', label='Env', kind=KIND_SELECT, default='a', aliases=('environment',), scope='root',\n"+
		"                  options=(ProviderFieldOption('a', 'A'), ProviderFieldOption('b', 'B', 'second'))),\n))\n")
	s := GetProviderConfigSchema(ctx, nil, "dcprobe")
	if s == nil || s.Label != "Probe" || s.Storage != StorageHonchoHostBlock || len(s.Fields) != 1 {
		t.Fatalf("schema %+v", s)
	}
	f := s.Fields[0]
	if f.Key != "env" || f.Kind != KindSelect || f.Default != "a" || f.Scope != "root" || len(f.Aliases) != 1 || f.Aliases[0] != "environment" ||
		len(f.Options) != 2 || f.Options[1].Value != "b" || f.Options[1].Description != "second" || !f.AllowedValues()["a"] {
		t.Fatalf("field %+v", f)
	}
}
