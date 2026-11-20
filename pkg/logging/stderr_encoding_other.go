//go:build !windows

package logging

import "github.com/nickheyer/icebath/pkg/internal/pycodecs"

// LC_CTYPE codeset the stdio streams take, a C or POSIX locale coerced to utf-8
func platformStderrEncoding() string { return pycodecs.LocaleEncoding() }
