package pypwd

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Passwd is one account record like pwd.struct_passwd
type Passwd struct {
	Name, Passwd string
	UID, GID     int
	Gecos, Dir   string
	Shell        string
}

// Group is one group record like grp.struct_group
type Group struct {
	Name, Passwd string
	GID          int
	Mem          []string
}

// KeyError is the lookup miss pwd and grp raise
type KeyError struct{ Msg string }

func (e *KeyError) Error() string { return e.Msg }

var (
	passwdPath = "/etc/passwd"
	groupPath  = "/etc/group"
)

// Fields of every well formed non comment line of a colon separated database file
func records(path string, width int) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimLeft(sc.Text(), " \t")
		if line == "" || line[0] == '#' || line[0] == '+' || line[0] == '-' {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != width {
			continue
		}
		out = append(out, fields)
	}
	return out, sc.Err()
}

// Every account of the passwd database in file order
func Getpwall() ([]Passwd, error) {
	rows, err := records(passwdPath, 7)
	if err != nil {
		return nil, err
	}
	var out []Passwd
	for _, r := range rows {
		uid, err1 := strconv.Atoi(r[2])
		gid, err2 := strconv.Atoi(r[3])
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, Passwd{Name: r[0], Passwd: r[1], UID: uid, GID: gid, Gecos: r[4], Dir: r[5], Shell: r[6]})
	}
	return out, nil
}

// First account with uid, the KeyError when none matches
func Getpwuid(uid int) (Passwd, error) {
	all, err := Getpwall()
	if err != nil {
		return Passwd{}, err
	}
	for _, p := range all {
		if p.UID == uid {
			return p, nil
		}
	}
	return Passwd{}, &KeyError{fmt.Sprintf("getpwuid(): uid not found: %d", uid)}
}

// First group with gid and its member list, the KeyError when none matches
func Getgrgid(gid int) (Group, error) {
	rows, err := records(groupPath, 4)
	if err != nil {
		return Group{}, err
	}
	for _, r := range rows {
		n, err := strconv.Atoi(r[2])
		if err != nil || n != gid {
			continue
		}
		var mem []string
		for _, m := range strings.Split(r[3], ",") {
			if m != "" {
				mem = append(mem, m)
			}
		}
		return Group{Name: r[0], Passwd: r[1], GID: n, Mem: mem}, nil
	}
	return Group{}, &KeyError{fmt.Sprintf("getgrgid(): gid not found: %d", gid)}
}
