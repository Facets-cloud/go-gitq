package gitq

import (
	"archive/tar"
	"context"
	"io"
	"path"
	"strings"

	"github.com/facets-cloud/go-gitq/internal/object"
)

// downloadChunk is how many files are fetched before being written to the tar,
// bounding memory for large directories.
const downloadChunk = blobBatch * blobParallel

func (r *repo) download(ctx context.Context, ref, dir string, w io.Writer) error {
	dir = cleanPath(dir)
	_, c, err := r.rootOf(ctx, ref)
	if err != nil {
		return err
	}
	files, err := r.files(ctx, c.Tree, dir)
	if err != nil {
		return err
	}
	mtime := c.Committer.When
	tw := tar.NewWriter(w)
	for i := 0; i < len(files); i += downloadChunk {
		chunk := files[i:min(i+downloadChunk, len(files))]
		hashes := make([]object.Hash, len(chunk))
		for j, f := range chunk {
			if hashes[j], err = object.ParseHash(f.SHA); err != nil {
				return err
			}
		}
		blobs, err := r.fetchBlobs(ctx, hashes)
		if err != nil {
			return err
		}
		for j, f := range chunk {
			name := relName(dir, f.Path)
			data := blobs[hashes[j]]
			hdr := &tar.Header{Name: name, ModTime: mtime, Format: tar.FormatPAX}
			switch f.Mode {
			case object.ModeSymlink:
				hdr.Typeflag = tar.TypeSymlink
				hdr.Linkname = string(data)
				hdr.Mode = 0o777
				data = nil
			case object.ModeExec:
				hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeReg, 0o755, int64(len(data))
			default:
				hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeReg, 0o644, int64(len(data))
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if _, err := tw.Write(data); err != nil {
				return err
			}
		}
	}
	return tw.Close()
}

// relName returns a file path relative to dir; a dir that is itself the file yields its base name.
func relName(dir, p string) string {
	if dir == "" {
		return p
	}
	if p == dir {
		return path.Base(p)
	}
	return strings.TrimPrefix(p, dir+"/")
}
