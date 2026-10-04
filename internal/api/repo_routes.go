package api

import (
	"context"
	"net/http"

	"github.com/krelinga/drydock/internal/catalog"
)

// RepoCatalog is what the repository routes need from internal/catalog.
type RepoCatalog interface {
	List(ctx context.Context) (catalog.View, error)
	Trigger()
}

// RepoRoutes serves GET /api/repos and POST /api/repos/refresh (design §5).
// Catalog is nil when no GitHub App is configured; both routes then say so
// with app_not_configured rather than an empty list, which would read as
// "the App is installed on nothing".
type RepoRoutes struct {
	Catalog RepoCatalog
}

// Handlers returns the map Build consumes, keyed by route Name.
func (rr RepoRoutes) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"repos.list":    rr.list,
		"repos.refresh": rr.refresh,
	}
}

func (rr RepoRoutes) notConfigured(w http.ResponseWriter) {
	WriteError(w, http.StatusServiceUnavailable, CodeAppNotConfigured,
		"No GitHub App is configured, so there is no repository list.",
		"Start drydock serve with --github-app-id and --github-app-key.")
}

func (rr RepoRoutes) list(w http.ResponseWriter, r *http.Request) {
	if rr.Catalog == nil {
		rr.notConfigured(w)
		return
	}
	v, err := rr.Catalog.List(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not read the repository list.", "")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// refresh is the design's async shape: accept, start, answer 202, and let
// the client hear the outcome as a repo.refreshed or repo.refresh_failed
// event (frontend §4.2). A refresh already running is joined, not doubled.
func (rr RepoRoutes) refresh(w http.ResponseWriter, r *http.Request) {
	if rr.Catalog == nil {
		rr.notConfigured(w)
		return
	}
	rr.Catalog.Trigger()
	writeJSON(w, http.StatusAccepted, struct{}{})
}
