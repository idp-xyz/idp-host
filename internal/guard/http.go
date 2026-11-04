package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTPClassifier calls an external classification endpoint: POST {"text": ...} with an optional
// bearer key; the answer is {"allow": bool, "categories": [...]}. Timeouts, non-200 answers and
// malformed bodies are ErrClassifier.
type HTTPClassifier struct {
	Name     string
	Endpoint string
	APIKey   string
	HTTP     *http.Client
	Timeout  time.Duration // default 5 s
	MaxText  int           // bytes of text sent (default 64 KiB; longer text is cut)
}

// Classify implements Classifier.
func (c *HTTPClassifier) Classify(ctx context.Context, text string) (Verdict, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	limit := c.MaxText
	if limit == 0 {
		limit = 64 << 10
	}
	if len(text) > limit {
		text = text[:limit]
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Verdict{}, fmt.Errorf("%w: %v", ErrClassifier, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Transport: &http.Transport{Proxy: nil}}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Verdict{}, fmt.Errorf("%w: %s unreachable", ErrClassifier, c.Name)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return Verdict{}, fmt.Errorf("%w: %s answered HTTP %d", ErrClassifier, c.Name, resp.StatusCode)
	}
	var v Verdict
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return Verdict{}, fmt.Errorf("%w: %s answered an unexpected body", ErrClassifier, c.Name)
	}
	return v, nil
}
