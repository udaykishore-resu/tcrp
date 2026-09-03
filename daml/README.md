# daml

The Canton half of the platform's dual-ledger fund share: the same economic
instrument as `contracts/`, expressed as Daml templates because the two networks
are genuinely different products and pretending otherwise would hide the reason
the platform is dual-ledger at all.

## This directory is not built by this repository's CI

Nothing in the Go build compiles or runs these templates. There is no Daml SDK
in the toolchain this repository assumes, and adding one would mean the platform
no longer runs from a clean clone with only a Go toolchain.

To run the scenarios yourself:

```sh
cd daml
daml build
daml test          # runs every Script in Test.Fund
daml start         # sandbox plus navigator, if you want to click through it
```

The module directory is `Test/`, capitalised, because a Daml module name has to
match its path and `test.Fund` is not a legal module name.

## What is here

| Template | Signatory | Observers | Why it exists |
| --- | --- | --- | --- |
| `FundShare` | issuer | holder | The holding. Keyed by `(issuer, symbol, owner)`, so a balance change is an archive and a create. |
| `TransferProposal` | issuer | owner, recipient | The propose-accept pair: nothing lands in a portfolio without the recipient exercising a choice. |
| `Pledge` | issuer | owner, pledgee | Collateral. The pledgee observes what it holds and can release it; it cannot move it anywhere else. |
| `FundIssuance` | issuer | — | Standing authority to issue, so the transaction tree records which authority created a holding. |

Every template carries an `ensure` clause and every choice asserts its
preconditions with a message, so a rejected command says which rule rejected it.

## The two things this expresses that Solidity does not

**Selective disclosure.** A `FundShare` is signed by the issuer and observed by
its holder. Two holders of the same fund cannot see each other's positions,
cannot see each other's transfers, and cannot learn that the other exists. That
is not a privacy layer over a shared ledger — it is what the ledger is. The
scenario `testSelectiveDisclosure` in `Test/Fund.daml` asserts it directly:
Bravo's `queryContractId` of Alpha's holding returns `None`, and so does
Charlie's, while the issuer sees both because it signed both.

**Propose-accept.** On an ERC-20 the sender pushes tokens at an address and the
recipient finds out afterwards. `ProposeTransfer` takes the units out of the
sender's holding, shows the recipient exactly what is on offer, and waits. For a
security this is not a nicety: an unsolicited holding is a compliance problem
for whoever receives it.

## The Go simulator mirrors the same rules

`internal/ledger/canton.go` is an in-process simulator of this contract store.
It models signatories and observers, archive-and-recreate transfers, contract
keys, the propose-accept pair, the pledge, and the transaction tree.
`ContractsVisibleTo` and `FetchAs` implement the same disclosure rule these
templates get from the ledger model, and `internal/ledger/canton_test.go`
asserts the same property `testSelectiveDisclosure` asserts here: CP-A cannot
see CP-B's holding, and the issuer sees both.

Neither source is generated from the other. A divergence is a bug in whichever
one is wrong, and nothing enforces the correspondence automatically.

## Deliberate scoping

- **One participant.** These scenarios run every party on one participant node.
  Real Canton privacy is enforced across participants by the domain's
  sub-transaction protocol; here it is enforced by the ledger model alone, which
  is the same guarantee at the template level but not the same deployment.
- **Contract keys.** `FundShare` and `Pledge` use `key`/`maintainer`, which is
  the right model for "one holding per party per symbol" and is what makes
  archive-and-recreate unambiguous. Contract keys are deprecated in Daml 3; a
  port would replace them with an explicit index contract, which is more code
  and the same semantics.
- **No time model, no lifecycle events, no NAV.** The share has a quantity and
  no price. Valuation is the risk engine's job in `internal/risk`, against marks
  that do not belong on a ledger.
- **The bridge's replay protection is off-chain here.** `BridgeLock` and
  `BridgeUnlock` carry the nonce so the relayer can read it back, but nothing in
  Daml refuses a reused nonce the way `BridgeVault.sol` does. A production
  deployment would add a `BridgeNonce` contract keyed on the nonce, whose
  creation fails on a duplicate; it is left out here because the Canton side is
  the mint side in the platform's flow and the EVM side holds the collateral.
