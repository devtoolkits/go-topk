// Package topk implements the Filtered Space-Saving TopK streaming algorithm
/*

The original Space-Saving algorithm:
https://icmi.cs.ucsb.edu/research/tech_reports/reports/2005-23.pdf

The Filtered Space-Saving enhancement:
http://www.l2f.inesc-id.pt/~fmmb/wiki/uploads/Work/misnis.ref0a.pdf

This implementation follows the algorithm of the FSS paper, but not the
suggested implementation.  Specifically, we use a heap instead of a sorted list
of monitored items, and since we are also using a map to provide O(1) access on
update also don't need the c_i counters in the hash table.

Licensed under the MIT license.

*/
package topk

import (
	"bytes"
	"container/heap"
	"encoding/gob"
	"sort"
	"sync"

	"github.com/dgryski/go-sip13"
)

// Element is a TopK item
type Element struct {
	Key   string
	Count int
	Error int
}

type elementsByCountDescending []Element

func (elts elementsByCountDescending) Len() int { return len(elts) }
func (elts elementsByCountDescending) Less(i, j int) bool {
	return (elts[i].Count > elts[j].Count) || (elts[i].Count == elts[j].Count && elts[i].Key < elts[j].Key)
}
func (elts elementsByCountDescending) Swap(i, j int) { elts[i], elts[j] = elts[j], elts[i] }

type keys struct {
	m    map[string]int
	elts []Element
	sync.RWMutex
}

// Implement the container/heap interface

func (tk *keys) Len() int {
	tk.RLock()
	defer tk.RUnlock()

	return len(tk.elts)
}

func (tk *keys) Less(i, j int) bool {
	tk.RLock()
	defer tk.RUnlock()

	return (tk.elts[i].Count < tk.elts[j].Count) || (tk.elts[i].Count == tk.elts[j].Count && tk.elts[i].Error > tk.elts[j].Error)
}
func (tk *keys) Swap(i, j int) {
	tk.Lock()
	tk.elts[i], tk.elts[j] = tk.elts[j], tk.elts[i]

	tk.m[tk.elts[i].Key] = i
	tk.m[tk.elts[j].Key] = j
	tk.Unlock()
}

func (tk *keys) Push(x interface{}) {
	e := x.(Element)
	tk.Lock()
	tk.m[e.Key] = len(tk.elts)
	tk.elts = append(tk.elts, e)
	tk.Unlock()
}

func (tk *keys) Pop() interface{} {
	var e Element
	tk.Lock()
	e, tk.elts = tk.elts[len(tk.elts)-1], tk.elts[:len(tk.elts)-1]

	delete(tk.m, e.Key)
	tk.Unlock()

	return e
}

// Stream calculates the TopK elements for a stream
type Stream struct {
	n      int
	k      keys
	alphas []int
}

// New returns a Stream estimating the top n most frequent elements
func New(n int) *Stream {
	return &Stream{
		n:      n,
		k:      keys{m: make(map[string]int), elts: make([]Element, 0, n)},
		alphas: make([]int, n*6), // 6 is the multiplicative constant from the paper
	}
}

func reduce(x uint64, n int) uint32 {
	return uint32(uint64(uint32(x)) * uint64(n) >> 32)
}

// Insert adds an element to the stream to be tracked
// It returns an estimation for the just inserted element
func (s *Stream) Insert(x string, count int) Element {

	xhash := reduce(sip13.Sum64Str(0, 0, x), len(s.alphas))

	// are we tracking this element?
	s.k.RLock()
	idx, ok := s.k.m[x]
	s.k.RUnlock()

	if ok {
		s.k.Lock()
		s.k.elts[idx].Count += count
		s.k.Unlock()
		s.k.RLock()
		e := s.k.elts[idx]
		s.k.RUnlock()
		heap.Fix(&s.k, idx)
		return e
	}

	// can we track more elements?
	s.k.RLock()
	eltsSize := len(s.k.elts)
	s.k.RUnlock()
	if eltsSize < s.n {
		// there is free space
		e := Element{Key: x, Count: count}
		heap.Push(&s.k, e)
		return e
	}

	s.k.RLock()
	elt := s.k.elts[0].Count
	s.k.RUnlock()
	if s.alphas[xhash]+count < elt {
		e := Element{
			Key:   x,
			Error: s.alphas[xhash],
			Count: s.alphas[xhash] + count,
		}
		s.alphas[xhash] += count
		return e
	}

	// replace the current minimum element
	s.k.RLock()
	minKey := s.k.elts[0].Key
	s.k.RUnlock()

	mkhash := reduce(sip13.Sum64Str(0, 0, minKey), len(s.alphas))
	s.k.RLock()
	s.alphas[mkhash] = s.k.elts[0].Count
	s.k.RUnlock()

	e := Element{
		Key:   x,
		Error: s.alphas[xhash],
		Count: s.alphas[xhash] + count,
	}
	s.k.Lock()
	s.k.elts[0] = e
	s.k.Unlock()

	// we're not longer monitoring minKey
	s.k.Lock()
	delete(s.k.m, minKey)
	// but 'x' is as array position 0
	s.k.m[x] = 0
	s.k.Unlock()

	heap.Fix(&s.k, 0)
	return e
}

// Keys returns the current estimates for the most frequent elements
func (s *Stream) Keys() []Element {
	elts := append([]Element(nil), s.k.elts...)
	sort.Sort(elementsByCountDescending(elts))
	return elts
}

// Estimate returns an estimate for the item x
func (s *Stream) Estimate(x string) Element {
	xhash := reduce(sip13.Sum64Str(0, 0, x), len(s.alphas))

	// are we tracking this element?
	if idx, ok := s.k.m[x]; ok {
		e := s.k.elts[idx]
		return e
	}
	count := s.alphas[xhash]
	e := Element{
		Key:   x,
		Error: count,
		Count: count,
	}
	return e
}

func (s *Stream) GobEncode() ([]byte, error) {
	buf := bytes.Buffer{}
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(s.n); err != nil {
		return nil, err
	}
	if err := enc.Encode(s.k.m); err != nil {
		return nil, err
	}
	if err := enc.Encode(s.k.elts); err != nil {
		return nil, err
	}
	if err := enc.Encode(s.alphas); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *Stream) GobDecode(b []byte) error {
	dec := gob.NewDecoder(bytes.NewBuffer(b))
	if err := dec.Decode(&s.n); err != nil {
		return err
	}
	if err := dec.Decode(&s.k.m); err != nil {
		return err
	}
	if err := dec.Decode(&s.k.elts); err != nil {
		return err
	}
	if err := dec.Decode(&s.alphas); err != nil {
		return err
	}
	return nil
}
