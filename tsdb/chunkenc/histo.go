// Copyright 2021 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// The code in this file was largely written by Damian Gryski as part of
// https://github.com/dgryski/go-tsz and published under the license below.
// It was modified to accommodate reading from byte slices without modifying
// the underlying bytes, which would panic when reading from mmap'd
// read-only byte slices.

// Copyright (c) 2015,2016 Damian Gryski <damian@gryski.com>
// All rights reserved.

// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are met:

// * Redistributions of source code must retain the above copyright notice,
// this list of conditions and the following disclaimer.
//
// * Redistributions in binary form must reproduce the above copyright notice,
// this list of conditions and the following disclaimer in the documentation
// and/or other materials provided with the distribution.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND
// ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
// WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
// DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
// FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
// DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
// SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
// CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
// OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package chunkenc

import (
	"encoding/binary"
	"math"
	"math/bits"

	"github.com/prometheus/prometheus/pkg/histogram"
)

const ()

// HistoChunk holds sparse histogram encoded sample data.
// Appends a histogram sample
// * schema defines the resolution (number of buckets per power of 2)
//   Currently, valid numbers are -4 <= n <= 8.
//   They are all for base-2 bucket schemas, where 1 is a bucket boundary in each case, and
//   then each power of two is divided into 2^n logarithmic buckets.
//   Or in other words, each bucket boundary is the previous boundary times 2^(2^-n).
//   In the future, more bucket schemas may be added using numbers < -4 or > 8.
// The bucket with upper boundary of 1 is always bucket 0.
// Then negative numbers for smaller boundaries and positive for uppers.
//
// fields are stored like so:
// field           ts    count zeroCount sum []posbuckets negbuckets
// observation 1   raw   raw   raw       raw []raw        []raw
// observation 2   delta delta delta     xor []delta      []delta
// observation >2  dod   dod   dod       xor []dod        []dod
// TODO zerothreshold
type HistoChunk struct {
	b bstream
}

// NewHistoChunk returns a new chunk with Histo encoding of the given size.
func NewHistoChunk() *HistoChunk {
	b := make([]byte, 2, 128)
	return &HistoChunk{b: bstream{stream: b, count: 0}}
}

// Encoding returns the encoding type.
func (c *HistoChunk) Encoding() Encoding {
	return EncSHS
}

// Bytes returns the underlying byte slice of the chunk.
func (c *HistoChunk) Bytes() []byte {
	return c.b.bytes()
}

// NumSamples returns the number of samples in the chunk.
func (c *HistoChunk) NumSamples() int {
	return int(binary.BigEndian.Uint16(c.Bytes()))
}

// Meta returns the histogram metadata.
// callers may only call this on chunks that have at least one sample
func (c *HistoChunk) Meta() (int32, []histogram.Span, []histogram.Span, error) {
	if c.NumSamples() == 0 {
		panic("HistoChunk.Meta() called on an empty chunk")
	}
	b := newBReader(c.Bytes()[2:])
	return readHistoChunkMeta(&b)
}

func (c *HistoChunk) Compact() {
	if l := len(c.b.stream); cap(c.b.stream) > l+chunkCompactCapacityThreshold {
		buf := make([]byte, l)
		copy(buf, c.b.stream)
		c.b.stream = buf
	}
}

// Appender implements the Chunk interface.
func (c *HistoChunk) Appender() (Appender, error) {
	it := c.iterator(nil)

	// To get an appender we must know the state it would have if we had
	// appended all existing data from scratch.
	// We iterate through the end and populate via the iterator's state.
	for it.Next() {
	}
	if err := it.Err(); err != nil {
		return nil, err
	}

	a := &histoAppender{
		b: &c.b,

		schema:          it.schema,
		posSpans:        it.posSpans,
		negSpans:        it.negSpans,
		t:               it.t,
		cnt:             it.cnt,
		zcnt:            it.zcnt,
		tDelta:          it.tDelta,
		cntDelta:        it.cntDelta,
		zcntDelta:       it.zcntDelta,
		posbuckets:      it.posbuckets,
		negbuckets:      it.negbuckets,
		posbucketsDelta: it.posbucketsDelta,
		negbucketsDelta: it.negbucketsDelta,

		sum:      it.sum,
		leading:  it.leading,
		trailing: it.trailing,

		buf64: make([]byte, binary.MaxVarintLen64),
	}
	if binary.BigEndian.Uint16(a.b.bytes()) == 0 {
		a.leading = 0xff
	}
	return a, nil
}

func countSpans(spans []histogram.Span) int {
	var cnt int
	for _, s := range spans {
		cnt += int(s.Length)
	}
	return cnt
}

func newHistoIterator(b []byte) *histoIterator {
	it := &histoIterator{
		br:       newBReader(b),
		numTotal: binary.BigEndian.Uint16(b),
		t:        math.MinInt64,
	}
	// The first 2 bytes contain chunk headers.
	// We skip that for actual samples.
	_, _ = it.br.readBits(16)
	return it
}

func (c *HistoChunk) iterator(it Iterator) *histoIterator {
	// TODO fix this. this is taken from xor.go // dieter not sure what the purpose of this is
	// Should iterators guarantee to act on a copy of the data so it doesn't lock append?
	// When using striped locks to guard access to chunks, probably yes.
	// Could only copy data if the chunk is not completed yet.
	//if histoIter, ok := it.(*histoIterator); ok {
	//	histoIter.Reset(c.b.bytes())
	//	return histoIter
	//}
	return newHistoIterator(c.b.bytes())
}

// Iterator implements the Chunk interface.
func (c *HistoChunk) Iterator(it Iterator) Iterator {
	return c.iterator(it)
}

type histoAppender struct {
	b *bstream

	// Metadata:
	schema             int32
	posSpans, negSpans []histogram.Span

	// For the fields that are tracked as dod's.
	// Note that we expect to handle negative deltas (e.g. resets) by
	// creating new chunks, we still want to support it in general hence
	// signed integer types.
	t                           int64
	cnt, zcnt                   uint64
	tDelta, cntDelta, zcntDelta int64

	posbuckets, negbuckets           []int64
	posbucketsDelta, negbucketsDelta []int64

	// The sum is Gorilla xor encoded.
	sum      float64
	leading  uint8
	trailing uint8

	buf64 []byte // For working on varint64's.
}

func putVarint(b *bstream, buf []byte, x int64) {
	for _, byt := range buf[:binary.PutVarint(buf, x)] {
		b.writeByte(byt)
	}
}

func putUvarint(b *bstream, buf []byte, x uint64) {
	for _, byt := range buf[:binary.PutUvarint(buf, x)] {
		b.writeByte(byt)
	}
}

func (a *histoAppender) Append(int64, float64) {}

// AppendHistogram appends a SparseHistogram to the chunk. We assume the
// histogram is properly structured. E.g. that the number of pos/neg buckets
// used corresponds to the number conveyed by the pos/neg span structures.
func (a *histoAppender) AppendHistogram(t int64, h histogram.SparseHistogram) {
	var tDelta, cntDelta, zcntDelta int64
	num := binary.BigEndian.Uint16(a.b.bytes())

	switch num {
	case 0:
		// the first append gets the privilege to dictate the metadata
		// but it's also responsible for encoding it into the chunk!

		writeHistoChunkMeta(a.b, h.Schema, h.PositiveSpans, h.NegativeSpans)
		a.schema = h.Schema
		a.posSpans, a.negSpans = h.PositiveSpans, h.NegativeSpans
		numPosBuckets, numNegBuckets := countSpans(h.PositiveSpans), countSpans(h.NegativeSpans)
		a.posbuckets = make([]int64, numPosBuckets)
		a.negbuckets = make([]int64, numNegBuckets)
		a.posbucketsDelta = make([]int64, numPosBuckets)
		a.negbucketsDelta = make([]int64, numNegBuckets)

		// now store actual data
		putVarint(a.b, a.buf64, t)
		putUvarint(a.b, a.buf64, h.Count)
		putUvarint(a.b, a.buf64, h.ZeroCount)
		a.b.writeBits(math.Float64bits(h.Sum), 64)
		for _, buck := range h.PositiveBuckets {
			putVarint(a.b, a.buf64, buck)
		}
		for _, buck := range h.NegativeBuckets {
			putVarint(a.b, a.buf64, buck)
		}
	case 1:
		// TODO if zerobucket thresh or schema is different, we should create a new chunk
		posInterjections, _ := compareSpans(a.posSpans, h.PositiveSpans)
		//if !ok {
		// TODO Ganesh this is when we know buckets have dis-appeared and we should create a new chunk instead
		//}
		negInterjections, _ := compareSpans(a.negSpans, h.NegativeSpans)
		//if !ok {
		// TODO Ganesh this is when we know buckets have dis-appeared and we should create a new chunk instead
		//}
		if len(posInterjections) > 0 || len(negInterjections) > 0 {
			// new buckets have appeared. we need to recode all prior histograms within the chunk before we can process this one.
			a.recode(posInterjections, negInterjections, h.PositiveSpans, h.NegativeSpans)
		}

		tDelta = t - a.t
		cntDelta = int64(h.Count) - int64(a.cnt)
		zcntDelta = int64(h.ZeroCount) - int64(a.zcnt)

		putVarint(a.b, a.buf64, tDelta)
		putVarint(a.b, a.buf64, cntDelta)
		putVarint(a.b, a.buf64, zcntDelta)

		a.writeSumDelta(h.Sum)

		for i, buck := range h.PositiveBuckets {
			delta := buck - a.posbuckets[i]
			putVarint(a.b, a.buf64, delta)
			a.posbucketsDelta[i] = delta
		}
		for i, buck := range h.NegativeBuckets {
			delta := buck - a.negbuckets[i]
			putVarint(a.b, a.buf64, delta)
			a.negbucketsDelta[i] = delta
		}
	default:
		// TODO if zerobucket thresh or schema is different, we should create a new chunk
		posInterjections, _ := compareSpans(a.posSpans, h.PositiveSpans)
		//if !ok {
		// TODO Ganesh this is when we know buckets have dis-appeared and we should create a new chunk instead
		//}
		negInterjections, _ := compareSpans(a.negSpans, h.NegativeSpans)
		//if !ok {
		// TODO Ganesh this is when we know buckets have dis-appeared and we should create a new chunk instead
		//}
		if len(posInterjections) > 0 || len(negInterjections) > 0 {
			// new buckets have appeared. we need to recode all prior histograms within the chunk before we can process this one.
			a.recode(posInterjections, negInterjections, h.PositiveSpans, h.NegativeSpans)
		}
		tDelta = t - a.t
		cntDelta = int64(h.Count) - int64(a.cnt)
		zcntDelta = int64(h.ZeroCount) - int64(a.zcnt)

		tDod := tDelta - a.tDelta
		cntDod := cntDelta - a.cntDelta
		zcntDod := zcntDelta - a.zcntDelta

		putInt64VBBucket(a.b, tDod)
		putInt64VBBucket(a.b, cntDod)
		putInt64VBBucket(a.b, zcntDod)

		a.writeSumDelta(h.Sum)

		for i, buck := range h.PositiveBuckets {
			delta := buck - a.posbuckets[i]
			dod := delta - a.posbucketsDelta[i]
			putInt64VBBucket(a.b, dod)
			a.posbucketsDelta[i] = delta
		}
		for i, buck := range h.NegativeBuckets {
			delta := buck - a.negbuckets[i]
			dod := delta - a.negbucketsDelta[i]
			putInt64VBBucket(a.b, dod)
			a.negbucketsDelta[i] = delta
		}
	}

	binary.BigEndian.PutUint16(a.b.bytes(), num+1)

	a.t = t
	a.cnt = h.Count
	a.zcnt = h.ZeroCount
	a.tDelta = tDelta
	a.cntDelta = cntDelta
	a.zcntDelta = zcntDelta

	a.posbuckets, a.negbuckets = h.PositiveBuckets, h.NegativeBuckets
	// note that the bucket deltas were already updated above

	a.sum = h.Sum

}

// recode converts the current chunk to accommodate an expansion of the set of
// (positive and/or negative) buckets used, according to the provided interjections, resulting in
// the honoring of the provided new posSpans and negSpans
// note: the decode-recode can probably be done more efficiently, but that's for a future optimization
func (a *histoAppender) recode(posInterjections, negInterjections []interjection, posSpans, negSpans []histogram.Span) {
	it := newHistoIterator(a.b.bytes())
	app, err := NewHistoChunk().Appender()
	if err != nil {
		panic(err)
	}
	numPosBuckets, numNegBuckets := countSpans(posSpans), countSpans(negSpans)
	posbuckets := make([]int64, numPosBuckets) // new (modified) histogram buckets
	negbuckets := make([]int64, numNegBuckets) // new (modified) histogram buckets

	for it.Next() {
		tOld, hOld := it.AtHistogram()
		// save the modified histogram to the new chunk
		hOld.PositiveSpans, hOld.NegativeSpans = posSpans, negSpans
		if len(posInterjections) > 0 {
			hOld.PositiveBuckets = interject(hOld.PositiveBuckets, posbuckets, posInterjections)
		}
		if len(negInterjections) > 0 {
			hOld.NegativeBuckets = interject(hOld.NegativeBuckets, negbuckets, negInterjections)
		}
		// there is no risk of infinite recursion here as all histograms get appended with the same schema (number of buckets)
		app.AppendHistogram(tOld, hOld)
	}

	// adopt the new appender into ourselves
	// we skip porting some fields like schema, t, cnt and zcnt, sum because they didn't change between our old chunk and the recoded one
	app2 := app.(*histoAppender)
	a.b = app2.b
	a.posSpans, a.negSpans = posSpans, negSpans
	a.posbuckets, a.negbuckets = app2.posbuckets, app2.negbuckets
	a.posbucketsDelta, a.negbucketsDelta = app2.posbucketsDelta, app2.negbucketsDelta
}

func (a *histoAppender) writeSumDelta(v float64) {
	vDelta := math.Float64bits(v) ^ math.Float64bits(a.sum)

	if vDelta == 0 {
		a.b.writeBit(zero)
		return
	}
	a.b.writeBit(one)

	leading := uint8(bits.LeadingZeros64(vDelta))
	trailing := uint8(bits.TrailingZeros64(vDelta))

	// Clamp number of leading zeros to avoid overflow when encoding.
	if leading >= 32 {
		leading = 31
	}

	if a.leading != 0xff && leading >= a.leading && trailing >= a.trailing {
		a.b.writeBit(zero)
		a.b.writeBits(vDelta>>a.trailing, 64-int(a.leading)-int(a.trailing))
	} else {
		a.leading, a.trailing = leading, trailing

		a.b.writeBit(one)
		a.b.writeBits(uint64(leading), 5)

		// Note that if leading == trailing == 0, then sigbits == 64.  But that value doesn't actually fit into the 6 bits we have.
		// Luckily, we never need to encode 0 significant bits, since that would put us in the other case (vdelta == 0).
		// So instead we write out a 0 and adjust it back to 64 on unpacking.
		sigbits := 64 - leading - trailing
		a.b.writeBits(uint64(sigbits), 6)
		a.b.writeBits(vDelta>>trailing, int(sigbits))
	}
}

type histoIterator struct {
	br       bstreamReader
	numTotal uint16
	numRead  uint16

	// Meta
	schema             int32
	posSpans, negSpans []histogram.Span

	// for the fields that are tracked as dod's
	t                           int64
	cnt, zcnt                   uint64
	tDelta, cntDelta, zcntDelta int64

	posbuckets, negbuckets           []int64
	posbucketsDelta, negbucketsDelta []int64

	// for the fields that are gorilla xor coded
	sum      float64
	leading  uint8
	trailing uint8

	err error
}

func (it *histoIterator) Seek(t int64) bool {
	if it.err != nil {
		return false
	}

	for t > it.t || it.numRead == 0 {
		if !it.Next() {
			return false
		}
	}
	return true
}

func (it *histoIterator) At() (int64, float64) {
	panic("cannot call histoIterator.At().")
}

func (it *histoIterator) ChunkEncoding() Encoding {
	return EncSHS
}

func (it *histoIterator) AtHistogram() (int64, histogram.SparseHistogram) {
	return it.t, histogram.SparseHistogram{
		Count:           it.cnt,
		ZeroCount:       it.zcnt,
		Sum:             it.sum,
		ZeroThreshold:   0, // TODO
		Schema:          it.schema,
		PositiveSpans:   it.posSpans,
		NegativeSpans:   it.negSpans,
		PositiveBuckets: it.posbuckets,
		NegativeBuckets: it.negbuckets,
	}
}

func (it *histoIterator) Err() error {
	return it.err
}

func (it *histoIterator) Reset(b []byte) {
	// The first 2 bytes contain chunk headers.
	// We skip that for actual samples.
	it.br = newBReader(b[2:])
	it.numTotal = binary.BigEndian.Uint16(b)
	it.numRead = 0

	it.t, it.cnt, it.zcnt = 0, 0, 0
	it.tDelta, it.cntDelta, it.zcntDelta = 0, 0, 0

	for i := range it.posbuckets {
		it.posbuckets[i] = 0
		it.posbucketsDelta[i] = 0
	}
	for i := range it.negbuckets {
		it.negbuckets[i] = 0
		it.negbucketsDelta[i] = 0
	}

	it.sum = 0
	it.leading = 0
	it.trailing = 0
	it.err = nil
}

func (it *histoIterator) Next() bool {
	if it.err != nil || it.numRead == it.numTotal {
		return false
	}

	if it.numRead == 0 {

		// first read is responsible for reading chunk metadata and initializing fields that depend on it
		schema, posSpans, negSpans, err := readHistoChunkMeta(&it.br)
		if err != nil {
			it.err = err
			return false
		}
		it.schema = schema
		it.posSpans, it.negSpans = posSpans, negSpans
		numPosBuckets, numNegBuckets := countSpans(posSpans), countSpans(negSpans)
		it.posbuckets = make([]int64, numPosBuckets)
		it.negbuckets = make([]int64, numNegBuckets)
		it.posbucketsDelta = make([]int64, numPosBuckets)
		it.negbucketsDelta = make([]int64, numNegBuckets)

		// now read actual data

		t, err := binary.ReadVarint(&it.br)
		if err != nil {
			it.err = err
			return false
		}
		it.t = t

		cnt, err := binary.ReadUvarint(&it.br)
		if err != nil {
			it.err = err
			return false
		}
		it.cnt = cnt

		zcnt, err := binary.ReadUvarint(&it.br)
		if err != nil {
			it.err = err
			return false
		}
		it.zcnt = zcnt

		sum, err := it.br.readBits(64)
		if err != nil {
			it.err = err
			return false
		}
		it.sum = math.Float64frombits(sum)

		for i := range it.posbuckets {
			v, err := binary.ReadVarint(&it.br)
			if err != nil {
				it.err = err
				return false
			}
			it.posbuckets[i] = v
		}
		for i := range it.negbuckets {
			v, err := binary.ReadVarint(&it.br)
			if err != nil {
				it.err = err
				return false
			}
			it.negbuckets[i] = v
		}

		it.numRead++
		return true
	}

	if it.numRead == 1 {
		tDelta, err := binary.ReadVarint(&it.br)
		if err != nil {
			it.err = err
			return false
		}
		it.tDelta = tDelta
		it.t += int64(it.tDelta)

		cntDelta, err := binary.ReadVarint(&it.br)
		if err != nil {
			it.err = err
			return false
		}
		it.cntDelta = cntDelta
		it.cnt = uint64(int64(it.cnt) + it.cntDelta)

		zcntDelta, err := binary.ReadVarint(&it.br)
		if err != nil {
			it.err = err
			return false
		}
		it.zcntDelta = zcntDelta
		it.zcnt = uint64(int64(it.zcnt) + it.zcntDelta)

		ok := it.readSum()
		if !ok {
			return false
		}

		for i := range it.posbuckets {
			delta, err := binary.ReadVarint(&it.br)
			if err != nil {
				it.err = err
				return false
			}
			it.posbucketsDelta[i] = delta
			it.posbuckets[i] = it.posbuckets[i] + delta
		}

		for i := range it.negbuckets {
			delta, err := binary.ReadVarint(&it.br)
			if err != nil {
				it.err = err
				return false
			}
			it.negbucketsDelta[i] = delta
			it.negbuckets[i] = it.negbuckets[i] + delta
		}

		return true
	}

	tDod, err := readInt64VBBucket(&it.br)
	if err != nil {
		it.err = err
		return false
	}
	it.tDelta = it.tDelta + tDod
	it.t += it.tDelta

	cntDod, err := readInt64VBBucket(&it.br)
	if err != nil {
		it.err = err
		return false
	}
	it.cntDelta = it.cntDelta + cntDod
	it.cnt = uint64(int64(it.cnt) + it.cntDelta)

	zcntDod, err := readInt64VBBucket(&it.br)
	if err != nil {
		it.err = err
		return false
	}
	it.zcntDelta = it.zcntDelta + zcntDod
	it.zcnt = uint64(int64(it.zcnt) + it.zcntDelta)

	ok := it.readSum()
	if !ok {
		return false
	}

	for i := range it.posbuckets {
		dod, err := readInt64VBBucket(&it.br)
		if err != nil {
			it.err = err
			return false
		}
		it.posbucketsDelta[i] = it.posbucketsDelta[i] + dod
		it.posbuckets[i] = it.posbuckets[i] + it.posbucketsDelta[i]
	}

	for i := range it.negbuckets {
		dod, err := readInt64VBBucket(&it.br)
		if err != nil {
			it.err = err
			return false
		}
		it.negbucketsDelta[i] = it.negbucketsDelta[i] + dod
		it.negbuckets[i] = it.negbuckets[i] + it.negbucketsDelta[i]
	}

	return true
}

func (it *histoIterator) readSum() bool {
	bit, err := it.br.readBitFast()
	if err != nil {
		bit, err = it.br.readBit()
	}
	if err != nil {
		it.err = err
		return false
	}

	if bit == zero {
		// it.sum = it.sum
	} else {
		bit, err := it.br.readBitFast()
		if err != nil {
			bit, err = it.br.readBit()
		}
		if err != nil {
			it.err = err
			return false
		}
		if bit == zero {
			// reuse leading/trailing zero bits
			// it.leading, it.trailing = it.leading, it.trailing
		} else {
			bits, err := it.br.readBitsFast(5)
			if err != nil {
				bits, err = it.br.readBits(5)
			}
			if err != nil {
				it.err = err
				return false
			}
			it.leading = uint8(bits)

			bits, err = it.br.readBitsFast(6)
			if err != nil {
				bits, err = it.br.readBits(6)
			}
			if err != nil {
				it.err = err
				return false
			}
			mbits := uint8(bits)
			// 0 significant bits here means we overflowed and we actually need 64; see comment in encoder
			if mbits == 0 {
				mbits = 64
			}
			it.trailing = 64 - it.leading - mbits
		}

		mbits := 64 - it.leading - it.trailing
		bits, err := it.br.readBitsFast(mbits)
		if err != nil {
			bits, err = it.br.readBits(mbits)
		}
		if err != nil {
			it.err = err
			return false
		}
		vbits := math.Float64bits(it.sum)
		vbits ^= bits << it.trailing
		it.sum = math.Float64frombits(vbits)
	}

	it.numRead++
	return true
}
