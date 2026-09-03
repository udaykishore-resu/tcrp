package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/identity"
	"github.com/udaykishore-resu/tcrp/internal/obs"
	"github.com/udaykishore-resu/tcrp/internal/platform"
)

// ---------------------------------------------------------------------------
// request-scoped values
// ---------------------------------------------------------------------------

type requestIDKey struct{}

// requestIDOf returns the correlation id attached to ctx, or the empty string.
func requestIDOf(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// recorder captures the status and size a handler wrote, which neither the
// access log nor the metrics recorder can otherwise observe.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *recorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *recorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Flush forwards to the underlying writer when it supports flushing, so a
// streaming middleware below does not lose the ability by being wrapped.
func (w *recorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func wrap(w http.ResponseWriter) *recorder {
	if rec, ok := w.(*recorder); ok {
		return rec
	}
	return &recorder{ResponseWriter: w}
}

// ---------------------------------------------------------------------------
// 1. panic recovery
// ---------------------------------------------------------------------------

// recover turns a panic into a 500 in the standard error shape.
//
// It is outermost so that a panic anywhere below still produces a response, a
// log line and a request id. A process that dies on a malformed argument is a
// denial of service with extra steps.
func (s *server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := wrap(w)
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic serving request",
					"method", r.Method, "path", r.URL.Path,
					"request_id", requestIDOf(r.Context()), "panic", v)
				if rec.status == 0 {
					writeError(rec, r, http.StatusInternalServerError, CodeInternal,
						"the request could not be completed")
				}
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// ---------------------------------------------------------------------------
// 2. request body cap
// ---------------------------------------------------------------------------

// limitBody caps the request body before any handler can read it. It is applied
// once, here, rather than remembered at each call site.
func (s *server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, s.opts.MaxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// 3. request id
// ---------------------------------------------------------------------------

// requestID attaches a correlation id, honouring one the caller supplied so a
// gateway's id survives into the platform's logs.
func (s *server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(HeaderRequestID))
		if id == "" || len(id) > 128 {
			id = newRequestID()
		}
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req_unknown"
	}
	return "req_" + hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// 4. trace span
// ---------------------------------------------------------------------------

// traceSpan starts a server span for the request, continuing an upstream trace
// when the caller sent a W3C traceparent. The response carries the span's own
// traceparent back, so a client can quote it in a support request.
func (s *server) traceSpan(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.p.Tracer == nil {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		if tp := r.Header.Get(identity.HeaderTraceParent); tp != "" {
			if sc, ok := obs.ParseTraceParent(tp); ok {
				ctx = obs.WithRemoteContext(ctx, sc)
			}
		}
		ctx, span := s.p.Tracer.Start(ctx, "http.server", obs.KindServer)
		defer span.End()
		span.SetAttrs(map[string]any{
			"http.request.method": r.Method,
			"url.path":            r.URL.Path,
			"tcrp.request.id":     requestIDOf(ctx),
		})
		w.Header().Set(identity.HeaderTraceParent, span.Context().TraceParent())

		rec := wrap(w)
		next.ServeHTTP(rec, r.WithContext(ctx))
		span.SetAttr("http.response.status_code", rec.status)
		if rec.status >= 500 {
			span.SetStatus(obs.StatusError, http.StatusText(rec.status))
		}
	})
}

// ---------------------------------------------------------------------------
// 5. authentication
// ---------------------------------------------------------------------------

// openPaths are served without a principal: the probes and the scrape endpoint,
// because a probe that needs a credential fails for the wrong reason, and the
// dashboard, which authenticates from the browser once it loads.
func openPath(p string) bool {
	switch p {
	case "/healthz", "/readyz", "/metrics", "/":
		return true
	}
	return !strings.HasPrefix(p, "/v1/")
}

// authenticate resolves the acting principal and refuses /v1 without one.
//
// The production path is a signed principal assertion in identity.HeaderPrincipal,
// verified against the platform's signer. HeaderDevPrincipal is a development
// affordance: it names one of platform.PrincipalNames and asserts nothing, so a
// production build leaves Options.DevPrincipalHeader false and the header is
// then ignored entirely rather than merely discouraged.
func (s *server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.principalOf(r)
		if err != nil {
			if openPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			writeError(w, r, http.StatusUnauthorized, CodeUnauthenticated, err.Error())
			return
		}
		ctx := identity.WithPrincipal(r.Context(), p)
		if sp := obs.SpanFromContext(ctx); sp != nil {
			sp.SetAttrs(map[string]any{
				obs.AttrPrincipalSubject: p.Subject,
				obs.AttrPrincipalTenant:  p.Tenant,
				obs.AttrPrincipalScopes:  p.ScopeString(),
			})
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// principalOf resolves the caller's identity from the request headers.
func (s *server) principalOf(r *http.Request) (identity.Principal, error) {
	if tok := strings.TrimSpace(r.Header.Get(identity.HeaderPrincipal)); tok != "" {
		if s.p.Signer == nil {
			return identity.Principal{}, Error{Code: CodeUnauthenticated, Message: "no principal verifier is configured"}
		}
		p, err := s.p.Signer.Verify(tok, s.clock.Now())
		if err != nil {
			return identity.Principal{}, err
		}
		return p, nil
	}
	if s.opts.DevPrincipalHeader {
		if name := strings.TrimSpace(r.Header.Get(HeaderDevPrincipal)); name != "" {
			p, ok := platform.PrincipalByName(name)
			if !ok {
				return identity.Principal{}, Error{
					Code:    CodeUnauthenticated,
					Message: "unknown principal " + name + "; expected one of " + strings.Join(platform.PrincipalNames(), ", "),
				}
			}
			return p, nil
		}
	}
	return identity.Principal{}, identity.ErrNoPrincipal
}

// ---------------------------------------------------------------------------
// 6. access log
// ---------------------------------------------------------------------------

// accessLog emits one structured line per request, after the handler has run so
// the status and duration are real rather than intended.
func (s *server) accessLog(pattern string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.clock.Now()
		rec := wrap(w)
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		p := identity.FromContext(r.Context())
		attrs := []any{
			"method", r.Method,
			"route", pattern,
			"path", r.URL.Path,
			"status", status,
			"bytes", rec.bytes,
			"duration_ms", float64(s.clock.Since(start).Microseconds()) / 1000,
			"request_id", requestIDOf(r.Context()),
			"principal", p.Subject,
			"tenant", p.Tenant,
		}
		if sp := obs.SpanFromContext(r.Context()); sp != nil {
			attrs = append(attrs, "trace_id", sp.Context().TraceID)
		}
		if status >= 500 {
			s.log.Error("http request", attrs...)
			return
		}
		s.log.Info("http request", attrs...)
	})
}

// ---------------------------------------------------------------------------
// 7. metrics
// ---------------------------------------------------------------------------

// httpMetrics are the transport's own series. They are registered on the
// platform's registry so /metrics carries them alongside the policy, guardrail
// and agent counters rather than in a second exposition nobody scrapes.
type httpMetrics struct {
	reg *obs.Registry
}

func newHTTPMetrics(reg *obs.Registry) *httpMetrics { return &httpMetrics{reg: reg} }

// observe records the request count, latency and in-flight gauge for one route.
func (s *server) observe(pattern string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.metrics == nil || s.metrics.reg == nil {
			next.ServeHTTP(w, r)
			return
		}
		reg := s.metrics.reg
		inflight := reg.Gauge("tcrp_http_requests_in_flight", "Requests currently being served.", nil)
		inflight.Add(1)
		defer inflight.Add(-1)

		start := s.clock.Now()
		rec := wrap(w)
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		labels := map[string]string{"route": pattern, "status": statusClass(status)}
		reg.Counter("tcrp_http_requests_total", "HTTP requests served by route and status class.", labels).Inc()
		reg.Histogram("tcrp_http_request_seconds", "HTTP request latency by route.", nil,
			map[string]string{"route": pattern}).ObserveDuration(s.clock.Since(start))
	})
}

// statusClass buckets a status code, so the label cardinality stays bounded by
// the number of routes rather than by the number of distinct codes.
func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}

// ---------------------------------------------------------------------------
// 8. rate limiting
// ---------------------------------------------------------------------------

// rateLimit refuses a principal that is asking for more than its allowance.
func (s *server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.limiter == nil {
			next.ServeHTTP(w, r)
			return
		}
		key := identity.FromContext(r.Context()).Subject
		if key == "" {
			key = identity.Anonymous.Subject
		}
		if !s.limiter.allow(key) {
			writeError(w, r, http.StatusTooManyRequests, CodeRateLimited,
				"request allowance exhausted for "+key)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limiter is a per-principal token bucket on the platform's clock.
//
// It is deliberately small. The purpose is to stop one caller — or one runaway
// dashboard — from consuming the process, not to implement a quota system; a
// deployment fronts this with the institution's own gateway and this stays as
// the last line that is impossible to forget to configure.
type limiter struct {
	clock   obs.Clock
	perMin  int
	burst   float64
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(clock obs.Clock, perMinute int) *limiter {
	if perMinute < 0 {
		return nil
	}
	if perMinute == 0 {
		perMinute = DefaultRatePerMinute
	}
	return &limiter{
		clock:   clock,
		perMin:  perMinute,
		burst:   float64(perMinute),
		buckets: map[string]*bucket{},
	}
}

// allow reports whether the principal may make one more request now.
func (l *limiter) allow(key string) bool {
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Minutes() * float64(l.perMin)
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
