// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package vp8enc

import (
	"fmt"
	"os"
	"runtime"
	"testing"
)

// TestMain skips the package on s390x. It is NOT qemu: the same test binary
// (Go 1.27.4, GOARCH=s390x) segfaults on real s390x hardware (LinuxONE) in
// govpx v0.5.2, at (*VP8Encoder).beginInterRDModeDecisionFrame, an array
// assignment between two fields of a ~5 MiB struct. A 15-line program with
// no govpx in it (a [5<<20]byte field before two [1024]uint32 fields, one
// assigned to the other through a pointer) faults the same way, so the
// suspect is the Go toolchain's s390x code for large field offsets. The
// repro is govpx-s390x-repro.md. The skip is printed so that it cannot read
// as a pass.
func TestMain(m *testing.M) {
	if runtime.GOARCH == "s390x" {
		fmt.Println("SKIP vp8enc: govpx encoder faults on s390x (large-offset struct copy), see govpx-s390x-repro.md")
		os.Exit(0)
	}
	os.Exit(m.Run())
}
