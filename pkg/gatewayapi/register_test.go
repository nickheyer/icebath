package gatewayapi

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestBuiltinAdapterImportAnswersAPIServerAdapterClass(t *testing.T) {
	spec := gateway.BuiltinAdapters[gateway.PlatformAPIServer]
	cls, probe, err := gateway.BuiltinAdapterImport(spec.Module, spec.AdapterName, spec.Requirement)
	if err != nil {
		t.Fatalf("BuiltinAdapterImport(api_server) = %v", err)
	}
	if cls == nil || cls.Name != "APIServerAdapter" || cls.New == nil {
		t.Fatalf("class = %#v, want the APIServerAdapter class", cls)
	}
	if !cls.Traits.ServesProfilePrefix {
		t.Fatal("APIServerAdapter.serves_profile_prefix is false")
	}
	if check, ok := probe.(func() bool); !ok || !check() {
		t.Fatalf("check_api_server_requirements = %#v, want a passing probe", probe)
	}
}
