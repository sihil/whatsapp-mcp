package main

import (
	"fmt"
	"net"

	"go.mau.fi/whatsmeow/types/events"
)

// listenLocal binds the REST API port on the loopback interface. main calls it
// before touching the WhatsApp session, so the port doubles as a
// single-instance lock: a second bridge fails here instead of logging in with
// the same session and knocking the first one offline (see terminalEventReason).
func listenLocal(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}

// terminalEventReason reports whether evt leaves the client permanently
// offline. whatsmeow reconnects on its own after ordinary stream errors, but
// not after StreamReplaced (another client logged in with this session), so
// the process has to exit and let its supervisor restart it — otherwise it
// keeps serving the REST API with no WhatsApp connection behind it.
func terminalEventReason(evt interface{}) (string, bool) {
	switch evt.(type) {
	case *events.StreamReplaced:
		return "session was opened by another client (stream replaced)", true
	}
	return "", false
}
