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
	DirectURL string
	Filename  string
	Size      int64
}

// Hoster resolves public hoster links to direct download URLs.
type Hoster interface {
	Name() string
	Matches(link string) bool
	Resolve(ctx context.Context, link string) (Resolved, error)
}

// Registry holds the set of hosters that the application knows about.
type Registry struct {
	hosters []Hoster
}

func NewRegistry(hs ...Hoster) *Registry {
	return &Registry{hosters: append([]Hoster(nil), hs...)}
}

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
	Hoster string
	Detail string
}

func (e *ErrAuth) Error() string {
	return fmt.Sprintf("%s: auth failed: %s", e.Hoster, e.Detail)
}
