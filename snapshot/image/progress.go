package image

import "sync"

// Progress tells how far Create got imaging a disk, for showing progress
// while it runs. Create updates it when CreateOptions.Progress is set; its
// methods may be called from any goroutine. A nil Progress ignores updates.
type Progress struct {
	mu sync.Mutex

	// +checklocks:mu
	volume int
	// +checklocks:mu
	volumes int
	// +checklocks:mu
	volumeName string

	// doneBefore is the size of the volumes already imaged, volumeDone the
	// part of the current volume imaged, total the size of all volumes.
	// +checklocks:mu
	doneBefore int64
	// +checklocks:mu
	volumeDone int64
	// +checklocks:mu
	total int64

	// checking is set while the volume's upload is saved and its chunks
	// checked, of which checked are done out of toCheck.
	// +checklocks:mu
	checking bool
	// +checklocks:mu
	checked int64
	// +checklocks:mu
	toCheck int64
}

// ProgressSnapshot is the state of a Progress at one moment.
type ProgressSnapshot struct {
	// Volume is the 1-based index of the volume being imaged among Volumes,
	// 0 before the first.
	Volume     int    `json:"partition"`
	Volumes    int    `json:"partitions"`
	VolumeName string `json:"partitionName"`

	// BytesDone counts the bytes of the disk's volumes imaged so far, chunks
	// of free space included, out of BytesTotal.
	BytesDone  int64 `json:"bytesDone"`
	BytesTotal int64 `json:"bytesTotal"`

	// Checking is set once a volume is read, while what was uploaded is
	// saved and the repository is asked whether it holds every chunk:
	// ChunksChecked of ChunksToCheck, which is 0 until the chunks are
	// listed.
	Checking      bool  `json:"checking,omitempty"`
	ChunksChecked int64 `json:"chunksChecked,omitempty"`
	ChunksToCheck int64 `json:"chunksToCheck,omitempty"`
}

// Snapshot returns the current state.
func (p *Progress) Snapshot() ProgressSnapshot {
	if p == nil {
		return ProgressSnapshot{}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	done := min(p.doneBefore+p.volumeDone, p.total)

	return ProgressSnapshot{
		Volume: p.volume, Volumes: p.volumes, VolumeName: p.volumeName, BytesDone: done, BytesTotal: p.total,
		Checking: p.checking, ChunksChecked: p.checked, ChunksToCheck: p.toCheck,
	}
}

func (p *Progress) startDisk(volumes []VolumeSource) {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.volume, p.volumes, p.volumeName = 0, len(volumes), ""
	p.doneBefore, p.volumeDone, p.total = 0, 0, 0

	for i := range volumes {
		p.total += volumes[i].Size
	}
}

func (p *Progress) startVolume(index int, name string) {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.volume, p.volumeName, p.volumeDone = index, name, 0
	p.checking, p.checked, p.toCheck = false, 0, 0
}

// startChecking marks the volume as read: its upload is saved and checked.
func (p *Progress) startChecking() {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.checking, p.checked, p.toCheck = true, 0, 0
}

// checkedChunks records how many of the volume's chunks were checked.
func (p *Progress) checkedChunks(done, total int64) {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.checked, p.toCheck = done, total
}

// restartVolume starts the current volume over, as raw volumes are read twice.
func (p *Progress) restartVolume() {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.volumeDone = 0
}

func (p *Progress) addVolumeBytes(n int64) {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.volumeDone += n
}

func (p *Progress) finishVolume(size int64) {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.doneBefore += size
	p.volumeDone = 0
	p.checking, p.checked, p.toCheck = false, 0, 0
}
