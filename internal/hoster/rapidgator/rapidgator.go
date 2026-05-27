// Package rapidgator implements the Hoster interface for rapidgator.net
// premium accounts using the official v2 API.
package rapidgator

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/janbalmer/downloadclean/internal/hoster"
)

const defaultBaseURL = "https://rapidgator.net"

var fileIDRe = regexp.MustCompile(`(?i)^https?://(?:www\.)?rapidgator\.net/file/([A-Za-z0-9]+)(?:/|$)`)

// Client implements [hoster.Hoster] for rapidgator.net.
type Client struct {
	// Login is the rapidgator.net account login (typically an email).
	Login string
	// Password is the rapidgator.net account password.
	Password string

	// BaseURL overrides the API root (used in tests).
	BaseURL string
	// HTTPClient overrides the HTTP client (used in tests).
	HTTPClient *http.Client

	mu    sync.Mutex
	token string
}

// defaultRapidgatorClient is shared across all Client instances that don't
// supply their own HTTPClient, improving connection reuse.
var defaultRapidgatorClient = sync.OnceValue(func() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
})

// New returns a Client that authenticates as login/password.
func New(login, password string) *Client {
	return &Client{Login: login, Password: password}
}

func (c *Client) Name() string { return "rapidgator" }

func (c *Client) Matches(link string) bool {
	return fileIDRe.MatchString(link)
}

func (c *Client) Resolve(ctx context.Context, link string) (hoster.Resolved, error) {
	fileID, err := FileID(link)
	if err != nil {
		return hoster.Resolved{}, err
	}

	// Try once with the cached token, re-login once on auth failure.
	for attempt := range 2 {
		tok, err := c.ensureToken(ctx, attempt == 1)
		if err != nil {
			return hoster.Resolved{}, err
		}
		res, err := c.callDownload(ctx, fileID, tok)
		if err == nil {
			// Rapidgator's public URLs end in /<id>/<name>.html. If the API
			// didn't return a filename, fall back to that basename minus the
			// .html — keeping this here so the queue helper stays
			// hoster-agnostic.
			if res.Filename == "" {
				res.Filename = basenameFromPublicURL(link)
			}
			return res, nil
		}
		var authErr *hoster.ErrAuth
		if errors.As(err, &authErr) {
			c.clearToken()
			continue
		}
		return hoster.Resolved{}, err
	}
	return hoster.Resolved{}, fmt.Errorf("rapidgator: download: auth retry exhausted")
}

func basenameFromPublicURL(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	b := path.Base(u.Path)
	b = strings.TrimSuffix(b, ".html")
	if b == "." || b == "/" || b == "" {
		return ""
	}
	return b
}

// FileID extracts the file id from a rapidgator file URL.
// Accepts both .../file/<id>/<name>.html and .../file/<id>/ forms.
func FileID(link string) (string, error) {
	m := fileIDRe.FindStringSubmatch(link)
	if m == nil {
		return "", fmt.Errorf("rapidgator: not a rapidgator file URL: %q", link)
	}
	return m[1], nil
}

func (c *Client) baseURL() string {
	return cmp.Or(c.BaseURL, defaultBaseURL)
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return defaultRapidgatorClient()
}

func (c *Client) clearToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

// ensureToken returns a valid session token, logging in if needed. The lock
// is held across login() so two concurrent callers can't both issue a login.
func (c *Client) ensureToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" {
		return c.token, nil
	}
	tok, err := c.login(ctx)
	if err != nil {
		return "", err
	}
	c.token = tok
	return tok, nil
}

type loginEnvelope struct {
	Response struct {
		Token string `json:"token"`
	} `json:"response"`
	Status  int    `json:"status"`
	Details string `json:"details"`
}

func (c *Client) login(ctx context.Context) (string, error) {
	q := url.Values{
		"login":    {c.Login},
		"password": {c.Password},
	}
	endpoint := c.baseURL() + "/api/v2/user/login?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("rapidgator: login: %w", redactURLError(err))
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", &hoster.ErrAuth{Hoster: "rapidgator", Detail: trimBody(body)}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("rapidgator: login: HTTP %d: %s", resp.StatusCode, trimBody(body))
	}
	if readErr != nil {
		return "", fmt.Errorf("rapidgator: login: read: %w", readErr)
	}
	var env loginEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return "", fmt.Errorf("rapidgator: login: parse: %w (body: %s)", err, trimBody(body))
	}
	if env.Status != 0 && env.Status != 200 {
		if env.Status == 401 || env.Status == 403 {
			return "", &hoster.ErrAuth{Hoster: "rapidgator", Detail: env.Details}
		}
		return "", fmt.Errorf("rapidgator: login: api status %d: %s", env.Status, env.Details)
	}
	if env.Response.Token == "" {
		return "", &hoster.ErrAuth{Hoster: "rapidgator", Detail: env.Details}
	}
	return env.Response.Token, nil
}

type downloadEnvelope struct {
	Response struct {
		DownloadURL string      `json:"download_url"`
		Filename    string      `json:"filename"`
		Size        json.Number `json:"size"`
	} `json:"response"`
	Status  int    `json:"status"`
	Details string `json:"details"`
}

// callDownload returns *hoster.ErrAuth when the API rejects the token (either
// via HTTP status 401/403 or via {"status":401,...} inside an HTTP 200 body);
// the Resolve loop uses errors.As to detect and refresh.
func (c *Client) callDownload(ctx context.Context, fileID, token string) (hoster.Resolved, error) {
	q := url.Values{
		"file_id": {fileID},
		"token":   {token},
	}
	endpoint := c.baseURL() + "/api/v2/file/download?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return hoster.Resolved{}, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return hoster.Resolved{}, fmt.Errorf("rapidgator: download: %w", redactURLError(err))
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return hoster.Resolved{}, &hoster.ErrAuth{Hoster: "rapidgator", Detail: trimBody(body)}
	}
	if resp.StatusCode != http.StatusOK {
		return hoster.Resolved{}, fmt.Errorf("rapidgator: download: HTTP %d: %s", resp.StatusCode, trimBody(body))
	}
	if readErr != nil {
		return hoster.Resolved{}, fmt.Errorf("rapidgator: download: read: %w", readErr)
	}
	var env downloadEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return hoster.Resolved{}, fmt.Errorf("rapidgator: download: parse: %w (body: %s)", err, trimBody(body))
	}
	if env.Status != 0 && env.Status != 200 {
		if env.Status == 401 || env.Status == 403 {
			return hoster.Resolved{}, &hoster.ErrAuth{Hoster: "rapidgator", Detail: env.Details}
		}
		return hoster.Resolved{}, fmt.Errorf("rapidgator: download: api status %d: %s", env.Status, env.Details)
	}
	if env.Response.DownloadURL == "" {
		return hoster.Resolved{}, fmt.Errorf("rapidgator: download: empty download_url (details: %s)", env.Details)
	}
	var size int64
	if env.Response.Size != "" {
		s, perr := strconv.ParseInt(env.Response.Size.String(), 10, 64)
		if perr != nil {
			return hoster.Resolved{}, fmt.Errorf("rapidgator: download: size %q: %w", env.Response.Size.String(), perr)
		}
		size = s
	}
	return hoster.Resolved{
		DirectURL: env.Response.DownloadURL,
		Filename:  env.Response.Filename,
		Size:      size,
	}, nil
}

// redactURLError strips the query string from any *url.Error in the chain so
// rapidgator credentials and session tokens (which travel as query params)
// don't leak into wrapped error messages or logs.
func redactURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	if u, perr := url.Parse(ue.URL); perr == nil {
		u.RawQuery = ""
		ue.URL = u.String()
	} else {
		ue.URL = "[redacted]"
	}
	return err
}

func trimBody(b []byte) string {
	const max = 200
	s := string(b)
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
