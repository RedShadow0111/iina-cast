package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	// StartAt is where the movie is when the cast begins. The DLNA remux
	// still copies from the beginning, so the television's clock matches
	// the file. The Chromecast remux seeks the input to this time and keeps
	// the original timestamps (-copyts), so the receiver's currentTime is
	// the movie time. CastLimit is how many seconds to read after that;
	// zero reads through the end. CastFile is the name written in OutDir.
	StartAt   float64
	CastLimit float64
	CastFile  string
	// OnProgress receives ffmpeg's output clock, in seconds, as the remux
	// grows. The server turns that into a time-to-byte index. It may block:
	// while it blocks, the progress pipe fills and, together with Throttle,
	// the copy waits until the television has caught up.
	OnProgress func(outSec float64)
	// Throttle pauses the ffmpeg process while the copy is too far ahead of
	// the bytes already handed to the television. Nil for the AirPlay path.
	Throttle *procThrottle
	// ForDLNA selects the single-file MPEG-TS recipe. DLNA renderers on TVs
	// such as Xiaomi pull one URL; they do not play the fMP4 HLS playlist
	// the AirPlay path serves.
	ForDLNA bool
	// ForCast packages HLS the Default Media Receiver can open. H.264 and
	// HEVC are copied. CastModel, Width and Height are recorded for the
	// receiver; only a video codec that cannot be copied is scaled to
	// H.264 1080p.
	ForCast   bool
	CastModel string
	Width     int
	Height    int
	// ForceRemux refuses to hand the original file over. Set when IINA
	// selected an audio track other than the file's first. SubFile is a
	// prepared .srt the server hands out beside the video; it is not muxed.
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
// The copy always starts at the beginning of the movie. An input seek would
// open faster, and it is exactly what made the television show a different
// time from IINA.
//
// -readrate caps the disk read at 4× playback after a 15-second burst, so a
// seek cannot flatten the disk. The steady pace is not this flag: the helper
// pauses ffmpeg once the file is about a minute ahead of what the TV has
// actually been sent, and the HTTP server then feeds the TV at just over
// realtime. An uncapped copy reads the whole movie at once, the playhead
// falls out of the disk cache, and a few minutes in both pictures hitch.
func BuildDLNAArgs(c JobConfig) []string {
	args := []string{
		"-hide_banner", "-nostats", "-loglevel", "warning", "-y",
		"-flush_packets", "1",
		"-progress", "pipe:1",
		"-readrate", "4",
		"-readrate_initial_burst", "15",
		"-i", c.Source,
		"-map", "0:" + strconv.Itoa(c.VMap),
		"-map", "0:" + strconv.Itoa(c.AMap),
		"-sn", "-dn",
	}
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
	return append(args,
		"-muxdelay", "0", "-muxpreload", "0",
		"-f", "mpegts",
		filepath.Join(c.OutDir, "media.ts"),
	)
}

// castScale caps a re-encode at 1080p without stretching. The filter is a
// no-op for a picture that is already smaller.
const castScale = "scale=w='min(1920,iw)':h='min(1080,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2"

// BuildCastArgs writes one faststart MP4 the Default Media Receiver can
// open at once. Video is copied. Audio is AAC: this receiver played the
// picture and reported no audio track for the file's AC-3, in a
// fragmented MP4 and in a progressive one it still had to scan from the
// end. A faststart file puts the index at the front, so playback starts
// after the first few seconds of the remux instead of after the whole
// movie. Subtitles stay a sidecar.
func BuildCastArgs(c JobConfig) []string {
	recipe := planCast(c.VCodec, c.CastModel, c.Width, c.Height)
	args := []string{
		"-hide_banner", "-nostats", "-loglevel", "warning", "-y",
		"-progress", "pipe:1",
	}
	// Input options. -t after -i is measured against the copied timestamps
	// and writes an empty file; before -i it is a length to read.
	if c.StartAt > 0.5 {
		args = append(args, "-ss", strconv.FormatFloat(c.StartAt, 'f', 3, 64))
	}
	if c.CastLimit > 0 {
		args = append(args, "-t", strconv.FormatFloat(c.CastLimit, 'f', 3, 64))
	}
	args = append(args, "-i", c.Source)
	if c.StartAt > 0.5 {
		args = append(args, "-copyts")
	}
	args = append(args,
		"-map", "0:"+strconv.Itoa(c.VMap),
		"-map", "0:"+strconv.Itoa(c.AMap),
		"-map_chapters", "-1",
		"-map_metadata", "-1",
		"-sn", "-dn",
	)
	if recipe.Scale {
		args = append(args, "-vf", castScale)
	}
	switch {
	case recipe.CopyVideo && recipe.HEVCOut:
		args = append(args, "-c:v", "copy", "-tag:v", "hvc1")
	case recipe.CopyVideo:
		args = append(args, "-c:v", "copy")
	default:
		args = append(args,
			"-c:v", "h264_videotoolbox",
			"-profile:v", "high", "-level:v", "4.2",
			"-b:v", "10M", "-allow_sw", "1", "-pix_fmt", "yuv420p",
			"-force_key_frames", "expr:gte(t,n_forced*2)",
		)
	}
	if c.ACodec == "aac" {
		args = append(args, "-c:a", "copy")
	} else if c.AChannels > 2 {
		// The receiver stays silent on AC-3. Stereo AAC is what it plays.
		args = append(args, "-c:a", "aac", "-b:a", "192k", "-ac", "2")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "192k")
	}
	name := c.CastFile
	if name == "" {
		name = "media.mp4"
	}
	return append(args,
		"-movflags", "+faststart",
		"-f", "mp4",
		filepath.Join(c.OutDir, name),
	)
}

// procThrottle pauses ffmpeg while the remux is too far ahead of the client
// and continues it once the television has caught up. SIGSTOP is immediate:
// blocking the progress pipe alone lets ffmpeg write for minutes before the
// pipe fills.
type procThrottle struct {
	mu     sync.Mutex
	pause  func()
	resume func()
}

func (t *procThrottle) bind(pause, resume func()) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.pause = pause
	t.resume = resume
	t.mu.Unlock()
}

// hold stops ffmpeg while stay reports that the file is still ahead, and
// continues it before returning. finished and a cancelled ctx both release it.
func (t *procThrottle) hold(stay func() bool, finished func() bool, ctx context.Context) {
	if t == nil || stay == nil || !stay() {
		return
	}
	t.mu.Lock()
	pause, resume := t.pause, t.resume
	t.mu.Unlock()
	if pause != nil {
		pause()
	}
	defer func() {
		if resume != nil {
			resume()
		}
	}()
	for stay() && (finished == nil || !finished()) {
		if ctx == nil {
			time.Sleep(150 * time.Millisecond)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(150 * time.Millisecond):
		}
	}
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
	switch {
	case c.ForCast:
		args = BuildCastArgs(c)
	case c.ForDLNA:
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
	var paused atomic.Bool
	pauseProc := func() {
		if cmd.Process == nil || jobDone.Load() != 0 {
			return
		}
		if paused.CompareAndSwap(false, true) {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGSTOP)
		}
	}
	resumeProc := func() {
		if cmd.Process == nil {
			return
		}
		if paused.CompareAndSwap(true, false) {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGCONT)
		}
	}
	if c.Throttle != nil {
		c.Throttle.bind(pauseProc, resumeProc)
	}
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
					Emit(out, "progress", map[string]any{"pct": ProgressPct(us, c.Duration)})
					if c.OnProgress != nil {
						c.OnProgress(float64(us) / 1e6)
					}
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
		// A stopped process does not handle SIGTERM until it is continued.
		resumeProc()
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
