package tools

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Runs the pty helper mode named by ICEBATH_PTY_TEST_HELPER and exits, a no op in a normal test run
func ptyTestHelperEntry() {
	mode := os.Getenv("ICEBATH_PTY_TEST_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "readline":
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		fmt.Printf("GOT:%s\n", trimLineEnd(line))
	case "sleep-print":
		ms, _ := strconv.Atoi(os.Getenv("ICEBATH_PTY_TEST_MS"))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		fmt.Println(os.Getenv("ICEBATH_PTY_TEST_TEXT"))
	case "print-sleep":
		fmt.Println(os.Getenv("ICEBATH_PTY_TEST_TEXT"))
		ms, _ := strconv.Atoi(os.Getenv("ICEBATH_PTY_TEST_MS"))
		time.Sleep(time.Duration(ms) * time.Millisecond)
	case "cwd":
		wd, _ := os.Getwd()
		fmt.Println(wd)
	case "env":
		fmt.Println(os.Getenv("ICEBATH_PTY_TEST_TEXT"))
	}
	os.Exit(0)
}

func trimLineEnd(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
