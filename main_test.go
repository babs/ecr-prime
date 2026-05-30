package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestCleanRepo(t *testing.T) {
	cases := map[string]string{
		"ns/app:tag":                  "ns/app",
		"dist/mlmodels/public/foo:v1": "dist/mlmodels/public/foo", // tag stripped, multi-slash kept
		"a/b":                         "a/b",
		"/x/":                         "x",
		"repo":                        "repo",
		"a:b/c":                       "a:b/c", // colon before slash is NOT a tag
		"  ns/app  ":                  "ns/app",
	}
	for in, want := range cases {
		if got := cleanRepo(in); got != want {
			t.Errorf("cleanRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseTarget(t *testing.T) {
	host := "123456789012.dkr.ecr.eu-west-1.amazonaws.com"
	t.Run("ok", func(t *testing.T) {
		cases := []struct {
			args               []string
			wantHost, wantRepo string
		}{
			{[]string{host, "ns/app"}, host, "ns/app"},
			{[]string{host + "/ns/app"}, host, "ns/app"},
			{[]string{host + "/dist/mlmodels/public/foo:v1"}, host, "dist/mlmodels/public/foo"},
		}
		for _, c := range cases {
			h, r, err := parseTarget(c.args)
			if err != nil {
				t.Fatalf("parseTarget(%v) unexpected err: %v", c.args, err)
			}
			if h != c.wantHost || r != c.wantRepo {
				t.Errorf("parseTarget(%v) = (%q,%q), want (%q,%q)", c.args, h, r, c.wantHost, c.wantRepo)
			}
		}
	})
	t.Run("err", func(t *testing.T) {
		for _, args := range [][]string{
			{},
			{"a", "b", "c"},
			{"no-slash-single-arg"},
			{"", "repo"},
		} {
			if _, _, err := parseTarget(args); err == nil {
				t.Errorf("parseTarget(%v) expected error, got nil", args)
			}
		}
	})
}

func TestRegionFromHost(t *testing.T) {
	cases := map[string]string{
		"123456789012.dkr.ecr.eu-west-1.amazonaws.com":      "eu-west-1",
		"123456789012.dkr.ecr-fips.us-east-1.amazonaws.com": "us-east-1",
		"123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn":  "cn-north-1",
		"123456789012.dkr.ecr.us-gov-west-1.amazonaws.com":  "us-gov-west-1",
		"example.com": "",
		"123456789012.dkr.notecr.eu-west-1.amazonaws.com": "",
	}
	for host, want := range cases {
		if got := regionFromHost(host); got != want {
			t.Errorf("regionFromHost(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestBuildHost(t *testing.T) {
	cases := []struct {
		account, region, partition string
		fips                       bool
		want                       string
		wantErr                    bool
	}{
		{"123456789012", "eu-west-1", "aws", false, "123456789012.dkr.ecr.eu-west-1.amazonaws.com", false},
		{"123456789012", "eu-west-1", "", false, "123456789012.dkr.ecr.eu-west-1.amazonaws.com", false},
		{"123456789012", "us-east-1", "aws", true, "123456789012.dkr.ecr-fips.us-east-1.amazonaws.com", false},
		{"123456789012", "cn-north-1", "aws-cn", false, "123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn", false},
		{"123456789012", "us-gov-west-1", "aws-us-gov", false, "123456789012.dkr.ecr.us-gov-west-1.amazonaws.com", false},
		{"123456789012", "eu-west-1", "bogus", false, "", true},
	}
	for _, c := range cases {
		got, err := buildHost(c.account, c.region, c.partition, c.fips)
		if c.wantErr {
			if err == nil {
				t.Errorf("buildHost(%q,%q,%q,%v) expected error", c.account, c.region, c.partition, c.fips)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("buildHost(%q,%q,%q,%v) = (%q,%v), want (%q,nil)", c.account, c.region, c.partition, c.fips, got, err, c.want)
		}
		// every host we construct must pass the egress allowlist
		if !hostRe.MatchString(got) {
			t.Errorf("buildHost produced host %q that fails hostRe", got)
		}
	}
}

func TestCancelLocationPinsToHost(t *testing.T) {
	host := "123456789012.dkr.ecr.eu-west-1.amazonaws.com"
	cases := map[string]string{
		"":                                 "",
		"/v2/ns/app/blobs/uploads/abc?x=1": "https://" + host + "/v2/ns/app/blobs/uploads/abc?x=1",
		"https://" + host + "/v2/ns/app/blobs/uploads/abc":      "https://" + host + "/v2/ns/app/blobs/uploads/abc",
		"https://evil.example.com/v2/ns/app/blobs/uploads/abc":  "https://" + host + "/v2/ns/app/blobs/uploads/abc", // host pinned, token can't egress
		"https://evil.example.com/v2/x/blobs/uploads/abc?q=1#f": "https://" + host + "/v2/x/blobs/uploads/abc?q=1",
	}
	for loc, want := range cases {
		if got := cancelLocation(host, loc); got != want {
			t.Errorf("cancelLocation(%q, %q) = %q, want %q", host, loc, got, want)
		}
	}
}

func TestHostRe(t *testing.T) {
	valid := []string{
		"123456789012.dkr.ecr.eu-west-1.amazonaws.com",
		"123456789012.dkr.ecr-fips.us-east-1.amazonaws.com",
		"123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn",
	}
	invalid := []string{
		"evil.example.com",
		"123456789012.dkr.ecr.eu-west-1.amazonaws.com.evil.com",
		"12345.dkr.ecr.eu-west-1.amazonaws.com", // short account
		"123456789012.dkr.notecr.eu-west-1.amazonaws.com",
	}
	for _, h := range valid {
		if !hostRe.MatchString(h) {
			t.Errorf("hostRe should match %q", h)
		}
	}
	for _, h := range invalid {
		if hostRe.MatchString(h) {
			t.Errorf("hostRe should NOT match %q", h)
		}
	}
}

func TestDoPrimeRequest(t *testing.T) {
	nop := zap.NewNop()
	ctx := context.Background()

	t.Run("202-returns-location", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/v2/ns/app/blobs/uploads/uuid-1")
			w.WriteHeader(http.StatusAccepted)
		}))
		defer srv.Close()
		loc, err := doPrimeRequest(ctx, nop, srv.Client(), srv.URL, "Basic x", 3, 0)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if loc != "/v2/ns/app/blobs/uploads/uuid-1" {
			t.Errorf("loc = %q", loc)
		}
	})

	t.Run("403-permanent-not-transient", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors":[{"code":"DENIED"}]}`))
		}))
		defer srv.Close()
		_, err := doPrimeRequest(ctx, nop, srv.Client(), srv.URL, "Basic x", 3, 0)
		if err == nil {
			t.Fatal("expected error")
		}
		if errors.Is(err, errTransient) {
			t.Errorf("403 must NOT be transient: %v", err)
		}
		if !strings.Contains(err.Error(), "DENIED") {
			t.Errorf("error should carry the body: %v", err)
		}
		if got := hits.Load(); got != 1 {
			t.Errorf("403 must not be retried, got %d hits", got)
		}
	})

	t.Run("503-retried-then-transient", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		_, err := doPrimeRequest(ctx, nop, srv.Client(), srv.URL, "Basic x", 3, time.Millisecond)
		if !errors.Is(err, errTransient) {
			t.Errorf("503-exhausted must be transient: %v", err)
		}
		if got := hits.Load(); got != 3 {
			t.Errorf("expected 3 attempts, got %d", got)
		}
	})

	t.Run("503-then-202-recovers", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if hits.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Location", "/v2/x/blobs/uploads/u2")
			w.WriteHeader(http.StatusAccepted)
		}))
		defer srv.Close()
		loc, err := doPrimeRequest(ctx, nop, srv.Client(), srv.URL, "Basic x", 3, time.Millisecond)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if loc != "/v2/x/blobs/uploads/u2" {
			t.Errorf("loc = %q", loc)
		}
		if got := hits.Load(); got != 2 {
			t.Errorf("expected 2 attempts, got %d", got)
		}
	})
}

func TestResolveTarget(t *testing.T) {
	host := "123456789012.dkr.ecr.eu-west-1.amazonaws.com"
	t.Run("account-mode", func(t *testing.T) {
		h, r, reg, err := resolveTarget("123456789012", "eu-west-1", "aws", false, []string{"ns/app:tag"})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if h != host || r != "ns/app" || reg != "eu-west-1" {
			t.Errorf("got (%q,%q,%q)", h, r, reg)
		}
	})
	t.Run("host-mode-region-from-host", func(t *testing.T) {
		h, r, reg, err := resolveTarget("", "", "aws", false, []string{host + "/ns/app"})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if h != host || r != "ns/app" || reg != "eu-west-1" {
			t.Errorf("got (%q,%q,%q)", h, r, reg)
		}
	})
	t.Run("errors", func(t *testing.T) {
		cases := []struct {
			name                       string
			account, region, partition string
			args                       []string
		}{
			{"bad-account", "123", "eu-west-1", "aws", []string{"ns/app"}},
			{"account-no-region", "123456789012", "", "aws", []string{"ns/app"}},
			{"account-wrong-argc", "123456789012", "eu-west-1", "aws", []string{"a", "b"}},
			{"non-ecr-host", "", "", "aws", []string{"evil.example.com/ns/app"}},
			{"bad-partition", "123456789012", "eu-west-1", "nope", []string{"ns/app"}},
		}
		for _, c := range cases {
			if _, _, _, err := resolveTarget(c.account, c.region, c.partition, false, c.args); err == nil {
				t.Errorf("%s: expected error, got nil", c.name)
			}
		}
	})
}
