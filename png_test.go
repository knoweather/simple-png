package simple_png

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"image"
	stdpng "image/png"
	"io"
	"log"
	"os"
	"testing"
	"time"
)

// chunkBytes builds a png chunk with a correct CRC (using the standard
// library's crc32 as an independent reference of ours)
func chunkBytes(name string, data []byte) []byte {
	out := make([]byte, 12+len(data))
	binary.BigEndian.PutUint32(out[:4], uint32(len(data)))
	copy(out[4:8], name)
	copy(out[8:], data)
	binary.BigEndian.PutUint32(out[8+len(data):], crc32.ChecksumIEEE(out[4:8+len(data)]))
	return out
}

func pngSignature() []byte {
	return []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
}

func buildPng(chunks ...[]byte) []byte {
	out := pngSignature()
	for _, c := range chunks {
		out = append(out, c...)
	}
	out = append(out, chunkBytes("IEND", nil)...)
	return out
}

func ihdrBytes(w, h uint32, bitDepth, colorType uint8) []byte {
	var data = make([]byte, 13)
	binary.BigEndian.PutUint32(data[:4], w)
	binary.BigEndian.PutUint32(data[4:8], h)
	data[8] = bitDepth
	data[9] = colorType
	// compression, filter and interlace method 0
	return data
}

func compressRaw(data []byte) []byte {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write(data)
	_ = zw.Close()
	return buf.Bytes()
}

func idatBytes(data []byte) []byte {
	return chunkBytes("IDAT", data)
}

// filterRows applies png scanline filters (method 0) to unfiltered scanlines.
// An encoder knows all the original pixel values, so the predictors can be
// computed from them directly.
func filterRows(rows [][]byte, stride, bpp int, filters []byte) []byte {
	out := make([]byte, 0, len(rows)*(stride+1))
	prev := make([]byte, stride)
	for y, orig := range rows {
		ft := filters[y%len(filters)]
		out = append(out, ft)
		for x := 0; x < stride; x++ {
			var a, c byte
			if x >= bpp {
				a = orig[x-bpp]
				c = prev[x-bpp]
			}
			b := prev[x]
			var pred byte
			switch ft {
			case 0:
				pred = 0
			case 1:
				pred = a
			case 2:
				pred = b
			case 3:
				pred = byte((int(a) + int(b)) / 2)
			case 4:
				pred = paethFilter(a, b, c)
			}
			out = append(out, orig[x]-pred)
		}
		copy(prev, orig)
	}
	return out
}

var allFilters = []byte{0, 1, 2, 3, 4}

// decodeWithStd decodes with the standard library as a reference oracle
func decodeWithStd(t *testing.T, data []byte) image.Image {
	t.Helper()
	want, err := stdpng.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("standard png decode failed: %v", err)
	}
	return want
}

// assertImageEqual compares two images pixel by pixel
func assertImageEqual(t *testing.T, want, got image.Image) {
	t.Helper()
	if !got.Bounds().Eq(want.Bounds()) {
		t.Fatalf("bounds: got %v want %v", got.Bounds(), want.Bounds())
	}
	for y := want.Bounds().Min.Y; y < want.Bounds().Max.Y; y++ {
		for x := want.Bounds().Min.X; x < want.Bounds().Max.X; x++ {
			wr, wg, wb, wa := want.At(x, y).RGBA()
			gr, gg, gb, ga := got.At(x, y).RGBA()
			if wr != gr || wg != gg || wb != gb || wa != ga {
				t.Fatalf("pixel (%d, %d): got (%d %d %d %d), want (%d %d %d %d)",
					x, y, gr, gg, gb, ga, wr, wg, wb, wa)
			}
		}
	}
}

// parseAndDecode parses with our library and decodes to an image
func parseAndDecode(t *testing.T, data []byte) image.Image {
	t.Helper()
	p, err := ParsePng(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParsePng failed: %v", err)
	}
	img, err := p.IDATsToColor()
	if err != nil {
		t.Fatalf("IDATsToColor failed: %v", err)
	}
	return img
}

func TestParsePng(t *testing.T) {
	open, err := os.Open("./demo.png")
	if err != nil {
		t.Skipf("demo.png is not readable in this environment: %v", err)
	}
	defer open.Close()
	p, err := ParsePng(open)
	if err != nil {
		t.Fatal(err)
	}
	if p.Width != 256 || p.Height != 81 {
		t.Fatalf("invalid size: got %dx%d, want 256x81", p.Width, p.Height)
	}
	if p.Channel != 3 {
		t.Fatalf("invalid channel: got %d, want 3", p.Channel)
	}
	if len(p.IDATs) != 1 {
		t.Fatalf("invalid idats: got %d, want 1", len(p.IDATs))
	}
	if p.IHDR.ColorType != 2 || p.IHDR.BitDepth != 8 {
		t.Fatalf("invalid ihdr: %v", p.IHDR)
	}
	if p.PHYS == nil || p.PHYS.X != 3780 || p.PHYS.Y != 3780 || p.PHYS.UnitSpecifier != 1 {
		t.Fatalf("invalid phys: %v", p.PHYS)
	}
	if len(p.TEXTs) != 1 || p.TEXTs[0].Keyword != "Software" || p.TEXTs[0].Text != "Snipaste" {
		t.Fatalf("invalid text: %v", p.TEXTs)
	}
	// the parsed base chunks are consumed, only unknown chunks remain
	var names []ChunkName
	for _, c := range p.chunks {
		names = append(names, ChunkName(c.code[:]))
	}
	if len(names) != 0 {
		t.Fatalf("expected all known chunks to be consumed, got %v", names)
	}
	log.Println(p.IDATs[0], p.TEXTs[0], p.PHYS)
}

type CustomChunkParse struct {
	Data []byte
}

func (c *CustomChunkParse) ChunkName() ChunkName {
	return ChunkName("sRGB")
}

func (c *CustomChunkParse) Parse(chunk *chunk) error {
	c.Data = chunk.data[:]
	return nil
}

func TestCustomChunk(t *testing.T) {
	open, err := os.Open("./png-format.png")
	if err != nil {
		t.Skipf("png-format.png is not readable in this environment: %v", err)
	}
	defer open.Close()
	p, err := ParsePng(open)
	if err != nil {
		t.Fatal(err)
	}
	log.Println(p.BKGD, p.ZTXTs, p.TIME, p.IHDR)
	for i, idaT := range p.IDATs {
		log.Println(i, *idaT)
	}

	// png-format.png contains a sRGB chunk unknown to the parser
	var unknown []ChunkName
	for _, c := range p.chunks {
		unknown = append(unknown, ChunkName(c.code[:]))
	}
	if len(unknown) != 1 || unknown[0] != "sRGB" {
		t.Fatalf("expected only the unknown sRGB chunk, got %v", unknown)
	}

	// parse the custom chunk and get it back by name
	c := &CustomChunkParse{}
	if err = p.ParseChunk(c); err != nil {
		t.Fatalf("custom chunk parse failed: %v", err)
	}
	if len(c.Data) != 1 {
		t.Fatalf("invalid custom chunk data: %v", c.Data)
	}
	found, err := p.GetOtherChunkByName("sRGB")
	if err != nil || len(found) != 1 || found[0] != c {
		t.Fatalf("GetOtherChunkByName failed: %v %v", found, err)
	}
}

func TestIDATsToColorFiles(t *testing.T) {
	for _, name := range []string{"./demo.png", "./png-format.png"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Skipf("%s is not readable in this environment: %v", name, err)
			continue
		}
		want := decodeWithStd(t, data)
		got := parseAndDecode(t, data)
		assertImageEqual(t, want, got)
	}
}

func TestDecodeGray8AllFilters(t *testing.T) {
	width, height := 9, 5
	var rows [][]byte
	var expect []uint8
	for y := 0; y < height; y++ {
		var row []byte
		for x := 0; x < width; x++ {
			v := uint8(x*31 + y*7)
			row = append(row, v)
			expect = append(expect, v)
		}
		rows = append(rows, row)
	}
	bpp := 1
	stride := width
	var ihdrData = ihdrBytes(9, 5, 8, 0)
	data := buildPng(chunkBytes("IHDR", ihdrData), idatBytes(compressRaw(filterRows(rows, stride, bpp, allFilters))))
	want := decodeWithStd(t, data)
	got := parseAndDecode(t, data)
	assertImageEqual(t, want, got)

	gray, ok := got.(*image.Gray)
	if !ok {
		t.Fatalf("expected *image.Gray, got %T", got)
	}
	for i, v := range expect {
		if gray.Pix[i] != v {
			t.Fatalf("gray pixel %d: got %d want %d", i, gray.Pix[i], v)
		}
	}
}

func TestDecodeSubByteDepths(t *testing.T) {
	for _, depth := range []uint8{1, 2, 4} {
		width, height := 16, 2
		stride := (width*int(depth) + 7) / 8
		bpp := 1
		var rows [][]byte
		var expect []uint8
		for y := 0; y < height; y++ {
			row := make([]byte, stride)
			for x := 0; x < width; x++ {
				v := uint16((x*3 + y*5) & (1<<depth - 1))
				// pack the sample most significant bit first
				pos := x * int(depth)
				shift := uint(8 - pos%8 - int(depth))
				row[pos>>3] |= byte(v << shift)
				expect = append(expect, uint8(v)*uint8(255/(1<<depth-1)))
			}
			rows = append(rows, row)
		}
		data := buildPng(
			chunkBytes("IHDR", ihdrBytes(16, 2, depth, 0)),
			idatBytes(compressRaw(filterRows(rows, stride, bpp, []byte{0, 4}))),
		)
		want := decodeWithStd(t, data)
		got := parseAndDecode(t, data)
		assertImageEqual(t, want, got)
	}
}

func TestDecodeTruecolorTRNS(t *testing.T) {
	// 2x2 rgb d8, tRNS marks the color of the first pixel transparent
	width, height := 2, 2
	var rows [][]byte
	pixels := []uint8{
		10, 20, 30, 40, 50, 60,
		100, 110, 120, 130, 140, 150,
	}
	for y := 0; y < height; y++ {
		rows = append(rows, pixels[y*width*3:(y+1)*width*3])
	}
	var trns = make([]byte, 6)
	// tRNS sample values live in a 16-bit container even for 8-bit images,
	// so 10/20/30 are stored as 10*0x101 etc (the 8-bit sample scaled per spec)
	binary.BigEndian.PutUint16(trns[0:2], 10*0x101)
	binary.BigEndian.PutUint16(trns[2:4], 20*0x101)
	binary.BigEndian.PutUint16(trns[4:6], 30*0x101)
	data := buildPng(
		chunkBytes("IHDR", ihdrBytes(2, 2, 8, 2)),
		chunkBytes("tRNS", trns),
		idatBytes(compressRaw(filterRows(rows, width*3, 3, []byte{0}))),
	)
	img, ok := parseAndDecode(t, data).(*image.NRGBA64)
	if !ok {
		t.Fatalf("expected *image.NRGBA64, got %T", img)
	}
	if r, g, b, a := img.At(0, 0).RGBA(); r != 0 || g != 0 || b != 0 || a != 0 {
		t.Fatalf("first pixel must be transparent: %v", img.At(0, 0))
	}
	if r, g, b, a := img.At(1, 0).RGBA(); r != 40<<8|40 || g != 50<<8|50 || b != 60<<8|60 || a != 65535 {
		t.Fatalf("second pixel must be opaque: (%d %d %d %d)", r, g, b, a)
	}
}

func TestDecodePaletteSubByteTRNS(t *testing.T) {
	// 4x2 palette image, bit depth 4, 4 colors, tRNS with 4 alphas
	width, height := 4, 2
	stride := (width*int(4) + 7) / 8
	bpp := 1
	var rows [][]byte
	for y := 0; y < height; y++ {
		row := make([]byte, stride)
		for x := 0; x < width; x++ {
			v := uint8((x + y*2) % 4)
			pos := x * 4
			row[pos>>3] |= v << (8 - pos%8 - 4)
		}
		rows = append(rows, row)
	}
	var plte []byte
	for i := 0; i < 4; i++ {
		plte = append(plte, uint8(i*60), uint8(i*10), uint8(200-i*40))
	}
	alphas := []byte{0, 128, 255, 64}
	data := buildPng(
		chunkBytes("IHDR", ihdrBytes(4, 2, 4, 3)),
		chunkBytes("PLTE", plte),
		chunkBytes("tRNS", alphas),
		idatBytes(compressRaw(filterRows(rows, stride, bpp, []byte{1}))),
	)
	want := decodeWithStd(t, data)
	got := parseAndDecode(t, data)
	assertImageEqual(t, want, got)
}

func TestDecodeGrayAlpha(t *testing.T) {
	// 3x1 gray+alpha d8
	rows := [][]byte{{10, 200, 30, 100, 50, 255}}
	data := buildPng(
		chunkBytes("IHDR", ihdrBytes(3, 1, 8, 4)),
		idatBytes(compressRaw(filterRows(rows, 6, 2, []byte{4}))),
	)
	want := decodeWithStd(t, data)
	got := parseAndDecode(t, data)
	assertImageEqual(t, want, got)
}

func TestDecodeRGBA16MultiIDAT(t *testing.T) {
	// 2x2 rgba d16, the compressed stream split into 3 idat chunks
	width, height := 2, 2
	var rows [][]byte
	for y := 0; y < height; y++ {
		rowBytes := make([]byte, width*4*2)
		for x := 0; x < width*4; x++ {
			binary.BigEndian.PutUint16(rowBytes[x*2:], uint16((x+y*width*4)*2000+7))
		}
		rows = append(rows, rowBytes)
	}
	compressed := compressRaw(filterRows(rows, width*4*2, 4*2, []byte{2}))
	var idats [][]byte
	for i := 0; i < len(compressed); i += 7 {
		end := i + 7
		if end > len(compressed) {
			end = len(compressed)
		}
		idats = append(idats, idatBytes(compressed[i:end]))
	}
	chunks := [][]byte{chunkBytes("IHDR", ihdrBytes(2, 2, 16, 6))}
	chunks = append(chunks, idats...)
	data := buildPng(chunks...)
	want := decodeWithStd(t, data)
	got := parseAndDecode(t, data)
	assertImageEqual(t, want, got)
}

func TestDecodeGray16TRNS(t *testing.T) {
	// 2x1 gray d16, tRNS marks the first gray level transparent
	width := 2
	rows := [][]byte{{0x12, 0x34, 0xAB, 0xCD}}
	var trns = make([]byte, 2)
	binary.BigEndian.PutUint16(trns, 0x1234)
	data := buildPng(
		chunkBytes("IHDR", ihdrBytes(2, 1, 16, 0)),
		chunkBytes("tRNS", trns),
		idatBytes(compressRaw(filterRows(rows, width*2, 2, []byte{0}))),
	)
	want := decodeWithStd(t, data)
	got := parseAndDecode(t, data)
	assertImageEqual(t, want, got)
}

func TestZTXTAndBKGD(t *testing.T) {
	compressed := compressRaw([]byte("compressed text payload"))
	var ztxt []byte
	ztxt = append(ztxt, []byte("Comment")...)
	ztxt = append(ztxt, 0x00)
	ztxt = append(ztxt, 0) // compression method 0
	ztxt = append(ztxt, compressed...)

	var bkgd1 = []byte{3}
	var bkgd2 = make([]byte, 2)
	binary.BigEndian.PutUint16(bkgd2, 0x0102)
	var bkgd6 = make([]byte, 6)
	binary.BigEndian.PutUint16(bkgd6[0:2], 0x0304)
	binary.BigEndian.PutUint16(bkgd6[2:4], 0x0506)
	binary.BigEndian.PutUint16(bkgd6[4:6], 0x0708)

	data := buildPng(
		chunkBytes("IHDR", ihdrBytes(1, 1, 8, 3)),
		chunkBytes("PLTE", []byte{1, 2, 3}),
		chunkBytes("bKGD", bkgd1),
		chunkBytes("zTXt", ztxt),
		idatBytes(compressRaw([]byte{0})),
	)
	p, err := ParsePng(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ZTXTs) != 1 || p.ZTXTs[0].Keyword != "Comment" || p.ZTXTs[0].Text != "compressed text payload" {
		t.Fatalf("invalid ztxt: %+v", p.ZTXTs)
	}
	if p.BKGD == nil || p.BKGD.Palette != 3 {
		t.Fatalf("invalid bkgd: %+v", p.BKGD)
	}

	// 2 byte bkgd (gray)
	data = buildPng(
		chunkBytes("IHDR", ihdrBytes(1, 1, 8, 0)),
		chunkBytes("bKGD", bkgd2),
		idatBytes(compressRaw([]byte{0})),
	)
	p, err = ParsePng(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if p.BKGD == nil || p.BKGD.Gray != 0x0102 || p.BKGD.Palette != 0 {
		t.Fatalf("invalid bkgd: %+v", p.BKGD)
	}

	// 6 byte bkgd (rgb)
	data = buildPng(
		chunkBytes("IHDR", ihdrBytes(1, 1, 8, 2)),
		chunkBytes("bKGD", bkgd6),
		idatBytes(compressRaw(filterRows([][]byte{{1, 2, 3}}, 3, 3, []byte{0}))),
	)
	p, err = ParsePng(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if p.BKGD == nil || p.BKGD.Red != 0x0304 || p.BKGD.Green != 0x0506 || p.BKGD.Blue != 0x0708 {
		t.Fatalf("invalid bkgd: %+v", p.BKGD)
	}
}

func TestTEXTPreservesWhitespace(t *testing.T) {
	var text []byte
	text = append(text, []byte("Title")...)
	text = append(text, 0x00)
	text = append(text, []byte(" padded text  ")...)
	data := buildPng(
		chunkBytes("IHDR", ihdrBytes(1, 1, 8, 0)),
		chunkBytes("tEXt", text),
		idatBytes(compressRaw([]byte{0, 0, 0})),
	)
	p, err := ParsePng(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.TEXTs) != 1 || p.TEXTs[0].Text != " padded text  " {
		t.Fatalf("invalid text: %+v", p.TEXTs)
	}
}

func TestHISTAndPLTE(t *testing.T) {
	// hIST: 3 palette entries, 3 uint16 frequencies
	var hist []byte
	for _, v := range []uint16{1, 65535, 342} {
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], v)
		hist = append(hist, b[:]...)
	}
	data := buildPng(
		chunkBytes("IHDR", ihdrBytes(1, 1, 8, 3)),
		chunkBytes("PLTE", []byte{1, 2, 3, 4, 5, 6, 7, 8, 9}),
		chunkBytes("hIST", hist),
		idatBytes(compressRaw([]byte{0})),
	)
	p, err := ParsePng(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.PLTE.Colors) != 3 || p.PLTE.Colors[2].Blue != 9 {
		t.Fatalf("invalid plte: %+v", p.PLTE)
	}
	if len(p.HIST.Elements) != 3 || p.HIST.Elements[0] != 1 || p.HIST.Elements[1] != 65535 || p.HIST.Elements[2] != 342 {
		t.Fatalf("invalid hist: %+v", p.HIST)
	}
}

func TestCRCCorruption(t *testing.T) {
	// a corrupted ancillary chunk cannot affect the zlib stream
	data := buildPng(
		chunkBytes("IHDR", ihdrBytes(1, 1, 8, 0)),
		chunkBytes("cst1", []byte{0xA5, 0x5A, 0x01}),
		idatBytes(compressRaw([]byte{0, 0, 0})),
	)
	// corrupt a byte inside the data of the ancillary chunk
	// signature(8) + IHDR(12+13) + 8 byte chunk header
	customStart := 8 + (12 + 13)
	data[customStart+8] ^= 0xFF

	// default: CRC verification is off, the file still parses
	p, err := ParsePng(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("expected a successful parse with crc off: %v", err)
	}
	if len(p.RawChunks()) != 1 {
		t.Fatalf("expected 1 raw chunk, got %d", len(p.RawChunks()))
	}

	// opt-in: the same file must be rejected
	if _, err = ParsePng(bytes.NewReader(data), true); err == nil {
		t.Fatal("expected a crc error when verification is on")
	}
}

func TestInvalidIHDR(t *testing.T) {
	// zero size
	data := buildPng(
		chunkBytes("IHDR", ihdrBytes(0, 10, 8, 0)),
		idatBytes(compressRaw([]byte{0})),
	)
	if _, err := ParsePng(bytes.NewReader(data)); err == nil {
		t.Fatal("expected an error for a zero width")
	}

	// invalid color type
	if err := (&IHDR{Width: 1, Height: 1, BitDepth: 8, ColorType: 5}).Validate(); err == nil {
		t.Fatal("expected an error for color type 5")
	}

	// invalid bit depth for the color type
	if err := (&IHDR{Width: 1, Height: 1, BitDepth: 4, ColorType: 6}).Validate(); err == nil {
		t.Fatal("expected an error for bit depth 4 in color type 6")
	}
}

func TestTIME(t *testing.T) {
	var data []byte
	data = append(data, []byte{0x07, 0xDB}...) // 2011
	data = append(data, 12, 25, 18, 30, 59)
	c := &chunk{data: data}
	tm := &TIME{}
	if err := tm.Parse(c); err != nil {
		t.Fatal(err)
	}
	if tm.Year != 2011 || tm.Month != 12 || tm.Day != 25 || tm.Hour != 18 || tm.Minute != 30 || tm.Second != 59 {
		t.Fatalf("invalid time: %+v", tm)
	}
	if tm.ToTime() != time.Date(2011, 12, 25, 18, 30, 59, 0, time.UTC) {
		t.Fatalf("invalid unix time: %v", tm.ToTime())
	}

	// short data must not panic
	if err := tm.Parse(&chunk{data: data[:3]}); err == nil {
		t.Fatal("expected an error for short time data")
	}
}

// BenchmarkDecode compares a full parse + pixel decode between this library
// and the standard library image/png decoder as a reference.
func BenchmarkDecode(b *testing.B) {
	for _, name := range []string{"./demo.png", "./png-format.png"} {
		data, err := os.ReadFile(name)
		if err != nil {
			b.Skipf("%s is not readable in this environment: %v", name, err)
		}
		b.Run(name+" | simple_png", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				p, err := ParsePng(bytes.NewReader(data))
				if err != nil {
					b.Fatal(err)
				}
				if _, err = p.IDATsToColor(); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(name+" | std lib", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := stdpng.Decode(bytes.NewReader(data)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkParse measures only the chunk parsing layer (no pixel decode),
// with png.Decode of the standard library as the closest comparable baseline.
func BenchmarkParse(b *testing.B) {
	for _, name := range []string{"./demo.png", "./png-format.png"} {
		data, err := os.ReadFile(name)
		if err != nil {
			b.Skipf("%s is not readable in this environment: %v", name, err)
		}
		b.Run(name+" | simple_png ParsePng", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := ParsePng(bytes.NewReader(data)); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(name+" | std lib Decode", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := stdpng.Decode(bytes.NewReader(data)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestITXT(t *testing.T) {
	// uncompressed iTXt as commonly used for XMP metadata
	var itxt []byte
	itxt = append(itxt, []byte("XML:com.adobe.xmp")...)
	itxt = append(itxt, 0x00, 0, 0, 0x00)                       // keyword, flag 0, method 0, null
	itxt = append(itxt, []byte("x-default")...)                 // language tag
	itxt = append(itxt, 0x00)                                   // null
	itxt = append(itxt, []byte("XMP 元数据标题")...)                // translated keyword (utf-8)
	itxt = append(itxt, 0x00)                                   // null
	itxt = append(itxt, []byte("<x:xmpmeta xmlns:x='adobe:ns:meta/'></x:xmpmeta>")...)

	data := buildPng(
		chunkBytes("IHDR", ihdrBytes(1, 1, 8, 0)),
		chunkBytes("iTXt", itxt),
		idatBytes(compressRaw([]byte{0, 0, 0})),
	)
	p, err := ParsePng(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ITXTs) != 1 {
		t.Fatalf("expected 1 itxt, got %d", len(p.ITXTs))
	}
	x := p.ITXTs[0]
	if x.Keyword != "XML:com.adobe.xmp" || x.CompressionFlag != 0 || x.LanguageTag != "x-default" ||
		x.TranslatedKeyword != "XMP 元数据标题" || x.Text != "<x:xmpmeta xmlns:x='adobe:ns:meta/'></x:xmpmeta>" {
		t.Fatalf("invalid itxt: %+v", x)
	}

	// compressed iTXt
	compressed := compressRaw([]byte("compressed itxt body"))
	var citxt []byte
	citxt = append(citxt, []byte("Comment")...)
	citxt = append(citxt, 0x00, 1, 0, 0x00) // keyword, flag 1 (compressed), method 0 (zlib), null
	citxt = append(citxt, 0x00)             // language tag: ""...
	citxt = append(citxt, 0x00)             // translated keyword: ""
	citxt = append(citxt, compressed...)
	data = buildPng(
		chunkBytes("IHDR", ihdrBytes(1, 1, 8, 0)),
		chunkBytes("iTXt", citxt),
		idatBytes(compressRaw([]byte{0, 0, 0})),
	)
	p, err = ParsePng(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ITXTs) != 1 || p.ITXTs[0].Text != "compressed itxt body" || p.ITXTs[0].CompressionFlag != 1 {
		t.Fatalf("invalid compressed itxt: %+v", p.ITXTs)
	}
}

func TestRawChunks(t *testing.T) {
	// sRGB stays in the file as a raw chunk, the standard crc value matches
	data := buildPng(
		chunkBytes("IHDR", ihdrBytes(1, 1, 8, 0)),
		chunkBytes("sRGB", []byte{0}), // perceptual
		idatBytes(compressRaw([]byte{0, 0, 0})),
	)
	p, err := ParsePng(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	raw := p.RawChunks()
	if len(raw) != 1 || raw[0].Name != "sRGB" || len(raw[0].Data) != 1 {
		t.Fatalf("invalid raw chunks: %+v", raw)
	}
	if _, ok := p.RawChunkByName("eXIf"); ok {
		t.Fatal("eXIf chunk should not exist")
	}
	if exifData, ok := p.RawChunkByName("sRGB"); !ok || len(exifData) != 1 {
		t.Fatalf("invalid srgb raw chunk: %+v", exifData)
	}
}

// chunkTypeCodes walks a png stream and lists its chunk type codes in order.
func chunkTypeCodes(t *testing.T, data []byte) []string {
	t.Helper()
	r := bytes.NewReader(data)
	var header [8]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		t.Fatal(err)
	}
	var out []string
	for {
		var lb [4]byte
		var code [4]byte
		if _, err := io.ReadFull(r, lb[:]); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(r, code[:]); err != nil {
			t.Fatal(err)
		}
		size := binary.BigEndian.Uint32(lb[:])
		if _, err := io.CopyN(io.Discard, r, int64(size)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.CopyN(io.Discard, r, 4); err != nil { // crc
			t.Fatal(err)
		}
		out = append(out, string(code[:]))
		if string(code[:]) == "IEND" {
			return out
		}
	}
}

func TestWriteToRoundtrip(t *testing.T) {
	for _, name := range []string{"./demo.png", "./png-format.png"} {
		t.Run(name, func(t *testing.T) {
			original, err := os.ReadFile(name)
			if err != nil {
				t.Skipf("%s is not readable in this environment: %v", name, err)
			}
			p, err := ParsePng(bytes.NewReader(original))
			if err != nil {
				t.Fatal(err)
			}
			written, err := p.MarshalBytes()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, written) {
				t.Fatalf("roundtrip is not byte-identical: got %d bytes, want %d", len(written), len(original))
			}
			// the re-written file must also validate with crc checking on
			if _, err = ParsePng(bytes.NewReader(written), true); err != nil {
				t.Fatalf("re-written file has invalid crcs: %v", err)
			}
		})
	}
}

func TestEditMetadata(t *testing.T) {
	data := buildPng(
		chunkBytes("IHDR", ihdrBytes(1, 1, 8, 0)),
		chunkBytes("tEXt", append(append([]byte("Author"), 0), []byte("before")...)),
		idatBytes(compressRaw([]byte{0, 0x55})),
	)
	p, err := ParsePng(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.TEXTs) != 1 || p.TEXTs[0].Text != "before" {
		t.Fatalf("invalid initial text: %+v", p.TEXTs)
	}
	p.TEXTs[0].Text = "after"
	written, err := p.MarshalBytes()
	if err != nil {
		t.Fatal(err)
	}
	p2, err := ParsePng(bytes.NewReader(written))
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.TEXTs) != 1 || p2.TEXTs[0].Text != "after" {
		t.Fatalf("edited text was not written: %+v", p2.TEXTs)
	}
	// the image data must be untouched
	assertImageEqual(t, decodeWithStd(t, written), parseAndDecode(t, written))
}

func TestDeleteRawChunk(t *testing.T) {
	data := buildPng(
		chunkBytes("IHDR", ihdrBytes(1, 1, 8, 0)),
		chunkBytes("sRGB", []byte{0}),
		idatBytes(compressRaw([]byte{0, 0x55})),
	)
	p, err := ParsePng(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if n := p.DeleteRawChunk("sRGB"); n != 1 {
		t.Fatalf("expected to remove 1 chunk, removed %d", n)
	}
	written, err := p.MarshalBytes()
	if err != nil {
		t.Fatal(err)
	}
	p2, err := ParsePng(bytes.NewReader(written))
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.RawChunks()) != 0 {
		t.Fatalf("sRGB chunk was not removed: %+v", p2.RawChunks())
	}
	// the ancillary chunk must have gone from the stream, but the image must
	// still decode
	if codes := chunkTypeCodes(t, written); codes[1] != "IDAT" {
		t.Fatalf("unexpected chunk order: %v", codes)
	}
	assertImageEqual(t, decodeWithStd(t, written), parseAndDecode(t, written))
}

func TestAddRawChunkXMP(t *testing.T) {
	data := buildPng(
		chunkBytes("IHDR", ihdrBytes(1, 1, 8, 0)),
		idatBytes(compressRaw([]byte{0, 0x55})),
	)
	p, err := ParsePng(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	name, payload, err := NewITXTChunk("XML:com.adobe.xmp", "", "", "<xmp>hello</xmp>", true)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.AddRawChunk(name, payload); err != nil {
		t.Fatal(err)
	}
	// it must be illegal to add a single-occurrence chunk
	if err = p.AddRawChunk(IHDRChunk, make([]byte, 13)); err == nil {
		t.Fatal("expected an error when adding an IHDR chunk")
	}
	written, err := p.MarshalBytes()
	if err != nil {
		t.Fatal(err)
	}
	// the new chunk must be inserted before the first IDAT
	codes := chunkTypeCodes(t, written)
	if codes[1] != "iTXt" || codes[2] != "IDAT" || codes[3] != "IEND" {
		t.Fatalf("unexpected chunk order: %v", codes)
	}
	p2, err := ParsePng(bytes.NewReader(written))
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.ITXTs) != 1 || p2.ITXTs[0].Keyword != "XML:com.adobe.xmp" || p2.ITXTs[0].Text != "<xmp>hello</xmp>" {
		t.Fatalf("added xmp chunk was not parsed back: %+v", p2.ITXTs)
	}
	assertImageEqual(t, decodeWithStd(t, written), parseAndDecode(t, written))
}

// adam7Encode turns unfiltered scanlines of the final (non-interlaced) layout
// into the filtered datastream of an Adam7 interlaced png. The pass streams
// are unfiltered separately per pass, like a real encoder does.
func adam7Encode(t *testing.T, width, height int, bitDepth, colorType uint8, rows [][]byte, filters []byte) []byte {
	t.Helper()
	channels := colorTypeChannels[colorType]
	bitsPerPixel := channels * int(bitDepth)
	bpp := (bitsPerPixel + 7) / 8
	var out []byte
	for _, ps := range adam7Passes {
		pw, ph := adam7PassSize(width, height, ps)
		if pw <= 0 || ph <= 0 {
			continue
		}
		passStride := (pw*bitsPerPixel + 7) / 8
		passRows := make([][]byte, ph)
		for j := range passRows {
			fy := ps.yOff + j*ps.dy
			r := make([]byte, passStride)
			for i := 0; i < pw; i++ {
				fx := ps.xOff + i*ps.dx
				for c := 0; c < channels; c++ {
					v := sampleAt(rows[fy], int(bitDepth), fx*channels+c)
					writeSample(r, int(bitDepth), i*channels+c, v)
				}
			}
			passRows[j] = r
		}
		out = append(out, filterRows(passRows, passStride, bpp, filters)...)
	}
	return out
}

// ihdrBytesInterlaced is ihdrBytes with Adam7 interlace method 1.
func ihdrBytesInterlaced(w, h uint32, bitDepth, colorType uint8) []byte {
	d := ihdrBytes(w, h, bitDepth, colorType)
	d[12] = 1
	return d
}

func TestAdam7Truecolor(t *testing.T) {
	const w, h = 5, 5
	rows := make([][]byte, h)
	for y := 0; y < h; y++ {
		r := make([]byte, w*3)
		for x := 0; x < w; x++ {
			o := x * 3
			r[o] = byte(x * 50)
			r[o+1] = byte(y * 50)
			r[o+2] = byte((x + y) * 20)
		}
		rows[y] = r
	}
	data := buildPng(
		chunkBytes("IHDR", ihdrBytesInterlaced(w, h, 8, 2)),
		idatBytes(compressRaw(adam7Encode(t, w, h, 8, 2, rows, allFilters))),
	)
	assertImageEqual(t, decodeWithStd(t, data), parseAndDecode(t, data))
}

func TestAdam7PaletteSubByte(t *testing.T) {
	const w, h = 7, 5
	var plte []byte
	for i := 0; i < 8; i++ {
		plte = append(plte, byte(i*30), 255-byte(i*30), byte(i*17)+10)
	}
	trns := []byte{255, 128, 0, 64, 255, 255, 255, 255}
	rows := make([][]byte, h)
	for y := 0; y < h; y++ {
		r := make([]byte, (w*4+7)/8)
		for x := 0; x < w; x++ {
			writeSample(r, 4, x, uint16((x+y)%8))
		}
		rows[y] = r
	}
	data := buildPng(
		chunkBytes("IHDR", ihdrBytesInterlaced(w, h, 4, 3)),
		chunkBytes("PLTE", plte),
		chunkBytes("tRNS", trns),
		idatBytes(compressRaw(adam7Encode(t, w, h, 4, 3, rows, allFilters))),
	)
	assertImageEqual(t, decodeWithStd(t, data), parseAndDecode(t, data))
}
