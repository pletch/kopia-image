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
    // Starts the scheduled image task as an administrator, after the UAC
    // prompt, for when the repository server is denied starting it. Resolves
    // to "started" or "canceled" (the prompt was declined); rejects with an
    // Error carrying KopiaUI's message.
    runImageTaskElevated: function () {
      return postElevated("run-image-task", {});
    },
    // Schedules images of this computer with this window's repository, as
    // an administrator after the UAC prompt: settings are volumes (drive
    // letters, one per disk), at (HH:MM), days (weekday names, none for
    // every day), checkDay (a weekday or "none"), checkAt, wake and
    // allowOnBattery. Resolves to "done" or "canceled"; rejects with an Error
    // carrying why it failed.
    installImageSchedule: function (settings) {
      return postElevated("install-image-schedule", settings);
    },
    // Removes the scheduled image tasks, as an administrator after the UAC
    // prompt. Resolves to "done" or "canceled".
    removeImageSchedule: function () {
      return postElevated("remove-image-schedule", {});
    },
    // Makes the scheduled image tasks run the kopia KopiaUI bundles, as an
    // administrator after the UAC prompt. Resolves to "done" or "canceled".
    updateImageSchedule: function () {
      return postElevated("update-image-schedule", {});
    },
    // Keeps the scheduled image tasks from starting, or lets them start
    // again, as an administrator after the UAC prompt; a run in progress
    // goes on. Resolve to "done" or "canceled".
    pauseImageSchedule: function () {
      return postElevated("pause-image-schedule", {});
    },
    resumeImageSchedule: function () {
      return postElevated("resume-image-schedule", {});
    },
    // Creates images of the disks holding the given volumes (drive letters)
    // now, whether or not images are scheduled: after the UAC prompt, kopia
    // image create runs with this window's repository without a window; the
    // Images page shows its progress. Resolves to "started" or "canceled".
    createImageElevated: function (volumes) {
      return postElevated("create-image", { volumes: volumes });
    },
    // Asks the image run in progress to stop, as an administrator after the
    // UAC prompt; it keeps what it uploaded. Resolves to "done" or "canceled".
    stopImageElevated: function () {
      return postElevated("stop-image", {});
    },
    // Opens a console window that builds recovery media for this window's
    // repository, after the UAC prompt, adding the drivers stored with the
    // images of the computers named in driverHosts, if any. Resolves to
    // "started" or "canceled".
    createRecoveryMediaElevated: function (driverHosts) {
      return postElevated("create-recovery-media", { driverHosts: driverHosts || [] });
    },
  };

  function postElevated(action, body) {
    return post(action, body).then(function (r) {
      if (!r.ok) {
        return r.text().then(function (t) {
          throw new Error(t.trim() || "HTTP " + r.status);
        });
      }

      return r.json().then(function (v) {
        return v.status;
      });
    });
  }

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
