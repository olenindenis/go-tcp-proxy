package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"go-tcp-proxy/internal/config"
)

const (
	reasonEOF           = "eof"
	reasonIdleTimeout   = "idle_timeout"
	reasonHalfClose     = "half_close_timeout"
	reasonMaxLifetime   = "max_lifetime"
	reasonShutdown      = "shutdown"
	reasonClientError   = "client_error"
	reasonUpstreamError = "upstream_error"
	reasonDialError     = "dial_error"
)

const bufSize = 32 << 10

var bufPool = sync.Pool{New: func() any { return new([bufSize]byte) }}

type session struct {
	client   net.Conn
	upstream net.Conn
	timeouts config.Timeouts

	lastActivity atomic.Int64

	halfCloseTimer atomic.Pointer[time.Timer]

	reason    atomic.Pointer[string]
	closeOnce sync.Once
}

func newSession(client, upstream net.Conn, timeouts config.Timeouts) *session {
	s := &session{client: client, upstream: upstream, timeouts: timeouts}
	s.touch()
	return s
}

func (s *session) touch() { s.lastActivity.Store(time.Now().UnixNano()) }

func (s *session) idleDeadline() time.Time {
	return time.Unix(0, s.lastActivity.Load()).Add(s.timeouts.Idle)
}

func (s *session) close(reason string) {
	s.reason.CompareAndSwap(nil, &reason)
	s.closeOnce.Do(func() {
		_ = s.client.Close()
		_ = s.upstream.Close()
	})
}

func (s *session) closeReason() string {
	if r := s.reason.Load(); r != nil {
		return *r
	}
	return reasonEOF
}

func (s *session) run(ctx context.Context, bytesUp, bytesDown prometheus.Counter) {
	if d := s.timeouts.MaxSession; d > 0 {
		t := time.AfterFunc(d, func() { s.close(reasonMaxLifetime) })
		defer t.Stop()
	}
	stop := context.AfterFunc(ctx, func() { s.close(reasonShutdown) })
	defer stop()

	var wg sync.WaitGroup
	wg.Go(func() { s.pipe(s.upstream, s.client, bytesUp, reasonClientError, reasonUpstreamError) })
	wg.Go(func() { s.pipe(s.client, s.upstream, bytesDown, reasonUpstreamError, reasonClientError) })
	wg.Wait()
	if t := s.halfCloseTimer.Load(); t != nil {
		t.Stop()
	}
	s.close(reasonEOF)
}

func (s *session) pipe(dst, src net.Conn, bytes prometheus.Counter, srcErrReason, dstErrReason string) {
	buf := bufPool.Get().(*[bufSize]byte)
	defer bufPool.Put(buf)

	for {
		if err := src.SetReadDeadline(s.idleDeadline()); err != nil {
			s.close(srcErrReason)
			return
		}
		n, rerr := src.Read(buf[:])
		if n > 0 {
			s.touch()
			if err := dst.SetWriteDeadline(time.Now().Add(s.timeouts.Idle)); err != nil {
				s.close(dstErrReason)
				return
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {

				if isTimeout(werr) {
					s.close(reasonIdleTimeout)
				} else {
					s.close(dstErrReason)
				}
				return
			}
			s.touch()
			bytes.Add(float64(n))
		}
		switch {
		case rerr == nil:
		case errors.Is(rerr, io.EOF):
			s.halfClose(dst)
			return
		case isTimeout(rerr):
			if !time.Now().Before(s.idleDeadline()) {
				s.close(reasonIdleTimeout)
				return
			}

		default:
			s.close(srcErrReason)
			return
		}
	}
}

func (s *session) halfClose(dst net.Conn) {
	cw, ok := dst.(interface{ CloseWrite() error })
	if !ok || cw.CloseWrite() != nil {
		s.close(reasonEOF)
		return
	}
	t := time.AfterFunc(s.timeouts.HalfClose, func() { s.close(reasonHalfClose) })
	if !s.halfCloseTimer.CompareAndSwap(nil, t) {
		t.Stop()
	}
}

func isTimeout(err error) bool {
	ne, ok := errors.AsType[net.Error](err)
	return ok && ne.Timeout()
}
