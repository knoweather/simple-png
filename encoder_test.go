package simple_png

import (
	"bytes"
	"image"
	"image/color"
	stdpng "image/png"
	"os"
	"testing"
)

// referenceNRGBA converts any image through the NRGBA color model: the
// straight alpha reference an encoder must reproduce for premultiplied
// source images.
func referenceNRGBA(src image.Image) *image.NRGBA {
	b := src.Bounds()
	ref := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			c := color.NRGBAModel.Convert(src.At(x+b.Min.X, y+b.Min.Y)).(color.NRGBA)
			ref.SetNRGBA(x, y, c)
		}
	}
	return ref
}

// assertEncodeRoundtrip encodes src with every encoder setting, then checks
// that the stream decodes to the given reference with BOTH the standard
// library decoder and the library itself.
func assertEncodeRoundtrip(t *testing.T, src image.Image, ref image.Image, encoders ...Encoder) {
	t.Helper()
	for _, e := range encoders {
		var buf bytes.Buffer
		if _, err := e.Encode(src, &buf); err != nil {
			t.Fatal(err)
		}
		data := buf.Bytes()
		assertImageEqual(t, ref, decodeWithStd(t, data))
		assertImageEqual(t, ref, parseAndDecode(t, data))
	}
}

func mustEncode(t *testing.T, src image.Image, e Encoder) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := e.Encode(src, &buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestEncodeGray(t *testing.T) {
	const width, height = 16, 9
	src := image.NewGray(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			src.SetGray(x, y, color.Gray{Y: uint8(x*10 + y*25)})
		}
	}
	assertEncodeRoundtrip(t, src, src,
		Encoder{},                                  // defaults: heuristic + default level
		Encoder{FilterStrategy: FilterNone},        // no filter
		Encoder{CompressionLevel: CompressionNone}, // huffman only
		Encoder{CompressionLevel: 9, FilterStrategy: FilterNone},
	)
}

func TestEncodeGray16(t *testing.T) {
	const width, height = 7, 5
	src := image.NewGray16(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			src.SetGray16(x, y, color.Gray16{Y: uint16(x*9000 + y*777)})
		}
	}
	assertEncodeRoundtrip(t, src, src, Encoder{}, Encoder{FilterStrategy: FilterNone})
}

func TestEncodeNRGBA(t *testing.T) {
	const width, height = 9, 7
	src := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			// ramp colors and alpha to catch byte swap errors
			src.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x * 28), G: uint8(y * 36), B: uint8(x + y*7), A: uint8(255 - (x+y)%4*60),
			})
		}
	}
	assertEncodeRoundtrip(t, src, src, Encoder{}, Encoder{FilterStrategy: FilterNone})
}

func TestEncodeNRGBA64(t *testing.T) {
	const width, height = 5, 5
	src := image.NewNRGBA64(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			v := uint16(3000*x + 7000*y)
			src.SetNRGBA64(x, y, color.NRGBA64{R: v, G: v << 1, B: v >> 1, A: uint16(65535 - y*9000)})
		}
	}
	assertEncodeRoundtrip(t, src, src, Encoder{}, Encoder{FilterStrategy: FilterNone})
}

func TestEncodePremultipliedRGBA(t *testing.T) {
	const width, height = 8, 8
	src := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			// the semi-transparent, premultiplied values verify the
			// straight alpha inversion of the fallback path
			src.SetRGBA(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 30), B: 200, A: uint8(128 + (x+y)/2)})
		}
	}
	assertEncodeRoundtrip(t, src, referenceNRGBA(src), Encoder{}, Encoder{FilterStrategy: FilterNone})
}

func TestEncodePalettedWithAlpha(t *testing.T) {
	palette := color.Palette{
		color.RGBA{R: 10, G: 20, B: 30, A: 255},
		color.NRGBA{R: 200, G: 50, B: 100, A: 255},
		color.RGBA{R: 0, G: 255, B: 255, A: 255},
		color.NRGBA{R: 90, G: 40, B: 200, A: 128}, // semi transparent
		color.NRGBA{R: 255, G: 255, B: 255, A: 0}, // fully transparent
	}
	const width, height = 19, 4
	src := image.NewPaletted(image.Rect(0, 0, width, height), palette)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			src.Pix[y*width+x] = uint8((x + y) % len(palette))
		}
	}
	assertEncodeRoundtrip(t, src, src, Encoder{}, Encoder{FilterStrategy: FilterNone})
	// the palette with <= 16 entries and straight alpha values must come
	// out of the file as a 4 bit palette png with a tRNS chunk of the magic
	// [255, 255, 255, 128, 0] values
	p, err := ParsePng(bytes.NewReader(mustEncode(t, src, Encoder{})))
	if err != nil {
		t.Fatal(err)
	}
	if p.IHDR.BitDepth != 4 || p.IHDR.ColorType != 3 || len(p.PLTE.Colors) != len(palette) {
		t.Fatalf("invalid palette png: bit depth %d, color type %d, %d entries",
			p.IHDR.BitDepth, p.IHDR.ColorType, len(p.PLTE.Colors))
	}
	if p.TRNS == nil || len(p.TRNS.Data) != 5 ||
		p.TRNS.Data[0] != 255 || p.TRNS.Data[1] != 255 || p.TRNS.Data[2] != 255 ||
		p.TRNS.Data[3] != 128 || p.TRNS.Data[4] != 0 {
		t.Fatalf("invalid trns: %+v", p.TRNS)
	}
}

func TestEncodeEmptyImage(t *testing.T) {
	// zero width
	if _, err := (&Encoder{}).Encode(image.NewGray(image.Rect(0, 0, 0, 3)), &bytes.Buffer{}); err == nil {
		t.Fatal("expected an error for an empty image width")
	}
	// a sub-image with zero bounds width must fail the same way
	src := image.NewGray(image.Rect(0, 0, 10, 3))
	if _, err := (&Encoder{}).Encode(src.SubImage(image.Rect(5, 1, 5, 3)), &bytes.Buffer{}); err == nil {
		t.Fatal("expected an error for an empty sub-image")
	}
}

func TestEncodeExtraChunks(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	src.SetNRGBA(0, 0, color.NRGBA{R: 1, G: 2, B: 3, A: 4})
	var phys []byte
	phys = append(phys, []byte{0, 0, 0x3B, 0x1A}...) // 15130 pixels per meter
	phys = append(phys, []byte{0, 0, 0x3B, 0x1A}...)
	phys = append(phys, 1) // meters
	name, payload, err := NewITXTChunk("XML:com.adobe.xmp", "", "", "<xmp>v1</xmp>", false)
	if err != nil {
		t.Fatal(err)
	}
	buf := &bytes.Buffer{}
	n, err := (&Encoder{}).Encode(src, buf,
		RawChunk{Name: PHYSChunk, Data: phys},
		RawChunk{Name: name, Data: payload},
	)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(buf.Len()) {
		t.Fatalf("written size mismatch: %d, want %d", n, buf.Len())
	}
	// the extras must come between IHDR and the IDAT chunks
	codes := chunkTypeCodes(t, buf.Bytes())
	if len(codes) != 5 ||
		codes[0] != "IHDR" || codes[1] != "pHYs" || codes[2] != "iTXt" || codes[3] != "IDAT" || codes[4] != "IEND" {
		t.Fatalf("unexpected chunk order: %v", codes)
	}
	p, err := ParsePng(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if p.PHYS == nil || p.PHYS.X != 15130 || p.PHYS.UnitSpecifier != 1 {
		t.Fatalf("invalid phys: %+v", p.PHYS)
	}
	if len(p.ITXTs) != 1 || p.ITXTs[0].Text != "<xmp>v1</xmp>" {
		t.Fatalf("invalid itxt: %+v", p.ITXTs)
	}
	assertImageEqual(t, decodeWithStd(t, buf.Bytes()), parseAndDecode(t, buf.Bytes()))

	// forbidden extra chunk names must be rejected
	for _, bad := range []ChunkName{IHDRChunk, IDATChunk, IENDChunk, "abc"} {
		buf.Reset()
		if _, err = (&Encoder{}).Encode(src, buf, RawChunk{Name: bad, Data: []byte{0}}); err == nil {
			t.Fatalf("expected an error for extra chunk %q", string(bad))
		}
	}
}

func TestEncodeChunkSize(t *testing.T) {
	const width, height = 96, 64
	src := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: uint8(x * y), G: uint8(x + y), B: uint8(x | y), A: 255})
		}
	}
	buf := &bytes.Buffer{}
	if _, err := (&Encoder{ChunkSize: 100}).Encode(src, buf); err != nil {
		t.Fatal(err)
	}
	nIDAT := 0
	for _, c := range chunkTypeCodes(t, buf.Bytes()) {
		if c == "IDAT" {
			nIDAT++
		}
	}
	if nIDAT < 2 {
		t.Fatalf("expected several idat chunks, got %d", nIDAT)
	}
	assertImageEqual(t, decodeWithStd(t, buf.Bytes()), parseAndDecode(t, buf.Bytes()))
}

func TestEncodeCompressionLevels(t *testing.T) {
	// every level must produce a stream decoding to the same pixels; on a
	// smooth gradient the filter heuristic + default level must not be
	// worse than no compression at all
	src := image.NewGray(image.Rect(0, 0, 128, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			src.SetGray(x, y, color.Gray{Y: uint8(x / 2)})
		}
	}
	sizes := map[string]int{}
	for name, enc := range map[string]Encoder{
		"none": {FilterStrategy: FilterNone, CompressionLevel: CompressionNone},
		"fast": {CompressionLevel: 1},
		"def":  {},
		"best": {CompressionLevel: 9},
	} {
		data := mustEncode(t, src, enc)
		sizes[name] = len(data)
		assertImageEqual(t, decodeWithStd(t, data), parseAndDecode(t, data))
	}
	if sizes["def"] > sizes["none"] {
		t.Fatalf("default compression larger than compression off: %d vs %d", sizes["def"], sizes["none"])
	}
}

func TestReplaceImage(t *testing.T) {
	p, err := ParsePng(bytes.NewReader(mustReadFile(t, "./png-format.png")))
	if err != nil {
		t.Fatal(err)
	}
	const newWidth, newHeight = 200, 120
	ref := image.NewNRGBA(image.Rect(0, 0, newWidth, newHeight))
	for y := 0; y < newHeight; y++ {
		for x := 0; x < newWidth; x++ {
			ref.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: uint8(x ^ y), A: 255})
		}
	}
	if err = p.ReplaceImage(ref); err != nil {
		t.Fatal(err)
	}
	written, err := p.MarshalBytes()
	if err != nil {
		t.Fatal(err)
	}
	p2, err := ParsePng(bytes.NewReader(written))
	if err != nil {
		t.Fatal(err)
	}
	if p2.Width != newWidth || p2.Height != newHeight {
		t.Fatalf("invalid size after replace: %dx%d", p2.Width, p2.Height)
	}
	if _, ok := p2.RawChunkByName("sRGB"); !ok {
		t.Fatal("sRGB chunk was lost while replacing the image")
	}
	codes := chunkTypeCodes(t, written)
	if codes[0] != "IHDR" || codes[1] != "sRGB" || codes[len(codes)-1] != "IEND" {
		t.Fatalf("unexpected chunk order: %v", codes)
	}
	assertImageEqual(t, ref, decodeWithStd(t, written))
	assertImageEqual(t, ref, parseAndDecode(t, written))

	// replacing again with the same type must work
	if err = p2.ReplaceImage(referenceNRGBA(ref)); err != nil {
		t.Fatalf("replacing with an equal image type must work: %v", err)
	}
	// a color type or bit depth mismatch must be rejected
	if err = p2.ReplaceImage(image.NewGray(image.Rect(0, 0, 10, 10))); err == nil {
		t.Fatal("expected a color type mismatch error")
	}
}

func TestReplacePaletted(t *testing.T) {
	palette := color.Palette{
		color.NRGBA{R: 0, G: 0, B: 0, A: 255},
		color.NRGBA{R: 255, G: 255, B: 255, A: 255},
		color.NRGBA{R: 0, G: 0, B: 255, A: 128},
		color.NRGBA{R: 255, G: 0, B: 0, A: 255},
	}
	src := image.NewPaletted(image.Rect(0, 0, 10, 7), palette)
	for y := 0; y < 7; y++ {
		for x := 0; x < 10; x++ {
			src.Pix[y*10+x] = uint8((x + y) % 4)
		}
	}
	p, err := ParsePng(bytes.NewReader(mustEncode(t, src, Encoder{})))
	if err != nil {
		t.Fatal(err)
	}
	// replacing with another palette of the same size class (<= 16 entries
	// => 4 bit) works, at any size
	ref := image.NewPaletted(image.Rect(0, 0, 12, 5), palette)
	for y := 0; y < 5; y++ {
		for x := 0; x < 12; x++ {
			ref.Pix[y*12+x] = uint8((x * 2 + y) % 4)
		}
	}
	if err = p.ReplaceImage(ref); err != nil {
		t.Fatal(err)
	}
	written, err := p.MarshalBytes()
	if err != nil {
		t.Fatal(err)
	}
	p2, err := ParsePng(bytes.NewReader(written))
	if err != nil {
		t.Fatal(err)
	}
	if p2.Width != 12 || p2.Height != 5 {
		t.Fatalf("invalid size: %dx%d", p2.Width, p2.Height)
	}
	assertImageEqual(t, ref, decodeWithStd(t, written))

	// a palette bigger than 16 entries produces an 8 bit file, that must
	// not replace a 4 bit one
	big := color.Palette{
		color.RGBA{R: 1, G: 2, B: 3, A: 255},
	}
	for i := 0; i < 24; i++ {
		big = append(big, color.RGBA{R: uint8(i), G: 20, B: 30, A: 255})
	}
	if err = p2.ReplaceImage(image.NewPaletted(image.Rect(0, 0, 5, 5), big)); err == nil {
		t.Fatal("expected a bit depth mismatch error")
	}
}

func TestReplaceImageInvalid(t *testing.T) {
	// demo.png is a color type 2 (rgb) file, an alpha image must be rejected
	p, err := ParsePng(bytes.NewReader(mustReadFile(t, "./demo.png")))
	if err != nil {
		t.Fatal(err)
	}
	if err = p.ReplaceImage(image.NewNRGBA(image.Rect(0, 0, 50, 50))); err == nil {
		t.Fatal("expected a color type mismatch error for the rgb file")
	}
}

// mustReadFile reads a test file, skipping the test when the repo binary
// files are not readable in the current environment.
func mustReadFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Skipf("%s is not readable in this environment: %v", name, err)
	}
	return data
}

func BenchmarkEncode(b *testing.B) {
	src := image.NewNRGBA(image.Rect(0, 0, 575, 1083))
	for y := 0; y < 1083; y++ {
		for x := 0; x < 575; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: uint8(x * y), A: 255})
		}
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Encode(src, &bytes.Buffer{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeStd(b *testing.B) {
	src := image.NewNRGBA(image.Rect(0, 0, 575, 1083))
	for y := 0; y < 1083; y++ {
		for x := 0; x < 575; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: uint8(x * y), A: 255})
		}
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := stdpng.Encode(&bytes.Buffer{}, src); err != nil {
			b.Fatal(err)
		}
	}
}
