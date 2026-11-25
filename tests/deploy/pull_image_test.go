package deploy

// deploy/pull-image.sh: the retries around `docker pull`, with a fake docker on PATH (no daemon, no network).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	pullImage = "ghcr.io/idp-xyz/mirror:public.ecr.aws__docker__library__postgres__17-alpine"
	pullLimit = "Error response from daemon: error from registry: Data limit exceeded"
)

// FAKE_PULLS_FAILING pulls are refused with the registry's message, later ones succeed; FAKE_PRESENT names the image the
// daemon already has. Every call is appended to $FAKE_DIR/calls.
const fakeDocker = `#!/bin/sh
echo "$*" >> "$FAKE_DIR/calls"
case "$1 $2" in
  "image inspect") [ "$3" = "$FAKE_PRESENT" ]; exit $? ;;
  "pull -q")
    n=$(grep -c '^pull ' "$FAKE_DIR/calls")
    if [ "$n" -le "$FAKE_PULLS_FAILING" ]; then echo "` + pullLimit + `" >&2; exit 1; fi
    echo sha256:0; exit 0 ;;
esac
exit 99
`

// runPull runs the script with the fake docker; it returns stderr, the exit code, the pulls the fake saw and the waits.
func runPull(t *testing.T, failing, present, waits string, images ...string) (string, int, []string, []string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "calls"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// a sleep that records the wait instead of sleeping, so that the default waits can be checked too
	sleep := "#!/bin/sh\necho \"$1\" >> \"" + dir + "/sleeps\"\n"
	if err := os.WriteFile(filepath.Join(dir, "sleep"), []byte(sleep), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{"../../deploy/pull-image.sh"}, images...)...)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "FAKE_DIR="+dir, "FAKE_PULLS_FAILING="+failing, "FAKE_PRESENT="+present)
	if waits != "" {
		cmd.Env = append(cmd.Env, "IDP_PULL_WAITS="+waits)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	var pulls []string
	for _, l := range strings.Split(string(calls), "\n") {
		if strings.HasPrefix(l, "pull ") {
			pulls = append(pulls, l)
		}
	}
	sleeps, _ := os.ReadFile(filepath.Join(dir, "sleeps"))
	return stderr.String(), code, pulls, strings.Fields(string(sleeps))
}

func TestPullRetriesAndSucceeds(t *testing.T) {
	stderr, code, pulls, _ := runPull(t, "2", "", "0 0 0 0", pullImage)
	if code != 0 || len(pulls) != 3 {
		t.Fatalf("exit %d after %d pulls, want 0 after 3: %s", code, len(pulls), stderr)
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if len(lines) != 2 {
		t.Fatalf("want one line per failed attempt, got %q", stderr)
	}
	for _, l := range lines {
		if !strings.Contains(l, pullImage) || !strings.Contains(l, "of 5 failed") || !strings.Contains(l, "Data limit exceeded") {
			t.Errorf("a failed attempt must name the image and the daemon's message: %q", l)
		}
	}
}

func TestPullGivesUpAfterFiveAttempts(t *testing.T) {
	stderr, code, pulls, _ := runPull(t, "99", "", "0 0 0 0", pullImage)
	if code == 0 || len(pulls) != 5 {
		t.Fatalf("exit %d after %d pulls, want non-zero after 5", code, len(pulls))
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	want := "BLOCKED: could not pull " + pullImage + " after 5 attempts: " + pullLimit
	if len(lines) != 5 || lines[4] != want {
		t.Fatalf("the last line must be %q, got %q", want, stderr)
	}
}

func TestPullWaitsAreTheDocumentedOnes(t *testing.T) {
	_, _, _, waits := runPull(t, "99", "", "", pullImage)
	if strings.Join(waits, " ") != "15 30 60 120" {
		t.Fatalf("waits %q, want 15 30 60 120", waits)
	}
}

func TestPullSkipsAPresentImage(t *testing.T) {
	stderr, code, pulls, _ := runPull(t, "99", pullImage, "0 0 0 0", pullImage)
	if code != 0 || len(pulls) != 0 || stderr != "" {
		t.Fatalf("a present image is not pulled: exit %d, pulls %v, stderr %q", code, pulls, stderr)
	}
	if _, code, pulls, _ = runPull(t, "99", "", "0 0 0 0"); code != 0 || len(pulls) != 0 {
		t.Fatalf("no image, nothing to do: exit %d, pulls %v", code, pulls)
	}
}
