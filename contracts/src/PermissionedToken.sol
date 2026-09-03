// SPDX-License-Identifier: Apache-2.0
pragma solidity 0.8.24;

import {AccessControl, Pausable} from "./AccessControl.sol";
import {ICompliance, IERC20, IIdentityRegistry} from "./Interfaces.sol";

/// @title PermissionedToken
/// @notice An ERC-3643-style permissioned share of a tokenised money market
/// fund.
///
/// @dev The difference between this and an ERC-20 is one line in one place:
/// every credit passes `_beforeTokenTransfer`, which asks the identity registry
/// whether the recipient may hold the token at all and the compliance module
/// whether this particular movement is permitted. Everything else — the roles,
/// the pause, the freeze, the forced transfer — exists because that one line
/// has consequences that somebody has to be able to unwind.
///
/// Design decisions worth stating rather than discovering:
///
/// - A transfer to an address the registry does not know reverts. Not returns
///   false, not silently succeeds: reverts, so an integrator cannot ignore it.
/// - `forcedTransfer` bypasses the pause, the freeze and the compliance
///   ceilings but not the registry. Recovery exists because the ordinary path
///   is blocked; if it honoured the same blocks it would not be recovery. It
///   still refuses an unverified recipient, because recovery must not become
///   the hole in the permissioning.
/// - Locking for the bridge is a token operation rather than an allowance and a
///   pull. The vault holds real units at a real address, so the cross-network
///   supply control can check `balanceOf(vault)` rather than trusting a number
///   the bridge keeps about itself.
/// - There is no allowance path for the vault and no ERC-777-style hooks. Both
///   are attack surface a fund share does not need.
///
/// The Go simulator in internal/ledger/evm.go mirrors these rules, and
/// internal/ledger/evm_test.go asserts the same properties this contract's
/// Foundry suite asserts. Neither is generated from the other: they are two
/// statements of one specification, and a divergence between them is a bug in
/// whichever one is wrong.
contract PermissionedToken is AccessControl, Pausable, IERC20 {
    string public name;
    string public symbol;
    uint8 public constant decimals = 0; // whole shares; the fund does not fractionalise

    uint256 private _totalSupply;
    mapping(address account => uint256) private _balances;
    mapping(address owner => mapping(address spender => uint256)) private _allowances;
    mapping(address account => bool) public frozen;

    IIdentityRegistry public identityRegistry;
    ICompliance public compliance;
    address public bridgeVault;

    event IdentityRegistryUpdated(address indexed registry);
    event ComplianceUpdated(address indexed compliance);
    event BridgeVaultUpdated(address indexed vault);
    event AddressFrozen(address indexed account, bool frozen, address indexed by);
    event ForcedTransfer(address indexed from, address indexed to, uint256 amount, address indexed by);
    event Minted(address indexed to, uint256 amount, string memo);
    event Redeemed(address indexed from, uint256 amount, string memo);
    event BridgeLocked(address indexed holder, uint256 amount, uint256 indexed nonce);
    event BridgeUnlocked(address indexed holder, uint256 amount, uint256 indexed nonce);

    error ZeroAddress();
    error ZeroAmount();
    error NotVerified(address account);
    error AccountFrozen(address account);
    error InsufficientBalance(address account, uint256 held, uint256 needed);
    error InsufficientAllowance(address owner, address spender, uint256 allowed, uint256 needed);
    error ComplianceRejected(string reason);
    error NotVault(address caller);

    constructor(string memory name_, string memory symbol_, address owner_, IIdentityRegistry registry_, ICompliance compliance_) {
        if (owner_ == address(0) || address(registry_) == address(0) || address(compliance_) == address(0)) {
            revert ZeroAddress();
        }
        name = name_;
        symbol = symbol_;
        identityRegistry = registry_;
        compliance = compliance_;
        _grantRole(OWNER_ROLE, owner_);
        _grantRole(AGENT_ROLE, owner_);
        _grantRole(COMPLIANCE_ROLE, owner_);
    }

    // --- administration ---------------------------------------------------

    /// @notice Repoints the identity registry. Owner-only.
    function setIdentityRegistry(IIdentityRegistry registry_) external onlyRole(OWNER_ROLE) {
        if (address(registry_) == address(0)) revert ZeroAddress();
        identityRegistry = registry_;
        emit IdentityRegistryUpdated(address(registry_));
    }

    /// @notice Repoints the compliance module. Owner-only.
    function setCompliance(ICompliance compliance_) external onlyRole(OWNER_ROLE) {
        if (address(compliance_) == address(0)) revert ZeroAddress();
        compliance = compliance_;
        emit ComplianceUpdated(address(compliance_));
    }

    /// @notice Names the bridge vault. Owner-only. Only this address may call
    /// `bridgeLock` and `bridgeUnlock`.
    function setBridgeVault(address vault) external onlyRole(OWNER_ROLE) {
        if (vault == address(0)) revert ZeroAddress();
        bridgeVault = vault;
        emit BridgeVaultUpdated(vault);
    }

    /// @notice Halts every ordinary movement. Owner-only.
    function pause() external onlyRole(OWNER_ROLE) {
        _pause();
    }

    /// @notice Resumes the token. Owner-only.
    function unpause() external onlyRole(OWNER_ROLE) {
        _unpause();
    }

    /// @notice Freezes or unfreezes an account. Agent-only.
    function setAddressFrozen(address account, bool freeze) external onlyRole(AGENT_ROLE) {
        frozen[account] = freeze;
        emit AddressFrozen(account, freeze, msg.sender);
    }

    // --- ERC-20 -----------------------------------------------------------

    /// @inheritdoc IERC20
    function totalSupply() external view returns (uint256) {
        return _totalSupply;
    }

    /// @inheritdoc IERC20
    function balanceOf(address account) external view returns (uint256) {
        return _balances[account];
    }

    /// @inheritdoc IERC20
    function allowance(address owner, address spender) external view returns (uint256) {
        return _allowances[owner][spender];
    }

    /// @inheritdoc IERC20
    function approve(address spender, uint256 amount) external returns (bool) {
        _allowances[msg.sender][spender] = amount;
        emit Approval(msg.sender, spender, amount);
        return true;
    }

    /// @inheritdoc IERC20
    function transfer(address to, uint256 amount) external returns (bool) {
        _transfer(msg.sender, to, amount);
        return true;
    }

    /// @inheritdoc IERC20
    function transferFrom(address from, address to, uint256 amount) external returns (bool) {
        uint256 allowed = _allowances[from][msg.sender];
        if (allowed < amount) revert InsufficientAllowance(from, msg.sender, allowed, amount);
        _allowances[from][msg.sender] = allowed - amount;
        _transfer(from, to, amount);
        return true;
    }

    // --- issuance lifecycle -----------------------------------------------

    /// @notice Issues new shares. Agent-only.
    function mint(address to, uint256 amount, string calldata memo) external onlyRole(AGENT_ROLE) whenNotPaused {
        if (to == address(0)) revert ZeroAddress();
        if (amount == 0) revert ZeroAmount();
        _requireVerified(to);
        if (frozen[to]) revert AccountFrozen(to);
        _requireCompliant(address(0), to, amount);

        _totalSupply += amount;
        _balances[to] += amount;
        compliance.created(to, amount);
        emit Transfer(address(0), to, amount);
        emit Minted(to, amount, memo);
    }

    /// @notice Retires shares on redemption. Agent-only.
    /// @dev It works on a frozen account on purpose: a redemption is how a
    /// holder that has been frozen for a sanctions match is eventually made
    /// whole or wound down, and blocking it would leave the position stranded.
    function redeem(address from, uint256 amount, string calldata memo) external onlyRole(AGENT_ROLE) whenNotPaused {
        if (amount == 0) revert ZeroAmount();
        uint256 held = _balances[from];
        if (held < amount) revert InsufficientBalance(from, held, amount);

        _balances[from] = held - amount;
        _totalSupply -= amount;
        compliance.destroyed(from, amount);
        emit Transfer(from, address(0), amount);
        emit Redeemed(from, amount, memo);
    }

    /// @notice Moves shares without the holder's consent. Agent-only.
    /// @dev The recovery path: a lost key, a court order, a mistaken
    /// settlement. It bypasses the pause, the freeze and the compliance
    /// ceilings — each of which may be the reason recovery is needed — but not
    /// the identity registry.
    function forcedTransfer(address from, address to, uint256 amount) external onlyRole(AGENT_ROLE) {
        if (to == address(0)) revert ZeroAddress();
        if (amount == 0) revert ZeroAmount();
        _requireVerified(to);
        uint256 held = _balances[from];
        if (held < amount) revert InsufficientBalance(from, held, amount);

        _balances[from] = held - amount;
        _balances[to] += amount;
        compliance.transferred(from, to, amount);
        emit Transfer(from, to, amount);
        emit ForcedTransfer(from, to, amount, msg.sender);
    }

    // --- bridge -----------------------------------------------------------

    /// @notice Moves shares into the bridge vault. Vault-only.
    /// @dev The vault must be exempt in the compliance module, or a fund at its
    /// holder ceiling could never bridge.
    function bridgeLock(address holder, uint256 amount, uint256 nonce) external whenNotPaused {
        if (msg.sender != bridgeVault) revert NotVault(msg.sender);
        if (amount == 0) revert ZeroAmount();
        if (frozen[holder]) revert AccountFrozen(holder);
        uint256 held = _balances[holder];
        if (held < amount) revert InsufficientBalance(holder, held, amount);

        _balances[holder] = held - amount;
        _balances[bridgeVault] += amount;
        compliance.transferred(holder, bridgeVault, amount);
        emit Transfer(holder, bridgeVault, amount);
        emit BridgeLocked(holder, amount, nonce);
    }

    /// @notice Returns shares from the bridge vault. Vault-only.
    function bridgeUnlock(address holder, uint256 amount, uint256 nonce) external whenNotPaused {
        if (msg.sender != bridgeVault) revert NotVault(msg.sender);
        if (amount == 0) revert ZeroAmount();
        _requireVerified(holder);
        if (frozen[holder]) revert AccountFrozen(holder);
        uint256 held = _balances[bridgeVault];
        if (held < amount) revert InsufficientBalance(bridgeVault, held, amount);
        _requireCompliant(bridgeVault, holder, amount);

        _balances[bridgeVault] = held - amount;
        _balances[holder] += amount;
        compliance.transferred(bridgeVault, holder, amount);
        emit Transfer(bridgeVault, holder, amount);
        emit BridgeUnlocked(holder, amount, nonce);
    }

    // --- internals --------------------------------------------------------

    function _transfer(address from, address to, uint256 amount) private whenNotPaused {
        if (to == address(0)) revert ZeroAddress();
        if (amount == 0) revert ZeroAmount();
        _beforeTokenTransfer(from, to, amount);

        uint256 held = _balances[from];
        if (held < amount) revert InsufficientBalance(from, held, amount);
        _balances[from] = held - amount;
        _balances[to] += amount;
        compliance.transferred(from, to, amount);
        emit Transfer(from, to, amount);
    }

    /// @dev The one place every ordinary movement is checked. The order is the
    /// order the Go simulator checks in, so a revert reason identifies the same
    /// rule on both sides.
    function _beforeTokenTransfer(address from, address to, uint256 amount) private view {
        if (frozen[from]) revert AccountFrozen(from);
        if (frozen[to]) revert AccountFrozen(to);
        _requireVerified(to);
        _requireCompliant(from, to, amount);
    }

    function _requireVerified(address account) private view {
        if (!identityRegistry.isVerified(account)) revert NotVerified(account);
    }

    function _requireCompliant(address from, address to, uint256 amount) private view {
        (bool ok, string memory reason) = compliance.canTransfer(from, to, amount);
        if (!ok) revert ComplianceRejected(reason);
    }
}
