package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/krelinga/drydock/internal/auth"
)

// SessionRoutes are the three /api/auth/session handlers (design §5, §13.2).
type SessionRoutes struct {
	Auth *auth.Service
}

// Handlers returns the map Build consumes, keyed by route Name.
func (s SessionRoutes) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"auth.session.create": s.signIn,
		"auth.session.read":   s.read,
		"auth.session.delete": s.signOut,
	}
}

// signIn is the only route in the system that answers without a session. The
// gate has already checked Host and Origin, so a cross-site login-CSRF that
// signs the operator into a session the attacker chose is refused there.
func (s SessionRoutes) signIn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "Send a JSON body with a password.", "")
		return
	}

	res, err := s.Auth.SignIn(r.Context(), clientIP(r), body.Password, deviceLabel(r.UserAgent()))
	body.Password = ""
	if lo, ok := auth.IsLockedOut(err); ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(lo.RetryAfter.Round(time.Second)/time.Second)))
		WriteError(w, http.StatusTooManyRequests, CodeLockedOut,
			"Too many failed sign-ins. Wait before trying again.", "")
		return
	}
	switch {
	case errors.Is(err, auth.ErrBadPassword):
		WriteError(w, http.StatusUnauthorized, CodeBadPassword, "That password is not right.", "")
		return
	case errors.Is(err, auth.ErrNotConfigured):
		// Says what to do, because the only person who can see this before
		// a password exists is the operator setting the box up.
		WriteError(w, http.StatusServiceUnavailable, CodeNotConfigured,
			"Drydock has no password yet. Run `drydock passwd` on the host.", "")
		return
	case err != nil:
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Sign-in failed.", "")
		return
	}
	setSessionCookie(w, res.Cookie, auth.AbsoluteLifetime)
	// Design §12, *Repeated failed sign-ins*: surfaced on the next sign-in,
	// once. With none, the answer is the bare 204 it always was; with some,
	// a 200 saying how many and from where — the one sign-in that learns it,
	// since the count is of failures since the last success.
	if res.FailedSinceLastSignIn > 0 {
		writeJSON(w, http.StatusOK, SignInNotice{FailedAttempts: res.FailedSinceLastSignIn, FailedSources: res.FailedSources})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SignInNotice is a sign-in's 200 body: the bad-password attempts since the
// previous successful sign-in from anywhere, and the addresses they came from.
type SignInNotice struct {
	FailedAttempts int      `json:"failed_attempts"`
	FailedSources  []string `json:"failed_sources"`
}

type deviceJSON struct {
	ID         string    `json:"id"`
	Label      string    `json:"label"`
	CreatedIP  string    `json:"created_ip"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	// IsCurrent lets the device list warn that revoking this one signs you
	// out (frontend §4.5 #6), rather than doing it as a surprise.
	IsCurrent bool `json:"is_current"`
}

// read returns the current session and every signed-in device. The id shown
// is the cookie's SHA-256 — safe to display and useless as a cookie.
func (s SessionRoutes) read(w http.ResponseWriter, r *http.Request) {
	cur, _ := SessionFrom(r.Context())
	all, err := s.Auth.Sessions.List(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not list sessions.", "")
		return
	}
	out := struct {
		Current string       `json:"current"`
		Devices []deviceJSON `json:"devices"`
	}{Current: cur.ID, Devices: []deviceJSON{}}
	for _, d := range all {
		out.Devices = append(out.Devices, deviceJSON{
			ID: d.ID, Label: d.Label, CreatedIP: d.CreatedIP,
			CreatedAt: d.CreatedAt, LastSeenAt: d.LastSeenAt, ExpiresAt: d.ExpiresAt,
			IsCurrent: d.ID == cur.ID,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// signOut ends this session, or every session with ?all=true — design §13.2's
// one-click response to a lost phone.
func (s SessionRoutes) signOut(w http.ResponseWriter, r *http.Request) {
	cur, _ := SessionFrom(r.Context())
	var err error
	if r.URL.Query().Get("all") == "true" {
		err = s.Auth.Sessions.RevokeAll(r.Context())
	} else {
		err = s.Auth.Sessions.Revoke(r.Context(), cur.ID)
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Sign-out failed.", "")
		return
	}
	setSessionCookie(w, "", 0)
	w.WriteHeader(http.StatusNoContent)
}

// setSessionCookie writes design §13.2's cookie. Every attribute is required:
// __Host- makes the browser refuse it without Secure and Path=/ and with a
// Domain (Spike 04 verified all three), SameSite=Lax is what lets the preview
// handshake's cross-site navigation carry it, and HttpOnly keeps it from any
// script. maxAge 0 clears it.
func setSessionCookie(w http.ResponseWriter, value string, maxAge time.Duration) {
	c := &http.Cookie{
		Name: auth.CookieName, Value: value, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	}
	if maxAge > 0 {
		c.MaxAge = int(maxAge / time.Second)
	} else {
		c.MaxAge = -1 // emits Max-Age=0: delete now
	}
	http.SetCookie(w, c)
}

// clientIP is the address Caddy saw. Trustworthy here, and only here, because
// the API socket admits Caddy and nothing else (§13.2's note): no client can
// reach Drydock without Caddy replacing this header. If a misconfigured Caddy
// appended instead of replacing, the last entry is still the one Caddy added.
func clientIP(r *http.Request) string {
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return "unknown"
	}
	parts := strings.Split(xff, ",")
	return strings.TrimSpace(parts[len(parts)-1])
}

// deviceLabel is design §4's "user-agent digest": enough to recognise a device
// in a list ("iPhone Safari"), not the full string, which is a fingerprint.
func deviceLabel(ua string) string {
	platform := "Unknown device"
	for _, p := range []struct{ needle, name string }{
		{"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Android", "Android"},
		{"Macintosh", "Mac"}, {"Windows", "Windows"}, {"CrOS", "ChromeOS"}, {"Linux", "Linux"},
	} {
		if strings.Contains(ua, p.needle) {
			platform = p.name
			break
		}
	}
	browser := ""
	for _, b := range []struct{ needle, name string }{
		{"Firefox/", "Firefox"}, {"Edg/", "Edge"}, {"CriOS/", "Chrome"}, {"Chrome/", "Chrome"},
		{"Safari/", "Safari"},
	} {
		if strings.Contains(ua, b.needle) {
			browser = b.name
			break
		}
	}
	if browser == "" {
		return platform
	}
	return platform + " " + browser
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
