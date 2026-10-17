package simple_png

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"io"
	"time"

	"github.com/pkg/errors"
)

// png format  https://www.w3.org/TR/PNG-Chunks.html
var by binary.ByteOrder = binary.BigEndian

type ChunkName string

const (
	IHDRChunk ChunkName = "IHDR"
	PLTEChunk ChunkName = "PLTE"
	IDATChunk ChunkName = "IDAT"
	IENDChunk ChunkName = "IEND"

	BKGDChunk ChunkName = "bKGD"
	CHRMChunk ChunkName = "cHRM"
	GAMAChunk ChunkName = "gAMA"
	HISTChunk ChunkName = "hIST"
	SBITChunk ChunkName = "sBIT"
	TRNSChunk ChunkName = "tRNS"
	PHYSChunk ChunkName = "pHYs"
	TEXTChunk ChunkName = "tEXt"
	ZTXTChunk ChunkName = "zTXt"
	ITXTChunk ChunkName = "iTXt"
	TIMEChunk ChunkName = "tIME"
)

// ISO_3309_CRC32 computes the 32-bit CRC defined in ISO 3309 (ITU-T V.42) and
// required by the PNG specification for every chunk. It is the same polynomial
// as IEEE 802.3 / zlib's crc32: x32+x26+x23+x22+x16+x12+x11+x10+x8+x7+x5+x4+x2+x+1
func ISO_3309_CRC32(b []byte) uint32 {
	return crc32.ChecksumIEEE(b)
}

// zlibCompress compresses data as a zlib stream with the default compression
// level, as required by compression method 0.
func zlibCompress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// zlibDecompress decompresses a zlib stream.
func zlibDecompress(data []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	if err = zr.Close(); err != nil {
		return nil, err
	}
	return plain, nil
}

// NewITXTChunk builds the data area of a new iTXt chunk, e.g. for embedding
// XMP metadata (keyword "XML:com.adobe.xmp") into an already parsed file
// through AddRawChunk. Its result is meant to be used as the args of
// AddRawChunk:
//
//	name, data, err := NewITXTChunk("XML:com.adobe.xmp", "", "", xmp, true)
//	p.AddRawChunk(name, data)
//
// The keyword must be 1-79 bytes long and none of the values may contain a
// null byte.
func NewITXTChunk(keyword, languageTag, translatedKeyword, text string, compress bool) (ChunkName, []byte, error) {
	if separator := bytes.IndexByte([]byte(keyword), 0x00); len(keyword) < 1 || len(keyword) > 79 || separator >= 0 {
		return "", nil, errors.New("invalid itxt keyword: 1-79 bytes without null")
	}
	for _, s := range []string{languageTag, translatedKeyword, text} {
		if bytes.IndexByte([]byte(s), 0x00) >= 0 {
			return "", nil, errors.New("invalid itxt chunk value: contains null byte")
		}
	}
	var flag byte
	var textData = []byte(text)
	if compress {
		flag = 1
		var err error
		if textData, err = zlibCompress(textData); err != nil {
			return "", nil, errors.WithStack(err)
		}
	}
	out := make([]byte, 0, len(keyword)+len(languageTag)+len(translatedKeyword)+len(textData)+7)
	out = append(out, keyword...)
	out = append(out, 0x00, flag, 0x00, 0x00)
	out = append(out, languageTag...)
	out = append(out, 0x00)
	out = append(out, translatedKeyword...)
	out = append(out, 0x00)
	out = append(out, textData...)
	return ITXTChunk, out, nil
}

type ChunkParse interface {
	ChunkName() ChunkName
	Parse(chunk *chunk) error
}

type chunk struct {
	len  [4]byte
	code [4]byte
	data []byte
	crc  [4]byte
}

// checkCRC verifies the stored CRC (over the chunk type code and data) as
// required by the PNG specification.
func (c *chunk) checkCRC() error {
	buf := make([]byte, 0, len(c.code)+len(c.data))
	buf = append(buf, c.code[:]...)
	buf = append(buf, c.data...)
	want := by.Uint32(c.crc[:])
	if got := ISO_3309_CRC32(buf); got != want {
		return errors.Errorf("chunk %q crc mismatch: stored %08x, computed %08x", string(c.code[:]), want, got)
	}
	return nil
}

/*

--------------------------------------------------------------------------------------

*/

// IHDR
// The IHDR chunk must appear FIRST. It contains:
//
//	Width:              4 bytes
//	Height:             4 bytes
//	Bit depth:          1 byte
//	Color type:         1 byte
//	Compression method: 1 byte
//	Filter method:      1 byte
//	Interlace method:   1 byte
//
// Width and height give the image dimensions in pixels. They are 4-byte integers. Zero is an invalid value. The maximum for each is (2^31)-1 in order to accommodate languages that have difficulty with unsigned 4-byte values.
// Bit depth is a single-byte integer giving the number of bits per sample or per palette index (not per pixel). Valid values are 1, 2, 4, 8, and 16, although not all values are allowed for all color types.
//
// Color type is a single-byte integer that describes the interpretation of the image data. Color type codes represent sums of the following values: 1 (palette used), 2 (color used), and 4 (alpha channel used). Valid values are 0, 2, 3, 4, and 6.
//
// Bit depth restrictions for each color type are imposed to simplify implementations and to prohibit combinations that do not compress well. Decoders must support all legal combinations of bit depth and color type. The allowed combinations are:
//
//	Color    Allowed    Interpretation
//	Type    Bit Depths
//
//	0       1,2,4,8,16  Each pixel is a grayscale sample.
//
//	2       8,16        Each pixel is an R,G,B triple.
//
//	3       1,2,4,8     Each pixel is a palette index;
//	                    a PLTE chunk must appear.
//
//	4       8,16        Each pixel is a grayscale sample,
//	                    followed by an alpha sample.
//
//	6       8,16        Each pixel is an R,G,B triple,
//	                    followed by an alpha sample.
//
// The sample depth is the same as the bit depth except in the case of color type 3, in which the sample depth is always 8 bits.
// Compression method is a single-byte integer that indicates the method used to compress the image data. At present, only compression method 0 (deflate/inflate compression with a 32K sliding window) is defined. All standard PNG images must be compressed with this scheme. The compression method field is provided for possible future expansion or proprietary variants. Decoders must check this byte and report an error if it holds an unrecognized code. See Deflate/Inflate Compression for details.
//
// Filter method is a single-byte integer that indicates the preprocessing method applied to the image data before compression. At present, only filter method 0 (adaptive filtering with five basic filter types) is defined. As with the compression method field, decoders must check this byte and report an error if it holds an unrecognized code. See Filter Algorithms for details.
//
// Interlace method is a single-byte integer that indicates the transmission order of the image data. Two values are currently defined: 0 (no interlace) or 1 (Adam7 interlace). See Interlaced data order for details.
type IHDR struct {
	Width             uint32
	Height            uint32
	BitDepth          uint8
	ColorType         uint8
	CompressionMethod uint8
	FilterMethod      uint8
	InterlaceMethod   uint8
}

func (c *IHDR) Parse(chunk *chunk) error {
	if chunk.data == nil || len(chunk.data) < 13 {
		return errors.New("invalid chunk data")
	}
	c.Width = by.Uint32(chunk.data[:4])
	c.Height = by.Uint32(chunk.data[4:8])
	c.BitDepth = chunk.data[8]
	c.ColorType = chunk.data[9]
	c.CompressionMethod = chunk.data[10]
	c.FilterMethod = chunk.data[11]
	c.InterlaceMethod = chunk.data[12]
	return nil
}

func (c *IHDR) ChunkName() ChunkName {
	return IHDRChunk
}

// Validate checks the IHDR fields against the allowed combinations defined
// by the PNG specification.
func (c *IHDR) Validate() error {
	if c.Width == 0 || c.Height == 0 {
		return errors.New("invalid image size")
	}
	if c.CompressionMethod != 0 {
		return errors.Errorf("unsupported compression method %d", c.CompressionMethod)
	}
	if c.FilterMethod != 0 {
		return errors.Errorf("unsupported filter method %d", c.FilterMethod)
	}
	if c.InterlaceMethod > 1 {
		return errors.Errorf("invalid interlace method %d", c.InterlaceMethod)
	}
	var depths []uint8
	switch c.ColorType {
	case 0:
		depths = []uint8{1, 2, 4, 8, 16}
	case 2, 6:
		depths = []uint8{8, 16}
	case 3:
		depths = []uint8{1, 2, 4, 8}
	case 4:
		depths = []uint8{8, 16}
	default:
		return errors.Errorf("invalid color type %d", c.ColorType)
	}
	for _, d := range depths {
		if c.BitDepth == d {
			return nil
		}
	}
	return errors.Errorf("invalid bit depth %d for color type %d", c.BitDepth, c.ColorType)
}

/*

--------------------------------------------------------------------------------------

*/

// IEND
// The IEND chunk must appear LAST. It marks the end of the PNG datastream. The chunk's data field is empty.
type IEND struct {
}

func (i *IEND) ChunkName() ChunkName {
	return IENDChunk
}

func (i *IEND) Parse(chunk *chunk) error {
	if len(chunk.data) != 0 {
		return errors.New("invalid iend chunk data")
	}
	return nil
}

/*

--------------------------------------------------------------------------------------

*/

// PLTE
//
//	The PLTE chunk contains from 1 to 256 palette entries, each a three-byte series of the form:
//	 Red:   1 byte (0 = black, 255 = red)
//	 Green: 1 byte (0 = black, 255 = green)
//	 Blue:  1 byte (0 = black, 255 = blue)
//
// The number of entries is determined from the chunk length. A chunk length not divisible by 3 is an error.
// This chunk must appear for color type 3, and can appear for color types 2 and 6; it must not appear for color types 0 and 4. If this chunk does appear, it must precede the first IDAT chunk. There must not be more than one PLTE chunk.
//
// For color type 3 (indexed color), the PLTE chunk is required. The first entry in PLTE is referenced by pixel value 0, the second by pixel value 1, etc. The number of palette entries must not exceed the range that can be represented in the image bit depth (for example, 2^4 = 16 for a bit depth of 4). It is permissible to have fewer entries than the bit depth would allow. In that case, any out-of-range pixel value found in the image data is an error.
//
// For color types 2 and 6 (truecolor and truecolor with alpha), the PLTE chunk is optional. If present, it provides a suggested set of from 1 to 256 colors to which the truecolor image can be quantized if the viewer cannot display truecolor directly. If PLTE is not present, such a viewer will need to select colors on its own, but it is often preferable for this to be done once by the encoder. (See Recommendations for Encoders: Suggested palettes.)
//
// Note that the palette uses 8 bits (1 byte) per sample regardless of the image bit depth specification. In particular, the palette is 8 bits deep even when it is a suggested quantization of a 16-bit truecolor image.
//
// There is no requirement that the palette entries all be used by the image, nor that they all be different.
type PLTE struct {
	Colors []*PLTEColor
}

type PLTEColor struct {
	Red   uint8
	Green uint8
	Blue  uint8
}

func (p *PLTE) ChunkName() ChunkName {
	return PLTEChunk
}

func (p *PLTE) Parse(chunk *chunk) error {
	if chunk.data == nil || len(chunk.data)%3 != 0 || len(chunk.data) < 3 {
		return errors.New("invalid plte chunk data")
	}
	p.Colors = make([]*PLTEColor, 0, len(chunk.data)/3)
	for i := 0; i+2 < len(chunk.data); i += 3 {
		p.Colors = append(p.Colors, &PLTEColor{
			Red:   chunk.data[i],
			Green: chunk.data[i+1],
			Blue:  chunk.data[i+2],
		})
	}

	return nil
}

/*

--------------------------------------------------------------------------------------

*/

// IDAT
// The IDAT chunk contains the actual image data. To create this data:
// Begin with image scanlines represented as described in Image layout; the layout and total size of this raw data are determined by the fields of IHDR.
// Filter the image data according to the filtering method specified by the IHDR chunk. (Note that with filter method 0, the only one currently defined, this implies prepending a filter type byte to each scanline.)
// Compress the filtered data using the compression method specified by the IHDR chunk.
// The IDAT chunk contains the output datastream of the compression algorithm.
// To read the image data, reverse this process.
//
// There can be multiple IDAT chunks; if so, they must appear consecutively with no other intervening chunks. The compressed datastream is then the concatenation of the contents of all the IDAT chunks. The encoder can divide the compressed datastream into IDAT chunks however it wishes. (Multiple IDAT chunks are allowed so that encoders can work in a fixed amount of memory; typically the chunk size will correspond to the encoder's buffer size.) It is important to emphasize that IDAT chunk boundaries have no semantic significance and can occur at any point in the compressed datastream. A PNG file in which each IDAT chunk contains only one data byte is legal, though remarkably wasteful of space. (For that matter, zero-length IDAT chunks are legal, though even more wasteful.)
//
// See Filter Algorithms and Deflate/Inflate Compression for details.
type IDAT struct {
	Length        uint32
	ChunkTypeCode string
	Data          []byte
}

func (i *IDAT) ChunkName() ChunkName {
	return IDATChunk
}

func (i *IDAT) Parse(chunk *chunk) error {
	i.Length = by.Uint32(chunk.len[:])
	i.ChunkTypeCode = string(chunk.code[:])
	i.Data = chunk.data[:]
	return nil
}

/*

--------------------------------------------------------------------------------------

*/

// BKGD
// The bKGD chunk specifies a default background color to present the image against. Note that viewers are not bound to honor this chunk; a viewer can choose to use a different background.
// For color type 3 (indexed color), the bKGD chunk contains:
//
//	Palette index:  1 byte
//
// The value is the palette index of the color to be used as background.
// For color types 0 and 4 (grayscale, with or without alpha), bKGD contains:
//
//	Gray:  2 bytes, range 0 .. (2^bitdepth)-1
//
// (For consistency, 2 bytes are used regardless of the image bit depth.) The value is the gray level to be used as background.
// For color types 2 and 6 (truecolor, with or without alpha), bKGD contains:
//
//	Red:   2 bytes, range 0 .. (2^bitdepth)-1
//	Green: 2 bytes, range 0 .. (2^bitdepth)-1
//	Blue:  2 bytes, range 0 .. (2^bitdepth)-1
//
// (For consistency, 2 bytes per sample are used regardless of the image bit depth.) This is the RGB color to be used as background.
// When present, the bKGD chunk must precede the first IDAT chunk, and must follow the PLTE chunk, if any.
//
// See Recommendations for Decoders: Background color.
type BKGD struct {
	// For color type 3 (indexed color): the palette index of the background color.
	Palette uint8
	// For color types 0 and 4 (grayscale): the gray level of the background color.
	Gray uint16
	// For color types 2 and 6 (truecolor): the rgb color of the background.
	Red   uint16
	Green uint16
	Blue  uint16
}

func (b *BKGD) ChunkName() ChunkName {
	return BKGDChunk
}

func (b *BKGD) Parse(chunk *chunk) error {
	switch len(chunk.data) {
	case 1: // color type 3: palette index
		b.Palette = chunk.data[0]
	case 2: // color types 0 and 4: gray
		b.Gray = by.Uint16(chunk.data[:2])
	case 6: // color types 2 and 6: rgb
		b.Red = by.Uint16(chunk.data[0:2])
		b.Green = by.Uint16(chunk.data[2:4])
		b.Blue = by.Uint16(chunk.data[4:6])
	default:
		return errors.New("invalid bkgd chunk data")
	}
	return nil
}

/*

--------------------------------------------------------------------------------------

*/

// CHRM
// Applications that need device-independent specification of colors in a PNG file can use the cHRM chunk to specify the 1931 CIE x,y chromaticities of the red, green, and blue primaries used in the image, and the referenced white point. See Color Tutorial for more information.
// The cHRM chunk contains:
//
//	White Point x: 4 bytes
//	White Point y: 4 bytes
//	Red x:         4 bytes
//	Red y:         4 bytes
//	Green x:       4 bytes
//	Green y:       4 bytes
//	Blue x:        4 bytes
//	Blue y:        4 bytes
//
// Each value is encoded as a 4-byte unsigned integer, representing the x or y value times 100000. For example, a value of 0.3127 would be stored as the integer 31270.
// cHRM is allowed in all PNG files, although it is of little value for grayscale images.
//
// If the encoder does not know the chromaticity values, it should not write a cHRM chunk; the absence of a cHRM chunk indicates that the image's primary colors are device-dependent.
//
// If the cHRM chunk appears, it must precede the first IDAT chunk, and it must also precede the PLTE chunk if present.
//
// See Recommendations for Encoders: Encoder color handling, and Recommendations for Decoders: Decoder color handling.
type CHRM struct {
	WhiteX uint32
	WhiteY uint32
	RedX   uint32
	RedY   uint32
	GreenX uint32
	GreenY uint32
	BlueX  uint32
	BlueY  uint32
}

func (c *CHRM) ChunkName() ChunkName {
	return CHRMChunk
}

func (c *CHRM) Parse(chunk *chunk) error {
	if chunk.data == nil || len(chunk.data) < 32 {
		return errors.New("invalid chrm chunk data")
	}
	c.WhiteX = by.Uint32(chunk.data[:4])
	c.WhiteY = by.Uint32(chunk.data[4:8])
	c.RedX = by.Uint32(chunk.data[8:12])
	c.RedY = by.Uint32(chunk.data[12:16])
	c.GreenX = by.Uint32(chunk.data[16:20])
	c.GreenY = by.Uint32(chunk.data[20:24])
	c.BlueX = by.Uint32(chunk.data[24:28])
	c.BlueY = by.Uint32(chunk.data[28:32])
	return nil
}

/*

--------------------------------------------------------------------------------------

*/

// GAMA
// The gAMA chunk specifies the gamma of the camera (or simulated camera) that produced the image, and thus the gamma of the image with respect to the original scene. More precisely, the gAMA chunk encodes the file_gamma value, as defined in Gamma Tutorial.
// The gAMA chunk contains:
//
//	Image gamma: 4 bytes
//
// The value is encoded as a 4-byte unsigned integer, representing gamma times 100000. For example, a gamma of 0.45 would be stored as the integer 45000.
// If the encoder does not know the image's gamma value, it should not write a gAMA chunk; the absence of a gAMA chunk indicates that the gamma is unknown.
//
// If the gAMA chunk appears, it must precede the first IDAT chunk, and it must also precede the PLTE chunk if present.
//
// See Gamma correction, Recommendations for Encoders: Encoder gamma handling, and Recommendations for Decoders: Decoder gamma handling.
type GAMA struct {
	ImageGamma uint32
}

func (g *GAMA) ChunkName() ChunkName {
	return GAMAChunk
}

func (g *GAMA) Parse(chunk *chunk) error {
	if chunk.data == nil || len(chunk.data) < 4 {
		return errors.New("invalid gama chunk data")
	}
	g.ImageGamma = by.Uint32(chunk.data[:4])
	return nil
}

/*

--------------------------------------------------------------------------------------

*/

// HIST
// The hIST chunk gives the approximate usage frequency of each color in the color palette. A histogram chunk can appear only when a palette chunk appears. If a viewer is unable to provide all the colors listed in the palette, the histogram may help it decide how to choose a subset of the colors for display.
// The hIST chunk contains a series of 2-byte (16 bit) unsigned integers. There must be exactly one entry for each entry in the PLTE chunk. Each entry is proportional to the fraction of pixels in the image that have that palette index; the exact scale factor is chosen by the encoder.
//
// Histogram entries are approximate, with the exception that a zero entry specifies that the corresponding palette entry is not used at all in the image. It is required that a histogram entry be nonzero if there are any pixels of that color.
//
// When the palette is a suggested quantization of a truecolor image, the histogram is necessarily approximate, since a decoder may map pixels to palette entries differently than the encoder did. In this situation, zero entries should not appear.
//
// The hIST chunk, if it appears, must follow the PLTE chunk, and must precede the first IDAT chunk.
//
// See Rationale: Palette histograms, and Recommendations for Decoders: Suggested-palette and histogram usage.
type HIST struct {
	Elements []uint16
}

func (h *HIST) ChunkName() ChunkName {
	return HISTChunk
}

func (h *HIST) Parse(chunk *chunk) error {
	if len(chunk.data)%2 != 0 {
		return errors.New("invalid hist chunk data")
	}
	n := len(chunk.data) / 2
	h.Elements = make([]uint16, 0, n)
	for i := 0; i < n; i++ {
		h.Elements = append(h.Elements, by.Uint16(chunk.data[i*2:i*2+2]))
	}
	return nil
}

/*

--------------------------------------------------------------------------------------

*/

// PHYS
// The pHYs chunk specifies the intended pixel size or aspect ratio for display of the image. It contains:
//
//	Pixels per unit, X axis: 4 bytes (unsigned integer)
//	Pixels per unit, Y axis: 4 bytes (unsigned integer)
//	Unit specifier:          1 byte
//
// The following values are legal for the unit specifier:
//
//	0: unit is unknown
//	1: unit is the meter
//
// When the unit specifier is 0, the pHYs chunk defines pixel aspect ratio only; the actual size of the pixels remains unspecified.
// Conversion note: one inch is equal to exactly 0.0254 meters.
//
// If this ancillary chunk is not present, pixels are assumed to be square, and the physical size of each pixel is unknown.
//
// If present, this chunk must precede the first IDAT chunk.
//
// See Recommendations for Decoders: Pixel dimensions.
type PHYS struct {
	X             uint32
	Y             uint32
	UnitSpecifier uint8
}

func (p *PHYS) ChunkName() ChunkName {
	return PHYSChunk
}

func (p *PHYS) Parse(chunk *chunk) error {
	if chunk.data == nil || len(chunk.data) < 9 {
		return errors.New("invalid phys chunk data")
	}
	p.X = by.Uint32(chunk.data[:4])
	p.Y = by.Uint32(chunk.data[4:8])
	p.UnitSpecifier = chunk.data[8]
	return nil
}

/*

--------------------------------------------------------------------------------------

*/

// SBIT
//
// To simplify decoders, PNG specifies that only certain sample depths can be used, and further specifies that sample values should be scaled to the full range of possible values at the sample depth. However, the sBIT chunk is provided in order to store the original number of significant bits. This allows decoders to recover the original data losslessly even if the data had a sample depth not directly supported by PNG. We recommend that an encoder emit an sBIT chunk if it has converted the data from a lower sample depth.
// For color type 0 (grayscale), the sBIT chunk contains a single byte, indicating the number of bits that were significant in the source data.
//
// For color type 2 (truecolor), the sBIT chunk contains three bytes, indicating the number of bits that were significant in the source data for the red, green, and blue channels, respectively.
//
// For color type 3 (indexed color), the sBIT chunk contains three bytes, indicating the number of bits that were significant in the source data for the red, green, and blue components of the palette entries, respectively.
//
// For color type 4 (grayscale with alpha channel), the sBIT chunk contains two bytes, indicating the number of bits that were significant in the source grayscale data and the source alpha data, respectively.
//
// For color type 6 (truecolor with alpha channel), the sBIT chunk contains four bytes, indicating the number of bits that were significant in the source data for the red, green, blue and alpha channels, respectively.
//
// Each depth specified in sBIT must be greater than zero and less than or equal to the sample depth (which is 8 for indexed-color images, and the bit depth given in IHDR for other color types).
//
// A decoder need not pay attention to sBIT: the stored image is a valid PNG file of the sample depth indicated by IHDR. However, if the decoder wishes to recover the original data at its original precision, this can be done by right-shifting the stored samples (the stored palette entries, for an indexed-color image). The encoder must scale the data in such a way that the high-order bits match the original data.
//
// If the sBIT chunk appears, it must precede the first IDAT chunk, and it must also precede the PLTE chunk if present.
//
// See Recommendations for Encoders: Sample depth scaling and Recommendations for Decoders: Sample depth rescaling.
type SBIT struct {
	OrgData [4]byte
}

func (s *SBIT) ChunkName() ChunkName {
	return SBITChunk
}

func (s *SBIT) Parse(chunk *chunk) error {
	if chunk.data == nil || len(chunk.data) < 1 {
		return errors.New("invalid sbit chunk data")
	}
	for i := 0; i < len(s.OrgData); i++ {
		if len(chunk.data) <= i {
			s.OrgData[i] = 0
		} else {
			s.OrgData[i] = chunk.data[i]
		}
	}
	return nil
}

/*

--------------------------------------------------------------------------------------

*/

// TEXT
//
// Textual information that the encoder wishes to record with the image can be stored in tEXt chunks. Each tEXt chunk contains a keyword and a text string, in the format:
//
//	Keyword:        1-79 bytes (character string)
//	Null separator: 1 byte
//	Text:           n bytes (character string)
//
// The keyword and text string are separated by a zero byte (null character). Neither the keyword nor the text string can contain a null character. Note that the text string is not null-terminated (the length of the chunk is sufficient information to locate the ending). The keyword must be at least one character and less than 80 characters long. The text string can be of any length from zero bytes up to the maximum permissible chunk size less the length of the keyword and separator.
type TEXT struct {
	Keyword   string
	Separator string
	Text      string
}

func (t *TEXT) ChunkName() ChunkName {
	return TEXTChunk
}

const nullSep = string(byte(0x00))

func (t *TEXT) Parse(chunk *chunk) error {
	sep := bytes.IndexByte(chunk.data, 0x00)
	if sep < 1 || sep > 79 {
		return errors.New("invalid text")
	}
	t.Keyword = string(chunk.data[:sep])
	t.Separator = nullSep
	t.Text = string(chunk.data[sep+1:])

	return nil
}

/*

--------------------------------------------------------------------------------------

*/

// TIME
//
// The tIME chunk gives the time of the last image modification (not the time of initial image creation). It contains:
//
//	Year:   2 bytes (complete; for example, 1995, not 95)
//	Month:  1 byte (1-12)
//	Day:    1 byte (1-31)
//	Hour:   1 byte (0-23)
//	Minute: 1 byte (0-59)
//	Second: 1 byte (0-60)    (yes, 60, for leap seconds; not 61,
//	                          a common error)
//
// Universal Time (UTC, also called GMT) should be specified rather than local time.
// The tIME chunk is intended for use as an automatically-applied time stamp that is updated whenever the image data is changed. It is recommended that tIME not be changed by PNG editors that do not change the image data. See also the Creation Time tEXt keyword, which can be used for a user-supplied time.
type TIME struct {
	Year   uint16
	Month  uint8
	Day    uint8
	Hour   uint8
	Minute uint8
	Second uint8
}

func (t *TIME) ChunkName() ChunkName {
	return TIMEChunk
}

func (t *TIME) Parse(chunk *chunk) error {
	if len(chunk.data) < 7 {
		return errors.New("invalid time chunk data")
	}
	t.Year = by.Uint16(chunk.data[:2])
	t.Month = chunk.data[2]
	t.Day = chunk.data[3]
	t.Hour = chunk.data[4]
	t.Minute = chunk.data[5]
	t.Second = chunk.data[6]
	return nil
}

func (t *TIME) ToTime() time.Time {
	return time.Date(int(t.Year), time.Month(t.Month), int(t.Day), int(t.Hour), int(t.Minute), int(t.Second), 0, time.UTC)
}

// ITXT
//
// The iTXt chunk contains textual data just like tEXt and zTXt, but the text
// is UTF-8 encoded and both the translated keyword and the text itself can be
// compressed. XMP metadata in png files is stored as an iTXt chunk with the
// keyword "XML:com.adobe.xmp". The chunk layout is:
//
//	Keyword:                  1-79 bytes (character string)
//	Null separator:           1 byte
//	Compression flag:         1 byte (0: uncompressed, 1: compressed)
//	Compression method:       1 byte (0: zlib)
//	Null separator:           1 byte
//	Language tag (LCID):      n bytes of ASCII, implicit "x-default" if empty
//	Null separator:           1 byte
//	Translated keyword:       n bytes of UTF-8
//	Null separator:           1 byte
//	Text:                     n bytes of UTF-8 (optionally zlib compressed)
type ITXT struct {
	Keyword           string
	CompressionFlag   uint8
	CompressionMethod uint8
	LanguageTag       string
	TranslatedKeyword string
	Text              string
	// Data keeps the raw chunk payload.
	Data []byte
}

func (i *ITXT) ChunkName() ChunkName {
	return ITXTChunk
}

func (i *ITXT) Parse(chunk *chunk) error {
	sep := bytes.IndexByte(chunk.data, 0x00)
	if sep < 1 || sep > 79 {
		return errors.New("invalid itxt chunk data")
	}
	if len(chunk.data) < sep+3 {
		return errors.New("invalid itxt chunk data")
	}
	i.Keyword = string(chunk.data[:sep])
	i.Data = chunk.data[:]
	rest := chunk.data[sep+1:]
	if len(rest) < 3 {
		return errors.New("invalid itxt chunk data")
	}
	i.CompressionFlag = rest[0]
	i.CompressionMethod = rest[1]
	rest = rest[2:]
	if i.CompressionFlag > 1 {
		return errors.Errorf("invalid itxt compression flag %d", i.CompressionFlag)
	}
	if i.CompressionFlag == 1 && i.CompressionMethod != 0 {
		return errors.Errorf("unsupported itxt compression method %d", i.CompressionMethod)
	}
	// a null separator follows the compression method, then language tag,
	// translated keyword and text are null delimited; the text itself
	// extends to the end of the chunk
	if rest[0] != 0x00 {
		return errors.New("invalid itxt chunk data")
	}
	parts := bytes.SplitN(rest[1:], []byte{0x00}, 3)
	if len(parts) != 3 {
		return errors.New("invalid itxt chunk data")
	}
	i.LanguageTag = string(parts[0])
	i.TranslatedKeyword = string(parts[1])
	text := parts[2]
	if i.CompressionFlag == 1 {
		var err error
		if text, err = zlibDecompress(text); err != nil {
			return errors.WithStack(err)
		}
	}
	i.Text = string(text)
	return nil
}

/*

--------------------------------------------------------------------------------------

*/

// TRNS
// The tRNS chunk specifies that the image uses simple transparency: either alpha values associated with palette entries (for indexed-color images) or a single transparent color (for grayscale and truecolor images). Although simple transparency is not as elegant as the full alpha channel, it requires less storage space and is sufficient for many common cases.
// For color type 3 (indexed color), the tRNS chunk contains a series of one-byte alpha values, corresponding to entries in the PLTE chunk:
//
//	Alpha for palette index 0:  1 byte
//	Alpha for palette index 1:  1 byte
//	... etc ...
//
// Each entry indicates that pixels of the corresponding palette index must be treated as having the specified alpha value. Alpha values have the same interpretation as in an 8-bit full alpha channel: 0 is fully transparent, 255 is fully opaque, regardless of image bit depth. The tRNS chunk must not contain more alpha values than there are palette entries, but tRNS can contain fewer values than there are palette entries. In this case, the alpha value for all remaining palette entries is assumed to be 255. In the common case in which only palette index 0 need be made transparent, only a one-byte tRNS chunk is needed.
// For color type 0 (grayscale), the tRNS chunk contains a single gray level value, stored in the format:
//
//	Gray:  2 bytes, range 0 .. (2^bitdepth)-1
//
// (For consistency, 2 bytes are used regardless of the image bit depth.) Pixels of the specified gray level are to be treated as transparent (equivalent to alpha value 0); all other pixels are to be treated as fully opaque (alpha value (2^bitdepth)-1).
// For color type 2 (truecolor), the tRNS chunk contains a single RGB color value, stored in the format:
//
//	Red:   2 bytes, range 0 .. (2^bitdepth)-1
//	Green: 2 bytes, range 0 .. (2^bitdepth)-1
//	Blue:  2 bytes, range 0 .. (2^bitdepth)-1
//
// (For consistency, 2 bytes per sample are used regardless of the image bit depth.) Pixels of the specified color value are to be treated as transparent (equivalent to alpha value 0); all other pixels are to be treated as fully opaque (alpha value (2^bitdepth)-1).
// tRNS is prohibited for color types 4 and 6, since a full alpha channel is already present in those cases.
//
// Note: when dealing with 16-bit grayscale or truecolor data, it is important to compare both bytes of the sample values to determine whether a pixel is transparent. Although decoders may drop the low-order byte of the samples for display, this must not occur until after the data has been tested for transparency. For example, if the grayscale level 0x0001 is specified to be transparent, it would be incorrect to compare only the high-order byte and decide that 0x0002 is also transparent.
//
// When present, the tRNS chunk must precede the first IDAT chunk, and must follow the PLTE chunk, if any.
type TRNS struct {
	// For color type 3 (indexed color): alpha values for the first len(PaletteAlphas) palette entries.
	PaletteAlphas []uint8
	// For color types 0 and 4 (grayscale): the gray level to be treated as transparent.
	Gray uint16
	// For color type 2 (truecolor): the rgb color to be treated as transparent.
	Red   uint16
	Green uint16
	Blue  uint16
	// Data keeps the raw chunk payload, see TRNS.Data usage in IDATsToColor.
	Data []byte
}

func (T *TRNS) ChunkName() ChunkName {
	return TRNSChunk
}

// Parse fills the fields of TRNS based on the length of the chunk data:
// two bytes are a gray level and six bytes are an rgb color, any other
// length is a list of palette alpha values. Note that a tRNS chunk for a
// palette image with exactly two (or six) entries is indistinguishable
// from a gray level (or rgb color) without knowing the color type from
// IHDR; IDATsToColor re-interprets TRNS.Data with the actual color type
// when decoding.
func (T *TRNS) Parse(chunk *chunk) error {
	if chunk.data == nil || len(chunk.data) == 0 || len(chunk.data) > 256 {
		return errors.New("invalid trns chunk data")
	}
	T.Data = chunk.data[:]
	switch len(chunk.data) {
	case 2: // color types 0 and 4: gray level
		T.Gray = by.Uint16(chunk.data[:2])
	case 6: // color type 2: rgb color
		T.Red = by.Uint16(chunk.data[0:2])
		T.Green = by.Uint16(chunk.data[2:4])
		T.Blue = by.Uint16(chunk.data[4:6])
	default: // color type 3: palette alpha values
		T.PaletteAlphas = chunk.data[:]
	}
	return nil
}

/*

--------------------------------------------------------------------------------------

*/

// ZTXT
// The zTXt chunk contains textual data, just as tEXt does; however, zTXt takes advantage of compression. zTXt and tEXt chunks are semantically equivalent, but zTXt is recommended for storing large blocks of text.
// A zTXt chunk contains:
//
//	Keyword:            1-79 bytes (character string)
//	Null separator:     1 byte
//	Compression method: 1 byte
//	Compressed text:    n bytes
//
// The keyword and null separator are exactly the same as in the tEXt chunk. Note that the keyword is not compressed. The compression method byte identifies the compression method used in this zTXt chunk. The only value presently defined for it is 0 (deflate/inflate compression). The compression method byte is followed by a compressed datastream that makes up the remainder of the chunk. For compression method 0, this datastream adheres to the zlib datastream format (see Deflate/Inflate Compression). Decompression of this datastream yields Latin-1 text that is identical to the text that would be stored in an equivalent tEXt chunk.
// Any number of zTXt and tEXt chunks can appear in the same file. See the preceding definition of the tEXt chunk for the predefined keywords and the recommended format of the text.
//
// See Recommendations for Encoders: Text chunk processing, and Recommendations for Decoders: Text chunk processing.
type ZTXT struct {
	Keyword           string
	Separator         string
	CompressionMethod uint8
	Text              string
}

func (z *ZTXT) ChunkName() ChunkName {
	return ZTXTChunk
}

func (z *ZTXT) Parse(chunk *chunk) error {
	sep := bytes.IndexByte(chunk.data, 0x00)
	if sep < 1 || sep > 79 {
		return errors.New("invalid text")
	}
	// keyword + null + compression method + compressed text
	if len(chunk.data) < sep+1+1 {
		return errors.New("invalid ztxt chunk data")
	}
	z.Keyword = string(chunk.data[:sep])
	z.Separator = nullSep
	rest := chunk.data[sep+1:]
	z.CompressionMethod = rest[0]
	if z.CompressionMethod != 0 {
		return errors.Errorf("unsupported ztxt compression method %d", z.CompressionMethod)
	}
	text, err := zlibDecompress(rest[1:])
	if err != nil {
		return errors.WithStack(err)
	}
	z.Text = string(text)
	return nil
}
