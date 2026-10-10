/* ============================================================
 * VoxLink Desktop — settings.js
 * 设置：仅主题切换 / 版本号（应用端只暴露主题）
 * ============================================================ */
"use strict";

(function (VL) {
  var FALLBACK_VERSION = "1.1.9-beta";
  var els = {};

  function cacheEls() {
    els.theme = VL.$("#set-theme");
    els.save = VL.$("#set-save");
    els.error = VL.$("#settings-error");
    els.version = VL.$("#settings-version");
  }

  async function load() {
    try {
      var data = await VL.api("GetSettings");
      data = data || {};
      // 版本号从后端读取；后端未返回时兜底显示当前版本
      els.version.textContent = "v" + (data.version || FALLBACK_VERSION);
      VL.themes.current = VL.applyTheme(data.theme || "light");
      els.theme.value = VL.themes.current;
    } catch (e) {
      els.version.textContent = "v" + FALLBACK_VERSION;
    }
  }

  // SaveSettings 只保存 theme（应用端不暴露服务器地址等参数）
  async function save() {
    els.error.classList.add("hidden");
    els.save.disabled = true;
    try {
      await VL.api("SaveSettings", JSON.stringify({ theme: VL.themes.current }));
      VL.toast("已保存", "success");
    } catch (e) {
      if (e.code !== "NO_BACKEND") {
        els.error.textContent = VL.errText(e);
        els.error.classList.remove("hidden");
      }
    }
    els.save.disabled = false;
  }

  async function changeTheme() {
    var t = VL.applyTheme(els.theme.value);
    VL.themes.current = t;
    try {
      await VL.api("SaveSettings", JSON.stringify({ theme: t }));
    } catch (e) { /* 主题即时生效，持久化失败不弹错误 */ }
  }

  VL.initSettings = function () {
    cacheEls();
    els.save.addEventListener("click", save);
    els.theme.addEventListener("change", changeTheme);
    load();
  };
})(window.VL);