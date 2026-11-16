package disksource

// LocalDisk is a disk of this computer that holds a volume with a drive
// letter: one that "kopia image create" can image by naming that letter.
type LocalDisk struct {
	Number    uint32 `json:"number"`
	Model     string `json:"model"`
	Bus       string `json:"bus"`
	Removable bool   `json:"removable"`

	// System is set for the disk that holds Windows.
	System bool `json:"system"`

	Volumes []LocalVolume `json:"volumes"`
}

// LocalVolume is a volume with a drive letter.
type LocalVolume struct {
	// Letter is the drive letter with a colon, e.g. "C:".
	Letter     string `json:"letter"`
	Label      string `json:"label"`
	FileSystem string `json:"fileSystem"`
	Size       int64  `json:"size"`
}
