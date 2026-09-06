// Run with: go run ./samples/client --config ~/.config/dune/config.yaml
package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/sdk"
	"os"
	"time"
)

func main() {
	path := flag.String("config", config.DefaultPath(), "machine config")
	cwd := flag.String("cwd", "/tmp", "working directory on the execution machine")
	flag.Parse()
	c, e := config.Load(*path)
	check(e)
	tc, e := c.TLS()
	check(e)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, e := sdk.Dial(ctx, sdk.Options{Gateway: c.Gateway, Token: c.Token, Target: c.Target, TLSConfig: tc})
	check(e)
	defer client.Close()
	result, e := client.Exec(ctx, api.Exec{Command: api.Command{Argv: []string{"/usr/bin/uname", "-s"}}, WorkingDirectory: *cwd})
	check(e)
	fmt.Printf("target=%s incarnation=%s stdout=%q exit=%d\n", client.Binding.Target, client.Binding.Incarnation, result.Stdout, result.ExitCode)
}
func check(e error) {
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
