package sched

import (
	"encoding/xml"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestXMLIsWellFormedAndCarriesTheSettingsThatMatter(t *testing.T) {
	s := Spec{
		Name: "EpicAutoClaim-1001", Exe: `C:\Users\Zoë & Co\AppData\Local\EpicAutoClaim\EpicAutoClaim.exe`,
		Args: "run --scheduled", Dir: `C:\Users\Zoë & Co\AppData\Local\EpicAutoClaim`,
		UserSID: "S-1-5-21-1111111111-2222222222-3333333333-1001",
	}
	doc := s.XML()
	// Parses as XML (the declaration says UTF-16; the string itself is
	// what gets encoded, so drop the declaration for the parser).
	body := doc[strings.Index(doc, "?>")+2:]
	var task struct {
		Version  string `xml:"version,attr"`
		Marker   string `xml:"RegistrationInfo>Version"`
		Triggers struct {
			Calendar struct {
				StartBoundary string `xml:"StartBoundary"`
				Days          int    `xml:"ScheduleByDay>DaysInterval"`
				Every         string `xml:"Repetition>Interval"`
				For           string `xml:"Repetition>Duration"`
			} `xml:"CalendarTrigger"`
		} `xml:"Triggers"`
		Principal struct {
			UserID    string `xml:"UserId"`
			LogonType string `xml:"LogonType"`
			RunLevel  string `xml:"RunLevel"`
		} `xml:"Principals>Principal"`
		Settings struct {
			Multiple   string `xml:"MultipleInstancesPolicy"`
			NoBattery  string `xml:"DisallowStartIfOnBatteries"`
			StopOnBatt string `xml:"StopIfGoingOnBatteries"`
			CatchUp    string `xml:"StartWhenAvailable"`
			Limit      string `xml:"ExecutionTimeLimit"`
			Wake       string `xml:"WakeToRun"`
			IdleOnly   string `xml:"RunOnlyIfIdle"`
			Priority   int    `xml:"Priority"`
		} `xml:"Settings"`
		Exec struct {
			Command string `xml:"Command"`
			Args    string `xml:"Arguments"`
			Dir     string `xml:"WorkingDirectory"`
		} `xml:"Actions>Exec"`
	}
	if err := xml.Unmarshal([]byte(body), &task); err != nil {
		t.Fatalf("not well-formed: %v\n%s", err, doc)
	}
	// From midnight by the PC's own clock (no zone), every day.
	if task.Version != "1.2" || task.Triggers.Calendar.StartBoundary != "2026-01-01T00:00:00" || task.Triggers.Calendar.Days != 1 {
		t.Errorf("trigger: %+v", task.Triggers)
	}
	// Started again every hour through the day (a copy that finds the
	// program running ends at once), and at ordinary priority.
	if task.Triggers.Calendar.Every != "PT1H" || task.Triggers.Calendar.For != "P1D" || task.Settings.Priority != 5 {
		t.Errorf("repetition %q for %q, priority %d", task.Triggers.Calendar.Every, task.Triggers.Calendar.For, task.Settings.Priority)
	}
	// The schema wants the repetition before the start boundary.
	if strings.Index(doc, "<Repetition>") > strings.Index(doc, "<StartBoundary>") {
		t.Error("Repetition must come before StartBoundary")
	}
	// The marker is plain ASCII whatever the path, and changes with the
	// program's location.
	if task.Marker != s.Marker() || strings.ContainsAny(task.Marker, "&<> ") {
		t.Errorf("marker = %q", task.Marker)
	}
	for _, r := range task.Marker {
		if r > 126 {
			t.Errorf("marker is not ASCII: %q", task.Marker)
		}
	}
	moved := s
	moved.Exe = `D:\Tools\EpicAutoClaim.exe`
	if moved.Marker() == s.Marker() || !strings.HasPrefix(s.Marker(), "eac3-") {
		t.Errorf("markers do not tell tasks apart: %q %q", s.Marker(), moved.Marker())
	}
	// A task registered by an earlier version (for a set time of day) is
	// not taken for an up-to-date one.
	if strings.Contains("eac2-1630Z-0a1b2c3d", s.Marker()) {
		t.Error("an old task's marker must not match")
	}
	if task.Principal.UserID != s.UserSID || task.Principal.LogonType != "InteractiveToken" || task.Principal.RunLevel != "LeastPrivilege" {
		t.Errorf("principal: %+v", task.Principal)
	}
	st := task.Settings
	if st.Multiple != "IgnoreNew" || st.NoBattery != "false" || st.StopOnBatt != "false" || st.CatchUp != "true" || st.Limit != "PT0S" || st.Wake != "false" || st.IdleOnly != "false" {
		t.Errorf("settings: %+v", st)
	}
	// Special characters in the path survive the round trip.
	if task.Exec.Command != s.Exe || task.Exec.Args != s.Args || task.Exec.Dir != s.Dir {
		t.Errorf("action: %+v", task.Exec)
	}
}

func TestUTF16File(t *testing.T) {
	raw := UTF16File("aé😀")
	if raw[0] != 0xFF || raw[1] != 0xFE {
		t.Fatalf("no byte-order mark: % x", raw[:2])
	}
	var units []uint16
	for i := 2; i+1 < len(raw); i += 2 {
		units = append(units, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	if got := string(utf16.Decode(units)); got != "aé😀" {
		t.Fatalf("round trip = %q", got)
	}
}

func TestTaskName(t *testing.T) {
	a := TaskName("EpicAutoClaim", "S-1-5-21-1111111111-2222222222-3333333333-1001")
	if !strings.HasPrefix(a, "EpicAutoClaim-1001-") || len(a) != len("EpicAutoClaim-1001-")+4 {
		t.Errorf("TaskName = %q", a)
	}
	// The same user always gets the same name; a local and a domain
	// account that share the last number do not share a task.
	if a != TaskName("EpicAutoClaim", "S-1-5-21-1111111111-2222222222-3333333333-1001") {
		t.Error("TaskName is not stable")
	}
	if TaskName("EpicAutoClaim", "S-1-5-21-1-2-3-500") == TaskName("EpicAutoClaim", "S-1-5-21-9-8-7-500") {
		t.Error("two different accounts share a task name")
	}
	for _, sid := range []string{"", "weird"} {
		if got := TaskName("EpicAutoClaim", sid); got != "EpicAutoClaim" {
			t.Errorf("TaskName(%q) = %q", sid, got)
		}
	}
}
