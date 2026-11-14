package pyemail

import "strings"

// Address is one parsed mailbox, its display name and addr-spec
type Address struct {
	Name string
	Addr string
}

// RFC 2822 address list parser with the rules of the email._parseaddr AddrlistClass
type addrlist struct {
	field       []rune
	pos         int
	commentlist []string
}

const (
	addrSpecials    = "()<>@,:;.\"[]"
	addrLWS         = " \t"
	addrCR          = "\r\n"
	addrFWS         = addrLWS + addrCR
	addrAtomEnds    = addrSpecials + addrLWS + addrCR
	addrPhraseEnds  = "()<>@,:;\"[]" + addrLWS + addrCR
	addrDelimEscape = '\\'
)

func in(set string, r rune) bool { return strings.ContainsRune(set, r) }

// Skips white space and collects comments, returning the white space other than line breaks
func (a *addrlist) gotonext() string {
	var ws []rune
	for a.pos < len(a.field) {
		c := a.field[a.pos]
		if in(addrLWS+"\n\r", c) {
			if !in("\n\r", c) {
				ws = append(ws, c)
			}
			a.pos++
		} else if c == '(' {
			a.commentlist = append(a.commentlist, a.getcomment())
		} else {
			break
		}
	}
	return string(ws)
}

// Every address of the field, an empty pair standing for each one that does not parse
func (a *addrlist) getaddrlist() []Address {
	var result []Address
	for a.pos < len(a.field) {
		if ad := a.getaddress(); len(ad) > 0 {
			result = append(result, ad...)
		} else {
			result = append(result, Address{})
		}
	}
	return result
}

// The next address, or the members of the next group
func (a *addrlist) getaddress() []Address {
	a.commentlist = nil
	a.gotonext()
	oldpos, oldcl := a.pos, a.commentlist
	plist := a.getphraselist()
	a.gotonext()
	var returnlist []Address
	switch {
	case a.pos >= len(a.field):
		if len(plist) > 0 {
			returnlist = []Address{{strings.Join(a.commentlist, " "), plist[0]}}
		}
	case in(".@", a.field[a.pos]):
		a.pos, a.commentlist = oldpos, oldcl
		addrspec := a.getaddrspec()
		returnlist = []Address{{strings.Join(a.commentlist, " "), addrspec}}
	case a.field[a.pos] == ':':
		fieldlen := len(a.field)
		a.pos++
		for a.pos < len(a.field) {
			a.gotonext()
			if a.pos < fieldlen && a.field[a.pos] == ';' {
				a.pos++
				break
			}
			returnlist = append(returnlist, a.getaddress()...)
		}
	case a.field[a.pos] == '<':
		routeaddr := a.getrouteaddr()
		if len(a.commentlist) > 0 {
			returnlist = []Address{{strings.Join(plist, " ") + " (" + strings.Join(a.commentlist, " ") + ")", routeaddr}}
		} else {
			returnlist = []Address{{strings.Join(plist, " "), routeaddr}}
		}
	default:
		if len(plist) > 0 {
			returnlist = []Address{{strings.Join(a.commentlist, " "), plist[0]}}
		} else if in(addrSpecials, a.field[a.pos]) {
			a.pos++
		}
	}
	a.gotonext()
	if a.pos < len(a.field) && a.field[a.pos] == ',' {
		a.pos++
	}
	return returnlist
}

// The addr-spec of a route address, the route itself skipped
func (a *addrlist) getrouteaddr() string {
	if a.field[a.pos] != '<' {
		return ""
	}
	expectroute := false
	a.pos++
	a.gotonext()
	adlist := ""
	for a.pos < len(a.field) {
		if expectroute {
			a.getdomain()
			expectroute = false
		} else if a.field[a.pos] == '>' {
			a.pos++
			break
		} else if a.field[a.pos] == '@' {
			a.pos++
			expectroute = true
		} else if a.field[a.pos] == ':' {
			a.pos++
		} else {
			adlist = a.getaddrspec()
			a.pos++
			break
		}
		a.gotonext()
	}
	return adlist
}

// An RFC 2822 addr-spec, empty when its domain is missing
func (a *addrlist) getaddrspec() string {
	var aslist []string
	popBlank := func() {
		if n := len(aslist); n > 0 && strings.TrimSpace(aslist[n-1]) == "" {
			aslist = aslist[:n-1]
		}
	}
	a.gotonext()
	for a.pos < len(a.field) {
		preserveWS := true
		c := a.field[a.pos]
		if c == '.' {
			popBlank()
			aslist = append(aslist, ".")
			a.pos++
			preserveWS = false
		} else if c == '"' {
			aslist = append(aslist, `"`+Quote(a.getquote())+`"`)
		} else if in(addrAtomEnds, c) {
			popBlank()
			break
		} else {
			aslist = append(aslist, a.getatom(addrAtomEnds))
		}
		if ws := a.gotonext(); preserveWS && ws != "" {
			aslist = append(aslist, ws)
		}
	}
	if a.pos >= len(a.field) || a.field[a.pos] != '@' {
		return strings.Join(aslist, "")
	}
	aslist = append(aslist, "@")
	a.pos++
	a.gotonext()
	domain := a.getdomain()
	if domain == "" {
		return ""
	}
	return strings.Join(aslist, "") + domain
}

// The complete domain of an address, empty when a second at sign follows
func (a *addrlist) getdomain() string {
	var sdlist []string
	for a.pos < len(a.field) {
		c := a.field[a.pos]
		if in(addrLWS, c) {
			a.pos++
		} else if c == '(' {
			a.commentlist = append(a.commentlist, a.getcomment())
		} else if c == '[' {
			sdlist = append(sdlist, a.getdomainliteral())
		} else if c == '.' {
			a.pos++
			sdlist = append(sdlist, ".")
		} else if c == '@' {
			return ""
		} else if in(addrAtomEnds, c) {
			break
		} else {
			sdlist = append(sdlist, a.getatom(addrAtomEnds))
		}
	}
	return strings.Join(sdlist, "")
}

// A fragment delimited by a begin character and one of the end characters, comments nested when allowed
func (a *addrlist) getdelimited(beginchar rune, endchars string, allowcomments bool) string {
	if a.field[a.pos] != beginchar {
		return ""
	}
	var slist []string
	quote := false
	a.pos++
	for a.pos < len(a.field) {
		c := a.field[a.pos]
		if quote {
			slist = append(slist, string(c))
			quote = false
		} else if in(endchars, c) {
			a.pos++
			break
		} else if allowcomments && c == '(' {
			slist = append(slist, a.getcomment())
			continue
		} else if c == addrDelimEscape {
			quote = true
		} else {
			slist = append(slist, string(c))
		}
		a.pos++
	}
	return strings.Join(slist, "")
}

func (a *addrlist) getquote() string { return a.getdelimited('"', "\"\r", false) }

func (a *addrlist) getcomment() string { return a.getdelimited('(', ")\r", true) }

func (a *addrlist) getdomainliteral() string { return "[" + a.getdelimited('[', "]\r", false) + "]" }

// An RFC 2822 atom ending at one of the given characters
func (a *addrlist) getatom(atomends string) string {
	start := a.pos
	for a.pos < len(a.field) && !in(atomends, a.field[a.pos]) {
		a.pos++
	}
	return string(a.field[start:a.pos])
}

// A sequence of phrases, each an atom or a quoted string
func (a *addrlist) getphraselist() []string {
	var plist []string
	for a.pos < len(a.field) {
		c := a.field[a.pos]
		if in(addrFWS, c) {
			a.pos++
		} else if c == '"' {
			plist = append(plist, a.getquote())
		} else if c == '(' {
			a.commentlist = append(a.commentlist, a.getcomment())
		} else if in(addrPhraseEnds, c) {
			break
		} else {
			plist = append(plist, a.getatom(addrPhraseEnds))
		}
	}
	return plist
}

// Addresses of an unparsed address field, none for an empty field
func addressList(field string) []Address {
	if field == "" {
		return nil
	}
	return (&addrlist{field: []rune(field)}).getaddrlist()
}

// Position and text of each character with a backslash joined to the character it escapes
func iterEscapedChars(addr []rune, yield func(pos int, ch string)) {
	escape := false
	pos := 0
	for i, ch := range addr {
		pos = i
		if escape {
			yield(i, `\`+string(ch))
			escape = false
		} else if ch == '\\' {
			escape = true
		} else {
			yield(i, string(ch))
		}
	}
	if escape {
		yield(pos, `\`)
	}
}

// The address with every quoted real name removed
func stripQuotedRealnames(addr string) string {
	if !strings.Contains(addr, `"`) {
		return addr
	}
	rs := []rune(addr)
	start, openPos := 0, -1
	var result []string
	iterEscapedChars(rs, func(pos int, ch string) {
		if ch != `"` {
			return
		}
		if openPos < 0 {
			openPos = pos
			return
		}
		if start != openPos {
			result = append(result, string(rs[start:openPos]))
		}
		start, openPos = pos+1, -1
	})
	if start < len(rs) {
		result = append(result, string(rs[start:]))
	}
	return strings.Join(result, "")
}

// Whether the parentheses outside quoted real names balance
func checkParenthesis(addr string) bool {
	opens, balanced := 0, true
	iterEscapedChars([]rune(stripQuotedRealnames(addr)), func(_ int, ch string) {
		switch ch {
		case "(":
			opens++
		case ")":
			opens--
			if opens < 0 {
				balanced = false
			}
		}
	})
	return balanced && opens == 0
}

// A field value kept when its parentheses balance, else the literal the strict parser rejects
func preParseValidation(value string) string {
	if !checkParenthesis(value) {
		return "('', '')"
	}
	return value
}

// Parsed pairs with every address still holding a bracket emptied
func postParseValidation(parsed []Address) []Address {
	out := make([]Address, len(parsed))
	for i, v := range parsed {
		if strings.Contains(v.Addr, "[") {
			v = Address{}
		}
		out[i] = v
	}
	return out
}

// Real name and address of one address field under the strict parser, an empty pair when it does not parse to exactly one
func ParseAddr(addr string) Address {
	addrs := postParseValidation(addressList(preParseValidation(addr)))
	if len(addrs) != 1 {
		return Address{}
	}
	return addrs[0]
}

// Real name and address pairs of the field values under the strict parser, one empty pair when the count is off
func GetAddresses(fieldvalues []string) []Address {
	checked := make([]string, len(fieldvalues))
	for i, v := range fieldvalues {
		checked[i] = preParseValidation(v)
	}
	result := postParseValidation(addressList(strings.Join(checked, ", ")))
	n := 0
	for _, v := range checked {
		n += 1 + strings.Count(stripQuotedRealnames(v), ",")
	}
	if len(result) != n {
		return []Address{{}}
	}
	return result
}
