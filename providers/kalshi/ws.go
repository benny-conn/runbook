package kalshi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// wsConn manages a single WebSocket connection to Kalshi with channel
// subscriptions and automatic reconnection.
type wsConn struct {
	provider *Provider
	url      string

	mu       sync.Mutex
	handlers map[string]func(wsMessage) // channel type → handler
	symbols  []string
	cmdID    atomic.Int64
}

func newWSConn(p *Provider) *wsConn {
	base := p.baseURL
	// Convert REST base to WS URL.
	wsURL := "wss://api.elections.kalshi.com/trade-api/ws/v2"
	if base == demoBaseURL {
		wsURL = "wss://demo.kalshi.com/trade-api/ws/v2"
	}
	return &wsConn{
		provider: p,
		url:      wsURL,
		handlers: make(map[string]func(wsMessage)),
	}
}

func (w *wsConn) subscribe(channel string, symbols []string, handler func(wsMessage)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handlers[channel] = handler
	// Merge symbols (dedup).
	seen := make(map[string]bool, len(w.symbols)+len(symbols))
	for _, s := range w.symbols {
		seen[s] = true
	}
	for _, s := range symbols {
		if !seen[s] {
			w.symbols = append(w.symbols, s)
			seen[s] = true
		}
	}
}

// run connects and reads messages until ctx is cancelled. Reconnects on error.
func (w *wsConn) run(ctx context.Context) error {
	for {
		if err := w.connectAndRead(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("kalshi ws: connection error: %v — reconnecting in 5s", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (w *wsConn) connectAndRead(ctx context.Context) error {
	// Build auth query params.
	ts := time.Now().UnixMilli()
	sig, err := w.provider.sign(ts, "GET", "/trade-api/ws/v2")
	if err != nil {
		return fmt.Errorf("signing ws auth: %w", err)
	}

	dialURL := fmt.Sprintf("%s?api-key=%s&timestamp=%d&signature=%s",
		w.url, w.provider.apiKey, ts, sig)

	conn, _, err := websocket.Dial(ctx, dialURL, nil)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer conn.CloseNow()

	// Send subscriptions.
	w.mu.Lock()
	channels := make([]string, 0, len(w.handlers))
	for ch := range w.handlers {
		channels = append(channels, ch)
	}
	symbols := make([]string, len(w.symbols))
	copy(symbols, w.symbols)
	w.mu.Unlock()

	if len(channels) > 0 {
		id := int(w.cmdID.Add(1))
		cmd := wsCommand{
			ID:  id,
			Cmd: "subscribe",
			Params: wsSubscribeParams{
				Channels:      channels,
				MarketTickers: symbols,
			},
		}
		data, _ := json.Marshal(cmd)
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			return fmt.Errorf("ws subscribe: %w", err)
		}
		log.Printf("kalshi ws: subscribed to %v for %v", channels, symbols)
	}

	// Read loop.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("ws read: %w", err)
		}

		var msg wsMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("kalshi ws: unmarshal error: %v", err)
			continue
		}

		w.mu.Lock()
		handler, ok := w.handlers[msg.Type]
		w.mu.Unlock()
		if ok {
			handler(msg)
		}
	}
}
