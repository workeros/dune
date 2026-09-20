// raw-acp demonstrates public byte transport for an existing raw Runtime.
// Every mutation requires a key file saved by the caller before this invocation.
// It never starts/replaces an Agent, initializes ACP or retries unknown writes.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
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
	ca := flag.String("ca", "", "optional PEM CA")
	keyFile := flag.String("key", "", "saved original Runtime SubmissionKey JSON; required even for querying an interrupted write")
	action := flag.String("action", "state", "state, read, take, write or query")
	stream := flag.String("stream", "", "previously observed raw stream ID")
	owner := flag.String("owner", "", "caller-generated input owner ID")
	epoch := flag.Uint64("epoch", 0, "previously observed epoch for take; owned epoch for write")
	channel := flag.String("channel", "stdout", "stdout or stderr")
	offset := flag.Uint64("offset", 0, "original byte offset to read")
	message := flag.String("message", "", "complete JSON-RPC message file including its original LF/CRLF")
	flag.Parse()
	if *gateway == "" || *keyFile == "" {
		return fmt.Errorf("gateway and saved key file are required")
	}
	keyData, err := os.ReadFile(*keyFile)
	if err != nil {
		return err
	}
	var key api.SubmissionKey
	if err = json.Unmarshal(keyData, &key); err != nil {
		return err
	}
	if err = key.Validate(); err != nil {
		return err
	}
	if key.Target.RuntimeID == "" {
		return fmt.Errorf("key must select the original Runtime")
	}
	var body []byte
	if *action == "write" {
		f, err := os.Open(*message)
		if err != nil {
			return err
		}
		defer f.Close()
		body, err = io.ReadAll(io.LimitReader(f, api.RawACPMaxMessageBytes+1))
		if err != nil {
			return err
		}
		if len(body) > api.RawACPMaxMessageBytes {
			return fmt.Errorf("message exceeds 1 MiB")
		}
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := sdk.Dial(ctx, sdk.Options{Gateway: *gateway, Target: key.Target.MachineID, Token: os.Getenv("DUNE_TOKEN"), TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}})
	if err != nil {
		return err
	}
	defer client.Close()
	runtime := api.Runtime{ID: key.Target.RuntimeID, Incarnation: key.Target.RuntimeIncarnation, Generation: key.Target.RuntimeGeneration}
	var result any
	switch *action {
	case "state":
		result, err = client.RawACPState(ctx, runtime)
	case "read":
		result, err = client.ReadRawACP(ctx, runtime, api.RawACPRead{StreamID: *stream, Channel: *channel, Offset: *offset})
	case "query":
		result, err = client.QuerySubmission(ctx, key)
	case "take":
		result, err = client.TakeRawACP(ctx, key, api.RawACPTake{StreamID: *stream, ExpectedEpoch: *epoch, OwnerID: *owner})
	case "write":
		result, err = client.WriteRawACP(ctx, key, api.RawACPWrite{StreamID: *stream, InputEpoch: *epoch, OwnerID: *owner, Length: len(body), SHA256: fmt.Sprintf("%x", sha256.Sum256(body)), Data: body})
	default:
		return fmt.Errorf("unknown action")
	}
	// Retain receipts and STREAM_GAP windows even alongside a structured error.
	if encodeErr := json.NewEncoder(os.Stdout).Encode(result); encodeErr != nil {
		return encodeErr
	}
	return err
}
