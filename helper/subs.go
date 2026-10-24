package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// cueCanvasW is the width every cue is drawn on. ffmpeg then scales that
// canvas to the video, so a short line and a long one stay the same size.
const cueCanvasW = 1600

var (
	srtTimeRe = regexp.MustCompile(`(\d+):(\d\d):(\d\d)[,.](\d{1,3})\s*-->\s*(\d+):(\d\d):(\d\d)[,.](\d{1,3})`)
	srtTagRe  = regexp.MustCompile(`<[^>]*>|\{[^}]*\}`)
)

type srtCue struct {
	start float64
	end   float64
	text  string
}

// parseSRT reads a SubRip file. ffmpeg's subtitle conversion is what produces
// the file for an embedded track, so the parser accepts the comma or dot
// millisecond separator and drops styling tags.
func parseSRT(data []byte) []srtCue {
	text := string(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}))
	text = strings.ReplaceAll(text, "\r\n", "\n")
	blocks := regexp.MustCompile(`\n\s*\n`).Split(strings.TrimSpace(text), -1)
	var cues []srtCue
	for _, block := range blocks {
		m := srtTimeRe.FindStringSubmatch(block)
		if m == nil {
			continue
		}
		start := srtClock(m[1], m[2], m[3], m[4])
		end := srtClock(m[5], m[6], m[7], m[8])
		if end <= start {
			continue
		}
		idx := srtTimeRe.FindStringIndex(block)
		body := strings.TrimSpace(block[idx[1]:])
		body = srtTagRe.ReplaceAllString(body, "")
		body = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`).Replace(body)
		body = strings.TrimSpace(body)
		if body == "" {
			continue
		}
		cues = append(cues, srtCue{start: start, end: end, text: body})
	}
	return cues
}

func srtClock(h, m, s, ms string) float64 {
	hi, _ := strconv.Atoi(h)
	mi, _ := strconv.Atoi(m)
	si, _ := strconv.Atoi(s)
	msi, _ := strconv.Atoi(ms)
	if len(ms) == 1 {
		msi *= 100
	} else if len(ms) == 2 {
		msi *= 10
	}
	return float64(hi*3600+mi*60+si) + float64(msi)/1000
}

func subtitleFont() (*opentype.Font, error) {
	candidates := []string{
		"/System/Library/Fonts/Supplemental/Arial.ttf",
		"/System/Library/Fonts/Supplemental/Arial Unicode.ttf",
		"/Library/Fonts/Arial.ttf",
	}
	var last error
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			last = err
			continue
		}
		f, err := opentype.Parse(b)
		if err != nil {
			last = err
			continue
		}
		return f, nil
	}
	if last == nil {
		last = fmt.Errorf("no font")
	}
	return nil, fmt.Errorf("subtitle font: %w", last)
}

func wrapCue(face font.Face, text string, maxW int) []string {
	var lines []string
	for _, para := range strings.Split(text, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			lines = append(lines, "")
			continue
		}
		cur := ""
		for _, w := range words {
			next := w
			if cur != "" {
				next = cur + " " + w
			}
			if font.MeasureString(face, next).Ceil() > maxW && cur != "" {
				lines = append(lines, cur)
				cur = w
				continue
			}
			cur = next
		}
		if cur != "" {
			lines = append(lines, cur)
		}
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	return lines
}

func renderCue(face font.Face, text string) (*image.NRGBA, error) {
	const pad = 24
	maxW := cueCanvasW - pad*2
	lines := wrapCue(face, text, maxW)
	metrics := face.Metrics()
	lineH := metrics.Height.Ceil() + 6
	if lineH < 8 {
		lineH = 8
	}
	height := pad*2 + lineH*len(lines)
	img := image.NewNRGBA(image.Rect(0, 0, cueCanvasW, height))
	ascent := metrics.Ascent.Ceil()
	white := color.NRGBA{255, 255, 255, 255}
	black := color.NRGBA{0, 0, 0, 230}
	for i, line := range lines {
		lw := font.MeasureString(face, line).Ceil()
		x := (cueCanvasW - lw) / 2
		if x < pad {
			x = pad
		}
		y := pad + ascent + i*lineH
		for _, o := range []image.Point{{-3, 0}, {3, 0}, {0, -3}, {0, 3}, {-2, -2}, {2, -2}, {-2, 2}, {2, 2}} {
			drawString(img, face, line, x+o.X, y+o.Y, black)
		}
		drawString(img, face, line, x, y, white)
	}
	return img, nil
}

func drawString(img *image.NRGBA, face font.Face, text string, x, y int, col color.Color) {
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(col),
		Face: face,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(text)
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

type reelSeg struct {
	file string
	dur  float64
}

// buildSubtitleOverlay draws each cue and writes an ffconcat whose timeline
// matches the subtitle file. duration is the film length, so the reel covers
// the whole movie and a cue cannot be left stuck on the last frame.
func buildSubtitleOverlay(srtPath, outDir string, duration float64) (string, error) {
	data, err := os.ReadFile(srtPath)
	if err != nil {
		return "", err
	}
	cues := parseSRT(data)
	if len(cues) == 0 {
		return "", fmt.Errorf("no cues in %s", filepath.Base(srtPath))
	}
	parsed, err := subtitleFont()
	if err != nil {
		return "", err
	}
	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{
		Size:    54,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return "", err
	}
	if c, ok := face.(interface{ Close() error }); ok {
		defer c.Close()
	}

	blank := image.NewNRGBA(image.Rect(0, 0, cueCanvasW, 8))
	blankPath := filepath.Join(outDir, "sub_blank.png")
	if err := writePNG(blankPath, blank); err != nil {
		return "", err
	}

	var segs []reelSeg
	clock := 0.0
	for i, cue := range cues {
		if cue.start > clock+0.04 {
			segs = append(segs, reelSeg{file: blankPath, dur: cue.start - clock})
		}
		img, err := renderCue(face, cue.text)
		if err != nil {
			return "", err
		}
		pngPath := filepath.Join(outDir, fmt.Sprintf("sub_%04d.png", i+1))
		if err := writePNG(pngPath, img); err != nil {
			return "", err
		}
		segs = append(segs, reelSeg{file: pngPath, dur: cue.end - cue.start})
		clock = cue.end
	}
	if duration > clock+0.04 {
		segs = append(segs, reelSeg{file: blankPath, dur: duration - clock})
	} else {
		segs = append(segs, reelSeg{file: blankPath, dur: 0.5})
	}

	concatPath := filepath.Join(outDir, "subs.ffconcat")
	var b strings.Builder
	b.WriteString("ffconcat version 1.0\n")
	for _, s := range segs {
		fmt.Fprintf(&b, "file '%s'\nduration %.3f\n", concatEscape(s.file), s.dur)
	}
	fmt.Fprintf(&b, "file '%s'\n", concatEscape(segs[len(segs)-1].file))
	if err := os.WriteFile(concatPath, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return concatPath, nil
}

func concatEscape(p string) string {
	return strings.ReplaceAll(p, "'", `'\''`)
}

func dlnaBitrate(width, height int) string {
	if width <= 0 || height <= 0 {
		return "16M"
	}
	// Enough bits that the drawn letters survive the encode.
	bps := width * height * 5
	if bps < 4_000_000 {
		bps = 4_000_000
	}
	if bps > 45_000_000 {
		bps = 45_000_000
	}
	return strconv.Itoa(bps/1_000_000) + "M"
}

var videoSizeRe = regexp.MustCompile(`Video:.*?(\d{2,5})x(\d{2,5})`)

func videoSize(ffmpeg, path string) (int, int) {
	cmd := exec.Command(ffmpeg, "-hide_banner", "-i", path)
	out, _ := cmd.CombinedOutput()
	m := videoSizeRe.FindSubmatch(out)
	if m == nil {
		return 0, 0
	}
	w, _ := strconv.Atoi(string(m[1]))
	h, _ := strconv.Atoi(string(m[2]))
	return w, h
}
