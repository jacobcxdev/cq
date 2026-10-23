package dots

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// socketKeepalive matches websockets' 20-second ping and pong liveness without
// treating an otherwise healthy idle connection as expired. WriteControl may
// run concurrently with the transport's single application writer.
func socketKeepalive(ctx context.Context, conn *websocket.Conn, onFailure func()) {
	socketKeepaliveAt(ctx, conn, onFailure, 20*time.Second, 20*time.Second)
}

func socketKeepaliveAt(ctx context.Context, conn *websocket.Conn, onFailure func(), interval, timeout time.Duration) {
	var lock sync.Mutex
	var expected string
	var pending chan struct{}
	conn.SetPongHandler(func(value string) error {
		lock.Lock()
		defer lock.Unlock()
		if pending != nil && value == expected {
			close(pending)
			pending = nil
		}
		return nil
	})
	go func() {
		defer func() {
			if recover() != nil {
				onFailure()
			}
		}()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				payload := strconv.FormatInt(now.UnixNano(), 10)
				pong := make(chan struct{})
				lock.Lock()
				expected, pending = payload, pong
				lock.Unlock()
				if err := conn.WriteControl(websocket.PingMessage, []byte(payload), time.Now().Add(timeout)); err != nil {
					onFailure()
					return
				}
				timer := time.NewTimer(timeout)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-pong:
				case <-timer.C:
					onFailure()
					return
				}
				timer.Stop()
			}
		}
	}()
}
