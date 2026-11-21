package cli

import (
	"io"
	"unicode/utf8"

	"github.com/pkg/errors"
	"golang.org/x/term"
)

// Keys that readMasked handles.
const (
	keyCtrlC     = 3
	keyBackspace = 8
	keyLineFeed  = '\n'
	keyReturn    = '\r'
	keyDelete    = 127
)

var errPasswordCanceled = errors.New("canceled")

// readMaskedPassword reads a password from the terminal fd, showing a * for
// each character typed, so that people see that their typing arrives. When
// the terminal cannot be switched to raw mode, it reads without echo.
func readMaskedPassword(fd int, in io.Reader, out io.Writer) ([]byte, error) {
	state, err := term.MakeRaw(fd)
	if err != nil {
		return term.ReadPassword(fd) //nolint:wrapcheck
	}

	defer term.Restore(fd, state) //nolint:errcheck

	return readMasked(in, out)
}

// readMasked reads a line from in, a terminal in raw mode, writing a * to out
// for each character, erasing one for each backspace, until Enter.
func readMasked(in io.Reader, out io.Writer) ([]byte, error) {
	var (
		pass []byte
		b    [1]byte
	)

	for {
		n, err := in.Read(b[:])
		if err != nil {
			return nil, errors.Wrap(err, "unable to read the password")
		}

		if n == 0 {
			continue
		}

		switch c := b[0]; {
		case c == keyReturn || c == keyLineFeed:
			return pass, nil

		case c == keyCtrlC:
			return nil, errPasswordCanceled

		case c == keyBackspace || c == keyDelete:
			if len(pass) > 0 {
				_, size := utf8.DecodeLastRune(pass)
				pass = pass[:len(pass)-size]

				io.WriteString(out, "\b \b") //nolint:errcheck
			}

		case c < ' ':
			// other control keys are not part of a password

		default:
			pass = append(pass, c)

			// one * per character, not per byte of its encoding
			if c < utf8.RuneSelf || utf8.RuneStart(c) {
				io.WriteString(out, "*") //nolint:errcheck
			}
		}
	}
}
