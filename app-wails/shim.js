// Provides the window.kopiaUI API that Electron KopiaUI exposes through its
// preload script (app/public/preload.js). The web UI only shows the directory
// picker and "Browse" buttons when this object exists.
//
// It also sends links to other sites to the system browser, as Electron
// KopiaUI's window-open handler does, so that no other site is loaded in the
// repository window.
(function () {
  if (window.kopiaUI) {
    return;
  }

  function post(action, body) {
    return fetch("/__kopiaui/" + action, {
      method: "POST",
      headers: body ? { "Content-Type": "application/json" } : {},
      body: body ? JSON.stringify(body) : undefined,
    });
  }

  window.kopiaUI = {
    selectDirectory: function (onSelected) {
      post("select-dir")
        .then(function (r) {
          return r.json();
        })
        .then(function (v) {
          if (v && v.path) {
            onSelected(v.path);
          }
        })
        .catch(function (e) {
          console.warn("selectDirectory failed", e);
        });
    },
    browseDirectory: function (path) {
      post("browse-dir", { path: path }).catch(function (e) {
        console.warn("browseDirectory failed", e);
      });
    },
  };

  // Returns the absolute URL if it is an http(s) page of another site.
  function externalURL(href) {
    try {
      var u = new URL(href, window.location.href);

      if ((u.protocol === "http:" || u.protocol === "https:") && u.origin !== window.location.origin) {
        return u.href;
      }
    } catch (e) {
      // not a URL
    }

    return null;
  }

  function openExternal(url) {
    post("open-url", { url: url }).catch(function (e) {
      console.warn("opening " + url + " failed", e);
    });
  }

  // Capture phase, so that the link is handled before the web UI's own handlers.
  document.addEventListener(
    "click",
    function (ev) {
      var a = ev.target && ev.target.closest ? ev.target.closest("a[href]") : null;
      var url = a ? externalURL(a.getAttribute("href")) : null;

      if (url) {
        ev.preventDefault();
        ev.stopPropagation();
        openExternal(url);
      }
    },
    true
  );

  var originalOpen = window.open;

  window.open = function (href) {
    var url = href ? externalURL(String(href)) : null;

    if (url) {
      openExternal(url);
      return null;
    }

    return originalOpen.apply(window, arguments);
  };
})();
