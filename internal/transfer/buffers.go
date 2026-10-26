package transfer

import (
	"sync"
	"sync/atomic"
)

const (
	// BufferBudget bounds the read buffers of all download streams together,
	// whatever the number of tasks; streams beyond it read with SmallBuffer.
	BufferBudget = 32 << 20
	SmallBuffer  = 8 << 10
)

var (
	buffersInUse atomic.Int64
	bufferPools  sync.Map // size -> *sync.Pool of *[]byte
)

func bufferPool(size int) *sync.Pool {
	if pool, ok := bufferPools.Load(size); ok {
		return pool.(*sync.Pool)
	}
	pool, _ := bufferPools.LoadOrStore(size, &sync.Pool{New: func() any {
		buf := make([]byte, size)
		return &buf
	}})
	return pool.(*sync.Pool)
}

// GetBuffer returns a read buffer of the preferred size while the shared
// budget allows it, and a small buffer otherwise. Return it with PutBuffer.
func GetBuffer(preferred int) []byte {
	if preferred > SmallBuffer {
		if buffersInUse.Add(int64(preferred)) <= BufferBudget {
			return *bufferPool(preferred).Get().(*[]byte)
		}
		buffersInUse.Add(-int64(preferred))
	}
	return make([]byte, SmallBuffer)
}

// PutBuffer returns a buffer from GetBuffer. The caller must not use it again.
func PutBuffer(buf []byte) {
	if size := cap(buf); size > SmallBuffer {
		buffersInUse.Add(-int64(size))
		buf = buf[:size]
		bufferPool(size).Put(&buf)
	}
}

// BuffersInUse reports the bytes of budgeted read buffers currently held.
func BuffersInUse() int64 {
	return buffersInUse.Load()
}
