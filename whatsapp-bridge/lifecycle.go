package main

import (
	"fmt"
	"net"
	"net/http"
	"sync/atomic"

	"go.mau.fi/whatsmeow/types/events"
)

// listenLocal binds the REST API port on the loopback interface. main calls it
// before touching the WhatsApp session, so the port doubles as a
// single-instance lock: a second bridge fails here instead of logging in with
// the same session and knocking the first one offline (see terminalEventReason).
func listenLocal(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}

// serveGated starts answering on listener right away and returns the gate that
// lets requests through to next. Until the gate is set the bridge is still
// pairing or connecting, and every request gets a 503: left unserved, requests
// would sit in the accept backlog and run whenever the connection came up —
// after the caller had already timed out and possibly retried.
func serveGated(listener net.Listener, next http.Handler) *atomic.Bool {
	ready := new(atomic.Bool)
	go func() {
		if err := http.Serve(listener, gateHandler(ready, next)); err != nil {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()
	return ready
}

// gateHandler answers 503 in plain text (not the JSON {success,message} body:
// nothing was attempted, and the Python client only counts JSON answers
// against its send rate limit) until ready is set.
func gateHandler(ready *atomic.Bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, "WhatsApp bridge is starting and not connected yet; try again shortly", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
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
