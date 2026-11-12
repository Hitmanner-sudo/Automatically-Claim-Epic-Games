package claim

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/feed"
)

// oldBrowser stands in for the DevTools endpoint of the launcher's browser
// (Chromium 90) where it behaves differently from a current Chromium, which
// is what the other tests run against:
//
//   - a command sent to a frame that has gone away is never answered;
//   - Target.setAutoAttach sent to a frame that is being held is answered
//     only once the frame runs.
type oldBrowser struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	ws       *websocket.Conn
	got      []string // "session|method" in the order received
	gone     bool     // the child no longer answers anything
	answered map[string]time.Time
}

func newOldBrowser(t *testing.T) *oldBrowser {
	b := &oldBrowser{t: t, answered: map[string]time.Time{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ws := "ws" + strings.TrimPrefix(b.srv.URL, "http") + "/devtools/page/1"
		_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "1", "type": "page", "title": "Store", "url": "https://launcher.store.epicgames.com/", "webSocketDebuggerUrl": ws}})
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/json/list", http.StatusFound) })
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux.HandleFunc("/devtools/page/1", func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		b.mu.Lock()
		b.ws = c
		b.mu.Unlock()
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var m struct {
				ID        int64  `json:"id"`
				Method    string `json:"method"`
				SessionID string `json:"sessionId"`
				Params    struct {
					RequestID string `json:"requestId"`
				} `json:"params"`
			}
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			b.mu.Lock()
			b.got = append(b.got, m.SessionID+"|"+m.Method)
			if m.Params.RequestID != "" {
				b.answered[m.Params.RequestID] = time.Now()
			}
			silent := m.SessionID == "CHILD" && (b.gone || m.Method == "Target.setAutoAttach")
			b.mu.Unlock()
			if silent {
				continue
			}
			b.send(map[string]any{"id": m.ID, "result": map[string]any{}, "sessionId": m.SessionID})
			if m.SessionID == "" && m.Method == "Target.setAutoAttach" {
				b.send(map[string]any{"method": "Target.attachedToTarget", "params": map[string]any{
					"sessionId":          "CHILD",
					"waitingForDebugger": true,
					"targetInfo":         map[string]any{"targetId": "c1", "type": "iframe", "url": "https://newassets.hcaptcha.com/captcha/v1/x/static/hcaptcha.html"},
				}})
			}
		}
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

func (b *oldBrowser) send(v any) {
	data, _ := json.Marshal(v)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ws != nil {
		_ = b.ws.WriteMessage(websocket.TextMessage, data)
	}
}

// hangUp ends the connection, as a launcher that crashes or closes does.
func (b *oldBrowser) hangUp() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ws != nil {
		_ = b.ws.Close()
	}
}

func (b *oldBrowser) saw(entry string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, g := range b.got {
		if g == entry {
			return true
		}
	}
	return false
}

func (b *oldBrowser) waitFor(what string, cond func() bool, within time.Duration) {
	b.t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			b.mu.Lock()
			got := strings.Join(b.got, "\n  ")
			b.mu.Unlock()
			b.t.Fatalf("%s did not happen within %v; the browser received:\n  %s", what, within, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAHeldFrameIsLetRunAndAVanishedOneHoldsNobodyUp(t *testing.T) {
	b := newOldBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := Open(ctx, b.srv.URL, Options{SettleWait: 50 * time.Millisecond, UIWait: 10 * time.Second, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// The frame is supervised and then let run, although the browser does
	// not answer the request to have the frame's own children handed over.
	b.waitFor("letting the held frame run", func() bool { return b.saw("CHILD|Runtime.runIfWaitingForDebugger") }, 3*time.Second)
	if !b.saw("CHILD|Fetch.enable") || !b.saw("CHILD|Target.setAutoAttach") {
		t.Fatal("the frame was let run without being supervised first")
	}
	frames, _ := s.unsupervised()
	if len(frames) != 0 {
		t.Fatalf("unsupervised frames: %v", frames)
	}

	// The frame goes away with a request of its own still held back: the
	// browser never answers what is sent to it now. The page's own
	// requests must be answered all the same, at once.
	b.mu.Lock()
	b.gone = true
	b.mu.Unlock()
	paused := func(session, id, url string) map[string]any {
		ev := map[string]any{"method": "Fetch.requestPaused", "params": map[string]any{
			"requestId": id, "request": map[string]any{"url": url, "method": "GET"},
		}}
		if session != "" {
			ev["sessionId"] = session
		}
		return ev
	}
	b.send(paused("CHILD", "child-1", "https://newassets.hcaptcha.com/c/x.js"))
	b.send(paused("CHILD", "child-2", "https://newassets.hcaptcha.com/c/y.js"))
	sent := time.Now()
	b.send(paused("", "page-1", "https://launcher.store.epicgames.com/purchase"))
	b.waitFor("answering the page's own request", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		_, ok := b.answered["page-1"]
		return ok
	}, 2*time.Second)
	if d := time.Since(sent); d > time.Second {
		t.Fatalf("the page's request waited %v behind a frame that had gone away", d)
	}
}

func TestALauncherThatGoesAwayEndsTheClaimAtOnce(t *testing.T) {
	// Seen on a real PC: the launcher crashed a second before the order
	// button was to be pressed. The claim must say so at once (not wait out
	// every address the checkout could be at), and the session must report
	// that it is over, so that the launcher is started again.
	b := newOldBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s, err := Open(ctx, b.srv.URL, Options{SettleWait: 50 * time.Millisecond, UIWait: 10 * time.Second, CheckoutWait: 40 * time.Second, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Gone() {
		t.Fatal("the session counts as over before anything has happened")
	}
	go func() {
		time.Sleep(1500 * time.Millisecond)
		b.hangUp()
	}()
	started := time.Now()
	res := s.Claim(ctx, feed.Offer{Title: "Game", Namespace: "ns", OfferID: "id"})
	if res.Outcome != Failed || !strings.Contains(res.Detail, "the launcher went away") || !strings.Contains(res.Detail, "nothing was pressed") {
		t.Fatalf("result = %v, want a failure that says the launcher went away and nothing was pressed", res)
	}
	if !s.Gone() {
		t.Fatal("the session does not report that the launcher is gone")
	}
	if d := time.Since(started); d > 12*time.Second {
		t.Fatalf("the claim took %v to notice", d)
	}
}
