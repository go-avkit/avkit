// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package vp8enc

import (
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"testing"
)

// s390xCopyBug reports whether the Go release named by version (a
// runtime.Version() string) has the s390x Move-loop miscompilation: a copy of
// more than 1024 bytes whose size is a multiple of 256, with dst == src+size
// (two adjacent fields, the second assigned from the first), loops without
// end, because the loop bound is merged with dst. govpx v0.5.2 hits it at
// (*VP8Encoder).beginInterRDModeDecisionFrame, `e.interModeSpeedErrorBins =
// e.interModeErrorBins` (two adjacent [1024]uint32 fields). It is not qemu:
// it faults on real s390x hardware. golang/go#83097, fixed on master by
// CL 866742 (commit 4ae1ba9d28); present in go1.25.9, go1.26.9 and go1.27.4.
// Anything that is not a plain release up to go1.27.4 (a newer release, a
// devel build) is assumed fixed, so the skip lifts itself after a toolchain
// bump.
func s390xCopyBug(version string) bool {
	m := regexp.MustCompile(`^go1\.(\d+)(?:\.(\d+))?`).FindStringSubmatch(version)
	if m == nil {
		return false
	}
	minor, _ := strconv.Atoi(m[1])
	patch := 0
	if m[2] != "" {
		patch, _ = strconv.Atoi(m[2])
	}
	if regexp.MustCompile(`devel`).MatchString(version) {
		return false
	}
	return minor < 27 || (minor == 27 && patch <= 4)
}

// TestMain skips the package on s390x with a toolchain that has the bug
// above. The skip is printed so that it cannot read as a pass. The
// reproduction is govpx-s390x-repro.md.
func TestMain(m *testing.M) {
	if runtime.GOARCH == "s390x" && s390xCopyBug(runtime.Version()) {
		fmt.Printf("SKIP vp8enc: %s miscompiles large adjacent-field copies on s390x (golang/go#83097), see govpx-s390x-repro.md\n", runtime.Version())
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestS390xCopyBug(t *testing.T) {
	for v, want := range map[string]bool{
		"go1.25.9": true, "go1.26.9": true, "go1.27": true, "go1.27.4": true,
		"go1.27rc1": true, "go1.27.5": false, "go1.28rc1": false,
		"go1.28.0": false, "go1.28-devel_3fabb12ed4 Fri": false, "devel": false,
	} {
		if got := s390xCopyBug(v); got != want {
			t.Errorf("s390xCopyBug(%q) = %v, want %v", v, got, want)
		}
	}
}
