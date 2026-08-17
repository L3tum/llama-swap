package ring

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuffer_EmptySliceIsNil(t *testing.T) {
	b := NewBuffer[int](4)
	assert.Nil(t, b.Slice())
}

func TestBuffer_PushBelowCapacity(t *testing.T) {
	b := NewBuffer[int](4)
	b.Push(1)
	b.Push(2)
	assert.Equal(t, []int{1, 2}, b.Slice())
}

func TestBuffer_PushAtCapacity(t *testing.T) {
	b := NewBuffer[int](3)
	b.Push(1)
	b.Push(2)
	b.Push(3)
	assert.Equal(t, []int{1, 2, 3}, b.Slice())
}

func TestBuffer_PushOverCapacityEvictsOldest(t *testing.T) {
	b := NewBuffer[int](3)
	b.Push(1)
	b.Push(2)
	b.Push(3)
	b.Push(4)
	assert.Equal(t, []int{2, 3, 4}, b.Slice())
}

func TestBuffer_CapacityOne(t *testing.T) {
	b := NewBuffer[int](1)
	b.Push(1)
	b.Push(2)
	assert.Equal(t, []int{2}, b.Slice())
}

func TestBuffer_ZeroCapacityDefaultsToOne(t *testing.T) {
	b := NewBuffer[int](0)
	b.Push(42)
	assert.Equal(t, []int{42}, b.Slice())
}

func TestBuffer_SliceReturnsCopy(t *testing.T) {
	b := NewBuffer[int](4)
	b.Push(10)
	s := b.Slice()
	s[0] = 99
	assert.Equal(t, []int{10}, b.Slice())
}

func TestBuffer_InsertionOrderPreservedAfterWrap(t *testing.T) {
	b := NewBuffer[int](4)
	for i := 1; i <= 8; i++ {
		b.Push(i)
	}
	assert.Equal(t, []int{5, 6, 7, 8}, b.Slice())
}

func TestBuffer_LatestEmpty(t *testing.T) {
	b := NewBuffer[int](4)
	v, ok := b.Latest()
	assert.False(t, ok)
	assert.Zero(t, v)
}

func TestBuffer_LatestSingleElement(t *testing.T) {
	b := NewBuffer[int](4)
	b.Push(42)
	v, ok := b.Latest()
	assert.True(t, ok)
	assert.Equal(t, 42, v)
}

func TestBuffer_LatestMultipleElements(t *testing.T) {
	b := NewBuffer[int](4)
	b.Push(1)
	b.Push(2)
	b.Push(3)
	v, ok := b.Latest()
	assert.True(t, ok)
	assert.Equal(t, 3, v)
}

func TestBuffer_LatestAfterWrap(t *testing.T) {
	b := NewBuffer[int](3)
	for i := 1; i <= 6; i++ {
		b.Push(i)
	}
	// Buffer contains [4, 5, 6], latest should be 6
	v, ok := b.Latest()
	assert.True(t, ok)
	assert.Equal(t, 6, v)
}

func TestBuffer_LatestCapacityOne(t *testing.T) {
	b := NewBuffer[int](1)
	b.Push(1)
	b.Push(2)
	b.Push(3)
	v, ok := b.Latest()
	assert.True(t, ok)
	assert.Equal(t, 3, v)
}

func TestBuffer_LatestReturnsCopy(t *testing.T) {
	type val struct{ N int }
	b := NewBuffer[val](4)
	b.Push(val{N: 1})
	b.Push(val{N: 2})
	v, ok := b.Latest()
	assert.True(t, ok)
	v.N = 99
	v2, _ := b.Latest()
	assert.Equal(t, 2, v2.N) // original unchanged
}
