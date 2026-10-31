package proxy

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"time"
)

const nativeDatagramLimit = 65507
const nativeDatagramIdle = 60 * time.Second

// Native flows use a separate capability. The ordinary worker capability must
// never gain access to destinations outside its existing Codex allowlist.
func NewNativeEgressBroker(dialer EgressDialer, token string) http.Handler {
	selectNetwork := func(r *http.Request) (string, bool) {
		if len(r.Header.Values("X-CQ-Native-Flow")) != 1 || r.Header.Get("X-CQ-Native-Flow") != "1" || len(r.Header.Values("X-CQ-Native-Network")) != 1 {
			return "", false
		}
		network := r.Header.Get("X-CQ-Native-Network")
		return network, network == "tcp" || network == "udp"
	}
	forward := newEgressBroker(dialer, token, nativeEgressTarget, selectNetwork)
	expected := sha256.Sum256([]byte("Bearer " + token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI != "cq-native-probe:0" && len(r.Header.Values("X-CQ-Native-Probe")) == 0 {
			forward.ServeHTTP(w, r)
			return
		}
		presented := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if !validEgressBrokerToken(token) || len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare(expected[:], presented[:]) != 1 {
			http.Error(w, "egress broker authentication failed", http.StatusUnauthorized)
			return
		}
		network, valid := selectNetwork(r)
		if r.Method != http.MethodConnect || r.RequestURI != "cq-native-probe:0" || r.Host != r.RequestURI ||
			len(r.Header.Values("X-CQ-Native-Probe")) != 1 || r.Header.Get("X-CQ-Native-Probe") != "1" ||
			!valid || network != "tcp" || r.ContentLength > 0 || len(r.TransferEncoding) != 0 {
			http.Error(w, "invalid native probe", http.StatusBadRequest)
			return
		}
		ready, ok := dialer.(interface{ NativeReady() bool })
		if !ok || !ready.NativeReady() {
			http.Error(w, "native egress unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func nativeEgressTarget(target string) bool {
	if !canonicalEgressBrokerTarget(target) {
		return false
	}
	host, _, _ := net.SplitHostPort(target)
	if ip, err := netip.ParseAddr(host); err == nil {
		return isNativePublicAddress(ip)
	}
	return host != "localhost" && len(host) > 0
}

// nativeDatagramStream translates bounded datagrams to the stream broker.
// Each direction owns its buffers; one reader and writer may run concurrently.
type nativeDatagramStream struct {
	net.Conn
	readFrame []byte
	readAt    int
	header    [2]byte
	headerAt  int
	writeData []byte
	writeAt   int
}

func newNativeDatagramStream(conn net.Conn) *nativeDatagramStream {
	return &nativeDatagramStream{Conn: conn, readFrame: make([]byte, nativeDatagramLimit+3)}
}

func (c *nativeDatagramStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.readAt == 0 {
		if err := c.SetDeadline(time.Now().Add(nativeDatagramIdle)); err != nil {
			return 0, err
		}
		n, err := c.Conn.Read(c.readFrame[2:])
		if err != nil {
			return 0, err
		}
		if n > nativeDatagramLimit {
			return 0, errors.New("native datagram exceeds limit")
		}
		binary.BigEndian.PutUint16(c.readFrame[:2], uint16(n))
		c.readFrame = c.readFrame[:n+2]
	}
	n := copy(p, c.readFrame[c.readAt:])
	c.readAt += n
	if c.readAt == len(c.readFrame) {
		c.readAt = 0
		c.readFrame = c.readFrame[:cap(c.readFrame)]
	}
	return n, nil
}

func (c *nativeDatagramStream) Write(p []byte) (int, error) {
	used := 0
	for len(p) > 0 {
		if c.headerAt < 2 {
			n := copy(c.header[c.headerAt:], p)
			c.headerAt += n
			used, p = used+n, p[n:]
			if c.headerAt < 2 {
				continue
			}
			size := int(binary.BigEndian.Uint16(c.header[:]))
			if size > nativeDatagramLimit {
				return used, errors.New("native datagram exceeds limit")
			}
			c.writeData = make([]byte, size)
		}
		n := copy(c.writeData[c.writeAt:], p)
		c.writeAt += n
		used, p = used+n, p[n:]
		if c.writeAt != len(c.writeData) {
			continue
		}
		if err := c.SetDeadline(time.Now().Add(nativeDatagramIdle)); err != nil {
			return used, err
		}
		n, err := c.Conn.Write(c.writeData)
		if err != nil {
			return used, err
		}
		if n != len(c.writeData) {
			return used, io.ErrShortWrite
		}
		c.headerAt, c.writeAt, c.writeData = 0, 0, nil
	}
	return used, nil
}
