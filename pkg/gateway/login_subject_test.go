package gateway

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestOneParserReadsTheLoginSubjectForEveryCaller(t *testing.T) {
	saved := state.ParseAuthenticatedSubject
	state.ParseAuthenticatedSubject = ParseAuthenticatedSubject
	t.Cleanup(func() { state.ParseAuthenticatedSubject = saved })
	subject, err := AuthenticatedSubject(map[string]any{"provider": "basic", "issuer": "", "user_id": "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	if parts, ok := ParseAuthenticatedSubject(subject); !ok || parts != [3]string{"basic", "", "e2e"} {
		t.Fatalf("parsed %v %v", parts, ok)
	}
	for _, bad := range []any{"uid:501", `auth:v1:"basic"`, "auth:v1:[1,2,3]", `auth:v1:["a","b"]`, "auth:v1:[", nil, 7} {
		if _, ok := ParseAuthenticatedSubject(bad); ok {
			t.Fatalf("%v parsed as a login", bad)
		}
		if label, ok := state.LoginLabel(bad); ok {
			t.Fatalf("%v labelled %q", bad, label)
		}
		if text, isText := bad.(string); isText && structuredLoginSubject(text) {
			t.Fatalf("%q read as a structured login", text)
		}
	}
	if !structuredLoginSubject(subject) {
		t.Fatalf("%q not read as a structured login", subject)
	}
}
