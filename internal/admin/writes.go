package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
	"github.com/arc119226/crypto-exchange/internal/webhook"
)

// Every write, once. The REST methods and the back office both call these
// and only differ in how they report the error: problem+json or a flash. The
// transaction, the audit record and the outbox event are one thing here, so
// there is no path into the tables that skips one of them.
//
// Each takes the actor off the context (actorFrom): a person with a session
// or the machine holding the API key.

func (h *Handler) createAccount(ctx context.Context, owner *string) (ledger.Account, error) {
	a := actorFrom(ctx)
	var created ledger.Account
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		acct, err := h.ledger.CreateSpotAccount(ctx, tx, owner)
		if err != nil {
			return err
		}
		created = acct
		return h.audit.Record(ctx, tx, audit.Event{
			ActorType: a.Type, ActorID: a.ID, IP: a.IP, Action: "account.create", TargetType: "account", TargetID: acct.ID,
			After: toAccount(acct), CorrelationID: telemetry.CorrelationID(ctx),
		})
	})
	return created, err
}

func (h *Handler) setAccountStatus(ctx context.Context, id, status, reason string) (ledger.Account, error) {
	a := actorFrom(ctx)
	var after ledger.Account
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		before, err := h.ledger.Account(ctx, id)
		if err != nil {
			return err
		}
		after, err = h.ledger.SetAccountStatus(ctx, tx, id, ledger.AccountStatus(status))
		if err != nil {
			return err
		}
		return h.audit.Record(ctx, tx, audit.Event{
			ActorType: a.Type, ActorID: a.ID, IP: a.IP, Action: "account.status.update", TargetType: "account", TargetID: id,
			Before: map[string]any{"status": before.Status, "reason": nil}, After: map[string]any{"status": after.Status, "reason": reason},
			CorrelationID: telemetry.CorrelationID(ctx),
		})
	})
	return after, err
}

func (h *Handler) createAdjustment(ctx context.Context, p ledger.AdjustParams) (ledger.JournalEntry, bool, error) {
	a := actorFrom(ctx)
	var (
		entry    ledger.JournalEntry
		replayed bool
	)
	p.CorrelationID = telemetry.CorrelationID(ctx)
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		entry, replayed, err = h.ledger.Adjust(ctx, tx, p)
		if err != nil || replayed {
			return err
		}
		return h.audit.Record(ctx, tx, audit.Event{
			ActorType: a.Type, ActorID: a.ID, IP: a.IP, Action: "ledger.adjustment.create", TargetType: "journal_entry", TargetID: fmt.Sprint(entry.ID),
			After: map[string]any{
				"account_id": p.AccountID, "asset": p.Asset, "amount": p.Amount, "direction": p.Direction,
				"reason": p.Reason, "idempotency_key": p.IdempotencyKey,
			},
			CorrelationID: telemetry.CorrelationID(ctx),
		})
	})
	return entry, replayed, err
}

func (h *Handler) createHouseAdjustment(ctx context.Context, p ledger.HouseAdjustParams) (ledger.JournalEntry, bool, error) {
	a := actorFrom(ctx)
	var (
		entry    ledger.JournalEntry
		replayed bool
	)
	p.CorrelationID = telemetry.CorrelationID(ctx)
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		entry, replayed, err = h.ledger.AdjustHouse(ctx, tx, p)
		if err != nil || replayed {
			return err
		}
		return h.audit.Record(ctx, tx, audit.Event{
			ActorType: a.Type, ActorID: a.ID, IP: a.IP, Action: "ledger.house_adjustment.create",
			TargetType: "journal_entry", TargetID: fmt.Sprint(entry.ID),
			After: map[string]any{
				"code": p.Code, "asset": p.Asset, "amount": p.Amount,
				"direction": p.Direction, "reason": p.Reason, "idempotency_key": p.IdempotencyKey,
			},
			CorrelationID: telemetry.CorrelationID(ctx),
		})
	})
	return entry, replayed, err
}

// setMarketStatus is the shape every registry write follows: read before,
// write, stop if nothing changed, audit, event -- one transaction, so the
// engine can only learn about a change that is committed and a change can
// never be committed without the event that carries it (§7.3, ADR-0002).
func (h *Handler) setMarketStatus(ctx context.Context, symbol, status, reason string) (registry.Market, error) {
	a := actorFrom(ctx)
	var market registry.Market
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		before, err := h.registry.GetMarket(ctx, h.tenant, symbol)
		if err != nil {
			return err
		}
		after, changed, err := h.registry.SetMarketStatus(ctx, tx, h.tenant, symbol, status)
		if err != nil {
			return err
		}
		market = after
		if !changed {
			return nil // already in that status: no audit row, no event
		}
		if err := h.audit.Record(ctx, tx, audit.Event{
			ActorType: a.Type, ActorID: a.ID, IP: a.IP, Action: "market.status.update",
			TargetType: "market", TargetID: after.Symbol,
			Before:        map[string]any{"status": before.Status, "version": before.Version},
			After:         map[string]any{"status": after.Status, "version": after.Version, "reason": reason},
			CorrelationID: telemetry.CorrelationID(ctx),
		}); err != nil {
			return err
		}
		evt, err := registry.MarketUpdatedEvent(h.tenant, after, []string{"status"}, reason, time.Now().UTC().Truncate(time.Microsecond))
		if err != nil {
			return err
		}
		evt.CorrelationID = telemetry.CorrelationID(ctx)
		_, err = eventbus.Outbox{}.Append(ctx, tx, evt)
		return err
	})
	return market, err
}

// reviewWithdrawal records a decision for the chain worker. A person's user
// id goes into reviewed_by; the API key leaves it empty and the audit trail
// carries the actor.
func (h *Handler) reviewWithdrawal(ctx context.Context, id string, approve bool, note string) (withdrawal.Record, error) {
	a := actorFrom(ctx)
	return h.withdrawals.Review(ctx, withdrawal.ReviewParams{
		ID: id, Approve: approve, Note: note,
		AdminID: a.UserID(), ActorType: a.Type, ActorID: a.ID, IP: a.IP,
	})
}

func (h *Handler) resolveWithdrawal(ctx context.Context, id string, action withdrawal.Action, note string) (withdrawal.Record, error) {
	a := actorFrom(ctx)
	return h.withdrawals.RequestResolve(ctx, withdrawal.ResolveParams{
		ID: id, Action: action, Note: note, ActorType: a.Type, ActorID: a.ID, IP: a.IP,
	})
}

func (h *Handler) createWebhookEndpoint(ctx context.Context, url string, events []string, label string) (webhook.Endpoint, string, error) {
	a := actorFrom(ctx)
	var (
		ep     webhook.Endpoint
		secret string
	)
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		ep, secret, err = h.webhooks.Create(ctx, tx, url, events, label)
		if err != nil {
			return err
		}
		// The audit record names the endpoint and never the secret.
		return h.audit.Record(ctx, tx, audit.Event{
			ActorType: a.Type, ActorID: a.ID, IP: a.IP, Action: "webhook.endpoint.create",
			TargetType: "webhook_endpoint", TargetID: ep.ID,
			After:         map[string]any{"url": ep.URL, "events": ep.Events, "label": ep.Label, "status": ep.Status},
			CorrelationID: telemetry.CorrelationID(ctx),
		})
	})
	return ep, secret, err
}

func (h *Handler) updateWebhookEndpoint(ctx context.Context, id, url string, events []string, label, reason string) (webhook.Endpoint, error) {
	a := actorFrom(ctx)
	var ep webhook.Endpoint
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		before, err := h.webhooks.Get(ctx, id)
		if err != nil {
			return err
		}
		ep, err = h.webhooks.Update(ctx, tx, id, url, events, label)
		if err != nil {
			return err
		}
		return h.audit.Record(ctx, tx, audit.Event{
			ActorType: a.Type, ActorID: a.ID, IP: a.IP, Action: "webhook.endpoint.update",
			TargetType: "webhook_endpoint", TargetID: ep.ID,
			Before:        map[string]any{"url": before.URL, "events": before.Events, "label": before.Label},
			After:         map[string]any{"url": ep.URL, "events": ep.Events, "label": ep.Label, "reason": reason},
			CorrelationID: telemetry.CorrelationID(ctx),
		})
	})
	return ep, err
}

// setWebhookEndpointStatus disables or enables, dropping what is queued on
// disable and recording how many in the audit row -- the only place that
// will say why a customer did not receive them.
func (h *Handler) setWebhookEndpointStatus(ctx context.Context, id, status, reason string) (webhook.Endpoint, error) {
	a := actorFrom(ctx)
	var ep webhook.Endpoint
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		before, err := h.webhooks.Get(ctx, id)
		if err != nil {
			return err
		}
		var dropped int64
		ep, dropped, err = h.webhooks.SetStatus(ctx, tx, id, status)
		if err != nil {
			return err
		}
		return h.audit.Record(ctx, tx, audit.Event{
			ActorType: a.Type, ActorID: a.ID, IP: a.IP, Action: "webhook.endpoint.status.update",
			TargetType: "webhook_endpoint", TargetID: ep.ID,
			Before:        map[string]any{"status": before.Status},
			After:         map[string]any{"status": ep.Status, "reason": reason, "queued_dropped": dropped},
			CorrelationID: telemetry.CorrelationID(ctx),
		})
	})
	return ep, err
}

func (h *Handler) replayWebhookDelivery(ctx context.Context, endpointID, deliveryID string) (webhook.Replay, error) {
	a := actorFrom(ctx)
	var r webhook.Replay
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = h.webhooks.Replay(ctx, tx, endpointID, deliveryID)
		if err != nil {
			return err
		}
		return h.audit.Record(ctx, tx, audit.Event{
			ActorType: a.Type, ActorID: a.ID, IP: a.IP, Action: "webhook.endpoint.replay",
			TargetType: "webhook_endpoint", TargetID: endpointID,
			After:         map[string]any{"event_id": r.EventID, "run_id": r.RunID, "delivery_id": deliveryID},
			CorrelationID: telemetry.CorrelationID(ctx),
		})
	})
	return r, err
}

// setUserStatus freezes or releases a user and, in the same transaction,
// their spot account: a frozen person whose account still trades is not
// frozen (docs/plan-v1.0.md §12, domain.md §24). A user with no spot
// account -- registration and bootstrap open one, nothing else does -- is
// not an error.
func (h *Handler) setUserStatus(ctx context.Context, id, status, reason string) (auth.User, error) {
	if h.users == nil {
		return auth.User{}, errUsersDisabled
	}
	a := actorFrom(ctx)
	var after auth.User
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		before, user, changed, err := h.users.SetUserStatus(ctx, tx, id, status)
		if err != nil {
			return err
		}
		after = user
		if !changed {
			return nil // already in that status: no audit row, no event
		}
		var accountID *string
		switch acct, err := h.ledger.SpotAccountOf(ctx, tx, id); {
		case errors.Is(err, ledger.ErrAccountNotFound):
		case err != nil:
			return err
		default:
			if _, err := h.ledger.SetAccountStatus(ctx, tx, acct.ID, ledger.AccountStatus(status)); err != nil {
				return err
			}
			accountID = &acct.ID
		}
		if err := h.audit.Record(ctx, tx, audit.Event{
			ActorType: a.Type, ActorID: a.ID, IP: a.IP, Action: "user.status.update", TargetType: "user", TargetID: id,
			Before:        map[string]any{"status": before.Status, "version": before.Version},
			After:         map[string]any{"status": after.Status, "version": after.Version, "reason": reason, "account_id": accountID},
			CorrelationID: telemetry.CorrelationID(ctx),
		}); err != nil {
			return err
		}
		evt, err := auth.UserStatusUpdatedEvent(h.tenant, before, after, reason, time.Now().UTC().Truncate(time.Microsecond))
		if err != nil {
			return err
		}
		evt.CorrelationID = telemetry.CorrelationID(ctx)
		_, err = eventbus.Outbox{}.Append(ctx, tx, evt)
		return err
	})
	return after, err
}

// setUserKYCLevel moves a user between KYC levels. Nothing pending is
// re-decided; the next withdrawal reads the new level.
func (h *Handler) setUserKYCLevel(ctx context.Context, id string, level int, reason string) (auth.User, error) {
	if h.users == nil {
		return auth.User{}, errUsersDisabled
	}
	a := actorFrom(ctx)
	var after auth.User
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		before, user, changed, err := h.users.SetUserKYCLevel(ctx, tx, id, level)
		if err != nil {
			return err
		}
		after = user
		if !changed {
			return nil
		}
		if err := h.audit.Record(ctx, tx, audit.Event{
			ActorType: a.Type, ActorID: a.ID, IP: a.IP, Action: "user.kyc_level.update", TargetType: "user", TargetID: id,
			Before:        map[string]any{"kyc_level": before.KYCLevel, "version": before.Version},
			After:         map[string]any{"kyc_level": after.KYCLevel, "version": after.Version, "reason": reason},
			CorrelationID: telemetry.CorrelationID(ctx),
		}); err != nil {
			return err
		}
		evt, err := auth.UserKYCLevelUpdatedEvent(h.tenant, before, after, reason, time.Now().UTC().Truncate(time.Microsecond))
		if err != nil {
			return err
		}
		evt.CorrelationID = telemetry.CorrelationID(ctx)
		_, err = eventbus.Outbox{}.Append(ctx, tx, evt)
		return err
	})
	return after, err
}

// errAlreadyExists is a POST for a row that exists; PUT edits it.
var errAlreadyExists = errors.New("admin: already exists")

// upsertAsset creates (create=true) or edits an asset. The registry's upsert
// bumps the version only when a field differs, so a PUT equal to the row
// leaves no audit row and no event.
func (h *Handler) upsertAsset(ctx context.Context, in registry.AssetInput, create bool, reason string) (registry.Asset, error) {
	a := actorFrom(ctx)
	var out registry.Asset
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		before, err := h.registry.GetAsset(ctx, h.tenant, in.Symbol)
		exists := err == nil
		switch {
		case err != nil && !errors.Is(err, registry.ErrNotFound):
			return err
		case create && exists:
			return errAlreadyExists
		case !create && !exists:
			return err
		}
		after, err := h.registry.UpsertAsset(ctx, tx, h.tenant, in)
		if err != nil {
			return err
		}
		out = after
		if exists && after.Version == before.Version {
			return nil
		}
		action, changed, beforeFields := "asset.create", []string{"created"}, map[string]any(nil)
		if exists {
			action, changed, beforeFields = "asset.update", assetChanges(before, after), assetFields(before)
		}
		afterFields := assetFields(after)
		afterFields["reason"] = reason
		if err := h.audit.Record(ctx, tx, audit.Event{
			ActorType: a.Type, ActorID: a.ID, IP: a.IP, Action: action, TargetType: "asset", TargetID: after.Symbol,
			Before: beforeFields, After: afterFields, CorrelationID: telemetry.CorrelationID(ctx),
		}); err != nil {
			return err
		}
		evt, err := registry.AssetUpdatedEvent(h.tenant, after, changed, reason, time.Now().UTC().Truncate(time.Microsecond))
		if err != nil {
			return err
		}
		evt.CorrelationID = telemetry.CorrelationID(ctx)
		_, err = eventbus.Outbox{}.Append(ctx, tx, evt)
		return err
	})
	return out, err
}

// upsertMarket creates or edits a market; market.updated names the fields
// that changed, and the engine opens or reconfigures the book on reload.
func (h *Handler) upsertMarket(ctx context.Context, in registry.MarketInput, create bool, reason string) (registry.Market, error) {
	a := actorFrom(ctx)
	var out registry.Market
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		before, err := h.registry.GetMarket(ctx, h.tenant, in.Symbol)
		exists := err == nil
		switch {
		case err != nil && !errors.Is(err, registry.ErrNotFound):
			return err
		case create && exists:
			return errAlreadyExists
		case !create && !exists:
			return err
		}
		after, err := h.registry.UpsertMarket(ctx, tx, h.tenant, in)
		if err != nil {
			return err
		}
		out = after
		if exists && after.Version == before.Version {
			return nil
		}
		action, changed, beforeFields := "market.create", []string{"created"}, map[string]any(nil)
		if exists {
			action, changed, beforeFields = "market.update", marketChanges(before, after), marketFields(before)
		}
		afterFields := marketFields(after)
		afterFields["reason"] = reason
		if err := h.audit.Record(ctx, tx, audit.Event{
			ActorType: a.Type, ActorID: a.ID, IP: a.IP, Action: action, TargetType: "market", TargetID: after.Symbol,
			Before: beforeFields, After: afterFields, CorrelationID: telemetry.CorrelationID(ctx),
		}); err != nil {
			return err
		}
		evt, err := registry.MarketUpdatedEvent(h.tenant, after, changed, reason, time.Now().UTC().Truncate(time.Microsecond))
		if err != nil {
			return err
		}
		evt.CorrelationID = telemetry.CorrelationID(ctx)
		_, err = eventbus.Outbox{}.Append(ctx, tx, evt)
		return err
	})
	return out, err
}

// upsertFeeSchedule creates or edits a fee schedule. One event for the
// schedule, none per market on it: the engine reloads everything either way.
func (h *Handler) upsertFeeSchedule(ctx context.Context, in registry.FeeScheduleInput, create bool, reason string) (registry.FeeSchedule, error) {
	a := actorFrom(ctx)
	var out registry.FeeSchedule
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		before, err := h.registry.GetFeeSchedule(ctx, h.tenant, in.Name)
		exists := err == nil
		switch {
		case err != nil && !errors.Is(err, registry.ErrNotFound):
			return err
		case create && exists:
			return errAlreadyExists
		case !create && !exists:
			return err
		}
		after, err := h.registry.UpsertFeeSchedule(ctx, tx, h.tenant, in)
		if err != nil {
			return err
		}
		out = after
		if exists && after.Version == before.Version {
			return nil
		}
		action, changed := "fee_schedule.create", []string{"created"}
		var beforeFields map[string]any
		if exists {
			action, changed = "fee_schedule.update", feeChanges(before, after)
			beforeFields = map[string]any{"maker_bps": before.MakerBps, "taker_bps": before.TakerBps, "version": before.Version}
		}
		if err := h.audit.Record(ctx, tx, audit.Event{
			ActorType: a.Type, ActorID: a.ID, IP: a.IP, Action: action, TargetType: "fee_schedule", TargetID: after.Name,
			Before:        beforeFields,
			After:         map[string]any{"maker_bps": after.MakerBps, "taker_bps": after.TakerBps, "version": after.Version, "reason": reason},
			CorrelationID: telemetry.CorrelationID(ctx),
		}); err != nil {
			return err
		}
		evt, err := registry.FeeScheduleUpdatedEvent(h.tenant, after, changed, reason, time.Now().UTC().Truncate(time.Microsecond))
		if err != nil {
			return err
		}
		evt.CorrelationID = telemetry.CorrelationID(ctx)
		_, err = eventbus.Outbox{}.Append(ctx, tx, evt)
		return err
	})
	return out, err
}

// upsertWithdrawalLimit sets the limits of one asset at one level, creating
// the row when there is none. No event: the withdrawal worker reads the
// table on every request (docs/plan-v1.0.md §6.4.2).
func (h *Handler) upsertWithdrawalLimit(ctx context.Context, in registry.WithdrawalLimitInput, reason string) (registry.WithdrawalLimit, error) {
	if err := registry.ValidateWithdrawalLimit(in); err != nil {
		return registry.WithdrawalLimit{}, err
	}
	a := actorFrom(ctx)
	var out registry.WithdrawalLimit
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		before, err := h.registry.GetWithdrawalLimit(ctx, h.tenant, in.Asset, in.KYCLevel)
		exists := err == nil
		if err != nil && !errors.Is(err, registry.ErrNotFound) {
			return err
		}
		after, err := h.registry.UpsertWithdrawalLimit(ctx, tx, h.tenant, in)
		if err != nil {
			return err
		}
		out = after
		if exists && after.Version == before.Version {
			return nil
		}
		action := "withdrawal_limit.create"
		var beforeFields map[string]any
		if exists {
			action = "withdrawal_limit.update"
			beforeFields = map[string]any{
				"auto_approve_limit": before.AutoApproveLimit, "daily_limit": before.DailyLimit,
				"require_manual_review": before.RequireManualReview, "version": before.Version,
			}
		}
		return h.audit.Record(ctx, tx, audit.Event{
			ActorType: a.Type, ActorID: a.ID, IP: a.IP, Action: action, TargetType: "withdrawal_limit",
			TargetID: fmt.Sprintf("%s/%d", after.Asset, after.KYCLevel), Before: beforeFields,
			After: map[string]any{
				"auto_approve_limit": after.AutoApproveLimit, "daily_limit": after.DailyLimit,
				"require_manual_review": after.RequireManualReview, "version": after.Version, "reason": reason,
			},
			CorrelationID: telemetry.CorrelationID(ctx),
		})
	})
	return out, err
}

// requestReload asks every engine to reload with no row changed.
func (h *Handler) requestReload(ctx context.Context, reason string) (eventbus.Envelope, error) {
	a := actorFrom(ctx)
	evt, err := registry.RegistryReloadEvent(h.tenant, reason, time.Now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return eventbus.Envelope{}, err
	}
	evt.CorrelationID = telemetry.CorrelationID(ctx)
	err = h.inTx(ctx, func(tx pgx.Tx) error {
		if err := h.audit.Record(ctx, tx, audit.Event{
			ActorType: a.Type, ActorID: a.ID, IP: a.IP, Action: "engine.reload", TargetType: "registry", TargetID: h.tenant,
			After: map[string]any{"reason": reason, "event_id": evt.EventID}, CorrelationID: telemetry.CorrelationID(ctx),
		}); err != nil {
			return err
		}
		_, err := eventbus.Outbox{}.Append(ctx, tx, evt)
		return err
	})
	return evt, err
}

// The audit trail keeps the whole writable row on both sides so a change
// can be read without the event; the event keeps only the field names.

func assetFields(a registry.Asset) map[string]any {
	return map[string]any{
		"name": a.Name, "chain_id": a.ChainID, "contract_address": a.ContractAddress, "is_native": a.IsNative,
		"scale": a.Scale, "display_scale": a.DisplayScale, "required_confirmations": a.RequiredConfirmations,
		"min_deposit": a.MinDeposit, "min_withdrawal": a.MinWithdrawal, "withdrawal_fee": a.WithdrawalFee,
		"withdrawal_fee_bps": a.WithdrawalFeeBps, "deposit_fee_bps": a.DepositFeeBps, "sweep_threshold": a.SweepThreshold,
		"deposit_enabled": a.DepositEnabled, "withdraw_enabled": a.WithdrawEnabled, "status": a.Status, "version": a.Version,
	}
}

func assetChanges(b, a registry.Asset) []string {
	var out []string
	add := func(name string, same bool) {
		if !same {
			out = append(out, name)
		}
	}
	add("name", b.Name == a.Name)
	add("chain_id", b.ChainID == a.ChainID)
	add("contract_address", ptrEq(b.ContractAddress, a.ContractAddress))
	add("is_native", b.IsNative == a.IsNative)
	add("scale", b.Scale == a.Scale)
	add("display_scale", b.DisplayScale == a.DisplayScale)
	add("required_confirmations", b.RequiredConfirmations == a.RequiredConfirmations)
	add("min_deposit", b.MinDeposit.Equal(a.MinDeposit))
	add("min_withdrawal", b.MinWithdrawal.Equal(a.MinWithdrawal))
	add("withdrawal_fee", b.WithdrawalFee.Equal(a.WithdrawalFee))
	add("withdrawal_fee_bps", b.WithdrawalFeeBps == a.WithdrawalFeeBps)
	add("deposit_fee_bps", b.DepositFeeBps == a.DepositFeeBps)
	add("sweep_threshold", b.SweepThreshold.Equal(a.SweepThreshold))
	add("deposit_enabled", b.DepositEnabled == a.DepositEnabled)
	add("withdraw_enabled", b.WithdrawEnabled == a.WithdrawEnabled)
	add("status", b.Status == a.Status)
	return out
}

func marketFields(m registry.Market) map[string]any {
	return map[string]any{
		"base_asset": m.BaseSymbol, "quote_asset": m.QuoteSymbol, "price_tick": m.PriceTick, "qty_step": m.QtyStep,
		"min_notional": m.MinNotional, "max_qty": m.MaxQty, "max_slippage_bps": m.MaxSlippageBps,
		"fee_schedule": m.FeeScheduleName, "self_trade_policy": m.SelfTradePolicy, "status": m.Status, "version": m.Version,
	}
}

func marketChanges(b, a registry.Market) []string {
	var out []string
	add := func(name string, same bool) {
		if !same {
			out = append(out, name)
		}
	}
	add("base_asset", b.BaseSymbol == a.BaseSymbol)
	add("quote_asset", b.QuoteSymbol == a.QuoteSymbol)
	add("price_tick", b.PriceTick.Equal(a.PriceTick))
	add("qty_step", b.QtyStep.Equal(a.QtyStep))
	add("min_notional", b.MinNotional.Equal(a.MinNotional))
	add("max_qty", (b.MaxQty == nil) == (a.MaxQty == nil) && (b.MaxQty == nil || b.MaxQty.Equal(*a.MaxQty)))
	add("max_slippage_bps", ptrEq(b.MaxSlippageBps, a.MaxSlippageBps))
	add("fee_schedule", b.FeeScheduleName == a.FeeScheduleName)
	add("self_trade_policy", b.SelfTradePolicy == a.SelfTradePolicy)
	add("status", b.Status == a.Status)
	return out
}

func feeChanges(b, a registry.FeeSchedule) []string {
	var out []string
	if b.MakerBps != a.MakerBps {
		out = append(out, "maker_bps")
	}
	if b.TakerBps != a.TakerBps {
		out = append(out, "taker_bps")
	}
	return out
}

func ptrEq[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// rotateWebhookSecret issues a new signing secret for an endpoint and keeps
// the old one for grace. The audit row says when the old one stops, and
// nothing about either secret.
func (h *Handler) rotateWebhookSecret(ctx context.Context, id string, grace time.Duration, reason string) (webhook.Rotated, error) {
	if h.webhooks == nil {
		return webhook.Rotated{}, errWebhooksDisabled
	}
	a := actorFrom(ctx)
	var out webhook.Rotated
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		r, err := h.webhooks.RotateSecret(ctx, tx, id, grace, time.Now().UTC())
		if err != nil {
			return err
		}
		out = r
		return h.audit.Record(ctx, tx, audit.Event{
			ActorType: a.Type, ActorID: a.ID, IP: a.IP, Action: "webhook.endpoint.rotate_secret",
			TargetType: "webhook_endpoint", TargetID: id,
			After:         map[string]any{"previous_secret_until": r.PreviousUntil, "grace": grace.String(), "reason": reason},
			CorrelationID: telemetry.CorrelationID(ctx),
		})
	})
	return out, err
}
