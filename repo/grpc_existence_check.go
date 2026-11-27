package repo

import "sync"

const (
	// existenceCheckWarmup is how many checks a session makes before it
	// starts judging whether they pay off.
	existenceCheckWarmup = 64

	// existenceCheckMinHitRate is the share of checks that must find the
	// content already stored for checking every content to be worth it.
	existenceCheckMinHitRate = 0.1

	// existenceCheckProbeInterval is how often a session that stopped
	// checking still checks, to notice when contents start to exist again.
	existenceCheckProbeInterval = 16

	// existenceCheckWindow bounds the history, so the hit rate follows what
	// the session is uploading now.
	existenceCheckWindow = 256
)

// existenceCheckPolicy decides whether a client asks the server whether a
// large content exists before sending it.
//
// The check saves sending a content the server already has, at the cost of a
// round trip and server work for every one. On a first upload, such as the
// first image of a disk, almost no content exists and a 1 TB image made
// about a million checks that found nothing. When checks rarely find a
// content, the session sends contents directly, checking one in
// existenceCheckProbeInterval to keep measuring. The server deduplicates
// and un-deletes contents it is sent, so skipping a check costs bandwidth
// only, never correctness.
type existenceCheckPolicy struct {
	mu sync.Mutex

	checks  int
	hits    int
	skipped int
}

// shouldCheck reports whether the next large content should be checked.
func (p *existenceCheckPolicy) shouldCheck() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.checks < existenceCheckWarmup || float64(p.hits) >= existenceCheckMinHitRate*float64(p.checks) {
		return true
	}

	p.skipped++

	return p.skipped%existenceCheckProbeInterval == 0
}

// record notes the result of a check.
func (p *existenceCheckPolicy) record(found bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.checks++

	if found {
		p.hits++
	}

	if p.checks >= existenceCheckWindow {
		p.checks /= 2
		p.hits /= 2
	}
}
