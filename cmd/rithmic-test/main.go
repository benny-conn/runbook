// Command rithmic-test exercises Rithmic APIs for integration testing.
//
// Usage:
//
//	go run ./cmd/rithmic-test [flags]
//
// Credentials come from configs/default.json or env vars
// (RITHMIC_URI, RITHMIC_SYSTEM_NAME, RITHMIC_USERNAME, RITHMIC_PASSWORD, RITHMIC_ACCOUNT_ID).
//
// Flags:
//
//	-symbol    Symbol to test (default "ESU5")
//	-bars      Fetch historical bars (default true)
//	-quotes    Stream live quotes for a few seconds (default true)
//	-trades    Stream live trades for a few seconds (default false)
//	-orders    Test order plant login (default false)
//	-seconds   How many seconds to stream (default 5)
//	-diag      Run plant login diagnostics only (test each plant)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/benny-conn/runbook/provider"
	"github.com/benny-conn/runbook/providers/rithmic"
)

func main() {
	symbol := flag.String("symbol", "ESU5", "symbol to test (use front-month contract, e.g. ESU5)")
	doBars := flag.Bool("bars", true, "fetch historical bars")
	doQuotes := flag.Bool("quotes", true, "stream live quotes")
	doTrades := flag.Bool("trades", false, "stream live trades")
	doOrders := flag.Bool("orders", false, "test order plant login")
	doDiag := flag.Bool("diag", false, "run plant login diagnostics only")
	doSystems := flag.Bool("systems", false, "list available systems and exit")
	seconds := flag.Int("seconds", 5, "seconds to stream quotes/trades")
	configPath := flag.String("config", "configs/default.json", "path to config JSON")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Load config
	cfg := rithmic.Config{}
	if data, err := os.ReadFile(*configPath); err == nil {
		var full map[string]json.RawMessage
		if err := json.Unmarshal(data, &full); err == nil {
			if raw, ok := full["rithmic"]; ok {
				json.Unmarshal(raw, &cfg)
			}
		}
	}

	fmt.Printf("=== Rithmic Test ===\n")
	fmt.Printf("  URI:    %s\n", cfg.URI)
	fmt.Printf("  System: %s\n", cfg.SystemName)
	fmt.Printf("  User:   %s\n", cfg.Username)
	fmt.Printf("  Acct:   %s\n", cfg.AccountID)
	fmt.Printf("  Exch:   %s\n", cfg.Exchange)
	fmt.Printf("  Symbol: %s\n", *symbol)
	fmt.Println()

	p := rithmic.New(cfg)
	defer p.Close()

	// --- List available systems ---
	if *doSystems {
		fmt.Println("=== Available Systems ===")
		systems, err := p.ListSystems(ctx)
		if err != nil {
			fmt.Printf("  ERROR: %v\n", err)
		} else if len(systems) == 0 {
			fmt.Println("  (none returned)")
		} else {
			for _, s := range systems {
				fmt.Printf("  %s\n", s)
			}
		}
		fmt.Println("\n=== Done ===")
		return
	}

	// --- Diagnostics mode: test each plant login ---
	if *doDiag {
		fmt.Println("=== Plant Login Diagnostics ===")
		results := p.TestPlantLogin(ctx)
		for _, name := range []string{"TICKER_PLANT", "ORDER_PLANT", "HISTORY_PLANT"} {
			err := results[name]
			if err != nil {
				fmt.Printf("  %-15s FAIL  %v\n", name, err)
			} else {
				fmt.Printf("  %-15s OK\n", name)
			}
		}
		fmt.Println("\n=== Done ===")
		return
	}

	// --- Contract spec (no connection needed) ---
	fmt.Println("=== Contract Spec ===")
	spec, err := p.GetContractSpec(ctx, *symbol)
	if err != nil {
		fmt.Printf("  warning: %v\n", err)
	}
	fmt.Printf("  %s  tick=%.2f  tickVal=$%.2f  pointVal=$%.2f\n",
		spec.Symbol, spec.TickSize, spec.TickValue, spec.PointValue)

	// --- Live quotes (ticker plant) ---
	if *doQuotes {
		fmt.Printf("\n=== Live Quotes (%s, %ds) ===\n", *symbol, *seconds)
		quoteCtx, quoteCancel := context.WithTimeout(ctx, time.Duration(*seconds)*time.Second)
		defer quoteCancel()

		count := 0
		err := p.SubscribeQuotes(quoteCtx, []string{*symbol}, func(q provider.Quote) {
			count++
			if count <= 20 {
				fmt.Printf("  %s  bid=%.2f(%v)  ask=%.2f(%v)\n",
					q.Timestamp.Format("15:04:05.000"),
					q.BidPrice, q.BidSize, q.AskPrice, q.AskSize)
			}
		})
		if err != nil && quoteCtx.Err() == nil {
			fmt.Printf("  ERROR: %v\n", err)
		} else {
			fmt.Printf("  received %d quotes in %ds\n", count, *seconds)
		}
	}

	// --- Live trades (ticker plant) ---
	if *doTrades {
		fmt.Printf("\n=== Live Trades (%s, %ds) ===\n", *symbol, *seconds)
		tradeCtx, tradeCancel := context.WithTimeout(ctx, time.Duration(*seconds)*time.Second)
		defer tradeCancel()

		count := 0
		err := p.SubscribeTrades(tradeCtx, []string{*symbol}, func(t provider.Trade) {
			count++
			if count <= 20 {
				fmt.Printf("  %s  price=%.2f  size=%.0f\n",
					t.Timestamp.Format("15:04:05.000"), t.Price, t.Size)
			}
		})
		if err != nil && tradeCtx.Err() == nil {
			fmt.Printf("  ERROR: %v\n", err)
		} else {
			fmt.Printf("  received %d trades in %ds\n", count, *seconds)
		}
	}

	// --- Historical bars (history plant) ---
	if *doBars {
		fmt.Printf("\n=== Historical Bars (%s, 1h, last 24h) ===\n", *symbol)
		end := time.Now().UTC()
		start := end.Add(-24 * time.Hour)
		bars, err := p.FetchBars(ctx, *symbol, "1h", start, end)
		if err != nil {
			fmt.Printf("  ERROR: %v\n", err)
		} else {
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
	}

	// --- Order plant login (optional) ---
	if *doOrders {
		fmt.Println("\n=== Order Plant Login ===")
		acct, err := p.GetAccount(ctx)
		if err != nil {
			fmt.Printf("  ERROR: %v\n", err)
			fmt.Println("  (this is expected if your test account lacks ORDER_PLANT permissions)")
		} else {
			fmt.Printf("  connected OK (cash=%.2f equity=%.2f)\n", acct.Cash, acct.Equity)
		}
	}

	fmt.Println("\n=== Done ===")
}
