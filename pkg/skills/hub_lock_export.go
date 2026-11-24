package skills

import (
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Installed entries keyed by identifier in lock order, ClawHub catalog aliases after them, exact ids winning
func (l *HubLockFile) ByIdentifierObject() (*jsonutil.Object, error) {
	rows, err := l.ListInstalled()
	if err != nil {
		return nil, err
	}
	installed := jsonutil.NewObject()
	var kept []*jsonutil.Object
	for _, entry := range rows {
		id, _ := entry.Get("identifier")
		if !anyutil.IsTrue(id) {
			continue
		}
		kept = append(kept, entry)
		installed.Set(anyutil.String(id), entry)
	}
	for _, entry := range kept {
		if src, _ := entry.Get("source"); src == "clawhub" {
			id, _ := entry.Get("identifier")
			if alias, ok := clawhubCatalogIdentifier(id); ok {
				if _, exists := installed.Get(alias); !exists {
					installed.Set(alias, entry)
				}
			}
		}
	}
	return installed, nil
}
