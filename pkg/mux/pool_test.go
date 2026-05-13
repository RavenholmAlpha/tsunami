package mux

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

type testReadWriteCloser struct {
	bytes.Buffer
}

func (c *testReadWriteCloser) Close() error {
	return nil
}

func TestGetOrCreateSessionConcurrentFirstDial(t *testing.T) {
	var dials atomic.Int32
	pool := NewPool(DefaultPoolConfig(), func() (*protocol.Session, error) {
		seq := uint64(dials.Add(1))
		time.Sleep(20 * time.Millisecond)
		return protocol.NewSession(&testReadWriteCloser{}, seq), nil
	})
	defer pool.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := pool.GetOrCreateSession()
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("GetOrCreateSession returned error: %v", err)
		}
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dial count = %d, want 1", got)
	}
	if got := pool.SessionCount(); got != 1 {
		t.Fatalf("session count = %d, want 1", got)
	}
}
