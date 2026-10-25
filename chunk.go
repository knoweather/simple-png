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

// by 是解析 PNG 多字节字段的字节序：PNG 规范规定 chunk 内的整数一律大端存储。
// 见 https://www.w3.org/TR/PNG-Chunks.html
//
// by is the byte order used for every multi-byte PNG field: the PNG
// specification stores all chunk integers big-endian.
// See https://www.w3.org/TR/PNG-Chunks.html
var by binary.ByteOrder = binary.BigEndian

// ChunkName 是 chunk 4 字节类型码的字符串别名。类型码首字母的大小写是标志位：
// 大写表示关键 chunk（解码器必须理解），小写表示辅助 chunk（可安全跳过）。
//
// ChunkName is a string alias of the 4-byte chunk type code. The case of
// the first letter is a flag: uppercase marks a critical chunk (a decoder
// must understand it), lowercase marks an ancillary chunk (it may be
// skipped safely).
type ChunkName string

// 本库原生支持的 chunk 类型码：4 个关键 chunk 与全部辅助 chunk。
//
// The chunk type codes supported natively by this library: the four
// critical chunks and every ancillary chunk it parses.
const (
	IHDRChunk ChunkName = "IHDR" // 图像头 / image header
	PLTEChunk ChunkName = "PLTE" // 调色板 / palette
	IDATChunk ChunkName = "IDAT" // 压缩图像数据 / compressed image data
	IENDChunk ChunkName = "IEND" // 数据流结束 / end of datastream

	BKGDChunk ChunkName = "bKGD" // 建议背景色 / suggested background color
	CHRMChunk ChunkName = "cHRM" // 基色与白点色度 / chromaticities and white point
	GAMAChunk ChunkName = "gAMA" // 图像 gamma / image gamma
	HISTChunk ChunkName = "hIST" // 调色板使用频率 / palette usage histogram
	SBITChunk ChunkName = "sBIT" // 源数据有效位数 / significant bits of source data
	TRNSChunk ChunkName = "tRNS" // 简单透明 / simple transparency
	PHYSChunk ChunkName = "pHYs" // 像素物理尺寸 / physical pixel dimensions
	TEXTChunk ChunkName = "tEXt" // 未压缩文本 / uncompressed text
	ZTXTChunk ChunkName = "zTXt" // zlib 压缩文本 / zlib-compressed text
	ITXTChunk ChunkName = "iTXt" // UTF-8 文本 / UTF-8 encoded text
	TIMEChunk ChunkName = "tIME" // 最后修改时间 / time of last modification

	SRGBChunk ChunkName = "sRGB" // sRGB 渲染意图 / sRGB rendering intent
	ICCPChunk ChunkName = "iCCP" // 内嵌 ICC 配置文件 / embedded ICC profile
	EXIFChunk ChunkName = "eXIf" // EXIF 元数据 / EXIF metadata
)

// ISO_3309_CRC32 计算 PNG 规范为每个 chunk 规定的 32 位 CRC：即 ISO 3309
// （ITU-T V.42）定义的 CRC，多项式与 IEEE 802.3 / zlib 的 crc32 相同：
// x32+x26+x23+x22+x16+x12+x11+x10+x8+x7+x5+x4+x2+x+1。
//
// ISO_3309_CRC32 computes the 32-bit CRC the PNG specification requires
// for every chunk: the CRC defined by ISO 3309 (ITU-T V.42), which uses
// the same polynomial as IEEE 802.3 / zlib's crc32:
// x32+x26+x23+x22+x16+x12+x11+x10+x8+x7+x5+x4+x2+x+1.
func ISO_3309_CRC32(b []byte) uint32 {
	return crc32.ChecksumIEEE(b)
}

// zlibCompress 以默认压缩级别生成 zlib 数据流；PNG 规范目前只定义了压缩
// 方法 0（deflate，32 KiB 滑动窗口），iTXt/zTXt/iCCP 的压缩部分都用它。
//
// zlibCompress compresses data into a zlib stream at the default
// compression level, as required by PNG compression method 0 (deflate
// with a 32 KiB sliding window), which the compressed parts of iTXt,
// zTXt and iCCP chunks rely on.
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

// zlibDecompress 解开一个 zlib 数据流，用于 iTXt、zTXt 与 iCCP chunk 中
// 压缩过的正文或配置文件。
//
// zlibDecompress inflates a zlib stream; it is used for the compressed
// text of iTXt and zTXt chunks and for the compressed profile of iCCP.
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

// NewITXTChunk 构造一个新 iTXt chunk 的数据区，例如用 AddRawChunk 把 XMP
// 元数据（关键字 "XML:com.adobe.xmp"）嵌入已解析的文件：
//
//	name, data, err := NewITXTChunk("XML:com.adobe.xmp", "", "", xmp, true)
//	p.AddRawChunk(name, data)
//
// 关键字必须为 1-79 字节且不含空字节，语言标签、翻译关键字与正文也都不得
// 含空字节；compress=true 时正文按压缩标志 1（zlib）压缩。返回的
// (name, data) 正是 AddRawChunk 需要的两个参数。
//
// NewITXTChunk builds the data area of a new iTXt chunk, e.g. for
// embedding XMP metadata (keyword "XML:com.adobe.xmp") into an already
// parsed file through AddRawChunk:
//
//	name, data, err := NewITXTChunk("XML:com.adobe.xmp", "", "", xmp, true)
//	p.AddRawChunk(name, data)
//
// The keyword must be 1-79 bytes long and none of the values may contain
// a null byte; with compress=true the text is compressed (compression
// flag 1). The returned (name, data) pair is exactly what AddRawChunk
// expects.
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

// ChunkParse 是本库原生解析的每一种 chunk 类型的接口，调用方也可以为自定义
// chunk 类型实现它：ChunkName 返回待匹配的 4 字节类型码，Parse 接收原始
// chunk（name + data + crc）并填充接收者的字段。
//
// ChunkParse is the interface of every chunk type the library parses
// natively and of any custom chunk type a caller provides: implement
// ChunkName (the 4-byte type code to look up) and Parse (which receives
// the raw chunk as name + data + crc and fills the receiver's fields).
type ChunkParse interface {
	ChunkName() ChunkName
	Parse(chunk RawChunk) error
}

// chunk 是磁盘上一个 chunk 的四段式布局：4 字节长度、4 字节类型码、数据区、
// 4 字节 CRC。类型码与 CRC 按值持有，数据区以切片引用底层字节。
//
// chunk is the on-disk layout of a PNG chunk: 4-byte length, 4-byte type
// code, data area and 4-byte CRC. The code and the CRC are held by value
// while data aliases the underlying bytes.
type chunk struct {
	len  [4]byte
	code [4]byte
	data []byte
	crc  [4]byte
}

// checkCRC 按 PNG 规范校验 CRC：校验范围是类型码加数据区（不含长度字段），
// 存储值与重算值不一致时返回错误。
//
// checkCRC verifies the stored CRC as required by the PNG
// specification: the checksum covers the type code plus the data area
// (not the length field), and a mismatch is reported as an error.
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

// ---------------------------------------------------------------------------
// IHDR chunk：数据流的第一个必需 chunk / the first mandatory chunk of a datastream
// ---------------------------------------------------------------------------

// IHDR chunk 必须是数据流中第一个出现的 chunk，给出图像的尺寸与格式参数：
//
//	Width:              4 bytes
//	Height:             4 bytes
//	Bit depth:          1 byte
//	Color type:         1 byte
//	Compression method: 1 byte
//	Filter method:      1 byte
//	Interlace method:   1 byte
//
// 宽、高以像素计，是 4 字节无符号整数：0 为非法值，最大值为 (2^31)-1，以
// 兼容难以处理 4 字节无符号整数的语言。bit depth 是每样本（每调色板索引）
// 而非每像素的位数，合法取值为 1、2、4、8、16，但并非每种 color type 都允许
// 全部取值。color type 描述图像数据的解释方式，由 1（使用调色板）、2（使用
// 彩色）、4（使用 alpha 通道）三个标志位相加得到，合法取值为 0、2、3、4、6。
// 各 color type 允许的 bit depth 如下（限制取值组合可以简化实现并排除压缩
// 效率差的组合，解码器必须支持全部合法组合）：
//
//	Color    Allowed    Interpretation
//	Type    Bit Depths
//
//	0       1,2,4,8,16  每像素一个灰度样本 / one grayscale sample per pixel
//
//	2       8,16        每像素一个 R,G,B 三元组 / one R,G,B triple per pixel
//
//	3       1,2,4,8     每像素一个调色板索引，必须带 PLTE chunk /
//	                    one palette index per pixel; a PLTE chunk must appear
//
//	4       8,16        灰度样本加一个 alpha 样本 / a grayscale sample plus an alpha sample
//
//	6       8,16        R,G,B 三元组加一个 alpha 样本 / an R,G,B triple plus an alpha sample
//
// 样本深度等于 bit depth，只有索引色（color type 3）例外，其样本深度恒为 8
// 位。compression method 表示压缩方式，目前只定义了 0（deflate/inflate，32
// KiB 滑动窗口），所有标准 PNG 都必须使用它，该字段为将来的扩展或私有变体
// 预留；解码器必须检查此字节，遇到无法识别的编码即报错。filter method 表示
// 压缩前的预处理方式，目前只定义了 0（五种基础滤波类型上的自适应滤波），
// 解码器同样必须检查此字节并拒绝未知编码。interlace method 表示数据传输
// 顺序，目前定义了 0（非隔行）与 1（Adam7 隔行）两种。
//
// IHDR is the mandatory first chunk of a PNG datastream; it carries the
// image dimensions and format parameters:
//
//	Width:              4 bytes
//	Height:             4 bytes
//	Bit depth:          1 byte
//	Color type:         1 byte
//	Compression method: 1 byte
//	Filter method:      1 byte
//	Interlace method:   1 byte
//
// Width and height are the image dimensions in pixels, stored as 4-byte
// unsigned integers: zero is invalid and each is capped at (2^31)-1 to
// accommodate languages that have difficulty with unsigned 4-byte
// values. Bit depth is the number of bits per sample (per palette
// index), not per pixel; the valid values are 1, 2, 4, 8 and 16, but
// not every color type allows every value. Color type describes how the
// image data is interpreted: it is the sum of the flags 1 (palette
// used), 2 (color used) and 4 (alpha channel used), and its valid
// values are 0, 2, 3, 4 and 6; the allowed combinations are listed in
// the table above.
//
// The sample depth equals the bit depth except for color type 3, whose
// sample depth is always 8 bits. Compression method names the
// compression scheme; only method 0 (deflate/inflate with a 32 KiB
// sliding window) is defined today and every standard PNG must use it,
// the field being reserved for future expansion or proprietary
// variants — decoders must check the byte and report an error on an
// unrecognized code. Filter method names the preprocessing applied
// before compression; only method 0 (adaptive filtering over five
// basic filter types) is defined, and decoders must likewise check the
// byte and reject unknown codes. Interlace method selects the data
// transmission order: 0 (no interlace) or 1 (Adam7 interlace).
type IHDR struct {
	Width             uint32
	Height            uint32
	BitDepth          uint8
	ColorType         uint8
	CompressionMethod uint8
	FilterMethod      uint8
	InterlaceMethod   uint8
}

// Parse 按 IHDR 固定的 13 字节布局填充各字段；数据区不足 13 字节时报错。
//
// Parse fills the fields from the fixed 13-byte IHDR layout; it reports
// an error when the data area is shorter than 13 bytes.
func (c *IHDR) Parse(chunk RawChunk) error {
	if chunk.Data == nil || len(chunk.Data) < 13 {
		return errors.New("invalid chunk data")
	}
	c.Width = by.Uint32(chunk.Data[:4])
	c.Height = by.Uint32(chunk.Data[4:8])
	c.BitDepth = chunk.Data[8]
	c.ColorType = chunk.Data[9]
	c.CompressionMethod = chunk.Data[10]
	c.FilterMethod = chunk.Data[11]
	c.InterlaceMethod = chunk.Data[12]
	return nil
}

// ChunkName 返回类型码 IHDR / ChunkName returns the chunk type code IHDR.
func (c *IHDR) ChunkName() ChunkName {
	return IHDRChunk
}

// Validate 按 PNG 规范校验各字段的合法组合：宽、高非零，压缩与滤波方法为
// 0，隔行方法不超过 1，且 color type 与 bit depth 的搭配在允许表内。
//
// Validate checks the IHDR fields against the combinations allowed by
// the PNG specification: non-zero dimensions, compression and filter
// method 0, an interlace method of at most 1, and a color type / bit
// depth pair from the table in the IHDR comment.
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

// ---------------------------------------------------------------------------
// IEND chunk：数据流的最后一个 chunk / the last chunk of a datastream
// ---------------------------------------------------------------------------

// IEND chunk 必须是数据流的最后一个 chunk，其数据区为空，标志 PNG 数据流
// 到此结束。
//
// IEND is the mandatory last chunk of a PNG datastream; its data field
// is empty and it marks the end of the image data.
type IEND struct {
}

// ChunkName 返回类型码 IEND / ChunkName returns the chunk type code IEND.
func (i *IEND) ChunkName() ChunkName {
	return IENDChunk
}

// Parse 校验数据区为空，否则报错。
//
// Parse reports an error unless the data area is empty.
func (i *IEND) Parse(chunk RawChunk) error {
	if len(chunk.Data) != 0 {
		return errors.New("invalid iend chunk data")
	}
	return nil
}

// ---------------------------------------------------------------------------
// PLTE chunk：调色板 / the color palette
// ---------------------------------------------------------------------------

// PLTE chunk 给出 1-256 个调色板项，每项 3 字节（R、G、B，0 = 黑，255 =
// 满强度）：
//
//	The PLTE chunk contains from 1 to 256 palette entries, each a three-byte series of the form:
//	 Red:   1 byte (0 = black, 255 = red)
//	 Green: 1 byte (0 = black, 255 = green)
//	 Blue:  1 byte (0 = black, 255 = blue)
//
// 项数由 chunk 长度决定，长度不能被 3 整除即为非法。color type 3（索引色）
// 必须携带 PLTE：像素值 0 取第 1 项，像素值 1 取第 2 项，依此类推；项数
// 不得超过 bit depth 可表示的范围（例如 bit depth 4 时最多 2^4 = 16 项），
// 少于该范围是允许的，但此时图像数据中任何越界像素值都算错误。color type
// 2/6（真彩，含带 alpha 者）可以携带 PLTE：此时它提供 1-256 色的建议量化
// 调色板，供无法直接显示真彩的查看器使用；没有 PLTE 时查看器只能自行选
// 色，而这往往由编码器一次性完成更好。color type 0/4（灰度）不得出现
// PLTE。若 PLTE 出现，它必须位于第一个 IDAT 之前，且整个文件至多一个。
// 调色板每样本恒占 8 位（1 字节），与图像 bit depth 无关——即使它是 16 位
// 真彩的建议量化也是如此。并不要求所有调色板项都被图像用到，也不要求各项
// 互不相同。
//
// The PLTE chunk holds 1-256 palette entries of three bytes each (red,
// green and blue; 0 = black, 255 = full intensity):
//
//	The PLTE chunk contains from 1 to 256 palette entries, each a three-byte series of the form:
//	 Red:   1 byte (0 = black, 255 = red)
//	 Green: 1 byte (0 = black, 255 = green)
//	 Blue:  1 byte (0 = black, 255 = blue)
//
// The entry count follows from the chunk length, which must be
// divisible by 3. The chunk is required for color type 3 (indexed
// color): pixel value n selects entry n, the entry count may not exceed
// what the image bit depth can represent (for example 2^4 = 16 entries
// at bit depth 4), and fewer entries are allowed — but then any
// out-of-range pixel value in the image data is an error. For color
// types 2 and 6 (truecolor, with or without alpha) the chunk is
// optional: it then suggests a set of 1-256 colors to quantize the
// truecolor image to when the viewer cannot display truecolor directly
// (without it the viewer picks colors on its own, which is usually
// better done once by the encoder). Color types 0 and 4 (grayscale)
// must not carry a PLTE chunk. When present it must precede the first
// IDAT, and there must not be more than one. The palette is always 8
// bits (1 byte) per sample regardless of the image bit depth — even
// when it is a suggested quantization of a 16-bit truecolor image. The
// palette entries need not all be used by the image, nor need they be
// distinct.
type PLTE struct {
	Colors []*PLTEColor
}

// PLTEColor 是调色板中的一项：红、绿、蓝各 1 字节的样本值。
//
// PLTEColor is one entry of the palette: one byte of sample each for
// red, green and blue.
type PLTEColor struct {
	Red   uint8
	Green uint8
	Blue  uint8
}

// ChunkName 返回类型码 PLTE / ChunkName returns the chunk type code PLTE.
func (p *PLTE) ChunkName() ChunkName {
	return PLTEChunk
}

// Parse 把数据区按 3 字节一项拆分为调色板项；数据区为空或长度不能被 3
// 整除时报错。
//
// Parse splits the data area into three-byte palette entries; an empty
// data area or a length that is not divisible by 3 is an error.
func (p *PLTE) Parse(chunk RawChunk) error {
	if chunk.Data == nil || len(chunk.Data)%3 != 0 || len(chunk.Data) < 3 {
		return errors.New("invalid plte chunk data")
	}
	p.Colors = make([]*PLTEColor, 0, len(chunk.Data)/3)
	for i := 0; i+2 < len(chunk.Data); i += 3 {
		p.Colors = append(p.Colors, &PLTEColor{
			Red:   chunk.Data[i],
			Green: chunk.Data[i+1],
			Blue:  chunk.Data[i+2],
		})
	}

	return nil
}

// ---------------------------------------------------------------------------
// IDAT chunk：压缩后的图像数据 / the compressed image data
// ---------------------------------------------------------------------------

// IDAT chunk 承载真正的图像数据。编码过程：先按 Image layout 把图像排成扫
// 描行（布局与总长由 IHDR 各字段决定），再按 IHDR 指定的滤波方法滤波（滤波
// 方法 0——目前唯一定义的方法——意味着每条扫描行前加一个滤波类型字节），
// 最后按 IHDR 指定的压缩方法压缩；IDAT 的数据区就是压缩算法的输出数据流。
// 读取图像数据即逆着这一流程还原。
//
// IDAT 可以有多个：此时它们必须连续出现，中间不得插入其他 chunk，压缩数据
// 流是所有 IDAT 数据区的直接拼接。编码器可以随意切分压缩数据流（允许多个
// IDAT 是为了让编码器能在固定大小的内存中工作，chunk 尺寸通常对应编码器
// 的缓冲区大小）。要强调的是，IDAT 的 chunk 边界在压缩数据流中没有语义，
// 可以在任意位置出现：每个 IDAT 只装一个数据字节的 PNG 文件是合法的（只是
// 极其浪费空间），零长度的 IDAT 也同样合法。
//
// The IDAT chunk contains the actual image data. To create this data:
// begin with image scanlines represented as described in Image layout
// (the layout and total size of this raw data are determined by the
// fields of IHDR); filter the image data according to the filtering
// method specified by the IHDR chunk (with filter method 0, the only
// one currently defined, this implies prepending a filter type byte to
// each scanline); then compress the filtered data using the compression
// method specified by IHDR. The IDAT chunk contains the output
// datastream of the compression algorithm. To read the image data,
// reverse this process.
//
// There can be multiple IDAT chunks; if so, they must appear
// consecutively with no other intervening chunks and the compressed
// datastream is then the concatenation of the contents of all the IDAT
// chunks. The encoder can divide the compressed datastream into IDAT
// chunks however it wishes (multiple IDAT chunks are allowed so that
// encoders can work in a fixed amount of memory; typically the chunk
// size will correspond to the encoder's buffer size). It is important
// to emphasize that IDAT chunk boundaries have no semantic significance
// and can occur at any point in the compressed datastream: a PNG file
// in which each IDAT chunk contains only one data byte is legal, though
// remarkably wasteful of space (zero-length IDAT chunks are legal too,
// though even more wasteful).
type IDAT struct {
	Length        uint32
	ChunkTypeCode string
	Data          []byte
}

// ChunkName 返回类型码 IDAT / ChunkName returns the chunk type code IDAT.
func (i *IDAT) ChunkName() ChunkName {
	return IDATChunk
}

// Parse 记录数据区的长度、chunk 类型码与压缩字节本身；压缩流的重组与解压
// 由上层完成。
//
// Parse records the data length, the chunk type code and the compressed
// bytes themselves; reassembling and inflating the stream is left to
// the caller.
func (i *IDAT) Parse(chunk RawChunk) error {
	i.Length = uint32(len(chunk.Data))
	i.ChunkTypeCode = string(chunk.Name)
	i.Data = chunk.Data[:]
	return nil
}

// ---------------------------------------------------------------------------
// bKGD chunk：建议的背景色 / the suggested background color
// ---------------------------------------------------------------------------

// bKGD chunk 指定展示图像时建议使用的背景色；查看器并不必须遵守，可以自行
// 选择其他背景。数据区的布局随 color type 而变：
//
// 索引色（color type 3）：
//
//	Palette index:  1 byte
//
// 值为背景色的调色板索引。灰度（color type 0 和 4，含带 alpha 者）：
//
//	Gray:  2 bytes, range 0 .. (2^bitdepth)-1
//
// （为保持一致，不论图像 bit depth 一律使用 2 字节。）值为背景的灰度级。
// 真彩（color type 2 和 6，含带 alpha 者）：
//
//	Red:   2 bytes, range 0 .. (2^bitdepth)-1
//	Green: 2 bytes, range 0 .. (2^bitdepth)-1
//	Blue:  2 bytes, range 0 .. (2^bitdepth)-1
//
// （同样不论 bit depth 每样本一律 2 字节。）这是背景使用的 RGB 颜色。若
// bKGD 出现，它必须位于第一个 IDAT 之前，并位于 PLTE（如果有）之后。
//
// The bKGD chunk specifies a default background color to present the
// image against; viewers are not bound to honor it and may choose a
// different background. The data layout depends on the color type.
// For color type 3 (indexed color) it holds a single palette index
// byte, the palette entry to be used as background. For color types 0
// and 4 (grayscale, with or without alpha) it holds:
//
//	Gray:  2 bytes, range 0 .. (2^bitdepth)-1
//
// (two bytes are used regardless of the image bit depth for
// consistency) and the value is the gray level to be used as
// background. For color types 2 and 6 (truecolor, with or without
// alpha) it holds:
//
//	Red:   2 bytes, range 0 .. (2^bitdepth)-1
//	Green: 2 bytes, range 0 .. (2^bitdepth)-1
//	Blue:  2 bytes, range 0 .. (2^bitdepth)-1
//
// (again two bytes per sample regardless of the bit depth), the RGB
// color to be used as background. When present, the bKGD chunk must
// precede the first IDAT chunk and must follow the PLTE chunk, if any.
type BKGD struct {
	// Palette 是 color type 3（索引色）背景色的调色板索引。
	// Palette is the palette index of the background color for color type 3 (indexed color).
	Palette uint8
	// Gray 是 color type 0/4（灰度）背景色的灰度级。
	// Gray is the gray level of the background color for color types 0 and 4 (grayscale).
	Gray uint16
	// Red、Green、Blue 是 color type 2/6（真彩）背景色的三个分量。
	// Red, Green and Blue are the components of the background color for color types 2 and 6 (truecolor).
	Red   uint16
	Green uint16
	Blue  uint16
}

// ChunkName 返回类型码 bKGD / ChunkName returns the chunk type code bKGD.
func (b *BKGD) ChunkName() ChunkName {
	return BKGDChunk
}

// Parse 按数据区长度区分 color type：1 字节为调色板索引，2 字节为灰度值，
// 6 字节为 RGB；其余长度非法。
//
// Parse dispatches on the data length to distinguish the color types:
// 1 byte is a palette index, 2 bytes a gray level and 6 bytes an RGB
// triple; any other length is invalid.
func (b *BKGD) Parse(chunk RawChunk) error {
	switch len(chunk.Data) {
	case 1: // color type 3：调色板索引 / palette index
		b.Palette = chunk.Data[0]
	case 2: // color type 0/4：灰度级 / gray level
		b.Gray = by.Uint16(chunk.Data[:2])
	case 6: // color type 2/6：RGB 颜色 / RGB color
		b.Red = by.Uint16(chunk.Data[0:2])
		b.Green = by.Uint16(chunk.Data[2:4])
		b.Blue = by.Uint16(chunk.Data[4:6])
	default:
		return errors.New("invalid bkgd chunk data")
	}
	return nil
}

// ---------------------------------------------------------------------------
// cHRM chunk：基色与白点的 1931 CIE 色度 / 1931 CIE chromaticities of the primaries and white point
// ---------------------------------------------------------------------------

// cHRM chunk 供需要与设备无关地描述颜色的应用使用：它给出图像所用红、绿、
// 蓝基色以及参考白点的 1931 CIE x,y 色度。数据区为 8 个 4 字节无符号整数：
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
// 每个值是色度乘以 100000 后的 4 字节无符号整数，例如 0.3127 存为 31270。
// 所有 PNG 文件都可以携带 cHRM，只是它对灰度图像几乎没有意义。编码器不知
// 道色度值时不应写入 cHRM：缺少该 chunk 即表示图像的基色依赖具体设备。
// 若 cHRM 出现，它必须位于第一个 IDAT 之前，且在 PLTE（如果有）之前。
//
// The cHRM chunk lets applications that need device-independent
// specification of colors in a PNG file specify the 1931 CIE x,y
// chromaticities of the red, green and blue primaries used in the image
// and of the referenced white point. The chunk contains eight 4-byte
// unsigned integers:
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
// Each value is encoded as a 4-byte unsigned integer representing the x
// or y value times 100000; a value of 0.3127, for example, is stored as
// the integer 31270. cHRM is allowed in all PNG files although it is of
// little value for grayscale images. If the encoder does not know the
// chromaticity values it should not write a cHRM chunk: the absence of
// the chunk indicates that the image's primary colors are
// device-dependent. When present, the cHRM chunk must precede the first
// IDAT chunk and also the PLTE chunk, if any.
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

// ChunkName 返回类型码 cHRM / ChunkName returns the chunk type code cHRM.
func (c *CHRM) ChunkName() ChunkName {
	return CHRMChunk
}

// Parse 按固定顺序读取 8 个 4 字节色度值；数据区不足 32 字节时报错。
//
// Parse reads the eight 4-byte chromaticity values in their fixed
// order; a data area shorter than 32 bytes is an error.
func (c *CHRM) Parse(chunk RawChunk) error {
	if chunk.Data == nil || len(chunk.Data) < 32 {
		return errors.New("invalid chrm chunk data")
	}
	c.WhiteX = by.Uint32(chunk.Data[:4])
	c.WhiteY = by.Uint32(chunk.Data[4:8])
	c.RedX = by.Uint32(chunk.Data[8:12])
	c.RedY = by.Uint32(chunk.Data[12:16])
	c.GreenX = by.Uint32(chunk.Data[16:20])
	c.GreenY = by.Uint32(chunk.Data[20:24])
	c.BlueX = by.Uint32(chunk.Data[24:28])
	c.BlueY = by.Uint32(chunk.Data[28:32])
	return nil
}

// ---------------------------------------------------------------------------
// gAMA chunk：图像 gamma / the image gamma
// ---------------------------------------------------------------------------

// gAMA chunk 指定拍摄（或模拟拍摄）图像的相机的 gamma，也就是图像相对于
// 原始场景的 gamma；更准确地说，它编码的是 Gamma Tutorial 中定义的
// file_gamma 值。数据区为 4 个字节：
//
//	Image gamma: 4 bytes
//
// 值以 4 字节无符号整数编码，等于 gamma 乘以 100000，例如 gamma 0.45 存为
// 45000。编码器不知道图像的 gamma 值时不应写入 gAMA：缺少该 chunk 即表示
// gamma 未知。若 gAMA 出现，它必须位于第一个 IDAT 之前，且在 PLTE（如果
// 有）之前。
//
// The gAMA chunk specifies the gamma of the camera (or simulated
// camera) that produced the image, and thus the gamma of the image with
// respect to the original scene; more precisely it encodes the
// file_gamma value as defined in the PNG Gamma Tutorial. The chunk
// contains four bytes:
//
//	Image gamma: 4 bytes
//
// The value is encoded as a 4-byte unsigned integer representing gamma
// times 100000; a gamma of 0.45 is stored as the integer 45000. If the
// encoder does not know the image's gamma value it should not write a
// gAMA chunk: the absence of the chunk indicates that the gamma is
// unknown. When present, the gAMA chunk must precede the first IDAT
// chunk and also the PLTE chunk, if any.
type GAMA struct {
	// ImageGamma 是 gamma×100000 的编码值。
	// ImageGamma is gamma times 100000, encoded as a 4-byte unsigned integer.
	ImageGamma uint32
}

// ChunkName 返回类型码 gAMA / ChunkName returns the chunk type code gAMA.
func (g *GAMA) ChunkName() ChunkName {
	return GAMAChunk
}

// Parse 读取 4 字节的 gamma 编码值；数据区不足 4 字节时报错。
//
// Parse reads the 4-byte encoded gamma value; a data area shorter than
// 4 bytes is an error.
func (g *GAMA) Parse(chunk RawChunk) error {
	if chunk.Data == nil || len(chunk.Data) < 4 {
		return errors.New("invalid gama chunk data")
	}
	g.ImageGamma = by.Uint32(chunk.Data[:4])
	return nil
}

// ---------------------------------------------------------------------------
// hIST chunk：调色板使用频率直方图 / the palette usage histogram
// ---------------------------------------------------------------------------

// hIST chunk 给出调色板中每个颜色被使用的大致频率；只有当 PLTE 出现时才
// 可以出现。查看器无法提供调色板列出的全部颜色时，直方图可以帮助它决定
// 选用哪个子集来显示。数据区是一串 2 字节（16 位）无符号整数，与 PLTE
// 的每一项一一对应；每个值正比于图像中取该调色板索引的像素所占比例，正
// 的比例因子由编码器选定。
//
// 直方图项是近似值，只有一个例外：零值表示对应的调色板项在图像中完全未
// 被使用——只要存在该颜色的像素，对应的直方图项就必须非零。当调色板是
// 真彩图像的建议量化时，直方图必然是近似的，因为解码器把像素映射到调色
// 板项的方式可能与编码器不同；这种情况下不应出现零项。若 hIST 出现，它
// 必须位于 PLTE 之后、第一个 IDAT 之前。
//
// The hIST chunk gives the approximate usage frequency of each color in
// the color palette; a histogram chunk can appear only when a palette
// chunk appears. If a viewer is unable to provide all the colors listed
// in the palette, the histogram may help it decide how to choose a
// subset of the colors for display. The chunk contains a series of
// 2-byte (16-bit) unsigned integers, exactly one entry for each entry
// in the PLTE chunk; each entry is proportional to the fraction of
// pixels in the image that have that palette index, the exact scale
// factor being chosen by the encoder.
//
// Histogram entries are approximate, with the exception that a zero
// entry specifies that the corresponding palette entry is not used at
// all in the image: an entry must be nonzero if there are any pixels of
// that color. When the palette is a suggested quantization of a
// truecolor image the histogram is necessarily approximate, since a
// decoder may map pixels to palette entries differently than the
// encoder did; in this situation zero entries should not appear. The
// hIST chunk, if it appears, must follow the PLTE chunk and must
// precede the first IDAT chunk.
type HIST struct {
	Elements []uint16
}

// ChunkName 返回类型码 hIST / ChunkName returns the chunk type code hIST.
func (h *HIST) ChunkName() ChunkName {
	return HISTChunk
}

// Parse 把数据区按 2 字节一项读成直方图；长度为奇数时报错。
//
// Parse reads the data area as 2-byte histogram entries; an odd length
// is an error.
func (h *HIST) Parse(chunk RawChunk) error {
	if len(chunk.Data)%2 != 0 {
		return errors.New("invalid hist chunk data")
	}
	n := len(chunk.Data) / 2
	h.Elements = make([]uint16, 0, n)
	for i := 0; i < n; i++ {
		h.Elements = append(h.Elements, by.Uint16(chunk.Data[i*2:i*2+2]))
	}
	return nil
}

// ---------------------------------------------------------------------------
// pHYs chunk：像素的物理尺寸 / the physical size of the pixels
// ---------------------------------------------------------------------------

// pHYs chunk 指定图像显示时每个像素的预期尺寸或宽高比。数据区为：
//
//	Pixels per unit, X axis: 4 bytes (unsigned integer)
//	Pixels per unit, Y axis: 4 bytes (unsigned integer)
//	Unit specifier:          1 byte
//
// 单位说明符的合法取值为：
//
//	0: unit is unknown
//	1: unit is the meter
//
// 取值为 0 时，pHYs 只定义像素宽高比，像素的实际尺寸仍未指定；换算注意
// 事项：1 英寸恰好等于 0.0254 米。没有该辅助 chunk 时，像素被视为方形
// 且物理尺寸未知。若出现，该 chunk 必须位于第一个 IDAT 之前。
//
// The pHYs chunk specifies the intended pixel size or aspect ratio for
// display of the image. It contains:
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
// When the unit specifier is 0, the pHYs chunk defines the pixel aspect
// ratio only; the actual size of the pixels remains unspecified.
// Conversion note: one inch is equal to exactly 0.0254 meters. If this
// ancillary chunk is not present, pixels are assumed to be square and
// the physical size of each pixel is unknown. If present, this chunk
// must precede the first IDAT chunk.
type PHYS struct {
	X             uint32
	Y             uint32
	UnitSpecifier uint8
}

// ChunkName 返回类型码 pHYs / ChunkName returns the chunk type code pHYs.
func (p *PHYS) ChunkName() ChunkName {
	return PHYSChunk
}

// Parse 读取 X、Y 两个像素密度值与 1 字节单位说明符；数据区不足 9 字节时
// 报错。
//
// Parse reads the two pixel-density values X and Y plus the 1-byte unit
// specifier; a data area shorter than 9 bytes is an error.
func (p *PHYS) Parse(chunk RawChunk) error {
	if chunk.Data == nil || len(chunk.Data) < 9 {
		return errors.New("invalid phys chunk data")
	}
	p.X = by.Uint32(chunk.Data[:4])
	p.Y = by.Uint32(chunk.Data[4:8])
	p.UnitSpecifier = chunk.Data[8]
	return nil
}

// ---------------------------------------------------------------------------
// sBIT chunk：源数据的有效位数 / significant bits of the source data
// ---------------------------------------------------------------------------

// 为简化解码器，PNG 规定只能使用某些样本深度，并进一步要求样本值被缩放到
// 该深度的完整取值范围；sBIT chunk 则用来记录原始的有效位数，使解码器即
// 使面对 PNG 不直接支持的样本深度也能无损恢复原始数据。若编码器把数据从
// 更低的样本深度转换而来，建议写出 sBIT。数据区的内容依 color type 而定：
// 灰度（color type 0）为 1 字节；真彩（color type 2）为 3 字节，依次是
// 红、绿、蓝通道在源数据中的有效位数；索引色（color type 3）为 3 字节，
// 依次是调色板项红、绿、蓝分量的有效位数；灰度加 alpha（color type 4）
// 为 2 字节，依次是源灰度数据与源 alpha 数据的有效位数；真彩加 alpha
// （color type 6）为 4 字节，依次是红、绿、蓝、alpha 通道的有效位数。
// sBIT 中给出的每个深度必须大于 0 且不超过样本深度（索引色图像为 8，其
// 余 color type 为 IHDR 给出的 bit depth）。
//
// 解码器可以不理会 sBIT：存储下来的图像本身就是按 IHDR 样本深度合法的
// PNG。但若解码器想按原始精度恢复数据，只需把存储的样本（索引色图像为
// 存储的调色板项）右移即可。编码器必须以高位与原始数据一致的方式缩放数
// 据。若 sBIT 出现，它必须位于第一个 IDAT 之前，且在 PLTE（如果有）之前。
//
// To simplify decoders, PNG specifies that only certain sample depths
// can be used and that sample values must be scaled to the full range
// of possible values at the sample depth. The sBIT chunk stores the
// original number of significant bits, which allows a decoder to
// recover the original data losslessly even when it had a sample depth
// not directly supported by PNG; an encoder should emit an sBIT chunk
// if it converted the data from a lower sample depth. The chunk holds
// one byte for color type 0 (grayscale), the number of bits that were
// significant in the source data; three bytes for color type 2
// (truecolor), the significant bits of the red, green and blue
// channels; three bytes for color type 3 (indexed color), the
// significant bits of the red, green and blue components of the palette
// entries; two bytes for color type 4 (grayscale with alpha), the
// significant bits of the source grayscale and alpha data; and four
// bytes for color type 6 (truecolor with alpha), the significant bits
// of the red, green, blue and alpha channels. Each depth must be
// greater than zero and less than or equal to the sample depth (8 for
// indexed-color images, the IHDR bit depth for the other color types).
//
// A decoder need not pay attention to sBIT: the stored image is a valid
// PNG file at the sample depth indicated by IHDR. To recover the
// original data at its original precision, however, it is enough to
// right-shift the stored samples (the stored palette entries, for an
// indexed-color image); the encoder must have scaled the data so that
// the high-order bits match the original data. If the sBIT chunk
// appears, it must precede the first IDAT chunk and also the PLTE
// chunk, if any.
type SBIT struct {
	OrgData [4]byte
}

// ChunkName 返回类型码 sBIT / ChunkName returns the chunk type code sBIT.
func (s *SBIT) ChunkName() ChunkName {
	return SBITChunk
}

// Parse 把数据区的前 4 字节读入 OrgData，不足的部分补 0；数据区为空时报错。
//
// Parse reads up to four bytes of the data area into OrgData, padding
// the missing ones with 0; an empty data area is an error.
func (s *SBIT) Parse(chunk RawChunk) error {
	if chunk.Data == nil || len(chunk.Data) < 1 {
		return errors.New("invalid sbit chunk data")
	}
	for i := 0; i < len(s.OrgData); i++ {
		if len(chunk.Data) <= i {
			s.OrgData[i] = 0
		} else {
			s.OrgData[i] = chunk.Data[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// tEXt chunk：未压缩的文本信息 / uncompressed textual information
// ---------------------------------------------------------------------------

// 编码器想随图像一起记录的文本信息可以存放在 tEXt chunk 中。每个 tEXt
// chunk 包含一个关键字和一个文本串，格式为：
//
//	Keyword:        1-79 bytes (character string)
//	Null separator: 1 byte
//	Text:           n bytes (character string)
//
// 关键字与文本串之间以一个零字节（空字符）分隔。关键字与文本串都不得含空
// 字符；文本串也不以空字符结尾（chunk 长度足以确定其结尾）。关键字至少 1
// 个字符且少于 80 个字符；文本串长度可以是 0 到（最大允许 chunk 尺寸减去
// 关键字与分隔符长度）之间的任意值。
//
// Textual information that the encoder wishes to record with the image
// can be stored in tEXt chunks. Each tEXt chunk contains a keyword and a
// text string in the format:
//
//	Keyword:        1-79 bytes (character string)
//	Null separator: 1 byte
//	Text:           n bytes (character string)
//
// The keyword and the text string are separated by a zero byte (null
// character). Neither the keyword nor the text string can contain a
// null character, and the text string is not null-terminated (the length
// of the chunk is sufficient information to locate the ending). The
// keyword must be at least one character and less than 80 characters
// long; the text string may be zero bytes long and is otherwise limited
// only by the maximum permissible chunk size less the length of the
// keyword and the separator.
type TEXT struct {
	Keyword   string
	Separator string
	Text      string
}

// ChunkName 返回类型码 tEXt / ChunkName returns the chunk type code tEXt.
func (t *TEXT) ChunkName() ChunkName {
	return TEXTChunk
}

// nullSep 是文本类 chunk 中关键字与正文之间的单字节空分隔符。
//
// nullSep is the single null byte that separates the keyword from the
// text in the text chunks.
const nullSep = string(byte(0x00))

// Parse 在数据区中定位第一个空字节，把其前的部分作为关键字、其后的部分
// 作为正文；关键字长度必须落在 1-79 字节之间，否则报错。
//
// Parse locates the first null byte of the data area, taking everything
// before it as the keyword and everything after it as the text; the
// keyword length must fall between 1 and 79 bytes or an error is
// reported.
func (t *TEXT) Parse(chunk RawChunk) error {
	sep := bytes.IndexByte(chunk.Data, 0x00)
	if sep < 1 || sep > 79 {
		return errors.New("invalid text")
	}
	t.Keyword = string(chunk.Data[:sep])
	t.Separator = nullSep
	t.Text = string(chunk.Data[sep+1:])

	return nil
}

// ---------------------------------------------------------------------------
// tIME chunk：最后修改时间 / the time of the last modification
// ---------------------------------------------------------------------------

// tIME chunk 给出图像最后一次修改（而非首次创建）的时间。数据区为：
//
//	Year:   2 bytes (complete; for example, 1995, not 95)
//	Month:  1 byte (1-12)
//	Day:    1 byte (1-31)
//	Hour:   1 byte (0-23)
//	Minute: 1 byte (0-59)
//	Second: 1 byte (0-60)    (yes, 60, for leap seconds; not 61,
//	                          a common error)
//
// 应当给出世界时（UTC，也称 GMT）而不是本地时间。tIME 用作图像数据一变化
// 就自动更新的时间戳；规范建议不改动图像数据的 PNG 编辑器不要修改它。
// 用户自定义的时间请改用 Creation Time 这个 tEXt 关键字。
//
// The tIME chunk gives the time of the last image modification (not the
// time of the initial image creation). It contains:
//
//	Year:   2 bytes (complete; for example, 1995, not 95)
//	Month:  1 byte (1-12)
//	Day:    1 byte (1-31)
//	Hour:   1 byte (0-23)
//	Minute: 1 byte (0-59)
//	Second: 1 byte (0-60)    (yes, 60, for leap seconds; not 61,
//	                          a common error)
//
// Universal Time (UTC, also called GMT) should be specified rather than
// local time. The tIME chunk is intended for use as an
// automatically-applied time stamp that is updated whenever the image
// data is changed; PNG editors that do not change the image data are
// recommended not to alter it. See also the Creation Time tEXt keyword,
// which can be used for a user-supplied time.
type TIME struct {
	Year   uint16
	Month  uint8
	Day    uint8
	Hour   uint8
	Minute uint8
	Second uint8
}

// ChunkName 返回类型码 tIME / ChunkName returns the chunk type code tIME.
func (t *TIME) ChunkName() ChunkName {
	return TIMEChunk
}

// Parse 读取 7 字节的时间字段（2 字节年 + 月、日、时、分、秒各 1 字节），
// 数据区不足 7 字节时报错。
//
// Parse reads the 7-byte time fields (a 2-byte year plus one byte each
// for month, day, hour, minute and second); a data area shorter than 7
// bytes is an error.
func (t *TIME) Parse(chunk RawChunk) error {
	if len(chunk.Data) < 7 {
		return errors.New("invalid time chunk data")
	}
	t.Year = by.Uint16(chunk.Data[:2])
	t.Month = chunk.Data[2]
	t.Day = chunk.Data[3]
	t.Hour = chunk.Data[4]
	t.Minute = chunk.Data[5]
	t.Second = chunk.Data[6]
	return nil
}

// ToTime 把各字段组装为 UTC 时区的 time.Time；各字段自身的合法性（例如
// 月份 1-12）由调用方负责。
//
// ToTime assembles the fields into a time.Time in the UTC zone; the
// caller is responsible for the validity of the individual fields (the
// month being 1-12, for example).
func (t *TIME) ToTime() time.Time {
	return time.Date(int(t.Year), time.Month(t.Month), int(t.Day), int(t.Hour), int(t.Minute), int(t.Second), 0, time.UTC)
}

// ---------------------------------------------------------------------------
// iTXt chunk：UTF-8 文本信息 / UTF-8 encoded textual information
// ---------------------------------------------------------------------------

// iTXt chunk 与 tEXt、zTXt 一样承载文本数据，但正文采用 UTF-8 编码，并且
// 翻译关键字与正文本身都可以压缩。PNG 文件中的 XMP 元数据即以关键字
// "XML:com.adobe.xmp" 的 iTXt chunk 存储。数据区布局：
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
//
// The iTXt chunk contains textual data just like tEXt and zTXt, but the
// text is UTF-8 encoded and both the translated keyword and the text
// itself can be compressed. XMP metadata in PNG files is stored as an
// iTXt chunk with the keyword "XML:com.adobe.xmp". The chunk layout is:
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
	// Data 保留 chunk 的原始数据区。 / Data keeps the raw chunk payload.
	Data []byte
}

// ChunkName 返回类型码 iTXt / ChunkName returns the chunk type code iTXt.
func (i *ITXT) ChunkName() ChunkName {
	return ITXTChunk
}

// Parse 校验并拆分 iTXt 数据区：关键字（1-79 字节）、压缩标志与压缩方法
// （标志只允许 0/1，压缩时方法只允许 0），随后的语言标签、翻译关键字与
// 正文以空字节分隔，正文延伸到 chunk 末尾；压缩标志为 1 时正文按 zlib
// 解压。
//
// Parse validates and splits the iTXt data area: the keyword (1-79
// bytes) plus the compression flag and method (only 0/1 is legal for the
// flag and only method 0 when compressed), then the language tag, the
// translated keyword and the text separated by null bytes, the text
// extending to the end of the chunk; with the compression flag set to 1
// the text is inflated from zlib.
func (i *ITXT) Parse(chunk RawChunk) error {
	sep := bytes.IndexByte(chunk.Data, 0x00)
	if sep < 1 || sep > 79 {
		return errors.New("invalid itxt chunk data")
	}
	if len(chunk.Data) < sep+3 {
		return errors.New("invalid itxt chunk data")
	}
	i.Keyword = string(chunk.Data[:sep])
	i.Data = chunk.Data[:]
	rest := chunk.Data[sep+1:]
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
	// 压缩方法之后是一个空分隔符，其后的语言标签、翻译关键字与正文都以
	// 空字节分隔，正文本身延伸到 chunk 末尾
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

// ---------------------------------------------------------------------------
// tRNS chunk：简单透明 / simple transparency
// ---------------------------------------------------------------------------

// tRNS chunk 声明图像使用简单透明：索引色图像为每个调色板项关联 alpha 值，
// 灰度与真彩图像则给出单一的透明色。简单透明不如完整 alpha 通道表现力强，
// 但占用空间更小，足以应对许多常见情况。数据区的布局随 color type 而变。
// 索引色（color type 3）是一串 1 字节 alpha 值，与 PLTE 的项一一对应：
//
//	Alpha for palette index 0:  1 byte
//	Alpha for palette index 1:  1 byte
//	... etc ...
//
// 每一项表示对应调色板索引的像素应按该 alpha 值处理。alpha 的解释与 8 位
// 完全相同：不论图像 bit depth，0 为全透明、255 为全不透明。tRNS 中的
// alpha 值不能多于调色板项数，但可以少于；此时其余调色板项的 alpha 视为
// 255。若只需把调色板索引 0 设为透明，1 字节的 tRNS 就够了。灰度
// （color type 0）是一个灰度值，格式为：
//
//	Gray:  2 bytes, range 0 .. (2^bitdepth)-1
//
// （为保持一致，不论 bit depth 一律使用 2 字节。）等于该灰度级的像素按透
// 明处理（等价于 alpha 0），其余像素按全不透明处理（alpha 值
// (2^bitdepth)-1）。真彩（color type 2）是一个 RGB 颜色值，格式为：
//
//	Red:   2 bytes, range 0 .. (2^bitdepth)-1
//	Green: 2 bytes, range 0 .. (2^bitdepth)-1
//	Blue:  2 bytes, range 0 .. (2^bitdepth)-1
//
// （同样每样本一律 2 字节。）等于该颜色值的像素按透明处理，其余像素按全不
// 透明处理。color type 4 与 6 已经有完整 alpha 通道，禁止携带 tRNS。
//
// 注意：处理 16 位灰度或真彩数据时，必须比较样本值的两个字节才能判断像素
// 是否透明。解码器为显示可以丢弃样本的低位字节，但在测试透明性之前不得这
// 样做：例如指定灰度 0x0001 透明时，只比较高位而把 0x0002 也判为透明就是
// 错误的。若 tRNS 出现，它必须位于第一个 IDAT 之前，并位于 PLTE（如果
// 有）之后。
//
// The tRNS chunk specifies that the image uses simple transparency:
// alpha values associated with palette entries for indexed-color
// images, or a single transparent color for grayscale and truecolor
// images. Although simple transparency is not as elegant as the full
// alpha channel, it requires less storage space and is sufficient for
// many common cases. The data layout depends on the color type. For
// color type 3 (indexed color) it is a series of one-byte alpha values
// corresponding to entries in the PLTE chunk:
//
//	Alpha for palette index 0:  1 byte
//	Alpha for palette index 1:  1 byte
//	... etc ...
//
// Each entry indicates that pixels of the corresponding palette index
// must be treated as having the specified alpha value; alpha is
// interpreted exactly as in an 8-bit full alpha channel — 0 is fully
// transparent and 255 fully opaque, regardless of the image bit depth.
// tRNS must not contain more alpha values than there are palette
// entries, but it may contain fewer; the remaining palette entries then
// default to 255. When only palette index 0 needs to be transparent, a
// one-byte tRNS chunk suffices. For color type 0 (grayscale) it holds a
// single gray level in the format:
//
//	Gray:  2 bytes, range 0 .. (2^bitdepth)-1
//
// (two bytes are used regardless of the bit depth for consistency);
// pixels of the specified gray level are treated as transparent
// (equivalent to alpha 0) and all others as fully opaque (alpha value
// (2^bitdepth)-1). For color type 2 (truecolor) it holds a single RGB
// color value in the format:
//
//	Red:   2 bytes, range 0 .. (2^bitdepth)-1
//	Green: 2 bytes, range 0 .. (2^bitdepth)-1
//	Blue:  2 bytes, range 0 .. (2^bitdepth)-1
//
// (again two bytes per sample regardless of the bit depth); pixels of
// the specified color value are treated as transparent and all others
// as fully opaque. tRNS is prohibited for color types 4 and 6, since a
// full alpha channel is already present in those cases.
//
// Note: when dealing with 16-bit grayscale or truecolor data it is
// important to compare both bytes of the sample values to determine
// whether a pixel is transparent. Decoders may drop the low-order byte
// of the samples for display, but this must not happen until after the
// data has been tested for transparency: if the grayscale level 0x0001
// is specified to be transparent, comparing only the high-order byte
// would wrongly decide that 0x0002 is also transparent. When present,
// the tRNS chunk must precede the first IDAT chunk and must follow the
// PLTE chunk, if any.
type TRNS struct {
	// PaletteAlphas 是 color type 3（索引色）前 len(PaletteAlphas) 个调色板项的 alpha 值。
	// PaletteAlphas holds the alpha values of the first len(PaletteAlphas) palette entries for color type 3 (indexed color).
	PaletteAlphas []uint8
	// Gray 是 color type 0/4（灰度）中视为透明的灰度级。
	// Gray is the gray level to be treated as transparent for color types 0 and 4 (grayscale).
	Gray uint16
	// Red、Green、Blue 是 color type 2（真彩）中视为透明的 RGB 颜色。
	// Red, Green and Blue form the rgb color to be treated as transparent for color type 2 (truecolor).
	Red   uint16
	Green uint16
	Blue  uint16
	// Data 保留原始数据区，其用法见 IDATsToColor 中对 TRNS.Data 的使用。
	// Data keeps the raw chunk payload, see its use in IDATsToColor.
	Data []byte
}

// ChunkName 返回类型码 tRNS / ChunkName returns the chunk type code tRNS.
func (T *TRNS) ChunkName() ChunkName {
	return TRNSChunk
}

// Parse 按 chunk 数据的长度填充 TRNS 的字段：2 字节是灰度值，6 字节是
// RGB 颜色，其余长度都按调色板 alpha 列表处理。注意：当调色板图像恰好有
// 两个（或六个）项时，tRNS 与灰度值（或 RGB 颜色）从长度上无法区分，必须
// 结合 IHDR 的 color type 才能判断；解码时 IDATsToColor 会按真实的
// color type 重新解释 TRNS.Data。
//
// Parse fills the fields of TRNS based on the length of the chunk data:
// two bytes are a gray level, six bytes an rgb color and any other
// length a list of palette alpha values. Note that a tRNS chunk of a
// palette image with exactly two (or six) entries is indistinguishable
// from a gray level (or rgb color) without the color type from IHDR;
// IDATsToColor re-interprets TRNS.Data with the actual color type when
// decoding.
func (T *TRNS) Parse(chunk RawChunk) error {
	if chunk.Data == nil || len(chunk.Data) == 0 || len(chunk.Data) > 256 {
		return errors.New("invalid trns chunk data")
	}
	T.Data = chunk.Data[:]
	switch len(chunk.Data) {
	case 2: // color type 0/4：灰度级 / gray level
		T.Gray = by.Uint16(chunk.Data[:2])
	case 6: // color type 2：RGB 颜色 / rgb color
		T.Red = by.Uint16(chunk.Data[0:2])
		T.Green = by.Uint16(chunk.Data[2:4])
		T.Blue = by.Uint16(chunk.Data[4:6])
	default: // color type 3：调色板 alpha 值 / palette alpha values
		T.PaletteAlphas = chunk.Data[:]
	}
	return nil
}

// ---------------------------------------------------------------------------
// zTXt chunk：zlib 压缩的文本信息 / zlib-compressed textual information
// ---------------------------------------------------------------------------

// zTXt chunk 与 tEXt 一样承载文本数据，但利用了压缩；二者语义等价，只是
// 大段文本更适合用 zTXt 存储。数据区布局：
//
//	Keyword:            1-79 bytes (character string)
//	Null separator:     1 byte
//	Compression method: 1 byte
//	Compressed text:    n bytes
//
// 关键字与空分隔符和 tEXt chunk 完全一致，注意关键字不参与压缩。压缩方法
// 字节标识该 zTXt 使用的压缩方式，目前只定义了 0（deflate/inflate）；其后
// 是构成 chunk 剩余部分的压缩数据流，压缩方法 0 的数据流遵循 zlib 数据流
// 格式。解开该数据流得到 Latin-1 文本，与写入等价 tEXt chunk 的文本相同。
// 同一个文件中可以出现任意多个 zTXt 与 tEXt chunk。
//
// The zTXt chunk contains textual data just as tEXt does, but takes
// advantage of compression; the two are semantically equivalent, zTXt
// being recommended for storing large blocks of text. A zTXt chunk
// contains:
//
//	Keyword:            1-79 bytes (character string)
//	Null separator:     1 byte
//	Compression method: 1 byte
//	Compressed text:    n bytes
//
// The keyword and null separator are exactly the same as in the tEXt
// chunk, and the keyword itself is not compressed. The compression
// method byte identifies the method used in this zTXt chunk; the only
// value presently defined is 0 (deflate/inflate). It is followed by a
// compressed datastream that makes up the remainder of the chunk; for
// compression method 0 this datastream adheres to the zlib datastream
// format. Decompression yields Latin-1 text identical to the text that
// would be stored in an equivalent tEXt chunk. Any number of zTXt and
// tEXt chunks can appear in the same file.
type ZTXT struct {
	Keyword           string
	Separator         string
	CompressionMethod uint8
	Text              string
}

// ChunkName 返回类型码 zTXt / ChunkName returns the chunk type code zTXt.
func (z *ZTXT) ChunkName() ChunkName {
	return ZTXTChunk
}

// Parse 校验并拆分 zTXt 数据区：关键字（1-79 字节）加空分隔符，再加 1 字节
// 压缩方法（只允许 0），其余为 zlib 压缩的正文。
//
// Parse validates and splits the zTXt data area: the keyword (1-79
// bytes) plus the null separator, one compression method byte (only 0
// is allowed) and the zlib-compressed text that follows.
func (z *ZTXT) Parse(chunk RawChunk) error {
	sep := bytes.IndexByte(chunk.Data, 0x00)
	if sep < 1 || sep > 79 {
		return errors.New("invalid text")
	}
	// 数据区至少要有 关键字 + 空分隔符 + 压缩方法字节
	// keyword + null + compression method + compressed text
	if len(chunk.Data) < sep+1+1 {
		return errors.New("invalid ztxt chunk data")
	}
	z.Keyword = string(chunk.Data[:sep])
	z.Separator = nullSep
	rest := chunk.Data[sep+1:]
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

// ---------------------------------------------------------------------------
// sRGB chunk：sRGB 渲染意图 / the sRGB rendering intent
// ---------------------------------------------------------------------------

// sRGB chunk 以一个字节的渲染意图（rendering intent）传递传统 sRGB 的
// gamma、色度、亮度与颜色数据。合法的渲染意图为：
//
//	0: perceptual
//	1: relative colorimetric
//	2: saturation
//	3: absolute colorimetric
//
// 一个 PNG 数据流中至多只能有一个 sRGB chunk。解码器同时遇到 iCCP 与
// sRGB chunk 时可以任选其一；本库把两者都原样保留，把取舍交给调用方。
// 若 sRGB 出现，它必须位于第一个 IDAT 之前。
//
// The sRGB chunk conveys the legacy sRGB gamma, chromaticity,
// luminance and color data through a single rendering intent byte. The
// legal rendering intents are:
//
//	0: perceptual
//	1: relative colorimetric
//	2: saturation
//	3: absolute colorimetric
//
// A PNG datastream shall contain no more than one sRGB chunk. A decoder
// encountering both iCCP and sRGB chunks is free to pick one; this
// library keeps both as-is and leaves the decision to the caller. If
// present, this chunk must precede the first IDAT chunk.
type SRGB struct {
	RenderingIntent uint8
}

// ChunkName 返回类型码 sRGB / ChunkName returns the chunk type code sRGB.
func (s *SRGB) ChunkName() ChunkName {
	return SRGBChunk
}

// Parse 读取 1 字节的渲染意图；数据区长度不等于 1 时报错。
//
// Parse reads the single rendering intent byte; any data area whose
// length is not exactly 1 is an error.
func (s *SRGB) Parse(chunk RawChunk) error {
	if chunk.Data == nil || len(chunk.Data) != 1 {
		return errors.New("invalid srgb chunk data")
	}
	s.RenderingIntent = chunk.Data[0]
	return nil
}

// ---------------------------------------------------------------------------
// iCCP chunk：内嵌 ICC 配置文件 / the embedded ICC profile
// ---------------------------------------------------------------------------

// iCCP chunk 内嵌一份 ICC 配置文件。数据区布局：
//
//	Profile name:         1-79 bytes (Latin-1) followed by a null separator
//	Compression method:   1 byte (only 0 = zlib/deflate is defined)
//	Compressed profile:   n bytes, a zlib datastream with the profile
//
// 一个 PNG 数据流中至多只能有一个 iCCP chunk。写出时该 chunk 默认逐字节
// 重现，除非 Profile 字段（或配置文件名、压缩方法）被修改，此时新的配置
// 文件会先重新压缩再写出。配置文件本身按原始 ICC 字节保留，用 Profile
// 字段访问。
//
// The iCCP chunk embeds an ICC profile. It contains:
//
//	Profile name:         1-79 bytes (Latin-1) followed by a null separator
//	Compression method:   1 byte (only 0 = zlib/deflate is defined)
//	Compressed profile:   n bytes, a zlib datastream with the profile
//
// A PNG datastream shall contain no more than one iCCP chunk. On the
// write side the chunk is reproduced byte for byte unless the Profile
// field was modified (or the profile name or the compression method
// changed), in which case the new profile is compressed again before
// writing. The profile itself is kept as raw ICC bytes; use Profile to
// access it.
type ICCP struct {
	// Name 是 Latin-1 编码的配置文件名，例如 "ICC profile"。
	// Name is the Latin-1 profile name, e.g. "ICC profile".
	Name string
	// CompressionMethod 是存储时配置文件的压缩方式；规范只定义了取值 0
	// （zlib/deflate）。
	// CompressionMethod is the compression of the profile as stored; only
	// the value 0 (zlib/deflate) is defined in the spec.
	CompressionMethod uint8
	// Profile 是解压后的 ICC 配置文件字节；修改该字段即可替换配置文件，
	// 写出时会重新压缩。
	// Profile is the decompressed ICC profile bytes; modify this field to
	// replace the profile, the chunk is re-compressed while writing.
	Profile []byte
}

// ChunkName 返回类型码 iCCP / ChunkName returns the chunk type code iCCP.
func (i *ICCP) ChunkName() ChunkName {
	return ICCPChunk
}

// Parse 读取配置文件名（1-79 字节）、校验压缩方法（只允许 0），再把 zlib
// 压缩的配置文件解压到 Profile；名称长度或数据区长度非法时报错。
//
// Parse reads the profile name (1-79 bytes), checks the compression
// method (only 0 is allowed) and inflates the zlib-compressed profile
// into Profile; an invalid name length or data length is an error.
func (i *ICCP) Parse(chunk RawChunk) error {
	sep := bytes.IndexByte(chunk.Data, 0x00)
	if sep < 1 || sep > 79 {
		return errors.New("invalid iccp profile name")
	}
	if len(chunk.Data) < sep+2 {
		return errors.New("invalid iccp chunk data")
	}
	i.Name = string(chunk.Data[:sep])
	rest := chunk.Data[sep+1:]
	i.CompressionMethod = rest[0]
	if i.CompressionMethod != 0 {
		return errors.Errorf("unsupported iccp compression method %d", i.CompressionMethod)
	}
	profile, err := zlibDecompress(rest[1:])
	if err != nil {
		return errors.WithStack(err)
	}
	i.Profile = profile
	return nil
}

// ---------------------------------------------------------------------------
// eXIf chunk：EXIF 元数据 / EXIF metadata
// ---------------------------------------------------------------------------

// eXIf chunk 保存 EXIF 元数据：一段可交换图像文件格式（TIFF）结构，字节序
// 为 II（小端）或 MM（大端），包含一个或多个图像文件目录（IFD）。规范允
// 许存在多个 eXIf chunk；本库只把第一个解析进 EXIF 结构，其余保持原始形
// 态（见 RawChunks）。TIFF 字节整体按原样保留：除非 Data 字段被修改，写
// 出时逐字节重现。结构化的只读视图见 Entries（IFD0 以及 Exif、GPS 子目
// 录都被展平）。
//
// The eXIf chunk holds EXIF metadata: an Exchangeable Image File Format
// (TIFF) structure, byte order II (little-endian) or MM (big-endian),
// with one or more image file directories (IFD). Multiple eXIf chunks
// are allowed by the spec; the library parses the FIRST one into the
// EXIF struct while the others stay raw (see RawChunks). The TIFF blob
// is kept as-is: the chunk is reproduced byte for byte unless the Data
// field was modified. Use Entries for the structured read-only view of
// the file directory (IFD0 plus the Exif and GPS sub-IFDs are
// flattened).
type EXIF struct {
	// Data 是去掉 chunk 类型字段后的 TIFF 数据流。
	// Data is the TIFF stream of the chunk without the chunk type fields.
	Data []byte
}

// ExifEntry 是 EXIF 图像文件目录（IFD）的一条目录项：标签、类型、个数，
// 以及其值的原始字节——当值放不进项本身时已按偏移解析到位。
//
// ExifEntry is one entry of an exIf IFD image directory: tag, type and
// count plus the raw bytes of its value (already resolved through the
// offset when the value does not fit into the entry itself).
type ExifEntry struct {
	Tag   uint16
	Type  uint16
	Count uint32
	Value []byte
}

// exifTypeSize 返回给定 TIFF 值类型单个元素的字节大小，未知类型返回 0。
//
// exifTypeSize returns the byte size of one element of the given TIFF
// value type, 0 for unknown types.
func exifTypeSize(t uint16) int {
	switch t {
	case 1, 2, 6, 7: // BYTE, ASCII, SBYTE, UNDEFINED
		return 1
	case 3, 8: // SHORT, SSHORT
		return 2
	case 4, 9, 11, 13: // LONG, SLONG, FLOAT, IFD
		return 4
	case 5, 10, 12: // RATIONAL, SRATIONAL, DOUBLE
		return 8
	}
	return 0
}

// ChunkName 返回类型码 eXIf / ChunkName returns the chunk type code eXIf.
func (e *EXIF) ChunkName() ChunkName {
	return EXIFChunk
}

// Parse 把 chunk 数据区的字节原样保存到 Data；真正的 TIFF 解析推迟到
// Entries。
//
// Parse stores the chunk payload verbatim in Data; the actual TIFF
// parsing is deferred to Entries.
func (e *EXIF) Parse(chunk RawChunk) error {
	e.Data = append(e.Data, chunk.Data...)
	return nil
}

// Entries 解析 chunk 的 TIFF 结构，把 IFD0 的所有目录项以及它链接的各级
// IFD 一并展平返回（指向 Exif、GPS 子目录的指针项也在内）。它不改动原始
// 字节，返回的只是只读视图。
//
// Entries parses the TIFF structure of the chunk and returns every IFD
// entry of the IFD0 plus its linked IFDs flattened (the pointer entries
// to the Exif and GPS sub-IFDs are included). The original bytes are
// not touched; the result is a read-only view.
func (e *EXIF) Entries() ([]ExifEntry, error) {
	if len(e.Data) < 8 {
		return nil, errors.New("invalid exif tiff stream")
	}
	var by binary.ByteOrder = binary.LittleEndian
	switch string(e.Data[:2]) {
	case "II":
	case "MM":
		by = binary.BigEndian
	default:
		return nil, errors.New("invalid exif byte order")
	}
	if by.Uint16(e.Data[2:4]) != 42 {
		return nil, errors.New("invalid exif tiff magic")
	}
	// 沿 IFD 链与 Exif/GPS 子目录遍历，并对畸形（甚至恶意）的偏移做防护
	// walk the IFD chain and the Exif/GPS sub-IFDs with guards against
	// malformed (or hostile) offsets
	var out []ExifEntry
	visited := map[uint32]bool{0: true}
	pending := []uint32{by.Uint32(e.Data[4:8])}
	for len(pending) > 0 {
		off := int(pending[0])
		pending = pending[1:]
		if visited[uint32(off)] || len(e.Data) < off+2 {
			continue
		}
		visited[uint32(off)] = true
		count := int(by.Uint16(e.Data[off : off+2]))
		// 每条目录项 12 字节，再加 4 字节的下一 IFD 偏移，都必须落在数据区内
		// 12 bytes per entry + 4 bytes next-IFD-offset must stay in bounds
		if count > 0x1000 || len(e.Data) < off+2+count*12+4 {
			return nil, errors.Errorf("invalid exif ifd at offset %d", off)
		}
		next := by.Uint32(e.Data[off+2+count*12 : off+6+count*12])
		if !visited[next] {
			pending = append(pending, next)
		}
		for i := 0; i < count; i++ {
			eo := off + 2 + i*12
			tag := by.Uint16(e.Data[eo : eo+2])
			typ := by.Uint16(e.Data[eo+2 : eo+4])
			cnt := by.Uint32(e.Data[eo+4 : eo+8])
			size := exifTypeSize(typ)
			if size == 0 {
				return nil, errors.Errorf("unknown exif type %d at offset %d", typ, eo)
			}
			total := size * int(cnt)
			var value []byte
			if total <= 4 {
				value = e.Data[eo+8 : eo+8+total]
			} else {
				vOff := int(by.Uint32(e.Data[eo+8 : eo+12]))
				if len(e.Data) < vOff+total {
					return nil, errors.Errorf("invalid exif value offset at tag %d", tag)
				}
				value = e.Data[vOff : vOff+total]
			}
			out = append(out, ExifEntry{Tag: tag, Type: typ, Count: cnt, Value: value})
			// 跟随规范定义的子目录指针：它们是旁支，不走下一 IFD 偏移链
			// follow the sub-IFD pointers of the spec: side chains, not
			// through the next-IFD-off offset chain
			if (tag == 0x8769 || tag == 0x8825 || tag == 0x014A) && typ == 4 && cnt == 1 {
				sub := by.Uint32(value[:4])
				if !visited[sub] {
					pending = append(pending, sub)
				}
			}
		}
	}
	return out, nil
}
