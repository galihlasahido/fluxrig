# iso8583-v87-ascii.yaml against jPOS ISO87APackager

The jPOS examples (`jpos-example/*/deploy/*.xml`) use `ASCIIChannel` with
`org.jpos.iso.packager.ISO87APackager`. This records how the shipped
`iso8583-v87-ascii.yaml` compares with that packager. The checks live in
`pkg/gears/native/iso8583/codec/jpos_compat_test.go`.

## What was verified and how

- **Verified by running jPOS.** `jpos-2.1.6` is not in the local Maven repository;
  `jpos-2.0.0.jar` is. The packager class, field packager classes and lengths were
  read from `ISO87APackager` in that jar by reflection, and the two wire strings in
  the test were produced by `ISOMsg.pack()` on it.
- **From memory, not verified.** That the 2.1.6 `ISO87APackager` has the same field
  table as 2.0.0. The class has been a fixed table for many releases, but this was
  not checked against the 2.1.6 jar.
- **Verified in Go.** The fluxrig side is read from the spec as the codec gear loads
  it (`Gear.moovSpec`), and packed and unpacked through the gear.
- **Not tested.** `ASCIIChannel` framing (4 ASCII digits of message length). It is
  channel behaviour, not packager behaviour. fluxrig's framing for it is a separate
  item.

## Field by field

jPOS classes: `IFA_NUMERIC` (digits, left zero-filled, ASCII), `IF_CHAR` (characters,
right space-filled, ASCII), `IFA_LLLCHAR` (3-digit ASCII length, then characters),
`IFA_BITMAP` (hexadecimal ASCII).

| DE | jPOS class, length | SDL spec | Match |
|:---|:---|:---|:---|
| MTI (0) | IFA_NUMERIC 4 | String 4, ASCII, fixed, left-pad `0` | yes |
| Bitmap (1) | IFA_BITMAP, 16 hex chars (8 bytes), bit 1 set only if a field above 64 is present | Bitmap, hex ASCII, `length: 16` (moov counts bytes: 16 bytes, 32 hex chars), bit 1 never set | **no** |
| 3 | IFA_NUMERIC 6 | Numeric 6, ASCII, fixed, left-pad `0` | yes |
| 7 | IFA_NUMERIC 10 | String 10, ASCII, fixed, left-pad `0` | yes |
| 11 | IFA_NUMERIC 6 | String 6, ASCII, fixed, left-pad `0` | yes |
| 12 | IFA_NUMERIC 6 | String 6, ASCII, fixed, left-pad `0` | yes |
| 39 | IF_CHAR 2 | String 2, ASCII, fixed | yes (see short values) |
| 41 | IF_CHAR 8 | String 8, ASCII, fixed | yes (see short values) |
| 63 | IFA_LLLCHAR 999 | String 999, ASCII, `LLL` prefix | yes |
| 70 | IFA_NUMERIC 3 | String 3, ASCII, fixed, left-pad `0` | yes |

Every field the examples use matches except the bitmap.

## Mismatches

1. **Bitmap length (blocks interoperability).** The shipped spec packs a 32-character
   bitmap and does not set bit 1. For `0800` with DE 70 the bytes differ only in
   bit 1 (`0220...` against jPOS `8220...`); for a message with fields up to 64 only,
   fluxrig sends 16 extra characters that a jPOS peer reads as data. In the other
   direction, jPOS bytes do not unpack with the shipped spec (both `0800` with DE 70
   and `0200` without it fail in the bitmap).
   - Fix: `1: {length: 8}` under `wire.fields`. With it, both reference messages pack
     **byte for byte** like jPOS and unpack.
   - The shipped spec was not changed. The one-line variant is
     `iso8583-v87-ascii-jpos.yaml`; a test fails if it drifts from the shipped file.
2. **Leading zeros are lost on unpack.** With either spec, a padded field comes back
   without its leading zeros (jPOS `000042` in DE 11 decodes as `42`, DE 7
   `0102030405` as `102030405`, DE 3 `000000` as `0`). This is independent of the
   bitmap and is not specific to jPOS. It was reproduced, not root-caused; the cause
   looks like the `padding` entries in `wire.fields` (moov strips padding on unpack).
   Not changed here.
3. **Short values.** jPOS pads DE 41 `T1` to `T1      `; the spec rejects it
   (`field length: 2 should be fixed: 8`). DE 3 `0` is zero-filled to `000000` by both.
4. **Fields the spec does not define.** jPOS ISO87A defines DE 2 to 128. The spec
   defines 2 to 64 (some as composites), 70 and 90. DE 65 to 69, 71 to 89, 91 to 103,
   105 to 128 are absent (61 fields); a message carrying one cannot unpack. None are
   used by the jPOS examples except 70.
5. **Other differences outside the examples' fields** (from the same jar dump):
   DE 44 is `LL` max 99 in the spec and max 25 in jPOS; DE 35 is Track2 and DE 55
   and DE 104 are composites in the spec, where jPOS has plain `IFA_LLNUM 37` and
   `IFA_LLLCHAR 999`. The wire form is the same ASCII length-prefixed text; only the
   parsing differs.

## Reference wire strings (jPOS 2.0.0)

```
0800 DE7=0102030405 DE11=000042 DE70=301
0800822000000000000004000000000000000102030405000042301

0200 DE3=000000 DE12=123456 DE39=00 DE41=TERM0001 DE63=ABC
0200201000000280000200000012345600TERM0001003ABC
```
