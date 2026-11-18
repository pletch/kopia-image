package server

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/serverapi"
	"github.com/kopia/kopia/repo/object"
)

// maxImageFolderEntries bounds the entries returned for one folder; the
// largest Windows folders (WinSxS) hold tens of thousands.
const maxImageFolderEntries = 100_000

// handleImageFiles lists a folder of an image snapshot: its root shows the
// volume images and, for each NTFS volume, a folder of its files, as a
// mounted image does. The credential stores of a volume are left out unless
// the server runs as an administrator.
func handleImageFiles(ctx context.Context, rc requestContext) (any, *apiError) {
	oid, err := object.ParseID(rc.queryParam("root"))
	if err != nil {
		return nil, requestError(serverapi.ErrorMalformedRequest, "invalid root object ID")
	}

	var parts []string

	for p := range strings.SplitSeq(rc.queryParam("path"), "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}

	var e fs.Entry = rc.srv.imageBrowseRoot(ctx, rc.rep, oid)

	for _, p := range parts {
		dir, ok := e.(fs.Directory)
		if !ok {
			return nil, requestError(serverapi.ErrorMalformedRequest, p+" is not in a folder")
		}

		e, err = dir.Child(ctx, p)
		if errors.Is(err, fs.ErrEntryNotFound) {
			return nil, notFoundError(p + " not found")
		}

		if err != nil {
			return nil, internalServerError(err)
		}
	}

	dir, ok := e.(fs.Directory)
	if !ok {
		return nil, requestError(serverapi.ErrorMalformedRequest, "not a folder")
	}

	entries, err := fs.GetAllEntries(ctx, dir)
	if err != nil {
		return nil, internalServerError(err)
	}

	resp := &serverapi.ImageFilesResponse{Path: strings.Join(parts, "/"), Entries: []serverapi.ImageFileEntry{}}

	for _, c := range entries {
		switch c.(type) {
		case fs.Directory, fs.File:
		default:
			continue
		}

		if len(resp.Entries) == maxImageFolderEntries {
			resp.Truncated = true
			break
		}

		resp.Entries = append(resp.Entries, serverapi.ImageFileEntry{
			Name:    c.Name(),
			Dir:     c.IsDir(),
			Size:    c.Size(),
			ModTime: c.ModTime(),
		})
	}

	// folders first, then by name, as Explorer lists them
	slices.SortFunc(resp.Entries, func(a, b serverapi.ImageFileEntry) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}

			return 1
		}

		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})

	return resp, nil
}
