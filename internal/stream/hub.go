package stream

import (
	"sync"
)

// Hub is the subscription index: which connections want which topic, and
// which belong to which account. Publishing encodes once and hands the
// bytes to every subscriber's buffer without blocking.
type Hub struct {
	mu       sync.RWMutex
	topics   map[string]map[*conn]struct{}
	accounts map[string]map[*conn]struct{}
	conns    map[*conn]struct{}
	metrics  *Metrics
}

// NewHub returns an empty hub.
func NewHub(m *Metrics) *Hub {
	return &Hub{topics: map[string]map[*conn]struct{}{}, accounts: map[string]map[*conn]struct{}{}, conns: map[*conn]struct{}{}, metrics: m}
}

func (h *Hub) add(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[c] = struct{}{}
}

// remove forgets a connection everywhere.
func (h *Hub) remove(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, c)
	for t := range c.subs {
		if set := h.topics[t]; set != nil {
			delete(set, c)
			if len(set) == 0 {
				delete(h.topics, t)
			}
		}
	}
	if c.account != "" {
		if set := h.accounts[c.account]; set != nil {
			delete(set, c)
			if len(set) == 0 {
				delete(h.accounts, c.account)
			}
		}
	}
}

// subscribe adds c to a topic; it reports false when c already had it.
func (h *Hub) subscribe(c *conn, t string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := c.subs[t]; ok {
		return false
	}
	c.subs[t] = struct{}{}
	set := h.topics[t]
	if set == nil {
		set = map[*conn]struct{}{}
		h.topics[t] = set
	}
	set[c] = struct{}{}
	return true
}

func (h *Hub) unsubscribe(c *conn, t string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := c.subs[t]; !ok {
		return false
	}
	delete(c.subs, t)
	if set := h.topics[t]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.topics, t)
		}
	}
	return true
}

// bind attaches c to an account's private feed.
func (h *Hub) bind(c *conn, account string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c.account = account
	set := h.accounts[account]
	if set == nil {
		set = map[*conn]struct{}{}
		h.accounts[account] = set
	}
	set[c] = struct{}{}
}

// Publish queues one encoded message to every subscriber of the topic.
// channel labels the metrics.
func (h *Hub) Publish(t string, channel string, b []byte) int {
	h.mu.RLock()
	set := h.topics[t]
	targets := make([]*conn, 0, len(set))
	for c := range set {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.send(b)
	}
	h.metrics.queued(channel, len(targets))
	return len(targets)
}

// publishAccount queues a private message to every connection of an
// account. fn lets the caller decide per connection (held, filtered).
func (h *Hub) publishAccount(account string, fn func(c *conn)) int {
	h.mu.RLock()
	set := h.accounts[account]
	targets := make([]*conn, 0, len(set))
	for c := range set {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	for _, c := range targets {
		fn(c)
	}
	return len(targets)
}

// Subscribers counts the connections on a topic.
func (h *Hub) Subscribers(t string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.topics[t])
}

// Connections counts open connections.
func (h *Hub) Connections() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

// closeAll asks every connection to close with the given reason.
func (h *Hub) closeAll(code int, reason string) {
	h.mu.RLock()
	targets := make([]*conn, 0, len(h.conns))
	for c := range h.conns {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.closeWith(code, reason)
	}
}
