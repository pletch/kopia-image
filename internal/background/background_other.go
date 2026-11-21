//go:build !windows && !linux

package background

func enter() (func(), error) {
	return func() {}, nil
}
