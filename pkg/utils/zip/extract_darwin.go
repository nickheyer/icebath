package ziputil

// Name element as valid UTF-8, which the file systems here require, bytes that are not read as Latin-1
func legalElement(el string) string { return validName(el) }
