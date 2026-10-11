/* ============================================================
 * VoxLink Desktop — settings.js
 * 设置：主题 / 玩家中继开关 / 加入前必装模组检查 / 版本号
 * ============================================================ */
"use strict";

(function (VL) {
  var FALLBACK_VERSION = "1.2.0-beta";
  var els = {};

  function cacheEls() {
    els.theme = VL.$("#set-theme");
    els.relay = VL.$("#set-relay");
    els.msJoin = VL.$("#set-ms-join");
    els.save = VL.$("#set-save");
    els.error = VL.$("#settings-error");
    els.version = VL.$("#settings-version");
  }

  // 「加入前检查必装 Mod」存在 modsync.json（不是 settings.json），要单独读。
  // SaveModSyncSettings 是整体回写，所以这里只把开关取回来显示，写的时候再读一次现状。
  function loadModSyncJoin() {
    if (!els.msJoin) return Promise.resolve();
    return VL.api("GetModSyncSettings").then(function (v) {
      els.msJoin.checked = !(v && v.joinRequiredModsCheck === false);
    }, function () { /* 后端不可用时保持默认勾选，不打扰玩家 */ });
  }

  async function load() {
    try {
      var data = await VL.api("GetSettings");
      data = data || {};
      // 版本号从后端读取；后端未返回时兜底显示当前版本
      els.version.textContent = "v" + (data.version || FALLBACK_VERSION);
      VL.themes.current = VL.applyTheme(data.theme || "light");
      els.theme.value = VL.themes.current;
      // relayEnabled 缺省按模组的默认值 true（VoxLinkConfig.java:41）
      els.relay.checked = data.relayEnabled !== false;
    } catch (e) {
      els.version.textContent = "v" + FALLBACK_VERSION;
    }
  }

  // SaveSettings 保存主题与玩家中继开关（应用端不暴露服务器地址等参数）
  async function save() {
    els.error.classList.add("hidden");
    els.save.disabled = true;
    try {
      await VL.api("SaveSettings", JSON.stringify({
        theme: VL.themes.current,
        relayEnabled: !!els.relay.checked
      }));
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

  async function changeRelay() {
    try {
      await VL.api("SaveSettings", JSON.stringify({ relayEnabled: !!els.relay.checked }));
    } catch (e) {
      if (e.code !== "NO_BACKEND") {
        els.error.textContent = VL.errText(e);
        els.error.classList.remove("hidden");
      }
      els.relay.checked = !els.relay.checked; // 保存失败就退回原状，别让界面骗人
    }
  }

  // 改动即时保存；失败就退回原状，别让界面骗人。
  // 写之前重新读一次现状：这个文件里还有实例清单和房主侧开关，少带字段等于把它们抹掉。
  async function changeModSyncJoin() {
    var before = els.msJoin.checked;
    try {
      var cur = await VL.api("GetModSyncSettings") || {};
      await VL.api("SaveModSyncSettings", JSON.stringify({
        instances: cur.instances || [],
        activeInstance: cur.activeInstance || "",
        joinRequiredModsCheck: !!before,
        hostModSyncPublish: !(cur.hostModSyncPublish === false)
      }));
    } catch (e) {
      if (e.code !== "NO_BACKEND") {
        els.error.textContent = VL.errText(e);
        els.error.classList.remove("hidden");
      }
      els.msJoin.checked = !before;
    }
  }

  VL.initSettings = function () {
    cacheEls();
    els.save.addEventListener("click", save);
    els.theme.addEventListener("change", changeTheme);
    els.relay.addEventListener("change", changeRelay);
    if (els.msJoin) els.msJoin.addEventListener("change", changeModSyncJoin);
    load().then(loadModSyncJoin);
    // 设置页每次被打开时重新取一遍：建房页也会写同一份 modsync.json，只看一次会显示旧值
    VL.refreshSettings = loadModSyncJoin;
  };
})(window.VL);