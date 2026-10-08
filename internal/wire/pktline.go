package wire

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/facets-cloud/go-gitq/internal/object"
)

const maxPktLen = 65520

type pktKind int

const (
	pktData pktKind = iota
	pktFlush
	pktDelim
	pktRespEnd
)

// pktWriter builds a pkt-line request body.
type pktWriter struct{ bytes.Buffer }

func (w *pktWriter) line(s string) {
	fmt.Fprintf(&w.Buffer, "%04x", len(s)+4)
	w.WriteString(s)
}

func (w *pktWriter) flush() { w.WriteString("0000") }
func (w *pktWriter) delim() { w.WriteString("0001") }

// pktReader reads pkt-lines. The returned payload aliases an internal buffer
// that is valid until the next call.
type pktReader struct {
	r   *bufio.Reader
	hdr [4]byte
	buf []byte
}

func newPktReader(r io.Reader) *pktReader {
	return &pktReader{r: bufio.NewReaderSize(r, 64<<10), buf: make([]byte, maxPktLen)}
}

// next returns the next packet. io.EOF is returned only at a clean packet boundary.
func (p *pktReader) next() (pktKind, []byte, error) {
	if _, err := io.ReadFull(p.r, p.hdr[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return 0, nil, fmt.Errorf("wire: truncated pkt-line header")
		}
		return 0, nil, err
	}
	var n [2]byte
	if _, err := hex.Decode(n[:], p.hdr[:]); err != nil {
		return 0, nil, fmt.Errorf("wire: bad pkt-line length %q", p.hdr[:])
	}
	l := int(n[0])<<8 | int(n[1])
	switch l {
	case 0:
		return pktFlush, nil, nil
	case 1:
		return pktDelim, nil, nil
	case 2:
		return pktRespEnd, nil, nil
	case 3:
		return 0, nil, fmt.Errorf("wire: bad pkt-line length 3")
	}
	l -= 4
	if l > len(p.buf) {
		p.buf = make([]byte, l)
	}
	b := p.buf[:l]
	if _, err := io.ReadFull(p.r, b); err != nil {
		return 0, nil, fmt.Errorf("wire: truncated pkt-line: %w", unexpected(err))
	}
	return pktData, b, nil
}

// nextLine is next for text lines: trailing "\n" is stripped and "ERR " packets
// become errors.
func (p *pktReader) nextLine() (pktKind, string, error) {
	k, b, err := p.next()
	if err != nil || k != pktData {
		return k, "", err
	}
	s := string(bytes.TrimSuffix(b, []byte("\n")))
	if len(s) >= 4 && s[:4] == "ERR " {
		return k, s, &remoteError{s[4:]}
	}
	return k, s, nil
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

type remoteError struct{ msg string }

func (e *remoteError) Error() string { return "wire: remote error: " + e.msg }

var errBadHash = errors.New("wire: bad object id")

func parseHex(s string) (object.Hash, error) {
	var h object.Hash
	if len(s) != 40 {
		return h, fmt.Errorf("%w %q", errBadHash, s)
	}
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return h, fmt.Errorf("%w %q", errBadHash, s)
	}
	return h, nil
}

func hexOf(h object.Hash) string { return hex.EncodeToString(h[:]) }
