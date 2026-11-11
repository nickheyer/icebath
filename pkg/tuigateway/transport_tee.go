package tuigateway

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// Mirrors writes to a primary and to secondaries whose failures are swallowed so a wedged sidecar never stalls stdio
type TeeTransport struct {
	primary     Transport
	secondaries []Transport
}

// Tee of a primary transport and its sidecars
func NewTeeTransport(primary Transport, secondaries ...Transport) *TeeTransport {
	return &TeeTransport{primary: primary, secondaries: secondaries}
}

// Writes the primary first and returns its result after every secondary got the frame
func (t *TeeTransport) Write(obj *pyjson.Object) bool {
	ok := t.primary.Write(obj)
	for _, sec := range t.secondaries {
		func() {
			defer func() { _ = recover() }()
			sec.Write(obj)
		}()
	}
	return ok
}

// Closes the primary, then every secondary even when the primary panics
func (t *TeeTransport) Close() {
	defer func() {
		for _, sec := range t.secondaries {
			func() {
				defer func() { _ = recover() }()
				sec.Close()
			}()
		}
	}()
	t.primary.Close()
}
