// SPDX-License-Identifier: Apache-2.0
pragma solidity 0.8.24;

import {AccessControl} from "./AccessControl.sol";
import {ICompliance, IIdentityRegistry} from "./Interfaces.sol";

/// @title Compliance
/// @notice The transferable rules of the offering: how many holders it may
/// have, how much any one of them may hold, which jurisdictions may receive and
/// which claims a recipient must carry.
///
/// @dev These are not arbitrary knobs. A holder ceiling is what keeps a private
/// placement private; a per-investor ceiling is a concentration rule the fund's
/// own prospectus imposes; a country list is the set of jurisdictions the
/// offering is registered in. Each of them turns a legal constraint into
/// something a transfer cannot get around, which is the entire argument for
/// putting the fund on a permissioned token rather than an ERC-20.
///
/// The module is bound to exactly one token. The hooks that maintain the holder
/// count are token-only, because a holder count anybody can move is not a
/// holder count.
contract Compliance is AccessControl, ICompliance {
    IIdentityRegistry public immutable registry;

    address public token;

    /// @notice Maximum number of addresses with a non-zero balance. Zero is
    /// unlimited.
    uint256 public maxHolders;
    /// @notice Maximum balance any one address may hold. Zero is unlimited.
    uint256 public maxPerInvestor;
    /// @notice Current number of addresses with a non-zero balance.
    uint256 public holderCount;

    /// @notice Addresses exempt from the ceilings: the bridge vault, and
    /// nothing else without an explicit decision. An exemption is a hole in a
    /// legal constraint, so each one is an event on the chain.
    mapping(address account => bool) public exempt;

    mapping(uint16 country => bool) private _allowedCountry;
    bool private _countryListActive;
    uint256[] private _requiredClaims;

    event TokenBound(address indexed token);
    event LimitsUpdated(uint256 maxHolders, uint256 maxPerInvestor);
    event CountryAllowed(uint16 indexed country, bool allowed);
    event RequiredClaimsUpdated(uint256[] topics);
    event ExemptionUpdated(address indexed account, bool exempt);
    /// @notice The holder register changed. Off-chain cap-table readers can
    /// rebuild the count from these events alone instead of replaying balances.
    event HolderJoined(address indexed account, uint256 holderCount);
    event HolderLeft(address indexed account, uint256 holderCount);

    error AlreadyBound();
    error NotToken(address caller);
    error ZeroAddress();

    modifier onlyToken() {
        if (msg.sender != token) revert NotToken(msg.sender);
        _;
    }

    constructor(address owner_, IIdentityRegistry registry_) {
        if (owner_ == address(0) || address(registry_) == address(0)) revert ZeroAddress();
        registry = registry_;
        _grantRole(OWNER_ROLE, owner_);
        _grantRole(COMPLIANCE_ROLE, owner_);
    }

    /// @notice Binds the module to its token. One-shot: rebinding a live
    /// compliance module to a second token would corrupt the holder count of
    /// both.
    function bindToken(address token_) external onlyRole(OWNER_ROLE) {
        if (token != address(0)) revert AlreadyBound();
        if (token_ == address(0)) revert ZeroAddress();
        token = token_;
        emit TokenBound(token_);
    }

    /// @notice Sets the holder and per-investor ceilings.
    /// @dev Lowering a ceiling is not retroactive. An address already above a
    /// newly lowered limit keeps its balance and simply cannot receive more;
    /// forcing it down would mean moving somebody's property because a
    /// parameter changed.
    function setLimits(uint256 maxHolders_, uint256 maxPerInvestor_) external onlyRole(COMPLIANCE_ROLE) {
        maxHolders = maxHolders_;
        maxPerInvestor = maxPerInvestor_;
        emit LimitsUpdated(maxHolders_, maxPerInvestor_);
    }

    /// @notice Adds or removes a country from the allow list. The first call
    /// activates the list; until then every registered country may receive.
    function setCountryAllowed(uint16 country_, bool allowed) external onlyRole(COMPLIANCE_ROLE) {
        _allowedCountry[country_] = allowed;
        _countryListActive = true;
        emit CountryAllowed(country_, allowed);
    }

    /// @notice Replaces the claim topics a recipient must carry.
    function setRequiredClaims(uint256[] calldata topics) external onlyRole(COMPLIANCE_ROLE) {
        _requiredClaims = topics;
        emit RequiredClaimsUpdated(topics);
    }

    /// @notice Exempts an address from the ceilings.
    function setExempt(address account, bool exempt_) external onlyRole(COMPLIANCE_ROLE) {
        exempt[account] = exempt_;
        emit ExemptionUpdated(account, exempt_);
    }

    /// @notice The claim topics a recipient must carry.
    function requiredClaims() external view returns (uint256[] memory) {
        return _requiredClaims;
    }

    /// @notice Reports whether a country may receive.
    function countryAllowed(uint16 country_) public view returns (bool) {
        return !_countryListActive || _allowedCountry[country_];
    }

    /// @inheritdoc ICompliance
    /// @dev `from` is the zero address on a mint and `to` is the zero address on
    /// a burn, matching the ERC-20 Transfer event convention, so one function
    /// covers all three movements and there is one place where the rules live.
    function canTransfer(address from, address to, uint256 amount) external view returns (bool, string memory) {
        if (to == address(0)) return (true, ""); // a burn removes a holder; nothing to check
        if (exempt[to]) return (true, "");

        if (!countryAllowed(registry.country(to))) {
            return (false, "Compliance: recipient country is not allowed");
        }
        uint256 n = _requiredClaims.length;
        for (uint256 i = 0; i < n; ++i) {
            // `registry` is immutable and set by the deployer, and the topic
            // list is bounded by COMPLIANCE_ROLE, so this is a fixed number of
            // view calls into a trusted contract, not attacker-controlled fan-out.
            // forge-lint: disable-next-line(calls-loop)
            if (!registry.hasClaim(to, _requiredClaims[i])) {
                return (false, "Compliance: recipient is missing a required claim");
            }
        }
        uint256 balanceTo = IERC20Balance(token).balanceOf(to);
        if (maxPerInvestor != 0 && balanceTo + amount > maxPerInvestor) {
            return (false, "Compliance: max balance per investor exceeded");
        }
        if (maxHolders != 0 && balanceTo == 0) {
            // A transfer that empties the sender swaps one holder for another
            // rather than adding one, so it does not consume headroom.
            // The balance comes from the one token bound in bindToken, and every
            // credit to an address passes the whitelist and this module, so it
            // cannot be inflated by an unsolicited transfer. Exact equality is
            // the rule being expressed: the sender is sending everything.
            // forge-lint: disable-start(incorrect-strict-equality)
            bool sendersSeatFreed =
                from != address(0) && !exempt[from] && IERC20Balance(token).balanceOf(from) == amount;
            // forge-lint: disable-end(incorrect-strict-equality)
            uint256 projected = sendersSeatFreed ? holderCount : holderCount + 1;
            if (projected > maxHolders) {
                return (false, "Compliance: max holders exceeded");
            }
        }
        return (true, "");
    }

    /// @inheritdoc ICompliance
    function transferred(address from, address to, uint256 amount) external onlyToken {
        _left(from, amount);
        _joined(to);
    }

    /// @inheritdoc ICompliance
    function created(address to, uint256) external onlyToken {
        _joined(to);
    }

    /// @inheritdoc ICompliance
    function destroyed(address from, uint256 amount) external onlyToken {
        _left(from, amount);
    }

    /// @dev Called after the balance has already moved, so a sender whose
    /// balance is now zero has left the register.
    function _left(address from, uint256) private {
        if (from == address(0) || exempt[from]) return;
        // Same token-sourced balance as in canTransfer; zero means the sender
        // has left the register.
        // forge-lint: disable-next-line(incorrect-strict-equality)
        if (IERC20Balance(token).balanceOf(from) == 0 && _counted[from]) {
            _counted[from] = false;
            holderCount -= 1;
            emit HolderLeft(from, holderCount);
        }
    }

    function _joined(address to) private {
        if (to == address(0) || exempt[to]) return;
        if (IERC20Balance(token).balanceOf(to) > 0) {
            // The hook runs once per movement, so a holder that was already
            // counted must not be counted twice. The token calls _joined only
            // after crediting, and the count is reconciled by _left, so the
            // pair is what keeps this exact.
            if (!_counted[to]) {
                _counted[to] = true;
                holderCount += 1;
                emit HolderJoined(to, holderCount);
            }
        } else {
            _counted[to] = false;
        }
    }

    mapping(address account => bool) private _counted;

    /// @notice Reports whether an address is currently counted as a holder.
    function counted(address account) external view returns (bool) {
        return _counted[account];
    }
}

/// @dev The single ERC-20 method the compliance module needs. Importing the
/// whole token interface here would make the module depend on the concrete
/// token, and the point of a module is that it does not.
interface IERC20Balance {
    function balanceOf(address account) external view returns (uint256);
}
