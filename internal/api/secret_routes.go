package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/krelinga/drydock/internal/secrets"
)

// SecretStore is what the secret routes need from internal/secrets. It has no
// method that returns a value: the routes could not leak one if they tried
// (§13.5, frontend §2.5).
type SecretStore interface {
	List(ctx context.Context) ([]secrets.Meta, error)
	// Undeliverable is nil when every stored secret can be delivered.
	Undeliverable(ctx context.Context) (*secrets.Undeliverable, error)
	Put(ctx context.Context, name, value, reach, description string) (secrets.PutResult, error)
	PutProse(ctx context.Context, name, reach, description string) (secrets.PutResult, error)
	Delete(ctx context.Context, name string) error
	SetGrants(ctx context.Context, name string, repoIDs []int64, allRepos bool) (secrets.Meta, error)
}

// SecretRoutes serves /api/secrets (design §5, §10). Store is nil when no
// master key is configured, and every route then says so.
//
// These answer 200 and 204 rather than the 202 of the workspace routes:
// nothing here is slow, and PUT's body carries the one thing the UI cannot
// learn from the stream in time to show it — which running workspaces a
// rotation reached, split by what it costs them (frontend §4.5 #5). Entity
// state still arrives through secret.* events, which is what the reducer
// applies (frontend §2.1).
type SecretRoutes struct {
	Store SecretStore
}

// Handlers returns the map Build consumes, keyed by route Name.
func (sr SecretRoutes) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"secrets.list":       sr.list,
		"secrets.put":        sr.put,
		"secrets.delete":     sr.delete,
		"secrets.grants.put": sr.grants,
	}
}

// maxSecretBody bounds a PUT: a value is at most secrets.MaxValueLen bytes,
// and JSON escaping can multiply that by six.
const maxSecretBody = 6*secrets.MaxValueLen + 16<<10

func (sr SecretRoutes) ready(w http.ResponseWriter) bool {
	if sr.Store == nil {
		WriteError(w, http.StatusServiceUnavailable, CodeSecretsNotConfigured,
			"No secrets master key is configured, so secrets cannot be stored.",
			"Start drydock serve with --secrets-key; the installer creates the key.")
		return false
	}
	return true
}

func (sr SecretRoutes) list(w http.ResponseWriter, r *http.Request) {
	if !sr.ready(w) {
		return
	}
	ms, err := sr.Store.List(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not read the secrets.", "")
		return
	}
	if ms == nil {
		ms = []secrets.Meta{}
	}
	// Whether the broker can deliver what is stored, from the snapshot it
	// reads: a page loaded after secret.undeliverable still learns of it,
	// and one loaded after the repair learns that too (frontend §4.5 #12).
	u, err := sr.Store.Undeliverable(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not read the secrets.", "")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Secrets       []secrets.Meta         `json:"secrets"`
		Undeliverable *secrets.Undeliverable `json:"undeliverable"`
	}{ms, u})
}

// decode reads a strict JSON body: an unknown field is refused rather than
// ignored, so a client that misspells `reach` is told so instead of having
// its write refused for a missing reach it thinks it sent.
func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		// The decoder's error can quote the body, and the body holds a
		// value: never pass it on.
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "Send a JSON object with the documented fields.", "")
		return false
	}
	return true
}

// optionalValue is PUT's `value`, which is three cases, not two, and each is
// decided here rather than left to a zero value (frontend §4.5 #13):
//
//   - absent: keep the stored value. Only the reach and description change,
//     which is never a rotation; for a name with no secret it is refused as
//     secret_value_required.
//   - a string: store it, after §10.1's rules. "" is a value that was sent,
//     and ValidateValue refuses it as secret_value_empty — an empty string is
//     never read as "keep".
//   - null: neither, so a bad request. Reading it as absent would make a
//     client's `value: undefined → null` bug silently keep a value the
//     operator meant to replace.
type optionalValue struct {
	set bool
	v   string
}

func (o *optionalValue) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return errors.New("value: null is neither a value nor absent")
	}
	o.set = true
	return json.Unmarshal(b, &o.v)
}

func (sr SecretRoutes) put(w http.ResponseWriter, r *http.Request) {
	if !sr.ready(w) {
		return
	}
	var body struct {
		Value       optionalValue `json:"value"`
		Reach       string        `json:"reach"`
		Description string        `json:"description"`
	}
	if !decode(w, r, maxSecretBody, &body) {
		return
	}
	var res secrets.PutResult
	var err error
	if body.Value.set {
		res, err = sr.Store.Put(r.Context(), r.PathValue("name"), body.Value.v, body.Reach, body.Description)
	} else {
		res, err = sr.Store.PutProse(r.Context(), r.PathValue("name"), body.Reach, body.Description)
	}
	body.Value = optionalValue{}
	if writeSecretError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (sr SecretRoutes) delete(w http.ResponseWriter, r *http.Request) {
	if !sr.ready(w) {
		return
	}
	if writeSecretError(w, sr.Store.Delete(r.Context(), r.PathValue("name"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (sr SecretRoutes) grants(w http.ResponseWriter, r *http.Request) {
	if !sr.ready(w) {
		return
	}
	var body struct {
		RepositoryIDs []int64 `json:"repository_ids"`
		AllRepos      bool    `json:"all_repos"`
	}
	if !decode(w, r, 64<<10, &body) {
		return
	}
	m, err := sr.Store.SetGrants(r.Context(), r.PathValue("name"), body.RepositoryIDs, body.AllRepos)
	if writeSecretError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Secret secrets.Meta `json:"secret"`
	}{m})
}

// writeSecretError writes err as the envelope and reports whether it did. A
// refused write carries its own code; nothing here quotes a value.
func writeSecretError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var inv *secrets.Invalid
	switch {
	case errors.As(err, &inv):
		WriteError(w, http.StatusBadRequest, inv.Code, inv.Message, inv.Detail)
	case errors.Is(err, secrets.ErrNotFound):
		WriteError(w, http.StatusNotFound, CodeNotFound, "There is no secret by that name.", "")
	default:
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not store the secret.", "")
	}
	return true
}
