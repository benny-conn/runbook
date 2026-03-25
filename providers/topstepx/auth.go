package topstepx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

const apiBaseURL = "https://api.topstepx.com/api"

type authClient struct {
	http  *http.Client
	creds authCreds

	mu    sync.RWMutex
	token string

	// Callbacks notified on successful token refresh.
	listenersMu sync.Mutex
	listeners   []func()
}

type authCreds struct {
	username string
	apiKey   string
}

func newAuthClient(creds authCreds) *authClient {
	return &authClient{
		http:  &http.Client{Timeout: 15 * time.Second},
		creds: creds,
	}
}

func (a *authClient) authenticate() error {
	body, _ := json.Marshal(map[string]string{
		"userName": a.creds.username,
		"apiKey":   a.creds.apiKey,
	})

	resp, err := a.http.Post(apiBaseURL+"/Auth/loginKey", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("topstepx auth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("topstepx auth %s: %s", resp.Status, b)
	}

	var r struct {
		Token        string `json:"token"`
		Success      bool   `json:"success"`
		ErrorCode    int    `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("topstepx auth decode: %w", err)
	}
	if !r.Success || r.Token == "" {
		return fmt.Errorf("topstepx auth failed: code=%d msg=%s", r.ErrorCode, r.ErrorMessage)
	}

	a.mu.Lock()
	a.token = r.Token
	a.mu.Unlock()
	return nil
}

// onRefresh registers a callback invoked after each successful token renewal.
// Used by managedHub to force reconnection with the new token.
func (a *authClient) onRefresh(fn func()) {
	a.listenersMu.Lock()
	defer a.listenersMu.Unlock()
	a.listeners = append(a.listeners, fn)
}

func (a *authClient) notifyRefresh() {
	a.listenersMu.Lock()
	fns := make([]func(), len(a.listeners))
	copy(fns, a.listeners)
	a.listenersMu.Unlock()

	for _, fn := range fns {
		fn()
	}
}

// startRenewal re-authenticates every 20 hours (tokens expire after ~24h).
// On failure, retries with backoff up to 5 times before logging a critical error.
func (a *authClient) startRenewal(ctx context.Context) {
	go func() {
		t := time.NewTicker(20 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.renewWithRetry(ctx)
			}
		}
	}()
}

func (a *authClient) renewWithRetry(ctx context.Context) {
	backoffs := []time.Duration{30 * time.Second, 60 * time.Second, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}

	for attempt := 0; attempt <= len(backoffs); attempt++ {
		if err := a.authenticate(); err != nil {
			if attempt < len(backoffs) {
				log.Printf("topstepx: token renewal failed (attempt %d/%d): %v — retrying in %s",
					attempt+1, len(backoffs)+1, err, backoffs[attempt])
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoffs[attempt]):
				}
				continue
			}
			log.Printf("topstepx: CRITICAL — token renewal exhausted all retries: %v", err)
			return
		}

		log.Println("topstepx: token renewed successfully")
		a.notifyRefresh()
		return
	}
}

func (a *authClient) getToken() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.token
}

// doPost makes an authenticated POST request to the TopstepX REST API.
func (a *authClient) doPost(ctx context.Context, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("topstepx marshal: %w", err)
		}
		reqBody = bytes.NewReader(b)
	} else {
		reqBody = bytes.NewReader([]byte("{}"))
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiBaseURL+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.getToken())

	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("topstepx %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("topstepx %s read: %w", path, err)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("topstepx %s %s: %s", path, resp.Status, respBody)
	}

	if out != nil {
		return json.Unmarshal(respBody, out)
	}
	return nil
}
