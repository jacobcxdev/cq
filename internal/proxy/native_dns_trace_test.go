package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNativeDNSTracePreservesPacketsAndExcludesContents(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	packet := append([]byte{0x12, 0x34, 0x81, 0x83, 0, 1, 0, 2, 0, 0, 0, 0}, []byte("private.example credential-secret")...)
	done := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- errors.New("packet echo panicked")
			}
		}()
		got := make([]byte, len(packet))
		_, err := io.ReadFull(server, got)
		if err == nil && string(got) != string(packet) {
			err = errors.New("packet changed")
		}
		if err == nil {
			_, err = server.Write(got)
		}
		done <- err
	}()
	var events []nativeDNSEvent
	dialer := nativeDNSTraceDialer{EgressDialer: egressBrokerTestDialer(func(context.Context, string, string) (net.Conn, error) { return client, nil }), emit: func(e nativeDNSEvent) { events = append(events, e) }}
	conn, err := dialer.DialContext(context.Background(), "udp", "[2001:4860:4860::8888]:53")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := conn.Write(packet); n != len(packet) || err != nil {
		t.Fatalf("write = %d, %v", n, err)
	}
	got := make([]byte, len(packet))
	if n, err := conn.Read(got); n != len(packet) || err != nil || string(got) != string(packet) {
		t.Fatalf("read = %d, %v", n, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("events = %d", len(events))
	}
	for i, stage := range []string{"dial", "send", "receive", "close"} {
		e := events[i]
		if e.Stage != stage || e.Flow == "" || e.Flow != events[0].Flow || e.Failed {
			t.Fatalf("event = %+v", e)
		}
		if i == 1 || i == 2 {
			if !e.Packet || e.ID != 0x1234 || !e.Response || e.RCode != 3 || e.Questions != 1 || e.Answers != 2 {
				t.Fatalf("header = %+v", e)
			}
		}
	}
	data, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private.example", "credential-secret"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("DNS contents leaked")
		}
	}
}

func TestNativeDNSTraceBypassesOtherTraffic(t *testing.T) {
	for _, tc := range []struct{ network, target string }{{"tcp", "8.8.8.8:443"}, {"udp", "8.8.8.8:443"}, {"udp", "dns.example:53"}} {
		t.Run(tc.network+tc.target, func(t *testing.T) {
			want := errors.New("unchanged error")
			d := nativeDNSTraceDialer{EgressDialer: egressBrokerTestDialer(func(_ context.Context, network, target string) (net.Conn, error) {
				if network != tc.network || target != tc.target {
					t.Fatal("dial changed")
				}
				return nil, want
			}), emit: func(nativeDNSEvent) { t.Fatal("unrelated traffic traced") }}
			if conn, err := d.DialContext(context.Background(), tc.network, tc.target); conn != nil || err != want {
				t.Fatalf("dial = %v, %v", conn, err)
			}
		})
	}
}

func TestNativeDNSTracePreservesDialError(t *testing.T) {
	want := errors.New("secret in upstream error")
	var events []nativeDNSEvent
	d := nativeDNSTraceDialer{EgressDialer: egressBrokerTestDialer(func(context.Context, string, string) (net.Conn, error) { return nil, want }), emit: func(e nativeDNSEvent) { events = append(events, e) }}
	if conn, err := d.DialContext(context.Background(), "udp", "8.8.8.8:53"); conn != nil || err != want {
		t.Fatalf("dial = %v, %v", conn, err)
	}
	if len(events) != 1 || !events[0].Failed || events[0].Packet {
		t.Fatalf("events = %+v", events)
	}
	data, _ := json.Marshal(events)
	if strings.Contains(string(data), want.Error()) {
		t.Fatal("error contents leaked")
	}
}

func TestNativeDNSTraceBoundsConcurrentEvents(t *testing.T) {
	var events []nativeDNSEvent
	c := &nativeDNSConn{window: time.Now(), emit: func(e nativeDNSEvent) { events = append(events, e) }}
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() { defer wg.Done(); c.record("send", []byte{1}, nil) }()
	}
	wg.Wait()
	if len(events) != 65 || events[64].Stage != "rate_limited" {
		t.Fatalf("events = %d", len(events))
	}
	for _, e := range events {
		if e.Packet {
			t.Fatal("short packet parsed as DNS")
		}
	}
	c.window = time.Now().Add(-time.Minute)
	c.record("receive", nil, io.EOF)
	if len(events) != 66 || events[65].Stage != "receive" || !events[65].Failed {
		t.Fatal("rate window did not reset")
	}
}
