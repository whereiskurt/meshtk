package network

import (
	"sync"
	"time"
)

type Limiter struct {
	Buckets             map[string]*LimiterBucket
	Rate                float64       // tokens per second
	Burst               int           // maximum burst size
	Lifetime            time.Duration // how long to keep inactive buckets
	MaxPenaltyThreshold float64       // how many missing tokens = automatic ban
	mu                  sync.Mutex
}

type LimiterBucket struct {
	Tokens float64
	Last   time.Time
}

func NewLimiter(tokenPerSecond float64, burstTokens int, lifetime time.Duration, penaltyThreshold float64) *Limiter {
	return &Limiter{
		Buckets:             make(map[string]*LimiterBucket),
		Rate:                tokenPerSecond,
		Burst:               burstTokens,
		Lifetime:            lifetime,
		MaxPenaltyThreshold: penaltyThreshold,
	}
}

func (l *Limiter) EnforceLimit(key string, penalty time.Duration) (wasSlowed bool, shouldKill bool, debt float64) {
	slow, kill, debt := l.CheckLimit(key)
	if kill {
		return false, true, debt
	} else if slow {
		penalty := min(penalty*time.Duration(-debt), 30*time.Second)
		time.Sleep(penalty)
		return true, false, debt
	} else {
		return false, false, debt
	}
}

func (l *Limiter) CheckLimit(key string) (slowConnection bool, killConnection bool, debt float64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	b, ok := l.Buckets[key]
	if !ok {
		b = &LimiterBucket{Tokens: float64(l.Burst), Last: now}
		l.Buckets[key] = b
	}

	elapsed := now.Sub(b.Last).Seconds()
	b.Tokens += elapsed * l.Rate
	if b.Tokens > float64(l.Burst) {
		b.Tokens = float64(l.Burst)
	}
	b.Last = now

	if b.Tokens >= 1 {
		b.Tokens -= 1
		return false, false, b.Tokens
	}

	b.Tokens -= 1 // Consume anyway, to track abusers deeper
	if b.Tokens < -l.MaxPenaltyThreshold {
		return false, true, b.Tokens
	}
	return true, false, b.Tokens
}

// Cleanup removes old buckets that haven't been used for a while.
func (l *Limiter) Cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	for key, b := range l.Buckets {
		if now.Sub(b.Last) > l.Lifetime {
			delete(l.Buckets, key)
		}
	}
}
