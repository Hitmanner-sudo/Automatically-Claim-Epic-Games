# Epic Auto Claim

A small Windows program that claims the Epic Games Store's free games for you.

It starts with Windows, looks at Epic's free-games list every hour, and when there is a giveaway you do not have yet it uses **your own Epic Games Launcher** (already installed and signed in) to add the game to your library. The launcher never appears on screen while this happens, and the program shows no notifications.

> Unofficial. Not affiliated with or endorsed by Epic Games. See [Things to know](#things-to-know).

## Setup

1. Install the Epic Games Launcher and sign in once with **Remember me** ticked. (If you already use the launcher, this is done.)
2. Get `EpicAutoClaim.exe` from the **Releases** page of this repository. It is the whole program: one file, nothing to unpack. (Which build has been tried on a real account is said under *What has been tested*.)
3. Double-click `EpicAutoClaim.exe`.

A single dialog confirms the installation. That is the only window the program ever shows. From then on it starts by itself every time you sign in to Windows and works on its own. (It adds itself to the programs that start with Windows, and a task to Windows' Task Scheduler that starts it again within the hour should it ever be ended. No administrator rights are needed.)

Windows may warn about an unknown publisher the first time, because the file is not code-signed. Choose **More info → Run anyway** if you trust this build.

## What it does

- **Starts when the PC does**: Windows runs it as you sign in, and it stays in the background. It waits two minutes first (the network and the launcher's own start are not ready before that).
- Looks **every hour** (you can choose another interval, or a set time of day). It also looks a few minutes after the moment Epic has announced the next giveaway for, so with the PC on a new game is normally in your library minutes after it becomes free. It remembers when it last looked: a PC that was off overnight looks straight away, and restarting the PC twice in an hour does not make it ask twice.
- Reads Epic's public free-games list, and does not give up on it easily: it tries both of Epic's servers several times over, reads the list again when it comes back empty or half-updated, goes by Epic's clock when the PC's is wrong, and, while the list cannot be read at all, still claims giveaways it already knows to be running. If nothing is new, that is all it does until the next hour.
- When something is free and not yet claimed, it starts the launcher on a separate Windows desktop that is never displayed, opens Epic's checkout inside it, confirms that the price is zero, and places the order. If the checkout cannot be opened directly, it goes the way you would: the game's store page, **Get**, and the launcher's own checkout window.
- Closes the hidden launcher again. The whole thing takes about a minute.
- If a claim could not be made (see below), it tries again later: after ten minutes if the launcher was in use, after longer and longer waits if something went wrong.
- Writes what it did to a log file. Nothing else.

### When it waits

It never interrupts you. A claim is postponed (and retried every ten minutes) while:

- the launcher window is open,
- a game installed through the launcher is running,
- the launcher is downloading or installing something,
- a full-screen game or presentation is in front.

If the launcher is only sitting idle in the tray, the program closes it for a moment and puts it back in the tray afterwards. You can turn that off (see [Settings](#settings)); it then waits until the launcher is not running at all.

If you start the launcher yourself while a claim is in progress, the program stops at once and opens the launcher for you.

### How little it uses

It is built to be unnoticeable on the weakest PC that can run the Epic Games Launcher at all.

- One file of under 8 MB. No installer, no service, no runtime to install, no tray icon.
- Between looks it sleeps: it gives its memory back to Windows, drops to the lowest priority for the processor, the disk and memory (what Windows 11 calls efficiency mode), and does nothing but look at the clock once a minute.
- An hourly look is one small download from Epic (the list is about 30 kB) and a glance at what it already knows.
- Once an hour Windows also starts a second copy, which sees the first one and is gone again in a fraction of a second. That is all the Task Scheduler task does, and it is what brings the program back if it was ever ended.
- The only real work is the claim itself, about half a minute per giveaway, and nearly all of that is the Epic Games Launcher running hidden, not this program.
- On disk it keeps a log capped at 1 MB (plus three old ones), a small state file, and the details of the last few attempts that did not succeed.

### What it will not do

- **Pay for anything.** The order button is pressed only when the checkout states that the order's total is zero: Epic's own order preview says so, or the page's total line does, or every price on the page is zero and the page says "This is free". Immediately before the press it looks once more: still no price anywhere, still the same button under the pointer. On top of that, every request the checkout sends is held back and checked before it leaves your PC, including those of frames and workers, which are put under the same watch the moment they come into being. A request that states a price is dropped, and from then on so is everything else that could change anything. A background worker that was already running, whose requests cannot be held back, is simply kept from reaching any order address. When the claim is over, the checkout page is closed, so nothing can be sent afterwards.
- **Solve CAPTCHAs.** If Epic asks for a human check, the program leaves the game alone and tries again hours later.
- **Touch your password.** It never signs in. It relies on the launcher already being signed in, and it does not read the launcher's saved credentials.
- **Send anything anywhere** except to Epic. There is no telemetry and no account with this project.

## Checking on it

Everything lives in `%LOCALAPPDATA%\EpicAutoClaim` (paste that into the Explorer address bar):

| File | What it is |
|---|---|
| `logs\EpicAutoClaim.log` | What the program did and why |
| `config.json` | Settings |
| `state.json` | Which giveaways it has seen and their status |
| `diagnostics\` | A screenshot and details of attempts that did not succeed |

From a terminal in that folder:

| Command | What it does |
|---|---|
| `EpicAutoClaim.exe status` | What has been claimed and what is waiting |
| `EpicAutoClaim.exe check` | Lists what is free right now; claims nothing |
| `EpicAutoClaim.exe once` | Does one round now instead of waiting (Ctrl+C stops it cleanly) |
| `EpicAutoClaim.exe once --dry-run` | Same, but stops just before placing the order |
| `EpicAutoClaim.exe once --dry-run --again` | The same rehearsal, also for games it has dealt with already; records nothing |
| `EpicAutoClaim.exe doctor --launch` | Starts the launcher hidden for a moment and reports what it found; claims nothing |
| `EpicAutoClaim.exe stop` | Stops the background process; Windows starts it again within the hour, or when you next sign in |
| `EpicAutoClaim.exe install` | Installs it, or brings the installation in line with the settings (same as a double-click, without the dialog) |
| `EpicAutoClaim.exe uninstall` | Stops it and removes it from Windows' start-up programs and Task Scheduler |

A good first test on your own PC is `EpicAutoClaim.exe doctor --launch` followed by `EpicAutoClaim.exe once --dry-run`.

## Settings

Edit `config.json`. The program notices the change by itself before its next look (within the hour, unless you have set a longer interval); to apply it at once, double-click the program again (or run `EpicAutoClaim.exe install`). Every setting is optional. If the file cannot be read after an edit (a stray comma, say), the program orders nothing until it is fixed, and `EpicAutoClaim.exe status` says so.

| Setting | Default | Meaning |
|---|---|---|
| `country` | your Windows region | Two-letter country of your Epic account, used to ask which games are free there |
| `checkEveryMinutes` | `60` | How often to look for free games, in minutes (15 to 1440) |
| `dailyAt` | not set | If set, as `"HH:MM"` in your local time (e.g. `"18:30"`), look once a day at that time instead |
| `restartIdleLauncher` | `true` | Allow closing a launcher that is idle in the tray for the duration of a claim |
| `dryRun` | `false` | Go through everything except placing the order |
| `keepDiagnostics` | `true` | Keep a screenshot and details of failed attempts on this PC |
| `launcherPath` | found automatically | Full path of `EpicGamesLauncher.exe`, if it cannot be found |

## What has been tested

Tested on real Windows machines (GitHub's Windows runners) with a real Epic Games Launcher (version 20.3.8), on every change:

- finding the launcher;
- starting it hidden, including the case where it first wants to update itself;
- bringing up its interface on the hidden desktop, with **no launcher window on the visible desktop** at any moment of the trial (the test fails if one shows up even briefly);
- recognising that a launcher is signed out;
- shutting everything down again with nothing left running.

Tested in a real browser against a stand-in for Epic's store (about 70 scenarios): placing a free order directly and through the store page, a checkout window opened by the launcher itself, a checkout shown inside the store page, the EU consent dialog, both the older and the 2026 checkout layout, holiday mystery games, already-owned games, region locks, CAPTCHAs, signed-out sessions, and every case found so far in which a price above zero appears anywhere (on the page, in Epic's order preview, in the order itself however it is written or encoded, sent through a service worker, a frame from another site or a worker, including ones that only come into being at the moment of the click, or re-sent by the page after the claim has ended), none of which may result in an order.

The same scenarios are also run **inside the launcher's own built-in browser** (Chromium 90, the engine the program meets in real use) on a GitHub Windows machine: the stand-in store's pages are loaded in the real launcher's own web view. That machine has no graphics card and the launcher answers in fits and starts on it, so in each run a few scenarios ran out of time, a different few each time: over four runs, 69 to 73 of 74 passed per run, and every one but one passed in at least two of the four, those in which something tries to get a paid order out included. In no run did an order get out. The one exception cannot work there (the launcher hands a page's reloads to the system's browser when the address is not Epic's, and the stand-in store's is not); seven more are left out because that browser does not open tabs for an outsider. This is a workflow of its own (**Browser tests in the launcher's own browser**, started by hand from the Actions tab) because it takes half an hour.

Tested on a real PC with a real, signed-in account (Windows 11, launcher 20.3.8, 12 November 2026):

- the launcher was started hidden and came up in a few seconds, with no launcher window on the visible desktop;
- a rehearsal (`once --dry-run`) reached Epic's real checkout for both games that were free that day and found the zero total three ways (Epic's order preview, the page's only price "$0.00", and its sentence "This is free");
- the real run then claimed a game: **order confirmed by Epic**, in 24 seconds from start to finish;
- for the other game, which the account already had, Epic refused the order ("you already own this item"); the program now records that as "already owned";
- when Epic switched to the next week's game later that day, it was claimed four minutes after it became free: a rehearsal first (zero total, three ways), then the real run, **order confirmed by Epic** 11 seconds after the program started.

Those runs were made with build 0.1.18. The order safeguards were tightened further afterwards (see *What it will not do*), and the later builds pass every test above, the run inside the launcher's own browser included, except that last one: they have not met Epic's real checkout yet. Windows Smart App Control on the test PC refused the one later build that was tried there (0.1.23), and that refusal was left alone. Until a later build has been rehearsed with `once --dry-run` on a signed-in PC, **0.1.18 is the build known to work from start to finish**.

What a single test cannot show is how Epic's pages will change. The program is written to fail safely when they do: if the checkout does not look the way it expects, it places no order, saves what it saw under `diagnostics\`, and tries again later. If a giveaway is not claimed, the log and that folder say why.

## Things to know

- Automating a store may be against Epic's terms of service. Use this at your own discretion.
- It needs the **classic** Epic Games Launcher, the one Epic's regular installer installs. Epic's new-generation launcher (an opt-in preview since late 2026) cannot be driven this way: tested against preview 0.115, it ignores the debugging switch and exits when started hidden. If that launcher is installed, the program says so in its log and does nothing.
- Now and then the launcher has an update of its own to install, and that is the one thing it cannot do hidden. On those occasions the program lets it update in its own start-in-tray mode and puts away any window it tries to show within about a hundredth of a second; a brief flicker is possible then, nothing more.
- For the seconds it runs, the hidden launcher is your launcher in every other respect: if a game of yours has an update waiting, it may begin downloading it, and stops when the hidden launcher closes (the launcher picks the download up again the next time it runs).
- While a claim runs, the launcher's browser accepts local debugging connections on a random port, reachable only from your own PC. The port closes when the hidden launcher exits, about a minute later.
- Antivirus programs are sometimes suspicious of small unsigned programs that start other programs invisibly. That is what this one does, on purpose, and all of its source is in this repository.
- **Windows Smart App Control.** Where Smart App Control is switched on (Windows Security → App & browser control), Windows starts only programs signed by a known publisher or judged safe by Microsoft's cloud service, and there is no "run anyway". This program is not signed (a certificate costs money and needs a registered identity), so Windows judges every build afresh. On the PC it was tested on, with Smart App Control on, builds 0.1.16 to 0.1.20 were allowed to start and build 0.1.23 was refused ("An Application Control policy has blocked this file"). A refused build installs nothing and breaks nothing. The ways out are a build that Windows accepts, signing the program yourself, or your own decision about Smart App Control; the program never touches security settings, and neither did the AI that wrote it.

## How it works

1. `internal/feed` reads `freeGamesPromotions`, Epic's public list, and keeps offers whose promotion is active and whose price is zero.
2. `internal/daemon` decides what is due, with back-off after failures, and writes its timetable down so that a restart changes nothing about it. A start-up entry has Windows start the program at sign-in; `internal/sched` adds the Task Scheduler task that starts it again within the hour if it is not running.
3. `internal/launcher` and `internal/win` find the launcher, check that you are not using it, and start it on a private desktop (`CreateDesktop`) in its start-in-tray mode with `-cefdebug=<port>`, which makes its embedded browser accept DevTools connections on localhost. The process tree lives in a job object, so it cannot outlive this program.
4. `internal/claim` opens Epic's checkout in the launcher's own browser session through `internal/cdp`, reads the page, verifies the price, and clicks. Requests that could place an order are intercepted and inspected first (`watch.go`).
5. `internal/runner` ties one such session together and steps aside if you start the launcher.

### Building

```
go test ./...
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H=windowsgui -s -w" -o EpicAutoClaim.exe ./cmd/epicautoclaim
```

Go 1.26 or newer. The checkout tests need Chrome or Chromium (set `CHROMIUM_PATH` if it is not found). `ci/egl-probe.ps1` is the script that was used to establish how the launcher behaves; it installs the launcher on a Windows runner and reports.

## Made entirely by AI

Every line of this program, its tests and this page was written by an AI coding agent (Claude Code). No human wrote, edited or debugged any of the code. The owner said what they wanted in a handful of chat messages, typos and all; the AI did the research, the design, the programming, the testing and the reviewing, and finally tested the result on the owner's own PC by remote control while the owner slept.

We are, frankly, rather pleased with ourselves:

- It does something the Epic Games Launcher has no switch for, namely using the launcher without the launcher ever being seen, and it found out how by experiment: installing the real launcher on throwaway Windows machines and probing it until it gave up its secrets.
- Its first run against a real account claimed a game in 24 seconds, with no window, no click and no notification. The next week's game was in the library four minutes after Epic put it up.
- It cannot spend money by design, and that design was attacked on purpose: independent AI reviewers went over the code again and again trying to make it buy something, lose a giveaway or show a window. They found more than sixty real problems. Every one was fixed, each with a test that fails if it ever comes back.
- More than 150 automated tests, most of them driving a real browser against a stand-in for Epic's store, run on every change, together with a rehearsal against the real launcher on a real Windows machine.
- Roughly 10,000 lines of Go and 4,700 more of tests, one small file to run, and next to nothing asked of the PC.

Not bad for something nobody typed.

## Removing it

Run `EpicAutoClaim.exe uninstall` (it removes the start-up entry and the task, and stops the program), then delete the `%LOCALAPPDATA%\EpicAutoClaim` folder.
