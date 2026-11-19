package pool

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kunchenguid/treehouse/v3/internal/hooks"
	"github.com/kunchenguid/treehouse/v3/internal/process"
	"github.com/kunchenguid/treehouse/v3/internal/vcs"
)

// A claim is opt-in: holder labels keep their existing diagnostic meaning.
// Persist each destructive boundary before entering it. In particular, an
// unknown seed inventory cannot be treated as an empty one on retry.
type acquisitionClaim struct {
	ID           string `json:"id"`
	Fingerprint  string `json:"fingerprint"`
	BaseBranch   string `json:"base_branch"`
	BaseCommit   string `json:"base_commit"`
	CommonGitDir string `json:"common_git_dir"`
	LeaseID      string `json:"lease_id"`
	Phase        string `json:"phase"`
	trusted      bool
}

func acquisitionDigest(key []byte, wt WorktreeEntry) string {
	data, _ := json.Marshal(struct {
		Name  string
		Path  string
		Claim *acquisitionClaim
	}{wt.Name, filepath.Clean(wt.Path), wt.Acquisition})
	digest := hmac.New(sha256.New, key)
	_, _ = digest.Write(data)
	return hex.EncodeToString(digest.Sum(nil))
}

func validAcquisitionDigest(key []byte, wt WorktreeEntry) bool {
	return wt.Acquisition != nil && wt.AcquisitionDigest != "" &&
		hmac.Equal([]byte(wt.AcquisitionDigest), []byte(acquisitionDigest(key, wt)))
}

func claimFingerprint(opts acquireOptions, postCreate []string) string {
	data, _ := json.Marshal(struct {
		Options AcquireOptions
		Holder  string
		Hooks   []string
	}{AcquireOptions{SkipFetch: opts.skipFetch, BaseBranch: opts.baseBranch,
		Branch: opts.branch, UniqueLeaf: opts.uniqueLeaf, WorktreePath: opts.worktreePath,
		IncludeManifest: opts.includeManifest, APFSSharing: opts.apfsSharing}, opts.leaseHolder, postCreate})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func acquire(repoRoot, poolDir string, poolSize int, postCreate []string, opts acquireOptions) (LeaseInfo, error) {
	if opts.claimID == "" {
		return acquireOnce(repoRoot, poolDir, poolSize, postCreate, opts)
	}
	if !opts.lease || vcs.BackendNameFor(repoRoot) != "git" {
		return LeaseInfo{}, fmt.Errorf("--claim-id requires --lease with the git backend")
	}
	if err := os.MkdirAll(poolDir, 0755); err != nil {
		return LeaseInfo{}, err
	}
	// The kernel releases this lock on death. Hold it through setup, without
	// holding the whole pool's state lock while user hooks execute. Never unlink
	// it: a waiter must always lock the same inode. Hash untrusted IDs for paths.
	sum := sha256.Sum256([]byte(opts.claimID))
	f, err := os.OpenFile(filepath.Join(poolDir, "treehouse-claim-"+hex.EncodeToString(sum[:])+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return LeaseInfo{}, err
	}
	defer f.Close()
	if err := lockFile(f); err != nil {
		return LeaseInfo{}, err
	}
	defer unlockFile(f)
	opts.claimFingerprint = claimFingerprint(opts, postCreate)
	return acquireOnce(repoRoot, poolDir, poolSize, postCreate, opts)
}

func beginClaim(repoRoot string, wt *WorktreeEntry, opts acquireOptions, base, commit string) error {
	if opts.claimID == "" {
		return nil
	}
	id, err := newLeaseID()
	if err != nil {
		return err
	}
	common, err := vcs.CommonGitDir(repoRoot)
	if err != nil {
		return err
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return err
	}
	wt.Acquisition = &acquisitionClaim{ID: opts.claimID, Fingerprint: opts.claimFingerprint,
		BaseBranch: base, BaseCommit: commit, CommonGitDir: common, LeaseID: id, Phase: "checkout", trusted: true}
	wt.AcquisitionDigest = ""
	return nil
}

func claimPhase(wt *WorktreeEntry, phase string) {
	if wt.Acquisition != nil {
		wt.Acquisition.Phase = phase
	}
}

// resumeClaim is called before healing/allocation, so a missing path cannot
// erase its reservation and an unsafe repair never silently consumes a slot.
func resumeClaim(repoRoot, poolDir string, state *State, opts acquireOptions) (LeaseInfo, bool, bool, error) {
	if opts.claimID == "" {
		return LeaseInfo{}, false, false, nil
	}
	index := -1
	for i, wt := range state.Worktrees {
		if wt.Acquisition != nil && wt.Acquisition.ID == opts.claimID {
			if index != -1 {
				return LeaseInfo{}, true, false, fmt.Errorf("claim %q names multiple slots; inspect pool state", opts.claimID)
			}
			index = i
		}
	}
	if index == -1 {
		return LeaseInfo{}, false, false, nil
	}
	wt := &state.Worktrees[index]
	c := wt.Acquisition
	fail := func(reason string) (LeaseInfo, bool, bool, error) {
		return LeaseInfo{}, true, false, fmt.Errorf("cannot resume claim %q in %s: %s; slot preserved for inspection", opts.claimID, wt.Path, reason)
	}
	if !c.trusted || !wt.Leased || wt.Destroying || c.Fingerprint != opts.claimFingerprint {
		return fail("claim authentication, reservation, or acquisition options do not match")
	}
	requester, requestErr := acquisitionCommonGitDir(repoRoot)
	original, originalErr := os.Stat(c.CommonGitDir)
	if requestErr != nil || originalErr != nil || !os.SameFile(requester, original) {
		return fail("claim belongs to another clone or its identity cannot be verified")
	}
	if _, err := os.Lstat(wt.Path); os.IsNotExist(err) && c.Phase == "checkout" {
		// With no directory there are no slot contents to discard. Git's add
		// handles a stale registration only for this exact path.
		commit, err := vcs.BranchCommit(repoRoot, c.BaseBranch)
		if err != nil || commit != c.BaseCommit {
			return fail("original base moved or cannot be verified")
		}
		if err := os.MkdirAll(filepath.Dir(wt.Path), 0755); err != nil {
			return fail(err.Error())
		}
		if err := vcs.AddWorktree(repoRoot, wt.Path, c.BaseBranch); err != nil {
			return fail(err.Error())
		}
	}
	flavor, err := vcs.WorktreeBackendNameChecked(wt.Path)
	if err != nil || flavor != "git" {
		return fail("worktree marker cannot be authenticated")
	}
	candidate, candidateErr := acquisitionCommonGitDir(wt.Path)
	if requestErr != nil || candidateErr != nil || !os.SameFile(requester, candidate) {
		return fail("worktree is not provably owned by this clone")
	}
	if c.Phase == "complete" {
		if wt.LeaseID != c.LeaseID || wt.LeaseHolder != opts.leaseHolder {
			return fail("lease identity changed")
		}
		// A lost reply must not reset work that already belongs to the caller.
		return leaseInfoFromEntry(*wt, c.BaseBranch), true, false, nil
	}
	inUse, err := process.IsWorktreeInUse(wt.Path)
	if err != nil || inUse {
		return fail("worktree processes are present or cannot be verified")
	}
	if c.Phase == "checkout" {
		matches, err := vcs.WorktreeAtCommit(wt.Path, c.BaseCommit)
		if err != nil || !matches {
			return fail("checkout is not detached at its original commit")
		}
		dirty, err := vcs.IsDirty(wt.Path)
		if err != nil || dirty {
			return fail("checkout has partial or modified tracked content")
		}
		unknown, err := vcs.HasUnseededBranchCreationOutput(wt.Path, nil)
		if err != nil || unknown {
			return fail("checkout contains unrecorded files")
		}
		c.Phase = "seeding"
		if err := persistState(poolDir, *state); err != nil {
			return fail(err.Error())
		}
		paths, err := seedWorktree(repoRoot, wt.Path, opts.includeManifest)
		if err != nil {
			return fail(err.Error())
		}
		setSeedInventory(wt, paths, true)
		if opts.branch != "" {
			if err := createBranch(wt.Path, opts.branch); err != nil {
				return fail(err.Error())
			}
		}
		if opts.apfsSharing {
			return fail("interrupted APFS sharing requires inspection")
		}
		claimPhase(wt, "hooks")
		if err := persistState(poolDir, *state); err != nil {
			return fail(err.Error())
		}
	}
	if c.Phase != "hooks" || !wt.SeedInventoryKnown || wt.LeaseID != "" || wt.LeaseHolder != acquisitionIncompleteLeaseHolder {
		return fail("interrupted seeding or preparation cannot be safely replayed")
	}
	return leaseInfoFromEntry(*wt, c.BaseBranch), true, true, nil
}

// Setup is required on every open acquisition, including claim repair. Keep
// the quarantine and claim open on failure. Only the matching reservation can
// become the caller's lease, and that grant completes the claim atomically.
func runAcquisitionHooks(poolDir string, acquired LeaseInfo, postCreate []string, opts acquireOptions) (LeaseInfo, error) {
	if err := hooks.RunRequired(postCreate, acquired.Path, opts.hookStdout, opts.hookStderr); err != nil {
		return LeaseInfo{}, fmt.Errorf("post_create failed in %s; worktree quarantined for inspection: %w", acquired.Path, err)
	}
	err := WithStateLock(poolDir, func() error {
		state, err := ReadState(poolDir)
		if err != nil {
			return err
		}
		for i := range state.Worktrees {
			wt := &state.Worktrees[i]
			if wt.Path != acquired.Path {
				continue
			}
			// An explicit return or destroy during setup must not let this
			// acquisition overwrite a newer reservation.
			if wt.Destroying || !wt.Leased || wt.LeaseID != acquired.LeaseID ||
				wt.LeaseHolder != acquisitionIncompleteLeaseHolder || !wt.LeasedAt.Equal(acquired.LeasedAt) {
				break
			}
			if opts.claimID != "" && (wt.Acquisition == nil || !wt.Acquisition.trusted ||
				wt.Acquisition.ID != opts.claimID || wt.Acquisition.Fingerprint != opts.claimFingerprint || wt.Acquisition.Phase != "hooks") {
				break
			}
			clearLease(wt)
			if err := markAcquired(wt, opts); err != nil {
				return err
			}
			acquired = leaseInfoFromEntry(*wt, acquired.BaseBranch)
			return persistState(poolDir, state)
		}
		return fmt.Errorf("acquisition reservation changed during post_create in %s", acquired.Path)
	})
	if err != nil {
		return LeaseInfo{}, err
	}
	return acquired, nil
}
