package httpclient

import "strings"

// Variable name folded to one case since Windows names ignore case
func envKey(name string) string { return strings.ToUpper(name) }
