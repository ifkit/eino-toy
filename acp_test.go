package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// fakeAgent is a minimal ACP agent. The test binary runs it when FAKE_ACP is set.
func fakeAgent() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var m struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		json.Unmarshal(sc.Bytes(), &m)
		switch m.Method {
		case "initialize":
			fmt.Printf(`{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":1}}`+"\n", m.ID)
		case "session/new":
			fmt.Printf(`{"jsonrpc":"2.0","id":%d,"result":{"sessionId":"s1"}}`+"\n", m.ID)
		case "session/prompt":
			fmt.Println(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"hm"}}}}`)
			fmt.Println(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"he"}}}}`)
			fmt.Println(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"llo"}}}}`)
			fmt.Printf(`{"jsonrpc":"2.0","id":%d,"result":{"stopReason":"end_turn"}}`+"\n", m.ID)
		}
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("FAKE_ACP") != "" {
		fakeAgent()
		return
	}
	os.Exit(m.Run())
}

func TestACPStream(t *testing.T) {
	t.Setenv("FAKE_ACP", "1")
	m, err := newACP(context.Background(), []string{os.Args[0]}, ".")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	for range 2 { // second turn proves the session survives
		sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
		if err != nil {
			t.Fatal(err)
		}
		var think, text string
		for {
			c, err := sr.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			think += c.ReasoningContent
			text += c.Content
		}
		if think != "hm" || text != "hello" {
			t.Fatalf("think=%q text=%q", think, text)
		}
	}
}
