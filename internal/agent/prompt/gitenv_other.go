//go:build !windows

package prompt

func envKey(k string) string { return k }
