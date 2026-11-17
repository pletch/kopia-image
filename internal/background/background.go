// Package background lowers the priority of the kopia process, so that a
// backup yields to what the person is doing on the computer and runs at full
// speed when it is idle.
package background

// Enter puts the process in background mode until leave is called: on
// Windows its CPU, disk and memory priority are lowered. Elsewhere it does
// nothing, since lowering the priority of a Go program there would have to
// reach every thread it starts.
//
// Enter is meant for a process that does one backup job, such as an image run
// or the server behind a KopiaUI window; it must not be called again before
// leave.
func Enter() (leave func(), err error) {
	return enter()
}
