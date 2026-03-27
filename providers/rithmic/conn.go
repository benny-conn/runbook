package rithmic

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// conn is a single authenticated WebSocket connection to one Rithmic plant.
type conn struct {
	ws  *websocket.Conn
	mu  sync.Mutex // serialises writes

	fcmID  string
	ibID   string
	hbSecs float64
}

// dial opens a WebSocket to the given URI and logs in.
func dial(ctx context.Context, uri, user, password, appName, appVersion, systemName string, infraType int32) (*conn, error) {
	ws, _, err := websocket.Dial(ctx, uri, nil)
	if err != nil {
		return nil, fmt.Errorf("rithmic dial %s: %w", uri, err)
	}
	ws.SetReadLimit(1 << 20) // 1 MiB

	c := &conn{ws: ws}

	// Send login
	loginBuf := buildRequestLogin(user, password, appName, appVersion, systemName, infraType)
	if err := c.send(ctx, loginBuf); err != nil {
		ws.Close(websocket.StatusNormalClosure, "login failed")
		return nil, fmt.Errorf("rithmic send login: %w", err)
	}

	// Read login response
	msg, err := c.recv(ctx)
	if err != nil {
		ws.Close(websocket.StatusNormalClosure, "no login response")
		return nil, fmt.Errorf("rithmic login response: %w", err)
	}

	resp, err := decodeMsg(msg)
	if err != nil {
		ws.Close(websocket.StatusNormalClosure, "bad login response")
		return nil, fmt.Errorf("rithmic decode login: %w", err)
	}

	if resp.TemplateID != tplResponseLogin {
		ws.Close(websocket.StatusNormalClosure, "unexpected response")
		return nil, fmt.Errorf("rithmic: expected login response (11), got template %d", resp.TemplateID)
	}

	if len(resp.RpCode) > 0 && resp.RpCode[0] != "0" {
		ws.Close(websocket.StatusNormalClosure, "login rejected")
		userMsg := ""
		if len(resp.UserMsg) > 0 {
			userMsg = resp.UserMsg[0]
		}
		return nil, fmt.Errorf("rithmic login failed: rp_code=%v msg=%s", resp.RpCode, userMsg)
	}

	c.fcmID = resp.FcmID
	c.ibID = resp.IbID
	c.hbSecs = resp.HeartbeatInterval
	if c.hbSecs == 0 {
		c.hbSecs = 30
	}

	log.Printf("rithmic: logged in to %s (infra=%d) fcm=%s ib=%s hb=%.0fs",
		systemName, infraType, c.fcmID, c.ibID, c.hbSecs)

	return c, nil
}

func (c *conn) send(ctx context.Context, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.Write(ctx, websocket.MessageBinary, data)
}

func (c *conn) recv(ctx context.Context) ([]byte, error) {
	_, data, err := c.ws.Read(ctx)
	return data, err
}

func (c *conn) close() {
	c.ws.Close(websocket.StatusNormalClosure, "goodbye")
}

// startHeartbeat sends periodic heartbeats. Blocks until ctx is cancelled.
func (c *conn) startHeartbeat(ctx context.Context) {
	interval := time.Duration(c.hbSecs) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	// Send at slightly less than the server interval to stay ahead
	interval = interval * 8 / 10

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.send(ctx, buildRequestHeartbeat()); err != nil {
				log.Printf("rithmic: heartbeat send failed: %v", err)
				return
			}
		}
	}
}

// readLoop reads messages off the wire and sends them to the provided channel.
// Blocks until the connection is closed or ctx is cancelled.
func (c *conn) readLoop(ctx context.Context, ch chan<- []byte) {
	for {
		data, err := c.recv(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("rithmic: read error: %v", err)
			return
		}

		tpl := templateID(data)
		// Silently consume heartbeat responses
		if tpl == tplResponseHeartbeat {
			continue
		}

		select {
		case ch <- data:
		case <-ctx.Done():
			return
		}
	}
}

// sendAndRecv sends a request and waits for a single response matching the
// expected template ID. Other messages received are discarded.
func (c *conn) sendAndRecv(ctx context.Context, req []byte, expectTpl int32) (*protoMsg, error) {
	if err := c.send(ctx, req); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	for {
		data, err := c.recv(ctx)
		if err != nil {
			return nil, err
		}
		tpl := templateID(data)
		if tpl == tplResponseHeartbeat {
			continue
		}
		if tpl == expectTpl {
			return decodeMsg(data)
		}
		// Not the one we want — keep reading
	}
}

// sendAndRecvAll sends a request and collects all responses matching the
// expected template ID until a final message with rp_code (no rq_handler_rp_code).
func (c *conn) sendAndRecvAll(ctx context.Context, req []byte, expectTpl int32) ([]*protoMsg, error) {
	if err := c.send(ctx, req); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var results []*protoMsg
	for {
		data, err := c.recv(ctx)
		if err != nil {
			return results, err
		}
		tpl := templateID(data)
		if tpl == tplResponseHeartbeat {
			continue
		}
		if tpl != expectTpl {
			continue
		}
		msg, err := decodeMsg(data)
		if err != nil {
			return results, err
		}

		// A message with rq_handler_rp_code is a data row; one without is the terminator.
		if len(msg.RqHandlerRpCode) == 0 && len(msg.RpCode) > 0 {
			// End marker
			return results, nil
		}
		results = append(results, msg)
	}
}
