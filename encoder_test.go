package simple_png

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	stdpng "image/png"
	"os"
	"testing"
)

// referenceNRGBA 将任意图像逐像素转换为 NRGBA 色彩模型下的直通 alpha 参考图，
// 作为编码器处理预乘 alpha 源图时必须复现的基准。
//
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

// assertEncodeRoundtrip 用每一种给定的编码器设置编码 src，再验证输出流在标准库
// 解码器和本库解码器下都还原为给定的参考图 ref。
//
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

// mustEncode 用给定编码器编码 src 并返回完整流字节，任何错误都以 t.Fatal 终止测试。
//
// mustEncode encodes src with e and returns the full stream bytes,
// failing the test with t.Fatal on any error.
func mustEncode(t *testing.T, src image.Image, e Encoder) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := e.Encode(src, &buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestEncodeGray 验证 8 位灰度图在默认、无过滤、无压缩、级别 9 等设置下的编码往返。
//
// TestEncodeGray round-trips an 8-bit grayscale image through the default,
// unfiltered, uncompressed and level-9 encoder settings.
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

// TestEncodeGray16 验证 16 位灰度图在默认与无过滤设置下的编码往返。
//
// TestEncodeGray16 round-trips a 16-bit grayscale image through the default
// and unfiltered encoder settings.
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

// TestEncodeNRGBA 验证带直通 alpha 的 NRGBA 图像在默认与无过滤设置下的编码往返。
//
// TestEncodeNRGBA round-trips an NRGBA image with straight alpha through the
// default and unfiltered encoder settings.
func TestEncodeNRGBA(t *testing.T) {
	const width, height = 9, 7
	src := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			// 颜色与 alpha 均取渐变值，用于暴露字节序交换错误 / ramp colors and alpha to catch byte swap errors
			src.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x * 28), G: uint8(y * 36), B: uint8(x + y*7), A: uint8(255 - (x+y)%4*60),
			})
		}
	}
	assertEncodeRoundtrip(t, src, src, Encoder{}, Encoder{FilterStrategy: FilterNone})
}

// TestEncodeNRGBA64 验证 16 位分量的 NRGBA64 图像在默认与无过滤设置下的编码往返。
//
// TestEncodeNRGBA64 round-trips a 16-bit-per-channel NRGBA64 image through
// the default and unfiltered encoder settings.
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

// TestEncodePremultipliedRGBA 验证预乘 alpha 的 RGBA 图像：编码结果必须与
// referenceNRGBA 得到的直通 alpha 参考图一致。
//
// TestEncodePremultipliedRGBA verifies that a premultiplied RGBA image is
// encoded so that it decodes to the straight alpha result of referenceNRGBA.
func TestEncodePremultipliedRGBA(t *testing.T) {
	const width, height = 8, 8
	src := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			// 半透明的预乘取值用于验证回退路径的直通 alpha 反算
			// the semi-transparent, premultiplied values verify the
			// straight alpha inversion of the fallback path
			src.SetRGBA(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 30), B: 200, A: uint8(128 + (x+y)/2)})
		}
	}
	assertEncodeRoundtrip(t, src, referenceNRGBA(src), Encoder{}, Encoder{FilterStrategy: FilterNone})
}

// TestEncodePalettedWithAlpha 验证含 alpha 的调色板图像编码往返，并检查输出为
// 4 位调色板 PNG 且 tRNS 携带正确的 alpha 值。
//
// TestEncodePalettedWithAlpha round-trips a paletted image with alpha and
// checks that the output is a 4-bit palette PNG whose tRNS chunk carries
// the expected alpha values.
func TestEncodePalettedWithAlpha(t *testing.T) {
	palette := color.Palette{
		color.RGBA{R: 10, G: 20, B: 30, A: 255},
		color.NRGBA{R: 200, G: 50, B: 100, A: 255},
		color.RGBA{R: 0, G: 255, B: 255, A: 255},
		color.NRGBA{R: 90, G: 40, B: 200, A: 128}, // 半透明 / semi transparent
		color.NRGBA{R: 255, G: 255, B: 255, A: 0}, // 完全透明 / fully transparent
	}
	const width, height = 19, 4
	src := image.NewPaletted(image.Rect(0, 0, width, height), palette)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			src.Pix[y*width+x] = uint8((x + y) % len(palette))
		}
	}
	assertEncodeRoundtrip(t, src, src, Encoder{}, Encoder{FilterStrategy: FilterNone})
	// <= 16 项且含直通 alpha 的调色板必须输出为 4 位调色板 PNG，
	// tRNS 块携带 [255, 255, 255, 128, 0] 这组期望值
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

// TestEncodeEmptyImage 验证零宽图像与零宽子图像的编码都会返回错误。
//
// TestEncodeEmptyImage verifies that encoding a zero-width image, including
// a sub-image with zero bounds width, returns an error.
func TestEncodeEmptyImage(t *testing.T) {
	// 零宽图像 / zero width
	if _, err := (&Encoder{}).Encode(image.NewGray(image.Rect(0, 0, 0, 3)), &bytes.Buffer{}); err == nil {
		t.Fatal("expected an error for an empty image width")
	}
	// 零宽子图像也必须以同样方式失败 / a sub-image with zero bounds width must fail the same way
	src := image.NewGray(image.Rect(0, 0, 10, 3))
	if _, err := (&Encoder{}).Encode(src.SubImage(image.Rect(5, 1, 5, 3)), &bytes.Buffer{}); err == nil {
		t.Fatal("expected an error for an empty sub-image")
	}
}

// TestEncodeExtraChunks 验证附加块（pHYs、iTXt）按规范插在 IHDR 与 IDAT 之间，
// 且非法或保留的附加块名会被编码器拒绝。
//
// TestEncodeExtraChunks verifies that extra chunks (pHYs, iTXt) are inserted
// between IHDR and IDAT as the spec requires, and that illegal or reserved
// extra chunk names are rejected by the encoder.
func TestEncodeExtraChunks(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	src.SetNRGBA(0, 0, color.NRGBA{R: 1, G: 2, B: 3, A: 4})
	var phys []byte
	phys = append(phys, []byte{0, 0, 0x3B, 0x1A}...) // 每米 15130 像素 / 15130 pixels per meter
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
	// 附加块必须位于 IHDR 与 IDAT 之间 / the extras must come between IHDR and the IDAT chunks
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

	// 被禁止的附加块名必须被拒绝 / forbidden extra chunk names must be rejected
	for _, bad := range []ChunkName{IHDRChunk, IDATChunk, IENDChunk, "abc"} {
		buf.Reset()
		if _, err = (&Encoder{}).Encode(src, buf, RawChunk{Name: bad, Data: []byte{0}}); err == nil {
			t.Fatalf("expected an error for extra chunk %q", string(bad))
		}
	}
}

// TestEncodeChunkSize 验证小 ChunkSize 会把数据拆成多个 IDAT 块，且解码像素不变。
//
// TestEncodeChunkSize verifies that a small ChunkSize splits the data into
// several IDAT chunks without changing the decoded pixels.
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

// TestEncodeCompressionLevels 验证各压缩级别都解出相同像素，并且平滑渐变图上
// 默认设置（过滤启发式 + 默认级别）的输出不比完全不压缩更大。
//
// TestEncodeCompressionLevels verifies that every compression level decodes
// to the same pixels and that on a smooth gradient the default settings
// (filter heuristic + default level) are never larger than no compression.
func TestEncodeCompressionLevels(t *testing.T) {
	// 每个级别都必须解出相同像素；平滑渐变上过滤启发式 + 默认级别
	// 的输出不得比完全不压缩还大
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

// TestReplaceImage 验证 ReplaceImage：替换像素后尺寸更新、sRGB 等辅助块保留、
// 块顺序正确，并拒绝颜色类型不匹配的替换。
//
// TestReplaceImage verifies ReplaceImage: the size is updated, ancillary
// chunks like sRGB survive, the chunk order stays valid, and a color type
// mismatch is rejected.
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
	if p2.SRGB == nil {
		t.Fatal("sRGB struct was lost while replacing the image")
	}
	codes := chunkTypeCodes(t, written)
	if codes[0] != "IHDR" || codes[1] != "sRGB" || codes[len(codes)-1] != "IEND" {
		t.Fatalf("unexpected chunk order: %v", codes)
	}
	assertImageEqual(t, ref, decodeWithStd(t, written))
	assertImageEqual(t, ref, parseAndDecode(t, written))

	// 同类型再次替换必须成功 / replacing again with the same type must work
	if err = p2.ReplaceImage(referenceNRGBA(ref)); err != nil {
		t.Fatalf("replacing with an equal image type must work: %v", err)
	}
	// 颜色类型或位深不匹配必须被拒绝 / a color type or bit depth mismatch must be rejected
	if err = p2.ReplaceImage(image.NewGray(image.Rect(0, 0, 10, 10))); err == nil {
		t.Fatal("expected a color type mismatch error")
	}
}

// TestReplacePaletted 验证调色板图像的替换：同位深类别（<= 16 项 => 4 位）的
// 调色板可任意尺寸替换，超过 16 项（8 位）必须被拒绝。
//
// TestReplacePaletted verifies paletted image replacement: a palette of the
// same size class (<= 16 entries => 4 bit) replaces at any size, while a
// palette larger than 16 entries (8 bit) must be rejected.
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
	// 同位深类别（<= 16 项 => 4 位）的调色板可任意尺寸替换
	// replacing with another palette of the same size class (<= 16 entries
	// => 4 bit) works, at any size
	ref := image.NewPaletted(image.Rect(0, 0, 12, 5), palette)
	for y := 0; y < 5; y++ {
		for x := 0; x < 12; x++ {
			ref.Pix[y*12+x] = uint8((x*2 + y) % 4)
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

	// 超过 16 项的调色板会生成 8 位文件，不能替换 4 位文件
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

// TestReplaceImageInvalid 验证对颜色类型 2（真彩 RGB）的文件替换为带 alpha
// 的图像时必须报颜色类型不匹配错误。
//
// TestReplaceImageInvalid verifies that replacing a color type 2 (truecolor
// RGB) file with an alpha image returns a color type mismatch error.
func TestReplaceImageInvalid(t *testing.T) {
	// demo.png 是颜色类型 2（RGB）文件，带 alpha 的图像必须被拒绝 / demo.png is a color type 2 (rgb) file, an alpha image must be rejected
	p, err := ParsePng(bytes.NewReader(mustReadFile(t, "./demo.png")))
	if err != nil {
		t.Fatal(err)
	}
	if err = p.ReplaceImage(image.NewNRGBA(image.Rect(0, 0, 50, 50))); err == nil {
		t.Fatal("expected a color type mismatch error for the rgb file")
	}
}

// mustReadFile 读取测试所需的仓库二进制文件；环境中不可读时以 t.Skip 跳过
// 测试而不是让其失败。
//
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

// TestEncodeAdam7Truecolor 验证 Adam7 隔行真彩（含 alpha）编码：IHDR 隔行方法
// 为 1，标准库解码器与本库 Adam7 解码器的结果一致。
//
// TestEncodeAdam7Truecolor verifies interlaced (Adam7) truecolor encoding
// with alpha: IHDR reports interlace method 1 and both the standard library
// decoder and the library's own decoder agree on the pixels.
func TestEncodeAdam7Truecolor(t *testing.T) {
	const width, height = 9, 7
	src := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			src.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x * 28), G: uint8(y * 36), B: uint8(x ^ y), A: uint8(255 - x*7 - y*9),
			})
		}
	}
	data := mustEncode(t, src, Encoder{Interlace: true})
	// 标准库校验 + IHDR 布局校验 / std lib check + IHDR layout check
	p, err := ParsePng(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if p.IHDR.InterlaceMethod != 1 {
		t.Fatalf("expected an interlaced file, got method %d", p.IHDR.InterlaceMethod)
	}
	assertImageEqual(t, src, decodeWithStd(t, data))
	// 本库 Adam7 解码器结果也必须一致 / our own Adam7 decoder must agree as well
	assertImageEqual(t, src, parseAndDecode(t, data))
}

// TestEncodeAdam7PaletteSubByte 验证隔行 + 4 位调色板（每字节多个像素）编码：
// IHDR 报告位深 4、隔行方法 1，两个解码器结果一致。
//
// TestEncodeAdam7PaletteSubByte verifies interlaced 4-bit palette encoding
// (multiple pixels per byte): IHDR reports bit depth 4, interlace method 1,
// and both decoders agree on the pixels.
func TestEncodeAdam7PaletteSubByte(t *testing.T) {
	palette := color.Palette{
		color.NRGBA{R: 10, G: 20, B: 30, A: 255},
		color.NRGBA{R: 200, G: 50, B: 100, A: 255},
		color.NRGBA{R: 0, G: 255, B: 255, A: 255},
		color.NRGBA{R: 90, G: 40, B: 200, A: 128},
		color.NRGBA{R: 255, G: 255, B: 255, A: 255},
		color.NRGBA{R: 0, G: 0, B: 0, A: 0},
	}
	const width, height = 17, 5
	src := image.NewPaletted(image.Rect(0, 0, width, height), palette)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			src.Pix[y*width+x] = uint8((x + y) % len(palette))
		}
	}
	data := mustEncode(t, src, Encoder{Interlace: true})
	p, err := ParsePng(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if p.IHDR.BitDepth != 4 || p.IHDR.InterlaceMethod != 1 {
		t.Fatalf("expected an interlaced 4 bit file, got depth %d method %d", p.IHDR.BitDepth, p.IHDR.InterlaceMethod)
	}
	assertImageEqual(t, src, decodeWithStd(t, data))
	assertImageEqual(t, src, parseAndDecode(t, data))
}

// TestEncodeAdam7Gray16 验证隔行 16 位灰度编码，标准库与本库解码结果一致。
//
// TestEncodeAdam7Gray16 verifies interlaced 16-bit grayscale encoding; the
// standard library and the library's own decoder agree on the pixels.
func TestEncodeAdam7Gray16(t *testing.T) {
	const width, height = 8, 6
	src := image.NewGray16(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			src.SetGray16(x, y, color.Gray16{Y: uint16(6000*x + 9000*y + 3)})
		}
	}
	data := mustEncode(t, src, Encoder{Interlace: true})
	assertImageEqual(t, src, decodeWithStd(t, data))
	assertImageEqual(t, src, parseAndDecode(t, data))
}

// TestEncodeAdam7TinyImages 验证小尺寸图像的隔行编码：Adam7 各 pass 的偏移会
// 跳过整个 pass，这里列出的每个尺寸都必须经两个解码器往返成功。
//
// TestEncodeAdam7TinyImages verifies interlaced encoding of tiny images:
// the Adam7 pass offsets skip whole passes, so every listed size must
// round-trip through both decoders.
func TestEncodeAdam7TinyImages(t *testing.T) {
	// 隔行 pass 偏移会跳过小图的整个 pass，这里列出的每个尺寸都必须
	// 经标准库解码器往返成功
	// the pass offsets skip whole passes of small images, every size here
	// must roundtrip through the std decoder
	sizes := []struct{ w, h int }{
		{1, 1}, {2, 2}, {3, 3}, {4, 4}, {5, 5}, {1, 8}, {8, 1}, {7, 9},
	}
	for _, size := range sizes {
		src := image.NewGray(image.Rect(0, 0, size.w, size.h))
		for y := 0; y < size.h; y++ {
			for x := 0; x < size.w; x++ {
				src.SetGray(x, y, color.Gray{Y: uint8(17*x + 29*y + 5)})
			}
		}
		for _, enc := range []Encoder{
			{Interlace: true},
			{Interlace: true, FilterStrategy: FilterNone},
		} {
			data := mustEncode(t, src, enc)
			assertImageEqual(t, src, decodeWithStd(t, data))
			assertImageEqual(t, src, parseAndDecode(t, data))
		}
	}
}

// BenchmarkEncode 测量默认 Encode 对 575x1083 NRGBA 图像的吞吐与分配次数。
//
// BenchmarkEncode measures throughput and allocations of the default Encode
// on a 575x1083 NRGBA image.
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

// BenchmarkEncodeStd 用同尺寸图像测量标准库 png.Encode，作为对比基准。
//
// BenchmarkEncodeStd measures the standard library png.Encode on the same
// image size as a comparison baseline.
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

// graySamplesFrom extracts grayscale samples from a decoded image. For bit
// depths below 8 the standard library and this package both expand samples to
// 8 bits by 255/(2^d-1); the inverse is exact for 1/2/4.
func graySamplesFrom(img image.Image, depth, n int) ([]uint16, error) {
	out := make([]uint16, 0, n)
	switch m := img.(type) {
	case *image.Gray:
		scale := uint16(1)
		if depth < 8 {
			scale = 0xFF / ((1 << uint(depth)) - 1)
		}
		b := m.Rect
		for y := b.Min.Y; y < b.Max.Y && len(out) < n; y++ {
			row := m.Pix[(y-b.Min.Y)*m.Stride:]
			for x := 0; x < b.Dx() && len(out) < n; x++ {
				v := uint16(row[x])
				if v%scale != 0 {
					return nil, fmt.Errorf("gray byte %d is not a multiple of %d", v, scale)
				}
				out = append(out, v/scale)
			}
		}
	case *image.Gray16:
		b := m.Rect
		for y := b.Min.Y; y < b.Max.Y && len(out) < n; y++ {
			row := m.Pix[(y-b.Min.Y)*m.Stride:]
			for x := 0; x < b.Dx() && len(out) < n; x++ {
				out = append(out, uint16(row[x*2])<<8|uint16(row[x*2+1]))
			}
		}
	default:
		return nil, fmt.Errorf("unexpected image type %T", img)
	}
	return out, nil
}

// TestEncodeGrayBitDepths encodes grayscale images at every supported depth
// (including Adam7) and verifies the result with both the standard library
// decoder and this package's own decoder.
func TestEncodeGrayBitDepths(t *testing.T) {
	const width, height = 7, 5
	n := width * height
	cases := []struct {
		depth     uint8
		interlace bool
		samples   []uint16
	}{
		{1, false, nil}, {1, true, nil},
		{2, false, nil},
		{4, false, nil}, {4, true, nil},
		{8, false, nil}, {8, true, nil},
		{16, false, nil}, {16, true, nil},
	}
	for _, tc := range cases {
		tc := tc
		name := fmt.Sprintf("depth%d", tc.depth)
		if tc.interlace {
			name += "-adam7"
		}
		t.Run(name, func(t *testing.T) {
			samples := make([]uint16, n)
			mask := uint32(1)<<tc.depth - 1
			for i := range samples {
				samples[i] = uint16((uint32(i)*13 + 5) & mask)
			}
			var img image.Image
			if tc.depth == 16 {
				g := image.NewGray16(image.Rect(0, 0, width, height))
				for i, v := range samples {
					g.Pix[i*2] = byte(v >> 8)
					g.Pix[i*2+1] = byte(v)
				}
				img = g
			} else {
				g := image.NewGray(image.Rect(0, 0, width, height))
				for i, v := range samples {
					g.Pix[i] = byte(v)
				}
				img = g
			}

			var buf bytes.Buffer
			if _, err := (&Encoder{BitDepth: tc.depth, Interlace: tc.interlace}).Encode(img, &buf); err != nil {
				t.Fatalf("Encode: %v", err)
			}
			data := buf.Bytes()
			if data[24] != tc.depth || data[25] != 0 {
				t.Fatalf("IHDR bit depth/color type = %d/%d, want %d/0", data[24], data[25], tc.depth)
			}
			wantInterlace := byte(0)
			if tc.interlace {
				wantInterlace = 1
			}
			if data[28] != wantInterlace {
				t.Fatalf("IHDR interlace = %d, want %d", data[28], wantInterlace)
			}

			stdImg, err := stdpng.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("stdlib decode: %v", err)
			}
			stdSamples, err := graySamplesFrom(stdImg, int(tc.depth), n)
			if err != nil {
				t.Fatalf("stdlib samples: %v", err)
			}
			p, err := ParsePng(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("ParsePng: %v", err)
			}
			spImg, err := p.IDATsToColor()
			if err != nil {
				t.Fatalf("IDATsToColor: %v", err)
			}
			spSamples, err := graySamplesFrom(spImg, int(tc.depth), n)
			if err != nil {
				t.Fatalf("simple-png samples: %v", err)
			}
			for i, want := range samples {
				if stdSamples[i] != want {
					t.Errorf("stdlib value[%d] = %d, want %d", i, stdSamples[i], want)
				}
				if spSamples[i] != want {
					t.Errorf("simple-png value[%d] = %d, want %d", i, spSamples[i], want)
				}
			}
		})
	}
}

// TestEncodeGrayBitDepthFrom16 exercises the Gray16 → shallower depth path,
// where the source values must fit the requested depth.
func TestEncodeGrayBitDepthFrom16(t *testing.T) {
	g := image.NewGray16(image.Rect(0, 0, 4, 2))
	for i := range g.Pix {
		g.Pix[i] = 0
	}
	for i := 0; i < 8; i++ {
		v := uint16(i * 3) // 0..21 fits 8 bits
		g.Pix[i*2] = byte(v >> 8)
		g.Pix[i*2+1] = byte(v)
	}
	var buf bytes.Buffer
	if _, err := (&Encoder{BitDepth: 8}).Encode(g, &buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if buf.Bytes()[24] != 8 {
		t.Fatalf("IHDR bit depth = %d, want 8", buf.Bytes()[24])
	}
}

// TestEncodeGrayBitDepthErrors pins the fail-loud rules: unsupported depths,
// a 16-bit request from *image.Gray, non-grayscale sources and samples that
// do not fit the requested depth.
func TestEncodeGrayBitDepthErrors(t *testing.T) {
	g := image.NewGray(image.Rect(0, 0, 1, 1))
	g.Pix[0] = 1
	var buf bytes.Buffer
	if _, err := (&Encoder{BitDepth: 3}).Encode(g, &buf); err == nil {
		t.Error("BitDepth 3 accepted; want an error")
	}
	if _, err := (&Encoder{BitDepth: 16}).Encode(g, &buf); err == nil {
		t.Error("BitDepth 16 with *image.Gray accepted; want an error")
	}
	if _, err := (&Encoder{BitDepth: 8}).Encode(image.NewNRGBA(image.Rect(0, 0, 1, 1)), &buf); err == nil {
		t.Error("non-grayscale source accepted; want an error")
	}
	g.Pix[0] = 16
	if _, err := (&Encoder{BitDepth: 4}).Encode(g, &buf); err == nil {
		t.Error("sample 16 at depth 4 accepted; want an error")
	}
}

// TestReplaceImageGrayBitDepth checks that ReplaceImage keeps a 4-bit
// grayscale file at 4 bits instead of falling back to 8.
func TestReplaceImageGrayBitDepth(t *testing.T) {
	src := image.NewGray(image.Rect(0, 0, 4, 3))
	for i := range src.Pix {
		src.Pix[i] = byte(i % 16)
	}
	var buf bytes.Buffer
	if _, err := (&Encoder{BitDepth: 4}).Encode(src, &buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	p, err := ParsePng(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ParsePng: %v", err)
	}
	newImg := image.NewGray(image.Rect(0, 0, 5, 2))
	for i := range newImg.Pix {
		newImg.Pix[i] = byte(i % 16)
	}
	if err := p.ReplaceImage(newImg); err != nil {
		t.Fatalf("ReplaceImage: %v", err)
	}
	var out bytes.Buffer
	if _, err := p.WriteTo(&out); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if out.Bytes()[24] != 4 {
		t.Fatalf("IHDR bit depth = %d, want 4", out.Bytes()[24])
	}
	decoded, err := p.IDATsToColor()
	if err != nil {
		t.Fatalf("IDATsToColor: %v", err)
	}
	got, err := graySamplesFrom(decoded, 4, len(newImg.Pix))
	if err != nil {
		t.Fatalf("samples: %v", err)
	}
	for i, v := range newImg.Pix {
		if got[i] != uint16(v) {
			t.Errorf("value[%d] = %d, want %d", i, got[i], v)
		}
	}
}

// TestReplaceImageGrayBitDepthRejectsWideSamples pins the no-scaling rule for
// ReplaceImage as well: a 4-bit file rejects a source sample above 15.
func TestReplaceImageGrayBitDepthRejectsWideSamples(t *testing.T) {
	src := image.NewGray(image.Rect(0, 0, 1, 1))
	var buf bytes.Buffer
	if _, err := (&Encoder{BitDepth: 4}).Encode(src, &buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	p, err := ParsePng(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ParsePng: %v", err)
	}
	wide := image.NewGray(image.Rect(0, 0, 1, 1))
	wide.Pix[0] = 16
	if err := p.ReplaceImage(wide); err == nil {
		t.Fatal("ReplaceImage accepted sample 16 for a 4-bit file; want an error")
	}
}
