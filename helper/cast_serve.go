package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// castLead is how much of the movie the first file holds. It is long enough
// that the rest of the remux, which continues in the background, is ready
// before this piece ends, and short enough that the television is playing
// within a few seconds.
const castLead = 240.0

// castPlayhead is the movie time the Chromecast is actually at. ffmpeg is
// paused once the playlist is three minutes ahead of it, and resumed when
// a seek moves the playhead forward.
type castPlayhead struct {
	mu  sync.Mutex
	pos float64
	out float64
}

func (h *castPlayhead) setPos(v float64) {
	h.mu.Lock()
	h.pos = v
	h.mu.Unlock()
}

func (h *castPlayhead) setOut(v float64) {
	h.mu.Lock()
	h.out = v
	h.mu.Unlock()
}

func (h *castPlayhead) ahead() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.out > h.pos+180
}

func nearCast(got, want float64) bool {
	if want < 1 {
		return got < 4
	}
	return math.Abs(got-want) <= 2.5
}

func holdRequested(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "hold"))
	return err == nil
}

// waitForCastMedia blocks until the remux can be handed to the receiver.
// Playback from the start can begin as soon as the first fragment exists.
// A later start waits until ffmpeg has finished the file: a fragmented MP4
// that is still growing ignores currentTime and plays from zero.
func waitForCastMedia(ctx context.Context, stream *mediaStream, start float64, done <-chan error, onDone func()) (loadAt float64, consumed bool, err error) {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	born := time.Now()
	for {
		if ctx.Err() != nil {
			return 0, false, ctx.Err()
		}
		if start <= 1 && stream.reached(1) {
			return 0, false, nil
		}
		select {
		case <-ctx.Done():
			return 0, false, ctx.Err()
		case jobErr := <-done:
			if onDone != nil {
				onDone()
			}
			if jobErr != nil && (start > 1 || !stream.reached(1)) {
				return 0, true, jobErr
			}
			if start <= 1 {
				return 0, true, nil
			}
			return start, true, nil
		case <-tick.C:
			if time.Since(born) > 30*time.Minute {
				return 0, false, fmt.Errorf("chromecast stream was not ready in time")
			}
		}
	}
}

// castSubtitle writes a WebVTT sidecar. The receiver plays the video as one
// MP4 and takes subtitles as a separate text track. A failure still casts
// the picture.
func castSubtitle(c JobConfig) string {
	if !c.HasSub() {
		return ""
	}
	dest := filepath.Join(c.OutDir, "sub.vtt")
	src := prepareSubtitle(c)
	if src == "" {
		return ""
	}
	if strings.EqualFold(extOf(src), ".vtt") {
		if src == dest {
			return dest
		}
		b, rerr := os.ReadFile(src)
		if rerr != nil {
			return ""
		}
		if werr := os.WriteFile(dest, b, 0o644); werr != nil {
			return ""
		}
		return dest
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.FFmpeg, "-y", "-hide_banner", "-loglevel", "error", "-i", src, "-map", "0:0", "-c:s", "webvtt", dest)
	if out, rerr := cmd.CombinedOutput(); rerr != nil {
		fmt.Fprintf(os.Stderr, "subtitle: %v %s\n", rerr, strings.TrimSpace(string(out)))
		os.Remove(dest)
		return ""
	}
	return dest
}

// waitForCastPlaylist blocks until the media playlist covers start, or until
// the encode has finished and this is all there will be. The clock is the
// movie clock: the playlist starts at 0:00 of the file, and start is a
// position in that file. Returned at is the currentTime to hand the device.
func waitForCastPlaylist(ctx context.Context, index string, start float64, ready func() bool, done <-chan error, onReady func()) (float64, error) {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	born := time.Now()
	var announced bool
	var finished bool
	var finErr error
	note := func() {
		if announced || !ready() || onReady == nil {
			return
		}
		announced = true
		onReady()
	}
	for {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		note()
		if ready() {
			covered := playlistSeconds(index)
			// An empty playlist (the header ffmpeg writes before the first
			// segment) makes the receiver report an error and stop. Wait
			// until a segment is actually listed.
			if covered > 0 && (start <= 1 || covered+1.5 >= start) {
				if start <= 1 {
					return 0, nil
				}
				return start, nil
			}
			if finished && covered <= 0 {
				if finErr != nil {
					return 0, finErr
				}
				return 0, fmt.Errorf("ffmpeg finished but produced no playlist")
			}
			if finished && covered+1.5 < start {
				fmt.Fprintf(os.Stderr, "cast: playlist covers %.1fs, wanted %.1fs\n", covered, start)
				return covered, nil
			}
			if time.Since(born) > 20*time.Minute {
				if covered > 1 {
					return covered, nil
				}
				return 0, fmt.Errorf("chromecast stream was not ready in time")
			}
		} else if finished {
			if finErr != nil {
				return 0, finErr
			}
			return 0, fmt.Errorf("ffmpeg finished but produced no playlist")
		} else if time.Since(born) > 120*time.Second {
			return 0, fmt.Errorf("packaging produced no playlist within 120s")
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case err := <-done:
			finished = true
			finErr = err
		case <-tick.C:
		}
	}
}

func parkCast(ctx context.Context, link *castLink, target float64, pause bool) bool {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		link.poll(ctx)
		p := link.playback()
		if pause && p.State == "PLAYING" {
			_ = link.command(ctx, "PAUSE", nil)
		}
		if !pause && p.State == "PAUSED" {
			_ = link.command(ctx, "PLAY", nil)
		}
		p = link.playback()
		ready := (pause && p.State == "PAUSED") || (!pause && p.State == "PLAYING")
		if ready && nearCast(p.Time, target) {
			return true
		}
		if (p.State == "PAUSED" || p.State == "PLAYING" || p.State == "BUFFERING") && !nearCast(p.Time, target) {
			_ = link.command(ctx, "SEEK", map[string]any{"currentTime": target})
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(300 * time.Millisecond):
		}
	}
	return false
}

func serveCast(c JobConfig, parent int, ipOverride, title, host, model string, port int, startAt float64, startPaused bool, pidfile string, setStopJob func(func()), fail func(string)) {
	ipAddr, err := castIPv4(host)
	if err != nil {
		fail(err.Error())
	}
	if port <= 0 || port > 65535 {
		port = 8009
	}
	if title == "" {
		title = "Video"
	}
	c.CastModel = model
	c.ForCast = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The first file is a few minutes from where IINA is, so the television
	// can start. The rest of the movie is remuxed alongside it and replaces
	// the first file once it is a complete faststart MP4.
	short := c
	short.StartAt = startAt
	short.CastFile = "now.mp4"
	whole := c.Duration > 1 && startAt+castLead >= c.Duration-1
	if !whole {
		short.CastLimit = castLead
	}
	portHTTP, httpStop, err := StartServer(c.OutDir, c.SubName, c.SubLang, 0)
	if err != nil {
		fail(err.Error())
	}
	ip, err := LanIP(ipOverride)
	if err != nil {
		httpStop()
		fail(err.Error())
	}
	nowURL := fmt.Sprintf("http://%s:%d/now.mp4", ip, portHTTP)
	restURL := fmt.Sprintf("http://%s:%d/rest.mp4", ip, portHTTP)
	subURL := ""
	if c.HasSub() {
		subURL = fmt.Sprintf("http://%s:%d/sub.vtt", ip, portHTTP)
	}

	head := &castPlayhead{pos: startAt}
	subReady := make(chan string, 1)
	go func() { subReady <- castSubtitle(c) }()

	var stopShort, stopLong func()
	setStopJob(func() {
		if stopShort != nil {
			stopShort()
		}
		if stopLong != nil {
			stopLong()
		}
	})
	var jobDone <-chan error
	stopShort, jobDone = RunJob(short, os.Stdout)
	var longDone <-chan error

	var (
		linkMu sync.Mutex
		link   *castLink
	)
	current := func() *castLink {
		linkMu.Lock()
		defer linkMu.Unlock()
		return link
	}
	var once sync.Once
	teardown := func() {
		once.Do(func() {
			cancel()
			if l := current(); l != nil {
				l.shutdown(holdRequested(c.OutDir))
			}
			if stopShort != nil {
				stopShort()
			}
			if stopLong != nil {
				stopLong()
			}
			httpStop()
			cleanOutDir(c.OutDir)
		})
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()
	WatchParent(parent, func() {
		teardown()
		os.Remove(pidfile)
		os.Exit(0)
	})

	stopped := func() bool { return ctx.Err() != nil }
	bail := func(err error) {
		if stopped() {
			teardown()
			Emit(os.Stdout, "stopped", nil)
			return
		}
		teardown()
		fail(err.Error())
	}

	Emit(os.Stdout, "ready", map[string]any{"url": nowURL, "base": 0})

	type dialResult struct {
		link *castLink
		err  error
	}
	dialed := make(chan dialResult, 1)
	go func() {
		opened, err := dialCast(ctx, ipAddr, port)
		if err != nil {
			dialed <- dialResult{err: fmt.Errorf("cannot connect to the Chromecast: %w", err)}
			return
		}
		linkMu.Lock()
		link = opened
		linkMu.Unlock()
		if err := opened.launch(ctx); err != nil {
			dialed <- dialResult{err: err}
			return
		}
		dialed <- dialResult{link: opened}
	}()

	// A faststart MP4 is not playable until ffmpeg has moved the index to
	// the front, which it does as it exits. The lead is a few minutes, so
	// this is seconds, not the length of the movie.
	var shortErr error
	select {
	case shortErr = <-jobDone:
		jobDone = nil
	case <-ctx.Done():
		teardown()
		Emit(os.Stdout, "stopped", nil)
		return
	}
	if shortErr != nil {
		bail(shortErr)
		return
	}
	if !whole {
		rest := c
		rest.StartAt = startAt
		rest.CastFile = "rest.mp4"
		stopLong, longDone = RunJob(rest, io.Discard)
	}
	if stopped() {
		teardown()
		Emit(os.Stdout, "stopped", nil)
		return
	}
	var opened *castLink
	select {
	case res := <-dialed:
		if res.err != nil {
			bail(res.err)
			return
		}
		opened = res.link
	case <-ctx.Done():
		teardown()
		Emit(os.Stdout, "stopped", nil)
		return
	}
	subPath := <-subReady
	loadSub := ""
	if subPath != "" {
		loadSub = subURL
	}
	leadDur := c.Duration
	if !whole {
		leadDur = startAt + castLead
	}
	if err := opened.load(ctx, castLoad{
		URL: nowURL, Title: title, Start: startAt, Duration: leadDur,
		Autoplay: !startPaused, ContentType: "video/mp4",
		SubURL: loadSub, SubLang: c.SubLang, SubName: c.SubName,
	}); err != nil {
		bail(err)
		return
	}
	if stopped() {
		teardown()
		Emit(os.Stdout, "stopped", nil)
		return
	}
	ok := parkCast(ctx, opened, startAt, startPaused)
	if stopped() {
		teardown()
		Emit(os.Stdout, "stopped", nil)
		return
	}
	Emit(os.Stdout, "landed", map[string]any{"pos": opened.playback().Time, "ok": ok})
	p := opened.playback()
	if p.State != "" {
		Emit(os.Stdout, "tv", map[string]any{"pos": p.pos(time.Now()), "paused": p.paused(), "ended": false})
	}

	cmds := &cmdReader{path: filepath.Join(c.OutDir, "cast.cmd")}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	nextStatus := time.Now()
	slowAfter := time.Now().Add(20 * time.Second)
	switched := whole
	ignoreIdleUntil := time.Time{}
	for {
		select {
		case <-ctx.Done():
			teardown()
			Emit(os.Stdout, "stopped", nil)
			return
		case err := <-jobDone:
			if err != nil {
				fmt.Fprintf(os.Stderr, "cast: %v\n", err)
			}
			jobDone = nil
		case err := <-longDone:
			longDone = nil
			if err != nil {
				fmt.Fprintf(os.Stderr, "cast rest: %v\n", err)
				break
			}
			pos := opened.playback().Time
			if pos < 1 {
				pos = startAt
			}
			if lerr := opened.load(ctx, castLoad{
				URL: restURL, Title: title, Start: pos, Duration: c.Duration,
				Autoplay: opened.playback().State != "PAUSED", ContentType: "video/mp4",
				SubURL: loadSub, SubLang: c.SubLang, SubName: c.SubName,
			}); lerr != nil {
				fmt.Fprintf(os.Stderr, "cast rest: %v\n", lerr)
				break
			}
			switched = true
			ignoreIdleUntil = time.Now().Add(4 * time.Second)
		case <-tick.C:
			stop := false
			for _, line := range cmds.poll() {
				action, pos, parsed := parseCastCmd(line)
				if !parsed {
					continue
				}
				if action == "stop" {
					stop = true
					break
				}
				switch action {
				case "play":
					if err := opened.command(ctx, "PLAY", nil); err != nil {
						fmt.Fprintf(os.Stderr, "cast play: %v\n", err)
					}
				case "pause":
					if err := opened.command(ctx, "PAUSE", nil); err != nil {
						fmt.Fprintf(os.Stderr, "cast pause: %v\n", err)
					}
				case "seek":
					head.setPos(pos)
					if err := opened.command(ctx, "SEEK", map[string]any{"currentTime": pos}); err != nil {
						fmt.Fprintf(os.Stderr, "cast seek: %v\n", err)
					}
				}
			}
			if stop {
				teardown()
				Emit(os.Stdout, "stopped", nil)
				return
			}
			if time.Now().Before(nextStatus) {
				continue
			}
			if time.Now().After(slowAfter) {
				nextStatus = time.Now().Add(time.Second)
			} else {
				nextStatus = time.Now().Add(200 * time.Millisecond)
			}
			opened.poll(ctx)
			p := opened.playback()
			if p.State == "IDLE" && time.Now().Before(ignoreIdleUntil) {
				continue
			}
			if p.State == "IDLE" && p.Idle == "ERROR" {
				teardown()
				fail("Chromecast stopped playing the stream")
			}
			if p.State == "IDLE" && (p.Idle == "FINISHED" || p.Idle == "CANCELLED") {
				// The opening piece ran out before the rest of the movie was
				// ready. Keep the session; the rest replaces it when it lands.
				if !switched && longDone != nil {
					continue
				}
				Emit(os.Stdout, "tv", map[string]any{"pos": p.Time, "paused": true, "ended": true})
				teardown()
				Emit(os.Stdout, "stopped", nil)
				return
			}
			if p.State == "" || p.State == "BUFFERING" {
				continue
			}
			pos := p.pos(time.Now())
			head.setPos(pos)
			Emit(os.Stdout, "tv", map[string]any{"pos": pos, "paused": p.paused(), "ended": false})
		}
	}
}
