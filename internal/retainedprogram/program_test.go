package retainedprogram

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRetainedProgramSurvivesReleaseOverwriteAndRemoval(t *testing.T) {
	directory := t.TempDir()
	source, retained := filepath.Join(directory, "release"), filepath.Join(directory, "program")
	if err := os.WriteFile(source, []byte("#!/bin/sh\nprintf original"), 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := Copy(source, retained)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("#!/bin/sh\nprintf replacement"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := Verify(retained, identity); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(retained).Output()
	if err != nil || string(out) != "original" {
		t.Fatal(string(out), err)
	}
	if _, err := Copy(retained, retained); err == nil {
		t.Fatal("existing pinned program replaced")
	}
	if err := os.WriteFile(retained, []byte("#!/bin/sh\nprintf changed!"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := Verify(retained, identity); err == nil {
		t.Fatal("mutated program verified")
	}
}

func TestRetainedProgramRejectsOversizeAndSymlinkWithoutReplacingDestination(t *testing.T) {
	directory := t.TempDir()
	source, destination := filepath.Join(directory, "source"), filepath.Join(directory, "destination")
	file, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := Copy(source, destination); err == nil {
		t.Fatal("oversize program retained")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("failed copy left destination", err)
	}
	if err := os.WriteFile(source, []byte("program"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, destination); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(source, destination); err == nil {
		t.Fatal("symlink destination replaced")
	}
	body, err := os.ReadFile(source)
	if err != nil || string(body) != "program" {
		t.Fatal("source changed", string(body), err)
	}
}

func TestExecutingProgramRejectsAnIdenticalSeparateCopy(t *testing.T) {
	copy := filepath.Join(t.TempDir(), "program")
	identity, err := Current(copy)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(copy, identity); err != nil {
		t.Fatal("fixture copy was not valid", err)
	}
	if err := VerifyExecuting(copy, identity); err == nil {
		t.Fatal("identical bytes on another inode were accepted as the running program")
	}
}
