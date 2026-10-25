package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerMIMEAndRange(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte("#EXTM3U\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "seg_0000.m4s"), []byte("0123456789"), 0o644)

	port, shutdown, err := StartServer(dir, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	resp, err := http.Get(base + "/index.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Fatalf("m3u8 content-type = %q", ct)
	}

	req, _ := http.NewRequest("GET", base+"/seg_0000.m4s", nil)
	req.Header.Set("Range", "bytes=2-4")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusPartialContent || string(body) != "234" {
		t.Fatalf("range request: status=%d body=%q", resp2.StatusCode, body)
	}
	if ct := resp2.Header.Get("Content-Type"); ct != "video/iso.segment" {
		t.Fatalf("m4s content-type = %q", ct)
	}
}

func TestServerRewritesMasterPlaylist(t *testing.T) {
	dir := t.TempDir()
	master := "#EXTM3U\n#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"default\",NAME=\"subtitle_0\",DEFAULT=NO,AUTOSELECT=YES,URI=\"index_vtt.m3u8\"\nindex.m3u8\n"
	os.WriteFile(filepath.Join(dir, "master.m3u8"), []byte(master), 0o644)
	os.WriteFile(filepath.Join(dir, "seg_0000.vtt"), []byte("WEBVTT\n"), 0o644)

	port, shutdown, err := StartServer(dir, "English", "en", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	resp, err := http.Get(base + "/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Fatalf("master content-type = %q", ct)
	}
	s := string(body)
	if !strings.Contains(s, "DEFAULT=YES") || !strings.Contains(s, `NAME="English"`) || !strings.Contains(s, `LANGUAGE="en"`) {
		t.Fatalf("master not rewritten:\n%s", s)
	}

	resp2, err := http.Get(base + "/seg_0000.vtt")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if ct := resp2.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/vtt") {
		t.Fatalf("vtt content-type = %q", ct)
	}
}

func TestServerMissingMasterFallsThrough(t *testing.T) {
	dir := t.TempDir()
	port, shutdown, err := StartServer(dir, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/master.m3u8", port))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing master should 404, got %d", resp.StatusCode)
	}
}

// Issue #26: while ffmpeg is still packaging a stream copy, its master.m3u8
// names the subtitle rendition but no variant, and AVFoundation (WebKit's
// <video>, the Apple TV) rejects it outright. The served master must carry a
// variant from the first request.
func TestServerAddsVariantToIncompleteMaster(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "master.m3u8"), []byte(ffmpegMasterNoVariant), 0o644)

	port, shutdown, err := StartServer(dir, "English", "en", 7_000_000)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown()

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/master.m3u8", port))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	s := string(body)
	if !strings.Contains(s, "#EXT-X-STREAM-INF:BANDWIDTH=7000000,SUBTITLES=\"subs\"\nindex.m3u8\n") {
		t.Fatalf("served master has no variant:\n%s", s)
	}
	if !strings.Contains(s, `NAME="English"`) {
		t.Fatalf("subtitle rewrite must still apply:\n%s", s)
	}
}

func TestStreamPausesWhenFarAheadOfTheClient(t *testing.T) {
	s := newMediaStream(3600)
	s.add(10, 10_000_000) // 1 MB/s → about 60MB of lead
	if s.tooFar(50 << 20) {
		t.Fatal("50MB is inside a minute of this stream")
	}
	if !s.tooFar(90 << 20) {
		t.Fatal("90MB ahead of a client that has read nothing must pause the copy")
	}
	s.noteSent(40 << 20)
	if s.tooFar(90 << 20) {
		t.Fatal("pause only while more than a minute ahead of bytes actually sent")
	}
	if !s.shouldStayPaused(90 << 20) {
		t.Fatal("the copy stays paused until the television has drained the lead")
	}
	s.noteSent(80 << 20)
	if s.shouldStayPaused(90 << 20) {
		t.Fatal("once the lead has drained, the copy must resume")
	}

	fast := newMediaStream(7200)
	fast.add(1, 200<<20) // 200 MB/s would be minutes of RAM if it were not capped
	if !fast.tooFar(200 << 20) {
		t.Fatal("a high bitrate remux must not stay hundreds of megabytes ahead")
	}
}

func TestPaceKeepsAMinuteAheadOfThePlayhead(t *testing.T) {
	const bps = 1_000_000.0
	cushion := paceCushion(bps)
	filled := time.Duration(float64(cushion)/bps/8*float64(time.Second)) + time.Millisecond
	if paceDelay(cushion, bps, 0, 0, 0) == 0 {
		t.Fatal("a fresh connection fills the buffer at 8×, not in one burst")
	}
	if paceDelay(cushion, bps, 0, 0, filled) != 0 {
		t.Fatal("once 8× has covered it, the opening buffer is allowed")
	}
	if paceDelay(cushion+5_000_000, bps, 0, 0, time.Hour) == 0 {
		t.Fatal("bytes past the buffer must wait even when the burst cap would allow them")
	}
	// Playhead at 100s: bytes already watched, plus the cushion, stay allowed.
	if paceDelay(int64(100*bps)+cushion, bps, 100, 0, time.Hour) != 0 {
		t.Fatal("the buffer ahead of the television is still inside the window")
	}
	d := paceDelay(int64(100*bps)+cushion+8<<20, bps, 100, 0, time.Hour)
	if d != 200*time.Millisecond {
		t.Fatalf("delay = %s, want the per-write cap", d)
	}
}

func TestPaceFollowsMovieTimeRatherThanTheAverage(t *testing.T) {
	// Opening half of the file is far denser than the movie-wide average.
	// 24 minutes in, average×time is still in the first half of the bytes.
	idx := newByteIndex([]timeByte{
		{0, 0},
		{24 * 60, 4_000_000_000},
		{150 * 60, 8_000_000_000},
	}, 8_000_000_000)
	p := newSendPace()
	p.setIndex(idx)
	p.configure(8_000_000_000, 150*60)
	p.setPlayhead(24*60, false)
	window, ok := p.byteAt(24*60 + 90)
	if !ok {
		t.Fatal("index")
	}
	average := int64((24*60 + 90) * (8_000_000_000 / (150 * 60)))
	if window <= average {
		t.Fatalf("window %d is not past the average estimate %d", window, average)
	}
	if paceDelayAt(24*60+90, 24*60+90, 0, time.Hour) != 0 {
		t.Fatal("the real byte of playhead+90s must be allowed")
	}
	if paceDelayAt(24*60+95, 24*60+90, 0, time.Hour) == 0 {
		t.Fatal("past that moment must wait")
	}
}

func TestFrozenPlayheadKeepsTheWindowMoving(t *testing.T) {
	p := newSendPace()
	p.setPlayhead(10, false)
	p.mu.Lock()
	p.movedAt = time.Now().Add(-20 * time.Second)
	p.playheadAt = time.Now()
	p.mu.Unlock()
	now := p.mediaNow()
	if now < 28 || now > 32 {
		t.Fatalf("frozen clock mediaNow=%v, want about 30", now)
	}
	p.setPlayhead(10, true)
	if got := p.mediaNow(); got != 10 {
		t.Fatalf("paused clock = %v, want 10", got)
	}
}

func TestGrowingRangeDoesNotEndAtTheBytesOnDisk(t *testing.T) {
	start, _, follow, ok := growingRange("bytes=100-", 500, 8000)
	if !ok || !follow || start != 100 {
		t.Fatalf("open range start=%d follow=%v ok=%v", start, follow, ok)
	}
	start, end, follow, ok := growingRange("bytes=0-9", 500, 8000)
	if !ok || follow || start != 0 || end != 9 {
		t.Fatalf("on-disk range start=%d end=%d follow=%v ok=%v", start, end, follow, ok)
	}
	start, _, follow, ok = growingRange("bytes=450-900", 500, 8000)
	if !ok || !follow || start != 450 {
		t.Fatalf("range past the writer must stay open: start=%d follow=%v ok=%v", start, follow, ok)
	}
}

func TestMediaServerRangeAndSubtitle(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "movie.mkv")
	sub := filepath.Join(dir, "movie.srt")
	os.WriteFile(media, []byte("0123456789"), 0o644)
	os.WriteFile(sub, []byte("1\n00:00:00,000 --> 00:00:01,000\nHi\n"), 0o644)

	port, shutdown, err := StartMediaServer(media, "video/x-matroska", sub)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	req, _ := http.NewRequest("GET", base+"/media", nil)
	req.Header.Set("Range", "bytes=0-3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(body) != "0123" {
		t.Fatalf("range: status=%d body=%q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/x-matroska" {
		t.Fatalf("content-type = %q", ct)
	}
	if resp.Header.Get("contentFeatures.dlna.org") == "" {
		t.Fatal("missing DLNA content features")
	}

	subResp, err := http.Get(base + "/sub.srt")
	if err != nil {
		t.Fatal(err)
	}
	subBody, _ := io.ReadAll(subResp.Body)
	subResp.Body.Close()
	if subResp.StatusCode != 200 || !strings.Contains(string(subBody), "Hi") {
		t.Fatalf("subtitle: status=%d body=%q", subResp.StatusCode, subBody)
	}
	stream := newMediaStream(10)
	stream.add(0, 0)
	stream.add(10, 10)
	stream.finish(10)
	port2, shutdown2, err := StartGrowingMediaServer(media, "video/mp4", "", stream)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown2()
	treq, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/media", port2), nil)
	treq.Header.Set("TimeSeekRange.dlna.org", "npt=0:00:05.000-")
	tresp, err := http.DefaultClient.Do(treq)
	if err != nil {
		t.Fatal(err)
	}
	tbody, _ := io.ReadAll(tresp.Body)
	tresp.Body.Close()
	if tresp.StatusCode != http.StatusPartialContent {
		t.Fatalf("time seek status=%d", tresp.StatusCode)
	}
	if !strings.HasPrefix(tresp.Header.Get("TimeSeekRange.dlna.org"), "npt=0:00:05.000-") {
		t.Fatalf("time seek header %q", tresp.Header.Get("TimeSeekRange.dlna.org"))
	}
	if string(tbody) != "56789" {
		t.Fatalf("time seek body %q", tbody)
	}

	other, err := http.Get(base + "/index.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	other.Body.Close()
	if other.StatusCode != http.StatusNotFound {
		t.Fatalf("unexpected path status %d", other.StatusCode)
	}
}
