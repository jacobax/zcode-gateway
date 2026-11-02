// 设置页「读回来」检查：saveSettings 里提交的每个控件，loadSettings 里必须
// 有一行把它还原。否则刷新后控件停在默认值，用户再点保存就把后端覆盖了。
//
// 这已经是第二次踩：claimEgress 只写了保存、没写还原，面板一直显示「仅直连」。
const fs = require("fs");

const path = process.argv[2] || "web/app.js";
const src = fs.readFileSync(path, "utf8");

function section(name, nextName) {
  const start = src.indexOf(`async function ${name}(`);
  if (start < 0) return "";
  const end = nextName ? src.indexOf(`async function ${nextName}(`, start + 1) : -1;
  return src.slice(start, end > 0 ? end : undefined);
}

const save = section("saveSettings", "loadProxies");
const load = section("loadSettings", "loadUpdate");
if (!save || !load) {
  console.error("找不到 saveSettings / loadSettings");
  process.exit(2);
}

// 保存：body 字面量里 key: $('id').xxx
const savedIds = new Set();
for (const m of save.matchAll(/:\s*\$\('([^']+)'\)/g)) savedIds.add(m[1]);

// 还原：$('id').value = / .checked = / .textContent = / .placeholder =
const loadedIds = new Set();
for (const m of load.matchAll(/\$\('([^']+)'\)\.(?:value|checked|textContent|placeholder)\s*=/g)) {
  loadedIds.add(m[1]);
}

const missing = [...savedIds].filter((id) => !loadedIds.has(id));
console.log(`保存的控件 ${savedIds.size} 个，还原的 ${loadedIds.size} 个`);
if (missing.length) {
  console.error("!! 这些控件保存了但从不还原（刷新后显示默认值，再保存会覆盖后端）:");
  for (const id of missing) console.error("   -", id);
  process.exit(1);
}
console.log("设置页读写对齐 OK");
