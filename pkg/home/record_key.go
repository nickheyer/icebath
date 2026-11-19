package home

import "strings"

// Key of the home directory in pid, lock, identity and runtime records
const RecordKey = "icebath_home"

// Same record key in the spelling records of an older icebath carry
func RetiredRecordKey(key string) string { return strings.Replace(key, RecordKey, "hermes_home", 1) }

// Moves the home of a record an older icebath wrote to the current key in place, so the next write carries it
func AdoptRecordKey(keys []string, vals map[string]any) {
	retired := RetiredRecordKey(RecordKey)
	if _, has := vals[RecordKey]; has {
		return
	}
	for i, k := range keys {
		if k == retired {
			keys[i] = RecordKey
			vals[RecordKey] = vals[retired]
			delete(vals, retired)
			return
		}
	}
}

// Value of a home record key, the older spelling read from a record written before the rename
func RecordValue(lookup func(string) (any, bool), key string) (any, bool) {
	if v, ok := lookup(key); ok {
		return v, true
	}
	return lookup(RetiredRecordKey(key))
}
