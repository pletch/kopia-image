//go:build !windows

package background

func enter() (func(), error) {
	return func() {}, nil
}
