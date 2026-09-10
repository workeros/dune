package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/daemon"
	internalgateway "github.com/aiomni/dune/internal/gateway"
	"github.com/aiomni/dune/internal/service"
	"github.com/aiomni/dune/internal/webapp"
	"github.com/aiomni/dune/pkg/fabricd"
	"github.com/aiomni/dune/pkg/host"
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

func run() error {
	flags := flag.NewFlagSet("dune", flag.ContinueOnError)
	configPath := flags.String("config", config.DefaultPath(), "machine or Web listener configuration")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	args := flags.Args()
	if len(args) == 0 {
		return fmt.Errorf("a Web or connector command is required; use dune help")
	}
	if args[0] == "help" || args[0] == "version" {
		fmt.Println("Dune\n  dune --config FILE init [IP:PORT] or init --listen IP:PORT --gateway ws://HOST:PORT/tunnel\n  dune --config FILE gateway\n  dune --config FILE fabricd\n  dune --config FILE web [--data DIR | --database-config FILE] [--url URL] [--cluster-config FILE]\n  dune --config FILE enroll --site URL --token TOKEN\n  dune --config FILE service install|restart|stop|status [--name dune]")
		return nil
	}
	if args[0] == "init" {
		initFlags := flag.NewFlagSet("init", flag.ContinueOnError)
		listen := initFlags.String("listen", "127.0.0.1:7443", "listen IP:port")
		endpoint := initFlags.String("gateway", "", "reachable ws://HOST:PORT/tunnel")
		if err := initFlags.Parse(args[1:]); err != nil {
			return err
		}
		if initFlags.NArg() > 1 {
			return fmt.Errorf("init accepts at most one positional listen address")
		}
		if initFlags.NArg() == 1 {
			*listen = initFlags.Arg(0)
		}
		return config.InitWithGateway(*configPath, *listen, *endpoint)
	}
	if args[0] == "enroll" {
		enrollFlags := flag.NewFlagSet("enroll", flag.ContinueOnError)
		site := enrollFlags.String("site", "", "Dune HTTP(S) site URL, including its deployment prefix")
		token := enrollFlags.String("token", "", "one-time machine binding token")
		certificate := enrollFlags.String("certificate", "", "optional custom trust certificate")
		if err := enrollFlags.Parse(args[1:]); err != nil {
			return err
		}
		return webapp.EnrollMachine(context.Background(), *configPath, *site, *token, *certificate)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	machineConfig, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("%w; initialize with dune init", err)
	}
	switch args[0] {
	case "gateway":
		return internalgateway.Run(ctx, machineConfig)
	case "service":
		if len(args) < 2 {
			return fmt.Errorf("service install|restart|stop|status [--name dune]")
		}
		serviceFlags := flag.NewFlagSet("service", flag.ContinueOnError)
		name := serviceFlags.String("name", "dune", "per-user service name")
		if err := serviceFlags.Parse(args[2:]); err != nil {
			return err
		}
		if machineConfig.SessionDir == "" {
			return fmt.Errorf("background connector requires session_dir; use dune enroll first")
		}
		return service.Run(args[1], *configPath, *name)
	case "fabricd":
		return daemon.Run(ctx, machineConfig)
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
	binaries := flags.String("binaries", "bin", "published dune-OS-ARCH binaries directory")
	publicURL := flags.String("url", "", "public browser HTTP(S) URL, optionally with a deployment prefix")
	gatewayURL := flags.String("gateway-url", "", "optional complete machine WS(S) URL; defaults to public URL + tunnel")
	disableRegistration := flags.Bool("disable-registration", false, "disable local sign-up; existing accounts can still log in")
	drainTimeout := flags.Duration("drain-timeout", 5*time.Second, "maximum graceful shutdown wait; zero closes immediately")
	webListen := flags.String("web-listen", "", "optional extra loopback HTTP listener for local validation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	options := host.Options{DataDir: *data, Assets: *assets, PublicURL: *publicURL, GatewayURL: *gatewayURL, Binaries: *binaries, DisableRegistration: *disableRegistration}
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
