package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/benny-conn/brandon-bot/engine"
	"github.com/benny-conn/brandon-bot/internal/db"
	"github.com/benny-conn/brandon-bot/provider"
	alpacaprovider "github.com/benny-conn/brandon-bot/providers/alpaca"
	coinbaseprovider "github.com/benny-conn/brandon-bot/providers/coinbase"
	ibkrprovider "github.com/benny-conn/brandon-bot/providers/ibkr"
	kalshiprovider "github.com/benny-conn/brandon-bot/providers/kalshi"
	massiveprovider "github.com/benny-conn/brandon-bot/providers/massive"
	topstepxprovider "github.com/benny-conn/brandon-bot/providers/topstepx"
	tradovateprovider "github.com/benny-conn/brandon-bot/providers/tradovate"
	"github.com/benny-conn/brandon-bot/strategies/script"
	"github.com/benny-conn/brandon-bot/strategy"
)

// RunConfig is the top-level structure for a JSON config file.
// Provider sections supply credentials; the strategy section is passed
// directly to the strategy's Configure method.
// Any field left empty falls back to the corresponding environment variable.
type RunConfig struct {
	Alpaca    alpacaprovider.Config    `json:"alpaca"`
	IBKR      ibkrprovider.Config      `json:"ibkr"`
	Tradovate tradovateprovider.Config `json:"tradovate"`
	TopstepX  topstepxprovider.Config  `json:"topstepx"`
	Massive   massiveprovider.Config   `json:"massive"`
	Coinbase  coinbaseprovider.Config  `json:"coinbase"`
	Kalshi    kalshiprovider.Config    `json:"kalshi"`
	Strategy  json.RawMessage          `json:"strategy"`
}

func main() {
	stratName := flag.String("strategy", "script", "strategy to run (script)")
	symbolsFlag := flag.String("symbols", "AAPL", "comma-separated list of symbols")
	capitalFlag := flag.Float64("capital", 10000, "starting capital in USD")
	providerFlag := flag.String("provider", "alpaca", "data + execution provider: alpaca, ibkr, tradovate, topstepx, coinbase, or kalshi")
	dataProviderFlag := flag.String("data-provider", "", "market data provider override: massive, alpaca, ibkr, tradovate, coinbase")
	execProviderFlag := flag.String("exec-provider", "", "execution provider override: alpaca, ibkr, tradovate, coinbase")
	scriptFlag := flag.String("script", "", "path to a .js script file (required when --strategy=script)")
	configFlag := flag.String("config", "", "path to JSON config file (provider credentials + strategy params)")
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

	strat, err := resolveStrategy(*stratName, *scriptFlag, runCfg.Strategy)
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

	// Split provider mode: --data-provider + --exec-provider override --provider.
	if *dataProviderFlag != "" || *execProviderFlag != "" {
		if *dataProviderFlag == "" || *execProviderFlag == "" {
			log.Fatal("--data-provider and --exec-provider must both be specified")
		}
		md = resolveMarketData(*dataProviderFlag, &runCfg)
		exec = resolveExecution(*execProviderFlag, &runCfg)
	} else {
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
		case "topstepx":
			p := topstepxprovider.New(runCfg.TopstepX)
			md, exec = p, p
		case "coinbase":
			p := coinbaseprovider.New(runCfg.Coinbase)
			md, exec = p, p
		case "kalshi":
			p := kalshiprovider.New(runCfg.Kalshi)
			md, exec = p, p
		default:
			log.Fatalf("unknown provider %q — use alpaca, ibkr, tradovate, topstepx, coinbase, or kalshi", *providerFlag)
		}
	}

	cfg := engine.DefaultConfig(*capitalFlag)
	// TopStepX force-closes positions at 4:10 PM ET — always flatten before they do.
	if *providerFlag == "topstepx" || *execProviderFlag == "topstepx" {
		cfg.FlattenAtClose = true
	}
	eng := engine.NewEngine(strat, md, exec, store, cfg)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	providerLabel := *providerFlag
	if *dataProviderFlag != "" {
		providerLabel = fmt.Sprintf("data=%s exec=%s", *dataProviderFlag, *execProviderFlag)
	}
	log.Printf("starting paper trading | provider=%s strategy=%s symbols=%s capital=%.2f",
		providerLabel, *stratName, strings.Join(symbols, ","), *capitalFlag)

	if err := eng.Run(ctx, symbols); err != nil && err != context.Canceled {
		log.Fatalf("engine stopped: %v", err)
	}

	fmt.Println("shutdown complete")
}

func resolveMarketData(name string, cfg *RunConfig) provider.MarketData {
	switch name {
	case "alpaca":
		return alpacaprovider.New(cfg.Alpaca)
	case "ibkr":
		return ibkrprovider.New(cfg.IBKR)
	case "tradovate":
		return tradovateprovider.New(cfg.Tradovate)
	case "topstepx":
		return topstepxprovider.New(cfg.TopstepX)
	case "massive":
		return massiveprovider.New(cfg.Massive)
	case "coinbase":
		return coinbaseprovider.New(cfg.Coinbase)
	case "kalshi":
		return kalshiprovider.New(cfg.Kalshi)
	default:
		log.Fatalf("unknown data provider %q — use massive, alpaca, ibkr, tradovate, topstepx, coinbase, or kalshi", name)
		return nil
	}
}

func resolveExecution(name string, cfg *RunConfig) provider.Execution {
	switch name {
	case "alpaca":
		return alpacaprovider.New(cfg.Alpaca)
	case "ibkr":
		return ibkrprovider.New(cfg.IBKR)
	case "tradovate":
		return tradovateprovider.New(cfg.Tradovate)
	case "topstepx":
		return topstepxprovider.New(cfg.TopstepX)
	case "coinbase":
		return coinbaseprovider.New(cfg.Coinbase)
	case "kalshi":
		return kalshiprovider.New(cfg.Kalshi)
	default:
		log.Fatalf("unknown exec provider %q — use alpaca, ibkr, tradovate, topstepx, coinbase, or kalshi", name)
		return nil
	}
}

func resolveStrategy(name, scriptPath string, strategyCfg json.RawMessage) (strategy.Strategy, error) {
	if name != "script" {
		return nil, fmt.Errorf("unknown strategy %q — all strategies are scripts now; use --script=<file>", name)
	}
	if scriptPath == "" {
		return nil, fmt.Errorf("--script flag is required")
	}
	src, err := os.ReadFile(scriptPath)
	if err != nil {
		return nil, fmt.Errorf("reading script %q: %w", scriptPath, err)
	}
	cfg := make(map[string]string)
	if len(strategyCfg) > 0 {
		var raw map[string]interface{}
		if err := json.Unmarshal(strategyCfg, &raw); err != nil {
			return nil, fmt.Errorf("parsing strategy config: %w", err)
		}
		for k, v := range raw {
			cfg[k] = fmt.Sprint(v)
		}
	}
	scriptName := strings.TrimSuffix(filepath.Base(scriptPath), filepath.Ext(scriptPath))
	log.Printf("script strategy config: script=%s name=%s", scriptPath, scriptName)
	for k, v := range cfg {
		log.Printf("  %s=%s", k, v)
	}
	return script.New(scriptName, string(src), cfg)
}
