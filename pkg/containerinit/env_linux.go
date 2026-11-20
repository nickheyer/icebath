package containerinit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Writes every variable of the container environment as one file, the snapshot with-contenv restores
func saveContainerEnv(dir string, environ []string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" || strings.ContainsAny(name, "/\x00") {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Environment with the snapshot applied over base, an empty file unsetting its variable
func loadContainerEnv(dir string, base []string) []string {
	env := map[string]string{}
	order := []string{}
	for _, kv := range base {
		name, value, _ := strings.Cut(kv, "=")
		if _, seen := env[name]; !seen {
			order = append(order, name)
		}
		env[name] = value
	}
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			if len(raw) == 0 {
				delete(env, e.Name())
				continue
			}
			if _, seen := env[e.Name()]; !seen {
				order = append(order, e.Name())
			}
			env[e.Name()] = string(raw)
		}
	}
	out := make([]string, 0, len(env))
	for _, name := range order {
		if v, ok := env[name]; ok {
			out = append(out, name+"="+v)
		}
	}
	return out
}

// Replaces this process with prog found on PATH, printing the verb's fatal line when it cannot
func execProgram(verb string, argv, env []string) int {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: fatal: unable to exec %s: %v\n", verb, argv[0], err)
		return 127
	}
	err = syscall.Exec(path, argv, env)
	fmt.Fprintf(os.Stderr, "%s: fatal: unable to exec %s: %v\n", verb, argv[0], err)
	return 126
}

// Runs the rest of argv with the saved container environment unless S6_KEEP_ENV keeps the current one
func runWithContenv(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "with-contenv: usage: with-contenv prog...")
		return 100
	}
	env := os.Environ()
	if keep := os.Getenv("S6_KEEP_ENV"); keep == "" || keep == "0" {
		env = loadContainerEnv(containerEnv, env)
	}
	return execProgram("with-contenv", args, env)
}

// Runs prog as the account named by a user name or a uid and gid pair with its supplementary groups
func runSetuidgid(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "s6-setuidgid: usage: s6-setuidgid account prog...")
		return 100
	}
	var a account
	if uidS, gidS, ok := strings.Cut(args[0], ":"); ok {
		var uid, gid int
		if _, err := fmt.Sscanf(uidS+" "+gidS, "%d %d", &uid, &gid); err != nil {
			fmt.Fprintf(os.Stderr, "s6-setuidgid: fatal: invalid uid:gid %s\n", args[0])
			return 100
		}
		a = account{uid: uid, gid: gid, groups: []int{gid}}
	} else {
		found, err := lookupAccount(args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "s6-setuidgid: fatal: unknown user: %s\n", args[0])
			return 1
		}
		a = found
	}
	if err := a.apply(); err != nil {
		fmt.Fprintf(os.Stderr, "s6-setuidgid: fatal: unable to set credentials: %v\n", err)
		return 111
	}
	return execProgram("s6-setuidgid", args[1:], os.Environ())
}
