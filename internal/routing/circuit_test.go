package routing

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/models"
)

// testClock is a manually advanced time source for driving cooldowns.
type testClock struct{ t time.Time }

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

const testCooldown = 10 * time.Second

func newTestBreaker(threshold int, opts ...BreakerOption) (*Breaker, *testClock) {
	clock := newTestClock()
	opts = append([]BreakerOption{WithBreakerClock(clock.now)}, opts...)
	return NewBreaker(threshold, testCooldown, opts...), clock
}

func TestBreakerClosedByDefault(t *testing.T) {
	b, _ := newTestBreaker(3)
	assert.Equal(t, models.CircuitStateClosed, b.State("never-seen"))
	assert.True(t, b.Allow("never-seen"))
}

func TestBreakerOpensAtThreshold(t *testing.T) {
	b, _ := newTestBreaker(3)

	b.ReportFailure("be")
	b.ReportFailure("be")
	assert.Equal(t, models.CircuitStateClosed, b.State("be"), "below threshold stays closed")
	assert.True(t, b.Allow("be"))

	b.ReportFailure("be")
	assert.Equal(t, models.CircuitStateOpen, b.State("be"), "threshold-th consecutive failure opens")
	assert.False(t, b.Allow("be"), "open circuit admits nothing before cooldown")
}

func TestBreakerSuccessResetsConsecutiveCount(t *testing.T) {
	b, _ := newTestBreaker(3)

	b.ReportFailure("be")
	b.ReportFailure("be")
	b.ReportSuccess("be")
	b.ReportFailure("be")
	b.ReportFailure("be")
	assert.Equal(t, models.CircuitStateClosed, b.State("be"),
		"failures interleaved with a success are not consecutive")

	b.ReportFailure("be")
	assert.Equal(t, models.CircuitStateOpen, b.State("be"))
}

func TestBreakerHalfOpensAfterCooldown(t *testing.T) {
	b, clock := newTestBreaker(1)
	b.ReportFailure("be")
	require.Equal(t, models.CircuitStateOpen, b.State("be"))

	clock.advance(testCooldown - time.Millisecond)
	assert.False(t, b.Allow("be"), "cooldown not yet elapsed")
	assert.Equal(t, models.CircuitStateOpen, b.State("be"))

	clock.advance(time.Millisecond)
	assert.True(t, b.Allow("be"), "first caller after cooldown is the probe")
	assert.Equal(t, models.CircuitStateHalfOpen, b.State("be"))
	assert.False(t, b.Allow("be"), "only one probe is admitted while half-open")
}

func TestBreakerHalfOpenSuccessCloses(t *testing.T) {
	b, clock := newTestBreaker(1)
	b.ReportFailure("be")
	clock.advance(testCooldown)
	require.True(t, b.Allow("be"))

	b.ReportSuccess("be")
	assert.Equal(t, models.CircuitStateClosed, b.State("be"))
	assert.True(t, b.Allow("be"))

	// The failure count restarted: one failure re-opens (threshold 1), and
	// the closed state got a clean slate rather than inheriting old counts.
	b.ReportFailure("be")
	assert.Equal(t, models.CircuitStateOpen, b.State("be"))
}

func TestBreakerHalfOpenFailureReopens(t *testing.T) {
	b, clock := newTestBreaker(1)
	b.ReportFailure("be")
	clock.advance(testCooldown)
	require.True(t, b.Allow("be"))

	b.ReportFailure("be")
	assert.Equal(t, models.CircuitStateOpen, b.State("be"))
	assert.False(t, b.Allow("be"), "re-opened circuit starts a fresh cooldown")

	// The fresh cooldown runs from the half-open failure, and the next probe
	// can then close it again.
	clock.advance(testCooldown)
	assert.True(t, b.Allow("be"))
	b.ReportSuccess("be")
	assert.Equal(t, models.CircuitStateClosed, b.State("be"))
}

func TestBreakerProbeSlotFreedByOutcome(t *testing.T) {
	b, clock := newTestBreaker(1)
	b.ReportFailure("be")
	clock.advance(testCooldown)

	require.True(t, b.Allow("be"))
	require.False(t, b.Allow("be"), "probe slot taken")

	b.ReportFailure("be") // probe failed → open again; slot released
	clock.advance(testCooldown)
	assert.True(t, b.Allow("be"), "a new probe is admitted after the next cooldown")
}

func TestBreakerStragglersIgnoredWhileOpen(t *testing.T) {
	b, clock := newTestBreaker(1)
	b.ReportFailure("be")
	require.Equal(t, models.CircuitStateOpen, b.State("be"))

	// Outcomes of calls that were already in flight when the circuit opened
	// must not close it early or extend the cooldown.
	b.ReportSuccess("be")
	assert.Equal(t, models.CircuitStateOpen, b.State("be"))
	clock.advance(testCooldown / 2)
	b.ReportFailure("be")
	clock.advance(testCooldown / 2)
	assert.True(t, b.Allow("be"), "original cooldown still governs")
}

func TestBreakerReportCanceled(t *testing.T) {
	b, clock := newTestBreaker(2)

	// Closed: verdict-free — neither counts a failure nor resets the streak.
	b.ReportFailure("be")
	b.ReportCanceled("be")
	b.ReportFailure("be")
	assert.Equal(t, models.CircuitStateOpen, b.State("be"),
		"canceled must not reset the consecutive-failure streak")

	// Half-open: a canceled probe returns the probe slot without a verdict,
	// so the next caller can probe instead of the backend being stuck.
	clock.advance(testCooldown)
	require.True(t, b.Allow("be"))
	require.False(t, b.Allow("be"), "probe slot held")
	b.ReportCanceled("be")
	assert.Equal(t, models.CircuitStateHalfOpen, b.State("be"),
		"a canceled probe is not a success: the circuit stays half-open")
	assert.True(t, b.Allow("be"), "the probe slot is available again")

	// Open: no-op.
	b.ReportFailure("be")
	require.Equal(t, models.CircuitStateOpen, b.State("be"))
	b.ReportCanceled("be")
	assert.Equal(t, models.CircuitStateOpen, b.State("be"))
}

func TestBreakerTracksBackendsIndependently(t *testing.T) {
	b, _ := newTestBreaker(1)
	b.ReportFailure("bad")
	assert.Equal(t, models.CircuitStateOpen, b.State("bad"))
	assert.Equal(t, models.CircuitStateClosed, b.State("good"))
	assert.True(t, b.Allow("good"))
}

func TestBreakerStateListener(t *testing.T) {
	type event struct {
		backend string
		state   models.CircuitState
	}
	var events []event
	clock := newTestClock()
	b := NewBreaker(1, testCooldown,
		WithBreakerClock(clock.now),
		WithStateListener(func(backend string, s models.CircuitState) {
			events = append(events, event{backend, s})
		}))

	b.ReportFailure("be") // closed → open
	clock.advance(testCooldown)
	b.Allow("be")         // open → half-open
	b.ReportSuccess("be") // half-open → closed

	assert.Equal(t, []event{
		{"be", models.CircuitStateOpen},
		{"be", models.CircuitStateHalfOpen},
		{"be", models.CircuitStateClosed},
	}, events)
}

func TestCircuitStateGaugeValue(t *testing.T) {
	assert.Equal(t, 0.0, CircuitStateGaugeValue(models.CircuitStateClosed))
	assert.Equal(t, 1.0, CircuitStateGaugeValue(models.CircuitStateHalfOpen))
	assert.Equal(t, 2.0, CircuitStateGaugeValue(models.CircuitStateOpen))
}
