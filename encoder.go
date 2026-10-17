package simple_png

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"image"
	"image/color"
	"io"

	"github.com/pkg/errors"
)

// CompressionDefault is the zero value of Encoder.CompressionLevel and means
// the zlib default compression level (6).
const CompressionDefault = 0

// CompressionNone switches the zlib compressor to "huffman only" mode: still
// lossless, the fastest level, but with the biggest output size.
const CompressionNone = -2

// FilterStrategy selects how scanline filters are chosen while encoding.
type FilterStrategy uint8

const (
	// FilterHeuristic picks the cheapest of the five png filter types per
	// scanline with the classic "minimum sum of absolute differences"
	// heuristic (the "adaptive filtering" recommendation of the spec). This
	// is the zero value: it normally gives the best compression.
	FilterHeuristic FilterStrategy = iota
	// FilterNone writes every scanline unfiltered (filter type 0 only),
	// which is the fastest to encode but compresses worst.
	FilterNone
)

// Encoder encodes standard library images as png streams. The pixel data of
// *image.Gray, *image.Gray16, *image.NRGBA, *image.NRGBA64 and
// *image.Paletted images is written directly from their Pix buffers; every
// other image.Image (premultiplied ones like *image.RGBA, *image.CMYK,
// sub-images...) is converted to 8 bit straight alpha RGB first.
//
// With Interlace the images are written as Adam7 interlaced streams.
type Encoder struct {
	// CompressionLevel is a zlib level: CompressionNone (-2), 1-9, or the
	// zero value CompressionDefault (0) for the default compression.
	CompressionLevel int
	// FilterStrategy selects the scanline filter heuristic, see the
	// FilterStrategy constants. FilterHeuristic costs about 4 extra passes
	// over the scanlines.
	FilterStrategy FilterStrategy
	// ChunkSize limits the size of every IDAT chunk in bytes; 0 (the zero
	// value) produces one single IDAT chunk.
	ChunkSize int
	// Interlace writes the scanlines as seven Adam7 interlacing passes
	// (progressive rendering on every decoder), default is non-interlaced.
	Interlace bool
}

// Encode encodes img as a png stream into w and returns the number of bytes
// written and an error. The extra arguments define ancillary chunks written
// after IHDR (and after the internal PLTE/tRNS of a palette image) and
// before the first IDAT chunk, e.g. XMP metadata built with NewITXTChunk or
// a raw pHYs chunk. Their CRC field is ignored, it is recomputed while
// writing. IHDR, IDAT, IEND, PLTE and tRNS cannot be passed as an extra
// chunk (PLTE/tRNS would collide with the ones of a palette image, use the
// chunks of an already parsed file through ReplaceImage instead).
func (e *Encoder) Encode(img image.Image, w io.Writer, extra ...RawChunk) (int64, error) {
	enc, err := encodeRows(img)
	if err != nil {
		return 0, err
	}
	parts, err := e.composeRows(enc, extra, e.interlaceMethod())
	if err != nil {
		return 0, err
	}
	cw := &chunkWriter{w: w}
	if err = cw.write(pngHeaderBytes); err != nil {
		return cw.n, err
	}
	for _, part := range parts {
		if err = cw.writeChunk(part.name, part.data); err != nil {
			return cw.n, err
		}
	}
	return cw.n, nil
}

// Encode encodes img as a png stream with default encoder settings (filter
// heuristic and default zlib compression), see Encoder for the details.
func Encode(img image.Image, w io.Writer, extra ...RawChunk) (int64, error) {
	return (&Encoder{}).Encode(img, w, extra...)
}
// ReplaceImage re-encodes the pixels of img and swaps them into an already
// parsed png, keeping every other chunk (metadata like tEXt, iTXt/XMP,
// tIME, raw chunks...) and its position in the stream untouched. The new
// image must match the color type and bit depth of the file (any size is
// allowed): the IHDR and the reported image dimensions are updated. Use it
// to replace the pixels of an existing file without losing its metadata.
// The whole operation runs under the internal write lock, so it is safe to
// call concurrently with the other editing methods and the readers of the
// same instance.
func (p *Png) ReplaceImage(img image.Image) error {
	p.Lock()
	defer p.Unlock()
	if p.IHDR == nil {
		return errors.New("png is not parsed")
	}
	enc, err := encodeRows(img)
	if err != nil {
		return err
	}
	if enc.colorType != p.IHDR.ColorType || enc.bitDepth != p.IHDR.BitDepth {
		return errors.Errorf(
			"cannot replace the image: the png has color type %d with bit depth %d, the given image has color type %d with bit depth %d",
			p.IHDR.ColorType, p.IHDR.BitDepth, enc.colorType, enc.bitDepth)
	}
	parts, err := (&Encoder{}).composeRows(enc, nil, p.IHDR.InterlaceMethod)
	if err != nil {
		return err
	}
	var idatData [][]byte
	for _, part := range parts {
		if part.name == IDATChunk {
			idatData = append(idatData, part.data)
		}
	}
	// update the IHDR values and the IDAT structs, WriteTo re-serializes
	// them from the structs afterwards
	p.IHDR.Width, p.IHDR.Height = uint32(enc.width), uint32(enc.height)
	p.IHDR.BitDepth, p.IHDR.ColorType = enc.bitDepth, enc.colorType
	p.Width, p.Height = enc.width, enc.height
	p.Channel = colorTypeChannels[enc.colorType]
	p.IDATs = make([]*IDAT, len(idatData))
	for i, d := range idatData {
		p.IDATs[i] = &IDAT{Length: uint32(len(d)), ChunkTypeCode: string(IDATChunk), Data: d}
	}
	// rebuild the original order list: everything except IDAT stays, the
	// new IDAT chunks take the position of the first IDAT (all further old
	// IDAT positions are dropped)
	var out []*chunk
	var consumed []bool
	var deleted []bool
	inserted := false
	for i, c := range p.origChunks {
		if ChunkName(c.code[:]) == IDATChunk {
			if inserted {
				continue
			}
			for _, d := range idatData {
				nc := &chunk{data: d}
				copy(nc.code[:], string(IDATChunk))
				by.PutUint32(nc.len[:], uint32(len(d)))
				out = append(out, nc)
				consumed = append(consumed, true)
				deleted = append(deleted, false)
			}
			inserted = true
			continue
		}
		out = append(out, c)
		consumed = append(consumed, p.origConsumed[i])
		deleted = append(deleted, p.origDeleted[i])
	}
	p.origChunks, p.origConsumed, p.origDeleted = out, consumed, deleted
	p.rebuildChunks()
	return nil
}

// fileChunk is one chunk of a stream to be written: name + data area.
type fileChunk struct {
	name ChunkName
	data []byte
}

// interlaceMethod returns the IHDR interlace method value of an Encoder.
func (e *Encoder) interlaceMethod() uint8 {
	if e.Interlace {
		return 1
	}
	return 0
}

// compose builds the full chunk list of a png datastream from the encoder
// settings: IHDR, [PLTE, tRNS], the extra chunks, the IDAT chunks and IEND.
func (e *Encoder) compose(enc *encodedImage, extra []RawChunk) ([]fileChunk, error) {
	return e.composeRows(enc, extra, e.interlaceMethod())
}

// composeRows builds the full chunk list of a png datastream with a given
// interlace method: IHDR, [PLTE, tRNS], the extra chunks, the IDAT chunks
// and IEND. ReplaceImage uses it to honor the interlace flag of the file it
// replaces the pixel data of.
func (e *Encoder) composeRows(enc *encodedImage, extra []RawChunk, interlace uint8) ([]fileChunk, error) {
	parts := make([]fileChunk, 0, 4+len(extra))
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[:4], uint32(enc.width))
	binary.BigEndian.PutUint32(ihdr[4:8], uint32(enc.height))
	ihdr[8], ihdr[9], ihdr[10], ihdr[11], ihdr[12] = enc.bitDepth, enc.colorType, 0, 0, interlace
	parts = append(parts, fileChunk{IHDRChunk, ihdr})
	if len(enc.plte) > 0 {
		parts = append(parts, fileChunk{PLTEChunk, enc.plte})
		if len(enc.trns) > 0 {
			parts = append(parts, fileChunk{TRNSChunk, enc.trns})
		}
	}
	for _, ex := range extra {
		if len(ex.Name) != 4 {
			return nil, errors.Errorf("invalid chunk name %q", string(ex.Name))
		}
		switch ex.Name {
		case IHDRChunk, IDATChunk, IENDChunk:
			return nil, errors.Errorf("cannot pass a %s chunk as an extra chunk", string(ex.Name))
		case PLTEChunk, TRNSChunk:
			if len(enc.plte) > 0 {
				return nil, errors.Errorf("cannot pass a %s chunk, the palette image already has one of its own", string(ex.Name))
			}
		}
		if len(ex.Data) > 0x7FFFFFFF {
			return nil, errors.Errorf("chunk %s data too large: %d bytes", string(ex.Name), len(ex.Data))
		}
		parts = append(parts, fileChunk{ex.Name, ex.Data})
	}
	// one zlib stream over all passes (or the whole image when not
	// interlaced), the filter heuristic restarts per pass with an empty
	// previous scanline as the spec requires
	bpp := (int(enc.channels)*int(enc.bitDepth) + 7) / 8
	filtered, err := e.filterScanlines(enc, bpp, interlace)
	if err != nil {
		return nil, err
	}
	idats, err := e.compressChunks(filtered)
	if err != nil {
		return nil, err
	}
	for _, d := range idats {
		parts = append(parts, fileChunk{IDATChunk, d})
	}
	parts = append(parts, fileChunk{IENDChunk, nil})
	return parts, nil
}

// encodedImage is an image reduced to its png scanline representation plus
// the data areas of the color type specific chunks.
type encodedImage struct {
	width, height int
	channels      uint8
	bitDepth      uint8
	colorType     uint8
	// rows are the unfiltered scanlines
	rows [][]byte
	// plte and trns hold the data areas of the respective chunks, only set
	// for palette images
	plte []byte
	trns []byte
}

// stride returns the size of one unfiltered scanline in bytes.
func (enc *encodedImage) stride() int {
	bitsPerPixel := int(enc.channels) * int(enc.bitDepth)
	return (enc.width*bitsPerPixel + 7) / 8
}

// encodeRows converts an image.Image into the png scanline representation of
// the closest matching color type.
func encodeRows(img image.Image) (*encodedImage, error) {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return nil, errors.New("cannot encode an image with empty bounds")
	}
	switch src := img.(type) {
	case *image.Gray:
		enc := &encodedImage{width: width, height: height, channels: 1, bitDepth: 8, colorType: 0}
		enc.rows = scanRows(src.Pix, src.Stride, bounds, 1, width, height)
		return enc, nil
	case *image.Gray16:
		enc := &encodedImage{width: width, height: height, channels: 1, bitDepth: 16, colorType: 0}
		enc.rows = scanRows(src.Pix, src.Stride, bounds, 2, width, height)
		return enc, nil
	case *image.NRGBA:
		enc := &encodedImage{width: width, height: height, channels: 4, bitDepth: 8, colorType: 6}
		enc.rows = scanRows(src.Pix, src.Stride, bounds, 4, width, height)
		return enc, nil
	case *image.NRGBA64:
		enc := &encodedImage{width: width, height: height, channels: 4, bitDepth: 16, colorType: 6}
		enc.rows = scanRows(src.Pix, src.Stride, bounds, 8, width, height)
		return enc, nil
	case *image.Paletted:
		return encodePaletted(src, bounds, width, height)
	default:
		// e.g. *image.RGBA or *image.RGBA64 (premultiplied alpha),
		// *image.CMYK or sub-images: convert through the NRGBA color model
		// so the alpha values come out straight
		enc := &encodedImage{width: width, height: height, channels: 4, bitDepth: 8, colorType: 6}
		enc.rows = make([][]byte, height)
		for y := 0; y < height; y++ {
			row := make([]byte, width*4)
			for x := 0; x < width; x++ {
				c := color.NRGBAModel.Convert(img.At(x+bounds.Min.X, y+bounds.Min.Y)).(color.NRGBA)
				o := x * 4
				row[o], row[o+1], row[o+2], row[o+3] = c.R, c.G, c.B, c.A
			}
			enc.rows[y] = row
		}
		return enc, nil
	}
}

// encodePaletted converts a *image.Paletted into the palette/index format,
// with a 4 bit index depth when the palette fits 16 entries.
func encodePaletted(src *image.Paletted, bounds image.Rectangle, width, height int) (*encodedImage, error) {
	palette := src.Palette
	if len(palette) == 0 {
		return nil, errors.New("cannot encode a paletted image with an empty palette")
	}
	if len(palette) > 256 {
		return nil, errors.Errorf("palette has %d entries, png supports at most 256", len(palette))
	}
	depth := 8
	if len(palette) <= 16 {
		depth = 4
	}
	enc := &encodedImage{width: width, height: height, channels: 1, bitDepth: uint8(depth), colorType: 3}
	// palette entries are written with straight alpha: NRGBA entries keep
	// their values while premultiplied entries must be inverted through the
	// NRGBA color model
	var alphas []uint8
	enc.plte = make([]byte, 0, len(palette)*3)
	for _, c := range palette {
		var r, g, b, a uint8
		if n, ok := c.(color.NRGBA); ok {
			r, g, b, a = n.R, n.G, n.B, n.A
		} else {
			n := color.NRGBAModel.Convert(c).(color.NRGBA)
			r, g, b, a = n.R, n.G, n.B, n.A
		}
		enc.plte = append(enc.plte, r, g, b)
		alphas = append(alphas, a)
	}
	// trailing opaque entries are unnecessary in tRNS, trim them (the spec
	// allows shorter tRNS chunks, every further entry is opaque)
	last := len(alphas)
	for last > 0 && alphas[last-1] == 255 {
		last--
	}
	enc.trns = alphas[:last]

	stride := (width*depth + 7) / 8
	enc.rows = make([][]byte, height)
	for y := 0; y < height; y++ {
		first := (bounds.Min.Y+y)*src.Stride + bounds.Min.X
		pix := src.Pix[first : first+width]
		row := make([]byte, stride)
		if depth == 8 {
			copy(row, pix)
		} else {
			for x := 0; x < width; x++ {
				writeSample(row, depth, x, uint16(pix[x]))
			}
		}
		enc.rows[y] = row
	}
	return enc, nil
}

// scanRows returns the scanlines of an image Pix buffer as row slices,
// reading bps bytes per pixel from anywhere inside the image bounds.
func scanRows(pix []byte, stride int, bounds image.Rectangle, bps, width, height int) [][]byte {
	rows := make([][]byte, height)
	for y := range rows {
		first := (bounds.Min.Y+y)*stride + bounds.Min.X*bps
		rows[y] = pix[first : first+width*bps]
	}
	return rows
}

// interlacePasses builds the unfiltered scanlines of the seven Adam7 passes
// from the unfiltered scanlines of the whole image. Every pass is a row
// slice of its own: pass 1 starts at pixel (0,0), pass 4 at (2,0)..., the
// skipped passes of very small images are nil.
func interlacePasses(enc *encodedImage) ([][][]byte, error) {
	passes := make([][][]byte, 0, 7)
	if enc.bitDepth >= 8 {
		bps := int(enc.channels) * int(enc.bitDepth) / 8
		for _, ps := range adam7Passes {
			pw, ph := adam7PassSize(enc.width, enc.height, ps)
			if pw <= 0 || ph <= 0 {
				passes = append(passes, nil)
				continue
			}
			rows := make([][]byte, 0, ph)
			for y := ps.yOff; y < enc.height; y += ps.dy {
				src := enc.rows[y]
				row := make([]byte, 0, pw*bps)
				for x := ps.xOff; x < enc.width; x += ps.dx {
					row = append(row, src[x*bps:(x+1)*bps]...)
				}
				rows = append(rows, row)
			}
			passes = append(passes, rows)
		}
		return passes, nil
	}
	// sub-byte samples (palette depths 1, 2, 4), packed MSB-first with
	// padding in the last byte of every pass scanline
	depth := int(enc.bitDepth)
	spB := 8 / depth
	mask := byte(1)<<uint(depth) - 1
	for _, ps := range adam7Passes {
		pw, ph := adam7PassSize(enc.width, enc.height, ps)
		if pw <= 0 || ph <= 0 {
			passes = append(passes, nil)
			continue
		}
		stride := (pw*int(enc.channels)*int(enc.bitDepth) + 7) / 8
		rows := make([][]byte, 0, ph)
		for y := ps.yOff; y < enc.height; y += ps.dy {
			src := enc.rows[y]
			row := make([]byte, stride)
			k := 0
			for x := ps.xOff; x < enc.width; x += ps.dx {
				v := (src[x/spB] >> uint(8-depth*(x%spB+1))) & mask
				writeSample(row, depth, k, uint16(v))
				k++
			}
			rows = append(rows, row)
		}
		passes = append(passes, rows)
	}
	return passes, nil
}

// filterScanlines applies png filter method 0 to the unfiltered scanlines:
// one filter sequence for the whole image when interlace is 0, else one
// sequence per Adam7 pass concatenated (the pass layout of interlacePasses
// restarts the filter heuristic per pass with an empty previous scanline).
func (e *Encoder) filterScanlines(enc *encodedImage, bpp int, interlace uint8) ([]byte, error) {
	if interlace == 0 {
		return e.applyFilterData(enc.rows, int(bpp))
	}
	passes, err := interlacePasses(enc)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, (enc.stride()+1)*enc.height)
	for _, rows := range passes {
		if len(rows) == 0 {
			continue
		}
		filtered, err := e.applyFilterData(rows, int(bpp))
		if err != nil {
			return nil, err
		}
		out = append(out, filtered...)
	}
	return out, nil
}

// applyFilterData applies png filter method 0 to one sequence of scanlines
// (all rows of the full image or all rows of one Adam7 pass). Rows may vary
// in width across sequences, inside a sequence they are equal.
func (e *Encoder) applyFilterData(rows [][]byte, bpp int) ([]byte, error) {
	var capStrides int
	for _, row := range rows {
		capStrides += len(row) + 1
	}
	out := make([]byte, 0, capStrides)
	if e.FilterStrategy == FilterNone {
		for _, row := range rows {
			out = append(out, 0)
			out = append(out, row...)
		}
		return out, nil
	}
	// find the widest scanline for the shared scratch buffers
	stride := 0
	for _, row := range rows {
		if len(row) > stride {
			stride = len(row)
		}
	}
	// heuristic: for every scanline compute all five filter types and keep
	// the one with the smallest sum of absolute values of the filtered
	// bytes (the smaller the sum, the smaller the signed deltas the
	// compressor has to encode)
	// the filter types work per byte with a bpp distance of one
	// sample, more precisely one full pixel in bytes (2 bytes for 16 bit
	// gray samples, 8 bytes for 64 bit rgba samples)
	var bufs [5][]byte
	for i := range bufs {
		bufs[i] = make([]byte, stride)
	}
	var sums [5]int
	prev := make([]byte, stride)
	for _, row := range rows {
		best := 0
		for ft := 0; ft < 5; ft++ {
			sums[ft] = filterRow(bufs[ft], row, prev, bpp, ft)
			if sums[ft] < sums[best] {
				best = ft
			}
		}
		out = append(out, byte(best))
		out = append(out, bufs[best][:len(row)]...)
		copy(prev, row)
	}
	return out, nil
}

// filterRow computes the filtered scanline of one filter type into buf and
// returns the sum of the absolute values of the filtered bytes.
func filterRow(buf, orig, prev []byte, bpp, ft int) int {
	sum := 0
	for x := 0; x < len(orig); x++ {
		var a, c byte
		if x >= bpp {
			a, c = orig[x-bpp], prev[x-bpp]
		}
		b := prev[x]
		var v byte
		switch ft {
		case 0:
			v = orig[x]
		case 1:
			v = orig[x] - a
		case 2:
			v = orig[x] - b
		case 3:
			v = orig[x] - byte((int(a)+int(b))/2)
		case 4:
			v = orig[x] - paethFilter(a, b, c)
		}
		buf[x] = v
		sum += absInt(int(int8(v)))
	}
	return sum
}

// compressChunks compresses the filtered scanline stream with zlib and cuts
// it into the IDAT chunk data parts.
func (e *Encoder) compressChunks(data []byte) ([][]byte, error) {
	level := e.CompressionLevel
	if level == 0 {
		level = zlib.DefaultCompression
	}
	var buf bytes.Buffer
	zw, err := zlib.NewWriterLevel(&buf, level)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if _, err = zw.Write(data); err != nil {
		return nil, errors.WithStack(err)
	}
	if err = zw.Close(); err != nil {
		return nil, errors.WithStack(err)
	}
	compressed := buf.Bytes()
	if e.ChunkSize <= 0 {
		return [][]byte{compressed}, nil
	}
	size := e.ChunkSize
	if size > 0x7FFFFFFF {
		size = 0x7FFFFFFF
	}
	pieces := make([][]byte, 0, len(compressed)/size+1)
	for off := 0; off < len(compressed); off += size {
		end := off + size
		if end > len(compressed) {
			end = len(compressed)
		}
		pieces = append(pieces, compressed[off:end])
	}
	return pieces, nil
}
