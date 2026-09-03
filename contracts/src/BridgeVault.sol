// SPDX-License-Identifier: Apache-2.0
pragma solidity 0.8.24;

import {AccessControl} from "./AccessControl.sol";
import {PermissionedToken} from "./PermissionedToken.sol";

/// @title BridgeVault
/// @notice Custody for shares locked against a mint on another network.
///
/// @dev The vault is the whole of the bridge that lives on this chain. It holds
/// the locked units at an address anybody can query, and it refuses to reuse a
/// nonce. Those two properties are what the off-chain relayer's guarantees rest
/// on:
///
/// - The cross-network supply control proves `balanceOf(vault)` equals the
///   quantity minted on the other network. That works because the vault's
///   holding is a real balance rather than an accumulator the bridge keeps
///   about itself, so a compromised relayer cannot make the two agree by
///   editing its own books.
/// - Replay protection is on chain, not only in the relayer. A relayer that
///   re-observes a lock after a chain reorganisation, or that retries a
///   submission whose acknowledgement was lost, cannot unlock twice however
///   badly it is written, because `unlock` refuses a nonce it has already
///   used. The Go relayer's in-memory guard is a fast path in front of this;
///   this is the guarantee.
///
/// What the vault does not do is decide. It has no view on whether an
/// attestation quorum was reached — that is the relayer's job, and the vault's
/// agent role is what the relayer holds. A production deployment would put a
/// multisig or a threshold-signature verifier in front of `unlock` so the chain
/// itself checks the quorum; this contract's separation of concerns is what
/// makes that a change to one function rather than to the token.
contract BridgeVault is AccessControl {
    PermissionedToken public immutable token;

    /// @notice Total units currently held against mints on the other network.
    uint256 public totalLocked;

    mapping(uint256 nonce => bool) public lockUsed;
    mapping(uint256 nonce => bool) public unlockUsed;

    event Locked(uint256 indexed nonce, address indexed holder, uint256 amount);
    event Unlocked(uint256 indexed nonce, address indexed holder, uint256 amount);

    error NonceUsed(uint256 nonce);
    error ZeroAddress();
    error ZeroAmount();
    error InsufficientLocked(uint256 held, uint256 needed);

    constructor(address owner_, PermissionedToken token_) {
        if (owner_ == address(0) || address(token_) == address(0)) revert ZeroAddress();
        token = token_;
        _grantRole(OWNER_ROLE, owner_);
        _grantRole(AGENT_ROLE, owner_);
    }

    /// @notice Locks shares against a mint on the other network. Agent-only.
    /// @param nonce The bridge nonce. It is the replay key for the whole flow
    /// and it is emitted so the relayer can read it back from the log after a
    /// reorganisation re-includes the transaction — which is why replay
    /// protection is keyed on it rather than on a transaction hash.
    function lock(uint256 nonce, address holder, uint256 amount) external onlyRole(AGENT_ROLE) {
        if (holder == address(0)) revert ZeroAddress();
        if (amount == 0) revert ZeroAmount();
        if (lockUsed[nonce]) revert NonceUsed(nonce);
        lockUsed[nonce] = true;
        totalLocked += amount;
        token.bridgeLock(holder, amount, nonce);
        emit Locked(nonce, holder, amount);
    }

    /// @notice Releases shares after a burn on the other network. Agent-only.
    function unlock(uint256 nonce, address holder, uint256 amount) external onlyRole(AGENT_ROLE) {
        if (holder == address(0)) revert ZeroAddress();
        if (amount == 0) revert ZeroAmount();
        if (unlockUsed[nonce]) revert NonceUsed(nonce);
        if (totalLocked < amount) revert InsufficientLocked(totalLocked, amount);
        unlockUsed[nonce] = true;
        totalLocked -= amount;
        token.bridgeUnlock(holder, amount, nonce);
        emit Unlocked(nonce, holder, amount);
    }
}
