package tradovate

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

type tokenState struct {
	accessToken   string
	mdAccessToken string
	expiresAt     time.Time
	userID        int64
}

type authClient struct {
	baseURL    string
	http       *http.Client
	mu         sync.RWMutex
	state      *tokenState
	creds      authCreds
}

type authCreds struct {
	username   string
	password   string
	appID      string
	appVersion string
	deviceID   string
	cid        string
	sec        string
}

func newAuthClient(demo bool, creds authCreds) *authClient {
	base := "https://live.tradovateapi.com/v1"
	if demo {
		base = "https://demo.tradovateapi.com/v1"
	}
	if creds.appVersion == "" {
		creds.appVersion = "1.0"
	}
	return &authClient{
		baseURL: base,
		http:    &http.Client{Timeout: 15 * time.Second},
		creds:   creds,
	}
}

func (a *authClient) authenticate() error {
	body, _ := json.Marshal(map[string]string{
		"name":       a.creds.username,
		"password":   a.creds.password,
		"appId":      a.creds.appID,
		"appVersion": a.creds.appVersion,
		"deviceId":   a.creds.deviceID,
		"cid":        a.creds.cid,
		"sec":        a.creds.sec,
	})

	resp, err := a.http.Post(a.baseURL+"/auth/accesstokenrequest", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("tradovate auth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("tradovate auth %s: %s", resp.Status, b)
	}

	var r struct {
		AccessToken    string `json:"accessToken"`
		MdAccessToken  string `json:"mdAccessToken"`
		ExpirationTime string `json:"expirationTime"`
		UserID         int64  `json:"userId"`
		UserStatus     string `json:"userStatus"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("tradovate auth decode: %w", err)
	}
	if r.AccessToken == "" {
		return fmt.Errorf("tradovate auth: empty access token — check credentials")
	}

	exp, _ := time.Parse(time.RFC3339, r.ExpirationTime)
	a.mu.Lock()
	a.state = &tokenState{
		accessToken:   r.AccessToken,
		mdAccessToken: r.MdAccessToken,
		expiresAt:     exp,
		userID:        r.UserID,
	}
	a.mu.Unlock()
	return nil
}

func (a *authClient) renew() error {
	a.mu.RLock()
	tok := ""
	if a.state != nil {
		tok = a.state.accessToken
	}
	a.mu.RUnlock()

	req, _ := http.NewRequest("GET", a.baseURL+"/auth/renewaccesstoken", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var r struct {
		AccessToken    string `json:"accessToken"`
		MdAccessToken  string `json:"mdAccessToken"`
		ExpirationTime string `json:"expirationTime"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return err
	}

	exp, _ := time.Parse(time.RFC3339, r.ExpirationTime)
	a.mu.Lock()
	a.state.accessToken = r.AccessToken
	a.state.mdAccessToken = r.MdAccessToken
	a.state.expiresAt = exp
	a.mu.Unlock()
	return nil
}

// startRenewal renews tokens every 85 minutes (tokens expire after 90).
func (a *authClient) startRenewal(ctx context.Context) {
	go func() {
		t := time.NewTicker(85 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := a.renew(); err != nil {
					_ = a.authenticate() // fall back to full re-auth
				}
			}
		}
	}()
}

func (a *authClient) tokens() (access, md string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.state == nil {
		return "", ""
	}
	return a.state.accessToken, a.state.mdAccessToken
}

func (a *authClient) userID() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.state == nil {
		return 0
	}
	return a.state.userID
}
