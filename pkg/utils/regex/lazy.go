package regexutil

import "sync"

// Function that compiles pattern with compile on its first call and hands back that same value on every later call, safe for concurrent first use, keeping package level patterns out of program start-up; a compile that panics panics again with the same value on every call
func Lazy[T any](compile func(string) T, pattern string) func() T {
	return sync.OnceValue(func() T { return compile(pattern) })
}
