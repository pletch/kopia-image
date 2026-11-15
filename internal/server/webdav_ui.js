// Injected into the web UI when the server runs with --webdav-mounts. The UI shows the
// WebDAV URL of a mounted snapshot in a read-only box; this opens the WebDAV page that
// mounts it instead of making the user copy the URL into a new tab.
(function () {
  "use strict";

  var mountButtonText = "Mount as Local Filesystem";
  var openMarker = "data-kopia-webdav-open";

  function snapshotDirOID() {
    var m = location.pathname.match(/^\/snapshots\/dir\/([^/]+)\/?$/);
    return m ? m[1] : null;
  }

  // The tab must be opened while handling the click, or the browser blocks it as a popup.
  document.addEventListener("click", function (e) {
    var button = e.target.closest && e.target.closest("button");
    var oid = snapshotDirOID();

    if (button && oid && button.textContent.trim() === mountButtonText) {
      window.open("/webdav/" + oid + "/", "_blank", "noopener");
    }
  }, true);

  function webdavPath(value) {
    try {
      var u = new URL(value, location.href);
      return u.pathname.indexOf("/webdav/") === 0 ? u.pathname : null;
    } catch (err) {
      return null;
    }
  }

  // Keeps one "Open" link after the copy button of the mounted-path box, and none when the box is gone.
  function syncOpenLinks() {
    var input = document.querySelector("input.mounted-path");
    var path = input && webdavPath(input.value);
    var anchor = input && input.nextElementSibling;

    document.querySelectorAll("a[" + openMarker + "]").forEach(function (a) {
      if (!path || a.previousElementSibling !== anchor) {
        a.remove();
      }
    });

    if (!path || !anchor) {
      return;
    }

    var link = anchor.nextElementSibling;
    if (!link || !link.hasAttribute(openMarker)) {
      link = document.createElement("a");
      link.setAttribute(openMarker, "");
      link.className = "btn btn-sm btn-primary";
      link.target = "_blank";
      link.rel = "noopener";
      link.textContent = "Open";
      link.title = "Open the page that mounts this snapshot as a drive";
      anchor.after(link);
    }

    if (link.getAttribute("href") !== path) {
      link.setAttribute("href", path);
    }
  }

  new MutationObserver(syncOpenLinks).observe(document.documentElement, {
    childList: true,
    subtree: true,
    attributes: true,
    attributeFilter: ["value"],
  });
})();
