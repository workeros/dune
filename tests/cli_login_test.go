package tests

import (
	"bufio"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/pkg/login"
)

func TestHumanCLILoginAndExecution(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { testPrefixedWorkbench(t, workbenchCase{humanCLI: true}) })
	t.Run("postgres-separate-gateway", func(t *testing.T) {
		database := postgresWorkbenchConfig(t)
		testPrefixedWorkbench(t, workbenchCase{database: &database, separateGateway: true, humanCLI: true})
	})
}

func exerciseHumanCLI(t *testing.T, ctx context.Context, site, dir, target string, confirm func(string, string)) func() {
	t.Helper()
	file := filepath.Join(dir, "human", "login.json")
	command := exec.CommandContext(ctx, binary, "login", "--site", site, "--file", file, "--no-browser")
	stderr, err := command.StderrPipe()
	must(t, err)
	must(t, command.Start())
	waited := false
	defer func() {
		if !waited {
			command.Process.Kill()
			command.Wait()
		}
	}()
	scanner := bufio.NewScanner(stderr)
	if !scanner.Scan() {
		t.Fatal("CLI did not produce confirmation URL")
	}
	value, err := url.Parse(strings.TrimPrefix(scanner.Text(), "Open "))
	must(t, err)
	if value.Query().Get("cli_login") == "" || !strings.HasPrefix(value.String(), site+"?cli_login=") {
		t.Fatal("CLI confirmation URL changed site")
	}
	if !scanner.Scan() {
		t.Fatal("CLI did not produce verification code")
	}
	code := strings.TrimPrefix(scanner.Text(), "Verify code: ")
	confirm(value.Query().Get("cli_login"), code)
	for scanner.Scan() {
	} // Drain status text so subprocess completion cannot block.
	must(t, scanner.Err())
	err = command.Wait()
	waited = true
	must(t, err)
	info, err := os.Stat(file)
	must(t, err)
	if info.Mode().Perm() != 0600 {
		t.Fatal("CLI credential file is not private")
	}
	credentials, err := config.LoadUserCredentials(file)
	must(t, err)
	if credentials.Session.Site != site {
		t.Fatal("CLI saved another site")
	}
	invoke := func(args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, args...)
		out, err := cmd.CombinedOutput()
		if strings.Contains(string(out), credentials.Session.Token) {
			t.Fatal("CLI printed its credential")
		}
		if err != nil {
			t.Fatalf("CLI failed: %v %s", err, out)
		}
		return out
	}
	out := invoke("--login", file, "machines")
	var machines []login.Machine
	must(t, json.Unmarshal(out, &machines))
	if len(machines) != 1 || machines[0].ID != target {
		t.Fatal("CLI discovery lost owner or target")
	}
	out = invoke("--login", file, "--target", target, "exec", "--cwd", dir, "--", "/bin/sh", "-c", "printf CLI_LOGIN_EXEC_OK")
	if !strings.Contains(string(out), "CLI_LOGIN_EXEC_OK") {
		t.Fatal("CLI execution did not reach fabricd")
	}
	human, err := login.New(login.Options{Site: site})
	must(t, err)
	t.Cleanup(human.Close)
	client, err := human.Dial(ctx, credentials.Session, target)
	must(t, err)
	t.Cleanup(func() { client.Close() })
	if _, err := client.List(ctx); err != nil {
		t.Fatal("human SDK cannot use CLI session", err)
	}
	return func() {
		t.Helper()
		if _, err := human.Machines(ctx, credentials.Session); err == nil {
			t.Fatal("CLI session survived browser logout")
		}
		if _, err := client.List(ctx); err == nil {
			t.Fatal("established CLI connection survived browser logout")
		}
		invoke("logout", "--file", file)
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatal("CLI logout kept local credential", err)
		}
	}
}
