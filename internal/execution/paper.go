package execution

import (
	"fmt"
	"os"

	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/shopspring/decimal"

	"brandon-bot/internal/strategy"
)

// PaperExecutor submits orders to the Alpaca paper trading account via REST.
type PaperExecutor struct {
	Client *alpaca.Client
}

func NewPaperExecutor() *PaperExecutor {
	return &PaperExecutor{
		Client: alpaca.NewClient(alpaca.ClientOpts{
			APIKey:    os.Getenv("ALPACA_API_KEY"),
			APISecret: os.Getenv("ALPACA_SECRET"),
			BaseURL:   os.Getenv("ALPACA_BASE_URL"),
		}),
	}
}

// PlaceOrder converts a strategy Order to an Alpaca REST request and submits it.
// Returns the Alpaca order (with its server-assigned ID) on success.
func (e *PaperExecutor) PlaceOrder(order strategy.Order) (*alpaca.Order, error) {
	qty := decimal.NewFromFloat(order.Qty)

	side := alpaca.Buy
	if order.Side == "sell" {
		side = alpaca.Sell
	}

	req := alpaca.PlaceOrderRequest{
		Symbol:      order.Symbol,
		Qty:         &qty,
		Side:        side,
		Type:        alpaca.Market,
		TimeInForce: alpaca.Day,
	}

	if order.OrderType == "limit" && order.LimitPrice > 0 {
		lp := decimal.NewFromFloat(order.LimitPrice)
		req.Type = alpaca.Limit
		req.LimitPrice = &lp
		req.TimeInForce = alpaca.GTC
	}

	placed, err := e.Client.PlaceOrder(req)
	if err != nil {
		return nil, fmt.Errorf("placing %s order for %s qty=%.2f: %w", order.Side, order.Symbol, order.Qty, err)
	}
	return placed, nil
}
