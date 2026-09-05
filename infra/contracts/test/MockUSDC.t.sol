// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {TestBase} from "../script/Vm.sol";
import {MockUSDC} from "../src/MockUSDC.sol";

contract MockUSDCTest is TestBase {
    MockUSDC internal usdc;
    address internal alice = address(0xA11CE);
    address internal bob = address(0xB0B);

    function setUp() public {
        usdc = new MockUSDC();
    }

    function test_Metadata() public view {
        require(usdc.decimals() == 6, "decimals");
        require(keccak256(bytes(usdc.symbol())) == keccak256("USDC"), "symbol");
        require(usdc.totalSupply() == 0, "supply");
    }

    function test_MintAndTransfer() public {
        usdc.mint(alice, 1_000e6);
        require(usdc.totalSupply() == 1_000e6, "supply after mint");
        require(usdc.balanceOf(alice) == 1_000e6, "alice after mint");

        vm.prank(alice);
        require(usdc.transfer(bob, 250e6), "transfer");
        require(usdc.balanceOf(alice) == 750e6, "alice after transfer");
        require(usdc.balanceOf(bob) == 250e6, "bob after transfer");
    }

    function test_TransferFromRespectsAllowance() public {
        usdc.mint(alice, 100e6);
        vm.prank(alice);
        usdc.approve(bob, 40e6);

        vm.prank(bob);
        usdc.transferFrom(alice, bob, 30e6);
        require(usdc.allowance(alice, bob) == 10e6, "allowance decremented");

        vm.prank(bob);
        vm.expectRevert(abi.encodeWithSelector(MockUSDC.InsufficientAllowance.selector, alice, bob, 10e6, 20e6));
        usdc.transferFrom(alice, bob, 20e6);
    }

    function test_RevertWhen_InsufficientBalance() public {
        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(MockUSDC.InsufficientBalance.selector, alice, 0, 1));
        usdc.transfer(bob, 1);
    }

    function test_RevertWhen_MintToZero() public {
        vm.expectRevert(MockUSDC.ZeroAddress.selector);
        usdc.mint(address(0), 1);
    }

    function test_CreateAddressForNonce0MatchesAnvilAccount0() public pure {
        // anvil account #0 deploying at nonce 0 → the address in test/fixtures/addresses.dev.json
        address deployer = 0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266;
        address predicted =
            address(uint160(uint256(keccak256(abi.encodePacked(bytes1(0xd6), bytes1(0x94), deployer, bytes1(0x80))))));
        require(predicted == 0x5FbDB2315678afecb367f032d93F642f64180aa3, "CREATE address");
    }
}
