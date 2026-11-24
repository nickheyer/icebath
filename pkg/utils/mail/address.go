package mailutil

import (
	"mime"
	"net/mail"
	"strings"

	"golang.org/x/net/idna"
)

var addressParser = &mail.AddressParser{WordDecoder: &mime.WordDecoder{CharsetReader: charsetReader}}

// Mailbox of one address such as Name <user@example.com>, its display name decoded from RFC 2047 in any character set; an error for a list, a group or text that is no address
func ParseAddress(s string) (*mail.Address, error) { return addressParser.Parse(s) }

// Mailboxes of an address list, the members of groups included
func parseAddressList(s string) ([]*mail.Address, error) { return addressParser.ParseList(s) }

// Address with an internationalized domain written in punycode, and whether it still needs SMTPUTF8 for a local part or a domain that stays outside ASCII
func asciiAddress(addr string) (string, bool) {
	if isASCII(addr) {
		return addr, false
	}
	at := strings.LastIndexByte(addr, '@')
	if at < 0 {
		return addr, true
	}
	local, domain := addr[:at], addr[at+1:]
	if !isASCII(domain) {
		if ascii, err := idna.Lookup.ToASCII(domain); err == nil {
			domain = ascii
		}
	}
	return local + "@" + domain, !isASCII(local) || !isASCII(domain)
}

// Address field value with display names outside ASCII encoded and internationalized domains in punycode, false when the value is no address list
func formatAddressList(value string) (string, bool) {
	list, err := parseAddressList(value)
	if err != nil || len(list) == 0 {
		return "", false
	}
	formatted := make([]string, len(list))
	for i, a := range list {
		a.Address, _ = asciiAddress(a.Address)
		formatted[i] = a.String()
	}
	return strings.Join(formatted, ", "), true
}
