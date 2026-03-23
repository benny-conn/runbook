package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
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
	"github.com/benny-conn/brandon-bot/strategies"
	"github.com/benny-conn/brandon-bot/strategy"
)

func main() {
	stratName := flag.String("strategy", "ma_crossover", "strategy to run")
	symbolsFlag := flag.String("symbols", "AAPL", "comma-separated list of symbols")
	fromFlag := flag.String("from", "", "start date (YYYY-MM-DD)")
	toFlag := flag.String("to", "", "end date (YYYY-MM-DD)")
	timeframeFlag := flag.String("timeframe", "1d", "bar timeframe: 1m, 5m, 15m, 1h, 1d")
	capital := flag.Float64("capital", 10000, "starting capital in USD")
	feedFlag := flag.String("feed", "iex", "Alpaca feed: iex or sip")
	dataProviderFlag := flag.String("data-provider", "alpaca", "market data provider: alpaca, massive, coinbase, or kalshi")
	configFlag := flag.String("config", "", "path to JSON config file for the strategy")
	flag.Parse()

	// Default date range based on timeframe when --from/--to are omitted.
	var from, to time.Time
	now := time.Now().UTC().Truncate(24 * time.Hour)

	if *toFlag == "" {
		to = now.Add(24*time.Hour - time.Second)
	} else {
		var err error
		to, err = time.Parse("2006-01-02", *toFlag)
		if err != nil {
			log.Fatalf("invalid --to date: %v", err)
		}
		to = to.Add(24*time.Hour - time.Second)
	}

	if *fromFlag == "" {
		dur := backtest.DefaultDuration(*timeframeFlag)
		from = to.Add(-dur)
	} else {
		var err error
		from, err = time.Parse("2006-01-02", *fromFlag)
		if err != nil {
			log.Fatalf("invalid --from date: %v", err)
		}
	}

	symbols := strings.Split(*symbolsFlag, ",")
	for i, s := range symbols {
		symbols[i] = strings.TrimSpace(strings.ToUpper(s))
	}

	strat, err := resolveStrategy(*stratName)
	if err != nil {
		log.Fatalf("unknown strategy %q: %v", *stratName, err)
	}

	if *configFlag != "" {
		cs, ok := strat.(strategy.Configurable)
		if !ok {
			log.Fatalf("strategy %q does not support --config (does not implement Configurable)", *stratName)
		}
		data, err := os.ReadFile(*configFlag)
		if err != nil {
			log.Fatalf("reading config file: %v", err)
		}
		if err := cs.Configure(data); err != nil {
			log.Fatalf("configuring strategy: %v", err)
		}
		fmt.Printf("Loaded config from %s\n", *configFlag)
	}

	fmt.Printf("Fetching %s bars for %s from %s to %s (provider=%s)...\n",
		*timeframeFlag, strings.Join(symbols, ", "),
		from.Format("2006-01-02"), to.Format("2006-01-02"), *dataProviderFlag)

	var md provider.MarketData
	switch *dataProviderFlag {
	case "alpaca":
		md = alpacaprovider.New(alpacaprovider.Config{Feed: *feedFlag})
	case "massive":
		md = massiveprovider.New(massiveprovider.Config{})
	case "topstepx":
		md = topstepxprovider.New(topstepxprovider.Config{})
	case "coinbase":
		md = coinbaseprovider.New(coinbaseprovider.Config{})
	case "kalshi":
		md = kalshiprovider.New(kalshiprovider.Config{})
	default:
		log.Fatalf("unknown data provider %q — use alpaca, massive, topstepx, coinbase, or kalshi", *dataProviderFlag)
	}
	bars, err := md.FetchBarsMulti(context.Background(), symbols, *timeframeFlag, from, to)
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

	eng := backtest.NewEngine(strat, *capital)
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
		Timeframe: *timeframeFlag,
		From:      from,
		To:        to,
	}, results)
	if err != nil {
		log.Printf("warning: could not save run to database: %v", err)
		return
	}
	fmt.Printf("\nRun saved to database (id=%d)\n", runID)
}

func resolveStrategy(name string) (strategy.Strategy, error) {
	switch name {
	case "ma_crossover":
		return strategies.NewMACrossover(), nil
	case "rsi_pullback":
		return strategies.NewRSIPullback(), nil
	default:
		return nil, fmt.Errorf("available strategies: ma_crossover, rsi_pullback, five_min_orb")
	}
}
