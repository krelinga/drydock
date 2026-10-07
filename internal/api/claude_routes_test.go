package api

import "testing"

// TestCodeFromBody: the login code route takes exactly {"code": "<string>"},
// and returns the code as a slice of its own — not one aliasing the body,
// which the handler zeroes, and not a string, which it could not. The control
// is the plain and the escaped forms, which are read.
func TestCodeFromBody(t *testing.T) {
	for _, tc := range []struct {
		body, want string
		ok         bool
	}{
		{`{"code":"Kq7v#Hs3j"}`, "Kq7v#Hs3j", true},
		{` {"code": "Kq7v#Hs3j"} ` + "\n", "Kq7v#Hs3j", true},
		{`{"code":"Kq7v#Hs3j"}`, "Kq7v#Hs3j", true},
		{`{"code":"Kq7v#Hs3j"} {}`, "", false},
		{`{"code":"Kq7v#Hs3j"}x`, "", false},
		{`{"code":"Kq7v#Hs3j","x":1}`, "", false},
		{`{"code":1}`, "", false},
		{`{"code":null}`, "", false},
		{`{"other":"Kq7v#Hs3j"}`, "", false},
		{`["Kq7v#Hs3j"]`, "", false},
		{``, "", false},
	} {
		body := []byte(tc.body)
		got, ok := codeFromBody(body)
		if ok != tc.ok || string(got) != tc.want {
			t.Errorf("%q: %q, %v; want %q, %v", tc.body, got, ok, tc.want, tc.ok)
			continue
		}
		if ok {
			wipe(body)
			if string(got) != tc.want {
				t.Errorf("%q: zeroing the body changed the code to %q: it aliases the body", tc.body, got)
			}
		}
	}
}
