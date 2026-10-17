//go:build js && wasm

// Command pngwasm — the simple-png WebAssembly entry point
// (GOOS=js GOARCH=wasm).
//
//	$ GOOS=js GOARCH=wasm go build -o png.wasm ./cmd/pngwasm
//
// Once started, the following functions register on globalThis; full usage
// in the wasm package docs (wasm/wasm.go):
//
//	pngOpen(bytes [, verifyCRC]) open a PNG byte stream; "" or "error: ..."
//	pngSummary()                 header + ordered leftover chunk table
//	pngRawChunkData(index)       one leftover chunk: {name, crc, len, bytes}
//	pngRawChunkByName(name)      every occurrence: [{name, crc, bytes}]
//	pngPixels()                  image data: {dtype:"rgba8", width, height, bytes}
//	pngEncode(w, h, rgba, extras) new PNG from RGBA pixels + {name,data} extras
//	pngSetRawChunk(name, bytes)  add a raw chunk; "" or "error: ..."
//	pngDeleteRawChunk(name)      remove a raw (leftover) chunk; "" or "error: ..."
//	pngWrite()                   re-serialized file bytes (ArrayBuffer)
//	pngClose()                   discard the current file
package main

import "github.com/knoweather/simple-png/wasm"

func main() {
	wasm.Run()
}
