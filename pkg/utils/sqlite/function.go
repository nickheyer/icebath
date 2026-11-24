package sqliteutil

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Body of a scalar SQL function: it receives the arguments as nil, int64, float64, string or []byte and answers nil, a bool, an integer, a float, a string or a []byte, or an error that becomes the statement's error
type ScalarFunc func(args []any) (any, error)

// One registered function with its SQL name
type function struct {
	name string
	fn   ScalarFunc
}

var (
	functions   sync.Map
	functionIDs atomic.Uintptr
)

// Result of the function body, a panic turned into an error so it never unwinds through SQLite
func (f *function) call(args []any) (out any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("function %s panicked: %v", f.name, r)
		}
	}()
	return f.fn(args)
}

// Decodes the arguments, runs the registered function and publishes its result or its error
func scalarTrampoline(tls *libc.TLS, ctx uintptr, argc int32, argv uintptr) {
	v, ok := functions.Load(sqlite3.Xsqlite3_user_data(tls, ctx))
	if !ok {
		resultError(tls, ctx, "function is no longer registered")
		return
	}
	f := v.(*function)
	args := make([]any, argc)
	for i := range args {
		args[i] = argValue(tls, *(*uintptr)(cPtr(argv + uintptr(i)*unsafe.Sizeof(uintptr(0)))))
	}
	out, err := f.call(args)
	if err != nil {
		resultError(tls, ctx, err.Error())
		return
	}
	setResult(tls, ctx, f.name, out)
}

// Forgets the Go function once SQLite drops its registration
func functionDestroy(_ *libc.TLS, id uintptr) { functions.Delete(id) }

// Go value of one sqlite3_value, text and blobs copied out of SQLite's memory
func argValue(tls *libc.TLS, v uintptr) any {
	switch sqlite3.Xsqlite3_value_type(tls, v) {
	case sqlite3.SQLITE_INTEGER:
		return sqlite3.Xsqlite3_value_int64(tls, v)
	case sqlite3.SQLITE_FLOAT:
		return sqlite3.Xsqlite3_value_double(tls, v)
	case sqlite3.SQLITE_TEXT:
		p := sqlite3.Xsqlite3_value_text(tls, v)
		n := sqlite3.Xsqlite3_value_bytes(tls, v)
		if p == 0 || n <= 0 {
			return ""
		}
		return string(unsafe.Slice((*byte)(cPtr(p)), n))
	case sqlite3.SQLITE_BLOB:
		p := sqlite3.Xsqlite3_value_blob(tls, v)
		n := sqlite3.Xsqlite3_value_bytes(tls, v)
		out := make([]byte, max(n, 0))
		if p != 0 && n > 0 {
			copy(out, unsafe.Slice((*byte)(cPtr(p)), n))
		}
		return out
	}
	return nil
}

// Publishes the function's answer, an unsupported type or an integer past 64 signed bits failing the call
func setResult(tls *libc.TLS, ctx uintptr, name string, v any) {
	switch x := v.(type) {
	case nil:
		sqlite3.Xsqlite3_result_null(tls, ctx)
	case bool:
		n := int64(0)
		if x {
			n = 1
		}
		sqlite3.Xsqlite3_result_int64(tls, ctx, n)
	case int64:
		sqlite3.Xsqlite3_result_int64(tls, ctx, x)
	case int:
		sqlite3.Xsqlite3_result_int64(tls, ctx, int64(x))
	case int32:
		sqlite3.Xsqlite3_result_int64(tls, ctx, int64(x))
	case int16:
		sqlite3.Xsqlite3_result_int64(tls, ctx, int64(x))
	case int8:
		sqlite3.Xsqlite3_result_int64(tls, ctx, int64(x))
	case uint8:
		sqlite3.Xsqlite3_result_int64(tls, ctx, int64(x))
	case uint16:
		sqlite3.Xsqlite3_result_int64(tls, ctx, int64(x))
	case uint32:
		sqlite3.Xsqlite3_result_int64(tls, ctx, int64(x))
	case uint:
		resultUint(tls, ctx, name, uint64(x))
	case uint64:
		resultUint(tls, ctx, name, x)
	case float64:
		sqlite3.Xsqlite3_result_double(tls, ctx, x)
	case float32:
		sqlite3.Xsqlite3_result_double(tls, ctx, float64(x))
	case string:
		resultText(tls, ctx, x)
	case []byte:
		resultBlob(tls, ctx, x)
	default:
		resultError(tls, ctx, fmt.Sprintf("function %s returned a value of unsupported type %T", name, v))
	}
}

func resultUint(tls *libc.TLS, ctx uintptr, name string, n uint64) {
	if n > math.MaxInt64 {
		resultError(tls, ctx, fmt.Sprintf("function %s returned %d, past the 64-bit signed integer range", name, n))
		return
	}
	sqlite3.Xsqlite3_result_int64(tls, ctx, int64(n))
}

// Publishes text, SQLite taking its own copy
func resultText(tls *libc.TLS, ctx uintptr, s string) {
	if len(s) > math.MaxInt32 {
		sqlite3.Xsqlite3_result_error_toobig(tls, ctx)
		return
	}
	p, err := libc.CString(s)
	if err != nil {
		sqlite3.Xsqlite3_result_error_nomem(tls, ctx)
		return
	}
	defer libc.Xfree(tls, p)
	sqlite3.Xsqlite3_result_text(tls, ctx, p, int32(len(s)), sqlite3.SQLITE_TRANSIENT)
}

// Publishes a blob, an empty one as a zero-length blob rather than NULL, SQLite taking its own copy
func resultBlob(tls *libc.TLS, ctx uintptr, b []byte) {
	if len(b) == 0 {
		sqlite3.Xsqlite3_result_zeroblob(tls, ctx, 0)
		return
	}
	if len(b) > math.MaxInt32 {
		sqlite3.Xsqlite3_result_error_toobig(tls, ctx)
		return
	}
	p, err := libc.CString(unsafe.String(unsafe.SliceData(b), len(b)))
	if err != nil {
		sqlite3.Xsqlite3_result_error_nomem(tls, ctx)
		return
	}
	defer libc.Xfree(tls, p)
	sqlite3.Xsqlite3_result_blob(tls, ctx, p, int32(len(b)), sqlite3.SQLITE_TRANSIENT)
}

// Publishes msg as the function's error
func resultError(tls *libc.TLS, ctx uintptr, msg string) {
	p, err := libc.CString(msg)
	if err != nil {
		sqlite3.Xsqlite3_result_error_nomem(tls, ctx)
		return
	}
	defer libc.Xfree(tls, p)
	sqlite3.Xsqlite3_result_error(tls, ctx, p, int32(min(len(msg), math.MaxInt32)))
}

// Registers fn on conn as the SQL function name taking nArg arguments (-1 for any number), replacing one of the same name and count; deterministic lets SQLite treat equal arguments as giving equal results, so the function may serve in indexes and checks; a nil fn removes the function
func CreateFunction(conn RawConn, name string, nArg int, deterministic bool, fn ScalarFunc) error {
	return withHandle(conn, func(h handle) error {
		cname, err := libc.CString(name)
		if err != nil {
			return err
		}
		defer libc.Xfree(h.tls, cname)
		flags := int32(sqlite3.SQLITE_UTF8)
		if deterministic {
			flags |= sqlite3.SQLITE_DETERMINISTIC
		}
		args := int32(max(-2, min(nArg, math.MaxInt16)))
		if fn == nil {
			if rc := sqlite3.Xsqlite3_create_function_v2(h.tls, h.db, cname, args, flags, 0, 0, 0, 0, 0); rc != sqlite3.SQLITE_OK {
				return codeError(h.tls, h.db, rc)
			}
			return nil
		}
		id := functionIDs.Add(1)
		functions.Store(id, &function{name: name, fn: fn})
		rc := sqlite3.Xsqlite3_create_function_v2(h.tls, h.db, cname, args, flags, id,
			cFuncPointer(scalarTrampoline), 0, 0, cFuncPointer(functionDestroy))
		if rc != sqlite3.SQLITE_OK {
			functions.Delete(id)
			return codeError(h.tls, h.db, rc)
		}
		return nil
	})
}
