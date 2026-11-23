package tuiserver

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Session row object of a JSON literal
func rowOf(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	row, err := pyjson.LoadsDict(text)
	if err != nil {
		t.Fatal(err)
	}
	return row
}
