package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/sourcegraph/run"
	"github.com/stretchr/testify/require"
)

var long = flag.Bool("long", false, "run long-running integration tests")

func TestMain(m *testing.M) {
	flag.Parse()
	if !*long {
		fmt.Println("integration: skipping (pass -long to run)")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestUploadDownloadRoundtrip(t *testing.T) {
	requireEnv(t,
		"LFSD_DATABASE_PASSWORD",
		"LFSD_R2_ACCOUNT_ID", "LFSD_R2_BUCKET",
		"LFSD_R2_ACCESS_KEY_ID", "LFSD_R2_SECRET_ACCESS_KEY",
		"GIT_LFS_TEST_PAT",
	)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	shutdownLfsd := setupLfsd(ctx, t)
	t.Cleanup(shutdownLfsd)

	pat := os.Getenv("GIT_LFS_TEST_PAT")
	remoteURL := fmt.Sprintf(
		"https://x-access-token:%s@github.com/unknwon/git-lfs-test.git", pat)
	lfsURL := fmt.Sprintf(
		"http://x-access-token:%s@127.0.0.1:3356/github.com/unknwon/git-lfs-test/info/lfs",
		pat)

	branch := "e2e-" + strconv.FormatInt(time.Now().Unix(), 10)
	t.Cleanup(func() {
		if err := run.Cmd(context.Background(), "git", "push", remoteURL, "--delete", branch).
			Environ([]string{"GIT_TERMINAL_PROMPT=0"}).Run().Wait(); err != nil {
			t.Logf("cleanup: failed to delete remote branch %s: %v", branch, err)
		}
	})

	pushDir := t.TempDir()
	blob := genRandomBlob(t, 1*1024*1024)
	wantSum := sha256.Sum256(blob)

	require.NoError(t, os.WriteFile(filepath.Join(pushDir, "large.bin"), blob, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pushDir, ".gitattributes"),
		[]byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pushDir, ".lfsconfig"),
		[]byte(fmt.Sprintf("[lfs]\n\turl = %s\n", lfsURL)), 0o644))

	git := gitCmd(ctx, t, pushDir)
	git("init", "-q", "-b", branch)
	git("lfs", "install", "--local")
	git("config", "user.email", "ci@example.com")
	git("config", "user.name", "ci")
	git("add", ".")
	git("commit", "-q", "-m", "e2e fixture")
	git("remote", "add", "origin", remoteURL)
	git("push", "origin", "HEAD:refs/heads/"+branch)

	// Skip smudge during clone so the LFS object isn't fetched as a side effect of
	// checkout. Then explicitly pull to exercise the download path against lfsd.
	pullDir := t.TempDir()
	require.NoError(t,
		run.Cmd(ctx, "git", "clone", "--branch", branch, "--depth", "1", remoteURL, pullDir).
			Environ([]string{"GIT_LFS_SKIP_SMUDGE=1", "GIT_TERMINAL_PROMPT=0"}).Run().Wait())

	gitCmd(ctx, t, pullDir)("lfs", "pull")

	got, err := os.ReadFile(filepath.Join(pullDir, "large.bin"))
	require.NoError(t, err)
	gotSum := sha256.Sum256(got)
	require.Equal(t, hex.EncodeToString(wantSum[:]), hex.EncodeToString(gotSum[:]))
}

func requireEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if os.Getenv(n) == "" {
			t.Fatalf("required env var %s is not set", n)
		}
	}
}
