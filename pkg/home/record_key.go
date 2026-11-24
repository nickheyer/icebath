package home

import (
	"strings"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Key of the home directory in pid, lock, identity and runtime records
const RecordKey = "icebath_home"

// Same record key in the spelling records of an older icebath carry
func RetiredRecordKey(key string) string { return strings.Replace(key, RecordKey, "hermes_home", 1) }

// Moves the home of a record an older icebath wrote to the current key in place, so the next write carries it
func AdoptRecordKey(record *jsonutil.Object) {
	if !record.Has(RecordKey) {
		record.Rename(RetiredRecordKey(RecordKey), RecordKey)
	}
}

// Value of a home record key, the older spelling read from a record written before the rename
func RecordValue(lookup func(string) (any, bool), key string) (any, bool) {
	if v, ok := lookup(key); ok {
		return v, true
	}
	return lookup(RetiredRecordKey(key))
}
