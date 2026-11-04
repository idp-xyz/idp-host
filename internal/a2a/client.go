package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client delegates to a remote A2A agent (ADR-0001 D30): it reads the agent card once, then sends
// blocking messages. Bearer is the HOST's credential at that peer (configured per peer).
type Client struct {
	HTTP *http.Client
	// Bearer returns the credential for a card URL ("" = none configured).
	Bearer func(cardURL string) string

	mu    sync.Mutex
	cards map[string]*Card
}

// ErrRemote is a remote agent's refusal or failure.
var ErrRemote = errors.New("remote agent")

// Card fetches (and caches) an agent card.
func (c *Client) card(ctx context.Context, cardURL string) (*Card, error) {
	c.mu.Lock()
	if c.cards == nil {
		c.cards = map[string]*Card{}
	}
	if card := c.cards[cardURL]; card != nil {
		c.mu.Unlock()
		return card, nil
	}
	c.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cardURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if b := c.bearer(cardURL); b != "" {
		req.Header.Set("Authorization", "Bearer "+b)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent card unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent card answered HTTP %d", resp.StatusCode)
	}
	var card Card
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&card); err != nil || card.URL == "" {
		return nil, errors.New("agent card is not readable")
	}
	c.mu.Lock()
	c.cards[cardURL] = &card
	c.mu.Unlock()
	return &card, nil
}

func (c *Client) bearer(u string) string {
	if c.Bearer == nil {
		return ""
	}
	return c.Bearer(u)
}

// Result of a blocking send.
type Result struct {
	TaskID string
	State  string
	Text   string
}

// Send delivers one user message to the agent behind cardURL and waits for the task to rest
// (blocking message/send). A task that ends anywhere but completed is ErrRemote with the state.
func (c *Client) Send(ctx context.Context, cardURL, contextID, messageID, text string) (*Result, error) {
	card, err := c.card(ctx, cardURL)
	if err != nil {
		return nil, err
	}
	blocking := true
	params := SendParams{Message: Message{Kind: "message", MessageID: messageID, Role: "user", Parts: []Part{{Kind: "text", Text: text}}, ContextID: contextID}}
	params.Configuration = &struct {
		Blocking      *bool `json:"blocking"`
		HistoryLength *int  `json:"historyLength"`
	}{Blocking: &blocking}
	raw, _ := json.Marshal(params)
	body, _ := json.Marshal(Request{JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Method: "message/send", Params: raw})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, card.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if b := c.bearer(cardURL); b != "" {
		req.Header.Set("Authorization", "Bearer "+b)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w unreachable: %v", ErrRemote, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w answered HTTP %d", ErrRemote, resp.StatusCode)
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%w answered no JSON-RPC response", ErrRemote)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("%w refused: %d %s", ErrRemote, out.Error.Code, out.Error.Message)
	}
	var task Task
	if err := json.Unmarshal(out.Result, &task); err != nil || task.Kind != "task" {
		// a bare message answer (agents may answer without a task)
		var msg Message
		if err2 := json.Unmarshal(out.Result, &msg); err2 == nil && msg.Kind == "message" {
			return &Result{State: StateCompleted, Text: msg.Text()}, nil
		}
		return nil, fmt.Errorf("%w answered neither a task nor a message", ErrRemote)
	}
	res := &Result{TaskID: task.ID, State: task.Status.State}
	var sb strings.Builder
	for _, a := range task.Artifacts {
		for _, p := range a.Parts {
			if p.Kind == "text" {
				sb.WriteString(p.Text)
			}
		}
	}
	res.Text = sb.String()
	if res.Text == "" && task.Status.Message != nil {
		res.Text = task.Status.Message.Text()
	}
	if task.Status.State != StateCompleted {
		return res, fmt.Errorf("%w task %s ended %s: %s", ErrRemote, task.ID, task.Status.State, res.Text)
	}
	return res, nil
}

// Deadline bounds a remote delegation: the remaining run time, at most ten minutes.
func Deadline(now, runDeadline time.Time) time.Duration {
	d := 10 * time.Minute
	if !runDeadline.IsZero() && runDeadline.Sub(now) < d {
		d = runDeadline.Sub(now)
	}
	if d < time.Second {
		d = time.Second
	}
	return d
}
