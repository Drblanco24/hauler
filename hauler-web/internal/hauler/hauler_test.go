package hauler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeHauler writes a shell script that stands in for the hauler binary. It
// appends its argv to argvFile (one arg per line, invocations separated by a
// blank line), prints script to stdout, and exits with code.
//
// Testing argv construction against a fake is the point: these tests are the
// contract with hauler's CLI surface, and they must fail loudly if a flag name
// is changed by accident. Whether the real binary honours those flags is what
// the end-to-end suite covers.
func fakeHauler(t *testing.T, argvFile, stdout, stderr string, code int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary is a POSIX shell script")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "hauler")

	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do printf '%%s\n' "$a" >> %q; done
printf '\n' >> %q
cat <<'HAULER_EOF_STDOUT'
%s
HAULER_EOF_STDOUT
cat >&2 <<'HAULER_EOF_STDERR'
%s
HAULER_EOF_STDERR
exit %d
`, argvFile, argvFile, stdout, stderr, code)

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake hauler: %v", err)
	}
	return path
}

// readArgv returns the argv of the first recorded invocation.
func readArgv(t *testing.T, argvFile string) []string {
	t.Helper()
	data, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("reading argv file: %v", err)
	}
	var out []string
	for _, ln := range strings.Split(string(data), "\n") {
		if ln == "" {
			break
		}
		out = append(out, ln)
	}
	return out
}

func argvString(t *testing.T, argvFile string) string {
	t.Helper()
	return strings.Join(readArgv(t, argvFile), " ")
}

func newTestClient(t *testing.T, bin string) *Client {
	t.Helper()
	return &Client{
		Bin:      bin,
		StoreDir: "/var/lib/hauler-web/store",
		LogLevel: "info",
	}
}

func TestSyncArgv(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	bin := fakeHauler(t, argv, "", "", 0)

	c := newTestClient(t, bin)
	c.TempDir = "/scratch"
	c.HaulerDir = "/var/lib/hauler-web/hauler"
	c.Retries = 4

	_, err := c.Sync(context.Background(), SyncOptions{
		Filenames:     []string{"/work/scoped.yaml"},
		Concurrency:   8,
		Platform:      "linux/amd64",
		ExcludeExtras: true,
		IgnoreErrors:  true,
	}, nil)
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	got := argvString(t, argv)
	for _, want := range []string{
		"store sync",
		"--filename /work/scoped.yaml",
		"--concurrency 8",
		"--platform linux/amd64",
		"--exclude-extras",
		"--ignore-errors",
		"--no-progress",
		"--store /var/lib/hauler-web/store",
		"--tempdir /scratch",
		"--retries 4",
		"--log-level info",
		"--haulerdir /var/lib/hauler-web/hauler",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q\ngot: %s", want, got)
		}
	}
}

// The progress renderer redraws rows in place with braille spinners. It must
// never be enabled for a run whose output is persisted as a log.
func TestSyncAlwaysDisablesProgress(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	bin := fakeHauler(t, argv, "", "", 0)

	c := newTestClient(t, bin)
	if _, err := c.Sync(context.Background(), SyncOptions{Filenames: []string{"m.yaml"}}, nil); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if !strings.Contains(argvString(t, argv), "--no-progress") {
		t.Error("sync argv does not contain --no-progress")
	}
}

func TestSyncRequiresInput(t *testing.T) {
	c := newTestClient(t, "/nonexistent/hauler")
	if _, err := c.Sync(context.Background(), SyncOptions{}, nil); err == nil {
		t.Fatal("Sync() with no manifests: expected an error, got nil")
	}
}

func TestCopySaveRemoveArgv(t *testing.T) {
	tests := []struct {
		name string
		call func(*Client) error
		want []string
	}{
		{
			name: "copy to registry",
			call: func(c *Client) error {
				_, err := c.Copy(context.Background(), CopyOptions{
					Target:    "registry://harbor.example/airgap",
					Insecure:  true,
					PlainHTTP: true,
				}, nil)
				return err
			},
			want: []string{"store copy registry://harbor.example/airgap", "--insecure", "--plain-http"},
		},
		{
			name: "save chunked",
			call: func(c *Client) error {
				_, err := c.Save(context.Background(), SaveOptions{
					Filename:  "haul-abc1234.tar.zst",
					ChunkSize: "2G",
				}, nil)
				return err
			},
			want: []string{"store save --filename haul-abc1234.tar.zst", "--chunk-size 2G"},
		},
		{
			name: "remove",
			call: func(c *Client) error {
				_, err := c.Remove(context.Background(), "alpine:3.20", nil)
				return err
			},
			want: []string{"store remove alpine:3.20"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argv := filepath.Join(t.TempDir(), "argv")
			bin := fakeHauler(t, argv, "", "", 0)
			if err := tt.call(newTestClient(t, bin)); err != nil {
				t.Fatalf("call error = %v", err)
			}
			got := argvString(t, argv)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("argv missing %q\ngot: %s", want, got)
				}
			}
		})
	}
}

func TestCopyAndSaveRejectEmptyInput(t *testing.T) {
	c := newTestClient(t, "/nonexistent/hauler")
	if _, err := c.Copy(context.Background(), CopyOptions{}, nil); err == nil {
		t.Error("Copy() with no target: expected an error")
	}
	if _, err := c.Save(context.Background(), SaveOptions{}, nil); err == nil {
		t.Error("Save() with no filename: expected an error")
	}
	if _, err := c.Remove(context.Background(), "", nil); err == nil {
		t.Error("Remove() with no reference: expected an error")
	}
}

// Info parses stdout, and hauler's logger writes to stdout too, so the command
// must run silent regardless of the client's configured level.
func TestInfoRunsSilentAndRequestsDigests(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	bin := fakeHauler(t, argv, `{
  "store-path": "/store",
  "store-id": "ec520cf6-e01b-4d6f-93ea-6588de0d5159",
  "artifacts": []
}`, "", 0)

	c := newTestClient(t, bin)
	c.LogLevel = "debug" // must be overridden for this command

	if _, err := c.Info(context.Background(), InfoOptions{}); err != nil {
		t.Fatalf("Info() error = %v", err)
	}

	got := argvString(t, argv)
	if !strings.Contains(got, "--log-level "+LogLevelSilent) {
		t.Errorf("Info() should force --log-level %s\ngot: %s", LogLevelSilent, got)
	}
	if strings.Contains(got, "--log-level debug") {
		t.Errorf("Info() leaked the client log level into argv\ngot: %s", got)
	}
	for _, want := range []string{"store info", "--output json", "--digests"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q\ngot: %s", want, got)
		}
	}
}

const sampleInfoJSON = `{
  "store-path": "/var/lib/hauler-web/store",
  "store-id": "ec520cf6-e01b-4d6f-93ea-6588de0d5159",
  "artifacts": [
    {
      "reference": "ghcr.io/hauler-dev/library/busybox:stable",
      "type": "image",
      "platform": "linux/amd64",
      "digest": "sha256:aaaa",
      "layers": 1,
      "size": 4194304
    },
    {
      "reference": "ghcr.io/hauler-dev/library/busybox:sha256-aaaa.sig",
      "type": "sigs",
      "platform": "",
      "digest": "sha256:bbbb",
      "layers": 1,
      "size": 2048
    },
    {
      "reference": "hauler/rancher:2.8.5",
      "type": "chart",
      "platform": "",
      "digest": "sha256:cccc",
      "layers": 1,
      "size": 102400,
      "problems": ["missing blob sha256:dddd"]
    }
  ]
}`

func TestInfoParsesArtifacts(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	bin := fakeHauler(t, argv, sampleInfoJSON, "", 0)

	info, err := newTestClient(t, bin).Info(context.Background(), InfoOptions{})
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}

	if got, want := info.StoreID, "ec520cf6-e01b-4d6f-93ea-6588de0d5159"; got != want {
		t.Errorf("StoreID = %q, want %q", got, want)
	}
	if got, want := len(info.Artifacts), 3; got != want {
		t.Fatalf("len(Artifacts) = %d, want %d", got, want)
	}

	// Cosign artifacts are real store contents but nobody declared them, so
	// they must not surface as images.
	imgs := info.Images()
	if got, want := len(imgs), 1; got != want {
		t.Fatalf("len(Images()) = %d, want %d", got, want)
	}
	if got, want := imgs[0].Digest, "sha256:aaaa"; got != want {
		t.Errorf("Images()[0].Digest = %q, want %q", got, want)
	}
	if got, want := imgs[0].Size, int64(4194304); got != want {
		t.Errorf("Images()[0].Size = %d, want %d", got, want)
	}

	if !info.HasDigest("sha256:aaaa") {
		t.Error("HasDigest(sha256:aaaa) = false, want true")
	}
	if info.HasDigest("sha256:zzzz") {
		t.Error("HasDigest(sha256:zzzz) = true, want false")
	}
	// An empty digest must never count as present, or every unresolved image
	// would be treated as already pulled.
	if info.HasDigest("") {
		t.Error(`HasDigest("") = true, want false`)
	}

	if got, want := len(info.Problems()), 1; got != want {
		t.Errorf("len(Problems()) = %d, want %d", got, want)
	}
}

// hauler writes logs to stdout, so even with --log-level disabled a future
// release could emit a banner or a deprecation warning alongside the JSON.
func TestInfoToleratesInterleavedLogLines(t *testing.T) {
	noisy := "2026-08-08T12:00:00Z INF using store at [/store]\n" +
		sampleInfoJSON + "\n" +
		"2026-08-08T12:00:01Z WRN !!! WARNING !!! something deprecated\n"

	argv := filepath.Join(t.TempDir(), "argv")
	bin := fakeHauler(t, argv, noisy, "", 0)

	info, err := newTestClient(t, bin).Info(context.Background(), InfoOptions{})
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if got, want := len(info.Artifacts), 3; got != want {
		t.Errorf("len(Artifacts) = %d, want %d", got, want)
	}
}

func TestInfoCheckFlag(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	bin := fakeHauler(t, argv, sampleInfoJSON, "", 0)

	if _, err := newTestClient(t, bin).Info(context.Background(), InfoOptions{Check: true}); err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if !strings.Contains(argvString(t, argv), "--check") {
		t.Error("argv missing --check")
	}
}

func TestInfoRejectsUnparseableOutput(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	bin := fakeHauler(t, argv, "not json at all", "", 0)

	if _, err := newTestClient(t, bin).Info(context.Background(), InfoOptions{}); err == nil {
		t.Fatal("Info() with garbage output: expected an error, got nil")
	}
}

// A fresh PVC has no store index. That is the normal first run, not a failure.
func TestInfoIfExistsTreatsMissingIndexAsEmpty(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	bin := fakeHauler(t, argv, "", "Error: store index not found: run 'hauler store add/sync/load' first", 1)

	info, err := newTestClient(t, bin).InfoIfExists(context.Background(), InfoOptions{})
	if err != nil {
		t.Fatalf("InfoIfExists() error = %v", err)
	}
	if len(info.Artifacts) != 0 {
		t.Errorf("len(Artifacts) = %d, want 0", len(info.Artifacts))
	}
}

// ...but a genuine failure must still be reported, not swallowed as "empty".
func TestInfoIfExistsPropagatesRealErrors(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	bin := fakeHauler(t, argv, "", "Error: permission denied opening store", 1)

	if _, err := newTestClient(t, bin).InfoIfExists(context.Background(), InfoOptions{}); err == nil {
		t.Fatal("InfoIfExists() with a real failure: expected an error, got nil")
	}
}

func TestErrorCarriesExitCodeAndOutput(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	bin := fakeHauler(t, argv, "", "Error: failed to pull alpine:3.20: unauthorized", 7)

	_, err := newTestClient(t, bin).Sync(context.Background(), SyncOptions{Filenames: []string{"m.yaml"}}, nil)
	if err == nil {
		t.Fatal("Sync() against a failing binary: expected an error, got nil")
	}

	var herr *Error
	if !errors.As(err, &herr) {
		t.Fatalf("error is %T, want *hauler.Error", err)
	}
	if got, want := herr.ExitCode(), 7; got != want {
		t.Errorf("ExitCode() = %d, want %d", got, want)
	}
	if !strings.Contains(herr.Error(), "unauthorized") {
		t.Errorf("Error() = %q, want it to quote hauler's message", herr.Error())
	}
	if !strings.Contains(herr.Error(), "store sync") {
		t.Errorf("Error() = %q, want it to name the command", herr.Error())
	}
}

// hauler reports most failures through its stdout logger, so an error message
// must not be lost just because stderr was empty.
func TestErrorFallsBackToStdout(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	bin := fakeHauler(t, argv, "2026-08-08T12:00:00Z ERR failed to resolve reference", "", 1)

	_, err := newTestClient(t, bin).Sync(context.Background(), SyncOptions{Filenames: []string{"m.yaml"}}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "failed to resolve reference") {
		t.Errorf("Error() = %q, want it to quote the stdout message", err.Error())
	}
}

func TestRunStreamsToSink(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	bin := fakeHauler(t, argv, "pulling alpine:3.20\npulled alpine:3.20", "", 0)

	var sink strings.Builder
	res, err := newTestClient(t, bin).Sync(context.Background(),
		SyncOptions{Filenames: []string{"m.yaml"}}, &sink)
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if !strings.Contains(sink.String(), "pulled alpine:3.20") {
		t.Errorf("sink = %q, want the streamed output", sink.String())
	}
	// The buffered copy must still be populated for error reporting.
	if !strings.Contains(res.Stdout, "pulled alpine:3.20") {
		t.Errorf("Result.Stdout = %q", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.Duration <= 0 {
		t.Error("Duration was not recorded")
	}
}

// A six-hour sync that hangs must be distinguishable from a manifest error.
func TestTimeoutIsReportedAsSuch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hauler")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("writing fake hauler: %v", err)
	}

	c := newTestClient(t, path)
	c.Timeout = 200 * time.Millisecond

	start := time.Now()
	_, err := c.Sync(context.Background(), SyncOptions{Filenames: []string{"m.yaml"}}, nil)
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took %s to fire", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("Error() = %q, want it to say the run timed out", err.Error())
	}
}

func TestContextCancellationIsReportedAsSuch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hauler")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("writing fake hauler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_, err := newTestClient(t, path).Sync(ctx, SyncOptions{Filenames: []string{"m.yaml"}}, nil)
	if err == nil {
		t.Fatal("expected a cancellation error, got nil")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("Error() = %q, want it to say the run was cancelled", err.Error())
	}
}

func TestVersion(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	bin := fakeHauler(t, argv, `{
  "gitVersion": "v2.0.1",
  "gitCommit": "c8304d5",
  "gitTreeState": "clean"
}`, "", 0)

	got, err := newTestClient(t, bin).Version(context.Background())
	if err != nil {
		t.Fatalf("Version() error = %v", err)
	}
	if want := "v2.0.1"; got != want {
		t.Errorf("Version() = %q, want %q", got, want)
	}
	if !strings.Contains(argvString(t, argv), "version --json") {
		t.Errorf("argv = %s, want `version --json`", argvString(t, argv))
	}
}

func TestDockerConfigIsExported(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "env")
	path := filepath.Join(dir, "hauler")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"$DOCKER_CONFIG\" > %q\n", out)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake hauler: %v", err)
	}

	c := newTestClient(t, path)
	c.DockerConfig = "/run/hauler-web/dockercfg"

	if _, err := c.Sync(context.Background(), SyncOptions{Filenames: []string{"m.yaml"}}, nil); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading captured env: %v", err)
	}
	if got, want := string(data), "/run/hauler-web/dockercfg"; got != want {
		t.Errorf("DOCKER_CONFIG = %q, want %q", got, want)
	}
}

func TestExtractJSONObject(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			name: "bare object",
			in:   "{\n  \"a\": 1\n}\n",
			want: "{\n  \"a\": 1\n}",
		},
		{
			name: "leading and trailing noise",
			in:   "INF starting\n{\n  \"a\": 1\n}\nINF done\n",
			want: "{\n  \"a\": 1\n}",
		},
		{
			name: "nested objects are indented and must not terminate early",
			in:   "{\n  \"a\": {\n    \"b\": 1\n  },\n  \"c\": 2\n}\n",
			want: "{\n  \"a\": {\n    \"b\": 1\n  },\n  \"c\": 2\n}",
		},
		{name: "no object", in: "nothing here\n", wantErr: true},
		{name: "unterminated", in: "{\n  \"a\": 1\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractJSONObject(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("extractJSONObject() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("extractJSONObject() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("extractJSONObject() = %q, want %q", got, tt.want)
			}
		})
	}
}
