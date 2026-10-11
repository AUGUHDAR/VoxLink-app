/* ============================================================
 * VoxLink Desktop — links.js
 * 相关链接卡片（点击通过 OpenInBrowser 打开）
 * ============================================================ */
"use strict";

(function (VL) {
  // 前 8 条逐条对齐模组 RelatedLinksScreen.java 的 LINKS 表（顺序与 URL 都取原文，
  // QQ 群那条连 authKey 参数也照抄：那是二维码绑定的固定值，不是会话令牌）。
  // 后 2 条是本应用端自己的仓库：模组仓库和应用端仓库不是一回事，玩家在两者之间
  // 反馈问题时需要能一眼分清，所以名称里带「模组」/「应用端」。
  var LINKS = [
    {
      name: "MC百科",
      desc: "模组介绍、使用教程与常见问题",
      url: "https://www.mcmod.cn/class/28295.html"
    },
    {
      name: "GitHub（模组）",
      desc: "VoxLink 模组的源码仓库，点个 Star 支持一下 qwq",
      url: "https://github.com/AUGUHDAR/VoxLink"
    },
    {
      name: "Gitee（模组）",
      desc: "模组源码仓库的国内镜像，克隆更快",
      url: "https://gitee.com/AUGUHDAR/VoxLink"
    },
    {
      name: "Modrinth",
      desc: "在 Modrinth 下载 VoxLink 模组",
      url: "https://modrinth.com/mod/voxlink"
    },
    {
      name: "CurseForge",
      desc: "在 CurseForge 下载 VoxLink 模组",
      url: "https://www.curseforge.com/minecraft/mc-mods/voxlink"
    },
    {
      name: "官网",
      desc: "VoxLink 官方网站，下载与说明都在上面",
      url: "https://p2p.wuhui.icu/"
    },
    {
      name: "Discord",
      desc: "加入 Discord 服务器，反馈与更新通知都在这里",
      url: "https://discord.gg/XaAFxvzPDS"
    },
    {
      name: "QQ群",
      desc: "进 QQ 群一起联机、报问题、提建议",
      url: "https://qm.qq.com/cgi-bin/qm/qr?k=OEkk9L8m8jdFMkbGDhKZs0u2U0azLAPo&jump_from=webapi&authKey=v0dYAQniGZypAJuoPZW/7FL0bfoc32h68oIHd9lqGwOvAduzcwsJNR7Mei9/YugW"
    },
    {
      name: "GitHub（应用端）",
      desc: "VoxLink 桌面应用端的源码仓库（就是这个程序本身）",
      url: "https://github.com/AUGUHDAR/VoxLink-app"
    },
    {
      name: "Gitee（应用端）",
      desc: "应用端源码仓库的国内镜像",
      url: "https://gitee.com/AUGUHDAR/VoxLink-app"
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
