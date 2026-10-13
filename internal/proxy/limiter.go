package proxy

import (
	"net/netip"
	"sync"
)

const (
	rejectGlobal = "global"
	rejectPerIP  = "per_ip"
	rejectClosed = "shutting_down"
)

type limiter struct {
	sem   chan struct{}
	perIP int

	mu   sync.Mutex
	byIP map[netip.Addr]int
}

func newLimiter(maxConns, perIP int) *limiter {
	return &limiter{
		sem:   make(chan struct{}, maxConns),
		perIP: perIP,
		byIP:  make(map[netip.Addr]int),
	}
}

func (l *limiter) tryAcquire(ip netip.Addr) (reason string, ok bool) {
	select {
	case l.sem <- struct{}{}:
	default:
		return rejectGlobal, false
	}
	if l.perIP > 0 {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.byIP[ip] >= l.perIP {
			<-l.sem
			return rejectPerIP, false
		}
		l.byIP[ip]++
	}
	return "", true
}

func (l *limiter) release(ip netip.Addr) {
	if l.perIP > 0 {
		l.mu.Lock()
		if l.byIP[ip]--; l.byIP[ip] <= 0 {
			delete(l.byIP, ip)
		}
		l.mu.Unlock()
	}
	<-l.sem
}

func (l *limiter) active() int {
	return len(l.sem)
}
