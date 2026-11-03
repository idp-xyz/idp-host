// Package transport holds the HTTP plumbing the HOST's API and its outbound clients share: bounded
// bodies, JSON and error responses, Server-Sent Events parsing and TLS configuration.
package transport

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
)

// ErrTooLarge is returned when a body exceeds its limit.
var ErrTooLarge = errors.New("body exceeds limit")

// ReadBounded reads at most limit bytes and fails if there are more.
func ReadBounded(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, ErrTooLarge
	}
	return b, nil
}

// MediaType returns the lower-cased media type of a Content-Type value ("" if unparseable).
func MediaType(contentType string) string {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return strings.ToLower(mt)
}

// WriteJSON writes v as an application/json response that is never cached.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// Problem is an API error: an HTTP status, a stable code and a message safe to show callers.
type Problem struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error implements error.
func (p *Problem) Error() string { return p.Code + ": " + p.Message }

// Fail builds a Problem.
func Fail(status int, code, message string) *Problem {
	return &Problem{Status: status, Code: code, Message: message}
}

// WriteProblem writes {"error": {...}} with the problem's status; any other error is a 500 whose
// detail is not disclosed.
func WriteProblem(w http.ResponseWriter, err error) {
	var p *Problem
	if !errors.As(err, &p) {
		p = Fail(http.StatusInternalServerError, "INTERNAL", "internal error")
	}
	WriteJSON(w, p.Status, map[string]any{"error": p})
}

// SSEEvent is one event of a text/event-stream body.
type SSEEvent struct{ ID, Event, Data string }

// ParseSSE splits an event-stream body into events (comments ignored, data lines joined by \n).
func ParseSSE(body []byte) []SSEEvent {
	var out []SSEEvent
	var cur SSEEvent
	var data []string
	flush := func() {
		if len(data) > 0 {
			cur.Data = strings.Join(data, "\n")
			out = append(out, cur)
		}
		cur, data = SSEEvent{}, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case strings.HasPrefix(line, "event:"):
			cur.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "id:"):
			cur.ID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		}
	}
	flush()
	return out
}

// SingleMessage returns the one JSON message of a response that is either application/json or an
// event stream carrying exactly one "message" event (other events ignored).
func SingleMessage(contentType string, body []byte) (json.RawMessage, error) {
	switch MediaType(contentType) {
	case "application/json":
		if !json.Valid(body) {
			return nil, errors.New("invalid JSON body")
		}
		return json.RawMessage(bytes.TrimSpace(body)), nil
	case "text/event-stream":
		var msgs []json.RawMessage
		for _, ev := range ParseSSE(body) {
			if ev.Event != "" && ev.Event != "message" {
				continue
			}
			if !json.Valid([]byte(ev.Data)) {
				return nil, errors.New("invalid JSON in SSE data")
			}
			msgs = append(msgs, json.RawMessage(ev.Data))
		}
		if len(msgs) != 1 {
			return nil, errors.New("expected exactly one message in the event stream")
		}
		return msgs[0], nil
	default:
		return nil, errors.New("unexpected content type " + contentType)
	}
}

// SSEWriter writes Server-Sent Events and flushes each one. Flushing goes through
// http.ResponseController, so middleware that wraps the ResponseWriter (metrics, tracing) only has
// to implement Unwrap.
type SSEWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

// NewSSEWriter sets the stream headers and returns a writer; nil if the connection cannot stream.
func NewSSEWriter(w http.ResponseWriter) *SSEWriter {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return nil
	}
	return &SSEWriter{w: w, rc: rc}
}

// Send writes one event (id and event name optional; data must be a single JSON document).
func (s *SSEWriter) Send(id, event string, data []byte) error {
	var buf bytes.Buffer
	if id != "" {
		buf.WriteString("id: " + id + "\n")
	}
	if event != "" {
		buf.WriteString("event: " + event + "\n")
	}
	buf.WriteString("data: ")
	buf.Write(bytes.TrimSpace(data))
	buf.WriteString("\n\n")
	if _, err := s.w.Write(buf.Bytes()); err != nil {
		return err
	}
	return s.rc.Flush()
}

// Comment writes a keep-alive comment line.
func (s *SSEWriter) Comment(text string) error {
	if _, err := s.w.Write([]byte(": " + text + "\n\n")); err != nil {
		return err
	}
	return s.rc.Flush()
}
