package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/aiomni/dune/internal/buildinfo"
	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/fabricd"
	"github.com/aiomni/dune/pkg/sdk"
)

func runVersion(configPath string, args []string) error {
	flags := flag.NewFlagSet("version", flag.ContinueOnError)
	runner := flags.Bool("runner", false, "also inspect the configured Runner through its Gateway")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("version accepts only --runner")
	}
	minimum, maximum := fabricd.HostProtocolRange()
	report := struct {
		Executable  api.BuildInfo    `json:"executable"`
		DTP         string           `json:"dtp_protocol"`
		ProtocolMin int              `json:"acp_host_protocol_min"`
		ProtocolMax int              `json:"acp_host_protocol_max"`
		Machine     *api.MachineInfo `json:"machine,omitempty"`
		Runtimes    *api.RuntimeList `json:"runtimes,omitempty"`
	}{Executable: buildinfo.Current(), DTP: api.Version, ProtocolMin: minimum, ProtocolMax: maximum}
	if *runner {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		tls, err := cfg.TLS()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		connection, err := sdk.Dial(ctx, sdk.Options{Gateway: cfg.Gateway, Token: cfg.Token, Target: cfg.Target, TLSConfig: tls})
		if err != nil {
			return err
		}
		defer connection.Close()
		var machine api.MachineInfo
		if err := connection.Call(ctx, "machine.info", struct{}{}, &machine); err != nil {
			return err
		}
		runtimes, err := connection.List(ctx)
		if err != nil {
			return err
		}
		report.Machine, report.Runtimes = &machine, &runtimes
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
