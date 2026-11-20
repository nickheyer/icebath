package gateway

import "time"

// Raw answer of one request line sent over the home control pipe as this OS user
func privateControlQuery(home string, request []byte, timeout time.Duration) ([]byte, error) {
	return queryRuntimeControl(home, request, timeout)
}
