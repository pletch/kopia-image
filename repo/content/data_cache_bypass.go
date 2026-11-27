package content

import "context"

type bypassDataCacheKey struct{}

// WithoutDataCache returns a context in which reads of data contents (those
// without a prefix) go straight to storage, or to the repository server,
// instead of through the local content cache. Metadata contents still use
// their cache.
//
// It is for reads that touch each content once, such as restoring a whole
// disk image: caching them only fills the cache, and its sweep spares
// entries younger than the minimum sweep age, so a restore running at
// hundreds of MB/s grew the cache by tens of gigabytes and evicted useful
// entries.
func WithoutDataCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, bypassDataCacheKey{}, true)
}

// DataCacheBypassed reports whether reads of data contents in ctx skip the
// local content cache.
func DataCacheBypassed(ctx context.Context) bool {
	v, _ := ctx.Value(bypassDataCacheKey{}).(bool)

	return v
}
