package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"sync"
)

// byteIndex maps movie time to a byte offset in the file actually being
// served. Pacing from the file's average bitrate falls behind whenever a
// stretch runs hotter than that average, and the television underruns many
// minutes in — the reload and the speed meter.
type byteIndex struct {
	mu   sync.Mutex
	pts  []timeByte
	size int64
}

type timeByte struct {
	t float64
	n int64
}

func newByteIndex(pts []timeByte, size int64) *byteIndex {
	sort.Slice(pts, func(i, j int) bool {
		if pts[i].t == pts[j].t {
			return pts[i].n < pts[j].n
		}
		return pts[i].t < pts[j].t
	})
	out := make([]timeByte, 0, len(pts))
	for _, p := range pts {
		if p.t < 0 || p.n < 0 {
			continue
		}
		if len(out) > 0 {
			prev := out[len(out)-1]
			if p.t == prev.t {
				continue
			}
			if p.n < prev.n {
				p.n = prev.n
			}
		}
		out = append(out, p)
	}
	return &byteIndex{pts: out, size: size}
}

func (x *byteIndex) len() int {
	if x == nil {
		return 0
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.pts)
}

// add records one remux progress sample. Points closer than a quarter of a
// second replace the last one so a long movie does not grow without bound.
func (x *byteIndex) add(t float64, n int64) {
	if x == nil || t < 0 || n < 0 {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.pts) > 0 {
		last := &x.pts[len(x.pts)-1]
		if t < last.t {
			return
		}
		if n < last.n {
			n = last.n
		}
		if t-last.t < 0.25 {
			last.t = t
			last.n = n
			return
		}
	}
	x.pts = append(x.pts, timeByte{t: t, n: n})
}

func (x *byteIndex) byteAt(t float64) (int64, bool) {
	if x == nil {
		return 0, false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.pts) == 0 {
		return 0, false
	}
	if t <= 0 {
		return x.clamp(0), true
	}
	pts := x.pts
	if t >= pts[len(pts)-1].t {
		return x.clamp(x.extrapolateByte(t)), true
	}
	i := sort.Search(len(pts), func(i int) bool { return pts[i].t >= t })
	if i == 0 {
		if pts[0].t <= 0 {
			return x.clamp(pts[0].n), true
		}
		return x.clamp(int64(float64(pts[0].n) * t / pts[0].t)), true
	}
	prev, next := pts[i-1], pts[i]
	if next.t == prev.t {
		return x.clamp(next.n), true
	}
	frac := (t - prev.t) / (next.t - prev.t)
	return x.clamp(prev.n + int64(frac*float64(next.n-prev.n))), true
}

func (x *byteIndex) timeAt(off int64) (float64, bool) {
	if x == nil {
		return 0, false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.pts) == 0 {
		return 0, false
	}
	if off <= 0 {
		return 0, true
	}
	pts := x.pts
	if off >= pts[len(pts)-1].n {
		return x.extrapolateTime(off), true
	}
	i := sort.Search(len(pts), func(i int) bool { return pts[i].n >= off })
	if i == 0 {
		if pts[0].n <= 0 {
			return pts[0].t, true
		}
		return pts[0].t * float64(off) / float64(pts[0].n), true
	}
	prev, next := pts[i-1], pts[i]
	if next.n == prev.n {
		return next.t, true
	}
	frac := float64(off-prev.n) / float64(next.n-prev.n)
	return prev.t + frac*(next.t-prev.t), true
}

func (x *byteIndex) extrapolateByte(t float64) int64 {
	pts := x.pts
	last := pts[len(pts)-1]
	n := last.n
	if len(pts) >= 2 && t > last.t {
		prev := pts[len(pts)-2]
		if last.t > prev.t && t-last.t <= 180 {
			rate := float64(last.n-prev.n) / (last.t - prev.t)
			n = last.n + int64(rate*(t-last.t))
		}
	}
	if x.size > 0 && n > x.size {
		n = x.size
	}
	if n < 0 {
		n = 0
	}
	return n
}

func (x *byteIndex) clamp(n int64) int64 {
	if n < 0 {
		return 0
	}
	if x.size > 0 && n > x.size {
		return x.size
	}
	return n
}

func (x *byteIndex) extrapolateTime(off int64) float64 {
	pts := x.pts
	last := pts[len(pts)-1]
	if off <= last.n || len(pts) < 2 {
		return last.t
	}
	prev := pts[len(pts)-2]
	if last.n <= prev.n {
		return last.t
	}
	rate := (last.t - prev.t) / float64(last.n-prev.n)
	extra := rate * float64(off-last.n)
	if extra > 180 {
		extra = 180
	}
	if extra < 0 {
		extra = 0
	}
	return last.t + extra
}

// buildFileIndex reads a Matroska cue index or an MP4 sample table. Either
// is a small structure at a known offset, so a multi-gigabyte movie is not
// scanned. A container with neither returns an error and the server falls
// back to the average bitrate.
func buildFileIndex(path string) (*byteIndex, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() < 16 {
		return nil, fmt.Errorf("file too small to index")
	}
	head := make([]byte, 12)
	if _, err := f.ReadAt(head, 0); err != nil {
		return nil, err
	}
	if head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3 {
		return indexMatroska(f, fi.Size())
	}
	if string(head[4:8]) == "ftyp" || string(head[4:8]) == "moov" || string(head[4:8]) == "mdat" {
		return indexMP4(f, fi.Size())
	}
	return nil, fmt.Errorf("no cue index in this container")
}

const (
	idEBML               = 0x1A45DFA3
	idSegment            = 0x18538067
	idSeekHead           = 0x114D9B74
	idSeek               = 0x4DBB
	idSeekID             = 0x53AB
	idSeekPosition       = 0x53AC
	idInfo               = 0x1549A966
	idTimestampScale     = 0x2AD7B1
	idCues               = 0x1C53BB6B
	idCuePoint           = 0xBB
	idCueTime            = 0xB3
	idCueTrackPositions  = 0xB7
	idCueClusterPosition = 0xF1
	idCluster            = 0x1F43B675
)

type ebmlReader struct {
	f    *os.File
	pos  int64
	size int64
}

type ebmlElem struct {
	id   uint64
	data int64
	size int64 // -1 when the element runs to the end of its parent
	end  int64
}

func (r *ebmlReader) readElem() (ebmlElem, error) {
	id, err := r.readID()
	if err != nil {
		return ebmlElem{}, err
	}
	sz, unknown, err := r.readSize()
	if err != nil {
		return ebmlElem{}, err
	}
	e := ebmlElem{id: id, data: r.pos}
	if unknown {
		e.size = -1
		e.end = r.size
		return e, nil
	}
	if sz > uint64(r.size) {
		return ebmlElem{}, fmt.Errorf("ebml element larger than the file")
	}
	e.size = int64(sz)
	e.end = r.pos + e.size
	if e.end < r.pos || e.end > r.size {
		return ebmlElem{}, fmt.Errorf("ebml element runs past the file")
	}
	return e, nil
}

func (r *ebmlReader) payload(e ebmlElem) ([]byte, error) {
	if e.size < 0 || e.size > 64<<20 {
		return nil, fmt.Errorf("ebml element is too large to read")
	}
	buf := make([]byte, e.size)
	if _, err := r.f.ReadAt(buf, e.data); err != nil {
		return nil, err
	}
	r.pos = e.end
	return buf, nil
}

func (r *ebmlReader) skip(e ebmlElem) error {
	if e.end < 0 || e.end > r.size {
		return fmt.Errorf("cannot skip an open-ended element")
	}
	r.pos = e.end
	return nil
}

func (r *ebmlReader) readID() (uint64, error) {
	raw, _, err := r.readVint(true)
	return raw, err
}

func (r *ebmlReader) readSize() (uint64, bool, error) {
	val, width, err := r.readVint(false)
	if err != nil {
		return 0, false, err
	}
	unknown := val == (uint64(1)<<uint(7*width))-1
	return val, unknown, nil
}

func (r *ebmlReader) readVint(keepMarker bool) (uint64, int, error) {
	var b [8]byte
	if _, err := r.f.ReadAt(b[:1], r.pos); err != nil {
		return 0, 0, err
	}
	width := 1
	mask := byte(0x80)
	for width <= 8 && b[0]&mask == 0 {
		width++
		mask >>= 1
	}
	if width > 8 || b[0]&mask == 0 {
		return 0, 0, fmt.Errorf("bad ebml vint at %d", r.pos)
	}
	if _, err := r.f.ReadAt(b[:width], r.pos); err != nil {
		return 0, 0, err
	}
	r.pos += int64(width)
	var raw uint64
	for i := 0; i < width; i++ {
		raw = raw<<8 | uint64(b[i])
	}
	if keepMarker {
		return raw, width, nil
	}
	return raw &^ (uint64(mask) << uint(8*(width-1))), width, nil
}

func indexMatroska(f *os.File, size int64) (*byteIndex, error) {
	r := &ebmlReader{f: f, size: size}
	var segment ebmlElem
	found := false
	for i := 0; i < 8 && r.pos+2 < size; i++ {
		e, err := r.readElem()
		if err != nil {
			return nil, err
		}
		if e.id == idEBML {
			if err := r.skip(e); err != nil {
				return nil, err
			}
			continue
		}
		if e.id == idSegment {
			segment = e
			found = true
			break
		}
		if err := r.skip(e); err != nil {
			return nil, err
		}
	}
	if !found {
		return nil, fmt.Errorf("matroska file has no segment")
	}
	segData := segment.data
	segEnd := segment.end
	if segEnd <= 0 || segEnd > size {
		segEnd = size
	}
	scale := uint64(1_000_000)
	var cuesAt int64 = -1
	var inline []byte
	type hop struct{ at int64 }
	var heads []hop
	seenHead := map[int64]bool{}
	for r.pos+2 < segEnd {
		e, err := r.readElem()
		if err != nil {
			break
		}
		switch e.id {
		case idInfo:
			buf, err := r.payload(e)
			if err != nil {
				return nil, err
			}
			if s := findUint(buf, idTimestampScale); s > 0 {
				scale = s
			}
		case idSeekHead:
			if seenHead[e.data] {
				if err := r.skip(e); err != nil {
					return nil, err
				}
				continue
			}
			seenHead[e.data] = true
			buf, err := r.payload(e)
			if err != nil {
				return nil, err
			}
			for _, ent := range parseSeekHead(buf) {
				at := segData + ent.pos
				if ent.id == idCues && cuesAt < 0 {
					cuesAt = at
				}
				if ent.id == idSeekHead && !seenHead[at] {
					heads = append(heads, hop{at})
				}
			}
		case idCues:
			buf, err := r.payload(e)
			if err != nil {
				return nil, err
			}
			inline = buf
		case idCluster:
			// The cue table sits after the clusters, or the opening SeekHead
			// already pointed at it. Skip a cluster only when we still have
			// to go looking; the body itself is the movie and is not read.
			if inline != nil || cuesAt >= 0 {
				goto cues
			}
			if err := r.skip(e); err != nil {
				goto cues
			}
		default:
			if err := r.skip(e); err != nil {
				return nil, err
			}
		}
		if inline != nil && cuesAt >= 0 {
			break
		}
	}
cues:
	for len(heads) > 0 && cuesAt < 0 && len(seenHead) < 8 {
		h := heads[0]
		heads = heads[1:]
		if seenHead[h.at] || h.at < 0 || h.at+2 > size {
			continue
		}
		seenHead[h.at] = true
		r.pos = h.at
		e, err := r.readElem()
		if err != nil || e.id != idSeekHead {
			continue
		}
		buf, err := r.payload(e)
		if err != nil {
			continue
		}
		for _, ent := range parseSeekHead(buf) {
			at := segData + ent.pos
			if ent.id == idCues && cuesAt < 0 {
				cuesAt = at
			}
			if ent.id == idSeekHead && !seenHead[at] {
				heads = append(heads, hop{at})
			}
		}
	}
	var raw []byte
	if inline != nil {
		raw = inline
	} else if cuesAt >= 0 {
		r.pos = cuesAt
		e, err := r.readElem()
		if err != nil {
			return nil, err
		}
		if e.id != idCues {
			return nil, fmt.Errorf("cue seek pointed at %x", e.id)
		}
		raw, err = r.payload(e)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("matroska file has no cue index")
	}
	pts := parseCues(raw, scale, segData)
	if len(pts) == 0 {
		return nil, fmt.Errorf("matroska cue index was empty")
	}
	return newByteIndex(pts, size), nil
}

type seekEnt struct {
	id  uint64
	pos int64
}

func parseSeekHead(buf []byte) []seekEnt {
	var out []seekEnt
	walkElements(buf, func(id uint64, body []byte) {
		if id != idSeek {
			return
		}
		var ent seekEnt
		var haveID, havePos bool
		walkElements(body, func(cid uint64, cbody []byte) {
			switch cid {
			case idSeekID:
				ent.id = uintBE(cbody)
				haveID = len(cbody) > 0
			case idSeekPosition:
				ent.pos = int64(uintBE(cbody))
				havePos = true
			}
		})
		if haveID && havePos && ent.pos >= 0 {
			out = append(out, ent)
		}
	})
	return out
}

func parseCues(buf []byte, scale uint64, segData int64) []timeByte {
	if scale == 0 {
		scale = 1_000_000
	}
	var pts []timeByte
	walkElements(buf, func(id uint64, body []byte) {
		if id != idCuePoint {
			return
		}
		var cueTime uint64
		var haveTime bool
		var cluster int64 = -1
		walkElements(body, func(cid uint64, cbody []byte) {
			switch cid {
			case idCueTime:
				cueTime = uintBE(cbody)
				haveTime = true
			case idCueTrackPositions:
				walkElements(cbody, func(tid uint64, tbody []byte) {
					if tid == idCueClusterPosition && cluster < 0 {
						cluster = int64(uintBE(tbody))
					}
				})
			}
		})
		if !haveTime || cluster < 0 {
			return
		}
		sec := float64(cueTime) * float64(scale) / 1e9
		pts = append(pts, timeByte{t: sec, n: segData + cluster})
	})
	return pts
}

func findUint(buf []byte, want uint64) uint64 {
	var found uint64
	walkElements(buf, func(id uint64, body []byte) {
		if id == want && found == 0 && len(body) > 0 && len(body) <= 8 {
			found = uintBE(body)
		}
	})
	return found
}

func walkElements(buf []byte, fn func(id uint64, body []byte)) {
	i := 0
	for i+2 <= len(buf) {
		id, w, ok := vintAt(buf[i:], true)
		if !ok || i+w >= len(buf) {
			return
		}
		i += w
		sz, w, ok := vintAt(buf[i:], false)
		if !ok {
			return
		}
		i += w
		if sz > uint64(len(buf)-i) {
			return
		}
		fn(id, buf[i:i+int(sz)])
		i += int(sz)
	}
}

func vintAt(b []byte, keepMarker bool) (uint64, int, bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	width := 1
	mask := byte(0x80)
	for width <= 8 && b[0]&mask == 0 {
		width++
		mask >>= 1
	}
	if width > 8 || width > len(b) || b[0]&mask == 0 {
		return 0, 0, false
	}
	var raw uint64
	for i := 0; i < width; i++ {
		raw = raw<<8 | uint64(b[i])
	}
	if keepMarker {
		return raw, width, true
	}
	return raw &^ (uint64(mask) << uint(8*(width-1))), width, true
}

func uintBE(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

func indexMP4(f *os.File, size int64) (*byteIndex, error) {
	var moovAt, moovSize int64 = -1, 0
	err := walkAtoms(f, 0, size, func(typ string, payload, end int64) error {
		if typ == "moov" {
			moovAt, moovSize = payload, end
			return errStopWalk
		}
		if typ == "mdat" {
			return errSkipChildren
		}
		return nil
	})
	if err != nil && err != errStopWalk {
		return nil, err
	}
	if moovAt < 0 {
		return nil, fmt.Errorf("mp4 has no moov")
	}
	pts, err := indexMoov(f, moovAt, moovSize)
	if err != nil {
		return nil, err
	}
	if len(pts) == 0 {
		return nil, fmt.Errorf("mp4 sample table was empty")
	}
	return newByteIndex(pts, size), nil
}

var errStopWalk = fmt.Errorf("stop")
var errSkipChildren = fmt.Errorf("skip")

func walkAtoms(f *os.File, start, end int64, fn func(typ string, payload, end int64) error) error {
	pos := start
	for pos+8 <= end {
		var hdr [16]byte
		if _, err := f.ReadAt(hdr[:8], pos); err != nil {
			return err
		}
		sz := int64(binary.BigEndian.Uint32(hdr[0:4]))
		typ := string(hdr[4:8])
		header := int64(8)
		switch sz {
		case 1:
			if pos+16 > end {
				return fmt.Errorf("truncated mp4 atom")
			}
			if _, err := f.ReadAt(hdr[8:16], pos+8); err != nil {
				return err
			}
			wide := binary.BigEndian.Uint64(hdr[8:16])
			if wide > uint64(end) {
				return fmt.Errorf("mp4 atom larger than the file")
			}
			sz = int64(wide)
			header = 16
		case 0:
			sz = end - pos
		}
		if sz < header || pos+sz > end {
			return fmt.Errorf("bad mp4 atom %s", typ)
		}
		payload := pos + header
		atomEnd := pos + sz
		err := fn(typ, payload, atomEnd)
		if err == errStopWalk {
			return errStopWalk
		}
		if err != nil && err != errSkipChildren {
			return err
		}
		if err != errSkipChildren && (typ == "moov" || typ == "trak" || typ == "mdia" || typ == "minf" || typ == "stbl") {
			if err := walkAtoms(f, payload, atomEnd, fn); err != nil {
				return err
			}
		}
		pos = atomEnd
	}
	return nil
}

func indexMoov(f *os.File, start, end int64) ([]timeByte, error) {
	var pts []timeByte
	var trakStart, trakEnd int64 = -1, 0
	var fallbackStart, fallbackEnd int64 = -1, 0
	err := walkAtoms(f, start, end, func(typ string, payload, atomEnd int64) error {
		if typ != "trak" {
			return nil
		}
		if fallbackStart < 0 {
			fallbackStart, fallbackEnd = payload, atomEnd
		}
		if trakIsVideo(f, payload, atomEnd) {
			trakStart, trakEnd = payload, atomEnd
			return errStopWalk
		}
		return errSkipChildren
	})
	if err != nil && err != errStopWalk {
		return nil, err
	}
	if trakStart < 0 {
		trakStart, trakEnd = fallbackStart, fallbackEnd
	}
	if trakStart < 0 {
		return nil, fmt.Errorf("mp4 has no track")
	}
	var scale uint32
	var stts, stsc, stco []byte
	var stco64 bool
	err = walkAtoms(f, trakStart, trakEnd, func(typ string, payload, atomEnd int64) error {
		switch typ {
		case "mdhd":
			buf, err := readSpan(f, payload, atomEnd)
			if err != nil {
				return err
			}
			scale = mdhdScale(buf)
		case "stts", "stsc", "stco", "co64":
			buf, err := readSpan(f, payload, atomEnd)
			if err != nil {
				return err
			}
			switch typ {
			case "stts":
				stts = buf
			case "stsc":
				stsc = buf
			case "stco":
				stco = buf
			case "co64":
				stco = buf
				stco64 = true
			}
		}
		return nil
	})
	if err != nil && err != errStopWalk {
		return nil, err
	}
	if scale == 0 || len(stts) < 8 || len(stsc) < 8 || len(stco) < 8 {
		return nil, fmt.Errorf("mp4 track is missing a sample table")
	}
	pts = chunkIndex(scale, stts, stsc, stco, stco64)
	return pts, nil
}

func trakIsVideo(f *os.File, start, end int64) bool {
	video := false
	_ = walkAtoms(f, start, end, func(typ string, payload, atomEnd int64) error {
		if typ != "hdlr" {
			return nil
		}
		buf, err := readSpan(f, payload, atomEnd)
		if err != nil || len(buf) < 12 {
			return errStopWalk
		}
		// version/flags (4) + pre_defined (4) + handler type.
		if string(buf[8:12]) == "vide" {
			video = true
		}
		return errStopWalk
	})
	return video
}

func readSpan(f *os.File, start, end int64) ([]byte, error) {
	if end < start || end-start > 32<<20 {
		return nil, fmt.Errorf("mp4 atom is too large")
	}
	buf := make([]byte, end-start)
	_, err := f.ReadAt(buf, start)
	return buf, err
}

func mdhdScale(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	ver := b[0]
	if ver == 0 && len(b) >= 20 {
		return binary.BigEndian.Uint32(b[12:16])
	}
	if ver == 1 && len(b) >= 32 {
		return binary.BigEndian.Uint32(b[20:24])
	}
	return 0
}

type sttsEnt struct {
	count uint32
	delta uint32
}

type stscEnt struct {
	first uint32
	per   uint32
}

func chunkIndex(scale uint32, stts, stsc, stco []byte, wide bool) []timeByte {
	var times []sttsEnt
	n := be32(stts, 4)
	for i := uint32(0); i < n; i++ {
		off := 8 + int(i)*8
		if off+8 > len(stts) {
			break
		}
		times = append(times, sttsEnt{be32(stts, off), be32(stts, off+4)})
	}
	var runs []stscEnt
	n = be32(stsc, 4)
	for i := uint32(0); i < n; i++ {
		off := 8 + int(i)*12
		if off+12 > len(stsc) {
			break
		}
		runs = append(runs, stscEnt{be32(stsc, off), be32(stsc, off+4)})
	}
	n = be32(stco, 4)
	var offs []int64
	for i := uint32(0); i < n; i++ {
		if wide {
			off := 8 + int(i)*8
			if off+8 > len(stco) {
				break
			}
			offs = append(offs, int64(binary.BigEndian.Uint64(stco[off:off+8])))
		} else {
			off := 8 + int(i)*4
			if off+4 > len(stco) {
				break
			}
			offs = append(offs, int64(be32(stco, off)))
		}
	}
	if len(times) == 0 || len(runs) == 0 || len(offs) == 0 || scale == 0 {
		return nil
	}
	var pts []timeByte
	var sampleTime uint64
	ti, left := 0, times[0].count
	run := 0
	var lastT float64 = -1
	for chunk := 1; chunk <= len(offs); chunk++ {
		for run+1 < len(runs) && int(runs[run+1].first) <= chunk {
			run++
		}
		per := int(runs[run].per)
		if per < 1 {
			per = 1
		}
		sec := float64(sampleTime) / float64(scale)
		if lastT < 0 || sec-lastT >= 0.5 || chunk == len(offs) {
			pts = append(pts, timeByte{t: sec, n: offs[chunk-1]})
			lastT = sec
		}
		need := per
		for need > 0 && ti < len(times) {
			step := need
			if uint32(step) > left {
				step = int(left)
			}
			sampleTime += uint64(step) * uint64(times[ti].delta)
			need -= step
			left -= uint32(step)
			if left == 0 {
				ti++
				if ti < len(times) {
					left = times[ti].count
				}
			}
		}
	}
	return pts
}

func be32(b []byte, off int) uint32 {
	if off < 0 || off+4 > len(b) {
		return 0
	}
	return binary.BigEndian.Uint32(b[off : off+4])
}
