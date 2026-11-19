//go:build unix

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/treehouse/v3/internal/process"
)

// Kill real acquisitions at the two boundaries where a caller has no reply:
// Git has checked out the files but worktree add has not returned, or a
// post_create command is still running before any caller lease is granted.
func TestGetClaimRepairsInterruptedAcquisition(t *testing.T) {
	for _, phase := range []string{"checkout", "setup"} {
		for _, capacity := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/capacity=%d", phase, capacity), func(t *testing.T) {
				repo, home := setupTestRepo(t)
				lab := t.TempDir()
				ready, allow := filepath.Join(lab, "ready"), filepath.Join(lab, "allow")
				failure := filepath.Join(lab, "setup-fails")
				env := []string{
					"TREEHOUSE_ROOT=" + filepath.Join(lab, "root"),
					"TREEHOUSE_TEST_CLAIM_READY=" + ready,
					"TREEHOUSE_TEST_CLAIM_ALLOW=" + allow,
					"TREEHOUSE_TEST_CLAIM_FAIL=" + failure,
				}
				// The allow file changes external readiness without changing the
				// acquisition command or hook configuration on retry.
				barrier := "if [ ! -f \"$TREEHOUSE_TEST_CLAIM_ALLOW\" ]; then " +
					"pwd > \"$TREEHOUSE_TEST_CLAIM_READY\"; " +
					"while [ ! -f \"$TREEHOUSE_TEST_CLAIM_ALLOW\" ]; do sleep 0.02; done; fi"
				configText := fmt.Sprintf("max_trees = %d\n", capacity)
				if phase == "checkout" {
					if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", "post-checkout"), []byte("#!/bin/sh\n"+barrier+"\n"), 0o755); err != nil {
						t.Fatal(err)
					}
				} else {
					// A setup retry must preserve earlier hook output instead of
					// resetting the tree. This ignored file survives the kill.
					gitCmd(t, repo, "config", "core.excludesFile", filepath.Join(lab, "ignored"))
					if err := os.WriteFile(filepath.Join(lab, "ignored"), []byte("setup-preserved\nsetup-completed\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(failure, nil, 0o644); err != nil {
						t.Fatal(err)
					}
					hook := "if [ ! -f setup-preserved ]; then printf original > setup-preserved; fi; " + barrier +
						"; if [ -f \"$TREEHOUSE_TEST_CLAIM_FAIL\" ]; then exit 42; fi; printf complete > setup-completed"
					configText += fmt.Sprintf("[hooks]\npost_create = [%q]\n", hook)
				}
				configDir := filepath.Join(home, ".config", "treehouse")
				if err := os.MkdirAll(configDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(configText), 0o644); err != nil {
					t.Fatal(err)
				}
				args := []string{"get", "--lease", "--no-fetch", "--json", "--lease-holder", "worker-A", "--claim-id", "interrupted-task"}
				slot := killClaimAtBarrier(t, repo, home, env, args, ready)
				before := claimStatuses(t, repo, home, env)
				if len(before) != 1 || before[0].Path != slot {
					t.Fatalf("interruption must retain exactly the original slot: %+v; slot=%s", before, slot)
				}
				originalID := interruptedClaimLeaseID(t, slot)
				if originalID == "" {
					t.Fatal("interrupted claim did not retain its lease identity")
				}
				if before[0].LeaseID != "" || !strings.Contains(before[0].LeaseHolder, "acquisition state incomplete") {
					t.Fatalf("interrupted %s granted a caller lease or lost quarantine: %+v", phase, before[0])
				}
				pendingPhase := "checkout"
				if phase == "setup" {
					pendingPhase = "hooks"
				}
				assertPendingClaim(t, slot, pendingPhase, originalID)
				if capacity == 1 {
					// A holder label alone must not authorize another claim to
					// repair, reset, or replace the interrupted acquisition.
					other := append([]string(nil), args...)
					other[len(other)-1] = "different-task"
					out, stderr, code := runTreehouse(t, repo, home, env, other...)
					if code == 0 || strings.TrimSpace(out) != "" {
						t.Fatalf("different claim took interrupted slot: code=%d stdout=%q stderr=%s", code, out, stderr)
					}
					afterOther := claimStatuses(t, repo, home, env)
					if len(afterOther) != 1 || afterOther[0].Path != slot || interruptedClaimLeaseID(t, slot) != originalID {
						t.Fatalf("different claim changed interrupted reservation: %+v", afterOther)
					}
				}
				if phase == "setup" {
					if data, err := os.ReadFile(filepath.Join(slot, "setup-preserved")); err != nil || string(data) != "original" {
						t.Fatalf("setup did not leave expected pre-kill output: %q, %v", data, err)
					}
				}
				if err := os.WriteFile(allow, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				if phase == "setup" {
					// A real kill and an explicit setup failure must both leave
					// the same claim open. Changing external readiness must be
					// enough for a later identical invocation to complete it.
					out, stderr, code := runTreehouse(t, repo, home, env, args...)
					if code == 0 || out != "" || !strings.Contains(stderr, "exit 42") {
						t.Fatalf("failed setup retry granted a lease: code=%d stdout=%q stderr=%s", code, out, stderr)
					}
					failed := claimStatuses(t, repo, home, env)
					if len(failed) != 1 || failed[0].Path != slot || failed[0].LeaseID != "" || !strings.Contains(failed[0].LeaseHolder, "acquisition state incomplete") {
						t.Fatalf("failed setup retry changed its pending area: %+v", failed)
					}
					assertPendingClaim(t, slot, "hooks", originalID)
					if _, err := os.Stat(filepath.Join(slot, "setup-completed")); !os.IsNotExist(err) {
						t.Fatalf("failed setup ran its later command: %v", err)
					}
					if err := os.Remove(failure); err != nil {
						t.Fatal(err)
					}
				}
				out, stderr, code := runTreehouse(t, repo, home, env, args...)
				var lease leaseJSONResult
				if err := json.Unmarshal([]byte(out), &lease); err != nil || code != 0 {
					t.Fatalf("identical retry failed: code=%d stdout=%q stderr=%s json=%v", code, out, stderr, err)
				}
				if lease.Path != slot || lease.LeaseID != originalID || lease.LeaseHolder != "worker-A" {
					t.Fatalf("retry must complete original claim: %+v; path=%s lease_id=%s", lease, slot, originalID)
				}
				after := claimStatuses(t, repo, home, env)
				if len(after) != len(before) || after[0].Path != slot || after[0].LeaseID != originalID || after[0].Status != "leased" || after[0].LeaseHolder != "worker-A" {
					t.Fatalf("retry must repair original slot without consuming capacity: before=%+v after=%+v", before, after)
				}
				if phase == "setup" {
					for name, want := range map[string]string{"setup-preserved": "original", "setup-completed": "complete"} {
						if data, err := os.ReadFile(filepath.Join(slot, name)); err != nil || string(data) != want {
							t.Fatalf("setup retry %s = %q, %v; want %q", name, data, err, want)
						}
					}
				}
				// A lost successful reply is the adjacent boundary: retrying
				// again must return the same grant and leave the pool size fixed.
				preserve := map[string]string{
					"README.md":     "tracked work after acquisition\n",
					"task-progress": "untracked work after acquisition\n",
				}
				if phase == "setup" {
					preserve["setup-completed"] = "caller changed setup output\n"
				}
				for name, contents := range preserve {
					if err := os.WriteFile(filepath.Join(slot, name), []byte(contents), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				out, stderr, code = runTreehouse(t, repo, home, env, args...)
				var repeated leaseJSONResult
				if err := json.Unmarshal([]byte(out), &repeated); err != nil || code != 0 || repeated.Path != slot || repeated.LeaseID != originalID {
					t.Fatalf("completed claim retry changed grant: code=%d lease=%+v stderr=%s json=%v", code, repeated, stderr, err)
				}
				if statuses := claimStatuses(t, repo, home, env); len(statuses) != 1 {
					t.Fatalf("completed claim retry consumed capacity: %+v", statuses)
				}
				for name, contents := range preserve {
					if data, err := os.ReadFile(filepath.Join(slot, name)); err != nil || string(data) != contents {
						t.Fatalf("completed retry changed %s: %q, %v; want %q", name, data, err, contents)
					}
				}
			})
		}
	}
}

func claimStatuses(t *testing.T, repo, home string, env []string) []statusJSONResult {
	t.Helper()
	out, stderr, code := runTreehouse(t, repo, home, env, "status", "--json")
	var statuses []statusJSONResult
	if err := json.Unmarshal([]byte(out), &statuses); err != nil || code != 0 {
		t.Fatalf("status: code=%d stdout=%q stderr=%s json=%v", code, out, stderr, err)
	}
	return statuses
}

func interruptedClaimLeaseID(t *testing.T, slot string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(slot)), "treehouse-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Worktrees []struct {
			Path        string `json:"path"`
			LeaseID     string `json:"lease_id"`
			Acquisition *struct {
				LeaseID string `json:"lease_id"`
			} `json:"acquisition"`
		} `json:"worktrees"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	for _, entry := range state.Worktrees {
		if entry.Path == slot {
			if entry.Acquisition != nil {
				return entry.Acquisition.LeaseID
			}
			return entry.LeaseID
		}
	}
	t.Fatalf("interrupted slot %s absent from state", slot)
	return ""
}

func assertPendingClaim(t *testing.T, slot, phase, futureID string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(slot)), "treehouse-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Worktrees []struct {
			Path        string `json:"path"`
			Leased      bool   `json:"leased"`
			LeaseID     string `json:"lease_id"`
			LeaseHolder string `json:"lease_holder"`
			Acquisition *struct {
				ID      string `json:"id"`
				Phase   string `json:"phase"`
				LeaseID string `json:"lease_id"`
			} `json:"acquisition"`
		} `json:"worktrees"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Worktrees) != 1 {
		t.Fatalf("pending claim changed pool count: %+v", state)
	}
	entry := state.Worktrees[0]
	if entry.Path != slot || !entry.Leased || entry.LeaseID != "" || entry.LeaseHolder != "quarantined: acquisition state incomplete" ||
		entry.Acquisition == nil || entry.Acquisition.ID != "interrupted-task" || entry.Acquisition.Phase != phase || entry.Acquisition.LeaseID != futureID {
		t.Fatalf("claim must stay open in its quarantined area without granting a caller lease: %+v", entry)
	}
}

func killClaimAtBarrier(t *testing.T, repo, home string, env, args []string, ready string) string {
	t.Helper()
	cmd := exec.Command(treehouseBin, args...)
	cmd.Dir = repo
	cmd.Env = buildEnv(home, env...)
	// Only this acquisition and its children receive the kill. Child Git
	// and hook processes must be gone before retry examines the worktree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	killAndWait := func() {
		if waited {
			return
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		waited = true
	}
	t.Cleanup(killAndWait)
	deadline := time.Now().Add(15 * time.Second)
	for {
		data, err := os.ReadFile(ready)
		if err == nil && strings.TrimSpace(string(data)) != "" {
			killAndWait()
			if stdout.Len() != 0 {
				t.Fatalf("interrupted acquisition already published a reply: %q", stdout.String())
			}
			slot := strings.TrimSpace(string(data))
			for {
				procs, err := process.FindProcessesInWorktree(slot)
				if err != nil {
					t.Fatal(err)
				}
				if len(procs) == 0 {
					return slot
				}
				if time.Now().After(deadline) {
					t.Fatalf("killed acquisition still has children in its worktree: %+v", procs)
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
		if time.Now().After(deadline) {
			killAndWait()
			t.Fatalf("acquisition did not reach barrier: stdout=%q stderr=%s", stdout.String(), stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
