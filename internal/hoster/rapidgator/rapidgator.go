// Package rapidgator implements the Hoster interface for rapidgator.net
// premium accounts using the official v2 API.
package rapidgator

import (
	"context"
	"encoding/json"
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

// Client implements hoster.Hoster for rapidgator.net.
type Client struct {
	Login    string
	Password string

	// BaseURL overrides the API root (used in tests).
	BaseURL string
	// HTTPClient overrides the HTTP client (used in tests).
	HTTPClient *http.Client

	mu    sync.Mutex
	token string

	defaultOnce   sync.Once
	defaultClient *http.Client
}

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
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.ensureToken(ctx, attempt == 1)
		if err != nil {
			return hoster.Resolved{}, err
		}
		res, status, err := c.callDownload(ctx, fileID, tok)
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
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
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
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return defaultBaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	c.defaultOnce.Do(func() {
		c.defaultClient = &http.Client{Timeout: 30 * time.Second}
	})
	return c.defaultClient
}

func (c *Client) clearToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

func (c *Client) ensureToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	if !force && c.token != "" {
		t := c.token
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()

	tok, err := c.login(ctx)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.token = tok
	c.mu.Unlock()
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
	u := fmt.Sprintf("%s/api/v2/user/login?login=%s&password=%s",
		c.baseURL(), url.QueryEscape(c.Login), url.QueryEscape(c.Password))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("rapidgator: login: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", &hoster.ErrAuth{Hoster: "rapidgator", Detail: trimBody(body)}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("rapidgator: login: HTTP %d: %s", resp.StatusCode, trimBody(body))
	}
	var env loginEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return "", fmt.Errorf("rapidgator: login: parse: %w (body: %s)", err, trimBody(body))
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

func (c *Client) callDownload(ctx context.Context, fileID, token string) (hoster.Resolved, int, error) {
	u := fmt.Sprintf("%s/api/v2/file/download?file_id=%s&token=%s",
		c.baseURL(), url.QueryEscape(fileID), url.QueryEscape(token))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return hoster.Resolved{}, 0, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return hoster.Resolved{}, 0, fmt.Errorf("rapidgator: download: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return hoster.Resolved{}, resp.StatusCode, fmt.Errorf(
			"rapidgator: download: HTTP %d: %s", resp.StatusCode, trimBody(body))
	}
	var env downloadEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return hoster.Resolved{}, resp.StatusCode,
			fmt.Errorf("rapidgator: download: parse: %w (body: %s)", err, trimBody(body))
	}
	if env.Response.DownloadURL == "" {
		// Treat as auth issue if details suggest so, else as a generic error.
		return hoster.Resolved{}, resp.StatusCode,
			fmt.Errorf("rapidgator: download: empty download_url (details: %s)", env.Details)
	}
	var size int64
	if env.Response.Size != "" {
		size, _ = strconv.ParseInt(env.Response.Size.String(), 10, 64)
	}
	return hoster.Resolved{
		DirectURL: env.Response.DownloadURL,
		Filename:  env.Response.Filename,
		Size:      size,
	}, resp.StatusCode, nil
}

func trimBody(b []byte) string {
	const max = 200
	s := string(b)
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
