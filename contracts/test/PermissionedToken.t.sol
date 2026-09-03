// SPDX-License-Identifier: Apache-2.0
pragma solidity 0.8.24;

import {Test} from "forge-std/Test.sol";

import {BridgeVault} from "../src/BridgeVault.sol";
import {Compliance} from "../src/Compliance.sol";
import {IdentityRegistry} from "../src/IdentityRegistry.sol";
import {PermissionedToken} from "../src/PermissionedToken.sol";
import {AccessControl, Pausable} from "../src/AccessControl.sol";

/// @dev Everything the unit, fuzz and invariant suites share.
abstract contract Fixture is Test {
    IdentityRegistry internal registry;
    Compliance internal compliance;
    PermissionedToken internal token;
    BridgeVault internal vault;

    address internal owner = makeAddr("owner");
    address internal alice = makeAddr("alice");
    address internal bob = makeAddr("bob");
    address internal carol = makeAddr("carol");
    address internal mallory = makeAddr("mallory"); // never registered

    uint16 internal constant US = 840;
    uint16 internal constant GB = 826;
    uint16 internal constant LU = 442;

    function _deploy() internal {
        vm.startPrank(owner);
        registry = new IdentityRegistry(owner);
        compliance = new Compliance(owner, registry);
        token = new PermissionedToken("Tokenised USD Money Market Fund Share", "TMMF", owner, registry, compliance);
        vault = new BridgeVault(owner, token);

        compliance.bindToken(address(token));
        compliance.setExempt(address(vault), true);
        token.setBridgeVault(address(vault));

        uint256[] memory topics = new uint256[](2);
        topics[0] = registry.CLAIM_KYC();
        topics[1] = registry.CLAIM_ACCREDITED();
        registry.registerIdentity(alice, US, owner, topics);
        registry.registerIdentity(bob, GB, owner, topics);
        registry.registerIdentity(carol, LU, owner, topics);
        vm.stopPrank();
    }

    function _mint(address to, uint256 amount) internal {
        vm.prank(owner);
        token.mint(to, amount, "primary issuance");
    }
}

contract PermissionedTokenTest is Fixture {
    function setUp() public {
        _deploy();
    }

    // --- the rule the whole design exists for -----------------------------

    function test_transferToUnregisteredAddressReverts() public {
        _mint(alice, 1_000);
        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(PermissionedToken.NotVerified.selector, mallory));
        token.transfer(mallory, 1);
        assertEq(token.balanceOf(alice), 1_000, "a reverted transfer moved a balance");
        assertEq(token.balanceOf(mallory), 0);
    }

    function test_mintToUnregisteredAddressReverts() public {
        vm.prank(owner);
        vm.expectRevert(abi.encodeWithSelector(PermissionedToken.NotVerified.selector, mallory));
        token.mint(mallory, 1, "");
    }

    function test_forcedTransferStillRefusesAnUnverifiedRecipient() public {
        _mint(alice, 100);
        vm.prank(owner);
        vm.expectRevert(abi.encodeWithSelector(PermissionedToken.NotVerified.selector, mallory));
        token.forcedTransfer(alice, mallory, 10);
    }

    // --- roles ------------------------------------------------------------

    function test_mintRequiresAgentRole() public {
        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(AccessControl.MissingRole.selector, token.AGENT_ROLE(), alice));
        token.mint(alice, 1, "");
    }

    function test_pauseRequiresOwnerRole() public {
        vm.prank(owner);
        token.grantRole(token.AGENT_ROLE(), alice);
        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(AccessControl.MissingRole.selector, token.OWNER_ROLE(), alice));
        token.pause();
    }

    function test_lastOwnerCannotBeRevoked() public {
        vm.prank(owner);
        vm.expectRevert(AccessControl.LastOwner.selector);
        token.revokeRole(token.OWNER_ROLE(), owner);
    }

    // --- lifecycle --------------------------------------------------------

    function test_mintAndRedeemMoveSupply() public {
        _mint(alice, 5_000);
        assertEq(token.totalSupply(), 5_000);
        vm.prank(owner);
        token.redeem(alice, 1_500, "redemption R-1");
        assertEq(token.totalSupply(), 3_500);
        assertEq(token.balanceOf(alice), 3_500);
    }

    function test_redeemWorksOnAFrozenAccount() public {
        _mint(alice, 100);
        vm.startPrank(owner);
        token.setAddressFrozen(alice, true);
        token.redeem(alice, 100, "wind-down");
        vm.stopPrank();
        assertEq(token.balanceOf(alice), 0);
    }

    function test_frozenAccountCannotSendOrReceive() public {
        _mint(alice, 100);
        _mint(bob, 100);
        vm.prank(owner);
        token.setAddressFrozen(alice, true);

        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(PermissionedToken.AccountFrozen.selector, alice));
        token.transfer(bob, 1);

        vm.prank(bob);
        vm.expectRevert(abi.encodeWithSelector(PermissionedToken.AccountFrozen.selector, alice));
        token.transfer(alice, 1);
    }

    function test_pauseBlocksTransfersButNotRecovery() public {
        _mint(alice, 100);
        vm.prank(owner);
        token.pause();

        vm.prank(alice);
        vm.expectRevert(Pausable.EnforcedPause.selector);
        token.transfer(bob, 1);

        // Recovery has to work while the token is paused, because a pause is
        // often exactly why recovery is being run.
        vm.prank(owner);
        token.forcedTransfer(alice, bob, 50);
        assertEq(token.balanceOf(bob), 50);
    }

    // --- compliance -------------------------------------------------------

    function test_maxPerInvestorIsEnforcedAtTheCeiling() public {
        vm.prank(owner);
        compliance.setLimits(0, 1_000);
        _mint(alice, 1_000); // exactly at the ceiling is allowed

        vm.prank(owner);
        vm.expectRevert(
            abi.encodeWithSelector(
                PermissionedToken.ComplianceRejected.selector, "Compliance: max balance per investor exceeded"
            )
        );
        token.mint(alice, 1, "");
    }

    function test_maxHoldersIsEnforced() public {
        vm.prank(owner);
        compliance.setLimits(2, 0);
        _mint(alice, 100);
        _mint(bob, 100);
        assertEq(compliance.holderCount(), 2);

        vm.prank(owner);
        vm.expectRevert(
            abi.encodeWithSelector(PermissionedToken.ComplianceRejected.selector, "Compliance: max holders exceeded")
        );
        token.mint(carol, 1, "");
    }

    function test_aTransferThatEmptiesTheSenderDoesNotConsumeHolderHeadroom() public {
        vm.prank(owner);
        compliance.setLimits(2, 0);
        _mint(alice, 100);
        _mint(bob, 100);
        // Alice sends everything to carol: two holders before, two after.
        vm.prank(alice);
        token.transfer(carol, 100);
        assertEq(compliance.holderCount(), 2);
        assertEq(token.balanceOf(alice), 0);
        assertEq(token.balanceOf(carol), 100);
    }

    function test_countryRestriction() public {
        _mint(alice, 100);
        vm.startPrank(owner);
        compliance.setCountryAllowed(US, true);
        compliance.setCountryAllowed(GB, true);
        vm.stopPrank();

        vm.prank(alice);
        vm.expectRevert(
            abi.encodeWithSelector(
                PermissionedToken.ComplianceRejected.selector, "Compliance: recipient country is not allowed"
            )
        );
        token.transfer(carol, 1); // carol is LU

        vm.prank(alice);
        token.transfer(bob, 1); // bob is GB
        assertEq(token.balanceOf(bob), 1);
    }

    function test_requiredClaimRestriction() public {
        _mint(alice, 100);
        uint256[] memory topics = new uint256[](1);
        topics[0] = 99; // a topic nobody holds
        vm.prank(owner);
        compliance.setRequiredClaims(topics);

        vm.prank(alice);
        vm.expectRevert(
            abi.encodeWithSelector(
                PermissionedToken.ComplianceRejected.selector, "Compliance: recipient is missing a required claim"
            )
        );
        token.transfer(bob, 1);
    }

    function test_deregisteredHolderKeepsItsBalanceAndCannotReceive() public {
        _mint(alice, 100);
        _mint(bob, 100);
        vm.prank(owner);
        registry.deleteIdentity(alice);

        assertEq(token.balanceOf(alice), 100, "deregistration confiscated a balance");
        vm.prank(bob);
        vm.expectRevert(abi.encodeWithSelector(PermissionedToken.NotVerified.selector, alice));
        token.transfer(alice, 1);

        // Recovery is the way the balance comes back, and it is a deliberate act.
        vm.prank(owner);
        token.forcedTransfer(alice, bob, 100);
        assertEq(token.balanceOf(bob), 200);
    }

    // --- bridge -----------------------------------------------------------

    function test_lockAndUnlockThroughTheVault() public {
        _mint(alice, 1_000);
        vm.prank(owner);
        vault.lock(1, alice, 400);

        assertEq(token.balanceOf(alice), 600);
        assertEq(token.balanceOf(address(vault)), 400);
        assertEq(vault.totalLocked(), 400);
        assertEq(token.totalSupply(), 1_000, "locking changed supply");

        vm.prank(owner);
        vault.unlock(1, alice, 400);
        assertEq(token.balanceOf(alice), 1_000);
        assertEq(vault.totalLocked(), 0);
    }

    function test_vaultRefusesAReusedNonce() public {
        _mint(alice, 1_000);
        vm.startPrank(owner);
        vault.lock(7, alice, 100);
        vm.expectRevert(abi.encodeWithSelector(BridgeVault.NonceUsed.selector, uint256(7)));
        vault.lock(7, alice, 100);

        vault.unlock(7, alice, 100);
        vm.expectRevert(abi.encodeWithSelector(BridgeVault.NonceUsed.selector, uint256(7)));
        vault.unlock(7, alice, 100);
        vm.stopPrank();
    }

    function test_onlyTheVaultMayLock() public {
        _mint(alice, 100);
        vm.prank(owner);
        vm.expectRevert(abi.encodeWithSelector(PermissionedToken.NotVault.selector, owner));
        token.bridgeLock(alice, 10, 1);
    }

    function test_vaultIsExemptFromTheHolderCeiling() public {
        vm.prank(owner);
        compliance.setLimits(1, 0);
        _mint(alice, 1_000);
        // With a one-holder ceiling and the vault not exempt, this would
        // revert and a capped fund could never bridge.
        vm.prank(owner);
        vault.lock(1, alice, 500);
        assertEq(token.balanceOf(address(vault)), 500);
    }

    // --- fuzz -------------------------------------------------------------

    /// @notice The whitelist holds for every recipient the fuzzer can reach:
    /// registered addresses receive, everything else reverts.
    function testFuzz_transferRespectsWhitelist(address to, uint256 amount) public {
        vm.assume(to != address(0));
        vm.assume(to != alice);
        amount = bound(amount, 1, 1_000_000);
        _mint(alice, 1_000_000);

        bool verified = registry.isVerified(to);
        vm.prank(alice);
        if (verified) {
            token.transfer(to, amount);
            assertEq(token.balanceOf(to), amount);
            assertEq(token.balanceOf(alice), 1_000_000 - amount);
        } else {
            vm.expectRevert(abi.encodeWithSelector(PermissionedToken.NotVerified.selector, to));
            token.transfer(to, amount);
            assertEq(token.balanceOf(to), 0);
            assertEq(token.balanceOf(alice), 1_000_000, "a reverted transfer moved a balance");
        }
    }

    /// @notice Supply is conserved by every transfer, whatever the amounts.
    function testFuzz_transferConservesSupply(uint256 seed, uint256 amount) public {
        uint256 opening = bound(seed, 1, type(uint128).max);
        amount = bound(amount, 0, opening);
        _mint(alice, opening);
        vm.prank(alice);
        if (amount == 0) {
            vm.expectRevert(PermissionedToken.ZeroAmount.selector);
            token.transfer(bob, amount);
        } else {
            token.transfer(bob, amount);
        }
        assertEq(token.balanceOf(alice) + token.balanceOf(bob), opening);
        assertEq(token.totalSupply(), opening);
    }

    /// @notice The per-investor ceiling holds for every ceiling and every
    /// amount, not just the ones a unit test happened to pick.
    function testFuzz_maxPerInvestorHolds(uint256 ceiling, uint256 amount) public {
        ceiling = bound(ceiling, 1, type(uint96).max);
        amount = bound(amount, 1, type(uint128).max);
        vm.prank(owner);
        compliance.setLimits(0, ceiling);

        vm.prank(owner);
        if (amount > ceiling) {
            vm.expectRevert(
                abi.encodeWithSelector(
                    PermissionedToken.ComplianceRejected.selector, "Compliance: max balance per investor exceeded"
                )
            );
            token.mint(alice, amount, "");
            assertEq(token.balanceOf(alice), 0);
        } else {
            token.mint(alice, amount, "");
            assertLe(token.balanceOf(alice), ceiling);
        }
    }
}

/// @dev The invariant handler. The fuzzer drives these entry points in random
/// order with random arguments; each one swallows an expected revert so that a
/// rejected action is a no-op rather than the end of the run, which is what
/// makes the invariants statements about reachable states rather than about one
/// happy path.
///
/// The handler deliberately never calls `deleteIdentity`. Deregistering a
/// holder that still has a balance is a legal state — the token permits it and
/// a unit test covers it — but it makes `invariant_noBalanceOutsideRegistry`
/// false by design, and an invariant that has to enumerate its own exceptions
/// stops being worth running.
contract Handler is Test {
    PermissionedToken public token;
    IdentityRegistry public registry;
    Compliance public compliance;
    BridgeVault public vault;
    address public owner;

    address[] public actors;

    /// @notice Ghost totals, accumulated independently of the token so that the
    /// invariants have something to check the token against.
    uint256 public ghostMinted;
    uint256 public ghostRedeemed;

    uint256 private nonceCounter;

    constructor(
        PermissionedToken token_,
        IdentityRegistry registry_,
        Compliance compliance_,
        BridgeVault vault_,
        address owner_,
        address[] memory actors_
    ) {
        token = token_;
        registry = registry_;
        compliance = compliance_;
        vault = vault_;
        owner = owner_;
        actors = actors_;
    }

    function actorCount() external view returns (uint256) {
        return actors.length;
    }

    function actorAt(uint256 i) external view returns (address) {
        return actors[i];
    }

    function _actor(uint256 seed) private view returns (address) {
        return actors[bound(seed, 0, actors.length - 1)];
    }

    function mint(uint256 actorSeed, uint256 amount) external {
        address to = _actor(actorSeed);
        amount = bound(amount, 1, 1e12);
        vm.prank(owner);
        try token.mint(to, amount, "fuzz") {
            ghostMinted += amount;
        } catch {}
    }

    function redeem(uint256 actorSeed, uint256 amount) external {
        address from = _actor(actorSeed);
        uint256 held = token.balanceOf(from);
        if (held == 0) return;
        amount = bound(amount, 1, held);
        vm.prank(owner);
        try token.redeem(from, amount, "fuzz") {
            ghostRedeemed += amount;
        } catch {}
    }

    function transfer(uint256 fromSeed, uint256 toSeed, uint256 amount) external {
        address from = _actor(fromSeed);
        address to = _actor(toSeed);
        uint256 held = token.balanceOf(from);
        if (held == 0 || from == to) return;
        amount = bound(amount, 1, held);
        vm.prank(from);
        try token.transfer(to, amount) {} catch {}
    }

    function forcedTransfer(uint256 fromSeed, uint256 toSeed, uint256 amount) external {
        address from = _actor(fromSeed);
        address to = _actor(toSeed);
        uint256 held = token.balanceOf(from);
        if (held == 0 || from == to) return;
        amount = bound(amount, 1, held);
        vm.prank(owner);
        try token.forcedTransfer(from, to, amount) {} catch {}
    }

    function lock(uint256 actorSeed, uint256 amount) external {
        address holder = _actor(actorSeed);
        uint256 held = token.balanceOf(holder);
        if (held == 0) return;
        amount = bound(amount, 1, held);
        nonceCounter += 1;
        vm.prank(owner);
        try vault.lock(nonceCounter, holder, amount) {} catch {}
    }

    function unlock(uint256 actorSeed, uint256 amount) external {
        address holder = _actor(actorSeed);
        uint256 locked = vault.totalLocked();
        if (locked == 0) return;
        amount = bound(amount, 1, locked);
        nonceCounter += 1;
        vm.prank(owner);
        try vault.unlock(nonceCounter, holder, amount) {} catch {}
    }

    function freeze(uint256 actorSeed, bool value) external {
        vm.prank(owner);
        try token.setAddressFrozen(_actor(actorSeed), value) {} catch {}
    }

    function setPaused(bool value) external {
        vm.prank(owner);
        if (value) {
            try token.pause() {} catch {}
        } else {
            try token.unpause() {} catch {}
        }
    }

    function setLimits(uint256 holders, uint256 perInvestor) external {
        holders = bound(holders, 0, actors.length);
        perInvestor = bound(perInvestor, 0, 1e18);
        vm.prank(owner);
        try compliance.setLimits(holders, perInvestor) {} catch {}
    }
}

contract PermissionedTokenInvariantTest is Fixture {
    Handler internal handler;
    address[] internal actorList;

    function setUp() public {
        _deploy();

        actorList.push(alice);
        actorList.push(bob);
        actorList.push(carol);
        handler = new Handler(token, registry, compliance, vault, owner, actorList);

        // The handler acts as the owner through vm.prank, so it needs no roles
        // of its own; what it does need is the vault's agent role, which the
        // owner already holds.
        //
        // Only the action selectors are fuzzed. Left to itself the fuzzer would
        // spend most of its calls on the handler's getters, which change
        // nothing and therefore test nothing.
        bytes4[] memory selectors = new bytes4[](9);
        selectors[0] = Handler.mint.selector;
        selectors[1] = Handler.redeem.selector;
        selectors[2] = Handler.transfer.selector;
        selectors[3] = Handler.forcedTransfer.selector;
        selectors[4] = Handler.lock.selector;
        selectors[5] = Handler.unlock.selector;
        selectors[6] = Handler.freeze.selector;
        selectors[7] = Handler.setPaused.selector;
        selectors[8] = Handler.setLimits.selector;
        targetSelector(FuzzSelector({addr: address(handler), selectors: selectors}));
        targetContract(address(handler));
    }

    /// @notice Supply is the sum of the balances that exist. There is no
    /// accounting anywhere in this token that can put a unit somewhere the sum
    /// does not reach.
    function invariant_totalSupplyEqualsSumOfBalances() public view {
        uint256 sum = token.balanceOf(address(vault));
        for (uint256 i = 0; i < actorList.length; ++i) {
            sum += token.balanceOf(actorList[i]);
        }
        assertEq(sum, token.totalSupply(), "supply is not the sum of balances");
    }

    /// @notice Every address holding the token is either in the identity
    /// registry or is the bridge vault. The vault is the one exception and it
    /// is the token's own custody account, named at deployment.
    function invariant_noBalanceOutsideRegistry() public view {
        for (uint256 i = 0; i < actorList.length; ++i) {
            address a = actorList[i];
            if (token.balanceOf(a) > 0) {
                assertTrue(registry.isVerified(a), "an unregistered address holds the token");
            }
        }
    }

    /// @notice The vault's own accounting equals its balance. If these ever
    /// diverge, the cross-network supply control is reconciling against a
    /// number that is not the collateral.
    function invariant_lockedEqualsVaultBalance() public view {
        assertEq(vault.totalLocked(), token.balanceOf(address(vault)), "vault accounting has drifted from its balance");
    }

    /// @notice Supply equals what was issued less what was redeemed. This is
    /// the on-chain half of the cross-network identity the Go supply control
    /// proves across both networks.
    function invariant_supplyEqualsIssuedLessRedeemed() public view {
        assertEq(token.totalSupply(), handler.ghostMinted() - handler.ghostRedeemed(), "supply drifted from issuance");
    }

    /// @notice The compliance module's holder count is the number of counted
    /// non-vault addresses with a balance. A holder ceiling enforced against a
    /// stale count is not enforced at all.
    function invariant_holderCountIsAccurate() public view {
        uint256 n;
        for (uint256 i = 0; i < actorList.length; ++i) {
            if (token.balanceOf(actorList[i]) > 0) n += 1;
        }
        assertEq(compliance.holderCount(), n, "holder count has drifted");
    }
}
