package rithmic

// Hand-rolled protobuf encoding/decoding for the Rithmic R|Protocol API.
//
// Rithmic uses proto2 with very high field numbers (100000+ offset). Rather
// than pulling in a full protobuf dependency, we encode/decode the ~15 message
// types we need using raw varint/length-delimited wire format.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Wire types
const (
	wireVarint  = 0
	wire64Bit   = 1
	wireLenDel  = 2
	wire32Bit   = 5
)

// ---------------------------------------------------------------------------
// Encoder
// ---------------------------------------------------------------------------

type encoder struct {
	buf []byte
}

func (e *encoder) bytes() []byte { return e.buf }

func (e *encoder) putVarint(field uint32, v uint64) {
	if v == 0 {
		return
	}
	e.putTag(field, wireVarint)
	e.appendVarint(v)
}

func (e *encoder) putInt32(field uint32, v int32) {
	if v == 0 {
		return
	}
	e.putTag(field, wireVarint)
	e.appendVarint(uint64(v))
}

func (e *encoder) putInt32Always(field uint32, v int32) {
	e.putTag(field, wireVarint)
	e.appendVarint(uint64(v))
}

func (e *encoder) putBool(field uint32, v bool) {
	if !v {
		return
	}
	e.putTag(field, wireVarint)
	e.appendVarint(1)
}

func (e *encoder) putDouble(field uint32, v float64) {
	if v == 0 {
		return
	}
	e.putTag(field, wire64Bit)
	b := [8]byte{}
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
	e.buf = append(e.buf, b[:]...)
}

func (e *encoder) putString(field uint32, v string) {
	if v == "" {
		return
	}
	e.putTag(field, wireLenDel)
	e.appendVarint(uint64(len(v)))
	e.buf = append(e.buf, v...)
}

func (e *encoder) putRepeatedString(field uint32, vs []string) {
	for _, v := range vs {
		e.putString(field, v)
	}
}

func (e *encoder) putTag(field uint32, wt uint32) {
	e.appendVarint(uint64(field)<<3 | uint64(wt))
}

func (e *encoder) appendVarint(v uint64) {
	var buf [10]byte
	n := binary.PutUvarint(buf[:], v)
	e.buf = append(e.buf, buf[:n]...)
}

// ---------------------------------------------------------------------------
// Decoder
// ---------------------------------------------------------------------------

type decoder struct {
	buf []byte
	pos int
}

func newDecoder(b []byte) *decoder { return &decoder{buf: b} }

func (d *decoder) done() bool { return d.pos >= len(d.buf) }

type field struct {
	num  uint32
	wire uint32
}

func (d *decoder) readTag() (field, error) {
	v, err := d.readVarint()
	if err != nil {
		return field{}, err
	}
	return field{num: uint32(v >> 3), wire: uint32(v & 7)}, nil
}

func (d *decoder) readVarint() (uint64, error) {
	v, n := binary.Uvarint(d.buf[d.pos:])
	if n <= 0 {
		return 0, errors.New("proto: bad varint")
	}
	d.pos += n
	return v, nil
}

func (d *decoder) readFixed64() (uint64, error) {
	if d.pos+8 > len(d.buf) {
		return 0, errors.New("proto: short buffer for fixed64")
	}
	v := binary.LittleEndian.Uint64(d.buf[d.pos : d.pos+8])
	d.pos += 8
	return v, nil
}

func (d *decoder) readFixed32() (uint32, error) {
	if d.pos+4 > len(d.buf) {
		return 0, errors.New("proto: short buffer for fixed32")
	}
	v := binary.LittleEndian.Uint32(d.buf[d.pos : d.pos+4])
	d.pos += 4
	return v, nil
}

func (d *decoder) readBytes() ([]byte, error) {
	n, err := d.readVarint()
	if err != nil {
		return nil, err
	}
	end := d.pos + int(n)
	if end > len(d.buf) {
		return nil, errors.New("proto: short buffer for bytes")
	}
	b := d.buf[d.pos:end]
	d.pos = end
	return b, nil
}

func (d *decoder) readString() (string, error) {
	b, err := d.readBytes()
	return string(b), err
}

func (d *decoder) readDouble() (float64, error) {
	v, err := d.readFixed64()
	return math.Float64frombits(v), err
}

func (d *decoder) skip(wt uint32) error {
	switch wt {
	case wireVarint:
		_, err := d.readVarint()
		return err
	case wire64Bit:
		if d.pos+8 > len(d.buf) {
			return errors.New("proto: short buffer")
		}
		d.pos += 8
	case wireLenDel:
		n, err := d.readVarint()
		if err != nil {
			return err
		}
		d.pos += int(n)
	case wire32Bit:
		if d.pos+4 > len(d.buf) {
			return errors.New("proto: short buffer")
		}
		d.pos += 4
	default:
		return fmt.Errorf("proto: unknown wire type %d", wt)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Field numbers (proto field IDs from Rithmic .proto files)
// ---------------------------------------------------------------------------

const (
	fTemplateID         = 154467
	fTemplateVersion    = 153634
	fUserMsg            = 132760
	fRqHandlerRpCode    = 132764
	fRpCode             = 132766
	fUser               = 131003
	fPassword           = 130004
	fAppName            = 130002
	fAppVersion         = 131803
	fSystemName         = 153628
	fInfraType          = 153621
	fAggregatedQuotes   = 153644
	fFcmID              = 154013
	fIbID               = 154014
	fUniqueUserID       = 153428
	fHeartbeatInterval  = 153633
	fSsboe              = 150100
	fUsecs              = 150101
	fSymbol             = 110100
	fExchange           = 110101
	fRequest            = 100000
	fUpdateBits         = 154211
	fPresenceBits       = 149138
	fClearBits          = 154571
	fIsSnapshot         = 110121
	fTradePrice         = 100006
	fTradeSize          = 100178
	fAggressor          = 112003
	fNetChange          = 100011
	fVolume             = 100032
	fBidPrice           = 100022
	fBidSize            = 100030
	fAskPrice           = 100025
	fAskSize            = 100031
	fAccountID          = 154008
	fAccountName        = 154002
	fUserType           = 154036
	fQuantity           = 112004
	fPrice              = 110306
	fTriggerPrice       = 149247
	fTransactionType    = 112003
	fDuration           = 112005
	fPriceType          = 112008
	fTradeRoute         = 112016
	fManualOrAuto       = 154710
	fBasketID           = 110300
	fNotifyType         = 153625
	fStatus             = 110303
	fAvgFillPrice       = 110322
	fTotalFillSize      = 154111
	fTotalUnfilledSize  = 154112
	fFillPrice          = 110307
	fFillSize           = 110308
	fSubscribeUpdates   = 154352
	fIsDefault          = 154689
	fBarType            = 119200
	fBarSubType         = 119208
	fBarTypeSpecifier   = 148162
	fStartIndex         = 153002
	fFinishIndex        = 153003
	fNumTrades          = 119204
	fBarVolume          = 119205
	fOpenPrice          = 100019
	fClosePrice         = 100021
	fHighPrice          = 100012
	fLowPrice           = 100013
	fDataBarSsboe       = 119202
	fDataBarUsecs       = 119203
	fSourceSsboe        = 150400
	fSourceUsecs        = 150401
	fUserTag            = 154119
	fCompletionReason   = 149273
	fOriginalBasketID   = 154497
	fText               = 120008
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
// Enum constants
// ---------------------------------------------------------------------------

const (
	infraTickerPlant = 1
	infraOrderPlant  = 2
	infraHistPlant   = 3
)

const (
	mdSubscribe   = 1
	mdUnsubscribe = 2
)

const (
	updateLastTrade = 1
	updateBBO       = 2
)

const (
	presenceLastTrade = 1
	presenceBid       = 1
	presenceAsk       = 2
)

const (
	txnBuy  = 1
	txnSell = 2
)

const (
	priceLimit      = 1
	priceMarket     = 2
	priceStopLimit  = 3
	priceStopMarket = 4
)

const (
	durationDay = 1
	durationGTC = 2
)

const (
	orderPlacementAuto = 2
)

// ExchangeOrderNotification notify types
const (
	exchNotifyFill   = 5
	exchNotifyReject = 6
)

// RithmicOrderNotification notify types
const (
	ronComplete          = 15
	ronModificationFailed = 16
	ronCancellationFailed = 17
	ronOpen              = 13
)

// Tick bar types
const (
	barTypeTick = 1
)

const (
	barSubTypeRegular = 1
)

// ---------------------------------------------------------------------------
// Message builders
// ---------------------------------------------------------------------------

func buildRequestLogin(user, password, appName, appVersion, systemName string, infraType int32) []byte {
	var e encoder
	e.putInt32Always(fTemplateID, tplRequestLogin)
	e.putString(fTemplateVersion, "3.9")
	e.putRepeatedString(fUserMsg, []string{"hello"})
	e.putString(fUser, user)
	e.putString(fPassword, password)
	e.putString(fAppName, appName)
	e.putString(fAppVersion, appVersion)
	e.putString(fSystemName, systemName)
	e.putInt32(fInfraType, infraType)
	if infraType == infraTickerPlant {
		e.putBool(fAggregatedQuotes, true)
	}
	return e.bytes()
}

func buildRequestLogout() []byte {
	var e encoder
	e.putInt32Always(fTemplateID, tplRequestLogout)
	e.putRepeatedString(fUserMsg, []string{"goodbye"})
	return e.bytes()
}

func buildRequestHeartbeat() []byte {
	var e encoder
	e.putInt32Always(fTemplateID, tplRequestHeartbeat)
	return e.bytes()
}

func buildRequestSystemInfo() []byte {
	var e encoder
	e.putInt32Always(fTemplateID, tplRequestSystemInfo)
	e.putRepeatedString(fUserMsg, []string{"hello"})
	return e.bytes()
}

func buildRequestMarketData(symbol, exchange string, subscribe bool) []byte {
	var e encoder
	e.putInt32Always(fTemplateID, tplRequestMarketData)
	e.putRepeatedString(fUserMsg, []string{"hello"})
	e.putString(fSymbol, symbol)
	e.putString(fExchange, exchange)
	if subscribe {
		e.putInt32(fRequest, mdSubscribe)
	} else {
		e.putInt32(fRequest, mdUnsubscribe)
	}
	e.putVarint(fUpdateBits, updateLastTrade|updateBBO)
	return e.bytes()
}

func buildRequestAccountList(fcmID, ibID string) []byte {
	var e encoder
	e.putInt32Always(fTemplateID, tplRequestAccountList)
	e.putRepeatedString(fUserMsg, []string{"hello"})
	e.putString(fFcmID, fcmID)
	e.putString(fIbID, ibID)
	return e.bytes()
}

func buildRequestLoginInfo() []byte {
	var e encoder
	e.putInt32Always(fTemplateID, tplRequestLoginInfo)
	e.putRepeatedString(fUserMsg, []string{"hello"})
	return e.bytes()
}

func buildRequestTradeRoutes() []byte {
	var e encoder
	e.putInt32Always(fTemplateID, tplRequestTradeRoutes)
	e.putRepeatedString(fUserMsg, []string{"hello"})
	e.putBool(fSubscribeUpdates, false)
	return e.bytes()
}

func buildRequestSubscribeOrderUpdates(fcmID, ibID, accountID string) []byte {
	var e encoder
	e.putInt32Always(fTemplateID, tplRequestOrderUpdates)
	e.putRepeatedString(fUserMsg, []string{"hello"})
	e.putString(fFcmID, fcmID)
	e.putString(fIbID, ibID)
	e.putString(fAccountID, accountID)
	return e.bytes()
}

func buildRequestNewOrder(fcmID, ibID, accountID, symbol, exchange, tradeRoute string,
	qty int32, txnType int32, priceType int32, price, triggerPrice float64, userTag string) []byte {
	var e encoder
	e.putInt32Always(fTemplateID, tplRequestNewOrder)
	e.putRepeatedString(fUserMsg, []string{"hello"})
	e.putString(fUserTag, userTag)
	e.putString(fFcmID, fcmID)
	e.putString(fIbID, ibID)
	e.putString(fAccountID, accountID)
	e.putString(fSymbol, symbol)
	e.putString(fExchange, exchange)
	e.putInt32(fQuantity, qty)
	e.putInt32(fTransactionType, txnType)
	e.putInt32(fDuration, durationDay)
	e.putInt32(fPriceType, priceType)
	e.putDouble(fPrice, price)
	e.putDouble(fTriggerPrice, triggerPrice)
	e.putString(fTradeRoute, tradeRoute)
	e.putInt32(fManualOrAuto, orderPlacementAuto)
	return e.bytes()
}

func buildRequestTickBarReplay(symbol, exchange string, startSsboe, finishSsboe int32) []byte {
	var e encoder
	e.putInt32Always(fTemplateID, tplRequestTickBarReplay)
	e.putRepeatedString(fUserMsg, []string{"hello"})
	e.putString(fSymbol, symbol)
	e.putString(fExchange, exchange)
	e.putInt32(fBarType, barTypeTick)
	e.putString(fBarTypeSpecifier, "1")
	e.putInt32(fBarSubType, barSubTypeRegular)
	e.putInt32(fStartIndex, startSsboe)
	e.putInt32(fFinishIndex, finishSsboe)
	return e.bytes()
}

// ---------------------------------------------------------------------------
// Generic message reader — extracts template_id plus common fields
// ---------------------------------------------------------------------------

type protoMsg struct {
	TemplateID       int32
	UserMsg          []string
	RpCode           []string
	RqHandlerRpCode  []string

	// Login response
	FcmID            string
	IbID             string
	UniqueUserID     string
	HeartbeatInterval float64
	SystemNames      []string

	// Account list
	AccountID   string
	AccountName string

	// Trade routes
	Exchange   string
	TradeRoute string
	IsDefault  bool
	RouteStatus string

	// Market data
	Symbol       string
	PresenceBits uint32
	ClearBits    uint32
	IsSnapshot   bool

	// Last trade
	TradePrice float64
	TradeSize  int32
	Aggressor  int32

	// BBO
	BidPrice float64
	BidSize  int32
	AskPrice float64
	AskSize  int32

	// Timestamps
	Ssboe int32
	Usecs int32

	// Order response
	BasketID string
	UserTag  string

	// Order notifications
	NotifyType       int32
	Status           string
	Quantity         int32
	Price            float64
	TriggerPrice     float64
	TransactionType  int32
	PriceTypeVal     int32
	AvgFillPrice     float64
	TotalFillSize    int32
	TotalUnfilledSize int32
	CompletionReason string
	Text             string
	OriginalBasketID string

	// Exchange order notification
	FillPrice float64
	FillSize  int32

	// Tick bar replay
	OpenPrice  float64
	HighPrice  float64
	LowPrice   float64
	ClosePrice float64
	BarVolume  uint64
	NumTrades  uint64
	DataBarSsboe []int32
	DataBarUsecs []int32
}

func decodeMsg(b []byte) (*protoMsg, error) {
	d := newDecoder(b)
	m := &protoMsg{}

	for !d.done() {
		f, err := d.readTag()
		if err != nil {
			return m, err
		}

		switch f.num {
		case fTemplateID:
			v, err := d.readVarint()
			if err != nil {
				return m, err
			}
			m.TemplateID = int32(v)

		case fUserMsg:
			s, err := d.readString()
			if err != nil {
				return m, err
			}
			m.UserMsg = append(m.UserMsg, s)

		case fRpCode:
			s, err := d.readString()
			if err != nil {
				return m, err
			}
			m.RpCode = append(m.RpCode, s)

		case fRqHandlerRpCode:
			s, err := d.readString()
			if err != nil {
				return m, err
			}
			m.RqHandlerRpCode = append(m.RqHandlerRpCode, s)

		case fFcmID:
			m.FcmID, _ = d.readString()
		case fIbID:
			m.IbID, _ = d.readString()
		case fUniqueUserID:
			m.UniqueUserID, _ = d.readString()
		case fHeartbeatInterval:
			m.HeartbeatInterval, _ = d.readDouble()

		case fSystemName:
			s, _ := d.readString()
			m.SystemNames = append(m.SystemNames, s)

		case fAccountID:
			m.AccountID, _ = d.readString()
		case fAccountName:
			m.AccountName, _ = d.readString()

		case fExchange:
			m.Exchange, _ = d.readString()
		case fTradeRoute:
			m.TradeRoute, _ = d.readString()
		case fIsDefault:
			v, _ := d.readVarint()
			m.IsDefault = v != 0
		case fStatus:
			m.Status, _ = d.readString()

		case fSymbol:
			m.Symbol, _ = d.readString()
		case fPresenceBits:
			v, _ := d.readVarint()
			m.PresenceBits = uint32(v)
		case fClearBits:
			v, _ := d.readVarint()
			m.ClearBits = uint32(v)
		case fIsSnapshot:
			v, _ := d.readVarint()
			m.IsSnapshot = v != 0

		case fTradePrice:
			m.TradePrice, _ = d.readDouble()
		case fTradeSize:
			v, _ := d.readVarint()
			m.TradeSize = int32(v)

		case fBidPrice:
			m.BidPrice, _ = d.readDouble()
		case fBidSize:
			v, _ := d.readVarint()
			m.BidSize = int32(v)
		case fAskPrice:
			m.AskPrice, _ = d.readDouble()
		case fAskSize:
			v, _ := d.readVarint()
			m.AskSize = int32(v)

		case fSsboe:
			v, _ := d.readVarint()
			m.Ssboe = int32(v)
		case fUsecs:
			v, _ := d.readVarint()
			m.Usecs = int32(v)

		case fBasketID:
			m.BasketID, _ = d.readString()
		case fUserTag:
			m.UserTag, _ = d.readString()

		case fNotifyType:
			v, _ := d.readVarint()
			m.NotifyType = int32(v)
		case fQuantity:
			v, _ := d.readVarint()
			m.Quantity = int32(v)
		case fPrice:
			m.Price, _ = d.readDouble()
		case fTriggerPrice:
			m.TriggerPrice, _ = d.readDouble()
		// fTransactionType == fAggressor (both 112003)
		case fPriceType:
			v, _ := d.readVarint()
			m.PriceTypeVal = int32(v)
		case fAvgFillPrice:
			m.AvgFillPrice, _ = d.readDouble()
		case fTotalFillSize:
			v, _ := d.readVarint()
			m.TotalFillSize = int32(v)
		case fTotalUnfilledSize:
			v, _ := d.readVarint()
			m.TotalUnfilledSize = int32(v)
		case fCompletionReason:
			m.CompletionReason, _ = d.readString()
		case fText:
			m.Text, _ = d.readString()
		case fOriginalBasketID:
			m.OriginalBasketID, _ = d.readString()

		case fFillPrice:
			m.FillPrice, _ = d.readDouble()
		case fFillSize:
			v, _ := d.readVarint()
			m.FillSize = int32(v)

		case fOpenPrice:
			m.OpenPrice, _ = d.readDouble()
		case fHighPrice:
			m.HighPrice, _ = d.readDouble()
		case fLowPrice:
			m.LowPrice, _ = d.readDouble()
		case fClosePrice:
			m.ClosePrice, _ = d.readDouble()
		case fBarVolume:
			v, _ := d.readVarint()
			m.BarVolume = uint64(v)
		case fNumTrades:
			v, _ := d.readVarint()
			m.NumTrades = uint64(v)
		case fDataBarSsboe:
			v, _ := d.readVarint()
			m.DataBarSsboe = append(m.DataBarSsboe, int32(v))
		case fDataBarUsecs:
			v, _ := d.readVarint()
			m.DataBarUsecs = append(m.DataBarUsecs, int32(v))

		default:
			// Handle the fAggressor/fTransactionType overlap
			if f.num == fAggressor {
				v, _ := d.readVarint()
				m.Aggressor = int32(v)
				m.TransactionType = int32(v)
			} else {
				if err := d.skip(f.wire); err != nil {
					return m, err
				}
			}
		}
	}
	return m, nil
}

// templateID extracts just the template_id from a raw protobuf message
// without fully decoding it.
func templateID(b []byte) int32 {
	d := newDecoder(b)
	for !d.done() {
		f, err := d.readTag()
		if err != nil {
			return 0
		}
		if f.num == fTemplateID {
			v, _ := d.readVarint()
			return int32(v)
		}
		if err := d.skip(f.wire); err != nil {
			return 0
		}
	}
	return 0
}
