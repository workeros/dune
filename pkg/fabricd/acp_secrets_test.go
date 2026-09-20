package fabricd

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestACPCredentialEchoIsRedactedFromOperationAndError(t *testing.T) {
	a, requests := queueFixture(t)
	a.state.SessionID = ""
	secret := "dune_agent_private_configuration_echo"
	if _, err := a.configureMCP(api.AgentMCP{URL: "https://host.test/mcp", Token: secret}); err != nil {
		t.Fatal(err)
	}
	created := submitAction(t, a, api.ACPAction{Action: "new"})
	rpc := takeRPC(t, requests)
	if !bytes.Contains(rpc.Params, []byte(secret)) {
		t.Fatal("native configuration was redacted")
	}
	replyRPC(a, rpc, map[string]string{"sessionId": "native"})
	waitOperation(t, a, created)
	operation := submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", Text: "hello"})
	rpc = takeRPC(t, requests)
	emitText(a, "native", "configuration token: "+secret)
	a.receive(api.Payload(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "error": map[string]any{"code": -32000, "message": "failed with " + secret, "data": map[string]string{"token": secret}}}))
	completed := waitOperation(t, a, operation)
	output := readOperation(t, a, operation)
	for _, value := range []any{completed, output, a.snapshot(), conversationPage(t, a.conversation)} {
		encoded := api.Payload(value)
		if bytes.Contains(encoded, []byte(secret)) || !bytes.Contains(encoded, []byte("[redacted]")) {
			t.Fatal("credential echo in public state or output")
		}
	}
}

func TestACPCredentialStderrRedactionAcrossEveryReadBoundary(t *testing.T) {
	secret := "dune_agent_private_stream_credential"
	for split := 1; split < len(secret); split++ {
		a, _ := queueFixture(t)
		a.mcpSecret.Store(&secret)
		sub, err := a.r.subscribe(false, false)
		if err != nil {
			t.Fatal(err)
		}
		reader := io.MultiReader(strings.NewReader("before "+secret[:split]), strings.NewReader(secret[split:]+" after\n"))
		a.readStderr(reader)
		var output bytes.Buffer
		for len(sub.q) > 0 {
			output.Write((<-sub.q).Data)
		}
		if output.String() != "before [redacted] after\n" {
			t.Fatalf("stderr boundary %d was not redacted", split)
		}
	}
}
