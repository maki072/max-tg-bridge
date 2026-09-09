package bridge

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"io"
	"net/http"
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
	resp, err := client.Get(url)
	if err != nil {
		return nil, "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", false, fmt.Errorf("http %d", resp.StatusCode)
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
