package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

const (
	// unmountArg, followed by a drive such as "X:\", asks the running instance
	// to unmount what one of its servers mounted there. Explorer's context
	// menu entry for Kopia drives, which the installer adds, passes it.
	unmountArg = "--unmount"

	unmountTimeout = 30 * time.Second
)

// errNotMountedHere is returned by unmountPath when no server of this
// instance mounted the path.
var errNotMountedHere = errors.New("not mounted by KopiaUI")

// errServerResponse wraps the error a server returned for an unmount request.
var errServerResponse = errors.New("server error")

// argValue returns the argument that follows name.
func argValue(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1], true
		}
	}

	return "", false
}

// unmountArgPath returns the drive given with unmountArg. A quoted drive root,
// "X:\", reaches the program as X:" since \" escapes the quote, so quotes are
// dropped; the installer's command passes %1 unquoted for that reason.
func unmountArgPath(args []string) (string, bool) {
	v, ok := argValue(args, unmountArg)
	if !ok {
		return "", false
	}

	v = strings.TrimSpace(strings.Trim(v, `"`))

	return v, v != ""
}

// unmountMessage is the message shown for an unmount request that failed.
func unmountMessage(path string, err error) string {
	if errors.Is(err, errNotMountedHere) {
		return path + " was not mounted by KopiaUI. If 'kopia mount' mounted it, stop that command (Ctrl+C in its window)."
	}

	return fmt.Sprintf("Unable to unmount %v: %v", path, err)
}

// handleUnmountRequest unmounts a path for another process, such as
// Explorer's context menu, and tells about a failure with a notification.
func (s *shell) handleUnmountRequest(path string) {
	err := s.unmountPath(path)
	if err == nil {
		slog.Info("unmounted on request", "path", path)
		return
	}

	slog.Warn("unable to unmount on request", "path", path, "err", err)

	if nerr := s.notifier.SendNotification(notifications.NotificationOptions{
		ID:    "unmount-" + path, // a later failure for the same drive replaces it
		Title: "Unmount",
		Body:  unmountMessage(path, err),
	}); nerr != nil {
		slog.Warn("unable to show notification", "err", nerr)
	}
}

// unmountPath asks each running server to unmount what it mounted at path.
func (s *shell) unmountPath(path string) error {
	s.mu.Lock()

	servers := make([]*repoServer, 0, len(s.servers))
	for _, srv := range s.servers {
		servers = append(servers, srv)
	}

	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), unmountTimeout)
	defer cancel()

	var errs []error

	for _, srv := range servers {
		err := srv.unmount(ctx, path)
		if err == nil {
			return nil
		}

		if !errors.Is(err, errNotMountedHere) && !errors.Is(err, errServerNotRunning) {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return errNotMountedHere
}

// unmount asks the server to unmount what it mounted at path.
func (s *repoServer) unmount(ctx context.Context, path string) error {
	status, body, err := s.controlPost(ctx, "unmount", map[string]string{"path": path})
	if err != nil {
		return err
	}

	switch status {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return errNotMountedHere
	}

	var apiErr struct {
		Error string `json:"error"`
	}

	if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != "" {
		return fmt.Errorf("%w: %v", errServerResponse, apiErr.Error)
	}

	return fmt.Errorf("%w: status %v: %v", errServerResponse, status, strings.TrimSpace(string(body)))
}
