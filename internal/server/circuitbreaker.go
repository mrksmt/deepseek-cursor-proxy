package server

import (
	"fmt"
	"log"
	"sync"
	"time"
)

const (
	cbFailureThreshold = 5
	cbOpenDuration     = 30 * time.Second
)

// circuitBreaker tracks upstream failure state and stops requests
// when too many consecutive failures occur, giving the upstream time to recover.
type circuitBreaker struct {
	mu        sync.Mutex
	failCount int32
	openUntil time.Time
}

func (cb *circuitBreaker) allow() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if time.Now().Before(cb.openUntil) {
		return fmt.Errorf("circuit breaker open (until %s)", cb.openUntil.Format(time.RFC3339))
	}
	return nil
}

func (cb *circuitBreaker) failure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failCount++
	if cb.failCount >= cbFailureThreshold {
		cb.openUntil = time.Now().Add(cbOpenDuration)
		log.Printf("circuit breaker opened until %s (%d failures)", cb.openUntil.Format(time.RFC3339), cb.failCount)
	}
}

func (cb *circuitBreaker) success() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failCount = 0
	cb.openUntil = time.Time{}
}
