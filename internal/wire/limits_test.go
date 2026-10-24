package wire

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The pkt-line reader accepts the protocol maximum of 65520 bytes (header
// included) and rejects anything longer, like git does.
func TestPktLineMaxLength(t *testing.T) {
	pkt := func(n int) string { return fmt.Sprintf("%04x", n) + strings.Repeat("x", n-4) }
	k, b, err := newPktReader(strings.NewReader(pkt(maxPktLen))).next()
	if err != nil || k != pktData || len(b) != maxPktLen-4 {
		t.Fatalf("65520-byte packet: kind %v, %d bytes, %v", k, len(b), err)
	}
	for _, n := range []int{maxPktLen + 1, 0xffff} {
		if _, _, err := newPktReader(strings.NewReader(pkt(n))).next(); err == nil ||
			!strings.Contains(err.Error(), "exceeds the protocol maximum") {
			t.Errorf("%d-byte packet: err = %v, want rejected", n, err)
		}
	}
}

// limitBody lets a body of exactly the limit through, fails one byte past it
// with ErrTooLarge naming the read, and never reads further.
func TestLimitBody(t *testing.T) {
	const size = 100
	for _, tc := range []struct {
		max     int64
		tooBig  bool
		maxRead int
	}{
		{0, false, size}, // no limit
		{size + 1, false, size},
		{size, false, size},
		{size - 1, true, size},
		{10, true, 11},
	} {
		src := strings.NewReader(strings.Repeat("x", size))
		resp := &http.Response{Body: io.NopCloser(src)}
		limitBody(resp, tc.max, "test response")
		got, err := io.ReadAll(resp.Body)
		if tc.tooBig {
			if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "test response larger than") ||
				int64(len(got)) != tc.max {
				t.Errorf("max %d: read %d bytes, err %v; want %d bytes and ErrTooLarge", tc.max, len(got), err, tc.max)
			}
			if _, err2 := resp.Body.Read(make([]byte, 8)); !errors.Is(err2, ErrTooLarge) {
				t.Errorf("max %d: read after the limit: %v", tc.max, err2)
			}
		} else if err != nil || len(got) != size {
			t.Errorf("max %d: read %d bytes, err %v; want all %d", tc.max, len(got), err, size)
		}
		closeBody(resp)
		if read := size - src.Len(); read > tc.maxRead {
			t.Errorf("max %d: %d bytes taken from the body, want at most %d", tc.max, read, tc.maxRead)
		}
	}
}
