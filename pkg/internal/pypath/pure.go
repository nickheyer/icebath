package pypath

import "strings"

// str of a pathlib PurePath of the host built from one string
func PurePathStr(p string) string {
	if windows {
		return NtPurePathStr(p)
	}
	return PosixPurePathStr(p)
}

// str of a PurePosixPath, empty and dot components dropped and exactly two leading slashes kept
func PosixPurePathStr(p string) string {
	root := ""
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		root = "//"
	case strings.HasPrefix(p, "/"):
		root = "/"
	}
	return joinPure(root, strings.Split(p, "/"), "/")
}

// str of a PureWindowsPath, separators turned to backslashes and empty and dot components dropped
func NtPurePathStr(p string) string {
	drive, root, rest := NtSplitRoot(strings.ReplaceAll(p, "/", `\`))
	return joinPure(drive+root, strings.Split(rest, `\`), `\`)
}

func joinPure(anchor string, parts []string, sep string) string {
	kept := []string{}
	for _, part := range parts {
		if part != "" && part != "." {
			kept = append(kept, part)
		}
	}
	if anchor == "" && len(kept) == 0 {
		return "."
	}
	return anchor + strings.Join(kept, sep)
}
