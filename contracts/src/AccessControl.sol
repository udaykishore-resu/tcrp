// SPDX-License-Identifier: Apache-2.0
pragma solidity 0.8.24;

/// @title AccessControl
/// @notice Minimal role-based access control.
/// @dev This is deliberately a local implementation rather than an import of
/// OpenZeppelin's. The Go platform this contract belongs to depends on nothing
/// outside its standard library, and the same discipline is applied here so
/// that the source in this directory can be read without chasing a dependency
/// tree. A production deployment should use OpenZeppelin's audited
/// `AccessControl` and `Pausable`; the surface used here is a strict subset of
/// theirs, so the swap is an import change.
abstract contract AccessControl {
    /// @notice Administers roles, pauses the token and repoints its modules.
    bytes32 public constant OWNER_ROLE = keccak256("OWNER_ROLE");
    /// @notice Runs issuance, redemption, recovery, freezing and the bridge.
    bytes32 public constant AGENT_ROLE = keccak256("AGENT_ROLE");
    /// @notice Writes the identity registry and the compliance parameters.
    bytes32 public constant COMPLIANCE_ROLE = keccak256("COMPLIANCE_ROLE");

    mapping(bytes32 role => mapping(address account => bool)) private _roles;

    event RoleGranted(bytes32 indexed role, address indexed account, address indexed sender);
    event RoleRevoked(bytes32 indexed role, address indexed account, address indexed sender);

    error MissingRole(bytes32 role, address account);

    modifier onlyRole(bytes32 role) {
        if (!_roles[role][msg.sender]) revert MissingRole(role, msg.sender);
        _;
    }

    /// @notice Reports whether an account holds a role.
    function hasRole(bytes32 role, address account) public view returns (bool) {
        return _roles[role][account];
    }

    /// @notice Grants a role. Owner-only.
    function grantRole(bytes32 role, address account) external onlyRole(OWNER_ROLE) {
        _grantRole(role, account);
    }

    /// @notice Revokes a role. Owner-only.
    /// @dev Revoking the last owner is refused. A contract with no owner cannot
    /// be paused, cannot repoint its compliance module and cannot appoint a new
    /// agent; recovering from that requires a redeployment and a migration of
    /// every holder.
    function revokeRole(bytes32 role, address account) external onlyRole(OWNER_ROLE) {
        if (role == OWNER_ROLE && _ownerCount == 1) revert LastOwner();
        if (_roles[role][account]) {
            _roles[role][account] = false;
            if (role == OWNER_ROLE) _ownerCount -= 1;
            emit RoleRevoked(role, account, msg.sender);
        }
    }

    error LastOwner();

    uint256 private _ownerCount;

    function _grantRole(bytes32 role, address account) internal {
        if (!_roles[role][account]) {
            _roles[role][account] = true;
            if (role == OWNER_ROLE) _ownerCount += 1;
            emit RoleGranted(role, account, msg.sender);
        }
    }
}

/// @title Pausable
/// @notice Emergency halt for every ordinary token movement.
abstract contract Pausable {
    bool private _paused;

    event Paused(address account);
    event Unpaused(address account);

    error EnforcedPause();
    error ExpectedPause();

    modifier whenNotPaused() {
        if (_paused) revert EnforcedPause();
        _;
    }

    /// @notice Reports whether the contract is halted.
    function paused() public view returns (bool) {
        return _paused;
    }

    function _pause() internal {
        if (_paused) revert ExpectedPause();
        _paused = true;
        emit Paused(msg.sender);
    }

    function _unpause() internal {
        if (!_paused) revert ExpectedPause();
        _paused = false;
        emit Unpaused(msg.sender);
    }
}
