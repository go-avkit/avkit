// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build race

package vp8enc

// raceBuild is true under the race detector, which slows the encoder about
// 10 times: the largest tests then use fewer frames and the size sweep a
// representative subset.
const raceBuild = true
