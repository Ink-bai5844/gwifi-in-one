package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func namedClient(server *httptest.Server, name string) *http.Client {
	transport := server.Client().Transport
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			copy := req.Clone(req.Context())
			copy.Header.Set("X-Test-Exit", name)
			return transport.RoundTrip(copy)
		}),
	}
}

func requestedRange(t *testing.T, r *http.Request) (int64, int64) {
	t.Helper()
	var start, end int64
	if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start {
		t.Errorf("invalid Range: %q", r.Header.Get("Range"))
	}
	if r.Header.Get("Accept-Encoding") != "identity" {
		t.Error("missing Accept-Encoding: identity")
	}
	return start, end
}

func writeRange(t *testing.T, w http.ResponseWriter, r *http.Request, data []byte, tag string) {
	t.Helper()
	start, end := requestedRange(t, r)
	w.Header().Set("ETag", tag)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(data[start : end+1])
}

func assertAbsent(t *testing.T, name string) {
	t.Helper()
	if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected absent path %s, got %v", name, err)
	}
}

func TestDownloadUsesBothExitsAndAssemblesExactBytes(t *testing.T) {
	data := bytes.Repeat([]byte("a nonuniform file body\x00\xff"), 4000)
	var started atomic.Int32
	ready := make(chan struct{})
	var seenMu sync.Mutex
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := requestedRange(t, r)
		if start != 0 || end != 0 {
			if r.Header.Get("If-Match") != `"version-1"` {
				t.Error("payload request missing If-Match")
			}
			seenMu.Lock()
			first := !seen[r.Header.Get("X-Test-Exit")]
			seen[r.Header.Get("X-Test-Exit")] = true
			seenMu.Unlock()
			if first && started.Add(1) == 2 {
				close(ready)
			}
			select {
			case <-ready:
			case <-r.Context().Done():
				return
			}
		}
		writeRange(t, w, r, data, `"version-1"`)
	}))
	defer server.Close()
	output := filepath.Join(t.TempDir(), "file.bin")
	var last Progress
	err := Download(context.Background(), []Exit{{Name: "wire", Client: namedClient(server, "wire")}, {Name: "wifi", Client: namedClient(server, "wifi")}}, Options{
		URL: server.URL, Output: output, ChunkSize: 4096,
	}, func(p Progress) {
		if p.Completed < last.Completed || p.Completed > p.Total {
			t.Error("invalid progress")
		}
		if _, exists := p.BytesByExit["callback-mutation"]; exists {
			t.Error("callback map is shared with internal state")
		}
		last = p
		p.BytesByExit["callback-mutation"] = 99
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("wrong assembled file: %v", err)
	}
	if last.Completed != int64(len(data)) || last.BytesByExit["wire"] == 0 || last.BytesByExit["wifi"] == 0 || last.BytesByExit["wire"]+last.BytesByExit["wifi"] != int64(len(data)) {
		t.Fatalf("wrong per-exit stats: %+v", last)
	}
	assertAbsent(t, output+".part")
}

func TestProbeRejectsUnsafeSources(t *testing.T) {
	for _, scenario := range []string{"ignores-range", "no-validator", "weak-etag", "different-etag", "different-length", "different-first-byte", "encoded", "wrong-range", "short-probe", "overlong-probe", "recent-last-modified"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tag := `"same"`
				total := 16
				body := []byte("a")
				contentRange := "bytes 0-0/16"
				switch scenario {
				case "ignores-range":
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("whole file"))
					return
				case "no-validator":
					tag = ""
				case "weak-etag":
					tag = `W/"weak"`
				case "different-etag":
					tag = `"` + r.Header.Get("X-Test-Exit") + `"`
				case "different-length":
					if r.Header.Get("X-Test-Exit") == "b" {
						total++
					}
					contentRange = fmt.Sprintf("bytes 0-0/%d", total)
				case "different-first-byte":
					if r.Header.Get("X-Test-Exit") == "b" {
						body = []byte("b")
					}
				case "encoded":
					w.Header().Set("Content-Encoding", "gzip")
				case "wrong-range":
					contentRange = "bytes 1-1/16"
				case "short-probe":
					body = nil
				case "overlong-probe":
					body = []byte("ab")
				case "recent-last-modified":
					tag = ""
					w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
					w.Header().Set("Last-Modified", time.Now().Add(-5*time.Second).UTC().Format(http.TimeFormat))
				}
				w.Header().Set("ETag", tag)
				w.Header().Set("Content-Range", contentRange)
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(body)
			}))
			defer server.Close()
			output := filepath.Join(t.TempDir(), "file.bin")
			err := Download(context.Background(), []Exit{{Name: "a", Client: namedClient(server, "a")}, {Name: "b", Client: namedClient(server, "b")}}, Options{URL: server.URL, Output: output}, nil)
			if err == nil {
				t.Fatal("unsafe source accepted")
			}
			assertAbsent(t, output)
			assertAbsent(t, output+".part")
		})
	}
}

func TestFailedPayloadRetainsPartWithoutPublishing(t *testing.T) {
	for _, scenario := range []string{"short-body", "long-body", "changed-etag", "wrong-range", "ignores-range", "precondition-failed"} {
		t.Run(scenario, func(t *testing.T) {
			data := bytes.Repeat([]byte("abcd"), 100)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				start, end := requestedRange(t, r)
				if start == 0 && end == 0 {
					writeRange(t, w, r, data, `"same"`)
					return
				}
				w.Header().Set("ETag", `"same"`)
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
				body := data[start : end+1]
				switch scenario {
				case "short-body":
					w.Header().Set("Content-Length", fmt.Sprint(len(body)))
					body = body[:len(body)-1]
				case "long-body":
					body = append(append([]byte{}, body...), 'x')
				case "changed-etag":
					w.Header().Set("ETag", `"changed"`)
				case "wrong-range":
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start+1, end+1, len(data)+1))
				case "ignores-range":
					w.WriteHeader(http.StatusOK)
					return
				case "precondition-failed":
					w.WriteHeader(http.StatusPreconditionFailed)
					return
				}
				w.WriteHeader(http.StatusPartialContent)
				// Force chunked framing for long bodies so the body-length check,
				// rather than a Content-Length header, must detect the extra byte.
				if scenario == "long-body" {
					w.(http.Flusher).Flush()
				}
				_, _ = w.Write(body)
			}))
			defer server.Close()
			output := filepath.Join(t.TempDir(), "file.bin")
			err := Download(context.Background(), []Exit{{Name: "a", Client: namedClient(server, "a")}}, Options{URL: server.URL, Output: output, ChunkSize: 100}, nil)
			if err == nil {
				t.Fatal("bad payload accepted")
			}
			assertAbsent(t, output)
			if _, err := os.Stat(output + ".part"); err != nil {
				t.Fatalf("partial file missing: %v", err)
			}
		})
	}
}

func TestCancellationRetainsPartialFile(t *testing.T) {
	data := bytes.Repeat([]byte("abcd"), 10000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeRange(t, w, r, data, `"same"`)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := filepath.Join(t.TempDir(), "file.bin")
	err := Download(ctx, []Exit{{Name: "a", Client: namedClient(server, "a")}}, Options{URL: server.URL, Output: output, ChunkSize: 1000}, func(p Progress) {
		if p.Completed > 0 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	assertAbsent(t, output)
	if info, err := os.Stat(output + ".part"); err != nil || info.Size() == 0 {
		t.Fatalf("nonempty partial file not retained: %v", err)
	}
}

func TestNeverOverwritesExistingFiles(t *testing.T) {
	for _, scenario := range []string{"existing-output", "existing-part", "output-created-during-download"} {
		t.Run(scenario, func(t *testing.T) {
			data := []byte("a reasonably small test file")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeRange(t, w, r, data, `"same"`)
			}))
			defer server.Close()
			output := filepath.Join(t.TempDir(), "file.bin")
			protected := output
			if scenario == "existing-part" {
				protected += ".part"
			}
			if scenario != "output-created-during-download" {
				if err := os.WriteFile(protected, []byte("keep me"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var once sync.Once
			err := Download(context.Background(), []Exit{{Name: "a", Client: namedClient(server, "a")}}, Options{URL: server.URL, Output: output, ChunkSize: 10}, func(p Progress) {
				if scenario == "output-created-during-download" && p.Completed > 0 {
					once.Do(func() {
						if err := os.WriteFile(output, []byte("keep me"), 0600); err != nil {
							t.Error(err)
						}
					})
				}
			})
			if err == nil {
				t.Fatal("expected existing file error")
			}
			got, readErr := os.ReadFile(protected)
			if readErr != nil || string(got) != "keep me" {
				t.Fatalf("protected file was changed: %q %v", got, readErr)
			}
			if scenario == "output-created-during-download" {
				part, err := os.ReadFile(output + ".part")
				if err != nil || !bytes.Equal(part, data) {
					t.Fatalf("complete part not retained: %v", err)
				}
			}
		})
	}
}

func TestStrongLastModifiedFallback(t *testing.T) {
	data := []byte("stable date validator")
	modified := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	date := modified.Add(time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := requestedRange(t, r)
		if end > 0 && r.Header.Get("If-Unmodified-Since") != modified.Format(http.TimeFormat) {
			t.Error("missing If-Unmodified-Since")
		}
		w.Header().Set("Date", date.Format(http.TimeFormat))
		w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer server.Close()
	output := filepath.Join(t.TempDir(), "file.bin")
	if err := Download(context.Background(), []Exit{{Name: "a", Client: namedClient(server, "a")}}, Options{URL: server.URL, Output: output, ChunkSize: 10}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("wrong contents: %v", err)
	}
}

type bytesThenError struct {
	data []byte
	err  error
}

func (r *bytesThenError) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

func (*bytesThenError) Close() error { return nil }

func TestTransportErrorAfterExactBytesIsNotIgnored(t *testing.T) {
	broken := errors.New("simulated broken transport")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		start, end := requestedRange(t, r)
		var body io.ReadCloser = io.NopCloser(strings.NewReader("a"))
		if end != 0 {
			body = &bytesThenError{data: []byte("abcd"), err: broken}
		}
		return &http.Response{
			StatusCode: http.StatusPartialContent,
			Header:     http.Header{"Etag": {`"same"`}, "Content-Range": {fmt.Sprintf("bytes %d-%d/4", start, end)}},
			Body:       body, ContentLength: end - start + 1, Request: r,
		}, nil
	})}
	output := filepath.Join(t.TempDir(), "file.bin")
	err := Download(context.Background(), []Exit{{Name: "a", Client: client}}, Options{URL: "http://example.test/file", Output: output}, nil)
	if !errors.Is(err, broken) {
		t.Fatalf("body transport error ignored: %v", err)
	}
	assertAbsent(t, output)
}
