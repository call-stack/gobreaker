package gobreaker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
)

var defaultRCB *RedisCircuitBreaker
var customRCB *RedisCircuitBreaker

func setupTestWithMiniredis() (*RedisCircuitBreaker, *miniredis.Miniredis, *redis.Client) {
	mr, err := miniredis.Run()
	if err != nil {
		panic(err)
	}

	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
	})

	return NewRedisCircuitBreaker(client, RedisSettings{
		Settings: Settings{
			Name:        "TestBreaker",
			MaxRequests: 3,
			Interval:    time.Second,
			Timeout:     time.Second * 2,
			ReadyToTrip: func(counts Counts) bool {
				return counts.ConsecutiveFailures > 5
			},
		},
	}), mr, client
}

func pseudoSleepRedis(ctx context.Context, rcb *RedisCircuitBreaker, period time.Duration) {
	state, _ := rcb.getRedisState(ctx)

	state.Expiry = state.Expiry.Add(-period)
	// Reset counts if the interval has passed
	if time.Now().After(state.Expiry) {
		state.Counts = Counts{}
	}
	rcb.setRedisState(ctx, state)
}

func successRequest(ctx context.Context, rcb *RedisCircuitBreaker) error {
	_, err := rcb.Execute(ctx, func() (interface{}, error) { return nil, nil })
	return err
}

func failRequest(ctx context.Context, rcb *RedisCircuitBreaker) error {
	_, err := rcb.Execute(ctx, func() (interface{}, error) { return nil, errors.New("fail") })
	if err != nil && err.Error() == "fail" {
		return nil
	}
	return err
}

func TestRedisCircuitBreakerInitialization(t *testing.T) {
	rcb, mr, _ := setupTestWithMiniredis()
	defer mr.Close()

	ctx := context.Background()

	assert.Equal(t, "TestBreaker", rcb.Name())
	assert.Equal(t, uint32(3), rcb.maxRequests)
	assert.Equal(t, time.Second, rcb.interval)
	assert.Equal(t, time.Second*2, rcb.timeout)
	assert.NotNil(t, rcb.readyToTrip)

	state := rcb.State(ctx)
	assert.Equal(t, StateClosed, state)
}

func TestRedisCircuitBreakerStateTransitions(t *testing.T) {
	rcb, mr, _ := setupTestWithMiniredis()
	defer mr.Close()

	ctx := context.Background()

	// Check if initial state is closed
	assert.Equal(t, StateClosed, rcb.State(ctx))

	// StateClosed to StateOpen
	for i := 0; i < 6; i++ {
		assert.NoError(t, failRequest(ctx, rcb))
	}

	assert.Equal(t, StateOpen, rcb.State(ctx))

	// Ensure requests fail when circuit is open
	err := failRequest(ctx, rcb)
	assert.Error(t, err)
	assert.Equal(t, ErrOpenState, err)

	// Wait for timeout to transition to half-open
	pseudoSleepRedis(ctx, rcb, rcb.timeout)
	assert.Equal(t, StateHalfOpen, rcb.State(ctx))

	// StateHalfOpen to StateClosed
	for i := 0; i < int(rcb.maxRequests); i++ {
		assert.NoError(t, successRequest(ctx, rcb))
	}
	assert.Equal(t, StateClosed, rcb.State(ctx))

	// StateClosed to StateOpen (again)
	for i := 0; i < 6; i++ {
		assert.NoError(t, failRequest(ctx, rcb))
	}
	assert.Equal(t, StateOpen, rcb.State(ctx))
}

func TestRedisCircuitBreakerExecution(t *testing.T) {
	rcb, mr, _ := setupTestWithMiniredis()
	defer mr.Close()

	ctx := context.Background()

	// Test successful execution
	result, err := rcb.Execute(ctx, func() (interface{}, error) {
		return "success", nil
	})
	assert.NoError(t, err)
	assert.Equal(t, "success", result)

	// Test failed execution
	_, err = rcb.Execute(ctx, func() (interface{}, error) {
		return nil, errors.New("test error")
	})
	assert.Error(t, err)
	assert.Equal(t, "test error", err.Error())
}

func TestRedisCircuitBreakerCounts(t *testing.T) {
	rcb, mr, _ := setupTestWithMiniredis()
	defer mr.Close()

	ctx := context.Background()

	for i := 0; i < 5; i++ {
		assert.Nil(t, successRequest(ctx, rcb))
	}

	state, _ := rcb.getRedisState(ctx)
	assert.Equal(t, Counts{5, 5, 0, 5, 0}, state.Counts)

	assert.Nil(t, failRequest(ctx, rcb))
	state, _ = rcb.getRedisState(ctx)
	assert.Equal(t, Counts{6, 5, 1, 0, 1}, state.Counts)
}

func TestRedisCircuitBreakerFallback(t *testing.T) {
	rcb, mr, _ := setupTestWithMiniredis()
	defer mr.Close()

	ctx := context.Background()

	// Test when Redis is unavailable
	mr.Close() // Simulate Redis being unavailable

	rcb.redisClient = nil

	state := rcb.State(ctx)
	assert.Equal(t, StateClosed, state, "Should fallback to in-memory state when Redis is unavailable")

	// Ensure operations still work without Redis
	assert.Nil(t, successRequest(ctx, rcb))
	assert.Nil(t, failRequest(ctx, rcb))
}

func TestCustomRedisCircuitBreaker(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	defer mr.Close()

	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
	})

	customRCB = NewRedisCircuitBreaker(client, RedisSettings{
		Settings: Settings{
			Name:        "CustomBreaker",
			MaxRequests: 3,
			Interval:    time.Second * 30,
			Timeout:     time.Second * 90,
			ReadyToTrip: func(counts Counts) bool {
				numReqs := counts.Requests
				failureRatio := float64(counts.TotalFailures) / float64(numReqs)
				return numReqs >= 3 && failureRatio >= 0.6
			},
		},
	})

	ctx := context.Background()

	t.Run("Initialization", func(t *testing.T) {
		assert.Equal(t, "CustomBreaker", customRCB.Name())
		assert.Equal(t, StateClosed, customRCB.State(ctx))
	})

	t.Run("Counts and State Transitions", func(t *testing.T) {
		// Perform 5 successful and 5 failed requests
		for i := 0; i < 5; i++ {
			assert.NoError(t, successRequest(ctx, customRCB))
			assert.NoError(t, failRequest(ctx, customRCB))
		}

		state, err := customRCB.getRedisState(ctx)
		assert.NoError(t, err)
		assert.Equal(t, StateClosed, state.State)
		assert.Equal(t, Counts{10, 5, 5, 0, 1}, state.Counts)

		// Perform one more successful request
		assert.NoError(t, successRequest(ctx, customRCB))
		state, err = customRCB.getRedisState(ctx)
		assert.NoError(t, err)
		assert.Equal(t, Counts{11, 6, 5, 1, 0}, state.Counts)

		// Simulate time passing to reset counts
		pseudoSleepRedis(ctx, customRCB, time.Second*30)

		// Perform requests to trigger StateOpen
		assert.NoError(t, successRequest(ctx, customRCB))
		assert.NoError(t, failRequest(ctx, customRCB))
		assert.NoError(t, failRequest(ctx, customRCB))

		// Check if the circuit breaker is now open
		assert.Equal(t, StateOpen, customRCB.State(ctx))

		state, err = customRCB.getRedisState(ctx)
		assert.NoError(t, err)
		assert.Equal(t, Counts{0, 0, 0, 0, 0}, state.Counts)
	})

	t.Run("Timeout and Half-Open State", func(t *testing.T) {
		// Simulate timeout to transition to half-open state
		pseudoSleepRedis(ctx, customRCB, time.Second*90)
		assert.Equal(t, StateHalfOpen, customRCB.State(ctx))

		// Successful requests in half-open state should close the circuit
		for i := 0; i < 3; i++ {
			assert.NoError(t, successRequest(ctx, customRCB))
		}
		assert.Equal(t, StateClosed, customRCB.State(ctx))
	})
}

func TestCustomRedisCircuitBreakerStateTransitions(t *testing.T) {
	// Setup
	var stateChange StateChange
	customSt := Settings{
		Name:        "cb",
		MaxRequests: 3,
		Interval:    5 * time.Second,
		Timeout:     5 * time.Second,
		ReadyToTrip: func(counts Counts) bool {
			return counts.ConsecutiveFailures >= 2
		},
		OnStateChange: func(name string, from State, to State) {
			stateChange = StateChange{name, from, to}
		},
	}

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("Failed to start miniredis: %v", err)
	}
	defer mr.Close()

	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
	})

	cb := NewRedisCircuitBreaker(client, RedisSettings{Settings: customSt})

	ctx := context.Background()

	// Test case
	t.Run("Circuit Breaker State Transitions", func(t *testing.T) {
		// Initial state should be Closed
		assert.Equal(t, StateClosed, cb.State(ctx))

		// Cause two consecutive failures to trip the circuit
		for i := 0; i < 2; i++ {
			err := failRequest(ctx, cb)
			assert.NoError(t, err, "Fail request should not return an error")
		}

		// Circuit should now be Open
		assert.Equal(t, StateOpen, cb.State(ctx))
		assert.Equal(t, StateChange{"cb", StateClosed, StateOpen}, stateChange)

		// Requests should fail immediately when circuit is Open
		err := successRequest(ctx, cb)
		assert.Error(t, err)
		assert.Equal(t, ErrOpenState, err)

		// Simulate timeout to transition to Half-Open
		pseudoSleepRedis(ctx, cb, 6*time.Second)
		assert.Equal(t, StateHalfOpen, cb.State(ctx))
		assert.Equal(t, StateChange{"cb", StateOpen, StateHalfOpen}, stateChange)

		// Successful requests in Half-Open state should close the circuit
		for i := 0; i < int(cb.maxRequests); i++ {
			err := successRequest(ctx, cb)
			assert.NoError(t, err)
		}

		// Circuit should now be Closed
		assert.Equal(t, StateClosed, cb.State(ctx))
		assert.Equal(t, StateChange{"cb", StateHalfOpen, StateClosed}, stateChange)
	})
}
