// plugin/main.js — orchestration. HARD RULE (docs/prototype.md): never call
// sidebar.* from utils.exec callbacks/promises; IINA runs those off the main
// thread and sidebar.show() SIGABRTs the app. Sidebar calls live only in
// main-thread contexts: plugin load, the iina.window-loaded event, the menu
// callback and onMessage handlers; the page polls "getState".

// ---- pure functions (node-testable) ----

// Sub codecs ffmpeg can convert to WebVTT. Image codecs (hdmv_pgs_subtitle,
// dvd_subtitle) and anything unknown can't pass through to the TV without a
// burn-in re-encode — out of scope, see docs/superpowers/specs/.
var TEXT_SUB_CODECS = { subrip: 1, srt: 1, ass: 1, ssa: 1, mov_text: 1, webvtt: 1, text: 1 };

function selectTracks(trackList, subsOn) {
  var video = null, audio = null, firstAudio = null, sub = null;
  for (var i = 0; i < trackList.length; i++) {
    var t = trackList[i];
    if (t.type === "video" && (!video || t.selected)) video = t;
    if (t.type === "audio") {
      if (firstAudio === null) firstAudio = t;
      if (t.selected) audio = t;
    }
    // IINA's subtitle button hides the track with sub-visibility and leaves
    // it selected. A selected "ru" track is not on unless that flag says so.
    // Omitting the flag (tests, older callers) keeps the selected track.
    if (t.type === "sub" && t.selected && subsOn !== false) sub = t;
  }
  if (!video) return null;
  if (!audio) audio = firstAudio;
  if (!audio) return null;
  var audioCount = 0;
  for (var j = 0; j < trackList.length; j++) {
    if (trackList[j].type === "audio") audioCount++;
  }
  var r = {
    vcodec: video.codec,
    acodec: audio.codec,
    achannels: audio["demux-channel-count"] || 2,
    vmap: video["ff-index"],
    amap: audio["ff-index"],
    firstAmap: firstAudio["ff-index"],
    audioCount: audioCount,
    sub: null,
    subDropped: false,
  };
  if (sub) {
    var path = sub.external ? (sub["external-filename"] || null) : null;
    if (!TEXT_SUB_CODECS[sub.codec] || (sub.external && !path)) {
      r.subDropped = true;
    } else {
      r.sub = { path: path, smap: sub["ff-index"], lang: sub.lang || "", title: sub.title || "" };
    }
  }
  return r;
}

// selectTracks already decided what happens to subtitles; this just names the
// decision for the sidebar. Before the redesign that answer existed only as a
// one-shot OSD flash, which vanished before it could be read.
function subtitleLabel(tracks) {
  if (!tracks) return { label: "", warn: false };
  if (tracks.subDropped) return { label: "Subtitles not supported", warn: true };
  if (!tracks.sub) return { label: "No subtitles", warn: false };
  return { label: "Subtitles: " + (tracks.sub.lang || tracks.sub.title || "On"),
           warn: false };
}

function parseHelperEvents(buffer, chunk) {
  var data = buffer + chunk;
  var lines = data.split("\n");
  var rest = lines.pop(); // trailing partial line (or "")
  var events = [];
  for (var i = 0; i < lines.length; i++) {
    var line = lines[i].trim();
    if (!line) continue;
    try {
      var obj = JSON.parse(line);
      if (obj && obj.event) events.push(obj);
    } catch (e) { /* stray non-JSON output; ignore */ }
  }
  return { events: events, rest: rest };
}

// utils.resolvePath("@data/") resolves to an absolute path of the form
// <...>/plugins/.data/<identifier> (verified on-disk against a running IINA
// 1.4.4 install — there is no "plugins-data" directory). The plugins
// directory (where the plugin bundle — and thus bin/ — actually lives) is
// the parent of the ".data" component. Returns null if the path doesn't
// have the expected shape.
function pluginsDirFromDataDir(dataPath) {
  if (!dataPath) return null;
  var normalized = dataPath.replace(/\/+$/, ""); // strip trailing slash(es)
  var idx = normalized.lastIndexOf("/.data/");
  if (idx === -1) return null;
  return normalized.slice(0, idx);
}

function isValidPid(pid) {
  return typeof pid === "number" && isFinite(pid) && pid > 0;
}

// mpv's "path" is a URL whenever IINA is playing a network stream. The bundled
// ffmpeg is built --disable-network and has no protocol handler for those, so
// the plugin declines up front instead of letting ffmpeg fail obscurely.
function hasURLScheme(p) {
  return !!p && /^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//.test(p);
}

// mpv usually reports a bare filesystem path, but can hand back a file:// URI.
// That names a local file we can cast, so strip the scheme and percent-decode
// rather than declining it — and do this before the URL-scheme test, which
// would otherwise call it a network stream.
function normalizeSource(p) {
  if (!p || p.indexOf("file://") !== 0) return p;
  var rest = p.slice("file://".length);
  if (rest.indexOf("localhost/") === 0) rest = rest.slice("localhost".length);
  try { return decodeURIComponent(rest); } catch (e) { return rest; }
}

// The "LAN address" preference (Info.json preferenceDefaults.lanIP, edited in
// Settings → Plugins → iina cast → Preferences) is handed to the helper as
// `serve -ip <addr>` when non-empty. The helper validates it (IPv4, assigned
// to this Mac, interface up) and reports a bad value as an `error` event the
// sidebar already shows, so the plugin only trims. Anything that isn't a
// string — unset, or a stale non-string in the preferences store — means
// automatic.
function lanIPOverride(value) {
  return typeof value === "string" ? value.trim() : "";
}

// The DLNA renderer shows this as the title of what's playing. The path's
// last component, without its extension, is the name IINA is already using.
function fileTitle(p) {
  if (!p) return "Video";
  var base = String(p).replace(/\\/g, "/").split("/").pop();
  var dot = base.lastIndexOf(".");
  if (dot > 0) base = base.slice(0, dot);
  return base || "Video";
}

// A video AirPlay receiver is a modern Apple TV. AppleTV1/2/3 is the identity
// cheap receivers advertise (the Xiaomi on this LAN does), and a real Apple
// TV 3 cannot pull this stream either. Macs and HomePods also answer
// _airplay._tcp; neither is a television for this cast.
function isVideoAirPlay(device) {
  var model = String(device && device.model || "").replace(/^"+|"+$/g, "");
  if (model.indexOf("AppleTV") !== 0) return false;
  return !/^AppleTV[123],/.test(model);
}

// One real target casts immediately. Two or more — a DLNA renderer, a
// Chromecast and a modern Apple TV each count — wait for a tap. A receiver
// that only pretends to be an Apple TV does not count, and every real
// Apple TV shares one system picker, so they are a single choice.
function chooseCast(dlna, airplay, cast) {
  var options = [];
  var renderers = dlna || [];
  for (var i = 0; i < renderers.length; i++) {
    var d = renderers[i];
    options.push({
      kind: "dlna",
      name: d.name || d.host || "TV",
      control: d.control || "",
      host: d.host || "",
    });
  }
  var list = airplay || [];
  for (var j = 0; j < list.length; j++) {
    if (!isVideoAirPlay(list[j])) continue;
    options.push({
      kind: "airplay",
      name: list[j].name || "AirPlay",
      control: "",
      host: "",
    });
    break;
  }
  var casts = cast || [];
  for (var k = 0; k < casts.length; k++) {
    var g = casts[k];
    if (!g || !g.host) continue;
    var port = g.port || 8009;
    options.push({
      kind: "cast",
      name: g.name || "Chromecast",
      host: g.host,
      port: port,
      model: g.model || "",
      control: g.host + ":" + port,
    });
  }
  if (options.length === 1 && options[0].kind === "dlna") {
    return { mode: "dlna", device: options[0] };
  }
  if (options.length === 1 && options[0].kind === "airplay") return { mode: "airplay" };
  if (options.length === 1 && options[0].kind === "cast") {
    return { mode: "cast", device: options[0] };
  }
  if (options.length > 1) return { mode: "choose", devices: options };
  return { mode: "none" };
}

// The renderer plays the file's first audio track and seeks that file by its
// own index. Remux only when IINA has selected a later audio track, which the
// file would not play by itself. Subtitles are a sidecar, and a hidden
// subtitle is not a reason to copy a large file.
function needsSelectedRemux(tracks) {
  if (!tracks) return false;
  return tracks.amap !== tracks.firstAmap;
}

// WebKit reports a stream it couldn't fetch at all (refused, unreachable,
// 404) as media error 4 with an empty MediaError.message, which is all a user
// or a bug report ever saw (issue #42). The plugin probes the URL with curl,
// spawned by IINA so the Mac's firewall, VPN and network filters treat it as
// they treat IINA's webview, and names what it found. `probe` is utils.exec's
// {status, stdout, stderr} for `curl -w %{http_code}`, or null if curl
// couldn't run.
function describeMediaFailure(url, code, probe) {
  var err = "media error " + (code === undefined || code === null ? "?" : code);
  var hostPort = String(url).replace(/^[a-z]+:\/\//i, "").split("/")[0];
  if (!probe) return "Stream failed to load (" + err + ") from " + hostPort + ".";
  if (probe.status !== 0) {
    var reason = String(probe.stderr || "").trim().replace(/^curl: \(\d+\) /, "") ||
                 "curl exit " + probe.status;
    return "This Mac can't reach its own stream at " + hostPort + ": " + reason +
           ". A firewall, VPN or network filter may be blocking it, or the " +
           "address is wrong — set LAN address in the plugin's preferences.";
  }
  var http = String(probe.stdout || "").trim();
  if (http !== "200") return "The stream server answered HTTP " + http + " (" + err + ").";
  return "This Mac reaches the stream at " + url + ", but WebKit wouldn't play it (" + err + ").";
}

// ---- muted-mirror sync core (spec: docs/superpowers/specs/2026-08-30-native-controls-mirror-design.md) ----
// The TV is the clock; mpv is the mirror. AirPlay HLS runs seconds behind any
// local clock, so mpv's position is never authoritative during a cast. These
// functions are pure — they take the mirror bookkeeping object plus
// observations and return new state/actions without touching IINA APIs — so
// node tests can drive every branch.

var DRIFT_TOLERANCE = 1.5; // seconds of mpv/TV divergence tolerated before correcting
var CLOCK_SEEK_LIMIT = 8; // larger gaps are a bad timeline, not drift: seeking the local file stalls it
var ECHO_WINDOW_MS = 2000; // how long our own time-pos set may echo back as an mpv seek event
var SEEK_HOLD_MS = 8000;   // how long a user seek is protected from a stale TV clock

function newMirror(startPos, savedMute, paused) {
  return {
    seq: 0,               // last issued command sequence number
    paused: paused,       // desired TV pause state (follows mpv)
    seekTo: null,         // pending user seek {seq, pos}; cleared when the page acks it
    startPos: startPos,   // where IINA was at cast start; the page best-effort seeks the TV here
    savedMute: savedMute, // mpv mute flag to restore on teardown
    expectMpvPause: null, // swallow the next pause.changed that echoes our own mpv.set
    lastSetPos: null,     // {pos, at} of our last programmatic time-pos set (echo suppression)
    holdPos: 0,           // absolute position of a user seek the TV has not reached yet
    holdUntil: 0,         // ms timestamp; until then a stale TV clock must not pull mpv back
  };
}

// mpv's pause flag changed. Bumps seq only for user-initiated changes; an
// expected echo of our own mpv.set(pause) must not become a command back to
// the TV, or TV-remote pauses would ping-pong.
function mirrorOnMpvPause(m, mpvPaused) {
  var n = Object.assign({}, m);
  if (m.expectMpvPause !== null && mpvPaused === m.expectMpvPause) {
    n.expectMpvPause = null;
    return n;
  }
  n.expectMpvPause = null;
  if (mpvPaused !== m.paused) {
    n.paused = mpvPaused;
    n.seq = m.seq + 1;
  }
  return n;
}

// mpv seeked. A seek near our own recent time-pos set is drift correction
// echoing back, not the user; anything else becomes a TV seek command.
// seekTo carries its own seq so the page never re-applies a seek it has
// already performed when a later pause command bumps the outer seq.
function mirrorOnMpvSeek(m, mpvPos, now) {
  var n = Object.assign({}, m);
  if (m.lastSetPos !== null &&
      now - m.lastSetPos.at < ECHO_WINDOW_MS &&
      Math.abs(mpvPos - m.lastSetPos.pos) <= DRIFT_TOLERANCE) {
    n.lastSetPos = null;
    return n;
  }
  n.seq = m.seq + 1;
  n.seekTo = { seq: n.seq, pos: mpvPos };
  // The page acks a seek as soon as it sends it, while the TV is still at
  // the old clock. Hold mpv on the requested position until the TV arrives
  // or the hold expires, so drift correction cannot undo the seek.
  n.holdPos = mpvPos;
  n.holdUntil = now + SEEK_HOLD_MS;
  return n;
}

// The page reported TV state. Decides what (if anything) to push into mpv.
// While commands are in flight (appliedSeq < seq) the TV state is stale, so
// nothing is reconciled against it — not even drift. ended only tears the
// cast down once the TV is actually the one playing: a short file's LOCAL
// (muted, hidden) playback ending before the user ever picks a TV must not
// kill the cast. An acked seekTo is cleared as soon as it's acked, even
// before wireless playback starts, so a pre-wireless user seek doesn't leave
// a stale command sitting in the sync block.
function mirrorOnTvState(m, tvState, mpvPos, mpvPaused, now) {
  var out = { m: m, setMpvPos: null, setMpvPaused: null, teardown: false };
  if (tvState.ended && tvState.wireless) { out.teardown = true; return out; }
  var n = Object.assign({}, m);
  var acked = tvState.appliedSeq >= m.seq;
  if (acked) n.seekTo = null;
  out.m = n;
  if (!tvState.wireless || !acked) return out; // nothing on the TV yet, or a command is in flight: no clock to follow
  if (tvState.paused !== n.paused) {      // TV-remote initiated: mirror into mpv
    n.paused = tvState.paused;
    n.expectMpvPause = tvState.paused;
    out.setMpvPaused = tvState.paused;
  }
  if (n.holdUntil && now < n.holdUntil) {
    if (Math.abs(tvState.pos - n.holdPos) <= DRIFT_TOLERANCE) n.holdUntil = 0;
    else return out;
  }
  // While either side is paused, moving time-pos makes IINA reopen a large
  // file and flash its cache-speed meter, and the TV rebuffers with it.
  // A gap of many minutes is the same failure: the TV clock and the movie
  // clock diverged, and pulling the local file across that gap stalls both.
  var gap = Math.abs(mpvPos - tvState.pos);
  if (!mpvPaused && !tvState.paused && gap > DRIFT_TOLERANCE && gap <= CLOCK_SEEK_LIMIT) {
    out.setMpvPos = tvState.pos;
    n.lastSetPos = { pos: tvState.pos, at: now };
  }
  return out;
}

if (typeof module !== "undefined") {
  module.exports = {
    selectTracks: selectTracks,
    subtitleLabel: subtitleLabel,
    parseHelperEvents: parseHelperEvents,
    pluginsDirFromDataDir: pluginsDirFromDataDir,
    isValidPid: isValidPid,
    hasURLScheme: hasURLScheme,
    normalizeSource: normalizeSource,
    lanIPOverride: lanIPOverride,
    fileTitle: fileTitle,
    isVideoAirPlay: isVideoAirPlay,
    chooseCast: chooseCast,
    needsSelectedRemux: needsSelectedRemux,
    describeMediaFailure: describeMediaFailure,
    newMirror: newMirror,
    mirrorOnMpvPause: mirrorOnMpvPause,
    mirrorOnMpvSeek: mirrorOnMpvSeek,
    mirrorOnTvState: mirrorOnTvState,
    DRIFT_TOLERANCE: DRIFT_TOLERANCE,
    ECHO_WINDOW_MS: ECHO_WINDOW_MS,
    SEEK_HOLD_MS: SEEK_HOLD_MS,
    CLOCK_SEEK_LIMIT: CLOCK_SEEK_LIMIT,
  };
}

// ---- IINA runtime ----

if (typeof iina !== "undefined") {
  var core = iina.core, mpv = iina.mpv, menu = iina.menu, sidebar = iina.sidebar,
      utils = iina.utils, file = iina.file, console = iina.console, event = iina.event,
      preferences = iina.preferences, overlay = iina.overlay || null;

  var state = { phase: "idle", url: null, pct: 0, msg: null };
  var stdoutRest = "";

  // Bumped every time a new cast starts or the cast is stopped. Async
  // callbacks (bin-dir resolution, helper stdout, exec .then/.catch) capture
  // the generation they belong to and no-op if it no longer matches — this
  // stops a superseded cast's stale callbacks from clobbering the state of
  // whatever cast (or idle state) came after it.
  var castGen = 0;

  // Active muted mirror (spec 2026-08-30), or null when not casting. Event
  // callbacks may ONLY swap this object via the pure mirrorOn* functions;
  // everything that touches mpv runs in onMessage/menu context.
  var mirror = null;
  // Set while a track change or a backwards seek is moving mpv itself, so
  // the pause and seek those sets emit are not sent back to the TV.
  var suppressSync = false;
  // The file was playing when the user sent it. It stays paused through the
  // network search; startCast turns this into state.resumeTogether for DLNA
  // and lets an AirPlay cast continue.
  var holdPlayback = false;
  // time-pos captured before the pause. Reading it afterwards often returns
  // 0, and the TV then opens the movie at the beginning.
  var haveHeldPos = false;
  var heldPos = 0;
  // DLNA resume alignment. The first time the TV reports that it is playing
  // belongs to the opening join. A later pause-to-play is the user: the
  // player is put back on the TV's clock then, including after a stall.
  var tvSeenPlaying = false;
  var tvWasPaused = false;
  var localAlignUntil = 0;

  // Every post to the page goes through this so the page always sees the live
  // sync block alongside the pipeline state.
  function stateForPage() {
    // duration and subs are derived here, not stored on `state`: selectTracks
    // otherwise runs only at cast start, so a stored label would be empty
    // before the first cast — exactly when the user is deciding whether to
    // cast — and would go stale if the subtitle track changed mid-session.
    // Safe to read mpv here: every caller is a sidebar.onMessage handler,
    // which IINA runs on the main thread.
    return {
      phase: state.phase, url: state.url, pct: state.pct, msg: state.msg,
      diag: state.diag || null,
      mode: state.mode || "airplay",
      devices: state.devices || [],
      dlnaName: state.dlnaName || "",
      tv: state.tv || null,
      duration: mpv.getNumber("duration") || 0,
      subs: subtitleLabel(playerTracks()),
      sync: mirror === null ? null : {
        seq: mirror.seq, paused: mirror.paused,
        seekTo: mirror.seekTo, startPos: mirror.startPos,
      },
    };
  }

  function endMirror() {
    if (mirror === null) return;
    mpv.set("mute", mirror.savedMute);
    mirror = null;
  }

  // Helper-initiated teardowns (stopped/error events, exec settling) happen in
  // exec callbacks, which must not touch mpv from off-main. They only mutate
  // `state`; the page's next poll lands here (onMessage: safe) and sweeps up.
  function reapMirror() {
    if (mirror !== null && (state.phase === "idle" || state.phase === "error")) endMirror();
  }

  event.on("mpv.pause.changed", function () {
    if (suppressSync || mirror === null) return;
    mirror = mirrorOnMpvPause(mirror, !!mpv.getFlag("pause"));
  });
  // The seek event often fires before time-pos has moved, especially for the
  // arrow keys. Remember that a seek happened; the next poll reads the
  // position once it has settled and sends that to the TV.
  event.on("mpv.seek", function () {
    if (suppressSync || mirror === null) return;
    state.seekPending = true;
  });
  // aid.changed only records the request. The next getState poll, which is
  // on the main thread, pauses both sides and rebuilds the stream.
  event.on("mpv.aid.changed", function () {
    if (suppressSync || mirror === null) return;
    if (state.mode !== "dlna" && state.mode !== "airplay" && state.mode !== "cast") return;
    if (state.phase !== "starting" && state.phase !== "ready" && state.phase !== "packaged") return;
    var tracks = playerTracks();
    state.switchAudio = !!(tracks && tracks.amap !== state.amap);
  });
  // A new file invalidates the whole cast: the stream on the TV is the old
  // file. file-loaded is an app event (main thread), so stopCast is safe here.
  event.on("iina.file-loaded", function () {
    if (mirror === null) return;
    stopCast();
  });
  // Closing the window does not always quit the process, so the helper's
  // parent watch never fires and the TV keeps playing. Stop while the
  // process can still run the helper. end-file covers Stop and a closed file
  // when the window itself stays open.
  function playerWentAway() {
    if (mirror === null && state.phase !== "starting" && state.phase !== "ready" &&
        state.phase !== "packaged" && state.phase !== "browsing") return;
    stopCast();
  }
  event.on("iina.window-will-close", playerWentAway);
  event.on("iina.window-did-close", playerWentAway);
  event.on("mpv.end-file", playerWentAway);

  // Resolve our own install dir: @data is <plugins>/.data/<identifier>; the
  // plugin itself lives directly under that <.../plugins> directory. Locate
  // bin/ relative to the data dir's parent plugins folder at runtime via a
  // glob through /bin/sh once, then cache it in @data so later launches skip
  // the lookup.
  // utils.exec wipes the environment (only LC_ALL survives — no HOME), so the
  // plugins dir is derived in JS via utils.resolvePath and passed to the
  // shell as an argument rather than relied on via $HOME.
  function resolveBinDir(cb) {
    var cached = null;
    try { cached = file.read("@data/bindir.txt"); } catch (e) {}
    if (cached && cached.trim()) { cb(cached.trim(), null); return; }
    var pluginsDir = pluginsDirFromDataDir(utils.resolvePath("@data/"));
    if (!pluginsDir) { cb(null, "cannot resolve plugins directory"); return; }
    // com.apple.quarantine does NOT clear the executable bit (verified: a
    // quarantined file still passes `[ -x ]`), so the `-x` test alone cannot
    // catch the browser-download case docs/distribution.md and README.md
    // promise is detected. Worse, exec'ing a quarantined binary blocks on a
    // Gatekeeper dialog instead of failing fast, so check for the xattr with
    // `/usr/bin/xattr -p` (absolute path — /bin/sh via utils.exec runs with
    // an empty PATH) BEFORE anything ever execs the binary. `xattr -p`
    // succeeding (exit 0) means the attribute is present, i.e. quarantined.
    var script =
      'for d in "$1"/*/; do' +
      '  if /usr/bin/grep -qs \'"identifier": *"dev.redshadow.iina-cast"\' "$d/Info.json"; then' +
      '    h="$d"bin/airplay-helper; f="$d"bin/ffmpeg;' +
      '    if [ -x "$h" ] && [ -x "$f" ] ' +
      '        && ! /usr/bin/xattr -p com.apple.quarantine "$h" >/dev/null 2>&1 ' +
      '        && ! /usr/bin/xattr -p com.apple.quarantine "$f" >/dev/null 2>&1; then' +
      '      printf %s "$d"bin; exit 0;' +
      '    fi;' +
      '    exit 3;' +           // found the plugin, but its binaries won't run
      '  fi;' +
      'done; exit 1';
    utils.exec("/bin/sh", ["-c", script, "sh", pluginsDir]).then(function (r) {
      if (r.status === 0 && r.stdout) {
        file.write("@data/bindir.txt", r.stdout);
        cb(r.stdout, null);
      } else if (r.status === 3) {
        // Never offer to download anything — docs/distribution.md non-goals.
        cb(null, "the bundled helper or ffmpeg is missing, not executable, or quarantined; " +
                 "reinstall the plugin through IINA (Settings → Plugins → Install)");
      } else {
        cb(null, "cannot locate plugin bin directory");
      }
    }).catch(function (e) {
      cb(null, "cannot locate plugin bin directory: " + String(e));
    });
  }

  // Drop the cached bin dir so the next resolveBinDir call re-derives it
  // instead of reusing a directory that just failed to produce a runnable
  // helper (e.g. the plugin was reinstalled to a new path).
  function invalidateBinDirCache() {
    try { file.write("@data/bindir.txt", ""); } catch (e) {}
  }

  function refuse(msg) {
    core.osd("iina cast: " + msg);
    state = { phase: "error", url: null, pct: 0, msg: msg };
    return null;
  }

  // The file IINA is playing, if it is a local file with a video and an
  // audio track. A refusal is already on state and on the OSD.
  function localCastSource() {
    var src = normalizeSource(mpv.getString("path"));
    if (!src) return refuse("nothing is playing");
    if (hasURLScheme(src)) {
      // The bundled ffmpeg is built --disable-network and has no protocol
      // handler for these at all, so say so plainly rather than letting
      // ffmpeg fail obscurely.
      return refuse("can only cast local files, not network streams");
    }
    if (src.charAt(0) !== "/") {
      // A relative path: the pipeline hands src straight to ffmpeg as -i,
      // which wants a plain absolute path. (file:// URIs no longer reach
      // this branch — normalizeSource above turns them into absolute paths
      // before either guard runs.)
      return refuse("needs a local file path");
    }
    var tracks = playerTracks();
    if (!tracks) return refuse("no castable video/audio tracks");
    return { src: src, tracks: tracks };
  }

  function startCast(opts) {
    opts = opts || {};
    var dlna = opts.mode === "dlna";
    var casting = opts.mode === "cast";
    var direct = dlna || casting;
    reapMirror(); // a helper-initiated teardown may have left a stale mirror
                  // un-reaped (only the getState poll normally does that); a
                  // restart must not let newMirror() capture that stale
                  // savedMute instead of the user's real pre-cast value.
    if (state.phase === "starting" || state.phase === "ready" || state.phase === "packaged") return;
    if (dlna && !opts.control) {
      refuse("no DLNA device selected");
      return;
    }
    if (casting && !opts.host) {
      refuse("no Chromecast selected");
      return;
    }
    var ready = localCastSource();
    if (!ready) return;
    var src = ready.src;
    var tracks = ready.tracks;
    if (tracks.subDropped) {
      core.osd("iina cast: selected subtitles can't be cast (image-based, unsupported, or file not found); casting without them");
    }
    var pid = getIINAPid();
    if (!isValidPid(pid)) {
      core.osd("iina cast: cannot determine IINA process id");
      state = { phase: "error", url: null, pct: 0, msg: "cannot determine IINA process id" };
      return;
    }
    // Read here, in the menu/onMessage (main-thread) context, and capture it:
    // the serve args are built inside resolveBinDir's callback, which runs
    // from a utils.exec promise, and nothing IINA-facing is called from there.
    var lanIP = lanIPOverride(preferences.get("lanIP"));
    var vwidth = mpv.getNumber("width") || 0;
    var vheight = mpv.getNumber("height") || 0;
    tvSeenPlaying = false;
    tvWasPaused = false;
    localAlignUntil = 0;
    var duration = mpv.getNumber("duration") || 0;
    var livePos = mpv.getNumber("time-pos") || 0;
    var startAt = livePos;
    if (typeof opts.startAt === "number" && opts.startAt >= 0) startAt = opts.startAt;
    else if (haveHeldPos) startAt = heldPos;
    haveHeldPos = false;
    // A file that is already playing pauses here, at the frame the TV will
    // be given. The player stays paused until the TV is actually playing
    // that frame. A file the user had paused stays paused. AirPlay keeps
    // the muted mirror, so a pause taken during the search is released.
    var resumeTogether = false;
    if (!direct) {
      if (holdPlayback) {
        holdPlayback = false;
        mpv.set("pause", false);
      }
    } else if (!opts.forcePlay && (holdPlayback || !mpv.getFlag("pause"))) {
      if (!mpv.getFlag("pause")) {
        mpv.set("pause", true);
        if (!opts.quiet) core.osd("Пауза: отправка на телевизор");
      }
      resumeTogether = true;
      holdPlayback = false;
    } else {
      holdPlayback = false;
    }
    var startPaused = opts.forcePlay ? false : !!mpv.getFlag("pause");
    var dlnaControl = dlna ? String(opts.control) : "";
    var dlnaName = direct ? String(opts.name || "") : "";
    var castHost = casting ? String(opts.host || "") : "";
    var castPort = casting ? (Number(opts.port) || 8009) : 0;
    var castModel = casting ? String(opts.model || "") : "";
    var outDir = utils.resolvePath("@tmp/hls");
    var gen = ++castGen;
    stdoutRest = "";
    state = {
      phase: "starting", url: null, pct: 0, msg: null,
      mode: casting ? "cast" : (dlna ? "dlna" : "airplay"),
      dlnaName: dlnaName, dlnaControl: dlnaControl,
      castHost: castHost, castPort: castPort, castModel: castModel,
      devices: [], tv: null,
      base: 0, amap: tracks.amap, forcePlay: !!opts.forcePlay,
      resumeTogether: resumeTogether, landed: false, joinUntil: 0,
      seenPos: startAt, seekPending: false, seekArmed: false, dlnaApplied: 0,
    };
    // Muted on the Mac. DLNA waits, paused, until the TV reports it has
    // seeked to startAt. AirPlay keeps playing into the mirror.
    mirror = newMirror(startAt, !!mpv.getFlag("mute"), startPaused);
    mpv.set("mute", true);
    if (direct && dlnaName && !opts.quiet) core.osd("На телевизор: " + dlnaName);

    resolveBinDir(function (binDir, resolveErr) {
      if (gen !== castGen) return; // superseded by a newer cast or a stop
      if (resolveErr) {
        state = { phase: "error", url: null, pct: 0, msg: resolveErr };
        return;
      }
      var helper = binDir + "/airplay-helper";
      var ffmpeg = binDir + "/ffmpeg";
      var serveArgs = [
        "serve",
        "-source", src, "-out", outDir,
        "-ffmpeg", ffmpeg,
        "-parent", String(pid),
        "-duration", String(duration),
        "-vcodec", tracks.vcodec, "-acodec", tracks.acodec,
        "-achannels", String(tracks.achannels),
        "-vmap", String(tracks.vmap), "-amap", String(tracks.amap),
      ];
      if (tracks.sub) {
        if (tracks.sub.path) serveArgs.push("-subpath", tracks.sub.path);
        else serveArgs.push("-smap", String(tracks.sub.smap));
        serveArgs.push("-sublang", tracks.sub.lang, "-subname", tracks.sub.title);
      }
      if (lanIP) serveArgs.push("-ip", lanIP);
      if (dlna) {
        serveArgs.push("-mode", "dlna", "-dlna", dlnaControl, "-title", fileTitle(src), "-start", String(startAt));
        if (startPaused) serveArgs.push("-paused");
        // The helper already remuxes MKV and other containers. This flag is
        // the case it cannot see: an MP4 with several audio tracks, or a
        // subtitle, where serving the original file would ignore IINA.
        if (needsSelectedRemux(tracks)) serveArgs.push("-remux");
      } else if (casting) {
        serveArgs.push("-mode", "cast", "-cast-host", castHost, "-cast-port", String(castPort),
          "-cast-model", castModel, "-title", fileTitle(src), "-start", String(startAt),
          "-vwidth", String(vwidth), "-vheight", String(vheight));
        if (startPaused) serveArgs.push("-paused");
      }
      utils.exec(helper, serveArgs, undefined, function (chunk) {
        if (gen !== castGen) return;
        var parsed = parseHelperEvents(stdoutRest, chunk);
        stdoutRest = parsed.rest;
        for (var i = 0; i < parsed.events.length; i++) {
          var ev = parsed.events[i];
          if (ev.event === "ready") {
            state.phase = "ready"; state.url = ev.url;
            state.base = typeof ev.base === "number" ? ev.base : 0;
            console.log("[cast] stream ready at " + ev.url);
          }
          else if (ev.event === "landed") {
            state.landed = true;
            state.landedOK = ev.ok !== false;
          }
          else if (ev.event === "progress") { state.pct = ev.pct; }
          else if (ev.event === "packaged") { state.phase = state.phase === "ready" ? "packaged" : state.phase; state.pct = 100; }
          else if (ev.event === "tv") {
            state.tv = {
              pos: typeof ev.pos === "number" ? ev.pos : 0,
              paused: !!ev.paused, ended: !!ev.ended,
            };
            state.tvAt = Date.now();
          }
          else if (ev.event === "error") {
            state = { phase: "error", url: null, pct: 0, msg: ev.msg, resumeTogether: !!state.resumeTogether };
          }
          else if (ev.event === "stopped") {
            state = { phase: "idle", url: null, pct: 0, msg: null, resumeTogether: !!state.resumeTogether };
          }
        }
      }, function (errChunk) {
        // IINA's stderr handler keeps firing with "" after EOF until the
        // process exits (issue #38); don't log those.
        var line = errChunk.trim();
        if (line) console.log("[helper:stderr] " + line);
      }).then(function () {
        if (gen !== castGen) return;
        if (state.phase !== "error") {
          state = { phase: "idle", url: null, pct: 0, msg: null, resumeTogether: !!state.resumeTogether };
        }
      }).catch(function (e) {
        invalidateBinDirCache(); // helper failed to start; don't trust the cached dir next time
        if (gen !== castGen) return;
        state = { phase: "error", url: null, pct: 0, msg: String(e), resumeTogether: !!state.resumeTogether };
      });
    });
  }

  function getIINAPid() {
    // mpv exposes the process pid of IINA's mpv core, which lives inside IINA
    // itself (libmpv), so it IS IINA's pid.
    return mpv.getNumber("pid");
  }

  // The discovery callback runs off the main thread, so it only records the
  // choice. The next getState poll (main thread) is what actually starts
  // the cast or opens the sidebar.
  function applyTargets(ev) {
    var choice = chooseCast(ev.devices || [], ev.airplay || [], ev.cast || []);
    if (choice.mode === "dlna") {
      state.phase = "browsing";
      state.mode = "dlna";
      state.devices = [];
      state.msg = null;
      state.pending = { mode: "dlna", control: choice.device.control, name: choice.device.name || "" };
    } else if (choice.mode === "airplay") {
      state.phase = "browsing";
      state.mode = "airplay";
      state.devices = [];
      state.msg = null;
      state.pending = { mode: "airplay" };
      state.needPanel = true;
    } else if (choice.mode === "cast") {
      state.phase = "browsing";
      state.mode = "cast";
      state.devices = [];
      state.msg = null;
      state.pending = {
        mode: "cast",
        host: choice.device.host,
        port: choice.device.port,
        name: choice.device.name || "",
        model: choice.device.model || "",
      };
    } else if (choice.mode === "choose") {
      state.phase = "browsing";
      state.mode = "dlna";
      state.devices = choice.devices;
      state.pending = null;
      state.msg = null;
      state.needPanel = true;
    } else {
      state = { phase: "error", url: null, pct: 0, msg: "No TV found on the network" };
    }
  }

  function discoverTargets() {
    if (state.phase === "starting" || state.phase === "ready" || state.phase === "packaged") return;
    var gen = ++castGen;
    state = { phase: "browsing", url: null, pct: 0, msg: null, mode: "", devices: [], dlnaName: "", dlnaControl: "", tv: null };
    var lanIP = lanIPOverride(preferences.get("lanIP"));
    resolveBinDir(function (binDir, resolveErr) {
      if (gen !== castGen) return;
      if (resolveErr) {
        state = { phase: "error", url: null, pct: 0, msg: resolveErr };
        return;
      }
      var args = ["dlna-discover"];
      if (lanIP) args.push("-ip", lanIP);
      var rest = "";
      utils.exec(binDir + "/airplay-helper", args, undefined, function (chunk) {
        if (gen !== castGen) return;
        var parsed = parseHelperEvents(rest, chunk);
        rest = parsed.rest;
        for (var i = 0; i < parsed.events.length; i++) {
          var ev = parsed.events[i];
          if (ev.event === "devices") applyTargets(ev);
          else if (ev.event === "error") {
            state = { phase: "error", url: null, pct: 0, msg: ev.msg || "DLNA discovery failed" };
          }
        }
      }).then(function () {
        if (gen !== castGen || state.phase !== "browsing") return;
        if (state.pending || (state.devices && state.devices.length)) return;
        state = { phase: "error", url: null, pct: 0, msg: "No TV found on the network" };
      }).catch(function (e) {
        if (gen !== castGen) return;
        state = { phase: "error", url: null, pct: 0, msg: String(e) };
      });
    });
  }

  function sendToTV() {
    if (state.phase === "starting" || state.phase === "ready" || state.phase === "packaged" || state.phase === "browsing") return;
    if (!localCastSource()) return;
    // Freeze the frame for the whole search. A DLNA cast keeps this pause
    // until the TV is on the same frame; an AirPlay cast lets it go.
    heldPos = mpv.getNumber("time-pos") || 0;
    haveHeldPos = true;
    if (!mpv.getFlag("pause")) {
      mpv.set("pause", true);
      holdPlayback = true;
      core.osd("Пауза: отправка на телевизор");
    }
    discoverTargets();
  }

  // The TV is paused at once, and the outgoing helper is told (via the hold
  // file) not to Stop it, so the last frame stays up while the new copy runs.
  function pauseRendererNow(control) {
    resolveBinDir(function (binDir, resolveErr) {
      if (resolveErr) return;
      if (state.mode === "cast") {
        utils.exec(binDir + "/airplay-helper", ["cast-ctl", "-out", utils.resolvePath("@tmp/hls"), "-action", "pause"]);
        return;
      }
      utils.exec(binDir + "/airplay-helper", ["dlna-ctl", "-control", control, "-action", "pause"]);
    });
  }

  // Replace the cast already on the TV. stayPaused keeps both sides paused
  // (an audio-track change). Otherwise mpv stays paused only until the TV
  // reports that it is playing the new file.
  function restartAt(pos, stayPaused) {
    var mode = state.mode;
    var control = state.dlnaControl || "";
    var name = state.dlnaName || "";
    var castHost = state.castHost || "";
    var castPort = state.castPort || 8009;
    var castModel = state.castModel || "";
    if (mode !== "dlna" && mode !== "airplay" && mode !== "cast") return;
    if (state.phase !== "starting" && state.phase !== "ready" && state.phase !== "packaged") return;
    if (mode === "dlna" && !control) return;
    if (mode === "cast" && !castHost) return;
    suppressSync = true;
    mpv.set("pause", true);
    mpv.set("time-pos", pos);
    if (mode === "dlna" || mode === "cast") {
      try { file.write("@tmp/hls/hold", "1"); } catch (e) {}
      // Only the "stay paused" path sends Pause itself. A seek that should
      // keep playing must not have a late Pause land on the new file.
      if (stayPaused) pauseRendererNow(control);
    }
    endMirror();
    castGen++;
    stdoutRest = "";
    state = { phase: "idle", url: null, pct: 0, msg: null };
    var next = { forcePlay: !stayPaused, quiet: true };
    if (mode === "dlna") startCast({ mode: "dlna", control: control, name: name, forcePlay: next.forcePlay, quiet: true, startAt: pos });
    else if (mode === "cast") startCast({ mode: "cast", host: castHost, port: castPort, model: castModel, name: name, forcePlay: next.forcePlay, quiet: true, startAt: pos });
    else startCast(next);
    if (mirror) mirror.lastSetPos = { pos: pos, at: Date.now() };
    suppressSync = false;
  }

  // A backwards seek recopy leaves mpv paused. Unpause it on the main
  // thread once the TV is playing, so the clocks start at the same frame.
  function resumeIfReady() {
    if (!state.forcePlay || (state.mode !== "dlna" && state.mode !== "cast") || !state.tv || state.tv.paused) return;
    if (state.phase !== "ready" && state.phase !== "packaged") return;
    state.forcePlay = false;
    mpv.set("pause", false);
  }

  // The search failed or the cast died before the TV was ready. A file we
  // paused for the handoff should play again; one the user had paused stays.
  function releaseHeldPlayback() {
    var held = holdPlayback || !!state.resumeTogether;
    if (!held) return;
    var ph = state.phase;
    if (ph === "starting" || ph === "ready" || ph === "packaged" || ph === "browsing") return;
    holdPlayback = false;
    haveHeldPos = false;
    state.resumeTogether = false;
    mpv.set("pause", false);
  }

  // The TV is paused on the same frame. Tell it to play, and leave the
  // player paused until the TV reports that it is actually playing. Starting
  // the player in the same turn as the SOAP call is why it ran for a second
  // before the television did.
  function resumeTogetherIfLanded() {
    if (!state.resumeTogether || !state.landed || mirror === null || (state.mode !== "dlna" && state.mode !== "cast")) return;
    if (state.phase !== "ready" && state.phase !== "packaged") return;
    if (state.landedOK === false) {
      state.resumeTogether = false;
      core.osd("Телевизор не перешёл на текущее место фильма");
      return;
    }
    if (!state.playSent) {
      state.playSent = true;
      dispatchDlna("play");
      return;
    }
    if (!state.tv || state.tv.paused || state.tv.ended) return;
    var target = mirror.startPos || 0;
    var tvPos = state.tv.pos || 0;
    if (target >= 1 && Math.abs(tvPos - target) > 4) return;
    var age = state.tvAt ? (Date.now() - state.tvAt) / 1000 : 0;
    if (age < 0 || age > 1.2) age = 0;
    var joinAt = tvPos + age;
    state.resumeTogether = false;
    var here = mpv.getNumber("time-pos") || 0;
    mirror = Object.assign({}, mirror, { paused: false, expectMpvPause: false });
    if (Math.abs(joinAt - here) > 0.3 && Math.abs(joinAt - here) < 3) {
      suppressSync = true;
      mpv.set("time-pos", joinAt);
      mirror.lastSetPos = { pos: joinAt, at: Date.now() };
      state.seenPos = joinAt;
      suppressSync = false;
    }
    state.joinUntil = Date.now() + 1500;
    mpv.set("pause", false);
  }

  // Arrow keys and the timeline both move time-pos, and both raise a seek
  // event. A jump of a few seconds with no seek event is the cache
  // catching up after a stall: sending that to the TV is the reload and
  // the download-speed meter. A seek event whose position has not landed
  // yet waits one poll, so we never send the position from before the key.
  function observePlayback() {
    if (suppressSync || mirror === null) return;
    var pos = mpv.getNumber("time-pos") || 0;
    var pending = !!state.seekPending;
    var seen = state.seenPos;
    if (typeof seen !== "number") {
      state.seenPos = pos;
      state.seekPending = false;
      state.seekArmed = false;
      return;
    }
    var echo = mirror.lastSetPos &&
      (Date.now() - mirror.lastSetPos.at) < ECHO_WINDOW_MS &&
      Math.abs(pos - mirror.lastSetPos.pos) <= DRIFT_TOLERANCE;
    if (echo) {
      state.seekPending = false;
      state.seekArmed = false;
      state.seenPos = pos;
      return;
    }
    var jumped = Math.abs(pos - seen) > 8;
    if (localAlignUntil && Date.now() < localAlignUntil) {
      state.seekPending = false;
      state.seekArmed = false;
      state.seenPos = pos;
      return;
    }
    if (pending && !jumped && !state.seekArmed) {
      state.seekArmed = true;
      return;
    }
    if (!(pending || jumped)) {
      state.seekArmed = false;
      state.seenPos = pos;
      return;
    }
    state.seekPending = false;
    state.seekArmed = false;
    state.seenPos = pos;
    mirror = mirrorOnMpvSeek(mirror, pos, Date.now());
  }

  // Returns true when the cast was torn down (the TV finished).
  function syncClock() {
    if (mirror === null || (state.mode !== "dlna" && state.mode !== "cast") || !state.tv) return false;
    if (state.tv.ended) {
      // Fall through: ending the movie still tears the cast down.
    } else if (state.joinUntil && Date.now() < state.joinUntil && state.tv.paused && !mirror.paused) {
      // Play was just sent. The next position report can still say paused
      // and would otherwise put the player back on pause.
      return false;
    }
    if (!state.tv.paused) state.joinUntil = 0;
    var r = mirrorOnTvState(mirror, {
      appliedSeq: state.dlnaApplied || 0,
      pos: state.tv.pos || 0,
      paused: !!state.tv.paused,
      wireless: true,
      ended: !!state.tv.ended,
    }, mpv.getNumber("time-pos") || 0, !!mpv.getFlag("pause"), Date.now());
    mirror = r.m;
    if (r.teardown) { stopCast(); return true; }
    if (r.setMpvPaused !== null) {
      suppressSync = true;
      mpv.set("pause", r.setMpvPaused);
      suppressSync = false;
    }
    // Steady playback is left a second or two apart: seeking the local file
    // to erase that is the rewind and the download meter. A pause that the
    // user then releases is different — the stall has already happened, and
    // the two clocks are put back together without asking the TV to reopen.
    if (!state.tv.paused) {
      if (tvSeenPlaying && tvWasPaused) {
        if (alignMpvToTV()) tvWasPaused = false;
      } else {
        tvWasPaused = false;
      }
      tvSeenPlaying = true;
    } else {
      tvWasPaused = true;
    }
    return false;
  }

  function alignMpvToTV() {
    if (mirror === null || (state.mode !== "dlna" && state.mode !== "cast") || !state.tv || state.tv.ended) return true;
    if (state.tv.paused) return false;
    if (mirror.seekTo) return true;
    if (mirror.holdUntil && Date.now() < mirror.holdUntil) return true;
    // Play was issued on this poll but not recorded yet. Try again next time,
    // still remembering that this resume has not been aligned.
    if ((state.dlnaApplied || 0) < mirror.seq) return false;
    if (mpv.getFlag("pause")) return false;
    var age = state.tvAt ? (Date.now() - state.tvAt) / 1000 : 0;
    if (age < 0 || age > 1.2) age = 0;
    var there = (state.tv.pos || 0) + age;
    var here = mpv.getNumber("time-pos") || 0;
    var gap = Math.abs(there - here);
    if (gap <= 0.6 || gap > 180) return true;
    suppressSync = true;
    mpv.set("time-pos", there);
    mirror.lastSetPos = { pos: there, at: Date.now() };
    state.seenPos = there;
    localAlignUntil = Date.now() + 2000;
    suppressSync = false;
    return true;
  }

  // One SOAP for the latest seek or pause. Runs from the poll, so it still
  // happens when the sidebar page is not the thing driving the TV.
  function pushPlayback() {
    if (mirror === null || (state.mode !== "dlna" && state.mode !== "cast")) return;
    if (state.mode === "dlna" && !state.dlnaControl) return;
    var applied = state.dlnaApplied || 0;
    if (mirror.seq <= applied) return;
    if (mirror.seekTo && mirror.seekTo.seq > applied) {
      if (dispatchDlna("seek", mirror.seekTo.pos)) return;
    }
    // Before the first position report the helper has the TV paused, which
    // is the state Play has to leave.
    var tvPaused = state.tv ? !!state.tv.paused : true;
    if (!!mirror.paused !== tvPaused) dispatchDlna(mirror.paused ? "pause" : "play");
    state.dlnaApplied = mirror.seq;
  }

  function onMainPoll() {
    reapMirror();
    releaseHeldPlayback();
    observePlayback();
    resumeTogetherIfLanded();
    if (syncClock()) return;
    pushPlayback();
    consumePending();
  }

  // Pending work that a callback was not allowed to do itself.
  function consumePending() {
    if (state.switchAudio) {
      state.switchAudio = false;
      var tracksNow = playerTracks();
      if (tracksNow && tracksNow.amap !== state.amap &&
          (state.phase === "starting" || state.phase === "ready" || state.phase === "packaged")) {
        var pos = (state.tv && typeof state.tv.pos === "number") ? state.tv.pos : (mpv.getNumber("time-pos") || 0);
        core.osd("Пауза: смена аудиодорожки");
        restartAt(pos, true);
        syncPlayerButton();
        return;
      }
    }
    resumeIfReady();
    if (state.needPanel) {
      state.needPanel = false;
      openSidebar();
    }
    syncPlayerButton();
    var job = state.pending;
    if (!job) return;
    state.pending = null;
    if (job.mode === "dlna") startCast({ mode: "dlna", control: job.control, name: job.name || "" });
    else if (job.mode === "cast") startCast({ mode: "cast", host: job.host, port: job.port, name: job.name || "", model: job.model || "" });
    else startCast();
  }

  // sub-visibility is the subtitle button. The track stays selected while hidden.
  function playerTracks() {
    return selectTracks(mpv.getNative("track-list") || [], !!mpv.getFlag("sub-visibility"));
  }

  function stopCast() {
    var control = (state.mode === "dlna" && state.dlnaControl) ? state.dlnaControl : "";
    var castOn = state.mode === "cast";
    var resumeLocal = holdPlayback || !!state.resumeTogether;
    holdPlayback = false;
    haveHeldPos = false;
    tvSeenPlaying = false;
    tvWasPaused = false;
    localAlignUntil = 0;
    endMirror();
    castGen++; // invalidate any in-flight cast's callbacks
    resolveBinDir(function (binDir, resolveErr) {
      if (resolveErr) { console.log("[stopCast] " + resolveErr); return; }
      // Stop the renderer first, then the helper. Closing the window used
      // to leave the TV playing because nothing sent Stop while IINA's
      // process was still alive.
      if (control) {
        utils.exec(binDir + "/airplay-helper", ["dlna-ctl", "-control", control, "-action", "stop"]);
      }
      if (castOn) {
        utils.exec(binDir + "/airplay-helper", ["cast-ctl", "-out", utils.resolvePath("@tmp/hls"), "-action", "stop"]);
      }
      utils.exec(binDir + "/airplay-helper", ["stop", "-out", utils.resolvePath("@tmp/hls")]);
    });
    state = { phase: "idle", url: null, pct: 0, msg: null };
    if (resumeLocal) mpv.set("pause", false);
  }

  // Seek and pause are one SOAP call each. Rebuilding the file here made a
  // large movie copy itself again, which is the cache-speed meter and the
  // stall on both screens. The time sent is IINA's time, not a position
  // inside a clipped copy.
  function dispatchDlna(action, pos) {
    if (action !== "play" && action !== "pause" && action !== "seek" && action !== "stop") return false;
    if (state.mode === "cast") {
      if (action === "seek") pos = Math.max(0, typeof pos === "number" ? pos : 0);
      resolveBinDir(function (binDir, resolveErr) {
        if (resolveErr) { console.log("[cast] " + resolveErr); return; }
        var args = ["cast-ctl", "-out", utils.resolvePath("@tmp/hls"), "-action", action];
        if (action === "seek") args.push("-pos", String(pos || 0));
        utils.exec(binDir + "/airplay-helper", args);
      });
      return false;
    }
    if (state.mode !== "dlna" || !state.dlnaControl) return false;
    var control = state.dlnaControl;
    if (action === "seek") pos = Math.max(0, typeof pos === "number" ? pos : 0);
    resolveBinDir(function (binDir, resolveErr) {
      if (resolveErr) { console.log("[dlna] " + resolveErr); return; }
      var args = ["dlna-ctl", "-control", control, "-action", action];
      if (action === "seek") args.push("-pos", String(pos || 0));
      utils.exec(binDir + "/airplay-helper", args);
    });
    return false;
  }

  // Info.json's sidebarTab makes IINA show an "AirPlay" tab in the sidebar's
  // Plugins section, which the user can open without ever touching the
  // Plugins menu. That tab is an empty WKWebView until the plugin calls
  // sidebar.loadFile (IINA never loads anything into it itself), so the page
  // must be loaded as soon as the window exists — not only from the menu
  // item, or the tab is blank and there is no Start casting button (#20).
  // loadFile throws before iina.window-loaded; a plugin loaded at launch runs
  // before that, one enabled from Settings runs after, so both paths are
  // covered: try now, and again when the window reports in.
  var sidebarLoaded = false;
  function loadSidebar(force) {
    if (sidebarLoaded && !force) return;
    try {
      sidebar.loadFile("sidebar.html"); // clears message listeners — register after
    } catch (e) {
      return; // no window yet; iina.window-loaded will retry
    }
    sidebar.onMessage("getState", function () {
      onMainPoll(); // discovery, seeks and the TV clock; main thread
      sidebar.postMessage("state", stateForPage());
    });
    // The page owns starting too, not just stopping: stopCast() tears the whole
    // pipeline down, so without this the sidebar would be a dead end and the
    // only way back to a cast would be re-running the menu item.
    sidebar.onMessage("start", function (d) {
      startCast(d || {});
      sidebar.postMessage("state", stateForPage());
    });
    sidebar.onMessage("cast", function () {
      sendToTV();
      sidebar.postMessage("state", stateForPage());
    });
    sidebar.onMessage("discover", function () {
      sendToTV();
      sidebar.postMessage("state", stateForPage());
    });
    // Play, pause and seek on a DLNA TV are SOAP calls, not the <video>
    // element the AirPlay path drives. The page posts one command per
    // change of the mirror sequence.
    sidebar.onMessage("dlnaCtl", function (d) {
      if (!d) return;
      dispatchDlna(d.action, d.pos);
    });
    sidebar.onMessage("stop", function () {
      stopCast();
      sidebar.postMessage("state", stateForPage());
    });
    // The page's <video> failed to load the stream twice (issue #42). Probe
    // the URL and leave the result on state.diag for the page's next poll.
    // Only the current stream counts: a late report about a stream that has
    // since been replaced or stopped would describe the wrong thing.
    sidebar.onMessage("mediaError", function (d) {
      if (!d || !state.url || d.url !== state.url) return;
      var gen = castGen, url = state.url;
      console.log("[cast] the page couldn't load " + url + " (media error " + (d.code == null ? "?" : d.code) + "); probing it");
      state.diag = null;
      function report(probe) {
        if (gen !== castGen || state.url !== url) return;
        var msg = describeMediaFailure(url, d.code, probe);
        state.diag = { url: url, msg: msg };
        console.log("[cast] " + msg);
      }
      // Absolute path: utils.exec runs with an empty PATH.
      utils.exec("/usr/bin/curl", ["-sS", "-o", "/dev/null", "-w", "%{http_code}",
                                   "--max-time", "5", url])
        .then(report, function () { report(null); });
    });
    sidebar.onMessage("tvState", function (tv) {
      reapMirror(); // a helper-initiated teardown may have left a stale mirror
                    // un-reaped; don't mpv.set against a dead stream.
      if (mirror === null || !tv) return;
      if (state.joinUntil && Date.now() < state.joinUntil && tv.paused && !mirror.paused) return;
      var r = mirrorOnTvState(mirror, tv, mpv.getNumber("time-pos") || 0,
                              !!mpv.getFlag("pause"), Date.now());
      mirror = r.m;
      if (r.teardown) { stopCast(); return; }
      if (r.setMpvPaused !== null) mpv.set("pause", r.setMpvPaused);
      // DLNA drift is not pulled into the local file from this poll. Doing
      // so reopens a large movie and the TV shows the download meter. The
      // resume path in syncClock is the one that puts the clocks back together.
      if (r.setMpvPos !== null && state.mode !== "dlna" && state.mode !== "cast") mpv.set("time-pos", r.setMpvPos);
    });
    sidebarLoaded = true;
  }
  function openSidebar() {
    loadSidebar(false); // already loaded in the normal case; don't reload a
                        // live page, that would tear down its <video> mid-cast
    sidebar.show();
  }

  // IINA's OSC customizer is a fixed list of built-in buttons. A plugin
  // cannot add a row to it (iina/iina#6208). The on-screen button is an
  // overlay hit target instead, and the preference hides it.
  var overlayLoaded = false;
  var buttonShown = null;
  function playerButtonWanted() {
    var v = preferences.get("showPlayerButton");
    if (v === false || v === "false" || v === 0 || v === "0") return false;
    return true;
  }
  function syncPlayerButton() {
    if (!overlay || !overlayLoaded) return;
    var want = playerButtonWanted();
    if (want === buttonShown) return;
    buttonShown = want;
    if (want) overlay.show();
    else overlay.hide();
  }
  function loadOverlay(force) {
    if (!overlay) return;
    if (overlayLoaded && !force) return;
    try {
      overlay.loadFile("overlay.html"); // clears message listeners — register after
    } catch (e) {
      return;
    }
    overlay.setClickable(true);
    overlay.onMessage("getState", function () {
      onMainPoll();
      overlay.postMessage("state", stateForPage());
    });
    overlay.onMessage("cast", function () {
      sendToTV();
      overlay.postMessage("state", stateForPage());
    });
    overlay.onMessage("pick", function (d) {
      if (!d) return;
      if (d.kind === "airplay") {
        openSidebar();
        startCast();
      } else if (d.kind === "cast") {
        startCast({ mode: "cast", host: d.host || "", port: d.port || 8009, name: d.name || "", model: d.model || "" });
      } else if (d.control) {
        startCast({ mode: "dlna", control: d.control, name: d.name || "" });
      }
      overlay.postMessage("state", stateForPage());
    });
    overlay.onMessage("stop", function () {
      stopCast();
      overlay.postMessage("state", stateForPage());
    });
    overlayLoaded = true;
    buttonShown = null;
    syncPlayerButton();
  }

  // Force on window-loaded: nothing can be casting before the window exists,
  // so a reload here is harmless, and it guards against a pre-window loadFile
  // that failed without throwing.
  event.on("iina.window-loaded", function () {
    loadSidebar(true);
    loadOverlay(true);
  });
  loadSidebar(false);
  loadOverlay(false);

  menu.addItem(menu.item("Send to TV", function () {
    openSidebar();  // main-thread menu callback: safe
    sendToTV();     // picks AirPlay or DLNA; the page polls for the result
  }));
}
