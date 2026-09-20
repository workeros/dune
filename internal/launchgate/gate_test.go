package launchgate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLaunchGateExcludesSwitchAndRejectsReplacedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	first, err := Acquire(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Acquire(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := Acquire(dir, true); !errors.Is(err, ErrBusy) {
		t.Fatal("switch crossed in-flight launch", err)
	}
	first.Close()
	second.Close()
	switcher, err := Acquire(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir, false); !errors.Is(err, ErrBusy) {
		t.Fatal("launch crossed switch", err)
	}
	switcher.Close()
	if err := os.Rename(filepath.Join(dir, "launch.lock"), filepath.Join(dir, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("original", filepath.Join(dir, "launch.lock")); err != nil {
		t.Fatal(err)
	}
	if file, err := Acquire(dir, false); err == nil {
		file.Close()
		t.Fatal("symlink gate accepted")
	}
}
