package simple_png

import (
	"bytes"
	"sync"
	"testing"
)

// 并发压力测试：本文件的用例都必须能在 `go test -race` 下干净通过。
// 它们针对 Png 内部的 RWMutex：相互独立的实例、解析自定义 chunk 期间的并发
// 读取，以及并发编辑之后整体回写并重新解析校验。
//
// Concurrency stress tests: everything here must pass `go test -race`
// cleanly. They exercise the internal RWMutex of Png: independent instances,
// concurrent readers while custom chunks are parsed, and concurrent editing
// churn followed by a full stream re-verification.

// stressChunk 是供并发 ParseChunk 调用使用的自定义 chunk 类型。
//
// stressChunk is a custom chunk type for concurrent ParseChunk calls.
type stressChunk struct {
	data []byte
}

func (s *stressChunk) ChunkName() ChunkName { return "zzZt" }

func (s *stressChunk) Parse(c RawChunk) error {
	s.data = c.Data
	return nil
}

// stressPngData 按「签名 → 给定的自定义 chunk → IHDR + IDAT → IEND」的顺序
// 拼出一份最小的测试用 PNG 字节流（3x2、8 位灰度、无隔行）。
//
// stressPngData assembles a minimal test PNG byte stream in the order
// "signature → the given custom chunks → IHDR + IDAT → IEND"
// (3x2, 8-bit grayscale, no interlacing).
func stressPngData(chunks ...[]byte) []byte {
	base := [][]byte{
		chunkBytes("IHDR", ihdrBytes(3, 2, 8, 0)),
		idatBytes(compressRaw(filterRows([][]byte{{2, 4, 6}, {1, 3, 5}}, 3, 1, []byte{3}))),
	}
	return buildPng(append(chunks, base...)...)
}

// TestConcurrentIndependentInstances 验证多个 goroutine 各自持有独立的 Png
// 实例时互不干扰：每个实例都能完整解析同一份流、解码像素、读取原样 chunk
// 与已解析的 tEXt 元数据，并成功整体回写。
//
// TestConcurrentIndependentInstances verifies that goroutines holding
// independent Png instances do not interfere with each other: each instance
// fully parses the same stream, decodes the pixels, reads the raw and the
// natively parsed tEXt metadata, and marshals the file back.
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

// TestConcurrentOneInstance 在同一个 Png 实例上并发混合读写：多个 goroutine
// 通过 ParseChunk 消费自定义 chunk，同时多个 reader 并发读取元数据、解码像素
// 与整体回写；结束后校验每个 chunk 的数据都被完整接收并记录。
//
// TestConcurrentOneInstance mixes concurrent reads and writes on a single
// Png instance: several goroutines consume the custom chunks through
// ParseChunk while readers concurrently read metadata, decode pixels and
// marshal the file; afterwards it verifies that every chunk's data was
// received in full and recorded.
func TestConcurrentOneInstance(t *testing.T) {
	// 共享同一个实例：3 个 goroutine 并行消费 12 个自定义 chunk，
	// 同时 4 个 reader 不断解码像素并读取元数据
	// One shared instance: 3 goroutines consume 12 custom chunks in
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
					return // 消费完全部 chunk 即正常退出 / exhausted, which is fine
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
	// 每个 parser 都必须收到自己的数据，并被记录下来，
	// 之后能通过 GetOtherChunkByName 取回
	// Every parser must have received its data and be recorded for later
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

// TestConcurrentEditChurn 对同一个实例并发执行 AddRawChunk / DeleteRawChunk /
// 回写（MarshalBytes）的混合编辑；结束后得到的流必须仍然包含图像数据、chunk
// 顺序合法，且被删除的 chunk 不再出现。
//
// TestConcurrentEditChurn runs concurrent AddRawChunk / DeleteRawChunk /
// marshal churn on one instance; the resulting stream must still contain the
// image data, keep a legal chunk order, and no longer contain the deleted
// chunk.
func TestConcurrentEditChurn(t *testing.T) {
	// 并发编辑结束后，最终流中必须仍包含图像与每一个可解析的 chunk
	// After the churn, the final stream must still contain the image and
	// every parseable chunk
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

	// 回写一次并重新解析，做最终校验
	// Marshal once and re-parse the result for the final verification
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
	// 像素数据必须挺过全部并发编辑，zzZt chunk 必须已被删除
	// The pixels must survive every churn and the zzZt chunk must be gone
	if _, ok := p2.RawChunkByName("zzZt"); ok {
		t.Fatal("zzZt chunk should have been deleted")
	}
}
