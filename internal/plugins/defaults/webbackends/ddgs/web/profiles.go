package web

import (
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
)

// Browser makes primp 2.0.1 impersonates
type family int

const (
	chrome family = iota
	edge
	opera
	safari
	firefox
)

// One browser primp impersonates, with the client hints a Chromium based one sends
type profile struct {
	family  family
	version string
	brands  string
}

var profiles = []profile{
	{chrome, "144", `"Not(A:Brand";v="8", "Chromium";v="144", "Google Chrome";v="144"`},
	{chrome, "145", `"Not:A-Brand";v="99", "Google Chrome";v="145", "Chromium";v="145"`},
	{chrome, "146", `"Not-A.Brand";v="24", "Chromium";v="146"`},
	{chrome, "147", `"Google Chrome";v="147", "Not.A/Brand";v="8", "Chromium";v="147"`},
	{chrome, "148", `"Chromium";v="148", "Google Chrome";v="148", "Not/A)Brand";v="99"`},
	{chrome, "149", `"Google Chrome";v="149", "Chromium";v="149", "Not)A;Brand";v="24"`},
	{chrome, "150", `"Not;A=Brand";v="8", "Chromium";v="150", "Google Chrome";v="150"`},
	{chrome, "151", `"Not=A?Brand";v="99", "Google Chrome";v="151", "Chromium";v="151"`},
	{chrome, "152", `"Chromium";v="152", "Not?A_Brand";v="24", "Google Chrome";v="152"`},
	{chrome, "153", `"Google Chrome";v="153", "Not_A Brand";v="8", "Chromium";v="153"`},
	{edge, "144", `"Not(A:Brand";v="8", "Chromium";v="144", "Microsoft Edge";v="144"`},
	{edge, "145", `"Not:A-Brand";v="99", "Microsoft Edge";v="145", "Chromium";v="145"`},
	{edge, "146", `"Chromium";v="146", "Not-A.Brand";v="24", "Microsoft Edge";v="146"`},
	{edge, "147", `"Microsoft Edge";v="147", "Not.A/Brand";v="8", "Chromium";v="147"`},
	{edge, "148", `"Chromium";v="148", "Microsoft Edge";v="148", "Not/A)Brand";v="99"`},
	{edge, "149", `"Microsoft Edge";v="149", "Chromium";v="149", "Not)A;Brand";v="24"`},
	{edge, "150", `"Not;A=Brand";v="8", "Chromium";v="150", "Microsoft Edge";v="150"`},
	{edge, "151", `"Not=A?Brand";v="99", "Microsoft Edge";v="151", "Chromium";v="151"`},
	{edge, "152", `"Chromium";v="152", "Not?A_Brand";v="24", "Microsoft Edge";v="152"`},
	{edge, "153", `"Microsoft Edge";v="153", "Not_A Brand";v="8", "Chromium";v="153"`},
	{opera, "126", `"Chromium";v="142", "Opera";v="126", "Not_A Brand";v="99"`},
	{opera, "127", `"Opera";v="127", "Chromium";v="143", "Not A(Brand";v="24"`},
	{opera, "128", `"Not(A:Brand";v="8", "Chromium";v="144", "Opera";v="128"`},
	{opera, "129", `"Not:A-Brand";v="99", "Opera";v="129", "Chromium";v="145"`},
	{opera, "130", `"Chromium";v="146", "Not-A.Brand";v="24", "Opera";v="130"`},
	{opera, "131", `"Opera";v="131", "Not.A/Brand";v="8", "Chromium";v="147"`},
	{opera, "132", `"Chromium";v="148", "Opera";v="132", "Not/A)Brand";v="99"`},
	{opera, "133", `"Opera";v="133", "Chromium";v="149", "Not)A;Brand";v="24"`},
	{opera, "134", `"Not;A=Brand";v="8", "Chromium";v="150", "Opera";v="134"`},
	{opera, "135", `"Not=A?Brand";v="99", "Opera";v="135", "Chromium";v="151"`},
	{safari, "18.5", ""},
	{safari, "26.0", ""},
	{safari, "26.3", ""},
	{safari, "26.4", ""},
	{firefox, "140", ""},
	{firefox, "146", ""},
	{firefox, "147", ""},
	{firefox, "148", ""},
	{firefox, "149", ""},
	{firefox, "150", ""},
	{firefox, "151", ""},
}

// Operating system primp puts a browser on
type system int

const (
	windows system = iota
	macOS
	linux
	android
	iOS
)

// How client hints name each system
var platforms = [...]string{`"Windows"`, `"macOS"`, `"Linux"`, `"Android"`, `"iOS"`}

// Headers of a browser picked at random, as primp's random impersonation sends them
func impersonate() http.Header {
	return profiles[rand.IntN(len(profiles))].header(system(rand.IntN(len(platforms))))
}

func (p profile) header(os system) http.Header {
	var fields map[string]string
	switch p.family {
	case safari:
		fields = map[string]string{
			"accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
			"accept-language": "en-US,en;q=0.9",
			"accept-encoding": "gzip, deflate, br",
			"sec-fetch-dest":  "document",
			"sec-fetch-mode":  "navigate",
			"sec-fetch-site":  "none",
			"priority":        "u=0, i",
		}
	case firefox:
		fields = map[string]string{
			"accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
			"accept-language":           "en-US,en;q=0.5",
			"accept-encoding":           "gzip, deflate, br, zstd",
			"dnt":                       "1",
			"sec-gpc":                   "1",
			"upgrade-insecure-requests": "1",
			"sec-fetch-dest":            "document",
			"sec-fetch-mode":            "navigate",
			"sec-fetch-site":            "none",
			"sec-fetch-user":            "?1",
			"priority":                  "u=0, i",
			"te":                        "trailers",
		}
	default:
		mobile := "?0"
		if os == android || os == iOS {
			mobile = "?1"
		}
		fields = map[string]string{
			"accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
			"accept-encoding":           "gzip, deflate, br, zstd",
			"accept-language":           "en-US,en;q=0.9",
			"upgrade-insecure-requests": "1",
			"sec-fetch-site":            "none",
			"sec-fetch-mode":            "navigate",
			"sec-fetch-dest":            "document",
			"sec-fetch-user":            "?1",
			"priority":                  "u=0, i",
			"sec-ch-ua":                 p.brands,
			"sec-ch-ua-mobile":          mobile,
			"sec-ch-ua-platform":        platforms[os],
		}
		if p.family == chrome && (p.version == "150" || p.version == "151" || p.version == "152") {
			fields["sec-purpose"] = "prefetch;prerender"
		}
		if p.family == opera && (p.version == "131" || p.version == "132") {
			fields["cache-control"] = "max-age=0"
		}
	}
	fields["user-agent"] = p.userAgent(os)
	header := http.Header{}
	for name, value := range fields {
		header.Set(name, value)
	}
	return header
}

func (p profile) userAgent(os system) string {
	switch p.family {
	case chrome:
		switch os {
		case android:
			return "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + p.version + ".0.0.0 Mobile Safari/537.36"
		case iOS:
			return "Mozilla/5.0 (iPhone; CPU iPhone OS 18_7 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/" + p.version + ".0.0.0 Mobile/15E148 Safari/604.1"
		}
		return desktop(os, "10_15_7", p.version)
	case edge:
		// Edge 147 names its full build, and another phone
		build, phone := p.version+".0.0.0", "Pixel 3 XL"
		if p.version == "147" {
			build, phone = "147.0.3912.51", "SM-G973F"
		}
		switch os {
		case android:
			return "Mozilla/5.0 (Linux; Android 10; " + phone + ") AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + p.version + ".0.0.0 Mobile Safari/537.36 EdgA/" + build
		case iOS:
			return "Mozilla/5.0 (iPhone; CPU iPhone OS 18_7_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 EdgiOS/" + build + " Mobile/15E148 Safari/605.1.15"
		}
		return desktop(os, "10_15_7", p.version) + " Edg/" + build
	case opera:
		// Opera runs the Chromium 16 versions past its own
		version, _ := strconv.Atoi(p.version)
		chromium := strconv.Itoa(version + 16)
		switch os {
		case android:
			return "Mozilla/5.0 (Linux; Android 10; SM-G970F) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + chromium + ".0.0.0 Mobile Safari/537.36 OPR/" + p.version + ".0.0.0"
		case iOS:
			return "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) OPiOS/" + p.version + ".0.0.0 Mobile/15E148 Safari/605.1.15"
		}
		return desktop(os, "15_7_3", chromium) + " OPR/" + p.version + ".0.0.0"
	case safari:
		if os == iOS {
			return "Mozilla/5.0 (iPhone; CPU iPhone OS " + strings.ReplaceAll(p.version, ".", "_") + " like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/" + p.version + " Mobile/15E148 Safari/604.1"
		}
		return "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/" + p.version + " Safari/605.1.15"
	}
	version := p.version + ".0"
	switch os {
	case windows:
		return "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:" + version + ") Gecko/20100101 Firefox/" + version
	case macOS:
		return "Mozilla/5.0 (Macintosh; Intel Mac OS X 14.7; rv:" + version + ") Gecko/20100101 Firefox/" + version
	case linux:
		return "Mozilla/5.0 (X11; Linux x86_64; rv:" + version + ") Gecko/20100101 Firefox/" + version
	case android:
		return "Mozilla/5.0 (Android 14; Mobile; rv:" + version + ") Gecko/" + version + " Firefox/" + version
	}
	// Firefox for iOS moved to iOS 18.2 at 149
	if number, _ := strconv.Atoi(p.version); number < 149 {
		return "Mozilla/5.0 (iPhone; CPU iPhone OS 17_7 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/" + version + " Mobile/15E148 Safari/605.1"
	}
	return "Mozilla/5.0 (iPhone; CPU iPhone OS 18_2 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/" + version + " Mobile/15E148 Safari/605.1.15"
}

// User agent of a Chromium based browser on a desktop system
func desktop(os system, mac, chromium string) string {
	platform := "X11; Linux x86_64"
	switch os {
	case windows:
		platform = "Windows NT 10.0; Win64; x64"
	case macOS:
		platform = "Macintosh; Intel Mac OS X " + mac
	}
	return "Mozilla/5.0 (" + platform + ") AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + chromium + ".0.0.0 Safari/537.36"
}
