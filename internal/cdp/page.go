package cdp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// Page wraps a connection to a page target with the handful of operations
// the claimer performs.
type Page struct {
	*Conn
	mouseX, mouseY float64
}

// OpenPage connects to a page target.
func OpenPage(ctx context.Context, t Target) (*Page, error) {
	if t.WebSocketDebuggerURL == "" {
		return nil, errors.New("target has no debugger URL (is another debugger attached?)")
	}
	c, err := Dial(ctx, t.WebSocketDebuggerURL)
	if err != nil {
		return nil, err
	}
	return &Page{Conn: c}, nil
}

// Navigate loads a URL in the page's main frame. It returns once the
// navigation has been committed, not when the page has finished loading.
func (p *Page) Navigate(ctx context.Context, url string) error {
	var res struct {
		FrameID   string `json:"frameId"`
		ErrorText string `json:"errorText"`
	}
	if err := p.Call(ctx, "Page.navigate", map[string]any{"url": url}, &res); err != nil {
		return err
	}
	if res.ErrorText != "" {
		return fmt.Errorf("navigate %s: %s", url, res.ErrorText)
	}
	return nil
}

// EvalError is a JavaScript exception thrown by an evaluated expression.
type EvalError struct{ Text string }

func (e *EvalError) Error() string { return "javascript: " + e.Text }

type evalResult struct {
	Result struct {
		Type  string          `json:"type"`
		Value json.RawMessage `json:"value"`
	} `json:"result"`
	ExceptionDetails *struct {
		Text      string `json:"text"`
		Exception *struct {
			Description string `json:"description"`
		} `json:"exception"`
	} `json:"exceptionDetails"`
}

func decodeEval(res evalResult, out any) error {
	if ex := res.ExceptionDetails; ex != nil {
		text := ex.Text
		if ex.Exception != nil && ex.Exception.Description != "" {
			text = ex.Exception.Description
		}
		return &EvalError{Text: text}
	}
	if out == nil || len(res.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(res.Result.Value, out)
}

// Eval evaluates a JavaScript expression in the main frame and decodes its
// (JSON-serialisable) value into out. Promises are awaited.
func (p *Page) Eval(ctx context.Context, expression string, out any) error {
	var res evalResult
	err := p.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	}, &res)
	if err != nil {
		return err
	}
	return decodeEval(res, out)
}

// Frame is one frame of the page.
type Frame struct {
	ID       string
	ParentID string
	URL      string
	Name     string
}

type frameTree struct {
	Frame struct {
		ID       string `json:"id"`
		ParentID string `json:"parentId"`
		URL      string `json:"url"`
		Name     string `json:"name"`
	} `json:"frame"`
	ChildFrames []frameTree `json:"childFrames"`
}

func flatten(t frameTree, out *[]Frame) {
	*out = append(*out, Frame{ID: t.Frame.ID, ParentID: t.Frame.ParentID, URL: t.Frame.URL, Name: t.Frame.Name})
	for _, c := range t.ChildFrames {
		flatten(c, out)
	}
}

// Frames lists the frames that live in this target's process (same-site
// iframes included; cross-site iframes are separate targets).
func (p *Page) Frames(ctx context.Context) ([]Frame, error) {
	var res struct {
		FrameTree frameTree `json:"frameTree"`
	}
	if err := p.Call(ctx, "Page.getFrameTree", nil, &res); err != nil {
		return nil, err
	}
	var out []Frame
	flatten(res.FrameTree, &out)
	return out, nil
}

// EvalInFrame evaluates an expression inside a specific frame.
func (p *Page) EvalInFrame(ctx context.Context, frameID, expression string, out any) error {
	var world struct {
		ExecutionContextID int64 `json:"executionContextId"`
	}
	if err := p.Call(ctx, "Page.createIsolatedWorld", map[string]any{"frameId": frameID, "worldName": "claim"}, &world); err != nil {
		return err
	}
	var res evalResult
	err := p.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
		"contextId":     world.ExecutionContextID,
	}, &res)
	if err != nil {
		return err
	}
	return decodeEval(res, out)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Page) mouse(ctx context.Context, typ string, x, y float64, extra map[string]any) error {
	params := map[string]any{"type": typ, "x": x, "y": y}
	for k, v := range extra {
		params[k] = v
	}
	return p.Call(ctx, "Input.dispatchMouseEvent", params, nil)
}

// Click moves the pointer to (x, y) in viewport coordinates and performs a
// left click there, the way a person using a mouse would: a short glide,
// press, brief hold, release.
func (p *Page) Click(ctx context.Context, x, y float64) error {
	const steps = 8
	fromX, fromY := p.mouseX, p.mouseY
	for i := 1; i <= steps; i++ {
		f := float64(i) / steps
		// ease-out so the pointer decelerates into the target
		f = 1 - (1-f)*(1-f)
		if err := p.mouse(ctx, "mouseMoved", fromX+(x-fromX)*f, fromY+(y-fromY)*f, nil); err != nil {
			return err
		}
		if err := sleepCtx(ctx, time.Duration(12+rand.IntN(18))*time.Millisecond); err != nil {
			return err
		}
	}
	p.mouseX, p.mouseY = x, y
	click := map[string]any{"button": "left", "buttons": 1, "clickCount": 1}
	if err := p.mouse(ctx, "mousePressed", x, y, click); err != nil {
		return err
	}
	if err := sleepCtx(ctx, time.Duration(50+rand.IntN(60))*time.Millisecond); err != nil {
		return err
	}
	click["buttons"] = 0
	return p.mouse(ctx, "mouseReleased", x, y, click)
}

// Screenshot captures the page as JPEG, for diagnostics.
func (p *Page) Screenshot(ctx context.Context) ([]byte, error) {
	var res struct {
		Data string `json:"data"`
	}
	if err := p.Call(ctx, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 60}, &res); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(res.Data)
}
