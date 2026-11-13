// Provides the window.kopiaUI API that Electron KopiaUI exposes through its
// preload script (app/public/preload.js). The web UI only shows the directory
// picker and "Browse" buttons when this object exists.
(function () {
  if (window.kopiaUI) {
    return;
  }

  window.kopiaUI = {
    selectDirectory: function (onSelected) {
      fetch("/__kopiaui/select-dir", { method: "POST" })
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
      fetch("/__kopiaui/browse-dir", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path: path }),
      }).catch(function (e) {
        console.warn("browseDirectory failed", e);
      });
    },
  };
})();
