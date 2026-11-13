// Package config defines the routing configuration the data plane serves.
//
// This is the "snapshot" the control plane will eventually push over gRPC
// (see proto/apigw/v1/config.proto). In Phase 1 it is read from a JSON file.
// Listener addresses are deliberately NOT part of it: they are node-local
// settings (flags), while this struct is cluster-wide, hot-reloadable state.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

const (
	LBRoundRobin = "round_robin"
	LBWeighted   = "weighted"

	DefaultConnectTimeoutMs        = 3_000
	DefaultResponseHeaderTimeoutMs = 30_000
	DefaultRequestTimeoutMs        = 60_000
)

type Config struct {
	// Version is informational for now; the control plane will assign it.
	Version  uint64    `json:"version"`
	Services []Service `json:"services"`
	Routes   []Route   `json:"routes"`
}

// Service is a logical backend: a set of targets plus how to talk to them.
type Service struct {
	Name        string   `json:"name"`
	LBAlgorithm string   `json:"lb_algorithm"` // round_robin (default) | weighted
	Targets     []Target `json:"targets"`

	// Dial timeout for a new upstream connection.
	ConnectTimeoutMs int `json:"connect_timeout_ms"`
	// Time to wait for upstream response headers after sending the request.
	ResponseHeaderTimeoutMs int `json:"response_header_timeout_ms"`
	// Hard cap for the whole exchange, including streaming the body.
	RequestTimeoutMs int `json:"request_timeout_ms"`
}

type Target struct {
	URL    string `json:"url"`    // http(s)://host:port[/base/path]
	Weight int    `json:"weight"` // used by "weighted"; default 1
}

type Route struct {
	Name    string      `json:"name"`
	Service string      `json:"service"`
	Hosts   []string    `json:"hosts"`   // empty = any; "*.example.com" = one label
	Methods []string    `json:"methods"` // empty = any
	Paths   []PathMatch `json:"paths"`

	// StripPrefix removes the matched prefix before forwarding
	// (/api/users/42 -> /42). Only applies to "prefix" matches.
	StripPrefix bool `json:"strip_prefix"`
	// PreserveHost forwards the client's Host header instead of the target's.
	PreserveHost bool `json:"preserve_host"`
	// Priority breaks ties between routes matching at the same path depth.
	Priority int `json:"priority"`
}

type PathMatch struct {
	Type  string `json:"type"` // exact | prefix
	Value string `json:"value"`
}

// Load reads, strictly decodes, defaults and validates a config file.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	c, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse decodes JSON strictly (unknown fields are errors, so typos like
// "stirp_prefix" fail loudly instead of being silently ignored) and then
// calls Normalize.
func Parse(r io.Reader) (*Config, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("decode config: unexpected data after top-level object")
	}
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Normalize applies defaults in place and validates the result. All problems
// are reported together rather than one at a time.
func (c *Config) Normalize() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	services := make(map[string]struct{}, len(c.Services))
	for i := range c.Services {
		s := &c.Services[i]
		if s.Name == "" {
			add("services[%d]: name is required", i)
			continue
		}
		if _, dup := services[s.Name]; dup {
			add("service %q: defined more than once", s.Name)
		}
		services[s.Name] = struct{}{}

		switch s.LBAlgorithm {
		case "":
			s.LBAlgorithm = LBRoundRobin
		case LBRoundRobin, LBWeighted:
		default:
			add("service %q: unknown lb_algorithm %q (want %q or %q)",
				s.Name, s.LBAlgorithm, LBRoundRobin, LBWeighted)
		}

		for _, f := range []struct {
			field string
			v     *int
			def   int
		}{
			{"connect_timeout_ms", &s.ConnectTimeoutMs, DefaultConnectTimeoutMs},
			{"response_header_timeout_ms", &s.ResponseHeaderTimeoutMs, DefaultResponseHeaderTimeoutMs},
			{"request_timeout_ms", &s.RequestTimeoutMs, DefaultRequestTimeoutMs},
		} {
			switch {
			case *f.v == 0:
				*f.v = f.def
			case *f.v < 0:
				add("service %q: %s must be positive", s.Name, f.field)
			}
		}

		if len(s.Targets) == 0 {
			add("service %q: at least one target is required", s.Name)
		}
		for j := range s.Targets {
			t := &s.Targets[j]
			u, err := url.Parse(t.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				add("service %q: target %q must be an absolute http(s) URL", s.Name, t.URL)
			}
			switch {
			case t.Weight == 0:
				t.Weight = 1
			case t.Weight < 0:
				add("service %q: target %q has negative weight", s.Name, t.URL)
			}
		}
	}

	routes := make(map[string]struct{}, len(c.Routes))
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.Name == "" {
			add("routes[%d]: name is required", i)
			continue
		}
		if _, dup := routes[r.Name]; dup {
			add("route %q: defined more than once", r.Name)
		}
		routes[r.Name] = struct{}{}

		if _, ok := services[r.Service]; !ok {
			add("route %q: unknown service %q", r.Name, r.Service)
		}
		if len(r.Paths) == 0 {
			add("route %q: at least one path is required", r.Name)
		}
		for _, p := range r.Paths {
			if p.Type != "exact" && p.Type != "prefix" {
				add("route %q: path type %q must be \"exact\" or \"prefix\"", r.Name, p.Type)
			}
			if !strings.HasPrefix(p.Value, "/") {
				add("route %q: path %q must start with \"/\"", r.Name, p.Value)
			}
		}
		for _, m := range r.Methods {
			if m == "" || strings.ToUpper(m) != m || strings.ContainsAny(m, " \t/") {
				add("route %q: method %q must be an upper-case token such as GET", r.Name, m)
			}
		}
	}

	return errors.Join(errs...)
}
