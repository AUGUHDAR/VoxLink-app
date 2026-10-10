/* ============================================================
 * VoxLink Desktop — join.js
 * 加入房间：6 格房间代码输入 / 密码 / RoomInfo 探测 / JoinRoom
 * ============================================================ */
"use strict";

(function (VL) {
  var CODE_CHARS = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
  var els = {};
  var probing = false;
  var probedHasPassword = false;

  function cacheEls() {
    els.inputs = VL.$$("#code-inputs input");
    els.pwField = VL.$("#join-pw-field");
    els.pw = VL.$("#join-pw");
    els.btn = VL.$("#join-btn");
    els.error = VL.$("#join-error");
    els.preview = VL.$("#join-preview");
    els.previewName = VL.$("#join-preview-name");
    els.previewLock = VL.$("#join-preview-lock");
  }

  function getCode() {
    return els.inputs.map(function (i) { return i.value.toUpperCase(); }).join("");
  }

  function isFull() {
    return getCode().length === 6;
  }

  function clearError() {
    els.error.classList.add("hidden");
    els.error.textContent = "";
    els.inputs.forEach(function (i) { i.classList.remove("shake"); });
  }

  function showError(msg) {
    els.error.textContent = msg;
    els.error.classList.remove("hidden");
    els.inputs.forEach(function (i) {
      i.classList.add("shake");
      setTimeout(function () { i.classList.remove("shake"); }, 400);
    });
  }

  function updateFilledStyle() {
    els.inputs.forEach(function (i) { i.classList.toggle("filled", !!i.value); });
  }

  function sanitize(v) {
    return String(v || "").toUpperCase().replace(/[^A-Z2-9]/g, "");
  }

  /* ---------------- RoomInfo 探测 ---------------- */
  async function probe() {
    var code = getCode();
    if (code.length < 6) {
      els.preview.classList.add("hidden");
      els.pwField.classList.add("hidden");
      probedHasPassword = false;
      return;
    }
    if (probing) return;
    probing = true;
    try {
      var data = await VL.api("RoomInfo", code);
      probedHasPassword = !!(data && data.hasPassword);
      els.previewName.textContent = (data && data.name) || "未命名房间";
      els.previewLock.hidden = !probedHasPassword;
      els.preview.classList.remove("hidden");
      els.pwField.classList.toggle("hidden", !probedHasPassword);
      els.error.classList.add("hidden");
      if (probedHasPassword) els.pw.focus();
    } catch (e) {
      probedHasPassword = false;
      els.preview.classList.add("hidden");
      els.pwField.classList.add("hidden");
      if (e.code === "ROOM_NOT_FOUND_OR_WRONG_PASSWORD" || e.code === "ROOM_NOT_FOUND") {
        showError("找不到这个房间 qwq 检查一下代码？");
      } else if (e.code !== "NO_BACKEND") {
        showError(VL.errText(e));
      }
    }
    probing = false;
  }
  var probeDebounced = VL.debounce(probe, 350);

  /* ---------------- 加入 ---------------- */
  async function join() {
    clearError();
    var code = getCode();
    if (code.length < 6) {
      showError("要先输入完整的 6 位房间代码哦");
      els.inputs[els.inputs.map(function (i) { return i.value ? 0 : 1; }).indexOf(1)].focus();
      return;
    }
    var password = els.pw.value;
    setBusy(true);
    try {
      var data = await VL.api("JoinRoom", JSON.stringify({ code: code, password: password }));
      VL.toast("加入成功，欢迎 awa", "success");
      if (VL.onJoined) VL.onJoined(data);
    } catch (e) {
      if (e.code === "ROOM_NOT_FOUND_OR_WRONG_PASSWORD") {
        showError("找不到这个房间 qwq 或密码不对");
        els.pwField.classList.remove("hidden");
        els.pw.classList.add("shake");
        setTimeout(function () { els.pw.classList.remove("shake"); }, 400);
        els.pw.focus();
      } else if (e.code === "ROOM_FULL") {
        showError("房间满了，稍后再来吧 :(");
      } else if (e.code !== "NO_BACKEND") {
        showError(VL.errText(e));
      }
    }
    setBusy(false);
  }

  function setBusy(b) {
    els.btn.disabled = b;
    var label = els.btn.querySelector("span:last-child");
    if (label) label.textContent = b ? "正在加入…" : "加入房间";
  }

  /* ---------------- 输入交互 ---------------- */
  function bindInputs() {
    els.inputs.forEach(function (input, idx) {
      input.addEventListener("input", function () {
        // 粘贴整段代码时自动分配到各格
        var raw = input.value;
        if (raw.length > 1) {
          var seq = sanitize(raw);
          var start = 0;
          for (var k = 0; k < seq.length && idx + k < 6; k++) {
            els.inputs[idx + k].value = seq[k];
            start = idx + k;
          }
          var next = Math.min(start + 1, 5);
          els.inputs[next].focus();
          if (els.inputs[next].value) els.inputs[next].select();
        } else {
          input.value = sanitize(raw).slice(0, 1);
          if (input.value && idx < 5) els.inputs[idx + 1].focus();
        }
        updateFilledStyle();
        clearError();
        probeDebounced();
        if (isFull() && !probedHasPassword) els.pw.focus();
      });

      input.addEventListener("keydown", function (ev) {
        if (ev.key === "Backspace" && !input.value && idx > 0) {
          els.inputs[idx - 1].focus();
          els.inputs[idx - 1].value = "";
          updateFilledStyle();
          ev.preventDefault();
        } else if (ev.key === "Enter") {
          join();
        } else if (ev.key === "ArrowLeft" && idx > 0) {
          els.inputs[idx - 1].focus();
        } else if (ev.key === "ArrowRight" && idx < 5) {
          els.inputs[idx + 1].focus();
        }
      });

      input.addEventListener("paste", function (ev) {
        ev.preventDefault();
        var text = sanitize((ev.clipboardData || window.clipboardData).getData("text"));
        if (!text) return;
        for (var k = 0; k < 6; k++) {
          els.inputs[k].value = text[k] || "";
        }
        var last = Math.min(text.length, 6) - 1;
        els.inputs[Math.max(0, last)].focus();
        updateFilledStyle();
        clearError();
        probeDebounced();
        if (isFull()) els.pw.focus();
      });
    });

    els.pw.addEventListener("keydown", function (ev) {
      if (ev.key === "Enter") join();
    });
    els.pw.addEventListener("input", clearError);
    els.btn.addEventListener("click", join);
  }

  /* ---------------- 初始化 ---------------- */
  VL.initJoin = function () {
    cacheEls();
    bindInputs();
    updateFilledStyle();
  };

  // 从大厅“加入”跳转时预填代码（预留）
  VL.joinPrefill = function (code) {
    var seq = sanitize(code);
    for (var k = 0; k < 6; k++) els.inputs[k].value = seq[k] || "";
    updateFilledStyle();
    if (seq.length === 6) probeDebounced();
  };
})(window.VL);
