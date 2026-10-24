package main

import (
	"context"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Safari refuses HLS served as application/octet-stream, and Go's built-in
// table doesn't know these extensions.
func init() {
	mime.AddExtensionType(".m3u8", "application/vnd.apple.mpegurl")
	mime.AddExtensionType(".m4s", "video/iso.segment")
	mime.AddExtensionType(".mp4", "video/mp4")
	mime.AddExtensionType(".vtt", "text/vtt")
}

// StartServer serves dir. master.m3u8 is rewritten on the way out
// (RewriteMasterPlaylist) so the subtitle rendition is on by default and
// labeled with the real track name/language, and given a variant line
// (EnsureVariant, advertising bandwidth) while ffmpeg has not yet written
// one; everything else is a plain file serve. subName/subLang may be empty
// (no-subs casts never produce a master.m3u8, so the rewrite path simply
// never runs for them).
func StartServer(dir, subName, subLang string, bandwidth int) (int, func(), error) {
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return 0, nil, err
	}
	files := http.FileServer(http.Dir(dir))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/master.m3u8" {
			if b, rerr := os.ReadFile(filepath.Join(dir, "master.m3u8")); rerr == nil {
				w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
				io.WriteString(w, EnsureVariant(RewriteMasterPlaylist(string(b), subName, subLang), bandwidth))
				return
			}
		}
		files.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}
	return ln.Addr().(*net.TCPAddr).Port, shutdown, nil
}

// StartMediaServer serves one media file at /media and, when subPath is
// non-empty, one subtitle file at /sub.srt. Both answers support Range:
// a DLNA renderer seeks by asking for bytes, and http.ServeContent does
// that. The TV is only told the URL once the file is complete, so a
// remux that is still growing is simply not linked yet and 404s.
func StartMediaServer(mediaPath, mediaType, subPath string) (int, func(), error) {
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return 0, nil, err
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media":
			serveFile(w, r, mediaPath, mediaType)
		case "/sub.srt":
			if subPath == "" {
				http.NotFound(w, r)
				return
			}
			serveFile(w, r, subPath, "text/srt")
		default:
			http.NotFound(w, r)
		}
	})
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}
	return ln.Addr().(*net.TCPAddr).Port, shutdown, nil
}

func serveFile(w http.ResponseWriter, r *http.Request, path, contentType string) {
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("transferMode.dlna.org", "Interactive")
	w.Header().Set("contentFeatures.dlna.org", dlnaFeatures(contentType))
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}
