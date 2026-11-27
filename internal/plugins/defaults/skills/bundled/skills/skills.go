package skills

import (
	_ "embed"
	"io/fs"
	"os"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	bundledconfig "github.com/nickheyer/icebath/internal/plugins/defaults/skills/bundled/config"
	"github.com/nickheyer/icebath/internal/skills"
)

// The bundled skills, category folders of skill folders, packed with their executable bits
//
//go:embed bundled.tar.zst
var archive []byte

type source struct {
	files fs.FS
}

func Register(registry *skills.Registry) {
	registry.Register(source{files: skills.Archive(archive)})
}

// The bundled skills, from the folder the profile names instead when it names one that is there
func (s source) Sources(cfg *config.Config) []skills.Source {
	if dir := strings.TrimSpace(cfg.GetString(bundledconfig.Path)); dir != "" {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return nil
		}
		return []skills.Source{{Name: "bundled", Kind: skills.Seeded, Files: os.DirFS(dir), Dir: dir}}
	}
	return []skills.Source{{Name: "bundled", Kind: skills.Seeded, Files: s.files}}
}
