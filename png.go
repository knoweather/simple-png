// Package simple_png 提供 PNG 文件的解析、像素解码、元数据编辑与再编码：
// ParsePng 把字节流解析为带类型化 chunk 结构的 Png，IDATsToColor 解码像素，
// WriteTo/MarshalBytes 把当前状态还原为字节流（未修改的 chunk 逐字节保留）。
// 所有多字节整数均按 PNG 规范以大端序（network byte order）存取。
//
// Package simple_png implements parsing, pixel decoding, metadata editing and
// re-encoding of PNG files: ParsePng turns a byte stream into a Png with typed
// chunk structs, IDATsToColor decodes the pixels, and WriteTo/MarshalBytes
// serialize the current state back to a byte stream, reproducing unmodified
// chunks byte for byte. All multi-byte integers are big endian (network byte
// order) as required by the PNG specification.
package simple_png

import (
	"io"
	"sync"

	"github.com/pkg/errors"
)

// PNG 签名固定为 8 字节：十进制 137 80 78 71 13 10 26 10（以 CR LF 结尾的
// 变体）；读写两端都以它标识流的开始，pngHeader 是其字符串形式
// The PNG signature is the fixed 8 bytes 137 80 78 71 13 10 26 10 (the CR LF
// variant); both the reader and the writer use it to identify the start of a
// stream, pngHeader is its string form
var pngHeaderBytes = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
var pngHeader = string(pngHeaderBytes)

// Png 是解析后的 PNG 文件模型：基础 chunk（IHDR、IDAT、PLTE 与各元数据
// chunk）保存在同名的类型化字段里，未被原生解析的 chunk 以原始形式留在内部
// 列表（见 RawChunks、RawChunkByName）。编辑方法（ParseChunk、AddRawChunk、
// DeleteRawChunk、ReplaceImage）与读取方法（IDATsToColor、WriteTo 等）通过
// 内嵌的 sync.RWMutex 串行化，可并发调用。
//
// Png is the parsed model of a PNG file: the base chunks (IHDR, IDAT, PLTE
// and the metadata chunks) live in the typed fields of the same name, chunks
// the library does not decode natively stay raw in an internal list (see
// RawChunks and RawChunkByName). The editing methods (ParseChunk, AddRawChunk,
// DeleteRawChunk, ReplaceImage) and the readers (IDATsToColor, WriteTo, ...)
// are serialized by the embedded sync.RWMutex and may be called concurrently.
type Png struct {
	// Width、Height 是 IHDR 的图像尺寸（像素）；Channel 是每像素采样数，
	// 由颜色类型推导：0/3→1、4→2、2→3、6→4
	// Width and Height are the IHDR image dimensions in pixels; Channel is
	// the samples per pixel derived from the color type: 0/3→1, 4→2,
	// 2→3, 6→4
	Width, Height int
	Channel       int
	sync.RWMutex
	sync.Once
	// IHDR 必有且唯一；IDATs 按文件顺序保存全部图像数据 chunk，其数据区的
	// 拼接就是唯一的 zlib 压缩数据流
	// IHDR is mandatory and unique; IDATs keeps every image data chunk in
	// file order, the concatenation of their data areas is the single zlib
	// datastream
	IHDR  *IHDR
	IDATs []*IDAT
	// PLTE 与各辅助 chunk 位于第一个 IDAT 之前：PLTE 仅对颜色类型 3 是
	// 必需的，其余字段仅在文件里存在对应 chunk 时才被填充
	// the palette and the ancillary chunks precede the first IDAT: only
	// PLTE is required, and only for color type 3; the other fields are
	// filled only when the file contains the respective chunk
	PLTE *PLTE
	BKGD *BKGD
	CHRM *CHRM
	GAMA *GAMA
	HIST *HIST
	PHYS *PHYS
	SBIT *SBIT

	// 文本类元数据：tEXt、zTXt、iTXt（XMP 存放在 iTXt 中）
	//
	// textual metadata: tEXt, zTXt and iTXt (XMP rides inside iTXt)
	TEXTs []*TEXT
	TRNS  *TRNS
	TIME  *TIME
	ZTXTs []*ZTXT
	ITXTs []*ITXT

	// 颜色配置与 EXIF 元数据
	//
	// color profiles and EXIF metadata
	ICCP *ICCP
	SRGB *SRGB
	EXIF *EXIF

	// IEND 标记数据流结束；OtherChunk 记录经 ParseChunk（notSave=false）
	// 保存的自定义解析器，可按类型码用 GetOtherChunkByName 取回；chunks
	// 是尚未消费、仍以原始形式存在的 chunk 列表
	//
	// IEND marks the end of the datastream; OtherChunk records the custom
	// parsers kept by ParseChunk (notSave=false), retrievable by type code
	// through GetOtherChunkByName; chunks is the list of the chunks that
	// are still raw (not consumed)
	IEND       *IEND
	OtherChunk map[ChunkName][]ChunkParse
	chunks     []*chunk

	// origChunks 以文件中的原始顺序保留全部 chunk（含已被消费或删除的），
	// 使 WriteTo 能忠实还原流；两个并行切片记录每个 chunk 的去向
	//
	// origChunks keeps every chunk of the file in its original order,
	// including the consumed and deleted ones, so that WriteTo can
	// reproduce the stream faithfully; the two parallel slices record what
	// happened to each chunk
	origChunks   []*chunk
	origConsumed []bool
	origDeleted  []bool
}

// ParsePng 解析 PNG 字节流：先校验 8 字节签名，再顺序读取各 chunk 直到
// IEND，解码基础 chunk（IHDR、IDAT、PLTE 与元数据 chunk），未消费的 chunk
// 以原始形式保留（见 RawChunks）。每个 chunk 的 CRC-32 校验默认关闭，传入
// verifyCRC=true 可拒绝 CRC 不匹配的流。
//
// ParsePng parses a PNG stream: it validates the 8-byte signature, reads
// chunks sequentially until IEND, decodes the base chunks (IHDR, IDAT, PLTE
// and the metadata chunks) and keeps every unconsumed chunk in raw form (see
// RawChunks). CRC-32 verification of each chunk is off by default; pass
// verifyCRC=true to reject streams with a mismatching chunk CRC.
func ParsePng(r io.Reader, verifyCRC ...bool) (*Png, error) {
	var p = &Png{}
	var hex = make([]byte, len(pngHeaderBytes))
	if _, err := io.ReadFull(r, hex); err != nil {
		return nil, errors.WithMessage(err, "invalid png: can not read signature")
	}
	if string(hex) != pngHeader {
		return nil, errors.New("invalid png: bad signature")
	}
	for {
		chunk, err := readChunk(r, len(verifyCRC) > 0 && verifyCRC[0])
		if err != nil {
			return nil, err
		}
		p.chunks = append(p.chunks, chunk)
		if ChunkName(chunk.code[:]) == IENDChunk {
			break
		}
	}
	// 在任何 chunk 被消费之前，先按原始顺序做快照
	//
	// snapshot the original chunk order before any chunk gets consumed
	p.origChunks = append([]*chunk(nil), p.chunks...)
	p.origConsumed = make([]bool, len(p.chunks))
	p.origDeleted = make([]bool, len(p.chunks))
	err := p.parseBaseChunk()
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return p, nil
}

// readChunk 读取一个完整 chunk：4 字节长度、4 字节类型码、数据区、4 字节
// CRC，全部按大端序解释；数据长度超过 2^31-1 视为非法（PNG 3.3 规定
// length 字段的最高位必须为 0）。verifyCRC=true 时按规范校验存储的 CRC。
//
// readChunk reads one full chunk: 4-byte length, 4-byte type code, the data
// area and the 4-byte CRC, all big endian; a data length above 2^31-1 is
// rejected (PNG 3.3 requires the top bit of the length field to be zero).
// With verifyCRC the stored CRC is verified as the specification mandates.
func readChunk(r io.Reader, verifyCRC bool) (*chunk, error) {
	var length = make([]byte, 4)
	var name = make([]byte, 4)
	var crc = make([]byte, 4)

	if _, err := io.ReadFull(r, length); err != nil {
		return nil, errors.WithMessage(err, "invalid png: can not read chunk length")
	}
	size := by.Uint32(length)
	if size > 0x7FFFFFFF {
		return nil, errors.Errorf("invalid chunk length %d", size)
	}
	if _, err := io.ReadFull(r, name); err != nil {
		return nil, errors.WithMessage(err, "invalid png: can not read chunk type")
	}
	content := make([]byte, size)
	if _, err := io.ReadFull(r, content); err != nil {
		return nil, errors.WithMessage(err, "invalid png: can not read chunk data")
	}
	if _, err := io.ReadFull(r, crc); err != nil {
		return nil, errors.WithMessage(err, "invalid png: can not read chunk crc")
	}
	c := &chunk{
		len:  [4]byte(length),
		code: [4]byte(name),
		data: content,
		crc:  [4]byte(crc),
	}
	// 按需校验存储的 CRC，参见 PNG 规范的 CRC 章节
	// verify the stored CRC when requested, see the PNG spec CRC chapter
	if verifyCRC {
		if err := c.checkCRC(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// chunkNotFoundErr 是 ParseChunk 找不到匹配 chunk 时返回的哨兵错误；
// parseOptionalChunk 用 errors.Is 区分“文件里没有该 chunk”与真正的解析失败
// chunkNotFoundErr is the sentinel ParseChunk returns when no chunk of the
// requested type exists; parseOptionalChunk uses errors.Is to tell "the file
// has no such chunk" from a real parse failure.
var chunkNotFoundErr = errors.New("chunk not found")

// markOrigConsumed 在原始顺序快照里把给定 chunk 标记为已消费；不在快照中
// （例如已被消费过）时静默返回
// markOrigConsumed flags a chunk as consumed in the original order snapshot;
// it returns silently when the chunk is not part of the snapshot.
func (p *Png) markOrigConsumed(c *chunk) {
	for i, oc := range p.origChunks {
		if oc == c {
			p.origConsumed[i] = true
			return
		}
	}
}

// ParseChunk 解析第一个类型码与 c.ChunkName() 匹配的 chunk：成功后该 chunk
// 被视为已消费并从原始 chunk 列表移除；除非 notSave 为 true，解析器实例会
// 记录到 OtherChunk，之后可用 GetOtherChunkByName 取回。文件中没有匹配的
// chunk 时返回 chunkNotFoundErr，且不修改任何状态。
//
// 整个操作在内部写锁下执行，同一实例上的 ParseChunk / AddRawChunk /
// DeleteRawChunk / WriteTo 并发调用会被串行化；传入的 Parse 回调不得再
// 回调同一个 Png 实例（RWMutex 不可重入）。
//
// ParseChunk parses the first chunk whose type code matches c.ChunkName().
// After a successful parse the chunk is consumed and removed from the raw
// chunk list. Unless notSave is true, the parser instance is recorded in
// OtherChunk and can be retrieved later with GetOtherChunkByName.
// If no matching chunk exists, chunkNotFoundErr is returned and nothing is
// modified.
//
// The whole operation runs under the internal write lock, so concurrent
// ParseChunk / AddRawChunk / DeleteRawChunk / WriteTo calls on the same
// instance are serialized. The Parse callback of the given chunk must not
// call back into the same Png instance (a RWMutex is not reentrant).
func (p *Png) ParseChunk(c ChunkParse, notSave ...bool) error {
	p.Lock()
	defer p.Unlock()
	if p.OtherChunk == nil {
		p.OtherChunk = make(map[ChunkName][]ChunkParse)
	}
	for i, cur := range p.chunks {
		if cur == nil || ChunkName(cur.code[:]) != c.ChunkName() {
			continue
		}
		if err := c.Parse(RawChunk{
			Name: ChunkName(cur.code[:]),
			Data: cur.data,
			CRC:  by.Uint32(cur.crc[:]),
		}); err != nil {
			return errors.WithStack(err)
		}
		// 消费该 chunk：从原始列表移除，并同步更新快照标记
		// consume the chunk: remove it from the raw list and update the snapshot
		p.chunks = append(p.chunks[:i], p.chunks[i+1:]...)
		p.markOrigConsumed(cur)
		if !(len(notSave) > 0 && notSave[0]) {
			p.OtherChunk[c.ChunkName()] = append(p.OtherChunk[c.ChunkName()], c)
		}
		return nil
	}
	return chunkNotFoundErr
}

// parseOptionalChunk parses an optional (ancillary) chunk.
// It returns false (with a nil error) when the chunk does not exist in the
// file, and propagates any real parse error.
func (p *Png) parseOptionalChunk(c ChunkParse) (bool, error) {
	err := p.ParseChunk(c, true)
	if errors.Is(err, chunkNotFoundErr) {
		return false, nil
	}
	return err == nil, err
}

func (p *Png) parseBaseChunk() error {
	var IHDR = &IHDR{}
	err := p.ParseChunk(IHDR, true)
	if err != nil {
		return errors.WithStack(err)
	}
	if err = IHDR.Validate(); err != nil {
		return errors.WithStack(err)
	}
	p.Width, p.Height = int(IHDR.Width), int(IHDR.Height)
	switch IHDR.ColorType {
	case 0, 3:
		p.Channel = 1
	case 4:
		p.Channel = 2
	case 2:
		p.Channel = 3
	case 6:
		p.Channel = 4
	}
	p.IHDR = IHDR

	var IDATs []*IDAT
	for {
		var idat = &IDAT{}
		err := p.ParseChunk(idat, true)
		if err != nil {
			if errors.Is(err, chunkNotFoundErr) {
				break
			} else {
				return errors.WithStack(err)
			}
		}
		IDATs = append(IDATs, idat)
	}
	if len(IDATs) == 0 {
		return errors.New("no IDAT found")
	}
	p.IDATs = IDATs

	var PLTE = &PLTE{}
	if found, err := p.parseOptionalChunk(PLTE); err != nil {
		return errors.WithStack(err)
	} else if found {
		p.PLTE = PLTE
	}

	var BKGD = &BKGD{}
	if found, err := p.parseOptionalChunk(BKGD); err != nil {
		return errors.WithStack(err)
	} else if found {
		p.BKGD = BKGD
	}

	var CHRM = &CHRM{}
	if found, err := p.parseOptionalChunk(CHRM); err != nil {
		return errors.WithStack(err)
	} else if found {
		p.CHRM = CHRM
	}

	var GAMA = &GAMA{}
	if found, err := p.parseOptionalChunk(GAMA); err != nil {
		return errors.WithStack(err)
	} else if found {
		p.GAMA = GAMA
	}

	var HIST = &HIST{}
	if found, err := p.parseOptionalChunk(HIST); err != nil {
		return errors.WithStack(err)
	} else if found {
		p.HIST = HIST
	}

	var PHYS = &PHYS{}
	if found, err := p.parseOptionalChunk(PHYS); err != nil {
		return errors.WithStack(err)
	} else if found {
		p.PHYS = PHYS
	}

	var SBIT = &SBIT{}
	if found, err := p.parseOptionalChunk(SBIT); err != nil {
		return errors.WithStack(err)
	} else if found {
		p.SBIT = SBIT
	}

	var TRNS = &TRNS{}
	if found, err := p.parseOptionalChunk(TRNS); err != nil {
		return errors.WithStack(err)
	} else if found {
		p.TRNS = TRNS
	}

	var TIME = &TIME{}
	if found, err := p.parseOptionalChunk(TIME); err != nil {
		return errors.WithStack(err)
	} else if found {
		p.TIME = TIME
	}

	var ICCP = &ICCP{}
	if found, err := p.parseOptionalChunk(ICCP); err != nil {
		return errors.WithStack(err)
	} else if found {
		p.ICCP = ICCP
	}
	var SRGB = &SRGB{}
	if found, err := p.parseOptionalChunk(SRGB); err != nil {
		return errors.WithStack(err)
	} else if found {
		p.SRGB = SRGB
	}
	var EXIF = &EXIF{}
	if found, err := p.parseOptionalChunk(EXIF); err != nil {
		return errors.WithStack(err)
	} else if found {
		p.EXIF = EXIF
	}

	var TEXTs []*TEXT
	for {
		var text = &TEXT{}
		err := p.ParseChunk(text, true)
		if err != nil {
			if errors.Is(err, chunkNotFoundErr) {
				break
			} else {
				return errors.WithStack(err)
			}
		}
		TEXTs = append(TEXTs, text)
	}
	p.TEXTs = TEXTs

	var ZTXTs []*ZTXT
	for {
		var text = &ZTXT{}
		err := p.ParseChunk(text, true)
		if err != nil {
			if errors.Is(err, chunkNotFoundErr) {
				break
			} else {
				return errors.WithStack(err)
			}
		}
		ZTXTs = append(ZTXTs, text)
	}
	p.ZTXTs = ZTXTs

	var ITXTs []*ITXT
	for {
		var text = &ITXT{}
		err := p.ParseChunk(text, true)
		if err != nil {
			if errors.Is(err, chunkNotFoundErr) {
				break
			} else {
				return errors.WithStack(err)
			}
		}
		ITXTs = append(ITXTs, text)
	}
	p.ITXTs = ITXTs

	var IEND = &IEND{}
	err = p.ParseChunk(IEND, true)
	if err != nil {
		return errors.WithStack(err)
	}
	p.IEND = IEND
	return nil
}

func (p *Png) GetOtherChunkByName(name ChunkName) ([]ChunkParse, error) {
	p.RLock()
	defer p.RUnlock()
	if list, ok := p.OtherChunk[name]; ok {
		return list, nil
	}
	return nil, chunkNotFoundErr
}

// RawChunk is a chunk that was left in the file and is not decoded natively
// yet, exposed in its raw form. It can also be parsed with a custom chunk
// type through ParseChunk.
type RawChunk struct {
	// Name is the 4 byte chunk type code, e.g. "sRGB" or "eXIf".
	Name ChunkName
	// Data is the raw chunk data (without the length, type and crc fields).
	Data []byte
	// CRC is the CRC stored in the file (verified during reading only when
	// verifyCRC was passed to ParsePng).
	CRC uint32
}

// RawChunks returns all chunks that are still left after base parsing, in the
// order they appear in the file. Use this for chunk types the library does
// not decode natively yet, like eXIf, sRGB or iCCP.
func (p *Png) RawChunks() []RawChunk {
	p.RLock()
	defer p.RUnlock()
	out := make([]RawChunk, 0, len(p.chunks))
	for _, c := range p.chunks {
		out = append(out, RawChunk{
			Name: ChunkName(c.code[:]),
			Data: c.data,
			CRC:  by.Uint32(c.crc[:]),
		})
	}
	return out
}

// RawChunkByName returns the raw data of the first still-present chunk of the
// given type, for use cases like reading XMP (iTXt, parsed natively) or EXIF
// (eXIf) metadata that has no dedicated struct yet.
func (p *Png) RawChunkByName(name ChunkName) ([]byte, bool) {
	p.RLock()
	defer p.RUnlock()
	for _, c := range p.chunks {
		if ChunkName(c.code[:]) == name {
			return c.data, true
		}
	}
	return nil, false
}

// DeleteRawChunk removes every remaining chunk of the given type from the
// stream and returns how many chunks were removed. A WriteTo afterwards
// writes the stream without those chunks. It is safe to call concurrently
// with the other editing methods and the readers of the same instance.
func (p *Png) DeleteRawChunk(name ChunkName) int {
	p.Lock()
	defer p.Unlock()
	var rest []*chunk
	for _, c := range p.chunks {
		if ChunkName(c.code[:]) == name {
			continue
		}
		rest = append(rest, c)
	}
	removed := len(p.chunks) - len(rest)
	p.chunks = rest
	for i, c := range p.origChunks {
		if !p.origConsumed[i] && ChunkName(c.code[:]) == name {
			p.origDeleted[i] = true
		}
	}
	return removed
}

// AddRawChunk inserts a new raw chunk into the stream before the first IDAT
// chunk (ancillary chunks must precede the first IDAT; if the file has no
// IDAT it goes right before IEND). You cannot add the single-occurrence
// IHDR, IDAT or IEND chunks with it.
// The CRC is computed over the given data and the chunk appears as a raw
// chunk (see RawChunks) and in WriteTo output. Use it e.g. with
// NewITXTChunk to embed XMP metadata into an untouched file. It is safe to
// call concurrently with the other editing methods and the readers of the
// same instance.
func (p *Png) AddRawChunk(name ChunkName, data []byte) error {
	p.Lock()
	defer p.Unlock()
	if len(name) != 4 {
		return errors.Errorf("invalid chunk name %q", string(name))
	}
	switch ChunkName(name) {
	case IHDRChunk, IDATChunk, IENDChunk:
		return errors.Errorf("cannot add a %s chunk with AddRawChunk", string(name))
	}
	if len(data) > 0x7FFFFFFF {
		return errors.New("chunk data too large")
	}
	if p.origChunks == nil {
		return errors.New("png is not parsed")
	}
	c := &chunk{data: data}
	copy(c.code[:], string(name))
	by.PutUint32(c.len[:], uint32(len(data)))
	buf := make([]byte, 0, 4+len(data))
	buf = append(buf, c.code[:]...)
	buf = append(buf, data...)
	by.PutUint32(c.crc[:], ISO_3309_CRC32(buf))
	// insert before the first IDAT, or before IEND when there is no IDAT
	insertAt := len(p.origChunks) - 1 // just before IEND
	for i, oc := range p.origChunks {
		if ChunkName(oc.code[:]) == IDATChunk {
			insertAt = i
			break
		}
	}
	n := len(p.origChunks) + 1
	var chunks = make([]*chunk, 0, n)
	var consumed = make([]bool, 0, n)
	var deleted = make([]bool, 0, n)
	for i := range p.origChunks {
		if i == insertAt {
			chunks = append(chunks, c)
			consumed = append(consumed, false)
			deleted = append(deleted, false)
		}
		chunks = append(chunks, p.origChunks[i])
		consumed = append(consumed, p.origConsumed[i])
		deleted = append(deleted, p.origDeleted[i])
	}
	p.origChunks, p.origConsumed, p.origDeleted = chunks, consumed, deleted
	p.rebuildChunks()
	return nil
}

// rebuildChunks re-derives the remaining raw chunk list from the original
// order snapshot.
func (p *Png) rebuildChunks() {
	var rest []*chunk
	for i, c := range p.origChunks {
		if p.origConsumed[i] || p.origDeleted[i] {
			continue
		}
		rest = append(rest, c)
	}
	p.chunks = rest
}
