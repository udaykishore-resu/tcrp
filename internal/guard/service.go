package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/identity"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Inspector is the interface the agent runtime depends on. Both the in-process
// engine and the remote client satisfy it, so the deployment topology — sidecar,
// separate service, or linked in — is a wiring decision and not a code change.
type Inspector interface {
	Inspect(ctx context.Context, req Request) Result
}

var _ Inspector = (*Engine)(nil)
var _ Inspector = (*Client)(nil)

// Handler exposes the engine over HTTP.
//
// The wire format is JSON over HTTP/1.1 with keep-alive. In a production
// deployment this is gRPC over a unix socket or a sidecar port; the interface
// above is the seam, and docs/adr/0003-guardrail-transport.md records why the
// repository ships the HTTP form.
func Handler(e *Engine, signer *identity.Signer, clock obs.Clock) http.Handler {
	if clock == nil {
		clock = obs.RealClock{}
	}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/inspect", func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		ctx := r.Context()
		if tp := r.Header.Get(identity.HeaderTraceParent); tp != "" {
			if sc, ok := obs.ParseTraceParent(tp); ok {
				ctx = obs.WithRemoteContext(ctx, sc)
			}
		}
		if signer != nil {
			tok := r.Header.Get(identity.HeaderPrincipal)
			p, err := signer.Verify(tok, clock.Now())
			if err != nil {
				writeErr(w, http.StatusUnauthorized, err)
				return
			}
			ctx = identity.WithPrincipal(ctx, p)
			req.Subject, req.Tenant = p.Subject, p.Tenant
		}
		res := e.Inspect(ctx, req)
		writeJSON(w, http.StatusOK, res)
	})

	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "ok", "policy": e.cfg.Version, "budget_ms": e.cfg.Budget.Milliseconds(),
		})
	})

	mux.HandleFunc("GET /v1/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"version":             e.cfg.Version,
			"budget_ms":           e.cfg.Budget.Milliseconds(),
			"stage_budget_ms":     e.cfg.StageBudget.Milliseconds(),
			"fail_open":           e.cfg.FailOpen,
			"injection_threshold": e.cfg.InjectionThreshold,
		})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// Client calls a remote guardrail service.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Signer  *identity.Signer
	Clock   obs.Clock
	Tracer  *obs.Tracer
	// FailOpen decides the verdict when the service itself is unreachable.
	// It is keyed by tier for the same reason the budget policy is.
	FailOpen map[string]bool
}

// NewClient builds a client with a timeout matched to the guardrail budget.
func NewClient(baseURL string, budget time.Duration) *Client {
	if budget <= 0 {
		budget = 150 * time.Millisecond
	}
	return &Client{
		BaseURL:  baseURL,
		HTTP:     &http.Client{Timeout: budget},
		Clock:    obs.RealClock{},
		FailOpen: map[string]bool{TierReadOnly: true},
	}
}

// Inspect calls the remote service.
func (c *Client) Inspect(ctx context.Context, req Request) Result {
	start := c.clock().Now()
	body, err := json.Marshal(req)
	if err != nil {
		return c.unavailable(req, err, c.clock().Since(start))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/inspect", bytes.NewReader(body))
	if err != nil {
		return c.unavailable(req, err, c.clock().Since(start))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if sp := obs.SpanFromContext(ctx); sp != nil {
		httpReq.Header.Set(identity.HeaderTraceParent, sp.Context().TraceParent())
	}
	if c.Signer != nil {
		if tok, err := c.Signer.Mint(identity.FromContext(ctx), c.clock().Now()); err == nil {
			httpReq.Header.Set(identity.HeaderPrincipal, tok)
		}
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return c.unavailable(req, err, c.clock().Since(start))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c.unavailable(req, fmt.Errorf("guard service returned %s", resp.Status), c.clock().Since(start))
	}
	var res Result
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return c.unavailable(req, err, c.clock().Since(start))
	}
	res.Duration = c.clock().Since(start)
	return res
}

func (c *Client) clock() obs.Clock {
	if c.Clock == nil {
		return obs.RealClock{}
	}
	return c.Clock
}

// unavailable renders the verdict for a guardrail that could not be reached.
// Availability of the control is itself a control: on a money-movement turn the
// absence of an answer is a block.
func (c *Client) unavailable(req Request, err error, d time.Duration) Result {
	tier := req.Tier
	if tier == "" {
		tier = TierReadOnly
	}
	res := Result{
		Duration: d, BudgetExceeded: true, RetrievalTrusted: false,
		Stages: []StageResult{{Stage: "remote", Error: err.Error(), Duration: d}},
	}
	if c.FailOpen[tier] {
		res.Verdict, res.FailMode = VerdictFlag, "fail-open"
		res.Reason = "guardrail service unavailable; read-only turn allowed under fail-open policy"
		res.RetrievalTrusted = true
	} else {
		res.Verdict, res.FailMode = VerdictBlock, "fail-closed"
		res.Reason = "guardrail service unavailable on a " + tier + " turn"
	}
	return res
}
