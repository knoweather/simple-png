# Simple-png

### pure go png parse

### [PNG (Portable Network Graphics) Specification](https://www.w3.org/TR/PNG-Chunks.html)

## Install

```
go get github.com/XC-Zero/simple-png
```

and import it:

```go

	import "github.com/XC-Zero/simple-png"

```

## Concurrency

Parsing with separate `Png` instances (one `ParsePng` per goroutine) is fully
concurrency safe. On a single instance, the readers (`RawChunks`, `RawChunkByName`,
`GetOtherChunkByName`, `IDATsToColor`, `WriteTo`) and the editing methods
(`ParseChunk`, `AddRawChunk`, `DeleteRawChunk`, `ReplaceImage`) are guarded by an
internal RWMutex, so mixed concurrent read/write use of one instance is also
safe. One rule to remember: the `Parse` callback of a custom chunk passed to
`ParseChunk` must not call back into the same `Png` instance (an RWMutex is not
reentrant). Everything is verified with `go test -race` stress tests.

```go

	open, err := os.Open("./png-format.png")
	if err != nil {
		panic(err)
	}
	// CRC-32 verification of each chunk is optional and off by default,
	// pass true as a second argument to opt in
	p, err := ParsePng(open)
	if err != nil {
		panic(err)
	}
	// print png image data
	log.Println(*p.IDATs[0])
	// print png addition text
	log.Println(*p.TEXTs[0])

	// decode the image (idat -> unfiltered pixels -> standard image.Image)
	// Adam7 interlaced pngs are supported
	img, err := p.IDATsToColor()
	if err != nil {
		panic(err)
	}
	log.Println(img.Bounds(), img.ColorModel())

	// metadata: tEXt / zTXt / iTXt (e.g. XMP) / tIME / pHYs / tRNS / bKGD ...
	for _, x := range p.ITXTs {
		if x.Keyword == "XML:com.adobe.xmp" {
			log.Println("XMP:", x.Text)
		}
	}

	// chunks the library does not decode natively yet (eXIf, sRGB, iCCP...)
	// are exposed in raw form
	for _, rc := range p.RawChunks() {
		log.Println(rc.Name, len(rc.Data))
	}
```


### EDIT: modify metadata and write the png back

```go

	open, err := os.Open("./png-format.png")
	if err != nil {
		panic(err)
	}
	p, err := ParsePng(open)
	if err != nil {
		panic(err)
	}

	// editing is done through the parsed structs, e.g. append an XMP block
	// as an iTXt chunk (must be inserted before the first IDAT)
	name, payload, err := NewITXTChunk("XML:com.adobe.xmp", "", "", xmpString, true)
	if err != nil {
		panic(err)
	}
	if err = p.AddRawChunk(name, payload); err != nil {
		panic(err)
	}
	// drop the eXIf we do not want anymore
	p.DeleteRawChunk("eXIf")
	// update the tIME
	p.TIME.Year = 2026

	// write back: untouched chunks are copied byte for byte, edited ones are
	// re-serialized, every chunk crc is recomputed
	var buf bytes.Buffer
	if _, err = p.WriteTo(&buf); err != nil {
		panic(err)
	}
	_ = buf.Bytes()
```


### ENCODE: create png streams from standard library images

```go

	// default settings: adaptive filter heuristic + zlib default level
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if _, err = simple_png.Encode(img, &buf); err != nil {
		panic(err)
	}

	// full control: zlib level (CompressionNone/1..9), filter strategy and
	// IDAT chunk size, plus ancillary chunks written before the first IDAT
	enc := &simple_png.Encoder{
		CompressionLevel: 9,
		FilterStrategy:   simple_png.FilterHeuristic,
		ChunkSize:        0,
	}
	name, payload, err := simple_png.NewITXTChunk("XML:com.adobe.xmp", "", "", xmp, true)
	if err != nil {
		panic(err)
	}
	if _, err = enc.Encode(img, &buf,
		simple_png.RawChunk{Name: simple_png.PHYSChunk, Data: phys},
		simple_png.RawChunk{Name: name, Data: payload},
	); err != nil {
		panic(err)
	}

	// or replace the pixels of an already parsed file (the color type and
	// bit depth must match) while keeping ALL its metadata chunks
	if err = p.ReplaceImage(newImg); err != nil {
		panic(err)
	}
	var out bytes.Buffer
	if _, err = p.WriteTo(&out); err != nil {
		panic(err)
	}
	```


### You also can custom chunk parse~

```go

    type CustomChunkParse struct {
	  // your struct 	
    }
    
    func (c *CustomChunkParse) ChunkName() ChunkName {
      return ChunkName("cust")
    }
    
    func (c *CustomChunkParse) Parse(chunk *chunk) error {
      // your custom parse
      return nil
    }
    
    func TestCustomChunk(t *testing.T) {
      open, err := os.Open("./png-format.png")
      if err != nil {
        panic(err)
      }
      p, err := ParsePng(open)
      if err != nil {
        panic(err)
      }
      c := &CustomChunkParse{}
      err = p.ParseChunk(c)
      if err != nil {
        panic(err)
       }
    }


```  


---  
# Png Struct   

  
  ![png-format.png](png-format.png)
   

---  
## Benchmark

compare with the standard library `image/png` (benchtime 2s, win/amd64, sha same machine):
  
| | simple_png | std lib |
|---|---|---|
| demo.png (256×81 rgb) **decode** | 277 µs | 205 µs |
| png-format.png (575×1083 rgba, 131 idat chunks) **decode** | **24.7 ms** | 25.2 ms |
| png-format.png **ParsePng only** (chunks + metadata, no pixels) | **0.66 ms** | not possible |
| 575×1083 rgba **encode** | 52.0 ms | 37.0 ms |

`IDATsToColor` decodes the standard image types (`*image.Gray`, `*image.Gray16`, `*image.RGBA`, `*image.RGBA64`, `*image.NRGBA`, `*image.NRGBA64`, `*image.Paletted`, matching the jpeg/png standard decoder conventions), so the result is a fully compliant `image.Image` and works with `draw` or any pipeline that accepts `image.Image`.

  
---  
## TODO:
 - dedicated structs for eXIf / iCCP / sRGB chunks (they are exposed raw meanwhile)
 - Adam7 interlace while encoding
 - FilterNone + zlib "no compression" fast path benchmark tuning