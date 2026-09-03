-- 0002_audit.sql — the hash-chained audit log, with an insert-only policy.
--
-- internal/audit writes a file. Every record is chained to its predecessor over
-- a canonical encoding and ed25519-signed, so a record cannot be altered,
-- reordered or removed without breaking the chain. audit.Log.Verify walks it and
-- reports the FIRST sequence number at which it breaks — which is the useful
-- answer, because everything before that sequence remains admissible.
--
-- This schema is that log in Postgres. The chain is still the primary integrity
-- mechanism: a database administrator who can write to this table can also
-- recompute a hash, and the constraints here do not stop them. What the
-- constraints stop is everything short of that — an application bug, a stray
-- UPDATE from a console, a second writer, a migration that "fixes" a row.
--
-- The ed25519 signature is what defends against the administrator, and only if
-- the signing key is somewhere the administrator cannot reach. In this
-- repository the key is generated per process in platform.New; a deployment
-- loads it from a KMS or HSM and the verification path does not change.
--
-- Target: PostgreSQL 14 or later. Idempotent: safe to re-run.

BEGIN;

-- ---------------------------------------------------------------------------
-- audit_records
-- ---------------------------------------------------------------------------
-- Column names and semantics mirror audit.Record exactly, so a row round-trips
-- to the Go type and Verify works over rows the same way it works over lines in
-- the file.

CREATE TABLE IF NOT EXISTS audit_records (
  -- CONTROL: chain ordering. seq is assigned by the writer, not by the
  -- database, because it is an input to the hash. A gap or a repeat is a broken
  -- chain and Verify reports where.
  seq         BIGINT      PRIMARY KEY,

  time        TIMESTAMPTZ NOT NULL,

  -- CONTROL: attribution. actor is the acting principal; on_behalf is the human
  -- an agent workload is acting for. Both, always — a decision recorded with
  -- only one of them is the failure conformance invariant AGENT-4 catches.
  actor       TEXT        NOT NULL,
  on_behalf   TEXT,
  tenant      TEXT        NOT NULL,

  -- Mirrors audit.Action. Constrained rather than free text because the
  -- conformance engine matches on these strings and a typo would silently
  -- produce an unmatched invariant — which reads as a pass.
  action      TEXT        NOT NULL CHECK (action IN (
                'agent.run.started', 'agent.run.completed', 'agent.run.failed',
                'agent.tool.proposed', 'agent.tool.invoked', 'agent.tool.failed',
                'policy.decision', 'guard.verdict',
                'approval.required', 'approval.granted', 'approval.denied', 'approval.expired',
                'ledger.transfer.executed', 'collateral.margin_call.issued',
                'token.minted', 'token.burned',
                'bridge.locked', 'bridge.minted',
                'recon.break.opened', 'recon.break.resolved',
                'risk.limit.breached', 'conformance.verdict')),

  subject     TEXT        NOT NULL,
  outcome     TEXT        NOT NULL CHECK (outcome IN ('allowed', 'denied', 'ok', 'error', 'pending')),

  -- CONTROL: correlation. A verdict cites the records it read; these are how a
  -- record is found from a run, a trace or a span.
  run_id      TEXT,
  trace_id    TEXT,
  span_id     TEXT,

  -- The action-specific payload: policy rule and input hash, guard verdict and
  -- latency, tool args, approval id and approvers. JSONB because the shape
  -- varies by action and because the canonical encoding is what is hashed, not
  -- this column's storage form.
  details     JSONB,

  -- CONTROL: tamper evidence. hash is over the canonical encoding of the record
  -- including prev_hash; signature is ed25519 over hash. Both are hex.
  prev_hash   TEXT        NOT NULL,
  hash        TEXT        NOT NULL,
  signature   TEXT,

  -- Cheap shape checks. They do not prove the chain — only Verify does — but a
  -- truncated or empty hash is a write bug worth failing at insert time.
  CONSTRAINT hash_is_sha256_hex      CHECK (hash ~ '^[0-9a-f]{64}$'),
  CONSTRAINT prev_hash_is_sha256_hex CHECK (prev_hash ~ '^[0-9a-f]{64}$' OR prev_hash = ''),

  -- CONTROL: no record is its own predecessor.
  CONSTRAINT hash_differs_from_prev CHECK (hash <> prev_hash),

  -- CONTROL: the chain is a chain. Exactly one record — the genesis — may carry
  -- an empty prev_hash, and it must be the first.
  CONSTRAINT genesis_is_seq_one CHECK (
    (seq = 1) OR (prev_hash <> '')
  )
);

-- CONTROL: one genesis. Without this, a second chain could be started alongside
-- the first and Verify would walk whichever one it happened to reach.
CREATE UNIQUE INDEX IF NOT EXISTS audit_records_single_genesis
  ON audit_records ((TRUE))
  WHERE prev_hash = '';

-- CONTROL: a hash appears once. A repeated hash means either a replayed record
-- or a collision, and the first is far more likely.
CREATE UNIQUE INDEX IF NOT EXISTS audit_records_hash_unique ON audit_records (hash);

-- CONTROL: each record has at most one successor. Two records claiming the same
-- predecessor is a fork, which is exactly what a chain exists to prevent.
CREATE UNIQUE INDEX IF NOT EXISTS audit_records_prev_hash_unique
  ON audit_records (prev_hash)
  WHERE prev_hash <> '';

-- Query paths. The conformance engine reads by run and by action; an operator
-- reads by actor and by time.
CREATE INDEX IF NOT EXISTS audit_records_run_idx     ON audit_records (run_id, seq) WHERE run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS audit_records_action_idx  ON audit_records (action, time DESC);
CREATE INDEX IF NOT EXISTS audit_records_actor_idx   ON audit_records (actor, time DESC);
CREATE INDEX IF NOT EXISTS audit_records_trace_idx   ON audit_records (trace_id) WHERE trace_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS audit_records_time_idx    ON audit_records (time DESC);
CREATE INDEX IF NOT EXISTS audit_records_details_idx ON audit_records USING GIN (details);

-- ---------------------------------------------------------------------------
-- CONTROL: insert-only. No UPDATE, no DELETE, no TRUNCATE. Ever.
-- ---------------------------------------------------------------------------
--
-- The chain already makes an edit detectable. This makes it impossible through
-- the normal path, which matters because "detectable" requires somebody to run
-- Verify and "impossible" does not. The two are complementary: the trigger stops
-- the accident, the chain catches the attack.
--
-- A TRUNCATE would leave the chain intact and empty, which Verify would accept —
-- so the statement-level TRUNCATE trigger is not redundant with the chain. It is
-- the one deletion the hash chain genuinely cannot see.

CREATE OR REPLACE FUNCTION audit_records_insert_only()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION
    'audit_records is insert-only: % is not permitted', TG_OP
    USING ERRCODE = 'insufficient_privilege',
          HINT = 'A correction is a new record, not an edit. See internal/audit.';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS audit_records_no_update ON audit_records;
CREATE TRIGGER audit_records_no_update
  BEFORE UPDATE OR DELETE ON audit_records
  FOR EACH ROW EXECUTE FUNCTION audit_records_insert_only();

DROP TRIGGER IF EXISTS audit_records_no_truncate ON audit_records;
CREATE TRIGGER audit_records_no_truncate
  BEFORE TRUNCATE ON audit_records
  FOR EACH STATEMENT EXECUTE FUNCTION audit_records_insert_only();

-- CONTROL: the chain links. Each record's prev_hash must be the hash of the
-- record before it, and seq must advance by exactly one.
--
-- This duplicates part of audit.Log.Verify, at insert time, where it is cheap
-- and where it stops a broken chain from being written rather than merely
-- reporting one later. Verify remains the authority — it also checks the hash
-- against the canonical encoding and checks the signature, neither of which the
-- database can do.
CREATE OR REPLACE FUNCTION audit_records_chain_links()
RETURNS TRIGGER AS $$
DECLARE
  last_seq  BIGINT;
  last_hash TEXT;
BEGIN
  SELECT seq, hash INTO last_seq, last_hash
    FROM audit_records ORDER BY seq DESC LIMIT 1;

  IF last_seq IS NULL THEN
    IF NEW.seq <> 1 THEN
      RAISE EXCEPTION 'first audit record must be seq 1, got %', NEW.seq
        USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
  END IF;

  IF NEW.seq <> last_seq + 1 THEN
    RAISE EXCEPTION 'audit sequence must advance by one: head is %, got %', last_seq, NEW.seq
      USING ERRCODE = 'check_violation';
  END IF;

  IF NEW.prev_hash <> last_hash THEN
    RAISE EXCEPTION
      'audit chain break at seq %: prev_hash % does not match the head hash %',
      NEW.seq, NEW.prev_hash, last_hash
      USING ERRCODE = 'check_violation';
  END IF;

  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS audit_records_chain ON audit_records;
CREATE TRIGGER audit_records_chain
  BEFORE INSERT ON audit_records
  FOR EACH ROW EXECUTE FUNCTION audit_records_chain_links();

-- ---------------------------------------------------------------------------
-- The chain head
-- ---------------------------------------------------------------------------
-- conform.Bundle carries the audit chain head at the moment of a conformance
-- run, so a verdict is anchored to a specific point in the record.

CREATE OR REPLACE VIEW audit_chain_head AS
SELECT seq, hash, time, action, actor
FROM audit_records
ORDER BY seq DESC
LIMIT 1;

-- A first-pass integrity check runnable from psql. It finds a broken LINK; it
-- does not recompute a hash from a record's content and it does not check a
-- signature. Only audit.Log.Verify does those, and this view is a triage tool
-- rather than a substitute for it.
CREATE OR REPLACE VIEW audit_chain_breaks AS
SELECT
  r.seq,
  r.time,
  r.action,
  r.prev_hash                                     AS claimed_prev,
  LAG(r.hash) OVER (ORDER BY r.seq)               AS actual_prev,
  r.seq - LAG(r.seq) OVER (ORDER BY r.seq)        AS seq_gap
FROM audit_records r
ORDER BY r.seq;

COMMIT;

-- ---------------------------------------------------------------------------
-- Notes for whoever deploys this
-- ---------------------------------------------------------------------------
--
-- 1. GRANTS. The application role gets INSERT and SELECT. Nothing else. Not
--    UPDATE, not DELETE, not TRUNCATE, not owner. The triggers enforce it, but a
--    trigger can be dropped by the table's owner and a missing grant cannot be
--    used by the application at all.
--
--      REVOKE ALL   ON audit_records FROM PUBLIC;
--      GRANT SELECT, INSERT ON audit_records TO tcrp_app;
--
--    The table should NOT be owned by tcrp_app.
--
-- 2. The auditor role gets SELECT and nothing else:
--
--      GRANT SELECT ON audit_records, audit_chain_head, audit_chain_breaks TO tcrp_auditor;
--
-- 3. RETENTION. There is none, deliberately. Deleting an audit record is not
--    possible through this schema, and a retention policy that requires it is a
--    policy decision that needs a documented owner and a signed export of what
--    is being removed. Partitioning by month and detaching a partition is the
--    mechanism if it comes to that; note that detaching a partition breaks the
--    chain's continuity for anyone verifying across the boundary, and the export
--    must carry the boundary hashes for the verification to be resumable.
--
-- 4. The chain-links trigger serialises inserts against the current head. Under
--    concurrent writers that is a hot row. That is inherent to a hash chain and
--    is the cost of the property; internal/audit takes a mutex for the same
--    reason. If throughput becomes the constraint, the answer is one chain per
--    tenant with a per-tenant head, not a weaker chain.
--
-- 5. The signature column is nullable because audit.New can be constructed
--    without WithSigning, and the tests do. A production deployment should add
--    a NOT NULL constraint once the signer is wired to a KMS key.
