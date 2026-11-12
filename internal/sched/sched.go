// Package sched has Windows run the program once a day, through the Task
// Scheduler, as the signed-in user and without administrator rights.
//
// A daily task is used rather than a start-up entry so that nothing happens
// merely because the PC was turned on: Windows starts the program at a set
// time of day, or, if the PC was off then, later the same day.
package sched

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"
)

// Spec describes the daily task.
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
	// Hour and Minute are the time of day. With UTC the time is a fixed
	// moment for the whole world (it does not move with daylight saving
	// time); otherwise it is this PC's wall-clock time.
	Hour, Minute int
	UTC          bool
}

// Slot names the time of day of a Spec in a short form ("16:30Z" or
// "18:00"), used to tell whether a registered task is still the wanted one.
func (s Spec) Slot() string {
	z := ""
	if s.UTC {
		z = "Z"
	}
	return fmt.Sprintf("%02d:%02d%s", s.Hour, s.Minute, z)
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
	boundary := fmt.Sprintf("2026-01-01T%02d:%02d:00", s.Hour, s.Minute)
	if s.UTC {
		boundary += "Z"
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-16"?>` + "\r\n")
	b.WriteString(`<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">` + "\r\n")
	b.WriteString("  <RegistrationInfo>\r\n")
	b.WriteString("    <Description>Looks for the Epic Games Store's free games once a day and claims them through the Epic Games Launcher, without showing anything. To remove it, run EpicAutoClaim.exe uninstall.</Description>\r\n")
	b.WriteString("  </RegistrationInfo>\r\n")
	b.WriteString("  <Triggers>\r\n")
	b.WriteString("    <CalendarTrigger>\r\n")
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
	b.WriteString("    <Priority>7</Priority>\r\n")
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

// TaskName is the task's name for a user: the program's name and the last
// part of the user's security identifier (plain digits, unique per user on
// a PC), so that two Windows users each get a task of their own.
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
	if len(clean) == 0 {
		return app
	}
	return app + "-" + string(clean)
}
