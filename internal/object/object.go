// Package object holds git object types and their (de)serialisation.
package object

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// Type is a git object type, numbered as in the pack format.
type Type int8

const (
	TypeCommit Type = 1
	TypeTree   Type = 2
	TypeBlob   Type = 3
	TypeTag    Type = 4
)

// String returns the loose-object name ("commit", "tree", "blob", "tag").
func (t Type) String() string {
	switch t {
	case TypeCommit:
		return "commit"
	case TypeTree:
		return "tree"
	case TypeBlob:
		return "blob"
	case TypeTag:
		return "tag"
	}
	return "unknown(" + strconv.Itoa(int(t)) + ")"
}

// Hash is a SHA-1 object id.
type Hash [20]byte

// ZeroHash is the all-zero id git uses for "no object".
var ZeroHash Hash

func (h Hash) String() string { return hex.EncodeToString(h[:]) }
func (h Hash) IsZero() bool   { return h == ZeroHash }

// ParseHash parses a 40-char hex id.
func ParseHash(s string) (Hash, error) {
	var h Hash
	if len(s) != 40 {
		return h, fmt.Errorf("object: invalid hash %q: want 40 hex chars", s)
	}
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return ZeroHash, fmt.Errorf("object: invalid hash %q: %w", s, err)
	}
	return h, nil
}

// HashOf computes the id of an object: sha1("<type> <len>\x00" + data).
func HashOf(t Type, data []byte) Hash {
	d := sha1.New()
	var hdr [32]byte
	b := append(hdr[:0], t.String()...)
	b = append(b, ' ')
	b = strconv.AppendInt(b, int64(len(data)), 10)
	b = append(b, 0)
	d.Write(b)
	d.Write(data)
	var h Hash
	d.Sum(h[:0])
	return h
}

// Object is a fully resolved (non-delta) object.
type Object struct {
	Type Type
	Data []byte
}

// Hash returns HashOf(o.Type, o.Data).
func (o Object) Hash() Hash { return HashOf(o.Type, o.Data) }

// Tree entry modes.
const (
	ModeDir       uint32 = 0o040000
	ModeFile      uint32 = 0o100644
	ModeExec      uint32 = 0o100755
	ModeSymlink   uint32 = 0o120000
	ModeSubmodule uint32 = 0o160000
)

type TreeEntry struct {
	Name string
	Mode uint32
	Hash Hash
}

// ParseTree decodes raw tree object data.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp <= 0 {
			return nil, errors.New("object: malformed tree entry mode")
		}
		var mode uint32
		for _, c := range data[:sp] {
			if c < '0' || c > '7' {
				return nil, fmt.Errorf("object: malformed tree mode %q", data[:sp])
			}
			mode = mode<<3 | uint32(c-'0')
		}
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul < 0 {
			return nil, errors.New("object: malformed tree entry name")
		}
		name := string(data[:nul])
		data = data[nul+1:]
		if len(data) < 20 {
			return nil, errors.New("object: truncated tree entry hash")
		}
		var h Hash
		copy(h[:], data[:20])
		data = data[20:]
		out = append(out, TreeEntry{Name: name, Mode: mode, Hash: h})
	}
	return out, nil
}

// treeSortKey returns the name used for git tree ordering.
func treeSortKey(e TreeEntry) string {
	if e.Mode&0o170000 == ModeDir {
		return e.Name + "/"
	}
	return e.Name
}

// EncodeTree encodes entries, sorting them in git order
// (directories compare as if their name had a trailing '/').
func EncodeTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return treeSortKey(sorted[i]) < treeSortKey(sorted[j])
	})
	size := 0
	for _, e := range sorted {
		size += 7 + 1 + len(e.Name) + 1 + 20
	}
	b := make([]byte, 0, size)
	for _, e := range sorted {
		b = strconv.AppendUint(b, uint64(e.Mode), 8)
		b = append(b, ' ')
		b = append(b, e.Name...)
		b = append(b, 0)
		b = append(b, e.Hash[:]...)
	}
	return b
}

type Signature struct {
	Name  string
	Email string
	When  time.Time // keeps its zone offset
}

type Commit struct {
	Tree      Hash
	Parents   []Hash
	Author    Signature
	Committer Signature
	Message   string
}

// ParseCommit decodes raw commit object data. Unknown headers (gpgsig, encoding,
// mergetag) are skipped, including multi-line continuation headers.
func ParseCommit(data []byte) (*Commit, error) {
	c := &Commit{}
	var haveTree bool
	for {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			// Headers only, no message separator.
			if len(data) > 0 {
				return nil, errors.New("object: commit header not newline-terminated")
			}
			break
		}
		line := data[:nl]
		data = data[nl+1:]
		if len(line) == 0 {
			c.Message = string(data)
			break
		}
		if line[0] == ' ' {
			continue // continuation of a skipped multi-line header
		}
		key, val, _ := bytes.Cut(line, []byte{' '})
		switch string(key) {
		case "tree":
			h, err := ParseHash(string(val))
			if err != nil {
				return nil, err
			}
			c.Tree, haveTree = h, true
		case "parent":
			h, err := ParseHash(string(val))
			if err != nil {
				return nil, err
			}
			c.Parents = append(c.Parents, h)
		case "author":
			s, err := parseSignature(val)
			if err != nil {
				return nil, err
			}
			c.Author = s
		case "committer":
			s, err := parseSignature(val)
			if err != nil {
				return nil, err
			}
			c.Committer = s
		}
	}
	if !haveTree {
		return nil, errors.New("object: commit has no tree")
	}
	return c, nil
}

func parseSignature(b []byte) (Signature, error) {
	var s Signature
	lt := bytes.IndexByte(b, '<')
	gt := bytes.LastIndexByte(b, '>')
	if lt < 0 || gt < lt {
		return s, fmt.Errorf("object: malformed signature %q", b)
	}
	s.Name = string(bytes.TrimSuffix(b[:lt], []byte{' '}))
	s.Email = string(b[lt+1 : gt])
	rest := bytes.Fields(b[gt+1:])
	if len(rest) < 1 {
		return s, fmt.Errorf("object: signature missing timestamp %q", b)
	}
	sec, err := strconv.ParseInt(string(rest[0]), 10, 64)
	if err != nil {
		return s, fmt.Errorf("object: bad signature timestamp %q", b)
	}
	off := 0
	if len(rest) > 1 {
		tz := rest[1]
		if len(tz) != 5 || (tz[0] != '+' && tz[0] != '-') {
			return s, fmt.Errorf("object: bad signature zone %q", b)
		}
		n, err := strconv.Atoi(string(tz[1:]))
		if err != nil {
			return s, fmt.Errorf("object: bad signature zone %q", b)
		}
		off = (n/100)*3600 + (n%100)*60
		if tz[0] == '-' {
			off = -off
		}
	}
	s.When = time.Unix(sec, 0).In(time.FixedZone("", off))
	return s, nil
}

func appendSignature(b []byte, s Signature) []byte {
	b = append(b, s.Name...)
	b = append(b, " <"...)
	b = append(b, s.Email...)
	b = append(b, "> "...)
	b = strconv.AppendInt(b, s.When.Unix(), 10)
	_, off := s.When.Zone()
	sign := byte('+')
	if off < 0 {
		sign = '-'
		off = -off
	}
	b = append(b, ' ', sign)
	return fmt.Appendf(b, "%02d%02d", off/3600, (off%3600)/60)
}

// EncodeCommit encodes a commit in canonical git form.
func EncodeCommit(c *Commit) []byte {
	b := make([]byte, 0, 256+len(c.Message))
	b = append(b, "tree "...)
	b = append(b, c.Tree.String()...)
	b = append(b, '\n')
	for _, p := range c.Parents {
		b = append(b, "parent "...)
		b = append(b, p.String()...)
		b = append(b, '\n')
	}
	b = append(b, "author "...)
	b = appendSignature(b, c.Author)
	b = append(b, "\ncommitter "...)
	b = appendSignature(b, c.Committer)
	b = append(b, "\n\n"...)
	b = append(b, c.Message...)
	return b
}
