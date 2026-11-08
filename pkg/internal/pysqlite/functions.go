package pysqlite

import (
	"sync"
	"sync/atomic"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Scalar SQL function body receiving decoded argument values and returning one result value or an error
type ScalarFunc func(args []any) (any, error)

// Error text sqlite3 reports for a Python function that raised
const functionRaisedMsg = "user-defined function raised exception"

var (
	scalarFunctions sync.Map
	scalarHandles   atomic.Uintptr
)

// Pointer view of a C address for reads through unsafe
func cMem(p uintptr) unsafe.Pointer { return unsafe.Add(unsafe.Pointer(nil), p) }

// Go value of one sqlite3_value with the Python sqlite3 type mapping
func valueOf(tls *libc.TLS, v uintptr) any {
	switch sqlite3.Xsqlite3_value_type(tls, v) {
	case sqlite3.SQLITE_INTEGER:
		return int64(sqlite3.Xsqlite3_value_int64(tls, v))
	case sqlite3.SQLITE_FLOAT:
		return sqlite3.Xsqlite3_value_double(tls, v)
	case sqlite3.SQLITE_TEXT:
		n := sqlite3.Xsqlite3_value_bytes(tls, v)
		p := sqlite3.Xsqlite3_value_text(tls, v)
		return string(unsafe.Slice((*byte)(cMem(p)), int(n)))
	case sqlite3.SQLITE_BLOB:
		n := sqlite3.Xsqlite3_value_bytes(tls, v)
		p := sqlite3.Xsqlite3_value_blob(tls, v)
		out := make([]byte, n)
		copy(out, unsafe.Slice((*byte)(cMem(p)), int(n)))
		return out
	}
	return nil
}

// Publishes an error text as the function result
func resultError(tls *libc.TLS, ctx uintptr, msg string) {
	cs, err := libc.CString(msg)
	if err != nil {
		sqlite3.Xsqlite3_result_error_nomem(tls, ctx)
		return
	}
	defer libc.Xfree(tls, cs)
	sqlite3.Xsqlite3_result_error(tls, ctx, cs, -1)
}

// Publishes null, int, bool, float, text or blob results, any other type failing as an unsupported Python type
func setResult(tls *libc.TLS, ctx uintptr, v any) {
	switch x := v.(type) {
	case nil:
		sqlite3.Xsqlite3_result_null(tls, ctx)
	case int64:
		sqlite3.Xsqlite3_result_int64(tls, ctx, x)
	case int:
		sqlite3.Xsqlite3_result_int64(tls, ctx, int64(x))
	case bool:
		n := int64(0)
		if x {
			n = 1
		}
		sqlite3.Xsqlite3_result_int64(tls, ctx, n)
	case float64:
		sqlite3.Xsqlite3_result_double(tls, ctx, x)
	case string:
		cs, err := libc.CString(x)
		if err != nil {
			sqlite3.Xsqlite3_result_error_nomem(tls, ctx)
			return
		}
		defer libc.Xfree(tls, cs)
		sqlite3.Xsqlite3_result_text(tls, ctx, cs, int32(len(x)), sqlite3.SQLITE_TRANSIENT)
	case []byte:
		if len(x) == 0 {
			sqlite3.Xsqlite3_result_zeroblob(tls, ctx, 0)
			return
		}
		p, err := libc.CString(string(x))
		if err != nil {
			sqlite3.Xsqlite3_result_error_nomem(tls, ctx)
			return
		}
		defer libc.Xfree(tls, p)
		sqlite3.Xsqlite3_result_blob(tls, ctx, p, int32(len(x)), sqlite3.SQLITE_TRANSIENT)
	default:
		resultError(tls, ctx, functionRaisedMsg)
	}
}

// Decodes the arguments, runs the registered Go function and publishes its result or its failure
func scalarTrampoline(tls *libc.TLS, ctx uintptr, argc int32, argv uintptr) {
	fn, ok := scalarFunctions.Load(sqlite3.Xsqlite3_user_data(tls, ctx))
	if !ok {
		resultError(tls, ctx, functionRaisedMsg)
		return
	}
	args := make([]any, argc)
	for i := range args {
		args[i] = valueOf(tls, *(*uintptr)(cMem(argv + uintptr(i)*unsafe.Sizeof(uintptr(0)))))
	}
	out, err := fn.(ScalarFunc)(args)
	if err != nil {
		resultError(tls, ctx, functionRaisedMsg)
		return
	}
	setResult(tls, ctx, out)
}

// Forgets the Go function once sqlite3 drops its registration
func scalarDestroy(_ *libc.TLS, handle uintptr) { scalarFunctions.Delete(handle) }

// Registers a scalar function on the connection like sqlite3 Connection.create_function, a nil fn removing it
func CreateFunction(conn RawConn, name string, nArg int, deterministic bool, fn ScalarFunc) error {
	return conn.Raw(func(driverConn any) error {
		db, tls, err := driverHandles(driverConn)
		if err != nil {
			return err
		}
		cname, err := libc.CString(name)
		if err != nil {
			return err
		}
		defer libc.Xfree(tls, cname)
		flags := int32(sqlite3.SQLITE_UTF8)
		if deterministic {
			flags |= sqlite3.SQLITE_DETERMINISTIC
		}
		if fn == nil {
			if rc := sqlite3.Xsqlite3_create_function_v2(tls, db, cname, int32(nArg), flags, 0, 0, 0, 0, 0); rc != sqlite3.SQLITE_OK {
				return targetError(tls, db)
			}
			return nil
		}
		handle := scalarHandles.Add(1)
		scalarFunctions.Store(handle, fn)
		rc := sqlite3.Xsqlite3_create_function_v2(tls, db, cname, int32(nArg), flags, handle,
			cFuncPointer(scalarTrampoline), 0, 0, cFuncPointer(scalarDestroy))
		if rc != sqlite3.SQLITE_OK {
			scalarFunctions.Delete(handle)
			return targetError(tls, db)
		}
		return nil
	})
}
