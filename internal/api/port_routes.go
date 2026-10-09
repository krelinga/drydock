package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/krelinga/drydock/internal/life"
	"github.com/krelinga/drydock/internal/preview"
)

// PortRegistry is what the port routes need from preview.Service (PF §6, §13
// step 4). Every write goes through it, so a disable or a retire always takes
// the port's preview sessions with it in the same transaction, and closes its
// open websockets after (preview.Service.Revoked).
type PortRegistry interface {
	PreviewsOn() bool
	Ports(ctx context.Context, workspaceID string, hidden bool) ([]preview.Port, error)
	Port(ctx context.Context, workspaceID, portID string) (preview.Port, error)
	Add(ctx context.Context, workspaceID string, spec preview.AddSpec) (preview.Port, error)
	Update(ctx context.Context, workspaceID, portID string, c preview.Change) (preview.Port, error)
	Retire(ctx context.Context, workspaceID, portID string) error
}

// PortScanner is discovery's half of the registry (PF §8.2, §13 step 5):
// preview.Scanner. Rescan asks for a scan that begins after the call and
// returns at once.
type PortScanner interface {
	Rescan(ctx context.Context, workspaceID string) error
}

// PortProber is the probe's dial: preview.Proxy.Probe, the proxy's own dial
// and no second resolver (PF §13.4, "What step 4 should know").
type PortProber interface {
	Probe(ctx context.Context, workspaceID string, port int) preview.ProbeResult
}

// PortRoutes serves the port registry. Every mutation answers 202 and is
// settled by its event — port.added, port.enabled, port.disabled,
// port.updated, port.retired — which carries the row; the client discards the
// 202's body, as it does every mutation's (frontend §2.1). The rescan is
// settled by port.scanned, which the scan it asked for writes.
type PortRoutes struct {
	Registry PortRegistry
	Prober   PortProber
	Scanner  PortScanner
}

// Handlers returns the map Build consumes. Without a Scanner, ports.rescan is
// left out and Build mounts its 501 behind the gate.
func (pr PortRoutes) Handlers() map[string]http.HandlerFunc {
	if pr.Registry == nil {
		return map[string]http.HandlerFunc{}
	}
	h := map[string]http.HandlerFunc{
		"ports.list":   pr.list,
		"ports.add":    pr.add,
		"ports.update": pr.update,
		"ports.retire": pr.retire,
	}
	if pr.Prober != nil {
		h["ports.probe"] = pr.probe
	}
	if pr.Scanner != nil {
		h["ports.rescan"] = pr.rescan
	}
	return h
}

// rescan is POST …/ports/rescan: a discovery scan that begins after the
// request, for every workspace, answered for this one by port.scanned. It
// takes no body, and never enables anything — the scan cannot.
func (pr PortRoutes) rescan(w http.ResponseWriter, r *http.Request) {
	err := pr.Scanner.Rescan(r.Context(), r.PathValue("id"))
	if errors.Is(err, life.ErrStopping) || errors.Is(err, life.ErrNotStarted) {
		WriteError(w, http.StatusServiceUnavailable, CodeUnavailable, "Drydock is shutting down.", "")
		return
	}
	if err != nil {
		writePortError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, struct{}{})
}

// PortList is GET /api/workspaces/{id}/ports. previews says whether a preview
// domain is configured: without one a port is listed and never enabled, and
// the panel says why rather than offering a switch that cannot work.
type PortList struct {
	Ports    []preview.Port `json:"ports"`
	Previews bool           `json:"previews"`
}

func (pr PortRoutes) list(w http.ResponseWriter, r *http.Request) {
	hidden := r.URL.Query().Get("hidden") == "true"
	ps, err := pr.Registry.Ports(r.Context(), r.PathValue("id"), hidden)
	if err != nil {
		writePortError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, PortList{Ports: ps, Previews: pr.Registry.PreviewsOn()})
}

// addBody is POST …/ports.
type addBody struct {
	ContainerPort  *int   `json:"container_port"`
	Label          string `json:"label"`
	UpstreamScheme string `json:"upstream_scheme"`
	HostHeader     string `json:"host_header"`
}

func (pr PortRoutes) add(w http.ResponseWriter, r *http.Request) {
	var body addBody
	if !decodeStrict(r, &body) || body.ContainerPort == nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			"The request needs a container_port, and optionally a label, upstream_scheme and host_header.", "")
		return
	}
	p, err := pr.Registry.Add(r.Context(), r.PathValue("id"), preview.AddSpec{
		ContainerPort: *body.ContainerPort, Label: body.Label,
		UpstreamScheme: body.UpstreamScheme, HostHeader: body.HostHeader})
	if err != nil {
		writePortError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, struct {
		ID string `json:"id"`
	}{p.ID})
}

// updateBody is PATCH …/ports/{port}: each key present is a change, and a key
// absent is kept. A null is not a change and is refused, as an unknown key is.
type updateBody struct {
	Enabled    *bool   `json:"enabled"`
	Hidden     *bool   `json:"hidden"`
	Label      *string `json:"label"`
	HostHeader *string `json:"host_header"`
}

func (pr PortRoutes) update(w http.ResponseWriter, r *http.Request) {
	raw, ok := readBody(r)
	var keys map[string]json.RawMessage
	var body updateBody
	if ok {
		ok = json.Unmarshal(raw, &keys) == nil && len(keys) > 0
	}
	for _, v := range keys {
		if string(bytes.TrimSpace(v)) == "null" {
			ok = false
		}
	}
	if ok {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		ok = dec.Decode(&body) == nil
	}
	if !ok {
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			"The request changes one or more of enabled, hidden, label and host_header.", "")
		return
	}
	_, err := pr.Registry.Update(r.Context(), r.PathValue("id"), r.PathValue("port"), preview.Change{
		Enabled: body.Enabled, Hidden: body.Hidden, Label: body.Label, HostHeader: body.HostHeader})
	if err != nil {
		writePortError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, struct{}{})
}

func (pr PortRoutes) retire(w http.ResponseWriter, r *http.Request) {
	if err := pr.Registry.Retire(r.Context(), r.PathValue("id"), r.PathValue("port")); err != nil {
		writePortError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, struct{}{})
}

// probe is GET …/ports/{port}/probe: the row's port, dialled now through the
// proxy's own dial. Any live row may be probed, enabled or not — the probe
// connects and closes, and shows nothing the port serves.
func (pr PortRoutes) probe(w http.ResponseWriter, r *http.Request) {
	p, err := pr.Registry.Port(r.Context(), r.PathValue("id"), r.PathValue("port"))
	if err != nil {
		writePortError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pr.Prober.Probe(r.Context(), p.WorkspaceID, p.ContainerPort))
}

// readBody reads a small JSON body.
func readBody(r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 4<<10+1))
	return b, err == nil && len(b) <= 4<<10
}

// decodeStrict decodes one small JSON object with no unknown keys.
func decodeStrict(r *http.Request, v any) bool {
	raw, ok := readBody(r)
	if !ok {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		return false
	}
	return true
}

// writePortError maps the registry's refusals to the envelope.
func writePortError(w http.ResponseWriter, err error) {
	var inv *preview.InvalidError
	switch {
	case errors.As(err, &inv):
		WriteError(w, http.StatusBadRequest, CodeBadRequest, inv.Reason, "")
	case errors.Is(err, preview.ErrNoWorkspace):
		WriteError(w, http.StatusNotFound, CodeNotFound, "There is no such workspace.", "")
	case errors.Is(err, preview.ErrNoPort):
		WriteError(w, http.StatusNotFound, CodeNotFound, "This workspace lists no such port.", "")
	case errors.Is(err, preview.ErrPortExists):
		WriteError(w, http.StatusConflict, CodePortExists, "This workspace already lists that port. Enable it where it is.", "")
	case errors.Is(err, preview.ErrTooManyPorts):
		WriteError(w, http.StatusConflict, CodeTooManyPorts, "This workspace lists as many ports as it may. Remove one first.", "")
	case errors.Is(err, preview.ErrWorkspaceDeleting):
		WriteError(w, http.StatusConflict, CodeInProgress, "This workspace is being deleted.", "")
	case errors.Is(err, preview.ErrPreviewsOff):
		WriteError(w, http.StatusServiceUnavailable, CodePreviewsNotConfigured,
			"No preview domain is configured, so no port can be previewed.",
			"Install with --preview-domain and its wildcard certificate.")
	default:
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not change the port.", "")
	}
}
