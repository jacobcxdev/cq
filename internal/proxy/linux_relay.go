//go:build linux

package proxy

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type linuxRelayID uint8

const (
	linuxRelayProxy  linuxRelayID = 1
	linuxRelayEgress linuxRelayID = 2

	linuxRelayMessageReady   uint8 = 1
	linuxRelayMessageOpen    uint8 = 2
	linuxRelayMessageSize          = 16
	linuxRelayMaxConnections       = 64
	linuxRelayByteLimit            = 16 << 20
	linuxRelayIdleTimeout          = 25 * time.Second
)

type linuxRelayDefinition struct {
	ID     linuxRelayID `json:"id"`
	Port   int          `json:"port"`
	Target string       `json:"target"`
}

type linuxRelayMessage struct {
	Kind     uint8
	RelayID  linuxRelayID
	Sequence uint64
}

func encodeLinuxRelayMessage(message linuxRelayMessage) []byte {
	encoded := make([]byte, linuxRelayMessageSize)
	copy(encoded[:4], "CQLR")
	encoded[4] = linuxNamespaceProtocolVersion
	encoded[5] = message.Kind
	encoded[6] = byte(message.RelayID)
	binary.BigEndian.PutUint64(encoded[8:], message.Sequence)
	return encoded
}

func decodeLinuxRelayMessage(encoded []byte) (linuxRelayMessage, error) {
	if len(encoded) != linuxRelayMessageSize || string(encoded[:4]) != "CQLR" || encoded[4] != linuxNamespaceProtocolVersion || encoded[7] != 0 {
		return linuxRelayMessage{}, errors.New("invalid Linux relay message")
	}
	message := linuxRelayMessage{Kind: encoded[5], RelayID: linuxRelayID(encoded[6]), Sequence: binary.BigEndian.Uint64(encoded[8:])}
	switch message.Kind {
	case linuxRelayMessageReady:
		if message.RelayID != 0 || message.Sequence != 1 {
			return linuxRelayMessage{}, errors.New("invalid Linux relay ready message")
		}
	case linuxRelayMessageOpen:
		if (message.RelayID != linuxRelayProxy && message.RelayID != linuxRelayEgress) || message.Sequence < 2 {
			return linuxRelayMessage{}, errors.New("invalid Linux relay open message")
		}
	default:
		return linuxRelayMessage{}, errors.New("unknown Linux relay message")
	}
	return message, nil
}

type linuxRelaySender struct {
	mu       sync.Mutex
	fd       int
	sequence uint64
}

func (sender *linuxRelaySender) ready() error {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	sender.sequence = 1
	return sendLinuxRelayPacket(sender.fd, linuxRelayMessage{Kind: linuxRelayMessageReady, Sequence: sender.sequence}, -1)
}

func (sender *linuxRelaySender) connection(id linuxRelayID, fd int) error {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	sender.sequence++
	return sendLinuxRelayPacket(sender.fd, linuxRelayMessage{Kind: linuxRelayMessageOpen, RelayID: id, Sequence: sender.sequence}, fd)
}

func sendLinuxRelayPacket(controlFD int, message linuxRelayMessage, passedFD int) error {
	var rights []byte
	if passedFD >= 0 {
		rights = unix.UnixRights(passedFD)
	}
	written, err := unix.SendmsgN(controlFD, encodeLinuxRelayMessage(message), rights, nil, unix.MSG_NOSIGNAL)
	if err != nil || written != linuxRelayMessageSize {
		return errors.New("send Linux relay message")
	}
	return nil
}

type linuxRelayBridges struct {
	mu          sync.Mutex
	connections map[net.Conn]struct{}
	closed      bool
	wait        sync.WaitGroup
}

func newLinuxRelayBridges() *linuxRelayBridges {
	return &linuxRelayBridges{connections: make(map[net.Conn]struct{})}
}

func (bridges *linuxRelayBridges) closeAndWait() {
	bridges.mu.Lock()
	bridges.closed = true
	for connection := range bridges.connections {
		_ = connection.Close()
	}
	bridges.mu.Unlock()
	bridges.wait.Wait()
}

func (bridges *linuxRelayBridges) isClosed() bool {
	bridges.mu.Lock()
	defer bridges.mu.Unlock()
	return bridges.closed
}

func (bridges *linuxRelayBridges) launch(fd int, address string, failures chan<- error) {
	bridges.mu.Lock()
	if bridges.closed {
		bridges.mu.Unlock()
		_ = unix.Close(fd)
		return
	}
	bridges.wait.Add(1)
	bridges.mu.Unlock()
	go func() {
		defer bridges.wait.Done()
		defer func() {
			if recover() != nil && !bridges.isClosed() {
				select {
				case failures <- errors.New("Linux relay panic"):
				default:
				}
			}
		}()
		if err := bridges.bridge(fd, address); err != nil && !bridges.isClosed() {
			select {
			case failures <- err:
			default:
			}
		}
	}()
}

func runLinuxRelayReceiver(controlFD int, definitions []linuxRelayDefinition, ready chan<- error, failures chan<- error, bridges *linuxRelayBridges, done chan<- struct{}) {
	defer close(done)
	defer func() {
		if recover() != nil {
			select {
			case failures <- errors.New("Linux relay panic"):
			default:
			}
		}
	}()
	targets := make(map[linuxRelayID]string, len(definitions))
	for _, definition := range definitions {
		targets[definition.ID] = definition.Target
	}
	expectedSequence := uint64(1)
	readySent := false
	connections := 0
	for {
		packet := make([]byte, linuxRelayMessageSize+1)
		oob := make([]byte, unix.CmsgSpace(4)+1)
		count, oobCount, flags, _, err := unix.Recvmsg(controlFD, packet, oob, 0)
		if err != nil {
			if !readySent {
				ready <- errors.New("Linux acceptance helper unavailable")
			}
			return
		}
		if count == 0 && oobCount == 0 {
			if !readySent {
				ready <- errors.New("Linux acceptance helper unavailable")
			}
			return
		}
		if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || count != linuxRelayMessageSize {
			failures <- errors.New("Linux relay protocol violation")
			return
		}
		message, err := decodeLinuxRelayMessage(packet[:count])
		if err != nil || message.Sequence != expectedSequence {
			failures <- errors.New("Linux relay protocol violation")
			return
		}
		expectedSequence++
		messages, err := unix.ParseSocketControlMessage(oob[:oobCount])
		if err != nil {
			failures <- errors.New("Linux relay protocol violation")
			return
		}
		if message.Kind == linuxRelayMessageReady {
			if readySent || len(messages) != 0 {
				failures <- errors.New("Linux relay protocol violation")
				return
			}
			readySent = true
			ready <- nil
			continue
		}
		if !readySent || len(messages) != 1 || connections >= linuxRelayMaxConnections {
			failures <- errors.New("Linux relay protocol violation")
			return
		}
		fds, err := unix.ParseUnixRights(&messages[0])
		if err != nil || len(fds) != 1 {
			for _, fd := range fds {
				_ = unix.Close(fd)
			}
			failures <- errors.New("Linux relay protocol violation")
			return
		}
		target, ok := targets[message.RelayID]
		if !ok {
			_ = unix.Close(fds[0])
			failures <- errors.New("Linux relay protocol violation")
			return
		}
		connections++
		bridges.launch(fds[0], target, failures)
	}
}

func (bridges *linuxRelayBridges) bridge(fd int, target string) error {
	file := os.NewFile(uintptr(fd), "linux-acceptance-relay")
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("open Linux relay connection")
	}
	inside, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		return errors.New("open Linux relay connection")
	}
	defer inside.Close()
	outbound, err := net.DialTimeout("tcp4", target, 3*time.Second)
	if err != nil {
		return errors.New("connect Linux relay target")
	}
	defer outbound.Close()
	bridges.mu.Lock()
	if bridges.closed {
		bridges.mu.Unlock()
		return nil
	}
	bridges.connections[inside] = struct{}{}
	bridges.connections[outbound] = struct{}{}
	bridges.mu.Unlock()
	defer func() {
		bridges.mu.Lock()
		delete(bridges.connections, inside)
		delete(bridges.connections, outbound)
		bridges.mu.Unlock()
	}()
	deadline := time.Now().Add(linuxRelayIdleTimeout)
	_ = inside.SetDeadline(deadline)
	_ = outbound.SetDeadline(deadline)
	results := make(chan error, 2)
	copyDirection := func(destination net.Conn, source net.Conn) {
		defer func() {
			if recover() != nil {
				results <- errors.New("Linux relay panic")
			}
		}()
		written, copyErr := io.Copy(destination, io.LimitReader(source, linuxRelayByteLimit+1))
		if written > linuxRelayByteLimit {
			copyErr = errors.New("Linux relay byte limit exceeded")
		}
		if tcp, ok := destination.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		results <- copyErr
	}
	go copyDirection(outbound, inside)
	go copyDirection(inside, outbound)
	first := <-results
	second := <-results
	for _, copyErr := range []error{first, second} {
		if copyErr != nil {
			if networkError, ok := copyErr.(net.Error); ok && networkError.Timeout() {
				continue
			}
			return errors.New("Linux relay transfer failed")
		}
	}
	return nil
}
