package micom

import (
	"io"
	"sync"
)

// sharedPort reads one port for a succession of daemons: each gets a view,
// and closing a view stops that daemon without closing the port, so the
// next one takes over the link: nothing the last had not read is lost.
type sharedPort struct {
	port io.ReadWriteCloser
	ch   chan []byte
	mu   sync.Mutex
	err  error  // the port's read error, once it has failed
	left []byte // read from the port, not yet by a view
}

func newSharedPort(p io.ReadWriteCloser) *sharedPort {
	s := &sharedPort{port: p, ch: make(chan []byte)}
	go func() {
		for {
			buf := make([]byte, 1024)
			n, err := p.Read(buf)
			if n > 0 {
				s.ch <- buf[:n]
			}
			if err != nil {
				s.mu.Lock()
				s.err = err
				s.mu.Unlock()
				close(s.ch)
				return
			}
		}
	}()
	return s
}

// view is the port for one daemon.
func (s *sharedPort) view() io.ReadWriteCloser {
	return &portView{s: s, closed: make(chan struct{})}
}

type portView struct {
	s      *sharedPort
	closed chan struct{}
	once   sync.Once
}

func (v *portView) Read(p []byte) (int, error) {
	s := v.s
	s.mu.Lock()
	if len(s.left) > 0 {
		n := copy(p, s.left)
		s.left = s.left[n:]
		s.mu.Unlock()
		return n, nil
	}
	s.mu.Unlock()
	select {
	case <-v.closed:
		return 0, io.ErrClosedPipe
	case b, ok := <-s.ch:
		if !ok {
			s.mu.Lock()
			defer s.mu.Unlock()
			return 0, s.err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		select {
		case <-v.closed:
			// Raced the close: the next view gets it.
			s.left = append(b, s.left...)
			return 0, io.ErrClosedPipe
		default:
		}
		n := copy(p, b)
		s.left = append(s.left, b[n:]...)
		return n, nil
	}
}

func (v *portView) Write(p []byte) (int, error) {
	select {
	case <-v.closed:
		return 0, io.ErrClosedPipe
	default:
	}
	return v.s.port.Write(p)
}

func (v *portView) Close() error {
	v.once.Do(func() { close(v.closed) })
	return nil
}
