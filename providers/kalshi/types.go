package kalshi

import "time"

// API response wrappers.

type balanceResponse struct {
	Balance float64 `json:"balance"` // USD cents or dollars depending on endpoint
}

type positionsResponse struct {
	MarketPositions []marketPosition `json:"market_positions"`
	Cursor          string           `json:"cursor"`
}

type marketPosition struct {
	Ticker              string `json:"ticker"`
	MarketResult        string `json:"market_result"` // "yes", "no", "all_no", "" (unresolved)
	Position            int    `json:"position"`       // net contract count (positive=yes, negative=no)
	TotalTraded         int    `json:"total_traded"`
	RealizedPnl         int    `json:"realized_pnl"`
	RestingOrdersCount  int    `json:"resting_orders_count"`
	Fees                int    `json:"fees"`
	TotalCost           int    `json:"total_cost"`
	MarketExposure      int    `json:"market_exposure"`
	SettlementPayout    int    `json:"settlement_payout"`
	SettlementFee       int    `json:"settlement_fee"`
}

type ordersResponse struct {
	Orders []apiOrder `json:"orders"`
	Cursor string     `json:"cursor"`
}

type apiOrder struct {
	OrderID         string    `json:"order_id"`
	Ticker          string    `json:"ticker"`
	Action          string    `json:"action"`          // "buy" or "sell"
	Side            string    `json:"side"`             // "yes" or "no"
	Type            string    `json:"type"`             // "limit" or "market"
	YesPrice        int       `json:"yes_price"`        // cents (1-99)
	NoPrice         int       `json:"no_price"`         // cents (1-99)
	Status          string    `json:"status"`           // "resting", "canceled", "executed", "pending"
	RemainingCount  int       `json:"remaining_count"`
	FilledCount     int       `json:"filled_count"`
	PlaceCount      int       `json:"place_count"`
	CreatedTime     time.Time `json:"created_time"`
	ExpirationTime  time.Time `json:"expiration_time"`
	TimeInForce     string    `json:"time_in_force"`
}

type createOrderRequest struct {
	Ticker    string `json:"ticker"`
	Action    string `json:"action"`               // "buy" or "sell"
	Side      string `json:"side"`                  // "yes" or "no"
	Type      string `json:"type"`                  // "limit" or "market"
	Count     int    `json:"count"`                 // number of contracts
	YesPrice  int    `json:"yes_price,omitempty"`   // cents (1-99), for limit orders
	Expiration *time.Time `json:"expiration_time,omitempty"`
}

type createOrderResponse struct {
	Order apiOrder `json:"order"`
}

type candlesticksResponse struct {
	Candlesticks []apiCandlestick `json:"candlesticks"`
}

type apiCandlestick struct {
	EndPeriodTS int             `json:"end_period_ts"` // unix timestamp
	Price       candlestickOHLC `json:"price"`
	Volume      int             `json:"volume"`
	OpenInterest int            `json:"open_interest"`
}

type candlestickOHLC struct {
	Open  float64 `json:"open"`
	High  float64 `json:"high"`
	Low   float64 `json:"low"`
	Close float64 `json:"close"`
}

type orderbookResponse struct {
	Orderbook apiOrderbook `json:"orderbook"`
}

type apiOrderbook struct {
	Yes [][]float64 `json:"yes"` // [[price, quantity], ...]
	No  [][]float64 `json:"no"`
}

// Market discovery types.

type marketsResponse struct {
	Markets []apiMarket `json:"markets"`
	Cursor  string      `json:"cursor"`
}

type apiMarket struct {
	Ticker           string    `json:"ticker"`
	EventTicker      string    `json:"event_ticker"`
	Title            string    `json:"title"`
	Status           string    `json:"status"` // "open", "closed", "settled"
	Volume           int       `json:"volume"`
	Volume24H        int       `json:"volume_24h"`
	OpenTime         time.Time `json:"open_time"`
	CloseTime        time.Time `json:"close_time"`
	LastPrice        int       `json:"last_price"`
	YesBid           int       `json:"yes_bid"`
	YesAsk           int       `json:"yes_ask"`
	OpenInterest     int       `json:"open_interest"`
	Result           string    `json:"result"`
	SeriesTicker     string    `json:"series_ticker"`
}

// WebSocket message types.

type wsCommand struct {
	ID     int         `json:"id"`
	Cmd    string      `json:"cmd"`
	Params interface{} `json:"params"`
}

type wsSubscribeParams struct {
	Channels      []string `json:"channels"`
	MarketTickers []string `json:"market_tickers,omitempty"`
}

type wsMessage struct {
	Type string          `json:"type"`
	Sid  int             `json:"sid"`
	Seq  int             `json:"seq"`
	Msg  wsMessageDetail `json:"msg"`
}

type wsMessageDetail struct {
	// trade fields
	Ticker    string    `json:"ticker"`
	YesPrice  int       `json:"yes_price"`
	NoPrice   int       `json:"no_price"`
	Count     int       `json:"count"`
	TakerSide string   `json:"taker_side"`
	CreatedAt time.Time `json:"ts"`

	// fill fields
	OrderID string `json:"order_id"`
	Action  string `json:"action"` // "buy" or "sell"
	Side    string `json:"side"`   // "yes" or "no"
	IsFill  bool   `json:"is_fill"`

	// ticker (price update) fields
	Price float64 `json:"price"`
	Volume int    `json:"volume"`

	// lifecycle fields
	MarketTicker string `json:"market_ticker"`
	Status       string `json:"status"` // "active", "closed", "settled"
	Result       string `json:"result"` // "yes", "no", "all_no"
}
