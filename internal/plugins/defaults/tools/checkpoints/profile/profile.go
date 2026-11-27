package profile

import (
	"fmt"

	"github.com/nickheyer/icebath/internal/checkpoints"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/profile"
)

// Checkpoints as profiles come and go: kept in the profile's folder, so deleted with it, and their projects moved to their new paths when
// it is renamed
type registrant struct {
	config *config.Config
}

func Register(registry *profile.Registry, cfg *config.Config) {
	registry.Register(registrant{config: cfg})
}

// Checkpoints live in the profile's folder, which goes with it
func (registrant) Delete(string) error {
	return nil
}

// Moves the projects that lay in the old folder of the profile to the keys of their folders in its new one, saying how many moved and how
// many would not
func (r registrant) Move(old, renamed string) ([]string, error) {
	migrated, err := checkpoints.Open(r.config.In(renamed)).Rekey(r.config.ProfileDir(old), r.config.ProfileDir(renamed))
	var lines []string
	if migrated > 0 {
		lines = append(lines, fmt.Sprintf("✓ Checkpoint identity updated: %d project(s)", migrated))
	}
	if err == nil {
		return lines, nil
	}
	failed := 1
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		failed = len(joined.Unwrap())
	}
	return lines, profile.Worded(fmt.Sprintf("Profile was renamed, but checkpoint identity migration failed for %d project(s).", failed))
}
