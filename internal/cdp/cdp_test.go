package cdp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/cdp"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/cdp/cdptest"
)

const testPage = `<!doctype html><html><head><title>t</title></head><body style="margin:0">
<button id="b" style="position:absolute;left:200px;top:150px;width:120px;height:40px">Go</button>
<iframe id="f" src="/inner" style="position:absolute;left:0;top:300px;width:400px;height:100px"></iframe>
<script>
window.clicks = [];
document.getElementById('b').addEventListener('click', function (e) {
  window.clicks.push({trusted: e.isTrusted, x: e.clientX, y: e.clientY});
});
window.moves = 0;
document.addEventListener('mousemove', function () { window.moves++; });
</script></body></html>`

func waitFor(t *testing.T, p *cdp.Page, expr string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		var ok bool
		err := p.Eval(ctx, expr, &ok)
		cancel()
		if err == nil && ok {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("condition never became true: %s", expr)
}

func TestAgainstRealBrowser(t *testing.T) {
	base := cdptest.Start(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/inner" {
			_, _ = w.Write([]byte(`<!doctype html><body><p id="in">inner frame</p></body>`))
			return
		}
		_, _ = w.Write([]byte(testPage))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v, err := cdp.Version(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if v.Browser == "" || v.WebSocketDebuggerURL == "" {
		t.Fatalf("version = %+v", v)
	}

	page, err := cdp.OpenPage(ctx, cdptest.FirstPage(t, base))
	if err != nil {
		t.Fatal(err)
	}
	defer page.Close()

	if err := page.Navigate(ctx, srv.URL+"/"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, page, `document.readyState === "complete" && !!document.getElementById("b")`)

	// structured values come back decoded
	var info struct {
		Title string `json:"title"`
		N     int    `json:"n"`
	}
	if err := page.Eval(ctx, `({title: document.title, n: document.querySelectorAll("button").length})`, &info); err != nil {
		t.Fatal(err)
	}
	if info.Title != "t" || info.N != 1 {
		t.Fatalf("info = %+v", info)
	}

	// promises are awaited
	var later string
	if err := page.Eval(ctx, `new Promise(function (r) { setTimeout(function () { r("done") }, 50) })`, &later); err != nil || later != "done" {
		t.Fatalf("promise = %q, %v", later, err)
	}

	// exceptions surface as errors
	var evalErr *cdp.EvalError
	if err := page.Eval(ctx, `(function () { throw new Error("boom") })()`, nil); !errors.As(err, &evalErr) || !strings.Contains(evalErr.Text, "boom") {
		t.Fatalf("expected EvalError, got %v", err)
	}

	// a real (trusted) click lands on the button
	if err := page.Click(ctx, 260, 170); err != nil {
		t.Fatal(err)
	}
	waitFor(t, page, `window.clicks.length === 1`)
	var click struct {
		Trusted bool    `json:"trusted"`
		X       float64 `json:"x"`
		Y       float64 `json:"y"`
	}
	if err := page.Eval(ctx, `window.clicks[0]`, &click); err != nil {
		t.Fatal(err)
	}
	if !click.Trusted || click.X != 260 || click.Y != 170 {
		t.Fatalf("click = %+v", click)
	}
	var moves int
	if err := page.Eval(ctx, `window.moves`, &moves); err != nil || moves < 3 {
		t.Fatalf("pointer did not glide to the target: moves=%d err=%v", moves, err)
	}

	// a caller that has been called off sends nothing: neither a script...
	dead, kill := context.WithCancel(ctx)
	kill()
	if err := page.Eval(dead, `window.afterCancel = true`, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Eval on a cancelled context: %v", err)
	}
	// ...nor a click
	if err := page.Click(dead, 260, 170); !errors.Is(err, context.Canceled) || errors.Is(err, cdp.ErrPressed) {
		t.Fatalf("Click on a cancelled context: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	var after struct {
		Flag   bool `json:"flag"`
		Clicks int  `json:"clicks"`
	}
	if err := page.Eval(ctx, `({flag: !!window.afterCancel, clicks: window.clicks.length})`, &after); err != nil || after.Flag || after.Clicks != 1 {
		t.Fatalf("commands were carried out after cancellation: %+v (%v)", after, err)
	}

	// same-site iframes can be evaluated in
	waitFor(t, page, `!!document.getElementById("f").contentDocument && document.getElementById("f").contentDocument.readyState === "complete"`)
	frames, err := page.Frames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var inner string
	for _, f := range frames {
		if strings.HasSuffix(f.URL, "/inner") {
			if err := page.EvalInFrame(ctx, f.ID, `document.getElementById("in").textContent`, &inner); err != nil {
				t.Fatal(err)
			}
		}
	}
	if inner != "inner frame" {
		t.Fatalf("frames = %+v inner = %q", frames, inner)
	}

	shot, err := page.Screenshot(ctx)
	if err != nil || len(shot) < 1000 {
		t.Fatalf("screenshot len=%d err=%v", len(shot), err)
	}

	// navigation errors are reported
	if err := page.Navigate(ctx, "http://127.0.0.1:1/"); err == nil {
		t.Fatal("expected a navigation error for a closed port")
	}

	// a cancelled context aborts a pending call
	short, cancelShort := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancelShort()
	if err := page.Eval(short, `new Promise(function () {})`, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}

	// calls on a closed connection fail fast
	_ = page.Close()
	select {
	case <-page.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("connection did not report closure")
	}
	if err := page.Eval(ctx, `1`, nil); !errors.Is(err, cdp.ErrClosed) {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
}
