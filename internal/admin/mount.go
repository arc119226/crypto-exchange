package admin

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// Mount registers the admin API routes on r. The router must already carry
// the correlation/metrics middleware and RequireAPIKey.
func Mount(r chi.Router, h *Handler) {
	badRequest := func(w http.ResponseWriter, r *http.Request, err error) {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", err.Error())
	}
	strict := gen.NewStrictHandlerWithOptions(h, nil, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: badRequest,
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			telemetry.Logger(r.Context()).Error("admin handler failed",
				slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.String("err", err.Error()))
			WriteProblem(w, r, http.StatusInternalServerError, "Internal Server Error", "")
		},
	})
	gen.HandlerWithOptions(strict, gen.ChiServerOptions{BaseRouter: r, ErrorHandlerFunc: badRequest})
}
