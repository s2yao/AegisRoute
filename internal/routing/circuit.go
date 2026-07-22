package routing

import (
	"sync"
	"time"

	"github.com/example/aegisroute/internal/models"
)

// Breaker is a per-backend circuit breaker keyed by backend name. It stops
// the gateway from hammering a backend that keeps failing: after threshold
// consecutive transient failures the circuit opens and the Selector skips
// that backend; once cooldown has elapsed one probe request is let through
// (half-open); a success closes the circuit, a failure re-opens it.
//
// Only transient failures (timeout, connection error, 502/503/504) may be
// reported as failures — a permanent upstream error (e.g. 400) proves the
// backend is alive and must be reported as a success.
//
// All methods are safe for concurrent use.
type Breaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time
	onChange  func(backend string, state models.CircuitState)

	mu       sync.Mutex
	circuits map[string]*circuit
}

// circuit is the per-backend state. failures counts consecutive transient
// failures while closed; probing marks the single in-flight half-open probe.
type circuit struct {
	state    models.CircuitState
	failures int
	openedAt time.Time
	probing  bool
}

// BreakerOption customizes a Breaker at construction.
type BreakerOption func(*Breaker)

// WithBreakerClock injects the time source, so tests can drive the
// open → half-open cooldown without sleeping.
func WithBreakerClock(now func() time.Time) BreakerOption {
	return func(b *Breaker) { b.now = now }
}

// WithStateListener registers a hook invoked (outside handler hot paths but
// under the breaker lock) on every state transition, so the composition root
// can mirror circuit state into the aegisroute_circuit_breaker_state gauge.
func WithStateListener(fn func(backend string, state models.CircuitState)) BreakerOption {
	return func(b *Breaker) { b.onChange = fn }
}

// NewBreaker builds a Breaker that opens a backend's circuit after threshold
// consecutive transient failures and half-opens it cooldown later.
func NewBreaker(threshold int, cooldown time.Duration, opts ...BreakerOption) *Breaker {
	b := &Breaker{
		threshold: threshold,
		cooldown:  cooldown,
		now:       time.Now,
		circuits:  map[string]*circuit{},
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Allow reports whether a request may be sent to the named backend, and
// reserves the half-open probe slot when it grants one. Closed always
// allows; open allows nothing until cooldown has elapsed, at which point the
// circuit half-opens and exactly one caller is admitted as the probe;
// further half-open callers are refused until the probe's outcome is
// reported via ReportSuccess or ReportFailure.
func (b *Breaker) Allow(backend string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.circuit(backend)

	switch c.state {
	case models.CircuitStateClosed:
		return true
	case models.CircuitStateOpen:
		if b.now().Sub(c.openedAt) < b.cooldown {
			return false
		}
		b.transition(backend, c, models.CircuitStateHalfOpen)
		c.probing = true
		return true
	default: // half-open
		if c.probing {
			return false
		}
		c.probing = true
		return true
	}
}

// ReportSuccess records a successful call (or a permanent upstream error —
// the backend responded, so it is healthy). It resets the consecutive
// failure count and closes a half-open circuit. A success reported while
// open (a straggler that started before the circuit opened) is ignored: the
// half-open probe is the only evidence that ends an open state.
func (b *Breaker) ReportSuccess(backend string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.circuit(backend)

	switch c.state {
	case models.CircuitStateClosed:
		c.failures = 0
	case models.CircuitStateHalfOpen:
		c.failures = 0
		c.probing = false
		b.transition(backend, c, models.CircuitStateClosed)
	}
}

// ReportCanceled records a call that ended because the *caller* went away
// (context canceled) before the backend's health could be judged. It is
// deliberately verdict-free: no failure is counted and no state changes —
// except that a half-open probe slot reserved by Allow is returned, so a
// canceled probe cannot leave the backend unprobeable forever.
func (b *Breaker) ReportCanceled(backend string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.circuit(backend)
	if c.state == models.CircuitStateHalfOpen {
		c.probing = false
	}
}

// ReportFailure records a transient failure. While closed it counts toward
// the threshold; the threshold-th consecutive failure opens the circuit. A
// failure in half-open re-opens immediately. A failure reported while
// already open (a straggler) is ignored so it cannot extend the cooldown.
func (b *Breaker) ReportFailure(backend string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.circuit(backend)

	switch c.state {
	case models.CircuitStateClosed:
		c.failures++
		if c.failures >= b.threshold {
			b.open(backend, c)
		}
	case models.CircuitStateHalfOpen:
		c.probing = false
		b.open(backend, c)
	}
}

// State returns the named backend's current circuit state. An unseen backend
// is closed. The stored state is returned as-is: an open circuit past its
// cooldown still reads open until the next Allow performs the half-open
// transition.
func (b *Breaker) State(backend string) models.CircuitState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.circuit(backend).state
}

// open moves c to open and stamps the cooldown start.
func (b *Breaker) open(backend string, c *circuit) {
	c.openedAt = b.now()
	b.transition(backend, c, models.CircuitStateOpen)
}

// transition sets the state and fires the listener. Callers hold b.mu.
func (b *Breaker) transition(backend string, c *circuit, state models.CircuitState) {
	c.state = state
	if b.onChange != nil {
		b.onChange(backend, state)
	}
}

// circuit returns the named backend's state, creating a closed one on first
// sight. Callers hold b.mu.
func (b *Breaker) circuit(backend string) *circuit {
	c, ok := b.circuits[backend]
	if !ok {
		c = &circuit{state: models.CircuitStateClosed}
		b.circuits[backend] = c
	}
	return c
}

// CircuitStateGaugeValue maps a circuit state to the value exported by the
// aegisroute_circuit_breaker_state gauge: 0=closed, 1=half-open, 2=open
// (matching the metric's help text).
func CircuitStateGaugeValue(s models.CircuitState) float64 {
	switch s {
	case models.CircuitStateHalfOpen:
		return 1
	case models.CircuitStateOpen:
		return 2
	default:
		return 0
	}
}
