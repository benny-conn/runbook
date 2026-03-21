// Package tradovate implements the provider interfaces using the Tradovate Partner API.
// WebSocket protocol details: https://partner.tradovate.com/overview/core-concepts/web-sockets
package tradovate

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

// wsFrame is a parsed inbound Tradovate WebSocket message.
// Tradovate wraps responses in 'a[...]' frames; each element looks like:
//
//	{"i": requestId, "s": statusCode, "d": data}
type wsFrame struct {
	RequestID int             `json:"i"`
	Status    int             `json:"s"`
	Data      json.RawMessage `json:"d"`
}

// wsConn is a Tradovate WebSocket connection that handles:
//   - Tradovate's custom line-delimited text framing
//   - Bearer-token authentication handshake on connect
//   - Heartbeat ([] every 2.5s, required or server closes connection)
//   - Request/response correlation by request ID
//   - Push events (fills, market data updates) routed to eventCh
type wsConn struct {
	conn      *websocket.Conn
	mu        sync.Mutex     // guards conn.Write
	nextReqID atomic.Int64
	handlers  map[int64]chan wsFrame
	handlerMu sync.RWMutex
	EventCh   chan wsFrame // receives unrequested push events
}

// dialTradovate connects to a Tradovate WebSocket endpoint and authenticates.
// token should be accessToken for the trading socket or mdAccessToken for market data.
func dialTradovate(ctx context.Context, url, token string) (*wsConn, error) {
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("tradovate ws dial %s: %w", url, err)
	}

	wc := &wsConn{
		conn:     conn,
		handlers: make(map[int64]chan wsFrame),
		EventCh:  make(chan wsFrame, 512),
	}

	// Server sends 'o' (open) immediately on connect.
	if err := wc.expectOpen(ctx); err != nil {
		conn.Close(websocket.StatusNormalClosure, "")
		return nil, err
	}

	// Authenticate with our bearer token.
	if err := wc.authorize(ctx, token); err != nil {
		conn.Close(websocket.StatusNormalClosure, "")
		return nil, err
	}

	go wc.readLoop(ctx)
	go wc.heartbeat(ctx)

	return wc, nil
}

func (wc *wsConn) expectOpen(ctx context.Context) error {
	_, msg, err := wc.conn.Read(ctx)
	if err != nil {
		return err
	}
	if len(msg) == 0 || msg[0] != 'o' {
		return fmt.Errorf("tradovate ws: expected 'o' open frame, got %q", msg)
	}
	return nil
}

func (wc *wsConn) authorize(ctx context.Context, token string) error {
	reqID := wc.nextReqID.Add(1)

	frame := fmt.Sprintf("authorize\n%d\n\n%s", reqID, token)
	wc.mu.Lock()
	err := wc.conn.Write(ctx, websocket.MessageText, []byte(frame))
	wc.mu.Unlock()
	if err != nil {
		return fmt.Errorf("tradovate ws authorize write: %w", err)
	}

	// readLoop hasn't started yet — read directly until we get our response.
	for {
		_, msg, err := wc.conn.Read(ctx)
		if err != nil {
			return err
		}
		s := string(msg)
		if len(s) == 0 || s[0] == 'h' {
			continue // server heartbeat, skip
		}
		if s[0] != 'a' {
			continue
		}
		var frames []wsFrame
		if err := json.Unmarshal([]byte(s[1:]), &frames); err != nil {
			continue
		}
		for _, f := range frames {
			if int64(f.RequestID) == reqID {
				if f.Status != 200 {
					return fmt.Errorf("tradovate ws authorize failed: status %d", f.Status)
				}
				return nil
			}
		}
	}
}

// Send writes a request and returns a channel for the response and the request ID.
// Caller is responsible for calling removeHandler(reqID) when done.
func (wc *wsConn) Send(ctx context.Context, endpoint string, body any) (chan wsFrame, int64, error) {
	reqID := wc.nextReqID.Add(1)
	ch := wc.registerHandler(reqID)

	var bodyStr string
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			wc.removeHandler(reqID)
			return nil, 0, err
		}
		bodyStr = string(b)
	}

	frame := fmt.Sprintf("%s\n%d\n\n%s", endpoint, reqID, bodyStr)
	wc.mu.Lock()
	err := wc.conn.Write(ctx, websocket.MessageText, []byte(frame))
	wc.mu.Unlock()
	if err != nil {
		wc.removeHandler(reqID)
		return nil, 0, err
	}
	return ch, reqID, nil
}

// Request sends a request and waits for a response, unmarshaling d into out.
func (wc *wsConn) Request(ctx context.Context, endpoint string, body, out any) error {
	ch, reqID, err := wc.Send(ctx, endpoint, body)
	if err != nil {
		return err
	}
	defer wc.removeHandler(reqID)

	select {
	case f := <-ch:
		if f.Status != 200 {
			return fmt.Errorf("tradovate %s: status %d", endpoint, f.Status)
		}
		if out != nil && f.Data != nil {
			return json.Unmarshal(f.Data, out)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(15 * time.Second):
		return fmt.Errorf("tradovate %s: timeout", endpoint)
	}
}

func (wc *wsConn) readLoop(ctx context.Context) {
	for {
		_, msg, err := wc.conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("tradovate ws read error: %v", err)
			}
			return
		}
		s := string(msg)
		if len(s) == 0 || s[0] != 'a' {
			continue // skip 'h' heartbeats and 'c' close
		}
		var frames []wsFrame
		if err := json.Unmarshal([]byte(s[1:]), &frames); err != nil {
			continue
		}
		for _, f := range frames {
			wc.dispatch(f)
		}
	}
}

func (wc *wsConn) dispatch(f wsFrame) {
	wc.handlerMu.RLock()
	ch, ok := wc.handlers[int64(f.RequestID)]
	wc.handlerMu.RUnlock()

	if ok {
		select {
		case ch <- f:
		default:
		}
		return
	}

	// No handler registered — it's a push event.
	select {
	case wc.EventCh <- f:
	default:
		log.Printf("tradovate ws: event channel full, dropping push event")
	}
}

// heartbeat sends [] every 2.5 seconds as required by Tradovate.
func (wc *wsConn) heartbeat(ctx context.Context) {
	t := time.NewTicker(2500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			wc.mu.Lock()
			_ = wc.conn.Write(ctx, websocket.MessageText, []byte("[]"))
			wc.mu.Unlock()
		}
	}
}

func (wc *wsConn) Close() {
	wc.conn.Close(websocket.StatusNormalClosure, "done")
}

func (wc *wsConn) registerHandler(id int64) chan wsFrame {
	ch := make(chan wsFrame, 1)
	wc.handlerMu.Lock()
	wc.handlers[id] = ch
	wc.handlerMu.Unlock()
	return ch
}

func (wc *wsConn) removeHandler(id int64) {
	wc.handlerMu.Lock()
	delete(wc.handlers, id)
	wc.handlerMu.Unlock()
}
