// Command topstepx-debug queries TopstepX account state for investigation.
//
// Usage:
//
//	go run ./cmd/topstepx-debug --config configs/default.json
//	go run ./cmd/topstepx-debug --config configs/default.json --cancel-orders
//	go run ./cmd/topstepx-debug --config configs/default.json --close-all
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	topstepxprovider "github.com/benny-conn/brandon-bot/providers/topstepx"
	"github.com/benny-conn/brandon-bot/strategy"
)

type config struct {
	TopstepX topstepxprovider.Config `json:"topstepx"`
}

func parseSymbol(raw string) string {
	if strings.HasPrefix(raw, "CON.") {
		parts := strings.Split(raw, ".")
		if len(parts) >= 4 {
			return parts[3]
		}
	}
	return raw
}

func main() {
	configFlag := flag.String("config", "", "path to JSON config file")
	cancelOrders := flag.Bool("cancel-orders", false, "cancel all open orders")
	closeAll := flag.Bool("close-all", false, "flatten all open positions with market orders (market hours only)")
	flag.Parse()

	if *configFlag == "" {
		log.Fatal("--config is required")
	}

	data, err := os.ReadFile(*configFlag)
	if err != nil {
		log.Fatalf("reading config: %v", err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("parsing config: %v", err)
	}

	if cfg.TopstepX.Username == "" || cfg.TopstepX.APIKey == "" {
		log.Fatal("topstepx.username and topstepx.api_key are required in config")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	p := topstepxprovider.New(cfg.TopstepX)

	fmt.Println("=== Authenticating ===")
	if err := p.Authenticate(ctx); err != nil {
		log.Fatalf("auth failed: %v", err)
	}
	fmt.Println("OK")

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

	if cfg.TopstepX.AccountID == 0 {
		fmt.Println("\nNo account_id in config. Add one to investigate further.")
		return
	}

	p.SetAccount(cfg.TopstepX.AccountID)
	fmt.Printf("\n=== Investigating account %d ===\n", cfg.TopstepX.AccountID)

	acct, err := p.GetAccount(ctx)
	if err != nil {
		log.Fatalf("get account: %v", err)
	}
	fmt.Printf("\nBalance: cash=$%.2f  equity=$%.2f\n", acct.Cash, acct.Equity)

	fmt.Println("\n--- Open Positions ---")
	positions, err := p.GetPositions(ctx)
	if err != nil {
		log.Fatalf("get positions: %v", err)
	}
	if len(positions) == 0 {
		fmt.Println("  (none)")
	}
	for _, pos := range positions {
		side := "LONG"
		if pos.Qty < 0 {
			side = "SHORT"
		}
		fmt.Printf("  %s  %s  qty=%.0f  avgEntry=$%.2f\n", parseSymbol(pos.Symbol), side, pos.Qty, pos.AvgEntryPrice)
	}

	fmt.Println("\n--- Open Orders ---")
	orders, err := p.GetOpenOrders(ctx)
	if err != nil {
		log.Fatalf("get open orders: %v", err)
	}
	if len(orders) == 0 {
		fmt.Println("  (none)")
	}
	for _, o := range orders {
		fmt.Printf("  id=%s  %s %s  qty=%.0f  filled=%.0f  type=%s  limit=%.2f  stop=%.2f\n",
			o.ID, o.Side, parseSymbol(o.Symbol), o.Qty, o.Filled, o.OrderType, o.LimitPrice, o.StopPrice)
	}

	// Cancel all open orders
	if *cancelOrders && len(orders) > 0 {
		fmt.Println("\n=== Cancelling All Open Orders ===")
		for _, o := range orders {
			fmt.Printf("  cancelling order %s (%s %s qty=%.0f) ...\n",
				o.ID, o.Side, parseSymbol(o.Symbol), o.Qty)
			if err := p.CancelOrder(ctx, o.ID); err != nil {
				log.Printf("  ERROR cancelling %s: %v", o.ID, err)
				continue
			}
			fmt.Printf("  cancelled OK\n")
		}

		time.Sleep(1 * time.Second)
		fmt.Println("\n--- Orders After Cancel ---")
		remaining, err := p.GetOpenOrders(ctx)
		if err != nil {
			log.Fatalf("get open orders: %v", err)
		}
		if len(remaining) == 0 {
			fmt.Println("  (none) — all orders cancelled")
		}
		for _, o := range remaining {
			fmt.Printf("  WARNING: id=%s still open\n", o.ID)
		}
	}

	// Close all positions (market hours only)
	if *closeAll && len(positions) > 0 {
		fmt.Println("\n=== Closing All Positions ===")

		// CME futures: Sun 5pm CT - Fri 5pm CT, daily maintenance 4-5pm CT
		ct, _ := time.LoadLocation("America/Chicago")
		now := time.Now().In(ct)
		hour, min, wd := now.Hour(), now.Minute(), now.Weekday()
		marketOpen := true
		if wd == time.Saturday {
			marketOpen = false
		} else if wd == time.Sunday && (hour < 17) {
			marketOpen = false
		} else if wd == time.Friday && (hour > 16 || (hour == 16 && min > 0)) {
			marketOpen = false
		} else if hour == 16 { // daily maintenance break 4-5pm CT
			marketOpen = false
		}

		if !marketOpen {
			fmt.Println("  ERROR: Market appears to be CLOSED right now.")
			fmt.Printf("  Current time: %s CT (%s)\n", now.Format("Mon 15:04"), wd)
			fmt.Println("  CME futures: Sun 5pm CT - Fri 4pm CT (break 4-5pm CT daily)")
			fmt.Println("  Use --cancel-orders to cancel any pending orders instead.")
			fmt.Println("  Aborting.")
		} else {
			for _, pos := range positions {
				sym := parseSymbol(pos.Symbol)
				side := "sell"
				qty := pos.Qty
				if pos.Qty < 0 {
					side = "buy"
					qty = math.Abs(pos.Qty)
				}

				fmt.Printf("  closing %s: market %s qty=%.0f ...\n", sym, side, qty)
				result, err := p.PlaceOrder(ctx, strategy.Order{
					Symbol:    sym,
					Side:      side,
					Qty:       qty,
					OrderType: "market",
				})
				if err != nil {
					log.Printf("  ERROR closing %s: %v", sym, err)
					continue
				}
				fmt.Printf("  order placed: id=%s\n", result.ID)
			}

			time.Sleep(2 * time.Second)

			fmt.Println("\n--- Positions After Close ---")
			remaining, err := p.GetPositions(ctx)
			if err != nil {
				log.Fatalf("get positions: %v", err)
			}
			if len(remaining) == 0 {
				fmt.Println("  FLAT — all positions closed")
			}
			for _, pos := range remaining {
				fmt.Printf("  WARNING: %s still open qty=%.0f\n", parseSymbol(pos.Symbol), pos.Qty)
			}
		}
	}

	fmt.Println("\n=== Done ===")
}
