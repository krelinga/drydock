package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/krelinga/drydock/internal/sys"
)

// DefaultBaseURL is GitHub's REST API.
const DefaultBaseURL = "https://api.github.com"

// Permission levels, as GitHub spells them.
const (
	Read  = "read"
	Write = "write"
)

// Client talks to GitHub as one App.
type Client struct {
	AppID   int64
	Key     *rsa.PrivateKey
	BaseURL string // DefaultBaseURL unless a test points it at a fake
	HTTP    *http.Client
	Clock   sys.Clock

	mu     sync.Mutex
	tokens map[string]Token
}

// Installation is one account the App is installed on.
type Installation struct {
	ID      int64
	Account string
	// AccountType is "User" or "Organization"; the installation-settings
	// URL differs between them.
	AccountType string
}

// SettingsURL is where the operator changes which repositories this
// installation covers — the link design §9.4 asks the UI to give when a
// repository they expected is missing.
func (i Installation) SettingsURL() string {
	if i.AccountType == "Organization" {
		return fmt.Sprintf("https://github.com/organizations/%s/settings/installations/%d", url.PathEscape(i.Account), i.ID)
	}
	return fmt.Sprintf("https://github.com/settings/installations/%d", i.ID)
}

// Repository is one repository an installation covers.
type Repository struct {
	ID            int64
	FullName      string
	DefaultBranch string
	Private       bool
	Archived      bool
	PushedAt      time.Time
}

// APIError is a non-2xx answer. Message is GitHub's own, which never carries
// a credential; the request's token is in a header and appears nowhere here.
type APIError struct {
	Status  int
	Method  string
	Path    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// IsNotFound reports whether err is a 404.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// Installations lists every installation of the App.
func (c *Client) Installations(ctx context.Context) ([]Installation, error) {
	jwt, err := JWT(c.AppID, c.Key, c.Clock.Now())
	if err != nil {
		return nil, err
	}
	var out []Installation
	for page := 1; ; page++ {
		var batch []struct {
			ID      int64 `json:"id"`
			Account struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"account"`
		}
		if err := c.do(ctx, "GET", fmt.Sprintf("/app/installations?per_page=100&page=%d", page), "Bearer "+jwt, nil, &batch); err != nil {
			return nil, err
		}
		for _, b := range batch {
			out = append(out, Installation{ID: b.ID, Account: b.Account.Login, AccountType: b.Account.Type})
		}
		if len(batch) < 100 {
			return out, nil
		}
	}
}

// TokenRequest says what a token may do. Permissions is GitHub's map, e.g.
// {"contents": "read"}. RepositoryIDs narrows it to those repositories; empty
// means every repository the installation covers, which only Drydock's own
// catalog asks for — a workspace's token always names its one repository.
type TokenRequest struct {
	InstallationID int64
	Permissions    map[string]string
	RepositoryIDs  []int64
}

func (r TokenRequest) key() string {
	perms := make([]string, 0, len(r.Permissions))
	for k, v := range r.Permissions {
		perms = append(perms, k+"="+v)
	}
	sort.Strings(perms)
	ids := append([]int64(nil), r.RepositoryIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return fmt.Sprintf("%d|%s|%v", r.InstallationID, strings.Join(perms, ","), ids)
}

// tokenReuseMargin: a cached token is reused only while it has this long
// left, so a caller never receives one that expires mid-operation. GitHub's
// tokens live an hour, so a token is reused for its first 55 minutes (§9.2).
const tokenReuseMargin = 5 * time.Minute

// maxCachedTokens bounds the cache (§4: "a bounded in-memory cache").
const maxCachedTokens = 256

// InstallationToken returns a token granting exactly r, from the cache when
// one with enough life left exists.
func (c *Client) InstallationToken(ctx context.Context, r TokenRequest) (Token, error) {
	if len(r.Permissions) == 0 {
		return Token{}, errors.New("github: a token request must name its permissions; an unscoped token gets everything the App has")
	}
	k := r.key()
	now := c.Clock.Now()
	c.mu.Lock()
	if t, ok := c.tokens[k]; ok && t.ExpiresAt.Sub(now) > tokenReuseMargin {
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()

	jwt, err := JWT(c.AppID, c.Key, now)
	if err != nil {
		return Token{}, err
	}
	body := map[string]any{"permissions": r.Permissions}
	if len(r.RepositoryIDs) > 0 {
		body["repository_ids"] = r.RepositoryIDs
	}
	var resp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.do(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", r.InstallationID), "Bearer "+jwt, body, &resp); err != nil {
		return Token{}, err
	}
	if resp.Token == "" || resp.ExpiresAt.IsZero() {
		return Token{}, errors.New("github: token response without a token or an expiry")
	}
	t := Token{value: resp.Token, ExpiresAt: resp.ExpiresAt}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens == nil {
		c.tokens = map[string]Token{}
	}
	if len(c.tokens) >= maxCachedTokens {
		c.evict(now)
	}
	c.tokens[k] = t
	return t, nil
}

// evict drops expired tokens, then, if the cache is still full, the one
// expiring soonest. c.mu held.
func (c *Client) evict(now time.Time) {
	var soonest string
	for k, t := range c.tokens {
		if !t.ExpiresAt.After(now) {
			delete(c.tokens, k)
			continue
		}
		if soonest == "" || t.ExpiresAt.Before(c.tokens[soonest].ExpiresAt) {
			soonest = k
		}
	}
	if len(c.tokens) >= maxCachedTokens && soonest != "" {
		delete(c.tokens, soonest)
	}
}

// Repositories lists every repository an installation covers.
func (c *Client) Repositories(ctx context.Context, tok Token) ([]Repository, error) {
	var out []Repository
	for page := 1; ; page++ {
		var resp struct {
			TotalCount   int `json:"total_count"`
			Repositories []struct {
				ID            int64     `json:"id"`
				FullName      string    `json:"full_name"`
				DefaultBranch string    `json:"default_branch"`
				Private       bool      `json:"private"`
				Archived      bool      `json:"archived"`
				PushedAt      time.Time `json:"pushed_at"`
			} `json:"repositories"`
		}
		if err := c.do(ctx, "GET", fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page), "token "+tok.Value(), nil, &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Repositories {
			out = append(out, Repository{ID: r.ID, FullName: r.FullName, DefaultBranch: r.DefaultBranch,
				Private: r.Private, Archived: r.Archived, PushedAt: r.PushedAt})
		}
		if len(resp.Repositories) == 0 || len(out) >= resp.TotalCount {
			return out, nil
		}
	}
}

// ContentEntry is one item of a directory listing.
type ContentEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // file, dir, symlink, submodule
}

// Contents fetches a path in a repository at ref. A file yields one entry; a
// directory yields its listing. A missing path is an error IsNotFound
// recognises.
func (c *Client) Contents(ctx context.Context, tok Token, fullName, path, ref string) ([]ContentEntry, error) {
	p := "/repos/" + escapeFullName(fullName) + "/contents/" + escapePath(path)
	if ref != "" {
		p += "?ref=" + url.QueryEscape(ref)
	}
	var raw json.RawMessage
	if err := c.do(ctx, "GET", p, "token "+tok.Value(), nil, &raw); err != nil {
		return nil, err
	}
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		var dir []ContentEntry
		if err := json.Unmarshal(raw, &dir); err != nil {
			return nil, fmt.Errorf("github: contents of %s/%s: %w", fullName, path, err)
		}
		return dir, nil
	}
	var file ContentEntry
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("github: contents of %s/%s: %w", fullName, path, err)
	}
	return []ContentEntry{file}, nil
}

func escapeFullName(fullName string) string {
	owner, repo, _ := strings.Cut(fullName, "/")
	return url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func (c *Client) do(ctx context.Context, method, path, auth string, body, out any) error {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(base, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "drydock")
	req.Header.Set("Authorization", auth)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		// net/http's error quotes the URL, never the headers, so no
		// credential reaches this string.
		return fmt.Errorf("github: %s %s: %w", method, stripQuery(path), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = http.StatusText(resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Method: method, Path: stripQuery(path), Message: e.Message}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("github: %s %s: response: %w", method, stripQuery(path), err)
	}
	return nil
}

func stripQuery(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// ParseAppID is for flags: an App ID is a positive integer.
func ParseAppID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("github app id %q: must be a positive integer (the App ID, not the Client ID)", s)
	}
	return id, nil
}
