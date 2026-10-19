// Package download assembles an HTTP file from independently routed clients.
// Callers are responsible for binding each client's transport to its exit.
package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Exit struct {
	Name    string
	Client  *http.Client
	Workers int // Zero inherits Options.WorkersPerExit; otherwise 1 through 32.
}

type Options struct {
	URL            string
	Output         string
	ChunkSize      int64 // Maximum range size; zero uses 4 MiB. Final ranges may be smaller.
	WorkersPerExit int   // Zero uses one worker per exit; maximum 32.
}

type Progress struct {
	Completed   int64
	Total       int64
	BytesByExit map[string]int64
}

type metadata struct {
	total       int64
	etag        string
	modified    time.Time
	modifiedRaw string
	strongDate  bool
	resolvedURL string
	firstByte   byte
}

type validator struct {
	etag     string
	modified time.Time
	dateRaw  string
}

type chunk struct{ start, end int64 }

// Download downloads an immutable representation into Output + ".part" and
// publishes it only after every range succeeds. Existing files are never
// overwritten. Version 1 does not resume: a pre-existing .part is an error.
//
// Callbacks are serialized and receive independent snapshots. Completed counts
// payload bytes written to .part (not probe bytes); a failed chunk may therefore
// contribute bytes. The final filename, not a 100% callback, signals success.
//
// Publication uses a hard link for an atomic, no-overwrite operation on Windows
// and Unix. If the destination filesystem does not support hard links, a complete
// .part is retained and an error is returned; there is no unsafe copy fallback.
func Download(ctx context.Context, exits []Exit, opts Options, onProgress func(Progress)) error {
	if err := validateOptions(exits, &opts); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(opts.Output); err == nil {
		return fmt.Errorf("output already exists: %s", opts.Output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect output: %w", err)
	}
	partPath := opts.Output + ".part"
	if _, err := os.Lstat(partPath); err == nil {
		return fmt.Errorf("partial file already exists (v1 cannot resume): %s", partPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect partial output: %w", err)
	}
	metas, err := probeExits(ctx, exits, opts.URL)
	if err != nil {
		return err
	}
	for i, m := range metas {
		if i > 0 && (m.total != metas[0].total || m.firstByte != metas[0].firstByte || m.resolvedURL != metas[0].resolvedURL) {
			return fmt.Errorf("exit %q returned a different file length, first byte, or redirect target", exits[i].Name)
		}
	}
	v, err := commonValidator(metas)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(partPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("create partial output (v1 cannot resume): %w", err)
	}
	defer file.Close()
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	total := metas[0].total
	var progressMu sync.Mutex
	progress := Progress{Total: total, BytesByExit: make(map[string]int64, len(exits))}
	for _, exit := range exits {
		progress.BytesByExit[exit.Name] = 0
	}
	report := func(name string, count int64) {
		progressMu.Lock()
		defer progressMu.Unlock()
		if count != 0 {
			progress.Completed += count
			progress.BytesByExit[name] += count
		}
		if onProgress != nil {
			snapshot := Progress{Completed: progress.Completed, Total: total, BytesByExit: make(map[string]int64, len(exits))}
			for key, value := range progress.BytesByExit {
				snapshot.BytesByExit[key] = value
			}
			onProgress(snapshot)
		}
	}
	report("", 0)
	// Workers claim chunks on demand: a faster exit naturally takes more work.
	workerCounts := make([]int, len(exits))
	var totalWorkers int64
	for i, exit := range exits {
		workerCounts[i] = exit.Workers
		if workerCounts[i] == 0 {
			workerCounts[i] = opts.WorkersPerExit
		}
		totalWorkers += int64(workerCounts[i])
	}
	var queueMu sync.Mutex
	var next int64
	nextChunk := func() (chunk, bool) {
		queueMu.Lock()
		defer queueMu.Unlock()
		if next == total {
			return chunk{}, false
		}
		remaining := total - next
		// Keep roughly two rounds of work available as the queue runs down,
		// instead of letting slow exits claim the entire tail in large ranges.
		// A 256 KiB floor bounds request overhead; already assigned ranges are
		// never split, canceled, or downloaded again. Tiny files stay one range.
		divisor := 2 * totalWorkers
		size := remaining / divisor
		if remaining%divisor != 0 {
			size++
		}
		if size < 256<<10 {
			size = 256 << 10
		}
		if size > opts.ChunkSize {
			size = opts.ChunkSize
		}
		if size > remaining {
			size = remaining
		}
		c := chunk{start: next, end: next + size - 1}
		next += size
		return c, true
	}
	var workers sync.WaitGroup
	var firstErr error
	var errorOnce sync.Once
	for exitIndex, e := range exits {
		exit := e
		for i := 0; i < workerCounts[exitIndex]; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for workCtx.Err() == nil {
					c, ok := nextChunk()
					if !ok {
						return
					}
					err := fetchChunk(workCtx, exit.Client, opts.URL, metas[0], v, c, file, func(n int64) { report(exit.Name, n) })
					if err != nil {
						errorOnce.Do(func() {
							firstErr = fmt.Errorf("exit %q, bytes %d-%d: %w", exit.Name, c.start, c.end, err)
							cancel()
						})
						return
					}
				}
			}()
		}
	}
	workers.Wait()
	if firstErr != nil {
		return fmt.Errorf("download failed; partial file retained at %s: %w", partPath, firstErr)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("partial file retained at %s: %w", partPath, err)
	}
	if progress.Completed != total {
		return fmt.Errorf("incomplete download: wrote %d of %d bytes; partial file retained", progress.Completed, total)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync partial output: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close partial output: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(partPath, opts.Output); err != nil {
		return fmt.Errorf("publish without overwriting failed; complete file retained at %s (filesystem must support hard links): %w", partPath, err)
	}
	if err := os.Remove(partPath); err != nil {
		return fmt.Errorf("download complete at %s, but could not remove the additional .part link: %w", opts.Output, err)
	}
	return nil
}

func validateOptions(exits []Exit, opts *Options) error {
	if len(exits) == 0 {
		return errors.New("at least one exit is required")
	}
	names := make(map[string]bool, len(exits))
	for _, exit := range exits {
		if strings.TrimSpace(exit.Name) == "" || names[exit.Name] || exit.Client == nil {
			return errors.New("each exit needs a unique nonempty name and an HTTP client")
		}
		if exit.Workers < 0 || exit.Workers > 32 {
			return fmt.Errorf("exit %q workers must be between 1 and 32 (zero inherits the default)", exit.Name)
		}
		names[exit.Name] = true
	}
	u, err := url.Parse(opts.URL)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("download URL must be an absolute HTTP or HTTPS URL without embedded credentials")
	}
	if strings.TrimSpace(opts.Output) == "" {
		return errors.New("output filename is required")
	}
	if opts.ChunkSize < 0 || opts.WorkersPerExit < 0 || opts.WorkersPerExit > 32 {
		return errors.New("chunk size must be positive; workers per exit must be between 1 and 32 (zero selects defaults)")
	}
	if opts.ChunkSize == 0 {
		opts.ChunkSize = 4 << 20
	}
	if opts.WorkersPerExit == 0 {
		opts.WorkersPerExit = 1
	}
	return nil
}

// Probe independently routed exits concurrently, without opening a partial file
// until all have agreed on the representation. A failed probe cancels its peers.
func probeExits(ctx context.Context, exits []Exit, rawURL string) ([]metadata, error) {
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	metas := make([]metadata, len(exits))
	var workers sync.WaitGroup
	var firstErr error
	var errorOnce sync.Once
	for i, exit := range exits {
		workers.Add(1)
		go func() {
			defer workers.Done()
			m, err := probe(probeCtx, exit.Client, rawURL)
			if err != nil {
				errorOnce.Do(func() {
					firstErr = fmt.Errorf("probe exit %q: %w", exit.Name, err)
					cancel()
				})
				return
			}
			metas[i] = m
		}()
	}
	workers.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return metas, ctx.Err()
}

func rangeRequest(ctx context.Context, client *http.Client, rawURL string, c chunk, v validator) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, errors.New("could not construct HTTP request")
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", c.start, c.end))
	req.Header.Set("Accept-Encoding", "identity")
	if v.etag != "" {
		req.Header.Set("If-Match", v.etag)
	} else if v.dateRaw != "" {
		req.Header.Set("If-Unmodified-Since", v.dateRaw)
	}
	resp, err := client.Do(req)
	if err != nil {
		// url.Error includes the full URL, possibly containing a signed token.
		var requestErr *url.Error
		if errors.As(err, &requestErr) {
			err = requestErr.Err
		}
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	return resp, nil
}

var contentRangePattern = regexp.MustCompile(`^bytes ([0-9]+)-([0-9]+)/([0-9]+)$`)

func validateRange(resp *http.Response, expected chunk, total int64) (int64, error) {
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("server must return HTTP 206 for Range, got %d", resp.StatusCode)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return 0, errors.New("server returned encoded content despite Accept-Encoding: identity")
	}
	if resp.Uncompressed {
		return 0, errors.New("HTTP client transparently decompressed the ranged response")
	}
	parts := contentRangePattern.FindStringSubmatch(resp.Header.Get("Content-Range"))
	if len(parts) != 4 {
		return 0, errors.New("missing or invalid Content-Range")
	}
	values := [3]int64{}
	for i := range values {
		value, err := strconv.ParseInt(parts[i+1], 10, 64)
		if err != nil {
			return 0, errors.New("Content-Range number is out of bounds")
		}
		values[i] = value
	}
	if values[0] != expected.start || values[1] != expected.end || values[2] <= values[1] || values[2] <= 0 || (total > 0 && values[2] != total) {
		return 0, errors.New("Content-Range does not match the requested bytes or file length")
	}
	if resp.ContentLength >= 0 && resp.ContentLength != expected.end-expected.start+1 {
		return 0, errors.New("Content-Length does not match the requested range")
	}
	return values[2], nil
}

func responseMetadata(resp *http.Response, total int64) metadata {
	m := metadata{total: total, etag: strings.TrimSpace(resp.Header.Get("ETag")), modifiedRaw: resp.Header.Get("Last-Modified")}
	m.modified, _ = http.ParseTime(m.modifiedRaw)
	date, err := http.ParseTime(resp.Header.Get("Date"))
	// RFC 9110 section 8.8.2.2 permits a cached Last-Modified validator to
	// be treated as strong with a sufficient Date gap. Keep the older RFC
	// 7232's conservative 60s margin rather than assuming synchronized clocks.
	m.strongDate = err == nil && !m.modified.IsZero() && date.Sub(m.modified) >= time.Minute
	if resp.Request != nil && resp.Request.URL != nil {
		m.resolvedURL = resp.Request.URL.String()
	}
	return m
}

func probe(ctx context.Context, client *http.Client, rawURL string) (metadata, error) {
	resp, err := rangeRequest(ctx, client, rawURL, chunk{0, 0}, validator{})
	if err != nil {
		return metadata{}, err
	}
	defer resp.Body.Close()
	total, err := validateRange(resp, chunk{0, 0}, 0)
	if err != nil {
		return metadata{}, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2))
	if err != nil {
		return metadata{}, fmt.Errorf("read probe body: %w", err)
	}
	if len(body) != 1 {
		return metadata{}, errors.New("probe body must contain exactly one byte")
	}
	m := responseMetadata(resp, total)
	m.firstByte = body[0]
	return m, nil
}

func isStrongETag(tag string) bool {
	if len(tag) < 2 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		return false
	}
	for i := 1; i < len(tag)-1; i++ {
		if tag[i] == '"' || tag[i] < 0x21 || tag[i] == 0x7f {
			return false
		}
	}
	return true
}

func commonValidator(metas []metadata) (validator, error) {
	first := metas[0]
	if isStrongETag(first.etag) {
		for _, m := range metas[1:] {
			if m.etag != first.etag {
				return validator{}, errors.New("exits do not share the same strong ETag; refusing to combine content")
			}
		}
		return validator{etag: first.etag}, nil
	}
	if first.strongDate {
		for _, m := range metas {
			if !m.strongDate || !m.modified.Equal(first.modified) || m.etag != first.etag {
				return validator{}, errors.New("exits do not share a consistent strong Last-Modified validator")
			}
		}
		return validator{modified: first.modified, dateRaw: first.modified.UTC().Format(http.TimeFormat)}, nil
	}
	return validator{}, errors.New("server needs a common strong ETag or Last-Modified at least 60 seconds older than Date; refusing unsafe range assembly")
}

func fetchChunk(ctx context.Context, client *http.Client, rawURL string, meta metadata, v validator, c chunk, file *os.File, report func(int64)) error {
	resp, err := rangeRequest(ctx, client, rawURL, c, v)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := validateRange(resp, c, meta.total); err != nil {
		return err
	}
	actual := responseMetadata(resp, meta.total)
	if actual.resolvedURL != meta.resolvedURL {
		return errors.New("redirect target changed during download")
	}
	if v.etag != "" {
		if actual.etag != v.etag {
			return errors.New("strong ETag changed or disappeared during download")
		}
	} else if !actual.strongDate || !actual.modified.Equal(v.modified) || actual.etag != meta.etag {
		return errors.New("Last-Modified validator changed or became weak during download")
	}
	remaining := c.end - c.start + 1
	offset := c.start
	buffer := make([]byte, 64<<10)
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		readSize := int64(len(buffer))
		if remaining < readSize {
			readSize = remaining
		}
		n, readErr := resp.Body.Read(buffer[:readSize])
		if n > 0 {
			written, writeErr := file.WriteAt(buffer[:n], offset)
			if written > 0 {
				offset += int64(written)
				remaining -= int64(written)
				report(int64(written))
			}
			if writeErr != nil {
				return fmt.Errorf("write chunk: %w", writeErr)
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if readErr != nil {
			if readErr == io.EOF && remaining == 0 {
				return nil
			}
			if readErr == io.EOF {
				readErr = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("read chunk: %w", readErr)
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	// Read to the real end: exact-sized bytes followed by a transport error or
	// extra bytes are a failure, not a successfully completed chunk.
	var extra [1]byte
	n, err := io.ReadFull(resp.Body, extra[:])
	if n != 0 {
		return errors.New("response body exceeds its requested range")
	}
	if err != io.EOF {
		if err == nil {
			err = errors.New("response body did not terminate")
		}
		return fmt.Errorf("finish reading chunk: %w", err)
	}
	return nil
}
