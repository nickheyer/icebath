package gateway

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
)

// Whether path is one of the home spellings or lies under one
func pathWithinHomes(path string, homes []string) bool {
	if path == "" {
		return false
	}
	p := filepath.Clean(path)
	for _, h := range homes {
		if h == "" {
			continue
		}
		root := filepath.Clean(h)
		if p == root || strings.HasPrefix(p, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Whether the pid a record names is a live process
func recordOwnerAlive(pid int, ok bool) bool {
	return ok && pid > 0 && statusPidExists(pid)
}

// Removes the host records and scope locks of an erased home whose owner is gone, then the emptied lock dir and its icebath parent
func RemoveErasedHomeRendezvous(homes ...string) []string {
	dir := GatewayLockDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var removed []string
	remove := func(p string) {
		if os.Remove(p) == nil {
			removed = append(removed, p)
		}
	}
	for _, e := range entries {
		name := e.Name()
		p := filepath.Join(dir, name)
		switch {
		case strings.HasPrefix(name, "host-") && strings.HasSuffix(name, ".json"):
			record := readJSONFile(p, false)
			recorded, _ := recGet(record, "home").(string)
			pid, ok := pidFromRecord(record, "pid")
			if !pathWithinHomes(recorded, homes) || recordOwnerAlive(pid, ok) {
				continue
			}
			remove(p)
			remove(strings.TrimSuffix(p, ".json") + ".lock")
		case strings.HasSuffix(name, ".lock") && !strings.HasPrefix(name, "host-"):
			record := readJSONFile(p, false)
			recorded, _ := recHome(record, home.RecordKey).(string)
			pid, ok := pidFromRecord(record, "pid")
			if !pathWithinHomes(recorded, homes) || recordOwnerAlive(pid, ok) {
				continue
			}
			remove(p)
		}
	}
	if os.Remove(dir) != nil {
		return removed
	}
	removed = append(removed, dir)
	if os.Getenv("ICEBATH_GATEWAY_LOCK_DIR") == "" {
		remove(filepath.Dir(dir))
	}
	return removed
}
