package script

import (
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
)

const (
	httpTimeout      = 30 * time.Second
	maxResponseBytes = 1024 * 1024 // 1MB
)

// ScriptStrategy implements strategy.Strategy via a Goja JS runtime.
type ScriptStrategy struct {
	mu            sync.Mutex
	vm            *goja.Runtime
	onTick        goja.Callable
	onFill        goja.Callable
	onTrade       goja.Callable
	onInit        goja.Callable
	onMarketOpen  goja.Callable
	onMarketClose goja.Callable
	onExit        goja.Callable
	name          string
}

// New creates a ScriptStrategy by compiling src in a Goja runtime.
func New(name, src string, config map[string]string) (*ScriptStrategy, error) {
	vm := goja.New()

	// Inject config object
	vm.Set("config", config)

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
		vm:            vm,
		onTick:        onTick,
		onFill:        extractOptional("onFill"),
		onTrade:       extractOptional("onTrade"),
		onInit:        extractOptional("onInit"),
		onMarketOpen:  extractOptional("onMarketOpen"),
		onMarketClose: extractOptional("onMarketClose"),
		onExit:        extractOptional("onExit"),
		name:          name,
	}, nil
}

// Name implements strategy.Strategy.
func (s *ScriptStrategy) Name() string { return s.name }

// ---------------------------------------------------------------------------
// strategy.Strategy (required)
// ---------------------------------------------------------------------------

// OnTick implements strategy.Strategy.
func (s *ScriptStrategy) OnTick(tick strategy.Tick, portfolio strategy.Portfolio) []strategy.Order {
	s.mu.Lock()
	defer s.mu.Unlock()

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
		fmt.Printf("script onTick error: %v\n", err)
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
		fmt.Printf("script onTrade error: %v\n", err)
		return nil
	}

	return s.parseOrders(result)
}

// HasOnTrade returns true if the script exports onTrade.
func (s *ScriptStrategy) HasOnTrade() bool {
	return s.onTrade != nil
}

// ---------------------------------------------------------------------------
// strategy.Initializer (optional)
// ---------------------------------------------------------------------------

// OnInit implements strategy.Initializer.
func (s *ScriptStrategy) OnInit(ctx strategy.InitContext) error {
	if s.onInit == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

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
		fmt.Printf("script onMarketOpen error: %v\n", err)
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
		fmt.Printf("script onMarketClose error: %v\n", err)
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
		orders = append(orders, o)
	}

	return orders
}
