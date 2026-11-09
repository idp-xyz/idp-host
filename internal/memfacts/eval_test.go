package memfacts

// TestExtractOnTheMemoryDataset runs the real Extractor over idp-cmi's memory evaluation set (tests/eval/dataset_memory.json)
// the way the HOST would: each persona's user messages in order, the facts extracted so far passed back as the known facts.
// It writes the facts it extracted; idp-cmi's TestMemoryDialogues reads that file as the "extracted" representation
// (IDP_EVAL_EXTRACTED=...). Skipped unless a model is given:
//
//	IDP_MEMEVAL_DATASET=../idp-cmi/tests/eval/dataset_memory.json IDP_MEMEVAL_MODEL_URL=http://127.0.0.1:8080/v1 \
//	IDP_MEMEVAL_MODEL=qwen3.8-flash-next-iq3_xxs IDP_MEMEVAL_OUT=/tmp/extracted.json go test ./internal/memfacts -run Dataset -v -timeout 2h

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-host/adapters/providers/openai"
)

func TestExtractOnTheMemoryDataset(t *testing.T) {
	path, url, name, out := os.Getenv("IDP_MEMEVAL_DATASET"), os.Getenv("IDP_MEMEVAL_MODEL_URL"), os.Getenv("IDP_MEMEVAL_MODEL"), os.Getenv("IDP_MEMEVAL_OUT")
	if path == "" || url == "" || name == "" || out == "" {
		t.Skip("set IDP_MEMEVAL_DATASET, IDP_MEMEVAL_MODEL_URL, IDP_MEMEVAL_MODEL and IDP_MEMEVAL_OUT")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ds struct {
		Personas []struct {
			ID  string `json:"id"`
			Raw []struct {
				Day     int    `json:"day"`
				Content string `json:"content"`
			} `json:"raw"`
		} `json:"personas"`
	}
	if err := json.Unmarshal(raw, &ds); err != nil {
		t.Fatal(err)
	}
	prov, err := openai.New(openai.Config{Name: "eval", BaseURL: url, AllowPlainHTTP: true, HTTP: &http.Client{Timeout: 120 * time.Second, Transport: &http.Transport{Proxy: nil}}})
	if err != nil {
		t.Fatal(err)
	}
	ex := &Extractor{Provider: prov, Model: name, MaxFacts: 5, Timeout: 90 * time.Second}
	limit := len(ds.Personas)
	if n, err := strconv.Atoi(os.Getenv("IDP_MEMEVAL_PERSONAS")); err == nil && n > 0 && n < limit {
		limit = n
	}
	type fact struct {
		Day       int    `json:"day"`
		Key       string `json:"key"`
		Value     string `json:"value"`
		Statement string `json:"statement"`
	}
	type persona struct {
		ID    string `json:"id"`
		Facts []fact `json:"facts"`
	}
	results := make([]persona, limit)
	var calls, failures, withFacts int
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := 0; i < limit; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			p := ds.Personas[i]
			known := map[string]string{}
			res := persona{ID: p.ID, Facts: []fact{}}
			for _, r := range p.Raw {
				user, _, _ := strings.Cut(strings.TrimPrefix(r.Content, "The user said: "), "\nThe assistant answered:")
				var ks []Known
				for k, v := range known {
					ks = append(ks, Known{Key: k, Value: v})
				}
				sort.Slice(ks, func(a, b int) bool { return ks[a].Key < ks[b].Key })
				facts, err := ex.Extract(context.Background(), user, ks)
				mu.Lock()
				calls++
				if err != nil {
					failures++
				}
				if len(facts) > 0 {
					withFacts++
				}
				mu.Unlock()
				for _, f := range facts {
					if normalizeForCompare(known[f.Key]) == normalizeForCompare(f.Value) {
						continue // the CMI would answer "unchanged"
					}
					known[f.Key] = f.Value
					res.Facts = append(res.Facts, fact{Day: r.Day, Key: f.Key, Value: f.Value, Statement: f.Statement})
				}
			}
			results[i] = res
		}(i)
	}
	wg.Wait()
	b, _ := json.MarshalIndent(map[string]any{"model": name + os.Getenv("IDP_MEMEVAL_LABEL"), "personas": results}, "", " ")
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d personas, %d extraction calls, %d failed, %d found a fact", limit, calls, failures, withFacts)
	if failures*10 > calls {
		t.Errorf("more than 10%% of the extractions failed (%d of %d)", failures, calls)
	}
}

func normalizeForCompare(v string) string {
	return strings.ToLower(strings.Join(strings.Fields(v), " "))
}
