// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {ScriptBase} from "./Vm.sol";
import {MockUSDC} from "../src/MockUSDC.sol";

/// @title Deploy
/// @notice Prepares the local chain for the exchange:
///         1. deploys MockUSDC from anvil account #0 (nonce 0) unless it already exists,
///         2. tops the hot wallet up to 100 ETH and 1,000,000 USDC,
///         3. writes /artifacts/addresses.json, the fixtures file read by `exchange seed`.
/// @dev Idempotent across restarts of a stateful anvil (`--state`): the CREATE
///      address for nonce 0 is recomputed and skipped when code is present.
///      Environment: ANVIL_DEPLOYER_KEY (private key), HOT_WALLET_ADDRESS.
contract Deploy is ScriptBase {
    uint256 internal constant HOT_WALLET_ETH = 100 ether;
    uint256 internal constant HOT_WALLET_USDC = 1_000_000 * 1e6;
    string internal constant ARTIFACT = "/artifacts/addresses.json";

    function run() external {
        uint256 deployerKey = vm.envUint("ANVIL_DEPLOYER_KEY");
        address deployer = vm.addr(deployerKey);
        address hotWallet = vm.envAddress("HOT_WALLET_ADDRESS");
        require(hotWallet != address(0), "HOT_WALLET_ADDRESS is unset: run `make gen-dev-secrets`");
        require(hotWallet != deployer, "HOT_WALLET_ADDRESS must not be the deployer");
        vm.label(deployer, "deployer");
        vm.label(hotWallet, "hot-wallet");

        address predicted = computeCreateAddressNonce0(deployer);
        MockUSDC usdc;

        vm.startBroadcast(deployerKey);
        if (predicted.code.length == 0) {
            require(
                vm.getNonce(deployer) == 0,
                "deployer nonce is not 0 but MockUSDC is missing: run `make reset` for a clean chain"
            );
            usdc = new MockUSDC();
            require(address(usdc) == predicted, "MockUSDC landed at an unexpected address");
        } else {
            usdc = MockUSDC(predicted);
        }
        if (hotWallet.balance < HOT_WALLET_ETH) {
            (bool ok,) = hotWallet.call{value: HOT_WALLET_ETH - hotWallet.balance}("");
            require(ok, "funding the hot wallet failed");
        }
        uint256 usdcBalance = usdc.balanceOf(hotWallet);
        if (usdcBalance < HOT_WALLET_USDC) {
            usdc.mint(hotWallet, HOT_WALLET_USDC - usdcBalance);
        }
        vm.stopBroadcast();

        // Keys mirror internal/registry.Fixtures (camelCase).
        string memory obj = "addresses";
        vm.serializeUint(obj, "chainId", block.chainid);
        vm.serializeAddress(obj, "deployer", deployer);
        vm.serializeAddress(obj, "hotWallet", hotWallet);
        vm.serializeAddress(obj, "usdc", address(usdc));
        string memory json = vm.serializeUint(obj, "deployedAtBlock", block.number);
        vm.writeJson(json, ARTIFACT);
    }

    /// @dev CREATE address for nonce 0: keccak256(rlp([deployer, 0]))[12:].
    ///      rlp = 0xd6 (list, 22 bytes) 0x94 (20-byte string) <deployer> 0x80 (empty = 0).
    function computeCreateAddressNonce0(address deployer) internal pure returns (address) {
        return address(uint160(uint256(keccak256(abi.encodePacked(bytes1(0xd6), bytes1(0x94), deployer, bytes1(0x80))))));
    }
}
