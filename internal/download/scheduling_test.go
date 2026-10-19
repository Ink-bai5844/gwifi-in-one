package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This transport exercises the complete range-validation/write path without
// sockets. If-Match distinguishes a one-byte payload from the one-byte probe.
func memoryRange(t *testing.T, r *http.Request, data []byte) *http.Response {
	t.Helper()
	start, end := requestedRange(t, r)
	if end >= int64(len(data)) {
		t.Errorf("range exceeds source: %d-%d of %d", start, end, len(data))
		return &http.Response{StatusCode: http.StatusRequestedRangeNotSatisfiable, Body: http.NoBody, Request: r}
	}
	return &http.Response{
		StatusCode: http.StatusPartialContent,
		Header: http.Header{
			"Etag":          {`"stable-source"`},
			"Content-Range": {fmt.Sprintf("bytes %d-%d/%d", start, end, len(data))},
		},
		Body: io.NopCloser(bytes.NewReader(data[start : end+1])), ContentLength: end - start + 1, Request: r,
	}
}

type onCloseBody struct {
	io.ReadCloser
	once    sync.Once
	onClose func()
}

func (b *onCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.onClose)
	return err
}

func TestAsymmetricWorkersKeepTailAvailableAndAssembleExactBytes(t *testing.T) {
	data := bytes.Repeat([]byte("nonuniform bytes\x00\xff"), 200000)
	limits := map[string]int{"wired": 3, "wifi": 1, "inherited": 2}
	var mu sync.Mutex
	active, maximum := map[string]int{}, map[string]int{}
	var assigned []chunk
	var firstBatchBytes int64
	var arrived int
	ready := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exits := []Exit{}
	for _, name := range []string{"wired", "wifi", "inherited"} {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("If-Match") == "" {
				return memoryRange(t, r, data), nil
			}
			if r.Header.Get("If-Match") != `"stable-source"` {
				t.Error("missing representation precondition")
			}
			start, end := requestedRange(t, r)
			mu.Lock()
			active[name]++
			if active[name] > maximum[name] {
				maximum[name] = active[name]
			}
			assigned = append(assigned, chunk{start, end})
			arrived++
			if arrived <= 6 {
				firstBatchBytes += end - start + 1
			}
			if arrived == 6 {
				close(ready)
			}
			mu.Unlock()
			decrement := func() {
				mu.Lock()
				active[name]--
				mu.Unlock()
			}
			// Every configured worker must reach this barrier before any
			// payload can finish. Timing and OS scheduling cannot fake it.
			select {
			case <-ready:
			case <-r.Context().Done():
				decrement()
				return nil, r.Context().Err()
			}
			resp := memoryRange(t, r, data)
			resp.Body = &onCloseBody{ReadCloser: resp.Body, onClose: decrement}
			return resp, nil
		})}
		workers := limits[name]
		if name == "inherited" {
			workers = 0
		}
		exits = append(exits, Exit{Name: name, Client: client, Workers: workers})
	}
	output := filepath.Join(t.TempDir(), "complete.bin")
	err := Download(ctx, exits, Options{URL: "http://source.test/file", Output: output, ChunkSize: 1 << 20, WorkersPerExit: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("assembled bytes differ: %v", err)
	}
	for name, limit := range limits {
		if maximum[name] != limit || active[name] != 0 {
			t.Errorf("exit %s: maximum=%d want=%d, active after completion=%d", name, maximum[name], limit, active[name])
		}
	}
	if firstBatchBytes >= int64(len(data)) {
		t.Fatal("first wave claimed the entire file, leaving no work for faster exits")
	}
	sort.Slice(assigned, func(i, j int) bool { return assigned[i].start < assigned[j].start })
	var next int64
	for _, c := range assigned {
		if c.start != next || c.end < c.start || c.end-c.start+1 > 1<<20 {
			t.Fatalf("gap, overlap, empty, or oversized range: %+v; expected start %d", c, next)
		}
		next = c.end + 1
	}
	if next != int64(len(data)) {
		t.Fatalf("scheduled %d of %d bytes", next, len(data))
	}
	if last := assigned[len(assigned)-1]; last.end-last.start+1 > 256<<10 {
		t.Fatalf("tail was not reduced: %+v", last)
	}
	assertAbsent(t, output+".part")
}

func TestTinyFilesWithManyWorkers(t *testing.T) {
	for _, size := range []int{1, 2, 7, 257} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i % 251)
			}
			var coveredMu sync.Mutex
			covered := make([]int, size)
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("If-Match") != "" {
					start, end := requestedRange(t, r)
					coveredMu.Lock()
					for i := start; i <= end; i++ {
						covered[i]++
					}
					coveredMu.Unlock()
				}
				return memoryRange(t, r, data), nil
			})}
			output := filepath.Join(t.TempDir(), "tiny.bin")
			err := Download(context.Background(), []Exit{{Name: "a", Client: client, Workers: 32}, {Name: "b", Client: client, Workers: 16}}, Options{URL: "http://source.test/file", Output: output, ChunkSize: 3}, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(output)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("assembled bytes differ: %v", err)
			}
			for i, count := range covered {
				if count != 1 {
					t.Errorf("byte %d requested %d times", i, count)
				}
			}
		})
	}
}

func TestCancellationStopsAllAsymmetricWorkers(t *testing.T) {
	data := bytes.Repeat([]byte("abcd"), 1<<18)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := make(chan struct{})
	var started, stopped atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("If-Match") == "" {
			return memoryRange(t, r, data), nil
		}
		if started.Add(1) == 3 {
			close(ready)
		}
		<-r.Context().Done()
		stopped.Add(1)
		return nil, r.Context().Err()
	})}
	output := filepath.Join(t.TempDir(), "canceled.bin")
	done := make(chan error, 1)
	go func() {
		done <- Download(ctx, []Exit{{Name: "a", Client: client, Workers: 2}, {Name: "b", Client: client, Workers: 1}}, Options{URL: "http://source.test/file", Output: output, ChunkSize: 64 << 10}, nil)
	}()
	select {
	case <-ready:
		cancel()
	case <-ctx.Done():
		t.Error("workers did not all start")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if started.Load() != 3 || stopped.Load() != 3 {
		t.Fatalf("started=%d stopped=%d", started.Load(), stopped.Load())
	}
	assertAbsent(t, output)
	if _, err := os.Stat(output + ".part"); err != nil {
		t.Fatalf("partial file not retained: %v", err)
	}
}

func TestProbesRunConcurrentlyAndFailureCancelsPeers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{}, 2)
	failure := errors.New("source refused probe")
	var peerStopped atomic.Bool
	a := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-r.Context().Done()
		peerStopped.Store(true)
		return nil, r.Context().Err()
	})}
	b := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		started <- struct{}{}
		for i := 0; i < 2; i++ {
			select {
			case <-started:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return nil, failure
	})}
	output := filepath.Join(t.TempDir(), "uncreated.bin")
	err := Download(ctx, []Exit{{Name: "a", Client: a}, {Name: "b", Client: b}}, Options{URL: "http://source.test/file", Output: output}, nil)
	if !errors.Is(err, failure) || !peerStopped.Load() {
		t.Fatalf("probe failure not propagated or peer not stopped: %v, stopped=%t", err, peerStopped.Load())
	}
	assertAbsent(t, output)
	assertAbsent(t, output+".part")
}

func TestInvalidExitWorkerCountsFailBeforeRequest(t *testing.T) {
	for _, count := range []int{-1, 33} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				t.Error("invalid worker count made an HTTP request")
				return nil, errors.New("unexpected request")
			})}
			output := filepath.Join(t.TempDir(), "absent.bin")
			err := Download(context.Background(), []Exit{{Name: "a", Client: client, Workers: count}}, Options{URL: "http://source.test/file", Output: output}, nil)
			if err == nil {
				t.Fatal("invalid worker count accepted")
			}
			assertAbsent(t, output)
			assertAbsent(t, output+".part")
		})
	}
}
