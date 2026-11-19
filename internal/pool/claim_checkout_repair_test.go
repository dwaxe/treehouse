package pool

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/treehouse/v3/internal/vcs"
)

// Build the signed pre-checkout state directly: this fixture has never called
// markAcquired. Its worktree and partial files belong only to this claim.
func pendingCheckoutClaim(t *testing.T) (string, string, AcquireOptions, State) {
	t.Helper()
	repo, poolDir := setupLocalRepo(t)
	if err := os.MkdirAll(poolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(poolDir, "1", "myrepo")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := vcs.AddWorktree(repo, path, "main"); err != nil {
		t.Fatal(err)
	}
	commit, err := vcs.BranchCommit(repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	opts := AcquireOptions{SkipFetch: true, ClaimID: "partial-checkout"}
	internal := acquireOptions{skipFetch: true, lease: true, leaseHolder: "worker", claimID: opts.ClaimID}
	internal.claimFingerprint = claimFingerprint(internal, nil)
	wt := WorktreeEntry{Name: "1", Path: path, CreatedAt: time.Now(), Leased: true,
		LeaseHolder: acquisitionIncompleteLeaseHolder, LeasedAt: time.Now()}
	if err := beginClaim(repo, &wt, internal, "main", commit); err != nil {
		t.Fatal(err)
	}
	state := State{Version: stateVersion, Worktrees: []WorktreeEntry{wt}}
	if err := WriteState(poolDir, state); err != nil {
		t.Fatal(err)
	}
	return repo, poolDir, opts, state
}

func TestClaimCheckoutRepairRefusesUnprovenExclusiveReservation(t *testing.T) {
	for _, changed := range []string{"granted-lease", "unreserved", "other-holder", "owner-pid", "owner-start", "destroying", "unknown-phase", "competing-claim", "invalid-digest"} {
		t.Run(changed, func(t *testing.T) {
			repo, poolDir, opts, state := pendingCheckoutClaim(t)
			wt := &state.Worktrees[0]
			path := wt.Path
			tracked := filepath.Join(path, "README.md")
			untracked := filepath.Join(path, "caller-progress")
			if err := os.WriteFile(tracked, []byte("preserve tracked work\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(untracked, []byte("preserve untracked work\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			switch changed {
			case "granted-lease":
				wt.LeaseID = "already-granted"
			case "unreserved":
				wt.Leased = false
			case "other-holder":
				wt.LeaseHolder = "someone-else"
			case "owner-pid":
				wt.OwnerPID = int32(os.Getpid())
			case "owner-start":
				wt.OwnerStartedAt = 1
			case "destroying":
				wt.Destroying = true
			case "unknown-phase":
				wt.Acquisition.Phase = "unknown"
			case "competing-claim":
				other := *wt
				claim := *wt.Acquisition
				other.Name, claim.ID = "2", "another-task"
				other.Acquisition = &claim
				state.Worktrees = append(state.Worktrees, other)
			}
			if err := WriteState(poolDir, state); err != nil {
				t.Fatal(err)
			}
			if changed == "invalid-digest" {
				// Preserve the old signature while changing the authenticated
				// claim. A subsequent ReadState must never reauthenticate it.
				data, err := os.ReadFile(stateFilePath(poolDir))
				if err != nil {
					t.Fatal(err)
				}
				var raw State
				if err := json.Unmarshal(data, &raw); err != nil {
					t.Fatal(err)
				}
				raw.Worktrees[0].Acquisition.BaseCommit = strings.Repeat("0", 40)
				data, err = json.Marshal(raw)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(stateFilePath(poolDir), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(stateFilePath(poolDir))
			if err != nil {
				t.Fatal(err)
			}
			lease, err := AcquireLeaseInfoWithOptions(repo, poolDir, 2, nil, "worker", opts)
			if err == nil || lease != (LeaseInfo{}) {
				t.Fatalf("unproven %s reservation was repaired: lease=%+v err=%v", changed, lease, err)
			}
			after, err := os.ReadFile(stateFilePath(poolDir))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("refusal changed state: err=%v\nbefore=%s\nafter=%s", err, before, after)
			}
			assertFileContents(t, tracked, "preserve tracked work\n")
			assertFileContents(t, untracked, "preserve untracked work\n")
			if changed == "unreserved" || changed == "other-holder" {
				// ReadState protects unknown seed state by normalizing its
				// quarantine. A later status write must not turn inconsistent
				// original reservation metadata into repair authorization.
				for range 2 {
					if _, err := List(poolDir); err != nil {
						t.Fatal(err)
					}
					lease, err := AcquireLeaseInfoWithOptions(repo, poolDir, 2, nil, "worker", opts)
					if err == nil || lease != (LeaseInfo{}) {
						t.Fatalf("status reauthorized inconsistent %s reservation: %+v, %v", changed, lease, err)
					}
					assertFileContents(t, tracked, "preserve tracked work\n")
					assertFileContents(t, untracked, "preserve untracked work\n")
				}
			}
		})
	}
}

func TestClaimCheckoutRepairRestoresRecordedBaseWithoutMovingOldBranch(t *testing.T) {
	repo, poolDir, opts, state := pendingCheckoutClaim(t)
	wt := &state.Worktrees[0]
	path := wt.Path
	oldCommit := wt.Acquisition.BaseCommit
	runGit(t, path, "checkout", "-b", "old-slot-branch")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("new claim base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "new claim base")
	newCommit, err := vcs.BranchCommit(repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	wt.Acquisition.BaseCommit = newCommit
	if err := WriteState(poolDir, state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("half-written checkout\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".gitignore"), []byte("dead-ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dead-untracked", "dead-ignored"} {
		if err := os.WriteFile(filepath.Join(path, name), []byte("dead run\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lease, err := AcquireLeaseInfoWithOptions(repo, poolDir, 1, nil, "worker", opts)
	if err != nil || lease.Path != path || lease.LeaseID != wt.Acquisition.LeaseID {
		t.Fatalf("claim did not restore old checkout in place: %+v, %v", lease, err)
	}
	assertFileContents(t, filepath.Join(path, "README.md"), "new claim base\n")
	for _, name := range []string{".gitignore", "dead-untracked", "dead-ignored"} {
		if _, err := os.Lstat(filepath.Join(path, name)); !os.IsNotExist(err) {
			t.Fatalf("dead-run output %s remains after repair: %v", name, err)
		}
	}
	if ok, err := vcs.WorktreeAtCommit(path, newCommit); err != nil || !ok {
		t.Fatalf("repaired slot is not detached at recorded base: %t, %v", ok, err)
	}
	if got, err := vcs.BranchCommit(repo, "old-slot-branch"); err != nil || got != oldCommit {
		t.Fatalf("repair moved the old branch: %s, %v; want %s", got, err, oldCommit)
	}
}

func TestClaimCheckoutRepairCompletedClaimNeverResetsCallerWork(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	opts := AcquireOptions{SkipFetch: true, ClaimID: "already-complete"}
	first, err := AcquireLeaseInfoWithOptions(repo, poolDir, 1, nil, "worker", opts)
	if err != nil {
		t.Fatal(err)
	}
	tracked := filepath.Join(first.Path, "README.md")
	untracked := filepath.Join(first.Path, "caller-progress")
	if err := os.WriteFile(tracked, []byte("legitimate tracked work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(untracked, []byte("legitimate untracked work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(stateFilePath(poolDir))
	if err != nil {
		t.Fatal(err)
	}
	again, err := AcquireLeaseInfoWithOptions(repo, poolDir, 1, nil, "worker", opts)
	if err != nil || again.Path != first.Path || again.LeaseID != first.LeaseID ||
		again.LeaseHolder != first.LeaseHolder || !again.LeasedAt.Equal(first.LeasedAt) || again.BaseBranch != first.BaseBranch {
		t.Fatalf("completed claim changed its grant: first=%+v again=%+v err=%v", first, again, err)
	}
	after, err := os.ReadFile(stateFilePath(poolDir))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("completed replay changed state: err=%v\nbefore=%s\nafter=%s", err, before, after)
	}
	assertFileContents(t, tracked, "legitimate tracked work\n")
	assertFileContents(t, untracked, "legitimate untracked work\n")
}
