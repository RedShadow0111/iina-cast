package main

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Safari refuses HLS served as application/octet-stream, and Go's built-in
// table doesn't know these extensions.
func init() {
	mime.AddExtensionType(".m3u8", "application/vnd.apple.mpegurl")
	mime.AddExtensionType(".m4s", "video/iso.segment")
	mime.AddExtensionType(".mp4", "video/mp4")
	mime.AddExtensionType(".vtt", "text/vtt")
}

// StartServer serves dir. master.m3u8 is rewritten on the way out
// (RewriteMasterPlaylist) so the subtitle rendition is on by default and
// labeled with the real track name/language, and given a variant line
// (EnsureVariant, advertising bandwidth) while ffmpeg has not yet written
// one; everything else is a plain file serve. subName/subLang may be empty
// (no-subs casts never produce a master.m3u8, so the rewrite path simply
// never runs for them).
func StartServer(dir, subName, subLang string, bandwidth int) (int, func(), error) {
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return 0, nil, err
	}
	files := http.FileServer(http.Dir(dir))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/master.m3u8" {
			if b, rerr := os.ReadFile(filepath.Join(dir, "master.m3u8")); rerr == nil {
				w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
				io.WriteString(w, EnsureVariant(RewriteMasterPlaylist(string(b), subName, subLang), bandwidth))
				return
			}
		}
		files.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}
	return ln.Addr().(*net.TCPAddr).Port, shutdown, nil
}

// StartMediaServer serves one finished media file at /media and, when
// subPath is non-empty, one subtitle file at /sub.srt. Both answers support
// Range: a DLNA renderer seeks by asking for bytes, and http.ServeContent
// does that.
func StartMediaServer(mediaPath, mediaType, subPath string) (int, func(), error) {
	return startMediaServer(mediaPath, mediaType, subPath, nil, nil)
}

// StartGrowingMediaServer is the remux case. The file is still being
// written. The handler stalls a read that catches the writer (DLNA
// connection stalling) and answers TimeSeekRange with the byte the movie
// time actually falls on, instead of a bitrate guess.
func StartGrowingMediaServer(mediaPath, mediaType, subPath string, stream *mediaStream) (int, func(), error) {
	return startMediaServer(mediaPath, mediaType, subPath, stream, nil)
}

func startMediaServer(mediaPath, mediaType, subPath string, stream *mediaStream, pace *sendPace) (int, func(), error) {
	if stream != nil && pace != nil {
		stream.pace = pace
	}
	raw, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return 0, nil, err
	}
	ln := pacedListener{raw}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media":
			serveMedia(w, r, mediaPath, mediaType, stream, pace)
		case "/sub.srt":
			if subPath == "" {
				http.NotFound(w, r)
				return
			}
			serveFile(w, r, subPath, "text/srt", nil)
		default:
			http.NotFound(w, r)
		}
	})
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}
	return ln.Addr().(*net.TCPAddr).Port, shutdown, nil
}

// pacedListener gives each TV connection an 8MB send buffer and lets TCP
// coalesce writes. A few-kilobyte buffer is why the picture jerks until the
// television has scraped together a second of video.
type pacedListener struct{ net.Listener }

func (l pacedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetWriteBuffer(8 << 20)
		tc.SetReadBuffer(1 << 20)
		tc.SetNoDelay(false)
	}
	return c, nil
}

// mediaStream is the time-to-byte index of a remux that is still growing.
type mediaStream struct {
	mu       sync.Mutex
	samples  []streamSample
	size     int64
	estimate int64
	duration float64
	done     bool
	// sent is the highest file offset already written to a client.
	sent int64
	pace *sendPace
}

type streamSample struct {
	t float64
	n int64
}

func newMediaStream(duration float64) *mediaStream {
	return &mediaStream{duration: duration}
}

func (m *mediaStream) add(outSec float64, size int64) {
	if m == nil || outSec < 0 || size < 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.samples) > 0 && outSec < m.samples[len(m.samples)-1].t {
		return
	}
	m.samples = append(m.samples, streamSample{t: outSec, n: size})
	if size > m.size {
		m.size = size
	}
	if m.duration > 1 && outSec >= 0.5 && size > 0 {
		// A little high on purpose. A short estimate makes the television
		// treat the middle of the movie as the end and reopen the stream.
		est := int64(float64(size) / outSec * m.duration * 1.15)
		if est > m.estimate {
			m.estimate = est
		}
	}
}

func (m *mediaStream) finish(size int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.done = true
	if size > m.size {
		m.size = size
	}
	m.estimate = m.size
}

func (m *mediaStream) isDone() bool {
	if m == nil {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.done
}

func (m *mediaStream) hasEstimate() bool {
	if m == nil {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.estimate > 0 || m.done
}

func (m *mediaStream) reached(t float64) bool {
	if m == nil {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.done {
		return true
	}
	if len(m.samples) == 0 {
		return t <= 0
	}
	return m.samples[len(m.samples)-1].t+0.5 >= t
}

// byteFor returns the file offset of movie time t. The bool is false when
// the remux has not reached that time yet.
func (m *mediaStream) byteFor(t float64) (int64, bool) {
	if m == nil {
		return 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if t <= 0 {
		return 0, true
	}
	if len(m.samples) == 0 {
		return 0, m.done
	}
	if t < m.samples[0].t {
		return 0, true
	}
	var prev streamSample
	havePrev := false
	for _, s := range m.samples {
		if s.t >= t {
			if !havePrev || s.t == prev.t {
				return s.n, true
			}
			frac := (t - prev.t) / (s.t - prev.t)
			return prev.n + int64(frac*float64(s.n-prev.n)), true
		}
		prev = s
		havePrev = true
	}
	if m.done {
		return m.size, true
	}
	return 0, false
}

func (m *mediaStream) totals() (size, total int64, done bool) {
	if m == nil {
		return 0, 0, true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	total = m.size
	if m.estimate > total {
		total = m.estimate
	}
	return m.size, total, m.done
}

// noteSent records how far a client has actually been served. The remux
// pauses from this, not from how large the file has grown.
func (m *mediaStream) noteSent(off int64) {
	if m == nil || off <= 0 {
		return
	}
	m.mu.Lock()
	if off > m.sent {
		m.sent = off
	}
	m.mu.Unlock()
}

// tooFar reports that the file is more than about a minute of media ahead of
// the bytes already sent. Past that, the disk cache around the playhead is
// gone and both pictures hitch a few minutes into the movie.
func (m *mediaStream) tooFar(size int64) bool {
	if m == nil || m.isDone() {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return size-m.sent > m.leadLimitLocked()
}

// shouldStayPaused is the low-water mark. The copy stays stopped until the
// television has drained most of the lead, so it does not pause and resume
// on every packet.
func (m *mediaStream) shouldStayPaused(size int64) bool {
	if m == nil || m.isDone() {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lim := m.leadLimitLocked()
	resume := int64(float64(lim) * 0.4)
	if resume < 8<<20 {
		resume = 8 << 20
	}
	return size-m.sent > resume
}

func (m *mediaStream) leadLimitLocked() int64 {
	const (
		leadSeconds = 60
		floor       = 32 << 20
		ceil        = 128 << 20
	)
	if len(m.samples) == 0 {
		return floor
	}
	last := m.samples[len(m.samples)-1]
	if last.t < 0.5 || last.n <= 0 {
		return floor
	}
	lim := int64(float64(last.n) / last.t * leadSeconds)
	if lim < floor {
		return floor
	}
	if lim > ceil {
		return ceil
	}
	return lim
}

func serveMedia(w http.ResponseWriter, r *http.Request, path, contentType string, stream *mediaStream, pace *sendPace) {
	if npt, ok := parseTimeSeek(r.Header.Get("TimeSeekRange.dlna.org")); ok && stream != nil {
		serveTimeSeek(w, r, path, contentType, stream, npt)
		return
	}
	if stream != nil && !stream.isDone() {
		serveGrowing(w, r, path, contentType, stream)
		return
	}
	serveFile(w, r, path, contentType, pace)
}

func serveFile(w http.ResponseWriter, r *http.Request, path, contentType string, pace *sendPace) {
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("transferMode.dlna.org", "Interactive")
	w.Header().Set("contentFeatures.dlna.org", dlnaFeatures(contentType))
	var body io.ReadSeeker = f
	if cp := pace.connFrom(0); cp != nil {
		body = &pacedSeeker{rs: f, p: cp}
	}
	http.ServeContent(w, r, st.Name(), st.ModTime(), body)
}

func serveTimeSeek(w http.ResponseWriter, r *http.Request, path, contentType string, stream *mediaStream, npt float64) {
	deadline := time.Now().Add(20 * time.Second)
	var off int64
	var ok bool
	for {
		off, ok = stream.byteFor(npt)
		if ok || time.Now().After(deadline) || stream.isDone() {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if !ok {
		off, _ = stream.byteFor(npt)
	}
	if strings.Contains(contentType, "mp2t") {
		off -= off % 188
		if off < 0 {
			off = 0
		}
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	size, total, _ := stream.totals()
	if total < size {
		total = size
	}
	if total <= 0 {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	if off > size {
		off = size
	}
	end := total - 1
	if end < off {
		end = off
	}
	dur := stream.duration
	if dur < npt {
		dur = npt
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("transferMode.dlna.org", "Interactive")
	w.Header().Set("contentFeatures.dlna.org", dlnaFeatures(contentType))
	w.Header().Set("TimeSeekRange.dlna.org", fmt.Sprintf("npt=%s-%s/%s", formatNPT(npt), formatNPT(dur), formatNPT(dur)))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, end, total))
	w.Header().Set("Content-Length", strconv.FormatInt(end-off+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	streamCopy(w, f, stream, off, end)
}

func serveGrowing(w http.ResponseWriter, r *http.Request, path, contentType string, stream *mediaStream) {
	if stream.isDone() {
		serveFile(w, r, path, contentType, stream.pace)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	size, total, _ := stream.totals()
	if total < size {
		total = size
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("transferMode.dlna.org", "Interactive")
	w.Header().Set("contentFeatures.dlna.org", dlnaFeatures(contentType))
	if rh := r.Header.Get("Range"); strings.HasPrefix(rh, "bytes=") {
		start, end, follow, ok := growingRange(rh, size, total)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
			http.Error(w, "range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if follow {
			// The bytes after start are not all on disk. Ending the body at
			// the current size makes the television close, reopen and rewind
			// to a keyframe — the reload and the speed meter.
			w.WriteHeader(http.StatusOK)
			streamFollow(w, f, stream, start)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		streamCopy(w, f, stream, start, end)
		return
	}
	// No length: the file is still being written, and a short Content-Length
	// would make the TV treat the first seconds as the whole movie.
	w.WriteHeader(http.StatusOK)
	streamFollow(w, f, stream, 0)
}

// growingRange parses one bytes= range. follow is set when the requested
// end is open or past the bytes on disk: the handler keeps the body open
// as the remux grows instead of closing at the current end.
func growingRange(h string, size, total int64) (start, end int64, follow, ok bool) {
	spec := strings.TrimPrefix(h, "bytes=")
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = spec[:i]
	}
	a, b, _ := strings.Cut(spec, "-")
	if a == "" && b == "" {
		return 0, 0, false, false
	}
	if a != "" {
		n, err := strconv.ParseInt(a, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, false, false
		}
		start = n
	}
	if b != "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n < start {
			return 0, 0, false, false
		}
		end = n
	} else {
		follow = true
	}
	if total <= 0 {
		total = size
	}
	if start >= size && (total <= 0 || start >= total) {
		return 0, 0, false, false
	}
	if follow || end >= size {
		return start, end, true, true
	}
	return start, end, false, true
}

// streamFollow writes the file as it grows and returns when the remux
// finishes or the client goes away. Closing because the writer paused is
// what makes the television reopen the movie mid-way.
func streamFollow(w http.ResponseWriter, f *os.File, stream *mediaStream, off int64) {
	var pace *connPace
	if stream != nil {
		pace = stream.pace.connFrom(off)
	}
	buf := make([]byte, 256*1024)
	for {
		n, err := f.ReadAt(buf, off)
		if n > 0 {
			if pace == nil && stream != nil {
				pace = stream.pace.connFrom(off)
			}
			if pace != nil {
				pace.wait(off + int64(n))
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			off += int64(n)
			if stream != nil {
				stream.noteSent(off)
			}
			continue
		}
		if err == nil {
			continue
		}
		if stream == nil || stream.isDone() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// streamCopy writes [start, end] and waits when the read catches a remux
// that has not finished. An early end of a finished file closes the body;
// players treat that as the end of the stream.
func streamCopy(w http.ResponseWriter, f *os.File, stream *mediaStream, start, end int64) {
	var pace *connPace
	if stream != nil {
		pace = stream.pace.connFrom(start)
	}
	buf := make([]byte, 256*1024)
	off := start
	stalls := 0
	for off <= end {
		want := int64(len(buf))
		if remain := end - off + 1; remain < want {
			want = remain
		}
		n, err := f.ReadAt(buf[:want], off)
		if n > 0 {
			if pace != nil {
				pace.wait(off + int64(n))
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			off += int64(n)
			if stream != nil {
				stream.noteSent(off)
			}
			stalls = 0
			continue
		}
		if err == nil {
			continue
		}
		if stream == nil || stream.isDone() || stalls >= 600 {
			return
		}
		stalls++
		time.Sleep(50 * time.Millisecond)
	}
}

// sendPace keeps about ninety seconds of the movie ahead of the television.
// The window is a file offset taken from the container's own time index, not
// playhead × average bitrate: a hot stretch of a 4K file is more bytes than
// that average, and the television otherwise underruns many minutes in.
type sendPace struct {
	mu         sync.Mutex
	bps        float64
	idx        *byteIndex
	playhead   float64
	paused     bool
	playheadAt time.Time
	movedAt    time.Time
	haveHead   bool
}

func newSendPace() *sendPace {
	return &sendPace{}
}

func (p *sendPace) configure(bytes int64, seconds float64) {
	if p == nil || bytes <= 0 || seconds < 0.5 {
		return
	}
	bps := float64(bytes) / seconds
	if bps < 256*1024 {
		bps = 256 * 1024
	}
	p.mu.Lock()
	p.bps = bps
	p.mu.Unlock()
}

func (p *sendPace) setIndex(idx *byteIndex) {
	if p == nil || idx == nil || idx.len() == 0 {
		return
	}
	p.mu.Lock()
	p.idx = idx
	p.mu.Unlock()
}

// observe records how far a remux has got, which is the only time-to-byte
// map a growing MPEG-TS has.
func (p *sendPace) observe(t float64, n int64) {
	if p == nil || t < 0 || n <= 0 {
		return
	}
	p.configure(n, t)
	p.mu.Lock()
	if p.idx == nil {
		p.idx = &byteIndex{}
	}
	idx := p.idx
	p.mu.Unlock()
	idx.add(t, n)
}

func (p *sendPace) timeAt(off int64) (float64, bool) {
	if p == nil {
		return 0, false
	}
	p.mu.Lock()
	idx := p.idx
	bps := p.bps
	p.mu.Unlock()
	if idx != nil && idx.len() > 0 {
		return idx.timeAt(off)
	}
	if bps > 0 && off >= 0 {
		return float64(off) / bps, true
	}
	return 0, false
}

func (p *sendPace) byteAt(t float64) (int64, bool) {
	if p == nil {
		return 0, false
	}
	p.mu.Lock()
	idx := p.idx
	bps := p.bps
	p.mu.Unlock()
	if idx != nil && idx.len() > 0 {
		return idx.byteAt(t)
	}
	if bps > 0 && t >= 0 {
		return int64(t * bps), true
	}
	return 0, false
}

func (p *sendPace) rate() float64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bps
}

// setPlayhead is the television's clock. While it is paused the window does
// not grow, so a paused TV is not fed the rest of the file.
func (p *sendPace) setPlayhead(sec float64, paused bool) {
	if p == nil || sec < 0 {
		return
	}
	now := time.Now()
	p.mu.Lock()
	moved := !p.haveHead || paused != p.paused
	if !moved {
		d := sec - p.playhead
		if d < 0 {
			d = -d
		}
		moved = d >= 0.15
	}
	if moved || p.movedAt.IsZero() {
		p.movedAt = now
	}
	p.playhead = sec
	p.paused = paused
	p.playheadAt = now
	p.haveHead = true
	p.mu.Unlock()
}

func (p *sendPace) mediaNow() float64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.haveHead {
		return 0
	}
	if p.paused {
		return p.playhead
	}
	// Between polls the clock is only a second stale. A position that stops
	// moving while the TV is still playing is a stuck report: follow the wall
	// clock from the last real movement, or the buffer runs dry.
	extra := time.Since(p.playheadAt).Seconds()
	if extra < 0 {
		extra = 0
	}
	stuck := time.Since(p.movedAt).Seconds()
	if stuck > 3 {
		extra = stuck
	} else if extra > 1.5 {
		extra = 1.5
	}
	return p.playhead + extra
}

func (p *sendPace) connFrom(at int64) *connPace {
	if p == nil || p.rate() <= 0 {
		return nil
	}
	if at < 0 {
		at = 0
	}
	return &connPace{parent: p, from: at, opened: time.Now()}
}

type connPace struct {
	parent *sendPace
	from   int64
	opened time.Time
}

func (c *connPace) wait(absoluteEnd int64) {
	if c == nil || absoluteEnd <= 0 {
		return
	}
	now := c.parent.mediaNow()
	fromT, hasFrom := c.parent.timeAt(c.from)
	endT, hasEnd := c.parent.timeAt(absoluteEnd)
	if hasFrom && hasEnd {
		base := now
		if fromT > base {
			base = fromT
		}
		windowT := base + 90
		if b0, ok := c.parent.byteAt(base); ok {
			const maxAhead = 768 << 20
			if b1, ok := c.parent.byteAt(windowT); ok && b1 > b0+maxAhead {
				if t, ok := c.parent.timeAt(b0 + maxAhead); ok && t > base {
					windowT = t
				}
			}
		}
		if d := paceDelayAt(endT, windowT, fromT, time.Since(c.opened)); d > 0 {
			time.Sleep(d)
		}
		return
	}
	bps := c.parent.rate()
	if bps <= 0 {
		return
	}
	if start := float64(c.from) / bps; start > now {
		now = start
	}
	if d := paceDelay(absoluteEnd, bps, now, c.from, time.Since(c.opened)); d > 0 {
		time.Sleep(d)
	}
}

// paceDelayAt waits once the byte about to be sent belongs to a later moment
// than the playhead plus the buffer. The wait is in movie time, so a dense
// stretch is allowed its real number of bytes. A fresh response still fills
// that window at 8× rather than in one dump. The wait is capped so a stalled
// client is noticed.
func paceDelayAt(endTime, windowTime, fromTime float64, elapsed time.Duration) time.Duration {
	if endTime < 0 {
		endTime = 0
	}
	if windowTime < 0 {
		windowTime = 0
	}
	if fromTime < 0 {
		fromTime = 0
	}
	if elapsed < 0 {
		elapsed = 0
	}
	var wait time.Duration
	if endTime > windowTime {
		wait = time.Duration((endTime - windowTime) * float64(time.Second))
	}
	burstEnd := fromTime + 8*elapsed.Seconds()
	if endTime > burstEnd {
		d := time.Duration((endTime - burstEnd) / 8 * float64(time.Second))
		if d > wait {
			wait = d
		}
	}
	if wait > 200*time.Millisecond {
		return 200 * time.Millisecond
	}
	if wait < 0 {
		return 0
	}
	return wait
}

// paceDelay keeps about ninety seconds of the movie ahead of the television.
// A fresh response fills that window at 8× realtime, so a bitrate spike has
// a buffer and the opening is not one unbounded dump. Once the window is
// full, further bytes wait at 1× the playhead: waiting at 4× would walk
// minutes ahead again. The wait is capped so a stalled client is noticed.
func paceDelay(absoluteEnd int64, bps, mediaNow float64, from int64, elapsed time.Duration) time.Duration {
	if bps <= 0 || absoluteEnd <= 0 {
		return 0
	}
	if mediaNow < 0 {
		mediaNow = 0
	}
	if from < 0 {
		from = 0
	}
	if elapsed < 0 {
		elapsed = 0
	}
	window := int64(mediaNow*bps) + paceCushion(bps)
	burst := from + int64(bps*8*elapsed.Seconds())
	var wait time.Duration
	if absoluteEnd > window {
		over := float64(absoluteEnd-window) / bps
		wait = time.Duration(over * float64(time.Second))
	}
	if absoluteEnd > burst {
		over := float64(absoluteEnd-burst) / (bps * 8)
		d := time.Duration(over * float64(time.Second))
		if d > wait {
			wait = d
		}
	}
	if wait > 200*time.Millisecond {
		return 200 * time.Millisecond
	}
	if wait < 0 {
		return 0
	}
	return wait
}

func paceCushion(bps float64) int64 {
	c := int64(bps * 90)
	if c < 8<<20 {
		c = 8 << 20
	}
	if c > 192<<20 {
		c = 192 << 20
	}
	return c
}

type pacedSeeker struct {
	rs  io.ReadSeeker
	p   *connPace
	off int64
}

func (s *pacedSeeker) Read(b []byte) (int, error) {
	n, err := s.rs.Read(b)
	if n > 0 {
		s.off += int64(n)
		if s.p != nil {
			s.p.wait(s.off)
		}
	}
	return n, err
}

func (s *pacedSeeker) Seek(off int64, whence int) (int64, error) {
	n, err := s.rs.Seek(off, whence)
	if err == nil {
		s.off = n
		if s.p != nil {
			s.p.from = n
			s.p.opened = time.Now()
		}
	}
	return n, err
}

func formatNPT(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	ms := int(sec*1000) % 1000
	if ms < 0 {
		ms = 0
	}
	return formatRelTime(sec) + fmt.Sprintf(".%03d", ms)
}
