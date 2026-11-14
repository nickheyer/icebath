package toolchain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Directory below the profile home of ctx that a home placed entry lives in
func (s *Store) homeDirOf(ctx context.Context, r *recipe) (string, error) {
	h, err := s.homes.Home(ctx)
	if err != nil {
		return "", err
	}
	return r.homeDir.In(h), nil
}

// Signature of the file at path under digest, false when it does not exist
func signatureOf(path, digest string) (fileSignature, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileSignature{}, false
	}
	id, err := fileutil.LstatID(path)
	if err != nil {
		return fileSignature{}, false
	}
	return fileSignature{digest: digest, id: id, size: fi.Size(), mtime: fi.ModTime().UnixNano()}, true
}

// Pinned plain files of a raw entry that are absent or changed since this process verified them
func (s *Store) pendingFiles(dir string, artifacts []Artifact) []Source {
	var pending []Source
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range artifacts {
		dest := filepath.Join(dir, a.fileName())
		if sig, ok := signatureOf(dest, a.SHA256); ok && s.verified[dest] == sig {
			continue
		}
		pending = append(pending, s.pinnedSource(a, dest))
	}
	return pending
}

// Remembers the signature of each verified file so a complete cache is accepted without rehashing
func (s *Store) rememberFiles(sources []Source) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, src := range sources {
		if sig, ok := signatureOf(src.Dest, src.SHA256); ok {
			s.verified[src.Dest] = sig
		}
	}
}

// Install of a home placed entry when its files are there, ErrNotInstalled otherwise, nothing downloaded
func (s *Store) homeInstalled(ctx context.Context, r *recipe, host hostTarget) (*Installation, error) {
	dir, err := s.homeDirOf(ctx, r)
	if err != nil {
		return nil, err
	}
	version, _ := s.lock.version(r.name)
	inst := &Installation{Name: r.name, Version: version, Dir: dir, Current: true}
	if r.raw {
		for _, a := range s.lock.artifacts(r.name, host) {
			if fi, err := os.Stat(filepath.Join(dir, a.fileName())); err != nil || !fi.Mode().IsRegular() {
				return nil, notInstalled(r.name)
			}
		}
		return inst, nil
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(r.present))); err != nil {
		return nil, notInstalled(r.name)
	}
	return inst, nil
}

// Brings the files of a home placed entry into the profile home, verifying every byte against its pin
func (s *Store) homeEnsure(ctx context.Context, r *recipe, host hostTarget, report func(Progress)) (*Installation, error) {
	dir, err := s.homeDirOf(ctx, r)
	if err != nil {
		return nil, err
	}
	version, _ := s.lock.version(r.name)
	artifacts := s.lock.artifacts(r.name, host)
	if len(artifacts) == 0 {
		return nil, s.noArtifact(r, host)
	}
	inst := &Installation{Name: r.name, Version: version, Dir: dir, Current: true}
	tick := func(done, total int64, active string, ranges map[string][]Range) {
		index := 0
		for i, a := range artifacts {
			if filepath.Base(active) == a.fileName() {
				index = i
			}
		}
		report(Progress{Stage: StageDownload, Done: done, Total: total, Label: archiveLabel(index, len(artifacts)), Ranges: ranges})
	}
	if r.raw {
		pending := s.pendingFiles(dir, artifacts)
		if len(pending) == 0 {
			return inst, nil
		}
		h, err := s.homes.Home(ctx)
		if err != nil {
			return nil, err
		}
		if err := s.homes.Mkdir(ctx, dir); err != nil {
			return nil, installFailure(r.name, err)
		}
		if err := s.downloader(r.partials.In(h), 0).run(ctx, pending, tick); err != nil {
			return nil, installFailure(r.name, err)
		}
		s.rememberFiles(pending)
		return inst, nil
	}
	present := filepath.Join(dir, filepath.FromSlash(r.present))
	if _, err := os.Stat(present); err == nil {
		return inst, nil
	}
	h, err := s.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.homes.Mkdir(ctx, dir); err != nil {
		return nil, installFailure(r.name, err)
	}
	partials, err := s.partialsRoot(ctx)
	if err != nil {
		return nil, err
	}
	l, err := fileutil.OpenLock(ctx, filepath.Join(partials, ".locks", partialKey("home:"+dir)), fileutil.LockPerm(0o600), fileutil.RecordHolder())
	if err != nil {
		return nil, err
	}
	defer l.Release()
	if _, err := os.Stat(present); err == nil {
		return inst, nil
	}
	scratch, err := os.MkdirTemp(s.homes.ScratchDir(h), "toolchain-")
	if err != nil {
		return nil, installFailure(r.name, err)
	}
	defer fileutil.RemoveTree(scratch, true)
	sources := make([]Source, len(artifacts))
	for i, a := range artifacts {
		sources[i] = s.pinnedSource(a, filepath.Join(scratch, "fetch", a.fileName()))
	}
	if err := s.downloader(partials, 0).run(ctx, sources, tick); err != nil {
		return nil, installFailure(r.name, err)
	}
	tree := filepath.Join(scratch, "tree")
	for index, src := range sources {
		report(Progress{Stage: StageUnpack, Label: archiveLabel(index, len(sources))})
		dest := tree
		if index > 0 {
			dest = filepath.Join(scratch, fmt.Sprintf("extra-%d", index))
		}
		if err := extract(ctx, r.name, src.Dest, dest); err != nil {
			return nil, installFailure(r.name, err)
		}
		if index > 0 {
			if err := mergeTree(ctx, dest, tree); err != nil {
				return nil, installFailure(r.name, err)
			}
		}
	}
	report(Progress{Stage: StageVerify})
	items, err := os.ReadDir(tree)
	if err != nil {
		return nil, installFailure(r.name, err)
	}
	for _, item := range items {
		target := filepath.Join(dir, item.Name())
		if err := removeAll(target); err != nil {
			return nil, installFailure(r.name, err)
		}
		if err := renameHeld(ctx, filepath.Join(tree, item.Name()), target); err != nil {
			return nil, installFailure(r.name, err)
		}
	}
	if _, err := os.Stat(present); err != nil {
		return nil, &InstallError{Name: r.name, Cause: "unpack failed: " + r.present + " missing under " + dir}
	}
	return inst, nil
}
