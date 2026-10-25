package main

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestMatroskaCuesMapTimeToBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(path, sampleMKV(), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := buildFileIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if idx.len() < 2 {
		t.Fatalf("points = %d", idx.len())
	}
	b0, _ := idx.byteAt(0)
	b30, _ := idx.byteAt(30)
	b60, _ := idx.byteAt(60)
	if !(b0 <= b30 && b30 <= b60) {
		t.Fatalf("offsets 0=%d 30=%d 60=%d", b0, b30, b60)
	}
	if b60 <= b0 {
		t.Fatal("60s is not ahead of the start in the file")
	}
	got, ok := idx.timeAt(b60)
	if !ok || math.Abs(got-60) > 0.5 {
		t.Fatalf("timeAt(byteAt(60))=%v ok=%v", got, ok)
	}
}

func TestMP4SampleTableMapsTimeToBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mp4")
	if err := os.WriteFile(path, sampleMP4(), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := buildFileIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	b0, _ := idx.byteAt(0)
	b1, _ := idx.byteAt(1)
	if b1 <= b0 {
		t.Fatalf("byteAt(0)=%d byteAt(1)=%d", b0, b1)
	}
	got, ok := idx.timeAt(b1)
	if !ok || math.Abs(got-1) > 0.05 {
		t.Fatalf("timeAt=%v ok=%v byte=%d", got, ok, b1)
	}
}

func sampleMKV() []byte {
	info := mkvEl(idInfo, mkvEl(idTimestampScale, mkvU(1_000_000)))
	cluster := mkvEl(idCluster, []byte{0, 0, 0, 0})
	cues := mkvEl(idCues, append(
		mkvCue(0, 0),
		mkvCue(60_000, 32)...,
	))
	seg := mkvEl(idSegment, append(append(info, cluster...), cues...))
	ebml := mkvEl(idEBML, mkvEl(0x4282, []byte("matroska")))
	return append(ebml, seg...)
}

func mkvCue(ms uint64, cluster int64) []byte {
	return mkvEl(idCuePoint, append(
		mkvEl(idCueTime, mkvU(ms)),
		mkvEl(idCueTrackPositions, mkvEl(idCueClusterPosition, mkvU(uint64(cluster))))...,
	))
}

func mkvEl(id uint64, payload []byte) []byte {
	out := append(mkvID(id), mkvSize(len(payload))...)
	return append(out, payload...)
}

func mkvID(id uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], id)
	i := 0
	for i < 7 && b[i] == 0 {
		i++
	}
	return append([]byte(nil), b[i:]...)
}

func mkvSize(n int) []byte {
	if n < 0x7F {
		return []byte{0x80 | byte(n)}
	}
	if n < 0x3FFF {
		return []byte{0x40 | byte(n>>8), byte(n)}
	}
	return []byte{0x20 | byte(n>>16), byte(n >> 8), byte(n)}
}

func mkvU(n uint64) []byte {
	if n == 0 {
		return []byte{0}
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], n)
	i := 0
	for i < 7 && b[i] == 0 {
		i++
	}
	return append([]byte(nil), b[i:]...)
}

func sampleMP4() []byte {
	mdhd := make([]byte, 24) // version 0, timescale at byte 12
	binary.BigEndian.PutUint32(mdhd[12:16], 1000)
	binary.BigEndian.PutUint32(mdhd[16:20], 2000)
	hdlr := make([]byte, 24)
	copy(hdlr[8:12], "vide")
	stts := make([]byte, 16)
	binary.BigEndian.PutUint32(stts[4:8], 1)  // one run
	binary.BigEndian.PutUint32(stts[8:12], 2) // two samples
	binary.BigEndian.PutUint32(stts[12:16], 1000)
	stsc := make([]byte, 20)
	binary.BigEndian.PutUint32(stsc[4:8], 1)
	binary.BigEndian.PutUint32(stsc[8:12], 1)  // first chunk
	binary.BigEndian.PutUint32(stsc[12:16], 1) // one sample each
	binary.BigEndian.PutUint32(stsc[16:20], 1)
	stco := make([]byte, 16)
	binary.BigEndian.PutUint32(stco[4:8], 2)
	binary.BigEndian.PutUint32(stco[8:12], 40)
	binary.BigEndian.PutUint32(stco[12:16], 160)
	stbl := mp4Atom("stbl",
		mp4Atom("stts", stts),
		mp4Atom("stsc", stsc),
		mp4Atom("stco", stco),
	)
	mdia := mp4Atom("mdia",
		mp4Atom("mdhd", mdhd),
		mp4Atom("hdlr", hdlr),
		mp4Atom("minf", stbl),
	)
	moov := mp4Atom("moov", mp4Atom("trak", mdia))
	return append(mp4Atom("ftyp", []byte("isom")), moov...)
}

func mp4Atom(typ string, parts ...[]byte) []byte {
	var payload []byte
	for _, p := range parts {
		payload = append(payload, p...)
	}
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(out)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}
