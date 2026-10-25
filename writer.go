package simple_png

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"

	"github.com/pkg/errors"
)

// chunkWriter 把 PNG 流写入底层 writer，并累计已写出的字节数。
//
// chunkWriter writes a png stream and counts the written bytes.
type chunkWriter struct {
	w   io.Writer
	n   int64
	buf [20]byte
}

func (cw *chunkWriter) write(b []byte) error {
	n, err := cw.w.Write(b)
	cw.n += int64(n)
	return err
}

// writeChunk 写出一个完整 chunk：4 字节长度、4 字节类型码、数据区、4 字节
// CRC。CRC 按 PNG 规范对「类型码 + 数据区」用 IEEE CRC-32 重新计算；长度与
// CRC 均为大端序，数据区不得超过 0x7FFFFFFF 字节。
//
// writeChunk writes one full chunk: 4-byte length, 4-byte type code, the
// data area, and a 4-byte CRC. The CRC is recomputed with the IEEE CRC-32
// over the type code plus the data area as required by the PNG spec; length
// and CRC are big-endian, and the data area must not exceed 0x7FFFFFFF
// bytes.
func (cw *chunkWriter) writeChunk(name ChunkName, data []byte) error {
	if len(name) != 4 {
		return errors.Errorf("invalid chunk name %q", string(name))
	}
	if len(data) > 0x7FFFFFFF {
		return errors.Errorf("chunk %s data too large: %d bytes", string(name), len(data))
	}
	binary.BigEndian.PutUint32(cw.buf[:4], uint32(len(data)))
	copy(cw.buf[4:], name)
	if err := cw.write(cw.buf[:8]); err != nil {
		return err
	}
	if err := cw.write(data); err != nil {
		return err
	}
	h := crc32.NewIEEE()
	_, _ = h.Write([]byte(name))
	_, _ = h.Write(data)
	binary.BigEndian.PutUint32(cw.buf[:4], h.Sum32())
	return cw.write(cw.buf[:4])
}

// WriteTo 把当前状态序列化为 PNG 流写入 w，实现 io.WriterTo（因此可直接配合
// io.Copy 使用）。解析时被消费进类型化字段的 chunk 会从解析后的结构体重新序列化，
// 对结构体的修改（以及通过 AddRawChunk / DeleteRawChunk 增删的 chunk）都会体现
// 在输出中；仍以原样保留的 chunk 则逐字节复制。对未经修改的文件做「解析 → 写回」
// 可逐字节复现原文件。每个写出的 chunk 的 CRC 都会重新计算。
//
// 注意：压缩文本 chunk（zTXt，以及压缩标志置位的 iTXt）在 Text 字段未被修改时
// 逐字节保留；一旦修改，则用默认 zlib 压缩级别重新压缩。
//
// 读取由内部读锁保护：其他 goroutine 可以同时通过 RawChunks /
// GetOtherChunkByName 读取元数据；同一实例上的并发写入则与编辑方法互相串行。
//
// WriteTo writes the png stream of the current state, implementing
// io.WriterTo (so it works with io.Copy). Chunks that were consumed while
// parsing are re-serialized from their parsed structs, so any modifications
// made to the structs (or chunks added/deleted through AddRawChunk /
// DeleteRawChunk) are reflected, while the chunks that were left raw are
// copied verbatim. Parsing and writing back a file without touching anything
// reproduces it byte for byte. The CRC of every written chunk is recomputed.
//
// Note: the compressed text chunks (zTXt and iTXt with the compression flag
// set) are preserved byte for byte unless their Text field was modified,
// in which case they are re-compressed with the default zlib level.
//
// Reading is guarded by the internal read lock: it is safe to write back
// while other goroutines read metadata through RawChunks /
// GetOtherChunkByName, while concurrent writes on the same instance are
// serialized with the editing methods.
func (p *Png) WriteTo(w io.Writer) (int64, error) {
	if p.IHDR == nil {
		return 0, errors.New("png is not parsed")
	}
	p.RLock()
	defer p.RUnlock()
	cw := &chunkWriter{w: w}
	if err := cw.write(pngHeaderBytes); err != nil {
		return cw.n, err
	}
	// 出现次数游标：把某类可重复 chunk 的第 k 次出现对应到它的第 k 个解析结构体
	// occurrence cursors map the k-th consumed chunk of a repeating chunk
	// type to the k-th parsed struct of that type
	var idatIdx, textIdx, ztxtIdx, itxtIdx int
	for i, c := range p.origChunks {
		if p.origDeleted[i] {
			continue
		}
		name := ChunkName(c.code[:])
		if !p.origConsumed[i] {
			// 未被消费的 chunk：原样复制
			// unconsumed chunk: copy it verbatim
			if err := cw.writeChunk(name, c.data); err != nil {
				return cw.n, err
			}
			continue
		}
		data, err := p.emitConsumed(name, c, &idatIdx, &textIdx, &ztxtIdx, &itxtIdx)
		if err != nil {
			return cw.n, err
		}
		if err = cw.writeChunk(name, data); err != nil {
			return cw.n, err
		}
	}
	return cw.n, nil
}

// emitConsumed 从解析后的结构体重建被消费 chunk 的数据区。出现次数游标耗尽
// （正常解析后不应发生）或该 chunk 类型没有专门的结构体（由用户自定义解析的
// 自定义 chunk）时，退化为直接返回原始数据。
//
// emitConsumed builds the data area of a consumed chunk from the parsed
// struct. An exhausted occurrence cursor (must not happen after a normal
// parse) or a chunk type without a dedicated struct (a custom chunk parsed
// by the user) falls back to the raw data.
func (p *Png) emitConsumed(name ChunkName, c *chunk, idatIdx, textIdx, ztxtIdx, itxtIdx *int) ([]byte, error) {
	switch name {
	case IHDRChunk:
		var b [13]byte
		binary.BigEndian.PutUint32(b[:4], p.IHDR.Width)
		binary.BigEndian.PutUint32(b[4:8], p.IHDR.Height)
		b[8], b[9], b[10], b[11], b[12] =
			p.IHDR.BitDepth, p.IHDR.ColorType, p.IHDR.CompressionMethod, p.IHDR.FilterMethod, p.IHDR.InterlaceMethod
		return b[:], nil

	case IDATChunk:
		if *idatIdx >= len(p.IDATs) {
			return c.data, nil
		}
		d := p.IDATs[*idatIdx].Data
		*idatIdx++
		return d, nil

	case PLTEChunk:
		if p.PLTE == nil {
			return c.data, nil
		}
		b := make([]byte, 0, len(p.PLTE.Colors)*3)
		for _, col := range p.PLTE.Colors {
			b = append(b, col.Red, col.Green, col.Blue)
		}
		return b, nil

	case BKGDChunk:
		if p.BKGD == nil {
			return c.data, nil
		}
		switch p.IHDR.ColorType {
		case 3: // 调色板索引 / palette index
			return []byte{p.BKGD.Palette}, nil
		case 0, 4: // 灰度 / grayscale
			var b [2]byte
			by.PutUint16(b[:], p.BKGD.Gray)
			return b[:], nil
		default: // 真彩色 / truecolor (RGB)
			var b [6]byte
			by.PutUint16(b[0:2], p.BKGD.Red)
			by.PutUint16(b[2:4], p.BKGD.Green)
			by.PutUint16(b[4:6], p.BKGD.Blue)
			return b[:], nil
		}

	case CHRMChunk:
		if p.CHRM == nil {
			return c.data, nil
		}
		var b [32]byte
		by.PutUint32(b[0:4], p.CHRM.WhiteX)
		by.PutUint32(b[4:8], p.CHRM.WhiteY)
		by.PutUint32(b[8:12], p.CHRM.RedX)
		by.PutUint32(b[12:16], p.CHRM.RedY)
		by.PutUint32(b[16:20], p.CHRM.GreenX)
		by.PutUint32(b[20:24], p.CHRM.GreenY)
		by.PutUint32(b[24:28], p.CHRM.BlueX)
		by.PutUint32(b[28:32], p.CHRM.BlueY)
		return b[:], nil

	case GAMAChunk:
		if p.GAMA == nil {
			return c.data, nil
		}
		var b [4]byte
		by.PutUint32(b[:], p.GAMA.ImageGamma)
		return b[:], nil

	case HISTChunk:
		if p.HIST == nil {
			return c.data, nil
		}
		b := make([]byte, 0, len(p.HIST.Elements)*2)
		for _, e := range p.HIST.Elements {
			var v [2]byte
			by.PutUint16(v[:], e)
			b = append(b, v[:]...)
		}
		return b, nil

	case PHYSChunk:
		if p.PHYS == nil {
			return c.data, nil
		}
		var b [9]byte
		by.PutUint32(b[:4], p.PHYS.X)
		by.PutUint32(b[4:8], p.PHYS.Y)
		b[8] = p.PHYS.UnitSpecifier
		return b[:], nil

	case SBITChunk:
		if p.SBIT == nil {
			return c.data, nil
		}
		// sBIT 的数据区长度取决于颜色类型
		// the length of sBIT depends on the color type
		var n int
		switch p.IHDR.ColorType {
		case 0:
			n = 1
		case 2, 3:
			n = 3
		case 4:
			n = 2
		default:
			n = 4
		}
		return p.SBIT.OrgData[:n], nil

	case TRNSChunk:
		if p.TRNS == nil {
			return c.data, nil
		}
		return p.TRNS.Data, nil

	case SRGBChunk:
		if p.SRGB == nil {
			return c.data, nil
		}
		return []byte{p.SRGB.RenderingIntent}, nil

	case ICCPChunk:
		if p.ICCP == nil {
			return c.data, nil
		}
		// c.data 是「profile 名称 + 0x00 + 压缩方法 + 压缩后的 profile」；当名称、
		// 压缩方法与（解压后的）profile 均未修改时，逐字节保留原数据
		// c.data is name + null + method + compressed profile; keep it
		// byte for byte when the name, the method and the (decompressed)
		// profile were not modified
		sep := bytes.IndexByte(c.data, 0x00)
		if sep >= 0 {
			if sep+2 <= len(c.data) {
				if string(c.data[:sep]) == p.ICCP.Name && c.data[sep+1] == p.ICCP.CompressionMethod {
					orig, err := zlibDecompress(c.data[sep+2:])
					if err == nil && bytes.Equal(orig, p.ICCP.Profile) {
						return c.data, nil
					}
				}
			}
			if p.ICCP.CompressionMethod != 0 {
				return nil, errors.Errorf("unsupported iccp compression method %d", p.ICCP.CompressionMethod)
			}
			compressed, err := zlibCompress(p.ICCP.Profile)
			if err != nil {
				return nil, errors.WithStack(err)
			}
			out := make([]byte, 0, sep+2+len(compressed))
			out = append(out, p.ICCP.Name...)
			out = append(out, 0x00, p.ICCP.CompressionMethod)
			out = append(out, compressed...)
			return out, nil
		}
		return c.data, nil

	case EXIFChunk:
		if p.EXIF == nil {
			return c.data, nil
		}
		return p.EXIF.Data, nil

	case TIMEChunk:
		if p.TIME == nil {
			return c.data, nil
		}
		var b [7]byte
		by.PutUint16(b[:2], p.TIME.Year)
		b[2], b[3], b[4], b[5], b[6] = p.TIME.Month, p.TIME.Day, p.TIME.Hour, p.TIME.Minute, p.TIME.Second
		return b[:], nil

	case TEXTChunk:
		if *textIdx >= len(p.TEXTs) {
			return c.data, nil
		}
		t := p.TEXTs[*textIdx]
		*textIdx++
		out := make([]byte, 0, len(t.Keyword)+len(t.Text)+1)
		out = append(out, t.Keyword...)
		out = append(out, 0x00)
		out = append(out, t.Text...)
		return out, nil

	case ZTXTChunk:
		if *ztxtIdx >= len(p.ZTXTs) {
			return c.data, nil
		}
		z := p.ZTXTs[*ztxtIdx]
		*ztxtIdx++
		// c.data 是「关键词 + 0x00 + 压缩方法 + 压缩后的文本」；文本未被修改时
		// 逐字节保留原数据
		// c.data is keyword + null + method + compressed text; preserve it
		// byte for byte when the text was not modified
		sep := bytes.IndexByte(c.data, 0x00)
		if sep >= 0 && sep+2 <= len(c.data) {
			plain, err := zlibDecompress(c.data[sep+2:])
			if err == nil && string(plain) == z.Text {
				return c.data, nil
			}
		}
		compressed, err := zlibCompress([]byte(z.Text))
		if err != nil {
			return nil, errors.WithStack(err)
		}
		var method byte
		if sep >= 0 && sep+1 < len(c.data) {
			method = c.data[sep+1]
		}
		out := make([]byte, 0, len(z.Keyword)+len(compressed)+2)
		out = append(out, z.Keyword...)
		out = append(out, 0x00, method)
		out = append(out, compressed...)
		return out, nil

	case ITXTChunk:
		if *itxtIdx >= len(p.ITXTs) {
			return c.data, nil
		}
		x := p.ITXTs[*itxtIdx]
		*itxtIdx++
		text := []byte(x.Text)
		if x.CompressionFlag == 1 {
			text, err := zlibCompress(text)
			if err != nil {
				return nil, errors.WithStack(err)
			}
			return rebuildITXT(x, text), nil
		}
		// 未压缩的 iTXt：结构体未被修改时与原 chunk 逐字节一致
		// uncompressed iTXt: identical to the original chunk byte for byte
		// when the struct is untouched
		return rebuildITXT(x, text), nil

	default:
		// 由用户自定义解析的 chunk 类型没有对应的重新序列化逻辑，保留其原始数据
		// a custom chunk type parsed by the user has no re-serializer here,
		// keep its original raw data
		return c.data, nil
	}
}

// rebuildITXT 把 ITXT 结构体序列化回 iTXt 数据区：关键词、0x00、压缩标志、
// 压缩方法、0x00、语言标签、0x00、翻译关键词、0x00、文本。
//
// rebuildITXT serializes an ITXT struct back into the iTXt data area:
// keyword, 0x00, compression flag, compression method, 0x00, language tag,
// 0x00, translated keyword, 0x00, text.
func rebuildITXT(x *ITXT, text []byte) []byte {
	out := make([]byte, 0, len(x.Keyword)+len(x.LanguageTag)+len(x.TranslatedKeyword)+len(text)+7)
	out = append(out, x.Keyword...)
	out = append(out, 0x00, x.CompressionFlag, x.CompressionMethod)
	out = append(out, 0x00)
	out = append(out, x.LanguageTag...)
	out = append(out, 0x00)
	out = append(out, x.TranslatedKeyword...)
	out = append(out, 0x00)
	out = append(out, text...)
	return out
}

// MarshalBytes 以字节切片的形式返回当前状态的 PNG 流，细节见 WriteTo。
//
// MarshalBytes returns the png stream of the current state as a byte slice,
// see WriteTo for the details.
func (p *Png) MarshalBytes() ([]byte, error) {
	var buf bytes.Buffer
	if _, err := p.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
