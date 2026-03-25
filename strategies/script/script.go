package script

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/benny-conn/brandon-bot/strategy"
	"github.com/dop251/goja"
)

// Compile-time interface checks.
var (
	_ strategy.Strategy            = (*ScriptStrategy)(nil)
	_ strategy.TradeSubscriber     = (*ScriptStrategy)(nil)
	_ strategy.Initializer         = (*ScriptStrategy)(nil)
	_ strategy.DailySessionHandler = (*ScriptStrategy)(nil)
	_ strategy.Shutdowner          = (*ScriptStrategy)(nil)
	_ strategy.SymbolResolver      = (*ScriptStrategy)(nil)
)

const (
	httpTimeout      = 30 * time.Second
	maxResponseBytes = 1024 * 1024 // 1MB
)

const maxRuntimeErrors = 10

// ScriptStrategy implements strategy.Strategy via a Goja JS runtime.
type ScriptStrategy struct {
	mu             sync.Mutex
	vm             *goja.Runtime
	onTick         goja.Callable
	onFill         goja.Callable
	onTrade        goja.Callable
	onInit         goja.Callable
	onMarketOpen   goja.Callable
	onMarketClose  goja.Callable
	onExit         goja.Callable
	resolveSymbols goja.Callable
	name           string
	runtimeErrors  []string
	errorSeen      map[string]bool
	dataGlobal     *dataGlobal // nil if no MarketData provider
}

// New creates a ScriptStrategy by compiling src in a Goja runtime.
func New(name, src string, config map[string]string, opts ...Option) (*ScriptStrategy, error) {
	var options scriptOptions
	for _, o := range opts {
		o(&options)
	}

	vm := goja.New()

	// Inject config object
	vm.Set("config", config)

	// Inject capital global (read-only initial capital for position sizing)
	if options.hasCapital {
		vm.Set("capital", options.capital)
	}

	// Inject console.log
	console := vm.NewObject()
	console.Set("log", func(call goja.FunctionCall) goja.Value {
		args := make([]interface{}, len(call.Arguments))
		for i, arg := range call.Arguments {
			args[i] = arg.Export()
		}
		fmt.Println(args...)
		return goja.Undefined()
	})
	vm.Set("console", console)

	// Shared HTTP client
	httpClient := &http.Client{Timeout: httpTimeout}

	// Helper: apply headers from a JS object onto an http.Request.
	applyHeaders := func(req *http.Request, val goja.Value) {
		if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
			return
		}
		if hm, ok := val.Export().(map[string]interface{}); ok {
			for k, v := range hm {
				req.Header.Set(k, fmt.Sprint(v))
			}
		}
	}

	// Helper: execute request and return a {status, body} JS object.
	doRequest := func(req *http.Request) goja.Value {
		resp, err := httpClient.Do(req)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

		result := vm.NewObject()
		result.Set("status", resp.StatusCode)
		result.Set("body", string(body))
		return result
	}

	// requireHTTPS validates the URL scheme.
	requireHTTPS := func(method, url string) {
		if len(url) < 8 || url[:8] != "https://" {
			panic(vm.NewGoError(fmt.Errorf("http.%s: only HTTPS URLs are allowed", method)))
		}
	}

	httpObj := vm.NewObject()

	// http.get(url, headers?)
	httpObj.Set("get", func(call goja.FunctionCall) goja.Value {
		url := call.Argument(0).String()
		requireHTTPS("get", url)

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			panic(vm.NewGoError(err))
		}

		if len(call.Arguments) > 1 {
			applyHeaders(req, call.Argument(1))
		}

		return doRequest(req)
	})

	// http.post(url, body, headers?)
	httpObj.Set("post", func(call goja.FunctionCall) goja.Value {
		url := call.Argument(0).String()
		requireHTTPS("post", url)

		bodyStr := ""
		if len(call.Arguments) > 1 {
			arg := call.Argument(1)
			exported := arg.Export()
			switch v := exported.(type) {
			case string:
				bodyStr = v
			default:
				// Objects/arrays → JSON
				b, err := arg.ToObject(vm).MarshalJSON()
				if err != nil {
					panic(vm.NewGoError(fmt.Errorf("http.post: cannot serialize body: %w", err)))
				}
				bodyStr = string(b)
			}
		}

		req, err := http.NewRequest("POST", url, strings.NewReader(bodyStr))
		if err != nil {
			panic(vm.NewGoError(err))
		}

		if len(call.Arguments) > 2 {
			applyHeaders(req, call.Argument(2))
		}

		return doRequest(req)
	})

	vm.Set("http", httpObj)

	// Inject technical analysis library
	registerTA(vm)

	// Inject ML library
	registerML(vm)

	// Inject data access library (if MarketData provider supplied)
	var dg *dataGlobal
	if options.marketData != nil {
		dg = newDataGlobal(options.marketData)
		registerData(vm, dg)
	}

	// Run the script
	if _, err := vm.RunString(src); err != nil {
		return nil, fmt.Errorf("script compile error: %w", err)
	}

	// Extract onTick (required)
	onTick, ok := goja.AssertFunction(vm.Get("onTick"))
	if !ok {
		return nil, fmt.Errorf("script must export an onTick function")
	}

	// Extract optional callbacks
	extractOptional := func(name string) goja.Callable {
		v := vm.Get(name)
		if v == nil || goja.IsUndefined(v) {
			return nil
		}
		fn, _ := goja.AssertFunction(v)
		return fn
	}

	return &ScriptStrategy{
		vm:             vm,
		onTick:         onTick,
		onFill:         extractOptional("onFill"),
		onTrade:        extractOptional("onTrade"),
		onInit:         extractOptional("onInit"),
		onMarketOpen:   extractOptional("onMarketOpen"),
		onMarketClose:  extractOptional("onMarketClose"),
		onExit:         extractOptional("onExit"),
		resolveSymbols: extractOptional("resolveSymbols"),
		name:           name,
		errorSeen:      make(map[string]bool),
		dataGlobal:     dg,
	}, nil
}

// Name implements strategy.Strategy.
func (s *ScriptStrategy) Name() string { return s.name }

// RuntimeErrors returns the deduplicated runtime errors collected so far.
func (s *ScriptStrategy) RuntimeErrors() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.runtimeErrors))
	copy(out, s.runtimeErrors)
	return out
}

// trackError records a unique error string (caller must hold s.mu).
func (s *ScriptStrategy) trackError(msg string) {
	if s.errorSeen[msg] || len(s.runtimeErrors) >= maxRuntimeErrors {
		return
	}
	s.errorSeen[msg] = true
	s.runtimeErrors = append(s.runtimeErrors, msg)
}

// ---------------------------------------------------------------------------
// strategy.Strategy (required)
// ---------------------------------------------------------------------------

// OnTick implements strategy.Strategy.
func (s *ScriptStrategy) OnTick(tick strategy.Tick, portfolio strategy.Portfolio) []strategy.Order {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.dataGlobal != nil {
		s.dataGlobal.resetTickCount()
	}

	tickVal := s.vm.ToValue(map[string]interface{}{
		"symbol":    tick.Symbol,
		"timestamp": tick.Timestamp.UnixMilli(),
		"open":      tick.Open,
		"high":      tick.High,
		"low":       tick.Low,
		"close":     tick.Close,
		"volume":    tick.Volume,
	})

	portfolioVal := s.makePortfolioObj(portfolio)

	result, err := s.onTick(goja.Undefined(), tickVal, portfolioVal)
	if err != nil {
		msg := fmt.Sprintf("script onTick error: %v", err)
		fmt.Println(msg)
		s.trackError(msg)
		return nil
	}

	return s.parseOrders(result)
}

// OnFill implements strategy.Strategy.
func (s *ScriptStrategy) OnFill(fill strategy.Fill) {
	if s.onFill == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	fillVal := s.vm.ToValue(map[string]interface{}{
		"symbol":    fill.Symbol,
		"side":      fill.Side,
		"qty":       fill.Qty,
		"price":     fill.Price,
		"timestamp": fill.Timestamp.UnixMilli(),
	})
	s.onFill(goja.Undefined(), fillVal)
}

// ---------------------------------------------------------------------------
// strategy.TradeSubscriber (optional)
// ---------------------------------------------------------------------------

// OnTrade implements strategy.TradeSubscriber.
func (s *ScriptStrategy) OnTrade(trade strategy.Trade, portfolio strategy.Portfolio) []strategy.Order {
	if s.onTrade == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tradeVal := s.vm.ToValue(map[string]interface{}{
		"symbol":    trade.Symbol,
		"timestamp": trade.Timestamp.UnixMilli(),
		"price":     trade.Price,
		"size":      trade.Size,
	})

	portfolioVal := s.makePortfolioObj(portfolio)

	result, err := s.onTrade(goja.Undefined(), tradeVal, portfolioVal)
	if err != nil {
		msg := fmt.Sprintf("script onTrade error: %v", err)
		fmt.Println(msg)
		s.trackError(msg)
		return nil
	}

	return s.parseOrders(result)
}

// HasOnTrade returns true if the script exports onTrade.
func (s *ScriptStrategy) HasOnTrade() bool {
	return s.onTrade != nil
}

// ---------------------------------------------------------------------------
// strategy.SymbolResolver (optional)
// ---------------------------------------------------------------------------

// ResolveSymbols implements strategy.SymbolResolver. Calls the JS
// resolveSymbols(ctx) function if defined, where ctx exposes market discovery.
func (s *ScriptStrategy) ResolveSymbols(ctx strategy.InitContext) ([]string, error) {
	if s.resolveSymbols == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Build a context argument with discovery capabilities.
	arg := s.vm.NewObject()
	arg.Set("symbols", ctx.Symbols)
	arg.Set("timeframe", ctx.Timeframe)

	if ctx.Search != nil {
		arg.Set("searchAssets", func(call goja.FunctionCall) goja.Value {
			query := parseAssetQuery(s.vm, call)
			assets, err := ctx.Search.SearchAssets(context.Background(), query)
			if err != nil {
				panic(s.vm.NewGoError(err))
			}
			return s.vm.ToValue(assetsToJS(assets))
		})
	}

	// Legacy: keep listMarkets for backward compat with existing Kalshi scripts.
	if ctx.Discovery != nil {
		arg.Set("listMarkets", func(call goja.FunctionCall) goja.Value {
			opts := parseMarketListOpts(s.vm, call)
			markets, err := ctx.Discovery.ListMarkets(context.Background(), opts)
			if err != nil {
				panic(s.vm.NewGoError(err))
			}
			return s.vm.ToValue(marketsToJS(markets))
		})
	}

	result, err := s.resolveSymbols(goja.Undefined(), arg)
	if err != nil {
		return nil, fmt.Errorf("script resolveSymbols error: %w", err)
	}

	exported := result.Export()
	arr, ok := exported.([]interface{})
	if !ok {
		return nil, nil
	}
	symbols := make([]string, 0, len(arr))
	for _, v := range arr {
		if str, ok := v.(string); ok && str != "" {
			symbols = append(symbols, str)
		}
	}
	return symbols, nil
}

// ---------------------------------------------------------------------------
// strategy.Initializer (optional)
// ---------------------------------------------------------------------------

// OnInit implements strategy.Initializer.
func (s *ScriptStrategy) OnInit(ctx strategy.InitContext) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Register searchAssets global if provider supports asset search.
	if ctx.Search != nil {
		s.vm.Set("searchAssets", func(call goja.FunctionCall) goja.Value {
			query := parseAssetQuery(s.vm, call)
			assets, err := ctx.Search.SearchAssets(context.Background(), query)
			if err != nil {
				panic(s.vm.NewGoError(err))
			}
			return s.vm.ToValue(assetsToJS(assets))
		})
	}

	// Legacy: register kalshi global if provider supports market discovery.
	if ctx.Discovery != nil {
		kalshiObj := s.vm.NewObject()
		kalshiObj.Set("listMarkets", func(call goja.FunctionCall) goja.Value {
			opts := parseMarketListOpts(s.vm, call)
			markets, err := ctx.Discovery.ListMarkets(context.Background(), opts)
			if err != nil {
				panic(s.vm.NewGoError(err))
			}
			return s.vm.ToValue(marketsToJS(markets))
		})
		s.vm.Set("kalshi", kalshiObj)
	}

	// Register engine global for mid-run symbol management.
	if ctx.AddSymbols != nil {
		engineObj := s.vm.NewObject()
		engineObj.Set("addSymbols", func(call goja.FunctionCall) goja.Value {
			syms := make([]string, len(call.Arguments))
			for i, arg := range call.Arguments {
				syms[i] = arg.String()
			}
			ctx.AddSymbols(syms...)
			return goja.Undefined()
		})
		s.vm.Set("engine", engineObj)
	}

	if s.onInit == nil {
		return nil
	}

	ctxVal := s.vm.ToValue(map[string]interface{}{
		"symbols":   ctx.Symbols,
		"timeframe": ctx.Timeframe,
		"config":    ctx.Config,
	})

	if _, err := s.onInit(goja.Undefined(), ctxVal); err != nil {
		return fmt.Errorf("script onInit error: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// strategy.DailySessionHandler (optional)
// ---------------------------------------------------------------------------

// OnMarketOpen implements strategy.SessionHandler.
func (s *ScriptStrategy) OnMarketOpen(portfolio strategy.Portfolio) []strategy.Order {
	if s.onMarketOpen == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	portfolioVal := s.makePortfolioObj(portfolio)

	result, err := s.onMarketOpen(goja.Undefined(), portfolioVal)
	if err != nil {
		msg := fmt.Sprintf("script onMarketOpen error: %v", err)
		fmt.Println(msg)
		s.trackError(msg)
		return nil
	}

	return s.parseOrders(result)
}

// OnMarketClose implements strategy.SessionHandler.
func (s *ScriptStrategy) OnMarketClose(portfolio strategy.Portfolio) []strategy.Order {
	if s.onMarketClose == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	portfolioVal := s.makePortfolioObj(portfolio)

	result, err := s.onMarketClose(goja.Undefined(), portfolioVal)
	if err != nil {
		msg := fmt.Sprintf("script onMarketClose error: %v", err)
		fmt.Println(msg)
		s.trackError(msg)
		return nil
	}

	return s.parseOrders(result)
}

// ---------------------------------------------------------------------------
// strategy.Shutdowner (optional)
// ---------------------------------------------------------------------------

// OnExit implements strategy.Shutdowner.
func (s *ScriptStrategy) OnExit() {
	if s.onExit == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.onExit(goja.Undefined()); err != nil {
		fmt.Printf("script onExit error: %v\n", err)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// makePortfolioObj wraps a strategy.Portfolio as a JS object with callable methods.
func (s *ScriptStrategy) makePortfolioObj(p strategy.Portfolio) goja.Value {
	obj := s.vm.NewObject()
	obj.Set("cash", func(goja.FunctionCall) goja.Value { return s.vm.ToValue(p.Cash()) })
	obj.Set("equity", func(goja.FunctionCall) goja.Value { return s.vm.ToValue(p.Equity()) })
	obj.Set("totalPL", func(goja.FunctionCall) goja.Value { return s.vm.ToValue(p.TotalPL()) })
	obj.Set("position", func(call goja.FunctionCall) goja.Value {
		sym := call.Argument(0).String()
		pos := p.Position(sym)
		if pos == nil {
			return goja.Null()
		}
		posObj := s.vm.NewObject()
		posObj.Set("symbol", pos.Symbol)
		posObj.Set("qty", pos.Qty)
		posObj.Set("avgCost", pos.AvgCost)
		posObj.Set("marketValue", pos.MarketValue)
		posObj.Set("unrealizedPL", pos.UnrealizedPL)
		return posObj
	})
	obj.Set("positions", func(goja.FunctionCall) goja.Value {
		positions := p.Positions()
		arr := make([]interface{}, len(positions))
		for i, pos := range positions {
			m := map[string]interface{}{
				"symbol":       pos.Symbol,
				"qty":          pos.Qty,
				"avgCost":      pos.AvgCost,
				"marketValue":  pos.MarketValue,
				"unrealizedPL": pos.UnrealizedPL,
			}
			arr[i] = m
		}
		return s.vm.ToValue(arr)
	})
	return obj
}

// parseOrders converts a JS return value into []strategy.Order.
func (s *ScriptStrategy) parseOrders(val goja.Value) []strategy.Order {
	exported := val.Export()
	arr, ok := exported.([]interface{})
	if !ok {
		return nil
	}

	orders := make([]strategy.Order, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		o := strategy.Order{}
		if v, ok := m["symbol"].(string); ok {
			o.Symbol = v
		}
		if v, ok := m["side"].(string); ok {
			o.Side = v
		}
		if v, ok := m["orderType"].(string); ok {
			o.OrderType = v
		}
		if v, ok := m["qty"]; ok {
			switch vt := v.(type) {
			case float64:
				o.Qty = vt
			case int64:
				o.Qty = float64(vt)
			}
		}
		if v := getFloatField(m, "limitPrice"); v != 0 {
			o.LimitPrice = v
		}
		if v := getFloatField(m, "stopPrice"); v != 0 {
			o.StopPrice = v
		}
		if v := getFloatField(m, "stopLoss"); v != 0 {
			o.StopLoss = v
		}
		if v := getFloatField(m, "takeProfit"); v != 0 {
			o.TakeProfit = v
		}
		if v, ok := m["reason"].(string); ok {
			o.Reason = v
		}
		orders = append(orders, o)
	}

	return orders
}

// getFloatField extracts a numeric field from a JS-exported map.
func getFloatField(m map[string]interface{}, key string) float64 {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch vt := v.(type) {
	case float64:
		return vt
	case int64:
		return float64(vt)
	}
	return 0
}

// parseMarketListOpts extracts MarketListOptions from a JS function call.
// Accepts either a string (backward-compatible status) or an options object.
func parseMarketListOpts(vm *goja.Runtime, call goja.FunctionCall) strategy.MarketListOptions {
	opts := strategy.MarketListOptions{Status: "open"}
	if len(call.Arguments) == 0 || goja.IsUndefined(call.Argument(0)) {
		return opts
	}

	arg := call.Argument(0)
	// Backward-compatible: bare string means status.
	if _, ok := arg.Export().(string); ok {
		opts.Status = arg.String()
		return opts
	}

	// Options object.
	obj := arg.ToObject(vm)
	if v := obj.Get("status"); v != nil && !goja.IsUndefined(v) {
		opts.Status = v.String()
	}
	if v := obj.Get("limit"); v != nil && !goja.IsUndefined(v) {
		opts.Limit = int(v.ToInteger())
	}
	if v := obj.Get("seriesTicker"); v != nil && !goja.IsUndefined(v) {
		opts.SeriesTicker = v.String()
	}
	if v := obj.Get("eventTicker"); v != nil && !goja.IsUndefined(v) {
		opts.EventTicker = v.String()
	}
	if v := obj.Get("minVolume"); v != nil && !goja.IsUndefined(v) {
		opts.MinVolume = int(v.ToInteger())
	}
	return opts
}

// parseAssetQuery extracts an AssetQuery from a JS function call.
// Accepts a string (text search shorthand) or an options object.
func parseAssetQuery(vm *goja.Runtime, call goja.FunctionCall) strategy.AssetQuery {
	query := strategy.AssetQuery{}
	if len(call.Arguments) == 0 || goja.IsUndefined(call.Argument(0)) {
		return query
	}

	arg := call.Argument(0)
	// Shorthand: bare string means text search.
	if s, ok := arg.Export().(string); ok {
		query.Text = s
		return query
	}

	obj := arg.ToObject(vm)
	if v := obj.Get("text"); v != nil && !goja.IsUndefined(v) {
		query.Text = v.String()
	}
	if v := obj.Get("assetClass"); v != nil && !goja.IsUndefined(v) {
		query.AssetClass = v.String()
	}
	if v := obj.Get("exchange"); v != nil && !goja.IsUndefined(v) {
		query.Exchange = v.String()
	}
	if v := obj.Get("status"); v != nil && !goja.IsUndefined(v) {
		query.Status = v.String()
	}
	if v := obj.Get("limit"); v != nil && !goja.IsUndefined(v) {
		query.Limit = int(v.ToInteger())
	}
	return query
}

// assetsToJS converts assets into a JS-friendly slice of maps.
func assetsToJS(assets []strategy.Asset) []interface{} {
	result := make([]interface{}, len(assets))
	for i, a := range assets {
		m := map[string]interface{}{
			"symbol":     a.Symbol,
			"name":       a.Name,
			"assetClass": a.AssetClass,
			"exchange":   a.Exchange,
			"tradable":   a.Tradable,
		}
		if a.Extra != nil {
			m["extra"] = a.Extra
		}
		result[i] = m
	}
	return result
}

// marketsToJS converts discovered markets into a JS-friendly slice of maps.
func marketsToJS(markets []strategy.DiscoveredMarket) []interface{} {
	result := make([]interface{}, len(markets))
	for i, m := range markets {
		result[i] = map[string]interface{}{
			"ticker":       m.Ticker,
			"title":        m.Title,
			"status":       m.Status,
			"eventTicker":  m.EventTicker,
			"seriesTicker": m.SeriesTicker,
			"volume":       m.Volume,
			"volume24h":    m.Volume24H,
			"openTime":     m.OpenTime.UnixMilli(),
			"closeTime":    m.CloseTime.UnixMilli(),
		}
	}
	return result
}
