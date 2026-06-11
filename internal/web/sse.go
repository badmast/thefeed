package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	// Disable the server-wide WriteTimeout for this long-lived SSE connection.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan string, 500)
	s.sseMu.Lock()
	s.clients[ch] = struct{}{}
	s.sseMu.Unlock()

	defer func() {
		s.sseMu.Lock()
		delete(s.clients, ch)
		s.sseMu.Unlock()
	}()

	s.logMu.RLock()
	for _, line := range s.logLines {
		data, _ := json.Marshal(line)
		fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
	}
	s.logMu.RUnlock()
	flusher.Flush()

	ctx := r.Context()
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			// SSE comment line as heartbeat — keeps the connection alive and
			// lets us detect a dead client (write error).
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case msg := <-ch:
			if _, err := fmt.Fprint(w, msg); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) broadcast(event string) {
	s.sseMu.Lock()
	defer s.sseMu.Unlock()
	for ch := range s.clients {
		select {
		case ch <- event:
		default:
		}
	}
}

// hasUIClients reports whether at least one UI (SSE) connection is open. The
// chat background loop polls more often while a UI is watching so incoming
// messages surface quickly even when the messenger view itself is closed.
func (s *Server) hasUIClients() bool {
	s.sseMu.Lock()
	defer s.sseMu.Unlock()
	return len(s.clients) > 0
}

func (s *Server) addLog(msg string) {
	ts := time.Now().Format("15:04:05")
	line := fmt.Sprintf("%s %s", ts, msg)

	s.logMu.Lock()
	s.logLines = append(s.logLines, line)
	if len(s.logLines) > 200 {
		s.logLines = s.logLines[len(s.logLines)-200:]
	}
	s.logMu.Unlock()

	data, _ := json.Marshal(line)
	s.broadcast(fmt.Sprintf("event: log\ndata: %s\n\n", data))
}
