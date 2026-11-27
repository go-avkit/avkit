# govpx v0.5.2 encoder faults on linux/s390x: Go compiler bug golang/go#83097

Not a govpx logic error and not qemu. Measured 2026-11-27 on real s390x
hardware (LinuxONE, 8 cores), test binary cross-compiled on darwin/arm64,
`CGO_ENABLED=0`.

## Cause

Go's s390x backend miscompiles a copy of more than 1024 bytes whose size is
a multiple of 256 when dst == src+size (two adjacent fields, the second
assigned from the first): the loop bound is merged with dst, the loop
advances dst and compares against it, so it never ends and runs off the
mapping. golang/go#83097 (the same rule seen through the GC), fixed on
master by CL 866742 (commit 4ae1ba9d28). Faulting: go1.25.9, go1.26.9,
go1.27.4. Working: master `go1.28-devel_3fabb12ed4`.

govpx v0.5.2 hits it at `(*VP8Encoder).beginInterRDModeDecisionFrame`,
`vp8_encoder_inter_speed.go:586`: `e.interModeSpeedErrorBins =
e.interModeErrorBins`, two adjacent `[1024]uint32` fields (4096 bytes).

    unexpected fault address 0x7cab6ec00000
    fatal error: fault

## Minimal repro, no govpx

    type enc struct {
        a, b [320]uint32 // 1280 bytes = 5*256, b follows a
        n    int
    }

    //go:noinline
    func (e *enc) copyAB() { e.b = e.a }

    func main() { e := new(enc); e.a[3] = 7; e.copyAB(); fmt.Println("ok", e.b[3], e.a[3]) }

Sweep on hardware: sizes up to 1024 bytes pass; 1280, 1536, 2048 fault;
1152..1276 pass (size not a multiple of 256). The faulting loop:

    ADD $1280, R2, R1          // R1 = dst = src+size, also the loop bound
    MVC $256, (R1), (R2)
    LA  256(R1), R1; LA 256(R2), R2
    CMPU R2, R1; BLT -4        // src < dst forever

## What avkit does meanwhile

`vp8enc/main_test.go` skips the package on s390x only when
`runtime.Version()` is a release up to go1.27.4, and prints why. A newer
toolchain lifts the skip by itself.
