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
	log.Println("Listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// ... (rest of Go code omitted in this file preview for brevity, same as assistant's previous message)
