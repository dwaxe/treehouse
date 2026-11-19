package gitvcs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AddClaimWorktree uses the recorded commit rather than a possibly moved branch.
// Only the pool's authenticated never-granted claim may request this operation.
func AddClaimWorktree(repoRoot, worktreePath, commit string) error {
	if !isCommitID(commit) {
		return fmt.Errorf("claim repair requires a resolved base commit")
	}
	_, err := runGit(repoRoot, "worktree", "add", "--force", "--detach", worktreePath, commit)
	return err
}

// ResetClaimCheckout is deliberately separate from ordinary reset: its caller
// proves exclusive ownership by an authenticated claim that never granted a
// lease. It may discard all partial files and the dead checkout's Git locks.
func ResetClaimCheckout(worktreePath, commit string) error {
	if !isCommitID(commit) {
		return fmt.Errorf("claim repair requires a resolved base commit")
	}
	worktree, err := openDirectoryNoFollow(worktreePath)
	if err != nil {
		return err
	}
	defer worktree.Close()
	identity, err := worktree.Stat()
	if err != nil {
		return err
	}
	authenticate := func() error {
		root, dir, err := openCleanupRoot(worktreePath, identity, authenticateLinkedWorktree)
		if err == nil {
			root.Close()
			dir.Close()
		}
		return err
	}
	if err := authenticate(); err != nil {
		return err
	}
	if _, err := refCommit(worktreePath, commit); err != nil {
		return err
	}
	head, err := worktreeHead(worktreePath)
	if err != nil {
		return err
	}
	headPath, err := gitPath(worktreePath, "HEAD")
	if err != nil {
		return err
	}
	guard, err := takeHeadGuard(headPath)
	if err != nil {
		return err
	}
	// The pool holds both claim and state locks and has excluded live children.
	// Mid-checkout Git leaves anonymous locks; only this never-granted path may
	// discard them. Do not remove the persistent Treehouse reset guard itself.
	for _, name := range []string{"HEAD.lock", "index.lock"} {
		lockPath, err := gitPath(worktreePath, name)
		if err == nil {
			info, statErr := os.Lstat(lockPath)
			if os.IsNotExist(statErr) {
				continue
			}
			if statErr != nil {
				err = statErr
			} else if !info.Mode().IsRegular() {
				err = fmt.Errorf("refusing non-regular checkout lock %s", lockPath)
			} else {
				err = os.Remove(lockPath)
			}
		}
		if err != nil {
			guard.Close()
			return err
		}
	}
	guard.Close()
	// Reuse the guarded detached-HEAD update. Its verifier runs under HEAD's
	// lock before read-tree, so even an attached old HEAD cannot move a branch.
	if err := resetWorktreeToRef(worktreePath, commit, head, false, nil, false, func() error {
		if err := authenticate(); err != nil {
			return err
		}
		_, err := runGit(worktreePath, "clean", "-ffdx")
		return err
	}); err != nil {
		return err
	}
	// Git marks a worktree add as initializing until checkout returns. Clear
	// only that dead-run marker; a separately requested worktree lock stays.
	locked := filepath.Join(filepath.Dir(headPath), "locked")
	if data, err := os.ReadFile(locked); err == nil && strings.TrimSpace(string(data)) == "initializing" {
		return os.Remove(locked)
	}
	return nil
}
