package sqliteutil

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"unsafe"

	"modernc.org/libc"
)

// Connection whose driver connection can be borrowed, as *sql.Conn and the types wrapping one give it
type RawConn interface {
	Raw(func(driverConn any) error) error
}

// Error of a driver connection that is not a modernc.org/sqlite connection, wrapped with its type
var ErrUnsupportedConn = errors.New("connection is not a modernc.org/sqlite connection")

// Error of a driver connection whose database is already closed
var ErrClosed = errors.New("sqlite connection is closed")

// Database handle and libc thread state of one driver connection, with the address of the driver connection that owns them
type handle struct {
	db    uintptr
	tls   *libc.TLS
	owner unsafe.Pointer
}

// Runs fn with the SQLite database handle and libc thread state behind conn while database/sql holds the connection for this call alone
func WithHandle(conn RawConn, fn func(tls *libc.TLS, db uintptr) error) error {
	return withHandle(conn, func(h handle) error { return fn(h.tls, h.db) })
}

func withHandle(conn RawConn, fn func(h handle) error) error {
	return conn.Raw(func(driverConn any) error {
		h, err := handleOf(driverConn)
		if err != nil {
			return err
		}
		return fn(h)
	})
}

// Positions of the db and tls fields in the driver's connection struct, ok false for a type that has no such layout
type connLayout struct {
	db, tls int
	ok      bool
}

// Layout of each driver connection type seen, keyed by its pointer type
var layouts sync.Map

// Handles the modernc.org/sqlite driver keeps in the unexported db and tls fields of its connection
func handleOf(driverConn any) (handle, error) {
	v := reflect.ValueOf(driverConn)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return handle{}, fmt.Errorf("%w: %T", ErrUnsupportedConn, driverConn)
	}
	l := layoutOf(v.Type())
	if !l.ok {
		return handle{}, fmt.Errorf("%w: %T", ErrUnsupportedConn, driverConn)
	}
	s := v.Elem()
	h := handle{
		db:    uintptr(s.Field(l.db).Uint()),
		tls:   (*libc.TLS)(s.Field(l.tls).UnsafePointer()),
		owner: v.UnsafePointer(),
	}
	if h.db == 0 || h.tls == nil {
		return handle{}, ErrClosed
	}
	return h, nil
}

func layoutOf(t reflect.Type) connLayout {
	if l, ok := layouts.Load(t); ok {
		return l.(connLayout)
	}
	var l connLayout
	if e := t.Elem(); e.Kind() == reflect.Struct && e.PkgPath() == "modernc.org/sqlite" {
		db, okDB := e.FieldByName("db")
		tls, okTLS := e.FieldByName("tls")
		if okDB && okTLS && len(db.Index) == 1 && len(tls.Index) == 1 &&
			db.Type.Kind() == reflect.Uintptr && tls.Type == reflect.TypeFor[*libc.TLS]() {
			l = connLayout{db: db.Index[0], tls: tls.Index[0], ok: true}
		}
	}
	layouts.Store(t, l)
	return l
}

// Pointer to C memory at address p, made without a uintptr conversion vet would question
func cPtr(p uintptr) unsafe.Pointer { return unsafe.Add(unsafe.Pointer(nil), p) }

// C function pointer of a Go function declaration laid out as modernc callbacks expect; undefined for closures
func cFuncPointer[T any](f T) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct{ f T }{f}))
}
