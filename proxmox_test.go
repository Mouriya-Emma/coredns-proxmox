package proxmox

import (
	"net"
	"net/netip"
	"reflect"
	"testing"
)

func mustPrefix(s string) netip.Prefix {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		panic(err)
	}
	return p
}

func TestFilterAllowed_NoCIDRsMeansAllowAll(t *testing.T) {
	p := &Proxmox{}
	in := []net.IP{net.ParseIP("192.168.1.22"), net.ParseIP("10.0.0.5")}
	got := p.filterAllowed(in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("want passthrough, got %v", got)
	}
}

func TestFilterAllowed_LAN24Keeps19216810_24Only(t *testing.T) {
	p := &Proxmox{AllowCIDRs: []netip.Prefix{mustPrefix("192.168.1.0/24")}}
	in := []net.IP{
		net.ParseIP("192.168.1.22"), // LAN, keep
		net.ParseIP("10.0.0.5"),     // drop (docker bridge)
		net.ParseIP("172.17.0.2"),   // drop (docker)
		net.ParseIP("100.85.224.16"),// drop (netbird mesh)
		net.ParseIP("fe80::1"),      // dropped earlier by appendParsed (LL), but prove CIDR also drops
	}
	got := p.filterAllowed(in)
	want := []net.IP{net.ParseIP("192.168.1.22")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestFilterAllowed_ExcludeIPsDropsListedIPs(t *testing.T) {
	p := &Proxmox{
		AllowCIDRs: []netip.Prefix{mustPrefix("192.168.1.0/24")},
		ExcludeIPs: []netip.Addr{
			netip.MustParseAddr("192.168.1.22"), // dns CT (static-claimed)
			netip.MustParseAddr("192.168.1.67"), // pve (static-claimed)
			netip.MustParseAddr("192.168.1.23"), // pve SR-IOV VF (not management)
			netip.MustParseAddr("192.168.1.68"), // pve debug PF (not management)
		},
	}
	in := []net.IP{
		net.ParseIP("192.168.1.22"), // exclude
		net.ParseIP("192.168.1.41"), // keep (app01)
		net.ParseIP("192.168.1.67"), // exclude
		net.ParseIP("192.168.1.68"), // exclude
	}
	got := p.filterAllowed(in)
	want := []net.IP{net.ParseIP("192.168.1.41")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestFilterAllowed_ExcludeIPsStandalone(t *testing.T) {
	// Without allow_cidr, exclude_ip still works on its own.
	p := &Proxmox{ExcludeIPs: []netip.Addr{netip.MustParseAddr("10.0.0.5")}}
	in := []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("10.0.0.6")}
	got := p.filterAllowed(in)
	want := []net.IP{net.ParseIP("10.0.0.6")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestAppendParsed_ParsesAndSkipsJunk(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"192.168.1.22/24", "192.168.1.22"},
		{"192.168.1.22", "192.168.1.22"},
		{"", ""},
		{"127.0.0.1", ""},       // loopback dropped
		{"0.0.0.0", ""},         // unspecified dropped
		{"169.254.1.1", ""},     // link-local v4 dropped
		{"fe80::1", ""},         // link-local v6 dropped
	}
	for _, c := range cases {
		got := appendParsed(nil, c.in)
		if c.want == "" {
			if len(got) != 0 {
				t.Errorf("%q: want empty, got %v", c.in, got)
			}
			continue
		}
		if len(got) != 1 || got[0].String() != c.want {
			t.Errorf("%q: want [%s], got %v", c.in, c.want, got)
		}
	}
}

func TestAddRecords_EmitsFQDNPerZone(t *testing.T) {
	p := &Proxmox{Zones: []string{"hb.lan.", "internal.hb.lan."}}
	rs := make(recordSet)
	p.addRecords(rs, "App01", []net.IP{net.ParseIP("192.168.1.41")})

	got := rs["app01.hb.lan."]
	if len(got) != 1 || got[0].String() != "192.168.1.41" {
		t.Errorf("hb.lan.: want one entry 192.168.1.41, got %v", got)
	}
	got = rs["app01.internal.hb.lan."]
	if len(got) != 1 {
		t.Errorf("internal.hb.lan.: want one entry, got %v", got)
	}
}

func TestAddRecords_StripsDotsInHostname(t *testing.T) {
	p := &Proxmox{Zones: []string{"hb.lan."}}
	rs := make(recordSet)
	p.addRecords(rs, "dns.hb.lan", []net.IP{net.ParseIP("192.168.1.22")})
	if _, ok := rs["dns.hb.lan."]; !ok {
		t.Errorf("expected key dns.hb.lan., got %v", rs)
	}
}

func TestNormaliseZones_StripsProtocolPortAddsDotLowercases(t *testing.T) {
	got := normaliseZones([]string{
		"dns://HB.lan.:5300",
		"other.Lan.:53",
		"tls://tls-zone:853",
		"plain",
		"",
	})
	want := []string{"hb.lan.", "other.lan.", "tls-zone.", "plain."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}
