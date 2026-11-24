//go:build !windows

package logging

import (
	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
)

// LC_CTYPE codeset the stdio streams take, a C or POSIX locale coerced to utf-8
func platformStderrEncoding() string { return charsetutil.Locale() }
