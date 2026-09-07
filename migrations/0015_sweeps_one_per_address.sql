-- +goose Up
-- One sweep in flight per address, across assets -- not per address and asset.
--
-- 0012 keyed this index on (tenant, chain, address, asset) and justified it by
-- the nonce: "the two would race for the same nonce". But the nonce is per
-- address (sweep/run.go asks the node for PendingNonceAt(from), §6.4.3), so
-- asset never belonged in the key. What two sweeps of one address actually
-- race for is the address's ether balance, and every one of them plans as if
-- it owned all of it: a native sweep reserves only its own 21000 gas, while
-- fundGas and stillAffordable read the raw balance.
--
-- So the two promise the same ether twice. A native sweep empties the address
-- a token sweep was just declared able to pay from; the token sweep then finds
-- it cannot afford its own transfer and is abandoned as balance_changed, which
-- is the good case -- it was observed on Sepolia, cost one gas-funding fee and
-- recovered on the next tick. The bad case is that the native transaction is
-- still in the mempool when the token sweep checks: the balance still looks
-- sufficient, the ERC-20 transfer is signed and broadcast, and then the native
-- one mines and takes the ether. The token transaction can no longer pay for
-- itself and is dropped, and nothing re-sends it -- so the token stays on that
-- deposit address until someone edits the database.
--
-- The address is the unit that owns the balance, so the address is the unit of
-- exclusion. planOne already treats a unique violation as normal and returns
-- nil, so a token sweep skipped this tick is simply planned on the next one.
DROP INDEX chain.sweeps_in_flight_uniq;

CREATE UNIQUE INDEX sweeps_in_flight_uniq ON chain.sweeps (tenant_id, chain_id, address_id)
    WHERE status IN ('requested', 'gas_funded', 'broadcast');
