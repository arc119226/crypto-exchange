// Package api implements the public REST API (api/public/v1/openapi.yaml) on
// top of the oapi-codegen strict server in internal/api/gen. Handlers only
// translate between the OpenAPI models and the domain packages; business
// rules live in internal/registry, internal/trading, ... (docs/plan-v1.0.md §8).
package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/arc119226/crypto-exchange/internal/api/gen"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// ProblemContentType is the RFC 7807 media type used for every error body.
const ProblemContentType = "application/problem+json"

// NewProblem builds an RFC 7807 body carrying the request's correlation id.
func NewProblem(ctx context.Context, status int, title, detail, instance string) gen.Problem {
	return gen.Problem{
		Type:          "about:blank",
		Title:         title,
		Status:        status,
		Detail:        detail,
		Instance:      instance,
		CorrelationID: telemetry.CorrelationID(ctx),
	}
}

// WriteProblem writes an RFC 7807 response. It is the single place that
// shapes error bodies for the public API, router fallbacks (404/405/500)
// included, so clients see one error format everywhere.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	w.Header().Set("Content-Type", ProblemContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(NewProblem(r.Context(), status, title, detail, r.URL.Path))
}
