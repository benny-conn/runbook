// Command topstepx-test exercises TopstepX APIs for integration testing.
//
// Usage:
//
//	TOPSTEPX_USERNAME=... TOPSTEPX_API_KEY=... go run ./cmd/topstepx-test [flags]
//
// Flags:
//
//	-symbol         Symbol to test (default "ES")
//	-account        Account ID to use; 0 = auto-select first (default 0)
//	-bars           Fetch historical bars (default true)
//	-quotes         Stream live quotes for a few seconds (default true)
//	-raw            Dump raw SignalR events
//	-seconds        How many seconds to stream quotes/raw events (default 5)
//	-test-cancel    Place a limit order far from market, verify, then cancel (no fill risk)
//	-test-roundtrip Market buy 1 MES then immediately sell (tiny risk, ~$1.25 max)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/benny-conn/brandon-bot/provider"
	topstepxprovider "github.com/benny-conn/brandon-bot/providers/topstepx"
	"github.com/benny-conn/brandon-bot/strategy"
)

func main() {
	symbol := flag.String("symbol", "ES", "symbol to test")
	accountID := flag.Int64("account", 0, "account ID (0 = auto-select first)")
	doBars := flag.Bool("bars", true, "fetch historical bars")
	doQuotes := flag.Bool("quotes", true, "stream live quotes")
	doRaw := flag.Bool("raw", false, "dump raw SignalR events (quotes+trades)")
	seconds := flag.Int("seconds", 5, "seconds to stream quotes/raw events")
	testCancel := flag.Bool("test-cancel", false, "place a limit order far from market, verify in open orders, then cancel")
	testRoundtrip := flag.Bool("test-roundtrip", false, "market buy 1 MES then immediately market sell (max risk ~$1.25)")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	p := topstepxprovider.New(topstepxprovider.Config{})

	// --- Authenticate ---
	fmt.Println("=== Authenticating ===")
	if err := p.Authenticate(ctx); err != nil {
		log.Fatalf("auth failed: %v", err)
	}
	fmt.Println("OK")

	// --- List accounts ---
	fmt.Println("\n=== Accounts ===")
	accounts, err := p.ListAccounts(ctx)
	if err != nil {
		log.Fatalf("list accounts: %v", err)
	}
	if len(accounts) == 0 {
		fmt.Println("no active accounts found")
		os.Exit(1)
	}
	for _, a := range accounts {
		fmt.Printf("  id=%-12d name=%-30s balance=$%.2f canTrade=%v\n",
			a.ID, a.Name, a.Balance, a.CanTrade)
	}

	// --- Select account ---
	if *accountID != 0 {
		p.SetAccount(*accountID)
		fmt.Printf("\nUsing specified account %d\n", *accountID)
	} else {
		p.SetAccount(accounts[0].ID)
		fmt.Printf("\nAuto-selected account %d (%s)\n", accounts[0].ID, accounts[0].Name)
	}

	// --- Account info ---
	fmt.Println("\n=== Account Info ===")
	acct, err := p.GetAccount(ctx)
	if err != nil {
		log.Fatalf("get account: %v", err)
	}
	fmt.Printf("  cash=$%.2f  equity=$%.2f\n", acct.Cash, acct.Equity)

	// --- Positions ---
	fmt.Println("\n=== Open Positions ===")
	positions, err := p.GetPositions(ctx)
	if err != nil {
		log.Fatalf("get positions: %v", err)
	}
	if len(positions) == 0 {
		fmt.Println("  (none)")
	}
	for _, pos := range positions {
		fmt.Printf("  %s  qty=%.0f  avgEntry=%.2f\n", pos.Symbol, pos.Qty, pos.AvgEntryPrice)
	}

	// --- Open orders ---
	fmt.Println("\n=== Open Orders ===")
	orders, err := p.GetOpenOrders(ctx)
	if err != nil {
		log.Fatalf("get open orders: %v", err)
	}
	if len(orders) == 0 {
		fmt.Println("  (none)")
	}
	for _, o := range orders {
		fmt.Printf("  %s  %s %s  qty=%.0f  filled=%.0f  limit=%.2f  stop=%.2f\n",
			o.ID, o.Side, o.Symbol, o.Qty, o.Filled, o.LimitPrice, o.StopPrice)
	}

	// --- Historical bars ---
	if *doBars {
		fmt.Printf("\n=== Historical Bars (%s, 1h, last 24h) ===\n", *symbol)
		end := time.Now().UTC()
		start := end.Add(-24 * time.Hour)
		bars, err := p.FetchBars(ctx, *symbol, "1h", start, end)
		if err != nil {
			log.Fatalf("fetch bars: %v", err)
		}
		fmt.Printf("  got %d bars\n", len(bars))
		offset := 0
		if len(bars) > 5 {
			offset = len(bars) - 5
		}
		for _, b := range bars[offset:] {
			fmt.Printf("  %s  O=%.2f H=%.2f L=%.2f C=%.2f V=%.0f\n",
				b.Timestamp.Format("2006-01-02 15:04"), b.Open, b.High, b.Low, b.Close, b.Volume)
		}
	}

	// --- Raw SignalR events ---
	if *doRaw {
		fmt.Printf("\n=== Raw SignalR Events (%s, %ds) ===\n", *symbol, *seconds)
		rawCtx, rawCancel := context.WithTimeout(ctx, time.Duration(*seconds)*time.Second)
		defer rawCancel()

		count := 0
		err := p.SubscribeRawMarketEvents(rawCtx, []string{*symbol}, func(ev topstepxprovider.RawMarketEvent) {
			count++
			if count <= 10 {
				fmt.Printf("\n  [%s]\n", ev.Target)
				for i, arg := range ev.Arguments {
					var pretty json.RawMessage
					if err := json.Unmarshal(arg, &pretty); err == nil {
						formatted, _ := json.MarshalIndent(pretty, "    ", "  ")
						fmt.Printf("    arg[%d]: %s\n", i, formatted)
					}
				}
			}
		})
		if err != nil && rawCtx.Err() == nil {
			log.Fatalf("subscribe raw: %v", err)
		}
		fmt.Printf("\n  received %d raw events in %ds\n", count, *seconds)
	}

	// --- Live quotes ---
	if *doQuotes && !*doRaw {
		fmt.Printf("\n=== Live Quotes (%s, %ds) ===\n", *symbol, *seconds)
		quoteCtx, quoteCancel := context.WithTimeout(ctx, time.Duration(*seconds)*time.Second)
		defer quoteCancel()

		count := 0
		err := p.SubscribeQuotes(quoteCtx, []string{*symbol}, func(q provider.Quote) {
			count++
			if count <= 20 {
				fmt.Printf("  %s  bid=%.2f  ask=%.2f\n",
					q.Timestamp.Format("15:04:05.000"), q.BidPrice, q.AskPrice)
			}
		})
		if err != nil && quoteCtx.Err() == nil {
			log.Fatalf("subscribe quotes: %v", err)
		}
		fmt.Printf("  received %d quotes in %ds\n", count, *seconds)
	}

	// --- Test: place + cancel (zero risk) ---
	if *testCancel {
		runTestCancel(ctx, p, *symbol)
	}

	// --- Test: round-trip MES (tiny risk) ---
	if *testRoundtrip {
		runTestRoundtrip(ctx, p)
	}

	fmt.Println("\n=== Done ===")
}

// runTestCancel places a limit buy 200 points below market, verifies it in
// open orders, then cancels it. No fill risk.
func runTestCancel(ctx context.Context, p *topstepxprovider.Provider, symbol string) {
	fmt.Printf("\n=== Test: Place + Cancel (%s) ===\n", symbol)

	// Get current price from a quick quote snapshot.
	fmt.Println("  getting current price...")
	quoteCtx, quoteCancel := context.WithTimeout(ctx, 5*time.Second)
	defer quoteCancel()

	var bidPrice float64
	err := p.SubscribeQuotes(quoteCtx, []string{symbol}, func(q provider.Quote) {
		if bidPrice == 0 && q.BidPrice > 0 {
			bidPrice = q.BidPrice
			quoteCancel() // got what we need
		}
	})
	if err != nil && quoteCtx.Err() == nil {
		log.Fatalf("  get price: %v", err)
	}
	if bidPrice == 0 {
		log.Fatalf("  could not get current price for %s", symbol)
	}

	// Place limit buy 200 points below — will never fill.
	limitPrice := bidPrice - 200
	fmt.Printf("  current bid=%.2f, placing limit buy at %.2f (200 pts below)\n", bidPrice, limitPrice)

	result, err := p.PlaceOrder(ctx, strategy.Order{
		Symbol:     symbol,
		Side:       "buy",
		Qty:        1,
		OrderType:  "limit",
		LimitPrice: limitPrice,
	})
	if err != nil {
		log.Fatalf("  place order: %v", err)
	}
	fmt.Printf("  order placed: id=%s\n", result.ID)

	// Brief pause for order to register.
	time.Sleep(500 * time.Millisecond)

	// Verify it shows in open orders.
	fmt.Println("  checking open orders...")
	orders, err := p.GetOpenOrders(ctx)
	if err != nil {
		log.Fatalf("  get open orders: %v", err)
	}
	found := false
	for _, o := range orders {
		if o.ID == result.ID {
			fmt.Printf("  FOUND: %s %s %s qty=%.0f limit=%.2f\n",
				o.ID, o.Side, o.Symbol, o.Qty, o.LimitPrice)
			found = true
			break
		}
	}
	if !found {
		fmt.Printf("  WARNING: order %s not found in open orders (may have been rejected)\n", result.ID)
	}

	// Cancel it.
	fmt.Printf("  cancelling order %s...\n", result.ID)
	if err := p.CancelOrder(ctx, result.ID); err != nil {
		log.Fatalf("  cancel order: %v", err)
	}
	fmt.Println("  cancelled OK")

	// Verify it's gone.
	time.Sleep(500 * time.Millisecond)
	orders, err = p.GetOpenOrders(ctx)
	if err != nil {
		log.Fatalf("  get open orders after cancel: %v", err)
	}
	for _, o := range orders {
		if o.ID == result.ID {
			fmt.Printf("  WARNING: order %s still in open orders after cancel\n", result.ID)
			return
		}
	}
	fmt.Println("  verified: order no longer in open orders")
	fmt.Println("  PASS: PlaceOrder -> GetOpenOrders -> CancelOrder all working")
}

// runTestRoundtrip does a market buy 1 MES, then immediately market sells.
// MES tick = $1.25, so worst-case slippage is ~$1.25-$2.50.
func runTestRoundtrip(ctx context.Context, p *topstepxprovider.Provider) {
	fmt.Println("\n=== Test: Round-trip MES ===")
	fmt.Println("  this will execute real trades on MES (Micro E-mini S&P)")
	fmt.Println("  max risk: ~$1.25-$2.50 (1 tick slippage on 1 contract)")

	// Step 1: market buy 1 MES
	fmt.Println("\n  placing market BUY 1 MES...")
	buyResult, err := p.PlaceOrder(ctx, strategy.Order{
		Symbol:    "MES",
		Side:      "buy",
		Qty:       1,
		OrderType: "market",
	})
	if err != nil {
		log.Fatalf("  buy MES: %v", err)
	}
	fmt.Printf("  buy order placed: id=%s\n", buyResult.ID)

	// Brief pause for fill.
	time.Sleep(1 * time.Second)

	// Check position.
	fmt.Println("  checking positions...")
	positions, err := p.GetPositions(ctx)
	if err != nil {
		log.Fatalf("  get positions: %v", err)
	}
	for _, pos := range positions {
		fmt.Printf("  position: %s  qty=%.0f  avgEntry=%.2f\n", pos.Symbol, pos.Qty, pos.AvgEntryPrice)
	}

	// Step 2: market sell 1 MES to flatten
	fmt.Println("\n  placing market SELL 1 MES (flatten)...")
	sellResult, err := p.PlaceOrder(ctx, strategy.Order{
		Symbol:    "MES",
		Side:      "sell",
		Qty:       1,
		OrderType: "market",
	})
	if err != nil {
		log.Fatalf("  sell MES: %v", err)
	}
	fmt.Printf("  sell order placed: id=%s\n", sellResult.ID)

	// Brief pause for fill.
	time.Sleep(1 * time.Second)

	// Verify flat.
	fmt.Println("  checking positions after flatten...")
	positions, err = p.GetPositions(ctx)
	if err != nil {
		log.Fatalf("  get positions: %v", err)
	}
	if len(positions) == 0 {
		fmt.Println("  position: flat (none)")
	}
	for _, pos := range positions {
		fmt.Printf("  position: %s  qty=%.0f  avgEntry=%.2f\n", pos.Symbol, pos.Qty, pos.AvgEntryPrice)
	}

	// Check final balance.
	acct, err := p.GetAccount(ctx)
	if err != nil {
		log.Fatalf("  get account: %v", err)
	}
	fmt.Printf("  balance after round-trip: $%.2f\n", acct.Cash)
	fmt.Println("  PASS: market buy -> position check -> market sell -> flatten all working")
}
