package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
	"github.com/arc119226/crypto-exchange/internal/webhook"
)

// ListWebhookEndpoints implements GET /admin/v1/webhooks.
func (h *Handler) ListWebhookEndpoints(ctx context.Context, _ gen.ListWebhookEndpointsRequestObject) (gen.ListWebhookEndpointsResponseObject, error) {
	if h.webhooks == nil {
		return nil, errWebhooksDisabled
	}
	rows, err := h.webhooks.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list webhook endpoints: %w", err)
	}
	out := make([]gen.WebhookEndpoint, 0, len(rows))
	for _, e := range rows {
		out = append(out, toWebhookEndpoint(e))
	}
	return gen.ListWebhookEndpoints200JSONResponse(gen.WebhookEndpointList{WebhookEndpoints: out}), nil
}

// CreateWebhookEndpoint implements POST /admin/v1/webhooks.
//
// The signing secret is in the 201 body and nowhere else. It is sealed before
// it is stored, gen.WebhookEndpoint has no field to carry it, and the audit
// record deliberately names only what the endpoint is -- so no read path, and
// no log, can produce it a second time.
func (h *Handler) CreateWebhookEndpoint(ctx context.Context, req gen.CreateWebhookEndpointRequestObject) (gen.CreateWebhookEndpointResponseObject, error) {
	const instance = "/admin/v1/webhooks"
	if h.webhooks == nil {
		return nil, errWebhooksDisabled
	}
	if req.Body == nil {
		return gen.CreateWebhookEndpoint400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "missing body"),
		}, nil
	}
	b := req.Body
	var (
		ep     webhook.Endpoint
		secret string
	)
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		ep, secret, err = h.webhooks.Create(ctx, tx, b.URL, b.Events, b.Label)
		if err != nil {
			return err
		}
		return h.audit.Record(ctx, tx, audit.Event{
			ActorType: audit.ActorAPIKey, ActorID: actorID, Action: "webhook.endpoint.create",
			TargetType: "webhook_endpoint", TargetID: ep.ID,
			After: map[string]any{
				"url": ep.URL, "events": ep.Events, "label": ep.Label, "status": ep.Status,
			},
			CorrelationID: telemetry.CorrelationID(ctx),
		})
	})
	switch {
	case errors.Is(err, webhook.ErrInvalid):
		return gen.CreateWebhookEndpoint400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error()),
		}, nil
	case err != nil:
		return nil, fmt.Errorf("create webhook endpoint: %w", err)
	}
	e := toWebhookEndpoint(ep)
	return gen.CreateWebhookEndpoint201JSONResponse(gen.CreatedWebhookEndpoint{
		ID: e.ID, URL: e.URL, Events: e.Events, Label: e.Label, Status: e.Status,
		CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt, Secret: secret,
	}), nil
}

// UpdateWebhookEndpoint implements PUT /admin/v1/webhooks/{id}.
func (h *Handler) UpdateWebhookEndpoint(ctx context.Context, req gen.UpdateWebhookEndpointRequestObject) (gen.UpdateWebhookEndpointResponseObject, error) {
	instance := "/admin/v1/webhooks/" + req.ID
	if h.webhooks == nil {
		return nil, errWebhooksDisabled
	}
	if req.Body == nil || req.Body.Reason == "" {
		return gen.UpdateWebhookEndpoint400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "url, events and reason are required"),
		}, nil
	}
	b := req.Body
	var ep webhook.Endpoint
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		before, err := h.webhooks.Get(ctx, req.ID)
		if err != nil {
			return err
		}
		ep, err = h.webhooks.Update(ctx, tx, req.ID, b.URL, b.Events, b.Label)
		if err != nil {
			return err
		}
		return h.audit.Record(ctx, tx, audit.Event{
			ActorType: audit.ActorAPIKey, ActorID: actorID, Action: "webhook.endpoint.update",
			TargetType: "webhook_endpoint", TargetID: ep.ID,
			Before: map[string]any{"url": before.URL, "events": before.Events, "label": before.Label},
			After: map[string]any{
				"url": ep.URL, "events": ep.Events, "label": ep.Label, "reason": b.Reason,
			},
			CorrelationID: telemetry.CorrelationID(ctx),
		})
	})
	switch {
	case errors.Is(err, webhook.ErrNotFound):
		return gen.UpdateWebhookEndpoint404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, instance, "webhook endpoint "+req.ID+" does not exist"),
		}, nil
	case errors.Is(err, webhook.ErrInvalid):
		return gen.UpdateWebhookEndpoint400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error()),
		}, nil
	case err != nil:
		return nil, fmt.Errorf("update webhook endpoint %s: %w", req.ID, err)
	}
	return gen.UpdateWebhookEndpoint200JSONResponse(toWebhookEndpoint(ep)), nil
}

// SetWebhookEndpointStatus implements PUT /admin/v1/webhooks/{id}/status.
//
// Disabling also drops what is still queued for the endpoint, in this same
// transaction, and the count goes in the audit record: it is the only place
// that will say why a customer never received those events.
func (h *Handler) SetWebhookEndpointStatus(ctx context.Context, req gen.SetWebhookEndpointStatusRequestObject) (gen.SetWebhookEndpointStatusResponseObject, error) {
	instance := "/admin/v1/webhooks/" + req.ID + "/status"
	if h.webhooks == nil {
		return nil, errWebhooksDisabled
	}
	if req.Body == nil || req.Body.Reason == "" {
		return gen.SetWebhookEndpointStatus400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "status and reason are required"),
		}, nil
	}
	status := string(req.Body.Status)
	var (
		ep      webhook.Endpoint
		dropped int64
	)
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		before, err := h.webhooks.Get(ctx, req.ID)
		if err != nil {
			return err
		}
		ep, dropped, err = h.webhooks.SetStatus(ctx, tx, req.ID, status)
		if err != nil {
			return err
		}
		return h.audit.Record(ctx, tx, audit.Event{
			ActorType: audit.ActorAPIKey, ActorID: actorID, Action: "webhook.endpoint.status.update",
			TargetType: "webhook_endpoint", TargetID: ep.ID,
			Before: map[string]any{"status": before.Status},
			After: map[string]any{
				"status": ep.Status, "reason": req.Body.Reason, "queued_dropped": dropped,
			},
			CorrelationID: telemetry.CorrelationID(ctx),
		})
	})
	switch {
	case errors.Is(err, webhook.ErrNotFound):
		return gen.SetWebhookEndpointStatus404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, instance, "webhook endpoint "+req.ID+" does not exist"),
		}, nil
	case errors.Is(err, webhook.ErrInvalid):
		return gen.SetWebhookEndpointStatus400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error()),
		}, nil
	case err != nil:
		return nil, fmt.Errorf("set webhook endpoint status %s: %w", req.ID, err)
	}
	return gen.SetWebhookEndpointStatus200JSONResponse(toWebhookEndpoint(ep)), nil
}

// ListWebhookDeliveries implements GET /admin/v1/webhooks/{id}/deliveries.
func (h *Handler) ListWebhookDeliveries(ctx context.Context, req gen.ListWebhookDeliveriesRequestObject) (gen.ListWebhookDeliveriesResponseObject, error) {
	instance := "/admin/v1/webhooks/" + req.ID + "/deliveries"
	if h.webhooks == nil {
		return nil, errWebhooksDisabled
	}
	// 404 on an endpoint that does not exist, rather than an empty list: they
	// mean different things to whoever is looking for a customer's history.
	if _, err := h.webhooks.Get(ctx, req.ID); err != nil {
		if errors.Is(err, webhook.ErrNotFound) {
			return gen.ListWebhookDeliveries404ApplicationProblemPlusJSONResponse{
				NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, instance, "webhook endpoint "+req.ID+" does not exist"),
			}, nil
		}
		return nil, fmt.Errorf("get webhook endpoint %s: %w", req.ID, err)
	}
	limit, offset := page(req.Params.Limit, req.Params.Offset)
	rows, err := h.webhooks.Deliveries(ctx, req.ID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list webhook deliveries: %w", err)
	}
	out := make([]gen.WebhookDelivery, 0, len(rows))
	for _, d := range rows {
		out = append(out, toWebhookDelivery(d))
	}
	return gen.ListWebhookDeliveries200JSONResponse(gen.WebhookDeliveryList{Deliveries: out}), nil
}

// ReplayWebhookDelivery implements
// POST /admin/v1/webhooks/{id}/deliveries/{delivery_id}/replay.
//
// 202: this queues a run, it does not send anything. The customer receives the
// event a second time with the same event_id, which is what a replay is.
func (h *Handler) ReplayWebhookDelivery(ctx context.Context, req gen.ReplayWebhookDeliveryRequestObject) (gen.ReplayWebhookDeliveryResponseObject, error) {
	instance := "/admin/v1/webhooks/" + req.ID + "/deliveries/" + req.DeliveryID + "/replay"
	if h.webhooks == nil {
		return nil, errWebhooksDisabled
	}
	var r webhook.Replay
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = h.webhooks.Replay(ctx, tx, req.ID, req.DeliveryID)
		if err != nil {
			return err
		}
		return h.audit.Record(ctx, tx, audit.Event{
			ActorType: audit.ActorAPIKey, ActorID: actorID, Action: "webhook.endpoint.replay",
			TargetType: "webhook_endpoint", TargetID: req.ID,
			After: map[string]any{
				"event_id": r.EventID, "run_id": r.RunID, "delivery_id": req.DeliveryID,
			},
			CorrelationID: telemetry.CorrelationID(ctx),
		})
	})
	switch {
	case errors.Is(err, webhook.ErrNotFound):
		return gen.ReplayWebhookDelivery404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, instance, "no delivery "+req.DeliveryID+" for webhook endpoint "+req.ID),
		}, nil
	case errors.Is(err, webhook.ErrDisabled):
		return gen.ReplayWebhookDelivery409ApplicationProblemPlusJSONResponse{
			ConflictApplicationProblemPlusJSONResponse: conflict(ctx, instance,
				"the endpoint is disabled, so a replay would queue work nothing delivers; enable it first"),
		}, nil
	case errors.Is(err, webhook.ErrQueued):
		return gen.ReplayWebhookDelivery409ApplicationProblemPlusJSONResponse{
			ConflictApplicationProblemPlusJSONResponse: conflict(ctx, instance, stillQueued(ctx, h, req.ID, req.DeliveryID)),
		}, nil
	case err != nil:
		return nil, fmt.Errorf("replay webhook delivery %s: %w", req.DeliveryID, err)
	}
	return gen.ReplayWebhookDelivery202JSONResponse(gen.WebhookReplay{
		EndpointID: r.EndpointID, EventID: r.EventID, RunID: r.RunID, NextAttemptAt: r.NextAttemptAt,
	}), nil
}

// stillQueued builds the refusal, naming when the retry that blocked the
// replay is due. Best effort: if the row has gone in the meantime, the general
// sentence is still true.
func stillQueued(ctx context.Context, h *Handler, endpointID, deliveryID string) string {
	const base = "this event is still queued for the endpoint, so the retry schedule will send it"
	at, err := h.webhooks.QueuedAt(ctx, endpointID, deliveryID)
	if err != nil {
		return base
	}
	return base + "; next attempt at " + at.UTC().Format(time.RFC3339)
}

var errWebhooksDisabled = errors.New("admin: webhooks are not enabled on this deployment")

func toWebhookEndpoint(e webhook.Endpoint) gen.WebhookEndpoint {
	return gen.WebhookEndpoint{
		ID: e.ID, URL: e.URL, Events: e.Events, Label: e.Label,
		Status: gen.WebhookEndpointStatus(e.Status), CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
}

func toWebhookDelivery(d webhook.Delivery) gen.WebhookDelivery {
	out := gen.WebhookDelivery{
		ID: d.ID, EventID: d.EventID, EventType: d.EventType, RunID: d.RunID,
		Attempt: d.Attempt, Status: gen.WebhookDeliveryStatus(d.Status), Error: d.Error,
		DurationMs: int32(d.Duration.Milliseconds()), //nolint:gosec // a stored duration in ms is far below 2^31
		CreatedAt:  d.CreatedAt, DeliveredAt: d.DeliveredAt,
	}
	if d.ResponseStatus != nil {
		code := int(*d.ResponseStatus)
		out.ResponseStatus = &code
	}
	return out
}
