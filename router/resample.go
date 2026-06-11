package main

import "encoding/binary"

// resampler converts a stream of PCM16 mono audio from srcRate to dstRate
// using linear interpolation. It is stateful: chunk boundaries carry the last
// sample and the fractional read position so arbitrary chunking produces the
// same output as one large buffer.
type resampler struct {
	ratio   float64 // srcRate / dstRate; > 1 means downsampling
	pos     float64 // fractional read position, relative to prev (index 0)
	prev    int16   // last sample of the previous chunk
	hasPrev bool
	oddByte *byte // dangling byte when a chunk splits a sample
}

func newResampler(srcRate, dstRate int) *resampler {
	return &resampler{ratio: float64(srcRate) / float64(dstRate)}
}

// process consumes a chunk of little-endian PCM16 bytes at srcRate and
// returns the corresponding chunk at dstRate.
func (r *resampler) process(in []byte) []byte {
	if r.oddByte != nil {
		in = append([]byte{*r.oddByte}, in...)
		r.oddByte = nil
	}
	if len(in)%2 == 1 {
		b := in[len(in)-1]
		r.oddByte = &b
		in = in[:len(in)-1]
	}
	if len(in) == 0 {
		return nil
	}

	// samples[0] is the carried previous sample (when present), so r.pos
	// indexes a contiguous stream across chunks.
	n := len(in) / 2
	offset := 0
	if r.hasPrev {
		offset = 1
	}
	samples := make([]int16, n+offset)
	if r.hasPrev {
		samples[0] = r.prev
	}
	for i := range n {
		samples[offset+i] = int16(binary.LittleEndian.Uint16(in[2*i:]))
	}

	var out []byte
	for r.pos+1 < float64(len(samples)) {
		i := int(r.pos)
		frac := r.pos - float64(i)
		s := float64(samples[i])*(1-frac) + float64(samples[i+1])*frac
		var buf [2]byte
		binary.LittleEndian.PutUint16(buf[:], uint16(int16(s)))
		out = append(out, buf[0], buf[1])
		r.pos += r.ratio
	}

	// Rebase pos so the last sample becomes index 0 of the next chunk.
	r.prev = samples[len(samples)-1]
	r.hasPrev = true
	r.pos -= float64(len(samples) - 1)

	return out
}
