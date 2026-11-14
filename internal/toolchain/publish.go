package toolchain

import (
	"context"
	"maps"
	"slices"
	"strings"
)

type DriftError struct {
	Problems map[string]string
}

func (e *DriftError) Error() string {
	names := slices.Sorted(maps.Keys(e.Problems))
	rows := make([]string, len(names))
	for i, name := range names {
		rows[i] = name + ": " + e.Problems[name]
	}
	return "tools not on PATH: " + strings.Join(rows, "; ")
}

func (s *Store) Publish(ctx context.Context, o EnsureOptions) error {
	host, err := s.hostTarget(ctx)
	if err != nil {
		return err
	}
	s.installBeforeLock(ctx, o.ConfirmHostPackage)
	o.Explicit, o.ConfirmHostPackage = true, nil
	for _, name := range s.order {
		r := s.recipes[name]
		if !r.required || r.inHome() || r.missing(host.target) != "" {
			continue
		}
		if _, err := s.Ensure(ctx, name, o); err != nil {
			return err
		}
	}
	problems, err := s.Drift(ctx)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return &DriftError{Problems: problems}
	}
	return nil
}
