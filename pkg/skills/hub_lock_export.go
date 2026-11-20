package skills

import (
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Installed entries keyed by identifier in lock order, ClawHub catalog aliases after them, exact ids winning
func (l *HubLockFile) ByIdentifierObject() (*pyjson.Object, error) {
	rows, err := l.ListInstalled()
	if err != nil {
		return nil, err
	}
	installed := pyjson.NewObject()
	var kept []*pyjson.Object
	for _, entry := range rows {
		id, _ := entry.Get("identifier")
		if !pyrepr.Truthy(id) {
			continue
		}
		kept = append(kept, entry)
		installed.Set(pyrepr.Str(id), entry)
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
