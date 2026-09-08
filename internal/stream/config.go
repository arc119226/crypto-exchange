package stream

import "time"

// Config is what a deployment decides about the stream role. Zero values
// take the defaults of docs/plan-v1.0.md §7.5.
type Config struct {
	// WriteBuffer is the per-connection send queue; a client that lets it
	// fill is disconnected.
	WriteBuffer int
	// PingInterval and PongTimeout bound a silent peer: a ping every
	// interval, and a pong that does not arrive in time closes the socket.
	PingInterval time.Duration
	PongTimeout  time.Duration
	WriteTimeout time.Duration
	// MaxMessageBytes caps a client frame; the protocol needs far less.
	MaxMessageBytes int64
	// AuthTimeout is how long a private connection may stay unauthenticated.
	AuthTimeout time.Duration
	// ResumeWindow is how long after the auth acknowledgement a resume is
	// still accepted; live frames are held meanwhile.
	ResumeWindow time.Duration
	// ResumePageSize is the outbox page read per round trip during a replay.
	ResumePageSize int32
	// MaxSubscriptions caps public subscriptions per connection.
	MaxSubscriptions int
	// DepthLevels bounds the levels per side in a depth snapshot.
	DepthLevels int
	// FlushInterval is the last-resort close of an open engine command in
	// the shadow book; see marketdata.BookProjector.
	FlushInterval time.Duration
	// TickerInterval and KlineInterval coalesce pushes per market.
	TickerInterval time.Duration
	KlineInterval  time.Duration
	// SnapshotInterval bounds how often a market's depth is written to the
	// cache the api role reads.
	SnapshotInterval time.Duration
	// AllowedOrigins are the Origin hosts accepted on upgrade ("*" for any;
	// empty for same host only). No cookie is involved, so this bounds
	// resource use, not credentials.
	AllowedOrigins []string
}

func (c Config) withDefaults() Config {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	if c.WriteBuffer <= 0 {
		c.WriteBuffer = 256
	}
	def(&c.PingInterval, 15*time.Second)
	def(&c.PongTimeout, 15*time.Second)
	def(&c.WriteTimeout, 5*time.Second)
	if c.MaxMessageBytes <= 0 {
		c.MaxMessageBytes = 4096
	}
	def(&c.AuthTimeout, 5*time.Second)
	def(&c.ResumeWindow, time.Second)
	if c.ResumePageSize <= 0 {
		c.ResumePageSize = 500
	}
	if c.MaxSubscriptions <= 0 {
		c.MaxSubscriptions = 64
	}
	if c.DepthLevels <= 0 {
		c.DepthLevels = 200
	}
	def(&c.FlushInterval, 50*time.Millisecond)
	def(&c.TickerInterval, time.Second)
	def(&c.KlineInterval, 250*time.Millisecond)
	def(&c.SnapshotInterval, 100*time.Millisecond)
	return c
}

func (c Config) allowsAnyOrigin() bool {
	for _, o := range c.AllowedOrigins {
		if o == "*" {
			return true
		}
	}
	return false
}
