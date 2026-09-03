# Phase 2 — Dual-ledger settlement

`internal/ledger`, `internal/bridge`, plus `contracts/` (Solidity) and `daml/`.

3,605 lines of Go source, 2,382 of test. 1,420 lines of Solidity, 584 of Daml.

## What this phase delivers

The same tokenised fund share existing on two settlement networks at once, with
a bridge between them that proves nothing was created or destroyed crossing it.

### Two ledgers with genuinely different semantics

The point of modelling both is that they are **not** two encodings of one idea.
A platform that models one and calls it "the ledger" hides the reason the problem
is hard.

**`internal/ledger/evm.go`** — an ERC-3643-style permissioned token. Every credit
passes a compliance hook. It enforces the whitelist, the holder ceiling, the
per-investor ceiling, the country list, required claim topics, pause, freeze, the
forced-transfer recovery path, and a bridge vault holding locked units. It models
blocks, a head and a finalised height, reverts, and **reorganisations that orphan
transactions**.

**`internal/ledger/canton.go`** — a Daml contract store. Signatories and
observers, archive-and-recreate transfers, contract keys, the propose-accept
pair, the pledge, and a transaction tree. `ContractsVisibleTo` and `FetchAs`
implement **selective disclosure**: two holders of the same fund cannot see each
other's positions, cannot see each other's transfers, and cannot learn the other
exists. That is not a privacy layer over a shared ledger; it is what the ledger
is.

`ledger.World` builds both from the seeded dataset, plus the mappings the rest of
the platform needs — a counterparty is a hex address on one network and a Daml
party on the other, and something has to hold that translation. It reproduces the
seed's opening positions rather than replaying the seed's event stream, because
the seed's events include the three anomalies that exist to give the lakehouse's
reconciliation controls something to find, and replaying them here would move
balances the risk engine has already been told about.

CP-HOTEL is deliberately absent from the identity registry, exactly as the seed
leaves it un-whitelisted.

### The bridge

Lock-and-mint over an M-of-N set of ed25519 attestations. Units are locked into a
vault on the source ledger; an independent set of signers attests that the lock
happened and is final; a quorum authorises a mint of the same quantity on the
target. Redemption is the reverse. Nothing is transferred — each ledger holds its
own units and the vault balance is the link.

**The interesting part of a bridge is not the happy path, which is a for loop.**
The relayer is built around four failure modes and each has a test:

| Failure | Behaviour |
|---|---|
| Lock orphaned by a reorg **before** finality | Nothing was minted. The pending attestation is abandoned and an alert says so. |
| Lock orphaned by a reorg **after** finality | Units already exist against collateral that no longer does. **No safe automatic repair**: `AlertFinalityViolation`, and the relayer does not mint again — because the one thing worse than a stuck bridge is a silent double mint. |
| The same lock observed twice | Replay protection keyed on **the bridge nonce carried by the lock**, not on the transaction hash or the block, so re-inclusion under a different hash mints nothing. |
| A signer unreachable, or target submission fails | A quorum of M of N still authorises; a failed submission retries with backoff and is idempotent by nonce. |

Attestations are signed over a canonical digest of their fields, so a mutated
field, an unknown signer, a signature counted twice and a quorum one short are
four distinct rejections — and all four are tested rather than asserted.

`internal/bridge/supply.go` reconciles locked, minted, burned and unlocked
continuously and exposes `tcrp_bridge_supply_delta`, which must be zero.

### The specification the simulators mirror

`contracts/` holds the Solidity: `AccessControl`, `Interfaces`,
`IdentityRegistry`, `Compliance`, `PermissionedToken`, `BridgeVault` — with a
Foundry suite of unit tests, a fuzz suite
(`testFuzz_transferRespectsWhitelist`) and five invariants driven by a random
`Handler`.

`daml/` holds the Daml: `FundShare`, `TransferProposal`, `Pledge`,
`FundIssuance` — with scripts including `testSelectiveDisclosure`.

Neither source is generated from the other. They are two statements of one
specification, and the revert reason strings are deliberately identical on the
EVM side so a test matching on a reason names the same rule in either language.

ADR 0008 covers the whole design, including a section on what the attestation
model **does not** defend against.

## Demonstrating it

```sh
go test ./internal/ledger/... ./internal/bridge/... -count=1 -v
```

Selective disclosure — the property Solidity cannot express:

```sh
go test ./internal/ledger/ -run TestSelectiveDisclosure -count=1 -v
```

The whitelist, at the ledger and at the gateway. `TestSmokeRun` shows the
gateway refusing first, before anything reaches a chain:

```sh
go test ./internal/platform/ -run TestSmokeRun -v -count=1 | grep -A2 'CP-HOTEL'
```

```
=== Transfer 5,000,000 shares to CP-HOTEL
status=completed steps=4 trajectory=[check_whitelist draft_transfer execute_transfer]
  step 3 tool=execute_transfer  outcome=policy_denied  rule=deny-transfer-to-non-whitelisted
```

Concurrency, because a ledger that is only correct single-threaded is not a
ledger:

```sh
go test ./internal/ledger/ -run 'Concurrent|SlowSubscriber' -race -count=1 -v
```

`TestEVMConcurrentWritesConserveSupply`, `TestCantonConcurrentReadsAndWrites` and
`TestSlowSubscriberDoesNotBlockTheLedger`.

The Solidity and Daml, **only where those toolchains exist** — nothing in this
repository's Go build compiles or runs them:

```sh
cd contracts && forge install foundry-rs/forge-std --no-commit && forge test -vvv
cd daml     && daml build && daml test
```

`.github/workflows/foundry.yml` and `daml.yml` run those, gated so they skip
where the toolchain is absent.

## The tests that prove it

| Package | Tests | What they establish |
|---|---:|---|
| `internal/ledger` | 36 | A transfer to an unregistered address reverts; the compliance ceilings hold at the boundary; recovery bypasses pause and freeze but **not** the registry; locking does not change supply; selective disclosure — CP-A cannot see CP-B's holding while the issuer sees both; propose-accept; contract keys; reorgs orphan transactions; concurrent writes are safe |
| `internal/bridge` | 37 | All four reorg and quorum failure modes; a mutated attestation field is rejected; an unknown signer is rejected; a signature counted twice does not reach quorum; a quorum one short is rejected; supply is conserved; a replayed nonce mints nothing; redemption is the mirror of the mint path |

Conformance invariants over this phase, evaluated against the ledgers and the
bridge's own records rather than against the relayer's belief about itself:

| Invariant | Statement | Severity |
|---|---|---|
| `SETTLE-1` | Supply is conserved across the bridge | critical |
| `SETTLE-2` | No token holder sits outside the identity registry on either ledger | — |
| `SETTLE-3` | Every bridge mint carries a quorum attestation and a matching source lock | — |
| `SETTLE-4` | No bridge nonce is minted twice | — |
| `SETTLE-5` | Circulating supply equals issuance less redemptions | — |

## What this phase deliberately does not do

- **There is no chain.** Both ledgers are in-process simulators. No consensus, no
  gas, no network, no mempool, no MEV.
- **The bridge signers are in-process and hold keys in memory.** A real
  deployment runs them as separate processes in separate failure domains with
  keys in separate HSMs. The quorum arithmetic is identical either way; the
  failure domains are the whole point and here they do not exist.
- **The Solidity and Daml are not built by anything here.** No `solc`, no Daml
  SDK, no CI job in this repository's default workflow. Adding one would cost the
  clean-clone property, which is worth more. `contracts/README.md` and
  `daml/README.md` both say so at the top.
- **Nothing enforces the correspondence between the simulators and those
  sources.** A divergence is a bug in whichever one is wrong, and the honest
  mitigation is that both suites are short enough to read side by side.
- **No governance over the signer set, no fee model, no rate limiting.** Changing
  the set means constructing a new `SignerSet`.
- **The Canton side has no on-chain nonce guard.** `BridgeLock` and
  `BridgeUnlock` carry the nonce so the relayer can read it back, but nothing in
  Daml refuses a duplicate. `daml/README.md` names the fix: a `BridgeNonce`
  contract keyed on the nonce, whose creation fails on a duplicate.
- **`BridgeVault.unlock` trusts the vault's agent.** The quorum is checked
  off-chain. A production deployment puts a threshold-signature verifier in front
  of it so the chain checks the quorum itself; the vault is separated from the
  token precisely so that is a change to one function.

## What the next phase needs from this one

Phase 3 needs the `domain.LedgerEvent` stream both simulators emit — that is the
seam an indexer against a real chain replaces them at, and everything above it is
indifferent to which side of the swap it is on.
