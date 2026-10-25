package main

import (
	"context"
	"flag"
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

// serveDLNA hands one URL to a DLNA renderer and leaves the helper up as
// the file server until IINA quits or the cast is stopped. A file the TV
// can play directly is served from its original path, so open, pause and
// seek never copy it. Anything else is stream-copied to MPEG-TS and the
// TV is told the URL as soon as the first bytes exist.
func serveDLNA(c JobConfig, parent int, ipOverride, control, title string, start float64, startPaused bool, pidfile string, setStopJob func(func()), fail func(string)) {
	control, err := validateControlURL(control)
	if err != nil {
		fail(err.Error())
	}
	if strings.TrimSpace(title) == "" {
		title = strings.TrimSuffix(filepath.Base(c.Source), filepath.Ext(c.Source))
	}

	subPath := prepareSubtitle(c)
	if subPath != "" {
		c.SubFile = subPath
	}
	// A subtitle is a sidecar. It must not force a copy of the whole movie.
	direct, mime := planDirect(c.Source, c.VCodec, c.ACodec, c.ForceRemux)

	// base stays zero. The file's first second is the movie's first second,
	// and every seek is IINA's time. Clipping the head is what made the TV
	// show 35:06 when IINA was at 56:23.
	base := 0.0
	mediaPath := c.Source
	var stream *mediaStream
	pace := newSendPace()
	if direct {
		if idx, err := buildFileIndex(c.Source); err != nil {
			fmt.Fprintf(os.Stderr, "index: %v\n", err)
		} else {
			pace.setIndex(idx)
			fmt.Fprintf(os.Stderr, "index: %d points\n", idx.len())
		}
	}
	if !direct {
		mediaPath = filepath.Join(c.OutDir, "media.ts")
		mime = "video/mp2t"
		stream = newMediaStream(c.Duration)
	} else if c.Duration > 1 {
		if fi, statErr := os.Stat(c.Source); statErr == nil {
			pace.configure(fi.Size(), c.Duration)
		}
	}
	var port int
	var shutdown func()
	port, shutdown, err = startMediaServer(mediaPath, mime, subPath, stream, pace)
	if err != nil {
		fail(err.Error())
	}

	ip, err := LanIP(ipOverride)
	if err != nil {
		shutdown()
		fail(err.Error())
	}
	mediaURL := fmt.Sprintf("http://%s:%d/media", ip, port)
	subURL := ""
	if subPath != "" {
		subURL = fmt.Sprintf("http://%s:%d/sub.srt", ip, port)
	}

	var jobStop func()
	setStopJob(func() {
		if jobStop != nil {
			jobStop()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	if !direct {
		c.ForDLNA = true
		th := &procThrottle{}
		c.Throttle = th
		c.OnProgress = func(t float64) {
			fi, statErr := os.Stat(mediaPath)
			if statErr != nil {
				return
			}
			stream.add(t, fi.Size())
			pace.observe(t, fi.Size())
			if !stream.tooFar(fi.Size()) {
				return
			}
			th.hold(func() bool {
				n := fi.Size()
				if fi2, err2 := os.Stat(mediaPath); err2 == nil {
					n = fi2.Size()
				}
				return stream.shouldStayPaused(n)
			}, stream.isDone, ctx)
		}
		var jobDone <-chan error
		jobStop, jobDone = RunJob(c, os.Stdout)
		go func() {
			err := <-jobDone
			var n int64
			if fi, statErr := os.Stat(mediaPath); statErr == nil {
				n = fi.Size()
			}
			stream.finish(n)
			if err != nil {
				fmt.Fprintf(os.Stderr, "remux: %v\n", err)
			}
		}()
	}

	var once sync.Once
	// A track change writes "hold" before replacing this process. Pausing
	// leaves the last frame up; Stop would blank the TV for the whole copy.
	stopTV := func() {
		sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer scancel()
		hold := filepath.Join(c.OutDir, "hold")
		if _, err := os.Stat(hold); err == nil {
			os.Remove(hold)
			avAction(sctx, control, "Pause", [][2]string{{"InstanceID", "0"}})
			return
		}
		dlnaStop(sctx, control)
	}
	teardown := func() {
		once.Do(func() {
			cancel()
			stopTV()
			if jobStop != nil {
				jobStop()
			}
			shutdown()
			cleanOutDir(c.OutDir)
		})
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	WatchParent(parent, func() {
		teardown()
		os.Remove(pidfile)
		os.Exit(0)
	})

	if !direct {
		// Eight megabytes, or eight seconds, whichever comes first. Starting
		// on the first quarter-megabyte is why the opening stutters: the
		// television runs out of data before the next packets arrive.
		if err := waitStream(mediaPath, stream, 8<<20, 30*time.Second); err != nil {
			teardown()
			fail(err.Error())
		}
	}

	playCtx, playCancel := context.WithTimeout(context.Background(), 20*time.Second)
	var size int64
	// A growing remux must not advertise its current size: the TV would
	// treat that as the whole movie and land a seek early. Duration in the
	// DIDL is the movie length. A finished or direct file advertises its size.
	if stream == nil || stream.isDone() {
		if fi, statErr := os.Stat(mediaPath); statErr == nil {
			size = fi.Size()
		}
	}
	err = dlnaPlay(playCtx, control, mediaURL, didlLite(title, mediaURL, mime, size, subURL, c.Duration))
	playCancel()
	if err != nil {
		teardown()
		fail("DLNA: " + err.Error())
	}
	// Seek to IINA's time, not to a position inside a clipped copy. Pausing
	// before the seek is what leaves the TV at 0:00: a renderer ignores
	// REL_TIME until it is actually playing, and a pause at the first frame
	// wins the race. landed is emitted only after the TV clock matches.
	if start >= 1 {
		go func() {
			if stream != nil {
				deadline := time.Now().Add(3 * time.Minute)
				for time.Now().Before(deadline) && !stream.reached(start) && !stream.isDone() {
					select {
					case <-ctx.Done():
						return
					case <-time.After(80 * time.Millisecond):
					}
				}
			}
			if ctx.Err() != nil {
				return
			}
			ok := parkPlayback(ctx, control, start, startPaused)
			Emit(os.Stdout, "landed", map[string]any{"pos": start, "ok": ok})
		}()
	} else {
		if startPaused {
			pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Second)
			avAction(pctx, control, "Pause", [][2]string{{"InstanceID", "0"}})
			pcancel()
		}
		Emit(os.Stdout, "landed", map[string]any{"pos": start, "ok": true})
	}
	Emit(os.Stdout, "ready", map[string]any{"url": mediaURL, "base": base})

	go pollTransport(ctx, control, os.Stdout, base, pace)

	<-sigs
	teardown()
	Emit(os.Stdout, "stopped", nil)
}

// prepareSubtitle returns a file the server can hand out as /sub.srt.
// An external .srt is served from where it is, so stopping the cast cannot
// delete the user's subtitle. Anything else text-based is converted into
// the output directory. A failure here still casts the video.
func prepareSubtitle(c JobConfig) string {
	if c.SubPath == "" && c.SubMap == "" {
		return ""
	}
	if c.SubPath != "" && strings.EqualFold(extOf(c.SubPath), ".srt") {
		if _, err := os.Stat(c.SubPath); err == nil {
			return c.SubPath
		}
		return ""
	}
	dest := filepath.Join(c.OutDir, "sub.srt")
	args := []string{"-y", "-hide_banner", "-loglevel", "error"}
	if c.SubPath != "" {
		args = append(args, "-i", c.SubPath, "-map", "0:0")
	} else {
		args = append(args, "-i", c.Source, "-map", "0:"+c.SubMap)
	}
	args = append(args, "-c:s", "srt", dest)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.FFmpeg, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "subtitle extract: %v %s\n", err, strings.TrimSpace(string(out)))
		os.Remove(dest)
		return ""
	}
	return dest
}

func runDiscover(argv []string) {
	fs := flag.NewFlagSet("dlna-discover", flag.ExitOnError)
	ipFlag := fs.String("ip", "", "LAN IPv4 address to search from")
	fs.Parse(argv)
	ip := strings.TrimSpace(*ipFlag)
	if ip == "" {
		got, err := LanIP("")
		if err != nil {
			Emit(os.Stdout, "error", map[string]any{"msg": err.Error()})
			os.Exit(1)
		}
		ip = got
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	var devices []Device
	var discErr error
	var air []AirPlay
	var casts []CastDevice
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		devices, discErr = DiscoverRenderers(ctx, ip)
	}()
	go func() {
		defer wg.Done()
		air = browseAirPlay(ctx)
	}()
	go func() {
		defer wg.Done()
		casts = browseCast(ctx)
	}()
	wg.Wait()
	if discErr != nil {
		Emit(os.Stdout, "error", map[string]any{"msg": discErr.Error()})
		os.Exit(1)
	}
	if devices == nil {
		devices = []Device{}
	}
	if air == nil {
		air = []AirPlay{}
	}
	if casts == nil {
		casts = []CastDevice{}
	}
	Emit(os.Stdout, "devices", map[string]any{"devices": devices, "airplay": air, "cast": casts})
}

func runCtl(argv []string) {
	fs := flag.NewFlagSet("dlna-ctl", flag.ExitOnError)
	control := fs.String("control", "", "AVTransport control URL")
	action := fs.String("action", "", "play, pause, seek or stop")
	pos := fs.Float64("pos", 0, "seek target in seconds")
	fs.Parse(argv)
	ctl, err := validateControlURL(*control)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	switch *action {
	case "play":
		_, err = avAction(ctx, ctl, "Play", [][2]string{{"InstanceID", "0"}, {"Speed", "1"}})
	case "pause":
		_, err = avAction(ctx, ctl, "Pause", [][2]string{{"InstanceID", "0"}})
	case "stop":
		err = dlnaStop(ctx, ctl)
	case "seek":
		err = dlnaSeek(ctx, ctl, *pos)
	default:
		err = fmt.Errorf("unknown DLNA action %q", *action)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func waitStream(path string, stream *mediaStream, min int64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		fi, err := os.Stat(path)
		have := err == nil && fi.Size() > 0
		paced := err == nil && stream.reached(8) && fi.Size() >= 1<<20 && (stream.hasEstimate() || stream.duration == 0)
		enough := paced || (err == nil && fi.Size() >= min && (stream.hasEstimate() || stream.duration == 0))
		if enough || (have && stream.isDone()) {
			return nil
		}
		if stream.isDone() {
			return fmt.Errorf("remux produced no media")
		}
		if time.Now().After(deadline) {
			if have {
				return nil
			}
			return fmt.Errorf("timed out preparing the stream")
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// parkPlayback seeks the renderer to start and, when pause is set, leaves it
// paused there. A seek issued while the transport is still stopped is
// ignored, which is why the TV otherwise opens the movie at 0:00. The clock
// is read back and the seek is repeated until it matches.
func parkPlayback(ctx context.Context, control string, start float64, pause bool) bool {
	deadline := time.Now().Add(15 * time.Second)
	attempt := 0
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		target := formatRelTime(start)
		if attempt%2 == 1 {
			target = formatRelTimePadded(start)
		}
		attempt++
		sctx, scancel := context.WithTimeout(ctx, 4*time.Second)
		avAction(sctx, control, "Seek", [][2]string{
			{"InstanceID", "0"},
			{"Unit", "REL_TIME"},
			{"Target", target},
		})
		scancel()
		select {
		case <-ctx.Done():
			return false
		case <-time.After(350 * time.Millisecond):
		}
		rctx, rcancel := context.WithTimeout(ctx, 3*time.Second)
		tr, _, err := readTransport(rctx, control, true)
		rcancel()
		if err != nil || !tr.PosOK || math.Abs(tr.Pos-start) > 2.5 {
			continue
		}
		if !pause || tr.Paused {
			return true
		}
		pctx, pcancel := context.WithTimeout(ctx, 4*time.Second)
		avAction(pctx, control, "Pause", [][2]string{{"InstanceID", "0"}})
		pcancel()
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
		rctx, rcancel = context.WithTimeout(ctx, 3*time.Second)
		tr, _, err = readTransport(rctx, control, true)
		rcancel()
		if err == nil && tr.PosOK && math.Abs(tr.Pos-start) <= 2.5 {
			return true
		}
		// Pause dropped the clock. Loop and seek again.
	}
	return false
}

func pollTransport(ctx context.Context, control string, out io.Writer, base float64, pace *sendPace) {
	seen := false
	// Fast at the start so the player can join the moment the TV actually
	// starts, not a second after it. Steady state does not need that rate.
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	slow := time.NewTimer(20 * time.Second)
	defer slow.Stop()
	var last Transport
	var haveLast bool
	abs := func(rel float64) float64 { return base + rel }
	for {
		select {
		case <-ctx.Done():
			return
		case <-slow.C:
			tick.Reset(time.Second)
			continue
		case <-tick.C:
			pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			tr, seenNow, err := readTransport(pctx, control, seen)
			cancel()
			seen = seenNow
			if err != nil {
				continue
			}
			if tr.Ended {
				pos := abs(tr.Pos)
				if !tr.PosOK && haveLast {
					pos = abs(last.Pos)
				}
				Emit(out, "tv", map[string]any{"pos": pos, "paused": true, "ended": true})
				return
			}
			if tr.PosOK && pace != nil {
				pace.setPlayhead(abs(tr.Pos), tr.Paused)
			}
			if !tr.PosOK {
				continue
			}
			if haveLast && tr == last {
				continue
			}
			last, haveLast = tr, true
			Emit(out, "tv", map[string]any{
				"pos": abs(tr.Pos), "paused": tr.Paused, "ended": false,
			})
		}
	}
}
