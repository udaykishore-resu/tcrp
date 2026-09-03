-- 0001_agent.sql — the production persistence path for internal/agent.Store.
--
-- agent.MemoryStore keeps runs and approvals in memory and mirrors them to JSON
-- files, which is what lets the platform run from a clean clone. This schema is
-- the same contract with the same semantics, for a deployment.
--
-- The point of this file is not the tables. It is that the controls the
-- application enforces are ALSO enforced here, as constraints, so that a bug in
-- the runtime, a second writer, a migration script or a database console cannot
-- produce a state the application would have refused. Every constraint below
-- names the control it enforces.
--
-- Target: PostgreSQL 14 or later (gen_random_uuid, JSONB, generated columns).
-- Idempotent: safe to re-run.

BEGIN;

-- ---------------------------------------------------------------------------
-- runs
-- ---------------------------------------------------------------------------

-- Mirrors agent.RunStatus. A closed enum rather than free text, because
-- "awaiting_approval" is a state the system must survive a restart in and a
-- typo in it would silently make a paused run unresumable.
DO $$ BEGIN
  CREATE TYPE run_status AS ENUM (
    'pending', 'running', 'awaiting_approval',
    'completed', 'failed', 'blocked', 'rejected'
  );
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS runs (
  id                  TEXT        PRIMARY KEY,
  query               TEXT        NOT NULL,

  -- The acting principal, flattened from identity.Principal. subject and
  -- on_behalf_of are separate columns rather than JSONB fields because the
  -- four-eyes constraint on approvals has to reference them.
  principal_subject   TEXT        NOT NULL,
  principal_kind      TEXT        NOT NULL CHECK (principal_kind IN ('human', 'agent')),
  principal_tenant    TEXT        NOT NULL,
  principal_desk      TEXT,
  on_behalf_of        TEXT,
  principal_roles     TEXT[]      NOT NULL DEFAULT '{}',
  principal_scopes    TEXT[]      NOT NULL DEFAULT '{}',

  environment         TEXT        NOT NULL,
  status              run_status  NOT NULL,
  trace_id            TEXT,

  answer              TEXT,
  error               TEXT,
  failures            INTEGER     NOT NULL DEFAULT 0 CHECK (failures >= 0),

  -- CONTROL: indirect prompt injection. Set false when the guardrail finds
  -- instruction-like text in retrieved content; the policy rule
  -- deny-state-change-on-untrusted-context then refuses every non-read-only
  -- call for the rest of the run. It is a column rather than a derived value
  -- because the run must resume with it after a restart.
  retrieval_trusted   BOOLEAN     NOT NULL DEFAULT TRUE,

  -- CONTROL: model substitution and policy tampering are attributable per run.
  -- Which model, which policy bundle version, which guardrail policy produced
  -- this run's behaviour. See docs/threat-model.md sections 6 and 8.
  model_name          TEXT        NOT NULL,
  policy_bundle       TEXT,
  guard_policy        TEXT,

  -- The message history and accumulated tool data. JSONB rather than normalised
  -- because nothing queries inside them; they exist to resume a run.
  messages            JSONB       NOT NULL DEFAULT '[]'::jsonb,
  tool_data           JSONB       NOT NULL DEFAULT '[]'::jsonb,
  -- Grounded strings the output guardrail checks the answer's numbers against.
  grounded            JSONB       NOT NULL DEFAULT '[]'::jsonb,

  -- agent.PendingCall. Non-null exactly while the run is paused on a gate.
  pending             JSONB,

  created_at          TIMESTAMPTZ NOT NULL,
  updated_at          TIMESTAMPTZ NOT NULL,

  -- CONTROL: a paused run must be resumable. A run in awaiting_approval with no
  -- pending call cannot be resumed and is a corrupt state; a run that is not
  -- awaiting must not carry one, or a resume could execute a call the loop has
  -- already moved past.
  CONSTRAINT pending_iff_awaiting CHECK (
    (status = 'awaiting_approval' AND pending IS NOT NULL) OR
    (status <> 'awaiting_approval' AND pending IS NULL)
  ),

  -- CONTROL: the agent never becomes the system. An agent principal always acts
  -- on behalf of a named human; without that the four-eyes check below has
  -- nothing to compare against, and conformance invariant AGENT-4 (every span
  -- carries the principal identity) has half an identity to carry.
  CONSTRAINT agent_acts_on_behalf CHECK (
    principal_kind <> 'agent' OR (on_behalf_of IS NOT NULL AND on_behalf_of <> '')
  ),

  CONSTRAINT updated_not_before_created CHECK (updated_at >= created_at)
);

-- The approvals queue: "which runs are waiting". Partial, because the paused
-- runs are a tiny fraction of the table and this is the query an operator runs.
CREATE INDEX IF NOT EXISTS runs_awaiting_idx
  ON runs (created_at)
  WHERE status = 'awaiting_approval';

CREATE INDEX IF NOT EXISTS runs_principal_idx ON runs (principal_subject, created_at DESC);
CREATE INDEX IF NOT EXISTS runs_tenant_idx    ON runs (principal_tenant, created_at DESC);
CREATE INDEX IF NOT EXISTS runs_trace_idx     ON runs (trace_id) WHERE trace_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- run_steps
-- ---------------------------------------------------------------------------
-- One governed step, retained in full so a trajectory can be replayed and
-- scored without re-running the model. This is the table the eval harness and
-- the drift detector read.

CREATE TABLE IF NOT EXISTS run_steps (
  run_id          TEXT        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
  n               INTEGER     NOT NULL CHECK (n >= 1),

  tool            TEXT,
  tier            TEXT        CHECK (tier IS NULL OR tier IN ('read_only', 'state_change', 'money_movement')),
  args            JSONB,
  reasoning       TEXT,

  -- CONTROL: AGENT-2 — every tool invocation is preceded by an allowing policy
  -- decision for that tool. These four columns are that decision, recorded
  -- against the step it authorized.
  policy_allow    BOOLEAN     NOT NULL DEFAULT FALSE,
  policy_rule     TEXT,
  policy_reason   TEXT,
  obligations     TEXT[]      NOT NULL DEFAULT '{}',

  -- CONTROL: AGENT-3 — the guardrail ran. A step with an invoked outcome and no
  -- guard verdict is the shape AGENT-3 fails on.
  guard_verdict   TEXT,
  guard_reason    TEXT,

  approval_id     TEXT,
  approved_by     TEXT[]      NOT NULL DEFAULT '{}',

  outcome         TEXT        NOT NULL CHECK (outcome IN (
                    'invoked', 'policy_denied', 'guard_blocked', 'awaiting_approval',
                    'tool_error', 'answered', 'unknown_tool', 'step_budget_exceeded')),
  error           TEXT,
  duration_ms     DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
  span_id         TEXT,
  at              TIMESTAMPTZ NOT NULL,

  PRIMARY KEY (run_id, n),

  -- CONTROL: money movement never executes without an approval. AGENT-1 is
  -- evaluated against the audit log; this is the same claim asserted where the
  -- row is written, so a step that reached a money-movement tool with no
  -- approval attached cannot be recorded at all.
  CONSTRAINT money_movement_carries_an_approval CHECK (
    tier IS DISTINCT FROM 'money_movement'
    OR outcome <> 'invoked'
    OR (approval_id IS NOT NULL AND cardinality(approved_by) >= 1)
  ),

  -- CONTROL: an invoked step was allowed. The inverse — a denied step recorded
  -- as invoked — is exactly the shape AGENT-2 exists to catch.
  CONSTRAINT invoked_implies_allowed CHECK (
    outcome <> 'invoked' OR policy_allow
  )
);

CREATE INDEX IF NOT EXISTS run_steps_tool_idx     ON run_steps (tool, at DESC) WHERE tool IS NOT NULL;
CREATE INDEX IF NOT EXISTS run_steps_denied_idx   ON run_steps (at DESC) WHERE outcome = 'policy_denied';
CREATE INDEX IF NOT EXISTS run_steps_approval_idx ON run_steps (approval_id) WHERE approval_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- approvals
-- ---------------------------------------------------------------------------

DO $$ BEGIN
  CREATE TYPE approval_status AS ENUM ('pending', 'granted', 'denied', 'expired');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS approvals (
  id              TEXT            PRIMARY KEY,
  run_id          TEXT            NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
  step            INTEGER         NOT NULL CHECK (step >= 1),

  tool            TEXT            NOT NULL,
  tier            TEXT            NOT NULL CHECK (tier IN ('read_only', 'state_change', 'money_movement')),
  summary         TEXT            NOT NULL,
  args            JSONB,

  -- CONTROL: approval replay. args_hash is the canonical JSON of the arguments
  -- at request time. What the approver saw is what executes: the approval
  -- authorises THAT transfer, not "a transfer". A resume compares against it.
  args_hash       TEXT            NOT NULL,

  -- CONTROL: four-eyes. requester and on_behalf_of are what the self-approval
  -- constraint on approval_grants references.
  requester       TEXT            NOT NULL,
  on_behalf_of    TEXT,

  required_roles  TEXT[]          NOT NULL DEFAULT '{}',

  -- CONTROL: dual approval above the policy bundle's threshold. The count comes
  -- from the policy obligation (require_human_approval = 1,
  -- require_dual_approval = 2), not from application code, so raising the
  -- threshold is a bundle change rather than a deploy.
  required_count  INTEGER         NOT NULL CHECK (required_count >= 1),

  status          approval_status NOT NULL DEFAULT 'pending',
  policy_rule     TEXT,

  created_at      TIMESTAMPTZ     NOT NULL,

  -- CONTROL: approval freshness. Mirrors agent.Config.ApprovalWindow (15 min)
  -- and the policy rule deny-stale-approval (900s). An approval older than the
  -- window no longer describes current market conditions.
  expires_at      TIMESTAMPTZ     NOT NULL,
  decided_at      TIMESTAMPTZ,

  CONSTRAINT expiry_after_creation CHECK (expires_at > created_at),

  -- A terminal approval has a decision time; a pending one does not.
  CONSTRAINT decided_iff_terminal CHECK (
    (status = 'pending' AND decided_at IS NULL) OR
    (status <> 'pending' AND decided_at IS NOT NULL)
  )
);

CREATE INDEX IF NOT EXISTS approvals_pending_idx
  ON approvals (created_at)
  WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS approvals_expiring_idx
  ON approvals (expires_at)
  WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS approvals_run_idx ON approvals (run_id, step);

-- ---------------------------------------------------------------------------
-- approval_grants
-- ---------------------------------------------------------------------------
-- One human's response. Grants and denials share the table with a decision
-- column, because "who responded to this gate and how" is one question.

CREATE TABLE IF NOT EXISTS approval_grants (
  id             BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  approval_id    TEXT        NOT NULL REFERENCES approvals (id) ON DELETE CASCADE,
  approver       TEXT        NOT NULL,
  approver_roles TEXT[]      NOT NULL DEFAULT '{}',
  decision       TEXT        NOT NULL CHECK (decision IN ('grant', 'deny')),
  note           TEXT,
  at             TIMESTAMPTZ NOT NULL
);

-- CONTROL: an approver cannot respond twice.
--
-- agent.ApprovalService.Grant returns ErrDuplicateGrant for this, checking
-- against both grants and denials. That check is a read followed by a write and
-- is therefore racy under concurrency; this index is not. Two approvers clicking
-- at the same moment cannot both be counted, and one person cannot satisfy a
-- required_count of 2 by responding twice.
CREATE UNIQUE INDEX IF NOT EXISTS approval_grants_one_per_approver
  ON approval_grants (approval_id, approver);

-- CONTROL: four eyes — a requester may not approve their own request.
--
-- Enforced in agent.ApprovalService.Grant (ErrSelfApproval), restated as the
-- policy rule deny-self-approval at dispatch, and restated here so that no
-- writer can bypass both. The subquery covers the on_behalf_of case: the agent
-- runs as svc/collateral-agent on behalf of j.okafor, and j.okafor approving is
-- self-approval even though the subjects differ.
--
-- A trigger rather than a CHECK because a CHECK cannot reference another table.
CREATE OR REPLACE FUNCTION approval_grants_reject_self_approval()
RETURNS TRIGGER AS $$
DECLARE
  req  TEXT;
  obo  TEXT;
BEGIN
  SELECT requester, on_behalf_of INTO req, obo
    FROM approvals WHERE id = NEW.approval_id;

  IF NEW.approver = req THEN
    RAISE EXCEPTION
      'self-approval refused: % is the requester of approval %', NEW.approver, NEW.approval_id
      USING ERRCODE = 'check_violation';
  END IF;

  IF obo IS NOT NULL AND obo <> '' AND NEW.approver = obo THEN
    RAISE EXCEPTION
      'self-approval refused: % is the human approval % was raised on behalf of', NEW.approver, NEW.approval_id
      USING ERRCODE = 'check_violation';
  END IF;

  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS approval_grants_no_self_approval ON approval_grants;
CREATE TRIGGER approval_grants_no_self_approval
  BEFORE INSERT OR UPDATE ON approval_grants
  FOR EACH ROW EXECUTE FUNCTION approval_grants_reject_self_approval();

-- CONTROL: a grant cannot be recorded after the window has closed.
--
-- ApprovalService.Grant checks this and marks the approval expired as it does.
-- A grant that races the expiry sweep must still fail closed.
CREATE OR REPLACE FUNCTION approval_grants_reject_late()
RETURNS TRIGGER AS $$
DECLARE
  exp TIMESTAMPTZ;
  st  approval_status;
BEGIN
  SELECT expires_at, status INTO exp, st FROM approvals WHERE id = NEW.approval_id;

  IF NEW.at > exp THEN
    RAISE EXCEPTION
      'approval % closed at %; a response at % is too late', NEW.approval_id, exp, NEW.at
      USING ERRCODE = 'check_violation';
  END IF;

  IF st <> 'pending' THEN
    RAISE EXCEPTION
      'approval % is %; it no longer accepts responses', NEW.approval_id, st
      USING ERRCODE = 'check_violation';
  END IF;

  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS approval_grants_no_late_response ON approval_grants;
CREATE TRIGGER approval_grants_no_late_response
  BEFORE INSERT ON approval_grants
  FOR EACH ROW EXECUTE FUNCTION approval_grants_reject_late();

-- CONTROL: an approval_grant is a fact about a decision that was made. It is
-- never edited and never removed. Same reasoning as the audit chain in 0002.
CREATE OR REPLACE FUNCTION approval_grants_append_only()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION 'approval_grants is append-only: % is not permitted', TG_OP
    USING ERRCODE = 'insufficient_privilege';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS approval_grants_no_update ON approval_grants;
CREATE TRIGGER approval_grants_no_update
  BEFORE UPDATE OR DELETE ON approval_grants
  FOR EACH ROW EXECUTE FUNCTION approval_grants_append_only();

CREATE INDEX IF NOT EXISTS approval_grants_approver_idx ON approval_grants (approver, at DESC);

-- ---------------------------------------------------------------------------
-- A read model the operator actually asks for
-- ---------------------------------------------------------------------------
-- "What is waiting on me." ApprovalService.Pending is the application's version;
-- this is the same question answerable by a process that started none of the
-- runs, which is the property that makes approvals durable objects rather than
-- goroutines.

CREATE OR REPLACE VIEW pending_approvals AS
SELECT
  a.id,
  a.run_id,
  a.tool,
  a.tier,
  a.summary,
  a.requester,
  a.on_behalf_of,
  a.required_roles,
  a.required_count,
  COUNT(g.id) FILTER (WHERE g.decision = 'grant') AS grants,
  a.created_at,
  a.expires_at,
  (a.expires_at - now())                          AS time_remaining
FROM approvals a
LEFT JOIN approval_grants g ON g.approval_id = a.id
WHERE a.status = 'pending'
GROUP BY a.id
ORDER BY a.created_at;

COMMIT;

-- ---------------------------------------------------------------------------
-- Notes for whoever deploys this
-- ---------------------------------------------------------------------------
--
-- 1. GRANTS. The application role needs INSERT and SELECT everywhere, UPDATE on
--    runs and approvals, and NOTHING on approval_grants beyond INSERT and
--    SELECT. The append-only trigger enforces that anyway; the grant makes the
--    intent legible in \dp.
--
-- 2. The expiry sweep (ApprovalService.ExpireDue) is the only writer that moves
--    an approval to 'expired'. Run it on a schedule shorter than the window.
--
-- 3. runs.messages, tool_data and grounded are JSONB and unbounded. A long run
--    with large tool results will be a large row. TOAST handles it; a retention
--    policy is still a good idea and is not defined here because it is a
--    business decision, not a schema one.
--
-- 4. There is no ON DELETE for runs other than the cascade. Deleting a run
--    deletes its steps and its approvals, which destroys the evidence AGENT-1
--    and AGENT-2 are evaluated against. Do not grant DELETE on runs to the
--    application role.
