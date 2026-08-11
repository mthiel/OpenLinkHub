# ENE DRAM RGB Support — Code Review Notes

Review scope: `feature/ene-memory-support` vs `main` (commits `8d8f5664`,
`86a673d5`). Build gate passes; wire protocol verified against OpenRGB's
`ENESMBusController` (register constants, byte-swapped pointer write to
cmd `0x00`, read-cmd `0x81` / write-cmd `0x01` / block-cmd `0x03`,
R/G/B ordering, direct-mode `0x8020` + apply `0x80A0`, LED count at
config-table offset `0x1C02`, and the version-to-direct-register map).

No definite crashes or deadlocks found. Mutex discipline, `transferEne`
bounds checking, `Stop()` ordering, and the `WriteWordData` /
`WriteByteData` ioctl struct layouts all check out.

Findings are listed in descending severity; each carries its current
status. Follow-up fixes live on `bugfix/ene-memory-support-fixes-cr1`.

---

## 1. Medium — Channel indices are detection-order-based; persisted profile data can drift across DIMMs

**Status: RESOLVED** (`6b896896` on `feature/ene-memory-support`; CR1
cleanup `0da712cc` on `bugfix/ene-memory-support-fixes-cr1`).

`getDevices` assigned each ENE module a channel id derived from its position
in the detection result:

- `src/devices/memory/memory.go:832-833` (original)

```go
for eneIndex, em := range detectEneModules(d.dev.File) {
    i := maximumRegisters + eneIndex
```

A module's channel id therefore depended on how many ENE controllers were
detected on a given boot. Labels, RGB profiles, and per-LED overrides are
persisted keyed by channel id. If the detected module population changes
(a DIMM is removed, reseated, or one module fails its self-test and drops
out of the list), every module's channel id shifts, and the saved
`Labels` / `RGBProfiles` / `RGBOverride` / `RGBPerLed` silently apply to a
different physical DIMM.

The Corsair loop keys by slot position and is stable; the ENE path was not.

**Fix:** the channel id is now derived from the module's fixed position in
the `eneRamAddresses` pool rather than its position in the detection result,
so removing/reseating one DIMM no longer shifts the others' ids:

- `src/devices/memory/memory.go:836-837`
- `src/devices/memory/ene.go:148,169` (`eneModule.Index`, set during the
  pool scan in `detectEneModules`)

```go
for _, em := range detectEneModules(d.dev.File) {
    i := maximumRegisters + em.Index
```

`eneRamAddresses` is also now documented as order-load-bearing (append-only
`src/devices/memory/ene.go:54`), so a future edit that reorders the pool
can't silently remap persisted settings. For a fully-populated system the
new address-derived ids equal the old detection-order ids, so existing
profiles remain valid with no migration.

---

## 2. Low-Medium — Self-test probes an unvalidated SMBus address pool

**Status: OPEN** — no code change; the suggested hardening comment has not
been added. Detection rests on the `0xA0`-`0xAF` echo signature alone; a
non-ENE device on the extended pool could be misdetected as DRAM. The
`ORDER IS LOAD-BEARING` comment added to `eneRamAddresses` in the fix does
not cover this — consider a follow-up noting the extended addresses
(`0x4F`, `0x66`-`0x67`, `0x39`-`0x3D`) are unvalidated against real
hardware.

`eneSelfTest` scans 15 addresses including `0x39`-`0x3D`, `0x4F`, and
`0x66`-`0x67`, which on many boards are populated by non-ENE SMBus
peripherals (PMIC/RCD/SPD):

- `src/devices/memory/ene.go:54`

```go
var eneRamAddresses = []byte{0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x4F, 0x66, 0x67, 0x39, 0x3A, 0x3B, 0x3C, 0x3D}
```

Detection rests entirely on the `0xA0`-`0xAF` echo signature; if a non-ENE
device on one of these addresses ever echoes, it is registered as DRAM and
receives block writes at its address.

This matches OpenRGB's own address pool (minus `0x77`) and is therefore
consistent with prior art, but the docs only claim validation on the
`0x70`-`0x76` range (see `docs/memory-configuration.md`).

**Suggested hardening:** add a comment noting the extended pool
(`0x4F`, `0x66`-`0x67`, `0x39`-`0x3D`) is unvalidated against real
hardware.

---

## 3. Low — `Stop()` leaves ENE modules on the internal rainbow while Corsair modules are left black

**Status: ACCEPTED** — intentional, documented behavior; no change planned.

- `src/devices/memory/memory.go:326-342`

ENE modules are released to host-off, returning them to their boot rainbow,
whereas Corsair modules are explicitly driven black. This is intentional
per the code comment and docs, but it is an inconsistency in what the RAM
looks like when the daemon is down, noticeable to users with mixed
Corsair + ENE modules.

---

## 4. Low — `eneReadString` terminates only on NUL

**Status: OPEN** — informational; not observed on validated hardware and
OpenRGB shares the limitation.

- `src/devices/memory/ene.go:123-127`

A controller that pads the 16-byte device-name field with `0xFF` (rather
than `0x00`) yields a garbage version string. The
`eneVersionDirectRegister` lookup then fails and the module silently falls
back to the V1 direct register `0x8000` instead of V2 `0x8100` — colors
would go to the wrong registers.

Not observed on the validated hardware, and OpenRGB shares the same
limitation. Informational.

---

## 5. Low — `getSpdHwmonTemperatureFile` returns the first `temp*_input` from the glob

**Status: OPEN** — no code change; harmless if `temp1` is always the DIMM
temp.

- `src/devices/memory/memory.go:367-373`

If the `spd5118` driver exposes more than one temp input, the
alphabetically-first (typically `temp1_input`) is picked without confirming
it is the module's own sensor. Harmless if `temp1` is always the DIMM temp,
but there is no verification.

---

## Summary

Finding #1 (the only one with real user-visible impact) is fixed and
cleanup-reviewed on `bugfix/ene-memory-support-fixes-cr1`. #3 was accepted
as intentional. #2, #4, and #5 remain open as hardening/consistency notes
with no user-visible impact on the validated hardware.
