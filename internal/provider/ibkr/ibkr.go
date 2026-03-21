// Package ibkr implements provider.MarketData and provider.Execution using the
// IBKR Client Portal Gateway API (https://www.interactivebrokers.com/cpwebapi/).
//
// Prerequisites:
//   - Download and run IB Gateway (paper account mode) from https://www.interactivebrokers.com/en/trading/ibgateway.php
//   - Authenticate once via browser at https://localhost:5055
//   - Set IBKR_ACCOUNT_ID and optionally IBKR_GATEWAY_URL (defaults to https://localhost:5055)
//
// The gateway must remain running while the bot is active. This package sends
// a periodic /tickle to keep the session alive.
//
// Note on order confirmation: IBKR's Client Portal API may return a confirmation
// prompt before placing certain orders. This package automatically confirms.
package ibkr

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"brandon-bot/internal/provider"
	"brandon-bot/internal/strategy"
)

const defaultGatewayURL = "https://localhost:5055"

// Provider implements provider.MarketData and provider.Execution via IBKR Client Portal.
type Provider struct {
	client    *httpClient
	accountID string

	// Tracks known order states to detect transitions into Filled / PartialFill.
	fillMu  sync.Mutex
	fillSeen map[string]orderSnapshot
}

type orderSnapshot struct {
	status  string
	filled  float64
}

// New creates an IBKR provider from environment variables.
// Required: IBKR_ACCOUNT_ID
// Optional: IBKR_GATEWAY_URL (default https://localhost:5055)
func New() *Provider {
	gatewayURL := os.Getenv("IBKR_GATEWAY_URL")
	if gatewayURL == "" {
		gatewayURL = defaultGatewayURL
	}
	return &Provider{
		client:    newHTTPClient(gatewayURL),
		accountID: os.Getenv("IBKR_ACCOUNT_ID"),
		fillSeen:  make(map[string]orderSnapshot),
	}
}

// — MarketData —

func (p *Provider) FetchBars(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]provider.Bar, error) {
	conid, err := p.client.lookupConid(symbol)
	if err != nil {
		return nil, err
	}

	barSize, period := ibkrBarParams(timeframe, start, end)
	startStr := start.UTC().Format("20060102-15:04:05")

	var resp struct {
		Data []struct {
			T int64   `json:"t"` // epoch milliseconds
			O float64 `json:"o"`
			C float64 `json:"c"`
			H float64 `json:"h"`
			L float64 `json:"l"`
			V float64 `json:"v"`
		} `json:"data"`
	}

	path := fmt.Sprintf(
		"/iserver/marketdata/history?conid=%d&period=%s&bar=%s&startTime=%s&outsideRth=1",
		conid, period, barSize, startStr,
	)
	if err := p.client.get(path, &resp); err != nil {
		return nil, fmt.Errorf("IBKR historical bars for %s: %w", symbol, err)
	}

	bars := make([]provider.Bar, 0, len(resp.Data))
	for _, d := range resp.Data {
		ts := time.Unix(d.T/1000, (d.T%1000)*int64(time.Millisecond)).UTC()
		if ts.Before(start) || ts.After(end) {
			continue
		}
		bars = append(bars, provider.Bar{
			Symbol:    symbol,
			Timestamp: ts,
			Open:      d.O,
			High:      d.H,
			Low:       d.L,
			Close:     d.C,
			Volume:    d.V,
		})
	}
	return bars, nil
}

func (p *Provider) FetchBarsMulti(ctx context.Context, symbols []string, timeframe string, start, end time.Time) ([]provider.Bar, error) {
	type result struct {
		bars []provider.Bar
		err  error
	}
	ch := make(chan result, len(symbols))
	for _, sym := range symbols {
		go func(s string) {
			bars, err := p.FetchBars(ctx, s, timeframe, start, end)
			ch <- result{bars, err}
		}(sym)
	}
	var all []provider.Bar
	for range symbols {
		r := <-ch
		if r.err != nil {
			return nil, r.err
		}
		all = append(all, r.bars...)
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].Timestamp.Before(all[j].Timestamp)
	})
	return all, nil
}

// SubscribeBars streams live OHLCV bars via the IBKR Client Portal WebSocket.
// Bars that pre-date the subscription time (initial historical backfill) are skipped.
func (p *Provider) SubscribeBars(ctx context.Context, symbols []string, timeframe string, handler func(provider.Bar)) error {
	p.client.startTickle(ctx)

	conids := make(map[int64]string) // conid → symbol
	for _, sym := range symbols {
		id, err := p.client.lookupConid(sym)
		if err != nil {
			return err
		}
		conids[id] = sym
	}

	conn, _, err := websocket.Dial(ctx, p.client.wsURL(), &websocket.DialOptions{
		HTTPClient: p.client.http,
	})
	if err != nil {
		return fmt.Errorf("IBKR websocket: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	barSize, _ := ibkrBarParams(timeframe, time.Time{}, time.Time{})
	subscribeAt := time.Now()

	for conid := range conids {
		sub := fmt.Sprintf(
			`sbd+%d+{"period":"today","bar":"%s","source":"t","format":"%%o/%%c/%%h/%%l/%%v","outsideRth":true}`,
			conid, barSize,
		)
		if err := conn.Write(ctx, websocket.MessageText, []byte(sub)); err != nil {
			return fmt.Errorf("IBKR bar subscribe: %w", err)
		}
	}

	log.Printf("ibkr: subscribed to bars for %v (timeframe=%s)", symbols, timeframe)

	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("IBKR websocket read: %w", err)
		}

		var envelope struct {
			Topic string `json:"topic"`
			Data  []struct {
				T int64   `json:"t"`
				O float64 `json:"o"`
				C float64 `json:"c"`
				H float64 `json:"h"`
				L float64 `json:"l"`
				V float64 `json:"v"`
			} `json:"data"`
		}
		if err := json.Unmarshal(msg, &envelope); err != nil {
			continue
		}

		var matchedConid int64
		for conid := range conids {
			prefix := fmt.Sprintf("sbd+%d", conid)
			if strings.HasPrefix(envelope.Topic, prefix) {
				matchedConid = conid
				break
			}
		}
		if matchedConid == 0 {
			continue
		}
		sym := conids[matchedConid]

		for _, d := range envelope.Data {
			ts := time.Unix(d.T/1000, (d.T%1000)*int64(time.Millisecond)).UTC()
			// Skip historical backfill bars delivered on initial subscription.
			if ts.Before(subscribeAt) {
				continue
			}
			handler(provider.Bar{
				Symbol:    sym,
				Timestamp: ts,
				Open:      d.O,
				High:      d.H,
				Low:       d.L,
				Close:     d.C,
				Volume:    d.V,
			})
		}
	}
}

// SubscribeTrades streams real-time trade prints via the IBKR Client Portal WebSocket.
// IBKR field 31 = last price, field 7295 = last size.
func (p *Provider) SubscribeTrades(ctx context.Context, symbols []string, handler func(provider.Trade)) error {
	conids := make(map[int64]string)
	for _, sym := range symbols {
		id, err := p.client.lookupConid(sym)
		if err != nil {
			return err
		}
		conids[id] = sym
	}

	conn, _, err := websocket.Dial(ctx, p.client.wsURL(), &websocket.DialOptions{
		HTTPClient: p.client.http,
	})
	if err != nil {
		return fmt.Errorf("IBKR websocket: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	// Field 31 = last price, 7295 = last size.
	for conid := range conids {
		sub := fmt.Sprintf(`smd+%d+{"fields":["31","7295"]}`, conid)
		if err := conn.Write(ctx, websocket.MessageText, []byte(sub)); err != nil {
			return fmt.Errorf("IBKR trade subscribe: %w", err)
		}
	}

	log.Printf("ibkr: subscribed to trades for %v", symbols)

	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("IBKR websocket read: %w", err)
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(msg, &raw); err != nil {
			continue
		}

		topicRaw, ok := raw["topic"]
		if !ok {
			continue
		}
		var topic string
		if err := json.Unmarshal(topicRaw, &topic); err != nil {
			continue
		}

		var matchedConid int64
		for conid := range conids {
			if topic == fmt.Sprintf("smd+%d", conid) {
				matchedConid = conid
				break
			}
		}
		if matchedConid == 0 {
			continue
		}

		lastPriceRaw, hasPrice := raw["31"]
		if !hasPrice {
			continue
		}
		var priceStr string
		if err := json.Unmarshal(lastPriceRaw, &priceStr); err != nil {
			continue
		}
		price, err := strconv.ParseFloat(priceStr, 64)
		if err != nil {
			continue
		}

		var size float64
		if sizeRaw, ok := raw["7295"]; ok {
			var sizeStr string
			if err := json.Unmarshal(sizeRaw, &sizeStr); err == nil {
				size, _ = strconv.ParseFloat(sizeStr, 64)
			}
		}

		handler(provider.Trade{
			Symbol:    conids[matchedConid],
			Timestamp: time.Now().UTC(),
			Price:     price,
			Size:      size,
		})
	}
}

// — Execution —

func (p *Provider) GetAccount(ctx context.Context) (provider.Account, error) {
	var summary map[string]struct {
		Amount float64 `json:"amount"`
	}
	if err := p.client.get(fmt.Sprintf("/portfolio/%s/summary", p.accountID), &summary); err != nil {
		return provider.Account{}, fmt.Errorf("IBKR account summary: %w", err)
	}
	var acct provider.Account
	if v, ok := summary["totalcashvalue"]; ok {
		acct.Cash = v.Amount
	}
	if v, ok := summary["netliquidation"]; ok {
		acct.Equity = v.Amount
	}
	return acct, nil
}

func (p *Provider) GetPositions(ctx context.Context) ([]provider.Position, error) {
	var raw []struct {
		Ticker   string  `json:"ticker"`
		Position float64 `json:"position"`
		AvgCost  float64 `json:"avgCost"`
		MktPrice float64 `json:"mktPrice"`
	}
	if err := p.client.get(fmt.Sprintf("/portfolio/%s/positions/0", p.accountID), &raw); err != nil {
		return nil, fmt.Errorf("IBKR positions: %w", err)
	}
	positions := make([]provider.Position, 0, len(raw))
	for _, r := range raw {
		if r.Position == 0 {
			continue
		}
		positions = append(positions, provider.Position{
			Symbol:        r.Ticker,
			Qty:           r.Position,
			AvgEntryPrice: r.AvgCost,
			CurrentPrice:  r.MktPrice,
		})
	}
	return positions, nil
}

// PlaceOrder submits an order via IBKR Client Portal.
// IBKR may return a confirmation prompt; this is handled automatically.
func (p *Provider) PlaceOrder(ctx context.Context, order strategy.Order) (provider.OrderResult, error) {
	conid, err := p.client.lookupConid(order.Symbol)
	if err != nil {
		return provider.OrderResult{}, err
	}

	side := "BUY"
	if order.Side == "sell" {
		side = "SELL"
	}
	orderType := "MKT"
	tif := "DAY"
	orderBody := map[string]any{
		"conid":     strconv.FormatInt(conid, 10),
		"orderType": orderType,
		"side":      side,
		"quantity":  order.Qty,
		"tif":       tif,
	}
	if order.OrderType == "limit" && order.LimitPrice > 0 {
		orderBody["orderType"] = "LMT"
		orderBody["tif"] = "GTC"
		orderBody["price"] = order.LimitPrice
	}

	req := map[string]any{"orders": []any{orderBody}}

	var rawResp json.RawMessage
	if err := p.client.post(
		fmt.Sprintf("/iserver/account/%s/orders", p.accountID),
		req, &rawResp,
	); err != nil {
		return provider.OrderResult{}, fmt.Errorf("placing order: %w", err)
	}

	return p.resolveOrderResponse(rawResp)
}

// resolveOrderResponse handles both direct order results and IBKR confirmation prompts.
func (p *Provider) resolveOrderResponse(rawResp json.RawMessage) (provider.OrderResult, error) {
	// Try direct result: [{"order_id": "123", ...}]
	var orderResults []struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(rawResp, &orderResults); err == nil &&
		len(orderResults) > 0 && orderResults[0].OrderID != "" {
		return provider.OrderResult{ID: orderResults[0].OrderID}, nil
	}

	// Try confirmation prompt: [{"id": "reply_id", "message": ["..."]}]
	var confirmReqs []struct {
		ID      string   `json:"id"`
		Message []string `json:"message"`
	}
	if err := json.Unmarshal(rawResp, &confirmReqs); err == nil && len(confirmReqs) > 0 {
		log.Printf("ibkr: order requires confirmation — %v", confirmReqs[0].Message)
		var confirmed []struct {
			OrderID string `json:"order_id"`
		}
		if err := p.client.post(
			fmt.Sprintf("/iserver/reply/%s", confirmReqs[0].ID),
			map[string]bool{"confirmed": true},
			&confirmed,
		); err != nil {
			return provider.OrderResult{}, fmt.Errorf("confirming order: %w", err)
		}
		if len(confirmed) > 0 {
			return provider.OrderResult{ID: confirmed[0].OrderID}, nil
		}
	}

	return provider.OrderResult{}, fmt.Errorf("unexpected IBKR order response: %s", rawResp)
}

// SubscribeFills polls /iserver/account/orders every 500ms and emits a Fill event
// whenever an order transitions into Filled or its PartialFill quantity increases.
func (p *Provider) SubscribeFills(ctx context.Context, handler func(provider.Fill)) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.pollFills(handler)
		}
	}
}

func (p *Provider) pollFills(handler func(provider.Fill)) {
	// IBKR wraps order list in {"orders": [...]} or returns a raw array.
	var wrapper struct {
		Orders []ibkrOrder `json:"orders"`
	}
	if err := p.client.get("/iserver/account/orders", &wrapper); err != nil {
		log.Printf("ibkr: polling orders: %v", err)
		return
	}

	p.fillMu.Lock()
	defer p.fillMu.Unlock()

	for _, o := range wrapper.Orders {
		prev, known := p.fillSeen[o.OrderID]
		cur := orderSnapshot{status: o.Status, filled: o.FilledQty}

		switch {
		case o.Status == "Filled" && (!known || prev.status != "Filled"):
			handler(provider.Fill{
				OrderID:   o.OrderID,
				Symbol:    o.Ticker,
				Side:      strings.ToLower(o.Side),
				Qty:       o.FilledQty,
				Price:     o.AvgPrice,
				Timestamp: time.Now().UTC(),
				Partial:   false,
			})
		case o.Status == "PartialFill" && o.FilledQty > prev.filled:
			handler(provider.Fill{
				OrderID:   o.OrderID,
				Symbol:    o.Ticker,
				Side:      strings.ToLower(o.Side),
				Qty:       o.FilledQty - prev.filled,
				Price:     o.AvgPrice,
				Timestamp: time.Now().UTC(),
				Partial:   true,
			})
		}

		p.fillSeen[o.OrderID] = cur
	}
}

type ibkrOrder struct {
	OrderID   string  `json:"orderId"`
	Status    string  `json:"status"`
	Ticker    string  `json:"ticker"`
	Side      string  `json:"side"`
	FilledQty float64 `json:"filledQuantity"`
	AvgPrice  float64 `json:"avgPrice"`
}

// ibkrBarParams converts a canonical timeframe string to IBKR bar size and period strings.
// period is derived from the date range when both start and end are provided.
func ibkrBarParams(timeframe string, start, end time.Time) (barSize, period string) {
	switch timeframe {
	case "1s":
		barSize = "1secs"
	case "1m":
		barSize = "1min"
	case "5m":
		barSize = "5mins"
	case "15m":
		barSize = "15mins"
	case "1h":
		barSize = "1h"
	case "1d":
		barSize = "1day"
	default:
		barSize = "1min"
	}

	if !start.IsZero() && !end.IsZero() {
		days := int(end.Sub(start).Hours()/24) + 1
		switch {
		case days <= 1:
			period = "1d"
		case days <= 7:
			period = "1w"
		case days <= 30:
			period = "1m"
		default:
			period = fmt.Sprintf("%dd", days)
		}
	} else {
		period = "1d"
	}
	return
}
