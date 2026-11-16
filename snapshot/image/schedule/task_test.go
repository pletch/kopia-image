package schedule

import (
	"encoding/binary"
	"encoding/xml"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/stretchr/testify/require"
)

func decodeUTF16(t *testing.T, b []byte) string {
	t.Helper()

	require.Equal(t, []byte{0xff, 0xfe}, b[:2], "byte order mark")

	u := make([]uint16, (len(b)-2)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2+2*i:])
	}

	return string(utf16.Decode(u))
}

func testTask() *Task {
	return &Task{
		Name:          `\Kopia\Image create`,
		Description:   "Nightly image of C:",
		Command:       `C:\Program Files\Kopia\kopia.exe`,
		Arguments:     []string{"--config-file", `C:\ProgramData\kopia\image\repository.config`, "image", "create", "C:"},
		At:            time.Date(2000, 1, 1, 2, 30, 0, 0, time.UTC),
		TimeLimit:     4 * time.Hour,
		RetryInterval: 15 * time.Minute,
		RetryCount:    3,
		WakeToRun:     true,
	}
}

func TestTaskXML_daily(t *testing.T) {
	now := time.Date(2026, 11, 16, 15, 0, 0, 0, time.Local) //nolint:gosmopolitan

	b, err := testTask().XML(now)
	require.NoError(t, err)

	text := decodeUTF16(t, b)
	require.True(t, strings.HasPrefix(text, `<?xml version="1.0" encoding="UTF-16"?>`))

	for _, want := range []string{
		`<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">`,
		`<URI>\Kopia\Image create</URI>`,
		`<StartBoundary>2026-11-16T02:30:00</StartBoundary>`,
		`<DaysInterval>1</DaysInterval>`,
		`<UserId>S-1-5-18</UserId>`,
		`<RunLevel>HighestAvailable</RunLevel>`,
		`<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>`,
		`<DisallowStartIfOnBatteries>true</DisallowStartIfOnBatteries>`,
		`<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>`,
		`<StartWhenAvailable>true</StartWhenAvailable>`,
		`<RunOnlyIfNetworkAvailable>true</RunOnlyIfNetworkAvailable>`,
		`<WakeToRun>true</WakeToRun>`,
		`<ExecutionTimeLimit>PT4H</ExecutionTimeLimit>`,
		`<Interval>PT15M</Interval>`,
		`<Count>3</Count>`,
		`<Command>C:\Program Files\Kopia\kopia.exe</Command>`,
		`<Arguments>--config-file C:\ProgramData\kopia\image\repository.config image create C:</Arguments>`,
	} {
		require.Contains(t, text, want)
	}

	// the document is well-formed
	var v struct {
		XMLName xml.Name
	}

	require.NoError(t, xml.Unmarshal([]byte(strings.Replace(text, `encoding="UTF-16"`, "", 1)), &v))
}

func TestTaskXML_weekly(t *testing.T) {
	task := testTask()
	task.Days = []time.Weekday{time.Sunday, time.Wednesday}
	task.RetryCount = 0
	task.AllowOnBatteries = true

	b, err := task.XML(time.Now()) //nolint:forbidigo
	require.NoError(t, err)

	text := decodeUTF16(t, b)
	require.Contains(t, text, "<Sunday></Sunday>")
	require.Contains(t, text, "<Wednesday></Wednesday>")
	require.NotContains(t, text, "<Days>")
	require.Contains(t, text, "<WeeksInterval>1</WeeksInterval>")
	require.NotContains(t, text, "RestartOnFailure")
	require.Contains(t, text, "<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>")
}

func TestTaskXML_missingCommand(t *testing.T) {
	_, err := (&Task{}).XML(time.Now()) //nolint:forbidigo
	require.Error(t, err)
}

func TestIsoDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                             "PT0S",
		4 * time.Hour:                 "PT4H",
		15 * time.Minute:              "PT15M",
		90 * time.Second:              "PT1M30S",
		2*time.Hour + 5*time.Second:   "PT2H5S",
		26*time.Hour + 3*time.Minute:  "PT26H3M",
		time.Second + time.Nanosecond: "PT1S",
	} {
		require.Equal(t, want, isoDuration(d), "%v", d)
	}
}

func TestJoinArgs(t *testing.T) {
	cases := map[string][]string{
		`a b`:                          {"a", "b"},
		`""`:                           {""},
		`"C:\Program Files\x"`:         {`C:\Program Files\x`},
		`"a \"q\" b"`:                  {`a "q" b`},
		`C:\dir\`:                      {`C:\dir\`},
		`"C:\my dir\\"`:                {`C:\my dir\`},
		`"a\\\"b c"`:                   {`a\"b c`},
		`--state-file=C:\x\state.json`: {`--state-file=C:\x\state.json`},
	}

	for want, args := range cases {
		require.Equal(t, want, JoinArgs(args), "%q", args)
	}
}
