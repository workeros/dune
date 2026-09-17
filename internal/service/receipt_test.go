package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartupReceiptMatchesLiveProcessAndNonce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready.json")
	nonce := strings.Repeat("a", 32)
	before := time.Now()
	t.Setenv("TZ", "Asia/Shanghai")
	if err := WriteStartupReceipt(path, nonce); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	t.Setenv("TZ", "UTC")
	if err := VerifyStartupReceipt(path, nonce, filepath.Dir(exe), before); err != nil {
		t.Fatal(err)
	}
	if err := VerifyStartupReceipt(path, strings.Repeat("b", 32), filepath.Dir(exe), before); err == nil {
		t.Fatal("accepted stale nonce")
	}
	if err := VerifyStartupReceipt(path, nonce, t.TempDir(), before); err == nil {
		t.Fatal("accepted wrong executable")
	}
	if err := VerifyStartupReceipt(path, nonce, filepath.Dir(exe), time.Now()); err == nil {
		t.Fatal("accepted old receipt")
	}
}
