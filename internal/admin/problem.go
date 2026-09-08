// Package admin implements the operator API (api/admin/v1/openapi.yaml) on
// top of the oapi-codegen strict server in internal/admin/gen, and the back
// office: the pages under /admin (ui*.go, templates/, static/) and the
// session middleware they sit behind (session.go).
//
// Every write lives once, as an unexported method on Handler that runs the
// transaction, the audit record and the outbox event together. The REST
// method maps its errors to problem+json; the back office maps the same
// errors to a flash message. Neither can drift from the other because
// neither owns the write.
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
