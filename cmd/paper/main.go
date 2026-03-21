package main

import (
	"context"
	"encoding/json"
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
	tradovateprovider "brandon-bot/internal/provider/tradovate"
	"brandon-bot/internal/strategy"
)

// RunConfig is the top-level structure for a JSON config file.
// Provider sections supply credentials; the strategy section is passed
// directly to the strategy's Configure method.
// Any field left empty falls back to the corresponding environment variable.
type RunConfig struct {
	Alpaca    alpacaprovider.Config    `json:"alpaca"`
	IBKR      ibkrprovider.Config      `json:"ibkr"`
	Tradovate tradovateprovider.Config `json:"tradovate"`
	Strategy  json.RawMessage          `json:"strategy"`
}

func main() {
	stratName     := flag.String("strategy", "ma_crossover", "strategy to run")
	symbolsFlag   := flag.String("symbols", "AAPL", "comma-separated list of symbols")
	capitalFlag   := flag.Float64("capital", 10000, "starting capital in USD")
	timeframeFlag := flag.String("timeframe", "1m", "bar timeframe: 1s, 1m, 5m, 15m, 1h, 1d")
	providerFlag  := flag.String("provider", "alpaca", "data + execution provider: alpaca, ibkr, or tradovate")
	configFlag    := flag.String("config", "", "path to JSON config file (provider credentials + strategy params)")
	flag.Parse()

	symbols := strings.Split(*symbolsFlag, ",")
	for i, s := range symbols {
		symbols[i] = strings.TrimSpace(strings.ToUpper(s))
	}

	var runCfg RunConfig
	if *configFlag != "" {
		data, err := os.ReadFile(*configFlag)
		if err != nil {
			log.Fatalf("reading config file %q: %v", *configFlag, err)
		}
		if err := json.Unmarshal(data, &runCfg); err != nil {
			log.Fatalf("parsing config file %q: %v", *configFlag, err)
		}
	}

	strat, err := resolveStrategy(*stratName)
	if err != nil {
		log.Fatalf("unknown strategy %q: %v", *stratName, err)
	}
	if len(runCfg.Strategy) > 0 {
		if c, ok := strat.(strategy.Configurable); ok {
			if err := c.Configure(runCfg.Strategy); err != nil {
				log.Fatalf("configuring strategy: %v", err)
			}
		}
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
		p := alpacaprovider.New(runCfg.Alpaca)
		md, exec = p, p
	case "ibkr":
		p := ibkrprovider.New(runCfg.IBKR)
		md, exec = p, p
	case "tradovate":
		p := tradovateprovider.New(runCfg.Tradovate)
		md, exec = p, p
	default:
		log.Fatalf("unknown provider %q — use alpaca, ibkr, or tradovate", *providerFlag)
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
	case "five_min_orb":
		return strategy.NewFiveMinuteORB(strategy.FiveMinuteORBConfig{}), nil
	default:
		return nil, fmt.Errorf("available strategies: ma_crossover, rsi_pullback, five_min_orb")
	}
}
