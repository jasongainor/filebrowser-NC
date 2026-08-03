/*
 * Legacy browse-only UI — targets Safari 9 (iOS 9.2.1, iPad 3).
 *
 * This file is served verbatim from the Go binary and MUST NOT be routed
 * through Vite. It is ES5 on purpose. Safari 9 lacks all of the following,
 * so none of them may appear here:
 *
 *   let / const          arrow functions      template literals
 *   Promise chaining     fetch()              async / await
 *   Object.assign        default parameters   destructuring
 *   ES modules           Proxy (why the Vue 3 app cannot boot at all)
 *
 * It is also loaded as an external script with no inline handlers, because
 * the server sends `Content-Security-Policy: default-src 'self'` — inline
 * script and onclick= attributes are blocked.
 *
 * Scope is deliberately small: authenticate, walk directories, read text
 * files, download. No upload — iOS 9 predates the Files app and its file
 * input can only reach the photo library, which is useless for NC programs.
 */

(function () {
  "use strict";

  var TOKEN_KEY = "fb_legacy_token";

  // The page is always served at <base>/legacy/, so stripping that suffix
  // yields the BaseURL the server was configured with.
  var root = window.location.pathname.replace(/\/legacy\/?$/, "");
  var apiRoot = root + "/api";

  var token = null;
  var currentDir = "/";

  var el = {
    crumbs: document.getElementById("crumbs"),
    status: document.getElementById("status"),
    list: document.getElementById("list"),
    login: document.getElementById("login"),
    loginForm: document.getElementById("loginform"),
    user: document.getElementById("user"),
    pass: document.getElementById("pass"),
    loginBtn: document.getElementById("loginbtn"),
    logout: document.getElementById("logout"),
    reload: document.getElementById("reload"),
    viewer: document.getElementById("viewer"),
    viewName: document.getElementById("viewname"),
    viewBody: document.getElementById("viewbody"),
    viewDl: document.getElementById("viewdl"),
    viewClose: document.getElementById("viewclose")
  };

  /* ---------- small helpers ---------- */

  function show(node) {
    node.className = node.className.replace(/\s*hidden\s*/g, "");
  }

  function hide(node) {
    if (node.className.indexOf("hidden") === -1) {
      node.className = node.className ? node.className + " hidden" : "hidden";
    }
  }

  function setStatus(msg, isError) {
    el.status.innerHTML = "";
    el.status.className = isError ? "error" : "";
    if (msg) {
      el.status.appendChild(document.createTextNode(msg));
    }
  }

  // Encode each path segment separately so that slashes survive but spaces,
  // "#" and friends do not break the URL. NC program names are full of them.
  function encodePath(p) {
    var parts = p.split("/");
    var out = [];
    for (var i = 0; i < parts.length; i++) {
      out.push(encodeURIComponent(parts[i]));
    }
    return out.join("/");
  }

  function decodePath(p) {
    var parts = p.split("/");
    var out = [];
    for (var i = 0; i < parts.length; i++) {
      out.push(decodeURIComponent(parts[i]));
    }
    return out.join("/");
  }

  function formatSize(bytes) {
    if (bytes < 1024) {
      return bytes + " B";
    }
    var units = ["KB", "MB", "GB", "TB"];
    var n = bytes / 1024;
    var i = 0;
    while (n >= 1024 && i < units.length - 1) {
      n = n / 1024;
      i++;
    }
    return (n < 10 ? n.toFixed(1) : Math.round(n)) + " " + units[i];
  }

  // Safari 9's Date parser handles the RFC3339 stamps the API returns, but
  // toLocaleString output is inconsistent across old iOS builds. Format by
  // hand so the shop floor always sees the same thing.
  function formatDate(iso) {
    var d = new Date(iso);
    if (isNaN(d.getTime())) {
      return "";
    }
    function pad(n) {
      return n < 10 ? "0" + n : "" + n;
    }
    return (
      d.getFullYear() +
      "-" +
      pad(d.getMonth() + 1) +
      "-" +
      pad(d.getDate()) +
      " " +
      pad(d.getHours()) +
      ":" +
      pad(d.getMinutes())
    );
  }

  /* ---------- auth ---------- */

  function saveToken(t) {
    token = t;
    try {
      window.localStorage.setItem(TOKEN_KEY, t);
    } catch (e) {
      // Private browsing on iOS 9 makes localStorage throw on write. The
      // in-memory copy still works for the life of the tab.
    }
    // The download links below are plain navigations that cannot carry an
    // X-Auth header, so the API also accepts this cookie on GET requests.
    document.cookie = "auth=" + t + "; Path=" + (root || "/") + "; SameSite=Strict;";
  }

  function loadToken() {
    try {
      return window.localStorage.getItem(TOKEN_KEY);
    } catch (e) {
      return null;
    }
  }

  function clearToken() {
    token = null;
    try {
      window.localStorage.removeItem(TOKEN_KEY);
    } catch (e) {
      // ignore
    }
    document.cookie = "auth=; Max-Age=0; Path=" + (root || "/") + "; SameSite=Strict;";
  }

  function showLogin(msg) {
    hide(el.viewer);
    hide(el.crumbs);
    el.list.innerHTML = "";
    show(el.login);
    hide(el.logout);
    hide(el.reload);
    setStatus(msg || "", !!msg);
  }

  function showBrowser() {
    hide(el.login);
    show(el.crumbs);
    show(el.logout);
    show(el.reload);
  }

  /* ---------- transport ---------- */

  // cb(err, data). `err` is a string suitable for display; a null body with a
  // null error means the request was rejected and the login screen is already
  // back up.
  //
  // ownAuthErrors skips that automatic bounce, for callers that need to report
  // a rejection themselves — /api/login answers a bad password with 403, which
  // is not an expired session.
  function request(method, url, body, cb, ownAuthErrors) {
    var xhr = new XMLHttpRequest();
    xhr.open(method, url, true);
    if (token) {
      xhr.setRequestHeader("X-Auth", token);
    }
    if (body !== null) {
      xhr.setRequestHeader("Content-Type", "application/json");
    }
    xhr.onreadystatechange = function () {
      if (xhr.readyState !== 4) {
        return;
      }
      if (xhr.status === 0) {
        cb("Cannot reach the server. Check the network.", null);
        return;
      }
      if (xhr.status === 401 || xhr.status === 403) {
        if (ownAuthErrors) {
          cb(null, null);
          return;
        }
        clearToken();
        showLogin("Session expired. Sign in again.");
        cb(null, null);
        return;
      }
      if (xhr.status < 200 || xhr.status >= 300) {
        cb("Server error " + xhr.status + ".", null);
        return;
      }
      cb(null, xhr.responseText);
    };
    xhr.send(body === null ? null : body);
  }

  function requestJSON(method, url, body, cb) {
    request(method, url, body, function (err, text) {
      if (err || text === null) {
        cb(err, null);
        return;
      }
      var parsed;
      try {
        parsed = JSON.parse(text);
      } catch (e) {
        cb("Server sent a malformed response.", null);
        return;
      }
      cb(null, parsed);
    });
  }

  /* ---------- rendering ---------- */

  function renderCrumbs(dir) {
    el.crumbs.innerHTML = "";

    var link = document.createElement("a");
    link.href = "#/";
    link.appendChild(document.createTextNode("Home"));
    el.crumbs.appendChild(link);

    var parts = dir.split("/");
    var acc = "";
    for (var i = 0; i < parts.length; i++) {
      if (parts[i] === "") {
        continue;
      }
      acc = acc + "/" + parts[i];

      var sep = document.createElement("span");
      sep.className = "sep";
      sep.appendChild(document.createTextNode("/"));
      el.crumbs.appendChild(sep);

      var a = document.createElement("a");
      a.href = "#" + encodePath(acc) + "/";
      a.appendChild(document.createTextNode(parts[i]));
      el.crumbs.appendChild(a);
    }
  }

  function renderList(dir, items) {
    el.list.innerHTML = "";

    // Directories first, then files, each alphabetical and case-insensitive.
    // The server sorts by the *account's* saved preference, which an operator
    // never set; a stable order matters more here than honouring it.
    items.sort(function (a, b) {
      if (a.isDir !== b.isDir) {
        return a.isDir ? -1 : 1;
      }
      var an = a.name.toLowerCase();
      var bn = b.name.toLowerCase();
      return an < bn ? -1 : an > bn ? 1 : 0;
    });

    for (var i = 0; i < items.length; i++) {
      var item = items[i];
      var full = (dir === "/" ? "" : dir) + "/" + item.name;

      var li = document.createElement("li");
      li.className = item.isDir ? "dir" : "file";

      var a = document.createElement("a");
      a.href = "#" + encodePath(full) + (item.isDir ? "/" : "");

      var name = document.createElement("span");
      name.className = "nm";
      name.appendChild(document.createTextNode(item.name));
      a.appendChild(name);

      var meta = document.createElement("span");
      meta.className = "meta";
      var metaText = formatDate(item.modified);
      if (!item.isDir) {
        metaText = formatSize(item.size) + (metaText ? " · " + metaText : "");
      }
      meta.appendChild(document.createTextNode(metaText));
      a.appendChild(meta);

      li.appendChild(a);
      el.list.appendChild(li);
    }

    if (items.length === 0) {
      setStatus("This folder is empty.", false);
    }
  }

  /* ---------- navigation ---------- */

  function openDir(dir) {
    currentDir = dir;
    hide(el.viewer);
    show(el.list);
    renderCrumbs(dir);
    setStatus("Loading…", false);

    requestJSON("GET", apiRoot + "/resources" + encodePath(dir), null, function (err, data) {
      if (err) {
        setStatus(err, true);
        return;
      }
      if (!data) {
        return; // 401 already handled
      }
      setStatus("", false);
      renderList(dir, data.items || []);
    });
  }

  function openFile(path) {
    setStatus("Loading…", false);

    requestJSON("GET", apiRoot + "/resources" + encodePath(path), null, function (err, data) {
      if (err) {
        setStatus(err, true);
        return;
      }
      if (!data) {
        return;
      }
      setStatus("", false);

      hide(el.list);
      show(el.viewer);
      el.viewName.innerHTML = "";
      el.viewName.appendChild(document.createTextNode(data.name));
      el.viewDl.href = apiRoot + "/raw" + encodePath(path);

      el.viewBody.innerHTML = "";
      if (data.type === "text") {
        // `content` is omitted for an empty file rather than sent as "".
        var body = typeof data.content === "string" ? data.content : "";
        el.viewBody.appendChild(document.createTextNode(body));
      } else {
        el.viewBody.appendChild(
          document.createTextNode(
            "No preview for this file type (" +
              (data.type || "unknown") +
              ", " +
              formatSize(data.size) +
              ").\nUse Download to open it."
          )
        );
      }
    });
  }

  function route() {
    if (!token) {
      showLogin(null);
      return;
    }
    showBrowser();

    var hash = window.location.hash.replace(/^#/, "");
    if (hash === "") {
      hash = "/";
    }
    var path = decodePath(hash);

    // A trailing slash marks a directory; everything else is a file. The
    // listing writes both forms, so this never has to guess.
    if (path === "/" || path.charAt(path.length - 1) === "/") {
      var dir = path === "/" ? "/" : path.substring(0, path.length - 1);
      openDir(dir);
    } else {
      openFile(path);
    }
  }

  /* ---------- events ---------- */

  el.loginForm.onsubmit = function (e) {
    if (e && e.preventDefault) {
      e.preventDefault();
    }
    var creds = JSON.stringify({
      username: el.user.value,
      password: el.pass.value,
      recaptcha: ""
    });
    setStatus("Signing in…", false);
    el.loginBtn.disabled = true;

    // Not requestJSON: /api/login answers with the bare JWT as text/plain.
    request("POST", apiRoot + "/login", creds, function (err, text) {
      el.loginBtn.disabled = false;
      if (err) {
        setStatus(err, true);
        return;
      }
      if (text === null) {
        setStatus("Wrong user name or password.", true);
        return;
      }
      el.pass.value = "";
      saveToken(text);
      showBrowser();
      route();
    }, true);
    return false;
  };

  el.logout.onclick = function () {
    clearToken();
    window.location.hash = "";
    showLogin(null);
  };

  el.reload.onclick = function () {
    route();
  };

  el.viewClose.onclick = function () {
    window.location.hash = encodePath(currentDir === "/" ? "/" : currentDir + "/");
    if (currentDir === "/") {
      // Setting the same hash fires no event; re-route by hand.
      route();
    }
  };

  // onhashchange exists in Safari 9, so the iPad's back gesture works.
  window.onhashchange = route;

  /* ---------- boot ---------- */

  token = loadToken();
  route();
})();
