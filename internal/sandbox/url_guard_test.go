package sandbox

import (
	"errors"
	"net"
	"testing"
)

// denyPrivate is the secure default policy.
var denyPrivate = OutboundURLPolicy{AllowPrivate: false}

// allowPrivate is the self-hosted opt-in policy.
var allowPrivate = OutboundURLPolicy{AllowPrivate: true}

func TestPolicyRejectsAlwaysForbiddenTargets(t *testing.T) {
	// These must be rejected under BOTH policies: link-local carries the cloud
	// metadata service, and the rest are not routable to a sandbox.
	cases := []struct{ name, raw string }{
		{"cloud metadata", "http://169.254.169.254/latest/meta-data/"},
		{"link local", "http://169.254.1.1"},
		{"unspecified", "http://0.0.0.0"},
		{"mdns suffix", "http://cube.local"},
		{"bad scheme file", "file:///etc/passwd"},
		{"bad scheme gopher", "gopher://evil"},
		{"empty", ""},
		{"no host", "http://"},

		// Other spellings of 169.254.169.254. Each wraps the metadata address
		// in an IPv6 encoding that a guard inspecting only the outer address
		// family reads as ordinary public IPv6.
		{"IPv4-mapped metadata", "http://[::ffff:169.254.169.254]"},
		{"6to4 metadata", "http://[2002:a9fe:a9fe::1]"},
		{"NAT64 metadata", "http://[64:ff9b::a9fe:a9fe]"},
		{"IPv4-compatible metadata", "http://[::169.254.169.254]"},
		{"Teredo", "http://[2001:0000:4136:e378:8000:63bf:3fff:fdd2]"},

		// fec0::/10 is not covered by net.IP.IsPrivate, so it used to pass
		// under both policies.
		{"IPv6 site-local", "http://[fec0::1]"},

		// Cannot reach a sandbox, and a config naming one is a typo worth
		// reporting at save time.
		{"broadcast", "http://255.255.255.255"},
		{"reserved 240/4", "http://240.0.0.1"},
		{"benchmarking range", "http://198.18.0.1"},
		{"IETF assignments", "http://192.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for label, policy := range map[string]OutboundURLPolicy{
				"deny-private":  denyPrivate,
				"allow-private": allowPrivate,
			} {
				err := policy.Validate(tc.raw)
				if err == nil {
					t.Fatalf("[%s] Validate(%q) = nil, want error", label, tc.raw)
				}
				if !errors.Is(err, ErrUnsafeOutboundURL) {
					t.Fatalf("[%s] Validate(%q) error = %v, want ErrUnsafeOutboundURL",
						label, tc.raw, err)
				}
			}
		})
	}
}

func TestPolicyRejectsPrivateTargetsByDefault(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:8080",
		"http://localhost:8080",
		"http://[::1]:8080",
		"http://10.0.0.5",
		"http://172.16.3.4",
		"http://192.168.1.1",
		"http://100.64.0.1",
	} {
		if err := denyPrivate.Validate(raw); err == nil {
			t.Fatalf("denyPrivate.Validate(%q) = nil, want error", raw)
		}
	}
}

func TestPolicyAllowsPrivateTargetsWhenOptedIn(t *testing.T) {
	// Self-hosted Cube listens on 127.0.0.1:33000 by default, so this opt-in
	// is what makes per-tenant Cube configuration possible at all.
	for _, raw := range []string{
		"http://127.0.0.1:33000",
		"http://localhost:33000",
		"http://10.0.0.5",
		"http://192.168.1.1:80",
	} {
		if err := allowPrivate.Validate(raw); err != nil {
			t.Fatalf("allowPrivate.Validate(%q) = %v, want nil", raw, err)
		}
	}
}

func TestPolicyAllowsPublicLiteralAddresses(t *testing.T) {
	// Literal addresses keep this hermetic: no DNS required. They are
	// documentation ranges, which this policy accepts on purpose — they are
	// never routed, so refusing them would protect nothing while costing every
	// test here a DNS lookup. The end-user URL guard in internal/utils makes
	// the opposite call for the same class.
	for _, raw := range []string{
		"http://203.0.113.10:8080",
		"https://203.0.113.10",
		"https://[2001:db8::1]:443/v1",
	} {
		if err := denyPrivate.Validate(raw); err != nil {
			t.Fatalf("denyPrivate.Validate(%q) = %v, want nil", raw, err)
		}
	}
}

func TestPolicyAllowsPublicHostname(t *testing.T) {
	const host = "sandbox.example"
	lookup := func(got string) ([]net.IP, error) {
		if got != host {
			t.Fatalf("lookup host = %q, want %q", got, host)
		}
		return []net.IP{net.ParseIP("203.0.113.10"), net.ParseIP("2001:db8::1")}, nil
	}
	if err := denyPrivate.validateWithLookupIP("https://"+host, lookup); err != nil {
		t.Fatalf("denyPrivate.Validate(%q) = %v, want nil", host, err)
	}
}

func TestPolicyValidateRejectsUnresolvableHost(t *testing.T) {
	// Fail closed: if we cannot verify where a host points, we refuse it. This
	// also gives the admin an early "that hostname does not exist" signal.
	lookup := func(string) ([]net.IP, error) { return nil, errors.New("fixture: DNS unavailable") }
	err := denyPrivate.validateWithLookupIP("https://sandbox.invalid", lookup)
	if !errors.Is(err, ErrUnsafeOutboundURL) {
		t.Fatalf("unresolvable host error = %v, want ErrUnsafeOutboundURL", err)
	}
}

func TestPolicyValidatesEveryDNSAnswer(t *testing.T) {
	// DNS policy tests are hermetic: a proxy's fake-IP response, a resolver
	// outage, or a changed public hostname must not change their expectations.
	for _, tc := range []struct {
		name      string
		addresses []net.IP
	}{
		{"empty", nil},
		{"nil address", []net.IP{nil}},
		{"public then metadata", []net.IP{net.ParseIP("203.0.113.10"), net.ParseIP("169.254.169.254")}},
		{"metadata then public", []net.IP{net.ParseIP("169.254.169.254"), net.ParseIP("203.0.113.10")}},
		{"fake IP range", []net.IP{net.ParseIP("198.18.0.113")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, policy := range []OutboundURLPolicy{denyPrivate, allowPrivate} {
				err := policy.validateWithLookupIP("https://sandbox.example", func(string) ([]net.IP, error) {
					return tc.addresses, nil
				})
				if !errors.Is(err, ErrUnsafeOutboundURL) {
					t.Fatalf("AllowPrivate=%v: error = %v, want ErrUnsafeOutboundURL", policy.AllowPrivate, err)
				}
			}
		})
	}
}

func TestPolicyDialControlMirrorsValidation(t *testing.T) {
	// The dialer must forbid exactly what validation forbids, otherwise a
	// saved config would fail mysteriously at first use.
	alwaysBlocked := []string{
		"169.254.169.254:80",
		"0.0.0.0:80",
		"[::ffff:169.254.169.254]:80",
		"[2002:a9fe:a9fe::1]:80",
		"[64:ff9b::a9fe:a9fe]:80",
		"[::169.254.169.254]:80",
		"[fec0::1]:80",
		"255.255.255.255:80",
	}
	privateOnly := []string{"127.0.0.1:8080", "10.1.2.3:443", "[::1]:8080", "100.64.0.1:443"}

	for _, address := range alwaysBlocked {
		if err := denyPrivate.DialControl("tcp", address, nil); err == nil {
			t.Fatalf("denyPrivate.DialControl(%q) = nil, want error", address)
		}
		if err := allowPrivate.DialControl("tcp", address, nil); err == nil {
			t.Fatalf("allowPrivate.DialControl(%q) = nil, want error", address)
		}
	}
	for _, address := range privateOnly {
		if err := denyPrivate.DialControl("tcp", address, nil); err == nil {
			t.Fatalf("denyPrivate.DialControl(%q) = nil, want error", address)
		}
		if err := allowPrivate.DialControl("tcp", address, nil); err != nil {
			t.Fatalf("allowPrivate.DialControl(%q) = %v, want nil", address, err)
		}
	}
	for _, address := range []string{"203.0.113.10:443", "[2001:db8::1]:443"} {
		if err := denyPrivate.DialControl("tcp", address, nil); err != nil {
			t.Fatalf("denyPrivate.DialControl(%q) = %v, want nil", address, err)
		}
	}
}

func TestDefaultOutboundURLPolicyFailsClosed(t *testing.T) {
	if DefaultOutboundURLPolicy().AllowPrivate {
		t.Fatal("callers without a workspace config must fail closed")
	}
}
