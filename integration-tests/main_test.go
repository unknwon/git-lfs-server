//go:build !windows

package integration_tests

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sourcegraph/run"
	"github.com/stretchr/testify/require"
	"go.bobheadxi.dev/streamline/streamexec"
)

var long = flag.Bool("long", false, "run long-running integration tests")

func TestMain(m *testing.M) {
	flag.Parse()
	if !*long {
		fmt.Println("integration-tests: skipping (pass -long to run)")
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
	// Set lfs.url in local git config (not .lfsconfig) so the PAT never lands in a
	// committed file. GitHub push protection rejects pushes that contain a PAT.
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

	git := gitCmd(ctx, t, pushDir)
	git("init", "-q", "-b", branch)
	git("lfs", "install", "--local")
	git("config", "user.email", "ci@example.com")
	git("config", "user.name", "ci")
	git("config", "lfs.url", lfsURL)
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

	pullGit := gitCmd(ctx, t, pullDir)
	pullGit("config", "lfs.url", lfsURL)
	pullGit("lfs", "pull")

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

func setupLfsd(ctx context.Context, t *testing.T) func() {
	t.Helper()

	root := repoRoot(ctx, t)
	binPath := filepath.Join(root, ".bin", "lfsd")
	require.NoError(t, os.MkdirAll(filepath.Dir(binPath), 0o755))

	require.NoError(t,
		run.Cmd(ctx, "go", "build", "-o", binPath, "./cmd/lfsd").
			Dir(root).Run().Wait(),
		"go build lfsd")

	confPath := filepath.Join(root, "integration-tests", "testdata", "config.ini")
	cmd := exec.CommandContext(ctx, binPath)
	cmd.Env = append(os.Environ(), "LFSD_CONFIG_PATH="+confPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stream, err := streamexec.Start(cmd, streamexec.Combined)
	require.NoError(t, err, "start lfsd")

	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		err := stream.Stream(func(line string) {
			t.Logf("[lfsd] %s", line)
		})
		if err != nil && !strings.Contains(err.Error(), "signal: killed") {
			t.Logf("[lfsd] stream ended: %v", err)
		}
	}()

	waitHealthy(ctx, t, "http://127.0.0.1:3356/healthz", 30*time.Second)

	return func() {
		if cmd.Process != nil {
			killProcessGroup(cmd.Process.Pid)
		}
		// Wait for the streaming goroutine to drain before returning so it never
		// calls t.Logf after the test completes.
		<-streamDone
	}
}

func waitHealthy(ctx context.Context, t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build healthz request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("lfsd never became healthy: %v", lastErr)
}

func killProcessGroup(pid int) {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return
	}
	for i := 0; i < 10; i++ {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
			if strings.Contains(err.Error(), "no such process") {
				return
			}
		}
		time.Sleep(time.Second)
	}
}

func gitCmd(ctx context.Context, t *testing.T, dir string) func(args ...string) {
	t.Helper()
	return func(args ...string) {
		t.Helper()
		// run.Cmd joins parts with spaces and re-shell-splits, so any arg with
		// whitespace or shell metacharacters must be shell-quoted via run.Arg.
		parts := []string{"git"}
		for _, a := range args {
			parts = append(parts, run.Arg(a))
		}
		err := run.Cmd(ctx, parts...).
			Dir(dir).
			Environ([]string{"GIT_TERMINAL_PROMPT=0"}).
			Run().Wait()
		if err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
	}
}

func genRandomBlob(t *testing.T, size int) []byte {
	t.Helper()
	buf := make([]byte, size)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	return buf
}

func repoRoot(ctx context.Context, t *testing.T) string {
	t.Helper()
	out, err := run.Cmd(ctx, "git", "rev-parse", "--show-toplevel").Run().String()
	require.NoError(t, err)
	return strings.TrimSpace(out)
}
