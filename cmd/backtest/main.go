package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/benny-conn/brandon-bot/backtest"
	"github.com/benny-conn/brandon-bot/internal/db"
	"github.com/benny-conn/brandon-bot/provider"
	alpacaprovider "github.com/benny-conn/brandon-bot/providers/alpaca"
	coinbaseprovider "github.com/benny-conn/brandon-bot/providers/coinbase"
	kalshiprovider "github.com/benny-conn/brandon-bot/providers/kalshi"
	massiveprovider "github.com/benny-conn/brandon-bot/providers/massive"
	topstepxprovider "github.com/benny-conn/brandon-bot/providers/topstepx"
	"github.com/benny-conn/brandon-bot/strategies/script"
	"github.com/benny-conn/brandon-bot/strategy"
)

func main() {
	stratName := flag.String("strategy", "script", "strategy to run (script)")
	symbolsFlag := flag.String("symbols", "AAPL", "comma-separated list of symbols")
	fromFlag := flag.String("from", "", "start date (YYYY-MM-DD)")
	toFlag := flag.String("to", "", "end date (YYYY-MM-DD)")
	capital := flag.Float64("capital", 10000, "starting capital in USD")
	feedFlag := flag.String("feed", "iex", "Alpaca feed: iex or sip")
	dataProviderFlag := flag.String("data-provider", "alpaca", "market data provider: alpaca, massive, coinbase, or kalshi")
	scriptFlag := flag.String("script", "", "path to a .js script file (required when --strategy=script)")
	configFlag := flag.String("config", "", "path to JSON config file for the strategy")
	flag.Parse()

	symbols := strings.Split(*symbolsFlag, ",")
	for i, s := range symbols {
		symbols[i] = strings.TrimSpace(strings.ToUpper(s))
	}

	var strategyCfg json.RawMessage
	if *configFlag != "" {
		data, err := os.ReadFile(*configFlag)
		if err != nil {
			log.Fatalf("reading config file: %v", err)
		}
		var wrapper struct {
			Strategy json.RawMessage `json:"strategy"`
		}
		if err := json.Unmarshal(data, &wrapper); err != nil {
			// Treat the whole file as strategy config.
			strategyCfg = data
		} else if len(wrapper.Strategy) > 0 {
			strategyCfg = wrapper.Strategy
		} else {
			strategyCfg = data
		}
		fmt.Printf("Loaded config from %s\n", *configFlag)
	}

	strat, err := resolveStrategy(*stratName, *scriptFlag, strategyCfg, *capital)
	if err != nil {
		log.Fatalf("unknown strategy %q: %v", *stratName, err)
	}
	// Get timeframe from the strategy (single source of truth).
	timeframes := strat.Timeframes()
	if len(timeframes) == 0 {
		log.Fatal("strategy Timeframes() must return at least one timeframe")
	}
	baseTimeframe := timeframes[0]
	// Use the finest (shortest) timeframe for data fetching.
	for _, tf := range timeframes[1:] {
		if backtest.DefaultDuration(tf) < backtest.DefaultDuration(baseTimeframe) {
			baseTimeframe = tf
		}
	}

	// Default date range based on timeframe when --from/--to are omitted.
	var from, to time.Time
	now := time.Now().UTC().Truncate(24 * time.Hour)

	if *toFlag == "" {
		to = now.Add(24*time.Hour - time.Second)
	} else {
		to, err = time.ParseInLocation("2006-01-02", *toFlag, time.UTC)
		if err != nil {
			log.Fatalf("invalid --to date: %v", err)
		}
		to = to.Add(24*time.Hour - time.Second)
	}

	if *fromFlag == "" {
		dur := backtest.DefaultDuration(baseTimeframe)
		from = to.Add(-dur)
	} else {
		from, err = time.ParseInLocation("2006-01-02", *fromFlag, time.UTC)
		if err != nil {
			log.Fatalf("invalid --from date: %v", err)
		}
	}

	fmt.Printf("Fetching %s bars for %s from %s to %s (provider=%s)...\n",
		baseTimeframe, strings.Join(symbols, ", "),
		from.Format("2006-01-02"), to.Format("2006-01-02"), *dataProviderFlag)

	// Parse provider configs from the config file if available.
	var providerCfg struct {
		Alpaca   alpacaprovider.Config   `json:"alpaca"`
		TopstepX topstepxprovider.Config `json:"topstepx"`
		Coinbase coinbaseprovider.Config `json:"coinbase"`
		Kalshi   kalshiprovider.Config   `json:"kalshi"`
		Massive  massiveprovider.Config  `json:"massive"`
	}
	if *configFlag != "" {
		data, _ := os.ReadFile(*configFlag)
		json.Unmarshal(data, &providerCfg)
	}

	var md provider.MarketData
	switch *dataProviderFlag {
	case "alpaca":
		if providerCfg.Alpaca.Feed == "" {
			providerCfg.Alpaca.Feed = *feedFlag
		}
		md = alpacaprovider.New(providerCfg.Alpaca)
	case "massive":
		md = massiveprovider.New(providerCfg.Massive)
	case "topstepx":
		md = topstepxprovider.New(providerCfg.TopstepX)
	case "coinbase":
		md = coinbaseprovider.New(providerCfg.Coinbase)
	case "kalshi":
		md = kalshiprovider.New(providerCfg.Kalshi)
	default:
		log.Fatalf("unknown data provider %q — use alpaca, massive, topstepx, coinbase, or kalshi", *dataProviderFlag)
	}
	bars, err := md.FetchBarsMulti(context.Background(), symbols, baseTimeframe, from, to)
	if err != nil {
		log.Fatalf("fetching historical data: %v", err)
	}
	fmt.Printf("Loaded %d bars\n", len(bars))

	// Convert provider.Bar → strategy.Tick for the backtest engine.
	ticks := make([]strategy.Tick, len(bars))
	for i, b := range bars {
		ticks[i] = strategy.Tick{
			Symbol:    b.Symbol,
			Timestamp: b.Timestamp,
			Open:      b.Open,
			High:      b.High,
			Low:       b.Low,
			Close:     b.Close,
			Volume:    int64(b.Volume),
		}
	}

	// Query contract specs from provider for futures multipliers.
	var opts []backtest.EngineOption
	if csp, ok := md.(provider.ContractSpecProvider); ok {
		multipliers := make(map[string]float64)
		specs := make(map[string]strategy.ContractSpec)
		for _, sym := range symbols {
			spec, err := csp.GetContractSpec(context.Background(), sym)
			if err != nil {
				log.Printf("contract spec for %s: %v (defaulting to equity)", sym, err)
				continue
			}
			specs[sym] = strategy.ContractSpec{
				Symbol: spec.Symbol, TickSize: spec.TickSize,
				TickValue: spec.TickValue, PointValue: spec.PointValue,
			}
			if spec.PointValue > 1.0 {
				multipliers[sym] = spec.PointValue
				fmt.Printf("  %s: point_value=%.2f (tick_size=%.4f tick_value=%.4f)\n",
					sym, spec.PointValue, spec.TickSize, spec.TickValue)
			}
		}
		if len(multipliers) > 0 {
			opts = append(opts, backtest.WithMultipliers(multipliers))
		}
		// Pass contract specs to strategy for getContract() global.
		if csc, ok := strat.(strategy.ContractSpecConsumer); ok {
			csc.SetContractSpecs(specs)
		}
	}

	eng := backtest.NewEngine(strat, *capital, opts...)
	results := eng.Run(ticks)
	results.Print()

	store, err := db.Open()
	if err != nil {
		log.Printf("warning: could not open database, skipping logging: %v", err)
		return
	}
	defer store.Close()

	runID, err := store.SaveBacktestRun(db.BacktestRunParams{
		Strategy:  *stratName,
		Symbols:   symbols,
		Timeframe: baseTimeframe,
		From:      from,
		To:        to,
	}, results)
	if err != nil {
		log.Printf("warning: could not save run to database: %v", err)
		return
	}
	fmt.Printf("\nRun saved to database (id=%d)\n", runID)
}

func resolveStrategy(name, scriptPath string, strategyCfg json.RawMessage, capital float64) (strategy.Strategy, error) {
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
	return script.New(scriptName, string(src), cfg, script.WithCapital(capital))
}
