package wire

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

func (c *Client) capabilities(ctx context.Context) (Caps, error) {
	resp, err := c.do(ctx, "GET", "/info/refs?service=git-upload-pack", nil,
		map[string]string{"Git-Protocol": "version=2"})
	if err != nil {
		return Caps{}, err
	}
	defer closeBody(resp)
	var caps Caps
	pr := newPktReader(resp.Body)
	first := true
	for {
		k, line, err := pr.nextLine()
		if err != nil {
			if err == io.EOF && !first {
				return caps, nil
			}
			if err == io.EOF {
				return caps, fmt.Errorf("wire: empty capability advertisement")
			}
			return caps, err
		}
		if first {
			first = false
			if k == pktData && strings.HasPrefix(line, "# service=") {
				// Smart-HTTP preamble: service line followed by a flush.
				if k, _, err := pr.nextLine(); err != nil || k != pktFlush {
					return caps, fmt.Errorf("wire: malformed service preamble: %v", err)
				}
				if k, line, err = pr.nextLine(); err != nil {
					return caps, unexpected(err)
				}
			}
			if k != pktData || line != "version 2" {
				return caps, nil // v0/v1 server
			}
			caps.Version2 = true
			continue
		}
		if k != pktData {
			return caps, nil
		}
		caps.Raw = append(caps.Raw, line)
		if v, ok := strings.CutPrefix(line, "fetch="); ok {
			caps.Fetch = strings.Fields(v)
		}
	}
}

func (c *Client) lsRefs(ctx context.Context, prefixes []string) ([]Ref, error) {
	var w pktWriter
	w.line("command=ls-refs\n")
	w.delim()
	w.line("symrefs\n")
	w.line("peel\n")
	for _, p := range prefixes {
		w.line("ref-prefix " + p + "\n")
	}
	w.flush()
	resp, err := c.do(ctx, "POST", "/git-upload-pack", w.Bytes(), uploadPackHeaders())
	if err != nil {
		return nil, err
	}
	defer closeBody(resp)
	pr := newPktReader(resp.Body)
	var refs []Ref
	for {
		k, line, err := pr.nextLine()
		if err != nil {
			return nil, unexpected(err)
		}
		if k != pktData {
			return refs, nil
		}
		f := strings.Split(line, " ")
		if len(f) < 2 {
			return nil, fmt.Errorf("wire: bad ls-refs line %q", line)
		}
		r := Ref{Name: f[1]}
		if f[0] != "unborn" {
			if r.Hash, err = parseHex(f[0]); err != nil {
				return nil, err
			}
		}
		for _, a := range f[2:] {
			if v, ok := strings.CutPrefix(a, "symref-target:"); ok {
				r.Symref = v
			} else if v, ok := strings.CutPrefix(a, "peeled:"); ok {
				if r.Peeled, err = parseHex(v); err != nil {
					return nil, err
				}
			}
		}
		refs = append(refs, r)
	}
}

func (c *Client) fetch(ctx context.Context, req FetchRequest) ([]byte, error) {
	if len(req.Wants) == 0 {
		return nil, fmt.Errorf("wire: fetch with no wants")
	}
	var w pktWriter
	w.line("command=fetch\n")
	w.delim()
	w.line("ofs-delta\n")
	w.line("no-progress\n")
	for _, h := range req.Wants {
		w.line("want " + hexOf(h) + "\n")
	}
	for _, h := range req.Haves {
		w.line("have " + hexOf(h) + "\n")
	}
	if req.Depth > 0 {
		w.line("deepen " + strconv.Itoa(req.Depth) + "\n")
	}
	if req.Filter != "" {
		w.line("filter " + req.Filter + "\n")
	}
	w.line("done\n")
	w.flush()

	resp, err := c.do(ctx, "POST", "/git-upload-pack", w.Bytes(), uploadPackHeaders())
	if err != nil {
		return nil, err
	}
	defer closeBody(resp)
	pr := newPktReader(resp.Body)

	// Section headers, skipping everything until "packfile".
	for {
		k, line, err := pr.nextLine()
		if err != nil {
			return nil, unexpected(err)
		}
		if k != pktData {
			return nil, fmt.Errorf("wire: fetch response ended without packfile")
		}
		switch line {
		case "packfile":
			return readSideband(pr, resp.ContentLength)
		case "acknowledgments", "shallow-info", "wanted-refs":
			if err := skipSection(pr); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("wire: unexpected fetch section %q", line)
		}
	}
}

// skipSection discards lines until a delim. A flush means the response ended.
func skipSection(pr *pktReader) error {
	for {
		k, _, err := pr.nextLine()
		if err != nil {
			return unexpected(err)
		}
		switch k {
		case pktDelim:
			return nil
		case pktFlush, pktRespEnd:
			return fmt.Errorf("wire: fetch response ended without packfile")
		}
	}
}

// readSideband collects channel-1 data until flush.
func readSideband(pr *pktReader, sizeHint int64) ([]byte, error) {
	var out bytes.Buffer
	if sizeHint > 0 && sizeHint < 1<<31 {
		out.Grow(int(sizeHint))
	} else {
		out.Grow(64 << 10)
	}
	for {
		k, b, err := pr.next()
		if err != nil {
			return nil, fmt.Errorf("wire: reading pack: %w", unexpected(err))
		}
		if k != pktData {
			return out.Bytes(), nil
		}
		if len(b) == 0 {
			continue
		}
		switch b[0] {
		case 1:
			out.Write(b[1:])
		case 2:
		case 3:
			return nil, &remoteError{strings.TrimSpace(string(b[1:]))}
		default:
			if bytes.HasPrefix(b, []byte("ERR ")) {
				return nil, &remoteError{strings.TrimSpace(string(b[4:]))}
			}
			return nil, fmt.Errorf("wire: bad sideband channel %d", b[0])
		}
	}
}
