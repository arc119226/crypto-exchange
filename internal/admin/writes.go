package admin

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/audit"
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
