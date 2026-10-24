package main

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type JobConfig struct {
	FFmpeg    string
	Source    string
	OutDir    string
	VCodec    string
	ACodec    string
	AChannels int
	VMap      int
	AMap      int
	// Subtitles are optional; zero values mean "no subtitle track". SubMap
	// holds the ff-index as a string so the zero value can't be mistaken
	// for stream 0. SubPath (external file, fed as input 1) wins over SubMap.
	SubPath  string
	SubMap   string
	SubLang  string
	SubName  string
	Duration float64
	// StartAt drops the head of a DLNA remux with an input seek, so a track
	// change or a backwards seek only copies the tail. The output timeline
	// starts at zero; the server adds StartAt back onto the TV clock.
	StartAt float64
	// ForDLNA selects the single-file MPEG-TS recipe. DLNA renderers on TVs
	// such as Xiaomi pull one URL; they do not play the fMP4 HLS playlist
	// the AirPlay path serves.
	ForDLNA bool
	// ForceRemux refuses to hand the original file over. Set when the file
	// has more than one audio track, so the TV would not play the one IINA
	// has selected. SubFile is a prepared .srt the server hands out beside
	// the video; it is not muxed into the MPEG-TS.
	ForceRemux bool
	SubFile    string
}

func (c JobConfig) HasSub() bool { return c.SubPath != "" || c.SubMap != "" }

// PlaylistName is the playlist the TV is handed: ffmpeg's master playlist
// when a subtitle rendition exists, the media playlist alone otherwise.
func (c JobConfig) PlaylistName() string {
	if c.HasSub() {
		return "master.m3u8"
	}
	return "index.m3u8"
}

// BuildArgs ports the proven serve.sh recipe (docs/prototype.md) to a live
// event playlist. Apple TV compatibility matrix: docs/feasibility.md.
func BuildArgs(c JobConfig) []string {
	args := []string{
		"-hide_banner", "-nostats", "-loglevel", "warning", "-y",
		"-progress", "pipe:1",
		"-i", c.Source,
	}
	if c.SubPath != "" {
		args = append(args, "-i", c.SubPath)
	}
	args = append(args,
		"-map", "0:"+strconv.Itoa(c.VMap),
		"-map", "0:"+strconv.Itoa(c.AMap),
	)
	switch {
	case c.SubPath != "":
		args = append(args, "-map", "1:0", "-dn")
	case c.SubMap != "":
		args = append(args, "-map", "0:"+c.SubMap, "-dn")
	default:
		args = append(args, "-sn", "-dn")
	}
	switch c.VCodec {
	case "h264":
		args = append(args, "-c:v", "copy")
	case "hevc":
		args = append(args, "-c:v", "copy", "-tag:v", "hvc1")
	default:
		args = append(args, "-c:v", "hevc_videotoolbox", "-b:v", "12M", "-tag:v", "hvc1")
	}
	switch c.ACodec {
	case "aac", "ac3", "eac3":
		args = append(args, "-c:a", "copy")
	default:
		if c.AChannels > 2 {
			args = append(args, "-c:a", "eac3", "-b:a", "640k", "-ac", "6")
		} else {
			args = append(args, "-c:a", "aac", "-b:a", "256k")
		}
	}
	if c.HasSub() {
		args = append(args, "-c:s", "webvtt")
	}
	args = append(args,
		"-f", "hls", "-hls_time", "6", "-hls_playlist_type", "event",
		"-hls_segment_type", "fmp4", "-hls_flags", "independent_segments",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", filepath.Join(c.OutDir, "seg_%04d.m4s"),
	)
	if c.HasSub() {
		// v:0/a:0/s:0 are positions within each stream type among the MAPPED
		// output streams (BuildArgs always maps exactly one video, one audio,
		// one subtitle, in that order) — not ff-indices — so this is correct
		// for any -vmap/-amap/-smap values. Without -var_stream_map, ffmpeg
		// writes the subtitle rendition to disk but omits the
		// #EXT-X-MEDIA:TYPE=SUBTITLES line from the master playlist.
		args = append(args, "-var_stream_map", "v:0,a:0,s:0,sgroup:subs")
		args = append(args, "-master_pl_name", "master.m3u8")
	}
	return append(args, filepath.Join(c.OutDir, "index.m3u8"))
}

// BuildDLNAArgs remuxes into one MPEG-TS, stream-copying every codec the
// renderer can take. MPEG-TS is a single sequential write: there is no
// second pass to move a moov atom, and a finished file seeks by REL_TIME.
// Subtitles stay a sidecar (/sub.srt in the DIDL). MPEG-TS has no mov_text,
// and muxing them would not make the copy any faster.
//
// StartAt places -ss before -i. The copy then begins at the previous
// keyframe instead of reading the whole file, and the output clock starts
// at zero. The caller reports TV time as StartAt plus the renderer's clock.
func BuildDLNAArgs(c JobConfig) []string {
	args := []string{
		"-hide_banner", "-nostats", "-loglevel", "warning", "-y",
		"-progress", "pipe:1",
	}
	if c.StartAt >= 0.5 {
		args = append(args, "-ss", strconv.FormatFloat(c.StartAt, 'f', 3, 64))
	}
	args = append(args,
		"-i", c.Source,
		"-map", "0:"+strconv.Itoa(c.VMap),
		"-map", "0:"+strconv.Itoa(c.AMap),
		"-sn", "-dn",
	)
	switch c.VCodec {
	case "h264", "hevc":
		args = append(args, "-c:v", "copy")
	default:
		args = append(args, "-c:v", "hevc_videotoolbox", "-b:v", "12M")
	}
	switch c.ACodec {
	case "aac", "ac3", "eac3", "mp3":
		args = append(args, "-c:a", "copy")
	default:
		if c.AChannels > 2 {
			args = append(args, "-c:a", "eac3", "-b:a", "640k", "-ac", "6")
		} else {
			args = append(args, "-c:a", "aac", "-b:a", "256k")
		}
	}
	if c.StartAt >= 0.5 {
		// An input seek on a stream copy can leave a negative first PTS.
		// The TV's clock is that PTS, so pin the tail to zero.
		args = append(args, "-avoid_negative_ts", "make_zero")
	}
	return append(args,
		"-f", "mpegts",
		filepath.Join(c.OutDir, "media.ts"),
	)
}

func ParseProgressValue(line string) (string, string, bool) {
	k, v, found := strings.Cut(strings.TrimSpace(line), "=")
	if !found || k == "" {
		return "", "", false
	}
	return k, v, true
}

func ProgressPct(outTimeUS int64, duration float64) float64 {
	if duration <= 0 {
		return 0
	}
	pct := float64(outTimeUS) / 1e6 / duration * 100
	if pct > 100 {
		return 100
	}
	if pct < 0 {
		return 0
	}
	return pct
}

func RunJob(c JobConfig, out io.Writer) (func(), <-chan error) {
	done := make(chan error, 1)
	args := BuildArgs(c)
	if c.ForDLNA {
		args = BuildDLNAArgs(c)
	}
	cmd := exec.Command(c.FFmpeg, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		done <- err
		return func() {}, done
	}

	// Flag to track when job has completed (0 = running, 1 = done)
	var jobDone atomic.Int32
	// Closed once ffmpeg has exited and its result is on done, so stop can
	// block until the process is really gone.
	finished := make(chan struct{})

	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			k, v, ok := ParseProgressValue(sc.Text())
			if !ok {
				continue
			}
			switch k {
			case "out_time_us":
				if us, perr := strconv.ParseInt(v, 10, 64); perr == nil {
					dur := c.Duration
					// -ss before -i resets the output clock, so the bar is
					// the tail still being copied, not the whole movie.
					if c.ForDLNA && c.StartAt >= 0.5 && c.Duration > c.StartAt {
						dur = c.Duration - c.StartAt
					}
					Emit(out, "progress", map[string]any{"pct": ProgressPct(us, dur)})
				}
			case "progress":
				if v == "end" {
					Emit(out, "packaged", nil)
				}
			}
		}
		// Handle any scanner error, then wait for process
		werr := cmd.Wait()
		if werr != nil {
			werr = fmt.Errorf("ffmpeg: %w; stderr: %s", werr, strings.TrimSpace(stderr.String()))
		}
		// Mark job as done before sending to channel
		jobDone.Store(1)
		done <- werr
		close(finished)
	}()

	// stop signals ffmpeg's process group and returns only once ffmpeg has
	// exited: the caller sweeps the output directory next, and a segment
	// ffmpeg was still flushing would otherwise reappear after the sweep.
	// ffmpeg honours SIGTERM within a fraction of a second; a process that
	// ignores it is SIGKILLed after a grace period chosen to stay inside the
	// 3s the `stop` subcommand's TakeOver waits for the old instance.
	// Safe to call more than once and after done has fired.
	stop := func() {
		// Only signal if job is still running
		if jobDone.Load() == 0 {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			// Close stdout to help the scanner exit if it's still reading
			if c, ok := stdout.(io.Closer); ok {
				c.Close()
			}
		}
		select {
		case <-finished:
			return
		case <-time.After(2 * time.Second):
		}
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-finished:
		case <-time.After(time.Second):
		}
	}
	return stop, done
}
