package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// acpModel talks to an agent process over ACP (JSON-RPC 2.0, one JSON object per line).
// The agent keeps the conversation history, so only the last user message is sent.
// One turn at a time: the app does not send a new prompt while busy.
type acpModel struct {
	cmd     *exec.Cmd
	in      io.WriteCloser
	out     *bufio.Scanner
	nextID  int
	session string
}

type rpcMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func newACP(ctx context.Context, argv []string, cwd string) (*acpModel, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	m := &acpModel{cmd: cmd, in: in, out: bufio.NewScanner(out)}
	m.out.Buffer(nil, 16<<20)

	if _, err := m.call("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}, nil); err != nil {
		m.Close()
		return nil, fmt.Errorf("acp initialize: %w", err)
	}
	res, err := m.call("session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}}, nil)
	if err == nil {
		var r struct {
			SessionID string `json:"sessionId"`
		}
		if err = json.Unmarshal(res, &r); err == nil {
			m.session = r.SessionID
			return m, nil
		}
	}
	m.Close()
	return nil, fmt.Errorf("acp session/new: %w", err)
}

func (m *acpModel) Close() {
	m.in.Close()
	m.cmd.Process.Kill()
	m.cmd.Wait()
}

func (m *acpModel) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = m.in.Write(append(b, '\n'))
	return err
}

// call sends a request and reads until its response. Notifications go to onUpdate.
func (m *acpModel) call(method string, params any, onUpdate func(json.RawMessage)) (json.RawMessage, error) {
	m.nextID++
	id := strconv.Itoa(m.nextID)
	if err := m.send(map[string]any{"jsonrpc": "2.0", "id": m.nextID, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for m.out.Scan() {
		var msg rpcMsg
		if err := json.Unmarshal(m.out.Bytes(), &msg); err != nil {
			return nil, err
		}
		switch {
		case msg.Method == "":
			if string(msg.ID) != id {
				continue
			}
			if msg.Error != nil {
				return nil, errors.New(msg.Error.Message)
			}
			return msg.Result, nil
		case msg.ID == nil:
			if msg.Method == "session/update" && onUpdate != nil {
				onUpdate(msg.Params)
			}
		case msg.Method == "session/request_permission":
			// No permission UI: deny every tool call.
			m.send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}})
		default:
			m.send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32601, "message": "method not supported"}})
		}
	}
	if err := m.out.Err(); err != nil {
		return nil, err
	}
	return nil, io.ErrUnexpectedEOF
}

func (m *acpModel) Stream(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	var text string
	for i := len(input) - 1; i >= 0; i-- {
		if input[i].Role == schema.User {
			text = input[i].Content
			break
		}
	}
	if text == "" {
		return nil, errors.New("acp: no user message")
	}
	sr, sw := schema.Pipe[*schema.Message](8)
	go func() {
		defer sw.Close()
		_, err := m.call("session/prompt", map[string]any{
			"sessionId": m.session,
			"prompt":    []any{map[string]any{"type": "text", "text": text}},
		}, func(p json.RawMessage) {
			var n struct {
				Update struct {
					Kind    string `json:"sessionUpdate"`
					Content struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"update"`
			}
			if json.Unmarshal(p, &n) != nil {
				return
			}
			switch n.Update.Kind {
			case "agent_message_chunk":
				sw.Send(schema.AssistantMessage(n.Update.Content.Text, nil), nil)
			case "agent_thought_chunk":
				sw.Send(&schema.Message{Role: schema.Assistant, ReasoningContent: n.Update.Content.Text}, nil)
			}
		})
		if err != nil {
			sw.Send(nil, err)
		}
	}()
	return sr, nil
}

func (m *acpModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	sr, err := m.Stream(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.ConcatMessageStream(sr)
}
