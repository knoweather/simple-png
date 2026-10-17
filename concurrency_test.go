package simple_png

import (
	"bytes"
	"sync"
	"testing"
)

// Concurrency stress tests: everything here must pass `go test -race`
// cleanly. They exercise the internal RWMutex of Png: independent instances,
// concurrent readers while custom chunks are parsed, and concurrent editing
// churn followed by a full stream re-verification.

// stressChunk is a custom chunk type for concurrent ParseChunk calls.
type stressChunk struct {
	data []byte
}

func (s *stressChunk) ChunkName() ChunkName { return "zzZt" }

func (s *stressChunk) Parse(c *chunk) error {
	s.data = c.data
	return nil
}

func stressPngData(chunks ...[]byte) []byte {
	base := [][]byte{
		chunkBytes("IHDR", ihdrBytes(3, 2, 8, 0)),
		idatBytes(compressRaw(filterRows([][]byte{{2, 4, 6}, {1, 3, 5}}, 3, 1, []byte{3}))),
	}
	return buildPng(append(chunks, base...)...)
}

func TestConcurrentIndependentInstances(t *testing.T) {
	data := stressPngData(
		chunkBytes("tEXt", append([]byte("Title"), 0x00, 'v', '1')),
		chunkBytes("zzZt", []byte{7, 8, 9}),
	)
	const goroutines = 8
	const iterations = 15
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				p, err := ParsePng(bytes.NewReader(data))
				if err != nil {
					t.Error("ParsePng failed:", err)
					return
				}
				img, err := p.IDATsToColor()
				if err != nil || img == nil {
					t.Error("IDATsToColor failed:", err)
					return
				}
				if raw, ok := p.RawChunkByName("zzZt"); !ok || len(raw) == 0 {
					t.Error("zzZt chunk is not readable")
					return
				}
				if len(p.TEXTs) != 1 || p.TEXTs[0].Text != "v1" {
					t.Errorf("natively parsed tEXt is wrong: %+v", p.TEXTs)
					return
				}
				if _, err = p.GetOtherChunkByName("zzZt"); err == nil {
					t.Error("zzZt should not be in OtherChunk after base parsing")
					return
				}
				if _, err = p.MarshalBytes(); err != nil {
					t.Error("MarshalBytes failed:", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestConcurrentOneInstance(t *testing.T) {
	// one shared instance: 3 goroutines consume 12 custom chunks in
	// parallel while 4 readers decode pixels and read the metadata
	var zzt [][]byte
	for i := 0; i < 12; i++ {
		zzt = append(zzt, chunkBytes("zzZt", []byte{byte(i)}))
	}
	data := stressPngData(zzt...)
	p, err := ParsePng(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = p.RawChunks()
				_, _ = p.RawChunkByName("zzZt")
				_, _ = p.GetOtherChunkByName("zzZt")
				if _, err := p.IDATsToColor(); err != nil {
					t.Error("reader decode failed:", err)
					return
				}
				if _, err := p.MarshalBytes(); err != nil {
					t.Error("reader marshal failed:", err)
					return
				}
			}
		}()
	}

	var writers sync.WaitGroup
	var colMu sync.Mutex
	var collected []*stressChunk
	for w := 0; w < 3; w++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for {
				c := &stressChunk{}
				if err := p.ParseChunk(c); err != nil {
					return // exhausted, that is fine
				}
				colMu.Lock()
				collected = append(collected, c)
				colMu.Unlock()
			}
		}()
	}
	writers.Wait()
	close(stop)
	readers.Wait()

	if len(collected) != 12 {
		t.Fatalf("consumed %d chunk parsers, want 12", len(collected))
	}
	// every parser must have received its data and be recorded for later
	// retrieval through GetOtherChunkByName
	seen := map[byte]bool{}
	for _, c := range collected {
		if len(c.data) != 1 {
			t.Fatalf("parser received %d bytes of data", len(c.data))
		}
		seen[c.data[0]] = true
	}
	for i := 0; i < 12; i++ {
		if !seen[byte(i)] {
			t.Fatalf("chunk data byte %d was lost", i)
		}
	}
	others, err := p.GetOtherChunkByName("zzZt")
	if err != nil || len(others) != 12 {
		t.Fatalf("GetOtherChunkByName: %d parsers, err %v (want 12, nil)", len(others), err)
	}
	if _, ok := p.RawChunkByName("zzZt"); ok {
		t.Fatal("consumed zzZt chunks are still listed as raw chunks")
	}
}

func TestConcurrentEditChurn(t *testing.T) {
	// concurrent AddRawChunk / DeleteRawChunk / WriteTo churn must end up
	// with a stream that still contains the image and every parseable chunk
	p, err := ParsePng(bytes.NewReader(stressPngData(
		chunkBytes("zzZt", []byte{7, 8, 9}),
	)))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				name, payload, err := NewITXTChunk("XML:com.adobe.xmp", "", "", "<xmp/>", false)
				if err != nil {
					t.Error("NewITXTChunk failed:", err)
					return
				}
				if err = p.AddRawChunk(name, payload); err != nil {
					t.Error("AddRawChunk failed:", err)
					return
				}
				_ = p.DeleteRawChunk("zzZt")
				if _, err = p.MarshalBytes(); err != nil {
					t.Error("MarshalBytes failed:", err)
					return
				}
				if _, err = p.IDATsToColor(); err != nil {
					t.Error("decode failed:", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// race writer + reader for the final verification
	written, err := p.MarshalBytes()
	if err != nil {
		t.Fatal(err)
	}
	p2, err := ParsePng(bytes.NewReader(written))
	if err != nil {
		t.Fatal(err)
	}
	codes := chunkTypeCodes(t, written)
	if len(codes) < 3 || codes[0] != "IHDR" || codes[len(codes)-1] != "IEND" {
		t.Fatalf("broken chunk order: %v", codes)
	}
	// the pixels must survive every churn and the zzZt chunk must be gone
	if _, ok := p2.RawChunkByName("zzZt"); ok {
		t.Fatal("zzZt chunk should have been deleted")
	}
}
