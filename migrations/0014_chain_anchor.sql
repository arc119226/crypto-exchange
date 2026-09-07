-- +goose Up
-- Phase 4d: the chain-identity guard stops being pinned to genesis.
--
-- 0009 recorded chain.chain_state.genesis_hash and Start compared it with
-- eth_getBlockByNumber(0). On anvil that is free. On a public testnet
-- endpoint it is the one call in the startup path whose depth is unbounded,
-- and free endpoints are pools of heterogeneous backends: the same request
-- for block 0 against ethereum-sepolia-rpc.publicnode.com returned a block
-- one minute and {"code":4444,"message":"pruned history unavailable"} the
-- next. The guard was asking for the one block it needs least.
--
-- The anchor is now whatever ETH_SCAN_START_BLOCK says -- the block this
-- database's view of the chain begins at. Its hash answers "is this the same
-- chain?" exactly as well as genesis did, at a depth the node still serves,
-- and it needs no second setting that could disagree with the first. anvil
-- leaves ETH_SCAN_START_BLOCK at 0, so its anchor stays the genesis block and
-- its behaviour is unchanged.
--
-- Recording which block was pinned also closes a smaller hole: until now
-- ETH_SCAN_START_BLOCK could be changed on a live database and nothing
-- noticed, because it is only read when no cursor exists.
ALTER TABLE chain.chain_state RENAME COLUMN genesis_hash TO anchor_hash;

ALTER TABLE chain.chain_state
    ADD COLUMN anchor_block bigint NOT NULL DEFAULT 0 CHECK (anchor_block >= 0);

-- No new GRANTs: 0009 granted SELECT/INSERT/UPDATE on chain.chain_state at
-- table level, which covers columns added later.
