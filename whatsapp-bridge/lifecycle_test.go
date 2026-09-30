package main

import (
	"net"
	"testing"

	"go.mau.fi/whatsmeow/types/events"
)

func TestListenLocalRejectsSecondInstance(t *testing.T) {
	first, err := listenLocal(0)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	defer first.Close()

	addr := first.Addr().(*net.TCPAddr)
	if !addr.IP.IsLoopback() {
		t.Errorf("listener bound to %v, want loopback", addr.IP)
	}

	second, err := listenLocal(addr.Port)
	if err == nil {
		second.Close()
		t.Fatalf("second listen on port %d succeeded, want an error", addr.Port)
	}
}

func TestTerminalEventReason(t *testing.T) {
	tests := []struct {
		name     string
		evt      interface{}
		terminal bool
	}{
		{"stream replaced", &events.StreamReplaced{}, true},
		{"connected", &events.Connected{}, false},
		{"disconnected reconnects on its own", &events.Disconnected{}, false},
		{"message", &events.Message{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, terminal := terminalEventReason(tt.evt)
			if terminal != tt.terminal {
				t.Errorf("terminal = %v, want %v", terminal, tt.terminal)
			}
			if terminal && reason == "" {
				t.Error("terminal event has no reason")
			}
		})
	}
}
