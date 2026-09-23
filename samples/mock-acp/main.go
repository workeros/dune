// mock-acp is a deterministic test AgentServer, not a language model.
// It exercises the ACP stdio lifecycle without credentials or external services.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

func main() {
	if path := os.Getenv("DUNE_MOCK_PROCESS_LOG"); path != "" {
		input, _ := os.Stdin.Stat()
		output, _ := os.Stdout.Stat()
		log, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			panic(err)
		}
		_ = json.NewEncoder(log).Encode(map[string]any{"pid": os.Getpid(), "stdin_pipe": input.Mode()&os.ModeNamedPipe != 0, "stdout_pipe": output.Mode()&os.ModeNamedPipe != 0})
		_ = log.Close()
	}
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), 256*1024)
	enc := json.NewEncoder(os.Stdout)
	send := func(v any) {
		if e := enc.Encode(v); e != nil {
			os.Exit(1)
		}
	}
	reply := func(id json.RawMessage, result any) {
		send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	historyEnabled := os.Getenv("DUNE_MOCK_HISTORY") == "1"
	historyPath := filepath.Join(".", ".dune-mock-acp-history.json")
	history := []map[string]any{}
	if historyEnabled {
		if b, e := os.ReadFile(historyPath); e == nil {
			_ = json.Unmarshal(b, &history)
		}
	}
	save := func(update map[string]any) {
		if !historyEnabled {
			return
		}
		history = append(history, update)
		b, _ := json.Marshal(history)
		_ = os.WriteFile(historyPath, b, 0600)
	}
	finish := func(id json.RawMessage, text string) {
		save(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": text}})
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "mock-session", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}}}})
		reply(id, map[string]any{"stopReason": "end_turn"})
	}
	var pending json.RawMessage
	fmt.Fprintln(os.Stderr, "Dune mock ACP ready (no model calls)")
	for scan.Scan() {
		var m message
		if e := json.Unmarshal(scan.Bytes(), &m); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		if m.Method == "" {
			if path := os.Getenv("DUNE_MOCK_RESPONSE_LOG"); path != "" {
				log, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					panic(err)
				}
				_, err = fmt.Fprintln(log, string(scan.Bytes()))
				_ = log.Close()
				if err != nil {
					panic(err)
				}
			}
			if string(m.ID) == `"mock-permission"` && pending != nil {
				finish(pending, "Mock permission response received: "+string(m.Result))
				pending = nil
			}
			continue
		}
		if path := os.Getenv("DUNE_MOCK_RPC_LOG"); path != "" {
			log, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				panic(err)
			}
			_, err = fmt.Fprintln(log, m.Method)
			_ = log.Close()
			if err != nil {
				panic(err)
			}
		}
		switch m.Method {
		case "initialize":
			if os.Getenv("DUNE_MOCK_INITIALIZE_FAILURE") == "1" {
				send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32603, "message": "private-initialize-error-sentinel"}})
				continue
			}
			caps := map[string]any{}
			if historyEnabled {
				caps["loadSession"] = true
				caps["sessionCapabilities"] = map[string]any{"list": map[string]any{}}
			}
			reply(m.ID, map[string]any{"protocolVersion": 1, "agentInfo": map[string]string{"name": "dune-mock-acp", "version": "1"}, "agentCapabilities": caps, "authMethods": []any{}})
		case "session/new":
			reply(m.ID, map[string]string{"sessionId": "mock-session"})
			if title := os.Getenv("DUNE_MOCK_SESSION_TITLE"); title != "" {
				for _, update := range []map[string]any{
					{"sessionUpdate": "session_info_update", "title": title},
					{"sessionUpdate": "session_info_update", "updatedAt": "2026-09-23T00:00:00Z"},
				} {
					send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "mock-session", "update": update}})
				}
			}
		case "session/list":
			if !historyEnabled {
				reply(m.ID, map[string]any{"sessions": []any{}})
				continue
			}
			cwd, _ := os.Getwd()
			reply(m.ID, map[string]any{"sessions": []any{map[string]string{"sessionId": "mock-session", "cwd": cwd, "title": "Mock native history (no model)"}}})
		case "session/load":
			for i, u := range history {
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "mock-session", "update": u}})
				if address := os.Getenv("DUNE_MOCK_LOAD_GATE"); address != "" && i == len(history)/2 {
					gate, err := net.DialTimeout("tcp", address, 5*time.Second)
					if err != nil {
						panic(err)
					}
					_ = gate.SetDeadline(time.Now().Add(30 * time.Second))
					_, err = gate.Write([]byte{1})
					if err != nil {
						panic(err)
					}
					var release [1]byte
					_, err = gate.Read(release[:])
					_ = gate.Close()
					if err != nil {
						panic(err)
					}
				}
			}
			reply(m.ID, map[string]any{})
		case "session/prompt":
			if address := os.Getenv("DUNE_MOCK_PROMPT_GATE"); address != "" && strings.Contains(string(m.Params), "barrier") {
				gate, err := net.DialTimeout("tcp", address, 5*time.Second)
				if err != nil {
					panic(err)
				}
				timeout := 30 * time.Second
				if configured := os.Getenv("DUNE_MOCK_PROMPT_GATE_TIMEOUT"); configured != "" {
					var err error
					timeout, err = time.ParseDuration(configured)
					if err != nil || timeout <= 0 || timeout > 10*time.Minute {
						panic("invalid mock prompt gate timeout")
					}
				}
				_ = gate.SetDeadline(time.Now().Add(timeout))
				_, _ = gate.Write([]byte{1})
				var release [1]byte
				if _, err := gate.Read(release[:]); err != nil {
					panic(err)
				}
				_ = gate.Close()
			}
			var params struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			}
			_ = json.Unmarshal(m.Params, &params)
			for _, c := range params.Prompt {
				save(map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]string{"type": "text", "text": c.Text}})
			}
			if strings.Contains(string(m.Params), "permission") {
				if pending != nil {
					send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32000, "message": "permission already pending"}})
					continue
				}
				pending = append(json.RawMessage(nil), m.ID...)
				send(map[string]any{"jsonrpc": "2.0", "id": "mock-permission", "method": "session/request_permission", "params": map[string]any{"sessionId": "mock-session", "toolCall": map[string]string{"toolCallId": "mock-tool", "title": "Mock tool call"}, "options": []any{map[string]string{"optionId": "allow", "name": "Allow once", "kind": "allow_once"}, map[string]string{"optionId": "reject", "name": "Reject", "kind": "reject_once"}}}})
			} else {
				finish(m.ID, "Mock response: "+string(m.Params))
			}
		case "session/cancel":
			if pending != nil {
				reply(pending, map[string]string{"stopReason": "cancelled"})
				pending = nil
			}
		default:
			if len(m.ID) > 0 {
				send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "Method not found"}})
			}
		}
	}
	if e := scan.Err(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
