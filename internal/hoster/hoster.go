// Package hoster defines the file-hoster abstraction: given a link to a
// hoster's public file page, resolve a direct download URL (plus optional
// filename and size).
package hoster

import (
	"context"
	"fmt"
)

// Resolved is the direct, fetchable result for a single public link.
type Resolved struct {
	// DirectURL is the URL the file should be downloaded from.
	DirectURL string
	// Filename is the hoster-suggested filename, or "" if unknown.
	Filename string
	// Size is the file size in bytes, or 0 if the hoster did not disclose it.
	Size int64
}

// Hoster resolves public hoster links to direct download URLs.
type Hoster interface {
	// Name returns the hoster's short identifier, e.g. "rapidgator".
	Name() string
	// Matches reports whether link looks like a URL this hoster can handle.
	// It should be a cheap syntactic check; do not make network calls.
	Matches(link string) bool
	// Resolve exchanges a public hoster URL for a direct download URL,
	// along with whatever filename and size metadata the hoster discloses.
	Resolve(ctx context.Context, link string) (Resolved, error)
}

// Registry holds the set of hosters that the application knows about.
// Register and Find are not safe for concurrent use: register all hosters
// at startup before any goroutine calls Find.
type Registry struct {
	hosters []Hoster
}

// NewRegistry returns a Registry seeded with the given hosters.
func NewRegistry(hs ...Hoster) *Registry {
	return &Registry{hosters: append([]Hoster(nil), hs...)}
}

// Register adds h to the registry. Not safe for concurrent use; call only
// during startup before any goroutine uses [Registry.Find].
func (r *Registry) Register(h Hoster) {
	r.hosters = append(r.hosters, h)
}

// Find returns the first hoster that claims the given link, or nil.
func (r *Registry) Find(link string) Hoster {
	for _, h := range r.hosters {
		if h.Matches(link) {
			return h
		}
	}
	return nil
}

// ErrAuth is returned when a hoster rejects the provided credentials.
type ErrAuth struct {
	// Hoster is the name of the hoster that rejected the credentials.
	Hoster string
	// Detail is the hoster's human-readable error message, if any.
	Detail string
}

func (e *ErrAuth) Error() string {
	return fmt.Sprintf("%s: auth failed: %s", e.Hoster, e.Detail)
}
