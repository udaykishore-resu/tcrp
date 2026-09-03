// SPDX-License-Identifier: Apache-2.0
pragma solidity 0.8.24;

/// @title IERC20
/// @notice The subset of ERC-20 a permissioned token still has to expose.
/// @dev An ERC-3643 token is ERC-20 compatible on purpose: custodians, wallets
/// and indexers already speak it. What differs is that every one of these calls
/// can revert on a rule rather than only on a balance, which is a contract an
/// ERC-20 integrator does not expect and has to be told about.
interface IERC20 {
    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);

    function totalSupply() external view returns (uint256);
    function balanceOf(address account) external view returns (uint256);
    function allowance(address owner, address spender) external view returns (uint256);
    function transfer(address to, uint256 amount) external returns (bool);
    function approve(address spender, uint256 amount) external returns (bool);
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
}

/// @title IIdentityRegistry
/// @notice Who is allowed to hold the token.
/// @dev The registry stores attestations about an address, not a person. That
/// is the whole reason KYC can live on-chain at all: the claim topics say an
/// accredited issuer has verified something, and the identifying data stays off
/// the chain with the issuer.
interface IIdentityRegistry {
    /// @notice Reports whether an address may receive the token.
    function isVerified(address account) external view returns (bool);

    /// @notice The ISO-3166 numeric country of the address's identity.
    function country(address account) external view returns (uint16);

    /// @notice Reports whether an address carries a claim topic.
    function hasClaim(address account, uint256 topic) external view returns (bool);
}

/// @title ICompliance
/// @notice The transferable rules of the offering.
/// @dev The read and the write are separate calls. `canTransfer` is a view so
/// that a wallet can predict a revert before it costs anybody gas; the hooks
/// are how the module keeps the state — holder counts above all — that its own
/// view depends on. A module that tried to do both in one call could not be
/// asked "would this work" without doing it.
interface ICompliance {
    /// @notice Reports whether a transfer is permitted, and why not if it is not.
    function canTransfer(address from, address to, uint256 amount) external view returns (bool ok, string memory reason);

    /// @notice Records a completed transfer. Token-only.
    function transferred(address from, address to, uint256 amount) external;

    /// @notice Records a completed mint. Token-only.
    function created(address to, uint256 amount) external;

    /// @notice Records a completed burn. Token-only.
    function destroyed(address from, uint256 amount) external;
}
