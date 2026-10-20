// Server sent events reader following the WHATWG event stream parsing rules
package sse

import (
	"bufio"
	"io"
	"iter"
	"strings"
)

// One dispatched event with data lines joined by newlines
type Event struct {
	Name string
	Data string
	ID   string
}

// Yields each event terminated by a blank line, discarding an unterminated trailing event
func Read(r io.Reader) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		br := bufio.NewReader(r)
		var name, id string
		var data strings.Builder
		hasData := false
		for {
			line, err := readLine(br)
			if err != nil && err != io.EOF {
				yield(Event{}, err)
				return
			}
			if err == io.EOF && line == "" {
				return
			}
			for _, l := range strings.Split(line, "\r") {
				if l == "" {
					if hasData {
						ev := Event{Name: name, Data: strings.TrimSuffix(data.String(), "\n"), ID: id}
						if !yield(ev, nil) {
							return
						}
					}
					name = ""
					data.Reset()
					hasData = false
					continue
				}
				field, value := split(l)
				switch field {
				case "event":
					name = value
				case "data":
					data.WriteString(value)
					data.WriteString("\n")
					hasData = true
				case "id":
					if !strings.ContainsRune(value, 0) {
						id = value
					}
				}
			}
			if err == io.EOF {
				return
			}
		}
	}
}

// Reads up to a newline, dropping the line feed and one trailing carriage return
func readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return line, err
}

// Splits a field line at the first colon, dropping one space after it
func split(line string) (string, string) {
	if strings.HasPrefix(line, ":") {
		return "", ""
	}
	field, value, found := strings.Cut(line, ":")
	if !found {
		return line, ""
	}
	return field, strings.TrimPrefix(value, " ")
}
