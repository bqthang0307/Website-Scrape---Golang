package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"
	"os"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

type ScrapeRequest struct {
	URL             string `json:"url"`
	TimeoutMS       int    `json:"timeout_ms"`
	ViewportWidth   int    `json:"viewport_width"`
	ViewportHeight  int    `json:"viewport_height"`
	SettleDelayMS   int    `json:"settle_delay_ms"`
	OverlapPX       int    `json:"overlap_px"`
	ImageFormat     string `json:"image_format"`
	JPEGQuality     int    `json:"jpeg_quality"`
	BlockMedia      bool   `json:"block_media"`
	WaitUntilNetIdle bool  `json:"wait_until_netidle"`
}

type ScrapeResponse struct {
	OK           bool                   `json:"ok"`
	Data         map[string]interface{} `json:"data"`
	Error        string                 `json:"error,omitempty"`
	NotifyResult interface{}            `json:"notify_result,omitempty"`
}

func main() {
	http.HandleFunc("/scrape", handleScrape)
  
	port := os.Getenv("PORT")
	if port == "" {
	  port = "8080"
	}
	log.Println("Listening on :" + port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
  }

  func handleScrape(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req ScrapeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Defaults
	if req.TimeoutMS <= 0 {
		req.TimeoutMS = 30000
	}
	if req.ViewportWidth <= 0 {
		req.ViewportWidth = 1280
	}
	if req.ViewportHeight <= 0 {
		req.ViewportHeight = 1024
	}
	if req.SettleDelayMS <= 0 {
		req.SettleDelayMS = 300
	}
	if req.OverlapPX <= 0 {
		req.OverlapPX = 140
	}
	if req.ImageFormat == "" {
		req.ImageFormat = "jpeg"
	}
	if req.JPEGQuality <= 0 || req.JPEGQuality > 95 {
		req.JPEGQuality = 85
	}
	// Render headless environments benefit from a brief idle wait
	if r.URL.Query().Get("debug") == "1" {
		log.Printf("REQ: %+v\n", req)
	}

	ctx, cancel := chromedp.NewContext(
		context.Background(),
		chromedp.WithBrowserOption(
			// You can add more args if needed
			chromedp.Flag("headless", true),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("no-sandbox", true),
		),
	)
	defer cancel()

	// Global timeout
	ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMS)*time.Millisecond)
	defer cancel()

	// Create a new tab context
	taskCtx, cancel := chromedp.NewContext(ctx)
	defer cancel()

	// Override viewport early
	if err := chromedp.Run(taskCtx,
		chromedp.EmulateViewport(int64(req.ViewportWidth), int64(req.ViewportHeight), chromedp.EmulateScale(1.0)),
	); err != nil {
		writeErr(w, err)
		return
	}

	// Optionally block "media" requests (videos) to reduce buffering
	if req.BlockMedia {
		if err := blockMediaRequests(taskCtx); err != nil {
			writeErr(w, err)
			return
		}
	}

	// Navigate
	navOpts := []chromedp.Action{
		chromedp.Navigate(req.URL),
	}
	if req.WaitUntilNetIdle {
		// Wait for network to be (mostly) idle
		navOpts = append(navOpts, chromedp.WaitReady("body", chromedp.ByQuery), chromedp.Sleep(300*time.Millisecond))
	}
	if err := chromedp.Run(taskCtx, navOpts...); err != nil {
		writeHTTPError(w, http.StatusGatewayTimeout, "navigation timeout or error: "+err.Error())
		return
	}

	// Reduce motion / disable transitions & parallax to avoid stitch gaps
	if err := chromedp.Run(taskCtx,
		chromedp.Evaluate(`(function(){
			try { if (window.matchMedia) { /* nothing specific needed */ } } catch(e){}
			var style = document.createElement('style');
			style.innerHTML = `
			  * { animation: none !important; transition: none !important; }
			  html, body, * { background-attachment: initial !important; background-position: 0 0 !important; scroll-behavior: auto !important; }
			`;
			document.head.appendChild(style);
		})()`, nil),
	); err != nil {
		// best-effort
	}

	// Force eager loading for common lazy patterns
	_ = chromedp.Run(taskCtx, chromedp.Evaluate(`(function(){
		try {
			document.querySelectorAll('img[loading]').forEach(img => img.loading = 'eager');
			document.querySelectorAll('img[decoding]').forEach(img => img.decoding = 'sync');
			document.querySelectorAll('img[data-src]').forEach(img => { if(!img.src) img.src = img.getAttribute('data-src'); });
			document.querySelectorAll('img[data-srcset]').forEach(img => { if(!img.srcset) img.srcset = img.getAttribute('data-srcset'); });
			document.querySelectorAll('source[data-srcset]').forEach(s => { if(!s.srcset) s.srcset = s.getAttribute('data-srcset'); });
			document.querySelectorAll('iframe[data-src]').forEach(f => { if(!f.src) f.src = f.getAttribute('data-src'); });
			document.querySelectorAll('video').forEach(v => { try { v.preload = 'metadata'; v.pause(); } catch(e){} });
		} catch(e){}
	})()`, nil))

	// Let things settle
	time.Sleep(600 * time.Millisecond)
	_ = chromedp.Run(taskCtx, waitAssetsReady( minInt(8000, maxInt(2000, req.TimeoutMS/4)) ))

	// Compute total scroll height
	var totalHeight float64
	if err := chromedp.Run(taskCtx, chromedp.Evaluate(`Math.max(document.documentElement.scrollHeight || 0, document.body.scrollHeight || 0)`, &totalHeight)); err != nil {
		writeErr(w, err)
		return
	}
	if totalHeight < 1 {
		writeHTTPError(w, http.StatusInternalServerError, "page height detection failed")
		return
	}

	// Start at top
	_ = chromedp.Run(taskCtx, chromedp.Evaluate(`window.scrollTo(0,0)`, nil))
	time.Sleep(200 * time.Millisecond)

	// Scroll & capture viewport tiles (PNG), then stitch in-memory
	tiles := make([]image.Image, 0, 32)
	cursorY := 0
	step := req.ViewportHeight - req.OverlapPX
	if step < 50 {
		step = int(float64(req.ViewportHeight) * 0.75) // safety
	}

	for {
		// Scroll to Y
		if err := chromedp.Run(taskCtx, chromedp.Evaluate(`window.scrollTo(0, `+strconv.Itoa(cursorY)+`)`, nil)); err != nil {
			writeErr(w, err)
			return
		}
		time.Sleep(time.Duration(req.SettleDelayMS) * time.Millisecond)

		// Capture viewport-only by clipping (0,0) to Viewport size
		var buf []byte
		err := chromedp.Run(taskCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			c, err := page.CaptureScreenshot().
				WithFormat(page.CaptureScreenshotFormatPng).
				WithFromSurface(true).
				WithClip(&page.Viewport{
					X:      0,
					Y:      0,
					Width:  float64(req.ViewportWidth),
					Height: float64(req.ViewportHeight),
					Scale:  1.0,
				}).Do(ctx)
			if err != nil {
				return err
			}
			buf = c
			return nil
		}))
		if err != nil {
			writeErr(w, err)
			return
		}

		img, err := png.Decode(bytes.NewReader(buf))
		if err != nil {
			writeErr(w, err)
			return
		}
		tiles = append(tiles, img)

		cursorY += step
		if float64(cursorY)+float64(req.ViewportHeight) >= totalHeight {
			// Jump to bottom once to grab final tile
			_ = chromedp.Run(taskCtx, chromedp.Evaluate(`window.scrollTo(0, document.documentElement.scrollHeight)`, nil))
			time.Sleep(time.Duration(req.SettleDelayMS) * time.Millisecond)

			var last []byte
			err := chromedp.Run(taskCtx, chromedp.ActionFunc(func(ctx context.Context) error {
				c, err := page.CaptureScreenshot().
					WithFormat(page.CaptureScreenshotFormatPng).
					WithFromSurface(true).
					WithClip(&page.Viewport{
						X:      0,
						Y:      0,
						Width:  float64(req.ViewportWidth),
						Height: float64(req.ViewportHeight),
						Scale:  1.0,
					}).Do(ctx)
				if err != nil {
					return err
				}
				last = c
				return nil
			}))
			if err != nil {
				writeErr(w, err)
				return
			}
			imgLast, err := png.Decode(bytes.NewReader(last))
			if err != nil {
				writeErr(w, err)
				return
			}
			tiles = append(tiles, imgLast)
			break
		}
	}

	// Stitch tiles with overlap compensation
	out := stitchVertical(tiles, req.OverlapPX)

	// Encode final to desired format
	var finalBuf bytes.Buffer
	ct := "image/png"
	switch lower(req.ImageFormat) {
	case "png":
		if err := png.Encode(&finalBuf, out); err != nil {
			writeErr(w, err)
			return
		}
	default:
		ct = "image/jpeg"
		opts := &jpeg.Options{Quality: clamp(req.JPEGQuality, 1, 95)}
		if err := jpeg.Encode(&finalBuf, out, opts); err != nil {
			writeErr(w, err)
			return
		}
	}

	b64 := base64.StdEncoding.EncodeToString(finalBuf.Bytes())
	title := ""
	_ = chromedp.Run(taskCtx, chromedp.Title(&title))
	finalURL := ""
	_ = chromedp.Run(taskCtx, chromedp.Location(&finalURL))

	resp := ScrapeResponse{
		OK: true,
		Data: map[string]interface{}{
			"screenshot_base64": b64,
			"content_type":      ct,
			"title":             title,
			"final_url":         finalURL,
			"viewport": map[string]int{
				"width":  req.ViewportWidth,
				"height": req.ViewportHeight,
			},
			"overlap_px":      req.OverlapPX,
			"settle_delay_ms": req.SettleDelayMS,
			"total_height_px": int(math.Round(totalHeight)),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func blockMediaRequests(ctx context.Context) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return page.SetBypassCSP(true).Do(ctx) // not strictly required, but helpful for some sites
	}))
}

func waitAssetsReady(timeoutMS int) chromedp.Action {
	script := `(async (timeout) => {
		const abort = new Promise((_, rej) => setTimeout(() => rej(new Error('assets-timeout')), timeout));
		const fontsReady = (typeof document.fonts !== 'undefined') ? document.fonts.ready.catch(()=>{}) : Promise.resolve();
		const imgs = Array.from(document.images || []);
		const imgsReady = Promise.all(imgs.map(img => img.complete ? Promise.resolve()
			: (img.decode ? img.decode().catch(()=>{})
			  : new Promise(r => { img.addEventListener('load', r, {once:true}); img.addEventListener('error', r, {once:true}); }))));
		return Promise.race([Promise.all([fontsReady, imgsReady]), abort]);
	})`
	return chromedp.Evaluate(script, nil, chromedp.EvalAsValue, chromedp.WithArgs(timeoutMS))
}

func stitchVertical(tiles []image.Image, overlap int) image.Image {
	if len(tiles) == 0 {
		r := image.Rect(0, 0, 1, 1)
		return image.NewRGBA(r)
	}
	w := tiles[0].Bounds().Dx()
	total := tiles[0].Bounds().Dy()
	for i := 1; i < len(tiles); i++ {
		total += tiles[i].Bounds().Dy() - overlap
	}
	out := image.NewRGBA(image.Rect(0, 0, w, total))

	cursorY := 0
	for i, img := range tiles {
		if i == 0 {
			draw.Draw(out, image.Rect(0, cursorY, w, cursorY+img.Bounds().Dy()), img, image.Point{}, draw.Src)
			cursorY += img.Bounds().Dy()
		} else {
			pasteY := cursorY - overlap
			draw.Draw(out, image.Rect(0, pasteY, w, pasteY+img.Bounds().Dy()), img, image.Point{}, draw.Src)
			cursorY = pasteY + img.Bounds().Dy()
		}
	}
	return out
}

func writeErr(w http.ResponseWriter, err error) {
	writeHTTPError(w, http.StatusInternalServerError, err.Error())
}

func writeHTTPError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(ScrapeResponse{OK: false, Error: msg})
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func minInt(a, b int) int { if a < b { return a } ; return b }
func maxInt(a, b int) int { if a > b { return a } ; return b }