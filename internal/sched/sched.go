// Package sched has Windows keep the program running, through the Task
// Scheduler, as the signed-in user and without administrator rights.
//
// The program is started when the user signs in (a start-up entry does
// that; a task that fires at sign-in needs administrator rights). The task
// here is what brings it back if it is not running: Windows starts it every
// hour, and a copy that finds another one running ends at once.
package sched

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf16"
)

// Spec describes the task.
type Spec struct {
	// Name of the task in the Task Scheduler's root folder.
	Name string
	// Exe is the program to start, Args its arguments, Dir its working
	// directory.
	Exe  string
	Args string
	Dir  string
	// UserSID is the security identifier of the user the task runs as (the
	// one creating it).
	UserSID string
}

// layout is raised whenever the task definition below changes in a way
// that an already registered task should pick up. (Layouts 1 and 2 were a
// task for a set time of day.)
const layout = 3

// Marker is a short plain-ASCII string written into the task (as its
// version) that says which program location and layout it was registered
// for. A registered task whose definition contains the wanted marker is up
// to date. (Comparing paths in the definition's text would not do: the tool
// that prints it re-encodes them.)
func (s Spec) Marker() string {
	sum := sha256.Sum256([]byte(strings.ToLower(s.Exe)))
	return fmt.Sprintf("eac%d-%s", layout, hex.EncodeToString(sum[:4]))
}

func xmlText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

// XML renders the task definition (Task Scheduler schema 1.2, which every
// Windows since Vista understands).
//
// Several settings are spelled out because their defaults would defeat the
// purpose: by default a task does not start on battery power, is stopped
// when the PC goes on battery, is killed after three days, and is simply
// skipped when the PC was off at its time.
func (s Spec) XML() string {
	// Midnight by this PC's clock, and from there every hour.
	const boundary = "2026-01-01T00:00:00"
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-16"?>` + "\r\n")
	b.WriteString(`<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">` + "\r\n")
	b.WriteString("  <RegistrationInfo>\r\n")
	b.WriteString("    <Version>" + xmlText(s.Marker()) + "</Version>\r\n")
	b.WriteString("    <Description>Keeps EpicAutoClaim running: starts it every hour unless it is running already. EpicAutoClaim looks for the Epic Games Store's free games every hour and claims them through the Epic Games Launcher, without showing anything. To remove it, run EpicAutoClaim.exe uninstall.</Description>\r\n")
	b.WriteString("  </RegistrationInfo>\r\n")
	b.WriteString("  <Triggers>\r\n")
	b.WriteString("    <CalendarTrigger>\r\n")
	// A trigger for every day whose task is started again every hour
	// through that day: in effect, every hour for good. A copy of the
	// program that is started while another one is running ends at once, so
	// nothing is fetched or started because of this; it is what brings the
	// program back within the hour if it was ended.
	b.WriteString("      <Repetition>\r\n        <Interval>PT1H</Interval>\r\n        <Duration>P1D</Duration>\r\n        <StopAtDurationEnd>false</StopAtDurationEnd>\r\n      </Repetition>\r\n")
	b.WriteString("      <StartBoundary>" + boundary + "</StartBoundary>\r\n")
	b.WriteString("      <Enabled>true</Enabled>\r\n")
	b.WriteString("      <ScheduleByDay>\r\n        <DaysInterval>1</DaysInterval>\r\n      </ScheduleByDay>\r\n")
	b.WriteString("    </CalendarTrigger>\r\n")
	b.WriteString("  </Triggers>\r\n")
	b.WriteString("  <Principals>\r\n")
	b.WriteString(`    <Principal id="Author">` + "\r\n")
	b.WriteString("      <UserId>" + xmlText(s.UserSID) + "</UserId>\r\n")
	b.WriteString("      <LogonType>InteractiveToken</LogonType>\r\n")
	b.WriteString("      <RunLevel>LeastPrivilege</RunLevel>\r\n")
	b.WriteString("    </Principal>\r\n")
	b.WriteString("  </Principals>\r\n")
	b.WriteString("  <Settings>\r\n")
	b.WriteString("    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>\r\n")
	b.WriteString("    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>\r\n")
	b.WriteString("    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>\r\n")
	b.WriteString("    <AllowHardTerminate>true</AllowHardTerminate>\r\n")
	b.WriteString("    <StartWhenAvailable>true</StartWhenAvailable>\r\n")
	b.WriteString("    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>\r\n")
	b.WriteString("    <IdleSettings>\r\n      <StopOnIdleEnd>false</StopOnIdleEnd>\r\n      <RestartOnIdle>false</RestartOnIdle>\r\n    </IdleSettings>\r\n")
	b.WriteString("    <AllowStartOnDemand>true</AllowStartOnDemand>\r\n")
	b.WriteString("    <Enabled>true</Enabled>\r\n")
	b.WriteString("    <Hidden>false</Hidden>\r\n")
	b.WriteString("    <RunOnlyIfIdle>false</RunOnlyIfIdle>\r\n")
	b.WriteString("    <WakeToRun>false</WakeToRun>\r\n")
	b.WriteString("    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>\r\n")
	// 7, the default for tasks, means below-normal priority (and low disk
	// priority), which everything the program starts would inherit: the
	// hidden launcher, and the user's own launcher when it is put back in
	// the tray. 5 is ordinary priority.
	b.WriteString("    <Priority>5</Priority>\r\n")
	b.WriteString("  </Settings>\r\n")
	b.WriteString(`  <Actions Context="Author">` + "\r\n")
	b.WriteString("    <Exec>\r\n")
	b.WriteString("      <Command>" + xmlText(s.Exe) + "</Command>\r\n")
	if s.Args != "" {
		b.WriteString("      <Arguments>" + xmlText(s.Args) + "</Arguments>\r\n")
	}
	if s.Dir != "" {
		b.WriteString("      <WorkingDirectory>" + xmlText(s.Dir) + "</WorkingDirectory>\r\n")
	}
	b.WriteString("    </Exec>\r\n")
	b.WriteString("  </Actions>\r\n")
	b.WriteString("</Task>\r\n")
	return b.String()
}

// UTF16File encodes text the way the Task Scheduler's tools read a task
// file: UTF-16, little-endian, with a byte-order mark.
func UTF16File(text string) []byte {
	units := utf16.Encode([]rune(text))
	out := make([]byte, 2, 2+2*len(units))
	out[0], out[1] = 0xFF, 0xFE
	for _, u := range units {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}

// TaskName is the task's name for a user, so that two Windows users each
// get a task of their own: the program's name, the last part of the user's
// security identifier (plain digits) and, because that part alone is not
// unique between a PC's own accounts and those of a domain, a few
// characters derived from the whole identifier.
func TaskName(app, userSID string) string {
	tail := userSID
	if i := strings.LastIndexByte(userSID, '-'); i >= 0 {
		tail = userSID[i+1:]
	}
	clean := make([]byte, 0, len(tail))
	for i := 0; i < len(tail); i++ {
		if c := tail[i]; c >= '0' && c <= '9' {
			clean = append(clean, c)
		}
	}
	if len(clean) == 0 || !strings.HasPrefix(userSID, "S-") {
		return app
	}
	sum := sha256.Sum256([]byte(userSID))
	return app + "-" + string(clean) + "-" + hex.EncodeToString(sum[:2])
}
