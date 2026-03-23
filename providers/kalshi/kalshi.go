// Package kalshi implements provider.MarketData and provider.Execution for the
// Kalshi prediction market exchange. Reads KALSHI_API_KEY and KALSHI_PRIVATE_KEY_PATH
// from env. Implements ContinuousMarket (no market sessions) and ClientSideStops
// (no native stop orders).
package kalshi

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/benny-conn/brandon-bot/provider"
	"github.com/benny-conn/brandon-bot/strategy"
)

const (
	prodBaseURL = "https://api.elections.kalshi.com/trade-api/v2"
	demoBaseURL = "https://demo.kalshi.com/trade-api/v2"
)

// Config holds Kalshi provider credentials and settings.
type Config struct {
	APIKey         string `json:"api_key"`
	PrivateKeyPath string `json:"private_key_path"` // path to PEM-encoded RSA private key
	PrivateKeyPEM  string `json:"private_key_pem"`  // alternative: raw PEM string
	Demo           bool   `json:"demo"`             // use demo environment
}

// Provider implements provider.MarketData, provider.Execution,
// provider.ContinuousMarket, and provider.ClientSideStops.
type Provider struct {
	apiKey     string
	privateKey *rsa.PrivateKey
	baseURL    string
	httpClient *http.Client

	// Rate limiters for Kalshi API (basic tier: 10 reads/sec, 10 writes/sec).
	// We use 10/sec for reads (half the 20/sec limit) to leave headroom.
	readLimiter  *rateLimiter
	writeLimiter *rateLimiter

	// WebSocket connection shared across subscriptions.
	ws     *wsConn
	wsOnce sync.Once

	// Positions cache for settlement handling.
	posMu     sync.RWMutex
	positions map[string]int // ticker → net contract count (from last GetPositions)
}

// New creates a Kalshi provider. Config fields override env vars.
func New(cfg Config) *Provider {
	apiKey := envOr(cfg.APIKey, "KALSHI_API_KEY")
	keyPath := envOr(cfg.PrivateKeyPath, "KALSHI_PRIVATE_KEY_PATH")

	var privKey *rsa.PrivateKey
	if cfg.PrivateKeyPEM != "" {
		var err error
		privKey, err = parsePrivateKey([]byte(cfg.PrivateKeyPEM))
		if err != nil {
			log.Println("kalshi: parsing private key PEM: %v", err)
		}
	} else if keyPath != "" {
		data, err := os.ReadFile(keyPath)
		if err != nil {
			log.Println("kalshi: reading private key %q: %v", keyPath, err)
		}
		privKey, err = parsePrivateKey(data)
		if err != nil {
			log.Println("kalshi: parsing private key %q: %v", keyPath, err)
		}
	}

	baseURL := prodBaseURL
	if cfg.Demo {
		baseURL = demoBaseURL
	}

	p := &Provider{
		apiKey:       apiKey,
		privateKey:   privKey,
		baseURL:      baseURL,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		readLimiter:  newRateLimiter(10), // 10 reads/sec (basic tier allows 20)
		writeLimiter: newRateLimiter(8),  // 8 writes/sec (basic tier allows 10)
		positions:    make(map[string]int),
	}
	p.ws = newWSConn(p)
	return p
}

// ContinuousMarket marks this provider as 24/7 — no market sessions.
func (p *Provider) ContinuousMarket() {}

// ClientSideStops marks this provider as not supporting native stop orders.
func (p *Provider) ClientSideStops() {}

// — Auth —

func parsePrivateKey(pemData []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}

	// Try PKCS#8 first, then PKCS#1.
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS#8 key is not RSA")
		}
		return rsaKey, nil
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// sign creates an RSA-PSS signature for Kalshi API authentication.
// The message format is: timestamp_ms + "\n" + method + "\n" + path
func (p *Provider) sign(timestampMS int64, method, path string) (string, error) {
	msg := fmt.Sprintf("%d\n%s\n%s", timestampMS, method, path)
	hash := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPSS(rand.Reader, p.privateKey, crypto.SHA256, hash[:], &rsa.PSSOptions{
		SaltLength: rsa.PSSSaltLengthEqualsHash,
	})
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// rateLimiter is a simple token-bucket rate limiter.
type rateLimiter struct {
	mu       sync.Mutex
	tokens   float64
	max      float64
	rate     float64 // tokens per second
	lastTime time.Time
}

func newRateLimiter(perSec float64) *rateLimiter {
	return &rateLimiter{
		tokens:   perSec,
		max:      perSec,
		rate:     perSec,
		lastTime: time.Now(),
	}
}

// wait blocks until a token is available or ctx is cancelled.
func (rl *rateLimiter) wait(ctx context.Context) error {
	for {
		rl.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(rl.lastTime).Seconds()
		rl.tokens += elapsed * rl.rate
		if rl.tokens > rl.max {
			rl.tokens = rl.max
		}
		rl.lastTime = now

		if rl.tokens >= 1 {
			rl.tokens--
			rl.mu.Unlock()
			return nil
		}

		// Calculate wait time for next token.
		waitDur := time.Duration((1-rl.tokens)/rl.rate*1000) * time.Millisecond
		rl.mu.Unlock()

		select {
		case <-time.After(waitDur):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// doRequest executes an authenticated HTTP request to the Kalshi API.
// It proactively rate-limits to stay within the basic tier (20 reads/sec, 10 writes/sec)
// and retries on 429 responses with exponential backoff.
func (p *Provider) doRequest(ctx context.Context, method, path string, body interface{}) ([]byte, error) {
	backoffs := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

	// Proactive rate limiting.
	limiter := p.readLimiter
	if method != "GET" {
		limiter = p.writeLimiter
	}

	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling request body: %w", err)
		}
	}

	for attempt := 0; ; attempt++ {
		if err := limiter.wait(ctx); err != nil {
			return nil, err
		}

		var reqBody io.Reader
		if bodyBytes != nil {
			reqBody = bytes.NewReader(bodyBytes)
		}

		url := p.baseURL + path
		req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
		if err != nil {
			return nil, err
		}

		ts := time.Now().UnixMilli()
		sig, err := p.sign(ts, method, path)
		if err != nil {
			return nil, fmt.Errorf("signing request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("KALSHI-ACCESS-KEY", p.apiKey)
		req.Header.Set("KALSHI-ACCESS-TIMESTAMP", fmt.Sprintf("%d", ts))
		req.Header.Set("KALSHI-ACCESS-SIGNATURE", sig)

		resp, err := p.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("http %s %s: %w", method, path, err)
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading response: %w", err)
		}

		if resp.StatusCode == 429 && attempt < len(backoffs) {
			delay := backoffs[attempt]
			// Respect Retry-After header if present.
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil {
					delay = time.Duration(secs) * time.Second
				}
			}
			log.Printf("kalshi: rate limited on %s %s, retrying in %s (attempt %d/%d)", method, path, delay, attempt+1, len(backoffs))
			select {
			case <-time.After(delay):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("kalshi API %s %s returned %d: %s", method, path, resp.StatusCode, string(respBody))
		}

		return respBody, nil
	}
}

// — MarketData —

func (p *Provider) FetchBars(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]provider.Bar, error) {
	interval, err := parseInterval(timeframe)
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf("/markets/%s/candlesticks?period_interval=%d&start_ts=%d&end_ts=%d",
		symbol, interval, start.Unix(), end.Unix())

	data, err := p.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("fetching candles for %s: %w", symbol, err)
	}

	var resp candlesticksResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parsing candles response: %w", err)
	}

	bars := make([]provider.Bar, len(resp.Candlesticks))
	for i, c := range resp.Candlesticks {
		bars[i] = provider.Bar{
			Symbol:    symbol,
			Timestamp: time.Unix(int64(c.EndPeriodTS), 0).UTC(),
			Open:      centsToPrice(c.Price.Open),
			High:      centsToPrice(c.Price.High),
			Low:       centsToPrice(c.Price.Low),
			Close:     centsToPrice(c.Price.Close),
			Volume:    float64(c.Volume),
		}
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

func (p *Provider) SubscribeBars(ctx context.Context, symbols []string, timeframe string, handler func(provider.Bar)) error {
	interval, err := parseDuration(timeframe)
	if err != nil {
		return err
	}

	// Poll candlestick endpoint on each interval to emit completed bars.
	// Kalshi doesn't have a native bar stream.
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("kalshi: bar polling active — interval=%s symbols=%v", timeframe, symbols)

	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			end := now
			start := now.Add(-interval)
			for _, sym := range symbols {
				bars, err := p.FetchBars(ctx, sym, timeframe, start, end)
				if err != nil {
					log.Printf("kalshi: bar poll error for %s: %v", sym, err)
					continue
				}
				for _, b := range bars {
					handler(b)
				}
			}
		}
	}
}

func (p *Provider) SubscribeTrades(ctx context.Context, symbols []string, handler func(provider.Trade)) error {
	p.ws.subscribe("trade", symbols, func(msg wsMessage) {
		handler(provider.Trade{
			Symbol:    msg.Msg.Ticker,
			Timestamp: msg.Msg.CreatedAt,
			Price:     float64(msg.Msg.YesPrice) / 100.0,
			Size:      float64(msg.Msg.Count),
		})
	})

	return p.ws.run(ctx)
}

func (p *Provider) SubscribeQuotes(ctx context.Context, symbols []string, handler func(provider.Quote)) error {
	// Use orderbook_delta channel for bid/ask updates.
	p.ws.subscribe("orderbook_delta", symbols, func(msg wsMessage) {
		handler(provider.Quote{
			Symbol:    msg.Msg.Ticker,
			Timestamp: time.Now(),
			BidPrice:  float64(msg.Msg.YesPrice) / 100.0,
			AskPrice:  float64(msg.Msg.NoPrice) / 100.0,
		})
	})

	return p.ws.run(ctx)
}

// — Execution —

func (p *Provider) GetAccount(ctx context.Context) (provider.Account, error) {
	data, err := p.doRequest(ctx, "GET", "/portfolio/balance", nil)
	if err != nil {
		return provider.Account{}, fmt.Errorf("kalshi get balance: %w", err)
	}

	var resp balanceResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return provider.Account{}, fmt.Errorf("parsing balance: %w", err)
	}

	// Balance is in cents — convert to dollars.
	cash := resp.Balance / 100.0
	return provider.Account{
		Cash:   cash,
		Equity: cash, // positions add to equity via market value
	}, nil
}

func (p *Provider) GetPositions(ctx context.Context) ([]provider.Position, error) {
	data, err := p.doRequest(ctx, "GET", "/portfolio/positions?settlement_status=unsettled", nil)
	if err != nil {
		return nil, fmt.Errorf("kalshi get positions: %w", err)
	}

	var resp positionsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parsing positions: %w", err)
	}

	// Cache positions for settlement handling.
	p.posMu.Lock()
	p.positions = make(map[string]int, len(resp.MarketPositions))
	for _, mp := range resp.MarketPositions {
		p.positions[mp.Ticker] = mp.Position
	}
	p.posMu.Unlock()

	var result []provider.Position
	for _, mp := range resp.MarketPositions {
		if mp.Position == 0 {
			continue
		}

		qty := float64(mp.Position)
		avgCost := 0.0
		if mp.Position != 0 {
			avgCost = float64(mp.TotalCost) / (float64(abs(mp.Position)) * 100.0)
		}

		result = append(result, provider.Position{
			Symbol:        mp.Ticker,
			Qty:           qty,
			AvgEntryPrice: avgCost,
		})
	}
	return result, nil
}

func (p *Provider) GetOpenOrders(ctx context.Context) ([]provider.OpenOrder, error) {
	data, err := p.doRequest(ctx, "GET", "/portfolio/orders?status=resting", nil)
	if err != nil {
		return nil, fmt.Errorf("kalshi get open orders: %w", err)
	}

	var resp ordersResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parsing orders: %w", err)
	}

	result := make([]provider.OpenOrder, len(resp.Orders))
	for i, o := range resp.Orders {
		side := "buy"
		if o.Action == "sell" {
			side = "sell"
		}
		result[i] = provider.OpenOrder{
			ID:         o.OrderID,
			Symbol:     o.Ticker,
			Side:       side,
			Qty:        float64(o.PlaceCount),
			Filled:     float64(o.FilledCount),
			OrderType:  o.Type,
			LimitPrice: float64(o.YesPrice) / 100.0,
		}
	}
	return result, nil
}

func (p *Provider) PlaceOrder(ctx context.Context, order strategy.Order) (provider.OrderResult, error) {
	count := int(math.Round(order.Qty))
	if count <= 0 {
		return provider.OrderResult{}, fmt.Errorf("invalid order quantity: %.2f", order.Qty)
	}

	action := order.Side // "buy" or "sell"

	orderType := "market"
	var yesPrice int
	if order.OrderType == "limit" && order.LimitPrice > 0 {
		orderType = "limit"
		yesPrice = priceToCents(order.LimitPrice)
	}

	req := createOrderRequest{
		Ticker: order.Symbol,
		Action: action,
		Side:   "yes", // we always trade YES contracts; buying NO = selling YES
		Type:   orderType,
		Count:  count,
	}
	if yesPrice > 0 {
		req.YesPrice = yesPrice
	}

	data, err := p.doRequest(ctx, "POST", "/portfolio/orders", req)
	if err != nil {
		return provider.OrderResult{}, fmt.Errorf("placing %s order for %s qty=%d: %w",
			action, order.Symbol, count, err)
	}

	var resp createOrderResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return provider.OrderResult{}, fmt.Errorf("parsing order response: %w", err)
	}

	return provider.OrderResult{ID: resp.Order.OrderID}, nil
}

func (p *Provider) CancelOrder(ctx context.Context, orderID string) error {
	_, err := p.doRequest(ctx, "DELETE", "/portfolio/orders/"+orderID, nil)
	if err != nil {
		return fmt.Errorf("kalshi cancel order %s: %w", orderID, err)
	}
	return nil
}

func (p *Provider) SubscribeFills(ctx context.Context, handler func(provider.Fill)) error {
	// Subscribe to fill events.
	p.ws.subscribe("fill", nil, func(msg wsMessage) {
		if !msg.Msg.IsFill {
			return
		}
		side := "buy"
		if msg.Msg.Action == "sell" {
			side = "sell"
		}
		handler(provider.Fill{
			OrderID:   msg.Msg.OrderID,
			Symbol:    msg.Msg.Ticker,
			Side:      side,
			Qty:       float64(msg.Msg.Count),
			Price:     float64(msg.Msg.YesPrice) / 100.0,
			Timestamp: msg.Msg.CreatedAt,
		})
	})

	// Also subscribe to market lifecycle for settlement events.
	p.ws.subscribe("market_lifecycle_v2", nil, func(msg wsMessage) {
		if msg.Msg.Status != "settled" {
			return
		}
		p.handleSettlement(msg.Msg.MarketTicker, msg.Msg.Result, handler)
	})

	return p.ws.run(ctx)
}

// handleSettlement processes a market settlement event. When a market resolves,
// any open position is closed at the settlement price ($1 for a winning YES
// position, $0 for a losing one).
func (p *Provider) handleSettlement(ticker, result string, handler func(provider.Fill)) {
	p.posMu.RLock()
	qty := p.positions[ticker]
	p.posMu.RUnlock()

	if qty == 0 {
		return
	}

	// Settlement price: if you hold YES and result is "yes", you get $1 per contract.
	// If result is "no" or "all_no", you get $0.
	var settlementPrice float64
	if result == "yes" && qty > 0 {
		settlementPrice = 1.0
	} else if (result == "no" || result == "all_no") && qty > 0 {
		settlementPrice = 0.0
	}

	log.Printf("kalshi: market %s settled → %s | position=%d contracts → settlement @ $%.2f",
		ticker, result, qty, settlementPrice)

	handler(provider.Fill{
		OrderID:   fmt.Sprintf("settlement-%s", ticker),
		Symbol:    ticker,
		Side:      "sell", // settlement closes the position
		Qty:       float64(abs(qty)),
		Price:     settlementPrice,
		Timestamp: time.Now(),
	})

	// Clear cached position.
	p.posMu.Lock()
	delete(p.positions, ticker)
	p.posMu.Unlock()
}

// — MarketDiscovery —

// ListMarkets implements strategy.MarketDiscovery. Fetches markets from the
// Kalshi catalog, filtered by status ("open", "closed", "settled", or "" for all).
func (p *Provider) ListMarkets(ctx context.Context, status string) ([]strategy.DiscoveredMarket, error) {
	path := "/markets"
	if status != "" {
		path += "?status=" + status
	}

	var allMarkets []apiMarket
	cursor := ""

	for {
		reqPath := path
		if cursor != "" {
			sep := "?"
			if strings.Contains(reqPath, "?") {
				sep = "&"
			}
			reqPath += sep + "cursor=" + cursor
		}

		data, err := p.doRequest(ctx, "GET", reqPath, nil)
		if err != nil {
			return nil, fmt.Errorf("kalshi list markets: %w", err)
		}

		var resp marketsResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("parsing markets response: %w", err)
		}

		allMarkets = append(allMarkets, resp.Markets...)

		if resp.Cursor == "" {
			break
		}
		cursor = resp.Cursor
	}

	result := make([]strategy.DiscoveredMarket, len(allMarkets))
	for i, m := range allMarkets {
		result[i] = strategy.DiscoveredMarket{
			Ticker:      m.Ticker,
			Title:       m.Title,
			Status:      m.Status,
			EventTicker: m.EventTicker,
			Volume:      m.Volume,
			OpenTime:    m.OpenTime,
			CloseTime:   m.CloseTime,
		}
	}
	return result, nil
}

// — Helpers —

func envOr(val, envKey string) string {
	if val != "" {
		return val
	}
	return os.Getenv(envKey)
}

// parseInterval converts a canonical timeframe to Kalshi's period_interval.
func parseInterval(tf string) (int, error) {
	switch tf {
	case "1m":
		return 1, nil
	case "1h":
		return 60, nil
	case "1d":
		return 1440, nil
	default:
		return 0, fmt.Errorf("kalshi: unsupported timeframe %q — use 1m, 1h, or 1d", tf)
	}
}

// parseDuration converts a canonical timeframe to a Go duration for polling.
func parseDuration(tf string) (time.Duration, error) {
	switch tf {
	case "1m":
		return time.Minute, nil
	case "5m":
		return 5 * time.Minute, nil
	case "15m":
		return 15 * time.Minute, nil
	case "1h":
		return time.Hour, nil
	case "1d":
		return 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("kalshi: unsupported timeframe %q", tf)
	}
}

// centsToPrice converts a Kalshi price (which may be in cents 0-100) to a 0.0-1.0 float.
// Kalshi candle prices can come as either cents or decimal — handle both.
func centsToPrice(v float64) float64 {
	if v > 1.0 {
		return v / 100.0
	}
	return v
}

// priceToCents converts a 0.0-1.0 price to Kalshi cents (1-99).
func priceToCents(price float64) int {
	cents := int(math.Round(price * 100))
	if cents < 1 {
		cents = 1
	}
	if cents > 99 {
		cents = 99
	}
	return cents
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// stripPath extracts just the path portion for timeframe validation logging.
func stripPath(s string) string {
	if idx := strings.Index(s, "?"); idx != -1 {
		return s[:idx]
	}
	return s
}
