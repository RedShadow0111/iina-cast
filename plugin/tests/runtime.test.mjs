// Exercises the IINA-runtime block of main.js against a fake `iina` global, so
// the sidebar<->plugin wiring (menu item, onMessage handlers, helper spawns) is
// covered by node --test rather than only by clicking around in IINA.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
const require = createRequire(import.meta.url);
const MAIN = require.resolve("../main.js");

function loadPlugin(opts = {}) {
  const trackList = opts.tracks || [
    { type: "video", selected: true, codec: "hevc", "ff-index": 0 },
    { type: "audio", selected: true, codec: "aac", "ff-index": 1, "demux-channel-count": 2 },
  ];
  const execs = [];
  const messages = {};       // name -> handler registered via sidebar.onMessage
  const overlayMessages = {};
  const posted = [];         // [name, payload] sent to the page
  const osd = [];
  const writes = [];
  const flags = { pause: !!opts.pause, mute: opts.mute || false, "sub-visibility": opts.subsVisible !== false };
  const sets = [];           // [name, value] pushed by mpv.set
  const events = {};         // event name -> callback registered via event.on
  const stdouts = [];        // per-serve stdout callbacks, for scripting helper events
  const stderrs = [];        // per-serve stderr callbacks
  const logs = [];           // console.log lines
  const menuItems = [];
  let loadedFile = null;
  let loads = 0;
  let shown = 0;
  // IINA's sidebar.loadFile throws until the player window exists (see
  // JavascriptAPISidebarView.swift); a plugin loaded at launch runs before
  // that, one enabled from Settings runs after.
  let windowLoaded = opts.windowLoaded !== undefined ? opts.windowLoaded : true;

  const iina = {
    core: { osd: (m) => osd.push(m), pause: () => { flags.pause = true; } },
    mpv: {
      getString: (k) => (k === "path" ? (opts.path !== undefined ? opts.path : "/movies/a.mkv") : null),
      getNumber: (k) => (k === "pid" ? 4321 : k === "duration" ? 120
                       : k === "time-pos" ? (flags["time-pos"] != null ? flags["time-pos"] : (opts.timePos !== undefined ? opts.timePos : 42)) : 0),
      getNative: () => trackList,
      getFlag: (k) => !!flags[k],
      set: (k, v) => { sets.push([k, v]); flags[k] = v; },
    },
    event: { on: (name, cb) => { events[name] = cb; } },
    menu: {
      item: (title, cb) => ({ title, cb }),
      addItem: (it) => { menuItems.push(it); },
    },
    sidebar: {
      loadFile: (f) => {
        if (!windowLoaded) throw new Error("sidebar.loadFile called when window is not available.");
        loadedFile = f; loads++;
        for (const k of Object.keys(messages)) delete messages[k];
      },
      onMessage: (name, cb) => { messages[name] = cb; },
      postMessage: (name, data) => posted.push([name, JSON.parse(JSON.stringify(data))]),
      show: () => { shown++; },
    },
    overlay: {
      loadFile: () => {
        if (!windowLoaded) throw new Error("overlay.loadFile called when window is not available.");
        for (const k of Object.keys(overlayMessages)) delete overlayMessages[k];
      },
      onMessage: (name, cb) => { overlayMessages[name] = cb; },
      postMessage: () => {},
      setClickable: () => {},
      show: () => {},
      hide: () => {},
    },
    utils: {
      resolvePath: (p) => "/plugins/.data/dev.faruk.iina-airplay" + (p === "@tmp/hls" ? "/tmp/hls" : ""),
      exec: (bin, args, cwd, onStdout, onStderr) => {
        execs.push({ bin, args });
        if (typeof onStdout === "function") stdouts.push(onStdout);
        if (typeof onStderr === "function") stderrs.push(onStderr);
        // opts.binDirLookup scripts the /bin/sh lookup that resolveBinDir runs
        // when no bin dir is cached; everything else hangs, as before.
        if (bin === "/bin/sh" && opts.binDirLookup) return Promise.resolve(opts.binDirLookup);
        // opts.curl scripts the reachability probe after a page media error
        // (issue #42): a result object resolves, an Error rejects.
        if (bin === "/usr/bin/curl" && opts.curl) {
          return opts.curl instanceof Error ? Promise.reject(opts.curl) : Promise.resolve(opts.curl);
        }
        return new Promise(() => {});
      },
    },
    file: {
      read: () => (opts.binDirLookup ? "" : "/plugins/dev.faruk.iina-airplay/bin"),
      write: (p, v) => { writes.push([p, v]); },
    },
    // Issue #33: preferences.get is synchronous in the JSContext and returns
    // the stored value or Info.json's preferenceDefaults entry. The default
    // for lanIP is "" (automatic), so that's the default here too.
    preferences: {
      get: (k) => (k === "lanIP" ? (opts.lanIP !== undefined ? opts.lanIP : "") : undefined),
    },
    console: { log: (m) => logs.push(m) },
  };

  globalThis.iina = iina;
  delete require.cache[MAIN];
  try { require("../main.js"); } finally { delete globalThis.iina; delete require.cache[MAIN]; }

  return {
    execs, posted, osd, logs, writes,
    flags, sets,
    fire: (name) => { assert.ok(events[name], `no event handler for "${name}"`); events[name](); },
    helperSays: (obj) => { assert.ok(stdouts.length, "no serve running"); stdouts.at(-1)(JSON.stringify(obj) + "\n"); },
    helperStderr: (chunk) => { assert.ok(stderrs.length, "no serve running"); stderrs.at(-1)(chunk); },
    clickMenu: () => menuItems[0].cb(),
    // Pipeline tests start an AirPlay cast directly. The menu item searches
    // the network first and only then decides.
    beginCast: () => messages.start({}),
    clickMenuNamed: (title) => {
      const it = menuItems.find((m) => m.title === title);
      assert.ok(it, "no menu item " + title);
      it.cb();
    },
    send: (name, data) => {
      assert.ok(messages[name], `no onMessage handler registered for "${name}"`);
      return messages[name](data);
    },
    sendOverlay: (name, data) => {
      assert.ok(overlayMessages[name], `no overlay handler registered for "${name}"`);
      return overlayMessages[name](data);
    },
    state: () => posted.filter(([n]) => n === "state").at(-1)?.[1],
    loadedFile: () => loadedFile,
    loads: () => loads,
    shown: () => shown,
    loadWindow: () => {
      windowLoaded = true;
      if (events["iina.window-loaded"]) events["iina.window-loaded"]();
    },
  };
}

const serves = (p) => p.execs.filter((e) => e.args[0] === "serve");
const stops = (p) => p.execs.filter((e) => e.args[0] === "stop");

test("Send to TV opens the sidebar and searches before it casts", () => {
  const p = loadPlugin();
  p.clickMenu();
  assert.equal(p.loadedFile(), "sidebar.html");
  assert.equal(p.shown(), 1);
  assert.equal(p.loads(), 1, "a reload would tear down the page's <video>");
  assert.equal(serves(p).length, 0);
  assert.equal(p.execs.filter((e) => e.args[0] === "dlna-discover").length, 1);
});

// Issue #20: Info.json declares a sidebarTab, so IINA shows an "AirPlay" tab
// the user can open without ever touching the Plugins menu. That tab is an
// empty WKWebView until the plugin calls sidebar.loadFile, so the page (and
// its Start casting button) must be loaded as soon as the window exists,
// not only from the menu item.
test("the sidebar tab works when opened directly, without the menu item", () => {
  const p = loadPlugin();               // window already up: plugin enabled from Settings
  assert.equal(p.loadedFile(), "sidebar.html");
  p.send("getState", {});
  assert.equal(p.state().phase, "idle");
  p.send("start", {});
  assert.equal(serves(p).length, 1);
});

test("a plugin loaded before the window loads the sidebar page on window-loaded", () => {
  const p = loadPlugin({ windowLoaded: false }); // normal launch order
  assert.equal(p.loadedFile(), null, "must not blow up before the window exists");
  p.loadWindow();
  assert.equal(p.loadedFile(), "sidebar.html");
  p.send("getState", {});
  assert.equal(p.state().phase, "idle");
});

test("the menu item shows the already-loaded page instead of reloading it", () => {
  const p = loadPlugin();
  p.clickMenu();
  assert.equal(p.loads(), 1, "a reload would tear down the page's <video> mid-cast");
  assert.equal(p.shown(), 1);
  assert.equal(serves(p).length, 0);
});

test("the page can start a cast after stopping, without reloading the plugin", () => {
  const p = loadPlugin();
  p.beginCast();
  assert.equal(serves(p).length, 1);

  p.send("stop", {});
  assert.equal(stops(p).length, 1);
  assert.equal(p.state().phase, "idle");

  p.send("start", {});
  assert.equal(serves(p).length, 2, "stopping must not leave the cast unstartable from the page");
  assert.equal(p.state().phase, "starting");
});

test("start is a no-op while a cast is already running", () => {
  const p = loadPlugin();
  p.beginCast();
  p.send("start", {});
  assert.equal(serves(p).length, 1);
});

const baseTracks = [
  { type: "video", selected: true, codec: "hevc", "ff-index": 0 },
  { type: "audio", selected: true, codec: "aac", "ff-index": 1, "demux-channel-count": 2 },
];

test("serve args carry -smap/-sublang/-subname for an embedded text sub", () => {
  const p = loadPlugin({ tracks: [...baseTracks,
    { type: "sub", selected: true, codec: "subrip", "ff-index": 2, lang: "en", title: "English" }] });
  p.beginCast();
  const args = serves(p)[0].args;
  const i = args.indexOf("-smap");
  assert.notEqual(i, -1);
  assert.equal(args[i + 1], "2");
  assert.equal(args[args.indexOf("-sublang") + 1], "en");
  assert.equal(args[args.indexOf("-subname") + 1], "English");
  assert.equal(args.indexOf("-subpath"), -1);
});

test("serve args carry -subpath for an external sub, not -smap", () => {
  const p = loadPlugin({ tracks: [...baseTracks,
    { type: "sub", selected: true, codec: "subrip", "ff-index": 2,
      external: true, "external-filename": "/subs/en.srt", lang: "en" }] });
  p.beginCast();
  const args = serves(p)[0].args;
  assert.equal(args[args.indexOf("-subpath") + 1], "/subs/en.srt");
  assert.equal(args.indexOf("-smap"), -1);
});

test("image subs cast without subs and OSD the reason", () => {
  const p = loadPlugin({ tracks: [...baseTracks,
    { type: "sub", selected: true, codec: "hdmv_pgs_subtitle", "ff-index": 2 }] });
  p.beginCast();
  assert.equal(serves(p).length, 1, "cast must still start");
  const args = serves(p)[0].args;
  assert.equal(args.indexOf("-smap"), -1);
  assert.equal(args.indexOf("-subpath"), -1);
  assert.ok(p.osd.some(m => /subtitle/i.test(m)), "expected an OSD about dropped subtitles");
});

test("no sub flags when no sub is selected", () => {
  const p = loadPlugin();
  p.beginCast();
  const args = serves(p)[0].args;
  assert.equal(args.indexOf("-smap"), -1);
  assert.equal(args.indexOf("-subpath"), -1);
  assert.equal(args.indexOf("-sublang"), -1);
});

test("a broken install names the fix instead of leaking an exec error", async () => {
  // Exit 3 is resolveBinDir's signal for "found the plugin, but its binaries
  // are missing or not executable" — a hand-copied or quarantined install.
  const p = loadPlugin({ binDirLookup: { status: 3, stdout: "" } });
  p.beginCast();
  await new Promise((r) => setImmediate(r)); // let the exec promise settle
  p.send("getState", {}); // simulate the page's poll to read the async result
  const s = p.state();
  assert.equal(s.phase, "error");
  assert.match(s.msg, /reinstall/i);
  assert.match(s.msg, /IINA/);
  assert.equal(serves(p).length, 0, "a broken install must not spawn the helper");
});

test("a network stream is declined with the stream-specific message, not silently absolute-path-rejected", () => {
  const p = loadPlugin({ path: "http://192.168.1.5:8080/video.mp4" });
  p.beginCast();
  assert.ok(p.osd.some(m => /network stream/i.test(m)), "expected an OSD naming network streams");
  assert.equal(serves(p).length, 0, "a network stream must not spawn the helper");
  p.send("getState", {});
  assert.equal(p.state().phase, "error", "a sidebar-only viewer must see the error, not a stuck button");
  assert.match(p.state().msg, /network stream/i);
});

test("a relative path gets the local-file-path message, not the network-stream one", () => {
  const p = loadPlugin({ path: "movie.mkv" });
  p.beginCast();
  assert.ok(p.osd.some(m => /local file path/i.test(m)), "expected an OSD naming the local-file-path problem");
  assert.ok(!p.osd.some(m => /network stream/i.test(m)), "a relative path is not a network stream");
  assert.equal(serves(p).length, 0, "a relative path must not spawn the helper");
  p.send("getState", {});
  assert.equal(p.state().phase, "error", "a sidebar-only viewer must see the error, not a stuck button");
  assert.match(p.state().msg, /local file path/i);
});

test("nothing playing sets the error state too, not just the OSD", () => {
  const p = loadPlugin({ path: null });
  p.beginCast();
  assert.ok(p.osd.some(m => /nothing is playing/i.test(m)));
  assert.equal(serves(p).length, 0);
  p.send("getState", {});
  assert.equal(p.state().phase, "error", "a sidebar-only viewer must see the error, not a stuck button");
  assert.match(p.state().msg, /nothing is playing/i);
});

test("no castable tracks sets the error state too, not just the OSD", () => {
  const p = loadPlugin({ tracks: [] });
  p.beginCast();
  assert.ok(p.osd.some(m => /no castable video\/audio tracks/i.test(m)));
  assert.equal(serves(p).length, 0);
  p.send("getState", {});
  assert.equal(p.state().phase, "error", "a sidebar-only viewer must see the error, not a stuck button");
  assert.match(p.state().msg, /no castable video\/audio tracks/i);
});

test("a file:// URI is normalized and cast, not declined as a network stream", () => {
  const p = loadPlugin({ path: "file:///movies/a.mkv" });
  p.beginCast();
  assert.equal(serves(p).length, 1, "a file:// URI names a real local file that should be castable");
  assert.ok(!p.osd.some(m => /network stream/i.test(m)), "file:// must not be mislabelled a network stream");
});

// ---- muted mirror wiring (spec 2026-08-30) ----

test("starting a cast mutes mpv and does NOT pause it", () => {
  const p = loadPlugin();
  p.beginCast();
  assert.deepEqual(p.sets.filter(([k]) => k === "mute").at(-1), ["mute", true]);
  assert.equal(p.flags.pause, false, "the muted mirror replaced core.pause()");
});

test("state posts carry the sync block while casting, null when idle", () => {
  const p = loadPlugin();
  p.beginCast();
  p.send("getState", {});
  assert.deepEqual(p.state().sync,
    { seq: 0, paused: false, seekTo: null, startPos: 42 });
  p.send("stop", {});
  assert.equal(p.state().sync, null);
});

test("stop restores the pre-cast mute flag (false case)", () => {
  const p = loadPlugin({ mute: false });
  p.beginCast();
  p.send("stop", {});
  assert.equal(p.flags.mute, false);
});

test("stop preserves a user's pre-cast mute (true case)", () => {
  const p = loadPlugin({ mute: true });
  p.beginCast();
  assert.equal(p.flags.mute, true, "muting an already-muted mpv is fine");
  p.send("stop", {});
  assert.equal(p.flags.mute, true, "the user's own mute must survive the cast");
});

test("an mpv pause bumps the sync seq and desired state", () => {
  const p = loadPlugin();
  p.beginCast();
  p.flags.pause = true;
  p.fire("mpv.pause.changed");
  p.send("getState", {});
  assert.equal(p.state().sync.seq, 1);
  assert.equal(p.state().sync.paused, true);
});

test("an mpv seek becomes a seekTo command once the position has moved", () => {
  const p = loadPlugin({ timePos: 42 });
  p.beginCast();
  p.flags["time-pos"] = 300;
  p.fire("mpv.seek");
  p.send("getState", {});
  assert.deepEqual(p.state().sync.seekTo, { seq: 1, pos: 300 });
});

test("a seek event that arrives before time-pos waits for the new position", () => {
  const p = loadPlugin({ timePos: 42 });
  p.beginCast();
  p.fire("mpv.seek");
  p.send("getState", {});
  assert.equal(p.state().sync.seekTo, null);
  p.flags["time-pos"] = 80;
  p.send("getState", {});
  assert.deepEqual(p.state().sync.seekTo, { seq: 1, pos: 80 });
});

test("a small time jump is not sent to the TV", () => {
  const p = loadPlugin({ timePos: 100 });
  p.send("start", { mode: "dlna", control: "http://192.168.0.220/ctl", name: "TV" });
  p.helperSays({ event: "ready", url: "http://192.168.0.84:9/media", base: 0 });
  p.helperSays({ event: "tv", pos: 100, paused: false, ended: false });
  p.flags["time-pos"] = 103;
  p.send("getState");
  const ctl = p.execs.filter((e) => e.args[0] === "dlna-ctl" && e.args.includes("seek"));
  assert.equal(ctl.length, 0);
});

test("a timeline jump is sent to the TV without a seek event", () => {
  const p = loadPlugin({ timePos: 10 });
  p.send("start", { mode: "dlna", control: "http://192.168.0.220/ctl", name: "TV" });
  p.helperSays({ event: "ready", url: "http://192.168.0.84:9/media", base: 0 });
  p.helperSays({ event: "tv", pos: 10, paused: false, ended: false });
  p.flags["time-pos"] = 40;
  p.send("getState");
  const ctl = p.execs.filter((e) => e.args[0] === "dlna-ctl" && e.args.includes("seek")).at(-1);
  assert.ok(ctl, "the poll must seek the TV");
  assert.equal(ctl.args[ctl.args.indexOf("-pos") + 1], "40");
});

test("closing the player window stops the television", () => {
  const p = loadPlugin();
  p.send("start", { mode: "dlna", control: "http://192.168.0.220/ctl", name: "TV" });
  p.fire("iina.window-will-close");
  assert.equal(stops(p).length, 1);
  const ctl = p.execs.filter((e) => e.args[0] === "dlna-ctl").at(-1);
  assert.equal(ctl.args[ctl.args.indexOf("-action") + 1], "stop");
  p.fire("iina.window-did-close");
  assert.equal(stops(p).length, 1, "a second close event must not start another stop race");
});

test("stopping the file stops the television", () => {
  const p = loadPlugin();
  p.send("start", { mode: "dlna", control: "http://192.168.0.220/ctl", name: "TV" });
  p.fire("mpv.end-file");
  assert.equal(stops(p).length, 1);
});

test("end-file without a cast does nothing", () => {
  const p = loadPlugin();
  p.fire("mpv.end-file");
  assert.equal(stops(p).length, 0);
});

test("a TV-remote pause is mirrored into mpv", () => {
  const p = loadPlugin();
  p.beginCast();
  p.send("tvState", { appliedSeq: 0, pos: 42, paused: true, wireless: true, ended: false });
  assert.deepEqual(p.sets.filter(([k]) => k === "pause").at(-1), ["pause", true]);
});

test("DLNA playback does not seek the local file to erase a small drift", () => {
  const p = loadPlugin({ timePos: 100 });
  p.send("start", { mode: "dlna", control: "http://192.168.0.220/ctl", name: "TV" });
  p.helperSays({ event: "ready", url: "http://192.168.0.84:9/media", base: 0 });
  p.helperSays({ event: "tv", pos: 106, paused: false, ended: false });
  p.send("getState");
  p.send("tvState", { appliedSeq: 0, pos: 106, paused: false, wireless: true, ended: false });
  assert.ok(!p.sets.some(([k]) => k === "time-pos"), "a drift seek is the mid-movie rebuffer");
});

test("resuming after a stall puts the player on the television clock without reopening it", () => {
  const p = loadPlugin({ timePos: 500 });
  p.send("start", { mode: "dlna", control: "http://192.168.0.220/ctl", name: "TV" });
  p.helperSays({ event: "ready", url: "http://192.168.0.84:9/media", base: 0 });
  p.helperSays({ event: "landed", pos: 500, ok: true });
  p.send("getState");
  p.helperSays({ event: "tv", pos: 500, paused: false, ended: false });
  p.send("getState");
  assert.equal(p.flags.pause, false);
  // The television stalls at 505 while the local file keeps going. Each step
  // is under the jump threshold, so it must not be sent to the TV as a seek.
  for (let pos = 505; pos <= 530; pos += 5) {
    p.flags["time-pos"] = pos;
    p.helperSays({ event: "tv", pos: 505, paused: false, ended: false });
    p.send("getState");
  }
  assert.ok(!p.sets.some(([k]) => k === "time-pos"), "steady playback is not pulled back");
  const seeksBefore = p.execs.filter((e) => e.args[0] === "dlna-ctl" && e.args.includes("seek")).length;
  p.flags.pause = true;
  p.fire("mpv.pause.changed");
  p.send("getState");
  p.helperSays({ event: "tv", pos: 505, paused: true, ended: false });
  p.send("getState");
  p.flags.pause = false;
  p.fire("mpv.pause.changed");
  p.send("getState");
  p.helperSays({ event: "tv", pos: 505, paused: false, ended: false });
  p.send("getState");
  const tp = p.sets.filter(([k]) => k === "time-pos");
  assert.ok(tp.length >= 1, "resume catches the player up to the television");
  assert.ok(Math.abs(tp.at(-1)[1] - 505) < 1.5, "landed on " + tp.at(-1)[1]);
  const seeksAfter = p.execs.filter((e) => e.args[0] === "dlna-ctl" && e.args.includes("seek")).length;
  assert.equal(seeksAfter, seeksBefore, "catching up must not make the TV reopen the movie");
});

test("drift beyond tolerance corrects mpv's clock to the TV's", () => {
  const p = loadPlugin({ timePos: 42 });
  p.beginCast();
  p.send("tvState", { appliedSeq: 0, pos: 50, paused: false, wireless: true, ended: false });
  assert.deepEqual(p.sets.filter(([k]) => k === "time-pos").at(-1), ["time-pos", 50]);
});

test("tvState.ended tears the cast down and restores mute", () => {
  const p = loadPlugin({ mute: false });
  p.beginCast();
  p.send("tvState", { appliedSeq: 0, pos: 120, paused: false, wireless: true, ended: true });
  assert.equal(stops(p).length, 1);
  assert.equal(p.flags.mute, false);
  p.send("getState", {});
  assert.equal(p.state().phase, "idle");
});

test("loading a new file during a cast tears it down", () => {
  const p = loadPlugin();
  p.beginCast();
  p.fire("iina.file-loaded");
  assert.equal(stops(p).length, 1);
  assert.equal(p.flags.mute, false);
});

test("file-loaded without a cast is a no-op", () => {
  const p = loadPlugin();
  p.fire("iina.file-loaded");
  assert.equal(stops(p).length, 0);
});

test("a pre-handshake tvState must not un-pause a paused cast", () => {
  const p = loadPlugin();
  p.flags.pause = true;         // mpv is paused when the user starts the cast
  p.beginCast();
  // appliedSeq -1 is what the page sends before its own seq-0 sync (carrying
  // the initial paused snapshot) has been applied: appliedSeq -1 < mirror.seq
  // 0 suspends reconciliation, so this pre-handshake tvState must not be
  // misread as a TV-initiated un-pause.
  p.send("tvState", { appliedSeq: -1, pos: 0, paused: false, wireless: true, ended: false });
  assert.ok(!p.sets.some(([k, v]) => k === "pause" && v === false),
    "a pre-handshake tvState must not un-pause mpv");
  p.send("getState", {});
  assert.equal(p.state().sync.paused, true, "the initial snapshot must still be the desired state");
});

test("the poll restores mute after the helper dies on its own", () => {
  const p = loadPlugin({ mute: false });
  p.beginCast();
  p.helperSays({ event: "stopped" });  // helper-initiated teardown: off-main, may not touch mpv
  assert.equal(p.flags.mute, true, "the exec callback itself must not touch mpv");
  p.send("getState", {});              // the next poll runs on-message: safe to restore
  assert.equal(p.flags.mute, false);
  assert.equal(p.state().sync, null);
});

test("tvState reaps a stale mirror after the helper dies, before touching mpv", () => {
  const p = loadPlugin({ mute: false });
  p.beginCast();
  p.helperSays({ event: "stopped" }); // helper-initiated teardown: off-main, only state mutates
  p.send("tvState", { appliedSeq: 0, pos: 50, paused: false, wireless: true, ended: false });
  assert.equal(p.flags.mute, false, "tvState must reap the stale mirror and restore mute");
  assert.ok(!p.sets.some(([k, v]) => k === "time-pos" && v === 50),
    "must not mpv.set against a dead stream");
});

test("restarting a cast after a helper-initiated teardown does not corrupt the saved mute", () => {
  const p = loadPlugin({ mute: false });
  p.beginCast();
  p.helperSays({ event: "stopped" });   // teardown lands off-main: only state mutates
  p.send("start", {});                   // restart before any getState poll ran
  p.send("stop", {});
  assert.equal(p.flags.mute, false, "savedMute must come from the user's real pre-cast value");
});

test("state carries duration and a subtitle label", () => {
  const p = loadPlugin();
  p.beginCast();
  p.send("getState", {}); // simulate the page's poll so a "state" message is posted
  const s = p.state();
  assert.equal(s.duration, 120);
  assert.deepEqual(s.subs, { label: "No subtitles", warn: false });
});

test("state reports the selected subtitle track", () => {
  const p = loadPlugin({ tracks: [
    { type: "video", selected: true, codec: "hevc", "ff-index": 0 },
    { type: "audio", selected: true, codec: "aac", "ff-index": 1, "demux-channel-count": 2 },
    { type: "sub", selected: true, codec: "subrip", "ff-index": 2, lang: "eng" },
  ] });
  p.beginCast();
  p.send("getState", {}); // simulate the page's poll so a "state" message is posted
  assert.equal(p.state().subs.label, "Subtitles: eng");
});

test("subtitle label is available before any cast starts", () => {
  const p = loadPlugin();
  p.beginCast();
  p.send("stop");
  const s = p.state();
  assert.equal(s.phase, "idle");
  assert.equal(s.subs.label, "No subtitles");
});

// Issue #33: the "LAN address" preference reaches the helper as `serve -ip`.
test("serve args carry no -ip when the LAN address setting is empty", () => {
  const p = loadPlugin();
  p.beginCast();
  assert.equal(serves(p)[0].args.indexOf("-ip"), -1);
});

test("serve args carry -ip when the LAN address setting is set", () => {
  const p = loadPlugin({ lanIP: "192.168.1.20" });
  p.beginCast();
  const args = serves(p)[0].args;
  const i = args.indexOf("-ip");
  assert.notEqual(i, -1, "expected -ip in serve args");
  assert.equal(args[i + 1], "192.168.1.20");
});

test("the LAN address setting is trimmed before it reaches the helper", () => {
  const p = loadPlugin({ lanIP: "  192.168.1.20 " });
  p.beginCast();
  const args = serves(p)[0].args;
  assert.equal(args[args.indexOf("-ip") + 1], "192.168.1.20");
});

test("a whitespace-only LAN address setting means automatic", () => {
  const p = loadPlugin({ lanIP: "   " });
  p.beginCast();
  assert.equal(serves(p)[0].args.indexOf("-ip"), -1);
});

// Issue #38: IINA's exec stderr readabilityHandler keeps firing with "" after
// the helper's stderr hits EOF, until the process terminationHandler clears it
// (JavascriptAPIUtils.swift, exec). Those empty chunks must not reach the log.
test("empty helper stderr chunks are not logged", () => {
  const p = loadPlugin();
  p.beginCast();
  for (let i = 0; i < 3; i++) p.helperStderr("");
  p.helperStderr("\n");
  assert.deepEqual(p.logs.filter((l) => l.startsWith("[helper:stderr]")), []);
});

test("non-empty helper stderr is logged, trimmed", () => {
  const p = loadPlugin();
  p.beginCast();
  p.helperStderr("ffmpeg: something went wrong\n");
  assert.deepEqual(p.logs.filter((l) => l.startsWith("[helper:stderr]")),
    ["[helper:stderr] ffmpeg: something went wrong"]);
});

// Issue #42: a page-side "media error 4" arrived with no URL and no reason in
// either the sidebar or IINA's log, so a report couldn't say what failed.
const STREAM = "http://192.168.1.2:5000/master.m3u8";
const curls = (p) => p.execs.filter((e) => e.bin === "/usr/bin/curl");
const tick = () => new Promise((r) => setImmediate(r));

test("the stream URL is logged when the helper reports ready", () => {
  const p = loadPlugin();
  p.beginCast();
  p.helperSays({ event: "ready", url: STREAM });
  assert.ok(p.logs.some((l) => l.includes(STREAM)), "expected the stream URL in the log");
});

test("a page media error probes the stream and reports why it failed", async () => {
  const p = loadPlugin({ curl: { status: 7, stdout: "000",
    stderr: "curl: (7) Failed to connect to 192.168.1.2 port 5000 after 1 ms: Couldn't connect to server" } });
  p.beginCast();
  p.helperSays({ event: "ready", url: STREAM });
  p.send("mediaError", { url: STREAM, code: 4 });
  assert.equal(curls(p).length, 1);
  assert.equal(curls(p)[0].args.at(-1), STREAM);
  assert.ok(p.logs.some((l) => l.includes("media error 4") && l.includes(STREAM)));
  await tick();
  p.send("getState");
  const diag = p.state().diag;
  assert.equal(diag.url, STREAM);
  assert.match(diag.msg, /can't reach its own stream/);
  assert.ok(p.logs.some((l) => l.includes("Couldn't connect to server")));
});

test("a curl that fails to run still leaves a diagnosis naming the URL", async () => {
  const p = loadPlugin({ curl: new Error("no such file") });
  p.beginCast();
  p.helperSays({ event: "ready", url: STREAM });
  p.send("mediaError", { url: STREAM, code: 4 });
  await tick();
  p.send("getState");
  assert.match(p.state().diag.msg, /media error 4/);
});

test("a media error for a stream that is no longer current is ignored", () => {
  const p = loadPlugin({ curl: { status: 0, stdout: "200", stderr: "" } });
  p.beginCast();
  p.helperSays({ event: "ready", url: STREAM });
  p.send("mediaError", { url: "http://192.168.1.2:4000/master.m3u8", code: 4 });
  assert.equal(curls(p).length, 0);
});

test("a probe that settles after the cast was stopped is dropped", async () => {
  const p = loadPlugin({ curl: { status: 0, stdout: "200", stderr: "" } });
  p.beginCast();
  p.helperSays({ event: "ready", url: STREAM });
  p.send("mediaError", { url: STREAM, code: 4 });
  p.send("stop");
  await tick();
  p.send("getState");
  assert.equal(p.state().diag, null);
});

const discovers = (p) => p.execs.filter((e) => e.args[0] === "dlna-discover");

test("Send to TV searches from the configured LAN address", () => {
  const p = loadPlugin({ lanIP: "192.168.0.84" });
  p.clickMenu();
  assert.equal(discovers(p).length, 1);
  const args = discovers(p)[0].args;
  assert.equal(args[args.indexOf("-ip") + 1], "192.168.0.84");
  p.send("getState");
  assert.equal(p.state().phase, "browsing");
});

test("one DLNA television starts without a second click", () => {
  const p = loadPlugin();
  p.clickMenu();
  p.helperSays({ event: "devices", devices: [
    { name: "CustomCars", control: "http://192.168.0.220/ctl", host: "192.168.0.220" },
  ], airplay: [{ name: "CustomCars", model: "AppleTV3,1" }] });
  p.send("getState");
  assert.equal(serves(p).length, 1);
  const args = serves(p)[0].args;
  assert.equal(args[args.indexOf("-mode") + 1], "dlna");
  assert.equal(args[args.indexOf("-dlna") + 1], "http://192.168.0.220/ctl");
  assert.equal(args[args.indexOf("-title") + 1], "a");
});

test("a modern Apple TV and a DLNA screen wait for a choice", () => {
  const p = loadPlugin();
  p.clickMenu();
  p.helperSays({ event: "devices", devices: [
    { name: "CustomCars", control: "http://192.168.0.220/ctl", host: "192.168.0.220" },
  ], airplay: [{ name: "Lounge", model: "AppleTV11,1" }] });
  p.send("getState");
  assert.equal(serves(p).length, 0);
  assert.equal(p.state().phase, "browsing");
  assert.equal(p.state().devices.length, 2);
  assert.equal(p.state().devices[1].kind, "airplay");
});

test("only a modern Apple TV starts AirPlay", () => {
  const p = loadPlugin();
  p.clickMenu();
  p.helperSays({ event: "devices", devices: [], airplay: [{ name: "Lounge", model: "AppleTV11,1" }] });
  p.send("getState");
  assert.equal(serves(p).length, 1);
  assert.equal(serves(p)[0].args.indexOf("-mode"), -1);
});

test("the player button can pick one of several TVs", () => {
  const p = loadPlugin();
  p.clickMenu();
  p.helperSays({ event: "devices", devices: [
    { name: "CustomCars", control: "http://192.168.0.220/ctl", host: "192.168.0.220" },
    { name: "Other", control: "http://192.168.0.221/ctl", host: "192.168.0.221" },
  ], airplay: [] });
  p.send("getState");
  p.sendOverlay("pick", { kind: "dlna", control: "http://192.168.0.221/ctl", name: "Other" });
  assert.equal(serves(p).length, 1);
  assert.equal(serves(p)[0].args[serves(p)[0].args.indexOf("-dlna") + 1], "http://192.168.0.221/ctl");
});

test("several DLNA televisions wait for a choice", () => {
  const p = loadPlugin();
  p.send("cast");
  p.helperSays({ event: "devices", devices: [
    { name: "CustomCars", control: "http://192.168.0.220/ctl", host: "192.168.0.220" },
    { name: "Other", control: "http://192.168.0.221/ctl", host: "192.168.0.221" },
  ], airplay: [] });
  p.send("getState");
  assert.equal(serves(p).length, 0);
  assert.equal(p.state().phase, "browsing");
  assert.equal(p.state().devices.length, 2);
});

test("an empty search says no television was found", () => {
  const p = loadPlugin();
  p.send("discover");
  p.helperSays({ event: "devices", devices: [], airplay: [{ name: "This Mac", model: "MacBookAir10,1" }] });
  p.send("getState");
  assert.equal(p.state().phase, "error");
  assert.equal(p.state().msg, "No TV found on the network");
});

test("choosing a DLNA renderer serves that device instead of HLS", () => {
  const p = loadPlugin();
  p.send("start", { mode: "dlna", control: "http://192.168.0.220/ctl", name: "CustomCars" });
  assert.equal(serves(p).length, 1);
  const args = serves(p)[0].args;
  assert.equal(args[args.indexOf("-mode") + 1], "dlna");
  assert.equal(args[args.indexOf("-dlna") + 1], "http://192.168.0.220/ctl");
  assert.equal(args[args.indexOf("-title") + 1], "a");
  assert.equal(args[args.indexOf("-start") + 1], "42");
  assert.notEqual(args.indexOf("-paused"), -1, "the TV waits, paused, on the same frame");
  assert.equal(p.flags.pause, true, "the player waits with it");
  assert.equal(args.indexOf("-remux"), -1, "one audio track in the helper's own decision");
  assert.equal(p.state().mode, "dlna");
  assert.equal(p.state().dlnaName, "CustomCars");
});

test("a DLNA cast remuxes when IINA selects a later audio track", () => {
  const p = loadPlugin({ tracks: [
    { type: "video", selected: true, codec: "hevc", "ff-index": 0 },
    { type: "audio", selected: false, codec: "aac", "ff-index": 1, "demux-channel-count": 2 },
    { type: "audio", selected: true, codec: "ac3", "ff-index": 2, "demux-channel-count": 6 },
  ] });
  p.send("start", { mode: "dlna", control: "http://192.168.0.220/ctl", name: "TV" });
  const args = serves(p)[0].args;
  assert.notEqual(args.indexOf("-remux"), -1);
  assert.equal(args[args.indexOf("-amap") + 1], "2");
  assert.equal(args[args.indexOf("-acodec") + 1], "ac3");
});

test("a paused DLNA cast asks the TV to start paused", () => {
  const p = loadPlugin({ pause: true });
  p.send("start", { mode: "dlna", control: "http://192.168.0.220/ctl", name: "TV" });
  assert.notEqual(serves(p)[0].args.indexOf("-paused"), -1);
  p.helperSays({ event: "ready", url: "http://192.168.0.84:9/media", base: 0 });
  p.helperSays({ event: "landed", pos: 42 });
  p.send("getState");
  assert.equal(p.flags.pause, true, "a file the user had paused stays paused");
  assert.equal(p.execs.filter((e) => e.args[0] === "dlna-ctl" && e.args.includes("play")).length, 0);
});

test("a playing file pauses until the TV is on that frame, then both play", () => {
  const p = loadPlugin({ timePos: 42 });
  p.send("start", { mode: "dlna", control: "http://192.168.0.220/ctl", name: "TV" });
  assert.equal(p.flags.pause, true);
  p.helperSays({ event: "ready", url: "http://192.168.0.84:9/media", base: 0 });
  p.helperSays({ event: "landed", pos: 42, ok: true });
  p.send("getState");
  assert.equal(p.flags.pause, true, "the player waits until the TV is actually playing");
  const play = p.execs.filter((e) => e.args[0] === "dlna-ctl" && e.args.includes("play"));
  assert.equal(play.length, 1);
  p.helperSays({ event: "tv", pos: 42, paused: false, ended: false });
  p.send("getState");
  assert.equal(p.flags.pause, false);
  assert.equal(p.state().sync.paused, false);
  assert.equal(p.execs.filter((e) => e.args[0] === "dlna-ctl" && e.args.includes("play")).length, 1);
});

test("the cast keeps the position from before the player was paused", () => {
  const p = loadPlugin({ timePos: 500 });
  p.send("cast");
  assert.equal(p.flags.pause, true);
  p.flags["time-pos"] = 0;
  p.helperSays({ event: "devices", devices: [
    { name: "CustomCars", control: "http://192.168.0.220/ctl", host: "192.168.0.220" },
  ], airplay: [] });
  p.send("getState");
  const args = serves(p)[0].args;
  assert.equal(args[args.indexOf("-start") + 1], "500");
});

test("a failed search resumes the file that was playing", () => {
  const p = loadPlugin();
  p.send("cast");
  assert.equal(p.flags.pause, true);
  p.helperSays({ event: "error", msg: "discovery failed" });
  p.send("getState");
  assert.equal(p.flags.pause, false);
});

test("an AirPlay cast does not pass DLNA flags", () => {
  const p = loadPlugin();
  p.beginCast();
  assert.equal(serves(p)[0].args.indexOf("-mode"), -1);
});

test("a DLNA position report is stored for the page to mirror", () => {
  const p = loadPlugin();
  p.send("start", { mode: "dlna", control: "http://192.168.0.220/ctl", name: "TV" });
  p.helperSays({ event: "ready", url: "http://192.168.0.84:9/media" });
  p.helperSays({ event: "tv", pos: 12.5, paused: false, ended: false });
  p.send("getState");
  assert.equal(p.state().phase, "ready");
  assert.deepEqual(p.state().tv, { pos: 12.5, paused: false, ended: false });
});

test("the page can pause and seek the DLNA renderer", () => {
  const p = loadPlugin();
  p.send("start", { mode: "dlna", control: "http://192.168.0.220/ctl", name: "TV" });
  p.send("dlnaCtl", { action: "seek", pos: 90 });
  p.send("dlnaCtl", { action: "pause" });
  const ctls = p.execs.filter((e) => e.args[0] === "dlna-ctl");
  assert.equal(ctls.length, 2);
  assert.equal(ctls[0].args[ctls[0].args.indexOf("-pos") + 1], "90");
  assert.equal(ctls[1].args[ctls[1].args.indexOf("-action") + 1], "pause");
});

test("a seek is sent as IINA's time, not a position inside a clipped copy", () => {
  const p = loadPlugin();
  p.send("start", { mode: "dlna", control: "http://192.168.0.220/ctl", name: "TV" });
  p.helperSays({ event: "ready", url: "http://192.168.0.84:9/media", base: 40 });
  p.send("dlnaCtl", { action: "seek", pos: 90 });
  const ctl = p.execs.filter((e) => e.args[0] === "dlna-ctl").at(-1);
  assert.equal(ctl.args[ctl.args.indexOf("-pos") + 1], "90");
  assert.equal(serves(p).length, 1);
});

test("a seek does not recopy the file", () => {
  const p = loadPlugin({ timePos: 40 });
  p.send("start", { mode: "dlna", control: "http://192.168.0.220/ctl", name: "TV" });
  p.helperSays({ event: "ready", url: "http://192.168.0.84:9/media", base: 40 });
  p.helperSays({ event: "tv", pos: 40, paused: false, ended: false });
  p.send("dlnaCtl", { action: "seek", pos: 3383 });
  assert.equal(serves(p).length, 1);
  const ctl = p.execs.filter((e) => e.args[0] === "dlna-ctl").at(-1);
  assert.equal(ctl.args[ctl.args.indexOf("-pos") + 1], "3383");
  assert.equal(p.writes.length, 0);
});

test("hidden subtitles are reported as off", () => {
  const p = loadPlugin({
    subsVisible: false,
    tracks: [
      { type: "video", selected: true, codec: "hevc", "ff-index": 0 },
      { type: "audio", selected: true, codec: "aac", "ff-index": 1, "demux-channel-count": 2 },
      { type: "sub", selected: true, codec: "subrip", "ff-index": 2, lang: "ru" },
    ],
  });
  p.send("getState", {});
  assert.equal(p.state().subs.label, "No subtitles");
});

test("switching the audio track pauses, syncs to the TV and recasts that track", () => {
  const tracks = [
    { type: "video", selected: true, codec: "hevc", "ff-index": 0 },
    { type: "audio", selected: true, codec: "aac", "ff-index": 1, "demux-channel-count": 2 },
    { type: "audio", selected: false, codec: "ac3", "ff-index": 2, "demux-channel-count": 6 },
  ];
  const p = loadPlugin({ tracks, timePos: 40 });
  p.send("start", { mode: "dlna", control: "http://192.168.0.220/ctl", name: "TV" });
  p.helperSays({ event: "ready", url: "http://192.168.0.84:9/media", base: 0 });
  p.helperSays({ event: "tv", pos: 55, paused: false, ended: false });
  tracks[1].selected = false;
  tracks[2].selected = true;
  p.flags["time-pos"] = 80;
  p.fire("mpv.aid.changed");
  assert.equal(serves(p).length, 1, "the change waits for the main-thread poll");
  p.send("getState");
  assert.equal(p.flags.pause, true);
  assert.equal(p.flags["time-pos"], 55);
  const args = serves(p).at(-1).args;
  assert.equal(args[args.indexOf("-amap") + 1], "2");
  assert.equal(args[args.indexOf("-acodec") + 1], "ac3");
  assert.equal(args[args.indexOf("-start") + 1], "55");
  assert.notEqual(args.indexOf("-paused"), -1);
  assert.notEqual(args.indexOf("-remux"), -1);
  assert.equal(stops(p).length, 0);
  assert.ok(p.osd.some((m) => m.includes("смена аудиодорожки")));
  p.send("getState");
  assert.equal(p.flags.pause, true, "playback stays paused until the user presses play");
  assert.equal(serves(p).length, 2);
});

test("an audio event for the track already casting does not restart", () => {
  const p = loadPlugin();
  p.send("start", { mode: "dlna", control: "http://192.168.0.220/ctl", name: "TV" });
  p.fire("mpv.aid.changed");
  p.send("getState");
  assert.equal(serves(p).length, 1);
});
