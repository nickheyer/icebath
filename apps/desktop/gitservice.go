package main

import (
	"github.com/nickheyer/icebath/pkg/desktop/files"
	"github.com/nickheyer/icebath/pkg/desktop/git"
)

// Binds the git group preload functions to pkg/desktop/git
type GitService struct{ svc *git.Service }

// Git group over the cached git and gh binaries the desktop resolves
func newGitService() *GitService {
	return &GitService{svc: git.NewService(files.ResolveGitBinary, files.ResolveGhBinary)}
}

func (b *GitService) GitWorktreeList(repoPath any) []git.Worktree {
	return b.svc.GitWorktreeList(repoPath)
}

func (b *GitService) GitWorktreeAdd(repoPath, options any) (git.AddedWorktree, error) {
	return b.svc.GitWorktreeAdd(repoPath, options)
}

func (b *GitService) GitWorktreeRemove(repoPath, worktreePath, options any) (git.RemovedWorktree, error) {
	return b.svc.GitWorktreeRemove(repoPath, worktreePath, options)
}

func (b *GitService) GitBranchSwitch(repoPath, branch any) (git.BranchSwitch, error) {
	return b.svc.GitBranchSwitch(repoPath, branch)
}

func (b *GitService) GitBranchList(repoPath any) []git.Branch { return b.svc.GitBranchList(repoPath) }

func (b *GitService) GitBaseBranchList(repoPath any) []git.BaseBranch {
	return b.svc.GitBaseBranchList(repoPath)
}

func (b *GitService) GitRepoStatus(repoPath any) *git.RepoStatus { return b.svc.GitRepoStatus(repoPath) }

func (b *GitService) GitFileDiff(repoPath, filePath any) (string, error) {
	return b.svc.GitFileDiff(repoPath, filePath)
}

func (b *GitService) GitScanRepos(roots, options any) []git.RepoScanResult {
	return b.svc.GitScanRepos(roots, options)
}

func (b *GitService) GitReviewList(repoPath, scope, baseRef any) (git.ReviewList, error) {
	return b.svc.GitReviewList(repoPath, scope, baseRef)
}

func (b *GitService) GitReviewDiff(repoPath, filePath, scope, baseRef, staged any) (string, error) {
	return b.svc.GitReviewDiff(repoPath, filePath, scope, baseRef, staged)
}

func (b *GitService) GitReviewStage(repoPath, filePath any) (git.OkResult, error) {
	return b.svc.GitReviewStage(repoPath, filePath)
}

func (b *GitService) GitReviewUnstage(repoPath, filePath any) (git.OkResult, error) {
	return b.svc.GitReviewUnstage(repoPath, filePath)
}

func (b *GitService) GitReviewRevert(repoPath, filePath any) (git.OkResult, error) {
	return b.svc.GitReviewRevert(repoPath, filePath)
}

func (b *GitService) GitReviewRevParse(repoPath, ref any) *string {
	return b.svc.GitReviewRevParse(repoPath, ref)
}

func (b *GitService) GitReviewCommit(repoPath, message, push any) (git.OkResult, error) {
	return b.svc.GitReviewCommit(repoPath, message, push)
}

func (b *GitService) GitReviewCommitContext(repoPath any) (git.CommitContext, error) {
	return b.svc.GitReviewCommitContext(repoPath)
}

func (b *GitService) GitReviewPush(repoPath any) (git.OkResult, error) {
	return b.svc.GitReviewPush(repoPath)
}

func (b *GitService) GitReviewShipInfo(repoPath any) git.ShipInfo {
	return b.svc.GitReviewShipInfo(repoPath)
}

func (b *GitService) GitReviewPrList(repoPath, branches, numbers any) (git.PrList, error) {
	return b.svc.GitReviewPrList(repoPath, branches, numbers)
}

func (b *GitService) GitReviewCreatePr(repoPath any) (git.CreatedPr, error) {
	return b.svc.GitReviewCreatePr(repoPath)
}
