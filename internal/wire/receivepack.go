package wire

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"io"
	"strings"

	"github.com/facets-cloud/go-gitq/internal/object"
)

type receiveAdvert struct {
	refs map[string]object.Hash
	caps map[string]string
}

func (c *Client) receivePackAdvert(ctx context.Context) (*receiveAdvert, error) {
	resp, err := c.do(ctx, "GET", "/info/refs?service=git-receive-pack", nil, nil)
	if err != nil {
		return nil, err
	}
	defer closeBody(resp)
	adv := &receiveAdvert{refs: map[string]object.Hash{}, caps: map[string]string{}}
	pr := newPktReader(resp.Body)
	first := true
	for {
		k, line, err := pr.nextLine()
		if err != nil {
			return nil, unexpected(err)
		}
		if first && k == pktData && strings.HasPrefix(line, "# service=") {
			if k, _, err := pr.nextLine(); err != nil || k != pktFlush {
				return nil, fmt.Errorf("wire: malformed receive-pack preamble: %v", err)
			}
			continue
		}
		if k != pktData {
			if first {
				return nil, fmt.Errorf("wire: server is not a smart-HTTP receive-pack endpoint")
			}
			return adv, nil
		}
		if first && line == "version 1" {
			continue
		}
		if first {
			first = false
			var caps string
			line, caps, _ = strings.Cut(line, "\x00")
			for _, cp := range strings.Fields(caps) {
				name, val, _ := strings.Cut(cp, "=")
				adv.caps[name] = val
			}
		}
		hs, name, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("wire: bad ref advertisement %q", line)
		}
		if name == "capabilities^{}" || strings.HasSuffix(name, "^{}") {
			continue
		}
		h, err := parseHex(hs)
		if err != nil {
			return nil, err
		}
		adv.refs[name] = h
	}
}

func (c *Client) push(ctx context.Context, updates []RefUpdate, pack []byte) error {
	if len(updates) == 0 {
		return nil
	}
	adv, err := c.receivePackAdvert(ctx)
	if err != nil {
		return err
	}
	allDeletes := true
	for _, u := range updates {
		if adv.refs[u.Name] != u.Old {
			return fmt.Errorf("%w: %s is at %s, expected %s", ErrStale, u.Name,
				hexOf(adv.refs[u.Name]), hexOf(u.Old))
		}
		if !u.New.IsZero() {
			allDeletes = false
		}
	}
	_, sideband := adv.caps["side-band-64k"]
	caps := "report-status"
	if sideband {
		caps += " side-band-64k"
	}
	if allDeletes {
		if _, ok := adv.caps["delete-refs"]; !ok {
			return fmt.Errorf("%w: server does not allow ref deletion", ErrUnsupported)
		}
	}
	caps += " agent=" + userAgent

	var w pktWriter
	for i, u := range updates {
		s := hexOf(u.Old) + " " + hexOf(u.New) + " " + u.Name
		if i == 0 {
			s += "\x00" + caps
		}
		w.line(s + "\n")
	}
	w.flush()
	if !allDeletes {
		if pack == nil {
			pack = emptyPack()
		}
		w.Write(pack)
	}

	resp, err := c.do(ctx, "POST", "/git-receive-pack", w.Bytes(), map[string]string{
		"Content-Type": "application/x-git-receive-pack-request",
		"Accept":       "application/x-git-receive-pack-result",
	})
	if err != nil {
		return err
	}
	defer closeBody(resp)

	var report *pktReader
	if sideband {
		var data bytes.Buffer
		pr := newPktReader(resp.Body)
	loop:
		for {
			k, b, err := pr.next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("wire: reading push report: %w", err)
			}
			if k != pktData {
				break loop
			}
			if len(b) == 0 {
				continue
			}
			switch b[0] {
			case 1:
				data.Write(b[1:])
			case 2:
			case 3:
				return &remoteError{strings.TrimSpace(string(b[1:]))}
			default:
				if bytes.HasPrefix(b, []byte("ERR ")) {
					return &remoteError{strings.TrimSpace(string(b[4:]))}
				}
				return fmt.Errorf("wire: bad sideband channel %d in push report", b[0])
			}
		}
		report = newPktReader(&data)
	} else {
		report = newPktReader(resp.Body)
	}
	return parseReport(report, updates)
}

func parseReport(pr *pktReader, updates []RefUpdate) error {
	k, line, err := pr.nextLine()
	if err != nil {
		return fmt.Errorf("wire: reading push report: %w", unexpected(err))
	}
	if k != pktData || !strings.HasPrefix(line, "unpack ") {
		return fmt.Errorf("wire: malformed push report %q", line)
	}
	if st := strings.TrimPrefix(line, "unpack "); st != "ok" {
		return fmt.Errorf("wire: remote unpack failed: %s", st)
	}
	seen := map[string]bool{}
	var firstErr error
	for {
		k, line, err := pr.nextLine()
		if err == io.EOF || (err == nil && k != pktData) {
			break
		}
		if err != nil {
			return fmt.Errorf("wire: reading push report: %w", err)
		}
		if ref, ok := strings.CutPrefix(line, "ok "); ok {
			seen[ref] = true
			continue
		}
		if rest, ok := strings.CutPrefix(line, "ng "); ok {
			ref, reason, _ := strings.Cut(rest, " ")
			seen[ref] = true
			if firstErr == nil {
				firstErr = ngError(ref, reason)
			}
			continue
		}
		// Unknown lines (e.g. "option" from report-status-v2) are ignored.
	}
	if firstErr != nil {
		return firstErr
	}
	for _, u := range updates {
		if !seen[u.Name] {
			return fmt.Errorf("wire: push report missing status for %s", u.Name)
		}
	}
	return nil
}

func ngError(ref, reason string) error {
	r := strings.ToLower(reason)
	for _, s := range []string{"non-fast-forward", "fetch first", "stale info",
		"failed to lock", "cannot lock ref", "but expected", "already exists"} {
		if strings.Contains(r, s) {
			return fmt.Errorf("%w: %s: %s", ErrStale, ref, reason)
		}
	}
	return &RejectedError{Ref: ref, Reason: reason}
}

// emptyPack is a valid zero-object pack, sent when an update only points a ref
// at objects the server already has.
func emptyPack() []byte {
	p := []byte{'P', 'A', 'C', 'K', 0, 0, 0, 2, 0, 0, 0, 0}
	sum := sha1.Sum(p)
	return append(p, sum[:]...)
}
