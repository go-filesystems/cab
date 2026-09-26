# cab

A pure-Go reader for the **Microsoft Cabinet** (`.cab`) format, presenting a
cabinet as a [`filesystem.Filesystem`](https://github.com/go-filesystems/interface).

No cgo, no dependencies outside the standard library and
`go-filesystems/interface`. MSZIP *is* raw DEFLATE, and `compress/flate` is
exactly the decoder it asks for.

```go
f, err := os.Open("driver.cab")
st, _ := f.Stat()

fs, err := cab.OpenReader(f, st.Size())   // filesystem.Filesystem, error
if err != nil {
    return err
}
defer fs.Close()

data, err := fs.ReadFile("sub/dir/readme.txt")
```

`OpenReader(r io.ReaderAt, size int64) (filesystem.Filesystem, error)` is the
shape every driver in this org answers to, so
[`go-filesystems/detect`](https://github.com/go-filesystems/detect) can open them
all through one function type. `cab.Open` is the same call returning the concrete
`*FS`, for the cabinet-specific accessors below.

`*FS` also implements the optional `filesystem.Opener`, so a caller that must
answer a 4 KiB request out of a 4 GiB member can:

```go
if o, ok := fs.(filesystem.Opener); ok {
    h, err := o.OpenFile("big.bin")   // reads nothing
    defer h.Close()
    n, err := h.ReadAt(buf, off)      // io.ReaderAt, to the letter
}
```

Beyond the interface: `SetID`, `CabinetIndex`, `Version`, `PrevCabinet`,
`PrevDisk`, `NextCabinet`, `NextDisk`, `Reserved`, `FolderCount`,
`Compression(path)`, `Attributes(path)`, `ModTime(path)`.

A cabinet is read-only. `WriteFile`, `MkDir`, `DeleteFile`, `DeleteDir` and
`Rename` return `ErrReadOnly`.

## Scope: None and MSZIP are read, Quantum and LZX are not

A `CFFOLDER` names one of four compression methods, and this package reads two:

| `typeCompress` | method | read here |
|---|---|---|
| 0 | None (stored) | **yes** |
| 1 | MSZIP (`CK` + raw DEFLATE) | **yes** |
| 2 | Quantum | no — refused by name |
| 3 | LZX | no — refused by name |

Quantum and LZX are out of scope **as a decision, not an omission**. Each is a
substantial algorithm in its own right — LZX especially, with tree-delta coding,
aligned-offset blocks and E8 call-offset translation — and nothing in this
ecosystem produces or consumes them: the cabinets in circulation here come from
`gcab` and from `makecab`'s default, which write MSZIP or store. A cabinet using
either is refused at `OpenReader` with `ErrCompressionUnsupported`, and the
message names the method **and the folder it was found in**:

```
cab: folder 0 uses LZX, which is a complete algorithm this package does not
implement: cab: compression method not read by this package
```

## Three sentences this package keeps apart

A reader that answers "bad file" to everything leaves its caller nothing to
decide with, so the failures are divided by what a caller can act on.

| sentence | sentinels | what it means |
|---|---|---|
| *I do not know what this is* | `ErrNotCabinet`, `ErrUnknownCompression` | the magic is not `MSCF`, or a `typeCompress` the format does not define. Try another reader. |
| *I know exactly what this is and do not read it yet* | `ErrCompressionUnsupported`, `ErrSpansCabinets` | Quantum or LZX; or a member continued from/into another cabinet of the set, which one `io.ReaderAt` cannot reach. The file is fine; this package is the limit. |
| *this file is broken* | `ErrTruncated`, `ErrCorrupt` | a structure runs past the end; a field, a checksum or a DEFLATE stream contradicts the rest. Nothing will read it. |

Plus the ordinary path errors: `ErrNotFound` (which satisfies
`errors.Is(err, io/fs.ErrNotExist)`, the contract every driver in this org owes
its callers), `ErrNotDirectory`, `ErrNotRegular`, `ErrNotSymlink`, `ErrReadOnly`,
`ErrNegativeOffset`.

An I/O error from the underlying reader is returned **as itself**, never
relabelled `ErrCorrupt`: a caller told the cabinet is broken when the disk is
what failed goes looking in the wrong place.

## Three traps, and the tests that would catch each

### The reserve sizes change the size of every later structure

`cfhdrRESERVE_PRESENT` (flags `0x0004`) is followed by `cbCFHeader`,
`cbCFFolder` and `cbCFData`, and each inserts an opaque area into **every**
instance of the structure it names — after the header's fixed fields, after
*every* `CFFOLDER`, and, for `cbCFData`, between *every* `CFDATA` header and its
compressed bytes. A reader that skips the header's own area and forgets the
other two lands one field out for the rest of the file.

⚠ **A single-folder cabinet cannot detect a reader that ignores `cbCFFolder`.**
The area sits after the last structure that uses it, and `coffFiles` is
absolute, so nothing moves. The first version of `testdata/reserve.cab` had one
folder, and that ablation passed — the finding is in the ablation table below.
It now has two.

### MSZIP history crosses block boundaries

A folder's uncompressed stream is the concatenation of its `CFDATA` blocks'
output, and under MSZIP the DEFLATE history carries **across** those blocks:
block *N* may reference bytes emitted by block *N−1*. `flate.NewReaderDict`,
seeded with exactly the preceding 32 KiB of folder output, is the remedy.

⚠ **Size is not the property that makes a fixture see this — the writer is.**
`testdata/mszip.cab` is 250 KB over eight blocks, and it decodes perfectly with
the window thrown away, because `gcab`'s compressor never emits a back-reference
into the preceding block. `testdata/history.cab` does, and there block 1 will
not inflate at all without the dictionary.

### The checksum's trailing bytes are not folded little-endian

`CFDATA.csum` is a 32-bit XOR of the block's little-endian words, but the
trailing 1–3 bytes are folded **high byte first** — the specification's
reference code walks them with a pointer, so the first leftover byte takes the
highest position. Folding them little-endian instead agrees for every `cbData`
that is ≡ 0 or 1 (mod 4), which was 22 of the 41 blocks in this corpus.

A recorded `csum` of 0 means "not computed" and is not compared; a non-zero one
is verified.

## Random access costs what the format costs

A folder is a **stream**. Under MSZIP, reaching byte *n* means inflating the *n*
bytes before it, because the history window makes no block independently
decodable. A handle keeps its decoder and its position, so reading forwards is
linear overall while reading **backwards** starts a fresh pass from the start of
the folder. Under None there is no history, so a handle restarts at the block
containing the offset and reads at most one block it does not need.

`Opener` is implemented anyway rather than withheld, because the alternative for
a caller serving small requests is `ReadFile`, which holds the whole member in
memory. `ReadAt` is safe to call concurrently, as `io.ReaderAt` requires.

## Names

A `CFFILE` name is NUL-terminated and uses **backslash** as its separator; this
package converts to `/` and cleans the result, so `sub\deep\file.txt` is reached
as `sub/deep/file.txt`. A cabinet holds no directory entries at all, so the
directories a listing needs are synthesised from the paths that are there.

The encoding is chosen by one attribute bit: with `_A_NAME_IS_UTF` (`0x80`) the
name is UTF-8 — and is rejected if it is not valid UTF-8 — and without it,
Windows-1252. The five byte values Windows-1252 leaves undefined (`0x81`,
`0x8D`, `0x8F`, `0x90`, `0x9D`) decode to U+FFFD.

## Fixtures, and the readers that vouch for them

Every fixture's **premise** is asserted before anything judges the decoder: the
method actually recorded in the `CFFOLDER` is read back from the file, and the
payload is large enough that a folder spans several `CFDATA` blocks. A writer
asked for MSZIP stores a small member anyway, and a corpus of small members
passes every method while only None ever runs.

| fixture | written by | folders | asserted by |
|---|---|---|---|
| `mszip.cab` | `gcab -c -z` | 1 × MSZIP, 8 blocks | 7-Zip 26.03, cabextract 1.11, bsdtar |
| `store.cab` | `gcab -c` | 1 × None, 8 blocks | 7-Zip; and it is the byte-for-byte reference the MSZIP decode is compared against |
| `paths.cab` | `gcab -c -z` | 1 × MSZIP | backslash paths and a UTF-8-flagged name |
| `reserve.cab` | crafted | 2 (MSZIP + None), all three reserve areas | gcab 1.6, cabextract 1.11 |
| `history.cab` | crafted, `flate.NewWriterDict` | 1 × MSZIP, 8 blocks, genuine cross-block references | gcab 1.6, 7-Zip 26.03, libarchive 3.7.4 |

Two disagreements between reference readers turned up while these were being
checked, and both are recorded here because a reader that trusts a single
witness would have drawn the wrong conclusion from either:

* **7-Zip 26.03** reports `Data Error` on any cabinet carrying a per-`CFDATA`
  reserve area. The specification puts `abReserve` between `cbUncomp` and
  `ab[]`; gcab and cabextract both read it that way, and so does this package.
* **cabextract 1.11 (libmspack)** stops after block 0 with `decompression
  error` on any cabinet whose MSZIP blocks reference the preceding block. gcab,
  7-Zip and libarchive all read those correctly.

Fixtures are embedded with `//go:embed`, because the emulated CI lanes run a
`go test -c` binary in a container with no `testdata/`. Verify with:

```
go test -c -o /tmp/t.test . && cd /tmp && ./t.test
```

## Ablation table

For every decision the reader makes, the test suite **undoes** it and checks that
the corpus notices. An ablation that *passes* is the finding: it says the fixture
cannot see the mistake, so the test guarding it is decoration. `go test -v -run
TestZZZAblationTable` prints the table; `record` fails the run when an ablation
meant to be caught is not, **and** when one documented as invisible suddenly is.

| ablation | detected | by what |
|---|---|---|
| `cbCFHeader` ignored | yes | refused: folder 0 records `typeCompress(5)` |
| `cbCFFolder` ignored | yes | refused: a structure at offset 2880154539 runs past the end |
| `cbCFData` ignored | yes | refused: block 2 holds 49298 bytes past the end |
| all three ignored | yes | refused: unknown compression |
| backslash left unconverted | yes | the unconverted name is not a path this filesystem answers to |
| MSZIP history dropped, on `mszip.cab` | **no — as documented** | the same bytes came back: gcab emits no cross-block references |
| MSZIP history dropped, on `history.cab` | yes | refused: block 1 will not inflate |
| MSZIP window truncated to 0 / 1 Ki / 8 Ki / 16 Ki / 32767 B | yes (each) | refused: block 1 will not inflate |
| `uoffFolderStart` ignored (both methods) | yes | wrong bytes, differing at byte 0 |
| `CK` signature corrupted | yes | refused — *by the checksum*, which is why the next row exists |
| `CK` corrupted **and** the checksum zeroed | yes | refused: block does not begin with `CK` |
| `CK` fed to the inflater | yes | `flate: corrupt input before offset 3` |
| UTF-8 name read as Windows-1252 | yes | the name no longer resolves: `sub/cafÃ©.txt` |
| Windows-1252 name read as UTF-8 | yes | refused as invalid UTF-8 |
| one byte flipped inside a block | yes | refused: checksum mismatch |
| checksum trailing bytes folded little-endian | yes | of 41 blocks, by `cbData` mod 4: {0: 8 agree / 0 differ} {1: 14/0} {2: 7/6} {3: 0/6} |

## Regenerating the fixtures

```
gcab -c -z mszip.cab big.txt small.txt
gcab -c    store.cab big.txt small.txt
gcab -c -z paths.cab big.txt sub/deep/nested.txt sub/café.txt
```

`reserve.cab` re-frames `mszip.cab` with two folders and all three reserve
areas; `history.cab` frames a payload whose every 32 KiB block repeats the block
before it, each compressed with `flate.NewWriterDict` seeded with that preceding
block. Both were extracted by the outside readers listed above and compared byte
for byte before being committed. The payload sha256 sums are pinned in
`fixtures_test.go`, so a fixture regenerated over a different payload fails
loudly rather than quietly measuring something else.

## Licence

BSD-3-Clause.
