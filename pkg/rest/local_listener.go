package rest

import (
	"errors"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// A cache cleaner can unlink a live Unix socket without stopping its server.
// Replace only the listening socket; accepted connections and downloader state
// remain in place. Close stops recovery before releasing the current socket.
type recoveringUnixListener struct {
	mu       sync.Mutex
	listener net.Listener
	address  string
	closed   bool
	done     chan struct{}
}

func listenLocal(network, address string) (net.Listener, error) {
	listener, err := net.Listen(network, address)
	// Abstract Unix sockets have no filesystem entry to watch.
	abstract := runtime.GOOS == "linux" && (strings.HasPrefix(address, "@") || strings.HasPrefix(address, "\x00"))
	if err != nil || network != "unix" || abstract {
		return listener, err
	}
	disableUnixUnlink(listener)
	l := &recoveringUnixListener{listener: listener, address: address, done: make(chan struct{})}
	go l.watch()
	return l, nil
}

func disableUnixUnlink(listener net.Listener) {
	// Closing an old listener must never unlink its replacement.
	if unix, ok := listener.(interface{ SetUnlinkOnClose(bool) }); ok {
		unix.SetUnlinkOnClose(false)
	}
}

func (l *recoveringUnixListener) watch() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-ticker.C:
			l.mu.Lock()
			if l.closed {
				l.mu.Unlock()
				return
			}
			_, err := os.Lstat(l.address)
			if !os.IsNotExist(err) {
				l.mu.Unlock()
				continue
			}
			next, err := net.Listen("unix", l.address)
			if err != nil {
				l.mu.Unlock()
				continue
			}
			disableUnixUnlink(next)
			old := l.listener
			l.listener = next
			l.mu.Unlock()
			old.Close()
		}
	}
}

func (l *recoveringUnixListener) Accept() (net.Conn, error) {
	for {
		l.mu.Lock()
		current, closed := l.listener, l.closed
		l.mu.Unlock()
		if closed {
			return nil, net.ErrClosed
		}
		conn, err := current.Accept()
		if err == nil {
			return conn, nil
		}
		l.mu.Lock()
		replaced := !l.closed && current != l.listener
		l.mu.Unlock()
		if replaced && errors.Is(err, net.ErrClosed) {
			continue
		}
		return nil, err
	}
}

func (l *recoveringUnixListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return net.ErrClosed
	}
	l.closed = true
	close(l.done)
	// Filesystem identity cannot prove which listener owns a replaced path.
	// Leave the socket entry for the existing startup cleanup rather than risk
	// unlinking another listener. Closing must never resurrect a removed entry.
	return l.listener.Close()
}

func (l *recoveringUnixListener) Addr() net.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.listener.Addr()
}
