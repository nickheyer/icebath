//go:build windows

package gateway

// Open flag refusing to follow a symlink, absent on Windows
const noFollowFlag = 0
