package admin

import "github.com/go-chi/chi/v5"

// chiRouter is a bare router, the way admin_role.go hands one to Routes.
func chiRouter() chi.Router { return chi.NewRouter() }
