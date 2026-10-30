package proxy

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// wireGuardProfile is the supported declarative wg-quick subset
// supported by the userspace egress transport. Never format its key fields.
type wireGuardProfile struct {
	addresses  []netip.Addr
	dnsServers []netip.Addr
	mtu        int
	privateKey [32]byte
	peers      []wireGuardPeer
}

type wireGuardPeer struct {
	publicKey    [32]byte
	presharedKey [32]byte
	endpoint     string
	allowedIPs   []netip.Prefix
	keepalive    int
}

func (wireGuardProfile) String() string   { return "wireGuardProfile{redacted}" }
func (wireGuardProfile) GoString() string { return "wireGuardProfile{redacted}" }
func (wireGuardPeer) String() string      { return "wireGuardPeer{redacted}" }
func (wireGuardPeer) GoString() string    { return "wireGuardPeer{redacted}" }

func parseWireGuardProfile(data []byte) (*wireGuardProfile, error) {
	if len(data) > 64*1024 {
		return nil, errors.New("WireGuard profile exceeds size limit")
	}
	p := &wireGuardProfile{mtu: 1420}
	interfaceSeen := false
	peerSeen := []map[string]bool{}
	seen := map[string]bool{}
	section := ""
	for i, line := range strings.Split(string(data), "\n") {
		fail := func() (*wireGuardProfile, error) { return nil, fmt.Errorf("invalid WireGuard profile at line %d", i+1) }
		// CRLF is accepted, but embedded control characters are never directives.
		line = strings.TrimSuffix(line, "\r")
		for _, c := range line {
			if (c < 32 && c != '\t') || c == 127 {
				return fail()
			}
		}
		if j := strings.IndexAny(line, "#;"); j >= 0 {
			line = line[:j]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if line != "[Interface]" && line != "[Peer]" {
				return fail()
			}
			section = line[1 : len(line)-1]
			if section == "Interface" {
				if interfaceSeen {
					return fail()
				}
				interfaceSeen = true
			} else {
				p.peers = append(p.peers, wireGuardPeer{})
				peerSeen = append(peerSeen, map[string]bool{})
			}
			continue
		}
		field, value, ok := strings.Cut(line, "=")
		field = strings.TrimSpace(field)
		value = strings.TrimSpace(value)
		if !ok || section == "" || value == "" {
			return fail()
		}
		list := field == "Address" || field == "DNS" || field == "AllowedIPs"
		id := section + "/" + field
		fields := seen
		var peer *wireGuardPeer
		if section == "Peer" {
			fields = peerSeen[len(peerSeen)-1]
			peer = &p.peers[len(p.peers)-1]
		}
		if fields[id] && !list {
			return fail()
		}
		fields[id] = true
		valid := true
		switch section + "/" + field {
		case "Interface/PrivateKey":
			p.privateKey, valid = parseWireGuardKey(value)
		case "Peer/PublicKey":
			peer.publicKey, valid = parseWireGuardKey(value)
		case "Peer/PresharedKey":
			peer.presharedKey, valid = parseWireGuardKey(value)
		case "Interface/Address":
			for _, v := range strings.Split(value, ",") {
				a, e := netip.ParsePrefix(strings.TrimSpace(v))
				if e != nil || a.Addr().Is4In6() {
					valid = false
					break
				}
				p.addresses = append(p.addresses, a.Addr())
			}
		case "Interface/DNS":
			for _, v := range strings.Split(value, ",") {
				a, e := netip.ParseAddr(strings.TrimSpace(v))
				if e != nil || a.Zone() != "" || a.Is4In6() {
					valid = false
					break
				}
				p.dnsServers = append(p.dnsServers, a)
			}
		case "Peer/AllowedIPs":
			for _, v := range strings.Split(value, ",") {
				a, e := netip.ParsePrefix(strings.TrimSpace(v))
				if e != nil || a.Addr().Is4In6() {
					valid = false
					break
				}
				peer.allowedIPs = append(peer.allowedIPs, a.Masked())
			}
		case "Interface/MTU":
			p.mtu, valid = parseWireGuardNumber(value, 1280, 9000)
		case "Peer/PersistentKeepalive":
			peer.keepalive, valid = parseWireGuardNumber(value, 0, 65535)
		case "Peer/Endpoint":
			valid = validWireGuardEndpoint(value)
			peer.endpoint = value
		default:
			valid = false
		}
		if !valid {
			return fail()
		}
	}
	for _, required := range []string{"Interface/PrivateKey", "Interface/Address", "Interface/DNS"} {
		if !seen[required] {
			return nil, errors.New("WireGuard profile is missing required configuration")
		}
	}
	if len(p.peers) == 0 {
		return nil, errors.New("WireGuard profile requires at least one peer")
	}
	keys := map[[32]byte]bool{}
	routes := map[netip.Prefix]int{}
	for i, peer := range p.peers {
		for _, required := range []string{"Peer/PublicKey", "Peer/Endpoint", "Peer/AllowedIPs"} {
			if !peerSeen[i][required] {
				return nil, errors.New("WireGuard profile is missing required peer configuration")
			}
		}
		if keys[peer.publicKey] {
			return nil, errors.New("WireGuard profile has duplicate peer keys")
		}
		keys[peer.publicKey] = true
		for _, route := range peer.allowedIPs {
			if owner, ok := routes[route]; ok && owner != i {
				return nil, errors.New("WireGuard profile has ambiguous peer routes")
			}
			routes[route] = i
		}
	}
	for _, dns := range p.dnsServers {
		routed, local := false, false
		for _, peer := range p.peers {
			for _, route := range peer.allowedIPs {
				routed = routed || route.Contains(dns)
			}
		}
		for _, addr := range p.addresses {
			local = local || addr.Is4() == dns.Is4()
		}
		if !routed || !local {
			return nil, errors.New("WireGuard DNS requires a matching local address family and allowed route")
		}
	}
	return p, nil
}

func parseWireGuardKey(value string) ([32]byte, bool) {
	var key [32]byte
	b, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(b) != 32 || base64.StdEncoding.EncodeToString(b) != value {
		return key, false
	}
	copy(key[:], b)
	return key, key != [32]byte{}
}

func parseWireGuardNumber(s string, low, high int) (int, bool) {
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, e := strconv.Atoi(s)
	return n, e == nil && n >= low && n <= high
}

func validWireGuardEndpoint(value string) bool {
	host, port, err := net.SplitHostPort(value)
	if err != nil || host == "" {
		return false
	}
	if _, ok := parseWireGuardNumber(port, 1, 65535); !ok {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Zone() == "" && !ip.Is4In6()
	}
	// Brackets are reserved for numeric IPv6, never DNS names.
	if strings.HasPrefix(value, "[") || len(host) > 253 {
		return false
	}
	host = strings.TrimSuffix(host, ".")
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// ipcConfig contains secret material and must only be passed directly to IpcSet.
// The caller resolves the endpoint, keeping arbitrary profile text out of IPC.
func (p *wireGuardProfile) ipcConfig(endpoints []netip.AddrPort) string {
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\nreplace_peers=true\n", hex.EncodeToString(p.privateKey[:]))
	for i, peer := range p.peers {
		fmt.Fprintf(&b, "public_key=%s\n", hex.EncodeToString(peer.publicKey[:]))
		if peer.presharedKey != [32]byte{} {
			fmt.Fprintf(&b, "preshared_key=%s\n", hex.EncodeToString(peer.presharedKey[:]))
		}
		fmt.Fprintf(&b, "endpoint=%s\n", endpoints[i])
		for _, prefix := range peer.allowedIPs {
			fmt.Fprintf(&b, "allowed_ip=%s\n", prefix)
		}
		if peer.keepalive > 0 {
			fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", peer.keepalive)
		}
	}
	return b.String()
}
