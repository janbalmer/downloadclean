package rapidgator

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestFileID(t *testing.T) {
	cases := []struct {
		in, want string
		err      bool
	}{
		{"https://rapidgator.net/file/abc123def/foo.rar.html", "abc123def", false},
		{"http://rapidgator.net/file/xyz/", "xyz", false},
		{"https://www.rapidgator.net/file/HelloWorld42/anything", "HelloWorld42", false},
		{"https://rapidgator.net/folder/abc/foo", "", true},
		{"https://example.com/file/abc/foo", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := FileID(c.in)
		if c.err {
			if err == nil {
				t.Errorf("FileID(%q): expected error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("FileID(%q): unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("FileID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMatches(t *testing.T) {
	c := New("a", "b")
	if !c.Matches("https://rapidgator.net/file/abc/foo.html") {
		t.Error("expected match")
	}
	if c.Matches("https://example.com/file/abc/foo") {
		t.Error("unexpected match")
	}
}

func TestResolve_Success(t *testing.T) {
	var loginHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/user/login":
			atomic.AddInt32(&loginHits, 1)
			if r.URL.Query().Get("login") != "user" || r.URL.Query().Get("password") != "pw" {
				http.Error(w, "bad creds", http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, `{"response":{"token":"TKN"},"status":200}`)
		case "/api/v2/file/download":
			if r.URL.Query().Get("token") != "TKN" || r.URL.Query().Get("file_id") != "abc123" {
				http.Error(w, "bad", http.StatusForbidden)
				return
			}
			fmt.Fprint(w, `{"response":{"download_url":"https://dl.example/abc","filename":"foo.rar","size":1024},"status":200}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{Login: "user", Password: "pw", BaseURL: srv.URL}
	res, err := c.Resolve(context.Background(), "https://rapidgator.net/file/abc123/foo.rar.html")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.DirectURL != "https://dl.example/abc" {
		t.Errorf("DirectURL = %q", res.DirectURL)
	}
	if res.Filename != "foo.rar" || res.Size != 1024 {
		t.Errorf("filename/size: %+v", res)
	}

	// Second Resolve should reuse the cached token (no extra login).
	_, err = c.Resolve(context.Background(), "https://rapidgator.net/file/abc123/foo.rar.html")
	if err != nil {
		t.Fatalf("Resolve 2: %v", err)
	}
	if got := atomic.LoadInt32(&loginHits); got != 1 {
		t.Errorf("login hits = %d, want 1 (token caching broken)", got)
	}
}

func TestResolve_TokenRefresh(t *testing.T) {
	var loginHits int32
	var firstDownload int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/user/login":
			n := atomic.AddInt32(&loginHits, 1)
			fmt.Fprintf(w, `{"response":{"token":"T%d"},"status":200}`, n)
		case "/api/v2/file/download":
			// First call: reject with 401 to force re-login. Second: succeed.
			if atomic.AddInt32(&firstDownload, 1) == 1 {
				http.Error(w, `{"status":401,"details":"expired"}`, http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, `{"response":{"download_url":"https://dl/x","filename":"x","size":7},"status":200}`)
		}
	}))
	defer srv.Close()

	c := &Client{Login: "u", Password: "p", BaseURL: srv.URL}
	c.token = "STALE" // pretend we have a cached but expired token

	res, err := c.Resolve(context.Background(), "https://rapidgator.net/file/zzz/y.rar")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.DirectURL != "https://dl/x" {
		t.Errorf("DirectURL = %q", res.DirectURL)
	}
	if got := atomic.LoadInt32(&loginHits); got != 1 {
		t.Errorf("login hits = %d, want 1", got)
	}
}

func TestResolve_BadCreds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"status":401,"details":"Login failed"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := &Client{Login: "u", Password: "p", BaseURL: srv.URL}
	_, err := c.Resolve(context.Background(), "https://rapidgator.net/file/abc/x")
	if err == nil {
		t.Fatal("expected error")
	}
}
