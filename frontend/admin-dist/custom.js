/* ============================================================
   custom.js — 管理面板增强：侧边栏注入「修改密码」入口 + 对话框
   原版 Settings 页也有改密码（/config/save 的 password 字段），
   这里提供一个更显眼的常驻入口。样式全部走 custom.css 的 sp-* 类。
   ============================================================ */
(function () {
  "use strict";
  var SCOPE = "data-v-efd8fa61"; // App.vue 的 scoped 属性，复用 .nav-item 样式必须带上

  /* ---------- fetch 拦截：/api/admin/* 的 ?pwd= 迁移到 X-Admin-Pwd 请求头 ----------
     管理密码不再出现在 URL（不进 nginx access log / history / Referer）。
     拦截层对原版 SPA 透明：它照旧拼 ?pwd=，线上传输已被改造。 */
  var _origFetch = window.fetch;
  if (_origFetch) {
    window.fetch = function (input, init) {
      try {
        var url = typeof input === "string" ? input : (input && input.url) || "";
        if (url.indexOf("/api/admin/") !== -1 && url.indexOf("pwd=") !== -1) {
          var m = url.match(/[?&]pwd=([^&]*)/);
          if (m) {
            // 只摘掉 pwd 参数本身，绝不能把分隔符一起吃掉：
            // 旧实现 replace(/[?&]pwd=[^&]*/g,"") 会把
            //   /api/admin/logs?pwd=x&type=error  ->  /api/admin/logs&type=error
            // 后端解析出的 action 变成 "logs&type=error"，落到 405，
            // 于是「运行日志/错误日志」页永远是空的。
            var clean = url.replace(/([?&])pwd=[^&]*(&?)/, function (_all, sep, amp) {
              if (sep === "?") return amp === "&" ? "?" : ""; // ?pwd=x&type -> ?type
              return amp === "&" ? "&" : "";                   // &pwd=x&b   -> &b
            });
            if (typeof input === "string") {
              input = clean;
            } else if (typeof Request === "function" && input instanceof Request) {
              // 保留 method/body/headers，只换 URL（旧实现用 Object.assign 造出的
              // 普通对象会丢掉 body，变成 GET）
              try { input = new Request(clean, input); } catch (e) { input = clean; }
            } else {
              input = Object.assign({}, input, { url: clean });
            }
            init = init || {};
            init.headers = Object.assign({}, init.headers || {});
            init.headers["X-Admin-Pwd"] = decodeURIComponent(m[1]);
          }
        }
      } catch (e) { /* 拦截失败退回原请求 */ }
      return _origFetch.call(window, input, init);
    };
  }

  function pwd() {
    return sessionStorage.getItem("admin_pwd") || "";
  }

  /* ---------- 侧边栏注入 ---------- */
  function mount() {
    var footer = document.querySelector(".sidebar-footer");
    if (!footer || document.getElementById("sp-pwd-btn")) return;
    var btn = document.createElement("button");
    btn.id = "sp-pwd-btn";
    btn.type = "button";
    btn.className = "nav-item";
    btn.setAttribute(SCOPE, "");
    btn.innerHTML =
      '<span class="nav-icon" ' + SCOPE + '="">' +
      '<svg viewBox="0 0 24 24" width="18" height="18"><path fill="currentColor" d="M12 17a2 2 0 1 0 0-4 2 2 0 0 0 0 4Zm6-9h-1V6a5 5 0 0 0-10 0v2H6a2 2 0 0 0-2 2v10a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V10a2 2 0 0 0-2-2ZM9 6a3 3 0 0 1 6 0v2H9V6Z"/></svg>' +
      "</span>" +
      '<span class="nav-label" ' + SCOPE + '="">修改密码</span>';
    btn.addEventListener("click", openDialog);
    footer.insertBefore(btn, footer.firstChild);

    // 添加节点（独立 token + 一键安装命令）
    var addBtn = document.createElement("button");
    addBtn.id = "sp-addnode-btn";
    addBtn.type = "button";
    addBtn.className = "nav-item";
    addBtn.setAttribute(SCOPE, "");
    addBtn.innerHTML =
      '<span class="nav-icon" ' + SCOPE + '="">' +
      '<svg viewBox="0 0 24 24" width="18" height="18"><path fill="currentColor" d="M19 13h-6v6h-2v-6H5v-2h6V5h2v6h6v2Z"/></svg>' +
      "</span>" +
      '<span class="nav-label" ' + SCOPE + '="">添加节点</span>';
    addBtn.addEventListener("click", openNodeDialog);
    footer.insertBefore(addBtn, footer.firstChild);
  }

  /* ---------- 添加节点对话框 ----------
     只要两个输入框：地区 + 运营商（都由使用者自己填，中文随便写）。
     节点 id 由中心随机生成，显示名自动拼成「地区+运营商」（成都 + 腾讯云 → 成都腾讯云）。 */
  function openNodeDialog() {
    if (document.getElementById("sp-novl")) return;
    var ovl = document.createElement("div");
    ovl.id = "sp-novl";
    ovl.className = "sp-ovl";
    ovl.innerHTML =
      '<div class="sp-dlg">' +
      '<div class="sp-head">添加监控节点</div>' +
      '<div class="sp-body">' +
      '<label class="sp-label">地区</label>' +
      '<input id="sp-nreg" type="text" class="sp-inp" placeholder="例如 成都（也可以写 四川成都）" maxlength="24" autocomplete="off">' +
      '<label class="sp-label">运营商</label>' +
      '<input id="sp-ncar" type="text" class="sp-inp" placeholder="例如 电信 / 联通 / 移动 / 腾讯云" maxlength="24" autocomplete="off">' +
      '<div class="sp-tip">节点 id 由中心随机生成（形如 n-7f3a9c21b40e），显示名自动拼成「地区+运营商」。</div>' +
      '<div id="sp-nerr" class="sp-err" style="display:none"></div>' +
      '<div id="sp-ncmd-wrap" style="display:none">' +
      '<div id="sp-nres" class="sp-tip"></div>' +
      '<label class="sp-label">一键安装命令（复制到目标服务器以 root 执行）</label>' +
      '<textarea id="sp-ncmd" class="sp-inp" rows="3" readonly style="resize:vertical;font-size:12px"></textarea>' +
      '<div class="sp-tip">自动下载二进制、登记身份公钥、写配置与服务并启动。命令只含一次性安装码（30 分钟有效，节点首次上线即作废），不含任何长期密钥。</div>' +
      "</div>" +
      "</div>" +
      '<div class="sp-foot">' +
      '<button id="sp-nclose" class="sp-btn sp-btn-sec" type="button">关闭</button>' +
      '<button id="sp-ncopy" class="sp-btn sp-btn-sec" type="button" style="display:none">复制命令</button>' +
      '<button id="sp-ngen" class="sp-btn sp-btn-pri" type="button">生成安装命令</button>' +
      "</div>" +
      "</div>";
    document.body.appendChild(ovl);

    var region = ovl.querySelector("#sp-nreg");
    var carrier = ovl.querySelector("#sp-ncar");
    var err = ovl.querySelector("#sp-nerr");
    var wrap = ovl.querySelector("#sp-ncmd-wrap");
    var res = ovl.querySelector("#sp-nres");
    var cmdBox = ovl.querySelector("#sp-ncmd");
    var gen = ovl.querySelector("#sp-ngen");
    var copy = ovl.querySelector("#sp-ncopy");

    function showErr(m) {
      err.textContent = m;
      err.style.display = "block";
    }
    function close() {
      if (ovl.parentNode) ovl.parentNode.removeChild(ovl);
    }
    ovl.querySelector("#sp-nclose").addEventListener("click", close);
    ovl.addEventListener("click", function (e) {
      if (e.target === ovl) close();
    });
    copy.addEventListener("click", function () {
      cmdBox.select();
      try {
        document.execCommand("copy");
        copy.textContent = "已复制";
        setTimeout(function () {
          copy.textContent = "复制命令";
        }, 1500);
      } catch (e) {}
    });
    gen.addEventListener("click", function () {
      err.style.display = "none";
      var reg = region.value.trim();
      var car = carrier.value.trim();
      if (!reg) return showErr("请填写地区");
      if (!car) return showErr("请填写运营商");
      gen.disabled = true;
      gen.textContent = "生成中…";
      fetch("/api/admin/node/add?pwd=" + encodeURIComponent(pwd()), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        // 地区按 city 字段提交（province 留空即不做省份白名单校验）；不带 name = 中心随机生成 id
        body: JSON.stringify({ city: reg, telecom: car })
      })
        .then(function (r) {
          if (r.status === 401) throw new Error("登录已过期，请重新登录");
          return r.json();
        })
        .then(function (d) {
          gen.disabled = false;
          gen.textContent = "生成安装命令";
          if (d && d.error) throw new Error(d.error);
          cmdBox.value = d.install_cmd;
          res.textContent =
            "节点 id：" + (d.name || "") +
            "　显示名：" + (d.label || "（未设置，显示节点 id）") +
            (d.code_expire ? "　安装码有效期至：" + d.code_expire : "");
          wrap.style.display = "block";
          copy.style.display = "inline-block";
        })
        .catch(function (e) {
          gen.disabled = false;
          gen.textContent = "生成安装命令";
          showErr(e.message || "生成失败");
        });
    });
    setTimeout(function () {
      region.focus();
    }, 60);
  }

  /* ---------- 对话框 ---------- */
  function openDialog() {
    if (document.getElementById("sp-ovl")) return;
    var ovl = document.createElement("div");
    ovl.id = "sp-ovl";
    ovl.className = "sp-ovl";
    ovl.innerHTML =
      '<div class="sp-dlg">' +
      '<div class="sp-head">修改管理密码</div>' +
      '<div class="sp-body">' +
      '<label class="sp-label">新密码</label>' +
      '<input id="sp-p1" type="password" class="sp-inp" autocomplete="new-password" placeholder="建议 8 位以上，含字母与数字">' +
      '<label class="sp-label">确认新密码</label>' +
      '<input id="sp-p2" type="password" class="sp-inp" autocomplete="new-password" placeholder="再输入一次">' +
      '<div class="sp-tip">修改成功后会写回服务器配置并立即生效，需用新密码重新登录。</div>' +
      '<div id="sp-err" class="sp-err" style="display:none"></div>' +
      "</div>" +
      '<div class="sp-foot">' +
      '<button id="sp-cancel" class="sp-btn sp-btn-sec" type="button">取消</button>' +
      '<button id="sp-ok" class="sp-btn sp-btn-pri" type="button">确认修改</button>' +
      "</div>" +
      "</div>";
    document.body.appendChild(ovl);

    var p1 = ovl.querySelector("#sp-p1");
    var p2 = ovl.querySelector("#sp-p2");
    var err = ovl.querySelector("#sp-err");
    var ok = ovl.querySelector("#sp-ok");
    function showErr(m) {
      err.textContent = m;
      err.style.display = "block";
    }
    function onKey(e) {
      if (e.key === "Escape") close();
    }
    function close() {
      // 取消/点遮罩关闭时也要摘掉键盘监听：旧实现只在按 Esc 时摘，
      // 每开一次对话框就永久留下一个监听器。
      document.removeEventListener("keydown", onKey);
      if (ovl.parentNode) ovl.parentNode.removeChild(ovl);
    }
    ovl.querySelector("#sp-cancel").addEventListener("click", close);
    ovl.addEventListener("click", function (e) {
      if (e.target === ovl) close();
    });
    document.addEventListener("keydown", onKey);

    ok.addEventListener("click", function () {
      err.style.display = "none";
      var a = p1.value;
      var b = p2.value;
      if (!a || a.length < 6) return showErr("密码至少 6 位");
      if (a !== b) return showErr("两次输入的密码不一致");
      ok.disabled = true;
      ok.textContent = "提交中…";
      fetch("/api/admin/config/save?pwd=" + encodeURIComponent(pwd()), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ password: a }),
      })
        .then(function (r) {
          if (r.status === 401) throw new Error("登录已过期，请重新登录后再修改");
          return r.json();
        })
        .then(function (d) {
          if (d && d.error) throw new Error(d.error);
          close();
          alert("密码已更新，请使用新密码重新登录");
          sessionStorage.removeItem("admin_pwd");
          location.reload();
        })
        .catch(function (e) {
          ok.disabled = false;
          ok.textContent = "确认修改";
          showErr(e.message || "修改失败");
        });
    });
    setTimeout(function () {
      p1.focus();
    }, 60);
  }

  /* ---------- 节点管理：给每张节点卡片补一行归属信息 ----------
     原版 NodeMgmt 组件只渲染 名称/地址/在线状态（编译后的 SPA，无源码），
     地区与运营商是我们新增的字段，所以在这里读 /api/admin/node/status 补上去。 */

  // nodeGeoText 把接口返回的一条节点信息拼成一行可读文本（纯函数，便于单测）
  function nodeGeoText(n) {
    if (!n) return "";
    if (n.auth === "center") return "中心服务（与探测节点同机部署）";
    var parts = [];
    parts.push("显示名 " + (n.display || n.name || "—"));
    parts.push("地区 " + (n.city || n.province || "未设置"));
    parts.push("运营商 " + (n.telecom || "未设置"));
    if (n.pubkey_fp) {
      parts.push("身份 " + (n.auth === "ed25519" ? "Ed25519" : n.auth) + "（指纹 " + n.pubkey_fp + "）");
    } else {
      parts.push("身份 " + (n.auth || "hmac"));
    }
    return parts.join("　·　");
  }

  // escHtml 本地转义（custom.js 里没有全局 esc；节点名是 ASCII 白名单，
  // 但显示名可由使用者输入，凡是拼进 HTML 的一律先转义）
  function escHtml(s) {
    return String(s == null ? "" : s)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
  }

  // 编辑弹窗：显示名 / 地区 / 运营商 / 前台是否展示
  function openNodeEditDialog(node) {
    if (document.getElementById("sp-neovl")) return;
    var ovl = document.createElement("div");
    ovl.id = "sp-neovl";
    ovl.className = "sp-ovl";
    ovl.innerHTML =
      '<div class="sp-dlg">' +
      '<div class="sp-head">编辑节点</div>' +
      '<div class="sp-body">' +
      '<div class="sp-tip">节点 id（' + escHtml(node.name) + '）是历史数据的主键，不能改；' +
      "想换 id 请用「添加节点」建新节点。这里只改展示信息，不影响采集。</div>" +
      '<label class="sp-label">显示名（前台显示的名字）</label>' +
      '<input id="sp-ne-label" class="sp-inp" maxlength="48" placeholder="例如 成都腾讯云-机房A">' +
      '<div class="sp-grid2">' +
      '<div><label class="sp-label">地区</label>' +
      '<input id="sp-ne-city" class="sp-inp" maxlength="24" placeholder="例如 成都"></div>' +
      '<div><label class="sp-label">运营商</label>' +
      '<input id="sp-ne-tel" class="sp-inp" maxlength="24" placeholder="例如 电信 / 腾讯云"></div>' +
      "</div>" +
      '<label class="sp-check"><input id="sp-ne-vis" type="checkbox"> 在前台（首页）展示这个节点</label>' +
      '<div class="sp-tip">取消勾选只是"不上首页"：节点照常探测、上报、入库，随时可以再打开。</div>' +
      '<div id="sp-neerr" class="sp-err" style="display:none"></div>' +
      "</div>" +
      '<div class="sp-foot">' +
      '<button id="sp-necancel" class="sp-btn sp-btn-sec" type="button">取消</button>' +
      '<button id="sp-neok" class="sp-btn sp-btn-pri" type="button">保存</button>' +
      "</div>" +
      "</div>";
    document.body.appendChild(ovl);

    var label = ovl.querySelector("#sp-ne-label");
    var city = ovl.querySelector("#sp-ne-city");
    var tel = ovl.querySelector("#sp-ne-tel");
    var vis = ovl.querySelector("#sp-ne-vis");
    var err = ovl.querySelector("#sp-neerr");
    var ok = ovl.querySelector("#sp-neok");
    label.value = node.display || "";
    city.value = node.city || "";
    tel.value = node.telecom || "";
    vis.checked = node.visible !== false;

    function showErr(m) {
      err.textContent = m;
      err.style.display = "block";
    }
    function close() {
      if (ovl.parentNode) ovl.parentNode.removeChild(ovl);
    }
    ovl.querySelector("#sp-necancel").addEventListener("click", close);
    ovl.addEventListener("click", function (e) {
      if (e.target === ovl) close();
    });
    ok.addEventListener("click", function () {
      err.style.display = "none";
      ok.disabled = true;
      ok.textContent = "保存中…";
      fetch("/api/admin/node/edit?pwd=" + encodeURIComponent(pwd()), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          name: node.name,
          label: label.value.trim(),
          city: city.value.trim(),
          telecom: tel.value.trim(),
          visible: !!vis.checked
        })
      })
        .then(function (r) {
          if (r.status === 401) throw new Error("登录已过期，请重新登录");
          return r.json();
        })
        .then(function (d) {
          ok.disabled = false;
          ok.textContent = "保存";
          if (d && d.error) throw new Error(d.error);
          node.display = label.value.trim();
          node.city = city.value.trim();
          node.telecom = tel.value.trim();
          node.visible = !!vis.checked;
          close();
          enhanceNodeCards();
          alert("已保存。" + (vis.checked ? "" : "该节点已从首页隐藏（仍在照常采集）。") +
                "首页可能有缓存，刷新一下即可看到。");
        })
        .catch(function (e) {
          ok.disabled = false;
          ok.textContent = "保存";
          showErr(e.message || "保存失败");
        });
    });
    setTimeout(function () {
      label.focus();
    }, 60);
  }

  var geoTimer = null;
  function enhanceNodeCards() {
    var cards = document.querySelectorAll(".node-card");
    if (!cards.length) return;
    fetch("/api/admin/node/status?pwd=" + encodeURIComponent(pwd()))
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) {
        if (!d || !d.nodes) return;
        var list = d.nodes;
        Array.prototype.forEach.call(document.querySelectorAll(".node-card"), function (card) {
          var nameEl = card.querySelector(".node-name");
          if (!nameEl) return;
          var text = (nameEl.textContent || "").trim();
          var hit = null;
          for (var i = 0; i < list.length; i++) {
            var nm = list[i].name || "";
            if (nm && text.indexOf(nm) === 0) { hit = list[i]; break; }
          }
          if (!hit) return;
          var el = card.querySelector("[data-sp-geo]");
          if (!el) {
            el = document.createElement("div");
            el.className = "sp-node-geo";
            el.setAttribute("data-sp-geo", "1");
            var main = card.querySelector(".node-main") || card;
            main.appendChild(el);
          }
          // 文本单独放一个 span：这样后面追加按钮时不会被 textContent 冲掉
          var span = el.querySelector(".sp-geo-text");
          if (!span) {
            span = document.createElement("span");
            span.className = "sp-geo-text";
            el.appendChild(span);
          }
          var want = nodeGeoText(hit);
          if (span.textContent !== want) span.textContent = want;
          // 编辑按钮只建一次；节点名不会变，闭包安全
          var btn = el.querySelector("[data-sp-geo-edit]");
          if (!btn) {
            btn = document.createElement("button");
            btn.type = "button";
            btn.className = "sp-geo-edit";
            btn.setAttribute("data-sp-geo-edit", "1");
            btn.textContent = "编辑";
            btn.addEventListener("click", function () { openNodeEditDialog(hit); });
            el.appendChild(btn);
          }
        });
      })
      .catch(function () { /* 拉不到就什么都不做，不影响原页面 */ });
  }

  // SPA 会重渲染（点"刷新"、切页面），用观察者跟着补，节流 150ms
  function watchNodePage() {
    var content = document.querySelector(".content");
    if (!content || content.dataset.spGeoWatch) return;
    content.dataset.spGeoWatch = "1";
    new MutationObserver(function () {
      if (geoTimer) return;
      geoTimer = setTimeout(function () {
        geoTimer = null;
        enhanceNodeCards();
      }, 150);
    }).observe(content, { childList: true, subtree: true });
  }

  /* ---------- 页面切换动画：路由切换时给新页面根元素加进入动画 ---------- */
  function pageTransition() {
    var content = document.querySelector(".content");
    if (!content || content.dataset.spAnim) return;
    content.dataset.spAnim = "1";
    new MutationObserver(function (muts) {
      muts.forEach(function (m) {
        Array.prototype.forEach.call(m.addedNodes, function (n) {
          if (n.nodeType === 1 && !n.classList.contains("sp-page-in")) {
            n.classList.add("sp-page-in");
          }
        });
      });
    }).observe(content, { childList: true });
  }

  /* Vue 渲染是异步的：MutationObserver 兜底等 sidebar-footer 出现 */
  function boot() {
    mount();
    pageTransition();
    watchNodePage();
    enhanceNodeCards();
    if (!document.querySelector(".sidebar-footer") || !document.querySelector(".content")) {
      new MutationObserver(function (m, obs) {
        mount();
        pageTransition();
        if (document.querySelector(".sidebar-footer") && document.querySelector(".content")) {
          obs.disconnect();
        }
      }).observe(document.body, { childList: true, subtree: true });
    }
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
})();
