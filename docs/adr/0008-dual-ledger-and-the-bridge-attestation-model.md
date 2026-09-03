# ADR 0008 — Dual ledger, and the bridge attestation model

## Status

Accepted. In force. `internal/ledger/`, `internal/bridge/`.

## Context

Tokenised fund shares are not settling on one network. Institutional issuance is
happening on permissioned EVM chains under ERC-3643-style restrictions, and on
Canton, and the two are genuinely different products rather than two encodings
of one idea. An EVM token is a shared-state ledger with a compliance hook on
every credit; a Daml contract store is a privacy-first ledger where two holders
of the same instrument cannot learn the other exists.

A platform that models one and calls it "the ledger" hides why the problem is
hard. A platform that models both must answer the question the industry has not
answered well: how a unit moves between them. Every available mechanism is
imperfect, and the choice is which imperfection to take.

## Decision

**Two ledgers, neither authoritative.** `domain.Ledger` has three values: `evm`,
`canton`, and `golden` — the institution's off-chain book. Reconciliation
compares both chains against the book, because "the chain says X" and "our books
say X" are different claims and a system that assumes they agree cannot find the
case where they do not.

**Lock-and-mint over M-of-N ed25519 attestations.** Units lock into a vault on
the source ledger; an independent signer set attests the lock happened and is
final; a quorum authorises a mint of the same quantity on the target. Redemption
is the reverse. Nothing is transferred — each ledger holds its own units and the
vault balance is the link.

**Signatures are over a canonical digest of the attestation's fields**, so a
mutated field, an unknown signer, a signature counted twice and a quorum one
short are four distinct rejections. `SignerSet` refuses a quorum larger than the
set or smaller than one at construction, because that is how a bridge ends up
with a quorum of one.

**Finality is a precondition, and the relayer is built around the four ways it
goes wrong.** Each has a test:

| Failure | Behaviour |
|---|---|
| Lock orphaned **before** finality | Nothing minted. Attestation abandoned, `tcrp_bridge_abandoned_total` increments, alert raised. |
| Lock orphaned **after** finality | `AlertFinalityViolation`. **No automatic repair**; the relayer does not mint again. A stuck bridge is chosen over a silent double mint. |
| Same lock observed twice | Replay protection keyed on **the bridge nonce carried by the lock**, not the transaction hash or block, so re-inclusion under a different hash mints nothing. |
| Signer unreachable, submission fails | A quorum of M of N still authorises; a failed submission retries with backoff, idempotent by nonce. |

**Replay protection exists at two layers.** Off-chain in the relayer, keyed on
the nonce; on-chain in `BridgeVault.sol`, which refuses a reused nonce. The
on-chain guard is the guarantee the off-chain one sits in front of rather than
replaces. Canton has no equivalent — the templates carry the nonce so the
relayer can read it back, but nothing in Daml refuses a duplicate.
`daml/README.md` names the fix.

## Consequences

**Supply conservation is checkable and is checked.** `SETTLE-1`, `SETTLE-4` and
`SETTLE-5` are conformance invariants evaluated against the ledgers and the
bridge's own records rather than the relayer's belief about itself.
`internal/bridge/supply.go` reconciles locked, minted, burned and unlocked and
exposes `tcrp_bridge_supply_delta`; non-zero is a page.

**Two ledger models means two rule sets to keep in step.** The EVM simulator
enforces the whitelist, ceilings, country list, claims, pause, freeze and
recovery. The Canton simulator enforces signatories and observers, contract
keys, propose-accept, the pledge and selective disclosure. Neither is a superset
of the other, which is the honest reason the platform is dual-ledger rather than
a demonstration of two adapters over one interface.

**Nothing enforces the correspondence with the Solidity and Daml sources.** They
are two statements of one specification, neither generated from the other. The
revert reason strings are deliberately identical on the EVM side, and
`canton_test.go` asserts the same disclosure property `testSelectiveDisclosure`
asserts. That is the whole mitigation and it is manual.

## What this model does not defend against

- **A compromised quorum.** M colluding signers can attest to a lock that never
  happened. Nothing detects that at attestation time; supply reconciliation
  detects it afterwards, which is forensics rather than prevention. Every
  production bridge failure of consequence has been this failure.
- **Signer key theft.** Keys are in memory here. A real deployment means
  separate processes, separate failure domains, separate HSMs — the difference
  between "M independent parties" and "M keys on one host", and the protocol
  assumes the former.
- **A post-finality reorg.** Detected and alerted, not repaired.
- **Governance of the signer set.** There is none: changing it means a new
  `SignerSet`. No rotation, revocation or threshold change.
- **Censorship.** A relayer declining to relay is indistinguishable from one
  that has not seen the lock. No forced-inclusion path.
- **Economic attacks.** No fee model, rate limiting, bond or slashing. Attesting
  falsely costs nothing.
- **The vault contract.** `unlock` trusts the vault's agent; the quorum is
  checked off-chain. Production puts a threshold-signature verifier in front of
  it, and the vault is separated from the token so that is one function.

## Alternatives considered

**Single ledger.** Removes the problem rather than solving it, and the problem is
the interesting part.

**Burn-and-mint.** Removes the vault and its custody risk, and with it the
on-chain balance to reconcile against. `SETTLE-1` is checkable precisely because
lock-and-mint leaves a number that must equal the target's minted supply, and
`invariant_lockedEqualsVaultBalance` asserts it on chain.

**Atomic swap with HTLCs.** Trust-minimised and right where it applies. Needs the
same hashlock on both chains, both parties online within the timeout, and does
not model issuance or redemption at all — most of what a fund share does.

**A notary or a light-client bridge.** A notary is this design renamed. A light
client is genuinely trust-minimised, a much larger chain-pair-specific project,
and not implementable against a Canton domain.
