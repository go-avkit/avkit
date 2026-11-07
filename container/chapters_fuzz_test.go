// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"sort"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/at-wat/ebml-go"
)

// FuzzChapterRecords feeds arbitrary bytes to the three places chapters are
// read from: a QuickTime chapter sample, a chpl box, and the EditionEntry
// elements of a Matroska Chapters element. Nothing may panic, and what comes out
// is in start order, no more than a reader keeps, and never before the file.
func FuzzChapterRecords(f *testing.F) {
	f.Add([]byte{0, 5, 'I', 'n', 't', 'r', 'o', 0, 0, 0, 12, 'e', 'n', 'c', 'd', 0, 0, 1, 0})
	f.Add([]byte{0, 6, 0xFE, 0xFF, 0x65, 0xE5, 0x67, 0x2C})
	f.Add(append([]byte{0, 0, 0, 22, 'c', 'h', 'p', 'l', 1, 0, 0, 0, 0, 0, 0, 0, 1},
		0, 0, 0, 0, 0, 0, 0, 0, 1, 'a'))
	var seed bytes.Buffer
	if err := ebml.Marshal(&mkvChapters{EditionEntry: mkvChaptersOf(fourChapters("eng"))[0].EditionEntry}, &seed); err != nil {
		f.Fatal(err)
	}
	f.Add(seed.Bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = chapterText(data)

		if box, err := mp4.DecodeBox(0, bytes.NewReader(data)); err == nil {
			if chpl, ok := box.(*mp4.ChplBox); ok {
				checkChapterList(t, chaptersOfChpl(chpl))
			}
		}

		var element mkvChapters
		if err := ebml.Unmarshal(bytes.NewReader(data), &element, mkvLeafLimit(len(data))); err == nil {
			checkChapterList(t, mkvReadChapters([]mkvChapters{element}))
		}
	})
}

func checkChapterList(t *testing.T, cs []Chapter) {
	t.Helper()
	if len(cs) > maxChapters {
		t.Fatalf("%d chapters kept", len(cs))
	}
	if !sort.SliceIsSorted(cs, func(i, j int) bool { return cs[i].Start < cs[j].Start }) {
		t.Fatalf("chapters out of order: %+v", cs)
	}
	for _, c := range cs {
		if c.Start < 0 || c.End < 0 || !isText(c.Title) {
			t.Fatalf("chapter %+v is not one", c)
		}
	}
}
