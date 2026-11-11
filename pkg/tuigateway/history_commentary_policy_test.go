package tuigateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Commentary row whose public item carries a provider key the default redaction masks
func secretCommentaryRow() (map[string]any, string) {
	secret := "sk-" + "demo0123456789abcdef"
	return commentaryRow("Start. <think>private scratchpad</think> Key=" + secret), secret
}

// Display commentary of one row projected under the given owning profile home
func projectedCommentary(t *testing.T, row map[string]any, profileHome string) []any {
	t.Helper()
	rows, err := HistoryToMessages(context.Background(), []any{row}, profileHome, true)
	if err != nil {
		t.Fatal(err)
	}
	commentary, _ := rows[0].(map[string]any)["display_commentary"].([]any)
	return commentary
}

func TestCommentaryRedactionFollowsTheOwningProfilePolicy(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "launch"))
	optedOut := filepath.Join(root, "opted-out")
	if err := os.MkdirAll(optedOut, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(optedOut, "config.yaml"), []byte("security:\n  redact_secrets: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tools.ResolveProfilePolicy(optedOut)
	row, secret := secretCommentaryRow()

	launch := projectedCommentary(t, row, "")
	if len(launch) != 1 || strings.Contains(launch[0].(string), secret) || strings.Contains(launch[0].(string), "<think>") {
		t.Fatalf("launch profile commentary %#v", launch)
	}
	unrecorded := projectedCommentary(t, row, filepath.Join(root, "unrecorded"))
	if len(unrecorded) != 1 || strings.Contains(unrecorded[0].(string), secret) {
		t.Fatalf("unrecorded profile commentary %#v", unrecorded)
	}
	raw := projectedCommentary(t, row, optedOut)
	if !reflect.DeepEqual(raw, []any{"Start.  Key=" + secret}) {
		t.Fatalf("opted out profile commentary %#v", raw)
	}
}

func TestCommentaryDisplayMetadataThatIsNotADictRaises(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	row := commentaryRow("I will inspect the files.")
	row["display_metadata"] = []any{"commentary_withheld"}
	_, err := HistoryToMessages(context.Background(), []any{row}, "", true)
	var attrErr *DisplayMetadataError
	if !errors.As(err, &attrErr) || err.Error() != "'list' object has no attribute 'get'" || attrErr.PyTypeName() != "AttributeError" {
		t.Fatalf("err = %v", err)
	}
	row["display_metadata"] = map[string]any{CommentaryWithheld: true}
	if commentary := projectedCommentary(t, row, ""); !reflect.DeepEqual(commentary, []any{}) {
		t.Fatalf("withheld commentary %#v", commentary)
	}
}
