package retainedprogram

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutingProgramHelper(t *testing.T) {
	body := os.Getenv("DUNE_EXECUTING_IDENTITY")
	if body == "" {
		t.Skip("retained executable subprocess")
	}
	var identity Identity
	if err := json.Unmarshal([]byte(body), &identity); err != nil {
		t.Fatal(err)
	}
	err := VerifyExecuting(os.Getenv("DUNE_EXECUTING_PATH"), identity)
	if (err == nil) != (os.Getenv("DUNE_EXECUTING_ACCEPT") == "1") {
		t.Fatal("unexpected executing-file verification", err)
	}
}

func TestExecutingFileIdentityAndMetadata(t *testing.T) {
	for _, name := range []string{"ancestor_alias", "different_inode", "final_symlink", "digest", "permissions", "hardlink"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			program := filepath.Join(dir, "program")
			identity, err := Current(program)
			if err != nil {
				t.Fatal(err)
			}
			expected := program
			accept := "0"
			switch name {
			case "ancestor_alias":
				alias := filepath.Join(t.TempDir(), "home")
				if err := os.Symlink(dir, alias); err != nil {
					t.Fatal(err)
				}
				expected = filepath.Join(alias, "program")
				accept = "1"
			case "different_inode":
				expected = filepath.Join(dir, "copy")
				if _, err := Current(expected); err != nil {
					t.Fatal(err)
				}
			case "final_symlink":
				expected = filepath.Join(dir, "link")
				if err := os.Symlink(program, expected); err != nil {
					t.Fatal(err)
				}
			case "digest":
				identity.SHA256 = strings.Repeat("0", 64)
			case "permissions":
				if err := os.Chmod(program, 0755); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(program, filepath.Join(dir, "hardlink")); err != nil {
					t.Fatal(err)
				}
			}
			body, _ := json.Marshal(identity)
			cmd := exec.Command(program, "-test.run=^TestExecutingProgramHelper$")
			cmd.Env = append(os.Environ(), "DUNE_EXECUTING_IDENTITY="+string(body), "DUNE_EXECUTING_PATH="+expected, "DUNE_EXECUTING_ACCEPT="+accept)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v", out, err)
			}
		})
	}
}
