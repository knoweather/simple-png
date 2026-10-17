package simple_png

import (
	"io"
	"sync"

	"github.com/pkg/errors"
)

var pngHeaderBytes = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
var pngHeader = string(pngHeaderBytes)

type Png struct {
	Width, Height int
	Channel       int
	sync.RWMutex
	sync.Once
	IHDR  *IHDR
	IDATs []*IDAT
	PLTE  *PLTE
	BKGD  *BKGD
	CHRM  *CHRM
	GAMA  *GAMA
	HIST  *HIST
	PHYS  *PHYS
	SBIT  *SBIT

	TEXTs []*TEXT
	TRNS  *TRNS
	TIME  *TIME
	ZTXTs []*ZTXT
	ITXTs []*ITXT

	IEND       *IEND
	OtherChunk map[ChunkName][]ChunkParse
	chunks     []*chunk

	// origChunks keeps every chunk of the file in its original order,
	// including the consumed and deleted ones, so that WriteTo can reproduce
	// the stream faithfully. The parallel slices record what happened to
	// each chunk.
	origChunks   []*chunk
	origConsumed []bool
	origDeleted  []bool
}

// ParsePng parses a png stream: it reads the signature and all chunks,
// decodes the base chunks (IHDR, IDAT, PLTE and the metadata chunks) and
// keeps every not-consumed chunk in raw form (see RawChunks).
// CRC-32 verification of each chunk is optional and off by default, pass
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
	// verify the stored CRC when requested, see PNG spec
	if verifyCRC {
		if err := c.checkCRC(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

	var chunkNotFoundErr = errors.New("chunk not found")

// markOrigConsumed marks a chunk as consumed in the original order snapshot.
func (p *Png) markOrigConsumed(c *chunk) {
	for i, oc := range p.origChunks {
		if oc == c {
			p.origConsumed[i] = true
			return
		}
	}
}

// ParseChunk parses the first chunk whose type code matches c.ChunkName().
// After a successful parse the chunk is consumed and removed from the raw
// chunk list. Unless notSave is true, the parser instance is recorded in
// OtherChunk and can be retrieved later with GetOtherChunkByName.
// If no matching chunk exists, chunkNotFoundErr is returned and nothing is
// modified.
func (p *Png) ParseChunk(c ChunkParse, notSave ...bool) error {
	p.Do(func() {
		p.OtherChunk = make(map[ChunkName][]ChunkParse)
	})
	for i, cur := range p.chunks {
		if cur == nil || ChunkName(cur.code[:]) != c.ChunkName() {
			continue
		}
		if err := c.Parse(cur); err != nil {
			return errors.WithStack(err)
		}
		// consume the parsed chunk
		p.chunks = append(p.chunks[:i], p.chunks[i+1:]...)
		p.markOrigConsumed(cur)
		if !(len(notSave) > 0 && notSave[0]) {
			p.Lock()
			p.OtherChunk[c.ChunkName()] = append(p.OtherChunk[c.ChunkName()], c)
			p.Unlock()
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
	for _, c := range p.chunks {
		if ChunkName(c.code[:]) == name {
			return c.data, true
		}
	}
	return nil, false
}

// DeleteRawChunk removes every remaining chunk of the given type from the
// stream and returns how many chunks were removed. A WriteTo afterwards
// writes the stream without those chunks.
func (p *Png) DeleteRawChunk(name ChunkName) int {
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
// NewITXTChunk to embed XMP metadata into an untouched file.
func (p *Png) AddRawChunk(name ChunkName, data []byte) error {
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
