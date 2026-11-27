package skills

import (
	_ "embed"
	"io/fs"
	"os"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	optionalconfig "github.com/nickheyer/icebath/internal/plugins/defaults/skills/optional/config"
	"github.com/nickheyer/icebath/internal/skills"
)

// The official optional skills, category folders of skill folders, packed with their executable bits
//
//go:embed optional.tar.zst
var archive []byte

type source struct {
	files fs.FS
}

func Register(registry *skills.Registry) {
	registry.Register(source{files: skills.Archive(archive)})
}

// The official optional skills, from the folder the profile names instead when it names one that is there
func (s source) Sources(cfg *config.Config) []skills.Source {
	if dir := strings.TrimSpace(cfg.GetString(optionalconfig.Path)); dir != "" {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return nil
		}
		return []skills.Source{{Name: "official", Kind: skills.Offered, Files: os.DirFS(dir), Dir: dir}}
	}
	return []skills.Source{{Name: "official", Kind: skills.Offered, Files: s.files}}
}
