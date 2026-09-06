// Package admin implements the operator REST API (api/admin/v1/openapi.yaml)
// on top of the oapi-codegen strict server in internal/admin/gen. Phase 2
// covers ledger accounts, balances, entries, trial balance, adjustments and
// the audit log; the htmx UI, TOTP sessions and the remaining resources
// arrive with Phase 5 (docs/plan-v1.0.md §12).
package admin

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// ProblemContentType is the RFC 7807 media type.
const ProblemContentType = "application/problem+json"

// NewProblem builds an RFC 7807 body carrying the request's correlation id.
func NewProblem(ctx context.Context, status int, title, detail, instance string) gen.Problem {
	return gen.Problem{Type: "about:blank", Title: title, Status: status, Detail: detail, Instance: instance, CorrelationID: telemetry.CorrelationID(ctx)}
}

// WriteProblem writes an RFC 7807 response.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	w.Header().Set("Content-Type", ProblemContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(NewProblem(r.Context(), status, title, detail, r.URL.Path))
}
