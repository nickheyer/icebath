package pysys

import "io"

// Capability of a stream the way getattr finds it, on the stream first then on each stream it wraps
func Attr[T any](stream any) (T, bool) {
	for stream != nil {
		if v, ok := stream.(T); ok {
			return v, true
		}
		inner, ok := stream.(interface{ Unwrap() io.Writer })
		if !ok {
			break
		}
		stream = inner.Unwrap()
	}
	var zero T
	return zero, false
}

// Reports stream.isatty(), the stream's own answer first, else whether its descriptor is a terminal
func Isatty(stream any) bool {
	if s, ok := Attr[interface{ IsATTY() bool }](stream); ok {
		return s.IsATTY()
	}
	if fd, ok := Fileno(stream); ok {
		return fdIsatty(fd)
	}
	return false
}

// Descriptor stream.fileno() returns, false when neither the stream nor what it wraps has one
func Fileno(stream any) (uintptr, bool) {
	if f, ok := Attr[interface{ Fd() uintptr }](stream); ok {
		return f.Fd(), true
	}
	return 0, false
}
