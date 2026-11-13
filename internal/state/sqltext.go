package state

import "strings"

// Ids one IN list binds at most, below the bound parameter cap of every SQLite build
const sqlInChunk = 900

// Epoch window a stored timestamp must lie in to be trusted
const (
	epochMin = "0.0"
	epochMax = "4200000000.0"
)

// LIKE pattern text matching s literally, paired with ESCAPE backslash
func EscapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Comma separated bound parameter marks, one per value
func bindMarks(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// Values split into lists of at most size elements, the last one shorter
func chunks[T any](values []T, size int) [][]T {
	var out [][]T
	for start := 0; start < len(values); start += size {
		out = append(out, values[start:min(start+size, len(values))])
	}
	return out
}

// SQL expression yielding expr when it lies inside the trusted epoch window and NULL otherwise
func sqlInWindow(expr string) string {
	return "(SELECT _win.v FROM (SELECT " + expr + " AS v) _win WHERE _win.v BETWEEN " + epochMin + " AND " + epochMax + ")"
}
