package main

import (
	"bytes"
	"io"
	"testing"
)

func TestIPCRoundTrip(t *testing.T) {
	envelopes := []ipcEnvelope{
		ipcSubscribeEnv("pm", 0, "123456789", "user", []string{"u1"}, []string{"r1"}, true),
		ipcSendEnv("123456789", "hello world"),
		ipcUnsubscribeEnv("123456789"),
		ipcAckEnv("123456789"),
		ipcInboundEnv("123456789", "@alice", "user @alice: hi"),
		ipcErrorEnv("routing-key-conflict", "already owned by builder"),
	}

	var buf bytes.Buffer
	wr := newIPCWriter(&buf)
	for _, env := range envelopes {
		if err := wr.Write(env); err != nil {
			t.Fatalf("write %s: %v", env.Type, err)
		}
	}

	rd := newIPCReader(&buf)
	for i, want := range envelopes {
		got, err := rd.Read()
		if err != nil {
			t.Fatalf("read[%d]: %v", i, err)
		}
		if got.V != ipcVersion {
			t.Errorf("[%d] v: got %d want %d", i, got.V, ipcVersion)
		}
		if got.Type != want.Type {
			t.Errorf("[%d] type: got %q want %q", i, got.Type, want.Type)
		}
		if got.RoutingKey != want.RoutingKey {
			t.Errorf("[%d] routingKey: got %q want %q", i, got.RoutingKey, want.RoutingKey)
		}
		if got.Content != want.Content {
			t.Errorf("[%d] content: got %q want %q", i, got.Content, want.Content)
		}
	}

	// Next read should return EOF.
	_, err := rd.Read()
	if err != io.EOF {
		t.Errorf("expected EOF after all envelopes, got %v", err)
	}
}

func TestIPCVersionMismatch(t *testing.T) {
	var buf bytes.Buffer
	// Write a fake envelope with the wrong version.
	buf.WriteString(`{"v":99,"type":"subscribe","routingKey":"abc"}` + "\n")
	rd := newIPCReader(&buf)
	_, err := rd.Read()
	if err == nil {
		t.Fatal("expected version mismatch error, got nil")
	}
}
