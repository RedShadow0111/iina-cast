package main

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A Chromecast does not speak AirPlay or DLNA. It advertises
// _googlecast._tcp and accepts the Cast protocol on TCP 8009: a TLS
// connection (the device certificate is not in the system trust store)
// carrying length-prefixed protobuf envelopes with JSON inside.
const (
	nsCastConn  = "urn:x-cast:com.google.cast.tp.connection"
	nsCastHeart = "urn:x-cast:com.google.cast.tp.heartbeat"
	nsCastRecv  = "urn:x-cast:com.google.cast.receiver"
	nsCastMedia = "urn:x-cast:com.google.cast.media"
	// Default Media Receiver. It plays an http URL we already serve; no
	// Cast developer registration and nothing downloaded at runtime.
	castAppDefault = "CC1AD845"
)

// CastDevice is one Google Cast receiver found on the LAN.
type CastDevice struct {
	Name  string `json:"name"`
	Model string `json:"model"`
	Host  string `json:"host"`
	Port  int    `json:"port"`
	ID    string `json:"id"`
}

// castRecipe is how the picture is packaged for the Default Media Receiver.
// H.264 and HEVC are copied into a faststart MP4. A plain md=Chromecast
// played that copy, including 4K Main 10, and rejected the same picture
// packed as HLS. Only a codec it cannot be given as-is is encoded to H.264.
// Audio is always AAC; the receiver stayed silent on AC-3.
type castRecipe struct {
	CopyVideo bool
	HEVCOut   bool
	Scale     bool
}

func planCast(vcodec, model string, width, height int) castRecipe {
	switch vcodec {
	case "h264":
		return castRecipe{CopyVideo: true}
	case "hevc":
		return castRecipe{CopyVideo: true, HEVCOut: true}
	default:
		// model, width and height stay on the recipe call so a future
		// device limit can see them. Today every other codec is encoded
		// to H.264, capped at 1080p by the scale filter.
		_, _, _ = model, width, height
		return castRecipe{Scale: true}
	}
}

func dnsSDInstances(browse, marker string) []string {
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(browse, "\n") {
		if !strings.Contains(line, "Add") || !strings.Contains(line, marker) {
			continue
		}
		i := strings.Index(line, marker)
		name := strings.Trim(line[i+len(marker):], " \t\r")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

func isTXTSep(b byte) bool {
	return b == ' ' || b == '\n' || b == '\r' || b == '\t'
}

func readTXTToken(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			b.WriteByte('\\')
			b.WriteByte(s[i+1])
			i++
			continue
		}
		if isTXTSep(s[i]) {
			break
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// unescapeDNSSD turns dns-sd's \ddd decimal bytes and \  into text.
func unescapeDNSSD(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b = append(b, s[i])
			continue
		}
		if s[i+1] == ' ' || s[i+1] == '\\' {
			b = append(b, s[i+1])
			i++
			continue
		}
		if i+3 < len(s) && isDigit(s[i+1]) && isDigit(s[i+2]) && isDigit(s[i+3]) {
			n := int(s[i+1]-'0')*100 + int(s[i+2]-'0')*10 + int(s[i+3]-'0')
			if n < 256 {
				b = append(b, byte(n))
				i += 3
				continue
			}
		}
		b = append(b, s[i+1])
		i++
	}
	return strings.Trim(string(b), `"`)
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func txtValue(text, key string) string {
	needle := key + "="
	from := 0
	for from < len(text) {
		i := strings.Index(text[from:], needle)
		if i < 0 {
			return ""
		}
		i += from
		if i == 0 || isTXTSep(text[i-1]) {
			return unescapeDNSSD(readTXTToken(text[i+len(needle):]))
		}
		from = i + len(needle)
	}
	return ""
}

var castReach = regexp.MustCompile(`can be reached at\s+(\S+?):(\d+)`)

func castReachable(text string) (string, int) {
	m := castReach.FindStringSubmatch(text)
	if len(m) != 3 {
		return "", 0
	}
	port, _ := strconv.Atoi(m[2])
	return strings.TrimSuffix(m[1], "."), port
}

var ipv4re = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)

func firstIPv4(text string) string {
	for _, m := range ipv4re.FindAllString(text, -1) {
		ip := net.ParseIP(m)
		if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		return ip.To4().String()
	}
	return ""
}

func resolveCastIPv4(ctx context.Context, host string) string {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil && !ip.IsLoopback() {
		return ip.To4().String()
	}
	lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	ips, err := net.DefaultResolver.LookupIP(lctx, "ip4", host)
	cancel()
	if err == nil {
		for _, ip := range ips {
			if v4 := ip.To4(); v4 != nil && !v4.IsLoopback() {
				return v4.String()
			}
		}
	}
	gctx, gcancel := context.WithTimeout(ctx, 2*time.Second)
	text := runScript(gctx, "/usr/bin/dns-sd", "-G", "v4", host)
	gcancel()
	return firstIPv4(text)
}

func castIPv4(host string) (string, error) {
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return "", fmt.Errorf("chromecast address is not a LAN IPv4 address")
	}
	return ip.To4().String(), nil
}

// browseCast lists _googlecast._tcp receivers and resolves each to an IPv4
// address. A receiver we cannot address is left out: the picker would
// otherwise offer a device the helper cannot open.
func browseCast(ctx context.Context) []CastDevice {
	if ctx.Err() != nil {
		return nil
	}
	bctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	text := runScript(bctx, "/usr/bin/dns-sd", "-B", "_googlecast._tcp", "local.")
	cancel()
	names := dnsSDInstances(text, "_googlecast._tcp.")
	if len(names) > 8 {
		names = names[:8]
	}
	out := make([]CastDevice, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			lctx, lcancel := context.WithTimeout(ctx, 3*time.Second)
			txt := runScript(lctx, "/usr/bin/dns-sd", "-L", name, "_googlecast._tcp", "local.")
			lcancel()
			host, port := castReachable(txt)
			if host == "" {
				return
			}
			ip := resolveCastIPv4(ctx, host)
			if ip == "" {
				fmt.Fprintf(os.Stderr, "cast: %s has no IPv4 address\n", name)
				return
			}
			friendly := txtValue(txt, "fn")
			if friendly == "" {
				friendly = name
			}
			if port == 0 {
				port = 8009
			}
			out[i] = CastDevice{
				Name: friendly, Model: txtValue(txt, "md"),
				Host: ip, Port: port, ID: txtValue(txt, "id"),
			}
		}(i, name)
	}
	wg.Wait()
	var devices []CastDevice
	seen := map[string]bool{}
	for _, d := range out {
		if d.Host == "" {
			continue
		}
		key := d.ID
		if key == "" {
			key = d.Host
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		devices = append(devices, d)
	}
	return devices
}

func playlistSeconds(path string) float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var sum float64
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#EXTINF:") {
			continue
		}
		num, _, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
		d, perr := strconv.ParseFloat(strings.TrimSpace(num), 64)
		if perr == nil && d > 0 {
			sum += d
		}
	}
	return sum
}

type cmdReader struct {
	path string
	off  int64
	buf  string
}

func (r *cmdReader) poll() []string {
	f, err := os.Open(r.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(r.off, io.SeekStart); err != nil {
		return nil
	}
	b, err := io.ReadAll(f)
	if err != nil || len(b) == 0 {
		return nil
	}
	r.off += int64(len(b))
	r.buf += string(b)
	var cmds []string
	for {
		i := strings.IndexByte(r.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(r.buf[:i])
		r.buf = r.buf[i+1:]
		if line != "" {
			cmds = append(cmds, line)
		}
	}
	return cmds
}

func parseCastCmd(line string) (string, float64, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", 0, false
	}
	switch fields[0] {
	case "play", "pause", "stop":
		return fields[0], 0, true
	case "seek":
		if len(fields) < 2 {
			return "", 0, false
		}
		pos, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || pos < 0 {
			return "", 0, false
		}
		return "seek", pos, true
	default:
		return "", 0, false
	}
}

func runCastCtl(argv []string) {
	fs := flag.NewFlagSet("cast-ctl", flag.ExitOnError)
	out := fs.String("out", "", "output dir of the cast")
	action := fs.String("action", "", "play, pause, seek or stop")
	pos := fs.Float64("pos", 0, "seek target in seconds")
	_ = fs.Parse(argv)
	if *out == "" || *action == "" {
		fmt.Fprintln(os.Stderr, "missing required flags: -out, -action")
		os.Exit(2)
	}
	line := *action
	switch *action {
	case "play", "pause", "stop":
	case "seek":
		line = fmt.Sprintf("seek %g", *pos)
	default:
		fmt.Fprintf(os.Stderr, "unknown cast action %q\n", *action)
		os.Exit(2)
	}
	f, err := os.OpenFile(filepath.Join(*out, "cast.cmd"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, line); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func appendVarint(dst []byte, v uint64) []byte {
	var buf [10]byte
	n := binary.PutUvarint(buf[:], v)
	return append(dst, buf[:n]...)
}

func appendProtoString(dst []byte, field int, s string) []byte {
	dst = appendVarint(dst, uint64(field<<3|2))
	dst = appendVarint(dst, uint64(len(s)))
	return append(dst, s...)
}

func encodeCast(src, dst, ns, payload string) []byte {
	var body []byte
	body = appendVarint(body, 1<<3)
	body = appendVarint(body, 0)
	body = appendProtoString(body, 2, src)
	body = appendProtoString(body, 3, dst)
	body = appendProtoString(body, 4, ns)
	body = appendVarint(body, 5<<3)
	body = appendVarint(body, 0)
	body = appendProtoString(body, 6, payload)
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(body)))
	copy(frame[4:], body)
	return frame
}

func decodeCast(b []byte) (src, dst, ns, utf8 string, ok bool) {
	i := 0
	for i < len(b) {
		key, n := binary.Uvarint(b[i:])
		if n <= 0 {
			return
		}
		i += n
		field := int(key >> 3)
		wire := int(key & 7)
		switch wire {
		case 0:
			_, n = binary.Uvarint(b[i:])
			if n <= 0 {
				return
			}
			i += n
		case 1:
			if i+8 > len(b) {
				return
			}
			i += 8
		case 2:
			ln, n := binary.Uvarint(b[i:])
			if n <= 0 {
				return
			}
			i += n
			if ln > uint64(len(b)-i) {
				return
			}
			val := b[i : i+int(ln)]
			i += int(ln)
			switch field {
			case 2:
				src = string(val)
			case 3:
				dst = string(val)
			case 4:
				ns = string(val)
			case 6:
				utf8 = string(val)
			}
		case 5:
			if i+4 > len(b) {
				return
			}
			i += 4
		default:
			return
		}
	}
	ok = true
	return
}

func readCastFrame(r io.Reader) (ns, payload string, err error) {
	var n uint32
	if err = binary.Read(r, binary.BigEndian, &n); err != nil {
		return
	}
	if n == 0 || n > 1<<20 {
		err = fmt.Errorf("bad cast frame")
		return
	}
	buf := make([]byte, n)
	if _, err = io.ReadFull(r, buf); err != nil {
		return
	}
	_, _, ns, payload, ok := decodeCast(buf)
	if !ok {
		err = fmt.Errorf("bad cast message")
	}
	return
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
}

type castPlayback struct {
	Session int
	State   string
	Time    float64
	Idle    string
	At      time.Time
}

func (p castPlayback) pos(now time.Time) float64 {
	t := p.Time
	if p.State == "PLAYING" && !p.At.IsZero() {
		d := now.Sub(p.At).Seconds()
		if d > 0 && d < 1.5 {
			t += d
		}
	}
	if t < 0 {
		return 0
	}
	return t
}

// paused is the user's pause, not a rebuffer. BUFFERING keeps the local
// player running; reporting it as a pause would stop IINA on every seek.
func (p castPlayback) paused() bool {
	return p.State == "PAUSED" || p.State == "IDLE" || p.State == ""
}

func playbackFrom(raw map[string]any) (castPlayback, bool) {
	arr, ok := raw["status"].([]any)
	if !ok || len(arr) == 0 {
		return castPlayback{}, false
	}
	st, ok := arr[0].(map[string]any)
	if !ok {
		return castPlayback{}, false
	}
	p := castPlayback{At: time.Now()}
	p.Session, _ = asInt(st["mediaSessionId"])
	p.State, _ = st["playerState"].(string)
	if n, ok := st["currentTime"].(float64); ok {
		p.Time = n
	}
	p.Idle, _ = st["idleReason"].(string)
	return p, p.State != ""
}

func receiverSession(raw map[string]any, appID string) (transport, session string) {
	status, _ := raw["status"].(map[string]any)
	apps, _ := status["applications"].([]any)
	for _, item := range apps {
		app, _ := item.(map[string]any)
		if app["appId"] != appID {
			continue
		}
		transport, _ = app["transportId"].(string)
		session, _ = app["sessionId"].(string)
		return transport, session
	}
	return "", ""
}

type castLoad struct {
	URL         string
	ContentType string
	Title       string
	Start       float64
	Duration    float64
	Autoplay    bool
	AudioFmt    string
	VideoFmt    string
	SubURL      string
	SubLang     string
	SubName     string
}

type castLink struct {
	conn         *tls.Conn
	wmu          sync.Mutex
	mu           sync.Mutex
	req          int
	wait         map[int]chan map[string]any
	dead         chan struct{}
	transport    string
	session      string
	mediaSession int
	play         castPlayback
	shut         bool
}

func dialCast(ctx context.Context, host string, port int) (*castLink, error) {
	d := &net.Dialer{Timeout: 5 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if dl, ok := ctx.Deadline(); ok {
		_ = tlsConn.SetDeadline(dl)
	} else {
		_ = tlsConn.SetDeadline(time.Now().Add(5 * time.Second))
	}
	if err := tlsConn.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	_ = tlsConn.SetDeadline(time.Time{})
	l := &castLink{
		conn: tlsConn,
		wait: map[int]chan map[string]any{},
		dead: make(chan struct{}),
	}
	go l.readLoop()
	if err := l.send("receiver-0", nsCastConn, map[string]any{
		"type": "CONNECT", "origin": map[string]any{},
		"userAgent": "IINA Cast",
		"senderInfo": map[string]any{
			"sdkType": 2, "version": "15.0.0", "browserVersion": "44.0.2403.30",
			"platform": 4, "systemVersion": "Mac OS X", "connectionType": 1,
		},
	}); err != nil {
		l.conn.Close()
		return nil, err
	}
	go l.heartbeat(ctx)
	return l, nil
}

func (l *castLink) heartbeat(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.dead:
			return
		case <-t.C:
			_ = l.send("receiver-0", nsCastHeart, map[string]string{"type": "PING"})
		}
	}
}

func (l *castLink) readLoop() {
	defer close(l.dead)
	for {
		_ = l.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		ns, payload, err := readCastFrame(l.conn)
		if err != nil {
			return
		}
		l.dispatch(ns, payload)
	}
}

func (l *castLink) dispatch(ns, payload string) {
	if payload == "" {
		return
	}
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(payload), &head) != nil {
		return
	}
	if head.Type == "PING" {
		_ = l.send("receiver-0", nsCastHeart, map[string]string{"type": "PONG"})
		return
	}
	if head.Type == "PONG" || head.Type == "CLOSE" {
		return
	}
	var raw map[string]any
	if json.Unmarshal([]byte(payload), &raw) != nil {
		return
	}
	if head.Type == "MEDIA_STATUS" {
		if p, ok := playbackFrom(raw); ok {
			l.setPlayback(p)
		}
	}
	id, ok := asInt(raw["requestId"])
	if !ok || id == 0 {
		id, ok = asInt(raw["launchRequestId"])
	}
	if !ok || id == 0 {
		return
	}
	l.mu.Lock()
	ch := l.wait[id]
	l.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- raw:
	default:
	}
}

func (l *castLink) setPlayback(p castPlayback) {
	l.mu.Lock()
	l.play = p
	if p.Session != 0 {
		l.mediaSession = p.Session
	}
	l.mu.Unlock()
}

func (l *castLink) playback() castPlayback {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.play
}

func (l *castLink) send(dest, ns string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	frame := encodeCast("sender-0", dest, ns, string(b))
	l.wmu.Lock()
	defer l.wmu.Unlock()
	if l.conn == nil {
		return fmt.Errorf("chromecast connection is closed")
	}
	_ = l.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, err = l.conn.Write(frame)
	return err
}

func (l *castLink) request(ctx context.Context, dest, ns string, payload map[string]any, timeout time.Duration) (map[string]any, error) {
	l.mu.Lock()
	l.req++
	id := l.req
	ch := make(chan map[string]any, 1)
	l.wait[id] = ch
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.wait, id)
		l.mu.Unlock()
	}()
	payload["requestId"] = id
	if err := l.send(dest, ns, payload); err != nil {
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case m := <-ch:
		return m, nil
	case <-timer.C:
		return nil, fmt.Errorf("chromecast did not answer")
	case <-l.dead:
		return nil, fmt.Errorf("chromecast closed the connection")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *castLink) launch(ctx context.Context) error {
	// Newer firmware answers LAUNCH with LAUNCH_STATUS (launchRequestId,
	// not requestId) and only later puts the app in RECEIVER_STATUS.
	raw, err := l.request(ctx, "receiver-0", nsCastRecv, map[string]any{
		"type": "LAUNCH", "appId": castAppDefault,
	}, 3*time.Second)
	if err != nil && ctx.Err() != nil {
		return err
	}
	transport, session := "", ""
	if raw != nil {
		transport, session = receiverSession(raw, castAppDefault)
	}
	deadline := time.Now().Add(12 * time.Second)
	for transport == "" && time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		raw, err = l.request(ctx, "receiver-0", nsCastRecv, map[string]any{"type": "GET_STATUS"}, 4*time.Second)
		if err != nil {
			return err
		}
		transport, session = receiverSession(raw, castAppDefault)
		if transport == "" {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-l.dead:
				return fmt.Errorf("chromecast closed the connection")
			case <-time.After(400 * time.Millisecond):
			}
		}
	}
	if transport == "" {
		return fmt.Errorf("chromecast did not open the player")
	}
	l.mu.Lock()
	l.transport = transport
	l.session = session
	l.mu.Unlock()
	return l.send(transport, nsCastConn, map[string]any{"type": "CONNECT", "origin": map[string]any{}})
}

func (l *castLink) load(ctx context.Context, m castLoad) error {
	l.mu.Lock()
	session := l.session
	dest := l.transport
	l.mu.Unlock()
	if dest == "" {
		return fmt.Errorf("chromecast has no player")
	}
	contentType := m.ContentType
	if contentType == "" {
		contentType = "application/vnd.apple.mpegurl"
	}
	media := map[string]any{
		"contentId":   m.URL,
		"contentType": contentType,
		"streamType":  "BUFFERED",
		"metadata":    map[string]any{"metadataType": 0, "title": m.Title},
	}
	if m.AudioFmt != "" {
		media["hlsSegmentFormat"] = m.AudioFmt
	}
	if m.VideoFmt != "" {
		media["hlsVideoSegmentFormat"] = m.VideoFmt
	}
	if m.Duration > 0 {
		media["duration"] = m.Duration
	}
	payload := map[string]any{
		"type": "LOAD", "media": media, "autoplay": m.Autoplay, "currentTime": m.Start,
	}
	if m.SubURL != "" {
		name := m.SubName
		if name == "" {
			name = "Subtitles"
		}
		lang := m.SubLang
		if lang == "" {
			lang = "und"
		}
		media["tracks"] = []any{map[string]any{
			"trackId": 1, "type": "TEXT", "trackContentId": m.SubURL,
			"trackContentType": "text/vtt", "subtype": "SUBTITLES",
			"language": lang, "name": name,
		}}
		payload["activeTrackIds"] = []any{1}
	}
	if session != "" {
		payload["sessionId"] = session
	}
	raw, err := l.request(ctx, dest, nsCastMedia, payload, 12*time.Second)
	if err != nil {
		return err
	}
	if t, _ := raw["type"].(string); t == "LOAD_FAILED" || t == "INVALID_REQUEST" || t == "ERROR" {
		fmt.Fprintf(os.Stderr, "cast load: %s\n", t)
		return fmt.Errorf("chromecast rejected the stream")
	}
	if p, ok := playbackFrom(raw); ok {
		l.setPlayback(p)
	}
	return nil
}

func (l *castLink) poll(ctx context.Context) {
	l.mu.Lock()
	dest := l.transport
	l.mu.Unlock()
	if dest == "" {
		return
	}
	raw, err := l.request(ctx, dest, nsCastMedia, map[string]any{"type": "GET_STATUS"}, 3*time.Second)
	if err != nil {
		return
	}
	if p, ok := playbackFrom(raw); ok {
		l.setPlayback(p)
	}
}

func (l *castLink) command(ctx context.Context, typ string, extra map[string]any) error {
	l.mu.Lock()
	sid := l.mediaSession
	dest := l.transport
	session := l.session
	l.mu.Unlock()
	if sid == 0 || dest == "" {
		return fmt.Errorf("chromecast has no media session")
	}
	payload := map[string]any{"type": typ, "mediaSessionId": sid}
	if session != "" {
		payload["sessionId"] = session
	}
	for k, v := range extra {
		payload[k] = v
	}
	raw, err := l.request(ctx, dest, nsCastMedia, payload, 4*time.Second)
	if err != nil {
		return err
	}
	if p, ok := playbackFrom(raw); ok {
		l.setPlayback(p)
	}
	return nil
}

// shutdown stops the receiver, or pauses it when a track change is about to
// open the same app again. A second call does nothing.
func (l *castLink) shutdown(hold bool) {
	l.mu.Lock()
	if l.shut {
		l.mu.Unlock()
		return
	}
	l.shut = true
	sid := l.mediaSession
	dest := l.transport
	l.mu.Unlock()
	if sid != 0 && dest != "" {
		typ := "STOP"
		if hold {
			typ = "PAUSE"
		}
		l.mu.Lock()
		l.req++
		id := l.req
		l.mu.Unlock()
		_ = l.send(dest, nsCastMedia, map[string]any{
			"type": typ, "mediaSessionId": sid, "requestId": id,
		})
		if !hold {
			_ = l.send(dest, nsCastConn, map[string]any{"type": "CLOSE"})
		}
	}
	if l.conn != nil {
		l.conn.Close()
	}
}
