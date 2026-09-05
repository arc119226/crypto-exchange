package api

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/arc119226/crypto-exchange/internal/api/gen"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// Mount registers the public API routes on r. The router must already carry
// the correlation-id and metrics middleware (internal/app adds them), so
// every problem body and log line produced here has a correlation_id.
//
// Errors returned by handlers become 500 problems and are logged with the
// request logger; parameter binding errors become 400 problems.
func Mount(r chi.Router, h *Handler) {
	badRequest := func(w http.ResponseWriter, r *http.Request, err error) {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", err.Error())
	}
	strict := gen.NewStrictHandlerWithOptions(h, nil, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: badRequest,
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			telemetry.Logger(r.Context()).Error("api handler failed",
				slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.String("err", err.Error()))
			WriteProblem(w, r, http.StatusInternalServerError, "Internal Server Error", "")
		},
	})
	gen.HandlerWithOptions(strict, gen.ChiServerOptions{BaseRouter: r, ErrorHandlerFunc: badRequest})
}
