// acp-conversation demonstrates public, read-only model access. It never starts
// an Agent or performs new/load/prompt. Use DUNE_TOKEN for an authorized token.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/sdk"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	gateway := flag.String("gateway", "", "authorized Gateway WebSocket URL")
	ca := flag.String("ca", "", "optional PEM CA for a private Gateway certificate")
	target := flag.String("target", "", "exact Runner target")
	runtimePath := flag.String("runtime", "", "JSON file containing the complete Runtime from discovery")
	cursor := flag.String("cursor", "", "opaque next_cursor from an earlier page")
	ids := flag.String("entries", "", "comma-separated entry IDs to refresh")
	conversation := flag.String("conversation", "", "observed conversation ID; required with entries")
	limit := flag.Int("limit", 50, "entries per page (1..200)")
	flag.Parse()
	if *gateway == "" || *target == "" || *runtimePath == "" {
		return fmt.Errorf("gateway, target and runtime are required")
	}
	if *cursor != "" && (*ids != "" || *conversation != "") {
		return fmt.Errorf("cursor cannot be combined with entries or conversation")
	}
	if *ids != "" && *conversation == "" {
		return fmt.Errorf("entries must use their previously observed conversation")
	}
	data, err := os.ReadFile(*runtimePath)
	if err != nil {
		return err
	}
	var runtime api.Runtime
	if err = json.Unmarshal(data, &runtime); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var roots *x509.CertPool
	if *ca != "" {
		pem, err := os.ReadFile(*ca)
		if err != nil {
			return err
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return fmt.Errorf("CA file has no certificates")
		}
	}
	client, err := sdk.Dial(ctx, sdk.Options{Gateway: *gateway, Target: *target, Token: os.Getenv("DUNE_TOKEN"), TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}})
	if err != nil {
		return err
	}
	defer client.Close()
	encode := json.NewEncoder(os.Stdout)
	encode.SetIndent("", "  ")
	if *ids != "" {
		result, err := client.GetACPConversationEntries(ctx, runtime, api.ACPConversationGet{ConversationID: *conversation, EntryIDs: strings.Split(*ids, ",")})
		if err != nil {
			return err
		}
		return encode.Encode(result)
	}
	request := api.ACPConversationRead{ConversationID: *conversation, Cursor: *cursor, Limit: limit}
	if request.Cursor == "" && request.ConversationID == "" {
		state, err := client.ACPState(ctx, runtime)
		if err != nil {
			return err
		}
		if state.Conversation == nil {
			return fmt.Errorf("Runtime has no conversation; reading does not create one")
		}
		request.ConversationID = state.Conversation.ID
	}
	page, err := client.ReadACPConversation(ctx, runtime, request)
	if err != nil {
		return err
	}
	return encode.Encode(page)
}
