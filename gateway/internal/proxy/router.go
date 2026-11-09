package proxy

import (
	"sort"
	"strings"

	"github.com/JackMahfouz/e-wallet/gateway/internal/config"
)

// route is a configured route together with the upstream it proxies to.
type route struct {
	config.Route
	upstream *upstream
}

// pathEntry holds every route registered for one path, keyed by method.
type pathEntry struct {
	path     string
	byMethod map[string]*route
	methods  []string
}

// router matches a request path and method to a route. Paths ending in "/" match everything
// beneath them (longest wins); all other paths match exactly. It is deliberately not
// http.ServeMux: it never redirects, and unknown and wrong-method requests get JSON errors.
type router struct {
	exact    map[string]*pathEntry
	prefixes []*pathEntry
}

func newRouter(routes []*route) *router {
	rt := &router{exact: map[string]*pathEntry{}}
	prefixes := map[string]*pathEntry{}

	for _, r := range routes {
		entries := rt.exact
		if r.IsPrefix() {
			entries = prefixes
		}

		entry, ok := entries[r.Path]
		if !ok {
			entry = &pathEntry{path: r.Path, byMethod: map[string]*route{}}
			entries[r.Path] = entry
		}

		for _, method := range r.Methods {
			if _, dup := entry.byMethod[method]; !dup {
				entry.methods = append(entry.methods, method)
			}
			entry.byMethod[method] = r
		}
	}

	for _, entry := range prefixes {
		rt.prefixes = append(rt.prefixes, entry)
	}
	sort.Slice(rt.prefixes, func(i, j int) bool { return len(rt.prefixes[i].path) > len(rt.prefixes[j].path) })

	return rt
}

// match returns the route for a request. When no route takes the method but the path is known,
// it returns the methods that are allowed there; when it returns neither, the path is unknown.
func (rt *router) match(path, method string) (*route, []string) {
	var allowed []string

	candidates := make([]*pathEntry, 0, 2)
	if entry, ok := rt.exact[path]; ok {
		candidates = append(candidates, entry)
	}
	for _, entry := range rt.prefixes {
		if strings.HasPrefix(path, entry.path) {
			candidates = append(candidates, entry)
		}
	}

	for _, entry := range candidates {
		if r, ok := entry.byMethod[method]; ok {
			return r, nil
		}
		allowed = append(allowed, entry.methods...)
	}

	sort.Strings(allowed)

	return nil, unique(allowed)
}

func unique(sorted []string) []string {
	var out []string
	for i, value := range sorted {
		if i == 0 || value != sorted[i-1] {
			out = append(out, value)
		}
	}

	return out
}
