package config

import (
	"strings"
	"testing"
)

func TestDefaultsAreApplied(t *testing.T) {
	c, err := Parse(strings.NewReader(`{
	  "services":[{"name":"s","targets":[{"url":"http://127.0.0.1:1"}]}],
	  "routes":[{"name":"r","service":"s","paths":[{"type":"prefix","value":"/"}]}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Services[0]
	if s.LBAlgorithm != LBRoundRobin ||
		s.ConnectTimeoutMs != DefaultConnectTimeoutMs ||
		s.ResponseHeaderTimeoutMs != DefaultResponseHeaderTimeoutMs ||
		s.RequestTimeoutMs != DefaultRequestTimeoutMs ||
		s.Targets[0].Weight != 1 {
		t.Fatalf("defaults not applied: %+v", s)
	}
}

func TestInvalidConfigs(t *testing.T) {
	const svc = `"services":[{"name":"s","targets":[{"url":"http://h:1"}]}]`
	cases := []struct {
		name, json, wantErr string
	}{
		{"unknown field (typo)", `{` + svc + `,"routes":[{"name":"r","service":"s","stirp_prefix":true,"paths":[{"type":"prefix","value":"/"}]}]}`, "unknown field"},
		{"trailing data", `{` + svc + `} {}`, "unexpected data"},
		{"duplicate service", `{"services":[{"name":"s","targets":[{"url":"http://h:1"}]},{"name":"s","targets":[{"url":"http://h:1"}]}]}`, "more than once"},
		{"no targets", `{"services":[{"name":"s"}]}`, "at least one target"},
		{"bad scheme", `{"services":[{"name":"s","targets":[{"url":"ftp://h"}]}]}`, "absolute http(s) URL"},
		{"missing host", `{"services":[{"name":"s","targets":[{"url":"http://"}]}]}`, "absolute http(s) URL"},
		{"negative weight", `{"services":[{"name":"s","targets":[{"url":"http://h:1","weight":-1}]}]}`, "negative weight"},
		{"negative timeout", `{"services":[{"name":"s","request_timeout_ms":-5,"targets":[{"url":"http://h:1"}]}]}`, "must be positive"},
		{"unknown lb", `{"services":[{"name":"s","lb_algorithm":"random","targets":[{"url":"http://h:1"}]}]}`, "unknown lb_algorithm"},
		{"unknown service", `{` + svc + `,"routes":[{"name":"r","service":"zzz","paths":[{"type":"prefix","value":"/"}]}]}`, "unknown service"},
		{"duplicate route", `{` + svc + `,"routes":[{"name":"r","service":"s","paths":[{"type":"prefix","value":"/"}]},{"name":"r","service":"s","paths":[{"type":"prefix","value":"/"}]}]}`, "more than once"},
		{"no paths", `{` + svc + `,"routes":[{"name":"r","service":"s"}]}`, "at least one path"},
		{"bad path type", `{` + svc + `,"routes":[{"name":"r","service":"s","paths":[{"type":"regex","value":"/"}]}]}`, "must be \"exact\" or \"prefix\""},
		{"relative path", `{` + svc + `,"routes":[{"name":"r","service":"s","paths":[{"type":"prefix","value":"api"}]}]}`, "must start with"},
		{"lowercase method", `{` + svc + `,"routes":[{"name":"r","service":"s","methods":["get"],"paths":[{"type":"prefix","value":"/"}]}]}`, "upper-case token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.json))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestAllProblemsReportedTogether(t *testing.T) {
	_, err := Parse(strings.NewReader(`{
	  "services":[{"name":"s","targets":[]}],
	  "routes":[{"name":"r","service":"nope"}]
	}`))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"at least one target", "unknown service", "at least one path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	if _, err := Load("../../config.example.json"); err != nil {
		t.Fatal(err)
	}
}
