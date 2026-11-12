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
		UserSID: "S-1-5-21-1111111111-2222222222-3333333333-1001", Hour: 16, Minute: 30, UTC: true,
	}
	doc := s.XML()
	// Parses as XML (the declaration says UTF-16; the string itself is
	// what gets encoded, so drop the declaration for the parser).
	body := doc[strings.Index(doc, "?>")+2:]
	var task struct {
		Version  string `xml:"version,attr"`
		Triggers struct {
			Calendar struct {
				StartBoundary string `xml:"StartBoundary"`
				Days          int    `xml:"ScheduleByDay>DaysInterval"`
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
	if task.Version != "1.2" || task.Triggers.Calendar.StartBoundary != "2026-01-01T16:30:00Z" || task.Triggers.Calendar.Days != 1 {
		t.Errorf("trigger: %+v", task.Triggers)
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

	// A local time of day carries no zone.
	s.UTC, s.Hour, s.Minute = false, 7, 5
	if !strings.Contains(s.XML(), "<StartBoundary>2026-01-01T07:05:00</StartBoundary>") {
		t.Errorf("local boundary missing:\n%s", s.XML())
	}
	if s.Slot() != "07:05" || (Spec{Hour: 16, Minute: 30, UTC: true}).Slot() != "16:30Z" {
		t.Errorf("slot names: %q", s.Slot())
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
	for sid, want := range map[string]string{
		"S-1-5-21-1111111111-2222222222-3333333333-1001": "EpicAutoClaim-1001",
		"S-1-5-21-1-2-3-500":                             "EpicAutoClaim-500",
		"":                                               "EpicAutoClaim",
		"weird":                                          "EpicAutoClaim",
	} {
		if got := TaskName("EpicAutoClaim", sid); got != want {
			t.Errorf("TaskName(%q) = %q, want %q", sid, got, want)
		}
	}
}
