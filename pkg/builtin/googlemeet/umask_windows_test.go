//go:build windows

package googlemeet

func umask(m int) int { return m }
