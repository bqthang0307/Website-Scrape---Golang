import express from "express";
import { chromium } from "playwright";
import sharp from "sharp";

const app = express();
app.use(express.json({ limit: "10mb" }));

app.post("/scrape", async (req, res) => {
  const {
    url,
    timeout_ms = 30000,
    viewport_width = 1280,
    viewport_height = 1024,
    settle_delay_ms = 300,
    overlap_px = 140,
    image_format = "jpeg", // "png" or "jpeg"
    jpeg_quality = 85,
  } = req.body;

  const browser = await chromium.launch({
    headless: true,
    args: ["--no-sandbox", "--disable-gpu"]
  });

  const context = await browser.newContext({
    viewport: { width: viewport_width, height: viewport_height },
    deviceScaleFactor: 1
  });

  const page = await context.newPage();

  try {
    // Set sane timeouts
    page.setDefaultNavigationTimeout(timeout_ms);
    page.setDefaultTimeout(timeout_ms);

    // Block heavy/analytics requests that can keep the network busy
    await context.route("**/*", route => {
      const reqUrl = route.request().url();
      const isAnalytics = [
        "googletagmanager.com",
        "google-analytics.com",
        "facebook.com/tr",
        "hotjar.com",
        "segment.com",
        "mixpanel.com",
        "fullstory.com"
      ].some(domain => reqUrl.includes(domain));
      const isMedia = /\.(mp4|webm|gif|mov|avi)(\?|$)/i.test(reqUrl);
      if (isAnalytics || isMedia) return route.abort();
      return route.continue();
    });

    // Avoid networkidle which is unreliable on sites with beacons/analytics
    await page.goto(url, { timeout: timeout_ms, waitUntil: "domcontentloaded" });
    // Give the page a moment to finish loading assets
    await page.waitForLoadState("load", { timeout: Math.min(timeout_ms, 10000) }).catch(() => {});

    // disable animations & parallax
    await page.addStyleTag({ content: `
      * { animation: none !important; transition: none !important; }
      html, body, * { background-attachment: initial !important; scroll-behavior: auto !important; }
    `});

    // force eager load for lazy images
    await page.evaluate(() => {
      document.querySelectorAll("img[loading]").forEach(img => img.loading = "eager");
      document.querySelectorAll("img[data-src]").forEach(img => {
        if (!img.src) img.src = img.getAttribute("data-src");
      });
    });

    let totalHeight = await page.evaluate(() =>
      Math.max(document.body.scrollHeight, document.documentElement.scrollHeight)
    );

    // Auto-scroll through the page to trigger lazy loading
    const scrollStep = Math.max(200, Math.floor(viewport_height * 0.8));
    let currentY = 0;
    while (currentY + viewport_height < totalHeight) {
      await page.evaluate(_y => window.scrollTo(0, _y), currentY);
      await page.waitForTimeout(settle_delay_ms);
      currentY += scrollStep;
    }
    // Ensure we hit the bottom at least once
    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    await page.waitForTimeout(Math.max(400, settle_delay_ms));
    // Recompute height in case content expanded after lazy loads
    totalHeight = await page.evaluate(() =>
      Math.max(document.body.scrollHeight, document.documentElement.scrollHeight)
    );
    // Return to top for consistent screenshots
    await page.evaluate(() => window.scrollTo(0, 0));
    await page.waitForTimeout(Math.min(800, Math.max(200, settle_delay_ms)));

    // First try native full-page screenshot to capture entire page in one image
    let finalBuffer = null;
    try {
      finalBuffer = await page.screenshot({
        fullPage: true,
        type: image_format === "jpeg" ? "jpeg" : "png",
        quality: image_format === "jpeg" ? jpeg_quality : undefined
      });
    } catch (_) {}

    if (!finalBuffer) {
      // Fallback: tile + stitch
      const tiles = [];
      let y = 0;
      while (y < totalHeight) {
        await page.evaluate(_y => window.scrollTo(0, _y), y);
        await page.waitForTimeout(settle_delay_ms);

        const buf = await page.screenshot({ fullPage: false });
        tiles.push(buf);

        y += viewport_height - overlap_px;
        if (y + viewport_height >= totalHeight) {
          await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
          await page.waitForTimeout(settle_delay_ms);
          tiles.push(await page.screenshot({ fullPage: false }));
          break;
        }
      }

      // Stitch vertically with Sharp (normalize widths, compute final height first)
      if (tiles.length === 0) {
        throw new Error("No screenshots captured");
      }

      const prepared = await Promise.all(
        tiles.map(async b => {
          const img = sharp(b).ensureAlpha();
          const meta = await img.metadata();
          return { img, meta };
        })
      );

      const targetWidth = Math.min(...prepared.map(p => p.meta.width || 0));
      const normalized = await Promise.all(
        prepared.map(async (p) => {
          let buf = await (p.meta.width !== targetWidth
            ? p.img.resize({ width: targetWidth }).toBuffer()
            : p.img.toBuffer());
          const meta = await sharp(buf).metadata();
          return { buf, width: meta.width || targetWidth, height: meta.height || 0 };
        })
      );

      // Compute final canvas height accounting for overlaps
      let finalHeight = 0;
      for (let i = 0; i < normalized.length; i++) {
        const h = normalized[i].height;
        if (i === 0) finalHeight += h; else finalHeight += Math.max(0, h - overlap_px);
      }

      let stitched = sharp({
        create: {
          width: targetWidth,
          height: finalHeight,
          channels: 4,
          background: { r: 255, g: 255, b: 255, alpha: 1 }
        }
      });

      let yOffset = 0;
      for (let i = 0; i < normalized.length; i++) {
        const { buf, height } = normalized[i];
        const topY = i === 0 ? yOffset : yOffset - overlap_px;
        stitched = stitched.composite([{ input: buf, top: topY, left: 0 }]);
        yOffset = topY + height;
      }

      finalBuffer = await stitched
        .toFormat(image_format, image_format === "jpeg" ? { quality: jpeg_quality } : {})
        .toBuffer();
    }

    const b64 = finalBuffer.toString("base64");

    const title = await page.title();

    res.json({
      ok: true,
      data: {
        screenshot_base64: b64,
        content_type: image_format === "jpeg" ? "image/jpeg" : "image/png",
        title,
        final_url: page.url(),
        viewport: { width: viewport_width, height: viewport_height },
        overlap_px,
        settle_delay_ms,
        total_height_px: totalHeight
      }
    });
  } catch (err) {
    res.status(500).json({ ok: false, error: err.message });
  } finally {
    await browser.close();
  }
});

const port = process.env.PORT || 8090;
app.listen(port, () => {
  console.log(`Listening on :${port}`);
});
