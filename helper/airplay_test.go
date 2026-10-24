package main

import "testing"

func TestAirPlayVideoReceiver(t *testing.T) {
	if !airPlayVideo("AppleTV11,1") || !airPlayVideo("AppleTV6,2") {
		t.Fatal("a modern Apple TV is a video receiver")
	}
	for _, model := range []string{"AppleTV3,1", "AppleTV2,1", "MacBookAir10,1", "AudioAccessory6,1", ""} {
		if airPlayVideo(model) {
			t.Fatalf("%s is not a television this plugin can hand a stream to", model)
		}
	}
}

func TestParseAirPlayBrowseAndTXT(t *testing.T) {
	browse := "" +
		"Timestamp     A/R    Flags  if Domain               Service Type         Instance Name\n" +
		"23:14:29.639  Add        3  11 local.               _airplay._tcp.       MacBook Air — Андрей\n" +
		"23:14:29.639  Add        2  12 local.               _airplay._tcp.       CustomCars\r\n" +
		"23:14:29.639  Add        3   1 local.               _airplay._tcp.       MacBook Air — Андрей\n"
	names := airPlayNames(browse)
	if len(names) != 2 || names[0] != "MacBook Air — Андрей" || names[1] != "CustomCars" {
		t.Fatalf("names: %#v", names)
	}
	model, src := parseAirPlayTXT(" features=0x1 srcvers=220.68 model=AppleTV3,1 flags=0x4")
	if model != "AppleTV3,1" || src != "220.68" {
		t.Fatalf("txt: %q %q", model, src)
	}
	model, src = parseAirPlayTXT(" model=AppleTV11,1 srcvers=775.3.1")
	if model != "AppleTV11,1" || src != "775.3.1" || !airPlayVideo(model) {
		t.Fatalf("modern: %q %q", model, src)
	}
}
