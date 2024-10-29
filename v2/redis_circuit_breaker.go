package gobreaker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type CacheClient interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
}

// RedisCircuitBreaker extends CircuitBreaker with Redis-based state storage
type RedisCircuitBreaker[T any] struct {
	*CircuitBreaker[T]
	redisClient CacheClient
}

// RedisSettings extends Settings with Redis configuration
type RedisSettings struct {
	Settings
	RedisKey string
}

// NewRedisCircuitBreaker returns a new RedisCircuitBreaker configured with the given RedisSettings
func NewRedisCircuitBreaker[T any](redisClient CacheClient, settings RedisSettings) *RedisCircuitBreaker[T] {
	cb := NewCircuitBreaker[T](settings.Settings)
	return &RedisCircuitBreaker[T]{
		CircuitBreaker: cb,
		redisClient:    redisClient,
	}
}

// RedisState represents the CircuitBreaker state stored in Redis
type RedisState struct {
	State      State     `json:"state"`
	Generation uint64    `json:"generation"`
	Counts     Counts    `json:"counts"`
	Expiry     time.Time `json:"expiry"`
}

func (rcb *RedisCircuitBreaker[T]) State(ctx context.Context) State {
	if rcb.redisClient == nil {
		return rcb.CircuitBreaker.State()
	}

	state, err := rcb.getRedisState(ctx)
	if err != nil {
		// Fallback to in-memory state if Redis fails
		return rcb.CircuitBreaker.State()
	}

	now := time.Now()
	currentState, _ := rcb.currentState(state, now)

	// Update the state in Redis if it has changed
	if currentState != state.State {
		state.State = currentState
		if err := rcb.setRedisState(ctx, state); err != nil {
			// Log the error, but continue with the current state
			fmt.Printf("Failed to update state in Redis: %v\n", err)
		}
	}

	return state.State
}

// Execute runs the given request if the RedisCircuitBreaker accepts it
func (rcb *RedisCircuitBreaker[T]) Execute(ctx context.Context, req func() (T, error)) (T, error) {
	if rcb.redisClient == nil {
		return rcb.CircuitBreaker.Execute(req)
	}
	generation, err := rcb.beforeRequest(ctx)
	if err != nil {
		var zero T
		return zero, err
	}

	defer func() {
		e := recover()
		if e != nil {
			rcb.afterRequest(ctx, generation, false)
			panic(e)
		}
	}()

	result, err := req()
	rcb.afterRequest(ctx, generation, rcb.isSuccessful(err))

	return result, err
}

func (rcb *RedisCircuitBreaker[T]) beforeRequest(ctx context.Context) (uint64, error) {
	state, err := rcb.getRedisState(ctx)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	currentState, generation := rcb.currentState(state, now)

	if currentState != state.State {
		rcb.setState(&state, currentState, now)
		err = rcb.setRedisState(ctx, state)
		if err != nil {
			return 0, err
		}
	}

	if currentState == StateOpen {
		return generation, ErrOpenState
	} else if currentState == StateHalfOpen && state.Counts.Requests >= rcb.maxRequests {
		return generation, ErrTooManyRequests
	}

	state.Counts.onRequest()
	err = rcb.setRedisState(ctx, state)
	if err != nil {
		return 0, err
	}

	return generation, nil
}

func (rcb *RedisCircuitBreaker[T]) afterRequest(ctx context.Context, before uint64, success bool) {
	state, err := rcb.getRedisState(ctx)
	if err != nil {
		return
	}
	now := time.Now()
	currentState, generation := rcb.currentState(state, now)
	if generation != before {
		return
	}

	if success {
		rcb.onSuccess(&state, currentState, now)
	} else {
		rcb.onFailure(&state, currentState, now)
	}

	rcb.setRedisState(ctx, state)
}

func (rcb *RedisCircuitBreaker[T]) onSuccess(state *RedisState, currentState State, now time.Time) {
	if state.State == StateOpen {
		state.State = currentState
	}

	switch currentState {
	case StateClosed:
		state.Counts.onSuccess()
	case StateHalfOpen:
		state.Counts.onSuccess()
		if state.Counts.ConsecutiveSuccesses >= rcb.maxRequests {
			rcb.setState(state, StateClosed, now)
		}
	}
}

func (rcb *RedisCircuitBreaker[T]) onFailure(state *RedisState, currentState State, now time.Time) {
	switch currentState {
	case StateClosed:
		state.Counts.onFailure()
		if rcb.readyToTrip(state.Counts) {
			rcb.setState(state, StateOpen, now)
		}
	case StateHalfOpen:
		rcb.setState(state, StateOpen, now)
	}
}

func (rcb *RedisCircuitBreaker[T]) currentState(state RedisState, now time.Time) (State, uint64) {
	switch state.State {
	case StateClosed:
		if !state.Expiry.IsZero() && state.Expiry.Before(now) {
			rcb.toNewGeneration(&state, now)
		}
	case StateOpen:
		if state.Expiry.Before(now) {
			rcb.setState(&state, StateHalfOpen, now)
		}
	}
	return state.State, state.Generation
}

func (rcb *RedisCircuitBreaker[T]) setState(state *RedisState, newState State, now time.Time) {
	if state.State == newState {
		return
	}

	prev := state.State
	state.State = newState

	rcb.toNewGeneration(state, now)

	if rcb.onStateChange != nil {
		rcb.onStateChange(rcb.name, prev, newState)
	}
}

func (rcb *RedisCircuitBreaker[T]) toNewGeneration(state *RedisState, now time.Time) {

	state.Generation++
	state.Counts.clear()

	var zero time.Time
	switch state.State {
	case StateClosed:
		if rcb.interval == 0 {
			state.Expiry = zero
		} else {
			state.Expiry = now.Add(rcb.interval)
		}
	case StateOpen:
		state.Expiry = now.Add(rcb.timeout)
	default: // StateHalfOpen
		state.Expiry = zero
	}
}

func (rcb *RedisCircuitBreaker[T]) getRedisKey() string {
	return "cb:" + rcb.name
}

func (rcb *RedisCircuitBreaker[T]) getRedisState(ctx context.Context) (RedisState, error) {
	var state RedisState
	data, err := rcb.redisClient.Get(ctx, rcb.getRedisKey()).Bytes()
	if err == redis.Nil {
		// Key doesn't exist, return default state
		return RedisState{State: StateClosed}, nil
	} else if err != nil {
		return state, err
	}

	err = json.Unmarshal(data, &state)
	return state, err
}

func (rcb *RedisCircuitBreaker[T]) setRedisState(ctx context.Context, state RedisState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}

	return rcb.redisClient.Set(ctx, rcb.getRedisKey(), data, 0).Err()
}