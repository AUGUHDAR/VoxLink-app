/* ============================================================
 * VoxLink Desktop — links.js
 * 相关链接卡片（点击通过 OpenInBrowser 打开）
 * ============================================================ */
"use strict";

(function (VL) {
  var LINKS = [
    {
      name: "GitHub 项目",
      desc: "VoxLink 的源码仓库，点个 Star 支持一下 qwq",
      url: "https://github.com/AUGUHDAR/VoxLink"
    },
    {
      name: "MC 百科",
      desc: "模组介绍、使用教程与常见问题",
      url: "https://www.mcmod.cn/class/28295.html"
    },
    {
      name: "CurseForge",
      desc: "在 CurseForge 下载 VoxLink 模组",
      url: "https://www.curseforge.com/minecraft/mc-mods/voxlink"
    },
    {
      name: "Modrinth",
      desc: "在 Modrinth 下载 VoxLink 模组",
      url: "https://modrinth.com/mod/voxlink"
    }
  ];

  VL.initLinks = function () {
    var grid = VL.$("#links-grid");
    if (!grid) return;
    grid.innerHTML = LINKS.map(function (l, i) {
      return (
        '<div class="card card-hoverable link-card">' +
        '  <div class="link-name"><span class="ico">' + VL.iconSvg("link") + "</span>" + VL.esc(l.name) + "</div>" +
        '  <div class="link-desc">' + VL.esc(l.desc) + "</div>" +
        '  <div class="link-url">' + VL.esc(l.url) + "</div>" +
        '  <button class="btn btn-ghost btn-sm" data-url="' + VL.esc(l.url) + '" data-open="' + i + '">' +
        '    <span class="ico">' + VL.iconSvg("external") + "</span>打开" +
        "  </button>" +
        "</div>"
      );
    }).join("");

    grid.addEventListener("click", function (ev) {
      var btn = ev.target.closest("[data-url]");
      if (!btn) return;
      var url = btn.getAttribute("data-url");
      // 统一走后端浏览器打开，不在网页内跳转
      VL.api("OpenInBrowser", url).catch(function (e) {
        if (e.code !== "NO_BACKEND") VL.toast(VL.errText(e), "error");
      });
    });
  };
})(window.VL);
