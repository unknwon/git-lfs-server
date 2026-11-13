//go:build !windows

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/sourcegraph/run"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.bobheadxi.dev/streamline/streamexec"

	"unknwon.dev/git-lfs-server/lfs"
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
	content := genRandomContent(t, 1*1024*1024)
	wantSum := sha256.Sum256(content)

	require.NoError(t, os.WriteFile(filepath.Join(pushDir, "large.bin"), content, 0o644))
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

	oid := hex.EncodeToString(wantSum[:])
	// Check out in a fresh clone, so the smudge filter downloads the LFS object
	// from lfsd.
	requireClone := func(lfsURL string) {
		t.Helper()
		cloneDir := t.TempDir()
		require.NoError(t,
			run.Cmd(ctx, "git", "clone", "--no-checkout", "--branch", branch, "--depth", "1", remoteURL, cloneDir).
				Environ([]string{"GIT_TERMINAL_PROMPT=0"}).Run().Wait())
		cloneGit := gitCmd(ctx, t, cloneDir)
		cloneGit("config", "lfs.url", lfsURL)
		cloneGit("checkout", branch)
		got, err := os.ReadFile(filepath.Join(cloneDir, "large.bin"))
		require.NoError(t, err)
		require.True(t, bytes.Equal(content, got), "downloaded content differs from the pushed content")
	}
	requireClone(lfsURL)

	// The same object in another repository is only linked to it by uploading
	// the content again.
	otherLFSURL := "http://x:x@127.0.0.1:3356/local.test/unknwon/git-lfs-test/info/lfs"
	resp := batch(ctx, t, otherLFSURL, lfs.BatchRequest{
		Operation: lfs.OperationDownload,
		Objects:   []lfs.BatchObject{{OID: oid, Size: int64(len(content))}},
	})
	require.NotNil(t, resp.Objects[0].Error, "another repository must not download the object before uploading it")
	resp = batch(ctx, t, otherLFSURL, lfs.BatchRequest{
		Operation: lfs.OperationUpload,
		Objects:   []lfs.BatchObject{{OID: oid, Size: int64(len(content))}},
	})
	require.Contains(t, resp.Objects[0].Actions, "upload", "another repository must upload the object")

	git("config", "lfs.url", otherLFSURL)
	git("lfs", "push", "--object-id", "origin", oid)
	requireClone(otherLFSURL)

	// Both repositories share one copy, and no upload is left behind.
	client := s3.NewFromConfig(aws.Config{
		Region: "auto",
		Credentials: credentials.NewStaticCredentialsProvider(
			os.Getenv("LFSD_R2_ACCESS_KEY_ID"), os.Getenv("LFSD_R2_SECRET_ACCESS_KEY"), ""),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(fmt.Sprintf("https://%s.r2.cloudflarestorage.com", os.Getenv("LFSD_R2_ACCOUNT_ID")))
		o.UsePathStyle = true
	})
	bucket := aws.String(os.Getenv("LFSD_R2_BUCKET"))
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String(oid)})
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), aws.ToInt64(head.ContentLength))
	pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: bucket, Prefix: aws.String("uploads/")})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		require.NoError(t, err)
		for _, o := range page.Contents {
			assert.False(t, strings.HasSuffix(aws.ToString(o.Key), "/"+oid), "upload %q is left behind", aws.ToString(o.Key))
		}
	}
}

// batch sends a batch request to the LFS endpoint at lfsURL, whose user info
// is sent as HTTP Basic Auth credentials.
func batch(ctx context.Context, t *testing.T, lfsURL string, req lfs.BatchRequest) *lfs.BatchResponse {
	t.Helper()
	u, err := url.Parse(lfsURL + "/objects/batch")
	require.NoError(t, err)
	password, _ := u.User.Password()
	username := u.User.Username()
	u.User = nil

	body, err := json.Marshal(req)
	require.NoError(t, err)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	require.NoError(t, err)
	httpReq.SetBasicAuth(username, password)
	httpReq.Header.Set("Content-Type", lfs.MediaType)
	httpReq.Header.Set("Accept", lfs.MediaType)
	httpResp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	defer func() { _ = httpResp.Body.Close() }()
	require.Equal(t, http.StatusOK, httpResp.StatusCode)

	var resp lfs.BatchResponse
	require.NoError(t, json.NewDecoder(httpResp.Body).Decode(&resp))
	require.Len(t, resp.Objects, len(req.Objects))
	return &resp
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

func genRandomContent(t *testing.T, size int) []byte {
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
