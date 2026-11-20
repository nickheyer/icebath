//go:build windows

package tools

// O_EXCL alone refuses every pre existing path on Windows
const spillNoFollowFlag = 0
