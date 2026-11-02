// 前端冒烟：用最小 DOM 桩把 app.js 跑一遍，抓「顶层执行期」抛错。
//
// 为什么需要它：node --check 只查语法，查不出「少一个 } 导致后面的函数被
// 吞进上一个函数体、顶层监听引用时 ReferenceError」这种错。那次的症状是
// 面板右下角一直「连接中 v-」、账号列表空白——因为初始化那几行压根没跑到。
const fs = require("fs");

const path = process.argv[2] || "web/app.js";
const src = fs.readFileSync(path, "utf8");

const stubEl = new Proxy(
  {},
  {
    get(_t, k) {
      switch (k) {
        case "classList":
          return { add() {}, remove() {}, toggle() {}, contains: () => false };
        case "style":
        case "dataset":
        case "options":
          return {};
        case "querySelector":
        case "closest":
          return () => stubEl;
        case "querySelectorAll":
          return () => [];
        case "addEventListener":
        case "removeEventListener":
        case "appendChild":
        case "remove":
        case "focus":
        case "click":
        case "setAttribute":
          return () => {};
        case "value":
        case "textContent":
        case "innerHTML":
        case "placeholder":
        case "src":
        case "href":
          return "";
        case "checked":
        case "disabled":
          return false;
        case "children":
          return [];
        case "parentElement":
          return null;
        default:
          return undefined;
      }
    },
    set: () => true,
  }
);

global.document = {
  getElementById: () => stubEl,
  querySelector: () => stubEl,
  querySelectorAll: () => [],
  addEventListener: () => {},
  createElement: () => stubEl,
  body: stubEl,
  documentElement: stubEl,
};
global.window = {
  location: { href: "", pathname: "/", search: "" },
  addEventListener: () => {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  open: () => null,
  localStorage: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
};
global.localStorage = global.window.localStorage;
global.navigator = global.window.navigator;
global.location = global.window.location;
global.matchMedia = global.window.matchMedia;
global.fetch = () =>
  Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve("{}"), json: () => Promise.resolve({}) });
global.setInterval = () => 0;
global.clearInterval = () => {};
global.setTimeout = () => 0;
global.clearTimeout = () => {};
global.alert = () => {};
global.confirm = () => false;

let failed = false;
try {
  new Function(src)();
} catch (err) {
  failed = true;
  console.error("!! 顶层执行抛错:", err && err.message);
  const m = /:(\d+):(\d+)/.exec((err && err.stack) || "");
  if (m) {
    const line = Number(m[1]);
    const lines = src.split("\n");
    for (let i = Math.max(0, line - 3); i < Math.min(lines.length, line + 2); i++) {
      console.error(`   ${i + 1}| ${lines[i]}`);
    }
  }
}
if (!failed) console.log("app.js 顶层执行通过（DOM 桩）");
process.exit(failed ? 1 : 0);
