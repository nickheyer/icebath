package plugins

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestUserMemorySchemaImportsTheShippedConfigSchemaModuleWithoutOverride(t *testing.T) {
	unsetBundledOverride(t)
	installLiveServices(t)
	h, _ := liveHome(t, liveInterpreters(t)["system"], nil)
	memoryManager(t, h)
	ctx := homeCtx(h)
	schemaFile := filepath.Join(h, "plugins", "custom", "config_schema.py")
	memWrite(t, schemaFile, "from plugins.memory.config_schema import ProviderConfigSchema, ProviderField\n"+
		"CONFIG_SCHEMA = ProviderConfigSchema(name=\"custom\", label=\"Custom\", fields=[ProviderField(key=\"api_key\", label=\"Key\", kind=\"secret\")])\n")
	schema, err := MemoryProviderConfigSchema(ctx, schemaFile)
	record, _ := schema.(Record)
	fields := reflect.ValueOf(record["fields"])
	if err != nil || record["name"] != "custom" || record["label"] != "Custom" || fields.Kind() != reflect.Slice || fields.Len() != 1 {
		t.Fatalf("schema %#v %v", schema, err)
	}
	if field, _ := fields.Index(0).Interface().(Record); field["key"] != "api_key" || field["kind"] != "secret" {
		t.Fatalf("field %#v", fields.Index(0).Interface())
	}
}
