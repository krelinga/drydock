package api

import (
	"net/http/httptest"
	"testing"
)

// TestOriginAllowedRefusesTwoOrigins: the Origin belt (§13.3) accepts exactly
// one header exactly equal to the UI origin. Two headers are refused even when
// both are the UI origin, and so is the right origin beside a wrong one in
// either order: a malformed request is not given the benefit of the doubt,
// and an implementation that read only the first (Header.Get) would pass one
// of these. The control is the one exact header, which is allowed.
func TestOriginAllowedRefusesTwoOrigins(t *testing.T) {
	const ui = "https://drydock.example.com"
	g := SessionGate{UIOrigin: ui, UIHost: "drydock.example.com"}
	for _, tc := range []struct {
		name    string
		origins []string
		want    bool
	}{
		{"control: one exact origin", []string{ui}, true},
		{"two, both the UI origin", []string{ui, ui}, false},
		{"the UI origin first, then another", []string{ui, "https://evil.example"}, false},
		{"another first, then the UI origin", []string{"https://evil.example", ui}, false},
		{"the UI origin beside an empty one", []string{ui, ""}, false},
		{"absent", nil, false},
		{"empty", []string{""}, false},
		{"null", []string{"null"}, false},
		{"one header listing two", []string{ui + ", " + ui}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "https://drydock.example.com/api/auth/signout", nil)
			for _, o := range tc.origins {
				r.Header.Add("Origin", o)
			}
			if got := g.OriginAllowed(r); got != tc.want {
				t.Errorf("Origin %q: allowed %v, want %v", tc.origins, got, tc.want)
			}
		})
	}
}
