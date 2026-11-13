package git

// Git group of the preload API over the git and gh binaries the desktop resolves
type Service struct {
	gitBinary func() string
	ghBinary  func() string
}

// Git group with the cached git and gh resolvers the rest of the desktop shares
func NewService(gitBinary, ghBinary func() string) *Service {
	return &Service{gitBinary: gitBinary, ghBinary: ghBinary}
}

func (s *Service) GitWorktreeList(repoPath any) []Worktree {
	return listWorktrees(repoPath, s.gitBinary())
}

func (s *Service) GitWorktreeAdd(repoPath, options any) (AddedWorktree, error) {
	return addWorktree(repoPath, options, s.gitBinary())
}

func (s *Service) GitWorktreeRemove(repoPath, worktreePath, options any) (RemovedWorktree, error) {
	return removeWorktree(repoPath, worktreePath, options, s.gitBinary())
}

func (s *Service) GitBranchSwitch(repoPath, branch any) (BranchSwitch, error) {
	return switchBranch(repoPath, branch, s.gitBinary())
}

func (s *Service) GitBranchList(repoPath any) []Branch { return listBranches(repoPath, s.gitBinary()) }

func (s *Service) GitBaseBranchList(repoPath any) []BaseBranch {
	return listBaseBranches(repoPath, s.gitBinary())
}

func (s *Service) GitRepoStatus(repoPath any) *RepoStatus { return repoStatus(repoPath, s.gitBinary()) }

func (s *Service) GitFileDiff(repoPath, filePath any) (string, error) {
	return fileDiffVsHead(repoPath, filePath, s.gitBinary())
}

// Repositories under the roots, an empty list on any failure
func (s *Service) GitScanRepos(roots, options any) []RepoScanResult {
	if !jsTruthy(roots) {
		roots = []any{}
	}
	found, err := ScanGitRepos(roots, options)
	if err != nil {
		return []RepoScanResult{}
	}
	return found
}

func (s *Service) GitReviewList(repoPath, scope, baseRef any) (ReviewList, error) {
	return reviewList(repoPath, scope, baseRef, s.gitBinary())
}

func (s *Service) GitReviewDiff(repoPath, filePath, scope, baseRef, staged any) (string, error) {
	return reviewDiff(repoPath, filePath, scope, baseRef, staged, s.gitBinary())
}

func (s *Service) GitReviewStage(repoPath, filePath any) (OkResult, error) {
	return reviewStage(repoPath, filePath, s.gitBinary())
}

func (s *Service) GitReviewUnstage(repoPath, filePath any) (OkResult, error) {
	return reviewUnstage(repoPath, filePath, s.gitBinary())
}

func (s *Service) GitReviewRevert(repoPath, filePath any) (OkResult, error) {
	return reviewRevert(repoPath, filePath, s.gitBinary())
}

func (s *Service) GitReviewRevParse(repoPath, ref any) *string {
	return reviewRevParse(repoPath, ref, s.gitBinary())
}

func (s *Service) GitReviewCommit(repoPath, message, push any) (OkResult, error) {
	return reviewCommit(repoPath, message, push, s.gitBinary())
}

func (s *Service) GitReviewCommitContext(repoPath any) (CommitContext, error) {
	return reviewCommitContext(repoPath, s.gitBinary())
}

func (s *Service) GitReviewPush(repoPath any) (OkResult, error) {
	return reviewPush(repoPath, s.gitBinary())
}

func (s *Service) GitReviewShipInfo(repoPath any) ShipInfo {
	return reviewShipInfo(repoPath, s.ghBinary())
}

func (s *Service) GitReviewPrList(repoPath, branches, numbers any) (PrList, error) {
	return reviewPrList(repoPath, branches, numbers, s.ghBinary())
}

func (s *Service) GitReviewCreatePr(repoPath any) (CreatedPr, error) {
	return reviewCreatePr(repoPath, s.gitBinary(), s.ghBinary())
}
