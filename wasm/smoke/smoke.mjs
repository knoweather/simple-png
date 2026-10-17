// wasm/smoke/smoke.mjs — wasm interface smoke test (Node, no browser).
//
// Run from the repo root:
//
//	node wasm/smoke/smoke.mjs
//
// The script resolves the Go toolchain itself:
//   - GO_BIN_DIR env var wins; the default is
//     %TEMP%\opencode\gotool\go\bin (Windows) or /usr/local/go/bin,
//     falling back to PATH's go.
//
// Flow: GOOS=js GOARCH=wasm go build -o <tmp>/png.wasm ./cmd/pngwasm
//   → load $GOROOT/lib/wasm/wasm_exec.js in Node → boot the wasm binary
//   → assert the JS API end to end: open fixture → summary → pixels →
//   encode extras → edit chunk → write back → re-open written bytes.
//   Any failed assert exits non-zero.

import fs from "node:fs";
import path from "node:path";
import os from "node:os";
import vm from "node:vm";
import crypto from "node:crypto";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { TextEncoder, TextDecoder } from "node:util";

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, "..", ".."); // repo root

// ---- 1. Go toolchain ----
const DEFAULT_GO_BIN =
	process.platform === "win32"
		? path.join(os.tmpdir(), "opencode", "gotool", "go", "bin")
		: "/usr/local/go/bin";
const GO_BIN = process.env.GO_BIN_DIR || DEFAULT_GO_BIN;

let goExe = "go";
const goCand = path.join(GO_BIN, process.platform === "win32" ? "go.exe" : "go");
if (fs.existsSync(goCand)) goExe = goCand;

const goEnv = {
	...process.env,
	PATH: GO_BIN + path.delimiter + (process.env.PATH || ""),
	CGO_ENABLED: "0",
	GOOS: "js",
	GOARCH: "wasm",
	GOTOOLCHAIN: "local",
};

// ---- 2. Build the wasm binary ----
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "pngwasm-smoke-"));
const wasmPath = path.join(tmp, "png.wasm");
console.log("[smoke] go build ->", wasmPath);
execFileSync(goExe, ["build", "-o", wasmPath, "./cmd/pngwasm"], {
	cwd: root,
	env: goEnv,
	stdio: "inherit",
});

// ---- 3. Locate wasm_exec.js ----
const goroot = execFileSync(goExe, ["env", "GOROOT"], { cwd: root, env: goEnv })
	.toString()
	.trim();
let wasmExec = path.join(goroot, "lib", "wasm", "wasm_exec.js");
if (!fs.existsSync(wasmExec)) {
	wasmExec = path.join(goroot, "misc", "wasm", "wasm_exec.js");
}
if (!fs.existsSync(wasmExec)) {
	throw new Error(`cannot find wasm_exec.js (tried $GOROOT/lib/wasm and $GOROOT/misc/wasm), GOROOT=${goroot}`);
}
console.log("[smoke] wasm_exec.js ->", wasmExec);

// ---- 4. Globals wasm_exec.js expects in Node ----
// Deliberately keep globalThis.fs untouched: wasm_exec.js installs a
// synchronous fs shim (browser-like).
globalThis.TextEncoder = TextEncoder;
globalThis.TextDecoder = TextDecoder;
globalThis.performance ??= performance;
globalThis.crypto ??= crypto;

// wasm_exec.js is a plain script (IIFE); evaluate it in the global context.
vm.runInThisContext(fs.readFileSync(wasmExec, "utf8"), { filename: wasmExec });

// ---- 5. Boot the wasm binary ----
const go = new Go();
const { instance } = await WebAssembly.instantiate(fs.readFileSync(wasmPath), go.importObject);
go.run(instance).catch((err) => {
	console.error("[smoke] go.run error:", err);
	process.exit(1);
});

async function waitFor(fn, timeoutMs = 15000) {
	const t0 = Date.now();
	while (Date.now() - t0 < timeoutMs) {
		if (fn()) return;
		await new Promise((r) => setTimeout(r, 10));
	}
	throw new Error("timeout waiting for Go-side global registration");
}

// ---- 6. Assert helpers ----
const must = (cond, msg) => {
	if (!cond) throw new Error(msg);
};

async function main() {
	await waitFor(() => typeof globalThis.pngOpen === "function");
	for (const fn of ["pngSummary", "pngRawChunkData", "pngRawChunkByName", "pngPixels", "pngEncode", "pngSetRawChunk", "pngDeleteRawChunk", "pngWrite", "pngClose"]) {
		must(typeof globalThis[fn] === "function", `globalThis.${fn} not registered`);
	}

	// ---- real fixture: decode + header ----
	const file = path.join(root, "png-format.png");
	const r = globalThis.pngOpen(new Uint8Array(fs.readFileSync(file)));
	must(r === "", `pngOpen failed: ${r}`);

	const sum = globalThis.pngSummary();
	must(sum.width === 575 && sum.height === 1083, `summary size ${sum.width}x${sum.height}, want 575x1083`);
	must(sum.idatCount === 131, `idatCount should be 131, got ${sum.idatCount}`);
	// this fixture has IHDR + 131 IDAT + IEND (+ sRGB, consumed into its typed
	// struct) → the leftover raw list is EMPTY, which is a valid state:
	must(Array.isArray(sum.chunks) && sum.chunks.length === 0, `leftover chunks should be [], got ${sum.chunks.length}`);
	console.log("[smoke] fixture ok:", `colorType=${sum.colorType}(${sum.colorTypeName}) bitDepth=${sum.bitDepth} idats=${sum.idatCount}`);

	const px = globalThis.pngPixels();
	must(px.dtype === "rgba8", `pixels dtype ${px.dtype}`);
	must(px.width === 575 && px.height === 1083, `pixels size ${px.width}x${px.height}`);
	must(px.bytes.byteLength === 575 * 1083 * 4, `pixels byteLength ${px.bytes.byteLength}, want ${575 * 1083 * 4}`);

	// error convention: calls with no file open
	globalThis.pngClose();
	must(String(globalThis.pngSummary()).startsWith("error: "), "pngSummary with no file open should return 'error: ...'");
	must(String(globalThis.pngRawChunkData(0)).startsWith("error: "), "pngRawChunkData with no file open should error");

	// ---- encode: 4x3 RGBA + metadata extras (iTXt + custom chunk) ----
	const w = 4, h = 3;
	const rgba = new Uint8Array(w * h * 4);
	for (let i = 0; i < w * h; i++) {
		rgba[4 * i] = (i * 37) & 0xff;
		rgba[4 * i + 1] = (i * 59) & 0xff;
		rgba[4 * i + 2] = 200;
		rgba[4 * i + 3] = 255;
	}
	const xmpText = "<xmp>smoke</xmp>";
	const enc = globalThis.pngEncode(
		w, h, rgba,
		[{ name: "iTXt", data: xmpBytes(xmpText) }, { name: "cust", data: new Uint8Array([1, 2, 3, 4]) }]
	);
	must(enc instanceof ArrayBuffer && enc.byteLength > 0, "pngEncode should return file bytes");

	// ---- parse our own encode output; iTXt is consumed into summary.itx,
	// the unknown "cust" stays in the leftover raw list ----
	const reopenErr = globalThis.pngOpen(new Uint8Array(enc));
	if (reopenErr !== "") {
		throw new Error(`pngOpen on encoded bytes failed: ${reopenErr}`);
	}
	const sum2 = globalThis.pngSummary();
	must(sum2.width === w && sum2.height === h, `re-parsed size ${sum2.width}x${sum2.height}`);
	const names2 = sum2.chunks.map((c) => c.name);
	must(names2.includes("cust"), `leftover chunks should carry the custom chunk, got [${names2.join()}]`);
	// iTXt is consumed into its typed struct → visible via summary.itx:
	must(Array.isArray(sum2.itx) && sum2.itx.length === 1, `summary.itx should carry the iTXt, got ${sum2.itx.length}`);
	must(sum2.itx[0].keyword === "XML:com.adobe.xmp" && sum2.itx[0].text.includes("smoke"), "iTXt payload should survive encoding");

	// ---- edit + write back + re-open round trip ----
	// Note: only raw/leftover chunks are deletable; a consumed iTXt is
	// edited through its typed struct and simply survives write back.
	must(globalThis.pngSetRawChunk("cst2", new Uint8Array([9, 8, 7])) === "", "pngSetRawChunk failed");
	const out = globalThis.pngWrite();
	must(out instanceof ArrayBuffer && out.byteLength > 0, "pngWrite should return a non-empty ArrayBuffer");
	globalThis.pngClose();

	must(globalThis.pngOpen(new Uint8Array(out)) === "", "re-opening written bytes failed");
	const sum3 = globalThis.pngSummary();
	const names3 = sum3.chunks.map((c) => c.name);
	must(names3.includes("cust") && names3.includes("cst2"), `written file should carry cust+cst2, got [${names3.join()}]`);
	must(sum3.itx.length === 1 && sum3.itx[0].text.includes("smoke"), "consumed iTXt should survive write back unchanged");
	const cust = globalThis.pngRawChunkByName("cust");
	must(cust.length === 1 && new Uint8Array(cust[0].bytes).every((v, i) => v === i + 1), "cust payload should round-trip byte-exact");

	// deleting the raw cust, write again: cst2 remains, iTXt untouched
	must(globalThis.pngDeleteRawChunk("cust") === "", "pngDeleteRawChunk failed");
	const out2 = globalThis.pngWrite();
	globalThis.pngClose();

	must(globalThis.pngOpen(new Uint8Array(out2)) === "", "re-opening second written file failed");
	const sum4 = globalThis.pngSummary();
	const names4 = sum4.chunks.map((c) => c.name);
	must(!names4.includes("cust") && names4.includes("cst2"), `second write should drop cust but keep cst2, got [${names4.join()}]`);
	must(sum4.itx.length === 1 && sum4.itx[0].text.includes("smoke"), "iTXt must not be touched by raw-chunk deletion");
	// pixels must survive every round trip untouched
	const px2 = globalThis.pngPixels();
	must(px2.width === w && px2.height === h && px2.bytes.byteLength === w * h * 4, "pixels should decode after the round trips");
	const rgba2 = new Uint8Array(px2.bytes);
	for (let i = 0; i < w * h * 4; i++) {
		must(rgba2[i] === rgba[i], `pixel byte ${i} changed after round trip`);
	}
	globalThis.pngClose();

	console.log("[smoke] all asserts passed OK");
}

// xmpBytes builds a minimal iTXt chunk payload: keyword NUL, compression flag
// (0 = uncompressed), compression method (0), null separator, language tag
// NUL (empty), translated keyword NUL (empty), then the text — the exact
// layout NewITXTChunk produces for empty language/translated fields.
function xmpBytes(text) {
	const enc = new TextEncoder();
	const kw = enc.encode("XML:com.adobe.xmp");
	const txt = enc.encode(text);
	const out = [];
	out.push(...kw);
	out.push(0x00, 0x00, 0x00, 0x00, 0x00, 0x00); // NUL, flag, method, sep-NUL, lang done, translated-NUL... (all zero here)
	out.push(...txt);
	return new Uint8Array(out);
}

main()
	.then(() => process.exit(0))
	.catch((err) => {
		console.error("[smoke] failed:", err && err.stack ? err.stack : err);
		process.exit(1);
	});
