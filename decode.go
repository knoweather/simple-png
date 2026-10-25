package simple_png

import (
	"bytes"
	"compress/zlib"
	"image"
	"image/color"
	"io"

	"github.com/pkg/errors"
)

// colorTypeChannels 把 PNG 颜色类型映射为每像素的采样个数，对应 PNG 规范
// IHDR color type 与样本数的对应表。
//
// colorTypeChannels maps a PNG color type to the number of samples per pixel,
// per the IHDR color type table of the PNG specification.
var colorTypeChannels = map[uint8]int{
	0: 1, // 灰度 / grayscale
	2: 3, // 真彩色 RGB / truecolor RGB
	3: 1, // 索引调色板 / indexed palette
	4: 2, // 灰度 + alpha / grayscale + alpha
	6: 4, // 真彩色 RGB + alpha / truecolor RGB + alpha
}

// inflateIDATs 拼接全部 IDAT chunk 的数据（规范允许任意多个 IDAT，其拼接结果
// 就是压缩数据流），再按压缩方法 0 的要求用 zlib 解压。解压出的字节数必须恰好
// 等于由 IHDR 推导出的 want。
//
// inflateIDATs concatenates the data of all IDAT chunks (the spec allows any
// number of them; their concatenation is the compressed datastream) and
// decompresses it with zlib as required by compression method 0. The
// decompressed size must be exactly the want bytes derived from IHDR.
func (p *Png) inflateIDATs(want int) ([]byte, error) {
	readers := make([]io.Reader, 0, len(p.IDATs))
	for _, idat := range p.IDATs {
		readers = append(readers, bytes.NewReader(idat.Data))
	}
	zr, err := zlib.NewReader(io.MultiReader(readers...))
	if err != nil {
		return nil, errors.WithStack(err)
	}
	raw := make([]byte, want)
	if _, err = io.ReadFull(zr, raw); err != nil {
		return nil, errors.WithMessage(err, "unexpected decompressed idat size")
	}
	// zlib 流必须在此处正好耗尽：还能读出一个字节，就说明解压后的图像数据
	// 多于 IHDR 声明的大小。
	// the zlib stream must be exhausted exactly here: another readable byte
	// means the decompressed image data is larger than IHDR says
	var extra [1]byte
	extraN, _ := zr.Read(extra[:])
	if extraN != 0 {
		return nil, errors.Errorf("unexpected decompressed idat size: larger than %d", want)
	}
	if err = zr.Close(); err != nil {
		return nil, errors.WithStack(err)
	}
	return raw, nil
}

// unfilterInto 逆向滤波方法 0 的扫描线滤波，把还原后的扫描线写入 dest
// （这些切片可能直接别名 image.Pix 缓冲）。每条扫描线消耗 1 字节滤波类型，
// 后跟 len(dest[y]) 字节数据；raw 必须被恰好耗尽。
//
// unfilterInto reverses the scanline filter of filter method 0, writing the
// unfiltered scanlines into dest (whose slices may alias an image.Pix
// buffer). Each scanline consumes one filter type byte followed by
// len(dest[y]) data bytes; raw must be exactly consumed.
func unfilterInto(dest [][]byte, raw []byte, bpp int) error {
	if len(dest) == 0 {
		if len(raw) != 0 {
			return errors.Errorf("unexpected decompressed idat size: got %d, want 0", len(raw))
		}
		return nil
	}
	prev := make([]byte, len(dest[0]))
	off := 0
	for y := 0; y < len(dest); y++ {
		cur := dest[y]
		if off+1+len(cur) > len(raw) {
			return errors.Errorf("unexpected decompressed idat size: only %d bytes left for scanline %d", len(raw)-off, y)
		}
		filter := raw[off]
		data := raw[off+1 : off+1+len(cur)]
		off += 1 + len(cur)
		switch filter {
		case 0: // None
			copy(cur, data)
		case 1: // Sub
			for x := 0; x < len(cur); x++ {
				var a byte
				if x >= bpp {
					a = cur[x-bpp]
				}
				cur[x] = data[x] + a
			}
		case 2: // Up：减去上邻样本 / Up: subtract the sample above
			for x := 0; x < len(cur); x++ {
				cur[x] = data[x] + prev[x]
			}
		case 3: // Average：减去左邻与上邻均值 / Average: subtract mean of left and above
			for x := 0; x < len(cur); x++ {
				var a byte
				if x >= bpp {
					a = cur[x-bpp]
				}
				cur[x] = data[x] + byte((int(a)+int(prev[x]))/2)
			}
		case 4: // Paeth：Paeth 预测子 / Paeth predictor
			for x := 0; x < len(cur); x++ {
				var a, c byte
				if x >= bpp {
					a = cur[x-bpp]
					c = prev[x-bpp]
				}
				cur[x] = data[x] + paethFilter(a, prev[x], c)
			}
		default:
			return errors.Errorf("invalid filter type %d at scanline %d", filter, y)
		}
		prev = cur
	}
	if off != len(raw) {
		return errors.Errorf("unexpected decompressed idat size: got %d, want %d", len(raw), off)
	}
	return nil
}

// paethFilter 计算 PNG 规范定义的 Paeth 预测子：取线性预测值 p = a+b-c，
// 返回 a、b、c 中与 p 距离最小者（并列时按 a、b、c 的顺序取先者）。
//
// paethFilter computes the Paeth predictor as defined in the PNG
// specification: it forms the linear prediction p = a+b-c and returns whichever
// of a, b, c is closest to it (ties broken in the order a, b, c).
func paethFilter(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	pa, pb, pc := absInt(p-int(a)), absInt(p-int(b)), absInt(p-int(c))
	if pa <= pb && pa <= pc {
		return a
	}
	if pb <= pc {
		return b
	}
	return c
}

// absInt 返回 i 的绝对值。
//
// absInt returns the absolute value of i.
func absInt(i int) int {
	if i < 0 {
		return -i
	}
	return i
}

// adam7Pass describes one Adam7 interlacing pass.
type adam7Pass struct {
	xOff, yOff int
	dx, dy     int
}

// adam7Passes 按扫描线在数据流中的存储顺序列出 Adam7 隔行的七个 pass，
// 见 PNG 规范中的 Adam7 示意图。
//
// adam7Passes are the seven Adam7 passes in the order their scanlines are
// stored in the datastream; see the Adam7 illustration in the PNG spec.
var adam7Passes = []adam7Pass{
	{0, 0, 8, 8},
	{4, 0, 8, 8},
	{0, 4, 4, 8},
	{2, 0, 4, 4},
	{0, 2, 2, 4},
	{1, 0, 2, 2},
	{0, 1, 1, 2},
}

// adam7PassSize 返回单个 pass 的像素尺寸 (pw, ph)；当步长在图像范围内产生
// 不出任何像素时该 pass 被跳过，此时结果为 0。
//
// adam7PassSize returns the pixel size (pw, ph) of a single pass; a pass is
// skipped when its increments yield no pixel of the image, in which case the
// result is 0.
func adam7PassSize(width, height int, ps adam7Pass) (pw, ph int) {
	return (width-ps.xOff+ps.dx-1)/ps.dx, (height-ps.yOff+ps.dy-1)/ps.dy
}

// adam7Size 返回 Adam7 隔行图像全部七个 pass 的扫描线解压后共有的字节数，
// 即期望解压长度（每条扫描线含 1 字节滤波类型）。
//
// adam7Size returns the number of decompressed bytes the scanlines of all
// seven passes of an Adam7 interlaced image consist of, i.e. the expected
// inflated length (one filter type byte per scanline).
func adam7Size(width, height, channels, bitDepth int) int {
	total := 0
	for _, ps := range adam7Passes {
		pw, ph := adam7PassSize(width, height, ps)
		if pw <= 0 || ph <= 0 {
			continue
		}
		passStride := (pw*channels*bitDepth + 7) / 8
		total += ph * (passStride + 1)
	}
	return total
}

// deinterlaceInto 逆向 Adam7 隔行：逐个 pass 反滤波，再把样本写入非隔行图像
// 中对应的坐标（各行可能直接别名 image.Pix 缓冲）。raw 是全部 pass 数据流的
// 拼接，必须被恰好耗尽。
//
// deinterlaceInto reverses Adam7 interlacing: the seven passes are
// unfiltered one by one and their pixels are placed into the positions of
// the non-interlaced image (the rows may alias an image.Pix buffer).
// raw is the concatenation of all pass streams and must be exactly consumed.
func deinterlaceInto(dest [][]byte, raw []byte, width, height, channels, bitDepth, bpp int) error {
	off := 0
	for pass, ps := range adam7Passes {
		pw, ph := adam7PassSize(width, height, ps)
		if pw <= 0 || ph <= 0 {
			continue
		}
		passStride := (pw*channels*bitDepth + 7) / 8
		// 每个 pass 都当作一幅独立图像来反滤波
		// each pass is unfiltered like a separate image
		block := make([]byte, passStride*ph)
		rows := make([][]byte, ph)
		for j := range rows {
			rows[j] = block[j*passStride : (j+1)*passStride]
		}
		seg := raw[off : off+ph*(passStride+1)]
		if err := unfilterInto(rows, seg, bpp); err != nil {
			return errors.WithMessagef(err, "deinterlacing pass %d", pass+1)
		}
		off += ph * (passStride + 1)
		for j := 0; j < ph; j++ {
			fy := ps.yOff + j*ps.dy
			row := rows[j]
			for i := 0; i < pw; i++ {
				fx := ps.xOff + i*ps.dx
				for c := 0; c < channels; c++ {
					v := sampleAt(row, bitDepth, i*channels+c)
					writeSample(dest[fy], bitDepth, fx*channels+c, v)
				}
			}
		}
	}
	if off != len(raw) {
		return errors.Errorf("unexpected decompressed idat size: got %d, want %d", len(raw), off)
	}
	return nil
}

// writeSample stores one raw sample at the given sample index of a row,
// packing most significant bit first for bit depths smaller than 8.
func writeSample(row []byte, bitDepth, index int, v uint16) {
	switch bitDepth {
	case 16:
		by.PutUint16(row[index*2:], v)
	case 8:
		row[index] = byte(v)
	default: // 1, 2 or 4 bits per sample
		pos := index * bitDepth
		off := uint(8 - pos&0x7 - bitDepth)
		mask := byte(1<<bitDepth-1) << off
		row[pos>>3] = row[pos>>3]&^mask | byte(v)<<off
	}
}

// pixRows turns the Pix buffer of one of the image types supported here into
// scanline slices, so the filters can write into the image buffer directly.
func pixRows(pix []byte, pixStride, height int) [][]byte {
	rows := make([][]byte, height)
	for y := range rows {
		rows[y] = pix[y*pixStride : (y+1)*pixStride]
	}
	return rows
}

// sampleAt extracts the index-th sample (0-based, in the current scanline)
// as a raw unsigned value of bitDepth bits. Samples are packed most
// significant bit first for bit depths smaller than 8.
func sampleAt(row []byte, bitDepth, index int) uint16 {
	switch bitDepth {
	case 16:
		return uint16(row[index*2])<<8 | uint16(row[index*2+1])
	case 8:
		return uint16(row[index])
	default: // 1, 2 or 4 bits per sample
		pos := index * bitDepth
		b := row[pos>>3]
		off := uint(8 - pos&0x7 - bitDepth)
		return uint16(b>>off) & uint16(1<<bitDepth-1)
	}
}

// scaleTo8 scales a raw sample of the given bit depth to the full [0, 255] range.
func scaleTo8(v uint16, bitDepth uint8) uint8 {
	switch bitDepth {
	case 8:
		return uint8(v)
	case 16:
		return uint8(v >> 8)
	default:
		return uint8(v) * uint8(255/(1<<bitDepth-1))
	}
}

// scaleTo16 scales a raw sample of the given bit depth to the full [0, 65535] range.
func scaleTo16(v uint16, bitDepth uint8) uint16 {
	switch bitDepth {
	case 16:
		return v
	case 8:
		return v<<8 | v
	default:
		return v * uint16(65535/(1<<bitDepth-1))
	}
}

// rawTransparent returns the raw transparency value(s) of the tRNS chunk for
// the given color type, re-interpreting the raw tRNS data now that the color
// type is known.
func (p *Png) rawTransparent(colorType uint8) (gray uint16, red, green, blue uint16, ok bool) {
	if p.TRNS == nil || len(p.TRNS.Data) == 0 {
		return
	}
	switch colorType {
	case 0:
		if len(p.TRNS.Data) == 2 {
			return by.Uint16(p.TRNS.Data[:2]), 0, 0, 0, true
		}
	case 2:
		if len(p.TRNS.Data) == 6 {
			return 0, by.Uint16(p.TRNS.Data[0:2]), by.Uint16(p.TRNS.Data[2:4]), by.Uint16(p.TRNS.Data[4:6]), true
		}
	}
	return
}

// paletteAlpha returns the alpha value of a palette entry, defaulting to 255
// (fully opaque) when the tRNS chunk does not cover that entry.
func (p *Png) paletteAlpha(index int) uint8 {
	if p.TRNS != nil && index < len(p.TRNS.Data) {
		return p.TRNS.Data[index]
	}
	return 255
}

// IDATsToColor decodes the image: it concatenates and decompresses all IDAT
// chunks, reverses the scanline filtering and converts the pixels to a
// standard library image.Image (Grayscale/Gray16, RGBA/RGBA64, NRGBA/NRGBA64
// or Paletted depending on the color type and bit depth).
//
// tRNS simple transparency is applied to grayscale, truecolor and palette
// images. Adam7 interlaced images are supported.
//
// The pixel decode runs under the internal read lock: it is safe to decode
// concurrently with other readers of the same instance, while editing
// methods block until the decode has finished.
func (p *Png) IDATsToColor() (image.Image, error) {
	p.RLock()
	defer p.RUnlock()
	if p.IHDR == nil || len(p.IDATs) == 0 {
		return nil, errors.New("png is not parsed or has no IDAT chunks")
	}
	if err := p.IHDR.Validate(); err != nil {
		return nil, errors.WithStack(err)
	}
	channels, ok := colorTypeChannels[p.IHDR.ColorType]
	if !ok {
		return nil, errors.Errorf("invalid color type %d", p.IHDR.ColorType)
	}
	if p.IHDR.ColorType == 3 && p.PLTE == nil {
		return nil, errors.New("palette image must contain a PLTE chunk")
	}

	width, height := int(p.IHDR.Width), int(p.IHDR.Height)
	bitDepth := int(p.IHDR.BitDepth)
	bitsPerPixel := channels * bitDepth
	stride := (width*bitsPerPixel + 7) / 8
	bpp := (bitsPerPixel + 7) / 8
	interlaced := p.IHDR.InterlaceMethod != 0

	var want int
	if interlaced {
		want = adam7Size(width, height, channels, bitDepth)
	} else {
		want = (stride + 1) * height
	}
	raw, err := p.inflateIDATs(want)
	if err != nil {
		return nil, err
	}

	// For the image types whose Pix layout matches the scanline byte layout
	// the scanlines are unfiltered directly into the image buffer, then no
	// intermediate scanline copy is needed at all. All other cases use one
	// temporary block for all scanlines.
	var img image.Image
	var rows [][]byte
	switch {
	case p.IHDR.ColorType == 0 && bitDepth == 8 && !p.hasGrayTRNS():
		g := image.NewGray(image.Rect(0, 0, width, height))
		img, rows = g, pixRows(g.Pix, width, height)
	case p.IHDR.ColorType == 0 && bitDepth == 16 && !p.hasGrayTRNS():
		// image.Gray16.Pix is big-endian per pixel, exactly the scanline layout
		g := image.NewGray16(image.Rect(0, 0, width, height))
		img, rows = g, pixRows(g.Pix, width*2, height)
	case p.IHDR.ColorType == 3 && bitDepth == 8:
		palette, err := p.buildPalette()
		if err != nil {
			return nil, err
		}
		g := image.NewPaletted(image.Rect(0, 0, width, height), palette)
		img, rows = g, pixRows(g.Pix, width, height)
	case p.IHDR.ColorType == 6 && bitDepth == 8:
		g := image.NewNRGBA(image.Rect(0, 0, width, height))
		img, rows = g, pixRows(g.Pix, width*4, height)
	case p.IHDR.ColorType == 6 && bitDepth == 16:
		// image.NRGBA64.Pix is big-endian R,G,B,A per pixel, the scanline layout
		g := image.NewNRGBA64(image.Rect(0, 0, width, height))
		img, rows = g, pixRows(g.Pix, width*8, height)
	default:
		block := make([]byte, stride*height)
		rows = make([][]byte, height)
		for y := range rows {
			rows[y] = block[y*stride : (y+1)*stride]
		}
	}

	if interlaced {
		err = deinterlaceInto(rows, raw, width, height, channels, bitDepth, bpp)
	} else {
		err = unfilterInto(rows, raw, bpp)
	}
	if err != nil {
		return nil, err
	}
	if img != nil {
		if pal, ok := img.(*image.Paletted); ok {
			if err = validatePaletteIndices(pal.Pix, len(pal.Palette)); err != nil {
				return nil, err
			}
		}
		return img, nil
	}
	return p.rowsToImage(rows, width, height, channels)
}

// hasGrayTRNS reports whether a tRNS gray level applies to this image
// (color type 0; color type 2 is checked while converting to pixels).
func (p *Png) hasGrayTRNS() bool {
	_, _, _, _, ok := p.rawTransparent(0)
	return ok
}

// buildPalette converts the PLTE chunk (with tRNS alpha values when present)
// into a standard library color palette.
func (p *Png) buildPalette() (color.Palette, error) {
	if p.PLTE == nil {
		return nil, errors.New("palette image must contain a PLTE chunk")
	}
	palette := make(color.Palette, len(p.PLTE.Colors))
	for i, c := range p.PLTE.Colors {
		a := p.paletteAlpha(i)
		if a == 255 {
			palette[i] = color.RGBA{R: c.Red, G: c.Green, B: c.Blue, A: a}
			continue
		}
		// for entries with a tRNS alpha value the png palette is straight
		// alpha, so keep it as such (the same choice as the standard library
		// png decoder, color.NRGBA reports premultiplied values)
		palette[i] = color.NRGBA{R: c.Red, G: c.Green, B: c.Blue, A: a}
	}
	return palette, nil
}

// validatePaletteIndices checks that no pixel references a palette entry
// beyond the end of the palette.
func validatePaletteIndices(pix []uint8, paletteSize int) error {
	for _, idx := range pix {
		if int(idx) >= paletteSize {
			return errors.Errorf("palette index %d out of range (%d entries)", idx, paletteSize)
		}
	}
	return nil
}

func (p *Png) rowsToImage(rows [][]byte, width, height, channels int) (image.Image, error) {
	bitDepth := p.IHDR.BitDepth
	switch p.IHDR.ColorType {
	case 0:
		gray, _, _, _, trns := p.rawTransparent(0)
		if trns {
			return buildGrayAlpha(rows, width, height, bitDepth, func(g uint16) bool { return g == gray }), nil
		}
		switch bitDepth {
		case 16:
			// image.Gray16.Pix is big-endian per pixel, exactly the scanline layout
			img := image.NewGray16(image.Rect(0, 0, width, height))
			stride16 := width * 2
			for y := 0; y < height; y++ {
				copy(img.Pix[y*stride16:(y+1)*stride16], rows[y])
			}
			return img, nil
		case 8:
			img := image.NewGray(image.Rect(0, 0, width, height))
			for y := 0; y < height; y++ {
				copy(img.Pix[y*width:(y+1)*width], rows[y])
			}
			return img, nil
		default: // 1, 2 or 4 bits per sample
			img := image.NewGray(image.Rect(0, 0, width, height))
			for y := 0; y < height; y++ {
				base := y * width
				for x := 0; x < width; x++ {
					img.Pix[base+x] = scaleTo8(sampleAt(rows[y], int(bitDepth), x), bitDepth)
				}
			}
			return img, nil
		}

	case 2:
		_, tr, tg, tb, trns := p.rawTransparent(2)
		if trns {
			img := image.NewNRGBA64(image.Rect(0, 0, width, height))
			for y := 0; y < height; y++ {
				for x := 0; x < width; x++ {
					r := sampleAt(rows[y], int(bitDepth), x*channels)
					g := sampleAt(rows[y], int(bitDepth), x*channels+1)
					b := sampleAt(rows[y], int(bitDepth), x*channels+2)
					a := uint16(65535)
					if scaleTo16(r, bitDepth) == tr && scaleTo16(g, bitDepth) == tg && scaleTo16(b, bitDepth) == tb {
						a = 0
					}
					img.SetNRGBA64(x, y, color.NRGBA64{
						R: scaleTo16(r, bitDepth),
						G: scaleTo16(g, bitDepth),
						B: scaleTo16(b, bitDepth),
						A: a,
					})
				}
			}
			return img, nil
		}
		if bitDepth == 16 {
			img := image.NewRGBA64(image.Rect(0, 0, width, height))
			pixStride := width * 8
			for y := 0; y < height; y++ {
				base := y * pixStride
				row := rows[y]
				for x := 0; x < width; x++ {
					o := base + x*8
					copy(img.Pix[o:o+6], row[x*6:(x+1)*6])
					img.Pix[o+6] = 0xFF
					img.Pix[o+7] = 0xFF
				}
			}
			return img, nil
		}
		// nolint: gocritic // the default bit depth is 8
		img := image.NewRGBA(image.Rect(0, 0, width, height))
		pixStride := width * 4
		for y := 0; y < height; y++ {
			base := y * pixStride
			row := rows[y]
			for x := 0; x < width; x++ {
				o := base + x*4
				copy(img.Pix[o:o+3], row[x*3:(x+1)*3])
				img.Pix[o+3] = 0xFF
			}
		}
		return img, nil

	case 3:
		palette, err := p.buildPalette()
		if err != nil {
			return nil, err
		}
		img := image.NewPaletted(image.Rect(0, 0, width, height), palette)
		if bitDepth == 8 {
			for y := 0; y < height; y++ {
				copy(img.Pix[y*width:(y+1)*width], rows[y])
			}
		} else { // 1, 2 or 4
			for y := 0; y < height; y++ {
				base := y * width
				for x := 0; x < width; x++ {
					img.Pix[base+x] = byte(sampleAt(rows[y], int(bitDepth), x))
				}
			}
		}
		if err = validatePaletteIndices(img.Pix, len(palette)); err != nil {
			return nil, err
		}
		return img, nil

	case 4:
		if bitDepth == 16 {
			img := image.NewNRGBA64(image.Rect(0, 0, width, height))
			pixStride := width * 8
			for y := 0; y < height; y++ {
				base := y * pixStride
				row := rows[y]
				for x := 0; x < width; x++ {
					o := base + x*8
					g := row[x*4 : x*4+2]
					copy(img.Pix[o:o+2], g)     // R
					copy(img.Pix[o+2:o+4], g)   // G
					copy(img.Pix[o+4:o+6], g)   // B
					copy(img.Pix[o+6:o+8], row[x*4+2:x*4+4]) // A
				}
			}
			return img, nil
		}
		img := image.NewNRGBA(image.Rect(0, 0, width, height))
		pixStride := width * 4
		for y := 0; y < height; y++ {
			base := y * pixStride
			row := rows[y]
			for x := 0; x < width; x++ {
				o := base + x*4
				g := row[x*2]
				img.Pix[o] = g
				img.Pix[o+1] = g
				img.Pix[o+2] = g
				img.Pix[o+3] = row[x*2+1]
			}
		}
		return img, nil

	case 6:
		if bitDepth == 16 {
			// image.NRGBA64.Pix is big-endian R,G,B,A per pixel, exactly the scanline layout
			img := image.NewNRGBA64(image.Rect(0, 0, width, height))
			pixStride := width * 8
			for y := 0; y < height; y++ {
				copy(img.Pix[y*pixStride:(y+1)*pixStride], rows[y])
			}
			return img, nil
		}
		img := image.NewNRGBA(image.Rect(0, 0, width, height))
		pixStride := width * 4
		for y := 0; y < height; y++ {
			copy(img.Pix[y*pixStride:(y+1)*pixStride], rows[y])
		}
		return img, nil
	}
	return nil, errors.Errorf("invalid color type %d", p.IHDR.ColorType)
}

// buildGrayAlpha builds a grayscale image where pixels matching the tRNS
// gray level are fully transparent.
func buildGrayAlpha(rows [][]byte, width, height int, bitDepth uint8, transparent func(uint16) bool) image.Image {
	if bitDepth == 16 {
		img := image.NewNRGBA64(image.Rect(0, 0, width, height))
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				g := sampleAt(rows[y], 16, x)
				a := uint16(65535)
				if transparent(g) {
					a = 0
				}
				img.SetNRGBA64(x, y, color.NRGBA64{R: g, G: g, B: g, A: a})
			}
		}
		return img
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			g := sampleAt(rows[y], int(bitDepth), x)
			a := uint8(255)
			if transparent(g) {
				a = 0
			}
			v := scaleTo8(g, bitDepth)
			img.SetNRGBA(x, y, color.NRGBA{R: v, G: v, B: v, A: a})
		}
	}
	return img
}
