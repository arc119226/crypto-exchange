// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {ScriptBase} from "./Vm.sol";
import {MockUSDC} from "../src/MockUSDC.sol";

/// @title Deploy
/// @notice Prepares a test chain for the exchange:
///         1. deploys MockUSDC (or reuses one), 2. tops the hot wallet up,
///         3. writes the fixtures file read by `exchange seed`.
/// @dev The defaults are anvil's and reproduce the previous behaviour exactly:
///      deploy from nonce 0, fund the hot wallet with 100 ETH and 1,000,000
///      USDC, write /artifacts/addresses.json.
///
///      Four environment variables make it usable on a public testnet, where
///      none of those defaults hold. There the deployer's ether comes from a
///      faucet in amounts measured in hundredths, so the hot-wallet top-up has
///      to be turned off (HOT_WALLET_ETH_TARGET=0) and done by pointing a
///      faucet at the address instead; and a key that has ever sent a
///      transaction cannot satisfy the nonce-0 idempotency check, so an
///      already-deployed contract is named outright (USDC_ADDRESS).
///
///      Environment: CONTRACT_DEPLOYER_KEY, HOT_WALLET_ADDRESS, and optionally
///      USDC_ADDRESS, HOT_WALLET_ETH_TARGET, HOT_WALLET_USDC_TARGET,
///      ADDRESSES_OUT.
contract Deploy is ScriptBase {
    uint256 internal constant DEFAULT_HOT_WALLET_ETH = 100 ether;
    uint256 internal constant DEFAULT_HOT_WALLET_USDC = 1_000_000 * 1e6;
    string internal constant DEFAULT_ARTIFACT = "/artifacts/addresses.json";

    function run() external {
        uint256 deployerKey = vm.envUint("CONTRACT_DEPLOYER_KEY");
        address deployer = vm.addr(deployerKey);
        address hotWallet = vm.envAddress("HOT_WALLET_ADDRESS");
        require(hotWallet != address(0), "HOT_WALLET_ADDRESS is unset: run `make gen-dev-secrets`");
        require(hotWallet != deployer, "HOT_WALLET_ADDRESS must not be the deployer");
        vm.label(deployer, "deployer");
        vm.label(hotWallet, "hot-wallet");

        uint256 ethTarget = vm.envOr("HOT_WALLET_ETH_TARGET", DEFAULT_HOT_WALLET_ETH);
        uint256 usdcTarget = vm.envOr("HOT_WALLET_USDC_TARGET", DEFAULT_HOT_WALLET_USDC);
        string memory artifact = vm.envOr("ADDRESSES_OUT", DEFAULT_ARTIFACT);
        MockUSDC usdc = MockUSDC(vm.envOr("USDC_ADDRESS", address(0)));

        vm.startBroadcast(deployerKey);
        if (address(usdc) == address(0)) {
            usdc = MockUSDC(findOrDeploy(deployer));
        } else {
            require(address(usdc).code.length > 0, "USDC_ADDRESS has no code on this chain");
        }
        if (ethTarget > 0 && hotWallet.balance < ethTarget) {
            uint256 shortfall = ethTarget - hotWallet.balance;
            // Saying this plainly matters more than it looks: the failure a
            // testnet operator hits is the whole script reverting on a top-up
            // they never asked for, with nothing naming the variable that
            // turns it off.
            require(
                deployer.balance >= shortfall,
                "deployer cannot cover HOT_WALLET_ETH_TARGET: fund the hot wallet by faucet, then set it to 0"
            );
            (bool ok,) = hotWallet.call{value: shortfall}("");
            require(ok, "funding the hot wallet failed");
        }
        uint256 usdcBalance = usdc.balanceOf(hotWallet);
        if (usdcTarget > usdcBalance) {
            usdc.mint(hotWallet, usdcTarget - usdcBalance);
        }
        vm.stopBroadcast();

        // Keys mirror internal/registry.Fixtures (camelCase).
        string memory obj = "addresses";
        vm.serializeUint(obj, "chainId", block.chainid);
        vm.serializeAddress(obj, "deployer", deployer);
        vm.serializeAddress(obj, "hotWallet", hotWallet);
        vm.serializeAddress(obj, "usdc", address(usdc));
        string memory json = vm.serializeUint(obj, "deployedAtBlock", block.number);
        vm.writeJson(json, artifact);
    }

    /// @dev Idempotency without being told the address: the CREATE address for
    ///      nonce 0 is recomputed, and reused when code is already there. That
    ///      covers a stateful anvil restarting (`--state`) and a testnet
    ///      deployer whose first transaction was this deployment.
    ///
    ///      A deployer that had a nonce before its MockUSDC is the one case
    ///      this cannot resolve: deploying would land at an address no
    ///      previous run recorded, so it stops and asks to be told.
    function findOrDeploy(address deployer) internal returns (address) {
        address predicted = computeCreateAddressNonce0(deployer);
        if (predicted.code.length > 0) {
            return predicted;
        }
        require(
            vm.getNonce(deployer) == 0,
            "deployer nonce is not 0 but MockUSDC is missing: set USDC_ADDRESS, or `make reset` for a clean chain"
        );
        MockUSDC deployed = new MockUSDC();
        require(address(deployed) == predicted, "MockUSDC landed at an unexpected address");
        return address(deployed);
    }

    /// @dev CREATE address for nonce 0: keccak256(rlp([deployer, 0]))[12:].
    ///      rlp = 0xd6 (list, 22 bytes) 0x94 (20-byte string) <deployer> 0x80 (empty = 0).
    function computeCreateAddressNonce0(address deployer) internal pure returns (address) {
        return address(uint160(uint256(keccak256(abi.encodePacked(bytes1(0xd6), bytes1(0x94), deployer, bytes1(0x80))))));
    }
}
