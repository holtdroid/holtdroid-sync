package webviewsync

import (
	"archive/zip"
	"compress/flate"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const tailSize = 1 << 20

// remoteFile reads a file over HTTP range requests; the tail, where a zip
// keeps its directory, is fetched once.
type remoteFile struct {
	ctx  context.Context
	hc   *http.Client
	url  string
	size int64
	tail []byte
}

func openRemote(ctx context.Context, hc *http.Client, url string) (*remoteFile, error) {
	f := &remoteFile{ctx: ctx, hc: hc, url: url}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	f.size = resp.ContentLength
	if f.size <= 0 {
		return nil, fmt.Errorf("%s: unknown size", url)
	}
	start := max(0, f.size-tailSize)
	rc, err := f.get(start, f.size-start)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if f.tail, err = io.ReadAll(rc); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *remoteFile) get(off, n int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(f.ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-"+strconv.FormatInt(off+n-1, 10))
	resp, err := f.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: range request: %s", f.url, resp.Status)
	}
	return resp.Body, nil
}

func (f *remoteFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= f.size {
		return 0, io.EOF
	}
	if start := f.size - int64(len(f.tail)); off >= start {
		n := copy(p, f.tail[off-start:])
		if n < len(p) {
			return n, io.EOF
		}
		return n, nil
	}
	n := min(int64(len(p)), f.size-off)
	rc, err := f.get(off, n)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	read, err := io.ReadFull(rc, p[:n])
	if err == nil && n < int64(len(p)) {
		err = io.EOF
	}
	return read, err
}

type entry struct {
	io.Reader
	body io.Closer
}

func (e entry) Close() error { return e.body.Close() }

// openEntry streams the file whose name ends with suffix out of a remote
// zip with one ranged download of its data.
func openEntry(ctx context.Context, hc *http.Client, url, suffix string) (io.ReadCloser, int64, error) {
	f, err := openRemote(ctx, hc, url)
	if err != nil {
		return nil, 0, err
	}
	z, err := zip.NewReader(f, f.size)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", url, err)
	}
	for _, zf := range z.File {
		if !strings.HasSuffix(zf.Name, suffix) {
			continue
		}
		off, err := zf.DataOffset()
		if err != nil {
			return nil, 0, err
		}
		body, err := f.get(off, int64(zf.CompressedSize64))
		if err != nil {
			return nil, 0, err
		}
		switch zf.Method {
		case zip.Store:
			return entry{body, body}, int64(zf.UncompressedSize64), nil
		case zip.Deflate:
			return entry{flate.NewReader(body), body}, int64(zf.UncompressedSize64), nil
		}
		body.Close()
		return nil, 0, fmt.Errorf("%s: %s uses compression method %d", url, zf.Name, zf.Method)
	}
	return nil, 0, fmt.Errorf("%s: no %s", url, suffix)
}
