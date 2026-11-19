package pm

import (
	"os"
	"path/filepath"
)

// Sorted state dirs under installs whose checkout is gone or missing and unused past retention
func OrphanInstallStates(installs string) ([]string, error) {
	if st, err := os.Stat(installs); err != nil || !st.IsDir() {
		return nil, nil
	}
	dataRoot := filepath.Dir(resolvePath(installs))
	retention, retain, err := retentionSeconds(filepath.Dir(installs))
	if err != nil {
		return nil, err
	}
	states, err := sortedChildren(installs)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, state := range states {
		orphan, err := isOrphan(state, dataRoot, retention, retain)
		if err != nil {
			return nil, err
		}
		if orphan {
			out = append(out, state)
		}
	}
	return out, nil
}
