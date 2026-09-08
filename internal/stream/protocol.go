package stream

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/arc119226/crypto-exchange/internal/marketdata"
)

// Client operations.
const (
	OpSubscribe   = "subscribe"
	OpUnsubscribe = "unsubscribe"
	OpPing        = "ping"
	OpAuth        = "auth"
	OpResume      = "resume"
)

// Public channels. kline channels are "kline.<interval>".
const (
	ChannelDepth  = "depth"
	ChannelTrades = "trades"
	ChannelTicker = "ticker"
	klinePrefix   = "kline."
)

// Private channels.
const (
	ChannelOrders      = "orders"
	ChannelFills       = "fills"
	ChannelBalances    = "balances"
	ChannelDeposits    = "deposits"
	ChannelWithdrawals = "withdrawals"
)

// Error codes a client can act on.
const (
	CodeBadMessage           = "bad_message"
	CodeUnknownChannel       = "unknown_channel"
	CodeUnknownMarket        = "unknown_market"
	CodeTooManySubscriptions = "too_many_subscriptions"
	CodeAuthRequired         = "auth_required"
	CodeAuthFailed           = "auth_failed"
	CodeNotAuthenticated     = "not_authenticated"
	CodeResumeTooLate        = "resume_too_late"
	CodeResumeFailed         = "resume_failed"
	CodeNotAvailable         = "not_available"
)

// Close reasons (with close code 1008, policy violation).
const (
	ReasonAuthRequired = "auth_required"
	ReasonAuthFailed   = "auth_failed"
	ReasonSlowConsumer = "slow_consumer"
	ReasonBadMessage   = "bad_message"
	ReasonPongTimeout  = "pong_timeout"
)

// clientMessage is every operation a client may send.
type clientMessage struct {
	Op       string `json:"op"`
	Channel  string `json:"channel,omitempty"`
	Market   string `json:"market,omitempty"`
	Token    string `json:"token,omitempty"`
	SinceSeq *int64 `json:"since_seq,omitempty"`
}

// Control messages the server sends.
type ackMessage struct {
	Type    string `json:"type"`
	Channel string `json:"channel,omitempty"`
	Market  string `json:"market,omitempty"`
}

type errorMessage struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type authAck struct {
	Type       string `json:"type"`
	AccountID  string `json:"account_id"`
	AccountSeq int64  `json:"account_seq"`
}

type resumedAck struct {
	Type     string `json:"type"`
	SinceSeq int64  `json:"since_seq"`
	Replayed int    `json:"replayed"`
}

// Market-data messages.
type depthMessage struct {
	Channel string                  `json:"channel"`
	Type    string                  `json:"type"` // snapshot | delta
	Market  string                  `json:"market"`
	Seq     uint64                  `json:"seq"`
	At      time.Time               `json:"at"`
	Bids    []marketdata.PriceLevel `json:"bids"`
	Asks    []marketdata.PriceLevel `json:"asks"`
}

type tradeMessage struct {
	Channel string      `json:"channel"`
	Type    string      `json:"type"`
	Market  string      `json:"market"`
	Seq     uint64      `json:"seq"`
	At      time.Time   `json:"at"`
	Trade   publicTrade `json:"trade"`
}

type publicTrade struct {
	TradeID    string          `json:"trade_id"`
	Price      string          `json:"price"`
	Qty        string          `json:"qty"`
	QuoteQty   string          `json:"quote_qty"`
	TakerSide  marketdata.Side `json:"taker_side"`
	ExecutedAt time.Time       `json:"executed_at"`
}

type tickerMessage struct {
	Channel string            `json:"channel"`
	Type    string            `json:"type"`
	Market  string            `json:"market"`
	At      time.Time         `json:"at"`
	Ticker  marketdata.Ticker `json:"ticker"`
}

type klineMessage struct {
	Channel string            `json:"channel"`
	Type    string            `json:"type"`
	Market  string            `json:"market"`
	At      time.Time         `json:"at"`
	Candle  marketdata.Candle `json:"candle"`
}

// privateMessage wraps one account event for the private channels.
type privateMessage struct {
	Channel    string          `json:"channel"`
	Type       string          `json:"type"`
	Role       string          `json:"role,omitempty"` // fills: maker | taker
	Seq        *uint64         `json:"seq"`
	AccountSeq *int64          `json:"account_seq"`
	EventID    string          `json:"event_id"`
	OccurredAt time.Time       `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}

// channel is a parsed public channel name.
type channel struct {
	name     string
	interval marketdata.Interval // kline only
}

// parseChannel accepts depth, trades, ticker and kline.<interval>.
func parseChannel(name string) (channel, bool) {
	switch name {
	case ChannelDepth, ChannelTrades, ChannelTicker:
		return channel{name: name}, true
	}
	if strings.HasPrefix(name, klinePrefix) {
		iv, err := marketdata.ParseInterval(strings.TrimPrefix(name, klinePrefix))
		if err == nil {
			return channel{name: name, interval: iv}, true
		}
	}
	return channel{}, false
}

// topic is the hub key of a channel on a market.
func topic(channelName, market string) string { return channelName + "|" + market }

func klineChannel(iv marketdata.Interval) string { return klinePrefix + string(iv) }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// every type above marshals; a failure here is a programming error
		panic("stream: encode: " + err.Error())
	}
	return b
}
