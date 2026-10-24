package main

import (
	"context"
	"flag"
	"fmt"
	"io"
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
// can play directly is served from its original path. Anything else is
// remuxed to one MPEG-TS first; the renderer is not told the URL until
// that file is complete, because a finished file is what this TV can seek.
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
	direct, mime := planDirect(c.Source, c.VCodec, c.ACodec, c.ForceRemux || c.SubFile != "")

	// A remux's output clock starts at zero when the head was skipped, so
	// every position reported to IINA is this base plus the TV's REL_TIME.
	// A file served as-is keeps the source timeline and base stays zero.
	base := 0.0
	if !direct && start >= 0.5 {
		c.StartAt = start
		base = start
	}
	mediaPath := c.Source
	if !direct {
		mediaPath = filepath.Join(c.OutDir, "media.ts")
		mime = "video/mp2t"
	}
	port, shutdown, err := StartMediaServer(mediaPath, mime, subPath)
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
	var done <-chan error
	setStopJob(func() {
		if jobStop != nil {
			jobStop()
		}
	})
	if !direct {
		c.ForDLNA = true
		jobStop, done = RunJob(c, os.Stdout)
	}

	ctx, cancel := context.WithCancel(context.Background())
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
		select {
		case err := <-done:
			if err != nil {
				teardown()
				fail(err.Error())
			}
		case <-sigs:
			teardown()
			Emit(os.Stdout, "stopped", nil)
			return
		}
	}

	playCtx, playCancel := context.WithTimeout(context.Background(), 20*time.Second)
	fi, _ := os.Stat(mediaPath)
	var size int64
	if fi != nil {
		size = fi.Size()
	}
	err = dlnaPlay(playCtx, control, mediaURL, didlLite(title, mediaURL, mime, size, subURL))
	playCancel()
	if err != nil {
		teardown()
		fail("DLNA: " + err.Error())
	}
	// The tail already starts at `start`. Seeking a zero-based file to the
	// absolute position would jump past the end.
	if base == 0 && start >= 1 {
		time.Sleep(400 * time.Millisecond)
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		dlnaSeek(sctx, control, start)
		scancel()
	}
	if startPaused {
		pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Second)
		avAction(pctx, control, "Pause", [][2]string{{"InstanceID", "0"}})
		pcancel()
	}
	Emit(os.Stdout, "ready", map[string]any{"url": mediaURL, "base": base})

	go pollTransport(ctx, control, os.Stdout, base)

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
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		devices, discErr = DiscoverRenderers(ctx, ip)
	}()
	go func() {
		defer wg.Done()
		air = browseAirPlay(ctx)
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
	Emit(os.Stdout, "devices", map[string]any{"devices": devices, "airplay": air})
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

func pollTransport(ctx context.Context, control string, out io.Writer, base float64) {
	seen := false
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var last Transport
	var haveLast bool
	abs := func(rel float64) float64 { return base + rel }
	for {
		select {
		case <-ctx.Done():
			return
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
