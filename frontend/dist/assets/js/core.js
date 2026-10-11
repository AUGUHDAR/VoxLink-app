/* ============================================================
 * VoxLink Desktop — core.js
 * api 封装 / toast / 图标 / 通用工具
 * ============================================================ */
"use strict";

window.VL = window.VL || {};

(function (VL) {
  /* ---------------- 图标（内联 SVG，stroke: currentColor） ---------------- */
  var ICONS = {
    home: '<path d="M3 11l9-8 9 8"/><path d="M5 9.5V20a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V9.5"/>',
    join: '<rect x="3" y="4" width="18" height="16" rx="3"/><path d="M8 12h8m0 0-3.5-3.5M16 12l-3.5 3.5"/>',
    create: '<circle cx="12" cy="12" r="9"/><path d="M12 8v8M8 12h8"/>',
    room: '<rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/>',
    link: '<path d="M10 14a5 5 0 0 0 7.07 0l1.9-1.9a5 5 0 0 0-7.07-7.07l-1.06 1.06"/><path d="M14 10a5 5 0 0 0-7.07 0l-1.9 1.9a5 5 0 0 0 7.07 7.07l1.06-1.06"/>',
    settings: '<path d="M4 7h16M4 12h16M4 17h16"/><circle cx="9" cy="7" r="2" fill="var(--bg-elev)"/><circle cx="15" cy="12" r="2" fill="var(--bg-elev)"/><circle cx="7" cy="17" r="2" fill="var(--bg-elev)"/>',
    refresh: '<path d="M20 12a8 8 0 1 1-2.34-5.66"/><path d="M20 4v4.5h-4.5"/>',
    search: '<circle cx="11" cy="11" r="7"/><path d="m20.5 20.5-4.2-4.2"/>',
    copy: '<rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h2"/>',
    users: '<circle cx="9" cy="8" r="3.5"/><path d="M2.5 20c.8-3.3 3.4-5 6.5-5s5.7 1.7 6.5 5"/><circle cx="17" cy="9" r="2.5"/><path d="M16.2 15.3c2.4.4 4.4 1.9 5.1 4.7"/>',
    lock: '<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/>',
    signal: '<path d="M4.9 19.1a10 10 0 0 1 0-14.2M19.1 4.9a10 10 0 0 1 0 14.2M7.8 16.2a6 6 0 0 1 0-8.4M16.2 7.8a6 6 0 0 1 0 8.4"/><circle cx="12" cy="12" r="1.5"/>',
    log: '<path d="m5 7 4 5-4 5"/><path d="M12 17h7"/>',
    mods: '<rect x="3" y="3" width="8" height="8" rx="1.5"/><rect x="13" y="3" width="8" height="8" rx="1.5"/><rect x="3" y="13" width="8" height="8" rx="1.5"/><path d="M17 13.5v7M13.5 17h7"/>',
    relay: '<circle cx="12" cy="18" r="2"/><path d="M12 16v-6"/><path d="M8.5 5.5a6 6 0 0 1 7 0M7 9a9 9 0 0 1 10 0"/>',
    leave: '<path d="M15 4h4a1 1 0 0 1 1 1v14a1 1 0 0 1-1 1h-4"/><path d="M10 8l-4 4 4 4M6 12h10"/>',
    ticket: '<path d="M3 9V6.5A1.5 1.5 0 0 1 4.5 5h15A1.5 1.5 0 0 1 21 6.5V9a2.2 2.2 0 0 0 0 6v2.5a1.5 1.5 0 0 1-1.5 1.5h-15A1.5 1.5 0 0 1 3 17.5V15a2.2 2.2 0 0 0 0-6Z"/><path d="M13 8v8" stroke-dasharray="2 2"/>',
    external: '<path d="M14 4h6v6"/><path d="M20 4 11 13"/><path d="M19 13.5V19a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1h5.5"/>',
    check: '<path d="m5 12.5 4.5 4.5L19 7.5"/>'
  };

  VL.iconSvg = function (name) {
    var p = ICONS[name] || ICONS.link;
    return (
      '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" ' +
      'stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + p + "</svg>"
    );
  };

  VL.hydrateIcons = function (root) {
    var nodes = (root || document).querySelectorAll("[data-ico]");
    for (var i = 0; i < nodes.length; i++) {
      nodes[i].innerHTML = VL.iconSvg(nodes[i].getAttribute("data-ico"));
    }
  };

  /* ---------------- DOM / 字符串工具 ---------------- */
  VL.$ = function (sel, root) { return (root || document).querySelector(sel); };
  VL.$$ = function (sel, root) {
    return Array.prototype.slice.call((root || document).querySelectorAll(sel));
  };

  VL.esc = function (s) {
    if (s === undefined || s === null) return "";
    return String(s)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  };

  // 目录选择：任何带 data-browse-dir 的按钮，点击后把选中的路径写回它前面那个输入框，
  // 并补发一次 change，让既有的联动（校验/回写）照常跑。后端没有对话框能力时返回空串，
  // 此时什么都不改，玩家填的原值保持不动。
  document.addEventListener("click", function (ev) {
    var btn = ev.target && ev.target.closest ? ev.target.closest("[data-browse-dir]") : null;
    if (!btn) return;
    ev.preventDefault();
    var input = btn.previousElementSibling;
    if (!input || input.tagName !== "INPUT") return;
    VL.api("SelectModsDir").then(function (res) {
      var path = (res && res.path) || "";
      if (!path) return;
      input.value = path;
      input.dispatchEvent(new Event("change", { bubbles: true }));
      input.dispatchEvent(new Event("input", { bubbles: true }));
    }, function () { /* 无后端/无对话框能力：静默，别弹错误打扰玩家 */ });
  });

  VL.fmtSize = function (bytes) {
    var n = Number(bytes);
    if (!isFinite(n) || n <= 0) return "-";
    if (n < 1024) return n + " B";
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
    return (n / 1024 / 1024).toFixed(2) + " MB";
  };

  VL.relTime = function (ts) {
    var t = Number(ts);
    if (!isFinite(t) || t <= 0) return "";
    var diff = Math.max(0, Date.now() - t);
    if (diff < 10 * 1000) return "刚刚";
    if (diff < 60 * 1000) return Math.floor(diff / 1000) + " 秒前";
    if (diff < 60 * 60 * 1000) return Math.floor(diff / 60000) + " 分钟前";
    if (diff < 24 * 60 * 60 * 1000) return Math.floor(diff / 3600000) + " 小时前";
    return Math.floor(diff / 86400000) + " 天前";
  };

  VL.debounce = function (fn, wait) {
    var timer = null;
    return function () {
      var args = arguments, self = this;
      clearTimeout(timer);
      timer = setTimeout(function () { fn.apply(self, args); }, wait);
    };
  };

  /* ---------------- 后端调用封装 ---------------- */
  // window.go.main.App.X(参数) 全部返回 Promise<string>(JSON)。
  // 统一解析 {ok:true,data} / {ok:false,error,message}。
  VL.api = async function (name) {
    var bridge = window.go && window.go.main && window.go.main.App;
    if (!bridge || typeof bridge[name] !== "function") {
      var err = new Error("请在桌面 App 中运行");
      err.code = "NO_BACKEND";
      VL.toast(err.message, "error");
      throw err;
    }
    var args = Array.prototype.slice.call(arguments, 1);
    var raw;
    try {
      raw = await bridge[name].apply(bridge, args);
    } catch (e) {
      var callErr = new Error((e && e.message) || "调用后端失败");
      callErr.code = "BRIDGE_ERROR";
      throw callErr;
    }
    var res;
    try {
      res = JSON.parse(raw);
    } catch (e) {
      return raw; // 非 JSON（理论上不会发生），原样返回
    }
    if (res && res.ok === false) {
      var apiErr = new Error(res.message || res.error || "请求失败");
      apiErr.code = res.error || "UNKNOWN";
      throw apiErr;
    }
    return res && res.ok === true ? res.data : res;
  };

  // 订阅后端事件（无 runtime 时静默忽略）
  VL.on = function (event, cb) {
    if (window.runtime && typeof window.runtime.EventsOn === "function") {
      window.runtime.EventsOn(event, cb);
    }
  };

  /* ---------------- 错误文案 ---------------- */
  var ERROR_TEXT = {
    RATE_LIMITED: "请求太快啦，休息一下再试 qwq",
    ROOM_NOT_FOUND_OR_WRONG_PASSWORD: "找不到这个房间 qwq 或密码不对",
    ROOM_FULL: "房间满了，稍后再来吧 :(",
    NO_BACKEND: "请在桌面 App 中运行",
    NETWORK_ERROR: "连不上服务器，检查一下网络或服务器地址吧"
  };

  VL.errText = function (err) {
    if (!err) return "出了点小问题";
    if (err.code && ERROR_TEXT[err.code]) return ERROR_TEXT[err.code];
    return err.message || "出了点小问题";
  };

  /* ---------------- Toast ---------------- */
  var toastBox = null;
  var lastToast = { msg: "", t: 0 };

  VL.toast = function (msg, type) {
    if (!toastBox) toastBox = document.getElementById("toast-box");
    if (!toastBox) return;
    // 2 秒内相同消息去重，避免无后端时重复刷屏
    var now = Date.now();
    if (msg === lastToast.msg && now - lastToast.t < 2000) return;
    lastToast = { msg: msg, t: now };

    var el = document.createElement("div");
    el.className = "toast" + (type ? " toast-" + type : "");
    el.textContent = msg;
    toastBox.appendChild(el);
    setTimeout(function () {
      el.classList.add("out");
      setTimeout(function () { el.remove(); }, 300);
    }, 2600);
  };

  /* ---------------- 剪贴板 ---------------- */
  VL.copy = async function (text) {
    var ok = false;
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        await navigator.clipboard.writeText(text);
        ok = true;
      }
    } catch (e) { /* 走降级 */ }
    if (!ok) {
      try {
        var ta = document.createElement("textarea");
        ta.value = text;
        ta.style.position = "fixed";
        ta.style.opacity = "0";
        document.body.appendChild(ta);
        ta.select();
        ok = document.execCommand("copy");
        ta.remove();
      } catch (e) { ok = false; }
    }
    VL.toast(ok ? "已复制 " + text : "复制失败了 qwq", ok ? "success" : "error");
    return ok;
  };

  /* ---------------- 主题 ---------------- */
  VL.applyTheme = function (theme) {
    var t = theme === "dark" ? "dark" : "light";
    document.documentElement.setAttribute("data-theme", t);
    return t;
  };

  VL.themes = { current: "light" };
})(window.VL);
