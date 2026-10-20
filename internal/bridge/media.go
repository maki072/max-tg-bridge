package bridge

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/image/webp"
)

// tgUploadLimit is the largest file a bot may upload via the Telegram Bot API
// (sendVideo/sendAudio/sendDocument). Media above this can't be re-uploaded, so
// the bridge falls back to posting the source link instead.
const tgUploadLimit = 50 * 1024 * 1024

// httpDownload fetches a public URL into memory (bounded by maxBytes). MAX media
// URLs (i.oneme.ru / file-ms.oneme.ru) are directly fetchable without auth.
func httpDownload(url string, maxBytes int64) ([]byte, string, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("http %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, "", err
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// httpDownloadCapped fetches url into memory but, unlike httpDownload, refuses to
// silently truncate: if the resource is larger than maxBytes it returns
// tooLarge=true with no data, so callers can post the link instead of uploading
// a corrupt, unplayable partial file. Used for video/audio, which can be big.
func httpDownloadCapped(url string, maxBytes int64) (data []byte, contentType string, tooLarge bool, err error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "", false, err
	}
	// MAX's video CDN can reject header-less Go requests with a 400.
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
	req.Header.Set("Referer", "https://web.max.ru/")
	req.Header.Set("Origin", "https://web.max.ru")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		host, path := url, ""
		if i := strings.Index(url, "://"); i >= 0 {
			host = url[i+3:]
		}
		if i := strings.IndexAny(host, "/?"); i >= 0 {
			host, path = host[:i], host[i:]
		}
		if len(path) > 60 {
			path = path[:60] + "…"
		}
		return nil, "", false, fmt.Errorf("http %d (%s%s) body=%q", resp.StatusCode, host, path, snippet)
	}
	ct := resp.Header.Get("Content-Type")
	if resp.ContentLength > maxBytes {
		return nil, ct, true, nil
	}
	// Read one byte past the cap so an over-limit body with no Content-Length
	// header is still detected as too large rather than truncated.
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", false, err
	}
	if int64(len(b)) > maxBytes {
		return nil, ct, true, nil
	}
	return b, ct, false, nil
}

// webpToJPEG converts a WebP image to JPEG (Telegram sendPhoto needs JPEG/PNG,
// MAX serves photos as WebP).
func webpToJPEG(data []byte) ([]byte, error) {
	img, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// audioExt picks a file extension for downloaded audio from its Content-Type
// (MAX's voice URLs carry no file name).
func audioExt(contentType string) string {
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "ogg"), strings.Contains(ct, "opus"):
		return ".ogg"
	case strings.Contains(ct, "webm"):
		return ".webm"
	case strings.Contains(ct, "mpeg"), strings.Contains(ct, "mp3"):
		return ".mp3"
	case strings.Contains(ct, "mp4"), strings.Contains(ct, "m4a"), strings.Contains(ct, "aac"):
		return ".m4a"
	case strings.Contains(ct, "wav"):
		return ".wav"
	}
	return ".ogg"
}

// isAudioFile reports whether a file name looks like a playable audio file
// Telegram's sendAudio understands.
func isAudioFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp3", ".m4a", ".ogg", ".oga", ".opus", ".flac", ".wav", ".aac":
		return true
	}
	return false
}
