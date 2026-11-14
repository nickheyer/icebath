//go:build !windows

package config

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRoutingReadsTheFileBehindALinkedParentSegment(t *testing.T) {
	h := backendHome(t)
	real := filepath.Join(h, "real")
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "sub"), filepath.Join(h, "link")); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		filepath.Join(real, "config.yaml"): "where: behind the link\n", filepath.Join(h, "config.yaml"): "where: lexical\n",
		filepath.Join(real, "import.yaml"): "where: import behind the link\n", filepath.Join(h, "import.yaml"): "where: lexical import\n",
	} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	doc, err := ReadConfigDoc(ctx, h+"/link/../config.yaml")
	if err != nil || !reflect.DeepEqual(doc, map[string]any{"where": "behind the link"}) {
		t.Fatalf("home doc %v %v", doc, err)
	}
	doc, err = ReadConfigDoc(ctx, h+"/link/../import.yaml")
	if err != nil || !reflect.DeepEqual(doc, map[string]any{"where": "import behind the link"}) {
		t.Fatalf("explicit doc %v %v", doc, err)
	}
	b, home := LocalFileLayer(h + "/link/../config.yaml")
	doc, err = b.ReadUserDoc(ctx, home)
	if err != nil || !reflect.DeepEqual(doc, map[string]any{"where": "behind the link"}) {
		t.Fatalf("local layer doc %v %v", doc, err)
	}
}
