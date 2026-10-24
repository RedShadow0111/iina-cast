package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const avTransportURN = "urn:schemas-upnp-org:service:AVTransport:1"

// Device is one DLNA MediaRenderer the TV picker can target.
type Device struct {
	Name    string `json:"name"`
	Control string `json:"control"`
	Host    string `json:"host"`
}

// planDirect reports whether the source file can be handed to a renderer
// as-is. Only an MP4/MOV the TV will open, with copyable video and audio,
// and only when the caller does not need a different audio or subtitle
// than the one the file would play by itself. MKV and the other containers
// are remuxed to MP4: Xiaomi's renderer pulls a single MP4 and ignores the
// track IINA had selected inside a multi-track file. forceRemux is that
// case — more than one audio track, or a subtitle to embed.
func planDirect(source, vcodec, acodec string, forceRemux bool) (bool, string) {
	if forceRemux {
		return false, ""
	}
	switch strings.ToLower(extOf(source)) {
	case ".mp4", ".m4v", ".mov":
	default:
		return false, ""
	}
	switch vcodec {
	case "h264", "hevc":
	default:
		return false, ""
	}
	switch acodec {
	case "aac", "ac3", "eac3", "mp3":
	default:
		return false, ""
	}
	return true, "video/mp4"
}

func mediaMIME(path string) string {
	switch strings.ToLower(extOf(path)) {
	case ".mp4", ".m4v", ".mov":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".ts", ".mts", ".m2ts":
		return "video/mp2t"
	case ".avi":
		return "video/avi"
	default:
		return ""
	}
}

func extOf(path string) string {
	i := strings.LastIndex(path, ".")
	if i < 0 {
		return ""
	}
	return path[i:]
}

// dlnaFeatures is the fourth field of a protocolInfo and the matching
// contentFeatures.dlna.org header. OP=01 tells the renderer it may seek
// with HTTP Range.
func dlnaFeatures(string) string {
	return "DLNA.ORG_OP=01;DLNA.ORG_CI=0"
}

func protocolInfo(mime string) string {
	return "http-get:*:" + mime + ":" + dlnaFeatures(mime)
}

func didlLite(title, uri, mime string, size int64, subURI string) string {
	if strings.TrimSpace(title) == "" {
		title = "Video"
	}
	var b strings.Builder
	b.WriteString(`<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/" xmlns:sec="http://www.sec.co.kr/">`)
	b.WriteString(`<item id="0" parentID="-1" restricted="1">`)
	b.WriteString(`<dc:title>` + xmlEscape(title) + `</dc:title>`)
	b.WriteString(`<upnp:class>object.item.videoItem</upnp:class>`)
	if subURI != "" {
		b.WriteString(`<sec:CaptionInfo sec:type="srt">` + xmlEscape(subURI) + `</sec:CaptionInfo>`)
		b.WriteString(`<sec:CaptionInfoEx sec:type="srt">` + xmlEscape(subURI) + `</sec:CaptionInfoEx>`)
	}
	b.WriteString(`<res protocolInfo="` + xmlEscape(protocolInfo(mime)) + `"`)
	if size > 0 {
		b.WriteString(` size="` + strconv.FormatInt(size, 10) + `"`)
	}
	b.WriteString(`>` + xmlEscape(uri) + `</res></item></DIDL-Lite>`)
	return b.String()
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func formatRelTime(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	t := int(sec)
	return fmt.Sprintf("%d:%02d:%02d", t/3600, (t%3600)/60, t%60)
}

// parseRelTime reads a UPnP REL_TIME value. Renderers that do not report a
// position send "NOT_IMPLEMENTED" or an empty string; those are not zero,
// they are "no clock", and treating them as zero would rewind IINA.
func parseRelTime(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "NOT_IMPLEMENTED") {
		return 0, false
	}
	// Drop a fractional tail some stacks append ("0:01:02.500").
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, false
	}
	var nums [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, false
		}
		nums[i] = n
	}
	return float64(nums[0]*3600 + nums[1]*60 + nums[2]), true
}

func validateControlURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return "", fmt.Errorf("invalid DLNA control URL %q", raw)
	}
	return u.String(), nil
}

type xnode struct {
	local string
	text  string
	kids  []xnode
}

func parseTree(data []byte) (xnode, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return xnode{}, errors.New("empty device description")
		}
		if err != nil {
			return xnode{}, err
		}
		if start, ok := tok.(xml.StartElement); ok {
			return readNode(dec, start)
		}
	}
}

func readNode(dec *xml.Decoder, start xml.StartElement) (xnode, error) {
	n := xnode{local: start.Name.Local}
	for {
		tok, err := dec.Token()
		if err != nil {
			return n, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			kid, err := readNode(dec, t)
			if err != nil {
				return n, err
			}
			n.kids = append(n.kids, kid)
		case xml.EndElement:
			return n, nil
		case xml.CharData:
			n.text += string(t)
		}
	}
}

func childText(n xnode, local string) string {
	for _, k := range n.kids {
		if k.local == local {
			return strings.TrimSpace(k.text)
		}
	}
	return ""
}

func findRenderer(n xnode) (name, control string, ok bool) {
	if n.local == "device" {
		if name, control, ok = rendererHere(n); ok {
			return name, control, true
		}
	}
	for _, k := range n.kids {
		if name, control, ok = findRenderer(k); ok {
			return name, control, true
		}
	}
	return "", "", false
}

func rendererHere(device xnode) (string, string, bool) {
	name := childText(device, "friendlyName")
	for _, k := range device.kids {
		if k.local != "serviceList" {
			continue
		}
		for _, s := range k.kids {
			if s.local != "service" {
				continue
			}
			if strings.Contains(childText(s, "serviceType"), "AVTransport") {
				ctl := childText(s, "controlURL")
				if ctl == "" {
					return "", "", false
				}
				return name, ctl, true
			}
		}
	}
	return "", "", false
}

func resolveControl(location, control string) (string, error) {
	control = strings.TrimSpace(control)
	if control == "" {
		return "", errors.New("empty control URL")
	}
	if strings.HasPrefix(control, "http://") || strings.HasPrefix(control, "https://") {
		return control, nil
	}
	base, err := url.Parse(location)
	if err != nil {
		return "", err
	}
	// A URLBase of "http://host:port" has an empty path. Treat that as the
	// root. The control path is joined by hand: Xiaomi's controlURL contains
	// colons ("_urn:schemas-upnp-org:service:AVTransport_control"), and
	// url.Parse rejects that as a scheme.
	if base.Path == "" {
		base.Path = "/"
	}
	if strings.HasPrefix(control, "/") {
		base.Path = control
	} else {
		dir := base.Path
		if !strings.HasSuffix(dir, "/") {
			if i := strings.LastIndex(dir, "/"); i >= 0 {
				dir = dir[:i+1]
			} else {
				dir = "/"
			}
		}
		base.Path = dir + control
	}
	base.RawQuery = ""
	base.Fragment = ""
	if base.Scheme != "http" && base.Scheme != "https" {
		return "", fmt.Errorf("control URL %q is not http", base.String())
	}
	return base.String(), nil
}

// parseDeviceDescription pulls the AVTransport control URL out of a UPnP
// device description. The friendly name is the device that actually owns
// that service, so a gateway that embeds the renderer does not lend the
// renderer its own name.
func parseDeviceDescription(body []byte, location string) (Device, error) {
	root, err := parseTree(body)
	if err != nil {
		return Device{}, err
	}
	name, control, ok := findRenderer(root)
	if !ok {
		return Device{}, errors.New("no AVTransport service")
	}
	base := location
	if ub := findLocalText(root, "URLBase"); ub != "" {
		base = ub
	}
	abs, err := resolveControl(base, control)
	if err != nil {
		return Device{}, err
	}
	if name == "" {
		name = hostOf(abs)
	}
	return Device{Name: name, Control: abs, Host: hostOf(abs)}, nil
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	return host
}

func parseSSDPLocation(pkt string) (string, bool) {
	for _, line := range strings.Split(pkt, "\n") {
		line = strings.TrimRight(line, "\r")
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(key), "LOCATION") {
			loc := strings.TrimSpace(val)
			if strings.HasPrefix(strings.ToLower(loc), "http://") || strings.HasPrefix(strings.ToLower(loc), "https://") {
				return loc, true
			}
		}
	}
	return "", false
}

// locationFromPacket reads a LOCATION header and, when the advertised host
// is not the address the packet came from, rewrites the host. Xiaomi sends
// some NOTIFY packets with a placeholder location (http://10.10.10.10/...)
// while the datagram itself arrives from the TV's real LAN address.
func locationFromPacket(pkt string, src net.IP) (string, bool) {
	loc, ok := parseSSDPLocation(pkt)
	if !ok {
		return "", false
	}
	u, err := url.Parse(loc)
	if err != nil {
		return "", false
	}
	src4 := src.To4()
	if src4 != nil && u.Hostname() != src4.String() {
		port := u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		u.Host = net.JoinHostPort(src4.String(), port)
	}
	return u.String(), true
}

var dlnaClient = &http.Client{Timeout: 8 * time.Second}

func soapEnvelope(action string, args [][2]string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
	b.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	b.WriteString(`<u:` + action + ` xmlns:u="` + avTransportURN + `">`)
	for _, a := range args {
		b.WriteString(`<` + a[0] + `>` + xmlEscape(a[1]) + `</` + a[0] + `>`)
	}
	b.WriteString(`</u:` + action + `></s:Body></s:Envelope>`)
	return b.String()
}

func avAction(ctx context.Context, control, action string, args [][2]string) ([]byte, error) {
	body := soapEnvelope(action, args)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, control, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+avTransportURN+"#"+action+`"`)
	resp, err := dlnaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		if msg := xmlLocalText(buf, "faultstring"); msg != "" {
			return nil, fmt.Errorf("%s: %s", action, msg)
		}
		return nil, fmt.Errorf("%s: HTTP %d", action, resp.StatusCode)
	}
	if msg := xmlLocalText(buf, "faultstring"); msg != "" && bytes.Contains(buf, []byte("Fault")) {
		return nil, fmt.Errorf("%s: %s", action, msg)
	}
	return buf, nil
}

func xmlLocalText(data []byte, local string) string {
	if len(data) == 0 {
		return ""
	}
	root, err := parseTree(data)
	if err != nil {
		return ""
	}
	return findLocalText(root, local)
}

func findLocalText(n xnode, local string) string {
	if n.local == local {
		return strings.TrimSpace(n.text)
	}
	for _, k := range n.kids {
		if s := findLocalText(k, local); s != "" {
			return s
		}
	}
	return ""
}

func dlnaPlay(ctx context.Context, control, uri, meta string) error {
	if _, err := avAction(ctx, control, "SetAVTransportURI", [][2]string{
		{"InstanceID", "0"},
		{"CurrentURI", uri},
		{"CurrentURIMetaData", meta},
	}); err != nil {
		return err
	}
	_, err := avAction(ctx, control, "Play", [][2]string{
		{"InstanceID", "0"},
		{"Speed", "1"},
	})
	return err
}

func dlnaSeek(ctx context.Context, control string, sec float64) error {
	_, err := avAction(ctx, control, "Seek", [][2]string{
		{"InstanceID", "0"},
		{"Unit", "REL_TIME"},
		{"Target", formatRelTime(sec)},
	})
	return err
}

func dlnaStop(ctx context.Context, control string) error {
	_, err := avAction(ctx, control, "Stop", [][2]string{{"InstanceID", "0"}})
	return err
}

// Transport is one poll of the renderer. PosOK is false when the TV did not
// report a position; callers must not treat that as "at the start".
type Transport struct {
	Pos    float64
	PosOK  bool
	Paused bool
	Ended  bool
}

func readTransport(ctx context.Context, control string, seenPlay bool) (Transport, bool, error) {
	stateBody, err := avAction(ctx, control, "GetTransportInfo", [][2]string{{"InstanceID", "0"}})
	if err != nil {
		return Transport{}, seenPlay, err
	}
	posBody, err := avAction(ctx, control, "GetPositionInfo", [][2]string{{"InstanceID", "0"}})
	if err != nil {
		return Transport{}, seenPlay, err
	}
	state := xmlLocalText(stateBody, "CurrentTransportState")
	pos, posOK := parseRelTime(xmlLocalText(posBody, "RelTime"))
	playing := state == "PLAYING" || state == "PAUSED_PLAYBACK" || state == "TRANSITIONING"
	if playing {
		seenPlay = true
	}
	tr := Transport{
		Pos:    pos,
		PosOK:  posOK,
		Paused: state == "PAUSED_PLAYBACK",
	}
	// STOPPED before the renderer has ever been seen playing is just the
	// gap between SetAVTransportURI and Play, not the end of the film.
	if seenPlay && (state == "STOPPED" || state == "NO_MEDIA_PRESENT") {
		tr.Ended = true
	}
	return tr, seenPlay, nil
}

type located struct{ uri string }

func ifaceForIPv4(ip net.IP) *net.Interface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || !ipn.IP.To4().Equal(ip) {
				continue
			}
			copy := iface
			return &copy
		}
	}
	return nil
}

// collectNotifies reads unsolicited SSDP alive advertisements. The channel
// is closed when ctx ends, including when joining the multicast group fails,
// so the caller can range it without a second completion signal.
func collectNotifies(ctx context.Context, ip net.IP, out chan<- located) {
	defer close(out)
	iface := ifaceForIPv4(ip)
	if iface == nil {
		return
	}
	conn, err := net.ListenMulticastUDP("udp4", iface, &net.UDPAddr{
		IP: net.IPv4(239, 255, 255, 250), Port: 1900,
	})
	if err != nil {
		return
	}
	defer conn.Close()
	buf := make([]byte, 8192)
	for ctx.Err() == nil {
		conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		loc, ok := locationFromPacket(string(buf[:n]), addr.IP)
		if !ok {
			continue
		}
		select {
		case out <- located{uri: loc}:
		case <-ctx.Done():
			return
		}
	}
}

// DiscoverRenderers finds MediaRenderers on the LAN. It sends SSDP
// M-SEARCH and also listens for the unsolicited NOTIFY advertisements
// some TVs (Xiaomi's renderer among them) send instead of answering a
// search. localIP is the LAN address the TV can route back to; binding
// the query there keeps a VPN tunnel from swallowing the multicast.
func DiscoverRenderers(ctx context.Context, localIP string) ([]Device, error) {
	ip := net.ParseIP(localIP).To4()
	if ip == nil {
		return nil, fmt.Errorf("DLNA discovery needs an IPv4 address, got %q", localIP)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: 0})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	dst := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	for _, st := range []string{
		"urn:schemas-upnp-org:device:MediaRenderer:1",
		avTransportURN,
		"upnp:rootdevice",
	} {
		msg := "M-SEARCH * HTTP/1.1\r\n" +
			"HOST: 239.255.255.250:1900\r\n" +
			"MAN: \"ssdp:discover\"\r\n" +
			"MX: 2\r\n" +
			"ST: " + st + "\r\n\r\n"
		if _, err := conn.WriteToUDP([]byte(msg), dst); err != nil {
			return nil, err
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	listenCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	notes := make(chan located, 32)
	go collectNotifies(listenCtx, ip, notes)

	seen := map[string]bool{}
	var locations []string
	add := func(loc string) {
		if loc == "" || seen[loc] {
			return
		}
		seen[loc] = true
		locations = append(locations, loc)
	}
	buf := make([]byte, 8192)
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			break
		}
		conn.SetReadDeadline(time.Now().Add(remain))
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				break
			}
			select {
			case loc := <-notes:
				add(loc.uri)
				continue
			default:
			}
			return nil, err
		}
		if loc, ok := locationFromPacket(string(buf[:n]), addr.IP); ok {
			add(loc)
		}
		// Drain advertisements that arrived during this read.
		for {
			select {
			case loc := <-notes:
				add(loc.uri)
			default:
				goto next
			}
		}
	next:
	}
	cancel()
	for loc := range notes {
		add(loc.uri)
	}
	var devices []Device
	got := map[string]bool{}
	for _, loc := range locations {
		d, err := fetchRenderer(ctx, loc)
		if err != nil {
			continue
		}
		if got[d.Control] {
			continue
		}
		got[d.Control] = true
		devices = append(devices, d)
		if len(devices) == 16 {
			break
		}
	}
	return devices, nil
}

func fetchRenderer(ctx context.Context, location string) (Device, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return Device{}, err
	}
	resp, err := dlnaClient.Do(req)
	if err != nil {
		return Device{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return Device{}, fmt.Errorf("description HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Device{}, err
	}
	return parseDeviceDescription(body, location)
}
