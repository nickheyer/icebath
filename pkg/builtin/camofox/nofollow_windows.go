//go:build windows

package camofox

// Windows has no O_NOFOLLOW, the open runs with no extra flag as getattr(os, "O_NOFOLLOW", 0) does
const openNoFollow = 0
