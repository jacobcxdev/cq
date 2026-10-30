package proxy

import (
	"encoding/base64"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

func wgTestKey(n byte) string {
	var b [32]byte
	for i := range b {
		b[i] = n
	}
	return base64.StdEncoding.EncodeToString(b[:])
}
func wgTestProfile() string {
	return "[Interface]\nPrivateKey = " + wgTestKey(1) + "\nAddress = 10.0.0.2/32\nDNS = 10.0.0.1\n[Peer]\nPublicKey = " + wgTestKey(2) + "\nEndpoint = vpn.example.com:51820\nAllowedIPs = 10.0.0.0/24\n"
}

func TestWireGuardProfileValid(t *testing.T) {
	p, err := parseWireGuardProfile([]byte(wgTestProfile()))
	if err != nil {
		t.Fatal(err)
	}
	if p.mtu != 1420 || len(p.addresses) != 1 || p.peers[0].endpoint != "vpn.example.com:51820" {
		t.Fatal("incorrect parsed metadata")
	}
	text := p.ipcConfig([]netip.AddrPort{netip.MustParseAddrPort("192.0.2.1:51820")})
	if !strings.Contains(text, "replace_peers=true\npublic_key=") || !strings.Contains(text, "endpoint=192.0.2.1:51820\n") || !strings.Contains(text, "allowed_ip=10.0.0.0/24\n") {
		t.Fatal("incorrect IPC metadata")
	}
	if strings.Contains(text, "preshared_key=") || strings.Contains(text, "persistent_keepalive_interval=") {
		t.Fatal("unexpected optional IPC fields")
	}
	for _, s := range []string{fmt.Sprint(p), fmt.Sprintf("%#v", p), fmt.Sprint(*p), fmt.Sprintf("%#v", *p)} {
		if s != "wireGuardProfile{redacted}" {
			t.Fatal("profile formatting not redacted")
		}
	}
}

func TestWireGuardProfileRejects(t *testing.T) {
	base := wgTestProfile()
	cases := map[string]string{
		"empty": "", "oversize": strings.Repeat("#", 65537), "unknown": base + "PostUp = secret-sentinel\n", "hook": strings.Replace(base, "DNS =", "PostUp = secret-sentinel\nDNS =", 1),
		"duplicate section": base + "[Peer]\n", "duplicate scalar": base + "Endpoint = other:123\n", "bad section": strings.Replace(base, "[Peer]", "[Peer] suffix", 1),
		"bad key": strings.Replace(base, wgTestKey(1), "secret-sentinel", 1), "zero key": strings.Replace(base, wgTestKey(1), base64.StdEncoding.EncodeToString(make([]byte, 32)), 1),
		"dns domain": strings.Replace(base, "10.0.0.1\n", "secret-sentinel\n", 1), "dns route": strings.Replace(base, "10.0.0.0/24", "10.0.0.2/32", 1),
		"dns family": strings.Replace(base, "10.0.0.1\n", "::1\n", 1), "empty list": strings.Replace(base, "10.0.0.0/24", "10.0.0.0/24,", 1),
		"mtu": strings.Replace(base, "DNS =", "MTU = 1279\nDNS =", 1), "keepalive": base + "PersistentKeepalive = 65536\n", "missing": strings.Replace(base, "DNS = 10.0.0.1\n", "", 1),
	}
	for _, ep := range []string{"https://secret-sentinel:80", "a:0", "a:65536", "a b:123", "[::1%lo0]:123", "-bad:123", "a:123=foo", "::1:123"} {
		cases["endpoint "+ep] = strings.Replace(base, "vpn.example.com:51820", ep, 1)
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := parseWireGuardProfile([]byte(s))
			if err == nil || p != nil {
				t.Fatal("invalid profile accepted")
			}
			if strings.Contains(err.Error(), "secret-sentinel") || strings.Contains(err.Error(), wgTestKey(1)) || strings.Contains(err.Error(), wgTestKey(2)) {
				t.Fatal("error leaked profile data")
			}
		})
	}
}

func TestWireGuardProfileIPv6AndRepeatedLists(t *testing.T) {
	s := strings.Replace(wgTestProfile(), "Address = 10.0.0.2/32", "Address = 10.0.0.2/32, fd00::2/128\nMTU = 1280", 1)
	s = strings.Replace(s, "DNS = 10.0.0.1", "DNS = 10.0.0.1 ; comment\nDNS = fd00::1 # comment", 1)
	s += "AllowedIPs = fd00::/64\nPresharedKey = " + wgTestKey(3) + "\nPersistentKeepalive = 65535\n"
	p, err := parseWireGuardProfile([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.dnsServers) != 2 || len(p.peers[0].allowedIPs) != 2 || len(p.addresses) != 2 {
		t.Fatal("repeated lists not retained")
	}
	text := p.ipcConfig([]netip.AddrPort{netip.MustParseAddrPort("[2001:db8::1]:123")})
	if !strings.Contains(text, "endpoint=[2001:db8::1]:123\n") || !strings.Contains(text, "persistent_keepalive_interval=65535\n") || !strings.Contains(text, "preshared_key=") {
		t.Fatal("missing optional IPC fields")
	}
}

func TestWireGuardProfileBoundaries(t *testing.T) {
	for _, mtu := range []string{"1280", "9000"} {
		s := strings.Replace(wgTestProfile(), "DNS =", "MTU = "+mtu+"\nDNS =", 1) + "PersistentKeepalive = 0\n"
		if _, err := parseWireGuardProfile([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	for _, endpoint := range []string{"192.0.2.1:1", "[2001:db8::1]:65535", "vpn.example.com.:51820"} {
		s := strings.Replace(wgTestProfile(), "vpn.example.com:51820", endpoint, 1)
		if _, err := parseWireGuardProfile([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	s := wgTestProfile()
	s += strings.Repeat("#", 65536-len(s))
	if _, err := parseWireGuardProfile([]byte(s)); err != nil {
		t.Fatal(err)
	}
	for _, directive := range []string{"SaveConfig = true", "ListenPort = 51820", "PreUp = secret-sentinel", "Address = 10.0.0.2/32", "AllowedIPs = 10.0.0.0/24,invalid", "PersistentKeepalive = -1", "MTU = 9001", "Endpoint = vpn.example.com:123\x00"} {
		if p, err := parseWireGuardProfile([]byte(wgTestProfile() + directive + "\n")); p != nil || err == nil {
			t.Fatal("unsupported directive accepted")
		}
	}
	// Canonical base64 is required, including valid padding bits.
	bad := wgTestKey(1)
	bad = bad[:len(bad)-2] + "F="
	if _, err := parseWireGuardProfile([]byte(strings.Replace(wgTestProfile(), wgTestKey(1), bad, 1))); err == nil {
		t.Fatal("noncanonical key accepted")
	}
}

func TestWireGuardProfileMultiplePeers(t *testing.T) {
	second := "[Peer]\nPublicKey = " + wgTestKey(4) + "\nEndpoint = second.example:123\nAllowedIPs = 10.0.0.128/25\nPersistentKeepalive = 7\n"
	p, err := parseWireGuardProfile([]byte(wgTestProfile() + second))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.peers) != 2 || p.peers[1].keepalive != 7 {
		t.Fatal("peer settings not retained")
	}
	if fmt.Sprint(p.peers[1]) != "wireGuardPeer{redacted}" || fmt.Sprintf("%#v", p.peers[1]) != "wireGuardPeer{redacted}" {
		t.Fatal("peer formatting not redacted")
	}
	config := p.ipcConfig([]netip.AddrPort{netip.MustParseAddrPort("192.0.2.1:1"), netip.MustParseAddrPort("192.0.2.2:2")})
	if strings.Count(config, "replace_peers=true") != 1 || strings.Count(config, "public_key=") != 2 || !strings.Contains(config, "endpoint=192.0.2.2:2\nallowed_ip=10.0.0.128/25\n") {
		t.Fatal("incorrect multiple-peer IPC")
	}
	for _, bad := range []string{strings.Replace(second, wgTestKey(4), wgTestKey(2), 1), strings.Replace(second, "10.0.0.128/25", "10.0.0.0/24", 1), second + "Endpoint = third.example:123\n", "[Peer]\n"} {
		if _, err := parseWireGuardProfile([]byte(wgTestProfile() + bad)); err == nil {
			t.Fatal("ambiguous or incomplete peer accepted")
		}
	}
	union := strings.Replace(wgTestProfile(), "DNS = 10.0.0.1", "DNS = 10.0.0.129", 1)
	union = strings.Replace(union, "10.0.0.0/24", "10.0.0.0/25", 1)
	if _, err := parseWireGuardProfile([]byte(union + second)); err != nil {
		t.Fatal(err)
	}
}
