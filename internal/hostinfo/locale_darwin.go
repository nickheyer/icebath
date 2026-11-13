package hostinfo

// First preferred UI language of this user through CoreFoundation such as de-DE, empty when none is set or readable
func hostLocale() string {
	f, err := loadFrameworks()
	if err != nil {
		return ""
	}
	languages := f.call("CFLocaleCopyPreferredLanguages")
	if languages == 0 {
		return ""
	}
	defer f.release(languages)
	if int(f.call("CFArrayGetCount", languages)) < 1 {
		return ""
	}
	language, _ := f.text(f.call("CFArrayGetValueAtIndex", languages, 0))
	return language
}
