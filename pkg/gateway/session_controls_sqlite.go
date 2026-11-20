package gateway

import "github.com/nickheyer/icebath/pkg/state"

// Python sqlite3 exception class names by class
var sqliteClassNames = map[state.SQLiteClass]string{state.SQLiteDatabaseError: "DatabaseError",
	state.SQLiteOperationalError: "OperationalError", state.SQLiteIntegrityError: "IntegrityError",
	state.SQLiteInterfaceError: "InterfaceError", state.SQLiteInternalError: "InternalError",
	state.SQLiteDataError: "DataError", state.SQLiteProgrammingError: "ProgrammingError",
	state.SQLiteNotSupportedError: "NotSupportedError"}

// Whether an error is a sqlite3.Error of any class
func isSQLiteError(err error) bool {
	_, ok := state.SQLiteClassOf(err)
	return ok
}

// Class name of a sqlite3 error, the type name a storage failure log keys on
func sqliteErrorClass(err error) string {
	class, ok := state.SQLiteClassOf(err)
	if !ok {
		return "ProgrammingError"
	}
	return sqliteClassNames[class]
}
