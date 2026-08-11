# ENE DRAM RGB Support — CR3 Fix Review Notes

Review scope: `feature/ene-memory-support` vs
`enhancement/ene-memory-code-review-cr3`, i.e. the fix for finding #3 in
[`ene-memory-code-review.md`](ene-memory-code-review.md) (`Stop()` leaving
ENE modules on their internal rainbow instead of latching them black like
Corsair modules). `docs/ene-memory-code-review-cr1.md` is excluded from
this review — it documents the prior CR1 round and wasn't the target here.

Findings are listed in descending severity.

---

## 1. Medium (plausible) — `Stop()`'s new ENE path has no inter-channel pacing, unlike the identical sequence in `setDeviceColor()`

- `src/devices/memory/memory.go:328-339` (new `Stop()` ENE branch)
- `src/devices/memory/memory.go:1312-1324` (`setDeviceColor()`'s ENE branch, for comparison)

```go
// Stop() — new ENE branch: no delay before the next channel
d.eneSetDirect(d.Devices[k].EneAddress)
static := map[int][]byte{}
for i := 0; i < int(d.Devices[k].LedChannels); i++ {
    static[i] = []byte{0, 0, 0}
}
d.writeDeviceColor(k, rgb.SetColor(static))
continue
```

```go
// setDeviceColor() — same eneSetDirect -> writeDeviceColor sequence,
// but always paced before moving to the next channel
d.eneSetDirect(d.Devices[k].EneAddress)
...
d.writeDeviceColor(k, buffer)
time.Sleep(5 * time.Millisecond)
```

`Stop()` now performs the same `eneSetDirect` (2 I2C writes: direct-mode +
apply) followed by `writeDeviceColor` → `transferEne` (per-LED
pointer+block writes) that `setDeviceColor()` does, but loops over all ENE
channels back-to-back with zero delay, whereas `setDeviceColor()` always
sleeps 5ms after writing before advancing to the next channel.

If that pacing exists for I2C bus/firmware settling reasons rather than
incidental throttling, a system with multiple ENE modules could have some
modules silently fail to latch the black frame on daemon stop —
reintroducing a narrower version of the exact "ENE not black on `Stop()`"
bug this diff sets out to fix. `eneWriteByte`/`eneWriteBlock` errors are
`Warn`-logged only, not retried or surfaced, so the failure would be
silent.

**Suggested verification/hardening:** confirm on hardware with 2+ ENE
modules, or mirror the 5ms pacing in `Stop()`'s ENE branch for consistency
with the known-working `setDeviceColor()` path.

---

## 2. Low — Duplicated black-buffer construction; `Stop()`'s Corsair branch bypasses the existing `writeDeviceColor` dispatcher

- `src/devices/memory/memory.go:328-346` (`Stop()`, both branches)
- `src/devices/memory/memory.go:2358` (`writeDeviceColor`, already dispatches by protocol)

The ENE branch (334-337), the immediately-following Corsair branch
(341-344), and `setDeviceColor()`'s own copy (~1317-1319) each build an
identical "zero out `LedChannels` colors" map independently. The Corsair
branch in `Stop()` also calls `d.transfer(buffer, colorAddresses[k], ...)`
directly instead of the existing `d.writeDeviceColor(k, buffer)` helper,
which already picks `transferEne` vs. `d.transfer` by protocol and is used
everywhere else in the file (lines 1322, 1380, 1619, 1707).

Not a functional bug, but a future change to how a black frame is built,
or to `writeDeviceColor`'s non-ENE dispatch logic, has to be found and
applied in multiple places and could easily be missed in one.

**Suggested cleanup:** build the zero buffer once above the branch, invoke
`eneSetDirect` conditionally for ENE devices, then always call
`d.writeDeviceColor(k, buffer)` — removing both the duplication and the
Corsair-branch bypass.

---

## 3. Low — Doc citation range overruns into unrelated Corsair code

- `docs/ene-memory-code-review.md:102`

The updated citation `src/devices/memory/memory.go:328-343` overruns the
ENE-specific `if` block (which runs exactly 328-340) by 3 lines into the
unmodified, pre-existing Corsair branch's own black-buffer construction. A
reader following the citation to see "what changed for ENE" lands a few
lines into unrelated code.

**Suggested fix:** narrow the citation to `328-340`.

---

## 4. Low — Docs mischaracterize the removed `eneSetDirect` release path as pre-existing "dead code"

- `docs/ene-memory-code-review.md:112`

> `eneSetDirect` no longer takes an `enabled` parameter — it always takes
> host control, its former release path being dead code.

The release path (`enabled=false`, writing `val=0`) was live and actively
invoked by `Stop()` right up until this diff removed it — it only became
obsolete as a direct result of this diff's `Stop()` rewrite, not before.
Wording nit; doesn't change the substance of the fix.

---

## Summary

No confirmed correctness bugs. #1 is a plausible gap worth verifying on
hardware with multiple ENE modules (or hardening pre-emptively by mirroring
`setDeviceColor()`'s pacing). #2 is a cleanup opportunity to route both
`Stop()` branches through the existing `writeDeviceColor` dispatcher and
build the black buffer once. #3 and #4 are minor documentation accuracy
nits in the CR1-round writeup.
