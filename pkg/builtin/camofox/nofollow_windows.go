//go:build windows

package camofox

// Windows has no O_NOFOLLOW so the open carries no extra flag
const openNoFollow = 0
