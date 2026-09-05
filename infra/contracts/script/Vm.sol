// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @notice The subset of Foundry's cheatcode interface this project uses.
/// @dev Selectors derive from these signatures, which match foundry-rs/foundry
///      `Vm.sol`. Declaring them here avoids a forge-std git submodule; add
///      cheatcodes as they become necessary (keep signatures verbatim).
interface Vm {
    // environment
    function envUint(string calldata name) external view returns (uint256);
    function envAddress(string calldata name) external view returns (address);
    function addr(uint256 privateKey) external pure returns (address);
    function getNonce(address account) external view returns (uint64);
    function label(address account, string calldata newLabel) external;

    // scripting
    function startBroadcast(uint256 privateKey) external;
    function stopBroadcast() external;
    function serializeUint(string calldata objectKey, string calldata valueKey, uint256 value)
        external
        returns (string memory);
    function serializeAddress(string calldata objectKey, string calldata valueKey, address value)
        external
        returns (string memory);
    function writeJson(string calldata json, string calldata path) external;

    // testing
    function prank(address msgSender) external;
    function expectRevert() external;
    function expectRevert(bytes4 revertData) external;
    function expectRevert(bytes calldata revertData) external;
}

/// @dev Shared cheatcode handle. `hevm cheat code` hashes to
///      0x7109709ECfa91a80626fF3989D68f67F5b1DD12D, the address Foundry
///      intercepts.
abstract contract CheatsBase {
    Vm internal constant vm = Vm(address(uint160(uint256(keccak256("hevm cheat code")))));
}

/// @dev Marker read by `forge script` when a file holds several contracts.
abstract contract ScriptBase is CheatsBase {
    bool public IS_SCRIPT = true;
}

/// @dev Marker read by `forge test`.
abstract contract TestBase is CheatsBase {
    bool public IS_TEST = true;
}
