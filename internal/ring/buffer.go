package ring

type Buffer[T any] struct {
	buf  []T
	head int
	size int
}

func NewBuffer[T any](capacity int) Buffer[T] {
	if capacity < 1 {
		capacity = 1
	}
	return Buffer[T]{buf: make([]T, capacity)}
}

// Push adds v, overwriting the oldest entry when the buffer is full.
func (r *Buffer[T]) Push(v T) {
	cap := len(r.buf)
	if r.size < cap {
		r.buf[(r.head+r.size)%cap] = v
		r.size++
	} else {
		r.buf[r.head] = v
		r.head = (r.head + 1) % cap
	}
}

// Len returns the number of entries currently in the buffer.
func (r *Buffer[T]) Len() int {
	return r.size
}

// Cap returns the maximum capacity of the buffer.
func (r *Buffer[T]) Cap() int {
	return len(r.buf)
}

// Slice returns all entries in insertion order as a new slice.
func (r *Buffer[T]) Slice() []T {
	if r.size == 0 {
		return nil
	}
	cap := len(r.buf)
	result := make([]T, r.size)
	for i := 0; i < r.size; i++ {
		result[i] = r.buf[(r.head+i)%cap]
	}
	return result
}

// Latest returns the most recently pushed entry, or the zero value if empty.
// Returns the value and true if the buffer is non-empty, or the zero value and false.
func (r *Buffer[T]) Latest() (T, bool) {
	if r.size == 0 {
		var zero T
		return zero, false
	}
	// The most recent entry is at position (head + size - 1) % cap.
	idx := (r.head + r.size - 1) % len(r.buf)
	return r.buf[idx], true
}
