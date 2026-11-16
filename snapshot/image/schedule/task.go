// Package schedule installs kopia image runs as Windows Task Scheduler tasks:
// it builds task definitions with the settings an unattended image backup
// needs and registers them with schtasks.
//
// Building task definitions is platform independent; registering them is
// Windows-only and returns ErrNotSupported elsewhere.
package schedule

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/pkg/errors"
)

// ErrNotSupported is returned on platforms without Windows Task Scheduler.
var ErrNotSupported = errors.New("scheduling kopia image runs is supported only on Windows")

// systemSID is the security identifier of the LocalSystem account, which
// volume shadow copies require.
const systemSID = "S-1-5-18"

// Task describes a task that runs a program as SYSTEM on a schedule.
type Task struct {
	// Name is the task's path, e.g. `\Kopia\Image create`.
	Name        string
	Description string

	Command    string
	Arguments  []string
	WorkingDir string

	// At is the time of day to run; only its hour and minute are used.
	At time.Time

	// Days are the weekdays to run on; empty means every day.
	Days []time.Weekday

	// TimeLimit stops a run that takes longer, so a hung run does not block
	// the next one.
	TimeLimit time.Duration

	// RetryInterval and RetryCount restart a failed run.
	RetryInterval time.Duration
	RetryCount    int

	// WakeToRun wakes the computer from sleep to run.
	WakeToRun bool

	// AllowOnBatteries starts runs on battery power; runs never stop when
	// the computer switches to battery.
	AllowOnBatteries bool
}

type taskXML struct {
	XMLName    xml.Name `xml:"Task"`
	Version    string   `xml:"version,attr"`
	Xmlns      string   `xml:"xmlns,attr"`
	Info       infoXML  `xml:"RegistrationInfo"`
	Triggers   triggersXML
	Principals principalsXML
	Settings   settingsXML
	Actions    actionsXML
}

type infoXML struct {
	Description string
	URI         string
}

type triggersXML struct {
	Calendar calendarTriggerXML `xml:"CalendarTrigger"`
}

type calendarTriggerXML struct {
	StartBoundary string
	Enabled       bool
	ByDay         *byDayXML  `xml:"ScheduleByDay,omitempty"`
	ByWeek        *byWeekXML `xml:"ScheduleByWeek,omitempty"`
}

type byDayXML struct {
	DaysInterval int
}

type byWeekXML struct {
	DaysOfWeek    daysOfWeekXML
	WeeksInterval int
}

// daysOfWeekXML is a list of empty elements named after weekdays, e.g.
// <Sunday/>, which encoding/xml cannot express with struct tags.
type daysOfWeekXML struct {
	Days []string
}

func (d daysOfWeekXML) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	tokens := []xml.Token{start}

	for _, day := range d.Days {
		name := xml.Name{Local: day}
		tokens = append(tokens, xml.StartElement{Name: name}, xml.EndElement{Name: name})
	}

	tokens = append(tokens, start.End())

	for _, tok := range tokens {
		if err := e.EncodeToken(tok); err != nil {
			return errors.Wrap(err, "unable to encode days of week")
		}
	}

	return nil
}

type principalsXML struct {
	Principal principalXML
}

type principalXML struct {
	ID       string `xml:"id,attr"`
	UserID   string `xml:"UserId"`
	RunLevel string
}

type settingsXML struct {
	MultipleInstancesPolicy    string
	DisallowStartIfOnBatteries bool
	StopIfGoingOnBatteries     bool
	AllowHardTerminate         bool
	StartWhenAvailable         bool
	RunOnlyIfNetworkAvailable  bool
	AllowStartOnDemand         bool
	Enabled                    bool
	Hidden                     bool
	RunOnlyIfIdle              bool
	WakeToRun                  bool
	ExecutionTimeLimit         string
	Priority                   int
	RestartOnFailure           *restartXML `xml:",omitempty"`
}

type restartXML struct {
	Interval string
	Count    int
}

type actionsXML struct {
	Context string  `xml:",attr"`
	Exec    execXML `xml:"Exec"`
}

type execXML struct {
	Command          string
	Arguments        string
	WorkingDirectory string `xml:",omitempty"`
}

// belowNormalPriority keeps background runs from slowing interactive use.
const belowNormalPriority = 7

// XML returns the task definition as UTF-16LE XML with a byte order mark,
// the encoding schtasks /XML expects. now dates the first run.
func (t *Task) XML(now time.Time) ([]byte, error) {
	if t.Command == "" {
		return nil, errors.New("missing command")
	}

	// Task Scheduler reads a start boundary without a zone as local time
	local := now.Local()
	start := time.Date(local.Year(), local.Month(), local.Day(), t.At.Hour(), t.At.Minute(), 0, 0, local.Location())

	trigger := calendarTriggerXML{StartBoundary: start.Format("2006-01-02T15:04:05"), Enabled: true}

	if len(t.Days) == 0 {
		trigger.ByDay = &byDayXML{DaysInterval: 1}
	} else {
		trigger.ByWeek = &byWeekXML{WeeksInterval: 1}

		for _, d := range t.Days {
			trigger.ByWeek.DaysOfWeek.Days = append(trigger.ByWeek.DaysOfWeek.Days, d.String())
		}
	}

	settings := settingsXML{
		MultipleInstancesPolicy:    "IgnoreNew",
		DisallowStartIfOnBatteries: !t.AllowOnBatteries,
		StopIfGoingOnBatteries:     false,
		AllowHardTerminate:         true,
		StartWhenAvailable:         true,
		RunOnlyIfNetworkAvailable:  true,
		AllowStartOnDemand:         true,
		Enabled:                    true,
		WakeToRun:                  t.WakeToRun,
		ExecutionTimeLimit:         isoDuration(t.TimeLimit),
		Priority:                   belowNormalPriority,
	}

	if t.RetryCount > 0 {
		settings.RestartOnFailure = &restartXML{Interval: isoDuration(t.RetryInterval), Count: t.RetryCount}
	}

	doc := taskXML{
		Version:    "1.4",
		Xmlns:      "http://schemas.microsoft.com/windows/2004/02/mit/task",
		Info:       infoXML{Description: t.Description, URI: t.Name},
		Triggers:   triggersXML{Calendar: trigger},
		Principals: principalsXML{Principal: principalXML{ID: "Author", UserID: systemSID, RunLevel: "HighestAvailable"}},
		Settings:   settings,
		Actions: actionsXML{Context: "Author", Exec: execXML{
			Command:          t.Command,
			Arguments:        JoinArgs(t.Arguments),
			WorkingDirectory: t.WorkingDir,
		}},
	}

	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, errors.Wrap(err, "unable to encode the task")
	}

	text := `<?xml version="1.0" encoding="UTF-16"?>` + "\n" + string(body) + "\n"

	var b bytes.Buffer

	b.Write([]byte{0xff, 0xfe})

	for _, u := range utf16.Encode([]rune(text)) {
		_ = binary.Write(&b, binary.LittleEndian, u)
	}

	return b.Bytes(), nil
}

// isoDuration formats a duration as an ISO 8601 duration such as PT4H15M.
// Zero means no limit (PT0S).
func isoDuration(d time.Duration) string {
	d = d.Truncate(time.Second)

	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60 //nolint:mnd

	var sb strings.Builder

	sb.WriteString("PT")

	if h > 0 {
		fmt.Fprintf(&sb, "%dH", h)
	}

	if m > 0 {
		fmt.Fprintf(&sb, "%dM", m)
	}

	if s > 0 || (h == 0 && m == 0) {
		fmt.Fprintf(&sb, "%dS", s)
	}

	return sb.String()
}

// JoinArgs quotes arguments for a Windows command line so that the C runtime
// parses them back unchanged.
func JoinArgs(args []string) string {
	quoted := make([]string, len(args))

	for i, a := range args {
		quoted[i] = escapeArg(a)
	}

	return strings.Join(quoted, " ")
}

// escapeArg follows the rules of CommandLineToArgvW: quotes are escaped with
// a backslash, and backslashes are doubled only before a quote.
func escapeArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}

	var b strings.Builder

	b.WriteByte('"')

	slashes := 0

	for _, c := range []byte(s) {
		switch c {
		case '\\':
			slashes++
		case '"':
			// the backslashes already written are doubled, and the quote escaped
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
		default:
			slashes = 0
		}

		b.WriteByte(c)
	}

	b.WriteString(strings.Repeat(`\`, slashes))
	b.WriteByte('"')

	return b.String()
}
