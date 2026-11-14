//go:build !windows

package tool

func normcase(p string) string {
	return p
}
