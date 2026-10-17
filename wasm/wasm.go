//go:build js && wasm

// Package wasm exposes the simple-png parser and editor to JavaScript
// through a WebAssembly build (GOOS=js GOARCH=wasm).
//
// Build:
//
//	GOOS=js GOARCH=wasm go build -o png.wasm ./cmd/pngwasm
//
// JS-side usage (browser or Node via $GOROOT/lib/wasm/wasm_exec.js):
//
//	import { Go } from "./go.js"
//	const go = new Go()
//	const buf = await (await fetch("png.wasm")).arrayBuffer()
//	const { instance } = await WebAssembly.instantiate(buf, go.importObject)
//	go.run(instance)
//	await waitFor(() => globalThis.pngOpen) // wait for the Go-side registration
//
//	globalThis.pngOpen(new Uint8Array(fileBytes)) // "" or "error: ..."
//	const sum = globalThis.pngSummary() // header + ordered chunk table (see below)
//	const c = globalThis.pngRawChunkData(0) // a leftover chunk: {name, crc, len, bytes}
//	const cs = globalThis.pngRawChunkByName("iTXt") // [{name, crc, bytes}] per occurrence
//	const px = globalThis.pngPixels() // {dtype:"rgba8", width, height, bytes}
//	const enc = globalThis.pngEncode(4, 3, rgbaBytes, [{name:"iTXt", data}])
//	globalThis.pngSetRawChunk("cust", new Uint8Array([...]))
//	globalThis.pngDeleteRawChunk("EXIF")
//	const out = globalThis.pngWrite() // re-serialized file bytes (ArrayBuffer)
//	globalThis.pngClose()
//
// pngSummary() returns:
//
//	{
//	  width, height, bitDepth, colorType, colorTypeName,
//	  compression, filter, interlace, channel, idatCount,
//	  chunks: [{name, len, crc}] // chunks not consumed by native parsing,
//	}                            // in file order (see RawChunks)
//	  txx: [{keyword, text}]     tEXt entries (consumed, decoded)
//	  itx: [{keyword, compressionFlag, languageTag, translatedKeyword, text, len}]
//	                             iTXt entries (consumed, decoded; XMP rides here)
//
// colorTypeName maps the IHDR color type to one of "grayscale", "truecolor",
// "palette", "grayscale+alpha" or "truecolor+alpha".
//
// pngEncode(width, height, rgba, extras) builds a new PNG from RGBA 8-bit
// pixels: full NRGBA alpha, extras (a JS array of {name, data}) are emitted
// right after the PNG signature, before the internal chunks. Returns the
// file bytes (ArrayBuffer) or the "error: ..." string.
//
// Convention: every fallible function returns the string "error: ..." on
// failure (matching the simple-netcdf wasm package); pngWrite hands back an
// ArrayBuffer that re-serializes the current model — untouched chunks are
// copied byte for byte, so metadata write back stays faithful. Pixel bytes
// are always RGBA with one byte per channel (dtype "rgba8").
package wasm

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"syscall/js"

	"github.com/knoweather/simple-png"
)

var cur *simple_png.Png

// Run registers the JS global functions and blocks (wasm entry point).
func Run() {
	g := js.Global()
	g.Set("pngOpen", js.FuncOf(func(_ js.Value, args []js.Value) any {
		verifyCRC := len(args) > 1 && args[1].Truthy()
		if err := Open(jsToBytes(args[0]), verifyCRC); err != nil {
			return "error: " + err.Error()
		}
		return ""
	}))
	g.Set("pngSummary", js.FuncOf(func(_ js.Value, _ []js.Value) any {
		v, err := Summary()
		if err != nil {
			return "error: " + err.Error()
		}
		return v
	}))
	g.Set("pngRawChunkData", js.FuncOf(func(_ js.Value, args []js.Value) any {
		idx := 0
		if len(args) > 0 && args[0].Type() == js.TypeNumber {
			idx = args[0].Int()
		}
		v, err := RawChunkData(idx)
		if err != nil {
			return "error: " + err.Error()
		}
		return v
	}))
	g.Set("pngRawChunkByName", js.FuncOf(func(_ js.Value, args []js.Value) any {
		v, err := RawChunkByName(args[0].String())
		if err != nil {
			return "error: " + err.Error()
		}
		return v
	}))
	g.Set("pngPixels", js.FuncOf(func(_ js.Value, _ []js.Value) any {
		v, err := Pixels()
		if err != nil {
			return "error: " + err.Error()
		}
		return v
	}))
	g.Set("pngSetRawChunk", js.FuncOf(func(_ js.Value, args []js.Value) any {
		if err := SetRawChunk(args[0].String(), jsToBytes(args[1])); err != nil {
			return "error: " + err.Error()
		}
		return ""
	}))
	g.Set("pngDeleteRawChunk", js.FuncOf(func(_ js.Value, args []js.Value) any {
		if err := DeleteRawChunk(args[0].String()); err != nil {
			return "error: " + err.Error()
		}
		return ""
	}))
	g.Set("pngWrite", js.FuncOf(func(_ js.Value, _ []js.Value) any {
		v, err := Write()
		if err != nil {
			return "error: " + err.Error()
		}
		return v
	}))
	g.Set("pngEncode", js.FuncOf(func(_ js.Value, args []js.Value) any {
		v, err := Encode(args[0].Int(), args[1].Int(), jsToBytes(args[2]), args[3])
		if err != nil {
			return "error: " + err.Error()
		}
		return v
	}))
	g.Set("pngClose", js.FuncOf(func(_ js.Value, _ []js.Value) any {
		Close()
		return ""
	}))
	select {} // the wasm program idles until the JS side drops it
}

// Open parses a PNG byte stream, replacing the previously opened file.
// CRC verification is optional and off by default, mirroring ParsePng.
func Open(data []byte, verifyCRC bool) error {
	p, err := simple_png.ParsePng(bytes.NewReader(data), verifyCRC)
	if err != nil {
		return err
	}
	cur = p
	return nil
}

// Close discards the current file.
func Close() { cur = nil }

// Summary returns the IHDR header and the ordered chunk table (JS object).
// The chunk table lists the chunks that native parsing has NOT consumed
// (the base chunks IHDR/IDAT/PLTE/... live in their typed fields);
// idatCount is the number of IDAT chunks.
func Summary() (js.Value, error) {
	if cur == nil {
		return js.Undefined(), fmt.Errorf("no file open")
	}
	var header simple_png.IHDR
	if cur.IHDR != nil {
		header = *cur.IHDR
	}
	obj := newObject()
	obj.Set("width", int(header.Width))
	obj.Set("height", int(header.Height))
	obj.Set("bitDepth", int(header.BitDepth))
	obj.Set("colorType", int(header.ColorType))
	obj.Set("colorTypeName", colorTypeName(header.ColorType))
	obj.Set("compression", int(header.CompressionMethod))
	obj.Set("filter", int(header.FilterMethod))
	obj.Set("interlace", int(header.InterlaceMethod))
	obj.Set("channel", cur.Channel)
	obj.Set("idatCount", len(cur.IDATs))

	chunks := newArray()
	for i, rc := range cur.RawChunks() {
		o := newObject()
		o.Set("name", string(rc.Name))
		o.Set("len", len(rc.Data))
		o.Set("crc", float64(rc.CRC))
		chunks.SetIndex(i, o)
	}
	obj.Set("chunks", chunks)

	// Consumed text metadata, decoded — XMP rides inside iTXt:
	txx := newArray()
	for i, t := range cur.TEXTs {
		o := newObject()
		o.Set("keyword", t.Keyword)
		o.Set("text", t.Text)
		txx.SetIndex(i, o)
	}
	obj.Set("txx", txx)
	itx := newArray()
	for i, t := range cur.ITXTs {
		o := newObject()
		o.Set("keyword", t.Keyword)
		o.Set("compressionFlag", int(t.CompressionFlag))
		o.Set("languageTag", t.LanguageTag)
		o.Set("translatedKeyword", t.TranslatedKeyword)
		o.Set("text", t.Text)
		o.Set("len", len(t.Data))
		itx.SetIndex(i, o)
	}
	obj.Set("itx", itx)
	return obj, nil
}

// RawChunkData returns one raw chunk from the ordered RawChunks list.
// Returns {name, crc, len, bytes}.
func RawChunkData(index int) (js.Value, error) {
	if cur == nil {
		return js.Undefined(), fmt.Errorf("no file open")
	}
	rcs := cur.RawChunks()
	if index < 0 || index >= len(rcs) {
		return js.Undefined(), fmt.Errorf("chunk index %d out of range (0..%d)", index, len(rcs)-1)
	}
	rc := rcs[index]
	obj := newObject()
	obj.Set("name", string(rc.Name))
	obj.Set("crc", float64(rc.CRC))
	obj.Set("len", len(rc.Data))
	obj.Set("bytes", bytesToJS(rc.Data))
	return obj, nil
}

// RawChunkByName returns every occurrence of a chunk type as
// [{name, crc, bytes}]; an unknown type yields an empty array (not an error,
// matching RawChunks as the simplest read path).
func RawChunkByName(name string) (js.Value, error) {
	if cur == nil {
		return js.Undefined(), fmt.Errorf("no file open")
	}
	arr := newArray()
	for _, rc := range cur.RawChunks() {
		if string(rc.Name) != name {
			continue
		}
		o := newObject()
		o.Set("name", string(rc.Name))
		o.Set("crc", float64(rc.CRC))
		o.Set("bytes", bytesToJS(rc.Data))
		arr.SetIndex(arr.Length(), o)
	}
	return arr, nil
}

// Pixels decodes the image data (IDATsToColor) and returns it as RGBA 8-bit
// bytes: {dtype:"rgba8", width, height, bytes}. Every color type (including
// palette and 16-bit images) is converted through image.(*NRGBA).
func Pixels() (js.Value, error) {
	if cur == nil {
		return js.Undefined(), fmt.Errorf("no file open")
	}
	img, err := cur.IDATsToColor()
	if err != nil {
		return js.Undefined(), err
	}
	w, h := cur.Width, cur.Height
	if b := img.Bounds(); b.Dx() > w || b.Dy() > h {
		w, h = b.Dx(), b.Dy()
	}
	n := image.NewNRGBA(image.Rect(0, 0, w, h))
	// draw handles every supported image.Image type, palette and 16-bit included;
	// zero-alpha NRGBA pixels (when img covers a smaller area) stay transparent.
	draw.Draw(n, n.Bounds(), img, img.Bounds().Min, draw.Src)
	obj := newObject()
	obj.Set("dtype", "rgba8")
	obj.Set("width", w)
	obj.Set("height", h)
	// NewNRGBA gives Stride == 4*width exactly, so Pix is one dense buffer.
	obj.Set("bytes", bytesToJS(n.Pix))
	return obj, nil
}

// SetRawChunk adds a raw chunk to the model (mirrors Png.AddRawChunk).
func SetRawChunk(name string, data []byte) error {
	if cur == nil {
		return fmt.Errorf("no file open")
	}
	return cur.AddRawChunk(simple_png.ChunkName(name), data)
}

// DeleteRawChunk removes every raw chunk of the given type
// (mirrors Png.DeleteRawChunk). Only raw/leftover chunks are removable —
// chunks consumed into typed fields (IHDR/IDAT/tEXt/iTXt/...) are edited
// through their typed structs, so text metadata simply survives write back.
func DeleteRawChunk(name string) error {
	if cur == nil {
		return fmt.Errorf("no file open")
	}
	cur.DeleteRawChunk(simple_png.ChunkName(name))
	return nil
}

// Write re-serializes the current model into file bytes (ArrayBuffer).
// Untouched raw chunks are copied byte for byte; lengths, padding and CRCs
// are recomputed by the library.
func Write() (js.Value, error) {
	if cur == nil {
		return js.Undefined(), fmt.Errorf("no file open")
	}
	var buf bytes.Buffer
	if _, err := cur.WriteTo(&buf); err != nil {
		return js.Undefined(), err
	}
	return bytesToJS(buf.Bytes()), nil
}

// Encode builds a new PNG from RGBA 8-bit pixel bytes (width*height*4).
// extras is a JS array of {name, data} chunks emitted right after the PNG
// signature (any 4 byte type except IHDR/IDAT/IEND); returns the file bytes
// as an ArrayBuffer.
func Encode(width, height int, rgba []byte, extras js.Value) (js.Value, error) {
	if width <= 0 || height <= 0 {
		return js.Undefined(), fmt.Errorf("invalid size %dx%d", width, height)
	}
	if len(rgba) < width*height*4 {
		return js.Undefined(), fmt.Errorf("rgba payload too small: %d < %d", len(rgba), width*height*4)
	}
	img := &image.NRGBA{
		Pix:    rgba[:width*height*4],
		Stride: width * 4,
		Rect:   image.Rect(0, 0, width, height),
	}
	var rcExtras []simple_png.RawChunk
	if extras.Type() == js.TypeObject && !extras.IsNull() && !extras.IsUndefined() {
		n := extras.Get("length")
		if n.Type() != js.TypeNumber {
			return js.Undefined(), fmt.Errorf("extras must be an array of {name, data}")
		}
		for i := 0; i < n.Int(); i++ {
			e := extras.Index(i)
			name := e.Get("name")
			data := e.Get("data")
			if name.Type() != js.TypeString || data.IsNull() || data.IsUndefined() {
				return js.Undefined(), fmt.Errorf("extras[%d] must be {name, data}", i)
			}
			rcExtras = append(rcExtras, simple_png.RawChunk{
				Name: simple_png.ChunkName(name.String()),
				Data: jsToBytes(data),
			})
		}
	}
	var buf bytes.Buffer
	if _, err := simple_png.Encode(img, &buf, rcExtras...); err != nil {
		return js.Undefined(), err
	}
	return bytesToJS(buf.Bytes()), nil
}

// colorTypeName maps an IHDR color type code to a readable name.
func colorTypeName(t uint8) string {
	switch t {
	case 0:
		return "grayscale"
	case 2:
		return "truecolor"
	case 3:
		return "palette"
	case 4:
		return "grayscale+alpha"
	case 6:
		return "truecolor+alpha"
	}
	return "unknown"
}

// ---- JS value helpers ----

func newObject() js.Value { return js.Global().Get("Object").New() }
func newArray() js.Value  { return js.Global().Get("Array").New() }

// bytesToJS wraps a byte slice into a fresh Uint8Array and hands back the
// underlying ArrayBuffer (Go 1.27 CopyBytesToJS accepts only Uint8Array).
func bytesToJS(b []byte) js.Value {
	ua := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(ua, b)
	return ua.Get("buffer")
}

// copyBuf returns a defensive copy of b — WriteTo's buffer stays ours.
func copyBuf(b []byte) js.Value { return bytesToJS(b) }

// jsToBytes copies bytes out of a JS ArrayBuffer or TypedArray. Defensive
// copy: the Go side never borrows JS memory.
func jsToBytes(v js.Value) []byte {
	UA := js.Global().Get("Uint8Array")
	var src js.Value
	if buf := v.Get("buffer"); buf.Type() == js.TypeObject && !buf.IsNull() {
		// TypedArray: keep the sub-view semantics via byteOffset/byteLength.
		src = UA.New(buf, v.Get("byteOffset").Int(), v.Get("byteLength").Int())
	} else {
		src = UA.New(v) // ArrayBuffer → whole view
	}
	b := make([]byte, src.Get("length").Int())
	js.CopyBytesToGo(b, src)
	return b
}
