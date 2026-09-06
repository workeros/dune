package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/daemon"
	"github.com/aiomni/dune/internal/gateway"
	"github.com/aiomni/dune/internal/service"
	"github.com/aiomni/dune/internal/supervisor"
	"github.com/aiomni/dune/internal/webapp"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/fabricd"
	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/sdk"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func main() {
	if code, handled := fabricd.RunHelper(os.Args[1:]); handled {
		os.Exit(code)
	}

	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	fs := flag.NewFlagSet("dune", flag.ContinueOnError)
	path := fs.String("config", config.DefaultPath(), "machine config path")
	loginPath := fs.String("login", "", "private human CLI credential file; replaces machine config for execution")
	target := fs.String("target", "", "machine target for --login execution")
	runnerID := fs.String("runner", "", "logical Runner for --login execution; resolves one binding")

	if e := fs.Parse(os.Args[1:]); e != nil {
		return e
	}
	args := fs.Args()
	if *loginPath != "" {
		configSet := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "config" {
				configSet = true
			}
		})
		if configSet {
			return fmt.Errorf("--login and --config cannot be combined")
		}
		if len(args) == 0 {
			return fmt.Errorf("an execution command, machines or runners is required with --login")
		}
		switch args[0] {
		case "runners", "machines", "capabilities", "profile", "runtime", "exec", "files", "upload", "git", "upload-file", "ports":
		default:
			return fmt.Errorf("--login is only for human execution commands, machines and runners")
		}
	} else if *target != "" || *runnerID != "" {
		return fmt.Errorf("--target and --runner require --login")
	}
	if *target != "" && *runnerID != "" {
		return fmt.Errorf("--target and --runner cannot be combined")
	}
	if len(args) > 0 && (args[0] == "help" || args[0] == "version") {
		fmt.Println("Dune MVP/1\n  dune init [IP:PORT] or init --listen IP:PORT --gateway ws://HOST:PORT/tunnel\n  dune [gateway|fabricd]\n  dune web [--data DIR | --database-config FILE] [--url URL] [--identity-config FILE]\n  dune metadata import-json|copy-sqlite --source DIR [--data DIR | --database-config FILE]\n  dune login --site URL [--file FILE] [--no-browser] [--certificate FILE]\n  dune logout [--file FILE]\n  dune --login FILE machines|runners [--limit 1..100] [--cursor CURSOR]\n  dune --login FILE --runner RUNNER_ID exec [--cwd DIR] -- COMMAND ARG...\n  dune --login FILE --target MACHINE_ID exec [--cwd DIR] -- COMMAND ARG...\n  dune capabilities\n  dune profile start PROFILE.yaml [--detach]\n  dune runtime list|get|attach|stop ID\n  dune exec [--cwd DIR] -- COMMAND ARG...\n  dune files|upload|git REQUEST.json (or - for stdin)\n  dune upload-file LOCAL REMOTE\n  dune ports forward LOCAL_PORT REMOTE_PORT\nGlobal --config, --login, --runner and --target must precede the subcommand; --login and --config are mutually exclusive.")
		return nil
	}
	if len(args) > 0 && args[0] == "init" {
		flags := flag.NewFlagSet("init", flag.ContinueOnError)
		listen := flags.String("listen", "127.0.0.1:7443", "listen IP:port")
		endpoint := flags.String("gateway", "", "reachable ws://HOST:PORT/tunnel")
		if e := flags.Parse(args[1:]); e != nil {
			return e
		}
		if flags.NArg() > 1 {
			return fmt.Errorf("init accepts at most one positional listen address")
		}
		if flags.NArg() == 1 {
			*listen = flags.Arg(0)
		}
		return config.InitWithGateway(*path, *listen, *endpoint)
	}
	if len(args) > 0 && args[0] == "enroll" {
		flags := flag.NewFlagSet("enroll", flag.ContinueOnError)
		site := flags.String("site", "", "Dune HTTP(S) site URL, including its deployment prefix")
		token := flags.String("token", "", "one-time machine binding token")
		certificate := flags.String("certificate", "", "optional custom trust certificate")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return webapp.EnrollMachine(context.Background(), *path, *site, *token, *certificate)
	}
	if len(args) > 0 && args[0] == "metadata" {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		return runMetadata(ctx, args[1:])
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if len(args) > 0 && (args[0] == "login" || args[0] == "logout") {
		return runLogin(ctx, args[1:], args[0] == "logout")
	}
	var c config.Config
	var e error
	if *loginPath == "" {
		c, e = config.Load(*path)
		if e != nil {
			return fmt.Errorf("%w; initialize with dune init", e)
		}
	}
	if len(args) == 0 {
		if e := c.ValidateServer(); e != nil {
			return e
		}
		return supervisor.Run(ctx, *path)
	}
	switch args[0] {
	case "service":
		if len(args) < 2 {
			return fmt.Errorf("service install|restart|stop|status [--name dune]")
		}
		flags := flag.NewFlagSet("service", flag.ContinueOnError)
		name := flags.String("name", "dune", "per-user service name")
		if err := flags.Parse(args[2:]); err != nil {
			return err
		}
		if c.SessionDir == "" {
			return fmt.Errorf("background connector requires session_dir; use dune enroll first")
		}
		return service.Run(args[1], *path, *name)
	case "gateway":
		return gateway.Run(ctx, c)
	case "fabricd":
		return daemon.Run(ctx, c)
	case "web":
		flags := flag.NewFlagSet("web", flag.ContinueOnError)
		data := flags.String("data", ".local/web-accounts", "private SQLite metadata directory")
		databaseFile := flags.String("database-config", "", "private SQL configuration; replaces the default SQLite data directory")
		identityFile := flags.String("identity-config", "", "private OIDC configuration; replaces local password login")
		assets := flags.String("assets", "web/dist", "built React assets directory")
		binaries := flags.String("binaries", "bin", "published dune-OS-ARCH binaries directory")
		publicURL := flags.String("url", "", "public browser HTTP(S) URL, optionally with a deployment prefix")
		gatewayURL := flags.String("gateway-url", "", "optional complete machine WS(S) URL; defaults to public URL + tunnel")
		disableRegistration := flags.Bool("disable-registration", false, "disable local sign-up; existing accounts can still log in")
		webListen := flags.String("web-listen", "", "optional extra loopback HTTP listener for local validation")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		options := host.Options{DataDir: *data, Assets: *assets, PublicURL: *publicURL, GatewayURL: *gatewayURL, Binaries: *binaries, DisableRegistration: *disableRegistration}
		if *identityFile != "" {
			identity, err := config.Identity(ctx, *identityFile)
			if err != nil {
				return err
			}
			options.Identity = &identity
		}
		if *databaseFile != "" {
			dataSet := false
			flags.Visit(func(f *flag.Flag) {
				if f.Name == "data" {
					dataSet = true
				}
			})
			if dataSet {
				return fmt.Errorf("--data and --database-config cannot be combined")
			}
			database, err := config.Database(*databaseFile)
			if err != nil {
				return err
			}
			options.DataDir, options.Database = "", &database
		}
		return runWeb(ctx, c, options, *webListen)

	}
	var client *sdk.Client
	if *loginPath != "" {
		credentials, err := config.LoadUserCredentials(*loginPath)
		if err != nil {
			return err
		}
		human, err := loginClient(credentials.Session.Site, credentials.Certificate)
		if err != nil {
			return err
		}
		defer human.Close()
		if args[0] == "machines" || args[0] == "runners" {
			flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
			cursor := flags.String("cursor", "", "opaque continuation from the preceding page")
			limit := flags.Int("limit", 32, "page size (1..100)")
			if err := flags.Parse(args[1:]); err != nil {
				return err
			}
			if flags.NArg() != 0 || *limit < 1 || *limit > 100 {
				return fmt.Errorf("invalid discovery arguments")
			}
			query := runner.Query{Cursor: *cursor, Limit: *limit}
			if args[0] == "machines" {
				page, err := human.Machines(ctx, credentials.Session, query)
				if err != nil {
					return err
				}
				return printJSON(page)
			}
			page, err := human.Runners(ctx, credentials.Session, query)
			if err != nil {
				return err
			}
			return printJSON(page)
		}

		if *runnerID != "" {
			row, err := human.Runner(ctx, credentials.Session, *runnerID)
			if err != nil {
				return err
			}
			if row.Binding == nil {
				return fmt.Errorf("runner has no current machine binding")
			}
			client, e = human.DialRunner(ctx, credentials.Session, *row.Binding)
		} else {
			client, e = human.Dial(ctx, credentials.Session, *target)
		}
	} else {
		tc, err := c.TLS()
		if err != nil {
			return err
		}
		client, e = sdk.Dial(ctx, sdk.Options{Gateway: c.Gateway, Token: c.Token, Target: c.Target, TLSConfig: tc})
	}
	if e != nil {
		return e
	}
	defer client.Close()
	switch args[0] {
	case "capabilities":
		return printJSON(client.Binding)
	case "profile":
		if len(args) < 3 || args[1] != "start" {
			return fmt.Errorf("profile start FILE [--detach]")
		}
		b, e := os.ReadFile(args[2])
		if e != nil {
			return e
		}
		var p api.Profile
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if e = dec.Decode(&p); e != nil {
			return e
		}
		r, s, e := client.Start(ctx, p)
		if e != nil {
			return e
		}
		defer s.Close()
		if len(args) > 3 && args[3] == "--detach" {
			return printJSON(r)
		}
		fmt.Fprintln(os.Stderr, "Runtime:", r.ID)
		return interact(ctx, s, r.Adapter)
	case "runtime":
		if len(args) < 2 {
			return fmt.Errorf("runtime list|get|attach|stop")
		}
		list, e := client.List(ctx)
		if e != nil {
			return e
		}
		if args[1] == "list" {
			return printJSON(list)
		}
		if len(args) < 3 {
			return fmt.Errorf("Runtime ID required")
		}
		for _, r := range list {
			if r.ID != args[2] {
				continue
			}
			switch args[1] {
			case "get":
				return printJSON(r)
			case "stop":
				return client.Stop(ctx, r)
			case "attach":
				s, e := client.Attach(ctx, r, false)
				if e != nil {
					return e
				}
				defer s.Close()
				return interact(ctx, s, r.Adapter)
			}
		}
		return fmt.Errorf("Runtime not found or invalid action")
	case "exec":
		f := flag.NewFlagSet("exec", flag.ContinueOnError)
		cwd := f.String("cwd", "", "working directory")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		if *cwd == "" {
			*cwd, _ = os.Getwd()
		}
		r, e := client.Exec(ctx, api.Exec{Command: api.Command{Argv: f.Args()}, WorkingDirectory: *cwd})
		if e != nil {
			return e
		}
		printJSON(r)
		if r.ExitCode != 0 || r.TimedOut {
			return fmt.Errorf("command failed")
		}
		return nil
	case "files", "upload", "git":
		if len(args) != 2 {
			return fmt.Errorf("%s REQUEST.json (or -)", args[0])
		}
		var b []byte
		var e error
		if args[1] == "-" {
			b, e = io.ReadAll(io.LimitReader(os.Stdin, 1024*1024+1))
		} else {
			b, e = os.ReadFile(args[1])
		}
		if e != nil {
			return e
		}
		if len(b) > 1024*1024 {
			return fmt.Errorf("request too large")
		}
		var in, out any
		switch args[0] {
		case "files":
			in = &api.File{}
		case "upload":
			in = &api.Upload{}
		case "git":
			in = &api.Git{}
		}
		if e = json.Unmarshal(b, in); e != nil {
			return e
		}
		if args[0] == "git" {
			r, e := client.Git(ctx, *in.(*api.Git))
			if e != nil {
				return e
			}
			if e = printJSON(r); e != nil {
				return e
			}
			if r.ExitCode != 0 || r.TimedOut {
				return fmt.Errorf("Git operation failed")
			}
			return nil
		}
		if e = client.Call(ctx, args[0], in, &out); e != nil {
			return e
		}
		return printJSON(out)
	case "upload-file":
		if len(args) != 3 {
			return fmt.Errorf("upload-file LOCAL REMOTE")
		}
		return uploadFile(ctx, client, args[1], args[2])
	case "ports":
		if len(args) != 4 || args[1] != "forward" {
			return fmt.Errorf("ports forward LOCAL_PORT REMOTE_PORT")
		}
		remote, e := strconv.Atoi(args[3])
		if e != nil {
			return e
		}
		ln, e := net.Listen("tcp", "127.0.0.1:"+args[2])
		if e != nil {
			return e
		}
		defer ln.Close()
		go func() { <-ctx.Done(); ln.Close() }()
		for {
			conn, e := ln.Accept()
			if e != nil {
				if ctx.Err() != nil {
					return nil
				}
				return e
			}
			go func() {
				defer conn.Close()
				p, e := client.Connect(ctx, remote)
				if e != nil {
					fmt.Fprintln(os.Stderr, e)
					return
				}
				defer p.Close()
				done := make(chan error, 2)
				go func() {
					_, e := io.Copy(p, conn)
					if e == nil {
						e = p.CloseWrite()
					}
					done <- e
				}()
				go func() {
					_, e := io.Copy(conn, p)
					if e == nil {
						e = conn.(*net.TCPConn).CloseWrite()
					}
					done <- e
				}()
				e1 := <-done
				if e1 != nil {
					p.Close()
					conn.Close()
				}
				e2 := <-done
				if e1 == nil && e2 == nil {
					if e = p.Finish(); e != nil {
						fmt.Fprintln(os.Stderr, e)
					}
				}
			}()
		}
	default:
		return fmt.Errorf("unknown command; use dune help")
	}
}
func printJSON(v any) error {
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	return e.Encode(v)
}
func interact(ctx context.Context, s *sdk.Stream, adapter string) error {
	if adapter == "pty" && term.IsTerminal(int(os.Stdin.Fd())) {
		old, e := term.MakeRaw(int(os.Stdin.Fd()))
		if e != nil {
			return e
		}
		defer term.Restore(int(os.Stdin.Fd()), old)
		resize := func() {
			w, h, e := term.GetSize(int(os.Stdin.Fd()))
			if e == nil {
				s.Resize(uint16(h), uint16(w))
			}
		}
		resize()
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGWINCH)
		defer signal.Stop(ch)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-ch:
					resize()
				}
			}
		}()
	}
	go func() {
		if adapter == "acp" {
			scan := bufio.NewScanner(os.Stdin)
			scan.Buffer(make([]byte, 4096), 32768)
			for scan.Scan() {
				if _, e := s.Input(scan.Bytes()); e != nil {
					return
				}
			}
		} else {
			b := make([]byte, 32768)
			for {
				n, e := os.Stdin.Read(b)
				if n > 0 {
					if _, e := s.Input(b[:n]); e != nil {
						return
					}
				}
				if e != nil {
					return
				}
			}
		}
	}()
	for {
		m, e := s.Recv()
		if e != nil {
			return e
		}
		switch m.Kind {
		case "data":
			os.Stdout.Write(m.Data)
		case "stderr":
			os.Stderr.Write(m.Data)
		case "exit":
			var code int
			json.Unmarshal(m.Payload, &code)
			if code != 0 {
				return fmt.Errorf("Agent exit %d", code)
			}
			return nil
		}
	}
}
func uploadFile(ctx context.Context, c *sdk.Client, local, remote string) error {
	f, e := os.Open(local)
	if e != nil {
		return e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return e
	}
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return e
	}
	f.Seek(0, 0)
	if !filepath.IsAbs(remote) {
		return fmt.Errorf("remote path must be absolute")
	}
	u, e := c.Upload(ctx, api.Upload{Action: "create", Path: remote, Size: info.Size(), SHA256: hex.EncodeToString(h.Sum(nil))})
	if e != nil {
		return e
	}
	fmt.Fprintln(os.Stderr, "Upload:", u.ID)
	b := make([]byte, 32768)
	for {
		n, e := f.Read(b)
		if n > 0 {
			u, e = c.Upload(ctx, api.Upload{Action: "chunk", ID: u.ID, Offset: u.Offset, Data: b[:n]})
			if e != nil {
				return e
			}
		}
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return e
		}
	}
	u, e = c.Upload(ctx, api.Upload{Action: "commit", ID: u.ID})
	if e != nil {
		return e
	}
	return printJSON(u)
}
