package topstepx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// startRenewal re-authenticates every 23 hours (tokens expire after ~24h).
func (a *authClient) startRenewal(ctx context.Context) {
	go func() {
		t := time.NewTicker(23 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := a.authenticate(); err != nil {
					fmt.Printf("topstepx: token renewal failed: %v\n", err)
				}
			}
		}
	}()
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
