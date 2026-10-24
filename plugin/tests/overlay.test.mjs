// Runs overlay.html's inline script against stub DOM elements and a fake
// iina bridge, so the on-player button and the multi-TV list are covered
// without clicking around in IINA.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const html = readFileSync(new URL("../overlay.html", import.meta.url), "utf8");
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];

function loadPage() {
  const list = {
    className: "hide", kids: [], textContent: "",
    appendChild(c) { this.kids.push(c); return c; },
    replaceChildren() { this.kids = []; },
  };
  const btn = { className: "", title: "", onclick: null };
  const label = { textContent: "" };
  const posted = [];
  const handlers = {};
  const context = {
    document: {
      getElementById: (id) => (id === "list" ? list : id === "b" ? btn : label),
      createElement: () => ({ type: "", textContent: "", onclick: null, setAttribute() {} }),
    },
    iina: {
      postMessage: (name, data) => posted.push([name, data]),
      onMessage: (name, cb) => { handlers[name] = cb; },
    },
    setInterval: () => 0,
  };
  vm.runInNewContext(script, context);
  return {
    posted, list, btn, label,
    state: (s) => handlers.state(s),
    click: () => btn.onclick(),
  };
}

test("the idle button sends to the TV", () => {
  const p = loadPage();
  p.state({ phase: "idle" });
  assert.equal(p.label.textContent, "На ТВ");
  p.click();
  assert.deepEqual(p.posted.map(([n]) => n), ["getState", "cast"]);
});

test("searching does not cancel, and several TVs are a choice", () => {
  const p = loadPage();
  p.state({ phase: "browsing", devices: [] });
  assert.equal(p.label.textContent, "Поиск…");
  p.posted.length = 0;
  p.click();
  assert.deepEqual(p.posted, []);

  p.state({
    phase: "browsing",
    devices: [
      { kind: "dlna", name: "CustomCars", control: "http://192.168.0.220/ctl", host: "192.168.0.220" },
      { kind: "airplay", name: "Lounge", control: "", host: "" },
    ],
  });
  assert.equal(p.label.textContent, "Отмена");
  assert.equal(p.list.kids.length, 2);
  assert.equal(p.list.kids[0].textContent, "CustomCars");
  assert.equal(p.list.kids[1].textContent, "Lounge (AirPlay)");
  p.posted.length = 0;
  p.list.kids[0].onclick();
  p.list.kids[1].onclick();
  assert.deepEqual(JSON.parse(JSON.stringify(p.posted.filter(([n]) => n === "pick"))), [
    ["pick", { kind: "dlna", control: "http://192.168.0.220/ctl", name: "CustomCars" }],
    ["pick", { kind: "airplay", control: "", name: "Lounge" }],
  ]);
  p.posted.length = 0;
  p.click();
  assert.deepEqual(p.posted.map(([n]) => n), ["stop"]);
});

test("a DLNA playback that has reached the TV can be stopped", () => {
  const p = loadPage();
  p.state({ phase: "ready", mode: "dlna", tv: { pos: 1, paused: false }, dlnaName: "CustomCars" });
  assert.equal(p.label.textContent, "Стоп");
  p.posted.length = 0;
  p.click();
  assert.deepEqual(p.posted.map(([n]) => n), ["stop"]);
});
