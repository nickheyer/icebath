package main

import (
	"errors"
	"fmt"
)

// Container a generated target must have
type shape struct {
	format string
}

// One line report of a written target that passed inspection
func describe(want shape, data []byte) (string, error) {
	if err := inspect(want, data); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d bytes %s", len(data), want.format), nil
}

// Checks an output against its shape, a text target being non empty
func inspect(want shape, data []byte) error {
	if want.format != "TEXT" {
		return fmt.Errorf("no inspection for %s", want.format)
	}
	if len(data) == 0 {
		return errors.New("empty")
	}
	return nil
}
