package web

import (
	"context"
	"log/slog"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

// Whether the URL reaches cloud metadata, which the browser and media tools refuse whatever the private address settings say: an
// authority another client could read another host from, a metadata host name, or a host that is or resolves to a metadata or link
// local address. A URL that cannot be read is refused, a name that does not resolve is not
func AlwaysBlocked(raw string) bool {
	reason, err := ambiguity(raw, true)
	if err != nil {
		slog.Warn("Blocked request, unparseable URL (always-blocked floor) "+raw, "error", err)
		return true
	}
	if reason != "" {
		slog.Warn("Blocked request, ambiguous URL authority (always-blocked floor)", "reason", reason)
		return true
	}
	hostname, err := hostOf(raw)
	if err != nil {
		slog.Warn("Blocked request, unparseable URL (always-blocked floor) "+raw, "error", err)
		return true
	}
	if hostname == "" {
		return false
	}
	if slices.Contains(metadataHosts, hostname) {
		slog.Warn("Blocked request to internal hostname (always-blocked floor)", "host", hostname)
		return true
	}
	if ip, ok := literal(hostname); ok {
		if metadata(ip) {
			slog.Warn("Blocked request to cloud metadata address (always-blocked floor)", "host", hostname)
			return true
		}
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addresses, err := lookup(ctx, hostname)
	if err != nil {
		return false
	}
	for _, ip := range addresses {
		if metadata(ip) {
			slog.Warn("Blocked request to cloud metadata address (always-blocked floor)", "host", hostname, "ip", ip.String())
			return true
		}
	}
	return false
}

var numericHost = regexp.MustCompile(`^[0-9a-fx.]+$`)

// Whether a URL that names no scheme, such as //169.254.169.254/ in a script, reaches cloud metadata by its literal host, read without DNS:
// an ambiguous authority, a metadata host name, or a metadata address however its numbers are spelled
func MetadataLiteral(literalURL string) bool {
	raw := "http://" + strings.ReplaceAll(strings.TrimLeft(literalURL, `/\`), `\`, "/")
	reason, err := ambiguity(raw, true)
	if err != nil {
		return false
	}
	if reason != "" {
		return true
	}
	hostname, err := hostOf(raw)
	if err != nil {
		return false
	}
	if slices.Contains(metadataHosts, hostname) {
		return true
	}
	ip, ok := literal(hostname)
	if !ok && numericHost.MatchString(hostname) {
		ip, ok = lenientIPv4(hostname)
	}
	return ok && metadata(ip)
}

// IPv4 address of a host spelled the way inet_aton reads it: one to four parts, each decimal, octal with a leading 0 or hex with 0x, the
// last part filling the bytes the others leave
func lenientIPv4(host string) (netip.Addr, bool) {
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	values := make([]uint64, len(parts))
	for i, part := range parts {
		base := 10
		switch {
		case strings.HasPrefix(part, "0x") || strings.HasPrefix(part, "0X"):
			base, part = 16, part[2:]
		case len(part) > 1 && part[0] == '0':
			base, part = 8, part[1:]
		}
		if part == "" && base != 16 {
			return netip.Addr{}, false
		}
		n := uint64(0)
		if part != "" {
			var err error
			if n, err = strconv.ParseUint(part, base, 32); err != nil {
				return netip.Addr{}, false
			}
		}
		values[i] = n
	}
	var address uint64
	for i, n := range values[:len(values)-1] {
		if n > 255 {
			return netip.Addr{}, false
		}
		address |= n << (24 - 8*i)
	}
	last := values[len(values)-1]
	if room := 32 - 8*(len(values)-1); last >= 1<<room {
		return netip.Addr{}, false
	}
	address |= last
	return netip.AddrFrom4([4]byte{byte(address >> 24), byte(address >> 16), byte(address >> 8), byte(address)}), true
}

// Whether the address is one a local DNS proxy of the profile answers with in place of a real one
func FakeIP(cfg *config.Config, ip netip.Addr) bool {
	return within(reached(ip), fakeRanges(cfg))
}
