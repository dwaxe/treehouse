package pool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquireClaimSetupFailureKeepsOpenClaimForRetry(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reuse=%t", reuse), func(t *testing.T) {
			repo, poolDir := setupLocalRepo(t)
			var previous string
			if reuse {
				warm, err := AcquireLeaseInfo(repo, poolDir, 1, nil, "warm-up")
				if err != nil {
					t.Fatal(err)
				}
				previous = warm.Path
				if err := Release(poolDir, warm.Path); err != nil {
					t.Fatal(err)
				}
			}
			failure := filepath.Join(t.TempDir(), "setup-fails")
			if err := os.WriteFile(failure, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			opts := AcquireOptions{SkipFetch: true, ClaimID: "setup-retry"}
			hooks := []string{claimSetupProbeCommand(poolDir, failure), "echo ok > second-ran.txt"}
			var slot, futureID string
			for attempt := 0; attempt < 2; attempt++ {
				lease, err := AcquireLeaseInfoWithOptions(repo, poolDir, 1, hooks, "worker", opts)
				if err == nil || !strings.Contains(err.Error(), "exit 42") || lease != (LeaseInfo{}) {
					t.Fatalf("attempt %d granted failed setup: lease=%+v err=%v", attempt, lease, err)
				}
				state, err := ReadState(poolDir)
				if err != nil || len(state.Worktrees) != 1 {
					t.Fatalf("attempt %d changed pool count: %+v, %v", attempt, state, err)
				}
				wt := state.Worktrees[0]
				if !wt.Leased || wt.LeaseID != "" || wt.LeaseHolder != acquisitionIncompleteLeaseHolder || wt.Acquisition == nil || !wt.Acquisition.trusted || wt.Acquisition.Phase != "hooks" {
					t.Fatalf("failed setup did not keep an open quarantined claim: %+v", wt)
				}
				if attempt == 0 {
					slot, futureID = wt.Path, wt.Acquisition.LeaseID
				}
				if wt.Path != slot || wt.Acquisition.LeaseID != futureID || futureID == "" || (reuse && slot != previous) {
					t.Fatalf("attempt %d changed claim identity or area: %+v", attempt, wt)
				}
				assertFileContents(t, filepath.Join(slot, "setup-partial.txt"), "original\n")
				if _, err := os.Stat(filepath.Join(slot, "second-ran.txt")); !os.IsNotExist(err) {
					t.Fatalf("setup continued after failure: %v", err)
				}
			}
			if err := os.Remove(failure); err != nil {
				t.Fatal(err)
			}
			lease, err := AcquireLeaseInfoWithOptions(repo, poolDir, 1, hooks, "worker", opts)
			if err != nil || lease.Path != slot || lease.LeaseID != futureID || lease.LeaseHolder != "worker" {
				t.Fatalf("fixed setup did not grant its original area and identity: %+v, %v", lease, err)
			}
			assertFileContents(t, filepath.Join(slot, "setup-partial.txt"), "original\n")
			if _, err := os.Stat(filepath.Join(slot, "second-ran.txt")); err != nil {
				t.Fatalf("passing setup skipped the next command: %v", err)
			}
			state, err := ReadState(poolDir)
			if err != nil || len(state.Worktrees) != 1 || state.Worktrees[0].Acquisition.Phase != "complete" || state.Worktrees[0].LeaseID != futureID {
				t.Fatalf("successful grant did not complete the same claim: %+v, %v", state, err)
			}
		})
	}
}

func TestAcquireClaimResumedSetupCannotOverwriteChangedReservation(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	failure := filepath.Join(t.TempDir(), "setup-fails")
	if err := os.WriteFile(failure, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	opts := AcquireOptions{SkipFetch: true, ClaimID: "setup-replacement"}
	replace := quoteForShell(os.Args[0]) + " -test.run=^TestReplacePostCreateReservationProbe$ -- " + quoteForShell(poolDir)
	hooks := []string{claimSetupProbeCommand(poolDir, failure), replace}
	if lease, err := AcquireLeaseInfoWithOptions(repo, poolDir, 1, hooks, "original", opts); err == nil || lease != (LeaseInfo{}) {
		t.Fatalf("setup fixture did not fail before replacement: %+v, %v", lease, err)
	}
	if err := os.Remove(failure); err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireLeaseInfoWithOptions(repo, poolDir, 1, hooks, "original", opts)
	if err == nil || !strings.Contains(err.Error(), "reservation changed") || lease != (LeaseInfo{}) {
		t.Fatalf("resumed setup overwrote its replacement: %+v, %v", lease, err)
	}
	state, err := ReadState(poolDir)
	if err != nil || len(state.Worktrees) != 1 || state.Worktrees[0].LeaseHolder != "replacement" || state.Worktrees[0].LeaseID == "" || state.Worktrees[0].Acquisition != nil {
		t.Fatalf("replacement reservation changed: %+v, %v", state, err)
	}
}

func claimSetupProbeCommand(poolDir, failure string) string {
	return quoteForShell(os.Args[0]) + " -test.run=^TestClaimSetupBeforeGrantProbe$ -- " + quoteForShell(poolDir) + " " + quoteForShell(failure)
}

func TestClaimSetupBeforeGrantProbe(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-3] != "--" {
		return
	}
	poolDir, failure := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	state, err := ReadState(poolDir)
	if err != nil || len(state.Worktrees) != 1 {
		t.Fatalf("setup state: %+v, %v", state, err)
	}
	wt := state.Worktrees[0]
	if !wt.Leased || wt.LeaseID != "" || wt.LeaseHolder != acquisitionIncompleteLeaseHolder || wt.Acquisition == nil || !wt.Acquisition.trusted || wt.Acquisition.Phase != "hooks" {
		t.Fatalf("setup ran after grant or without its open claim: %+v", wt)
	}
	if _, err := os.Stat("setup-partial.txt"); os.IsNotExist(err) {
		if err := os.WriteFile("setup-partial.txt", []byte("original\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(failure); err == nil {
		os.Exit(42)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestAcquireClaimChangedOptionsRefuseWithoutChangingGrant(t *testing.T) {
	for _, changed := range []string{"holder", "branch", "base", "manifest", "hooks"} {
		t.Run(changed, func(t *testing.T) {
			repo, poolDir := setupLocalRepo(t)
			opts := AcquireOptions{SkipFetch: true, ClaimID: "task-one"}
			original, err := AcquireLeaseInfoWithOptions(repo, poolDir, 2, nil, "worker", opts)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(stateFilePath(poolDir))
			if err != nil {
				t.Fatal(err)
			}
			holder := "worker"
			var hooks []string
			switch changed {
			case "holder":
				holder = "someone-else"
			case "branch":
				opts.Branch = "dwaxe/different-branch"
			case "base":
				opts.BaseBranch = "main"
			case "manifest":
				opts.IncludeManifest = []byte{}
			case "hooks":
				hooks = []string{"echo changed"}
			}
			if _, err := AcquireLeaseInfoWithOptions(repo, poolDir, 2, hooks, holder, opts); err == nil || !strings.Contains(err.Error(), "options") {
				t.Fatalf("changed %s must refuse options mismatch, got %v", changed, err)
			}
			after, err := os.ReadFile(stateFilePath(poolDir))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("refusal changed original grant %s: %v\nbefore=%s\nafter=%s", original.LeaseID, err, before, after)
			}
		})
	}
}

func TestAcquireClaimInvalidDigestRemainsQuarantinedAfterList(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	opts := AcquireOptions{SkipFetch: true, ClaimID: "task-one"}
	if _, err := AcquireLeaseInfoWithOptions(repo, poolDir, 2, nil, "worker", opts); err != nil {
		t.Fatal(err)
	}
	state, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	state.Worktrees[0].AcquisitionDigest = "tampered"
	// Bypass WriteState deliberately: this is corruption by an external
	// writer, which a later List must not accidentally authenticate.
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFilePath(poolDir), data, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := List(poolDir); err != nil {
			t.Fatal(err)
		}
		state, err = ReadState(poolDir)
		if err != nil || len(state.Worktrees) != 1 {
			t.Fatalf("List lost quarantined claim: %+v, %v", state, err)
		}
		wt := state.Worktrees[0]
		if !wt.Leased || wt.LeaseHolder != acquisitionIncompleteLeaseHolder || wt.Acquisition == nil || wt.Acquisition.trusted || wt.AcquisitionDigest != "" {
			t.Fatalf("List reauthenticated or freed invalid claim: %+v", wt)
		}
	}
	if _, err := AcquireLeaseInfoWithOptions(repo, poolDir, 2, nil, "worker", opts); err == nil {
		t.Fatal("invalid claim was resumed after List")
	}
}

func TestAcquireClaimReleaseDropsMetadataAndNextLeaseHasNewID(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	opts := AcquireOptions{SkipFetch: true, ClaimID: "task-one"}
	first, err := AcquireLeaseInfoWithOptions(repo, poolDir, 1, nil, "worker", opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := Release(poolDir, first.Path); err != nil {
		t.Fatal(err)
	}
	state, err := ReadState(poolDir)
	if err != nil || len(state.Worktrees) != 1 {
		t.Fatalf("state after release: %+v, %v", state, err)
	}
	wt := state.Worktrees[0]
	if wt.Leased || wt.LeaseID != "" || wt.Acquisition != nil || wt.AcquisitionDigest != "" {
		t.Fatalf("release retained claim metadata: %+v", wt)
	}
	second, err := AcquireLeaseInfoWithOptions(repo, poolDir, 1, nil, "worker", opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path != second.Path || first.LeaseID == second.LeaseID || second.LeaseID == "" {
		t.Fatalf("released claim was not a fresh lease in the same slot: first=%+v second=%+v", first, second)
	}
}

func TestAcquireClaimCompletedBranchRetryPreservesWork(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	opts := AcquireOptions{SkipFetch: true, ClaimID: "task-one", Branch: "dwaxe/claim-replay"}
	first, err := AcquireLeaseInfoWithOptions(repo, poolDir, 2, nil, "worker", opts)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(first.Path, "README.md")
	if err := os.WriteFile(path, []byte("caller work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireLeaseInfoWithOptions(repo, poolDir, 2, nil, "worker", opts)
	if err != nil || second.Path != first.Path || second.LeaseID != first.LeaseID {
		t.Fatalf("existing branch prevented same-claim replay: first=%+v second=%+v err=%v", first, second, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "caller work\n" {
		t.Fatalf("completed replay changed work: %q, %v", data, err)
	}
}

func TestAcquireClaimDifferentCloneCannotResumeExistingOrMissingSlot(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing", true: "missing"}[missing], func(t *testing.T) {
			cloneA, cloneB, poolDir := setupSharedClonePool(t)
			opts := AcquireOptions{SkipFetch: true, ClaimID: "task-one"}
			first, err := AcquireLeaseInfoWithOptions(cloneA, poolDir, 2, nil, "worker", opts)
			if err != nil {
				t.Fatal(err)
			}
			if missing {
				state, err := ReadState(poolDir)
				if err != nil {
					t.Fatal(err)
				}
				// Recreate a signed pre-add reservation whose checkout never
				// appeared. The clone identity still belongs to clone A.
				wt := &state.Worktrees[0]
				wt.Acquisition.Phase = "checkout"
				wt.LeaseID = ""
				wt.LeaseHolder = acquisitionIncompleteLeaseHolder
				setSeedInventory(wt, nil, false)
				runGit(t, cloneA, "worktree", "remove", "--force", first.Path)
				if err := WriteState(poolDir, state); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(stateFilePath(poolDir))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := AcquireLeaseInfoWithOptions(cloneB, poolDir, 2, nil, "worker", opts); err == nil {
				t.Fatal("same claim was adopted by a different clone")
			}
			after, err := os.ReadFile(stateFilePath(poolDir))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("foreign retry changed original reservation: %v\nbefore=%s\nafter=%s", err, before, after)
			}
			if missing {
				if _, err := os.Lstat(first.Path); !os.IsNotExist(err) {
					t.Fatalf("foreign retry created original clone's slot: %v", err)
				}
			}
		})
	}
}

func TestAcquireClaimRequiresDurableLeaseThroughPoolAPI(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	if _, err := AcquireWithOptions(repo, poolDir, 1, nil, AcquireOptions{SkipFetch: true, ClaimID: "task-one"}); err == nil || !strings.Contains(err.Error(), "requires --lease") {
		t.Fatalf("non-lease pool API accepted claim: %v", err)
	}
}

func TestAcquireWithoutClaimCannotInheritFailedClaimIdentity(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	warm, err := AcquireLeaseInfo(repo, poolDir, 1, nil, "warm-up")
	if err != nil {
		t.Fatal(err)
	}
	if err := Release(poolDir, warm.Path); err != nil {
		t.Fatal(err)
	}
	oldCreateBranch := createBranch
	t.Cleanup(func() { createBranch = oldCreateBranch })
	var failedID string
	createBranch = func(string, string) error {
		state, err := ReadState(poolDir)
		if err != nil {
			t.Fatal(err)
		}
		failedID = state.Worktrees[0].Acquisition.LeaseID
		return errors.New("injected branch failure before creation")
	}
	if _, err := AcquireLeaseInfoWithOptions(repo, poolDir, 1, nil, "worker", AcquireOptions{SkipFetch: true, ClaimID: "failed-task", Branch: "dwaxe/fails"}); err == nil {
		t.Fatal("branch fixture did not fail")
	}
	createBranch = oldCreateBranch
	if failedID == "" {
		t.Fatal("branch fixture did not record interrupted identity")
	}
	state, err := ReadState(poolDir)
	if err != nil || len(state.Worktrees) != 1 || state.Worktrees[0].Leased {
		t.Fatalf("safe branch failure should make slot reusable: %+v, %v", state, err)
	}
	next, err := AcquireLeaseInfo(repo, poolDir, 1, nil, "worker")
	if err != nil || next.LeaseID == failedID {
		t.Fatalf("ordinary acquisition inherited failed claim: %+v, %v", next, err)
	}
	state, err = ReadState(poolDir)
	if err != nil || state.Worktrees[0].Acquisition != nil || state.Worktrees[0].AcquisitionDigest != "" {
		t.Fatalf("ordinary acquisition retained failed claim metadata: %+v, %v", state, err)
	}
}
