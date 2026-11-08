package tools

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
)

// Hard timeout of one schtasks call, and the shorter one the install probe allows each query
const (
	schtasksTimeout  = 15 * time.Second
	taskProbeTimeout = 5 * time.Second
)

var (
	gatewayTaskListingRe = regexp.MustCompile(`(?m)^"\\((?:` + gatewayTaskNameDefault + `|` + legacyGatewayTaskNameDefault + `)[^"\\]*)"`)
	xmlEntityDeclRe      = regexp.MustCompile(`<!ENTITY\s+([^\s%"']+)\s+(?:"([^"]*)"|'([^']*)')\s*>`)
)

// Runs schtasks with arguments under a timeout and returns its exit code, stdout and stderr
type schtasksRunner func(args []string, timeout time.Duration) (int, string, string)

// Scripts each gateway Scheduled Task starts, read from the task listing and every task action
func taskLaunchersFrom(run schtasksRunner) []string {
	code, listing, _ := run([]string{"/Query", "/FO", "CSV", "/NH"}, taskProbeTimeout)
	var names []string
	seen := map[string]bool{}
	if code == 0 {
		for _, m := range gatewayTaskListingRe.FindAllStringSubmatch(listing, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				names = append(names, m[1])
			}
		}
	}
	launchers := []string{}
	for _, name := range names {
		code, text, _ := run([]string{"/Query", "/TN", name, "/XML"}, taskProbeTimeout)
		var leaves map[string]string
		if code == 0 {
			leaves = taskXMLLeafValues(text)
		}
		command := strings.Trim(leaves["Task/Actions/Exec/Command"], `"`)
		launchers = append(launchers, serviceDefinitionLaunchers(`"`+command+`" `+leaves["Task/Actions/Exec/Arguments"])...)
	}
	return launchers
}

// Namespace agnostic leaf path to whitespace collapsed text map of a task XML, nil when the XML does not parse
func taskXMLLeafValues(text string) map[string]string {
	d := xml.NewDecoder(strings.NewReader(strings.TrimPrefix(text, "\ufeff")))
	d.Strict = true
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	type frame struct {
		path     string
		children bool
		text     strings.Builder
	}
	var stack []*frame
	declared := []map[string]bool{{"http://www.w3.org/XML/1998/namespace": true}}
	values := map[string]string{}
	rootDone := false
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil
		}
		switch t := tok.(type) {
		case xml.Directive:
			for _, m := range xmlEntityDeclRe.FindAllStringSubmatch(string(t), -1) {
				if d.Entity == nil {
					d.Entity = map[string]string{}
				}
				d.Entity[m[1]] = m[2] + m[3]
			}
		case xml.StartElement:
			if rootDone {
				return nil
			}
			scope := map[string]bool{}
			for k := range declared[len(declared)-1] {
				scope[k] = true
			}
			for _, a := range t.Attr {
				if a.Name.Space == "xmlns" || (a.Name.Space == "" && a.Name.Local == "xmlns") {
					scope[a.Value] = true
				}
			}
			if !xmlNameBound(t.Name, scope) {
				return nil
			}
			attrs := map[xml.Name]bool{}
			for _, a := range t.Attr {
				if attrs[a.Name] || (a.Name.Space != "xmlns" && a.Name.Space != "" && !scope[a.Name.Space]) {
					return nil
				}
				attrs[a.Name] = true
			}
			declared = append(declared, scope)
			path := t.Name.Local
			if len(stack) == 0 {
				version := ""
				for _, a := range t.Attr {
					if a.Name.Space == "" && a.Name.Local == "version" {
						version = a.Value
					}
				}
				values["Task@version"] = version
			} else {
				parent := stack[len(stack)-1]
				parent.children = true
				path = parent.path + "/" + path
			}
			stack = append(stack, &frame{path: path})
		case xml.CharData:
			if len(stack) == 0 {
				if len(bytes.TrimLeft(t, " \t\r\n")) != 0 {
					return nil
				}
				continue
			}
			top := stack[len(stack)-1]
			if !top.children {
				top.text.Write(t)
			}
		case xml.EndElement:
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			declared = declared[:len(declared)-1]
			if !top.children {
				values[top.path] = strings.Join(strings.FieldsFunc(top.text.String(), pyIsSpace), " ")
			}
			if len(stack) == 0 {
				rootDone = true
			}
		}
	}
	if !rootDone {
		return nil
	}
	return values
}

// Namespace agnostic leaf path to text map of a task XML for the gateway drift check, nil when unparsable
func TaskXMLLeafValues(text string) map[string]string { return taskXMLLeafValues(text) }

// Reports whether an element name carries no prefix or a namespace declared in scope
func xmlNameBound(name xml.Name, scope map[string]bool) bool {
	return name.Space == "" || scope[name.Space]
}

// Text of captured schtasks bytes, UTF-16 with a BOM, else strict UTF-8, then each console code page, then lossy UTF-8
func decodeSchtasksOutput(data []byte, pages []int, decoderFor func(cp int) codePageDecoder) (string, error) {
	if bytes.HasPrefix(data, []byte{0xff, 0xfe}) || bytes.HasPrefix(data, []byte{0xfe, 0xff}) {
		if text, ok := decodeUTF16BOM(data); ok {
			return text, nil
		}
	}
	for _, cp := range append([]int{65001}, pages...) {
		dec := decoderFor(cp)
		if dec == nil {
			continue
		}
		text, ok, err := dec(data)
		if err != nil {
			return "", err
		}
		if ok {
			return text, nil
		}
	}
	return decodeUTF8Replace(data), nil
}
