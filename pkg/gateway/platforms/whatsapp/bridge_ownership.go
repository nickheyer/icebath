package whatsapp

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Default profile keeps 3000 while secondary profiles allocate from this range
const (
	secondaryPortFirst = 3001
	secondaryPortLast  = 3999
)

// Whether the bridge could bind the loopback port now, TIME-WAIT leftovers read free and live listeners busy
func portIsFree(port int) bool {
	ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// Port number held in a profile record, ValueError text for anything that is not one
func readPortRecord(record string) (int, error) {
	raw, err := os.ReadFile(record)
	if err != nil {
		return 0, err
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err == nil {
		if port, convErr := strconv.Atoi(pystr.Strip(text)); convErr == nil {
			return port, nil
		}
	}
	return 0, fmt.Errorf("Bridge port record %s is not a port number; delete it to allocate a new port", record)
}

// Ports the records of sibling profiles claim, so a sibling whose bridge is down keeps its port
func siblingClaims(home string) map[int]bool {
	claims := map[int]bool{}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(home), "*", "platforms", "whatsapp", "bridge_port"))
	for _, record := range matches {
		if filepath.Dir(filepath.Dir(filepath.Dir(record))) == home {
			continue
		}
		if port, err := readPortRecord(record); err == nil {
			claims[port] = true
		}
	}
	return claims
}

// Bridge port of a multiplexed secondary profile, explicit config then its record then a fresh allocation
func SecondaryBridgePort(home string, explicit any) (int, error) {
	record := filepath.Join(home, "platforms", "whatsapp", "bridge_port")
	var port int
	if explicit != nil {
		p, err := portFromConfig(explicit)
		if err != nil {
			return 0, err
		}
		port = p
	} else if _, err := os.Stat(record); err == nil {
		if port, err = readPortRecord(record); err != nil {
			return 0, err
		}
	} else {
		claimed := siblingClaims(home)
		port = 0
		for p := secondaryPortFirst; p <= secondaryPortLast; p++ {
			if !claimed[p] && portIsFree(p) {
				port = p
				break
			}
		}
		if port == 0 {
			return 0, fmt.Errorf("No free bridge port in %d..%d", secondaryPortFirst, secondaryPortLast)
		}
		if err := os.MkdirAll(filepath.Dir(record), 0o777); err != nil {
			return 0, err
		}
		if err := os.WriteFile(record, []byte(strconv.Itoa(port)), 0o666); err != nil {
			return 0, err
		}
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("Invalid bridge port %d", port)
	}
	return port, nil
}

// Port for the launch profile and out of process sends, explicit config then the record then 3000, never allocating
func StandaloneBridgePort(home string, explicit any) (int, error) {
	var port int
	if explicit != nil {
		p, err := portFromConfig(explicit)
		if err != nil {
			return 0, err
		}
		port = p
	} else {
		record := filepath.Join(home, "platforms", "whatsapp", "bridge_port")
		if _, err := os.Stat(record); err != nil {
			return 3000, nil
		}
		p, err := readPortRecord(record)
		if err != nil {
			return 0, err
		}
		port = p
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("Invalid bridge port %d", port)
	}
	return port, nil
}

// Integer of a configured bridge_port the way int() reads it, ValueError text otherwise
func portFromConfig(v any) (int, error) {
	switch x := v.(type) {
	case int:
		return x, nil
	case int64:
		return int(x), nil
	case float64:
		return int(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		if p, err := strconv.Atoi(pystr.Strip(x)); err == nil {
			return p, nil
		}
		return 0, fmt.Errorf("invalid literal for int() with base 10: '%s'", x)
	}
	return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%T'", v)
}

// Free for an unbound port, ours when this profile's pidfile names the live process spawned on it, else an error
func CheckSecondaryOwnership(session string, port int) (string, error) {
	if portIsFree(port) {
		return "free", nil
	}
	pid, start, recordedPort := readBridgePidfile(session)
	if pid != nil && recordedPort != nil && *recordedPort == port && bridgePidIsOurs(*pid, start) {
		return "ours", nil
	}
	return "", fmt.Errorf("Bridge port %d is bound by a process this profile does not own", port)
}
