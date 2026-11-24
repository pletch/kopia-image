package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateLegacyFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "<missing>"
		}

		return string(b)
	}

	// the file of KopiaUI-Wails is renamed
	write(legacyStateFileName, "old")
	migrateLegacyFile(dir, legacyStateFileName, stateFileName)

	if read(stateFileName) != "old" || read(legacyStateFileName) != "<missing>" {
		t.Fatalf("not renamed: new=%q old=%q", read(stateFileName), read(legacyStateFileName))
	}

	// a file of the new name is kept
	write(legacyStateFileName, "older")
	migrateLegacyFile(dir, legacyStateFileName, stateFileName)

	if read(stateFileName) != "old" || read(legacyStateFileName) != "older" {
		t.Fatalf("new file replaced: new=%q old=%q", read(stateFileName), read(legacyStateFileName))
	}

	// nothing to rename
	migrateLegacyFile(dir, legacyWindowStateFileName, windowStateFileName)

	if read(windowStateFileName) != "<missing>" {
		t.Fatal("a file was created")
	}
}
