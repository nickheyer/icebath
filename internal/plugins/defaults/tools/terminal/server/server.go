// Lines a person runs in a chat after a !: run in their own shell in the chat's folder on the machine the server runs on, what each prints
// told as it comes, its secrets masked, and kept beside the session for its transcript, never read by the model
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools/redact"
	linesv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lines/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lines/v1/linesv1connect"
)

// Most characters of what a line printed that are kept with the session, its end
const longest = 8000

// Longest the output of a line that ended is read on, for what is left in the pipe, before a process it left running is no longer waited on
const drain = time.Second

// Longest a piece of output with no line break waits before it is told, such as a prompt
const flush = 200 * time.Millisecond

type service struct {
	config   *config.Config
	sessions *sessions.Registry
	terminal *terminal.Terminal
	// Writes to the files of the sessions' lines, one at a time
	mu sync.Mutex
}

func Register(registry *server.Registry, cfg *config.Config, stores *sessions.Registry, host *terminal.Terminal) {
	registry.Register(&service{config: cfg, sessions: stores, terminal: host})
}

func (s *service) Mount(services *connect.Server) {
	linesv1connect.RegisterLineServiceHandler(services, s)
}

// Settings and folder of the chat: those of its session when it has one, else the profile asked for and the folder given
func (s *service) chat(ctx context.Context, request *linesv1.RunRequest) (*config.Config, string, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, "", err
	}
	folder := request.GetFolder()
	if id := request.GetSessionId(); id != "" {
		store, err := s.sessions.Store(cfg.GetString(sessionsconfig.Store))
		if err != nil {
			return nil, "", err
		}
		session, err := store.Get(ctx, id)
		if err != nil {
			return nil, "", err
		}
		cfg = cfg.In(session.GetProfile())
		if session.GetDirectory() != "" {
			folder = session.GetDirectory()
		}
	}
	return cfg, folder, nil
}

func (s *service) Run(ctx context.Context, request *linesv1.RunRequest, stream linesv1connect.LineServiceRunServerStream) error {
	cfg, folder, err := s.chat(ctx, request)
	if err != nil {
		return err
	}
	line := &linesv1.Line{Time: timestamppb.Now(), Command: request.GetLine(), Folder: folder}
	if info, err := os.Stat(folder); folder == "" || err != nil || !info.IsDir() {
		line.Error = "The folder of this chat is not on this computer, so the line was not run."
		return stream.Send(&linesv1.RunEvent{Kind: &linesv1.RunEvent_Done{Done: line}})
	}
	cmd := shellCommand(request.GetShell(), request.GetLine())
	cmd.Dir = folder
	cmd.Env = append(os.Environ(), "TERM=dumb", "NO_COLOR=1", "PAGER=cat", "GIT_PAGER=cat")
	terminal.NewSession(cmd)
	p, err := terminal.Start(cmd, nil)
	if err != nil {
		line.Error = "Your shell did not start, so the line was not run. The error was: " + strings.TrimSuffix(err.Error(), ".") + "."
		return stream.Send(&linesv1.RunEvent{Kind: &linesv1.RunEvent_Done{Done: line}})
	}
	p.Own(s.terminal.Supervisor)
	defer p.Release()
	terminal.RegisterChild(cfg, p.Pid(), "a line the person ran in a chat")
	var output strings.Builder
	var sent error
	read := make(chan struct{})
	go func() {
		defer close(read)
		pieces(p.Output(), func(piece string) {
			piece = redact.Text(piece, false, redact.Ends)
			output.WriteString(piece)
			if sent == nil {
				sent = stream.Send(&linesv1.RunEvent{Kind: &linesv1.RunEvent_Output{Output: piece}})
			}
		})
	}()
	select {
	case <-ctx.Done():
		line.Stopped = true
		p.ForceKill()
	case <-wait(p):
	}
	code, _ := p.Wait(0)
	select {
	case <-read:
	case <-time.After(drain):
		p.Output().Close()
		<-read
	}
	line.Code = int32(code)
	line.Output, line.Cut = tail(output.String())
	if id := request.GetSessionId(); id != "" {
		if err := s.keep(cfg, id, line); err != nil {
			line.Error = "The line ran, but it was not kept with the session. The error was: " + strings.TrimSuffix(err.Error(), ".") + "."
		}
	}
	if sent != nil || ctx.Err() != nil {
		return sent
	}
	return stream.Send(&linesv1.RunEvent{Kind: &linesv1.RunEvent_Done{Done: line}})
}

// Channel closed once the process ends
func wait(p *terminal.HostProc) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		p.Wait(0)
		close(done)
	}()
	return done
}

// The person's shell running the line: the one given when this machine has it, else theirs here, else sh; cmd on Windows
func shellCommand(shell, line string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/C", line)
	}
	for _, candidate := range []string{shell, os.Getenv("SHELL"), "/bin/sh"} {
		if candidate == "" {
			continue
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return exec.Command(path, "-c", line)
		}
	}
	return exec.Command("sh", "-c", line)
}

// Reads what the process prints and hands it on a line at a time, a piece that ends in no line break once it waited a moment
func pieces(out *os.File, each func(piece string)) {
	reader := bufio.NewReaderSize(out, 64<<10)
	var pending strings.Builder
	for {
		out.SetReadDeadline(time.Now().Add(flush))
		chunk, err := reader.ReadString('\n')
		pending.WriteString(chunk)
		if strings.HasSuffix(chunk, "\n") || (errors.Is(err, os.ErrDeadlineExceeded) && pending.Len() > 0) {
			each(strings.ToValidUTF8(strings.ReplaceAll(pending.String(), "\r\n", "\n"), "�"))
			pending.Reset()
		}
		if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			if pending.Len() > 0 {
				each(strings.ToValidUTF8(pending.String(), "�"))
			}
			return
		}
	}
}

// End of the output that is kept, and whether more came before it
func tail(output string) (string, bool) {
	runes := []rune(output)
	if len(runes) <= longest {
		return output, false
	}
	return string(runes[len(runes)-longest:]), true
}

// File of the lines a person ran in a session, beside its other files in the profile's sessions folder
func file(cfg *config.Config, session string) string {
	return filepath.Join(cfg.ProfileDir(cfg.Profile()), "sessions", session+".shell.jsonl")
}

// Adds the line to its session's file
func (s *service) keep(cfg *config.Config, session string, line *linesv1.Line) error {
	data, err := json.Marshal(entryOf(line))
	if err != nil {
		return err
	}
	path := file(cfg, session)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// One line of a session's file
type entry struct {
	At      time.Time `json:"at"`
	Command string    `json:"command"`
	Output  string    `json:"output"`
	Cut     bool      `json:"cut,omitempty"`
	Code    int32     `json:"code"`
	Stopped bool      `json:"stopped,omitempty"`
	Folder  string    `json:"folder,omitempty"`
}

func entryOf(line *linesv1.Line) entry {
	return entry{At: line.GetTime().AsTime(), Command: line.GetCommand(), Output: line.GetOutput(), Cut: line.GetCut(), Code: line.GetCode(),
		Stopped: line.GetStopped(), Folder: line.GetFolder()}
}

func (s *service) List(ctx context.Context, request *linesv1.ListRequest) (*linesv1.ListResponse, error) {
	cfg, _, err := s.chat(ctx, &linesv1.RunRequest{SessionId: request.GetSessionId(), Profile: request.GetProfile()})
	if err != nil {
		return nil, err
	}
	response := &linesv1.ListResponse{}
	data, err := os.ReadFile(file(cfg, request.GetSessionId()))
	if errors.Is(err, os.ErrNotExist) {
		return response, nil
	}
	if err != nil {
		return nil, err
	}
	for _, raw := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var k entry
		if json.Unmarshal([]byte(raw), &k) != nil {
			continue
		}
		response.Lines = append(response.Lines, &linesv1.Line{Time: timestamppb.New(k.At), Command: k.Command, Output: k.Output, Cut: k.Cut,
			Code: k.Code, Stopped: k.Stopped, Folder: k.Folder})
	}
	return response, nil
}
