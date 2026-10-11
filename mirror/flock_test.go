package mirror

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFlock(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "lock")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// the child tells that it has acquired the lock by writing a line,
	// and holds the lock until its stdin is closed, so that the test
	// does not depend on how long it takes to start.
	cmd := exec.Command("flock", p, "sh", "-c", "echo locked; cat >/dev/null") //nolint:gosec // G204: p is a temporary file of the test
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	err = cmd.Start()
	if err != nil {
		t.Skip()
		return
	}
	waited := false
	defer func() {
		_ = stdin.Close()
		if !waited {
			_ = cmd.Wait()
		}
	}()
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(p) //nolint:gosec // G304: p is a temporary file of the test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	fl := Flock{f}
	if err = fl.Lock(); err == nil {
		t.Error(`err = fl.Lock(); err == nil`)
	} else {
		t.Log(err)
	}

	_ = stdin.Close()
	waited = true
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err = fl.Lock(); err != nil {
		t.Fatal(err)
	}
	if err = fl.Unlock(); err != nil {
		t.Error(err)
	}
}
