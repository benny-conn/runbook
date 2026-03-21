package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"brandon-bot/internal/db"
	"brandon-bot/internal/paper"
	"brandon-bot/internal/provider"
	alpacaprovider "brandon-bot/internal/provider/alpaca"
	ibkrprovider "brandon-bot/internal/provider/ibkr"
	"brandon-bot/internal/strategy"
)

func main() {
	stratName    := flag.String("strategy", "ma_crossover", "strategy to run")
	symbolsFlag  := flag.String("symbols", "AAPL", "comma-separated list of symbols")
	capitalFlag  := flag.Float64("capital", 10000, "starting capital in USD")
	timeframeFlag := flag.String("timeframe", "1m", "bar timeframe: 1s, 1m, 5m, 15m, 1h, 1d")
	feedFlag     := flag.String("feed", "iex", "Alpaca feed: iex or sip (ignored for IBKR)")
	providerFlag := flag.String("provider", "alpaca", "data + execution provider: alpaca or ibkr")
	flag.Parse()

	symbols := strings.Split(*symbolsFlag, ",")
	for i, s := range symbols {
		symbols[i] = strings.TrimSpace(strings.ToUpper(s))
	}

	strat, err := resolveStrategy(*stratName)
	if err != nil {
		log.Fatalf("unknown strategy %q: %v", *stratName, err)
	}

	store, err := db.Open()
	if err != nil {
		log.Fatalf("opening database: %v", err)
	}
	defer store.Close()

	var md provider.MarketData
	var exec provider.Execution

	switch *providerFlag {
	case "alpaca":
		p := alpacaprovider.New(*feedFlag)
		md, exec = p, p
	case "ibkr":
		p := ibkrprovider.New()
		md, exec = p, p
	default:
		log.Fatalf("unknown provider %q — use alpaca or ibkr", *providerFlag)
	}

	cfg := paper.DefaultConfig(*capitalFlag, *timeframeFlag)
	engine := paper.NewEngine(strat, md, exec, store, cfg)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("starting paper trading | provider=%s strategy=%s symbols=%s timeframe=%s capital=%.2f",
		*providerFlag, *stratName, strings.Join(symbols, ","), *timeframeFlag, *capitalFlag)

	if err := engine.Run(ctx, symbols); err != nil && err != context.Canceled {
		log.Fatalf("engine stopped: %v", err)
	}

	fmt.Println("shutdown complete")
}

func resolveStrategy(name string) (strategy.Strategy, error) {
	switch name {
	case "ma_crossover":
		return strategy.NewMACrossover(), nil
	case "rsi_pullback":
		return strategy.NewRSIPullback(), nil
	default:
		return nil, fmt.Errorf("available strategies: ma_crossover, rsi_pullback")
	}
}
