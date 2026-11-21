package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadMasked(t *testing.T) {
	cases := []struct {
		name, input, want, echo string
	}{
		{name: "plain", input: "secret\r", want: "secret", echo: "******"},
		{name: "line feed", input: "abc\n", want: "abc", echo: "***"},
		{name: "backspace", input: "abx\bc\r", want: "abc", echo: "***\b \b*"},
		{name: "delete key", input: "ab\x7f\x7f\x7fc\r", want: "c", echo: "**\b \b\b \b*"},
		{name: "multi-byte characters", input: "pä€\b!\r", want: "pä!", echo: "***\b \b*"},
		{name: "control keys ignored", input: "a\x1bb\tc\r", want: "abc", echo: "***"},
		{name: "empty", input: "\r", want: "", echo: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer

			got, err := readMasked(strings.NewReader(tc.input), &out)
			require.NoError(t, err)
			require.Equal(t, tc.want, string(got))
			require.Equal(t, tc.echo, out.String())
		})
	}

	_, err := readMasked(strings.NewReader("abc\x03"), &bytes.Buffer{})
	require.ErrorIs(t, err, errPasswordCanceled)

	_, err = readMasked(strings.NewReader("no end"), &bytes.Buffer{})
	require.Error(t, err)
}
