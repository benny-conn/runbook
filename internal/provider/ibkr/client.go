package ibkr

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// httpClient is a thin wrapper around http.Client pre-configured for the
// IB Gateway Client Portal API (localhost, self-signed TLS cert).
type httpClient struct {
	baseURL string
	http    *http.Client
	mu      sync.RWMutex
	conids  map[string]int64 // symbol → conid cache
}

func newHTTPClient(gatewayURL string) *httpClient {
	return &httpClient{
		baseURL: gatewayURL + "/v1/api",
		http: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // IB Gateway uses a self-signed cert
			},
		},
		conids: make(map[string]int64),
	}
}

func (c *httpClient) get(path string, out any) error {
	resp, err := c.http.Get(c.baseURL + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: %s — %s", path, resp.Status, body)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *httpClient) post(path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	resp, err := c.http.Post(c.baseURL+path, "application/json", r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s: %s — %s", path, resp.Status, b)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// startTickle pings the gateway every 55 seconds to keep the session alive.
// Returns immediately; stops when ctx is cancelled.
func (c *httpClient) startTickle(ctx context.Context) {
	go func() {
		t := time.NewTicker(55 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				var out json.RawMessage
				_ = c.post("/tickle", nil, &out)
			}
		}
	}()
}

// lookupConid resolves a ticker symbol to its IBKR contract ID.
// Results are cached in memory for the lifetime of the client.
func (c *httpClient) lookupConid(symbol string) (int64, error) {
	c.mu.RLock()
	if id, ok := c.conids[symbol]; ok {
		c.mu.RUnlock()
		return id, nil
	}
	c.mu.RUnlock()

	var results []struct {
		ConID int64 `json:"conid"`
	}
	if err := c.get(fmt.Sprintf("/iserver/secdef/search?symbol=%s", symbol), &results); err != nil {
		return 0, fmt.Errorf("conid lookup for %s: %w", symbol, err)
	}
	if len(results) == 0 {
		return 0, fmt.Errorf("no IBKR contract found for symbol %s", symbol)
	}

	id := results[0].ConID
	c.mu.Lock()
	c.conids[symbol] = id
	c.mu.Unlock()
	return id, nil
}

// wsURL derives the WebSocket URL from the gateway base URL.
// e.g. "https://localhost:5055" → "wss://localhost:5055/v1/api/ws"
func (c *httpClient) wsURL() string {
	base := c.baseURL
	// Strip the /v1/api path suffix and swap scheme.
	// baseURL = "https://host:port/v1/api"
	if len(base) > 8 && base[:8] == "https://" {
		return "wss://" + base[8:] + "/ws"
	}
	if len(base) > 7 && base[:7] == "http://" {
		return "ws://" + base[7:] + "/ws"
	}
	return base + "/ws"
}
