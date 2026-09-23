package runningprogram

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestImageChild(t *testing.T) {
	if os.Getenv("DUNE_TEST_RUNNING_IMAGE") == "" {
		t.Skip("child process")
	}
	first, err := Inspect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(first); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	second, err := Inspect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first.StartID != second.StartID || first.SHA256 != second.SHA256 || first.PID != second.PID {
		t.Fatal("replacement changed executing identity")
	}
	if err := json.NewEncoder(os.Stdout).Encode(second); err != nil {
		t.Fatal(err)
	}
}

func TestInspectPinsActualExecutingImageAcrossPathReplacement(t *testing.T) {
	if err := Capture(); err != nil {
		t.Fatal(err)
	}
	file, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "dune")
	output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	_, err = io.Copy(io.MultiWriter(output, digest), io.NewSectionReader(file, 0, info.Size()))
	closeErr := output.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	want := hex.EncodeToString(digest.Sum(nil))
	cmd := exec.Command(path, "-test.run=^TestImageChild$")
	cmd.Env = append(os.Environ(), "DUNE_TEST_RUNNING_IMAGE=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	decoder := json.NewDecoder(stdout)
	var first, second api.RunningProgram
	if err := decoder.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != want || first.PID != cmd.Process.Pid || first.StartID == "" {
		t.Fatal(first)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement is not the executing image"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".old"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, "inspect\n"); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&second); err != nil {
		t.Fatal(err)
	}
	if second.SHA256 != want || second.StartID != first.StartID {
		t.Fatal(second)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
