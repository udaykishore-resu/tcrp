# contracts

The Solidity half of the platform's dual-ledger fund share: an ERC-3643-style
permissioned token, its identity registry and compliance module, and the bridge
vault that holds units locked against a mint on the Canton side.

## This directory is not built by this repository's CI

Nothing in the Go build compiles, tests or deploys these contracts. `go build
./...` does not see them and `go test ./...` does not run them. There is no
`solc` and no Foundry in the toolchain this repository assumes, and adding one
would mean the platform no longer runs from a clean clone with only a Go
toolchain — which is a property worth more than an automated Solidity build.

To run the suite yourself:

```sh
cd contracts
forge install foundry-rs/forge-std --no-commit   # once
forge build
forge test -vvv
```

`forge test` runs three kinds of test from `test/PermissionedToken.t.sol`:

- **Unit tests** for each rule: the whitelist, the roles, the pause, the freeze,
  the recovery path, the compliance ceilings, the country and claim
  restrictions, and the vault's nonce replay guard.
- **A fuzz suite**, of which `testFuzz_transferRespectsWhitelist(address,uint256)`
  is the one that matters: for every recipient address the fuzzer can produce, a
  transfer succeeds if and only if the identity registry knows the recipient.
- **An invariant suite** driven by a `Handler` contract that issues, redeems,
  transfers, forces, freezes, pauses, locks and unlocks in random order:
  - `invariant_totalSupplyEqualsSumOfBalances`
  - `invariant_noBalanceOutsideRegistry`
  - `invariant_lockedEqualsVaultBalance`
  - `invariant_supplyEqualsIssuedLessRedeemed`
  - `invariant_holderCountIsAccurate`

The handler swallows expected reverts so a rejected action is a no-op rather
than the end of a run. It deliberately never deregisters an identity: a
deregistered holder keeps its balance, which is correct and is unit-tested, but
it would make `invariant_noBalanceOutsideRegistry` false by construction, and an
invariant that has to enumerate its own exceptions stops being worth running.

## The Go simulator mirrors these rules

`internal/ledger/evm.go` is an in-process simulator of this token. It is not
generated from this source and this source is not generated from it: they are
two statements of one specification. `internal/ledger/evm_test.go` asserts the
same properties the Foundry suite asserts — a transfer to an unregistered
address reverts, the ceilings hold at the boundary, recovery bypasses the pause
and the freeze but not the registry, locking does not change supply — and the
revert reason strings are deliberately the same on both sides, so a test that
matches on a reason is naming the same rule in either language.

A divergence between the two is a bug in whichever one is wrong. Nothing
enforces the correspondence automatically; that is a real limitation and the
honest mitigation is that both suites are short enough to read side by side.

## What is here, and what a production deployment would change

| File | What it is |
| --- | --- |
| `src/AccessControl.sol` | Minimal roles and pause. |
| `src/Interfaces.sol` | `IERC20`, `IIdentityRegistry`, `ICompliance`. |
| `src/IdentityRegistry.sol` | The whitelist: address, country, claim topics. |
| `src/Compliance.sol` | Holder ceiling, per-investor ceiling, country list, required claims. |
| `src/PermissionedToken.sol` | The token. Every credit passes `_beforeTokenTransfer`. |
| `src/BridgeVault.sol` | Custody for locked units, with on-chain nonce replay protection. |

Deliberate scoping, stated rather than left to be discovered:

- **`AccessControl` and `Pausable` are local implementations.** A production
  deployment should import OpenZeppelin's audited versions; the surface used
  here is a strict subset, so the swap is an import change. They are local
  because this repository's discipline is to depend on nothing it does not
  build, and a reader should be able to see every line that governs the token.
- **The constructor grants all three roles to one address.** That is right for a
  test fixture and wrong for a deployment: owner, agent and compliance officer
  should be separate keys held by separate people, and the agent in particular
  should be a multisig.
- **`unlock` trusts the vault's agent.** The attestation quorum is checked
  off-chain, by the relayer in `internal/bridge`. A production deployment would
  put a threshold-signature verifier in front of `unlock` so the chain itself
  checks the quorum; the vault is separated from the token precisely so that is
  a change to one function. The on-chain nonce guard is already there and is the
  guarantee the off-chain replay protection sits in front of rather than
  replaces.
- **`decimals` is zero.** The fund share does not fractionalise; quantities are
  whole shares, matching `domain.Position.Quantity` in the Go code.
- **No upgradeability, no ERC-777 hooks, no allowance path for the vault.** Each
  is attack surface a fund share does not need, and each would want its own
  review if it were added.
- **No gas benchmarking and no formal verification.** Both are things a real
  issuance would do before deployment.
