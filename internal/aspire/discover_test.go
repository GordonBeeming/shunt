package aspire

import (
	"net/url"
	"testing"
)

func TestPortFor(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"http://localhost:8080", 8080},
		{"https://localhost:7123", 7123},
		{"http://localhost", 80},
		{"https://localhost", 443},
		{"redis://localhost:40359", 40359},
		{"tcp://127.0.0.1:1500", 1500},
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", c.raw, err)
		}
		got, err := portFor(u)
		if err != nil {
			t.Errorf("portFor(%q) error: %v", c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("portFor(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

func TestResourceMatches(t *testing.T) {
	cases := []struct {
		actual, logical string
		want            bool
	}{
		{"cache", "cache", true},
		{"cache-vdqvwwje", "cache", true}, // Aspire 13 unique suffix
		{"cache2", "cache", false},
		{"redis", "cache", false},
		{"postgres-abc", "postgres", true},
	}
	for _, c := range cases {
		if got := resourceMatches(c.actual, c.logical); got != c.want {
			t.Errorf("resourceMatches(%q,%q) = %v, want %v", c.actual, c.logical, got, c.want)
		}
	}
}

func TestFind(t *testing.T) {
	eps := []Endpoint{
		{Resource: "cache-xyz", Name: "tcp", Scheme: "redis", Host: "localhost", Port: 40359},
		{Resource: "api-abc", Name: "http", Scheme: "http", Host: "localhost", Port: 5001},
	}
	if ep, ok := Find(eps, "cache", "tcp"); !ok || ep.Port != 40359 {
		t.Errorf("Find cache/tcp = %+v, %v", ep, ok)
	}
	if ep, ok := Find(eps, "api", ""); !ok || ep.Port != 5001 {
		t.Errorf("Find api/(any) = %+v, %v", ep, ok)
	}
	if _, ok := Find(eps, "db", ""); ok {
		t.Error("Find db should miss")
	}
	if _, ok := Find(eps, "cache", "http"); ok {
		t.Error("Find cache/http should miss (wrong endpoint name)")
	}
}
