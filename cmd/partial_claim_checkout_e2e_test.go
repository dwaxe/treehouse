//go:build unix

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The post-checkout barrier in TestGetClaimRepairsInterruptedAcquisition kills
// after all tracked files are written. These barriers run inside Git's smudge
// filter, while a real checkout has written some files and has not written the
// rest. A complete-checkout-only repair cannot pass these cases.
func TestGetClaimRepairsPartialCheckoutAtSeveralKillPoints(t *testing.T) {
	const files = 96
	for _, killAt := range []int{1, 48, 95} {
		t.Run(fmt.Sprintf("after-%03d-of-%d-files", killAt, files), func(t *testing.T) {
			repo, home := setupTestRepo(t)
			lab := t.TempDir()
			ready := filepath.Join(lab, "ready")
			allow := filepath.Join(lab, "allow-checkout")
			failure := filepath.Join(lab, "setup-fails")
			if err := os.WriteFile(failure, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(repo, "payload"), 0o755); err != nil {
				t.Fatal(err)
			}
			expected := make(map[string][]byte, files)
			for i := 0; i < files; i++ {
				name := fmt.Sprintf("payload/%03d.dat", i)
				// Six MiB of real tracked files, with distinct file contents.
				data := bytes.Repeat([]byte(fmt.Sprintf("%03d tracked checkout content\n", i)), 2400)
				expected[name] = data
				if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(name)), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for name, contents := range map[string]string{
				".gitattributes": "payload/*.dat filter=partial-claim\n",
				".gitignore":     "dead-cache/\nsetup-preserved\nsetup-completed\n",
			} {
				if err := os.WriteFile(filepath.Join(repo, name), []byte(contents), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			gitCmd(t, repo, "add", ".gitattributes", ".gitignore", "payload")
			gitCmd(t, repo, "commit", "-m", "Add a large checkout fixture")
			base := gitCmd(t, repo, "rev-parse", "HEAD")
			gitCmd(t, repo, "push", "origin", "main")
			gitCmd(t, repo, "config", "checkout.workers", "1")
			filter := filepath.Join(lab, "partial-checkout-filter")
			filterText := `#!/bin/sh
set -eu
if [ "$1" = "$TREEHOUSE_TEST_PARTIAL_AT" ] && [ ! -f "$TREEHOUSE_TEST_CLAIM_ALLOW" ]; then
    printf dead-run > checkout-junk
    mkdir -p dead-cache
    printf ignored-dead-run > dead-cache/partial
    (unset GIT_DIR GIT_WORK_TREE GIT_COMMON_DIR GIT_INDEX_FILE; git init --quiet --initial-branch=main dead-nested)
    printf nested-dead-run > dead-nested/partial
    pwd > "$TREEHOUSE_TEST_CLAIM_READY"
    while [ ! -f "$TREEHOUSE_TEST_CLAIM_ALLOW" ]; do sleep 0.02; done
fi
cat
`
			if err := os.WriteFile(filter, []byte(filterText), 0o755); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, repo, "config", "filter.partial-claim.clean", "cat")
			gitCmd(t, repo, "config", "filter.partial-claim.smudge", quotePOSIXReturnPath(filter)+" %f")
			gitCmd(t, repo, "config", "filter.partial-claim.required", "true")
			env := []string{
				"TREEHOUSE_ROOT=" + filepath.Join(lab, "root"),
				"TREEHOUSE_TEST_CLAIM_READY=" + ready,
				"TREEHOUSE_TEST_CLAIM_ALLOW=" + allow,
				"TREEHOUSE_TEST_PARTIAL_AT=" + fmt.Sprintf("payload/%03d.dat", killAt),
				"TREEHOUSE_TEST_PARTIAL_FAIL=" + failure,
			}
			hook := "if [ ! -f setup-preserved ]; then printf original > setup-preserved; fi; " +
				"if [ -f \"$TREEHOUSE_TEST_PARTIAL_FAIL\" ]; then exit 42; fi; printf complete > setup-completed"
			config := "max_trees = 1\n[hooks]\npost_create = [" + fmt.Sprintf("%q", hook) + "]\n"
			configDir := filepath.Join(home, ".config", "treehouse")
			if err := os.MkdirAll(configDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			args := []string{"get", "--lease", "--json", "--no-fetch", "--lease-holder", "partial-worker", "--claim-id", "interrupted-task"}
			slot := killClaimAtBarrier(t, repo, home, env, args, ready)
			futureID := interruptedClaimLeaseID(t, slot)
			assertPendingClaim(t, slot, "checkout", futureID)
			present := 0
			for name := range expected {
				if _, err := os.Stat(filepath.Join(slot, filepath.FromSlash(name))); err == nil {
					present++
				} else if !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			if present == 0 || present >= files {
				t.Fatalf("barrier must interrupt actual checkout: present=%d total=%d", present, files)
			}
			if diff := gitCmd(t, slot, "diff", "--name-only", "HEAD", "--", "payload"); strings.TrimSpace(diff) == "" {
				t.Fatal("checkout kill did not leave modified or missing tracked content")
			}
			t.Logf("SIGKILL during checkout: %d/%d tracked payload files present; tracked diff is nonempty; no caller lease", present, files)
			for _, name := range []string{"checkout-junk", "dead-cache/partial", "dead-nested/.git", "dead-nested/partial"} {
				if _, err := os.Stat(filepath.Join(slot, filepath.FromSlash(name))); err != nil {
					t.Fatalf("dead checkout did not leave %s: %v", name, err)
				}
			}
			if err := os.WriteFile(allow, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			// Repair must finish the partial checkout, but required setup still
			// prevents a lease. A later setup retry must preserve this output.
			out, stderr, code := runTreehouse(t, repo, home, env, args...)
			if code == 0 || out != "" || !strings.Contains(stderr, "exit 42") {
				t.Fatalf("partial-checkout repair did not reach required setup without a grant: code=%d stdout=%q stderr=%s", code, out, stderr)
			}
			assertPendingClaim(t, slot, "hooks", futureID)
			if got := gitCmd(t, slot, "rev-parse", "HEAD"); got != base {
				t.Fatalf("repair moved away from claim base: got %s, want %s", got, base)
			}
			for name, want := range expected {
				if got, err := os.ReadFile(filepath.Join(slot, filepath.FromSlash(name))); err != nil || !bytes.Equal(got, want) {
					t.Fatalf("repair did not restore %s: %v, got %d bytes want %d", name, err, len(got), len(want))
				}
			}
			if diff := gitCmd(t, slot, "diff", "--name-only", "HEAD", "--", "payload"); diff != "" {
				t.Fatalf("repair retained tracked partial checkout: %s", diff)
			}
			for _, name := range []string{"checkout-junk", "dead-cache", "dead-nested"} {
				if _, err := os.Lstat(filepath.Join(slot, name)); !os.IsNotExist(err) {
					t.Fatalf("repair kept dead-run output %s: %v", name, err)
				}
			}
			if err := os.Remove(failure); err != nil {
				t.Fatal(err)
			}
			out, stderr, code = runTreehouse(t, repo, home, env, args...)
			var lease leaseJSONResult
			if err := json.Unmarshal([]byte(out), &lease); err != nil || code != 0 || lease.Path != slot || lease.LeaseID != futureID {
				t.Fatalf("same-claim retry did not grant repaired area: code=%d stdout=%q stderr=%s json=%v", code, out, stderr, err)
			}
			if statuses := claimStatuses(t, repo, home, env); len(statuses) != 1 || statuses[0].Path != slot || statuses[0].LeaseID != futureID {
				t.Fatalf("partial-checkout retry consumed capacity: %+v", statuses)
			}
			if got, err := os.ReadFile(filepath.Join(slot, "setup-preserved")); err != nil || string(got) != "original" {
				t.Fatalf("setup replay discarded partial hook output: %q, %v", got, err)
			}
			if got, err := os.ReadFile(filepath.Join(slot, "setup-completed")); err != nil || string(got) != "complete" {
				t.Fatalf("successful setup did not finish: %q, %v", got, err)
			}
			t.Logf("same claim repaired %s at base %s; required setup first refused the grant, then granted lease %s; pool count=1", slot, base, futureID)
		})
	}
}
