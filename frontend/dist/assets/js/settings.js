/* ============================================================
 * VoxLink Desktop — settings.js
 * 设置：主题 / 玩家中继开关 / 加入前必装模组检查 / MC 实例管理 / 版本号
 * ============================================================ */
"use strict";

(function (VL) {
  var FALLBACK_VERSION = "1.2.0-beta";
  // 加载器就给这三个，跟「创建房间」页的新建实例下拉保持一致
  var LOADERS = ["fabric", "forge", "neoforge"];
  var els = {};
  var msView = null;   // GetModSyncSettings 的回包，界面和写回都以它为准
  var msBusy = false;  // 一次只让一个写回去跑，连点会互相盖掉
  var armTimer = null; // 「删除」的二次确认，几秒不点就自己缩回去

  function cacheEls() {
    els.theme = VL.$("#set-theme");
    els.relay = VL.$("#set-relay");
    els.msJoin = VL.$("#set-ms-join");
    els.save = VL.$("#set-save");
    els.error = VL.$("#settings-error");
    els.version = VL.$("#settings-version");
    // MC 实例管理
    els.list = VL.$("#set-msi-list");
    els.empty = VL.$("#set-msi-empty");
    els.count = VL.$("#set-msi-count");
    els.msError = VL.$("#set-msi-error");
    els.msForm = VL.$("#set-msi-form");
    els.msNew = VL.$("#set-msi-new");
    els.msRefresh = VL.$("#set-msi-refresh");
    els.msName = VL.$("#set-msi-name");
    els.msLoader = VL.$("#set-msi-loader");
    els.msDir = VL.$("#set-msi-dir");
    els.msGv = VL.$("#set-msi-gv");
    els.msSaveNew = VL.$("#set-msi-save-new");
    els.msCancelNew = VL.$("#set-msi-cancel-new");
    els.msSave = VL.$("#set-msi-save");
  }

  // 「加入前检查必装 Mod」和实例清单存在同一份 modsync.json（不是 settings.json），
  // 一次 GetModSyncSettings 把两边都刷回来。
  // SaveModSyncSettings 是整体回写，所以这里只取回来显示，写的时候再读一次现状。
  function loadModSync() {
    return VL.api("GetModSyncSettings").then(function (v) {
      msView = v || null;
      if (els.msJoin) els.msJoin.checked = !(msView && msView.joinRequiredModsCheck === false);
      renderInstances();
    }, function () { /* 后端不可用时保持默认勾选、列表留空，不打扰玩家 */ });
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

  /* ---------------- MC 实例管理 ----------------
   * 实例以前只能在「创建房间」页就地新建/删除，建好了就改不动。这里给每个实例
   * 摊开四个可以改的输入框：名字 / mods 目录 / 加载器 / MC 版本，外加删除和设为当前。
   * 存的是同一份 modsync.json，所以每次写回都必须带上完整的 instances /
   * activeInstance / 两个开关 —— 一律走 writeMs()，别处不要直接调 SaveModSyncSettings。 */

  function showMsError(msg) {
    if (!els.msError) return;
    els.msError.textContent = msg;
    els.msError.classList.remove("hidden");
  }

  function clearMsError() {
    if (!els.msError) return;
    els.msError.textContent = "";
    els.msError.classList.add("hidden");
  }

  function setMsBusy(b) {
    msBusy = b;
    [els.msSave, els.msNew, els.msSaveNew, els.msRefresh].forEach(function (el) {
      if (el) el.disabled = b;
    });
  }

  function loaderOptions(current) {
    var html = "";
    LOADERS.forEach(function (l) {
      html += '<option value="' + l + '"' + (l === current ? " selected" : "") + ">" + l + "</option>";
    });
    return html;
  }

  // 一个实例一行。data-orig 记住它进页面时的名字：activeInstance 是按名字引用的，
  // 改名时要靠这个对上，才知道「正在使用」要不要跟着搬走。
  function rowHtml(it, i, isActive) {
    var idp = "set-msi-i" + i + "-";
    var sub = [it.loader, it.gameVersion].filter(Boolean).join(" · ");
    return '<div class="field msi-item" data-orig="' + VL.esc(it.name) + '">' +
      '<div class="card-head">' +
        '<span class="card-title">实例 ' + (i + 1) + "</span>" +
        (isActive ? '<span class="pill pill-accent">正在使用</span>' : "") +
        '<span class="card-count">' + VL.esc(sub) + "</span>" +
      "</div>" +
      '<div class="field-row">' +
        '<div class="field">' +
          '<label class="field-label" for="' + idp + 'name">实例名</label>' +
          '<input type="text" class="input" id="' + idp + 'name" data-f="name" maxlength="24" ' +
          'autocomplete="off" value="' + VL.esc(it.name) + '">' +
        "</div>" +
        '<div class="field">' +
          '<label class="field-label" for="' + idp + 'loader">加载器</label>' +
          '<select class="select" id="' + idp + 'loader" data-f="loader">' + loaderOptions(it.loader) + "</select>" +
        "</div>" +
      "</div>" +
      '<div class="field">' +
        '<label class="field-label" for="' + idp + 'dir">mods 目录</label>' +
        '<input type="text" class="input" id="' + idp + 'dir" data-f="dir" autocomplete="off" ' +
        'value="' + VL.esc(it.modsDir) + '">' +
        '<button type="button" class="btn btn-ghost btn-sm" data-browse-dir title="从磁盘选目录">浏览</button>' +
      "</div>" +
      '<div class="field-row">' +
        '<div class="field">' +
          '<label class="field-label" for="' + idp + 'gv">MC 版本</label>' +
          '<input type="text" class="input" id="' + idp + 'gv" data-f="gv" maxlength="16" ' +
          'autocomplete="off" value="' + VL.esc(it.gameVersion) + '">' +
        "</div>" +
        '<div class="field ms-actions">' +
          '<button type="button" class="btn btn-ghost btn-sm" data-act="use"' + (isActive ? " disabled" : "") + ">设为当前</button>" +
          '<button type="button" class="btn btn-danger btn-sm" data-act="del">删除</button>' +
        "</div>" +
      "</div>" +
    "</div>";
  }

  function renderInstances() {
    if (!els.list) return;
    var list = (msView && msView.instances) || [];
    var active = (msView && msView.activeInstance) || "";
    clearTimeout(armTimer);
    if (els.count) els.count.textContent = list.length ? list.length + " 个实例" : "还没有实例";
    if (els.empty) els.empty.classList.toggle("hidden", list.length > 0);
    var html = "";
    list.forEach(function (it, i) { html += rowHtml(it, i, !!it.name && it.name === active); });
    els.list.innerHTML = html;
  }

  function fieldValue(row, key) {
    var el = row.querySelector('[data-f="' + key + '"]');
    return el ? String(el.value || "").trim() : "";
  }

  // 把界面上每一行读回来。名字或 mods 目录空着、或者两个实例重名，就先报错不写：
  // 后端遇到空名字/空目录会把那一行直接丢掉，等于玩家只是想改个版本却把实例弄没了。
  function collectRows() {
    var instances = [];
    var used = {};
    var renamed = {};
    var error = "";
    VL.$$(".msi-item", els.list).forEach(function (row, i) {
      var orig = row.getAttribute("data-orig") || "";
      var name = fieldValue(row, "name");
      var dir = fieldValue(row, "dir");
      if (!name) error = error || "第 " + (i + 1) + " 个实例的名字空着了，填一个吧";
      else if (used[name]) error = error || "「" + name + "」这个名字有两个实例在用，改一个再存";
      else if (!dir) error = error || "「" + name + "」的 mods 目录空着，把完整路径贴进来";
      if (name) used[name] = true;
      if (orig && name && orig !== name) renamed[orig] = name;
      instances.push({
        name: name,
        loader: fieldValue(row, "loader") || "fabric",
        gameVersion: fieldValue(row, "gv"),
        modsDir: dir
      });
    });
    // 「正在使用」是按名字找的：被改名的那一行如果正是它，就把新名字接过去，
    // 不然玩家下次开房会发现选的实例悄悄变了一个。
    var active = (msView && msView.activeInstance) || "";
    if (renamed[active]) active = renamed[active];
    if (!instances.length) active = "";
    return { instances: instances, activeInstance: active, error: error };
  }

  // 所有写回都走这里：先 Get 一次现状，只替换 patch 里带来的那部分，
  // 两个布尔开关原样带上（缺省按 true，跟后端一致），再整体写回。
  function writeMs(patch) {
    if (msBusy) return Promise.resolve(null);
    setMsBusy(true);
    clearMsError();
    return VL.api("GetModSyncSettings").then(function (cur) {
      var base = cur || {};
      var payload = {
        instances: base.instances || [],
        activeInstance: base.activeInstance || "",
        joinRequiredModsCheck: !(base.joinRequiredModsCheck === false),
        hostModSyncPublish: !(base.hostModSyncPublish === false)
      };
      Object.keys(patch || {}).forEach(function (k) { payload[k] = patch[k]; });
      return VL.api("SaveModSyncSettings", JSON.stringify(payload));
    }).then(function (v) {
      if (v) msView = v;
      if (els.msJoin) els.msJoin.checked = !(msView && msView.joinRequiredModsCheck === false);
      setMsBusy(false);
      renderInstances(); // 画的就是刚写进去的那份，界面不会骗人
      return v || msView;
    }, function (e) {
      setMsBusy(false);
      // 失败时不重画：玩家已经打进框里的字不能被一次失败抹掉
      if (e && e.code !== "NO_BACKEND") showMsError(VL.errText(e));
      return null;
    });
  }

  // 「保存修改」：改名 / 换 mods 目录 / 换加载器 / 换 MC 版本都靠这个整体写回
  function saveAll() {
    if (msBusy) return;
    var data = collectRows();
    if (data.error) { showMsError(data.error); return; }
    writeMs({ instances: data.instances, activeInstance: data.activeInstance }).then(function (ok) {
      if (ok) VL.toast("实例都存好啦 qwq", "success");
    });
  }

  // 「设为当前」：改的只是 activeInstance，清单本体照界面写回
  function useRow(row) {
    if (msBusy) return;
    var data = collectRows();
    if (data.error) { showMsError(data.error); return; }
    var name = fieldValue(row, "name");
    writeMs({ instances: data.instances, activeInstance: name }).then(function (ok) {
      if (ok) VL.toast("现在帮你同步 Mod 的是「" + name + "」", "success");
    });
  }

  function disarm(btn) {
    clearTimeout(armTimer);
    if (!btn) return;
    btn.classList.remove("armed");
    btn.textContent = "删除";
  }

  // 删除要点两次：第一次变成「真删？再点一次」，几秒不点就缩回去
  function delRow(btn, row) {
    if (msBusy) return;
    if (!btn.classList.contains("armed")) {
      VL.$$(".msi-item .btn-danger.armed", els.list).forEach(disarm);
      btn.classList.add("armed");
      btn.textContent = "真删？再点一次";
      armTimer = setTimeout(function () { disarm(btn); }, 5000);
      return;
    }
    disarm(btn);
    var data = collectRows();
    if (data.error) { showMsError(data.error); return; }
    var name = fieldValue(row, "name");
    var orig = row.getAttribute("data-orig") || "";
    var wasActive = data.activeInstance === name ||
      ((msView && msView.activeInstance) || "") === orig;
    var rest = data.instances.filter(function (it) { return it.name !== name; });
    var patch = { instances: rest };
    // 删掉的正是「正在使用」那个：activeInstance 不能留一个已经不存在的名字。
    // 后端遇到空值会自己指向第一个，这里干脆提前定下来，并在提示里说清楚改成了谁。
    if (wasActive) patch.activeInstance = rest.length ? rest[0].name : "";
    writeMs(patch).then(function (ok) {
      if (!ok) return;
      if (!wasActive) { VL.toast("已删除实例「" + name + "」", "success"); return; }
      VL.toast(rest.length
        ? "已删除正在使用的「" + name + "」，改用「" + rest[0].name + "」啦"
        : "已删除「" + name + "」，实例清空了，创建房间时选「不同步模组」就行", "success");
    });
  }

  function showMsForm(show) {
    if (!els.msForm) return;
    els.msForm.classList.toggle("hidden", !show);
    if (show && els.msName) els.msName.focus();
  }

  // 新建实例：追加到界面读回来的清单后面。要不要变成「正在使用」交给后端：
  // 之前一个实例都没有时它会自动指向第一个，已有实例时不打扰玩家当前选择。
  function saveNewInstance() {
    if (msBusy) return;
    var data = collectRows();
    if (data.error) { showMsError(data.error); return; }
    var name = (els.msName.value || "").trim();
    var dir = (els.msDir.value || "").trim();
    if (!name) { showMsError("给新实例起个名字吧"); return; }
    if (!dir) { showMsError("新实例要填 mods 目录，把完整路径贴进来"); return; }
    if (data.instances.some(function (it) { return it.name === name; })) {
      showMsError("已经有叫「" + name + "」的实例了，换个名字吧");
      return;
    }
    var list = data.instances.concat([{
      name: name,
      loader: els.msLoader.value || "fabric",
      gameVersion: (els.msGv.value || "").trim(),
      modsDir: dir
    }]);
    writeMs({ instances: list, activeInstance: data.activeInstance }).then(function (ok) {
      if (!ok) return;
      showMsForm(false);
      els.msName.value = "";
      els.msDir.value = "";
      els.msGv.value = "";
      els.msLoader.value = "fabric";
      VL.toast("已保存实例「" + name + "」，创建房间时就能在下拉里选它", "success");
    });
  }

  function onListClick(ev) {
    var btn = ev.target.closest("button[data-act]");
    if (!btn) return;
    var row = btn.closest(".msi-item");
    if (!row) return;
    if (btn.getAttribute("data-act") === "use") useRow(row);
    else delRow(btn, row);
  }

  function bindInstances() {
    if (els.list) els.list.addEventListener("click", onListClick);
    if (els.msSave) els.msSave.addEventListener("click", saveAll);
    if (els.msNew) els.msNew.addEventListener("click", function () { clearMsError(); showMsForm(true); });
    if (els.msCancelNew) els.msCancelNew.addEventListener("click", function () { clearMsError(); showMsForm(false); });
    if (els.msSaveNew) els.msSaveNew.addEventListener("click", saveNewInstance);
    if (els.msRefresh) els.msRefresh.addEventListener("click", function () { clearMsError(); loadModSync(); });
    if (els.msName) els.msName.addEventListener("keydown", function (ev) { if (ev.key === "Enter") saveNewInstance(); });
    if (els.msDir) els.msDir.addEventListener("keydown", function (ev) { if (ev.key === "Enter") saveNewInstance(); });
    if (els.msGv) els.msGv.addEventListener("keydown", function (ev) { if (ev.key === "Enter") saveNewInstance(); });
  }

  VL.initSettings = function () {
    cacheEls();
    els.save.addEventListener("click", save);
    els.theme.addEventListener("change", changeTheme);
    els.relay.addEventListener("change", changeRelay);
    if (els.msJoin) els.msJoin.addEventListener("change", changeModSyncJoin);
    bindInstances();
    load().then(loadModSync);
    // 设置页每次被打开时重新取一遍：建房页也会写同一份 modsync.json，只看一次会显示旧值
    // （实例清单跟那个开关在同一份文件里，一次 Get 顺便都刷回来）
    VL.refreshSettings = loadModSync;
  };
})(window.VL);
