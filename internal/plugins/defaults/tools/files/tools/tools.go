package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	filesconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/files/config"
	"github.com/nickheyer/icebath/internal/tools"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

const toolset = "file"

// Reason a write outside the working directory asks approval for
const outside = "writes outside the working directory"

type registrant struct{}

func Register(registry *tools.Registry) {
	registry.Register(registrant{})
}

func (registrant) Tools() []tools.Def {
	return []tools.Def{
		{
			Name:    "read_file",
			Toolset: toolset,
			Title:   "Read",
			Icon:    tools.IconRead,
			Subject: "path",
			Schema: tools.Schema(
				"Read a text file. Each line comes back as NUMBER|TEXT. A long file is read in pages with offset and limit.",
				[]string{"path"},
				map[string]any{
					"path":   tools.Property("string", "The file to read, absolute or relative to the working directory"),
					"offset": tools.Property("integer", "The line to start at, counted from 1"),
					"limit":  tools.Property("integer", "The most lines to return, 2000 at most"),
				},
			),
			Handler: tools.Bind(read),
		},
		{
			Name:    "write_file",
			Toolset: toolset,
			Title:   "Write",
			Icon:    tools.IconWrite,
			Subject: "path",
			Risks:   []string{outside},
			Schema: tools.Schema(
				"Write a file, replacing everything in it. Missing parent folders are created. To change part of a file, use patch.",
				[]string{"path", "content"},
				map[string]any{
					"path":    tools.Property("string", "The file to write, absolute or relative to the working directory"),
					"content": tools.Property("string", "The whole content of the file"),
				},
			),
			Handler: tools.Bind(write),
		},
		{
			Name:    "patch",
			Toolset: toolset,
			Title:   "Edit",
			Icon:    tools.IconEdit,
			Subject: "path",
			Risks:   []string{outside},
			Schema: tools.Schema(
				"Replace text in a file. old_string must match the file exactly, and only once unless replace_all is true.",
				[]string{"path", "old_string", "new_string"},
				map[string]any{
					"path":        tools.Property("string", "The file to change, absolute or relative to the working directory"),
					"old_string":  tools.Property("string", "The text to replace, with enough lines around it to match once"),
					"new_string":  tools.Property("string", "The text to put in its place"),
					"replace_all": tools.Property("boolean", "Replace every match instead of requiring exactly one"),
				},
			),
			Handler: tools.Bind(patch),
		},
		{
			Name:    "search_files",
			Toolset: toolset,
			Title:   "Search",
			Icon:    tools.IconSearch,
			Subject: "pattern",
			Schema: tools.Schema(
				"Search inside files with a regular expression, or find files by name. Paths in the result are relative to the working directory.",
				[]string{"pattern"},
				map[string]any{
					"pattern":   tools.Property("string", "A regular expression for content, or a glob such as *.go for files"),
					"target":    map[string]any{"type": "string", "enum": []string{"content", "files"}, "description": "Search inside files (content, the default) or file names (files)"},
					"path":      tools.Property("string", "The folder or file to search, the working directory by default"),
					"file_glob": tools.Property("string", "Only search inside files whose name matches this glob"),
					"limit":     tools.Property("integer", "The most results to return, 50 by default"),
				},
			),
			Handler: tools.Bind(search),
		},
	}
}

func read(_ context.Context, call *tools.Call, args struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}) (string, error) {
	file, err := os.Open(resolve(call.Env.Cwd, args.Path))
	if err != nil {
		return "", err
	}
	defer file.Close()
	offset, limit := max(args.Offset, 1), args.Limit
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	budget := call.Env.Config.GetInt(filesconfig.ReadMaxChars)
	reader := bufio.NewReader(file)
	var out strings.Builder
	for number := 1; ; number++ {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		if line == "" {
			break
		}
		if strings.IndexByte(line, 0) >= 0 {
			return "", errors.New("binary file")
		}
		if number < offset {
			continue
		}
		entry := fmt.Sprintf("%d|%s\n", number, cut(call, line))
		if number == offset+limit || out.Len() > 0 && out.Len()+len(entry) > budget {
			fmt.Fprintf(&out, "[more lines: continue with offset %d]\n", number)
			break
		}
		out.WriteString(entry)
	}
	return out.String(), nil
}

func write(ctx context.Context, call *tools.Call, args struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}) (string, error) {
	path := resolve(call.Env.Cwd, args.Path)
	if err := guard(ctx, call, path); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(args.Content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(args.Content), path), nil
}

func patch(ctx context.Context, call *tools.Call, args struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}) (string, error) {
	path := resolve(call.Env.Cwd, args.Path)
	if err := guard(ctx, call, path); err != nil {
		return "", err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	count := strings.Count(string(content), args.OldString)
	switch {
	case args.OldString == "" || args.OldString == args.NewString:
		return "", errors.New("old_string must not be empty or equal to new_string")
	case count == 0:
		return "", errors.New("old_string was not found in the file")
	case count > 1 && !args.ReplaceAll:
		return "", fmt.Errorf("old_string matches %d times: add lines around it or set replace_all", count)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(content), args.OldString, args.NewString)), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("replaced %d match(es) in %s", count, path), nil
}

func search(ctx context.Context, call *tools.Call, args struct {
	Pattern  string `json:"pattern"`
	Target   string `json:"target"`
	Path     string `json:"path"`
	FileGlob string `json:"file_glob"`
	Limit    int    `json:"limit"`
}) (string, error) {
	glob, expression := args.FileGlob, (*regexp.Regexp)(nil)
	if args.Target == "files" {
		glob = args.Pattern
	} else {
		var err error
		if expression, err = regexp.Compile(args.Pattern); err != nil {
			return "", err
		}
	}
	if _, err := filepath.Match(glob, ""); err != nil {
		return "", err
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 50
	}
	directory := call.Env.Cwd
	root := resolve(directory, args.Path)
	found := 0
	var out strings.Builder
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if ok, _ := filepath.Match(glob, entry.Name()); glob != "" && !ok || !entry.Type().IsRegular() {
			return nil
		}
		name, err := filepath.Rel(directory, path)
		if err != nil || !filepath.IsLocal(name) {
			name = path
		}
		if expression == nil {
			if found++; found > limit {
				return filepath.SkipAll
			}
			fmt.Fprintln(&out, name)
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer file.Close()
		scanner := bufio.NewScanner(file)
		scanner.Buffer(nil, 1<<20)
		for number := 1; scanner.Scan(); number++ {
			line := scanner.Text()
			if strings.IndexByte(line, 0) >= 0 {
				return nil
			}
			if expression.MatchString(line) {
				if found++; found > limit {
					return filepath.SkipAll
				}
				fmt.Fprintf(&out, "%s:%d:%s\n", name, number, cut(call, line))
			}
		}
		if err := scanner.Err(); !errors.Is(err, bufio.ErrTooLong) {
			return err
		}
		return nil
	})
	switch {
	case err != nil:
		return "", err
	case found == 0:
		return "no matches", nil
	case found > limit:
		fmt.Fprintf(&out, "[more than %d results: narrow the search or raise limit]\n", limit)
	}
	return out.String(), nil
}

func cut(call *tools.Call, line string) string {
	line = strings.TrimRight(line, "\r\n")
	if limit := call.Env.Config.GetInt(toolsconfig.OutputMaxLineLength); len(line) > limit {
		return line[:limit] + " [line cut]"
	}
	return line
}

func resolve(directory, path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, rest)
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(directory, path)
	}
	return path
}

func guard(ctx context.Context, call *tools.Call, path string) error {
	if name, err := filepath.Rel(call.Env.Cwd, path); err != nil || !filepath.IsLocal(name) {
		return call.Env.Approve(ctx, "write "+path, outside)
	}
	return nil
}
