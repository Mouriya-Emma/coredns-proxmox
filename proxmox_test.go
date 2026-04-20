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

func TestFilterIPs_NoCIDRsNoExcludeMeansAllowAll(t *testing.T) {
	in := []net.IP{net.ParseIP("192.168.1.22"), net.ParseIP("10.0.0.5")}
	got := filterIPs(in, nil, nil)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("want passthrough, got %v", got)
	}
}

func TestFilterIPs_AllowCIDRFiltersOut(t *testing.T) {
	allow := []netip.Prefix{mustPrefix("192.168.1.0/24")}
	in := []net.IP{
		net.ParseIP("192.168.1.22"),    // LAN, keep
		net.ParseIP("10.0.0.5"),        // drop (docker bridge)
		net.ParseIP("100.85.224.16"),   // drop (netbird mesh)
	}
	got := filterIPs(in, allow, nil)
	want := []net.IP{net.ParseIP("192.168.1.22")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestFilterIPs_ExcludeDropsSpecificIPs(t *testing.T) {
	allow := []netip.Prefix{mustPrefix("192.168.1.0/24")}
	exclude := []netip.Addr{
		netip.MustParseAddr("192.168.1.22"),
		netip.MustParseAddr("192.168.1.67"),
	}
	in := []net.IP{
		net.ParseIP("192.168.1.22"), // exclude
		net.ParseIP("192.168.1.41"), // keep
		net.ParseIP("192.168.1.67"), // exclude
	}
	got := filterIPs(in, allow, exclude)
	want := []net.IP{net.ParseIP("192.168.1.41")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestFilterIPs_ExcludeWorksWithoutAllow(t *testing.T) {
	exclude := []netip.Addr{netip.MustParseAddr("10.0.0.5")}
	in := []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("10.0.0.6")}
	got := filterIPs(in, nil, exclude)
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

func TestStore_UpsertLookupDelete(t *testing.T) {
	s := newStore()
	g1 := guestID{Node: "pve", Type: "qemu", VMID: 102}
	g2 := guestID{Node: "pve", Type: "lxc", VMID: 312}
	s.Upsert(g1, "app01.hb.lan.", []net.IP{net.ParseIP("192.168.1.41")})
	s.Upsert(g2, "dns.hb.lan.", []net.IP{net.ParseIP("192.168.1.22")})

	if got := s.Lookup("app01.hb.lan."); len(got) != 1 || got[0].String() != "192.168.1.41" {
		t.Errorf("app01 lookup: want [.41], got %v", got)
	}
	if got := s.Lookup("dns.hb.lan."); len(got) != 1 || got[0].String() != "192.168.1.22" {
		t.Errorf("dns lookup: want [.22], got %v", got)
	}
	if got := s.Lookup("nothere.hb.lan."); got != nil {
		t.Errorf("unknown: want nil, got %v", got)
	}

	s.Delete(g1)
	if got := s.Lookup("app01.hb.lan."); got != nil {
		t.Errorf("after delete: want nil, got %v", got)
	}
	if s.Size() != 1 {
		t.Errorf("size after delete: want 1, got %d", s.Size())
	}
}

func TestStore_MultipleGuestsSameFQDN(t *testing.T) {
	// Unlikely but possible: two PVE guests with the same name.
	// Lookup should return union of their IPs.
	s := newStore()
	s.Upsert(guestID{Node: "pve", Type: "qemu", VMID: 100}, "clone.hb.lan.", []net.IP{net.ParseIP("192.168.1.100")})
	s.Upsert(guestID{Node: "pve", Type: "qemu", VMID: 101}, "clone.hb.lan.", []net.IP{net.ParseIP("192.168.1.101")})

	got := s.Lookup("clone.hb.lan.")
	if len(got) != 2 {
		t.Fatalf("want 2 IPs, got %v", got)
	}
	// Order is map-iteration-order-dependent; check membership
	seen := map[string]bool{}
	for _, ip := range got {
		seen[ip.String()] = true
	}
	if !seen["192.168.1.100"] || !seen["192.168.1.101"] {
		t.Errorf("want both 100+101 present, got %v", got)
	}
}

func TestGuestMeta_FQDN(t *testing.T) {
	cases := []struct {
		name  string
		zones []string
		want  string
	}{
		{"App01", []string{"hb.lan."}, "app01.hb.lan."},
		{"dns", []string{"hb.lan."}, "dns.hb.lan."},
		{"host.already.fqdn", []string{"hb.lan."}, "host.hb.lan."},
		{"", []string{"hb.lan."}, ""},
		{"dns", nil, ""},
		{"dns", []string{}, ""},
	}
	for _, c := range cases {
		g := guestMeta{Name: c.name}
		got := g.fqdn(c.zones)
		if got != c.want {
			t.Errorf("%q+%v: want %q, got %q", c.name, c.zones, c.want, got)
		}
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
