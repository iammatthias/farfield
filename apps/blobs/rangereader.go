package main

import (
	"errors"
	"io"
)

// rangeReader adapts a ranged-GET backend into an io.ReadSeekCloser so
// http.ServeContent can answer Range requests from a store whose native
// stream is not seekable — R2. Video playback depends on this: Safari
// refuses to play media at all unless the server honors Range with 206s.
//
// Seeks are pure arithmetic. Bytes are fetched lazily from the logical
// offset, and a read at the position an open body is already at resumes
// that body — so ServeContent's size-probing seek dance (end, then back to
// start) reuses the initial stream instead of re-fetching.
type rangeReader struct {
	fetch   func(off int64) (io.ReadCloser, error)
	size    int64
	off     int64         // logical position
	body    io.ReadCloser // open stream positioned at bodyOff; nil when none
	bodyOff int64
}

func (rr *rangeReader) Read(p []byte) (int, error) {
	if rr.off >= rr.size {
		return 0, io.EOF
	}
	if rr.body != nil && rr.bodyOff != rr.off {
		rr.body.Close()
		rr.body = nil
	}
	if rr.body == nil {
		b, err := rr.fetch(rr.off)
		if err != nil {
			return 0, err
		}
		rr.body = b
		rr.bodyOff = rr.off
	}
	n, err := rr.body.Read(p)
	rr.off += int64(n)
	rr.bodyOff = rr.off
	if errors.Is(err, io.EOF) {
		rr.body.Close()
		rr.body = nil
	}
	return n, err
}

func (rr *rangeReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = rr.off + offset
	case io.SeekEnd:
		abs = rr.size + offset
	default:
		return 0, errors.New("rangeReader: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("rangeReader: negative position")
	}
	rr.off = abs
	return abs, nil
}

func (rr *rangeReader) Close() error {
	if rr.body == nil {
		return nil
	}
	err := rr.body.Close()
	rr.body = nil
	return err
}
