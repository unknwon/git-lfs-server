package integration_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sourcegraph/run"
	"github.com/stretchr/testify/require"
	"go.bobheadxi.dev/streamline/streamexec"
)

func setupLfsd(ctx context.Context, t *testing.T) func() {
	t.Helper()

	root := repoRoot(ctx, t)
	binPath := filepath.Join(root, ".bin", "lfsd")
	require.NoError(t, os.MkdirAll(filepath.Dir(binPath), 0o755))

	require.NoError(t,
		run.Cmd(ctx, "go", "build", "-o", binPath, "./cmd/lfsd").
			Dir(root).Run().Wait(),
		"go build lfsd")

	confPath := filepath.Join(root, "integration", "testdata", "config.ini")
	cmd := exec.CommandContext(ctx, binPath)
	cmd.Env = append(os.Environ(), "LFSD_CONFIG_PATH="+confPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stream, err := streamexec.Start(cmd, streamexec.Combined)
	require.NoError(t, err, "start lfsd")

	go func() {
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
		parts := append([]string{"git"}, args...)
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
