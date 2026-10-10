/* ============================================================
 * VoxLink Desktop — ticket.js
 * 工单：提交 / 我的工单 / 详情与追问
 *
 * 归属凭证（ticketSecret）由 Go 侧保管并随请求回传，前端只拿单号与内容：
 * 服务端只存凭证哈希，缺凭证的查询会被 403 挡下。
 * ============================================================ */
"use strict";

(function (VL) {
  var els = {};
  var current = null;
  var newFiles = [];
  var replyFiles = [];
  var deleteArmed = false;

  var ERR_TEXT = {
    TICKET_FORBIDDEN: "这台电脑没有这条工单的查询凭证（换过设备或重装过），所以打不开它",
    TICKET_NOT_FOUND: "找不到这条工单",
    TICKET_EMPTY: "写点什么，或者附上日志吧",
    INVALID_FILE: "附件读不到，重新选一次试试"
  };

  function errText(e) {
    if (e && e.code && ERR_TEXT[e.code]) return ERR_TEXT[e.code];
    return VL.errText(e);
  }

  function setErr(el, msg) {
    if (!el) return;
    el.textContent = msg || "";
    el.classList.toggle("hidden", !msg);
  }

  function show(view) {
    ["list", "new", "detail"].forEach(function (name) {
      var box = VL.$("#ticket-view-" + name);
      if (box) box.classList.toggle("hidden", name !== view);
    });
  }

  function fmtSize(n) {
    var v = Number(n) || 0;
    if (v < 1024) return v + " B";
    if (v < 1048576) return (v / 1024).toFixed(1) + " KB";
    return (v / 1048576).toFixed(1) + " MB";
  }

  function attLine(list) {
    if (!list || !list.length) return "";
    var parts = [];
    for (var i = 0; i < list.length; i++) {
      parts.push(VL.esc(list[i].name || "附件") + " · " + fmtSize(list[i].size));
    }
    return '<div class="ticket-msg-meta">附件：' + parts.join("　") + "</div>";
  }

  function msgRow(m, isPlayer) {
    var who = isPlayer ? "我" : "官方";
    var when = m.timeMs ? " · " + VL.relTime(m.timeMs) : "";
    // 自己发的那条才给撤回入口（撤回是删数据，点了要立刻锁住按钮）
    var act = isPlayer && m.id
      ? '<div class="ticket-msg-acts"><button type="button" class="ticket-msg-retract" data-mid="' +
        VL.esc(m.id) + '">撤回</button></div>'
      : "";
    return '<div class="ticket-msg' + (isPlayer ? "" : " ticket-msg-admin") + '">' +
      '<div class="ticket-msg-meta">' + who + when + "</div>" +
      '<div class="ticket-msg-text">' + VL.esc(m.text || "") + "</div>" +
      attLine(m.attachments) + act + "</div>";
  }

  function renderList(data) {
    var tickets = (data && data.tickets) || [];
    VL.updateTicketBadge(data && data.unread);
    if (!tickets.length) {
      els.list.innerHTML = "";
      els.empty.classList.remove("hidden");
      return;
    }
    els.empty.classList.add("hidden");
    els.list.innerHTML = tickets.map(function (t) {
      var badge = t.hasUnread
        ? '<span class="pill pill-warning">有新回复</span>'
        : (t.replyCount ? '<span class="pill pill-accent">' + t.replyCount + " 条回复</span>" : "");
      return '<div class="card ticket-item" data-tid="' + VL.esc(t.id) + '">' +
        '<div class="ticket-item-main">' +
        '<div class="ticket-item-id">' + VL.esc(t.id) + "</div>" +
        '<div class="ticket-item-sub">' + VL.relTime(t.lastTimeMs || t.timeMs) + "</div>" +
        "</div>" + badge + "</div>";
    }).join("");
  }

  function renderFiles(box, files, onChange) {
    box.innerHTML = files.map(function (p, i) {
      var name = String(p).split(/[\\/]/).pop();
      return '<span class="chip">' + VL.esc(name) +
        ' <button type="button" data-rm="' + i + '" title="移除">&times;</button></span>';
    }).join("");
    box.onclick = function (ev) {
      var btn = ev.target.closest("[data-rm]");
      if (!btn) return;
      files.splice(Number(btn.getAttribute("data-rm")), 1);
      onChange(files);
    };
  }

  async function refresh(poll) {
    try {
      renderList(await VL.api(poll ? "TicketPoll" : "TicketList"));
    } catch (e) {
      if (e.code !== "NO_BACKEND") renderList(null);
    }
  }

  async function openDetail(id) {
    current = id;
    replyFiles = [];
    renderFiles(els.replyFiles, replyFiles, function (f) { replyFiles = f; });
    setErr(els.replyError, "");
    disarmDelete();
    show("detail");
    els.thread.innerHTML = '<div class="ticket-msg-meta">加载中…</div>';
    try {
      var d = await VL.api("TicketDetail", id);
      renderDetail(d || {});
      refresh(false);
    } catch (e) {
      els.thread.innerHTML = '<div class="ticket-msg"><div class="ticket-msg-text">' +
        VL.esc(errText(e)) + "</div></div>";
    }
  }

  function renderDetail(d) {
    var rows = [];
    rows.push('<div class="ticket-msg"><div class="ticket-msg-meta">我 · ' +
      VL.relTime((d.time || 0) * 1000) + "</div>" +
      '<div class="ticket-msg-text">' + VL.esc(d.description || "") + "</div>" +
      attLine(d.attachments) + "</div>");
    var msgs = d.messages || [];
    for (var i = 0; i < msgs.length; i++) {
      rows.push(msgRow({
        id: msgs[i].id,
        text: msgs[i].text,
        timeMs: (msgs[i].time || 0) * 1000,
        attachments: msgs[i].attachments
      }, msgs[i].from !== "admin"));
    }
    if (!msgs.length) {
      rows.push('<div class="ticket-msg-meta">还没有回复，官方回来看这里 (￣▽￣)ゞ</div>');
    }
    els.thread.innerHTML = rows.join("");
  }

  function disarmDelete() {
    deleteArmed = false;
    if (els.del) {
      els.del.textContent = "删除工单";
      els.del.classList.remove("armed");
    }
  }

  async function pickFiles(box, files, setter) {
    try {
      var res = await VL.api("TicketPickFiles");
      var got = (res && res.files) || [];
      for (var i = 0; i < got.length; i++) {
        if (files.indexOf(got[i]) < 0) files.push(got[i]);
      }
      setter(files);
      renderFiles(box, files, setter);
    } catch (e) {
      if (e.code !== "NO_BACKEND" && e.code !== "DIALOG") VL.toast(errText(e), "error");
    }
  }

  VL.updateTicketBadge = function (unread) {
    var n = Number(unread) || 0;
    if (!els.badge) els.badge = VL.$("#ticket-badge");
    if (!els.badge) return;
    els.badge.textContent = n > 99 ? "99+" : String(n);
    els.badge.classList.toggle("hidden", n <= 0);
  };

  VL.initTicket = function () {
    els.list = VL.$("#ticket-list");
    els.empty = VL.$("#ticket-empty");
    els.badge = VL.$("#ticket-badge");
    els.thread = VL.$("#ticket-thread");
    els.desc = VL.$("#ticket-desc");
    els.newError = VL.$("#ticket-new-error");
    els.newFiles = VL.$("#ticket-new-files");
    els.replyText = VL.$("#ticket-reply-text");
    els.replyError = VL.$("#ticket-reply-error");
    els.replyFiles = VL.$("#ticket-reply-files");
    els.del = VL.$("#ticket-delete");
    els.send = VL.$("#ticket-reply-send");
    els.submit = VL.$("#ticket-submit");

    show("list");
    refresh(true);

    VL.$("#ticket-refresh").addEventListener("click", function () { refresh(true); });
    VL.$("#ticket-new").addEventListener("click", function () {
      setErr(els.newError, "");
      show("new");
      els.desc.focus();
    });
    VL.$("#ticket-new-cancel").addEventListener("click", function () { show("list"); });

    VL.$("#ticket-pick").addEventListener("click", function () {
      pickFiles(els.newFiles, newFiles, function (f) { newFiles = f; });
    });

    els.submit.addEventListener("click", async function () {
      var text = (els.desc.value || "").trim();
      if (!text && !newFiles.length) {
        setErr(els.newError, ERR_TEXT.TICKET_EMPTY);
        return;
      }
      els.submit.disabled = true;
      setErr(els.newError, "");
      try {
        var res = await VL.api("TicketSubmit", JSON.stringify({ description: text, files: newFiles }));
        newFiles = [];
        renderFiles(els.newFiles, newFiles, function (f) { newFiles = f; });
        els.desc.value = "";
        VL.toast("工单已提交：" + ((res && res.id) || ""), "success");
        if (res && res.id) openDetail(res.id); else show("list");
      } catch (e) {
        setErr(els.newError, errText(e));
      }
      els.submit.disabled = false;
    });

    els.list.addEventListener("click", function (ev) {
      var item = ev.target.closest("[data-tid]");
      if (item) openDetail(item.getAttribute("data-tid"));
    });

    VL.$("#ticket-detail-back").addEventListener("click", function () { show("list"); });

    // 撤回：线程是 innerHTML 重绘的，所以事件委托挂在容器上。
    // 点一次立刻禁用，免得慢响应下连点两下——那正是后台回复重复发送的同一个毛病。
    els.thread.addEventListener("click", async function (ev) {
      var btn = ev.target.closest(".ticket-msg-retract");
      if (!btn || !current) return;
      if (!window.confirm("撤回这条追问？撤回后会从会话中移除。")) return;
      var mid = btn.getAttribute("data-mid");
      btn.disabled = true;
      try {
        await VL.api("TicketRetract", JSON.stringify({ id: current, msg: mid }));
        VL.toast("已撤回", "success");
        openDetail(current);
      } catch (e) {
        btn.disabled = false;
        VL.toast(errText(e), "error");
      }
    });
    VL.$("#ticket-reply-pick").addEventListener("click", function () {
      pickFiles(els.replyFiles, replyFiles, function (f) { replyFiles = f; });
    });

    els.send.addEventListener("click", async function () {
      if (!current) return;
      var text = (els.replyText.value || "").trim();
      if (!text && !replyFiles.length) {
        setErr(els.replyError, ERR_TEXT.TICKET_EMPTY);
        return;
      }
      els.send.disabled = true;
      setErr(els.replyError, "");
      try {
        await VL.api("TicketReply", JSON.stringify({ id: current, text: text, files: replyFiles }));
        replyFiles = [];
        renderFiles(els.replyFiles, replyFiles, function (f) { replyFiles = f; });
        els.replyText.value = "";
        VL.toast("追问已发送", "success");
        openDetail(current);
      } catch (e) {
        setErr(els.replyError, errText(e));
      }
      els.send.disabled = false;
    });

    // 删除是两步确认：第一次点亮"再点一次确认"，避免误触丢单号
    els.del.addEventListener("click", async function () {
      if (!current) return;
      if (!deleteArmed) {
        deleteArmed = true;
        els.del.textContent = "再点一次确认删除";
        els.del.classList.add("armed");
        return;
      }
      els.del.disabled = true;
      try {
        await VL.api("TicketDelete", current);
        VL.toast("已删除", "success");
        disarmDelete();
        show("list");
        refresh(false);
      } catch (e) {
        setErr(els.replyError, errText(e));
        disarmDelete();
      }
      els.del.disabled = false;
    });
  };

  // 启动时静默轮询一次，把未读数点亮到侧栏角标
  VL.ticketBoot = function () { refresh(true); };
})(window.VL);
