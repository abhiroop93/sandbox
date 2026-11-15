// srvm-snap: in-sandbox workspace snapshot tool (docs/architecture.md D7). Static binary, injected via init container.
// It never holds storage credentials: the server hands it short-lived presigned URLs.
//
//	srvm-snap install <dst>                      copy self to dst (init container)
//	srvm-snap pack <dir> <outdir>                tar|zstd → 64 MiB chunks in outdir; prints {"chunks":N,"bytes":B}
//	srvm-snap upload <outdir> <urls.b64>         PUT chunk i to url[i] in parallel, then remove outdir
//	srvm-snap hydrate <dir> <urls.b64>           GET chunks in parallel, stream-decode into dir (replaces contents)
//	srvm-snap guard arm|disarm <marker>          mark the pod as holding an unsaved workspace (or not)
//	srvm-snap prestop <marker> <maxSeconds>      preStop hook: while the marker exists, hold shutdown so the server
//	                                             can save the workspace; returns at once for unarmed pods
package main

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

const chunkSize = 64 << 20
const snapDir = ".srvm-snap"

func main() {
	if len(os.Args) < 2 {
		die("usage: srvm-snap install|pack|upload|hydrate|guard|prestop ...")
	}
	var err error
	switch os.Args[1] {
	case "install":
		err = install(arg(2))
	case "pack":
		err = pack(arg(2), arg(3))
	case "upload":
		err = upload(arg(2), arg(3))
	case "hydrate":
		err = hydrate(arg(2), arg(3))
	case "guard":
		err = guard(arg(2), arg(3))
	case "prestop":
		var max int
		if _, err = fmt.Sscan(arg(3), &max); err == nil {
			prestop(arg(2), time.Duration(max)*time.Second, 500*time.Millisecond)
		}
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		die(err.Error())
	}
}

func arg(i int) string {
	if len(os.Args) <= i {
		die("missing argument")
	}
	return os.Args[i]
}

// guard arms/disarms the shutdown hold. The marker lives outside /workspace so it is never snapshotted.
func guard(op, marker string) error {
	switch op {
	case "arm":
		return os.WriteFile(marker, []byte("armed\n"), 0o644)
	case "disarm":
		if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return fmt.Errorf("guard: unknown op %q (arm|disarm)", op)
}

// prestop holds pod shutdown while the marker exists (the server removes it once the workspace is saved, or when it
// releases the pod itself). Bounded by max; the kubelet's terminationGracePeriodSeconds is the hard cap anyway.
// Never fails: a broken hook must not block deletion.
func prestop(marker string, max, every time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err != nil {
			return
		}
		time.Sleep(every)
	}
}

func die(msg string) { fmt.Fprintln(os.Stderr, "srvm-snap:", msg); os.Exit(1) }

func install(dst string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o755)
}

// chunkWriter splits a stream into fixed-size files part.NNNNN.
type chunkWriter struct {
	dir   string
	n     int
	cur   *os.File
	left  int64
	total int64
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if w.cur == nil || w.left == 0 {
			if w.cur != nil {
				if err := w.cur.Close(); err != nil {
					return written, err
				}
			}
			f, err := os.Create(filepath.Join(w.dir, fmt.Sprintf("part.%05d", w.n)))
			if err != nil {
				return written, err
			}
			w.cur, w.left = f, chunkSize
			w.n++
		}
		k := int64(len(p))
		if k > w.left {
			k = w.left
		}
		m, err := w.cur.Write(p[:k])
		written += m
		w.left -= int64(m)
		w.total += int64(m)
		p = p[m:]
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func pack(dir, out string) error {
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		return err
	}
	cw := &chunkWriter{dir: out}
	zw, err := zstd.NewWriter(cw, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(4))
	if err != nil {
		return err
	}
	tw := tar.NewWriter(zw)
	absOut, _ := filepath.Abs(out)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		ap, _ := filepath.Abs(p)
		if ap == absOut || strings.HasPrefix(ap, absOut+string(os.PathSeparator)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if cw.cur != nil {
		if err := cw.cur.Close(); err != nil {
			return err
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]int64{"chunks": int64(cw.n), "bytes": cw.total})
}

func decodeURLs(b64 string) ([]string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	var urls []string
	return urls, json.Unmarshal(raw, &urls)
}

var client = &http.Client{Timeout: 10 * time.Minute}

func parallel(n int, fn func(i int) error) error {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			for attempt := 0; attempt < 3; attempt++ {
				if errs[i] = fn(i); errs[i] == nil {
					return
				}
				time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			}
		}(i)
	}
	wg.Wait()
	return errors.Join(errs...)
}

func upload(out, b64 string) error {
	urls, err := decodeURLs(b64)
	if err != nil {
		return err
	}
	parts, _ := filepath.Glob(filepath.Join(out, "part.*"))
	sort.Strings(parts)
	if len(parts) != len(urls) {
		return fmt.Errorf("have %d chunks but %d urls", len(parts), len(urls))
	}
	err = parallel(len(parts), func(i int) error {
		b, err := os.ReadFile(parts[i])
		if err != nil {
			return err
		}
		req, _ := http.NewRequest(http.MethodPut, urls[i], bytes.NewReader(b))
		req.ContentLength = int64(len(b))
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("PUT chunk %d: %s", i, resp.Status)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(out)
}

func hydrate(dir, b64 string) error {
	urls, err := decodeURLs(b64)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, snapDir+"-in")
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	err = parallel(len(urls), func(i int) error {
		resp, err := client.Get(urls[i])
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("GET chunk %d: %s", i, resp.Status)
		}
		f, err := os.Create(filepath.Join(tmp, fmt.Sprintf("part.%05d", i)))
		if err != nil {
			return err
		}
		_, err = io.Copy(f, resp.Body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	})
	if err != nil {
		return err
	}
	readers := make([]io.Reader, len(urls))
	for i := range urls {
		f, err := os.Open(filepath.Join(tmp, fmt.Sprintf("part.%05d", i)))
		if err != nil {
			return err
		}
		defer f.Close()
		readers[i] = f
	}
	zr, err := zstd.NewReader(io.MultiReader(readers...), zstd.WithDecoderConcurrency(4))
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	root, _ := filepath.Abs(dir)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target := filepath.Join(root, filepath.FromSlash(hdr.Name))
		if !strings.HasPrefix(target, root+string(os.PathSeparator)) { // no path traversal out of the workspace
			return fmt.Errorf("unsafe path in snapshot: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)|0o700); err != nil {
				return err
			}
		case tar.TypeSymlink:
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeReg:
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
		}
	}
	return os.RemoveAll(tmp)
}
