package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlanDirect(t *testing.T) {
	ok, mime := planDirect("/movies/a.mkv", "hevc", "aac", false)
	if !ok || mime != "video/x-matroska" {
		t.Fatalf("mkv hevc/aac should be served directly, got %v %q", ok, mime)
	}
	ok, mime = planDirect("/movies/a.mp4", "h264", "eac3", false)
	if !ok || mime != "video/mp4" {
		t.Fatalf("mp4 h264/eac3 should be served directly, got %v %q", ok, mime)
	}
	ok, mime = planDirect("/movies/a.ts", "h264", "ac3", false)
	if !ok || mime != "video/mp2t" {
		t.Fatalf("mpegts should be served directly, got %v %q", ok, mime)
	}
	if ok, _ = planDirect("/movies/a.mp4", "h264", "aac", true); ok {
		t.Fatal("a non-default audio track forces a remux")
	}
	if ok, _ = planDirect("/movies/a.mkv", "hevc", "truehd", false); ok {
		t.Fatal("truehd audio has to be transcoded")
	}
	if ok, _ = planDirect("/movies/a.mp4", "vp9", "aac", false); ok {
		t.Fatal("vp9 video has to be transcoded")
	}
	if ok, _ = planDirect("/movies/a.wmv", "h264", "aac", false); ok {
		t.Fatal("wmv is not a container the renderer is handed directly")
	}
}

func TestParseRelTime(t *testing.T) {
	got, ok := parseRelTime("1:02:03")
	if !ok || got != 3723 {
		t.Fatalf("got %v %v", got, ok)
	}
	if _, ok := parseRelTime("NOT_IMPLEMENTED"); ok {
		t.Fatal("NOT_IMPLEMENTED is not position zero")
	}
	if _, ok := parseRelTime(""); ok {
		t.Fatal("empty is not a position")
	}
	got, ok = parseRelTime("0:00:05.250")
	if !ok || got != 5 {
		t.Fatalf("fractional: got %v %v", got, ok)
	}
	if formatRelTime(3723) != "1:02:03" {
		t.Fatalf("format: %s", formatRelTime(3723))
	}
}

func TestDIDLEscapesTitleAndCarriesSubtitle(t *testing.T) {
	doc := didlLite(`A & B`, "http://10.0.0.5:9/media", "video/mp4", 100, "http://10.0.0.5:9/sub.srt", 3723)
	if !strings.Contains(doc, "A &amp; B") {
		t.Fatalf("title was not escaped:\n%s", doc)
	}
	if !strings.Contains(doc, `protocolInfo="http-get:*:video/mp4:DLNA.ORG_OP=11;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=60F00000000000000000000000000000"`) {
		t.Fatalf("protocolInfo missing:\n%s", doc)
	}
	if !strings.Contains(doc, `duration="1:02:03"`) {
		t.Fatalf("duration missing:\n%s", doc)
	}
	if !strings.Contains(doc, `size="100"`) || !strings.Contains(doc, `sec:CaptionInfoEx`) || !strings.Contains(doc, `<sec:CaptionInfo `) {
		t.Fatalf("size or subtitle missing:\n%s", doc)
	}
	if !strings.Contains(doc, ">http://10.0.0.5:9/media</res>") {
		t.Fatalf("media url missing:\n%s", doc)
	}
}

func TestParseDeviceDescriptionNestedRenderer(t *testing.T) {
	body := []byte(`<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <friendlyName>Router</friendlyName>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
        <controlURL>/wan</controlURL>
      </service>
    </serviceList>
    <deviceList>
      <device>
        <friendlyName>CustomCars</friendlyName>
        <serviceList>
          <service>
            <serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>
            <controlURL>/upnp/control/AVTransport</controlURL>
          </service>
        </serviceList>
      </device>
    </deviceList>
  </device>
</root>`)
	d, err := parseDeviceDescription(body, "http://192.168.0.220:49152/desc.xml")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "CustomCars" || d.Host != "192.168.0.220" {
		t.Fatalf("name/host: %+v", d)
	}
	if d.Control != "http://192.168.0.220:49152/upnp/control/AVTransport" {
		t.Fatalf("control: %s", d.Control)
	}
}

func TestParseDeviceDescriptionRejectsNonRenderer(t *testing.T) {
	body := []byte(`<root xmlns="urn:schemas-upnp-org:device-1-0"><device><friendlyName>Printer</friendlyName></device></root>`)
	if _, err := parseDeviceDescription(body, "http://192.168.0.1/desc.xml"); err == nil {
		t.Fatal("expected no AVTransport")
	}
}

func TestLocationFromPacketRewritesPlaceholderHost(t *testing.T) {
	pkt := "NOTIFY * HTTP/1.1\r\nLOCATION: http://10.10.10.10:49152/description.xml\r\nNT: upnp:rootdevice\r\n\r\n"
	loc, ok := locationFromPacket(pkt, net.ParseIP("192.168.0.220"))
	if !ok || loc != "http://192.168.0.220:49152/description.xml" {
		t.Fatalf("got %q %v", loc, ok)
	}
	same, ok := locationFromPacket("HTTP/1.1 200 OK\r\nLOCATION: http://192.168.0.220:49152/description.xml\r\n\r\n", net.ParseIP("192.168.0.220"))
	if !ok || same != "http://192.168.0.220:49152/description.xml" {
		t.Fatalf("matching host was rewritten: %q", same)
	}
}

func TestParseDeviceDescriptionUsesURLBase(t *testing.T) {
	body := []byte(`<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <friendlyName>CustomCars</friendlyName>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>
        <controlURL>_urn:schemas-upnp-org:service:AVTransport_control</controlURL>
      </service>
    </serviceList>
  </device>
  <URLBase>http://192.168.0.220:49152</URLBase>
</root>`)
	d, err := parseDeviceDescription(body, "http://192.168.0.220:49152/description.xml")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "CustomCars" || d.Control != "http://192.168.0.220:49152/_urn:schemas-upnp-org:service:AVTransport_control" {
		t.Fatalf("%+v", d)
	}
}

func TestParseSSDPLocation(t *testing.T) {
	pkt := "HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=1800\r\nLOCATION: http://192.168.0.220:49152/desc.xml\r\nST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n\r\n"
	loc, ok := parseSSDPLocation(pkt)
	if !ok || loc != "http://192.168.0.220:49152/desc.xml" {
		t.Fatalf("got %q %v", loc, ok)
	}
	if _, ok := parseSSDPLocation("HTTP/1.1 200 OK\r\nST: upnp:rootdevice\r\n"); ok {
		t.Fatal("a reply without LOCATION is not a device")
	}
}

func TestDLNAPlaySendsURIThenPlay(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		action := r.Header.Get("SOAPAction")
		calls = append(calls, action)
		if !strings.Contains(action, "#") {
			t.Errorf("SOAPAction %q", action)
		}
		if strings.Contains(action, "SetAVTransportURI") {
			if !strings.Contains(string(body), "http://10.0.0.8/media") || !strings.Contains(string(body), "A &amp;amp; B") {
				t.Errorf("body:\n%s", body)
			}
		}
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		w.Write([]byte(`<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body></s:Body></s:Envelope>`))
	}))
	defer srv.Close()
	if err := dlnaPlay(t.Context(), srv.URL, "http://10.0.0.8/media", didlLite("A & B", "http://10.0.0.8/media", "video/mp4", 0, "", 0)); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || !strings.Contains(calls[0], "SetAVTransportURI") || !strings.Contains(calls[1], "Play") {
		t.Fatalf("calls: %v", calls)
	}
}

func TestAVActionSurfacesFault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault><faultstring>Transition not available</faultstring></s:Fault></s:Body></s:Envelope>`))
	}))
	defer srv.Close()
	_, err := avAction(t.Context(), srv.URL, "Play", [][2]string{{"InstanceID", "0"}, {"Speed", "1"}})
	if err == nil || !strings.Contains(err.Error(), "Transition not available") {
		t.Fatalf("got %v", err)
	}
}

func TestParkPlaybackSeeksUntilTheClockMatches(t *testing.T) {
	pos := "0:00:00"
	state := "PLAYING"
	var seeks []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		action := r.Header.Get("SOAPAction")
		payload := ""
		switch {
		case strings.Contains(action, "Seek"):
			seeks = append(seeks, string(body))
			if strings.Contains(string(body), "0:10:00") || strings.Contains(string(body), "00:10:00") {
				pos = "0:10:00"
			}
		case strings.Contains(action, "Pause"):
			state = "PAUSED_PLAYBACK"
		case strings.Contains(action, "GetTransportInfo"):
			payload = "<CurrentTransportState>" + state + "</CurrentTransportState>"
		case strings.Contains(action, "GetPositionInfo"):
			payload = "<RelTime>" + pos + "</RelTime>"
		}
		w.Write([]byte(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` + payload + `</s:Body></s:Envelope>`))
	}))
	defer srv.Close()
	if !parkPlayback(t.Context(), srv.URL, 600, true) {
		t.Fatal("seek never landed on 10:00")
	}
	if len(seeks) == 0 || state != "PAUSED_PLAYBACK" || pos != "0:10:00" {
		t.Fatalf("seeks=%d state=%s pos=%s", len(seeks), state, pos)
	}
}

func TestReadTransport(t *testing.T) {
	payloads := []string{
		`<CurrentTransportState>STOPPED</CurrentTransportState>`,
		`<RelTime>0:00:00</RelTime>`,
		`<CurrentTransportState>PLAYING</CurrentTransportState>`,
		`<RelTime>0:00:01</RelTime>`,
		`<CurrentTransportState>PAUSED_PLAYBACK</CurrentTransportState>`,
		`<RelTime>0:00:02</RelTime>`,
	}
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := payloads[i]
		i++
		w.Write([]byte(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` + payload + `</s:Body></s:Envelope>`))
	}))
	defer srv.Close()

	tr, seen, err := readTransport(t.Context(), srv.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	if seen || tr.Ended || tr.Pos != 0 {
		t.Fatalf("initial STOPPED must not end the film: %+v seen=%v", tr, seen)
	}
	tr, seen, err = readTransport(t.Context(), srv.URL, seen)
	if err != nil {
		t.Fatal(err)
	}
	if !seen || tr.Paused || !tr.PosOK || tr.Pos != 1 {
		t.Fatalf("playing: %+v seen=%v", tr, seen)
	}
	tr, _, err = readTransport(t.Context(), srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	if !tr.Paused {
		t.Fatalf("expected pause, got %+v", tr)
	}
}

func TestValidateControlURL(t *testing.T) {
	if _, err := validateControlURL("file:///etc/passwd"); err == nil {
		t.Fatal("file URL must be rejected")
	}
	if _, err := validateControlURL("http://192.168.0.220/ctl"); err != nil {
		t.Fatal(err)
	}
}
