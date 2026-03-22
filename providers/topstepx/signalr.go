package topstepx

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

const (
	userHubURL   = "wss://rtc.topstepx.com/hubs/user"
	marketHubURL = "wss://rtc.topstepx.com/hubs/market"
)

// SignalR message types.
const (
	signalrInvocation  = 1
	signalrCompletion  = 3
	signalrPing        = 6
	signalrClose       = 7
)

const recordSep = '\x1e'

// signalrMsg represents a SignalR JSON protocol message.
type signalrMsg struct {
	Type         int               `json:"type"`
	Target       string            `json:"target,omitempty"`
	Arguments    []json.RawMessage `json:"arguments,omitempty"`
	InvocationID string            `json:"invocationId,omitempty"`
	Error        string            `json:"error,omitempty"`
	Result       json.RawMessage   `json:"result,omitempty"`
}

// hubConn wraps a WebSocket connection implementing the SignalR JSON protocol.
type hubConn struct {
	conn     *websocket.Conn
	mu       sync.Mutex // guards writes
	nextID   atomic.Int64
	handlers sync.Map // invocationID → chan signalrMsg

	// EventCh receives server-initiated invocations (push events).
	EventCh chan signalrMsg
}

// dialHub connects to a SignalR hub, performs the JSON protocol handshake,
// and starts the read loop and ping loop.
func dialHub(ctx context.Context, hubURL, token string) (*hubConn, error) {
	url := hubURL + "?access_token=" + token

	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("topstepx hub dial %s: %w", hubURL, err)
	}
	conn.SetReadLimit(1 << 20) // 1 MiB

	hc := &hubConn{
		conn:    conn,
		EventCh: make(chan signalrMsg, 512),
	}

	// SignalR handshake: send protocol selection, read server ack.
	handshake := []byte(`{"protocol":"json","version":1}` + string(recordSep))
	if err := conn.Write(ctx, websocket.MessageText, handshake); err != nil {
		conn.Close(websocket.StatusNormalClosure, "")
		return nil, fmt.Errorf("topstepx handshake write: %w", err)
	}

	_, msg, err := conn.Read(ctx)
	if err != nil {
		conn.Close(websocket.StatusNormalClosure, "")
		return nil, fmt.Errorf("topstepx handshake read: %w", err)
	}
	var ack struct {
		Error string `json:"error"`
	}
	cleaned := stripRecordSep(msg)
	if len(cleaned) > 0 {
		if err := json.Unmarshal(cleaned, &ack); err == nil && ack.Error != "" {
			conn.Close(websocket.StatusNormalClosure, "")
			return nil, fmt.Errorf("topstepx handshake error: %s", ack.Error)
		}
	}

	go hc.readLoop(ctx)
	go hc.pingLoop(ctx)

	return hc, nil
}

// Invoke calls a server method and waits for the completion response.
func (hc *hubConn) Invoke(ctx context.Context, method string, args ...any) (signalrMsg, error) {
	id := fmt.Sprintf("%d", hc.nextID.Add(1))
	ch := make(chan signalrMsg, 1)
	hc.handlers.Store(id, ch)
	defer hc.handlers.Delete(id)

	rawArgs := make([]json.RawMessage, len(args))
	for i, a := range args {
		b, err := json.Marshal(a)
		if err != nil {
			return signalrMsg{}, err
		}
		rawArgs[i] = b
	}

	msg := signalrMsg{
		Type:         signalrInvocation,
		Target:       method,
		Arguments:    rawArgs,
		InvocationID: id,
	}
	if err := hc.writeMsg(ctx, msg); err != nil {
		return signalrMsg{}, err
	}

	select {
	case resp := <-ch:
		if resp.Error != "" {
			return resp, fmt.Errorf("topstepx %s: %s", method, resp.Error)
		}
		return resp, nil
	case <-ctx.Done():
		return signalrMsg{}, ctx.Err()
	}
}

// Send calls a server method without expecting a completion response (fire-and-forget).
func (hc *hubConn) Send(ctx context.Context, method string, args ...any) error {
	rawArgs := make([]json.RawMessage, len(args))
	for i, a := range args {
		b, err := json.Marshal(a)
		if err != nil {
			return err
		}
		rawArgs[i] = b
	}

	msg := signalrMsg{
		Type:      signalrInvocation,
		Target:    method,
		Arguments: rawArgs,
	}
	return hc.writeMsg(ctx, msg)
}

func (hc *hubConn) writeMsg(ctx context.Context, msg signalrMsg) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, recordSep)

	hc.mu.Lock()
	defer hc.mu.Unlock()
	return hc.conn.Write(ctx, websocket.MessageText, b)
}

func (hc *hubConn) readLoop(ctx context.Context) {
	for {
		_, raw, err := hc.conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("topstepx hub read error: %v", err)
			}
			return
		}

		for _, chunk := range splitRecordSep(raw) {
			if len(chunk) == 0 {
				continue
			}

			var msg signalrMsg
			if err := json.Unmarshal(chunk, &msg); err != nil {
				continue
			}

			switch msg.Type {
			case signalrPing:
				_ = hc.writeMsg(ctx, signalrMsg{Type: signalrPing})

			case signalrCompletion:
				if msg.InvocationID != "" {
					if val, ok := hc.handlers.Load(msg.InvocationID); ok {
						select {
						case val.(chan signalrMsg) <- msg:
						default:
						}
					}
				}

			case signalrInvocation:
				// Server-initiated push event.
				select {
				case hc.EventCh <- msg:
				default:
					log.Printf("topstepx hub: event channel full, dropping %s", msg.Target)
				}

			case signalrClose:
				log.Printf("topstepx hub: server sent close: %s", msg.Error)
				return
			}
		}
	}
}

func (hc *hubConn) pingLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = hc.writeMsg(ctx, signalrMsg{Type: signalrPing})
		}
	}
}

func (hc *hubConn) Close() {
	hc.conn.Close(websocket.StatusNormalClosure, "done")
}

// stripRecordSep removes trailing 0x1E bytes.
func stripRecordSep(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == recordSep {
		b = b[:len(b)-1]
	}
	return b
}

// splitRecordSep splits a byte slice on the SignalR record separator.
func splitRecordSep(b []byte) [][]byte {
	var result [][]byte
	start := 0
	for i, c := range b {
		if c == recordSep {
			if i > start {
				result = append(result, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		result = append(result, b[start:])
	}
	return result
}
