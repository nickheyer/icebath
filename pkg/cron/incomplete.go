package cron

import (
	"github.com/nickheyer/icebath/pkg/tools"
)

// Registers the cronjob_manage tool once tools/cronjob_tools.py lands
func init() {
	tools.Register(cronjobManageToolDef())
}

// Entry points of the unported modules, kept referenced so the package fails to build until they exist
var (
	_ = func() []AutomationBlueprint { return listBlueprints() }
	_ = func(s *Store, job *Job) [][2]string { return cronListRows(s, job) }
	_ = func(s *Store) []string { return cronStatusLines(s) }
	_ = func(arg string) []string { return cronCommandLines(arg) }
	_ = func(s *Store, snapshotID string) map[string]any {
		return restoreCronPromptFieldsIfDegraded(s, snapshotID)
	}
	_ = func() Provider { return newChronosProvider() }
)
