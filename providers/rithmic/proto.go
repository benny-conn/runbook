package rithmic

// Protobuf encoding/decoding layer for the Rithmic R|Protocol API.
// Uses generated proto2 types from the rti sub-package.

import (
	"fmt"

	"github.com/benny-conn/runbook/providers/rithmic/rti"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// Template IDs
// ---------------------------------------------------------------------------

const (
	tplRequestLogin              = 10
	tplResponseLogin             = 11
	tplRequestLogout             = 12
	tplResponseLogout            = 13
	tplRequestSystemInfo         = 16
	tplResponseSystemInfo        = 17
	tplRequestHeartbeat          = 18
	tplResponseHeartbeat         = 19
	tplRequestMarketData         = 100
	tplResponseMarketData        = 101
	tplLastTrade                 = 150
	tplBestBidOffer              = 151
	tplRequestTickBarReplay      = 206
	tplResponseTickBarReplay     = 207
	tplRequestLoginInfo          = 300
	tplResponseLoginInfo         = 301
	tplRequestAccountList        = 302
	tplResponseAccountList       = 303
	tplRequestOrderUpdates       = 308
	tplResponseOrderUpdates      = 309
	tplRequestTradeRoutes        = 310
	tplResponseTradeRoutes       = 311
	tplRequestNewOrder           = 312
	tplResponseNewOrder          = 313
	tplRithmicOrderNotification  = 351
	tplExchangeOrderNotification = 352
)

// ---------------------------------------------------------------------------
// Infra types
// ---------------------------------------------------------------------------

const (
	infraTickerPlant = int32(rti.RequestLogin_TICKER_PLANT)
	infraOrderPlant  = int32(rti.RequestLogin_ORDER_PLANT)
	infraHistPlant   = int32(rti.RequestLogin_HISTORY_PLANT)
)

// ---------------------------------------------------------------------------
// templateID extracts just the template_id from raw protobuf bytes.
// ---------------------------------------------------------------------------

func templateID(b []byte) int32 {
	mt := &rti.MessageType{}
	if err := proto.Unmarshal(b, mt); err != nil {
		return 0
	}
	return mt.GetTemplateId()
}

// ---------------------------------------------------------------------------
// Decode helpers — unmarshal raw bytes into typed messages
// ---------------------------------------------------------------------------

func decodeResponseLogin(b []byte) (*rti.ResponseLogin, error) {
	msg := &rti.ResponseLogin{}
	return msg, proto.Unmarshal(b, msg)
}

func decodeResponseSystemInfo(b []byte) (*rti.ResponseRithmicSystemInfo, error) {
	msg := &rti.ResponseRithmicSystemInfo{}
	return msg, proto.Unmarshal(b, msg)
}

func decodeResponseLoginInfo(b []byte) (*rti.ResponseLoginInfo, error) {
	msg := &rti.ResponseLoginInfo{}
	return msg, proto.Unmarshal(b, msg)
}

func decodeResponseAccountList(b []byte) (*rti.ResponseAccountList, error) {
	msg := &rti.ResponseAccountList{}
	return msg, proto.Unmarshal(b, msg)
}

func decodeResponseTradeRoutes(b []byte) (*rti.ResponseTradeRoutes, error) {
	msg := &rti.ResponseTradeRoutes{}
	return msg, proto.Unmarshal(b, msg)
}

func decodeResponseNewOrder(b []byte) (*rti.ResponseNewOrder, error) {
	msg := &rti.ResponseNewOrder{}
	return msg, proto.Unmarshal(b, msg)
}

func decodeResponseMarketData(b []byte) (*rti.ResponseMarketDataUpdate, error) {
	msg := &rti.ResponseMarketDataUpdate{}
	return msg, proto.Unmarshal(b, msg)
}

func decodeLastTrade(b []byte) (*rti.LastTrade, error) {
	msg := &rti.LastTrade{}
	return msg, proto.Unmarshal(b, msg)
}

func decodeBestBidOffer(b []byte) (*rti.BestBidOffer, error) {
	msg := &rti.BestBidOffer{}
	return msg, proto.Unmarshal(b, msg)
}

func decodeResponseTickBarReplay(b []byte) (*rti.ResponseTickBarReplay, error) {
	msg := &rti.ResponseTickBarReplay{}
	return msg, proto.Unmarshal(b, msg)
}

func decodeExchangeOrderNotification(b []byte) (*rti.ExchangeOrderNotification, error) {
	msg := &rti.ExchangeOrderNotification{}
	return msg, proto.Unmarshal(b, msg)
}

func decodeRithmicOrderNotification(b []byte) (*rti.RithmicOrderNotification, error) {
	msg := &rti.RithmicOrderNotification{}
	return msg, proto.Unmarshal(b, msg)
}

func decodeResponseOrderUpdates(b []byte) (*rti.ResponseSubscribeForOrderUpdates, error) {
	msg := &rti.ResponseSubscribeForOrderUpdates{}
	return msg, proto.Unmarshal(b, msg)
}

// ---------------------------------------------------------------------------
// rpCodeOK checks whether a response's rp_code indicates success.
// ---------------------------------------------------------------------------

func rpCodeOK(rpCode []string) bool {
	return len(rpCode) == 0 || rpCode[0] == "0"
}

func rpCodeErr(rpCode []string, userMsg []string, context string) error {
	msg := ""
	if len(userMsg) > 0 {
		msg = userMsg[0]
	}
	return fmt.Errorf("rithmic %s: rp_code=%v msg=%s", context, rpCode, msg)
}

// ---------------------------------------------------------------------------
// Message builders
// ---------------------------------------------------------------------------

func mustMarshal(msg proto.Message) []byte {
	b, err := proto.Marshal(msg)
	if err != nil {
		panic(fmt.Sprintf("rithmic: marshal failed: %v", err))
	}
	return b
}

func int32p(v int32) *int32    { return &v }
func float64p(v float64) *float64 { return &v }
func stringp(v string) *string { return &v }
func boolp(v bool) *bool       { return &v }

func buildRequestLogin(user, password, appName, appVersion, systemName string, infraType int32) []byte {
	return mustMarshal(&rti.RequestLogin{
		TemplateId:      int32p(tplRequestLogin),
		TemplateVersion: stringp("3.9"),
		UserMsg:         []string{"hello"},
		User:            stringp(user),
		Password:        stringp(password),
		AppName:         stringp(appName),
		AppVersion:      stringp(appVersion),
		SystemName:      stringp(systemName),
		InfraType:       rti.RequestLogin_SysInfraType(infraType).Enum(),
	})
}

func buildRequestLogout() []byte {
	return mustMarshal(&rti.RequestLogout{
		TemplateId: int32p(tplRequestLogout),
		UserMsg:    []string{"hello"},
	})
}

func buildRequestHeartbeat() []byte {
	return mustMarshal(&rti.RequestHeartbeat{
		TemplateId: int32p(tplRequestHeartbeat),
	})
}

func buildRequestSystemInfo() []byte {
	return mustMarshal(&rti.RequestRithmicSystemInfo{
		TemplateId: int32p(tplRequestSystemInfo),
		UserMsg:    []string{"hello"},
	})
}

func buildRequestMarketData(symbol, exchange string, subscribe bool) []byte {
	req := rti.RequestMarketDataUpdate_SUBSCRIBE
	if !subscribe {
		req = rti.RequestMarketDataUpdate_UNSUBSCRIBE
	}
	bits := uint32(rti.RequestMarketDataUpdate_LAST_TRADE | rti.RequestMarketDataUpdate_BBO)
	return mustMarshal(&rti.RequestMarketDataUpdate{
		TemplateId: int32p(tplRequestMarketData),
		UserMsg:    []string{"hello"},
		Symbol:     stringp(symbol),
		Exchange:   stringp(exchange),
		Request:    req.Enum(),
		UpdateBits: &bits,
	})
}

func buildRequestAccountList(fcmID, ibID string) []byte {
	return mustMarshal(&rti.RequestAccountList{
		TemplateId: int32p(tplRequestAccountList),
		UserMsg:    []string{"hello"},
		FcmId:      stringp(fcmID),
		IbId:       stringp(ibID),
	})
}

func buildRequestLoginInfo() []byte {
	return mustMarshal(&rti.RequestLoginInfo{
		TemplateId: int32p(tplRequestLoginInfo),
		UserMsg:    []string{"hello"},
	})
}

func buildRequestTradeRoutes() []byte {
	return mustMarshal(&rti.RequestTradeRoutes{
		TemplateId: int32p(tplRequestTradeRoutes),
		UserMsg:    []string{"hello"},
	})
}

func buildRequestSubscribeOrderUpdates(fcmID, ibID, accountID string) []byte {
	return mustMarshal(&rti.RequestSubscribeForOrderUpdates{
		TemplateId: int32p(tplRequestOrderUpdates),
		UserMsg:    []string{"hello"},
		FcmId:      stringp(fcmID),
		IbId:       stringp(ibID),
		AccountId:  stringp(accountID),
	})
}

func buildRequestNewOrder(fcmID, ibID, accountID, symbol, exchange, tradeRoute string,
	qty int32, txnType int32, priceType int32, price, triggerPrice float64, userTag string) []byte {
	msg := &rti.RequestNewOrder{
		TemplateId:      int32p(tplRequestNewOrder),
		UserMsg:         []string{"hello"},
		UserTag:         stringp(userTag),
		FcmId:           stringp(fcmID),
		IbId:            stringp(ibID),
		AccountId:       stringp(accountID),
		Symbol:          stringp(symbol),
		Exchange:        stringp(exchange),
		Quantity:        int32p(qty),
		TransactionType: rti.RequestNewOrder_TransactionType(txnType).Enum(),
		Duration:        rti.RequestNewOrder_DAY.Enum(),
		PriceType:       rti.RequestNewOrder_PriceType(priceType).Enum(),
		TradeRoute:      stringp(tradeRoute),
		ManualOrAuto:    rti.RequestNewOrder_AUTO.Enum(),
	}
	if price != 0 {
		msg.Price = float64p(price)
	}
	if triggerPrice != 0 {
		msg.TriggerPrice = float64p(triggerPrice)
	}
	return mustMarshal(msg)
}

func buildRequestTickBarReplay(symbol, exchange string, startSsboe, finishSsboe int32) []byte {
	return mustMarshal(&rti.RequestTickBarReplay{
		TemplateId:    int32p(tplRequestTickBarReplay),
		UserMsg:       []string{"hello"},
		Symbol:        stringp(symbol),
		Exchange:      stringp(exchange),
		BarType:       rti.RequestTickBarReplay_TICK_BAR.Enum(),
		BarTypeSpecifier: stringp("1"),
		BarSubType:    rti.RequestTickBarReplay_REGULAR.Enum(),
		StartIndex:    int32p(startSsboe),
		FinishIndex:   int32p(finishSsboe),
	})
}

// ---------------------------------------------------------------------------
// Enum constants used by rithmic.go
// ---------------------------------------------------------------------------

const (
	txnBuy  = int32(rti.RequestNewOrder_BUY)
	txnSell = int32(rti.RequestNewOrder_SELL)

	priceLimit      = int32(rti.RequestNewOrder_LIMIT)
	priceMarket     = int32(rti.RequestNewOrder_MARKET)
	priceStopLimit  = int32(rti.RequestNewOrder_STOP_LIMIT)
	priceStopMarket = int32(rti.RequestNewOrder_STOP_MARKET)
)
