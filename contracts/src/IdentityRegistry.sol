// SPDX-License-Identifier: Apache-2.0
pragma solidity 0.8.24;

import {AccessControl} from "./AccessControl.sol";
import {IIdentityRegistry} from "./Interfaces.sol";

/// @title IdentityRegistry
/// @notice The whitelist an ERC-3643 token consults before every credit.
///
/// @dev An identity is an address, a country and a set of claim topics an
/// accredited issuer has attested. Nothing identifying is stored: the registry
/// records that a claim exists, and the data behind it stays with the claim
/// issuer. Putting a name on chain would be a data-protection incident with a
/// permanent audit trail.
///
/// Deregistration does not move a balance. A holder whose claim is revoked
/// keeps what it holds and can no longer receive more, which is what a revoked
/// claim actually means; getting the balance back is what the token's
/// `forcedTransfer` is for, and that is a deliberate, agent-authorised act
/// rather than a side effect of an administrative edit.
contract IdentityRegistry is AccessControl, IIdentityRegistry {
    /// @notice Claim topic: the holder has been identified.
    uint256 public constant CLAIM_KYC = 1;
    /// @notice Claim topic: the holder may hold this class of security.
    uint256 public constant CLAIM_ACCREDITED = 2;

    struct Identity {
        bool registered;
        uint16 country;
        address claimIssuer;
    }

    mapping(address account => Identity) private _identities;
    mapping(address account => mapping(uint256 topic => bool)) private _claims;

    event IdentityRegistered(address indexed account, uint16 country, address indexed claimIssuer);
    event IdentityRemoved(address indexed account);
    event CountryUpdated(address indexed account, uint16 country);
    event ClaimSet(address indexed account, uint256 indexed topic, bool held);

    error ZeroAddress();
    error NotRegistered(address account);

    constructor(address owner_) {
        if (owner_ == address(0)) revert ZeroAddress();
        _grantRole(OWNER_ROLE, owner_);
        _grantRole(COMPLIANCE_ROLE, owner_);
    }

    /// @notice Registers an identity with its country and claim topics.
    function registerIdentity(address account, uint16 country_, address claimIssuer_, uint256[] calldata topics)
        external
        onlyRole(COMPLIANCE_ROLE)
    {
        if (account == address(0)) revert ZeroAddress();
        _identities[account] = Identity({registered: true, country: country_, claimIssuer: claimIssuer_});
        for (uint256 i = 0; i < topics.length; ++i) {
            _claims[account][topics[i]] = true;
            emit ClaimSet(account, topics[i], true);
        }
        emit IdentityRegistered(account, country_, claimIssuer_);
    }

    /// @notice Removes an identity. The address keeps any balance it holds.
    function deleteIdentity(address account) external onlyRole(COMPLIANCE_ROLE) {
        if (!_identities[account].registered) revert NotRegistered(account);
        delete _identities[account];
        emit IdentityRemoved(account);
    }

    /// @notice Updates the country on a registered identity.
    function updateCountry(address account, uint16 country_) external onlyRole(COMPLIANCE_ROLE) {
        if (!_identities[account].registered) revert NotRegistered(account);
        _identities[account].country = country_;
        emit CountryUpdated(account, country_);
    }

    /// @notice Adds or revokes a claim topic on a registered identity.
    function setClaim(address account, uint256 topic, bool held) external onlyRole(COMPLIANCE_ROLE) {
        if (!_identities[account].registered) revert NotRegistered(account);
        _claims[account][topic] = held;
        emit ClaimSet(account, topic, held);
    }

    /// @inheritdoc IIdentityRegistry
    function isVerified(address account) external view returns (bool) {
        return _identities[account].registered;
    }

    /// @inheritdoc IIdentityRegistry
    function country(address account) external view returns (uint16) {
        return _identities[account].country;
    }

    /// @inheritdoc IIdentityRegistry
    function hasClaim(address account, uint256 topic) external view returns (bool) {
        return _claims[account][topic];
    }

    /// @notice The claim issuer that attested an identity, for audit.
    function claimIssuer(address account) external view returns (address) {
        return _identities[account].claimIssuer;
    }
}
