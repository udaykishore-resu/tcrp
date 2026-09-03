package risk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Errors returned by the leader election and the fenced log.
var (
	// ErrNotLeader is returned when a node that does not hold the lease tries to
	// renew or release it.
	ErrNotLeader = errors.New("risk: not the leader")
	// ErrLeaseHeld is returned when the lease is held by somebody else and has
	// not yet expired.
	ErrLeaseHeld = errors.New("risk: lease held by another node")
	// ErrFenced is returned when a write arrives carrying a fencing token older
	// than one the log has already accepted. It is the error a stale leader gets
	// when it comes back from a pause and tries to carry on.
	ErrFenced = errors.New("risk: stale fencing token")
)

// Lease is the right to be the calculating node for a bounded period.
//
// The token is the point of the design. A lease alone cannot make a paused
// leader safe: it can wake up believing it still holds a lease that has in fact
// expired and been taken. The token is monotonically increasing and every write
// carries it, so the store can reject the stale leader's write without needing
// to know that it was paused. Correctness comes from the token, not from the
// clock agreeing across nodes.
type Lease struct {
	Holder  string    `json:"holder"`
	Token   uint64    `json:"token"`
	Expires time.Time `json:"expires"`
}

// Held reports whether the lease is still valid at now.
func (l Lease) Held(now time.Time) bool {
	return l.Holder != "" && now.Before(l.Expires)
}

// LeaseStore is the small amount of shared state leader election needs. The
// implementations here are in-memory and file-backed; a production deployment
// would put the same three operations on etcd or a database row, and nothing
// above this interface would change.
type LeaseStore interface {
	// Get returns the current lease, or the zero Lease if there is none.
	Get(ctx context.Context) (Lease, error)
	// Acquire takes the lease if it is free or expired at now, issuing a token
	// strictly greater than every token issued before. Acquiring a lease the
	// caller already holds renews it and keeps the token.
	Acquire(ctx context.Context, holder string, now time.Time, ttl time.Duration) (Lease, error)
	// Renew extends a lease the caller still holds at token.
	Renew(ctx context.Context, holder string, token uint64, now time.Time, ttl time.Duration) (Lease, error)
	// Release gives the lease up. The token is not reused.
	Release(ctx context.Context, holder string, token uint64) error
}

// MemoryLeaseStore is a LeaseStore for a single process: tests, the demo, and
// a single-node deployment that still wants the failover code path exercised.
type MemoryLeaseStore struct {
	mu      sync.Mutex
	lease   Lease
	highest uint64
}

// NewMemoryLeaseStore returns an empty store.
func NewMemoryLeaseStore() *MemoryLeaseStore { return &MemoryLeaseStore{} }

// Get returns the current lease.
func (s *MemoryLeaseStore) Get(context.Context) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lease, nil
}

// Acquire takes the lease if it is available.
func (s *MemoryLeaseStore) Acquire(_ context.Context, holder string, now time.Time, ttl time.Duration) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := acquire(s.lease, s.highest, holder, now, ttl)
	if err != nil {
		return Lease{}, err
	}
	s.lease = l
	if l.Token > s.highest {
		s.highest = l.Token
	}
	return l, nil
}

// Renew extends the caller's lease.
func (s *MemoryLeaseStore) Renew(_ context.Context, holder string, token uint64, now time.Time, ttl time.Duration) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := renew(s.lease, holder, token, now, ttl)
	if err != nil {
		return Lease{}, err
	}
	s.lease = l
	return l, nil
}

// Release gives up the lease.
func (s *MemoryLeaseStore) Release(_ context.Context, holder string, token uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lease.Holder != holder || s.lease.Token != token {
		return ErrNotLeader
	}
	s.lease = Lease{}
	return nil
}

// FileLeaseStore keeps the lease in a file, so two processes on one host — or
// two containers sharing a volume — can elect between themselves without a
// coordination service. Writes go through a temporary file and a rename, which
// is atomic on every filesystem the platform runs on, so a crash mid-write
// leaves the previous lease intact rather than a truncated one.
type FileLeaseStore struct {
	mu   sync.Mutex
	path string
}

// NewFileLeaseStore returns a store backed by path. The file is created on
// first write; a missing file means no lease has ever been taken.
func NewFileLeaseStore(path string) *FileLeaseStore { return &FileLeaseStore{path: path} }

type leaseFile struct {
	Lease   Lease  `json:"lease"`
	Highest uint64 `json:"highest_token"`
}

func (s *FileLeaseStore) read() (leaseFile, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return leaseFile{}, nil
	}
	if err != nil {
		return leaseFile{}, err
	}
	if len(b) == 0 {
		return leaseFile{}, nil
	}
	var lf leaseFile
	if err := json.Unmarshal(b, &lf); err != nil {
		return leaseFile{}, fmt.Errorf("risk: lease file %s: %w", s.path, err)
	}
	return lf, nil
}

func (s *FileLeaseStore) write(lf leaseFile) error {
	b, err := json.Marshal(lf)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".lease-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, s.path)
}

// Get returns the current lease.
func (s *FileLeaseStore) Get(context.Context) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lf, err := s.read()
	if err != nil {
		return Lease{}, err
	}
	return lf.Lease, nil
}

// Acquire takes the lease if it is available.
func (s *FileLeaseStore) Acquire(_ context.Context, holder string, now time.Time, ttl time.Duration) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lf, err := s.read()
	if err != nil {
		return Lease{}, err
	}
	l, err := acquire(lf.Lease, lf.Highest, holder, now, ttl)
	if err != nil {
		return Lease{}, err
	}
	lf.Lease = l
	if l.Token > lf.Highest {
		lf.Highest = l.Token
	}
	if err := s.write(lf); err != nil {
		return Lease{}, err
	}
	return l, nil
}

// Renew extends the caller's lease.
func (s *FileLeaseStore) Renew(_ context.Context, holder string, token uint64, now time.Time, ttl time.Duration) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lf, err := s.read()
	if err != nil {
		return Lease{}, err
	}
	l, err := renew(lf.Lease, holder, token, now, ttl)
	if err != nil {
		return Lease{}, err
	}
	lf.Lease = l
	if err := s.write(lf); err != nil {
		return Lease{}, err
	}
	return l, nil
}

// Release gives up the lease.
func (s *FileLeaseStore) Release(_ context.Context, holder string, token uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lf, err := s.read()
	if err != nil {
		return err
	}
	if lf.Lease.Holder != holder || lf.Lease.Token != token {
		return ErrNotLeader
	}
	lf.Lease = Lease{}
	return s.write(lf)
}

// acquire and renew are the store-independent rules, shared so that the
// in-memory and the file store cannot disagree about what a lease means.

func acquire(cur Lease, highest uint64, holder string, now time.Time, ttl time.Duration) (Lease, error) {
	if cur.Held(now) && cur.Holder != holder {
		return Lease{}, fmt.Errorf("%w: %s until %s", ErrLeaseHeld, cur.Holder, cur.Expires.UTC().Format(time.RFC3339))
	}
	token := cur.Token
	if cur.Holder != holder || !cur.Held(now) {
		if highest > token {
			token = highest
		}
		token++
	}
	return Lease{Holder: holder, Token: token, Expires: now.Add(ttl)}, nil
}

func renew(cur Lease, holder string, token uint64, now time.Time, ttl time.Duration) (Lease, error) {
	if cur.Holder != holder || cur.Token != token || !cur.Held(now) {
		return Lease{}, ErrNotLeader
	}
	return Lease{Holder: holder, Token: token, Expires: now.Add(ttl)}, nil
}

// Elector campaigns for the lease and keeps it renewed while it holds it.
type Elector struct {
	store  LeaseStore
	id     string
	clock  obs.Clock
	tracer *obs.Tracer

	ttl      time.Duration
	renew    time.Duration
	retry    time.Duration
	onChange func(leader bool, token uint64)

	mu     sync.RWMutex
	lease  Lease
	leader bool
}

// ElectorOption configures an Elector.
type ElectorOption func(*Elector)

// WithLeaseTTL sets how long an acquired lease is valid for.
func WithLeaseTTL(d time.Duration) ElectorOption {
	return func(e *Elector) { e.ttl = d }
}

// WithRenewInterval sets how often the leader renews. It must be comfortably
// shorter than the TTL, because the gap between them is the only slack a node
// has when the store is slow.
func WithRenewInterval(d time.Duration) ElectorOption {
	return func(e *Elector) { e.renew = d }
}

// WithRetryInterval sets how often a standby retries acquiring.
func WithRetryInterval(d time.Duration) ElectorOption {
	return func(e *Elector) { e.retry = d }
}

// WithLeadershipChange registers a callback fired whenever leadership is gained
// or lost, so a node can start and stop its recompute loop without polling
// IsLeader.
func WithLeadershipChange(fn func(leader bool, token uint64)) ElectorOption {
	return func(e *Elector) { e.onChange = fn }
}

// NewElector builds an elector for node id over store.
func NewElector(store LeaseStore, id string, clock obs.Clock, tracer *obs.Tracer, opts ...ElectorOption) *Elector {
	if clock == nil {
		clock = obs.RealClock{}
	}
	e := &Elector{
		store: store, id: id, clock: clock, tracer: tracer,
		ttl: 15 * time.Second, renew: 5 * time.Second, retry: 1 * time.Second,
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// IsLeader reports whether this node currently believes it holds the lease.
// A node must still send its fencing token with every write: believing is not
// the same as holding, and only the token settles the difference.
func (e *Elector) IsLeader() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.leader
}

// Token returns the fencing token of the lease this node holds, or zero.
func (e *Elector) Token() uint64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.leader {
		return 0
	}
	return e.lease.Token
}

// Lease returns the node's view of the lease.
func (e *Elector) Lease() Lease {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lease
}

// TryAcquire makes one attempt to take or renew the lease and reports whether
// this node is the leader afterwards. Campaign is this in a loop; it is
// exported because a test that wants to control every transition should not
// have to control a goroutine as well.
func (e *Elector) TryAcquire(ctx context.Context) (bool, error) {
	now := e.clock.Now()

	e.mu.RLock()
	held, token := e.leader, e.lease.Token
	e.mu.RUnlock()

	if held {
		l, err := e.store.Renew(ctx, e.id, token, now, e.ttl)
		if err == nil {
			e.set(true, l)
			return true, nil
		}
		if !errors.Is(err, ErrNotLeader) {
			return false, err
		}
		// The lease was lost. Fall through and campaign for a new one, which
		// will come back with a higher token.
		e.set(false, Lease{})
	}

	l, err := e.store.Acquire(ctx, e.id, now, e.ttl)
	if err != nil {
		if errors.Is(err, ErrLeaseHeld) {
			e.set(false, Lease{})
			return false, nil
		}
		return false, err
	}
	e.set(true, l)
	return true, nil
}

func (e *Elector) set(leader bool, l Lease) {
	e.mu.Lock()
	changed := e.leader != leader || e.lease.Token != l.Token
	e.leader, e.lease = leader, l
	e.mu.Unlock()
	if changed && e.onChange != nil {
		e.onChange(leader, l.Token)
	}
}

// Campaign runs the election loop until ctx is cancelled. It returns
// ctx.Err(), and it resigns on the way out so a graceful shutdown hands
// leadership over in a renew interval rather than a full TTL.
func (e *Elector) Campaign(ctx context.Context) error {
	if _, err := e.TryAcquire(ctx); err != nil && ctx.Err() == nil {
		e.observe(ctx, err)
	}
	for {
		wait := e.retry
		if e.IsLeader() {
			wait = e.renew
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = e.Resign(context.WithoutCancel(ctx))
			return ctx.Err()
		case <-timer.C:
		}
		if _, err := e.TryAcquire(ctx); err != nil && ctx.Err() == nil {
			e.observe(ctx, err)
		}
	}
}

func (e *Elector) observe(ctx context.Context, err error) {
	if e.tracer == nil {
		return
	}
	_, span := e.tracer.Start(ctx, "risk.election", obs.KindInternal)
	span.SetAttr("tcrp.risk.node", e.id)
	span.RecordError(err)
	span.End()
}

// Resign gives up the lease immediately. A resigned node is not the leader even
// if the store call fails, because continuing to act as leader after deciding
// to stop is the one behaviour failover cannot tolerate.
func (e *Elector) Resign(ctx context.Context) error {
	e.mu.RLock()
	held, token := e.leader, e.lease.Token
	e.mu.RUnlock()
	e.set(false, Lease{})
	if !held {
		return nil
	}
	return e.store.Release(ctx, e.id, token)
}

// EventLog is the ordered log both the leader and its standby read.
//
// Appends are fenced: a write carrying a token below the highest the log has
// accepted is rejected. That is what makes failover safe without a distributed
// lock — the standby's higher token permanently retires the old leader's, so
// the old leader cannot corrupt the log by waking up and continuing.
type EventLog struct {
	mu      sync.RWMutex
	events  []domain.LedgerEvent
	highest uint64
}

// NewEventLog returns an empty log.
func NewEventLog() *EventLog { return &EventLog{} }

// Append writes events under a fencing token.
func (l *EventLog) Append(token uint64, events ...domain.LedgerEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if token < l.highest {
		return fmt.Errorf("%w: token %d, log has accepted %d", ErrFenced, token, l.highest)
	}
	l.highest = token
	l.events = append(l.events, events...)
	return nil
}

// From returns the events at or after offset, where offset is a count of
// events already applied rather than an event sequence number, so a follower
// resumes exactly where it stopped.
func (l *EventLog) From(offset uint64) []domain.LedgerEvent {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if offset >= uint64(len(l.events)) {
		return nil
	}
	out := make([]domain.LedgerEvent, len(l.events)-int(offset))
	copy(out, l.events[offset:])
	return out
}

// Len returns the number of events in the log.
func (l *EventLog) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.events)
}

// HighestToken returns the highest fencing token the log has accepted.
func (l *EventLog) HighestToken() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.highest
}

// Standby follows the same event log as the leader without acting on it, so
// that a promotion resumes from the last event the standby applied rather than
// from the beginning of the log. Failover then costs a lease TTL, not a replay.
type Standby struct {
	mu      sync.Mutex
	log     *EventLog
	applied uint64
}

// NewStandby returns a standby following log.
func NewStandby(log *EventLog) *Standby { return &Standby{log: log} }

// Applied returns how many events the standby has consumed.
func (s *Standby) Applied() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applied
}

// Catchup returns the events the standby has not yet applied and marks them
// applied. It is idempotent: calling it twice with no new events returns
// nothing the second time.
func (s *Standby) Catchup() []domain.LedgerEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.log.From(s.applied)
	s.applied += uint64(len(pending))
	return pending
}

// Follow polls the log until ctx is cancelled, handing each batch to fn. It
// returns ctx.Err() and leaves no goroutine behind.
func (s *Standby) Follow(ctx context.Context, every time.Duration, fn func([]domain.LedgerEvent)) error {
	if every <= 0 {
		every = 100 * time.Millisecond
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if batch := s.Catchup(); len(batch) > 0 && fn != nil {
				fn(batch)
			}
		}
	}
}
