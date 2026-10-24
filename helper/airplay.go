package main

import (
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// AirPlay is one _airplay._tcp advertisement. Real is true only for an
// Apple TV new enough to pull this plugin's stream. AppleTV1/2/3 is the
// identity spoofed receivers copy, and a Mac or HomePod is not a television.
type AirPlay struct {
	Name    string `json:"name"`
	Model   string `json:"model"`
	Srcvers string `json:"srcvers"`
	Real    bool   `json:"real"`
}

func airPlayVideo(model string) bool {
	model = strings.Trim(model, `"`)
	if !strings.HasPrefix(model, "AppleTV") {
		return false
	}
	switch model {
	case "AppleTV1,1", "AppleTV2,1", "AppleTV3,1", "AppleTV3,2":
		return false
	}
	return true
}

var (
	airPlayModel = regexp.MustCompile(`(?:^|[\s])model=([^ \t\r\n]+)`)
	airPlaySrc   = regexp.MustCompile(`(?:^|[\s])srcvers=([^ \t\r\n]+)`)
)

func parseAirPlayTXT(text string) (model, srcvers string) {
	if m := airPlayModel.FindStringSubmatch(text); len(m) == 2 {
		model = strings.Trim(m[1], `"`)
	}
	if m := airPlaySrc.FindStringSubmatch(text); len(m) == 2 {
		srcvers = strings.Trim(m[1], `"`)
	}
	return model, srcvers
}

// airPlayNames reads `dns-sd -B _airplay._tcp` output. The same receiver is
// listed once per interface; the instance name is everything after the
// service type, and it may contain spaces.
func airPlayNames(browse string) []string {
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(browse, "\n") {
		if !strings.Contains(line, "Add") || !strings.Contains(line, "_airplay._tcp.") {
			continue
		}
		i := strings.Index(line, "_airplay._tcp.")
		name := strings.Trim(line[i+len("_airplay._tcp."):], " \t\r")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// runScript runs a command under `script` so a tool that line-buffers only
// on a terminal (dns-sd) still flushes when the timeout kills it.
func runScript(ctx context.Context, argv ...string) string {
	args := append([]string{"-q", "/dev/null"}, argv...)
	cmd := exec.CommandContext(ctx, "/usr/bin/script", args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	_ = cmd.Run()
	return buf.String()
}

func browseAirPlay(ctx context.Context) []AirPlay {
	bctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	text := runScript(bctx, "/usr/bin/dns-sd", "-B", "_airplay._tcp", "local.")
	cancel()
	names := airPlayNames(text)
	if len(names) > 4 {
		names = names[:4]
	}
	out := make([]AirPlay, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			lctx, lcancel := context.WithTimeout(ctx, 2*time.Second)
			txt := runScript(lctx, "/usr/bin/dns-sd", "-L", name, "_airplay._tcp", "local.")
			lcancel()
			model, src := parseAirPlayTXT(txt)
			out[i] = AirPlay{Name: name, Model: model, Srcvers: src, Real: airPlayVideo(model)}
		}(i, name)
	}
	wg.Wait()
	return out
}
