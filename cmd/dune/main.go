package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/daemon"
	internalgateway "github.com/aiomni/dune/internal/gateway"
	"github.com/aiomni/dune/internal/install"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/runnerupgrade"
	"github.com/aiomni/dune/internal/service"
	"github.com/aiomni/dune/internal/webapp"
	"github.com/aiomni/dune/pkg/fabricd"
	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/upgrade"
)

func main() {
	if code, handled := fabricd.RunHelper(os.Args[1:]); handled {
		os.Exit(code)
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	flags := flag.NewFlagSet("dune", flag.ContinueOnError)
	configPath := flags.String("config", config.DefaultPath(), "machine or Web listener configuration")
	serviceLogDir := flags.String("service-log-dir", "", "private bounded service diagnostics directory (fabricd only)")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	args := flags.Args()
	if len(args) == 0 {
		return fmt.Errorf("a Web or connector command is required; use dune help")
	}
	if *serviceLogDir != "" {
		if args[0] != "fabricd" {
			return fmt.Errorf("service diagnostics apply only to fabricd")
		}
		log, err := lifecycle.Open(*serviceLogDir, "service-events.jsonl")
		if err != nil {
			return err
		}
		log.Record(lifecycle.Entry{Kind: "service_starting"})
		defer func() {
			code := "STOPPED"
			if runErr != nil {
				code = "FAILED"
			}
			log.Record(lifecycle.Entry{Kind: "service_exit", Code: code})
			log.Close()
		}()
	}
	if args[0] == "version" {
		return runVersion(*configPath, args[1:])
	}
	if args[0] == "help" {
		fmt.Println("Dune\n  dune version [--runner] (JSON; --runner uses --config FILE)\n  dune --config FILE init [IP:PORT] or init --listen IP:PORT --gateway ws://HOST:PORT/api/v1/ws/tunnel\n  dune --config FILE gateway\n  dune --config FILE fabricd\n  dune --config FILE web [--data DIR | --database-config FILE] [--url URL] [--cluster-config FILE]\n  dune --config FILE enroll --site URL --token TOKEN --runner-id ID\n  dune --config FILE install [--root DIR] [--method service|managed]\n  dune --config FILE upgrade-check (read-only target executable preflight)\n  dune --config FILE service restart|stop|status [--name dune]")
		return nil
	}
	if args[0] == "init" {
		initFlags := flag.NewFlagSet("init", flag.ContinueOnError)
		listen := initFlags.String("listen", "127.0.0.1:7443", "listen IP:port")
		endpoint := initFlags.String("gateway", "", "reachable ws://HOST:PORT/api/v1/ws/tunnel")
		if err := initFlags.Parse(args[1:]); err != nil {
			return err
		}
		if initFlags.NArg() > 1 {
			return fmt.Errorf("init accepts at most one positional listen address")
		}
		if initFlags.NArg() == 1 {
			*listen = initFlags.Arg(0)
		}
		return config.Init(*configPath, *listen, *endpoint)
	}
	if args[0] == "enroll" {
		enrollFlags := flag.NewFlagSet("enroll", flag.ContinueOnError)
		site := enrollFlags.String("site", "", "Dune HTTP(S) site URL, including its deployment prefix")
		token := enrollFlags.String("token", "", "one-time machine binding token")
		runnerID := enrollFlags.String("runner-id", "", "stable pending Runner identifier from the install command")
		certificate := enrollFlags.String("certificate", "", "optional custom trust certificate")
		if err := enrollFlags.Parse(args[1:]); err != nil {
			return err
		}
		return webapp.EnrollMachine(context.Background(), *configPath, *site, *token, *certificate, *runnerID)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if args[0] == "upgrade-status" {
		statusFlags := flag.NewFlagSet("upgrade-status", flag.ContinueOnError)
		root := statusFlags.String("root", "", "original installation root")
		operation := statusFlags.String("operation", "", "operation ID; default is active or most recent")
		if err := statusFlags.Parse(args[1:]); err != nil {
			return err
		}
		result, err := runnerupgrade.Status(ctx, *root, *operation)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	if args[0] == "upgrade-recover" {
		recoveryFlags := flag.NewFlagSet("upgrade-recover", flag.ContinueOnError)
		root := recoveryFlags.String("root", "", "original installation root")
		operation := recoveryFlags.String("operation", "", "original blocked operation")
		revision := recoveryFlags.String("revision", "", "last observed operation revision")
		if err := recoveryFlags.Parse(args[1:]); err != nil {
			return err
		}
		return runnerupgrade.Recover(ctx, *root, *operation, *revision)
	}
	if args[0] == "repair-services" {
		repairFlags := flag.NewFlagSet("repair-services", flag.ContinueOnError)
		root := repairFlags.String("root", "", "registered installation root")
		if err := repairFlags.Parse(args[1:]); err != nil {
			return err
		}
		return install.RepairServices(ctx, *root)
	}
	if args[0] == "upgrade-worker" {
		workerFlags := flag.NewFlagSet("upgrade-worker", flag.ContinueOnError)
		root := workerFlags.String("root", "", "registered installation root")
		once := workerFlags.Bool("once", false, "resume one existing operation")
		if err := workerFlags.Parse(args[1:]); err != nil {
			return err
		}
		if *root == "" {
			return fmt.Errorf("installation root required")
		}
		if *once {
			return runnerupgrade.Run(ctx, *root)
		}
		return runnerupgrade.Watch(ctx, *root)
	}
	machineConfig, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	switch args[0] {

	case "upgrade-check":
		report := fabricd.CheckUpgrade(ctx, machineConfig.SessionDir)
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			return err
		}
		if !report.Allowed {
			return fmt.Errorf("uninterrupted connector replacement refused; see upgrade-check issues")
		}
		return nil
	case "install":
		installFlags := flag.NewFlagSet("install", flag.ContinueOnError)
		home, _ := os.UserHomeDir()
		root := installFlags.String("root", home+"/.local/share/dune", "new standard installation directory")
		name := installFlags.String("name", "dune", "per-user service name")
		method := installFlags.String("method", "service", "service or managed installation")
		if err := installFlags.Parse(args[1:]); err != nil {
			return err
		}
		return install.Run(ctx, *configPath, *root, *name, *method)

	case "gateway":
		return internalgateway.Run(ctx, machineConfig)
	case "service":
		if len(args) < 2 {
			return fmt.Errorf("service restart|stop|status [--name dune]")
		}
		serviceFlags := flag.NewFlagSet("service", flag.ContinueOnError)
		name := serviceFlags.String("name", "dune", "registered per-user service name")
		if err := serviceFlags.Parse(args[2:]); err != nil {
			return err
		}
		return service.Run(ctx, args[1], *name)

	case "fabricd":
		daemonFlags := flag.NewFlagSet("fabricd", flag.ContinueOnError)
		readyFile := daemonFlags.String("ready-file", "", "private startup receipt path")
		readyNonce := daemonFlags.String("ready-nonce", "", "private startup nonce")
		if err := daemonFlags.Parse(args[1:]); err != nil {
			return err
		}
		return daemon.Run(ctx, machineConfig, *readyFile, *readyNonce)
	case "web":
		return runWebCommand(ctx, machineConfig, args[1:])
	default:
		return fmt.Errorf("unknown command; use dune help")
	}
}

func runWebCommand(ctx context.Context, machineConfig config.Config, args []string) error {
	flags := flag.NewFlagSet("web", flag.ContinueOnError)
	data := flags.String("data", ".local/web-accounts", "private SQLite metadata directory")
	databaseFile := flags.String("database-config", "", "private SQL configuration; replaces the default SQLite data directory")
	clusterFile := flags.String("cluster-config", "", "private PostgreSQL cluster and mutual TLS peer configuration")
	assets := flags.String("assets", "web/dist", "built React assets directory")
	catalogPath := flags.String("upgrade-catalog", "", "approved immutable release catalog JSON")
	binaries := flags.String("binaries", "bin", "published dune-OS-ARCH binaries directory")
	publicURL := flags.String("url", "", "public browser HTTP(S) URL, optionally with a deployment prefix")
	gatewayURL := flags.String("gateway-url", "", "optional complete machine WS(S) URL; defaults to public URL + api/v1/ws/tunnel")
	trustedProxies := flags.String("trusted-proxies", "", "comma-separated trusted proxy CIDRs for login source limits")
	disableRegistration := flags.Bool("disable-registration", false, "disable local sign-up; existing accounts can still log in")
	drainTimeout := flags.Duration("drain-timeout", 5*time.Second, "maximum graceful shutdown wait; zero closes immediately")
	webListen := flags.String("web-listen", "", "optional extra loopback HTTP listener for local validation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	options := host.Options{DataDir: *data, Assets: *assets, PublicURL: *publicURL, GatewayURL: *gatewayURL, Binaries: *binaries, DisableRegistration: *disableRegistration}
	if *catalogPath != "" {
		file, err := os.Open(*catalogPath)
		if err != nil {
			return err
		}
		catalog, err := upgrade.ReadCatalog(file)
		file.Close()
		if err != nil {
			return fmt.Errorf("read approved upgrade catalog: %w", err)
		}
		options.UpgradeSource = catalog
	}
	if *trustedProxies != "" {
		options.TrustedProxies = strings.Split(*trustedProxies, ",")
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
	var peerListen string
	if *clusterFile != "" {
		if options.Database == nil || options.Database.Postgres == nil {
			return fmt.Errorf("--cluster-config requires a PostgreSQL --database-config")
		}
		cluster, err := config.Cluster(*clusterFile)
		if err != nil {
			return err
		}
		options.Cluster = &host.ClusterOptions{Peer: cluster.Peer}
		peerListen = cluster.Listen
	}
	return runWeb(ctx, machineConfig, options, *webListen, peerListen, *drainTimeout)
}
