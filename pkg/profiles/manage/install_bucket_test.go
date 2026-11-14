package manage

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
)

// Archive members by name with their contents
func archiveContents(t *testing.T, archive string) map[string]string {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = string(b)
	}
	return out
}

func TestCopiesProfilePayloadWithoutInstallArtifacts(t *testing.T) {
	for _, op := range []string{"clone", "export", "distribution"} {
		t.Run(op, func(t *testing.T) {
			tmp := manageEnv(t)
			captureOut(t)
			old := maybeRegisterGatewayService
			t.Cleanup(func() { maybeRegisterGatewayService = old })
			maybeRegisterGatewayService = func(string) {}
			h := filepath.Join(tmp, ".icebath")
			kept := map[string]string{"config.yaml": "model: {}\n", "SOUL.md": "profile identity\n",
				"skills/demo/SKILL.md": "demo instructions\n"}
			if op != "distribution" {
				kept["memories/MEMORY.md"] = "profile memory\n"
			}
			excluded := []string{".icebath-runtime", "node", "icebath-agent", "profiles"}
			for rel, content := range kept {
				writeFile(t, filepath.Join(h, filepath.FromSlash(rel)), content)
			}
			for _, name := range excluded {
				writeFile(t, filepath.Join(h, name, "must-not-copy"), "install state")
			}
			ctx := context.Background()
			var target string
			switch op {
			case "clone":
				from := "default"
				dir, err := CreateProfile(ctx, "clone", CreateOptions{CloneFrom: &from, CloneAll: true})
				if err != nil {
					t.Fatal(err)
				}
				target = dir
			case "distribution":
				m := profiles.NewDistributionManifest("copy")
				m.Version = "1.0.0"
				if _, err := profiles.WriteManifest(ctx, h, m); err != nil {
					t.Fatal(err)
				}
				if _, err := InstallDistribution(ctx, h, "copy", false, false); err != nil {
					t.Fatal(err)
				}
				dir, err := profiles.GetProfileDir("copy")
				if err != nil {
					t.Fatal(err)
				}
				target = dir
			default:
				archive, err := ExportProfile("default", filepath.Join(tmp, "profile.tar.gz"), nil)
				if err != nil {
					t.Fatal(err)
				}
				members := archiveContents(t, archive)
				for rel, content := range kept {
					got, ok := members["default/"+rel]
					if !ok || got != content {
						t.Fatalf("%s: %q %v", rel, got, ok)
					}
				}
				for name := range members {
					parts := strings.Split(name, "/")
					for _, ex := range excluded {
						if len(parts) > 1 && parts[1] == ex {
							t.Fatalf("install artifact %s exported", name)
						}
					}
				}
				return
			}
			for rel, content := range kept {
				if readText(t, filepath.Join(target, filepath.FromSlash(rel))) != content {
					t.Fatal(rel)
				}
			}
			for _, name := range excluded {
				if pathExists(filepath.Join(target, name)) {
					t.Fatal(name)
				}
			}
		})
	}
}
