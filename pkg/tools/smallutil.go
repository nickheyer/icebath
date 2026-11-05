package tools

import (
	"sort"
	"strconv"
)

func inList(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func itoa(n int) string { return strconv.Itoa(n) }

func sortStrings(s []string) { sort.Strings(s) }
