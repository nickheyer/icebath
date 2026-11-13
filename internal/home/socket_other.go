//go:build !darwin

package home

// Socket roots follow the temp root here unless it grows past the length budget
const forcedSocketRoot = ""
