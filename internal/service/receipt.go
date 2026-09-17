package service

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// StartupReceipt proves one local initialization. It does not claim Gateway
// connectivity or ongoing health.
type StartupReceipt struct {
	Nonce            string    `json:"nonce"`
	PID              int       `json:"pid"`
	ProcessStart     string    `json:"process_start"`
	ProgramDirectory string    `json:"program_directory"`
	InitializedAt    time.Time `json:"initialized_at"`
}

func processStart(pid int) (string, error) {
	command := exec.Command("/bin/ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	command.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	data, err := command.Output()
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", fmt.Errorf("process no longer running")
	}
	return value, nil
}
func WriteStartupReceipt(path, nonce string) error {
	if path == "" && nonce == "" {
		return nil
	}
	if !filepath.IsAbs(path) || len(nonce) != 32 {
		return fmt.Errorf("invalid startup receipt request")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	start, err := processStart(os.Getpid())
	if err != nil {
		return err
	}
	value := StartupReceipt{Nonce: nonce, PID: os.Getpid(), ProcessStart: start, ProgramDirectory: filepath.Dir(exe), InitializedAt: time.Now()}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".receipt-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
func VerifyStartupReceipt(path, nonce, directory string, after time.Time) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) > 4096 {
		return fmt.Errorf("startup receipt too large")
	}
	var value StartupReceipt
	if err = json.Unmarshal(data, &value); err != nil {
		return err
	}
	if value.Nonce != nonce || value.PID <= 0 || value.ProgramDirectory != directory || value.InitializedAt.Before(after) {
		return fmt.Errorf("startup receipt does not match this installation")
	}
	if err = syscall.Kill(value.PID, 0); err != nil {
		return err
	}
	start, err := processStart(value.PID)
	if err != nil || start != value.ProcessStart {
		return fmt.Errorf("initialized process is no longer running")
	}
	return nil
}
