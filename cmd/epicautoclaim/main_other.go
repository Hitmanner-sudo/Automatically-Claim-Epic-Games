//go:build !windows

// EpicAutoClaim is a Windows program: it drives the Epic Games Launcher,
// which only exists there. On other systems only the read-only "check"
// command is available, for development.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/feed"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "check" {
		country := "US"
		if len(os.Args) > 2 {
			country = os.Args[2]
		}
		snap, err := (&feed.Client{}).Fetch(context.Background(), country, "en-US", time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("Free right now in %s:\n", country)
		for _, o := range snap.FreeNow {
			fmt.Printf("  %s  (until %s)\n", o.Title, o.End.Local().Format("Mon 2 Jan 15:04"))
		}
		for _, o := range snap.Upcoming {
			fmt.Printf("Coming up: %s  (from %s)\n", o.Title, o.Start.Local().Format("Mon 2 Jan 15:04"))
		}
		return
	}
	fmt.Fprintln(os.Stderr, "EpicAutoClaim runs on Windows. Here only \"check [COUNTRY]\" works.")
	os.Exit(2)
}
