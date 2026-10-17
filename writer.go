package simple_png

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"

	"github.com/pkg/errors"
)

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

// writeChunk writes one full chunk: length, type code, data, crc (recomputed).
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
func (p *Png) WriteTo(w io.Writer) (int64, error) {
	if p.IHDR == nil {
		return 0, errors.New("png is not parsed")
	}
	cw := &chunkWriter{w: w}
	if err := cw.write(pngHeaderBytes); err != nil {
		return cw.n, err
	}
	// occurrence cursors map the k-th consumed chunk of a repeating chunk
	// type to the k-th parsed struct of that type
	var idatIdx, textIdx, ztxtIdx, itxtIdx int
	for i, c := range p.origChunks {
		if p.origDeleted[i] {
			continue
		}
		name := ChunkName(c.code[:])
		if !p.origConsumed[i] {
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
		case 3: // palette index
			return []byte{p.BKGD.Palette}, nil
		case 0, 4: // gray
			var b [2]byte
			by.PutUint16(b[:], p.BKGD.Gray)
			return b[:], nil
		default: // rgb
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
		// byte for byte identical to the parsed chunk when untouched
		return rebuildITXT(x, text), nil

	default:
		// a custom chunk type parsed by the user has no re-serializer here,
		// keep its original raw data
		return c.data, nil
	}
}

// rebuildITXT serializes an ITXT struct back into the iTXt data area.
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

// MarshalBytes returns the png stream of the current state as a byte slice,
// see WriteTo for the details.
func (p *Png) MarshalBytes() ([]byte, error) {
	var buf bytes.Buffer
	if _, err := p.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
