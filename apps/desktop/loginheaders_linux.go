//go:build linux

package main

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/desktop/connections"
)

// loginHeaderServer answers the sign-in web extension with the outgoing header set of each request
type loginHeaderServer struct {
	path     string
	mu       sync.Mutex
	pages    map[uint64]*connections.RemoteHeaderInjector
	requests map[string]int
	order    []string
	next     int
}

// Listens on an owner only socket in a private runtime directory
func startLoginHeaderServer(userData string) (*loginHeaderServer, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = filepath.Join(userData, "Partitions")
	}
	dir = filepath.Join(dir, "icebath-login-"+strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "headers.sock")
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, err
	}
	server := &loginHeaderServer{path: path, pages: map[uint64]*connections.RemoteHeaderInjector{}, requests: map[string]int{}}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.answer(conn)
		}
	}()
	return server, nil
}

func (s *loginHeaderServer) register(page uint64, injector *connections.RemoteHeaderInjector) {
	s.mu.Lock()
	s.pages[page] = injector
	s.mu.Unlock()
}

func (s *loginHeaderServer) unregister(page uint64) {
	s.mu.Lock()
	delete(s.pages, page)
	s.mu.Unlock()
}

// Request id of a hop, a redirect hop keeping the id of the hop it came from
func (s *loginHeaderServer) requestID(page, url, prev string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := 0, false
	if prev != "" {
		id, ok = s.requests[page+"\x00"+prev]
	}
	if !ok {
		s.next++
		id = s.next
	}
	key := page + "\x00" + url
	if _, seen := s.requests[key]; !seen {
		s.order = append(s.order, key)
	}
	s.requests[key] = id
	for len(s.order) > 4096 {
		delete(s.requests, s.order[0])
		s.order = s.order[1:]
	}
	return id
}

// Reads one request description and writes the header set the injector chose
func (s *loginHeaderServer) answer(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	line := func() (string, bool) {
		text, err := reader.ReadString('\n')
		return strings.TrimSuffix(text, "\n"), err == nil
	}
	page, ok1 := line()
	url, ok2 := line()
	prev, ok3 := line()
	if !ok1 || !ok2 || !ok3 {
		return
	}
	headers := map[string]string{}
	for {
		text, ok := line()
		if !ok || text == "" {
			break
		}
		if name, value, found := strings.Cut(text, "\x1f"); found {
			headers[name] = value
		}
	}
	pageID, _ := strconv.ParseUint(page, 10, 64)
	s.mu.Lock()
	injector := s.pages[pageID]
	s.mu.Unlock()
	if injector == nil {
		_, _ = conn.Write([]byte("-\n"))
		return
	}
	id := s.requestID(page, url, prev)
	out := injector.Apply(connections.RemoteRequestDetails{ID: &id, URL: url, RequestHeaders: headers})
	if out == nil {
		_, _ = conn.Write([]byte("-\n"))
		return
	}
	var b strings.Builder
	b.WriteString("+\n")
	for name, value := range out {
		b.WriteString(name + "\x1f" + value + "\n")
	}
	b.WriteString("\n")
	_, _ = conn.Write([]byte(b.String()))
}
