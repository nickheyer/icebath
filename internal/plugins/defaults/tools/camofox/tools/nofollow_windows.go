//go:build windows

package tools

// Windows has no O_NOFOLLOW; the file is removed and made anew instead
const noFollow = 0
